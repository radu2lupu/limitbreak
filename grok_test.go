package main

import (
	"bytes"
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

// grokAuthJSON is grok's auth.json for a browser sign-in: one login keyed by issuer and principal.
func grokAuthJSON(name string, expires time.Time) []byte {
	token := fakeJWT(map[string]any{"exp": expires.Unix(), "sub": "user-" + name, "nonce": expires.UnixNano()})
	raw, _ := json.Marshal(map[string]any{"https://auth.x.ai::user-" + name: map[string]any{
		"key": token, "auth_mode": "oidc", "user_id": "user-" + name, "team_id": "team-" + name,
		"email": name + "@example.com", "refresh_token": "r-" + name, "oidc_issuer": "https://auth.x.ai",
		"expires_at": expires.UTC().Format(time.RFC3339Nano), "create_time": expires.Add(-6 * time.Hour).UTC().Format(time.RFC3339Nano)}})
	return raw
}

// writeGrokLogin leaves a sign-in in a Grok home and returns its access token.
func writeGrokLogin(t *testing.T, dir, name string, expires time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := grokAuthJSON(name, expires)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := parseGrokLogin(raw)
	return l.token
}

type grokSeen struct {
	method, path, auth, email, userID, grokUserID, tokenAuth, encoding string
	body                                                               []byte
}

// grokUpstream stands in for cli-chat-proxy.grok.com: it records each request and answers by the
// login (X-Userid) it carries.
type grokUpstream struct {
	*httptest.Server
	mu           sync.Mutex
	seen         []grokSeen
	billingCalls int
	reply        map[string]func(w http.ResponseWriter)
}

func newGrokUpstream(t *testing.T) *grokUpstream {
	g := &grokUpstream{reply: map[string]func(http.ResponseWriter){}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		if r.URL.RequestURI() == grokBillingPath {
			g.billingCalls++
			g.mu.Unlock()
			if r.Header.Get("X-Xai-Token-Auth") != "xai-grok-cli" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-11T05:53:28.428+00:00","end":"2026-11-18T05:53:28.428+00:00"},"creditUsagePercent":46.0,"productUsage":[{"product":"GrokBuild","usagePercent":15.0}],"isUnifiedBillingUser":true,"prepaidBalance":{"val":0}}}`))
			return
		}
		g.seen = append(g.seen, grokSeen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Email"),
			r.Header.Get("X-Userid"), r.Header.Get("X-Grok-User-Id"), r.Header.Get("X-Xai-Token-Auth"), r.Header.Get("Content-Encoding"), body})
		reply := g.reply[r.Header.Get("X-Userid")]
		g.mu.Unlock()
		if reply != nil {
			reply(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *grokUpstream) users() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []string
	for _, s := range g.seen {
		ids = append(ids, s.userID)
	}
	return strings.Join(ids, ",")
}

type grokRig struct {
	server    *httptest.Server
	pool      *pool
	grok      *grokAccounts
	up        *grokUpstream
	claudeUp  *upstream
	refreshes []string
}

// startGrok runs the proxy with Claude, Codex and Grok pools on home, Grok's upstream stubbed and
// the refresh through grok replaced by refresh (nil: it does nothing and succeeds).
func startGrok(t *testing.T, home string, refresh func(dir string) error) *grokRig {
	rig := &grokRig{up: newGrokUpstream(t), claudeUp: newUpstream(t)}
	claude := newClaudeAccounts(home, credentialsFile, "")
	claude.claude = ""
	rig.grok = newGrokAccounts(home, "", rig.up.URL)
	rig.grok.refresh = func(dir string) error {
		rig.refreshes = append(rig.refreshes, filepath.Base(dir))
		if refresh != nil {
			return refresh(dir)
		}
		return nil
	}
	rig.pool = newPool(rig.grok, rig.up.URL)
	codex := newCodexAccounts(home, "", "http://127.0.0.1:1")
	rig.server = httptest.NewServer(newProxy(newPool(claude, rig.claudeUp.URL), newPool(codex, "http://127.0.0.1:1"), rig.pool))
	t.Cleanup(rig.server.Close)
	return rig
}

// grokHome is a home with the default Grok login (the client's own) and a named "work" one.
func grokHome(t *testing.T) (home, defaultToken, workToken string) {
	home = t.TempDir()
	defaultToken = writeGrokLogin(t, filepath.Join(home, ".grok"), "me", time.Now().Add(6*time.Hour))
	workToken = writeGrokLogin(t, filepath.Join(home, ".grok-work"), "work", time.Now().Add(6*time.Hour))
	return home, defaultToken, workToken
}

// sendGrok sends a request the way grok does with GROK_CLI_CHAT_PROXY_BASE_URL at the client's
// path: its own login and who-headers, a zstd body.
func sendGrok(t *testing.T, base, client, method, path, ownToken string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, base+"/_c/"+client+"/grok"+path, bytes.NewReader(zstdBody))
	req.Header.Set("Authorization", "Bearer "+ownToken)
	req.Header.Set("X-Xai-Token-Auth", "xai-grok-cli")
	req.Header.Set("X-Email", "me@example.com")
	req.Header.Set("X-Userid", "user-me")
	req.Header.Set("X-Grok-User-Id", "user-me")
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestGrokGoesUpstreamWithoutTheMountOnTheClientsOwnLoginForTheDefaultAccount(t *testing.T) {
	home, own, _ := grokHome(t)
	rig := startGrok(t, home, nil)
	resp, body := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	if resp.StatusCode != 200 || body != "event: response.completed\ndata: {}\n\n" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.StatusCode, body, resp.Header)
	}
	seen := rig.up.seen[0]
	if seen.path != "/v1/responses" || seen.auth != "Bearer "+own || seen.userID != "user-me" || seen.encoding != "zstd" || !bytes.Equal(seen.body, zstdBody) {
		t.Fatalf("%+v", seen)
	}
	if len(rig.refreshes) != 0 || len(rig.claudeUp.seen) != 0 {
		t.Fatal(rig.refreshes, rig.claudeUp.seen)
	}
	if active := getStatus(t, rig.server.URL); !hasActive(active, "grok", "default", "agent_a") {
		t.Fatalf("%+v", active)
	}
}

