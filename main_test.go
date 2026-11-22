package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const far = 4_000_000_000

func makeHome(t *testing.T, logins map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, token := range logins {
		dir, claudeJSON := filepath.Join(home, ".claude-"+name), ""
		if name == "default" {
			dir, claudeJSON = filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
		} else {
			claudeJSON = filepath.Join(dir, ".claude.json")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSON(t, filepath.Join(dir, ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "expiresAt": float64(far) * 1000}})
		writeJSON(t, claudeJSON, map[string]any{"oauthAccount": map[string]any{"emailAddress": name + "@example.com"}})
	}
	return home
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// upstream stands in for Anthropic: it records the tokens it was asked with and rejects those in spent.
type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []string
	paths []string
	// unauthorized tokens are answered 401, like a revoked or expired login.
	unauthorized []string
}

func newUpstream(t *testing.T, spent ...string) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		token := r.Header.Get("Authorization")
		u.mu.Lock()
		u.seen = append(u.seen, token)
		u.paths = append(u.paths, r.URL.RequestURI())
		u.mu.Unlock()
		for _, s := range u.unauthorized {
			if token == "Bearer "+s {
				w.WriteHeader(401)
				w.Write([]byte("unauthorized"))
				return
			}
		}
		for _, s := range spent {
			if token == "Bearer "+s {
				w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
				w.Header().Set("anthropic-ratelimit-unified-reset", "4000000000")
				w.WriteHeader(429)
				w.Write([]byte("spent"))
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: a\n\ndata: b\n\n"))
	}))
	t.Cleanup(u.Close)
	return u
}

func start(t *testing.T, home string, up *upstream) (*httptest.Server, *pool) {
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	claude := newPool(acc, up.URL)
	server := httptest.NewServer(newProxy(claude))
	t.Cleanup(server.Close)
	return server, claude
}

func post(t *testing.T, base, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer claude-own-login")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestOnlyARejectedUnifiedStatusIsASpentWindow(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-reset", "1800003600")
	if spent, reset := spentReset(429, h); !spent || reset != time.Unix(1800003600, 0) {
		t.Fatal(spent, reset)
	}
	h.Del("anthropic-ratelimit-unified-reset")
	if spent, reset := spentReset(429, h); !spent || !reset.IsZero() {
		t.Fatal(spent, reset)
	}
	if spent, _ := spentReset(429, http.Header{}); spent {
		t.Fatal("a plain rate limit is not a spent window")
	}
	h.Set("anthropic-ratelimit-unified-reset", "1")
	if spent, _ := spentReset(200, h); spent {
		t.Fatal("only a 429 is")
	}
}

func TestRouteSplitsAClientFromTheUpstreamPath(t *testing.T) {
	cases := []struct{ in, client, path string }{
		{"/v1/messages?beta=true", "shared", "/v1/messages?beta=true"},
		{"/api/oauth/usage", "shared", "/api/oauth/usage"},
		{"/_c/member_1/v1/messages", "member_1", "/v1/messages"},
	}
	for _, c := range cases {
		client, path, ok := route(c.in)
		if !ok || client != c.client || path != c.path {
			t.Fatal(c, client, path, ok)
		}
	}
	if _, _, ok := route("/_c/x"); ok {
		t.Fatal("a client with no path")
	}
}

func TestKeychainServiceMatchesTheVerifiedHashes(t *testing.T) {
	want := map[string]string{
		"":                                   "Claude Code-credentials",
		"/home/user/.claude-work":            "Claude Code-credentials-174d0719",
		"/home/user/.claude-science-account": "Claude Code-credentials-37a79caf",
	}
	for dir, service := range want {
		if got := keychainService(dir); got != service {
			t.Fatal(dir, got)
		}
	}
}

