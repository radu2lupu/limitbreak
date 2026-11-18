package main

// The Grok provider: the Grok Build (`grok`) sign-ins on this host. The default account is ~/.grok,
// which belongs to grok itself: Limit Break never refreshes or rewrites it, and a request on it keeps
// the client's own credentials. The others are ~/.grok-<name> dirs (a separate GROK_HOME each) whose
// auth.json holds a session login; a request on one has its token and who-headers swapped for that
// login's. Such a login is refreshed by grok itself (`GROK_HOME=<dir> grok models`), never by calling
// the sign-in server from here.
//
// grok (with `GROK_CLI_CHAT_PROXY_BASE_URL=http://127.0.0.1:48617/_c/<client>/grok/v1`) sends every
// backend request to that base: models, settings, storage, traces, sessions and the model turns,
// `POST /v1/responses` (an OpenAI Responses-style SSE stream; zstd body). The `/grok` mount comes off
// and they go on to https://cli-chat-proxy.grok.com. Only the model turns are put on an account; the
// rest keep grok's own login. A 402, or a 403 or 429 whose body says the plan's usage, credits or
// spending cap ran out, marks the account spent and the request goes again on the next account.
// Usage per account is `GET /v1/billing?format=credits` (it spends nothing).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGrokUpstream = "https://cli-chat-proxy.grok.com"
	// grokMount is where clients reach Grok's upstream: its /v1/… paths are Claude's too.
	grokMount        = "/grok"
	grokBillingPath  = "/v1/billing?format=credits"
	grokTokenAuth    = "xai-grok-cli"
	grokTokenAuthKey = "X-Xai-Token-Auth"
	// grokRefreshWithin is when a named login's access token (about 6 hours long) is refreshed
	// before it ends.
	grokRefreshWithin = time.Hour
	// grokRefreshEvery is the least time between two refreshes of one login.
	grokRefreshEvery = 5 * time.Minute
	// grokUsageEvery is the least time between two usage reads of one account.
	grokUsageEvery = 5 * time.Minute
)

// grokWhoHeaders are the request headers grok puts its login's identity in, by the login field
// they carry. A request on a named account has each one it carries set to that login's.
var grokWhoHeaders = map[string]func(grokLogin) string{
	"X-Email":        func(l grokLogin) string { return l.email },
	"X-Userid":       func(l grokLogin) string { return l.userID },
	"X-Grok-User-Id": func(l grokLogin) string { return l.userID },
	"X-Teamid":       func(l grokLogin) string { return l.teamID },
}

// grokSpentPhrases are what a refusal's body says when the plan's usage, credits or spending cap
// ran out, in grok's own words for those errors (lowercased). A plain rate limit says none of them.
var grokSpentPhrases = []string{
	"usage limit", "usage_limit", "weekly limit", "monthly limit", "spending limit", "spending-limit",
	"spending cap", "out of credits", "run out of credits", "usage balance exhausted", "usage_pool_exhausted",
	"credit limit", "credit_limit",
}

// MARK: Decisions (pure)

// grokLogin is what a request on a Grok sign-in needs, and who it is.
type grokLogin struct {
	token string
	// expires is when the access token ends; zero when it does not say.
	expires time.Time
	email   string
	userID  string
	teamID  string
}

// parseGrokLogin reads grok's auth.json, a map of logins by issuer: the one with a token that lasts
// longest. False when there is none.
func parseGrokLogin(raw []byte) (grokLogin, bool) {
	var doc map[string]struct {
		Key       string `json:"key"`
		ExpiresAt string `json:"expires_at"`
		Email     string `json:"email"`
		UserID    string `json:"user_id"`
		TeamID    string `json:"team_id"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return grokLogin{}, false
	}
	var best grokLogin
	found := false
	for _, e := range doc {
		if e.Key == "" {
			continue
		}
		l := grokLogin{token: e.Key, email: e.Email, userID: e.UserID, teamID: e.TeamID}
		if t, err := time.Parse(time.RFC3339Nano, e.ExpiresAt); err == nil {
			l.expires = t
		} else {
			var claims struct {
				Exp float64 `json:"exp"`
			}
			if jwtClaims(e.Key, &claims) && claims.Exp > 0 {
				l.expires = time.Unix(int64(claims.Exp), 0)
			}
		}
		if !found || l.expires.After(best.expires) {
			best, found = l, true
		}
	}
	return best, found
}

// grokSpentReset reports whether a reply is a spent plan: a 402 (grok: "run out of credits"), or a
// 403 or 429 whose body says the usage, credits or spending cap ran out. It lifts at the body's
// resets_at (unix seconds) or Retry-After when either says; zero when unsaid.
func grokSpentReset(status int, h http.Header, body []byte, now time.Time) (bool, time.Time) {
	switch status {
	case http.StatusPaymentRequired:
	case http.StatusForbidden, http.StatusTooManyRequests:
		text := strings.ToLower(string(body))
		matched := false
		for _, phrase := range grokSpentPhrases {
			if strings.Contains(text, phrase) {
				matched = true
				break
			}
		}
		if !matched {
			return false, time.Time{}
		}
	default:
		return false, time.Time{}
	}
	var doc struct {
		ResetsAt float64 `json:"resets_at"`
		Error    struct {
			ResetsAt float64 `json:"resets_at"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &doc)
	switch {
	case doc.Error.ResetsAt > 0:
		return true, time.Unix(int64(doc.Error.ResetsAt), 0)
	case doc.ResetsAt > 0:
		return true, time.Unix(int64(doc.ResetsAt), 0)
	}
	if after := h.Get("Retry-After"); after != "" {
		if seconds, err := strconv.Atoi(after); err == nil && seconds > 0 {
			return true, now.Add(time.Duration(seconds) * time.Second)
		}
		if at, err := http.ParseTime(after); err == nil {
			return true, at
		}
	}
	return true, time.Time{}
}