func hasActive(rows []accountStatus, provider, id, client string) bool {
	for _, row := range rows {
		if row.Provider == provider && row.ID == id {
			for _, c := range row.ActiveFor {
				if c == client {
					return true
				}
			}
		}
	}
	return false
}

func TestClaudesMessagesStillGoToClaudeBesideTheGrokMount(t *testing.T) {
	home, _, _ := grokHome(t)
	rig := startGrok(t, home, nil)
	resp, err := http.Post(rig.server.URL+"/_c/agent_a/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(rig.claudeUp.paths) != 1 || rig.claudeUp.paths[0] != "/v1/messages" || len(rig.up.seen) != 0 {
		t.Fatal(rig.claudeUp.paths, rig.up.seen)
	}
}

func TestGrokSwapsTheLoginAndWhoHeadersForANamedAccountOnModelTurnsOnly(t *testing.T) {
	home, own, workToken := grokHome(t)
	rig := startGrok(t, home, nil)
	if status, body := put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"grok","id":"work"}`); status != 200 || !strings.Contains(body, `"activeFor":["agent_a"]`) {
		t.Fatal(status, body)
	}
	sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/storage", own)
	sendGrok(t, rig.server.URL, "agent_a", "GET", "/v1/models", own)
	turn, storage, models := rig.up.seen[0], rig.up.seen[1], rig.up.seen[2]
	if turn.auth != "Bearer "+workToken || turn.email != "work@example.com" || turn.userID != "user-work" || turn.grokUserID != "user-work" ||
		turn.tokenAuth != "xai-grok-cli" || !bytes.Equal(turn.body, zstdBody) || turn.encoding != "zstd" {
		t.Fatalf("%+v", turn)
	}
	for _, other := range []grokSeen{storage, models} {
		if other.auth != "Bearer "+own || other.userID != "user-me" {
			t.Fatalf("grok's own requests went on another login: %+v", other)
		}
	}
	if models.method != "GET" || models.path != "/v1/models" || storage.path != "/v1/storage" {
		t.Fatalf("%+v %+v", storage, models)
	}
}

func grokRefusal(status int, body string, header map[string]string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func TestASpentGrokPlanMovesTheClientToTheNextAccountBeforeGrokSeesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  func(http.ResponseWriter)
		resets int64
	}{
		{"402", grokRefusal(402, `{"error":"run out of credits"}`, nil), 0},
		{"429 weekly limit", grokRefusal(429, `{"code":"usage_limit_reached","error":"You hit your weekly limit."}`, map[string]string{"Retry-After": "7200"}), -1},
		{"403 spending cap", grokRefusal(403, `{"error":{"message":"You've hit your spending cap.","resets_at":1900000000}}`, nil), 1_900_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, own, workToken := grokHome(t)
			rig := startGrok(t, home, nil)
			rig.up.reply["user-me"] = tc.reply
			resp, body := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
			if resp.StatusCode != 200 || !strings.Contains(body, "response.completed") {
				t.Fatal(resp.StatusCode, body)
			}
			if rig.up.users() != "user-me,user-work" || rig.up.seen[1].auth != "Bearer "+workToken || !bytes.Equal(rig.up.seen[1].body, zstdBody) {
				t.Fatal(rig.up.seen)
			}
			sendGrok(t, rig.server.URL, "agent_b", "POST", "/v1/responses", own)
			if rig.up.users() != "user-me,user-work,user-work" {
				t.Fatal("a spent account is skipped for every client", rig.up.users())
			}
			until := rig.pool.spentUntil[defaultID]
			switch {
			case tc.resets > 0 && until.Unix() != tc.resets:
				t.Fatal(until)
			case tc.resets < 0 && (until.Before(time.Now().Add(119*time.Minute)) || until.After(time.Now().Add(121*time.Minute))):
				t.Fatal("Retry-After not used", until)
			case !until.After(time.Now()):
				t.Fatal("not spent", until)
			}
		})
	}
}

func TestAPlainGrokRateLimitOrForbiddenGoesBackAsItIs(t *testing.T) {
	for _, status := range []int{429, 403} {
		home, own, _ := grokHome(t)
		rig := startGrok(t, home, nil)
		rig.up.reply["user-me"] = grokRefusal(status, `{"error":"You've hit the rate limit for your plan. Try again shortly."}`, nil)
		resp, body := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
		if resp.StatusCode != status || !strings.Contains(body, "rate limit for your plan") || rig.up.users() != "user-me" {
			t.Fatal(resp.StatusCode, body, rig.up.users())
		}
		if rig.pool.spentUntil[defaultID].After(time.Now()) {
			t.Fatal("a plain refusal marked the account spent")
		}
	}
}

func TestEveryGrokAccountSpentHandsTheRefusalBack(t *testing.T) {
	home, own, _ := grokHome(t)
	rig := startGrok(t, home, nil)
	rig.up.reply["user-me"] = grokRefusal(402, `{"error":"run out of credits"}`, nil)
	rig.up.reply["user-work"] = grokRefusal(402, `{"error":"run out of credits"}`, nil)
	resp, body := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	if resp.StatusCode != 402 || !strings.Contains(body, "out of credits") || rig.up.users() != "user-me,user-work" {
		t.Fatal(resp.StatusCode, body, rig.up.users())
	}
}

func TestANamedGrokLoginIsRefreshedThroughGrokNearItsEndAndAfterA401(t *testing.T) {
	home, own, _ := grokHome(t)
	workDir := filepath.Join(home, ".grok-work")
	ending := writeGrokLogin(t, workDir, "work", time.Now().Add(20*time.Minute))
	var renewed string
	rig := startGrok(t, home, func(dir string) error {
		renewed = writeGrokLogin(t, dir, "work", time.Now().Add(6*time.Hour))
		return nil
	})
	put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"grok","id":"work"}`)
	sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	if strings.Join(rig.refreshes, ",") != ".grok-work" || rig.up.seen[0].auth != "Bearer "+renewed || renewed == ending {
		t.Fatal(rig.refreshes, rig.up.seen)
	}

	// A token the backend rejects is refreshed and the request sent again on the same account.
	rig.grok.refreshed = loginRenewal{}
	rig.refreshes = nil
	rejected := writeGrokLogin(t, workDir, "work", time.Now().Add(5*time.Hour))
	rig.up.reply["user-work"] = func(w http.ResponseWriter) { w.WriteHeader(401) }
	var again string
	rig.grok.refresh = func(dir string) error {
		rig.refreshes = append(rig.refreshes, filepath.Base(dir))
		again = writeGrokLogin(t, dir, "work", time.Now().Add(6*time.Hour))
		delete(rig.up.reply, "user-work")
		return nil
	}
	resp, _ := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	last := rig.up.seen[len(rig.up.seen)-2:]
	if resp.StatusCode != 200 || strings.Join(rig.refreshes, ",") != ".grok-work" || last[0].auth != "Bearer "+rejected || last[1].auth != "Bearer "+again {
		t.Fatal(resp.StatusCode, rig.refreshes, last)
	}
}

