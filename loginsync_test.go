package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCoffer is a Coffer: versioned secrets with a compare-and-swap put, listed by pattern. It is
// a cofferStore itself and, through handler, speaks Coffer's wire contract for the real client.
type fakeCoffer struct {
	mu   sync.Mutex
	data map[string][]byte
	ver  map[string]int64
	down bool
	puts int
}

func newFakeCoffer() *fakeCoffer {
	return &fakeCoffer{data: map[string][]byte{}, ver: map[string]int64{}}
}

func (f *fakeCoffer) get(path string) ([]byte, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, 0, errors.New("coffer unreachable")
	}
	val, ok := f.data[path]
	if !ok {
		return nil, 0, errCofferNotFound
	}
	return append([]byte(nil), val...), f.ver[path], nil
}

func (f *fakeCoffer) putIf(path string, value []byte, expect int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return 0, errors.New("coffer unreachable")
	}
	if f.ver[path] != expect {
		return 0, errCofferConflict
	}
	f.data[path] = append([]byte(nil), value...)
	f.ver[path]++
	f.puts++
	return f.ver[path], nil
}

// list matches a pattern whose last segment is "*": one or more further segments.
func (f *fakeCoffer) list(pattern string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("coffer unreachable")
	}
	prefix := strings.TrimSuffix(pattern, "*")
	var out []string
	for path := range f.data {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			out = append(out, path)
		}
	}
	return out, nil
}

func (f *fakeCoffer) handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"bad token"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/secrets/") && r.Method == http.MethodGet:
			val, v, err := f.get(strings.TrimPrefix(r.URL.Path, "/v1/secrets/"))
			if errors.Is(err, errCofferNotFound) {
				http.Error(w, `{"error":"no such secret"}`, http.StatusNotFound)
				return
			}
			w.Header().Set(cofferVersionHeader, strconv.FormatInt(v, 10))
			w.Write(val)
		case strings.HasPrefix(r.URL.Path, "/v1/secrets/") && r.Method == http.MethodPut:
			expect, err := strconv.ParseInt(r.URL.Query().Get("if-version"), 10, 64)
			if err != nil {
				http.Error(w, `{"error":"bad if-version"}`, http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			v, err := f.putIf(strings.TrimPrefix(r.URL.Path, "/v1/secrets/"), body, expect)
			if errors.Is(err, errCofferConflict) {
				http.Error(w, `{"error":"version conflict"}`, http.StatusConflict)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"version": v})
		case r.URL.Path == "/v1/list":
			paths, _ := f.list(r.URL.Query().Get("pattern"))
			json.NewEncoder(w).Encode(map[string]any{"paths": paths})
		default:
			http.NotFound(w, r)
		}
	})
}

// mac is one Mac's Limit Break: its own home, syncer and refresh.
type mac struct {
	t         *testing.T
	home      string
	s         *loginSyncer
	clock     *time.Time
	refreshes int
	grok      func(dir string) error // the guarded refresh
}

func newMac(t *testing.T, store cofferStore, name string, clock *time.Time) *mac {
	t.Helper()
	m := &mac{t: t, home: t.TempDir(), clock: clock}
	claude := newClaudeAccounts(m.home, credentialsFile, "")
	m.s = newLoginSyncer(m.home, store, loginKinds(claude, nil))
	m.s.me = name
	m.s.now = func() time.Time { return *clock }
	// grok's own refresh: a fresh 6-hour login for the same account.
	m.grok = m.s.guard("grok", func(dir string) error {
		m.refreshes++
		writeGrokLogin(t, dir, "work", clock.Add(6*time.Hour))
		return nil
	})
	return m
}

func (m *mac) dir(name string) string { return filepath.Join(m.home, ".grok-"+name) }

func (m *mac) grokKind() syncKind {
	k, _ := m.s.kind("grok")
	return k
}

func (m *mac) token(name string) string {
	l, _ := readGrokLogin(m.dir(name))
	return l.token
}

func (m *mac) mustPublish(name string) {
	m.t.Helper()
	if action, err := m.s.publish(m.grokKind(), name, false); err != nil || action != "published" {
		m.t.Fatalf("publish %s: %q %v", name, action, err)
	}
}

