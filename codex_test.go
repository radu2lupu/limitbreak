package main

import (
	"bytes"
	"encoding/base64"
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

// zstdBody stands in for Codex's compressed request body: bytes that are not text.
var zstdBody = []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x01, 0xff, 0x7f}

func fakeJWT(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// writeCodexLogin leaves a ChatGPT sign-in in a Codex home and returns its access token.
func writeCodexLogin(t *testing.T, dir, name, accountID string, expires time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	access := fakeJWT(map[string]any{"exp": expires.Unix(), "name": name})
	id := fakeJWT(map[string]any{"email": name + "@example.com", "https://api.openai.com/auth": map[string]any{
		"chatgpt_plan_type": "plus", "chatgpt_user_id": "user-" + name, "chatgpt_account_id": accountID}})
	writeJSON(t, filepath.Join(dir, "auth.json"), map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil,
		"tokens": map[string]any{"access_token": access, "account_id": accountID, "id_token": id, "refresh_token": "r-" + name}})
	return access
}

type codexSeen struct {
	auth, account, path, encoding string
	body                          []byte
}

// chatgpt stands in for chatgpt.com: it records what each request was sent with and answers by
// the chatgpt-account-id it carries.
type chatgpt struct {
	*httptest.Server
	mu         sync.Mutex
	seen       []codexSeen
	usageCalls int
	reply      map[string]func(w http.ResponseWriter)
}

func newChatGPT(t *testing.T) *chatgpt {
	c := &chatgpt{reply: map[string]func(http.ResponseWriter){}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		if r.URL.Path == codexUsagePath {
			c.usageCalls++
			c.mu.Unlock()
			w.Write([]byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":100,"reset_at":1800000000},"secondary_window":{"used_percent":58,"limit_window_seconds":604800,"reset_at":1800500000}}}`))
			return
		}
		account := r.Header.Get("chatgpt-account-id")
		c.seen = append(c.seen, codexSeen{r.Header.Get("Authorization"), account, r.URL.RequestURI(), r.Header.Get("Content-Encoding"), body})
		reply := c.reply[account]
		c.mu.Unlock()
		if reply != nil {
			reply(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *chatgpt) accounts() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for _, s := range c.seen {
		ids = append(ids, s.account)
	}
	return strings.Join(ids, ",")
}

func usageLimitReached(resetsAt int64) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_at":` + jsonInt(resetsAt) + `,"resets_in_seconds":100}}`))
	}
}

func jsonInt(v int64) string { raw, _ := json.Marshal(v); return string(raw) }

type codexRig struct {
	server    *httptest.Server
	pool      *pool
	codex     *codexAccounts
	up        *chatgpt
	refreshes []string
}

// startCodex runs the proxy with a Claude pool and a Codex pool on home, chatgpt.com stubbed and
// the refresh through Codex replaced by refresh (nil: it does nothing and succeeds).
func startCodex(t *testing.T, home string, refresh func(dir string) error) *codexRig {
	rig := &codexRig{up: newChatGPT(t)}
	claude := newClaudeAccounts(home, credentialsFile, "")
	claude.claude = ""
	rig.codex = newCodexAccounts(home, "", rig.up.URL)
	rig.codex.refresh = func(dir string) error {
		rig.refreshes = append(rig.refreshes, filepath.Base(dir))
		if refresh != nil {
			return refresh(dir)
		}
		return nil
	}
	rig.pool = newPool(rig.codex, rig.up.URL)
	rig.server = httptest.NewServer(newProxy(newPool(claude, rig.up.URL), rig.pool))
	t.Cleanup(rig.server.Close)
	return rig
}

// codexHome is a home with the default Codex login (the client's own) and a named "work" one.
func codexHome(t *testing.T) (home, workToken string) {
	home = t.TempDir()
	writeCodexLogin(t, filepath.Join(home, ".codex"), "child", "acct-child", time.Now().Add(240*time.Hour))
	workToken = writeCodexLogin(t, filepath.Join(home, ".codex-work"), "work", "acct-work", time.Now().Add(240*time.Hour))
	return home, workToken
}

// sendCodex posts a Codex request the way Codex does: its own login, a zstd body.
func sendCodex(t *testing.T, base, client string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/_c/"+client+"/backend-api/codex/responses", bytes.NewReader(zstdBody))
	req.Header.Set("Authorization", "Bearer child-own")
	req.Header.Set("chatgpt-account-id", "acct-child")
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestCodexGoesToTheChatGPTBackendOnTheClientsOwnLoginForTheDefaultAccount(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	resp, body := sendCodex(t, rig.server.URL, "agent_a")
	if resp.StatusCode != 200 || body != "event: response.completed\ndata: {}\n\n" || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.StatusCode, body, resp.Header)
	}
	seen := rig.up.seen[0]
	if seen.path != "/backend-api/codex/responses" || seen.auth != "Bearer child-own" || seen.account != "acct-child" ||
		seen.encoding != "zstd" || !bytes.Equal(seen.body, zstdBody) {
		t.Fatalf("%+v", seen)
	}
	if len(rig.refreshes) != 0 {
		t.Fatal("the default login was refreshed", rig.refreshes)
	}
}

