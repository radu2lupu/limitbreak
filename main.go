// limitbreak: one `claude` install, several logins, switched without a restart.
//
// Run it on a host (Mac or Linux) and start Claude Code with
//
//	ANTHROPIC_BASE_URL=http://127.0.0.1:48617
//
// Claude Code keeps its own login (so its MCP servers, connectors and artifacts keep working).
// Every messages request goes to Anthropic with the active account's own access token. When Anthropic
// answers 429 with `anthropic-ratelimit-unified-status: rejected` (the account's 5-hour or weekly
// window is spent) the account is recorded spent until `anthropic-ratelimit-unified-reset` and the
// same request is sent again on the next account, before any reply reaches Claude. The session and
// its history never change.
//
// Accounts are the logins Claude Code already holds on the host: the default one and every
// `~/.claude-<name>` config dir with a login. Tokens are read, never copied or logged: from the
// macOS Keychain item Claude Code keeps per config dir, or from `<dir>/.credentials.json`. An
// expired token is refreshed by running one tiny `claude -p` under that dir, so Claude Code stays
// the only thing that refreshes a login. An account with a 1-year token from `limitbreak renew`
// (`claude setup-token`) is sent on that token instead, so its monthly sign-in end stops mattering.
//
// A path may start with a client name (`/_c/<client>/v1/messages`); each client has its own active
// account. Any other path is the shared client's. Requests that are not the messages API are
// forwarded untouched with Claude Code's own credentials. `GET /_status` lists the accounts (who they
// are, spent-until, auth end, last usage, active-for clients); `PUT /_c/<client>/_active` with
// `{"id":"<account>"}` sets a client's active account.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// neverExpires stands for a login that states no expiry (epoch milliseconds).
	neverExpires        = 4_000_000_000_000
	defaultPort         = 48617
	defaultUpstream     = "https://api.anthropic.com"
	sharedClient        = "shared"
	activePath          = "/_active"
	fallbackExhaustion  = time.Hour
	unavailableBackoff  = 5 * time.Minute
	discoveryInterval   = 30 * time.Second
	defaultID           = "default"
	spentHeader         = "Anthropic-Ratelimit-Unified-Status"
	resetHeader         = "Anthropic-Ratelimit-Unified-Reset"
	credentialsKeychain = "keychain"
	credentialsFile     = "file"
)

// MARK: Decisions (pure)

// spentReset reports whether a reply is a spent window, and when it lifts (zero when unsaid). A
// plain rate limit (429 without the unified status) clears in seconds and goes back untouched.
func spentReset(status int, h http.Header) (spent bool, reset time.Time) {
	if status != http.StatusTooManyRequests || !strings.EqualFold(h.Get(spentHeader), "rejected") {
		return false, time.Time{}
	}
	if seconds, err := strconv.ParseFloat(h.Get(resetHeader), 64); err == nil {
		return true, time.Unix(int64(seconds), 0)
	}
	return true, time.Time{}
}

// route is the client and upstream path of a request target. A client is named by a
// `/_c/<client>` prefix; any other path is the shared client's and goes upstream as it is.
func route(target string) (client, path string, ok bool) {
	if rest, found := strings.CutPrefix(target, "/_c/"); found {
		slash := strings.Index(rest, "/")
		if slash <= 0 {
			return "", "", false
		}
		return rest[:slash], rest[slash:], true
	}
	if strings.HasPrefix(target, "/") {
		return sharedClient, target, true
	}
	return "", "", false
}