func TestNextAccountSkipsSpentAndIsNilWhenAllAreSpent(t *testing.T) {
	order := []account{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	now := time.Unix(50, 0)
	if got := nextAccount(order, "a", map[string]time.Time{"b": time.Unix(100, 0)}, now); got.ID != "c" {
		t.Fatal(got)
	}
	if got := nextAccount(order, "a", map[string]time.Time{"b": time.Unix(10, 0)}, now); got.ID != "b" {
		t.Fatal(got)
	}
	all := map[string]time.Time{"a": time.Unix(100, 0), "b": time.Unix(100, 0), "c": time.Unix(100, 0)}
	if nextAccount(order, "a", all, now) != nil {
		t.Fatal("all spent")
	}
}

func TestAccountsAreTheDefaultAndEveryDotClaudeDirWithALogin(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	os.MkdirAll(filepath.Join(home, ".claude-empty"), 0o755)
	list := newClaudeAccounts(home, credentialsFile, "").list()
	if len(list) != 2 || list[0].ID != "default" || list[1].ID != "work" || list[1].Email != "work@example.com" {
		t.Fatal(list)
	}
}

func TestMovesToTheNextAccountWhenOneIsSpentWithoutTheClientNoticing(t *testing.T) {
	up := newUpstream(t, "t-default")
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	if status, body := post(t, server.URL, "/v1/messages"); status != 200 || body != "event: a\n\ndata: b\n\n" {
		t.Fatal(status, body)
	}
	post(t, server.URL, "/v1/messages")
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-work,Bearer t-work" {
		t.Fatal(up.seen)
	}
}

func TestAllSpentHandsTheLimitBack(t *testing.T) {
	up := newUpstream(t, "t-default", "t-work")
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	if status, body := post(t, server.URL, "/v1/messages"); status != 429 || body != "spent" {
		t.Fatal(status, body)
	}
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-work" {
		t.Fatal(up.seen)
	}
}

func TestClientsKeepTheirOwnActiveAccountAndThePrefixIsStripped(t *testing.T) {
	up := newUpstream(t, "t-default")
	server, acc := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	post(t, server.URL, "/_c/agent_a/v1/messages")
	if acc.active["agent_a"] != "work" {
		t.Fatal(acc.active)
	}
	if up.paths[len(up.paths)-1] != "/v1/messages" {
		t.Fatal(up.paths)
	}
}

func TestAnExpiredTokenIsNotUsed(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{"accessToken": "old", "expiresAt": 1000}})
	up := newUpstream(t)
	server, _ := start(t, home, up)
	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 {
		t.Fatal(status)
	}
	if strings.Join(up.seen, ",") != "Bearer t-work" {
		t.Fatal(up.seen)
	}
}

func TestStatusListsAccountsWithoutTokens(t *testing.T) {
	up := newUpstream(t)
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default"}), up)
	resp, err := http.Get(server.URL + "/_status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "default@example.com") || strings.Contains(string(raw), "t-default") {
		t.Fatal(string(raw))
	}
}

func TestOnlyMessagesAreSentOnAnAccountEverythingElseKeepsClaudesOwnLogin(t *testing.T) {
	up := newUpstream(t)
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default"}), up)
	post(t, server.URL, "/api/oauth/profile")
	post(t, server.URL, "/v1/messages?beta=true")
	if strings.Join(up.seen, ",") != "Bearer claude-own-login,Bearer t-default" {
		t.Fatal(up.seen)
	}
}

func TestWithoutAnyUsableAccountTheRequestGoesOnWithClaudesOwnLogin(t *testing.T) {
	up := newUpstream(t)
	server, _ := start(t, t.TempDir(), up)
	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 {
		t.Fatal(status)
	}
	if strings.Join(up.seen, ",") != "Bearer claude-own-login" {
		t.Fatal(up.seen)
	}
}

func TestAnAccountWhoseTokenIsRejectedIsSkippedWithoutClaudeSeeingThe401(t *testing.T) {
	up := newUpstream(t)
	up.unauthorized = []string{"t-default"}
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	if status, body := post(t, server.URL, "/v1/messages"); status != 200 || body != "event: a\n\ndata: b\n\n" {
		t.Fatal(status, body)
	}
	post(t, server.URL, "/v1/messages")
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-default,Bearer t-work,Bearer t-work" {
		t.Fatal(up.seen)
	}
}

func TestEveryTokenRejectedFallsBackToClaudesOwnLogin(t *testing.T) {
	up := newUpstream(t)
	up.unauthorized = []string{"t-default", "t-work"}
	server, _ := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 {
		t.Fatal(status)
	}
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-default,Bearer t-work,Bearer t-work,Bearer claude-own-login" {
		t.Fatal(up.seen)
	}
}

