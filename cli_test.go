package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signIn stands in for the browser sign-in: it leaves a login (and who it is) in the config dir.
func signIn(t *testing.T, email string) func(dir string) error {
	return func(dir string) error {
		writeJSON(t, filepath.Join(dir, ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{"accessToken": "t-" + email, "expiresAt": float64(4_000_000_000_000)}})
		writeJSON(t, filepath.Join(dir, ".claude.json"), map[string]any{"oauthAccount": map[string]any{"emailAddress": email}})
		return nil
	}
}

func TestAddNamesTheNextFreeAccountAndReportsWhoSignedIn(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "account-1": "t-one"})
	a := newClaudeAccounts(home, credentialsFile, "")
	var out bytes.Buffer
	if err := addAccount(a, "", signIn(t, "new@example.com"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Added account-2 (new@example.com). 3 accounts now.") {
		t.Fatal(out.String())
	}
	if len(a.list()) != 3 {
		t.Fatal("the new login is not discovered")
	}
}

func TestAddWarnsWhenTheSameLoginIsAddedTwice(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default"})
	var out bytes.Buffer
	if err := addAccount(newClaudeAccounts(home, credentialsFile, ""), "again", signIn(t, "default@example.com"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "same login") {
		t.Fatal(out.String())
	}
}

func TestAddRefusesBadNamesAndCleansUpAFailedSignIn(t *testing.T) {
	home := t.TempDir()
	a := newClaudeAccounts(home, credentialsFile, "")
	for _, name := range []string{"default", "../x", "a b"} {
		if err := addAccount(a, name, signIn(t, "x@example.com"), &bytes.Buffer{}); err == nil {
			t.Fatalf("%q accepted", name)
		}
	}
	failing := func(string) error { return os.ErrPermission }
	if err := addAccount(a, "work", failing, &bytes.Buffer{}); err == nil {
		t.Fatal("failed sign-in reported as added")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude-work")); !os.IsNotExist(err) {
		t.Fatal("empty dir left behind")
	}
	noLogin := func(string) error { return nil }
	if err := addAccount(a, "work", noLogin, &bytes.Buffer{}); err == nil {
		t.Fatal("sign-in without a login reported as added")
	}
}

func TestRemoveDeletesTheLoginAfterConfirmationAndNeverTheDefault(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	a := newClaudeAccounts(home, credentialsFile, "")
	var out bytes.Buffer
	if err := removeAccount(a, []string{"work"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	if len(a.list()) != 2 {
		t.Fatal("removed without confirmation")
	}
	if err := removeAccount(a, []string{"default", "-y"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("default removed")
	}
	if err := removeAccount(a, []string{"work"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if list := a.list(); len(list) != 1 || list[0].ID != "default" {
		t.Fatal(list)
	}
}

func TestListMarksTheActiveAccountFromStatus(t *testing.T) {
	raw := []byte(`[{"id":"default","spentUntil":null,"activeFor":[]},{"id":"work","spentUntil":null,"activeFor":["shared"]}]`)
	if activeFromStatus(raw) != "work" {
		t.Fatal("active not read")
	}
}

// fakeSetupToken stands in for `claude setup-token` under script(1): it records a messy terminal
// session holding a token named after the account, and remembers what it ran and where.
type fakeSetupToken struct {
	ran         []string
	typescripts []string
}

func (f *fakeSetupToken) run(acc account, typescript string) error {
	f.ran = append(f.ran, acc.ID)
	f.typescripts = append(f.typescripts, typescript)
	info, err := os.Stat(typescript)
	if err != nil || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("typescript not a private file: %v %v", info, err)
	}
	return os.WriteFile(typescript, []byte("\x1b[1msk-ant-oat01-new-"+acc.ID+"\x1b[22m\r\n\r\nStore this token securely.\r\n"), 0o600)
}

func TestRenewSkipsFreshTokensUnlessAllAndNeverStoresARejectedOne(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work", "spare": "t-spare"})
	a := newClaudeAccounts(home, credentialsFile, "")
	if err := a.writeLongToken(accountNamed(t, a, "default"), newLongToken("sk-ant-oat01-fresh", time.Now())); err != nil {
		t.Fatal(err)
	}
	var validated []string
	ok := func(token string) (int, error) { validated = append(validated, token); return 200, nil }

	fake := &fakeSetupToken{}
	var out bytes.Buffer
	if err := renewAccounts(a, nil, fake.run, ok, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if strings.Join(fake.ran, ",") != "spare,work" {
		t.Fatal(fake.ran)
	}
	if !strings.Contains(out.String(), "Renewing work (work@example.com). In the browser, make sure claude.ai is signed in as work@example.com, then authorize.") ||
		!strings.Contains(out.String(), "default has a 1-year token until") {
		t.Fatal(out.String())
	}
	for _, ts := range fake.typescripts {
		if _, err := os.Stat(filepath.Dir(ts)); !os.IsNotExist(err) {
			t.Fatal("typescript left behind", ts)
		}
	}
	if got := a.readToken(accountNamed(t, a, "work")); got != "sk-ant-oat01-new-work" {
		t.Fatal(got)
	}
	if got := a.readToken(accountNamed(t, a, "default")); got != "sk-ant-oat01-fresh" {
		t.Fatal(got)
	}

	fake = &fakeSetupToken{}
	if err := renewAccounts(a, []string{"--all"}, fake.run, ok, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.ran, ",") != "default,spare,work" {
		t.Fatal(fake.ran)
	}

	rejected := func(string) (int, error) { return 401, nil }
	a.deleteLongToken(accountNamed(t, a, "work"))
	out.Reset()
	if err := renewAccounts(a, []string{"work", "nobody"}, (&fakeSetupToken{}).run, rejected, &out); err == nil {
		t.Fatal("a rejected token reported as renewed")
	}
	if _, stored := a.readLongToken(accountNamed(t, a, "work")); stored {
		t.Fatal("a rejected token was stored")
	}
	// An unknown name fails while choosing, before any account is renewed.
	if !strings.Contains(out.String(), "Failed: nobody (no such account); work (Anthropic rejected the new token (HTTP 401); not stored).") {
		t.Fatal(out.String())
	}

	noToken := func(account, string) error { return nil }
	if err := renewAccounts(a, []string{"spare"}, noToken, ok, &bytes.Buffer{}); err == nil {
		t.Fatal("a run without a token reported as renewed")
	}
}

func TestRemoveAlsoDeletesTheLongToken(t *testing.T) {
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	a := newClaudeAccounts(home, credentialsFile, "")
	work := accountNamed(t, a, "work")
	a.writeLongToken(work, newLongToken("sk-ant-oat01-x", time.Now()))
	if err := removeAccount(a, []string{"work", "-y"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.readLongToken(work); ok {
		t.Fatal("token left after remove")
	}
}

func TestCountTokensValidatorAsksWithTheTokenAndOAuthBeta(t *testing.T) {
	var got *http.Request
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, body = r, string(raw)
		w.WriteHeader(403)
	}))
	defer server.Close()
	status, err := countTokensValidator(server.URL + "/")("sk-ant-oat01-x")
	if err != nil || status != 403 {
		t.Fatal(status, err)
	}
	if got.URL.Path != "/v1/messages/count_tokens" || got.Header.Get("Authorization") != "Bearer sk-ant-oat01-x" ||
		got.Header.Get("anthropic-version") != "2023-06-01" || got.Header.Get("anthropic-beta") != "oauth-2025-04-20" ||
		!strings.Contains(body, `"model":"claude-haiku-4-5"`) {
		t.Fatal(got.URL.Path, got.Header, body)
	}
}

func TestWithLoginHint(t *testing.T) {
	link := "https://claude.com/cai/oauth/authorize?code=true&scope=user%3Ainference&state=abc"
	got := withLoginHint(link, "a+b@example.com")
	u, _ := url.Parse(got)
	if u.Query().Get("login_hint") != "a+b@example.com" || u.Query().Get("state") != "abc" || u.Query().Get("scope") != "user:inference" {
		t.Fatalf("hint not added cleanly: %s", got)
	}
	for _, tc := range []struct{ link, email string }{
		{link, ""},
		{"https://evil.example/cai/oauth/authorize?x=1", "a@b.c"},
		{"https://claude.com/settings", "a@b.c"},
		{link + "&login_hint=other%40x.com", "a@b.c"},
	} {
		if got := withLoginHint(tc.link, tc.email); got != tc.link {
			t.Errorf("withLoginHint(%q, %q) = %q, want unchanged", tc.link, tc.email, got)
		}
	}
}

func TestShareCopiesFreshTokensToTheSameNamedAccounts(t *testing.T) {
	src := newClaudeAccounts(makeHome(t, map[string]string{"default": "a", "work": "b", "spare": "c"}), credentialsFile, "")
	dst := newClaudeAccounts(makeHome(t, map[string]string{"default": "x", "work": "y"}), credentialsFile, "")
	now := time.Now()
	src.writeLongToken(accountNamed(t, src, "default"), newLongToken("sk-ant-oat01-def", now))
	src.writeLongToken(accountNamed(t, src, "spare"), newLongToken("sk-ant-oat01-spare", now))
	expired := newLongToken("sk-ant-oat01-old", now.Add(-400*24*time.Hour))
	src.writeLongToken(accountNamed(t, src, "work"), expired)

	ok := func(string) (int, error) { return 200, nil }
	send := func(host, id string, record []byte) (string, error) {
		var out bytes.Buffer
		err := importToken(dst, []string{id}, ok, bytes.NewReader(record), &out)
		if err != nil {
			return err.Error(), err
		}
		return strings.TrimSpace(out.String()), nil
	}
	var out bytes.Buffer
	err := shareTokens(src, []string{"mini"}, send, &out)
	// spare does not exist on the destination, so it fails; work is skipped as expired.
	if err == nil || !strings.Contains(err.Error(), "spare") || !strings.Contains(out.String(), "work has no 1-year token") {
		t.Fatal(err, out.String())
	}
	if got := dst.readToken(accountNamed(t, dst, "default")); got != "sk-ant-oat01-def" {
		t.Fatal(got)
	}
	if got := dst.readToken(accountNamed(t, dst, "work")); got != "y" {
		t.Fatal(got)
	}

	rejected := func(string) (int, error) { return 401, nil }
	record, _ := json.Marshal(newLongToken("sk-ant-oat01-bad", now))
	if importToken(dst, []string{"work"}, rejected, bytes.NewReader(record), &bytes.Buffer{}) == nil {
		t.Fatal("a rejected token was imported")
	}
	if importToken(dst, []string{"work"}, ok, strings.NewReader("{}"), &bytes.Buffer{}) == nil {
		t.Fatal("an empty record was imported")
	}
}

func TestKeychainModeAlsoReadsTheTokenFile(t *testing.T) {
	home := makeHome(t, map[string]string{"work": "y"})
	file := newClaudeAccounts(home, credentialsFile, "")
	acc := accountNamed(t, file, "work")
	acc.ID = "limitbreak-test-absent-" + strings.ReplaceAll(t.Name(), "/", "-") // never in the real Keychain
	file.writeLongToken(acc, newLongToken("sk-ant-oat01-file", time.Now()))
	kc := newClaudeAccounts(home, credentialsKeychain, "")
	if got, ok := kc.readLongToken(acc); !ok || got.Token != "sk-ant-oat01-file" {
		t.Fatal(got, ok)
	}
}