func TestA401OnTheDefaultGrokLoginGoesBackToGrok(t *testing.T) {
	home, own, _ := grokHome(t)
	rig := startGrok(t, home, nil)
	rig.up.reply["user-me"] = func(w http.ResponseWriter) { w.WriteHeader(401); w.Write([]byte("expired")) }
	resp, body := sendGrok(t, rig.server.URL, "agent_a", "POST", "/v1/responses", own)
	if resp.StatusCode != 401 || body != "expired" || rig.up.users() != "user-me" || len(rig.refreshes) != 0 {
		t.Fatal(resp.StatusCode, body, rig.up.users(), rig.refreshes)
	}
}

func TestGrokStatusHasProviderEmailAndTheBillingWindowAndNoToken(t *testing.T) {
	home, own, workToken := grokHome(t)
	os.MkdirAll(filepath.Join(home, ".grok-notes"), 0o700) // some other dir: not a login
	rig := startGrok(t, home, nil)
	resp, err := http.Get(rig.server.URL + "/_status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, secret := range []string{own, workToken, "r-me", "r-work"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("a token in the status", string(raw))
		}
	}
	var rows []accountStatus
	json.Unmarshal(raw, &rows)
	var grok []accountStatus
	for _, row := range rows {
		if row.Provider == "grok" {
			grok = append(grok, row)
		}
	}
	if len(grok) != 2 || grok[0].ID != "default" || grok[1].ID != "work" || grok[1].Email != "work@example.com" || grok[1].Label != "work@example.com" ||
		grok[1].AuthEnds != nil || grok[1].SpentUntil != nil {
		t.Fatalf("%+v", grok)
	}
	w := grok[0].Windows
	if len(w) != 1 || w[0].ID != "weekly" || w[0].Label != "weekly" || w[0].UsedPercent != 46 || w[0].WindowMinutes != 7*24*60 ||
		w[0].ResetsAt == nil || *w[0].ResetsAt != time.Date(2026, 11, 18, 5, 53, 28, 0, time.UTC).Unix() {
		t.Fatalf("%+v", w)
	}
	getStatus(t, rig.server.URL)
	if rig.up.billingCalls != 2 || len(rig.refreshes) != 0 {
		t.Fatal("billing read more than once per account, or a usage read refreshed a login", rig.up.billingCalls, rig.refreshes)
	}
}

