package main

// Sharing a Grok login between Macs.
//
// A Grok login is a session token (about 6 hours long) renewed with a refresh token that rotates, so
// two Macs holding the whole login would sign each other out. One Mac (the owner) keeps the login and
// its refresh. It makes a token-only copy for each Mac it shares with: the access token and who it
// is, never the refresh token (a copy is a grok auth.json without one, which is what marks it shared
// and never refreshable). The owner keeps the copy fresh by pushing: its serve loop refreshes the
// login through grok when under grokShareRefreshWithin is left and pushes whenever the token
// changed or the last push failed. There is no pull: the receiver holds nothing that could ask the
// owner, and a push after every refresh needs the owner awake only once per token, not for every
// request. A copy outlives its owner's sleep by whatever the token has left; on waking, the owner's
// first tick refreshes and pushes again.
//
// A Mac with no login of its own (nothing in ~/.grok) runs grok on the shared copy: see
// grokAccounts.spends. grok will not start without an auth.json, so such a Mac gives each agent home
// a placeholder one (limitbreak grok-placeholder <dir>), which every request's Authorization and
// identity headers replace.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	grokIssuer   = "https://auth.x.ai"
	grokClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// grokPlaceholderEmail marks the stand-in login: it is never an account.
	grokPlaceholderEmail = "limitbreak@placeholder.invalid"
	// grokSharedName is the account a Mac's default login becomes on the Macs it is shared to.
	grokSharedName = "shared"
	// grokShareEvery is how often the owner checks its shares; grokShareRefreshWithin is how much
	// token life triggers a refresh, so a copy always has at least that much left on arrival.
	grokShareEvery         = 5 * time.Minute
	grokShareRefreshWithin = 3 * time.Hour
)

// MARK: The token-only copy and the placeholder (pure)

// tokenOnlyGrok is the copy of a Grok auth.json that is safe to give another Mac: its one login with
// the longest token, without the refresh token.
func tokenOnlyGrok(raw []byte) ([]byte, grokLogin, error) {
	var doc map[string]map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil, grokLogin{}, fmt.Errorf("not a Grok auth.json")
	}
	var bestKey string
	var best grokLogin
	for key, entry := range doc {
		single, _ := json.Marshal(map[string]any{key: entry})
		l, ok := parseGrokLogin(single)
		if ok && !l.placeholder && (bestKey == "" || l.expires.After(best.expires)) {
			bestKey, best = key, l
		}
	}
	if bestKey == "" {
		return nil, grokLogin{}, fmt.Errorf("no Grok login in it")
	}
	entry := doc[bestKey]
	delete(entry, "refresh_token")
	out, err := json.Marshal(map[string]any{bestKey: entry})
	best.refreshable = false
	return out, best, err
}

// grokPlaceholderAuth is an auth.json grok accepts but that is nobody's: a login with a JWT of the
// shape grok checks (every claim, expiring in 2100) and no refresh token. grok sends no request
// without an auth.json; Limit Break swaps in a real login's token and identity on each one.
func grokPlaceholderAuth() []byte {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	jwt := enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": grokIssuer, "sub": "limitbreak", "aud": []string{grokClientID}, "exp": 4102444800, "iat": 1767225600,
		"scope": "openid profile email offline_access api:access", "principal_type": "User", "principal_id": "limitbreak",
		"client_id": grokClientID, "jti": "limitbreak", "tier": 1, "team_id": "limitbreak"}) + "." + "bGltaXRicmVhaw"
	out, _ := json.MarshalIndent(map[string]any{grokIssuer + "::" + grokClientID: map[string]any{
		"auth_mode": "oidc", "key": jwt, "expires_at": "2099-12-31T00:00:00Z", "email": grokPlaceholderEmail,
		"user_id": "limitbreak", "team_id": "limitbreak", "oidc_issuer": grokIssuer, "oidc_client_id": grokClientID,
		"principal_id": "limitbreak", "principal_type": "User", "create_time": "2026-01-01T00:00:00Z",
		"first_name": "Limit Break", "team_zero_data_retention": false, "coding_data_retention_opt_out": false}}, "", "  ")
	return out
}

// tokenFingerprint names a token without revealing it, to tell whether a push is due.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// writeGrokAuth writes dir/auth.json at 0600 through a temp file renamed over it, so a reader sees the
// old file or the new one whole and the token is never readable by others.
func writeGrokAuth(dir string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".limitbreak-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "auth.json"))
}

// MARK: Receiving