// pass is a sync pass; it fails the test on an error and returns the actions by path.
func (m *mac) pass() map[string]string {
	m.t.Helper()
	out := map[string]string{}
	for _, r := range m.s.tick() {
		if r.Err != nil {
			m.t.Fatalf("%s: %v", r.Path, r.Err)
		}
		out[r.Path] = r.Action
	}
	return out
}

const workPath = "limitbreak/grok/work"

func TestALoginPublishedOnOneMacIsInstalledOnTheOtherAndTracked(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(5*time.Hour))
	a.mustPublish("work")
	if !syncedAccounts(a.home, "grok")["work"] {
		t.Fatal("the publishing Mac does not track it")
	}

	if got := b.pass()[workPath]; got != "installed" {
		t.Fatal(got)
	}
	if b.token("work") != a.token("work") || b.token("work") == "" {
		t.Fatal("the login did not arrive whole")
	}
	if got := b.pass()[workPath]; got != actionUnchanged {
		t.Fatal(got)
	}
	info, _ := os.Stat(filepath.Join(b.dir("work"), "auth.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}

	// The state file says versions and fingerprints, never a token.
	raw, _ := os.ReadFile(stateFile(b.home, "coffer-sync.json"))
	if strings.Contains(string(raw), b.token("work")) || !strings.Contains(string(raw), workPath) {
		t.Fatalf("%s", raw)
	}

	// And it is an account, reported as shared with its token's end.
	g := newGrokAccounts(b.home, "", "http://127.0.0.1:1")
	list := g.list()
	if len(list) != 1 || list[0].ID != "work" || !list[0].Shared || list[0].TokenEnds.IsZero() {
		t.Fatalf("%+v", list)
	}
}

func TestOneMacRefreshesAndTheOtherPicksTheLoginUpWithoutRefreshing(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(30*time.Minute)) // under grokRefreshWithin
	a.mustPublish("work")
	b.pass()

	// A's request finds it due: it takes the lease, refreshes, and writes the new login back.
	if err := a.grok(a.dir("work")); err != nil || a.refreshes != 1 {
		t.Fatal(err, a.refreshes)
	}
	if _, v, _ := store.get(workPath); v != 2 {
		t.Fatalf("Coffer is at version %d, want the refreshed login at 2", v)
	}
	// The lease is let go: B may take it, but first it pulls A's login, which is no longer due.
	if err := b.grok(b.dir("work")); err != nil || b.refreshes != 0 {
		t.Fatal("B spent the refresh token again:", err, b.refreshes)
	}
	if b.token("work") != a.token("work") {
		t.Fatal("B did not pull A's refreshed login")
	}
}

func TestAMacWithoutTheLeaseDoesNotRefresh(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(30*time.Minute))
	a.mustPublish("work")
	b.pass()

	// A holds the lease (it is mid-refresh).
	release, err := a.s.takeLease("grok", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.grok(b.dir("work")); !errors.Is(err, errLeaseHeld) || b.refreshes != 0 {
		t.Fatal(err, b.refreshes)
	}
	// A crash never lets it go: after the TTL B may take over.
	clock = clock.Add(loginLeaseTTL + time.Second)
	writeGrokLogin(t, b.dir("work"), "work", clock.Add(10*time.Minute)) // still due, same login line
	if err := b.s.guard("grok", func(string) error { return nil })(b.dir("work")); err != nil {
		t.Fatal(err)
	}
	release() // a late release by the old holder must not free B's... it holds nothing now
}

