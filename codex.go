package main

// The Codex provider: the ChatGPT sign-ins Codex holds. The default account is ~/.codex, which
// belongs to Codex itself: Limit Break never refreshes or rewrites it, and a request on it keeps
// the client's own Authorization and chatgpt-account-id. The others are ~/.codex-<name> dirs whose
// auth.json is a ChatGPT sign-in (`auth_mode` "chatgpt"); a request on one has those two headers
// swapped for the login's. Such a login is refreshed by Codex (`codex app-server`, `account/read`
// with `refreshToken`), never by calling the OAuth endpoint from here.
//
// Codex (with `openai_base_url = "http://127.0.0.1:48617/_c/<client>/backend-api/codex"`) sends its
// requests to /backend-api/codex/…, which go on to https://chatgpt.com. Bodies (zstd) and their
// Content-Encoding pass as they are; replies stream back as they arrive. A 429 whose body says
// `usage_limit_reached` marks the account spent until `resets_at` and the request goes again on
// the next account; a reply whose `x-codex-*-used-percent` header reaches 100 marks it spent ahead.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultCodexUpstream = "https://chatgpt.com"
	codexUsagePath       = "/backend-api/wham/usage"
	// codexRefreshWithin is when a named login's access token is refreshed before it ends.
	codexRefreshWithin = 24 * time.Hour
	// codexRefreshEvery is the least time between two refreshes of one login, so a failing one is
	// not retried on every request.
	codexRefreshEvery = 5 * time.Minute
	// codexUsageEvery is the least time between two usage reads of one account.
	codexUsageEvery = 5 * time.Minute
)

// MARK: Decisions (pure)

// codexLogin is what a request on a Codex sign-in needs, and who it is.
type codexLogin struct {
	token     string
	accountID string
	// expires is when the access token ends; zero when it does not say.
	expires time.Time
	email   string
	plan    string
	userID  string
}