// importGrok stores a token-only Grok login read from in as the named account here
// (~/.grok-<name>). It refuses a login that still has a refresh token (two Macs refreshing one would
// sign each other out), a placeholder, an ended or older token than the one held, and a dir that is
// a login of its own or another tool's.
func importGrok(a *grokAccounts, args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: limitbreak import-grok <name> < auth.json")
	}
	name := args[0]
	if name == defaultID || !accountName.MatchString(name) {
		return fmt.Errorf("%q is not a name a shared Grok login can take", name)
	}
	raw, err := io.ReadAll(io.LimitReader(in, 64*1024))
	if err != nil {
		return err
	}
	l, ok := parseGrokLogin(raw)
	switch {
	case !ok:
		return fmt.Errorf("no Grok login on stdin")
	case l.refreshable:
		return fmt.Errorf("refusing a login with a refresh token: share a token-only copy")
	case l.placeholder:
		return fmt.Errorf("refusing the placeholder login")
	case !l.expires.After(a.now()):
		return fmt.Errorf("the token has ended")
	}
	dir := a.dirFor(name)
	if held, ok := readGrokLogin(dir); ok {
		switch {
		case held.refreshable:
			return fmt.Errorf("%s holds a login of its own; not overwriting it", dir)
		case !l.expires.After(held.expires):
			fmt.Fprintf(out, "%s already has a token until %s.\n", name, held.expires.Format(time.RFC3339))
			return nil
		}
	} else if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s exists and is not a shared login; not touching it", dir)
	}
	if err := writeGrokAuth(dir, raw); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s now has a shared Grok login until %s.\n", name, l.expires.Format(time.RFC3339))
	return nil
}

// grokPlaceholderCommand writes the placeholder auth.json into dir (a Grok home for an agent) so grok
// starts there with no login. It never overwrites a login of someone's.
func grokPlaceholderCommand(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: limitbreak grok-placeholder <grok-home-dir>")
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	if l, ok := readGrokLogin(dir); ok && !l.placeholder {
		return fmt.Errorf("%s holds a real login; not overwriting it", dir)
	}
	if err := writeGrokAuth(dir, grokPlaceholderAuth()); err != nil {
		return err
	}
	fmt.Fprintf(out, "placeholder login written to %s\n", filepath.Join(dir, "auth.json"))
	return nil
}

// MARK: Sending

// grokShare is one account shared to one Mac: the source account here, the name it takes there, and
// what was last pushed.
type grokShare struct {
	Host string `json:"host"`
	// Name is the source account's id here; As is the account it becomes on Host.
	Name string `json:"name"`
	As   string `json:"as"`
	// Pushed is the fingerprint of the token last delivered; Error is the last push's failure.
	Pushed   string    `json:"pushed,omitempty"`
	PushedAt time.Time `json:"pushedAt,omitempty"`
	Error    string    `json:"error,omitempty"`
}

func (s grokShare) is(host, name string) bool { return s.Host == host && s.Name == name }

func sharesPath(home string) string {
	return filepath.Join(home, ".local", "state", "limitbreak", "grok-shares.json")
}

func readShares(home string) []grokShare {
	raw, err := os.ReadFile(sharesPath(home))
	if err != nil {
		return nil
	}
	var shares []grokShare
	_ = json.Unmarshal(raw, &shares)
	return shares
}