func TestWithCofferDownLocalLoginsKeepWorkingButNothingRotatingRefreshes(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a := newMac(t, store, "a", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(30*time.Minute))
	a.mustPublish("work")
	before := a.token("work")

	store.down = true
	for _, r := range a.s.tick() {
		if r.Err == nil {
			t.Fatalf("%+v", r)
		}
	}
	if err := a.grok(a.dir("work")); err == nil || a.refreshes != 0 {
		t.Fatal("a rotating login was refreshed without a lease:", err, a.refreshes)
	}
	if a.token("work") != before {
		t.Fatal("the local login changed")
	}

	// Coffer is back: a login a refresh left only on disk is written back (the compare-and-swap).
	store.down = false
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(6*time.Hour))
	if got := a.pass()[workPath]; got != "published" {
		t.Fatal(got)
	}
	val, v, _ := store.get(workPath)
	if v != 2 || fingerprint(val) != fingerprint(mustReadFile(t, filepath.Join(a.dir("work"), "auth.json"))) {
		t.Fatal(v)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWhenBothMovedTheLaterEndingLoginWins(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(time.Hour))
	a.mustPublish("work")
	b.pass()

	// B refreshes locally while its Coffer is out of reach; A publishes a longer one meanwhile.
	writeGrokLogin(t, b.dir("work"), "work", clock.Add(2*time.Hour))
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(6*time.Hour))
	if got := a.pass()[workPath]; got != "published" {
		t.Fatal(got)
	}
	if got := b.pass()[workPath]; got != "installed" {
		t.Fatal("B kept the shorter login:", got)
	}
	if b.token("work") != a.token("work") {
		t.Fatal("the Macs differ")
	}

	// The other way round: the local one ends later, so it is written back over Coffer's.
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(7*time.Hour))
	a.pass()
	writeGrokLogin(t, b.dir("work"), "work", clock.Add(9*time.Hour))
	if got := b.pass()[workPath]; got != "published" {
		t.Fatal(got)
	}
	if got := a.pass()[workPath]; got != "installed" || a.token("work") != b.token("work") {
		t.Fatal(got)
	}
}

func TestALosingCompareAndSwapWritesNothingAndTheNextPassReadsAgain(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a := newMac(t, store, "a", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(time.Hour))
	a.mustPublish("work")
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(2*time.Hour))
	// Another Mac wrote between A's read and A's write.
	if _, err := a.s.writeBack(workPath, mustReadFile(t, filepath.Join(a.dir("work"), "auth.json")), "x", 7); err != nil {
		t.Fatal(err)
	}
	if store.puts != 1 {
		t.Fatalf("a conflicting write went through (%d puts)", store.puts)
	}
}

func TestACrashBetweenInstallAndStateIsRecoveredByTheFingerprint(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(5*time.Hour))
	a.mustPublish("work")
	// B wrote the login, then died before recording it.
	os.MkdirAll(b.dir("work"), 0o700)
	os.WriteFile(filepath.Join(b.dir("work"), "auth.json"), mustReadFile(t, filepath.Join(a.dir("work"), "auth.json")), 0o600)
	if got := b.pass()[workPath]; got != "synced" || !syncedAccounts(b.home, "grok")["work"] {
		t.Fatal(got)
	}
}

func TestALoginOfItsOwnAndAnotherToolsDirAreNeverOverwritten(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(5*time.Hour))
	a.mustPublish("work")

	// B has a different login under the name and no record of syncing it.
	writeGrokLogin(t, b.dir("work"), "mine", clock.Add(4*time.Hour))
	own := b.token("work")
	var err error
	for _, r := range b.s.tick() {
		err = r.Err
	}
	if err == nil || b.token("work") != own || syncedAccounts(b.home, "grok")["work"] {
		t.Fatal("overwrote a login of its own:", err)
	}

	// Another tool's dir with that name.
	os.RemoveAll(b.dir("work"))
	os.MkdirAll(b.dir("work"), 0o700)
	os.WriteFile(filepath.Join(b.dir("work"), "notes"), []byte("x"), 0o600)
	for _, r := range b.s.tick() {
		err = r.Err
	}
	if err == nil {
		t.Fatal("claimed another tool's dir")
	}
	if _, serr := os.Stat(filepath.Join(b.dir("work"), "auth.json")); serr == nil {
		t.Fatal("wrote into another tool's dir")
	}

	// And a placeholder or junk in Coffer is not installed.
	store.putIf("limitbreak/grok/junk", []byte("{}"), 0)
	store.putIf("limitbreak/grok/ph", grokPlaceholderAuth(), 0)
	b.s.tick()
	for _, name := range []string{"junk", "ph"} {
		if _, err := os.Stat(b.dir(name)); err == nil {
			t.Fatal("installed", name)
		}
	}
}

