package main

// A sign-in's refresh token has a hard end (claudeAiOauth.refreshTokenExpiresAt, about 30 days)
// that refreshing does not move, so every login would need `claude auth login` once a month.
// `claude setup-token` issues a token good for a year with inference scope. That is all the proxy
// needs: it only sends /v1/messages with `Authorization: Bearer`. This file is the one owner of
// that per-account token: where it is kept, when it ends, and when to warn about an ending login.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	longTokenService = "limitbreak-token"
	longTokenFile    = ".limitbreak-token.json"
	longTokenLife    = 365 * 24 * time.Hour
	// longTokenMargin ends a token a day early, so clock skew or the minutes between issuing and
	// storing it never leave the proxy sending a dead one.
	longTokenMargin = 24 * time.Hour
	// renewWithin is when `limitbreak renew` (and `list`) treat an auth as due for renewal.
	renewWithin = 30 * 24 * time.Hour
	// warnWithin is when the running proxy starts warning that an account's auth is ending.
	warnWithin        = 7 * 24 * time.Hour
	warnEvery         = 24 * time.Hour
	authCheckInterval = 12 * time.Hour
)

type authKind int

const (
	authUnknown authKind = iota
	authSignIn
	authLongToken
)

// MARK: Decisions (pure)

type longToken struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"` // unix ms
	CreatedAt int64  `json:"createdAt"` // unix ms
}

func newLongToken(token string, now time.Time) longToken {
	return longToken{Token: token, CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(longTokenLife - longTokenMargin).UnixMilli()}
}

func (t longToken) valid(now time.Time) bool {
	return t.Token != "" && time.UnixMilli(t.ExpiresAt).After(now)
}

// authEnd is when an account stops being usable without the user: its long-lived token's end when
// it holds an unexpired one (the proxy prefers it), else the latest sign-in end across its
// credential sources. Zero and authUnknown when no source states one.
func authEnd(long *longToken, sources [][]byte, now time.Time) (time.Time, authKind) {
	if long != nil && long.valid(now) {
		return time.UnixMilli(long.ExpiresAt), authLongToken
	}
	var latest float64
	for _, raw := range sources {
		if l, ok := parseLogin(raw); ok && l.RefreshTokenExpiresAt != nil && *l.RefreshTokenExpiresAt > latest {
			latest = *l.RefreshTokenExpiresAt
		}
	}
	if latest == 0 {
		return time.Time{}, authUnknown
	}
	return time.UnixMilli(int64(latest)), authSignIn
}

// needsRenew is the one rule for "run limitbreak renew": anything but a long-lived token, or one
// that ends within renewWithin.
func needsRenew(end time.Time, kind authKind, now time.Time) bool {
	return kind != authLongToken || end.Before(now.Add(renewWithin))
}

// endsSoon is an auth the proxy warns about. An unknown end is not warned about: there is nothing
// to say when.
func endsSoon(end time.Time, now time.Time) bool {
	return !end.IsZero() && end.Before(now.Add(warnWithin))
}

func day(t time.Time) string { return t.Local().Format("2006-01-02") }

// authLine describes an account's auth for `limitbreak list`.
func authLine(id string, end time.Time, kind authKind, now time.Time) string {
	var s string
	switch {
	case kind == authLongToken:
		s = "1-year token until " + day(end)
	case kind == authSignIn && end.Before(now):
		s = "sign-in ended " + day(end)
	case kind == authSignIn:
		s = "sign-in ends " + day(end)
	default:
		s = "sign-in end unknown"
	}
	if needsRenew(end, kind, now) {
		s += " (run limitbreak renew " + id + ")"
	}
	return s
}

func authWarning(id string, end time.Time, kind authKind) string {
	what := "sign-in"
	if kind == authLongToken {
		what = "1-year token"
	}
	return fmt.Sprintf("account %s: %s ends %s; run \"limitbreak renew %s\"", id, what, day(end), id)
}

// MARK: Store

func longTokenPath(acc account) string { return filepath.Join(acc.Path, longTokenFile) }

// readLongToken is the account's stored long-lived token, expired or not; false when none.
func (a *accounts) readLongToken(acc account) (longToken, bool) {
	var raw []byte
	if a.credentials == credentialsKeychain {
		out, err := exec.Command("security", "find-generic-password", "-a", acc.ID, "-s", longTokenService, "-w").Output()
		if err != nil {
			return longToken{}, false
		}
		raw = out
	} else {
		out, err := os.ReadFile(longTokenPath(acc))
		if err != nil {
			return longToken{}, false
		}
		raw = out
	}
	var t longToken
	if json.Unmarshal(bytes.TrimSpace(raw), &t) != nil || t.Token == "" {
		return longToken{}, false
	}
	return t, true
}

func (a *accounts) writeLongToken(acc account, t longToken) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if a.credentials == credentialsKeychain {
		// `security` takes the secret as an argument, so it is visible in the process list for the
		// moment the command runs. Acceptable on a single-user Mac; it is never written to a log.
		out, err := exec.Command("security", "add-generic-password", "-U", "-a", acc.ID, "-s", longTokenService, "-w", string(raw)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("keychain: %v: %s", err, bytes.TrimSpace(out))
		}
		return nil
	}
	// A temp file is created 0600 and renamed over, so the token is never readable by others, not
	// even for a moment, and a crash leaves the old record whole.
	f, err := os.CreateTemp(acc.Path, ".limitbreak-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), longTokenPath(acc))
}

// deleteLongToken removes the account's long-lived token; none stored is not an error.
func (a *accounts) deleteLongToken(acc account) error {
	if a.credentials == credentialsKeychain {
		// Exit status 44 is "not found"; any other failure leaves nothing we can do from here.
		_ = exec.Command("security", "delete-generic-password", "-a", acc.ID, "-s", longTokenService).Run()
		return nil
	}
	if err := os.Remove(longTokenPath(acc)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// authInfo is the account's auth end and kind, read now.
func (a *accounts) authInfo(acc account) (time.Time, authKind) {
	var long *longToken
	if t, ok := a.readLongToken(acc); ok {
		long = &t
	}
	return authEnd(long, a.credentialSources(acc.Dir, acc.Path), a.now())
}

// MARK: Watch

// authWatch warns, from the running proxy, about accounts whose auth ends within warnWithin: a log
// line and a desktop notification, at most once per account per warnEvery.
type authWatch struct {
	accounts *accounts
	notify   func(message string)
	warned   map[string]time.Time
}

func newAuthWatch(a *accounts, notify func(string)) *authWatch {
	return &authWatch{accounts: a, notify: notify, warned: map[string]time.Time{}}
}

func (w *authWatch) check() {
	now := w.accounts.now()
	for _, acc := range w.accounts.list() {
		end, kind := w.accounts.authInfo(acc)
		if !endsSoon(end, now) {
			continue
		}
		if last, ok := w.warned[acc.ID]; ok && now.Sub(last) < warnEvery {
			continue
		}
		w.warned[acc.ID] = now
		message := authWarning(acc.ID, end, kind)
		log.Print(message)
		if w.notify != nil {
			w.notify(message)
		}
	}
}

func (w *authWatch) run() {
	for {
		w.check()
		time.Sleep(authCheckInterval)
	}
}

// notifyDesktop posts a macOS notification. The message goes in as an argument, not spliced into
// the script, so no quoting can break it.
func notifyDesktop(message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "osascript",
		"-e", "on run argv",
		"-e", `display notification (item 1 of argv) with title "limitbreak"`,
		"-e", "end run", message).Run()
}