func TestCodexSwapsTheLoginForANamedAccount(t *testing.T) {
	home, workToken := codexHome(t)
	rig := startCodex(t, home, nil)
	if status, body := put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"codex","id":"work"}`); status != 200 || !strings.Contains(body, `"activeFor":["agent_a"]`) {
		t.Fatal(status, body)
	}
	resp, _ := sendCodex(t, rig.server.URL, "agent_a")
	seen := rig.up.seen[0]
	if resp.StatusCode != 200 || seen.auth != "Bearer "+workToken || seen.account != "acct-work" || !bytes.Equal(seen.body, zstdBody) || seen.encoding != "zstd" {
		t.Fatalf("%d %+v", resp.StatusCode, seen)
	}
}

func TestAWebSocketUpgradeIsAnswered426(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	req, _ := http.NewRequest("GET", rig.server.URL+"/_c/agent_a/backend-api/codex/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || len(rig.up.seen) != 0 {
		t.Fatal(resp.StatusCode, rig.up.seen)
	}
}

func TestCodexUsageLimitMarksTheAccountSpentAndReplaysOnTheNextOne(t *testing.T) {
	home, workToken := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.up.reply["acct-child"] = usageLimitReached(1_900_000_000)
	resp, body := sendCodex(t, rig.server.URL, "agent_a")
	if resp.StatusCode != 200 || !strings.Contains(body, "response.completed") {
		t.Fatal(resp.StatusCode, body)
	}
	if rig.up.accounts() != "acct-child,acct-work" || rig.up.seen[1].auth != "Bearer "+workToken || !bytes.Equal(rig.up.seen[1].body, zstdBody) {
		t.Fatal(rig.up.seen)
	}
	sendCodex(t, rig.server.URL, "agent_b")
	if rig.up.accounts() != "acct-child,acct-work,acct-work" {
		t.Fatal("a spent account is skipped for every client", rig.up.accounts())
	}
	for _, row := range getStatus(t, rig.server.URL) {
		if row.Provider == "codex" && row.ID == "default" && (row.SpentUntil == nil || *row.SpentUntil != 1_900_000_000) {
			t.Fatal(row.SpentUntil)
		}
	}
}

func TestCodexOtherTooManyRequestsGoBackAsTheyAre(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.up.reply["acct-child"] = func(w http.ResponseWriter) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
	}
	resp, body := sendCodex(t, rig.server.URL, "agent_a")
	if resp.StatusCode != 429 || body != `{"error":{"type":"rate_limit_exceeded","message":"slow down"}}` || rig.up.accounts() != "acct-child" {
		t.Fatal(resp.StatusCode, body, rig.up.accounts())
	}
	if rig.pool.spentUntil["default"].After(time.Now()) {
		t.Fatal("a plain rate limit marked the account spent")
	}
}

func TestEveryCodexAccountSpentHandsTheLimitBack(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.up.reply["acct-child"] = usageLimitReached(1_900_000_000)
	rig.up.reply["acct-work"] = usageLimitReached(1_900_000_100)
	resp, body := sendCodex(t, rig.server.URL, "agent_a")
	if resp.StatusCode != 429 || !strings.Contains(body, "usage_limit_reached") || rig.up.accounts() != "acct-child,acct-work" {
		t.Fatal(resp.StatusCode, body, rig.up.accounts())
	}
}

func TestACodexReplyAtAHundredPercentMarksTheAccountSpentAhead(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.up.reply["acct-child"] = func(w http.ResponseWriter) {
		w.Header().Set("x-codex-primary-used-percent", "100")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-at", "1900000000")
		w.Header().Set("x-codex-secondary-used-percent", "61.5")
		w.Header().Set("x-codex-secondary-window-minutes", "10080")
		w.Write([]byte("data: ok\n\n"))
	}
	if resp, body := sendCodex(t, rig.server.URL, "agent_a"); resp.StatusCode != 200 || body != "data: ok\n\n" {
		t.Fatal(resp.StatusCode, body)
	}
	sendCodex(t, rig.server.URL, "agent_a")
	if rig.up.accounts() != "acct-child,acct-work" {
		t.Fatal(rig.up.accounts())
	}
	var def accountStatus
	for _, row := range getStatus(t, rig.server.URL) {
		if row.Provider == "codex" && row.ID == "default" {
			def = row
		}
	}
	if def.SpentUntil == nil || *def.SpentUntil != 1_900_000_000 || len(def.Windows) != 2 {
		t.Fatalf("%+v", def)
	}
	if w := def.Windows[0]; w.ID != "primary" || w.Label != "5h" || w.UsedPercent != 100 || w.WindowMinutes != 300 || *w.ResetsAt != 1_900_000_000 {
		t.Fatalf("%+v", w)
	}
	if w := def.Windows[1]; w.ID != "secondary" || w.Label != "weekly" || w.UsedPercent != 61.5 || w.ResetsAt != nil {
		t.Fatalf("%+v", w)
	}
	if rig.up.usageCalls != 1 { // work only: default's windows were just seen in headers
		t.Fatal("windows just seen in headers were read again", rig.up.usageCalls)
	}
}

func TestANamedCodexLoginIsRefreshedThroughCodexNearItsEndAndAfterA401(t *testing.T) {
	home, _ := codexHome(t)
	workDir := filepath.Join(home, ".codex-work")
	ending := writeCodexLogin(t, workDir, "work", "acct-work", time.Now().Add(time.Hour))
	var renewed string
	rig := startCodex(t, home, func(dir string) error {
		renewed = writeCodexLogin(t, dir, "work", "acct-work", time.Now().Add(240*time.Hour))
		return nil
	})
	put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"codex","id":"work"}`)
	sendCodex(t, rig.server.URL, "agent_a")
	if strings.Join(rig.refreshes, ",") != ".codex-work" || rig.up.seen[0].auth != "Bearer "+renewed || renewed == ending {
		t.Fatal(rig.refreshes, rig.up.seen)
	}

	// A token the backend rejects is refreshed and the request sent again on the same account.
	rig.codex.refreshed = map[string]time.Time{}
	rig.refreshes = nil
	rejected := writeCodexLogin(t, workDir, "work", "acct-work", time.Now().Add(240*time.Hour))
	rig.up.reply["acct-work"] = func(w http.ResponseWriter) { w.WriteHeader(401) }
	var again string
	rig.codex.refresh = func(dir string) error {
		rig.refreshes = append(rig.refreshes, filepath.Base(dir))
		again = writeCodexLogin(t, dir, "work", "acct-work", time.Now().Add(250*time.Hour))
		delete(rig.up.reply, "acct-work")
		return nil
	}
	resp, _ := sendCodex(t, rig.server.URL, "agent_a")
	last := rig.up.seen[len(rig.up.seen)-2:]
	if resp.StatusCode != 200 || strings.Join(rig.refreshes, ",") != ".codex-work" || last[0].auth != "Bearer "+rejected || last[1].auth != "Bearer "+again {
		t.Fatal(resp.StatusCode, rig.refreshes, last)
	}
}

