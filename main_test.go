package main

import (
	"encoding/json"
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
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-work,Bearer t-work" {
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
	if strings.Join(up.seen, ",") != "Bearer t-default,Bearer t-work,Bearer claude-own-login" {
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