// grokBillingWindows is the window in a `GET /v1/billing?format=credits` reply: the plan's credit
// use in its current period (weekly on SuperGrok), named by the period's type.
func grokBillingWindows(raw []byte, now time.Time) ([]usageWindow, error) {
	var doc struct {
		Config *struct {
			CurrentPeriod *struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
			CreditUsagePercent *float64 `json:"creditUsagePercent"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := []usageWindow{}
	if doc.Config == nil || doc.Config.CreditUsagePercent == nil {
		return out, nil
	}
	id, minutes, reset := "period", 0, int64(0)
	if p := doc.Config.CurrentPeriod; p != nil {
		if t := strings.ToLower(strings.TrimPrefix(p.Type, "USAGE_PERIOD_TYPE_")); t != "" {
			id = t
		}
		start, startErr := time.Parse(time.RFC3339Nano, p.Start)
		end, endErr := time.Parse(time.RFC3339Nano, p.End)
		if endErr == nil {
			reset = end.Unix()
			if startErr == nil {
				minutes = int(end.Sub(start).Round(time.Minute) / time.Minute)
			}
		}
	}
	return append(out, newWindow(id, *doc.Config.CreditUsagePercent, reset, minutes, now)), nil
}

// MARK: Accounts

type grokAccounts struct {
	home     string
	grok     string
	upstream string
	now      func() time.Time
	// refresh renews the sign-in in a Grok home through grok itself; injected for tests.
	refresh func(dir string) error
	client  *http.Client

	refreshed loginRenewal
	usages    usageCache
}

func newGrokAccounts(home, grok, upstream string) *grokAccounts {
	if grok == "" {
		grok = findGrok(home)
	}
	return &grokAccounts{home: home, grok: grok, upstream: strings.TrimRight(upstream, "/"), now: time.Now,
		refresh: grokModelsRefresh(grok), client: &http.Client{Timeout: 10 * time.Second}}
}

// findGrok is grok on PATH, else where its installer puts it (~/.grok/bin/grok), which a launchd
// service's PATH does not have.
func findGrok(home string) string {
	return findInstalled("grok", filepath.Join(home, ".grok", "bin", "grok"))
}

// findInstalled is the tool name on PATH, else the first of paths that is a file; empty when none is.
func findInstalled(name string, paths ...string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func (a *grokAccounts) name() string { return "grok" }

func (a *grokAccounts) mount() string { return grokMount }

func readGrokLogin(dir string) (grokLogin, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return grokLogin{}, false
	}
	return parseGrokLogin(raw)
}

// list is ~/.grok when it has a login, then each ~/.grok-<name> that has one, by name.
func (a *grokAccounts) list() []account {
	var found []account
	add := func(id, dir, path string) {
		l, ok := readGrokLogin(path)
		if !ok {
			return
		}
		who := ""
		if l.userID != "" {
			who = l.userID + "/" + l.teamID
		}
		found = append(found, account{ID: id, Email: l.email, Dir: dir, Path: path, Who: who})
	}
	add(defaultID, "", filepath.Join(a.home, ".grok"))
	entries, _ := os.ReadDir(a.home)
	for _, e := range entries { // ReadDir sorts by name
		name := e.Name()
		if !strings.HasPrefix(name, ".grok-") || len(name) == len(".grok-") || !e.IsDir() {
			continue
		}
		path := filepath.Join(a.home, name)
		add(strings.TrimPrefix(name, ".grok-"), path, path)
	}
	return found
}

func (a *grokAccounts) serves(path string) bool { return strings.HasPrefix(path, grokMount+"/") }

// spends is a model turn; path is the upstream one (without the mount).
func (a *grokAccounts) spends(path string) bool {
	for _, turn := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		if path == turn || strings.HasPrefix(path, turn+"/") || strings.HasPrefix(path, turn+"?") {
			return true
		}
	}
	return false
}

// authorize keeps the client's own credentials for the default account; a named one gets its
// login's token and identity headers, refreshed first when it ends within grokRefreshWithin.
func (a *grokAccounts) authorize(out *http.Request, acc account) bool {
	if acc.Dir == "" {
		return true
	}
	l, ok := readGrokLogin(acc.Path)
	if !ok || l.expires.Before(a.now().Add(grokRefreshWithin)) {
		if a.renew(acc) {
			l, ok = readGrokLogin(acc.Path)
		}
	}
	if !ok || !l.expires.After(a.now()) {
		return false
	}
	out.Header.Set("Authorization", "Bearer "+l.token)
	out.Header.Set(grokTokenAuthKey, grokTokenAuth)
	for header, value := range grokWhoHeaders {
		if out.Header.Get(header) != "" {
			out.Header.Set(header, value(l))
		}
	}
	return true
}

// renew refreshes a named login through grok, at most once per grokRefreshEvery; true when it did.
func (a *grokAccounts) renew(acc account) bool {
	return a.refreshed.renew(a.name(), "grok models", acc, a.now(), grokRefreshEvery, a.refresh)
}

func (a *grokAccounts) spent(status int, h http.Header, body []byte) (bool, time.Time) {
	return grokSpentReset(status, h, body, a.now())
}

// observe: Grok's replies carry request rate limits (x-ratelimit-*), not the plan's usage.
func (a *grokAccounts) observe(account, http.Header) time.Time { return time.Time{} }

// unauthorized: on the default account the 401 is the client's own login, which grok refreshes
// itself, so it goes back. A named login is refreshed and the request sent again.
func (a *grokAccounts) unauthorized(acc account) (retry, handBack bool) {
	if acc.Dir == "" {
		return false, true
	}
	log.Printf("grok account %s auth rejected (401); refreshing it", acc.ID)
	return a.renew(acc), false
}

// usage is the account's window from `GET /v1/billing?format=credits` (it spends nothing), read
// at most once per grokUsageEvery; a failed read keeps the last one. It never refreshes a login.
func (a *grokAccounts) usage(acc account) (map[string]usageValue, []usageWindow) {
	windows, err := a.usages.get(acc.ID, a.now(), grokUsageEvery, func() ([]usageWindow, error) { return a.readUsage(acc) })
	if err != nil {
		log.Printf("grok account %s: usage read failed: %v", acc.ID, err)
	}
	return nil, windows
}

func (a *grokAccounts) readUsage(acc account) ([]usageWindow, error) {
	l, ok := readGrokLogin(acc.Path)
	if !ok {
		return nil, errors.New("no sign-in")
	}
	if !l.expires.IsZero() && !l.expires.After(a.now()) {
		return nil, errors.New("token ended; grok refreshes it on its next run")
	}
	raw, err := fetchUsage(a.client, a.upstream+grokBillingPath, map[string]string{"Authorization": "Bearer " + l.token, grokTokenAuthKey: grokTokenAuth})
	if err != nil {
		return nil, err
	}
	return grokBillingWindows(raw, a.now())
}

// MARK: Refresh through grok

// grokEnv is the environment for running `grok` on the login in dir (its own GROK_HOME), without
// this proxy, an API key or another auth file in the way.
func grokEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GROK_HOME", "GROK_AUTH", "GROK_AUTH_PATH", "GROK_CLI_CHAT_PROXY_BASE_URL", "XAI_API_KEY", "GROK_CODE_XAI_API_KEY",
			"GROK_AUTH_EARLY_INVALIDATION_SECS":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GROK_HOME="+dir)
}

// grokModelsRefresh renews the sign-in in a Grok home by running `grok models` there with the
// refresh window wider than a token lasts, so grok refreshes the login before it lists the models
// and writes auth.json itself. The refresh worked when the token now ends later than before.
func grokModelsRefresh(grok string) func(dir string) error {
	return func(dir string) error {
		if grok == "" {
			return errors.New("grok is not installed")
		}
		before, _ := readGrokLogin(dir)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, grok, "models")
		cmd.Env = append(grokEnv(dir), "GROK_AUTH_EARLY_INVALIDATION_SECS=86400")
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("grok models: %v", err)
		}
		after, ok := readGrokLogin(dir)
		if !ok || !after.expires.After(before.expires) {
			if strings.Contains(string(output), "not authenticated") {
				return errors.New("signed out; run limitbreak add --provider grok again")
			}
			return errors.New("grok did not renew the login")
		}
		return nil
	}
}
