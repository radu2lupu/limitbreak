package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpencodeWindowsReadsRollingWeeklyAndMonthly(t *testing.T) {
	raw := []byte(`{"usage":{"rolling":{"status":"ok","percent":12,"resetsAt":"2026-11-18T09:13:31.000Z"},` +
		`"weekly":{"status":"ok","percent":100,"resetsAt":"2026-11-23T00:00:00.000Z"},"monthly":{"status":"ok","percent":40,"resetsAt":"2026-12-16T21:12:43.000Z"}}}`)
	windows, err := opencodeWindows(raw, blameNow)
	if err != nil || len(windows) != 3 {
		t.Fatalf("windows %+v err %v", windows, err)
	}
	if w := windows[0]; w.ID != "rolling" || w.Label != "5h" || w.UsedPercent != 12 || w.WindowMinutes != 300 || *w.ResetsAt != 1794993211 {
		t.Fatalf("rolling %+v", w)
	}
	if w := windows[1]; w.Label != "weekly" || w.WindowMinutes != 7*24*60 {
		t.Fatalf("weekly %+v", w)
	}
	if until := opencodeSpentUntil(windows); until == nil || *until != *windows[1].ResetsAt {
		t.Fatalf("a full weekly window should spend the account until it resets: %v", until)
	}
	if opencodeSpentUntil(windows[:1]) != nil {
		t.Fatal("a window with room spent the account")
	}
}

func TestOpencodeTokensSumsEachProvidersModels(t *testing.T) {
	raw := []byte(`{"models":[{"model":{"id":"a","providerID":"opencode-go"},"tokens":{"input":10,"output":2,"reasoning":3,"cache":{"read":100,"write":4}}},` +
		`{"model":{"id":"b","providerID":"opencode-go"},"tokens":{"input":1,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}},` +
		`{"model":{"id":"c","providerID":"openrouter"},"tokens":{"input":7}}]}`)
	got, err := opencodeTokens(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := tokenUsage{InputTokens: 11, CacheCreationInputTokens: 4, CacheReadInputTokens: 100, OutputTokens: 5}
	if got["opencode-go"] != want || got["openrouter"].InputTokens != 7 {
		t.Fatalf("tokens %+v", got)
	}
	if opencodeDays(5*time.Hour) != 0 || opencodeDays(7*24*time.Hour) != 7 || opencodeDays(36*time.Hour) != 2 {
		t.Fatal("days")
	}
}

func TestOpencodeReportListsGoWithWindowsAndZenWithNone(t *testing.T) {
	var asked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.Header.Get("Authorization") != "Bearer go-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"rolling":{"status":"ok","percent":3,"resetsAt":"2026-11-18T09:13:31Z"}}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"opencode-go":{"type":"api","key":"go-key"},"opencode":{"type":"api","key":"zen"},"openrouter":{"type":"api","key":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	statsDone := make(chan struct{}, 1)
	o := &opencodeAccounts{auth: auth, usageURL: server.URL, client: server.Client(), now: func() time.Time { return blameNow.Add(time.Duration(clock.Load())) },
		stats: func(days int) ([]byte, error) {
			defer func() { statsDone <- struct{}{} }()
			if days != 7 {
				return nil, errors.New("wrong days")
			}
			return []byte(`{"models":[{"model":{"providerID":"opencode-go"},"tokens":{"output":1000}}]}`), nil
		}}
	first := o.report(7 * 24 * time.Hour)
	if len(first) != 2 || first[0].ID != "opencode-go" || first[1].ID != "opencode" {
		t.Fatalf("accounts %+v", first)
	}
	if first[0].Provider != opencodeProvider || len(first[0].Windows) != 1 || first[0].Windows[0].UsedPercent != 3 {
		t.Fatalf("go %+v", first[0])
	}
	if first[0].NoLimit || !first[1].NoLimit {
		t.Fatalf("only zen has no limit: %+v", first)
	}
	if len(first[1].Windows) != 0 {
		t.Fatalf("zen has no limit, so no window: %+v", first[1].Windows)
	}
	<-statsDone
	for running := true; running; {
		o.mu.Lock()
		running = o.tokens[7].running
		o.mu.Unlock()
	}
	clock.Store(int64(time.Minute))
	second := o.report(7 * 24 * time.Hour)
	if second[0].Weighted != 5000 || second[0].Rest != 5000 || second[1].Weighted != 0 {
		t.Fatalf("tokens not used once read: %+v", second)
	}
	if asked.Load() != 1 {
		t.Fatalf("usage read %d times inside its interval", asked.Load())
	}
}
