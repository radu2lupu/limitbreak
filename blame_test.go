package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func util(v float64) *float64 { return &v }

var blameNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func rec(minutesAgo int, account, session string, five, seven *float64, u tokenUsage) requestRecord {
	return requestRecord{Time: blameNow.Add(-time.Duration(minutesAgo) * time.Minute), Provider: "claude", Client: "shared",
		Account: account, Session: session, FiveHour: five, SevenDay: seven, tokenUsage: u}
}

func noTranscripts(string) transcriptInfo { return transcriptInfo{Project: "?", Title: "?"} }

func TestUtilizationRisesGoToTheSessionThatSawThemAndAResetIsNobodys(t *testing.T) {
	records := []requestRecord{
		rec(50, "work", "a", util(0.10), util(0.30), tokenUsage{}),
		rec(40, "work", "a", util(0.12), util(0.30), tokenUsage{}), // a: +2
		rec(35, "home", "b", util(0.90), nil, tokenUsage{}),        // another account: its own window
		rec(30, "work", "b", util(0.15), util(0.31), tokenUsage{}), // b: +3, 7d +1
		rec(25, "work", "b", nil, nil, tokenUsage{}),               // says nothing, changes nothing
		rec(20, "work", "a", util(0.01), util(0.31), tokenUsage{}), // reset: nobody's
		rec(10, "work", "b", util(0.04), util(0.32), tokenUsage{}), // b: +3, 7d +1
	}
	rows := blame(records, blameOptions{since: 24 * time.Hour, by: "session"}, blameNow, noTranscripts)
	got := map[string][2]float64{}
	for _, r := range rows {
		got[r.Key] = [2]float64{r.FiveHourRise, r.SevenDayRise}
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if !near(got["a"][0], 2) || !near(got["a"][1], 0) || !near(got["b"][0], 6) || !near(got["b"][1], 2) {
		t.Fatal(got)
	}
}

func TestBlameWeighsTokensAndRanksByTheWeightedTotal(t *testing.T) {
	records := []requestRecord{
		rec(30, "work", "reader", nil, nil, tokenUsage{CacheReadInputTokens: 1_000_000}),                                         // 100k
		rec(20, "work", "writer", nil, nil, tokenUsage{InputTokens: 1000, CacheCreationInputTokens: 8000, OutputTokens: 20_000}), // 111k
		rec(10, "work", "reader", nil, nil, tokenUsage{OutputTokens: 1000}),                                                      // +5k
	}
	rows := blame(records, blameOptions{since: time.Hour, by: "session"}, blameNow, noTranscripts)
	if len(rows) != 2 || rows[0].Key != "writer" || rows[0].Weighted != 111_000 || rows[1].Weighted != 105_000 || rows[1].Requests != 2 {
		t.Fatalf("%+v", rows)
	}
	if weighted(tokenUsage{1, 1, 1, 1}) != 1+1.25+0.1+5 {
		t.Fatal("weights")
	}
}

func TestBlameSinceAndAccountFilterButRisesStillSeeEarlierRecords(t *testing.T) {
	records := []requestRecord{
		rec(300, "work", "old", util(0.10), nil, tokenUsage{OutputTokens: 10}),
		rec(30, "work", "new", util(0.20), nil, tokenUsage{OutputTokens: 10}),
		rec(20, "own", "mine", nil, nil, tokenUsage{OutputTokens: 10}),
	}
	rows := blame(records, blameOptions{since: time.Hour, by: "session"}, blameNow, noTranscripts)
	if len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
	for _, r := range rows {
		if r.Key == "new" && r.FiveHourRise < 9.99 {
			t.Fatal("the rise from the record before the period is lost", r)
		}
	}
	rows = blame(records, blameOptions{since: time.Hour, account: "own", by: "account"}, blameNow, noTranscripts)
	if len(rows) != 1 || rows[0].Key != "own" {
		t.Fatalf("%+v", rows)
	}
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionTranscriptFindsProjectAndTitleUnderAnyClaudeDir(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-x-Code-app", "s-prompt.jsonl"),
		`{"type":"session-owner","sessionId":"s-prompt"}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"injected"},"cwd":"`+home+`/Code/app"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"  fix the\nlogin   bug please and also make sure that the tests are all green before you stop"}]},"cwd":"`+home+`/Code/app"}`)
	writeTranscript(t, filepath.Join(home, ".claude-work", "projects", "-tmp", "s-title.jsonl"),
		`{"type":"user","message":{"role":"user","content":"hi"},"cwd":"/tmp"}`,
		`{"type":"ai-title","aiTitle":"First AI title","sessionId":"s-title"}`,
		`{"type":"ai-title","aiTitle":"Better AI title","sessionId":"s-title"}`)
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-tmp", "s-custom.jsonl"),
		`{"type":"user","message":{"role":"user","content":"hi"},"cwd":"/tmp"}`,
		`{"type":"custom-title","customTitle":"Named by me","sessionId":"s-custom"}`,
		`{"type":"ai-title","aiTitle":"AI title","sessionId":"s-custom"}`)

	got := sessionTranscript(home, "s-prompt")
	if got.Project != "~/Code/app" || got.Title != "fix the login bug please and also make sure that the tests …" {
		t.Fatalf("%+v", got)
	}
	if got := sessionTranscript(home, "s-title"); got.Project != "/tmp" || got.Title != "Better AI title" {
		t.Fatalf("%+v", got)
	}
	if got := sessionTranscript(home, "s-custom"); got.Title != "Named by me" {
		t.Fatalf("%+v", got)
	}
	writeTranscript(t, filepath.Join(home, "Library", "Application Support", "Hi, computer", "mesh-acp", "member_agent_x", "home", "revisions", "12", ".claude", "projects", "-x", "s-mesh.jsonl"),
		`{"type":"user","message":{"role":"user","content":"agent work"},"cwd":"/srv/agent"}`)
	if got := sessionTranscript(home, "s-mesh"); got.Project != "/srv/agent" || got.Title != "agent work" {
		t.Fatalf("%+v", got)
	}
	if got := sessionTranscript(home, "nope"); got != (transcriptInfo{"?", "?"}) {
		t.Fatalf("%+v", got)
	}
	if got := sessionTranscript(home, "../*"); got != (transcriptInfo{"?", "?"}) {
		t.Fatalf("%+v", got)
	}

	// By project, sessions in one cwd add up.
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-x-Code-app", "s-two.jsonl"),
		`{"type":"user","message":{"role":"user","content":"more"},"cwd":"`+home+`/Code/app"}`)
	records := []requestRecord{
		rec(5, "work", "s-prompt", nil, nil, tokenUsage{OutputTokens: 10}),
		rec(4, "work", "s-two", nil, nil, tokenUsage{OutputTokens: 10}),
		rec(3, "work", "s-title", nil, nil, tokenUsage{OutputTokens: 1}),
	}
	rows := blame(records, blameOptions{since: time.Hour, by: "project"}, blameNow, func(id string) transcriptInfo { return sessionTranscript(home, id) })
	if len(rows) != 2 || rows[0].Key != "~/Code/app" || rows[0].Requests != 2 || rows[1].Key != "/tmp" {
		t.Fatalf("%+v", rows)
	}
}