func put(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func getStatus(t *testing.T, base string) []accountStatus {
	t.Helper()
	resp, err := http.Get(base + "/_status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []accountStatus
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPutActiveSetsTheClientsAccountAndTheNextRequestUsesIt(t *testing.T) {
	up := newUpstream(t)
	server, acc := start(t, makeHome(t, map[string]string{"default": "t-default", "work": "t-work"}), up)
	if status, body := put(t, server.URL+"/_c/agent_a/_active", `{"id":"work"}`); status != 200 || !strings.Contains(body, `"activeFor":["agent_a"]`) {
		t.Fatal(status, body)
	}
	if acc.active["agent_a"] != "work" {
		t.Fatal(acc.active)
	}
	post(t, server.URL, "/_c/agent_a/v1/messages")
	post(t, server.URL, "/_c/agent_b/v1/messages")
	if strings.Join(up.seen, ",") != "Bearer t-work,Bearer t-default" {
		t.Fatal(up.seen)
	}
	for _, row := range getStatus(t, server.URL) {
		want := map[string]string{"default": "agent_b", "work": "agent_a"}[row.ID]
		if strings.Join(row.ActiveFor, ",") != want {
			t.Fatal(row.ID, row.ActiveFor)
		}
	}
}

func TestPutActiveRejectsAnUnknownAccountAndABadBody(t *testing.T) {
	up := newUpstream(t)
	server, acc := start(t, makeHome(t, map[string]string{"default": "t-default"}), up)
	if status, _ := put(t, server.URL+"/_c/agent_a/_active", `{"id":"nobody"}`); status != 404 {
		t.Fatal(status)
	}
	if status, _ := put(t, server.URL+"/_c/agent_a/_active", `not json`); status != 400 {
		t.Fatal(status)
	}
	if status, _ := post(t, server.URL, "/_c/agent_a/_active"); status != 405 {
		t.Fatal(status)
	}
	if _, ok := acc.active["agent_a"]; ok || len(up.seen) != 0 {
		t.Fatal(acc.active, up.seen)
	}
}

func TestStatusCarriesUsageAndAuthEndWithoutTokens(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	writeJSON(t, filepath.Join(home, ".claude-work", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "t-work", "expiresAt": float64(far) * 1000, "refreshTokenExpiresAt": float64(1_900_000_000_000)}})
	up := newUpstream(t)
	server, acc := start(t, home, up)
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1800000000")
	acc.provider.(*claudeAccounts).recordUsage(account{ID: "work", Email: "work@example.com"}, h)
	acc.provider.(*claudeAccounts).recordUsage(account{ID: "default", Email: "someone-else@example.com"}, h)

	resp, err := http.Get(server.URL + "/_status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "t-work") || strings.Contains(string(raw), "t-default") {
		t.Fatal("a token in the status", string(raw))
	}
	rows := getStatus(t, server.URL)
	byID := map[string]accountStatus{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	work, def := byID["work"], byID["default"]
	if work.AuthEnds == nil || *work.AuthEnds != 1_900_000_000 || def.AuthEnds != nil {
		t.Fatal(work.AuthEnds, def.AuthEnds)
	}
	if work.Usage["seven_day_utilization"].Value != 0.42 || work.Usage["five_hour_reset"].Value != 1800000000 || work.Usage["seven_day_utilization"].Observed.IsZero() {
		t.Fatal(work.Usage)
	}
	if def.Usage != nil {
		t.Fatal("usage recorded for another login is not this account's", def.Usage)
	}
}

func TestALockedKeychainCostsOneTimeoutThenIsSkippedForAWhile(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	now := time.Now()
	a := newClaudeAccounts(t.TempDir(), credentialsKeychain, "")
	a.now = func() time.Time { return now }

	start := time.Now()
	if _, ok := a.readKeychain([]string{"find-generic-password"}); ok {
		t.Fatal("a Keychain that never answers must not count as read")
	}
	if took := time.Since(start); took > keychainTimeout+2*time.Second {
		t.Fatalf("first read took %s, want about %s", took, keychainTimeout)
	}
	start = time.Now()
	if _, ok := a.readKeychain([]string{"find-generic-password"}); ok || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("second read must be skipped at once, took %s", time.Since(start))
	}
	now = now.Add(keychainBackoff + time.Second)
	if a.now().Before(a.keychainDownUntil) {
		t.Fatal("Keychain reads must resume after the backoff")
	}
}

// A running session stays on its account: making another account active moves only new sessions,
// and the running one moves when its account is spent or after it sat idle past the cache's life.
func TestARunningSessionKeepsItsAccountUntilItIsSpent(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work", "spare": "t-spare"})
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	p := newPool(acc, "http://127.0.0.1:1")
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	on := func(session string) string {
		t.Helper()
		got := p.current("c", session)
		if got == nil {
			t.Fatalf("%s: no account", session)
		}
		return got.ID
	}
	if got := on("running"); got != defaultID {
		t.Fatalf("running = %s", got)
	}
	if !p.setActive("c", "work") {
		t.Fatal("setActive")
	}
	if got := on("running"); got != defaultID {
		t.Fatalf("running moved to %s when another account was made active", got)
	}
	if got := on("new"); got != "work" {
		t.Fatalf("new = %s, want work", got)
	}
	if got := on(""); got != "work" {
		t.Fatalf("a request without a session = %s, want the client's active work", got)
	}
	p.spend("c", defaultID, now.Add(time.Hour))
	if got := on("running"); got == defaultID {
		t.Fatal("running stayed on its spent account")
	}
	running := on("running")
	p.setActive("c", "spare")
	now = now.Add(pinIdle - time.Minute)
	if got := on("running"); got != running {
		t.Fatalf("running moved to %s within the cache's life", got)
	}
	now = now.Add(pinIdle)
	if got := on("running"); got != "spare" {
		t.Fatalf("an idle session = %s, want the active spare", got)
	}
}

func TestPinsIdlePastTheCacheLifeAreDroppedOnceThereAreMany(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default"})
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	p := newPool(acc, "http://127.0.0.1:1")
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	for i := range maxPins {
		p.current("c", fmt.Sprint("old-", i))
	}
	now = now.Add(pinIdle)
	p.current("c", "fresh")
	if len(p.pins) != 1 {
		t.Fatalf("%d pins kept", len(p.pins))
	}
}

func TestEveryCacheBreakpointIsKeptForAnHour(t *testing.T) {
	cases := map[string]string{
		`{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`:                     `{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
		`{"m":[{"cache_control":{"type":"ephemeral","ttl":"5m"}},{"cache_control":{"type":"ephemeral"}}]}`: `{"m":[{"cache_control":{"type":"ephemeral","ttl":"1h"}},{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
		`{"m":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`:                                        `{"m":[{"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
		`{"m":[{"cache_control":{"type":"other"}}]}`:                                                       `{"m":[{"cache_control":{"type":"other"}}]}`,
		`{"model":"x","messages":[]}`:                                                                      `{"model":"x","messages":[]}`,
		`{"m":[{"text":"\"cache_control\":{ not json"}]}`:                                                  `{"m":[{"text":"\"cache_control\":{ not json"}]}`,
	}
	for in, want := range cases {
		if got := string(cacheOneHour([]byte(in))); got != want {
			t.Errorf("%s\n got %s\nwant %s", in, got, want)
		}
	}
}

// The proxy sends a Claude request with its breakpoints at an hour, on an account and on the
// client's own login alike; a request that spends nothing goes as it came.
func TestClaudeRequestsGoUpstreamWithTheHourCache(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	t.Cleanup(up.Close)
	home := makeHome(t, map[string]string{"default": "t-default"})
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	server := httptest.NewServer(newProxy(newPool(acc, up.URL)))
	t.Cleanup(server.Close)
	in := `{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`
	for _, path := range []string{"/v1/messages", "/api/other"} {
		resp, err := http.Post(server.URL+path, "application/json", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"ttl":"1h"`) || bodies[1] != in {
		t.Fatalf("%q", bodies)
	}
}

// The incident of 2026-11-22: another process's authorization dialog made every `security` call fail
// for a few seconds, and each account was benched for five minutes because it "had no token".
func TestAKeptTokenSurvivesAKeychainThatStopsAnswering(t *testing.T) {
	bin, marker, calls := t.TempDir(), filepath.Join(t.TempDir(), "up"), filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\necho x >> " + calls + "\n[ -e " + marker + " ] || { echo 'CSSM Exception -60008' >&2; exit 1; }\n" +
		`echo '{"token":"sk-ant-oat01-long","expiresAt":` + fmt.Sprint(int64(far)*1000) + `}'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := newClaudeAccounts(makeHome(t, map[string]string{"work": "t-work"}), credentialsKeychain, "")
	acc := accountNamed(t, a, "work")

	if got := a.token(acc); got != "sk-ant-oat01-long" {
		t.Fatal(got)
	}
	spawned := func() int { raw, _ := os.ReadFile(calls); return strings.Count(string(raw), "x") }
	before := spawned()
	if got := a.token(acc); got != "sk-ant-oat01-long" || spawned() != before {
		t.Fatalf("a kept token must be served without asking the Keychain: %q, %d new calls", got, spawned()-before)
	}
	// The Keychain fails and so does a dropped cache's re-read; the account falls back to its file token...
	os.Remove(marker)
	if !a.forgetToken(acc) {
		t.Fatal("nothing was kept")
	}
	if got := a.token(acc); got != "t-work" {
		t.Fatal("without a kept token the other source answers:", got)
	}
	// ...but a kept token is not lost to a failing read.
	a.keepToken(acc, "sk-ant-oat01-kept", a.now().Add(time.Hour))
	if got := a.token(acc); got != "sk-ant-oat01-kept" {
		t.Fatal(got)
	}
	// An ended one is not served.
	a.keepToken(acc, "sk-ant-oat01-ended", a.now().Add(30*time.Second))
	if got := a.token(acc); got == "sk-ant-oat01-ended" {
		t.Fatal("a token inside its margin must be read again")
	}
}

func TestA401DropsTheKeptTokenAndRetriesOnceWithAFreshRead(t *testing.T) {
	up := newUpstream(t)
	home := makeHome(t, map[string]string{"work": "t-old"})
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	server := httptest.NewServer(newProxy(newPool(acc, up.URL)))
	t.Cleanup(server.Close)

	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 {
		t.Fatal(status)
	}
	// The login was renewed since the token was kept: the old one is dead, the file holds a new one.
	up.unauthorized = []string{"t-old"}
	writeJSON(t, filepath.Join(home, ".claude-work", ".credentials.json"),
		map[string]any{"claudeAiOauth": map[string]any{"accessToken": "t-new", "expiresAt": float64(far) * 1000}})
	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 {
		t.Fatal(status)
	}
	if got := strings.Join(up.seen, ","); got != "Bearer t-old,Bearer t-old,Bearer t-new" {
		t.Fatal(got)
	}
	// The account was not benched: the next request goes on it, from the kept new token.
	if status, _ := post(t, server.URL, "/v1/messages"); status != 200 || up.seen[len(up.seen)-1] != "Bearer t-new" {
		t.Fatal(status, up.seen)
	}
}

// A login held only in the Keychain stays listed while the Keychain stops answering: dropping it left
// only the spent default, and requests passed through to that login's usage limit (2026-11-22).
func TestAKeychainOnlyLoginStaysListedWhileTheKeychainIsDown(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default"})
	if err := os.MkdirAll(filepath.Join(home, ".claude-work"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	answers := filepath.Join(bin, "answers")
	cred := `{"claudeAiOauth":{"accessToken":"t-work","expiresAt":99999999999999}}`
	script := "#!/bin/sh\nif [ -e " + answers + " ]; then\n  case \"$*\" in *'" + keychainService(filepath.Join(home, ".claude-work")) + "'*) echo '" + cred + "'; exit 0;; esac\n  exit 44\nfi\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(answers, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := newClaudeAccounts(home, credentialsKeychain, "")
	ids := func() []string {
		var out []string
		for _, acc := range a.list() {
			out = append(out, acc.ID)
		}
		return out
	}
	if got := ids(); len(got) != 2 || got[0] != defaultID || got[1] != "work" {
		t.Fatalf("with the Keychain answering: %v", got)
	}
	if err := os.Remove(answers); err != nil {
		t.Fatal(err)
	}
	if got := ids(); len(got) != 2 || got[0] != defaultID || got[1] != "work" {
		t.Fatalf("with the Keychain down: %v, want the Keychain-only login kept", got)
	}
}

func TestManagedClaudePoolExcludesLocalLogins(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "default-token", "local": "local-token", "managed": "local-managed-token"})
	a := newClaudeAccounts(home, credentialsFile, "")
	a.managedOnly = true
	if err := writeSyncState(home, map[string]syncEntry{loginPath("claude", "managed"): {Version: 1}}); err != nil {
		t.Fatal(err)
	}
	managed := account{ID: "managed", Path: filepath.Join(home, ".claude-managed")}
	token := newLongToken("sk-ant-oat01-managed", time.Now())
	if err := a.writeLongToken(managed, token); err != nil {
		t.Fatal(err)
	}
	accounts := a.list()
	if len(accounts) != 1 || accounts[0].ID != managed.ID || a.token(accounts[0]) != token.Token {
		t.Fatalf("unexpected managed pool: %v", accounts)
	}
	if a.token(account{ID: defaultID, Path: filepath.Join(home, ".claude")}) != "" {
		t.Fatal("managed pool served the native Claude sign-in")
	}
}
