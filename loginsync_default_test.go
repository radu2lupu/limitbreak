package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A Mac's own default login (~/.grok, ~/.codex) is published under a name and stays where it is on that
// Mac; every other Mac gets ~/.<provider>-<name>.

func (m *mac) defaultDir() string { return filepath.Join(m.home, ".grok") }

func (m *mac) shareDefault(name string) {
	m.t.Helper()
	if action, err := m.s.publish(m.grokKind(), name, true); err != nil || action != "published" {
		m.t.Fatalf("share default as %s: %q %v", name, action, err)
	}
}

const ownerPath = "limitbreak/grok/radu2lupu"

func TestADefaultLoginIsPublishedUnderANameAndInstalledNamedOnOtherMacs(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(5*time.Hour))
	a.shareDefault("radu2lupu")

	// The owner keeps it in ~/.grok and is not a copy: /_status does not call it shared.
	if got := a.pass()[ownerPath]; got != actionUnchanged {
		t.Fatal(got)
	}
	if syncedAccounts(a.home, "grok")["radu2lupu"] || !syncedEntries(a.home, "grok")["radu2lupu"].Default {
		t.Fatalf("%+v", readSyncState(a.home))
	}
	if _, err := os.Stat(a.dir("radu2lupu")); err == nil {
		t.Fatal("the owner grew a named copy of its own login")
	}

	// B installs it as a named account and calls it shared.
	if got := b.pass()[ownerPath]; got != "installed" {
		t.Fatal(got)
	}
	if b.token("radu2lupu") == "" || b.token("radu2lupu") != tokenOf(t, a.defaultDir()) {
		t.Fatal("the login did not arrive whole")
	}
	list := newGrokAccounts(b.home, "", "http://127.0.0.1:1").list()
	if len(list) != 1 || list[0].ID != "radu2lupu" || !list[0].Shared || list[0].TokenEnds.IsZero() {
		t.Fatalf("%+v", list)
	}
	list = newGrokAccounts(a.home, "", "http://127.0.0.1:1").list()
	if len(list) != 1 || list[0].ID != defaultID || list[0].Shared {
		t.Fatalf("%+v", list)
	}
}

func tokenOf(t *testing.T, dir string) string {
	t.Helper()
	l, _ := readGrokLogin(dir)
	return l.token
}

func TestAMacNeverInstallsACofferLoginIntoItsOwnDefaultDir(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(5*time.Hour))
	a.shareDefault("radu2lupu")

	// B has no login of its own: still nothing lands in ~/.grok.
	b.pass()
	if _, err := os.Stat(b.defaultDir()); err == nil {
		t.Fatal("wrote into B's default dir")
	}
	// B has a different one of its own: untouched.
	writeGrokLogin(t, b.defaultDir(), "mine", clock.Add(4*time.Hour))
	own := tokenOf(t, b.defaultDir())
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(7*time.Hour))
	a.pass()
	b.pass()
	if tokenOf(t, b.defaultDir()) != own || b.token("radu2lupu") != tokenOf(t, a.defaultDir()) {
		t.Fatal("B's own login moved or the named copy did not follow")
	}
	if syncedEntries(b.home, "grok")["radu2lupu"].Default {
		t.Fatal("B thinks it owns it")
	}
}

func TestARefreshOfTheDefaultDirIsWrittenBackAndTheOtherMacPicksItUp(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(5*time.Hour))
	a.shareDefault("radu2lupu")
	b.pass()

	// grok itself refreshes ~/.grok outside any lease: the pass writes it back, the later end wins.
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(11*time.Hour))
	if got := a.pass()[ownerPath]; got != "published" {
		t.Fatal(got)
	}
	if got := b.pass()[ownerPath]; got != "installed" || b.token("radu2lupu") != tokenOf(t, a.defaultDir()) {
		t.Fatal(got)
	}
}

func TestLimitBreaksOwnRefreshOfTheDefaultDirTakesTheLeaseAndIsPickedUp(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(2*time.Hour)) // under the loop's 3h look-ahead
	a.shareDefault("radu2lupu")
	b.pass()

	for _, r := range a.s.renewDue() {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if a.refreshes != 1 || tokenOf(t, a.defaultDir()) == b.token("radu2lupu") {
		t.Fatal("the owner did not refresh its default dir", a.refreshes)
	}
	if _, v, _ := store.get(ownerPath); v != 2 {
		t.Fatalf("Coffer at version %d, want the refreshed login at 2", v)
	}
	// B finds nothing due after pulling A's login, so it never spends the refresh token.
	for _, r := range b.s.renewDue() {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	b.pass()
	if b.refreshes != 0 || b.token("radu2lupu") != tokenOf(t, a.defaultDir()) {
		t.Fatal(b.refreshes)
	}

	// And while another Mac holds the lease the owner does not refresh.
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(time.Hour))
	a.pass()
	release, err := b.s.takeLease("grok", "radu2lupu")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := a.grok(a.defaultDir()); err == nil || a.refreshes != 1 {
		t.Fatal("refreshed without the lease:", err, a.refreshes)
	}
}