func TestOnlyANamedLoginWithItsRefreshTokenIsPublished(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a := newMac(t, store, "a", &clock)
	k := a.grokKind()
	if _, err := a.s.publish(k, defaultID, false); err == nil {
		t.Fatal("published the default account")
	}
	noRefresh, _ := dropRefreshToken(grokAuthJSON("work", clock.Add(time.Hour)))
	os.MkdirAll(a.dir("work"), 0o700)
	os.WriteFile(filepath.Join(a.dir("work"), "auth.json"), noRefresh, 0o600)
	if _, err := a.s.publish(k, "work", false); !errors.Is(err, errNothingToShare) {
		t.Fatal(err)
	}
	if _, err := a.s.publish(k, "nobody", false); !errors.Is(err, errNothingToShare) {
		t.Fatal(err)
	}
	if len(store.data) != 0 {
		t.Fatal("something was written to Coffer", store.data)
	}
}

func TestPublishingWhatCofferAlreadyHoldsTracksItAndADifferentOneIsRefused(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(5*time.Hour))
	a.mustPublish("work")

	// The same login on B (an admin put it there, say): tracked, nothing written.
	os.MkdirAll(b.dir("work"), 0o700)
	os.WriteFile(filepath.Join(b.dir("work"), "auth.json"), mustReadFile(t, filepath.Join(a.dir("work"), "auth.json")), 0o600)
	puts := store.puts
	if action, err := b.s.publish(b.grokKind(), "work", false); err != nil || !strings.Contains(action, "already in Coffer") || store.puts != puts {
		t.Fatal(action, err)
	}
	// A different one under that name is refused.
	writeGrokLogin(t, b.dir("other"), "other", clock.Add(5*time.Hour))
	store.putIf("limitbreak/grok/other", []byte("{}"), 0)
	if _, err := b.s.publish(b.grokKind(), "other", false); err == nil {
		t.Fatal("overwrote Coffer's login")
	}
}

func TestUnshareStopsTheSyncAndShareStartsItAgain(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(5*time.Hour))
	a.mustPublish("work")
	b.pass()
	if err := b.s.unshare(b.grokKind(), "work"); err != nil {
		t.Fatal(err)
	}
	if syncedAccounts(b.home, "grok")["work"] {
		t.Fatal("still synced")
	}
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(8*time.Hour))
	a.pass()
	before := b.token("work")
	if got := b.pass()[workPath]; got != actionIgnored || b.token("work") != before {
		t.Fatal(got)
	}
	// A refresh on B is its own again: no lease, no Coffer.
	store.down = true
	if err := b.grok(b.dir("work")); err != nil || b.refreshes != 1 {
		t.Fatal(err, b.refreshes)
	}
	store.down = false
	// Sharing again while B's login differs from Coffer's is refused: neither side is overwritten.
	if _, err := b.s.publish(b.grokKind(), "work", false); err == nil || syncedAccounts(b.home, "grok")["work"] {
		t.Fatal("shared a login that differs from Coffer's:", err)
	}
	// With Coffer's own login in place it is tracked again.
	os.WriteFile(filepath.Join(b.dir("work"), "auth.json"), mustReadFile(t, filepath.Join(a.dir("work"), "auth.json")), 0o600)
	if action, err := b.s.publish(b.grokKind(), "work", false); err != nil || action == actionIgnored || !syncedAccounts(b.home, "grok")["work"] {
		t.Fatal(action, err)
	}
}

func TestAnAccountNotSyncedIsRefreshedAsBefore(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	store.down = true // never consulted
	a := newMac(t, store, "a", &clock)
	writeGrokLogin(t, a.dir("local"), "work", clock.Add(10*time.Minute))
	if err := a.grok(a.dir("local")); err != nil || a.refreshes != 1 {
		t.Fatal(err, a.refreshes)
	}
}