func TestA401OnTheDefaultCodexLoginGoesBackToTheClient(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.up.reply["acct-child"] = func(w http.ResponseWriter) { w.WriteHeader(401); w.Write([]byte("expired")) }
	resp, body := sendCodex(t, rig.server.URL, "agent_a")
	if resp.StatusCode != 401 || body != "expired" || rig.up.accounts() != "acct-child" || len(rig.refreshes) != 0 {
		t.Fatal(resp.StatusCode, body, rig.up.accounts(), rig.refreshes)
	}
}

func TestAFailedRefreshOfAnEndedLoginSkipsItAndIsNotRetriedAtOnce(t *testing.T) {
	home, _ := codexHome(t)
	writeCodexLogin(t, filepath.Join(home, ".codex-work"), "work", "acct-work", time.Now().Add(-time.Hour))
	rig := startCodex(t, home, func(string) error { return os.ErrPermission })
	put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"codex","id":"work"}`)
	sendCodex(t, rig.server.URL, "agent_a")
	sendCodex(t, rig.server.URL, "agent_b")
	// work could not be refreshed, so default answered, and the refresh was tried once.
	if rig.up.accounts() != "acct-child,acct-child" || strings.Join(rig.refreshes, ",") != ".codex-work" {
		t.Fatal(rig.up.accounts(), rig.refreshes)
	}
}

func TestCodexUsageIsReadAtMostOnceEveryFewMinutesAndNormalised(t *testing.T) {
	home, _ := codexHome(t)
	rig := startCodex(t, home, nil)
	now := time.Now()
	rig.codex.now = func() time.Time { return now }
	getStatus(t, rig.server.URL)
	rows := getStatus(t, rig.server.URL)
	if rig.up.usageCalls != 2 { // once per account
		t.Fatal(rig.up.usageCalls)
	}
	var work accountStatus
	for _, row := range rows {
		if row.Provider == "codex" && row.ID == "work" {
			work = row
		}
	}
	if len(work.Windows) != 2 || work.Windows[0].Label != "5h" || work.Windows[0].UsedPercent != 5 || *work.Windows[0].ResetsAt != 1_800_000_000 ||
		work.Windows[1].Label != "weekly" || work.Windows[1].WindowMinutes != 10080 || work.Windows[1].Observed != now.Unix() {
		t.Fatalf("%+v", work.Windows)
	}
	now = now.Add(codexUsageEvery + time.Second)
	getStatus(t, rig.server.URL)
	if rig.up.usageCalls != 4 {
		t.Fatal(rig.up.usageCalls)
	}
	if len(rig.refreshes) != 0 {
		t.Fatal("a usage read refreshed a login")
	}
}

func TestStatusHasProviderLabelPlanAndWindowsForEveryAccountAndNoToken(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default"})
	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "t-default", "expiresAt": float64(far) * 1000, "subscriptionType": "max"}})
	workToken := writeCodexLogin(t, filepath.Join(home, ".codex-work"), "work", "acct-work", time.Now().Add(240*time.Hour))
	os.MkdirAll(filepath.Join(home, ".codex-chief"), 0o700) // some other tool's dir: not a login
	rig := startCodex(t, home, nil)
	claude := newClaudeAccounts(home, credentialsFile, "")
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1800000000")
	claude.recordUsage(account{ID: "default", Email: "default@example.com"}, h)

	resp, err := http.Get(rig.server.URL + "/_status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), workToken) || strings.Contains(string(raw), "t-default") || strings.Contains(string(raw), "r-work") {
		t.Fatal("a token in the status", string(raw))
	}
	var rows []map[string]any
	json.Unmarshal(raw, &rows)
	if len(rows) != 2 {
		t.Fatal(string(raw))
	}
	c, x := rows[0], rows[1]
	if c["provider"] != "claude" || c["id"] != "default" || c["label"] != "default@example.com" || c["plan"] != "max" || c["usage"] == nil {
		t.Fatal(c)
	}
	cw := c["windows"].([]any)[0].(map[string]any)
	if len(c["windows"].([]any)) != 1 || cw["id"] != "five_hour" || cw["label"] != "5h" || cw["usedPercent"] != 25.0 || cw["resetsAt"] != 1.8e9 || cw["windowMinutes"] != 300.0 || cw["observed"] == nil {
		t.Fatal(cw)
	}
	if x["provider"] != "codex" || x["id"] != "work" || x["email"] != "work@example.com" || x["label"] != "work@example.com" || x["plan"] != "plus" ||
		len(x["windows"].([]any)) != 2 || x["usage"] != nil || x["authEnds"] != nil {
		t.Fatal(x)
	}
}

func TestTheActiveAccountIsPerClientPerProvider(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	writeCodexLogin(t, filepath.Join(home, ".codex"), "child", "acct-child", time.Now().Add(240*time.Hour))
	writeCodexLogin(t, filepath.Join(home, ".codex-work"), "work", "acct-work", time.Now().Add(240*time.Hour))
	rig := startCodex(t, home, nil)
	if status, _ := put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"codex","id":"work"}`); status != 200 {
		t.Fatal(status)
	}
	if status, _ := put(t, rig.server.URL+"/_c/agent_a/_active", `{"id":"default"}`); status != 200 {
		t.Fatal(status)
	}
	if status, _ := put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"grok","id":"work"}`); status != 404 {
		t.Fatal(status)
	}
	if status, _ := put(t, rig.server.URL+"/_c/agent_a/_active", `{"provider":"codex","id":"nobody"}`); status != 404 {
		t.Fatal(status)
	}
	active := map[string]string{}
	for _, row := range getStatus(t, rig.server.URL) {
		for _, client := range row.ActiveFor {
			active[row.Provider+"/"+client] = row.ID
		}
	}
	if active["codex/agent_a"] != "work" || active["claude/agent_a"] != "default" || len(active) != 2 {
		t.Fatal(active)
	}
	sendCodex(t, rig.server.URL, "agent_a")
	if rig.up.accounts() != "acct-work" {
		t.Fatal(rig.up.accounts())
	}
}

func TestAppServerAccountReadSpeaksTheStdioProtocol(t *testing.T) {
	var sent bytes.Buffer
	reply := strings.Join([]string{
		`{"id":1,"result":{"userAgent":"x"}}`,
		`{"method":"configWarning","params":{}}`,
		`{"id":2,"result":{"account":{"type":"chatgpt","email":"a@b.c","planType":"plus"},"requiresOpenaiAuth":true}}`,
	}, "\n") + "\n"
	if err := appServerAccountRead(&sent, strings.NewReader(reply)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sent.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"method":"initialize"`) || lines[1] != `{"method":"initialized"}` ||
		lines[2] != `{"id":2,"method":"account/read","params":{"refreshToken":true}}` {
		t.Fatal(lines)
	}
	signedOut := `{"id":2,"result":{"account":null,"requiresOpenaiAuth":true}}` + "\n"
	if appServerAccountRead(io.Discard, strings.NewReader(signedOut)) == nil {
		t.Fatal("a signed-out home reported as refreshed")
	}
	failed := `{"id":2,"error":{"code":-32603,"message":"refresh token expired"}}` + "\n"
	if err := appServerAccountRead(io.Discard, strings.NewReader(failed)); err == nil || !strings.Contains(err.Error(), "refresh token expired") {
		t.Fatal(err)
	}
	if appServerAccountRead(io.Discard, strings.NewReader("")) == nil {
		t.Fatal("no answer reported as refreshed")
	}
}