func TestGrokDiscoveryAndSecondSignInsToTheSameLogin(t *testing.T) {
	home := t.TempDir()
	writeGrokLogin(t, filepath.Join(home, ".grok"), "me", time.Now().Add(time.Hour))
	writeGrokLogin(t, filepath.Join(home, ".grok-again"), "me", time.Now().Add(time.Hour))
	writeGrokLogin(t, filepath.Join(home, ".grok-b"), "b", time.Now().Add(time.Hour))
	os.MkdirAll(filepath.Join(home, ".grok-empty"), 0o700)
	os.WriteFile(filepath.Join(home, ".grok-file"), []byte("x"), 0o600)
	p := newPool(newGrokAccounts(home, "", ""), "http://127.0.0.1:1")
	list := p.list()
	if len(list) != 3 || list[0].ID != "default" || list[1].ID != "again" || list[2].ID != "b" || list[2].Who != "user-b/team-b" || list[2].Email != "b@example.com" {
		t.Fatalf("%+v", list)
	}
	if p.setActive("c", "again") {
		t.Fatal("a second sign-in to the default login was made active")
	}
}

func TestParseGrokLoginTakesTheLongestLastingKey(t *testing.T) {
	soon := time.Now().Add(time.Hour).UTC()
	later := time.Now().Add(5 * time.Hour).UTC()
	raw := fmt.Sprintf(`{"https://accounts.x.ai/sign-in":{"key":"old","expires_at":%q},"https://auth.x.ai::u":{"key":"new","expires_at":%q,"email":"e@x","user_id":"u","team_id":"t"},"x":{"expires_at":%q}}`,
		soon.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano), later.Add(time.Hour).Format(time.RFC3339Nano))
	l, ok := parseGrokLogin([]byte(raw))
	if !ok || l.token != "new" || l.email != "e@x" || l.userID != "u" || l.teamID != "t" || !l.expires.Equal(later) {
		t.Fatalf("%v %+v", ok, l)
	}
	if _, ok := parseGrokLogin([]byte(`{"a":{"email":"no key"}}`)); ok {
		t.Fatal("a login without a token")
	}
	jwtOnly := fmt.Sprintf(`{"a":{"key":%q}}`, fakeJWT(map[string]any{"exp": later.Unix()}))
	if l, ok := parseGrokLogin([]byte(jwtOnly)); !ok || l.expires.Unix() != later.Unix() {
		t.Fatal("expiry not read from the token", l.expires)
	}
}