// keychainService is the one rule for a login's Keychain item: `Claude Code-credentials` plus the
// first 8 hex of sha256 of its config dir; the default login (empty dir) has no suffix.
func keychainService(configDir string) string {
	if configDir == "" {
		return "Claude Code-credentials"
	}
	sum := sha256.Sum256([]byte(configDir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// nextAccount is the next account after current that is not spent; the first usable one when
// current is unknown. Nil when every account is spent.
func nextAccount(order []account, current string, spentUntil map[string]time.Time, now time.Time) *account {
	start := -1
	for i, a := range order {
		if a.ID == current {
			start = i
		}
	}
	for offset := 1; offset <= len(order); offset++ {
		candidate := order[(start+offset)%len(order)]
		if !spentUntil[candidate.ID].After(now) {
			return &candidate
		}
	}
	return nil
}

// MARK: Claude accounts

type account struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	// Dir is the config dir of a non-default login; empty for the default one.
	Dir  string `json:"-"`
	Path string `json:"-"`
	// AuthEnds is when the account stops being usable without the user (see authEnd); zero when
	// no source states it. Read at discovery, so `GET /_status` does not ask the Keychain.
	AuthEnds time.Time `json:"-"`
}

// claudeAccounts is the Claude provider: the logins Claude Code holds on this host.
type claudeAccounts struct {
	home        string
	credentials string
	claude      string
	now         func() time.Time
	// mu serializes writes to the usage record.
	mu sync.Mutex
}

func newClaudeAccounts(home, credentials, claude string) *claudeAccounts {
	if credentials == "auto" {
		credentials = credentialsFile
		if runtime.GOOS == "darwin" {
			credentials = credentialsKeychain
		}
	}
	if claude == "" {
		claude, _ = exec.LookPath("claude")
	}
	return &claudeAccounts{home: home, credentials: credentials, claude: claude, now: time.Now}
}

// list is every Claude login on the host, read now: the default one first, then each
// `~/.claude-<name>` dir that holds a login, by name.
func (a *claudeAccounts) list() []account {
	var found []account
	var sources [][][]byte
	defaultDir := filepath.Join(a.home, ".claude")
	if s := a.credentialSources("", defaultDir); len(s) > 0 {
		found = append(found, account{ID: defaultID, Path: defaultDir})
		sources = append(sources, s)
	}
	entries, _ := os.ReadDir(a.home)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(a.home, name)
		if !strings.HasPrefix(name, ".claude-") || len(name) == len(".claude-") {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		if s := a.credentialSources(path, path); len(s) > 0 {
			found = append(found, account{ID: strings.TrimPrefix(name, ".claude-"), Dir: path, Path: path})
			sources = append(sources, s)
		}
	}
	for i := range found {
		found[i].Email = a.email(found[i])
		found[i].AuthEnds, _ = a.authInfoFrom(found[i], sources[i])
	}
	return found
}

func (a *claudeAccounts) email(acc account) string {
	path := filepath.Join(acc.Path, ".claude.json")
	if acc.Dir == "" {
		path = filepath.Join(a.home, ".claude.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	_ = json.Unmarshal(raw, &doc)
	return doc.OAuthAccount.EmailAddress
}

func keychainQueries(configDir string) [][]string {
	service := keychainService(configDir)
	queries := [][]string{{"find-generic-password", "-s", service, "-w"}}
	if me, err := user.Current(); err == nil && me.Username != "" {
		queries = append([][]string{{"find-generic-password", "-a", me.Username, "-s", service, "-w"}}, queries...)
	}
	return queries
}

// credentialSources are the credential documents Claude Code may hold for a login: its Keychain
// item and its file. Either can be the stale leftover of an older sign-in, so every one is read.
func (a *claudeAccounts) credentialSources(configDir, path string) [][]byte {
	var sources [][]byte
	if a.credentials == credentialsKeychain {
		// Claude Code files its item under the user's name; other items with the same service
		// (an older sign-in under another account name) can be stale, so the user's is asked for
		// first and any other is kept as a second source.
		for _, args := range keychainQueries(configDir) {
			out, err := exec.Command("security", args...).Output()
			if err == nil && len(bytes.TrimSpace(out)) > 0 {
				sources = append(sources, out)
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(path, ".credentials.json")); err == nil {
		sources = append(sources, raw)
	}
	return sources
}

// token is a live access token, refreshed through Claude Code when every one has expired.
func (a *claudeAccounts) token(acc account) string {
	if live := a.readToken(acc); live != "" {
		return live
	}
	a.refresh(acc)
	return a.readToken(acc)
}

// readToken is the account's unexpired long-lived token when it has one (see longtoken.go), else
// the access token that has not expired and lasts longest, across the login's sources.
func (a *claudeAccounts) readToken(acc account) string {
	if long, ok := a.readLongToken(acc); ok && long.valid(a.now()) {
		return long.Token
	}
	var best string
	bestExpiry := -1.0
	for _, raw := range a.credentialSources(acc.Dir, acc.Path) {
		l, ok := parseLogin(raw)
		if !ok {
			continue
		}
		expiry := float64(neverExpires)
		if l.ExpiresAt != nil {
			expiry = *l.ExpiresAt
			if time.UnixMilli(int64(expiry)).Before(a.now()) {
				continue
			}
		}
		if l.AccessToken != "" && expiry > bestExpiry {
			best, bestExpiry = l.AccessToken, expiry
		}
	}
	return best
}

type login struct {
	AccessToken string   `json:"accessToken"`
	ExpiresAt   *float64 `json:"expiresAt"`
	// RefreshTokenExpiresAt is the sign-in's hard end (epoch ms); refreshing does not move it.
	RefreshTokenExpiresAt *float64 `json:"refreshTokenExpiresAt"`
}

// parseLogin reads a credential document: Claude Code's `{"claudeAiOauth": {...}}` or a bare login.
func parseLogin(raw []byte) (login, bool) {
	var doc struct {
		ClaudeAiOauth *login `json:"claudeAiOauth"`
		login
	}
	if json.Unmarshal(raw, &doc) != nil {
		return login{}, false
	}
	if doc.ClaudeAiOauth != nil {
		return *doc.ClaudeAiOauth, true
	}
	return doc.login, true
}

// claudeEnv is the environment for running `claude` as the login in configDir (empty: the default
// login), without this proxy or an injected token in the way.
func claudeEnv(configDir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") && !strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH_TOKEN=") {
			env = append(env, kv)
		}
	}
	if configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+configDir, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+configDir)
	}
	return env
}

func (a *claudeAccounts) refresh(acc account) {
	if a.claude == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.claude, "-p", "ok", "--model", "haiku")
	cmd.Stdin = nil
	cmd.Env = claudeEnv(acc.Dir)
	_ = cmd.Run()
}

// The Claude provider (see pool.go for the interface).

func (a *claudeAccounts) name() string { return "claude" }

// serves is every path: Claude's upstream answers whatever no other provider serves.
func (a *claudeAccounts) serves(string) bool { return true }

func (a *claudeAccounts) spends(path string) bool { return isInference(path) }

func (a *claudeAccounts) authorize(out *http.Request, acc account) bool {
	token := a.token(acc)
	if token == "" {
		return false
	}
	out.Header.Del("X-Api-Key")
	out.Header.Set("Authorization", "Bearer "+token)
	return true
}

func (a *claudeAccounts) spent(status int, h http.Header, _ []byte) (bool, time.Time) {
	return spentReset(status, h)
}

func (a *claudeAccounts) observe(acc account, h http.Header) time.Time {
	a.recordUsage(acc, h)
	return time.Time{}
}

// unauthorized: the account's token is dead (revoked, or its auth ended). Claude must not see that
// as its own login failing, so the account is skipped for a while, like a spent one.
func (a *claudeAccounts) unauthorized(acc account) (retry, handBack bool) {
	log.Printf("account %s auth rejected (401); run \"limitbreak renew %s\"", acc.ID, acc.ID)
	return false, false
}

func (a *claudeAccounts) usage(acc account) map[string]usageValue {
	records, err := readUsage(a.home)
	if err != nil {
		log.Printf("usage record read: %v", err)
	}
	return usageFor(records, acc)
}

// MARK: Proxy

type proxy struct {
	// pools are the providers' accounts; the first one answers every path no other one serves.
	pools  []*pool
	client *http.Client
}

func newProxy(pools ...*pool) *proxy {
	return &proxy{pools: pools,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
}

// poolFor is the pool whose provider serves path; the first pool when no other one does.
func (p *proxy) poolFor(path string) *pool {
	for _, pl := range p.pools[1:] {
		if pl.provider.serves(path) {
			return pl
		}
	}
	return p.pools[0]
}

func (p *proxy) status() []accountStatus {
	out := []accountStatus{}
	for _, pl := range p.pools {
		out = append(out, pl.status()...)
	}
	return out
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/_status" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.status())
		return
	}
	clientName, path, ok := route(r.URL.RequestURI())
	if !ok {
		apiError(w, http.StatusNotFound, "unknown path")
		return
	}
	if path == activePath && clientName != sharedClient {
		p.serveActive(w, r, clientName)
		return
	}
	pl := p.poolFor(path)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	if !pl.provider.spends(path) {
		p.passThrough(w, r, pl.upstream+path, body)
		return
	}
	renewed := map[string]bool{}
	for attempts := len(pl.list()) + 1; attempts > 0; attempts-- {
		acc := pl.current(clientName)
		if acc == nil {
			// Nothing to switch to (no login, or every account spent): the client's own login answers.
			p.passThrough(w, r, pl.upstream+path, body)
			return
		}
		out, err := upstreamRequest(r, pl.upstream+path, body)
		if err != nil {
			apiError(w, http.StatusBadRequest, "bad path")
			return
		}
		if !pl.provider.authorize(out, *acc) {
			// A login that cannot give a token is skipped for a while, like a spent one.
			if pl.spend(clientName, acc.ID, pl.now().Add(unavailableBackoff)) != nil {
				continue
			}
			p.passThrough(w, r, pl.upstream+path, body)
			return
		}
		resp, err := p.client.Do(out)
		if err != nil {
			apiError(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		spentAhead := pl.provider.observe(*acc, resp.Header)
		if resp.StatusCode == http.StatusUnauthorized {
			retry, handBack := pl.provider.unauthorized(*acc)
			if !handBack {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if retry && !renewed[acc.ID] {
					renewed[acc.ID] = true
					attempts++
					continue
				}
				if pl.spend(clientName, acc.ID, pl.now().Add(unavailableBackoff)) != nil {
					continue
				}
				p.passThrough(w, r, pl.upstream+path, body)
				return
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			// The reason a 429 is a spent window may be in its body; it is short, so it is read
			// here and put back in front of the rest for the client.
			peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
			if spent, reset := pl.provider.spent(resp.StatusCode, resp.Header, peek); spent {
				if pl.spend(clientName, acc.ID, reset) != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					continue
				}
				spentAhead = time.Time{}
			}
		}
		relay(w, resp)
		if !spentAhead.IsZero() {
			// This reply got through, but it says a window is now used up: the next request goes
			// to the next account rather than waiting to be refused.
			pl.spend(clientName, acc.ID, spentAhead)
		}
		return
	}
	p.passThrough(w, r, pl.upstream+path, body)
}

// serveActive answers `PUT /_c/<client>/_active` with `{"id":"<account id>"}`: the client's active
// account becomes that one (it still moves on when that account is spent). 404 for an unknown id.
func (p *proxy) serveActive(w http.ResponseWriter, r *http.Request, client string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		apiError(w, http.StatusMethodNotAllowed, "use PUT")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.ID == "" {
		apiError(w, http.StatusBadRequest, `body must be {"id":"<account id>"}`)
		return
	}
	if !p.pools[0].setActive(client, body.ID) {
		apiError(w, http.StatusNotFound, "no account "+body.ID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.status())
}

// A proxy that cannot place a request on an account sends it on as the client made it: it is
// never worse than no proxy.

// isInference is a request that spends an account's usage: the messages API. Everything else
// (claude.ai connectors, profile, MCP, artifacts) belongs to the login Claude Code itself holds.
func isInference(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

// upstreamRequest is the client's request addressed to target, with its own headers (its
// credentials included) less those that belong to one connection. The reply is asked for
// unencoded, so it can be relayed as it streams.
func upstreamRequest(r *http.Request, target string, body []byte) (*http.Request, error) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range r.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Host", "Content-Length", "Connection", "Accept-Encoding", "Transfer-Encoding", "Proxy-Connection", "Keep-Alive", "Expect":
			continue
		}
		out.Header[key] = values
	}
	out.Header.Set("Accept-Encoding", "identity")
	return out, nil
}

// passThrough sends the request upstream exactly as the client made it, with its own credentials.
func (p *proxy) passThrough(w http.ResponseWriter, r *http.Request, target string, body []byte) {
	out, err := upstreamRequest(r, target, body)
	if err != nil {
		apiError(w, http.StatusBadRequest, "bad path")
		return
	}
	resp, err := p.client.Do(out)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	relay(w, resp)
}

// relay sends the reply on as it arrives, so a stream of events is not held back.
func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for key, values := range resp.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Content-Length", "Transfer-Encoding", "Connection", "Content-Encoding":
			continue
		}
		w.Header()[key] = values
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && os.Getenv(loginHintEnv) != "" && strings.HasPrefix(args[0], "http") {
		os.Exit(openBrowser(args[0]))
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		os.Exit(runCommand(args[0], args[1:]))
	}
	serve(args)
}

func serve(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	port := flags.Int("port", envInt("LIMITBREAK_PORT", defaultPort), "port to listen on (loopback only)")
	upstream := flags.String("upstream", envString("LIMITBREAK_UPSTREAM", defaultUpstream), "Anthropic API address")
	home, _ := os.UserHomeDir()
	homeFlag := flags.String("home", home, "home dir holding the Claude logins")
	credentials := flags.String("credentials", "auto", "auto, keychain or file")
	_ = flags.Parse(args)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("limitbreak listening on http://%s", listener.Addr())
	a := newClaudeAccounts(*homeFlag, *credentials, "")
	go newAuthWatch(a, notifyDesktop).run()
	log.Fatal(http.Serve(listener, newProxy(newPool(a, *upstream))))
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return fallback
}
