package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func accountNamed(t *testing.T, a *accounts, id string) account {
	t.Helper()
	for _, acc := range freshAccounts(a) {
		if acc.ID == id {
			return acc
		}
	}
	t.Fatalf("no account %q", id)
	return account{}
}

func TestLongTokenRoundTripsThroughAPrivateFile(t *testing.T) {
	a := newAccounts(makeHome(t, map[string]string{"work": "t-work"}), credentialsFile, "")
	acc := accountNamed(t, a, "work")
	if _, ok := a.readLongToken(acc); ok {
		t.Fatal("token before any was written")
	}
	want := newLongToken("sk-ant-oat01-abc", time.Unix(1_800_000_000, 0))
	if err := a.writeLongToken(acc, want); err != nil {
		t.Fatal(err)
	}
	got, ok := a.readLongToken(acc)
	if !ok || got != want {
		t.Fatal(got, ok)
	}
	if want.ExpiresAt-want.CreatedAt != (364 * 24 * time.Hour).Milliseconds() {
		t.Fatal("expiry is not a year less a day", want)
	}
	info, err := os.Stat(filepath.Join(acc.Path, longTokenFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode(), err)
	}
	if err := a.deleteLongToken(acc); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.readLongToken(acc); ok {
		t.Fatal("token left after delete")
	}
	if err := a.deleteLongToken(acc); err != nil {
		t.Fatal("deleting nothing is not an error", err)
	}
}

func TestReadTokenPrefersAnUnexpiredLongTokenAndIgnoresAnExpiredOne(t *testing.T) {
	a := newAccounts(makeHome(t, map[string]string{"work": "t-work"}), credentialsFile, "")
	acc := accountNamed(t, a, "work")
	now := time.Now()
	a.writeLongToken(acc, newLongToken("sk-ant-oat01-long", now))
	if got := a.readToken(acc); got != "sk-ant-oat01-long" {
		t.Fatal(got)
	}
	a.writeLongToken(acc, newLongToken("sk-ant-oat01-old", now.Add(-400*24*time.Hour)))
	if got := a.readToken(acc); got != "t-work" {
		t.Fatal(got)
	}
}

func TestAuthEndIsTheLongTokenElseTheLatestSignInEnd(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signIn := func(d time.Duration) []byte {
		return []byte(`{"claudeAiOauth":{"accessToken":"x","refreshTokenExpiresAt":` + strconv.FormatInt(now.Add(d).UnixMilli(), 10) + `}}`)
	}
	sources := [][]byte{signIn(5 * 24 * time.Hour), []byte("not json"), signIn(20 * 24 * time.Hour), []byte(`{"claudeAiOauth":{"accessToken":"y"}}`)}

	end, kind := authEnd(nil, sources, now)
	if kind != authSignIn || !end.Equal(now.Add(20*24*time.Hour)) {
		t.Fatal(end, kind)
	}
	long := newLongToken("sk-ant-oat01-a", now)
	if end, kind := authEnd(&long, sources, now); kind != authLongToken || end.UnixMilli() != long.ExpiresAt {
		t.Fatal(end, kind)
	}
	expired := newLongToken("sk-ant-oat01-a", now.Add(-400*24*time.Hour))
	if _, kind := authEnd(&expired, sources, now); kind != authSignIn {
		t.Fatal("an expired long token still counted")
	}
	if end, kind := authEnd(nil, [][]byte{[]byte(`{"claudeAiOauth":{"accessToken":"y"}}`)}, now); kind != authUnknown || !end.IsZero() {
		t.Fatal(end, kind)
	}

	if !needsRenew(now.Add(300*24*time.Hour), authSignIn, now) || needsRenew(now.Add(300*24*time.Hour), authLongToken, now) || !needsRenew(now.Add(10*24*time.Hour), authLongToken, now) {
		t.Fatal("needsRenew")
	}
	if !endsSoon(now.Add(6*24*time.Hour), now) || endsSoon(now.Add(8*24*time.Hour), now) || endsSoon(time.Time{}, now) {
		t.Fatal("endsSoon")
	}
	if got := authLine("work", long.expires(), authLongToken, now); got != "1-year token until "+day(long.expires()) {
		t.Fatal(got)
	}
	if got := authLine("work", end, authSignIn, now); got != "sign-in ends "+day(end)+" (run limitbreak renew work)" {
		t.Fatal(got)
	}
	if got := authLine("work", time.Time{}, authUnknown, now); !strings.HasSuffix(got, "(run limitbreak renew work)") {
		t.Fatal(got)
	}
}