func TestTheSyncLoopRefreshesALoginThatEndsWithinItsLookAhead(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	a, b := newMac(t, store, "a", &clock), newMac(t, store, "b", &clock)
	writeGrokLogin(t, a.dir("work"), "work", clock.Add(2*time.Hour)) // over the request threshold, under the look-ahead
	a.mustPublish("work")
	b.pass()
	for _, r := range a.s.renewDue() {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if a.refreshes != 1 {
		t.Fatal(a.refreshes)
	}
	if got := b.pass()[workPath]; got != "installed" || b.token("work") != a.token("work") {
		t.Fatal(got)
	}
	// B's loop then finds nothing due.
	if got := b.s.renewDue(); len(got) != 0 || b.refreshes != 0 {
		t.Fatal(got, b.refreshes)
	}
}

func TestSixteenMacsTakeTheLeaseOnceBetweenThem(t *testing.T) {
	store := newFakeCoffer()
	clock := time.Now()
	var wins, held int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		s := newLoginSyncer(t.TempDir(), store, nil)
		s.me = "mac-" + strconv.Itoa(i)
		s.now = func() time.Time { return clock }
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.takeLease("grok", "work")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				_ = release
			case errors.Is(err, errLeaseHeld):
				held++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || held != 15 {
		t.Fatalf("%d won, %d held back", wins, held)
	}
}

func TestAReleasedLeaseCanBeTakenAtOnce(t *testing.T) {
	store, clock := newFakeCoffer(), time.Now()
	a := newLoginSyncer(t.TempDir(), store, nil)
	b := newLoginSyncer(t.TempDir(), store, nil)
	a.me, b.me = "a", "b"
	a.now, b.now = func() time.Time { return clock }, func() time.Time { return clock }
	release, err := a.takeLease("codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.takeLease("codex", "work"); !errors.Is(err, errLeaseHeld) {
		t.Fatal(err)
	}
	release()
	if _, err := b.takeLease("codex", "work"); err != nil {
		t.Fatal(err)
	}
}

// MARK: Claude and Codex

func TestAClaudeTokenIsInstalledAfterAnthropicAcceptsIt(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	home := t.TempDir()
	claude := newClaudeAccounts(home, credentialsFile, "")
	status := 200
	validate := func(string) (int, error) { return status, nil }
	s := newLoginSyncer(home, store, loginKinds(claude, validate))
	s.now = func() time.Time { return clock }

	token := newLongToken("sk-ant-oat01-spare", clock)
	raw, _ := json.Marshal(token)
	store.putIf("limitbreak/claude/spare", raw, 0)
	store.putIf("limitbreak/claude/expired", mustJSON(newLongToken("sk-ant-oat01-old", clock.Add(-400*24*time.Hour))), 0)

	status = 401
	for _, r := range s.tick() {
		if r.Err == nil {
			t.Fatalf("installed a rejected token: %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude-spare")); err == nil {
		t.Fatal("a rejected token left a dir behind")
	}

	status = 200
	var spare syncResult
	for _, r := range s.tick() {
		if r.Path == "limitbreak/claude/spare" {
			spare = r
		} else if r.Err == nil {
			t.Fatalf("installed an expired token: %+v", r)
		}
	}
	if spare.Err != nil || spare.Action != "installed" {
		t.Fatal(spare)
	}
	acc := account{ID: "spare", Dir: filepath.Join(home, ".claude-spare"), Path: filepath.Join(home, ".claude-spare")}
	if got, ok := claude.readLongToken(acc); !ok || got.Token != "sk-ant-oat01-spare" {
		t.Fatal(got, ok)
	}
	// It is an account of this Mac now.
	var found bool
	for _, a := range claude.list() {
		found = found || a.ID == "spare"
	}
	if !found {
		t.Fatal("the account is not listed")
	}
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

func TestAClaudeTokenIsPublishedFromANamedAccountOnly(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	home := makeHome(t, map[string]string{"default": "a", "founder": "b", "nokey": "c"})
	claude := newClaudeAccounts(home, credentialsFile, "")
	claude.writeLongToken(accountNamed(t, claude, "default"), newLongToken("sk-ant-oat01-def", clock))
	claude.writeLongToken(accountNamed(t, claude, "founder"), newLongToken("sk-ant-oat01-founder", clock))
	s := newLoginSyncer(home, store, loginKinds(claude, nil))
	s.now = func() time.Time { return clock }
	k, _ := s.kind("claude")
	if _, err := s.publish(k, defaultID, false); err == nil {
		t.Fatal("published the default account's token")
	}
	if action, err := s.publish(k, "founder", false); err != nil || action != "published" {
		t.Fatal(action, err)
	}
	if _, err := s.publish(k, "nokey", false); !errors.Is(err, errNothingToShare) {
		t.Fatal(err)
	}
	if got := s.localNames(k); strings.Join(got, ",") != "founder,nokey" {
		t.Fatal(got)
	}
	if val, _, _ := store.get("limitbreak/claude/founder"); !strings.Contains(string(val), "sk-ant-oat01-founder") {
		t.Fatal("not in Coffer")
	}
}

func TestACodexLoginIsSyncedLikeAGrokOne(t *testing.T) {
	clock, store := time.Now(), newFakeCoffer()
	mk := func() *loginSyncer {
		home := t.TempDir()
		s := newLoginSyncer(home, store, loginKinds(newClaudeAccounts(home, credentialsFile, ""), nil))
		s.now = func() time.Time { return clock }
		return s
	}
	a, b := mk(), mk()
	k, _ := a.kind("codex")
	dir := filepath.Join(a.home, ".codex-work")
	os.MkdirAll(dir, 0o700)
	auth := mustJSON(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"access_token": fakeJWT(map[string]any{"exp": clock.Add(48 * time.Hour).Unix()}), "refresh_token": "r", "account_id": "acct"}})
	os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600)
	if action, err := a.publish(k, "work", false); err != nil || action != "published" {
		t.Fatal(action, err)
	}
	res := b.tick()
	if len(res) == 0 {
		t.Fatal("nothing synced")
	}
	var installed bool
	for _, r := range res {
		installed = installed || (r.Path == "limitbreak/codex/work" && r.Action == "installed" && r.Err == nil)
	}
	if !installed || string(mustReadFile(t, filepath.Join(b.home, ".codex-work", "auth.json"))) != string(auth) {
		t.Fatalf("%+v", res)
	}
}