// parseCodexLogin reads Codex's auth.json; false unless it is a ChatGPT sign-in with a token.
func parseCodexLogin(raw []byte) (codexLogin, bool) {
	var doc struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			IDToken     string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.AuthMode != "chatgpt" || doc.Tokens == nil || doc.Tokens.AccessToken == "" {
		return codexLogin{}, false
	}
	l := codexLogin{token: doc.Tokens.AccessToken, accountID: doc.Tokens.AccountID}
	var access struct {
		Exp     float64 `json:"exp"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if jwtClaims(doc.Tokens.AccessToken, &access) && access.Exp > 0 {
		l.expires = time.Unix(int64(access.Exp), 0)
	}
	var id struct {
		Email string `json:"email"`
		Auth  struct {
			Plan      string `json:"chatgpt_plan_type"`
			UserID    string `json:"chatgpt_user_id"`
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	jwtClaims(doc.Tokens.IDToken, &id)
	l.email, l.plan, l.userID = id.Email, id.Auth.Plan, id.Auth.UserID
	if l.email == "" {
		l.email = access.Profile.Email
	}
	if l.accountID == "" {
		l.accountID = id.Auth.AccountID
	}
	return l, true
}

// jwtClaims decodes a JWT's claims into v without checking the signature: these tokens are only
// read for who they are and when they end.
func jwtClaims(token string, v any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(raw, v) == nil
}

// codexSpentReset reports whether a reply is a spent window: a 429 whose body has error.type
// "usage_limit_reached", until its resets_at (or resets_in_seconds from now). Zero when unsaid.
func codexSpentReset(status int, body []byte, now time.Time) (bool, time.Time) {
	if status != http.StatusTooManyRequests {
		return false, time.Time{}
	}
	var doc struct {
		Error struct {
			Type            string  `json:"type"`
			ResetsAt        float64 `json:"resets_at"`
			ResetsInSeconds float64 `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Error.Type != "usage_limit_reached" {
		return false, time.Time{}
	}
	switch {
	case doc.Error.ResetsAt > 0:
		return true, time.Unix(int64(doc.Error.ResetsAt), 0)
	case doc.Error.ResetsInSeconds > 0:
		return true, now.Add(time.Duration(doc.Error.ResetsInSeconds) * time.Second)
	}
	return true, time.Time{}
}

var codexUsedPercentHeader = regexp.MustCompile(`^X-Codex-(.+)-Used-Percent$`)

// codexHeaderWindows are the windows a reply's `x-codex-<window>-used-percent`,
// `-window-minutes` and `-reset-at` headers describe, by window id.
func codexHeaderWindows(h http.Header, now time.Time) []usageWindow {
	var out []usageWindow
	for key := range h {
		m := codexUsedPercentHeader.FindStringSubmatch(http.CanonicalHeaderKey(key))
		if m == nil {
			continue
		}
		used, err := strconv.ParseFloat(h.Get(key), 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			continue
		}
		minutes, _ := strconv.Atoi(h.Get("X-Codex-" + m[1] + "-Window-Minutes"))
		reset, _ := strconv.ParseInt(h.Get("X-Codex-"+m[1]+"-Reset-At"), 10, 64)
		out = append(out, newWindow(strings.ToLower(m[1]), used, reset, minutes, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// usedUp is when the latest used-up window (at 100%) resets; zero when none is used up. A used-up
// window that does not say when it resets counts from now (the pool then waits its fallback).
func usedUp(windows []usageWindow, now time.Time) time.Time {
	var until time.Time
	for _, w := range windows {
		if w.UsedPercent < 100 {
			continue
		}
		reset := now
		if w.ResetsAt != nil {
			reset = time.Unix(*w.ResetsAt, 0)
		}
		if reset.After(until) {
			until = reset
		}
	}
	return until
}

// whamWindows are the windows in a `GET /backend-api/wham/usage` reply, and its plan.
func whamWindows(raw []byte, now time.Time) ([]usageWindow, string, error) {
	type window struct {
		UsedPercent        float64 `json:"used_percent"`
		LimitWindowSeconds int     `json:"limit_window_seconds"`
		ResetAt            int64   `json:"reset_at"`
	}
	var doc struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "", err
	}
	out := []usageWindow{}
	if doc.RateLimit != nil {
		for _, w := range []struct {
			id string
			w  *window
		}{{"primary", doc.RateLimit.Primary}, {"secondary", doc.RateLimit.Secondary}} {
			if w.w != nil {
				out = append(out, newWindow(w.id, w.w.UsedPercent, w.w.ResetAt, w.w.LimitWindowSeconds/60, now))
			}
		}
	}
	return out, doc.PlanType, nil
}

// MARK: Accounts

type codexAccounts struct {
	home     string
	codex    string
	upstream string
	now      func() time.Time
	// refresh renews the sign-in in a Codex home through Codex itself; injected for tests.
	refresh func(dir string) error
	client  *http.Client

	mu        sync.Mutex
	refreshed map[string]time.Time
	usages    map[string]codexUsage
}

type codexUsage struct {
	read    time.Time
	windows []usageWindow
}

func newCodexAccounts(home, codex, upstream string) *codexAccounts {
	if codex == "" {
		codex, _ = exec.LookPath("codex")
	}
	return &codexAccounts{home: home, codex: codex, upstream: strings.TrimRight(upstream, "/"), now: time.Now,
		refresh: appServerRefresh(codex), client: &http.Client{Timeout: 10 * time.Second},
		refreshed: map[string]time.Time{}, usages: map[string]codexUsage{}}
}

func (a *codexAccounts) name() string { return "codex" }

func readCodexLogin(dir string) (codexLogin, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return codexLogin{}, false
	}
	return parseCodexLogin(raw)
}

// list is ~/.codex when it is a ChatGPT sign-in, then each ~/.codex-<name> that is one, by name.
func (a *codexAccounts) list() []account {
	var found []account
	add := func(id, dir, path string) {
		l, ok := readCodexLogin(path)
		if !ok {
			return
		}
		who := ""
		if l.userID != "" || l.accountID != "" {
			who = l.userID + "/" + l.accountID
		}
		found = append(found, account{ID: id, Email: l.email, Dir: dir, Path: path, Plan: l.plan, Who: who})
	}
	add(defaultID, "", filepath.Join(a.home, ".codex"))
	entries, _ := os.ReadDir(a.home)
	for _, e := range entries { // ReadDir sorts by name
		name := e.Name()
		if !strings.HasPrefix(name, ".codex-") || len(name) == len(".codex-") || !e.IsDir() {
			continue
		}
		path := filepath.Join(a.home, name)
		add(strings.TrimPrefix(name, ".codex-"), path, path)
	}
	return found
}

func (a *codexAccounts) serves(path string) bool { return strings.HasPrefix(path, "/backend-api/") }

func (a *codexAccounts) spends(path string) bool {
	return strings.HasPrefix(path, "/backend-api/codex/")
}

// authorize keeps the client's own credentials for the default account; a named one gets its
// login's token and account id, refreshed first when it ends within codexRefreshWithin.
func (a *codexAccounts) authorize(out *http.Request, acc account) bool {
	if acc.Dir == "" {
		return true
	}
	l, ok := readCodexLogin(acc.Path)
	if !ok || l.expires.Before(a.now().Add(codexRefreshWithin)) {
		if a.renew(acc) {
			l, ok = readCodexLogin(acc.Path)
		}
	}
	if !ok || !l.expires.After(a.now()) {
		return false
	}
	out.Header.Set("Authorization", "Bearer "+l.token)
	out.Header.Set("Chatgpt-Account-Id", l.accountID)
	return true
}

// renew refreshes a named login through Codex, at most once per codexRefreshEvery; true when it did.
func (a *codexAccounts) renew(acc account) bool {
	if acc.Dir == "" || a.refresh == nil {
		return false
	}
	a.mu.Lock()
	if last, ok := a.refreshed[acc.ID]; ok && a.now().Sub(last) < codexRefreshEvery {
		a.mu.Unlock()
		return false
	}
	a.refreshed[acc.ID] = a.now()
	a.mu.Unlock()
	if err := a.refresh(acc.Path); err != nil {
		log.Printf("codex account %s: refresh failed: %v", acc.ID, err)
		return false
	}
	log.Printf("codex account %s refreshed through codex app-server", acc.ID)
	return true
}

func (a *codexAccounts) spent(status int, _ http.Header, body []byte) (bool, time.Time) {
	return codexSpentReset(status, body, a.now())
}

// observe keeps the windows a reply's headers show (fresher than a usage read) and says when the
// account is used up ahead of being refused.
func (a *codexAccounts) observe(acc account, h http.Header) time.Time {
	now := a.now()
	windows := codexHeaderWindows(h, now)
	if len(windows) == 0 {
		return time.Time{}
	}
	a.mu.Lock()
	a.usages[acc.ID] = codexUsage{read: now, windows: windows}
	a.mu.Unlock()
	return usedUp(windows, now)
}

// unauthorized: on the default account the 401 is the client's own login, which Codex refreshes
// itself, so it goes back. A named login is refreshed and the request sent again.
func (a *codexAccounts) unauthorized(acc account) (retry, handBack bool) {
	if acc.Dir == "" {
		return false, true
	}
	log.Printf("codex account %s auth rejected (401); refreshing it", acc.ID)
	return a.renew(acc), false
}

// usage is the account's windows from `GET /backend-api/wham/usage` (it spends nothing), read at
// most once per codexUsageEvery; a failed read keeps the last windows. It never refreshes a login.
func (a *codexAccounts) usage(acc account) (map[string]usageValue, []usageWindow) {
	now := a.now()
	a.mu.Lock()
	cached, ok := a.usages[acc.ID]
	if ok && now.Sub(cached.read) < codexUsageEvery {
		a.mu.Unlock()
		return nil, cached.windows
	}
	a.usages[acc.ID] = codexUsage{read: now, windows: cached.windows}
	a.mu.Unlock()
	windows, err := a.readUsage(acc)
	if err != nil {
		log.Printf("codex account %s: usage read failed: %v", acc.ID, err)
		return nil, cached.windows
	}
	a.mu.Lock()
	a.usages[acc.ID] = codexUsage{read: now, windows: windows}
	a.mu.Unlock()
	return nil, windows
}

func (a *codexAccounts) readUsage(acc account) ([]usageWindow, error) {
	l, ok := readCodexLogin(acc.Path)
	if !ok {
		return nil, errors.New("no sign-in")
	}
	req, err := http.NewRequest(http.MethodGet, a.upstream+codexUsagePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	req.Header.Set("Chatgpt-Account-Id", l.accountID)
	req.Header.Set("User-Agent", "limitbreak")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	windows, _, err := whamWindows(raw, a.now())
	return windows, err
}

// MARK: Refresh through Codex

// codexEnv is the environment for running `codex` on the login in dir.
func codexEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CODEX_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "CODEX_HOME="+dir)
}

// appServerRefresh renews the sign-in in a Codex home by asking `codex app-server` for the
// account with `refreshToken`; Codex refreshes it and writes auth.json itself.
func appServerRefresh(codex string) func(dir string) error {
	return func(dir string) error {
		if codex == "" {
			return errors.New("codex is not on PATH")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, codex, "app-server")
		cmd.Env, cmd.Dir = codexEnv(dir), dir
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		answer := appServerAccountRead(stdin, stdout)
		// Closing its input ends the app-server; the context kills one that hangs.
		stdin.Close()
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
		return answer
	}
}

// appServerAccountRead speaks the app-server's stdio protocol, JSON-RPC messages one per line
// (without a "jsonrpc" member): initialize, initialized, then `account/read` with
// `{"refreshToken": true}`, and waits for that reply.
func appServerAccountRead(in io.Writer, out io.Reader) error {
	for _, message := range []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"limitbreak","version":"1"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"account/read","params":{"refreshToken":true}}`,
	} {
		if _, err := io.WriteString(in, message+"\n"); err != nil {
			return err
		}
	}
	lines := bufio.NewScanner(out)
	lines.Buffer(make([]byte, 64*1024), 16<<20)
	for lines.Scan() {
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Result *struct {
				Account *struct {
					Type string `json:"type"`
				} `json:"account"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(lines.Bytes(), &reply) != nil || string(reply.ID) != "2" {
			continue
		}
		switch {
		case reply.Error != nil:
			return fmt.Errorf("account/read: %s", reply.Error.Message)
		case reply.Result == nil || reply.Result.Account == nil:
			return errors.New("account/read: not signed in")
		}
		return nil
	}
	return errors.New("codex app-server ended without answering")
}