// updateShares reads the share list fresh, lets change edit it, and writes it back (0600, whole).
func updateShares(home string, change func([]grokShare) []grokShare) error {
	raw, err := json.MarshalIndent(change(readShares(home)), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(sharesPath(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".grok-shares-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), sharesPath(home))
}

// sshImportGrok delivers a token-only auth.json to `limitbreak import-grok` on an SSH host, unattended
// (no prompt, bounded waits): the owner's serve loop calls it with nobody there.
func sshImportGrok(host, name string, record []byte) (string, error) {
	return sshStdin(host, `"$HOME/.local/bin/limitbreak" import-grok `+shellQuote(name), record,
		"BatchMode=yes", "ConnectTimeout=15", "ServerAliveInterval=10", "ServerAliveCountMax=3")
}

// grokSharer keeps the shares' copies fresh: it refreshes the owner's login through grok and pushes.
type grokSharer struct {
	accounts *grokAccounts
	home     string
	send     remoteImport
	mu       sync.Mutex // one push at a time (the loop and the CLI never overlap on a token)
}

func newGrokSharer(a *grokAccounts, home string) *grokSharer {
	return &grokSharer{accounts: a, home: home, send: sshImportGrok}
}

// source is the share's account here.
func (s *grokSharer) source(sh grokShare) (account, bool) {
	for _, acc := range s.accounts.list() {
		if acc.ID == sh.Name {
			return acc, true
		}
	}
	return account{}, false
}

// push brings the copy on the share's host up to date: it refreshes the login when under
// grokShareRefreshWithin is left, and delivers the token-only copy when the token is not the one last
// delivered (or always, with force). It returns the share as it now stands.
func (s *grokSharer) push(sh grokShare, force bool) (grokShare, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.source(sh)
	if !ok {
		return sh, false, fmt.Errorf("no Grok account %q here", sh.Name)
	}
	l, _ := readGrokLogin(acc.Path)
	if !l.refreshable {
		return sh, false, fmt.Errorf("%s has no login of its own to share", sh.Name)
	}
	if l.expires.Before(s.accounts.now().Add(grokShareRefreshWithin)) {
		if err := s.accounts.refresh(acc.Path); err != nil {
			log.Printf("grok share %s: refresh failed: %v", sh.Name, err)
		} else {
			log.Printf("grok account %s refreshed through grok models for its shares", sh.Name)
		}
		l, _ = readGrokLogin(acc.Path)
	}
	if !l.expires.After(s.accounts.now()) {
		return sh, false, fmt.Errorf("the %s login's token has ended and could not be refreshed", sh.Name)
	}
	fp := tokenFingerprint(l.token)
	if !force && sh.Error == "" && sh.Pushed == fp {
		return sh, false, nil
	}
	raw, err := os.ReadFile(filepath.Join(acc.Path, "auth.json"))
	if err != nil {
		return sh, false, err
	}
	record, _, err := tokenOnlyGrok(raw)
	if err != nil {
		return sh, false, err
	}
	reply, err := s.send(sh.Host, sh.As, record)
	if err != nil {
		sh.Error = strings.TrimSpace(reply)
		if sh.Error == "" {
			sh.Error = err.Error()
		}
		return sh, false, fmt.Errorf("%s: %s", sh.Host, sh.Error)
	}
	sh.Pushed, sh.PushedAt, sh.Error = fp, s.accounts.now().UTC(), ""
	return sh, true, nil
}

// tick pushes every share that is due and records the outcome.
func (s *grokSharer) tick() {
	for _, sh := range readShares(s.home) {
		next, pushed, err := s.push(sh, false)
		if err != nil {
			log.Printf("grok share %s to %s: %v", sh.Name, sh.Host, err)
			if next.Error == "" {
				next.Error = err.Error()
			}
		} else if pushed {
			log.Printf("grok share %s pushed to %s (as %s)", sh.Name, sh.Host, sh.As)
		}
		if next != sh {
			s.record(next)
		}
	}
}

// record stores a share's new state over the one with the same host and account.
func (s *grokSharer) record(next grokShare) {
	err := updateShares(s.home, func(list []grokShare) []grokShare {
		if i := slices.IndexFunc(list, func(x grokShare) bool { return x.is(next.Host, next.Name) }); i >= 0 {
			list[i] = next
		}
		return list
	})
	if err != nil {
		log.Printf("grok shares: %v", err)
	}
}

// run ticks now and then every grokShareEvery, forever.
func (s *grokSharer) run() {
	for {
		s.tick()
		time.Sleep(grokShareEvery)
	}
}

// shareGrok shares Grok logins to host: each named account here, or every one with a login of its
// own, becomes a token-only account there (the default one as grokSharedName) that the serve loop then
// keeps fresh. The first copy is pushed now.
func shareGrok(s *grokSharer, args []string, out io.Writer) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: limitbreak share <ssh-host> [name...] --provider grok")
	}
	host := args[0]
	var names []string
	if len(args) > 1 {
		names = args[1:]
	} else {
		for _, acc := range s.accounts.list() {
			if l, _ := readGrokLogin(acc.Path); l.refreshable {
				names = append(names, acc.ID)
			}
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no Grok login here to share (limitbreak add --provider grok)")
	}
	var failed []string
	for _, name := range names {
		as := name
		if name == defaultID {
			as = grokSharedName
		}
		sh := grokShare{Host: host, Name: name, As: as}
		if old := slices.IndexFunc(readShares(s.home), func(x grokShare) bool { return x.is(host, name) }); old >= 0 {
			sh = readShares(s.home)[old]
		}
		sh, _, err := s.push(sh, true)
		if serr := updateShares(s.home, func(list []grokShare) []grokShare {
			if i := slices.IndexFunc(list, func(x grokShare) bool { return x.is(host, name) }); i >= 0 {
				list[i] = sh
				return list
			}
			return append(list, sh)
		}); serr != nil {
			return serr
		}
		if err != nil {
			fmt.Fprintf(out, "%s: %v (it is kept as a share and retried)\n", name, err)
			failed = append(failed, name)
			continue
		}
		fmt.Fprintf(out, "%s shared to %s as %s; kept fresh by this Mac while limitbreak serve runs.\n", name, host, as)
	}
	if len(failed) > 0 {
		return fmt.Errorf("not delivered to %s yet: %s", host, strings.Join(failed, ", "))
	}
	return nil
}

// unshareGrok stops sharing to host, all accounts or the named ones. The copies there stay until
// their tokens end.
func unshareGrok(home string, args []string, out io.Writer) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: limitbreak unshare <ssh-host> [name...] --provider grok")
	}
	host, names, removed := args[0], args[1:], 0
	err := updateShares(home, func(list []grokShare) []grokShare {
		kept := list[:0]
		for _, sh := range list {
			if sh.Host == host && (len(names) == 0 || slices.Contains(names, sh.Name)) {
				removed++
				continue
			}
			kept = append(kept, sh)
		}
		return kept
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped %d Grok share(s) to %s.\n", removed, host)
	return nil
}