// MARK: The client and its configuration

func TestTheClientSpeaksCoffersWireContract(t *testing.T) {
	store := newFakeCoffer()
	srv := httptest.NewServer(store.handler("tok"))
	defer srv.Close()
	c, err := newCofferClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.get("limitbreak/grok/work"); !errors.Is(err, errCofferNotFound) {
		t.Fatal(err)
	}
	if v, err := c.putIf("limitbreak/grok/work", []byte("one"), 0); err != nil || v != 1 {
		t.Fatal(v, err)
	}
	if _, err := c.putIf("limitbreak/grok/work", []byte("two"), 0); !errors.Is(err, errCofferConflict) {
		t.Fatal(err)
	}
	if v, err := c.putIf("limitbreak/grok/work", []byte("two"), 1); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	if val, v, err := c.get("limitbreak/grok/work"); err != nil || v != 2 || string(val) != "two" {
		t.Fatal(string(val), v, err)
	}
	if paths, err := c.list("limitbreak/grok/*"); err != nil || len(paths) != 1 || paths[0] != "limitbreak/grok/work" {
		t.Fatal(paths, err)
	}
	if _, err := c.putIf("x", nil, -1); err == nil {
		t.Fatal("a negative expected version was sent")
	}

	// A wrong token is an error that names the status, never the token.
	bad, _ := newCofferClient(srv.URL, "secret-token-value")
	_, _, err = bad.get("limitbreak/grok/work")
	var ce *cofferError
	if !errors.As(err, &ce) || ce.Status != http.StatusUnauthorized || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatal(err)
	}
	// An address that is down is said without echoing more than where.
	srv.Close()
	if _, _, err := c.get("limitbreak/grok/work"); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatal(err)
	}
}

