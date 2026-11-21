package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sharedGrokAuth is what an owner sends: its login without the refresh token.
func sharedGrokAuth(t *testing.T, name string, expires time.Time) []byte {
	t.Helper()
	record, _, err := tokenOnlyGrok(grokAuthJSON(name, expires))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestTokenOnlyCopyDropsTheRefreshTokenAndKeepsTheLongestLogin(t *testing.T) {
	soon, later := time.Now().Add(time.Hour), time.Now().Add(6*time.Hour)
	var doc map[string]any
	json.Unmarshal(grokAuthJSON("old", soon), &doc)
	var newer map[string]any
	json.Unmarshal(grokAuthJSON("new", later), &newer)
	for k, v := range newer {
		doc[k] = v
	}
	both, _ := json.Marshal(doc)
	record, l, err := tokenOnlyGrok(both)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "refresh_token") || strings.Contains(string(record), `"r-new"`) || strings.Contains(string(record), `"r-old"`) {
		t.Fatalf("the copy carries a refresh token: %s", record)
	}
	back, ok := parseGrokLogin(record)
	if !ok || back.refreshable || back.userID != "user-new" || back.teamID != "team-new" || back.email != "new@example.com" || back.token != l.token {
		t.Fatalf("%+v", back)
	}
	if _, _, err := tokenOnlyGrok([]byte(`{}`)); err == nil {
		t.Fatal("an empty auth.json has nothing to share")
	}
	if _, _, err := tokenOnlyGrok(grokPlaceholderAuth()); err == nil {
		t.Fatal("the placeholder is nobody's login to share")
	}
}

// sharerRig is an owner Mac (home with a default Grok login) and the pushes it sends.
type sharerRig struct {
	home    string
	grok    *grokAccounts
	sharer  *grokSharer
	sent    [][]byte
	hosts   []string
	names   []string
	failing bool
	refresh int
	clock   time.Time
}

func newSharerRig(t *testing.T, expires time.Time) *sharerRig {
	r := &sharerRig{home: t.TempDir(), clock: time.Now()}
	writeGrokLogin(t, filepath.Join(r.home, ".grok"), "me", expires)
	r.grok = newGrokAccounts(r.home, "", "http://127.0.0.1:1")
	r.grok.now = func() time.Time { return r.clock }
	r.grok.refresh = func(dir string) error {
		r.refresh++
		writeGrokLogin(t, dir, "me", r.clock.Add(6*time.Hour))
		return nil
	}
	r.sharer = newGrokSharer(r.grok, r.home)
	r.sharer.send = func(host, name string, record []byte) (string, error) {
		if r.failing {
			return "ssh: connect to host mini port 22: Operation timed out", errors.New("exit status 255")
		}
		r.hosts, r.names, r.sent = append(r.hosts, host), append(r.names, name), append(r.sent, append([]byte(nil), record...))
		return "ok", nil
	}
	return r
}

