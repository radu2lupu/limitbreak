package main

// Logins from Coffer (design: ROADMAP/logins-from-coffer.md).
//
// Coffer is the one shared store of provider logins. Each Mac keeps a local copy in the login dir the
// provider already uses (~/.grok-<name>, ~/.codex-<name>, ~/.claude-<name>) and keeps working while
// Coffer is unreachable. This file is the one owner of how the two are kept equal:
//
//   - a sync pass installs what Coffer holds and this Mac lacks, installs what Coffer has newer,
//     and writes a local change back with a compare-and-swap on Coffer's version;
//   - a rotating login (Grok, Codex) is refreshed by one Mac at a time, under a lease kept in Coffer,
//     and the refreshed login is written back before the lease is let go.
//
// The default account (~/.grok, ~/.codex, ~/.claude) is never synced: it belongs to the provider's own
// tool, and a copy of a rotating login in a second home would have two homes spending one refresh token.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// A login is Coffer path limitbreak/<provider>/<account>; its refresh lease is
	// limitbreak/lease/<provider>/<account>.
	cofferLoginRoot = "limitbreak"
	cofferLeaseRoot = "limitbreak/lease"
	// loginSyncEvery is how often serve runs a sync pass.
	loginSyncEvery = 5 * time.Minute
	// loginLeaseTTL is how long a refresh lease holds when its holder never lets it go. It is far above
	// clock skew between Macs and a refresh (a minute at most).
	loginLeaseTTL = 3 * time.Minute
)

var errLeaseHeld = errors.New("another Mac is refreshing it")

func loginPath(provider, name string) string { return cofferLoginRoot + "/" + provider + "/" + name }

func leasePath(provider, name string) string { return cofferLeaseRoot + "/" + provider + "/" + name }

// syncableName is whether an account can be synced: named, never the default one.
func syncableName(name string) bool { return name != defaultID && accountName.MatchString(name) }

// stateFile is a file in Limit Break's state dir.
func stateFile(home, name string) string {
	return filepath.Join(home, ".local", "state", "limitbreak", name)
}

// fingerprint names a login without revealing it: a short hash of its bytes.
func fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// MARK: State