func TestCofferIsConfiguredByEnvironmentOrFileAndOffWithoutBoth(t *testing.T) {
	home := t.TempDir()
	t.Setenv(cofferAddrEnv, "")
	t.Setenv(cofferTokenFileEnv, filepath.Join(home, "no-such-token"))
	if _, err := openCoffer(home); err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(cofferConfigFile(home)), 0o700)
	os.WriteFile(cofferConfigFile(home), []byte("http://100.64.0.1:8795\n"), 0o600)
	if _, err := openCoffer(home); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(home, "token")
	os.WriteFile(tokenFile, []byte("t0k\n"), 0o600)
	t.Setenv(cofferTokenFileEnv, tokenFile)
	c, err := openCoffer(home)
	if err != nil || c.base != "http://100.64.0.1:8795" || c.token != "t0k" {
		t.Fatal(c, err)
	}
	// With no environment, the token file beside the address file is used (a Mac whose Keychain is locked).
	t.Setenv(cofferTokenFileEnv, "")
	os.WriteFile(cofferTokenFile(home), []byte("f1le\n"), 0o600)
	if c, err := openCoffer(home); err != nil || c.token != "f1le" {
		t.Fatal(c, err)
	}
	t.Setenv(cofferTokenFileEnv, tokenFile)
	t.Setenv(cofferAddrEnv, "http://127.0.0.1:9")
	if c, _ := openCoffer(home); c.base != "http://127.0.0.1:9" {
		t.Fatal("the environment does not win")
	}
	if _, err := newCofferClient("coffer.example", "t"); err == nil {
		t.Fatal("an address without a scheme was accepted")
	}

	// A Mac with none of it has no syncer, and the commands say why.
	t.Setenv(cofferAddrEnv, "")
	os.Remove(cofferConfigFile(home))
	if _, err := syncerFor(home); err == nil || !strings.Contains(err.Error(), "Coffer is not set up") {
		t.Fatal(err)
	}
}

// setCofferEnv points this process's Coffer configuration at a fake Coffer for the CLI commands.
func setCofferEnv(t *testing.T, store *fakeCoffer) {
	t.Helper()
	srv := httptest.NewServer(store.handler("tok"))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("tok"), 0o600)
	t.Setenv(cofferAddrEnv, srv.URL)
	t.Setenv(cofferTokenFileEnv, tokenFile)
}

func TestShareAndSyncCommandsRunAgainstTheConfiguredCoffer(t *testing.T) {
	store := newFakeCoffer()
	setCofferEnv(t, store)

	owner, other := t.TempDir(), t.TempDir()
	writeGrokLogin(t, filepath.Join(owner, ".grok-work"), "work", time.Now().Add(5*time.Hour))
	var out strings.Builder
	if err := shareCommand("share", owner, []string{"--provider", "grok"}, &out); err != nil || !strings.Contains(out.String(), "grok/work: published") {
		t.Fatal(err, out.String())
	}
	out.Reset()
	if err := syncNowCommand(other, &out); err != nil || !strings.Contains(out.String(), "limitbreak/grok/work: installed") {
		t.Fatal(err, out.String())
	}
	if _, ok := readGrokLogin(filepath.Join(other, ".grok-work")); !ok {
		t.Fatal("not installed")
	}
	out.Reset()
	if err := shareCommand("unshare", other, []string{"work", "--provider", "grok"}, &out); err != nil || syncedAccounts(other, "grok")["work"] {
		t.Fatal(err, out.String())
	}
	if err := shareCommand("share", owner, []string{"--provider", "opencode"}, &out); err == nil {
		t.Fatal("shared an unsupported provider")
	}
}

func TestTrackedClaudeLoginMigratesToFileAndSurvivesRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", home) // Neither security nor Claude Code is available.
	store := newFakeCoffer()
	now := time.Now()
	token := newLongToken("sk-ant-oat01-managed", now)
	raw := mustJSON(token)
	path := loginPath("claude", "work")
	version, err := store.putIf(path, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	claude := newClaudeAccounts(home, credentialsKeychain, "")
	syncer := newLoginSyncer(home, store, loginKinds(claude, nil))
	kind, _ := syncer.kind("claude")
	acc := syncer.account(kind, "work")
	if err := os.Mkdir(acc.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acc.Path, "existing-config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncer.record(path, syncEntry{Version: version, Fingerprint: fingerprint(raw)}); err != nil {
		t.Fatal(err)
	}
	if action, err := syncer.syncLocked(kind, "work"); err != nil || action != "installed" {
		t.Fatalf("migration = %q, %v", action, err)
	}
	info, err := os.Stat(longTokenPath(acc))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("cache mode = %o", mode)
	}
	restarted := newClaudeAccounts(home, credentialsKeychain, "")
	if got := restarted.token(accountNamed(t, restarted, "work")); got != token.Token {
		t.Fatal("restarted proxy could not load the cached token")
	}
}