func TestBlameCommandPrintsATableAndJSON(t *testing.T) {
	home := t.TempDir()
	l := newRequestLog(home)
	now := time.Now().UTC()
	l.append(requestRecord{Time: now.Add(-time.Minute), Provider: "claude", Client: "shared", Account: "work", Session: "abcdef12-3456",
		tokenUsage: tokenUsage{InputTokens: 1200, CacheReadInputTokens: 3_400_000, OutputTokens: 340_000}})
	l.append(requestRecord{Time: now.Add(-48 * time.Hour), Provider: "claude", Account: "work", Session: "old"})
	var out bytes.Buffer
	if err := blameCommand(home, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SESSION") || !strings.Contains(out.String(), "abcdef12") || !strings.Contains(out.String(), "3.4M") ||
		!strings.Contains(out.String(), "340k") || strings.Contains(out.String(), "old") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := blameCommand(home, []string{"--since", "3d", "--by", "account", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var rows []blameRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Key != "work" || rows[0].Requests != 2 {
		t.Fatal(err, out.String())
	}
	if err := blameCommand(home, []string{"--by", "colour"}, &out); err == nil {
		t.Fatal("bad --by accepted")
	}
}

func TestHumanize(t *testing.T) {
	for n, want := range map[float64]string{950: "950", 9500: "9.5k", 340_000: "340k", 1_200_000: "1.2M", 2_000_000: "2M", 3.1e9: "3.1B"} {
		if got := humanize(n); got != want {
			t.Fatal(n, got)
		}
	}
}

func TestBlameShowsEachRowsModelsAndGroupsByModel(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	rec := func(session, model string, out int64) requestRecord {
		r := requestRecord{Time: now.Add(-time.Hour), Account: "work", Session: session, Model: model}
		r.OutputTokens = out
		return r
	}
	records := []requestRecord{rec("s1", "claude-opus-5-5", 900), rec("s1", "claude-haiku-5-5", 100), rec("s2", "claude-opus-5-5", 10)}
	none := func(string) transcriptInfo { return transcriptInfo{"?", "?"} }
	rows := blame(records, blameOptions{by: "session"}, now, none)
	if got := modelMix(rows[0].Models); rows[0].Key != "s1" || got != "opus-5-5 90%, haiku-5-5 10%" {
		t.Fatal(rows[0].Key, got)
	}
	if got := modelMix(rows[1].Models); got != "opus-5-5" {
		t.Fatal(got)
	}
	rows = blame(records, blameOptions{by: "model"}, now, none)
	if len(rows) != 2 || rows[0].Key != "claude-opus-5-5" || rows[0].Requests != 2 || rows[1].Key != "claude-haiku-5-5" {
		t.Fatalf("%+v", rows)
	}
}