func TestShareSendsATokenOnlyCopyNeverTheRefreshToken(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	var out bytes.Buffer
	if err := shareGrok(r.sharer, []string{"mini"}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if len(r.sent) != 1 || r.hosts[0] != "mini" || r.names[0] != grokSharedName {
		t.Fatalf("%v %v", r.hosts, r.names)
	}
	own, _ := readGrokLogin(filepath.Join(r.home, ".grok"))
	got, ok := parseGrokLogin(r.sent[0])
	if !ok || got.token != own.token || got.refreshable || strings.Contains(string(r.sent[0]), `"r-me"`) || strings.Contains(string(r.sent[0]), "refresh_token") {
		t.Fatalf("what went over the wire: %s", r.sent[0])
	}
	if strings.Contains(out.String(), own.token) || strings.Contains(out.String(), `"r-me"`) {
		t.Fatal("a token in the output")
	}
	shares := readShares(r.home)
	if len(shares) != 1 || shares[0].Pushed != tokenFingerprint(own.token) || strings.Contains(string(mustRead(t, sharesPath(r.home))), own.token) {
		t.Fatalf("%+v", shares)
	}
	if info, _ := os.Stat(sharesPath(r.home)); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAStaleCopyIsRefreshedFromTheOwnerAndPushedAgain(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	if err := shareGrok(r.sharer, []string{"mini"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r.sharer.tick() // nothing changed: nothing sent, no refresh
	if len(r.sent) != 1 || r.refresh != 0 {
		t.Fatal(len(r.sent), r.refresh)
	}

	// Three hours on the token has under grokShareRefreshWithin left: the owner refreshes through grok
	// and the copy that arrives is the new token.
	r.clock = r.clock.Add(3*time.Hour + time.Minute)
	r.sharer.tick()
	if r.refresh != 1 || len(r.sent) != 2 {
		t.Fatal(r.refresh, len(r.sent))
	}
	first, _ := parseGrokLogin(r.sent[0])
	second, _ := parseGrokLogin(r.sent[1])
	if second.token == first.token || !second.expires.After(r.clock.Add(5*time.Hour)) || second.refreshable {
		t.Fatalf("%+v", second)
	}
	r.sharer.tick() // delivered: quiet again
	if len(r.sent) != 2 || r.refresh != 1 {
		t.Fatal(len(r.sent), r.refresh)
	}
}

func TestAFailedPushIsRetriedOnTheNextTickAndKeptAsAShare(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	r.failing = true
	err := shareGrok(r.sharer, []string{"mini"}, &bytes.Buffer{})
	if err == nil || len(readShares(r.home)) != 1 || !strings.Contains(readShares(r.home)[0].Error, "timed out") {
		t.Fatal(err, readShares(r.home))
	}
	r.sharer.tick() // still down
	if len(r.sent) != 0 {
		t.Fatal(len(r.sent))
	}
	r.failing = false
	r.sharer.tick()
	if len(r.sent) != 1 || readShares(r.home)[0].Error != "" || readShares(r.home)[0].Pushed == "" {
		t.Fatal(len(r.sent), readShares(r.home))
	}
}

func TestAnOwnerWhoCannotRefreshSendsNothingOnceTheTokenEnded(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	r.grok.refresh = func(string) error { return errors.New("signed out") }
	if err := shareGrok(r.sharer, []string{"mini"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(7 * time.Hour)
	r.sharer.tick()
	if len(r.sent) != 1 || !strings.Contains(readShares(r.home)[0].Error, "ended") {
		t.Fatal(len(r.sent), readShares(r.home))
	}
}

func TestOnlyALoginWithItsOwnRefreshTokenIsShared(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	// A named account that is itself a shared copy is not shared onward.
	if err := os.MkdirAll(filepath.Join(r.home, ".grok-copy"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.home, ".grok-copy", "auth.json"), sharedGrokAuth(t, "copy", time.Now().Add(5*time.Hour)), 0o600)
	if err := shareGrok(r.sharer, []string{"mini"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(r.sent) != 1 || r.names[0] != grokSharedName {
		t.Fatal(r.names)
	}
	if shareGrok(r.sharer, []string{"mini", "copy"}, &bytes.Buffer{}) == nil {
		t.Fatal("a copy cannot be shared onward")
	}
}

func TestUnshareStopsTheKeepFresh(t *testing.T) {
	r := newSharerRig(t, time.Now().Add(6*time.Hour))
	shareGrok(r.sharer, []string{"mini"}, &bytes.Buffer{})
	if err := unshareGrok(r.home, []string{"mini"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r.clock = r.clock.Add(4 * time.Hour)
	r.sharer.tick()
	if len(readShares(r.home)) != 0 || len(r.sent) != 1 || r.refresh != 0 {
		t.Fatal(readShares(r.home), len(r.sent), r.refresh)
	}
}

func TestImportGrokStoresATokenOnlyLoginAndRefusesTheRest(t *testing.T) {
	home := t.TempDir()
	g := newGrokAccounts(home, "", "http://127.0.0.1:1")
	dir := filepath.Join(home, ".grok-shared")
	record := sharedGrokAuth(t, "owner", time.Now().Add(5*time.Hour))
	var out bytes.Buffer
	if err := importGrok(g, []string{"shared"}, bytes.NewReader(record), &out); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "auth.json")); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	l, ok := readGrokLogin(dir)
	if !ok || l.refreshable || strings.Contains(out.String(), l.token) {
		t.Fatalf("%+v %s", l, out.String())
	}

	// A newer token replaces it; an older or equal one does not.
	newer := sharedGrokAuth(t, "owner", time.Now().Add(6*time.Hour))
	if err := importGrok(g, []string{"shared"}, bytes.NewReader(newer), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readGrokLogin(dir); got.token == l.token {
		t.Fatal("the newer token was not stored")
	}
	if err := importGrok(g, []string{"shared"}, bytes.NewReader(record), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readGrokLogin(dir); got.token == l.token {
		t.Fatal("an older token replaced a newer one")
	}

	refuse := func(name string, raw []byte, args ...string) {
		t.Helper()
		if len(args) == 0 {
			args = []string{"shared2"}
		}
		if err := importGrok(g, args, bytes.NewReader(raw), &bytes.Buffer{}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if _, err := os.Stat(filepath.Join(home, ".grok-"+args[0])); err == nil {
			t.Fatalf("%s left a dir", name)
		}
	}
	refuse("a full login with its refresh token", grokAuthJSON("owner", time.Now().Add(5*time.Hour)))
	refuse("the placeholder", grokPlaceholderAuth())
	refuse("an ended token", sharedGrokAuth(t, "owner", time.Now().Add(-time.Hour)))
	refuse("garbage", []byte("nope"))
	refuse("the default name", record, "default")
	refuse("a path", record, "../x")

	// A login of its own, or another tool's dir, is never overwritten.
	writeGrokLogin(t, filepath.Join(home, ".grok-mine"), "mine", time.Now().Add(time.Hour))
	if importGrok(g, []string{"mine"}, bytes.NewReader(newer), &bytes.Buffer{}) == nil {
		t.Fatal("overwrote a login of its own")
	}
	os.MkdirAll(filepath.Join(home, ".grok-other"), 0o700)
	os.WriteFile(filepath.Join(home, ".grok-other", "notes"), []byte("x"), 0o600)
	if importGrok(g, []string{"other"}, bytes.NewReader(newer), &bytes.Buffer{}) == nil {
		t.Fatal("claimed another tool's dir")
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
	record := sharedGrokAuth(t, "owner", time.Now().Add(5*time.Hour))
	os.WriteFile(filepath.Join(shared, "auth.json"), record, 0o600)
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
		t.Fatal("a shared login was refreshed here", rig.refreshes)
	}

	// Status shows it: shared, with when its token ends, and no token.
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
	os.WriteFile(filepath.Join(shared, "auth.json"), sharedGrokAuth(t, "owner", time.Now().Add(5*time.Hour)), 0o600)
	rig := startGrok(t, home, nil)
	if g := rig.grok; g.loginless() || !g.spends("/v1/responses") || g.spends("/v1/models") || g.logs("/v1/models") || !g.logs("/v1/responses") {
		t.Fatal("a Mac with a login of its own must not widen")
	}
	sendGrok(t, rig.server.URL, "agent_a", "GET", "/v1/models", own)
	if rig.up.seen[0].auth != "Bearer "+own {
		t.Fatalf("%+v", rig.up.seen[0])
	}
}

func TestAnEndedSharedLoginIsNotRefreshedAndTheRequestGoesOnAsTheClientMadeIt(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".grok-shared")
	os.MkdirAll(shared, 0o700)
	// A copy whose token ended: its owner has not pushed since (asleep, or off the network).
	os.WriteFile(filepath.Join(shared, "auth.json"), sharedGrokAuth(t, "owner", time.Now().Add(-time.Minute)), 0o600)
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
