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

// markSynced records an account as kept in step with Coffer, as a sync pass would.
func markSynced(t *testing.T, home, provider, name string) {
	t.Helper()
	st := readSyncState(home)
	st[loginPath(provider, name)] = syncEntry{Version: 1}
	if err := writeSyncState(home, st); err != nil {
		t.Fatal(err)
	}
}

func TestThePlaceholderIsNotAnAccountAndIsNeverWrittenOverARealLogin(t *testing.T) {
	home := t.TempDir()
	agent := filepath.Join(home, "agent-home")
	var out bytes.Buffer
	if err := grokPlaceholderCommand([]string{agent}, &out); err != nil {
		t.Fatal(err)
	}
	if err := grokPlaceholderCommand([]string{agent}, &out); err != nil { // idempotent
		t.Fatal(err)
	}
	l, ok := readGrokLogin(agent)
	var claims struct {
		Exp float64 `json:"exp"`
		Sub string  `json:"sub"`
	}
	if !ok || !l.placeholder || l.refreshable || !jwtClaims(l.token, &claims) || claims.Exp < 4e9 || claims.Sub != "limitbreak" {
		t.Fatalf("%+v %+v", l, claims)
	}
	if info, _ := os.Stat(filepath.Join(agent, "auth.json")); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}

	// As ~/.grok it is no login: nothing listed, and the host counts as having none.
	g := newGrokAccounts(home, "", "http://127.0.0.1:1")
	os.Rename(agent, filepath.Join(home, ".grok"))
	if len(g.list()) != 0 || !g.loginless() {
		t.Fatalf("%+v", g.list())
	}

	real := filepath.Join(home, "real")
	writeGrokLogin(t, real, "me", time.Now().Add(time.Hour))
	if grokPlaceholderCommand([]string{real}, &out) == nil {
		t.Fatal("overwrote a real login")
	}
	if l, _ := readGrokLogin(real); l.placeholder {
		t.Fatal("the real login is gone")
	}
}

func TestALoginlessMacPutsEveryGrokRequestOnTheSharedAccount(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".grok-shared")
	os.MkdirAll(shared, 0o700)
	record := grokAuthJSON("owner", time.Now().Add(5*time.Hour))
	os.WriteFile(filepath.Join(shared, "auth.json"), record, 0o600)
	markSynced(t, home, "grok", "shared")
	own, _ := parseGrokLogin(record)
	// The agent's grok runs on the placeholder; its requests carry it.
	placeholder, _ := parseGrokLogin(grokPlaceholderAuth())

	rig := startGrok(t, home, nil)
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/models"}, {"POST", "/v1/storage"}, {"GET", "/v1/settings"}, {"POST", "/v1/traces"},
		{"POST", "/v1/sessions"}, {"POST", "/v1/responses"},
	} {
		if resp, _ := sendGrok(t, rig.server.URL, "agent_a", req.method, req.path, placeholder.token); resp.StatusCode != 200 {
			t.Fatal(req, resp.StatusCode)
		}
	}
	if len(rig.up.seen) != 6 {
		t.Fatal(len(rig.up.seen))
	}
	for _, seen := range rig.up.seen {
		if seen.auth != "Bearer "+own.token || seen.auth == "Bearer "+placeholder.token || seen.userID != "user-owner" || seen.grokUserID != "user-owner" ||
			seen.email != "owner@example.com" || seen.tokenAuth != "xai-grok-cli" {
			t.Fatalf("a request kept the placeholder or the wrong login: %+v", seen)
		}
	}
	if len(rig.refreshes) != 0 {
		t.Fatal("a login with hours left was refreshed", rig.refreshes)
	}

	// Status shows it: synced from Coffer, with when its token ends, and no token.
	rows := getStatus(t, rig.server.URL)
	var found *accountStatus
	for i, row := range rows {
		if row.Provider == "grok" {
			found = &rows[i]
		}
	}
	raw, _ := json.Marshal(rows)
	if found == nil || found.ID != "shared" || !found.Shared || found.TokenEnds == nil || *found.TokenEnds != own.expires.Unix() ||
		strings.Contains(string(raw), own.token) || !strings.Contains(string(raw), `"shared":true`) {
		t.Fatalf("%s", raw)
	}
}

func TestAMacWithItsOwnGrokLoginKeepsPuttingOnlyModelTurnsOnAnAccount(t *testing.T) {
	home, own, _ := grokHome(t)
	shared := filepath.Join(home, ".grok-shared")
	os.MkdirAll(shared, 0o700)
	os.WriteFile(filepath.Join(shared, "auth.json"), grokAuthJSON("owner", time.Now().Add(5*time.Hour)), 0o600)
	markSynced(t, home, "grok", "shared")
	rig := startGrok(t, home, nil)
	if g := rig.grok; g.loginless() || !g.spends("/v1/responses") || g.spends("/v1/models") || g.logs("/v1/models") || !g.logs("/v1/responses") {
		t.Fatal("a Mac with a login of its own must not widen")
	}
	sendGrok(t, rig.server.URL, "agent_a", "GET", "/v1/models", own)
	if rig.up.seen[0].auth != "Bearer "+own {
		t.Fatalf("%+v", rig.up.seen[0])
	}
}

func TestAnEndedLoginWithNoRefreshTokenIsNotRefreshedAndTheRequestGoesOnAsTheClientMadeIt(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".grok-shared")
	os.MkdirAll(shared, 0o700)
	// A login with no refresh token whose token ended: nothing here can renew it.
	noRefresh, err := dropRefreshToken(grokAuthJSON("owner", time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(shared, "auth.json"), noRefresh, 0o600)
	markSynced(t, home, "grok", "shared")
	placeholder, _ := parseGrokLogin(grokPlaceholderAuth())
	rig := startGrok(t, home, nil)
	sendGrok(t, rig.server.URL, "agent_a", "GET", "/v1/models", placeholder.token)
	if len(rig.refreshes) != 0 || len(rig.up.seen) != 1 || rig.up.seen[0].auth != "Bearer "+placeholder.token {
		t.Fatal(rig.refreshes, rig.up.seen)
	}
	rows := getStatus(t, rig.server.URL)
	for _, row := range rows {
		if row.Provider == "grok" && (!row.Shared || row.TokenEnds == nil || *row.TokenEnds > time.Now().Unix()) {
			t.Fatalf("%+v", row)
		}
	}
}

// dropRefreshToken is a Grok auth.json without its refresh token.
func dropRefreshToken(raw []byte) ([]byte, error) {
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	for _, entry := range doc {
		delete(entry, "refresh_token")
	}
	out, err := json.Marshal(doc)
	return out, err
}
