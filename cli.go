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

The default account is the one plain "claude auth login" signs in.`

var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func runCommand(command string, args []string) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	a := newAccounts(home, "auto", "")
	switch command {
	case "serve":
		serve(args)
		return 0
	case "add":
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		err = addAccount(a, name, loginAs(a), os.Stdout)
	case "video":
		err = runVideo(os.Stdout)
	case "list":
		listAccounts(a, os.Stdout)
	case "renew":
		err = renewAccounts(a, args, setupTokenRunner(a), countTokensValidator(envString("LIMITBREAK_UPSTREAM", defaultUpstream)), os.Stdout)
	case "share":
		err = shareTokens(a, args, sshImport, os.Stdout)
	case "import-token":
		err = importToken(a, args, countTokensValidator(envString("LIMITBREAK_UPSTREAM", defaultUpstream)), os.Stdin, os.Stdout)
	case "remove":
		err = removeAccount(a, args, os.Stdin, os.Stdout)
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

// freshAccounts is the account list read now, not the cached one.
func freshAccounts(a *accounts) []account {
	a.mu.Lock()
	a.discovered = nil
	a.mu.Unlock()
	return a.list()
}

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

// loginAs runs Claude Code's own browser sign-in into the config dir, with the terminal attached.
func loginAs(a *accounts) func(dir string) error {
	return func(dir string) error {
		if a.claude == "" {
			return fmt.Errorf("claude is not on PATH; install Claude Code first")
		}
		cmd := exec.Command(a.claude, "auth", "login", "--claudeai")
		cmd.Env = claudeEnv(dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
}

// addAccount signs a new login into ~/.claude-<name> (account-N when no name is given) and
// reports who it is. login is injected so the flow is testable without a browser.
func addAccount(a *accounts, name string, login func(dir string) error, out io.Writer) error {
	existing := freshAccounts(a)
	if name == "" {
		name = nextFreeName(existing)
	}
	if name == defaultID {
		return fmt.Errorf(`"default" is the account plain "claude auth login" signs in; pick another name`)
	}
	if !accountName.MatchString(name) {
		return fmt.Errorf("name %q: use letters, digits, - and _", name)
	}
	dir := filepath.Join(a.home, ".claude-"+name)
	_, statErr := os.Stat(dir)
	created := os.IsNotExist(statErr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fmt.Fprintf(out, "Signing in %q. Finish the sign-in in your browser...\n", name)
	if err := login(dir); err != nil {
		if created {
			_ = os.Remove(dir)
		}
		return fmt.Errorf("sign-in failed: %w", err)
	}
	var added *account
	list := freshAccounts(a)
	for i := range list {
		if list[i].ID == name {
			added = &list[i]
		}
	}
	if added == nil {
		if created {
			_ = os.Remove(dir)
		}
		return fmt.Errorf("no login found after sign-in; nothing was added")
	}
	who := added.Email
	if who == "" {
		who = "signed in"
	}
	fmt.Fprintf(out, "Added %s (%s). %d accounts now.\n", name, who, len(list))
	for _, other := range list {
		if other.ID != name && added.Email != "" && other.Email == added.Email {
			fmt.Fprintf(out, "Warning: %s is the same login as %q; two entries for one account do not add capacity. `limitbreak remove %s` drops it.\n", added.Email, other.ID, name)
		}
	}
	return nil
}

func listAccounts(a *accounts, out io.Writer) {
	list := freshAccounts(a)
	if len(list) == 0 {
		fmt.Fprintln(out, "No accounts signed in. Run `claude auth login` (default) or `limitbreak add`.")
		return
	}
	active := activeAccount()
	for _, acc := range list {
		mark := " "
		if acc.ID == active {
			mark = "*"
		}
		email := acc.Email
		if email == "" {
			email = "(email unknown)"
		}
		fmt.Fprintf(out, "%s %-16s %s\n", mark, acc.ID, email)
		end, kind := a.authInfo(acc)
		fmt.Fprintf(out, "    auth: %s\n", authLine(acc.ID, end, kind, a.now()))
		showUsage(out, a.home, acc)
	}
	if active == "" {
		fmt.Fprintln(out, "(proxy not running: start it with the installed service, or `limitbreak`)")
	}
}

// activeAccount asks the running proxy which account the shared client is on; empty when it is down.
func activeAccount() string {
	resp, err := (&http.Client{Timeout: time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/_status", envInt("LIMITBREAK_PORT", defaultPort)))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return activeFromStatus(raw)
}

func activeFromStatus(raw []byte) string {
	var rows []accountStatus
	if json.Unmarshal(raw, &rows) != nil {
		return ""
	}
	for _, r := range rows {
		for _, c := range r.ActiveFor {
			if c == sharedClient {
				return r.ID
			}
		}
	}
	return ""
}

// removeAccount deletes a non-default login: its config dir and, on macOS, its Keychain item.
func removeAccount(a *accounts, args []string, in io.Reader, out io.Writer) error {
	yes := false
	name := ""
	for _, arg := range args {
		if arg == "-y" || arg == "--yes" {
			yes = true
		} else {
			name = arg
		}
	}
	if name == "" {
		return fmt.Errorf("usage: limitbreak remove <name> [-y]")
	}
	if name == defaultID {
		return fmt.Errorf(`the default account is Claude Code's own login; sign out with "claude auth logout"`)
	}
	var target *account
	for _, acc := range freshAccounts(a) {
		if acc.ID == name {
			acc := acc
			target = &acc
		}
	}
	if target == nil {
		return fmt.Errorf("no account named %q", name)
	}
	if !yes {
		fmt.Fprintf(out, "Delete the login %q (%s) from this machine? [y/N] ", name, target.Email)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") {
			fmt.Fprintln(out, "Left as it is.")
			return nil
		}
	}
	if runtime.GOOS == "darwin" {
		for _, query := range keychainQueries(target.Dir) {
			args := append([]string{"delete-generic-password"}, query[1:len(query)-1]...)
			_ = exec.Command("security", args...).Run()
		}
	}
	if err := a.deleteLongToken(*target); err != nil {
		return err
	}
	if err := os.RemoveAll(target.Dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s.\n", name)
	return nil
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

func setupTokenRunner(a *accounts) tokenRunner {
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
func renewAccounts(a *accounts, args []string, run tokenRunner, validate tokenValidator, out io.Writer) error {
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
	list := freshAccounts(a)
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

func renewOne(a *accounts, acc account, run tokenRunner, validate tokenValidator, out io.Writer) error {
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
func shareTokens(a *accounts, args []string, send remoteImport, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: limitbreak share <ssh-host>")
	}
	host := args[0]
	now := a.now()
	var failed []string
	shared := 0
	for _, acc := range freshAccounts(a) {
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
func importToken(a *accounts, args []string, validate tokenValidator, in io.Reader, out io.Writer) error {
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
	for _, acc := range freshAccounts(a) {
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
