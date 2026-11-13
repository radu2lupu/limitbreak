package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const usage = `limitbreak [serve flags]        run the proxy (default)
limitbreak add [name]           sign in a new account (opens the browser)
limitbreak list                 show the accounts and when each one's login ends
limitbreak renew [name...] [--all]
                                give accounts a 1-year token (one browser authorization each)
limitbreak share <ssh-host>     copy the 1-year tokens to the same-named accounts on another machine
limitbreak remove <name> [-y]   sign an account out and delete its login
limitbreak video                watch a switch happen as an arcade battle (simulated, no real login touched)

add and remove take --provider claude|codex (default claude) and --json (one JSON event per
line, for apps; remove --json needs --yes).

The default account is the one plain "claude auth login" signs in (for Codex, "codex login").`

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func runCommand(command string, args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	a := newClaudeAccounts(home, "auto", "")
	switch command {
	case "serve":
		serve(args)
		return 0
	case "add", "remove":
		err = accountCommand(command, args, a, newCodexAccounts(home, "", envString("LIMITBREAK_CODEX_UPSTREAM", defaultCodexUpstream)))
	case "video":
		err = runVideo(os.Stdout)
	case "list":
		listAccounts([]loginProvider{a, newCodexAccounts(home, "", envString("LIMITBREAK_CODEX_UPSTREAM", defaultCodexUpstream))}, os.Stdout)
	case "renew":
		err = renewAccounts(a, args, setupTokenRunner(a), countTokensValidator(envString("LIMITBREAK_UPSTREAM", defaultUpstream)), os.Stdout)
	case "share":
		err = shareTokens(a, args, sshImport, os.Stdout)
	case "import-token":
		err = importToken(a, args, countTokensValidator(envString("LIMITBREAK_UPSTREAM", defaultUpstream)), os.Stdin, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// accountCommand runs add or remove for the provider named by --provider (claude by default),
// printing JSON events with --json.
func accountCommand(command string, args []string, providers ...loginProvider) error {
	providerName, asJSON, rest, err := accountFlags(args)
	var lp loginProvider
	for _, p := range providers {
		if p.name() == providerName {
			lp = p
		}
	}
	if err == nil && lp == nil {
		err = fmt.Errorf("unknown provider %q; use claude or codex", providerName)
	}
	if err != nil {
		if asJSON {
			newEvents(os.Stdout)(loginEvent{Event: "error", Message: err.Error()})
		}
		return err
	}
	name := ""
	if len(rest) > 0 {
		name = rest[0]
	}
	switch {
	case command == "add" && asJSON:
		return addAccountJSON(lp, name, commandRunner(lp.loginCommand), os.Stdin, os.Stdout)
	case command == "add":
		return addAccount(lp, name, loginAs(lp), os.Stdout)
	case asJSON:
		return removeAccountJSON(lp, rest, os.Stdout)
	}
	return removeAccount(lp, rest, os.Stdin, os.Stdout)
}

// accountFlags takes --provider <p> (or --provider=<p>) and --json out of args.
func accountFlags(args []string) (provider string, asJSON bool, rest []string, err error) {
	provider = "claude"
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--json":
			asJSON = true
		case arg == "--provider":
			if i+1 >= len(args) {
				return provider, asJSON, rest, fmt.Errorf("--provider needs a value: claude or codex")
			}
			i++
			provider = args[i]
		case strings.HasPrefix(arg, "--provider="):
			provider = strings.TrimPrefix(arg, "--provider=")
		default:
			rest = append(rest, arg)
		}
	}
	return provider, asJSON, rest, nil
}

// MARK: Providers the CLI signs in

// A loginProvider is a provider whose named logins the CLI adds and removes with the provider's
// own sign-in.
type loginProvider interface {
	provider
	// cliNames are how the provider's own app and commands are called, for messages.
	cliNames() cliNames
	// dirFor is where the named login lives.
	dirFor(name string) string
	// loginCommand is the provider's own sign-in into dir.
	loginCommand(dir string) (*exec.Cmd, error)
	// forget deletes a named login from this machine.
	forget(acc account) error
}

type cliNames struct{ app, login, logout string }

func (a *claudeAccounts) cliNames() cliNames {
	return cliNames{app: "Claude Code", login: "claude auth login", logout: "claude auth logout"}
}

func (a *claudeAccounts) dirFor(name string) string { return filepath.Join(a.home, ".claude-"+name) }

func (a *claudeAccounts) loginCommand(dir string) (*exec.Cmd, error) {
	if a.claude == "" {
		return nil, fmt.Errorf("claude is not on PATH; install Claude Code first")
	}
	cmd := exec.Command(a.claude, "auth", "login", "--claudeai")
	cmd.Env = claudeEnv(dir)
	return cmd, nil
}

// forget deletes a Claude login: its Keychain items on macOS, its 1-year token and its config dir.
func (a *claudeAccounts) forget(acc account) error {
	if runtime.GOOS == "darwin" {
		for _, query := range keychainQueries(acc.Dir) {
			args := append([]string{"delete-generic-password"}, query[1:len(query)-1]...)
			_ = exec.Command("security", args...).Run()
		}
	}
	if err := a.deleteLongToken(acc); err != nil {
		return err
	}
	return os.RemoveAll(acc.Dir)
}

func (a *codexAccounts) cliNames() cliNames {
	return cliNames{app: "Codex", login: "codex login", logout: "codex logout"}
}

func (a *codexAccounts) dirFor(name string) string { return filepath.Join(a.home, ".codex-"+name) }

func (a *codexAccounts) loginCommand(dir string) (*exec.Cmd, error) {
	if a.codex == "" {
		return nil, fmt.Errorf("codex is not on PATH; install Codex first")
	}
	cmd := exec.Command(a.codex, "login")
	cmd.Env, cmd.Dir = codexEnv(dir), dir
	return cmd, nil
}

// forget deletes a Codex login. A home dir Limit Break made goes whole; one it found (a
// ~/.codex-<name> another tool also keeps logs or sessions in) loses only its auth.json.
func (a *codexAccounts) forget(acc account) error {
	if _, err := os.Stat(filepath.Join(acc.Dir, createdMarker)); err == nil {
		return os.RemoveAll(acc.Dir)
	}
	if err := os.Remove(filepath.Join(acc.Dir, "auth.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// createdMarker marks a login dir Limit Break made itself, so remove may delete all of it.
const createdMarker = ".limitbreak-created"

func isAccount(list []account, id string) bool {
	for _, acc := range list {
		if acc.ID == id {
			return true
		}
	}
	return false
}

// MARK: Add and remove

// nextFreeName is account-N for the first N not taken.
func nextFreeName(taken []account) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t.ID] = true
	}
	for n := 1; ; n++ {
		if name := fmt.Sprintf("account-%d", n); !used[name] {
			return name
		}
	}
}

// loginAs runs the provider's own browser sign-in into the dir, with the terminal attached.
func loginAs(a loginProvider) func(dir string) error {
	return func(dir string) error {
		cmd, err := a.loginCommand(dir)
		if err != nil {
			return err
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
}

// prepareAdd picks and checks the new login's name (account-N when none is given) and makes its dir.
func prepareAdd(a loginProvider, name string) (string, string, bool, error) {
	if name == "" {
		name = nextFreeName(a.list())
	}
	if name == defaultID {
		return "", "", false, fmt.Errorf(`"default" is the account plain "%s" signs in; pick another name`, a.cliNames().login)
	}
	if !accountName.MatchString(name) {
		return "", "", false, fmt.Errorf("name %q: use letters, digits, - and _", name)
	}
	dir := a.dirFor(name)
	_, statErr := os.Stat(dir)
	created := os.IsNotExist(statErr)
	if !created && !isAccount(a.list(), name) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			// Another tool's dir with the same name (say ~/.codex-chief): never sign into it,
			// or a later remove would delete what it keeps there.
			return "", "", false, fmt.Errorf("%s already exists and is not a login here; pick another name", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false, err
	}
	if created {
		_ = os.WriteFile(filepath.Join(dir, createdMarker), nil, 0o600)
	}
	return name, dir, created, nil
}

// finishAdd checks the sign-in landed and returns the new account, every account now, and the
// others that are the same subscription. A dir it created is removed when nothing was added.
func finishAdd(a loginProvider, name, dir string, created bool, loginErr error) (account, []account, []account, error) {
	if loginErr != nil {
		if created {
			_ = os.RemoveAll(dir)
		}
		return account{}, nil, nil, fmt.Errorf("sign-in failed: %w", loginErr)
	}
	var added *account
	list := a.list()
	for i := range list {
		if list[i].ID == name {
			added = &list[i]
		}
	}
	if added == nil {
		if created {
			_ = os.RemoveAll(dir)
		}
		return account{}, nil, nil, fmt.Errorf("no login found after sign-in; nothing was added")
	}
	var same []account
	for _, other := range list {
		if other.ID != name && added.Who != "" && other.Who == added.Who {
			same = append(same, other)
		}
	}
	return *added, list, same, nil
}

// addAccount signs a new login into the provider's dir for name and reports who it is. login is
// injected so the flow is testable without a browser.
func addAccount(a loginProvider, name string, login func(dir string) error, out io.Writer) error {
	name, dir, created, err := prepareAdd(a, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Signing in %q. Finish the sign-in in your browser...\n", name)
	added, list, same, err := finishAdd(a, name, dir, created, login(dir))
	if err != nil {
		return err
	}
	who := added.Email
	if who == "" {
		who = "signed in"
	}
	fmt.Fprintf(out, "Added %s (%s). %d accounts now.\n", name, who, len(list))
	for _, other := range same {
		fmt.Fprintf(out, "Warning: %s is the same login as %q; two entries for one account do not add capacity. `limitbreak remove %s` drops it.\n", added.Email, other.ID, name)
	}
	return nil
}

func listAccounts(providers []loginProvider, out io.Writer) {
	active := activeAccounts()
	found := false
	for _, p := range providers {
		for _, acc := range p.list() {
			found = true
			mark := " "
			if acc.ID == active[p.name()] {
				mark = "*"
			}
			email := acc.Email
			if email == "" {
				email = "(email unknown)"
			}
			if acc.Plan != "" {
				email += " (" + acc.Plan + ")"
			}
			fmt.Fprintf(out, "%s %-6s %-16s %s\n", mark, p.name(), acc.ID, email)
			switch a := p.(type) {
			case *claudeAccounts:
				end, kind := a.authInfo(acc)
				fmt.Fprintf(out, "    auth: %s\n", authLine(acc.ID, end, kind, a.now()))
				showUsage(out, a.home, acc)
			case *codexAccounts:
				if acc.Dir == "" {
					fmt.Fprintf(out, "    auth: Codex's own login, refreshed by Codex\n")
				} else {
					fmt.Fprintf(out, "    auth: refreshed by limitbreak through codex app-server\n")
				}
				_, windows := a.usage(acc)
				showWindows(out, windows)
			}
		}
	}
	if !found {
		fmt.Fprintln(out, "No accounts signed in. Run `claude auth login` (default) or `limitbreak add`.")
		return
	}
	if len(active) == 0 {
		fmt.Fprintln(out, "(proxy not running: start it with the installed service, or `limitbreak`)")
	}
}

// activeAccounts asks the running proxy which account the shared client is on, by provider;
// empty when it is down.
func activeAccounts() map[string]string {
	resp, err := (&http.Client{Timeout: time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/_status", envInt("LIMITBREAK_PORT", defaultPort)))
	if err != nil {
		return map[string]string{}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return activeFromStatus(raw)
}

// activeFromStatus is the shared client's account per provider in a `GET /_status` reply; a row
// without a provider is Claude's.
func activeFromStatus(raw []byte) map[string]string {
	active := map[string]string{}
	var rows []accountStatus
	if json.Unmarshal(raw, &rows) != nil {
		return active
	}
	for _, r := range rows {
		for _, c := range r.ActiveFor {
			if c == sharedClient {
				provider := r.Provider
				if provider == "" {
					provider = "claude"
				}
				active[provider] = r.ID
			}
		}
	}
	return active
}

// namedLogin is the account a remove names: never the default one, which belongs to the provider.
func namedLogin(a loginProvider, name string) (account, error) {
	if name == "" {
		return account{}, fmt.Errorf("usage: limitbreak remove <name> [-y]")
	}
	if name == defaultID {
		n := a.cliNames()
		return account{}, fmt.Errorf(`the default account is %s's own login; sign out with "%s"`, n.app, n.logout)
	}
	for _, acc := range a.list() {
		if acc.ID == name {
			return acc, nil
		}
	}
	return account{}, fmt.Errorf("no account named %q", name)
}

// removeArgs is the name and whether -y/--yes was given.
func removeArgs(args []string) (name string, yes bool) {
	for _, arg := range args {
		if arg == "-y" || arg == "--yes" {
			yes = true
		} else {
			name = arg
		}
	}
	return name, yes
}

// removeAccount deletes a non-default login (see forget) after asking, unless -y.
func removeAccount(a loginProvider, args []string, in io.Reader, out io.Writer) error {
	name, yes := removeArgs(args)
	target, err := namedLogin(a, name)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintf(out, "Delete the login %q (%s) from this machine? [y/N] ", name, target.Email)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
			fmt.Fprintln(out, "Left as it is.")
			return nil
		}
	}
	if err := a.forget(target); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s.\n", name)
	return nil
}

// MARK: JSON events

// loginEvent is one line of `add --json` / `remove --json` output.
type loginEvent struct {
	// Event is open_url, device_code, need_code, added, removed or error.
	Event    string `json:"event"`
	URL      string `json:"url,omitempty"`
	Code     string `json:"code,omitempty"`
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	// SameAs names an account that is the same subscription as the one just added.
	SameAs  string `json:"sameAs,omitempty"`
	Message string `json:"message,omitempty"`
}

// newEvents writes events to out, one JSON object per line; safe from several goroutines.
func newEvents(out io.Writer) func(loginEvent) {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	return func(e loginEvent) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	}
}

// loginRunner runs a sign-in into dir with its terminal output going to output; each string on
// codes is a line typed into it.
type loginRunner func(dir string, output io.Writer, codes <-chan string) error

// commandRunner runs the provider's sign-in command, feeding it the codes. Its output also goes to
// stderr, for a person reading along.
func commandRunner(command func(dir string) (*exec.Cmd, error)) loginRunner {
	return func(dir string, output io.Writer, codes <-chan string) error {
		cmd, err := command(dir)
		if err != nil {
			return err
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		cmd.Stdout = io.MultiWriter(output, os.Stderr)
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() {
			for code := range codes {
				_, _ = io.WriteString(stdin, code+"\n")
			}
		}()
		return cmd.Wait()
	}
}

// addAccountJSON is addAccount for apps: it reports the sign-in as events (see loginWatch) and
// ends with added or error.
func addAccountJSON(a loginProvider, name string, run loginRunner, in io.Reader, out io.Writer) error {
	emit := newEvents(out)
	fail := func(err error) error {
		emit(loginEvent{Event: "error", Message: err.Error()})
		return err
	}
	name, dir, created, err := prepareAdd(a, name)
	if err != nil {
		return fail(err)
	}
	codes := make(chan string, 1)
	watch := &loginWatch{emit: emit, answers: bufio.NewReader(in), codes: codes}
	loginErr := run(dir, watch, codes)
	watch.flush()
	added, _, same, err := finishAdd(a, name, dir, created, loginErr)
	if err != nil {
		return fail(err)
	}
	e := loginEvent{Event: "added", Provider: a.name(), ID: added.ID, Email: added.Email}
	if len(same) > 0 {
		e.SameAs = same[0].ID
	}
	emit(e)
	return nil
}

// removeAccountJSON is removeAccount for apps: it needs --yes, and ends with removed or error.
func removeAccountJSON(a loginProvider, args []string, out io.Writer) error {
	emit := newEvents(out)
	fail := func(err error) error {
		emit(loginEvent{Event: "error", Message: err.Error()})
		return err
	}
	name, yes := removeArgs(args)
	if !yes {
		return fail(fmt.Errorf("remove --json needs --yes"))
	}
	target, err := namedLogin(a, name)
	if err != nil {
		return fail(err)
	}
	if err := a.forget(target); err != nil {
		return fail(err)
	}
	emit(loginEvent{Event: "removed", Provider: a.name(), ID: target.ID, Email: target.Email})
	return nil
}

var (
	signInURL  = regexp.MustCompile(`https?://[^\s"'<>]+`)
	deviceCode = regexp.MustCompile(`\b[A-Z0-9]{4,}-[A-Z0-9]{4,}\b`)
)

// loginWatch reads a sign-in's terminal output and turns it into events: the first sign-in link
// (not a localhost callback) as open_url; a one-time code to type on that page as device_code; a
// prompt to paste a code back as need_code, after which one line read from answers goes to the
// sign-in. Reading the answer never holds up the output, so a sign-in that finishes in the
// browser is not kept waiting for one.
type loginWatch struct {
	emit    func(loginEvent)
	answers *bufio.Reader
	codes   chan<- string

	line       []byte
	opened     bool
	asked      bool
	wantDevice bool
}

func (w *loginWatch) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.flush()
			continue
		}
		w.line = append(w.line, b)
	}
	// A prompt waits on its own line without ending it.
	w.prompt(cleanTerminal(string(w.line)))
	return len(p), nil
}

// flush handles the line so far as a whole line.
func (w *loginWatch) flush() {
	text := cleanTerminal(string(w.line))
	w.line = w.line[:0]
	for _, link := range signInURL.FindAllString(text, -1) {
		if u, err := url.Parse(link); err == nil && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && !w.opened {
			w.opened = true
			w.emit(loginEvent{Event: "open_url", URL: link})
		}
	}
	if strings.Contains(strings.ToLower(text), "one-time code") {
		w.wantDevice = true
	}
	if code := deviceCode.FindString(text); w.wantDevice && code != "" {
		w.wantDevice = false
		w.emit(loginEvent{Event: "device_code", Code: code})
	}
	w.prompt(text)
	w.asked = false
}

func (w *loginWatch) prompt(text string) {
	lower := strings.ToLower(text)
	if w.asked || !strings.Contains(lower, "paste") || !strings.Contains(lower, "code") {
		return
	}
	w.asked = true
	w.emit(loginEvent{Event: "need_code"})
	go func() {
		answer, err := w.answers.ReadString('\n')
		if answer = strings.TrimSpace(answer); answer != "" || err == nil {
			w.codes <- answer
		}
	}()
}

// cleanTerminal is text with terminal escapes and control characters taken out.
func cleanTerminal(s string) string {
	return controlChars.ReplaceAllString(terminalEscape.ReplaceAllString(s, ""), "")
}

// MARK: Renew

// A 1-year token as `claude setup-token` prints it.
var (
	longTokenPattern = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]+`)
	tokenContinued   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// Terminal escapes in a typescript: CSI (colors, cursor moves), OSC (titles, links), charset
	// designations, and any other two-byte escape. Order matters: the longer forms first.
	terminalEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()*+].|\x1b.`)
	controlChars   = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	lineBreaks     = regexp.MustCompile(`\r*\n|\r`)
)