func TestADefaultLoginIsRefusedWhereItWouldBeTwoCopiesOrNotALogin(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a := newMac(t, store, "a", &clock)
	k := a.grokKind()

	if _, err := a.s.publish(k, "radu2lupu", true); err == nil || !strings.Contains(err.Error(), "no grok login") {
		t.Fatal("published an absent default login:", err)
	}
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(5*time.Hour))
	// A named dir of that name already holds a login: two copies of one refresh token.
	writeGrokLogin(t, a.dir("radu2lupu"), "owner", clock.Add(5*time.Hour))
	if _, err := a.s.publish(k, "radu2lupu", true); err == nil {
		t.Fatal("made two local copies")
	}
	os.RemoveAll(a.dir("radu2lupu"))
	a.shareDefault("radu2lupu")
	// One name per default login.
	if _, err := a.s.publish(k, "second", true); err == nil {
		t.Fatal("published the default login twice")
	}
	// Syncing it as a named one now is a change of mind, not a no-op.
	if _, err := a.s.publish(k, "radu2lupu", false); err == nil {
		t.Fatal("silently switched the login's dir")
	}
	// Claude's default login is in the Keychain, not a dir.
	ck, _ := a.s.kind("claude")
	if _, err := a.s.publish(ck, "me", true); err == nil {
		t.Fatal("published a Claude default login")
	}
	// A login with no refresh token is not worth publishing.
	noRefresh, _ := dropRefreshToken(grokAuthJSON("x", clock.Add(time.Hour)))
	os.WriteFile(filepath.Join(a.defaultDir(), "auth.json"), noRefresh, 0o600)
	if err := a.s.unshare(k, "radu2lupu"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.publish(k, "radu2lupu", true); err == nil {
		t.Fatal("published a login that cannot refresh")
	}
}

func TestUnsharingTheOwnersLoginKeepsItInTheDefaultDirAndItCanBeSharedAgain(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a := newMac(t, store, "a", &clock)
	writeGrokLogin(t, a.defaultDir(), "owner", clock.Add(5*time.Hour))
	a.shareDefault("radu2lupu")
	if err := a.s.unshare(a.grokKind(), "radu2lupu"); err != nil {
		t.Fatal(err)
	}
	before := tokenOf(t, a.defaultDir())
	if got := a.pass()[ownerPath]; got != actionIgnored || tokenOf(t, a.defaultDir()) != before {
		t.Fatal(got)
	}
	if action, err := a.s.publish(a.grokKind(), "radu2lupu", true); err != nil || !strings.Contains(action, "already in Coffer") || !syncedEntries(a.home, "grok")["radu2lupu"].Default {
		t.Fatal(action, err)
	}
}

func TestACodexDefaultLoginIsPublishedLikeAGrokOne(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeCodexLogin(t, filepath.Join(a.home, ".codex"), "owner", "acct", clock.Add(48*time.Hour))
	k, _ := a.s.kind("codex")
	if action, err := a.s.publish(k, "radu2lupu", true); err != nil || action != "published" {
		t.Fatal(action, err)
	}
	var installed bool
	for _, r := range b.s.tick() {
		installed = installed || (r.Path == "limitbreak/codex/radu2lupu" && r.Action == "installed" && r.Err == nil)
	}
	if !installed {
		t.Fatal("not installed on B")
	}
	if _, err := os.Stat(filepath.Join(b.home, ".codex-radu2lupu", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.home, ".codex")); err == nil {
		t.Fatal("B's default Codex dir was created")
	}
}

func TestShareDefaultAsANameFromTheCommandLine(t *testing.T) {
	store := newFakeCoffer()
	setCofferEnv(t, store)
	owner, other := t.TempDir(), t.TempDir()
	writeGrokLogin(t, filepath.Join(owner, ".grok"), "owner", time.Now().Add(5*time.Hour))

	var out strings.Builder
	if err := shareCommand("share", owner, []string{"--provider", "grok", "default", "--as", "radu2lupu"}, &out); err != nil || !strings.Contains(out.String(), "grok/radu2lupu (this Mac's default login): published") {
		t.Fatal(err, out.String())
	}
	out.Reset()
	if err := syncNowCommand(other, &out); err != nil || !strings.Contains(out.String(), "limitbreak/grok/radu2lupu: installed") {
		t.Fatal(err, out.String())
	}
	if _, ok := readGrokLogin(filepath.Join(other, ".grok-radu2lupu")); !ok {
		t.Fatal("not installed as a named account")
	}
	// The default login needs a name, and a name goes only with it.
	for _, args := range [][]string{{"default", "--provider", "grok"}, {"work", "--as", "x", "--provider", "grok"}, {"default", "--as"}} {
		if err := shareCommand("share", owner, args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := shareCommand("unshare", owner, []string{"default", "--as", "x", "--provider", "grok"}, &out); err == nil {
		t.Fatal("--as accepted on unshare")
	}
}