func TestGrokSpentResetReadsStatusBodyAndRetryAfter(t *testing.T) {
	now := time.Unix(1_000, 0)
	for _, tc := range []struct {
		status int
		body   string
		header http.Header
		spent  bool
		reset  int64
	}{
		{402, ``, nil, true, 0},
		{429, `{"error":"usage balance exhausted"}`, http.Header{"Retry-After": {"60"}}, true, 1_060},
		{429, `{"resets_at":5000,"error":"Usage limit reached"}`, nil, true, 5_000},
		{403, `out of credits`, nil, true, 0},
		{429, `{"error":"rate limited"}`, nil, false, 0},
		{403, `{"error":"permission denied"}`, nil, false, 0},
		{500, `usage limit`, nil, false, 0},
		{200, `usage limit`, nil, false, 0},
	} {
		spent, reset := grokSpentReset(tc.status, tc.header, []byte(tc.body), now)
		want := time.Time{}
		if tc.reset > 0 {
			want = time.Unix(tc.reset, 0)
		}
		if spent != tc.spent || !reset.Equal(want) {
			t.Errorf("%d %s: %v %v", tc.status, tc.body, spent, reset)
		}
	}
}

func TestGrokBillingWithoutAPeriodOrPercent(t *testing.T) {
	now := time.Unix(1_000, 0)
	if w, err := grokBillingWindows([]byte(`{"config":{"monthlyLimit":{"val":0}}}`), now); err != nil || len(w) != 0 {
		t.Fatal(w, err)
	}
	if w, err := grokBillingWindows([]byte(`{"config":{"creditUsagePercent":100}}`), now); err != nil || len(w) != 1 || w[0].ID != "period" || w[0].ResetsAt != nil || w[0].UsedPercent != 100 {
		t.Fatal(w, err)
	}
	if _, err := grokBillingWindows([]byte(`nope`), now); err == nil {
		t.Fatal("bad JSON read")
	}
}

// MARK: grok itself, faked by this test binary

// fakeGrokEnv makes this test binary act as `grok` (see TestMain).
const fakeGrokEnv = "LIMITBREAK_TEST_FAKE_GROK"