// extractToken is the last 1-year token in a terminal recording of `claude setup-token`; empty
// when there is none. The TUI may wrap a long token over several lines, so a token that runs to
// the end of its line takes the following lines as long as each one is nothing but token
// characters. It stops at a blank line, at prose (anything with a space or punctuation), and at the
// start of another token, so a wrong join is unlikely; a truncated or joined token would still be
// caught by the validation that follows, and never stored.
func extractToken(typescript string) string {
	clean := terminalEscape.ReplaceAllStringFunc(typescript, func(seq string) string {
		if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "C") {
			return " " // cursor forward stands in for spaces
		}
		return ""
	})
	clean = controlChars.ReplaceAllString(clean, "")
	lines := lineBreaks.Split(clean, -1)
	found := ""
	for i, line := range lines {
		for _, m := range longTokenPattern.FindAllStringIndex(line, -1) {
			token := line[m[0]:m[1]]
			if strings.TrimRight(line[m[1]:], " \t") == "" {
				for _, next := range lines[i+1:] {
					more := strings.TrimSpace(next)
					if !tokenContinued.MatchString(more) || strings.Contains(more, "sk-ant-") {
						break
					}
					token += more
				}
			}
			found = token
		}
	}
	return found
}

// scriptCommand runs `claude setup-token` under script(1), which gives it a real terminal (it is
// an interactive TUI) and records everything it prints, so the token can be read back.
func scriptCommand(goos, claude, typescript string) []string {
	if goos == "darwin" {
		return []string{"script", "-q", typescript, claude, "setup-token"}
	}
	return []string{"script", "-q", "-c", shellQuote(claude) + " setup-token", typescript}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// tokenRunner runs `claude setup-token` as the account, recording its terminal output into the
// typescript file. Injected so renew is testable without a browser.
type tokenRunner func(acc account, typescript string) error

// tokenValidator asks Anthropic about a token and returns the HTTP status.
type tokenValidator func(token string) (int, error)

func setupTokenRunner(a *claudeAccounts) tokenRunner {
	return func(acc account, typescript string) error {
		if a.claude == "" {
			return fmt.Errorf("claude is not on PATH; install Claude Code first")
		}
		args := scriptCommand(runtime.GOOS, a.claude, typescript)
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = claudeEnv(acc.Dir)
		// Claude Code opens its sign-in page through $BROWSER. Pointing that at limitbreak lets it
		// add the account's email as login_hint, as `claude auth login --email` does.
		if self, err := os.Executable(); err == nil && acc.Email != "" {
			cmd.Env = append(cmd.Env, "BROWSER="+self, loginHintEnv+"="+acc.Email)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
}

// countTokensValidator checks a token with the cheapest call that needs inference scope and spends
// no usage.
func countTokensValidator(upstream string) tokenValidator {
	return func(token string) (int, error) {
		body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"ok"}]}`
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(upstream, "/")+"/v1/messages/count_tokens", strings.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, nil
	}
}

// renewAccounts gives accounts a 1-year token: named ones always, else every one that needs it
// (needsRenew), or all of them with --all. One failing account does not stop the others.
func renewAccounts(a *claudeAccounts, args []string, run tokenRunner, validate tokenValidator, out io.Writer) error {
	all := false
	var names []string
	for _, arg := range args {
		switch {
		case arg == "--all" || arg == "-all":
			all = true
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("usage: limitbreak renew [name...] [--all]")
		default:
			names = append(names, arg)
		}
	}
	list := a.list()
	var targets []account
	failed := map[string]string{}
	var failedOrder []string
	fail := func(id, reason string) {
		failed[id] = reason
		failedOrder = append(failedOrder, id)
	}
	if len(names) > 0 {
		byID := map[string]account{}
		for _, acc := range list {
			byID[acc.ID] = acc
		}
		for _, name := range names {
			acc, ok := byID[name]
			if !ok {
				fail(name, "no such account")
				continue
			}
			targets = append(targets, acc)
		}
	} else {
		now := a.now()
		for _, acc := range list {
			end, kind := a.authInfo(acc)
			if all || needsRenew(end, kind, now) {
				targets = append(targets, acc)
				continue
			}
			fmt.Fprintf(out, "%s has a 1-year token until %s; skipped (--all renews it anyway).\n", acc.ID, day(end))
		}
	}
	if len(list) == 0 && len(names) == 0 {
		fmt.Fprintln(out, "No accounts signed in. Run `claude auth login` (default) or `limitbreak add`.")
		return nil
	}
	var renewed []string
	for _, acc := range targets {
		if err := renewOne(a, acc, run, validate, out); err != nil {
			fmt.Fprintf(out, "%s not renewed: %v\n", acc.ID, err)
			fail(acc.ID, err.Error())
			continue
		}
		renewed = append(renewed, acc.ID)
	}
	if len(renewed) > 0 {
		fmt.Fprintf(out, "Renewed: %s.\n", strings.Join(renewed, ", "))
	} else if len(failedOrder) == 0 {
		fmt.Fprintln(out, "Nothing to renew.")
	}
	if len(failedOrder) > 0 {
		var parts []string
		for _, id := range failedOrder {
			parts = append(parts, id+" ("+failed[id]+")")
		}
		fmt.Fprintf(out, "Failed: %s.\n", strings.Join(parts, "; "))
		return fmt.Errorf("%d account(s) not renewed", len(failedOrder))
	}
	return nil
}

func renewOne(a *claudeAccounts, acc account, run tokenRunner, validate tokenValidator, out io.Writer) error {
	email, who := acc.Email, acc.Email
	if email == "" {
		email, who = "email unknown", "the account you want for "+acc.ID
	}
	fmt.Fprintf(out, "Renewing %s (%s). In the browser, make sure claude.ai is signed in as %s, then authorize.\n", acc.ID, email, who)
	token, runErr := captureToken(acc, run)
	if token == "" {
		if runErr != nil {
			return fmt.Errorf("claude setup-token: %w", runErr)
		}
		return fmt.Errorf("no token in claude setup-token's output")
	}
	status, err := validate(token)
	switch {
	case err != nil:
		fmt.Fprintf(out, "Warning: could not check the new token (%v); storing it anyway.\n", err)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("Anthropic rejected the new token (HTTP %d); not stored", status)
	case status != http.StatusOK:
		fmt.Fprintf(out, "Warning: checking the new token answered HTTP %d; storing it anyway.\n", status)
	}
	t := newLongToken(token, a.now())
	if err := a.writeLongToken(acc, t); err != nil {
		return fmt.Errorf("storing the token: %w", err)
	}
	fmt.Fprintf(out, "%s now has a 1-year token until %s.\n", acc.ID, day(time.UnixMilli(t.ExpiresAt)))
	return nil
}

// captureToken runs setup-token into a typescript in a private temp dir and deletes it as soon as
// it is read: the recording holds the token in the clear.
func captureToken(acc account, run tokenRunner) (string, error) {
	dir, err := os.MkdirTemp("", "limitbreak-renew-") // 0700
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "typescript")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	f.Close()
	runErr := run(acc, path)
	raw, _ := os.ReadFile(path)
	_ = os.RemoveAll(dir)
	return extractToken(string(raw)), runErr
}

// loginHintEnv carries the account's email to limitbreak running as $BROWSER during renew.
const loginHintEnv = "LIMITBREAK_LOGIN_HINT"

// withLoginHint adds login_hint=email to a Claude sign-in link that has none, so the sign-in page
// starts on that account. Any other link, or no email, comes back unchanged.
func withLoginHint(raw, email string) string {
	u, err := url.Parse(raw)
	if err != nil || email == "" || !strings.Contains(u.Path, "oauth/authorize") {
		return raw
	}
	host := u.Hostname()
	if !(host == "claude.com" || host == "claude.ai" || strings.HasSuffix(host, ".claude.com") || strings.HasSuffix(host, ".claude.ai")) {
		return raw
	}
	q := u.Query()
	if q.Get("login_hint") != "" {
		return raw
	}
	q.Set("login_hint", email)
	u.RawQuery = q.Encode()
	return u.String()
}

// openBrowser is limitbreak acting as $BROWSER for `claude setup-token`: it opens the link with
// the login hint added.
func openBrowser(link string) int {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, withLoginHint(link, os.Getenv(loginHintEnv))).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// MARK: Share

// A 1-year token is a plain bearer token that never rotates, so unlike a sign-in it can be copied
// to another machine without the two logging each other out.

// remoteImport sends one account's token record to `limitbreak import-token` on an SSH host.
type remoteImport func(host, id string, record []byte) (string, error)

func sshImport(host, id string, record []byte) (string, error) {
	cmd := exec.Command("ssh", host, `"$HOME/.local/bin/limitbreak" import-token `+shellQuote(id))
	cmd.Stdin = bytes.NewReader(record)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// shareTokens copies every account's unexpired 1-year token to the same-named account on host.
func shareTokens(a *claudeAccounts, args []string, send remoteImport, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: limitbreak share <ssh-host>")
	}
	host := args[0]
	now := a.now()
	var failed []string
	shared := 0
	for _, acc := range a.list() {
		t, ok := a.readLongToken(acc)
		if !ok || !t.valid(now) {
			fmt.Fprintf(out, "%s has no 1-year token; skipped (run limitbreak renew %s).\n", acc.ID, acc.ID)
			continue
		}
		record, err := json.Marshal(t)
		if err != nil {
			return err
		}
		reply, err := send(host, acc.ID, record)
		if reply != "" {
			fmt.Fprintf(out, "%s: %s\n", host, reply)
		}
		if err != nil {
			failed = append(failed, acc.ID)
			continue
		}
		shared++
	}
	if len(failed) > 0 {
		return fmt.Errorf("not shared to %s: %s", host, strings.Join(failed, ", "))
	}
	fmt.Fprintf(out, "Shared %d token(s) to %s.\n", shared, host)
	return nil
}

// importToken stores a token record read from in for the named local account, after Anthropic
// accepts it. The account must already exist here, so the proxy knows it.
func importToken(a *claudeAccounts, args []string, validate tokenValidator, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: limitbreak import-token <name> < record.json")
	}
	raw, err := io.ReadAll(io.LimitReader(in, 64*1024))
	if err != nil {
		return err
	}
	var t longToken
	if json.Unmarshal(raw, &t) != nil || !longTokenPattern.MatchString(t.Token) {
		return fmt.Errorf("no token record on stdin")
	}
	if !t.valid(a.now()) {
		return fmt.Errorf("the token has expired")
	}
	var target *account
	for _, acc := range a.list() {
		if acc.ID == args[0] {
			acc := acc
			target = &acc
		}
	}
	if target == nil {
		return fmt.Errorf("no account named %q here; sign it in first (limitbreak add %s)", args[0], args[0])
	}
	if status, err := validate(t.Token); err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return fmt.Errorf("Anthropic rejected the token (HTTP %d); not stored", status)
	}
	if err := a.writeLongToken(*target, t); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s now has a 1-year token until %s.\n", target.ID, day(time.UnixMilli(t.ExpiresAt)))
	return nil
}