// syncEntry is what was true when a login was last synced: Coffer's version of it and its fingerprint.
// An ignored entry is an account this Mac opted out of (limitbreak unshare).
type syncEntry struct {
	Version     int64  `json:"version,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Ignored     bool   `json:"ignored,omitempty"`
}

// syncState is the entries by Coffer path. A path with a live entry is a synced account.
type syncState map[string]syncEntry

func readSyncState(home string) syncState {
	st := syncState{}
	if raw, err := os.ReadFile(stateFile(home, "coffer-sync.json")); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func writeSyncState(home string, st syncState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := stateFile(home, "coffer-sync.json")
	return writeFileAtomic(filepath.Dir(path), filepath.Base(path), raw)
}

// writeFileAtomic writes dir/name at 0600 through a temp file renamed over it, so a reader sees the
// old file or the new one whole and a secret is never readable by others.
func writeFileAtomic(dir, name string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".limitbreak-*")
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
	return os.Rename(f.Name(), filepath.Join(dir, name))
}

// syncedAccounts is the provider's accounts kept in step with Coffer on this Mac, by account name.
func syncedAccounts(home, provider string) map[string]bool {
	out := map[string]bool{}
	prefix := cofferLoginRoot + "/" + provider + "/"
	for path, e := range readSyncState(home) {
		if name, ok := strings.CutPrefix(path, prefix); ok && !e.Ignored && !strings.Contains(name, "/") {
			out[name] = true
		}
	}
	return out
}

// MARK: Kinds

// syncKind is what differs between providers: what a login is on disk, whether Coffer's copy is one
// this Mac installs, and when it is refreshed.
type syncKind struct {
	provider string
	// prefix is the login dirs' name prefix in the home dir (".grok-").
	prefix string
	// within is how much token life triggers a refresh when a request needs the login; ahead is how much
	// triggers one in the sync loop. Zero for a login nobody refreshes (a Claude 1-year token).
	within, ahead time.Duration
	// read is the login in the account's dir as a Coffer value; false when it holds none.
	read func(acc account) ([]byte, bool)
	// accept says whether a value read from Coffer is a login to install.
	accept func(raw []byte, now time.Time) error
	// prepare readies a dir that holds no login of this kind for the first install; it refuses a dir
	// that is something else's.
	prepare func(acc account) error
	// write stores a value read from Coffer as the account's login.
	write func(acc account, raw []byte) error
	// ends is when the login's access token ends.
	ends func(raw []byte) time.Time
	// shareable says whether this Mac's login may be published to Coffer.
	shareable func(raw []byte, now time.Time) error
}

// loginKinds are the providers that sync. claude reads and writes 1-year tokens; validate (nil: none)
// is asked before one is installed, and a token Anthropic rejects is not.
func loginKinds(claude *claudeAccounts, validate tokenValidator) []syncKind {
	authJSON := func(acc account) string { return filepath.Join(acc.Path, "auth.json") }
	emptyDir := func(acc account) error {
		if entries, err := os.ReadDir(acc.Path); err == nil && len(entries) > 0 {
			return fmt.Errorf("%s exists and is not a login; not touching it", acc.Path)
		}
		return nil
	}
	grokReal := func(raw []byte) (grokLogin, error) {
		l, ok := parseGrokLogin(raw)
		switch {
		case !ok:
			return l, errors.New("not a Grok login")
		case l.placeholder:
			return l, errors.New("the placeholder login")
		}
		return l, nil
	}
	codexReal := func(raw []byte) (codexLogin, error) {
		l, ok := parseCodexLogin(raw)
		if !ok {
			return l, errors.New("not a ChatGPT Codex login")
		}
		return l, nil
	}
	return []syncKind{
		{
			provider: "claude", prefix: ".claude-",
			read: func(acc account) ([]byte, bool) {
				t, ok := claude.readLongToken(acc)
				if !ok {
					return nil, false
				}
				raw, err := json.Marshal(t)
				return raw, err == nil
			},
			accept: func(raw []byte, now time.Time) error {
				var t longToken
				if json.Unmarshal(raw, &t) != nil || !longTokenPattern.MatchString(t.Token) {
					return errors.New("not a 1-year token record")
				}
				if !t.valid(now) {
					return errors.New("the token has expired")
				}
				if validate != nil {
					if status, err := validate(t.Token); err == nil && (status == 401 || status == 403) {
						return fmt.Errorf("Anthropic rejected the token (HTTP %d)", status)
					}
				}
				return nil
			},
			prepare: func(acc account) error {
				if len(claude.credentialSources(acc.Dir, acc.Path)) > 0 {
					return nil // a sign-in of its own: the token is added beside it
				}
				_, err := claude.tokenOnlyAccount(acc.ID)
				return err
			},
			write: func(acc account, raw []byte) error {
				var t longToken
				if err := json.Unmarshal(raw, &t); err != nil {
					return err
				}
				return claude.writeLongToken(acc, t)
			},
			ends: func(raw []byte) time.Time {
				var t longToken
				_ = json.Unmarshal(raw, &t)
				return time.UnixMilli(t.ExpiresAt)
			},
			shareable: func(raw []byte, now time.Time) error {
				var t longToken
				if json.Unmarshal(raw, &t) != nil || !t.valid(now) {
					return errors.New("no unexpired 1-year token (run limitbreak renew)")
				}
				return nil
			},
		},
		{
			provider: "codex", prefix: ".codex-", within: codexRefreshWithin, ahead: codexRefreshWithin,
			read: func(acc account) ([]byte, bool) {
				raw, err := os.ReadFile(authJSON(acc))
				if _, perr := codexReal(raw); err != nil || perr != nil {
					return nil, false
				}
				return raw, true
			},
			accept:  func(raw []byte, _ time.Time) error { _, err := codexReal(raw); return err },
			prepare: emptyDir,
			write:   func(acc account, raw []byte) error { return writeFileAtomic(acc.Path, "auth.json", raw) },
			ends: func(raw []byte) time.Time {
				l, _ := parseCodexLogin(raw)
				return l.expires
			},
			shareable: func(raw []byte, _ time.Time) error { _, err := codexReal(raw); return err },
		},
		{
			provider: "grok", prefix: ".grok-", within: grokRefreshWithin, ahead: grokRefreshAhead,
			read: func(acc account) ([]byte, bool) {
				raw, err := os.ReadFile(authJSON(acc))
				if _, perr := grokReal(raw); err != nil || perr != nil {
					return nil, false
				}
				return raw, true
			},
			accept:  func(raw []byte, _ time.Time) error { _, err := grokReal(raw); return err },
			prepare: emptyDir,
			write:   func(acc account, raw []byte) error { return writeFileAtomic(acc.Path, "auth.json", raw) },
			ends: func(raw []byte) time.Time {
				l, _ := parseGrokLogin(raw)
				return l.expires
			},
			shareable: func(raw []byte, _ time.Time) error {
				l, err := grokReal(raw)
				if err == nil && !l.refreshable {
					err = errors.New("no refresh token: sign it in here (limitbreak add --provider grok <name>)")
				}
				return err
			},
		},
	}
}

// MARK: Syncer

type loginSyncer struct {
	home  string
	store cofferStore
	// me names this process as a lease holder.
	me    string
	now   func() time.Time
	kinds []syncKind
	// refresh is each provider's own refresh (grok models, codex app-server), as it was before guard
	// wrapped it.
	refresh map[string]func(dir string) error

	// mu serializes passes and refreshes in this process; Coffer's compare-and-swap does it between Macs.
	mu sync.Mutex
	// seen is the last error logged per path, so a Coffer that stays down is said once.
	seen map[string]string
}

func newLoginSyncer(home string, store cofferStore, kinds []syncKind) *loginSyncer {
	host, _ := os.Hostname()
	return &loginSyncer{home: home, store: store, me: fmt.Sprintf("%s/%d", host, os.Getpid()), now: time.Now, kinds: kinds,
		refresh: map[string]func(string) error{}, seen: map[string]string{}}
}

// openLoginSyncer is the syncer for this Mac's Coffer; an error says why there is none (no address or
// token), which is how a Mac without Coffer behaves.
func openLoginSyncer(home string, claude *claudeAccounts) (*loginSyncer, error) {
	c, err := openCoffer(home)
	if err != nil {
		return nil, err
	}
	validate := countTokensValidator(envString("LIMITBREAK_UPSTREAM", defaultUpstream))
	return newLoginSyncer(home, c, loginKinds(claude, validate)), nil
}

func (s *loginSyncer) kind(provider string) (syncKind, bool) {
	for _, k := range s.kinds {
		if k.provider == provider {
			return k, true
		}
	}
	return syncKind{}, false
}

func (s *loginSyncer) account(k syncKind, name string) account {
	dir := filepath.Join(s.home, k.prefix+name)
	return account{ID: name, Dir: dir, Path: dir}
}

func (s *loginSyncer) record(path string, e syncEntry) error {
	st := readSyncState(s.home)
	st[path] = e
	return writeSyncState(s.home, st)
}

// syncResult is what one account's pass did; Action is empty on an error.
type syncResult struct {
	Path   string
	Action string
	Err    error
}

// Actions that say nothing changed.
const (
	actionUnchanged = "unchanged"
	actionAbsent    = "not in Coffer"
	actionIgnored   = "ignored on this Mac"
)

// syncLocked is one account's pass; the caller holds s.mu. A rotating login's refresh runs only under
// refreshGuarded, never here.
func (s *loginSyncer) syncLocked(k syncKind, name string) (string, error) {
	path := loginPath(k.provider, name)
	acc := s.account(k, name)
	entry, tracked := readSyncState(s.home)[path]
	if entry.Ignored {
		return actionIgnored, nil
	}
	remote, version, err := s.store.get(path)
	if errors.Is(err, errCofferNotFound) {
		return actionAbsent, nil
	}
	if err != nil {
		return "", err
	}
	now := s.now()
	local, haveLocal := k.read(acc)
	if !haveLocal {
		if err := k.accept(remote, now); err != nil {
			return "", fmt.Errorf("not installing Coffer's login: %w", err)
		}
		if err := k.prepare(acc); err != nil {
			return "", err
		}
		return s.install(k, acc, path, remote, version)
	}
	fpLocal, fpRemote := fingerprint(local), fingerprint(remote)
	if fpLocal == fpRemote {
		// The same login on both sides: track it (also how a crash between install and state recovers).
		if tracked && entry.Version == version && entry.Fingerprint == fpLocal {
			return actionUnchanged, nil
		}
		return "synced", s.record(path, syncEntry{Version: version, Fingerprint: fpLocal})
	}
	if !tracked {
		return "", fmt.Errorf("%s holds a login of its own that differs from Coffer's; not overwriting it (limitbreak share %s --provider %s publishes it, if Coffer has none)",
			acc.Path, name, k.provider)
	}
	localChanged, remoteMoved := fpLocal != entry.Fingerprint, version != entry.Version
	switch {
	case localChanged && !remoteMoved:
		return s.writeBack(path, local, fpLocal, entry.Version)
	case localChanged && k.ends(local).After(k.ends(remote)):
		// Both moved: the login whose access token ends later is the newer one.
		return s.writeBack(path, local, fpLocal, version)
	}
	if err := k.accept(remote, now); err != nil {
		return "", fmt.Errorf("not installing Coffer's login: %w", err)
	}
	return s.install(k, acc, path, remote, version)
}

func (s *loginSyncer) install(k syncKind, acc account, path string, remote []byte, version int64) (string, error) {
	if err := k.write(acc, remote); err != nil {
		return "", err
	}
	return "installed", s.record(path, syncEntry{Version: version, Fingerprint: fingerprint(remote)})
}

// writeBack puts the local login in Coffer, only if Coffer is still at expect. Losing the race writes
// nothing; the next pass reads Coffer again.
func (s *loginSyncer) writeBack(path string, local []byte, fp string, expect int64) (string, error) {
	version, err := s.store.putIf(path, local, expect)
	if errors.Is(err, errCofferConflict) {
		return "Coffer changed meanwhile; reading again next pass", nil
	}
	if err != nil {
		return "", err
	}
	return "published", s.record(path, syncEntry{Version: version, Fingerprint: fp})
}

// publish is `limitbreak share`: this Mac's named login goes into Coffer (only when Coffer has none of
// that name yet) and is then kept in step. A name Coffer already tracks here is just synced.
func (s *loginSyncer) publish(k syncKind, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !syncableName(name) {
		return "", fmt.Errorf("%q cannot be shared: only named accounts are (sign it in under a name: limitbreak add --provider %s <name>)", name, k.provider)
	}
	path := loginPath(k.provider, name)
	if e, ok := readSyncState(s.home)[path]; ok {
		if !e.Ignored {
			return s.syncLocked(k, name)
		}
		st := readSyncState(s.home)
		delete(st, path)
		if err := writeSyncState(s.home, st); err != nil {
			return "", err
		}
	}
	acc := s.account(k, name)
	local, ok := k.read(acc)
	if !ok {
		action, err := s.syncLocked(k, name) // none here: install Coffer's, when it has one
		if err == nil && action == actionAbsent {
			return "", fmt.Errorf("%w: no %s login named %s here or in Coffer", errNothingToShare, k.provider, name)
		}
		return action, err
	}
	if err := k.shareable(local, s.now()); err != nil {
		return "", fmt.Errorf("%w: %s: %v", errNothingToShare, name, err)
	}
	remote, version, err := s.store.get(path)
	switch {
	case errors.Is(err, errCofferNotFound):
		v, err := s.store.putIf(path, local, 0)
		if errors.Is(err, errCofferConflict) {
			return "", errors.New("Coffer changed while publishing: run it again, and if it keeps failing the login was deleted from Coffer once (a deleted path cannot be written conditionally): an admin puts it again with `coffer put`")
		}
		if err != nil {
			return "", err
		}
		return "published", s.record(path, syncEntry{Version: v, Fingerprint: fingerprint(local)})
	case err != nil:
		return "", err
	case fingerprint(remote) == fingerprint(local):
		return "already in Coffer; now kept in step", s.record(path, syncEntry{Version: version, Fingerprint: fingerprint(local)})
	}
	return "", fmt.Errorf("Coffer already holds a different %s login named %s; remove one of them first", k.provider, name)
}

// errNothingToShare marks an account with no shareable login, which sharing all accounts skips.
var errNothingToShare = errors.New("nothing to share")

// unshare is `limitbreak unshare`: this Mac stops syncing the account. Coffer's copy and the local
// one stay.
func (s *loginSyncer) unshare(k syncKind, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record(loginPath(k.provider, name), syncEntry{Ignored: true})
}

// localNames are the named accounts of a provider with a dir on this Mac.
func (s *loginSyncer) localNames(k syncKind) []string {
	var names []string
	entries, _ := os.ReadDir(s.home)
	for _, e := range entries {
		if name, ok := strings.CutPrefix(e.Name(), k.prefix); ok && e.IsDir() && syncableName(name) {
			names = append(names, name)
		}
	}
	return names
}

// tick is one sync pass over every provider: the accounts Coffer lists and the ones already synced.
func (s *loginSyncer) tick() []syncResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []syncResult
	for _, k := range s.kinds {
		pattern := cofferLoginRoot + "/" + k.provider + "/*"
		paths, err := s.store.list(pattern)
		if err != nil {
			out = append(out, syncResult{Path: pattern, Err: err})
			continue
		}
		names := map[string]bool{}
		for _, path := range paths {
			if name, ok := strings.CutPrefix(path, cofferLoginRoot+"/"+k.provider+"/"); ok && syncableName(name) {
				names[name] = true
			}
		}
		for name := range syncedAccounts(s.home, k.provider) {
			names[name] = true
		}
		sorted := make([]string, 0, len(names))
		for name := range names {
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)
		for _, name := range sorted {
			action, err := s.syncLocked(k, name)
			out = append(out, syncResult{Path: loginPath(k.provider, name), Action: action, Err: err})
		}
	}
	return out
}

// MARK: Refresh ownership

// lease says which Mac is refreshing a login and until when.
type lease struct {
	Holder  string    `json:"holder"`
	Expires time.Time `json:"expires"`
}

// takeLease takes the refresh lease for a login by writing it with a compare-and-swap on the version
// read; it fails when another holder's lease has not expired or another Mac wrote first. The release
// func lets it go.
func (s *loginSyncer) takeLease(provider, name string) (func(), error) {
	path := leasePath(provider, name)
	raw, version, err := s.store.get(path)
	switch {
	case errors.Is(err, errCofferNotFound):
		version = 0
	case err != nil:
		return nil, err
	default:
		var held lease
		if json.Unmarshal(raw, &held) == nil && held.Holder != "" && held.Holder != s.me && held.Expires.After(s.now()) {
			return nil, errLeaseHeld
		}
	}
	mine, _ := json.Marshal(lease{Holder: s.me, Expires: s.now().Add(loginLeaseTTL).UTC()})
	taken, err := s.store.putIf(path, mine, version)
	if errors.Is(err, errCofferConflict) {
		return nil, errLeaseHeld
	}
	if err != nil {
		return nil, err
	}
	return func() {
		free, _ := json.Marshal(lease{Expires: s.now().UTC()})
		_, _ = s.store.putIf(path, free, taken) // best effort: it expires on its own
	}, nil
}

// refreshGuarded refreshes a synced rotating login on this Mac: under the lease, after pulling what
// another Mac may already have refreshed, and with the result written back before the lease is let go.
// within is how little token life left makes it due. No lease (Coffer down, or another Mac holds it)
// means no refresh.
func (s *loginSyncer) refreshGuarded(k syncKind, name string, within time.Duration, real func(dir string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.takeLease(k.provider, name)
	if err != nil {
		return fmt.Errorf("not refreshing %s/%s: %w", k.provider, name, err)
	}
	defer release()
	acc := s.account(k, name)
	if _, err := s.syncLocked(k, name); err != nil {
		return err
	}
	if raw, ok := k.read(acc); ok && k.ends(raw).After(s.now().Add(within)) {
		return nil // another Mac refreshed it
	}
	if err := real(acc.Path); err != nil {
		return err
	}
	if _, err := s.syncLocked(k, name); err != nil {
		// The refreshed login is on disk; a later pass writes it back.
		log.Printf("login sync %s/%s: refreshed, not yet written back: %v", k.provider, name, err)
	}
	return nil
}

// guard wraps a provider's own refresh so a synced account's refresh goes through refreshGuarded; any
// other account is refreshed as before.
func (s *loginSyncer) guard(provider string, real func(dir string) error) func(dir string) error {
	k, ok := s.kind(provider)
	if !ok {
		return real
	}
	s.refresh[provider] = real
	return func(dir string) error {
		name := strings.TrimPrefix(filepath.Base(dir), k.prefix)
		if filepath.Join(s.home, k.prefix+name) != dir || !syncableName(name) || !syncedAccounts(s.home, provider)[name] {
			return real(dir)
		}
		return s.refreshGuarded(k, name, k.within, real)
	}
}

// renewDue refreshes the synced rotating logins that end within their provider's look-ahead, so
// Coffer holds a fresh login even when no request has needed one.
func (s *loginSyncer) renewDue() []syncResult {
	var out []syncResult
	for _, k := range s.kinds {
		real := s.refresh[k.provider]
		if real == nil || k.ahead == 0 {
			continue
		}
		names := make([]string, 0)
		for name := range syncedAccounts(s.home, k.provider) {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			raw, ok := k.read(s.account(k, name))
			if !ok || k.ends(raw).After(s.now().Add(k.ahead)) {
				continue
			}
			err := s.refreshGuarded(k, name, k.ahead, real)
			if errors.Is(err, errLeaseHeld) {
				continue // another Mac has it; the next pass pulls its result
			}
			out = append(out, syncResult{Path: loginPath(k.provider, name), Action: "refreshed", Err: err})
		}
	}
	return out
}

// run is the serve loop: a sync pass, then the due refreshes, every loginSyncEvery.
func (s *loginSyncer) run() {
	for {
		s.report(append(s.tick(), s.renewDue()...))
		time.Sleep(loginSyncEvery)
	}
}

// report logs what a pass changed and each error once until it changes.
func (s *loginSyncer) report(results []syncResult) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			if text := r.Err.Error(); s.seen[r.Path] != text {
				s.seen[r.Path] = text
				log.Printf("login sync %s: %v", r.Path, r.Err)
			}
		default:
			delete(s.seen, r.Path)
			if r.Action != actionUnchanged && r.Action != actionAbsent && r.Action != actionIgnored {
				log.Printf("login sync %s: %s", r.Path, r.Action)
			}
		}
	}
}

// MARK: Commands

// syncCommand is `limitbreak sync`: one pass, with what it did.
func syncCommand(s *loginSyncer, out interface{ Write([]byte) (int, error) }) error {
	failed := 0
	for _, r := range s.tick() {
		switch {
		case r.Err != nil:
			failed++
			fmt.Fprintf(out, "%s: %v\n", r.Path, r.Err)
		case r.Action != actionAbsent && r.Action != actionIgnored:
			fmt.Fprintf(out, "%s: %s\n", r.Path, r.Action)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d account(s) did not sync", failed)
	}
	return nil
}