func TestCodexSpentResetReadsTheBody(t *testing.T) {
	now := time.Unix(1_000, 0)
	if spent, reset := codexSpentReset(429, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`), now); !spent || reset != time.Unix(1_060, 0) {
		t.Fatal(spent, reset)
	}
	if spent, _ := codexSpentReset(200, []byte(`{"error":{"type":"usage_limit_reached"}}`), now); spent {
		t.Fatal("only a 429")
	}
	if spent, _ := codexSpentReset(429, []byte(`not json`), now); spent {
		t.Fatal("an unreadable body is not a spent window")
	}
}

func TestCodexDiscoveryTakesChatGPTSignInsOnly(t *testing.T) {
	home := t.TempDir()
	writeCodexLogin(t, filepath.Join(home, ".codex-b"), "b", "acct-b", time.Now().Add(time.Hour))
	writeCodexLogin(t, filepath.Join(home, ".codex-a"), "a", "acct-a", time.Now().Add(time.Hour))
	os.MkdirAll(filepath.Join(home, ".codex-key"), 0o700)
	writeJSON(t, filepath.Join(home, ".codex-key", "auth.json"), map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": "sk-x"})
	list := newCodexAccounts(home, "", "").list()
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" || list[0].Email != "a@example.com" || list[0].Plan != "plus" || list[0].Who != "user-a/acct-a" {
		t.Fatalf("%+v", list)
	}
}

// A second sign-in to the same ChatGPT login (~/.codex-planner beside ~/.codex) shares its window:
// it is listed with sameAs, never rotated to, never made active, and its usage is not asked for.
func TestSecondSignInToTheSameSubscriptionIsNeverRotatedTo(t *testing.T) {
	home := t.TempDir()
	writeCodexLogin(t, filepath.Join(home, ".codex"), "me", "acct-me", time.Now().Add(time.Hour))
	writeCodexLogin(t, filepath.Join(home, ".codex-planner"), "me", "acct-me", time.Now().Add(-time.Hour))
	writeCodexLogin(t, filepath.Join(home, ".codex-work"), "work", "acct-work", time.Now().Add(time.Hour))
	p := newPool(newCodexAccounts(home, "", ""), "http://127.0.0.1:1")

	if got := p.current("c"); got == nil || got.ID != defaultID {
		t.Fatalf("current = %v, want default", got)
	}
	if next := p.spend("c", defaultID, time.Now().Add(time.Hour)); next == nil || next.ID != "work" {
		t.Fatalf("after default is spent: %v, want work (planner is the same subscription)", next)
	}
	if next := p.spend("c", "work", time.Now().Add(time.Hour)); next != nil {
		t.Fatalf("after work is spent: %v, want none", next.ID)
	}
	if p.setActive("c", "planner") {
		t.Fatal("planner was made active")
	}
	for _, s := range p.status() {
		want := ""
		if s.ID == "planner" {
			want = defaultID
		}
		if s.SameAs != want {
			t.Fatalf("%s sameAs = %q, want %q", s.ID, s.SameAs, want)
		}
	}
}