func (t longToken) expires() time.Time { return time.UnixMilli(t.ExpiresAt) }

func TestExtractTokenFromAMessyTypescript(t *testing.T) {
	const stale = "sk-ant-oat01-STALEstale"
	const head = "sk-ant-oat01-Abc_def-123"
	const tail = "XyZ-789_qq"
	recording := "Script started\r\n" +
		"\x1b]0;claude\x07\x1b[?25l\x1b[2K\x1b[1A\x1b[G" + stale + "\r\n\r\n" + // an earlier frame
		"\x1b[32m✓\x1b[39m Long-lived authentication token created successfully!\r\n\r\n" +
		"Your OAuth token (valid for 1 year):\r\n\r\n" +
		"\x1b[1m" + head + "\x1b[22m   \r\n" + // wrapped by the TUI, with padding
		"  \x1b[1m" + tail + "\x1b[22m\r\n" +
		"\r\n" +
		"Store this token securely.\r\n\x1b[?25h"
	if got := extractToken(recording); got != head+tail {
		t.Fatalf("%q", got)
	}
	// A token at the end of its line is not glued to the prose that follows.
	if got := extractToken("x " + head + "\r\nStore this token securely.\r\n"); got != head {
		t.Fatalf("%q", got)
	}
	// Nor to a following frame that prints the token again.
	if got := extractToken(head + "\n" + head + tail + "\n"); got != head+tail {
		t.Fatalf("%q", got)
	}
	// A cursor-forward stands for a space and ends the token.
	if got := extractToken(head + "\x1b[1CStore"); got != head {
		t.Fatalf("%q", got)
	}
	if got := extractToken("no token here\r\n"); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestScriptCommandPerPlatform(t *testing.T) {
	if got := strings.Join(scriptCommand("darwin", "/bin/claude", "/tmp/ts"), " "); got != "script -q /tmp/ts /bin/claude setup-token" {
		t.Fatal(got)
	}
	got := scriptCommand("linux", "/opt/it's/claude", "/tmp/ts")
	if strings.Join(got, "|") != `script|-q|-c|'/opt/it'\''s/claude' setup-token|/tmp/ts` {
		t.Fatal(got)
	}
}

func TestAuthWatchWarnsAboutAnEndingSignInOncePerDay(t *testing.T) {
	home := makeHome(t, map[string]string{"work": "t-work", "fine": "t-fine"})
	now := time.Unix(1_800_000_000, 0)
	writeJSON(t, filepath.Join(home, ".claude-work", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "t-work", "expiresAt": float64(far) * 1000, "refreshTokenExpiresAt": now.Add(3 * 24 * time.Hour).UnixMilli()}})
	writeJSON(t, filepath.Join(home, ".claude-fine", ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "t-fine", "expiresAt": float64(far) * 1000, "refreshTokenExpiresAt": now.Add(20 * 24 * time.Hour).UnixMilli()}})
	a := newAccounts(home, credentialsFile, "")
	a.now = func() time.Time { return now }
	var sent []string
	w := newAuthWatch(a, func(m string) { sent = append(sent, m) })
	w.check()
	w.check()
	if len(sent) != 1 || !strings.Contains(sent[0], `account work: sign-in ends`) || !strings.Contains(sent[0], `limitbreak renew work`) {
		t.Fatal(sent)
	}
	now = now.Add(25 * time.Hour)
	w.check()
	if len(sent) != 2 {
		t.Fatal(sent)
	}
}