// runFakeGrok is `grok login --oauth` (prints the sign-in link, leaves a login in $GROK_HOME) and
// `grok models` (renews the login in $GROK_HOME when asked to refresh early). It refuses when this
// proxy or another auth file is in its environment, as a real run there would sign in elsewhere.
func runFakeGrok(args []string) int {
	home := os.Getenv("GROK_HOME")
	if home == "" || os.Getenv("GROK_CLI_CHAT_PROXY_BASE_URL") != "" || os.Getenv("GROK_AUTH_PATH") != "" || os.Getenv("XAI_API_KEY") != "" {
		fmt.Fprintln(os.Stderr, "fake grok: wrong environment")
		return 3
	}
	name := os.Getenv(fakeGrokEnv)
	switch {
	case len(args) == 2 && args[0] == "login" && args[1] == "--oauth":
		fmt.Println("To sign in, open this URL in your browser:")
		fmt.Println("https://auth.x.ai/oauth2/auth?client_id=grok-cli&redirect_uri=http%3A%2F%2F127.0.0.1%3A5555%2Fcallback&state=abc")
		if name == "fail" {
			fmt.Fprintln(os.Stderr, "Authorization denied.")
			return 1
		}
		os.WriteFile(filepath.Join(home, "auth.json"), grokAuthJSON(name, time.Now().Add(6*time.Hour)), 0o600)
		fmt.Println("Signed in as " + name + "@example.com")
		return 0
	case len(args) == 1 && args[0] == "models":
		if name == "signed-out" {
			fmt.Println("You are not authenticated.")
			return 0
		}
		if os.Getenv("GROK_AUTH_EARLY_INVALIDATION_SECS") == "86400" {
			os.WriteFile(filepath.Join(home, "auth.json"), grokAuthJSON(name, time.Now().Add(6*time.Hour)), 0o600)
		}
		fmt.Println("Available models:\n  * grok-4.7")
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake grok: unexpected args", args)
	return 2
}

func TestMain(m *testing.M) {
	if os.Getenv(fakeGrokEnv) != "" {
		os.Exit(runFakeGrok(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeGrok(t *testing.T, behaviour string) string {
	t.Setenv(fakeGrokEnv, behaviour)
	t.Setenv("GROK_CLI_CHAT_PROXY_BASE_URL", "http://127.0.0.1:48617/_c/me/grok/v1") // must not reach grok
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestAddGrokSignsInWithGrokIntoItsOwnHomeAndRemoveDeletesIt(t *testing.T) {
	home := t.TempDir()
	writeGrokLogin(t, filepath.Join(home, ".grok"), "me", time.Now().Add(6*time.Hour))
	grok := newGrokAccounts(home, fakeGrok(t, "new"), "")
	var out bytes.Buffer
	if err := addAccountJSON(grok, "work", commandRunner(grok.loginCommand), strings.NewReader(""), &out); err != nil {
		t.Fatal(err, out.String())
	}
	list := events(t, out.String())
	if kinds(list) != "open_url,added" || !strings.HasPrefix(list[0].URL, "https://auth.x.ai/oauth2/auth?") ||
		list[1].Provider != "grok" || list[1].ID != "work" || list[1].Email != "new@example.com" {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".grok-work", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".grok", "auth.json")); !strings.Contains(string(raw), "me@example.com") {
		t.Fatal("the default login was touched")
	}

	out.Reset()
	if err := removeAccountJSON(grok, []string{"work", "--yes"}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if e := events(t, out.String()); kinds(e) != "removed" || e[0].Provider != "grok" || e[0].Email != "new@example.com" {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".grok-work")); !os.IsNotExist(err) {
		t.Fatal("the home limitbreak made was left", err)
	}
	if err := removeAccountJSON(grok, []string{"default", "--yes"}, io.Discard); err == nil {
		t.Fatal("the default login was removable")
	}
}

func TestAFailedGrokSignInLeavesNoDir(t *testing.T) {
	home := t.TempDir()
	grok := newGrokAccounts(home, fakeGrok(t, "fail"), "")
	var out bytes.Buffer
	if err := addAccountJSON(grok, "work", commandRunner(grok.loginCommand), strings.NewReader(""), &out); err == nil {
		t.Fatal("a failed sign-in was added")
	}
	if list := events(t, out.String()); kinds(list) != "open_url,error" {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".grok-work")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestGrokModelsRefreshRenewsTheLoginThroughGrok(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".grok-work")
	old := writeGrokLogin(t, dir, "work", time.Now().Add(10*time.Minute))
	if err := grokModelsRefresh(fakeGrok(t, "work"))(dir); err != nil {
		t.Fatal(err)
	}
	if l, _ := readGrokLogin(dir); l.token == old || l.expires.Before(time.Now().Add(5*time.Hour)) {
		t.Fatal("not renewed", l.expires)
	}
	writeGrokLogin(t, dir, "work", time.Now().Add(10*time.Minute))
	if err := grokModelsRefresh(fakeGrok(t, "signed-out"))(dir); err == nil || !strings.Contains(err.Error(), "signed out") {
		t.Fatal(err)
	}
	if err := grokModelsRefresh("")(dir); err == nil {
		t.Fatal("no grok reported as refreshed")
	}
}

func TestListShowsGrokAccountsWithTheirWindow(t *testing.T) {
	t.Setenv("LIMITBREAK_PORT", "1") // no proxy there: nothing is marked active
	home, _, _ := grokHome(t)
	up := newGrokUpstream(t)
	var out bytes.Buffer
	listAccounts([]loginProvider{newGrokAccounts(home, "", up.URL)}, &out)
	text := out.String()
	for _, want := range []string{"  grok   default          me@example.com", "Grok's own login, refreshed by grok",
		"  grok   work             work@example.com", "refreshed by limitbreak through grok", "weekly"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q not in\n%s", want, text)
		}
	}
}
