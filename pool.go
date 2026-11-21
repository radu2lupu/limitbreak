package main

// A provider is one kind of account (Claude, Codex, Grok): where its logins live, how a request is
// put on one, how a reply says the account is spent, and what its usage is. A pool is one
// provider's accounts as the proxy rotates them: which one each client is on, and which are spent
// until when. What differs between providers is behind the interface; the rotation is the pool's.

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type provider interface {
	// name is the provider's id in the API ("claude", "codex", "grok").
	name() string
	// list is every account with a login, read now, the default one first.
	list() []account
	// serves is a path this provider's upstream answers.
	serves(path string) bool
	// spends is a request sent on an account; the provider's other paths pass through untouched.
	spends(path string) bool
	// authorize puts the account's credentials on the outgoing request (which carries the
	// client's own); false when the account has none usable now.
	authorize(out *http.Request, acc account) bool
	// spent is whether a reply says the account's window is spent, and when it lifts (zero when
	// unsaid). body is the start of a 4xx reply's body, nil for any other reply.
	spent(status int, h http.Header, body []byte) (bool, time.Time)
	// observe records what a reply's headers say about the account's usage. A non-zero time
	// means they show a window already used up, until then.
	observe(acc account, h http.Header) time.Time
	// unauthorized is what to do after a 401: send again on the same account (its login was just
	// renewed), or hand the 401 back to the client (it was the client's own login). Otherwise
	// the account is skipped for a while.
	unauthorized(acc account) (retry, handBack bool)
	// usage is the account's raw usage values by name (nil when the provider keeps none) and its
	// windows, normalised.
	usage(acc account) (map[string]usageValue, []usageWindow)
}

// A rewriting provider changes a spending request's body before it goes out, on any login.
type rewriting interface {
	rewrite(body []byte) []byte
}

// A mounted provider's upstream paths collide with another provider's (Grok's /v1/… with Claude's), so
// clients reach it under a path prefix of its own, which comes off before the request goes upstream.
type mounted interface {
	mount() string
}

// upstreamPath is path as the pool's upstream knows it: without the provider's mount.
func (p *pool) upstreamPath(path string) string {
	if m, ok := p.provider.(mounted); ok {
		return strings.TrimPrefix(path, m.mount())
	}
	return path
}

type pool struct {
	provider provider
	upstream string
	now      func() time.Time

	mu           sync.Mutex
	discovered   []account
	discoveredAt time.Time
	active       map[string]string
	spentUntil   map[string]time.Time
	// pins are the account each running session is on (see current).
	pins map[string]sessionPin
}

// sessionPin is the account a session last went out on, and when.
type sessionPin struct {
	account string
	used    time.Time
}

func newPool(p provider, upstream string) *pool {
	return &pool{provider: p, upstream: strings.TrimRight(upstream, "/"), now: time.Now,
		active: map[string]string{}, spentUntil: map[string]time.Time{}, pins: map[string]sessionPin{}}
}

// list is the provider's accounts, read again at most every discoveryInterval.
func (p *pool) list() []account {
	p.mu.Lock()
	if len(p.discovered) > 0 && p.now().Sub(p.discoveredAt) < discoveryInterval {
		out := append([]account(nil), p.discovered...)
		p.mu.Unlock()
		return out
	}
	p.mu.Unlock()
	found := p.provider.list()
	p.mu.Lock()
	p.discovered, p.discoveredAt = found, p.now()
	p.mu.Unlock()
	return append([]account(nil), found...)
}

// rotation is the list without second sign-ins to a subscription already on it (same Who): they share
// its usage window, so moving to one gains nothing, and refreshing one can rotate a token another
// tool (or Codex itself) holds. The first one, default first then by name, stays.
func rotation(list []account) []account {
	seen := map[string]bool{}
	out := make([]account, 0, len(list))
	for _, acc := range list {
		if acc.Who != "" {
			if seen[acc.Who] {
				continue
			}
			seen[acc.Who] = true
		}
		out = append(out, acc)
	}
	return out
}

// sameAs names, for each second sign-in, the account it duplicates.
func sameAs(list []account) map[string]string {
	first := map[string]string{}
	dup := map[string]string{}
	for _, acc := range list {
		if acc.Who == "" {
			continue
		}
		if id, ok := first[acc.Who]; ok {
			dup[acc.ID] = id
		} else {
			first[acc.Who] = acc.ID
		}
	}
	return dup
}

// current is the account a request goes out on; nil when none is signed in or all are spent. A
// running session (its id non-empty, seen within pinIdle) stays on the account it was on until that
// one is spent, because the prompt cache is per account and a move re-writes its whole context. A new
// or idle session takes the client's active account, moving on from a spent one.
func (p *pool) current(client, session string) *account {
	list := rotation(p.list())
	if len(list) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	usable := func(id string) *account {
		for i := range list {
			if list[i].ID == id && !p.spentUntil[id].After(now) {
				return &list[i]
			}
		}
		return nil
	}
	pin, pinned := p.pins[session]
	acc := (*account)(nil)
	if session != "" && pinned && now.Sub(pin.used) < pinIdle {
		acc = usable(pin.account)
	}
	if acc == nil {
		want := p.active[client]
		if acc = usable(want); acc == nil {
			if acc = nextAccount(list, want, p.spentUntil, now); acc != nil {
				p.active[client] = acc.ID
			}
		}
	}
	if acc != nil && session != "" {
		p.pin(session, acc.ID, now)
	}
	return acc
}

// pin records the session on the account now, dropping pins idle past pinIdle once there are many.
func (p *pool) pin(session, id string, now time.Time) {
	if len(p.pins) >= maxPins {
		for s, old := range p.pins {
			if now.Sub(old.used) >= pinIdle {
				delete(p.pins, s)
			}
		}
	}
	p.pins[session] = sessionPin{account: id, used: now}
}

// spend records the account spent until `until` (an hour when zero) and returns the next usable one.
func (p *pool) spend(client, id string, until time.Time) *account {
	list := rotation(p.list())
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if until.After(now) {
		p.spentUntil[id] = until
	} else {
		p.spentUntil[id] = now.Add(fallbackExhaustion)
	}
	usable := nextAccount(list, id, p.spentUntil, now)
	next := "none (the client's own login answers)"
	if usable != nil {
		p.active[client] = usable.ID
		next = usable.ID
	}
	log.Printf("%s account %s spent until %s for client %s; now %s", p.provider.name(), id, p.spentUntil[id].Format(time.RFC3339), client, next)
	return usable
}

// setActive makes id the client's active account; false when no account has that id or it is a
// second sign-in to a subscription already listed (see rotation).
func (p *pool) setActive(client, id string) bool {
	for _, acc := range rotation(p.list()) {
		if acc.ID == id {
			p.mu.Lock()
			p.active[client] = id
			p.mu.Unlock()
			log.Printf("client %s set to %s account %s", client, p.provider.name(), id)
			return true
		}
	}
	return false
}

type accountStatus struct {
	ID string `json:"id"`
	// Provider is the kind of account: "claude", "codex" or "grok". Absent means "claude".
	Provider string `json:"provider"`
	Email    string `json:"email,omitempty"`
	// Label is the name to show for the account: its email when known, else its id.
	Label string `json:"label"`
	// Plan is the subscription ("max", "plus"); empty when unknown.
	Plan       string   `json:"plan"`
	SpentUntil *int64   `json:"spentUntil"`
	ActiveFor  []string `json:"activeFor"`
	// AuthEnds is when the account's auth ends (unix seconds); null when unknown.
	AuthEnds *int64 `json:"authEnds"`
	// Usage is the latest usage headers seen for the account (see usage_record.go), by name.
	Usage map[string]usageValue `json:"usage,omitempty"`
	// Windows are the account's usage windows, the same shape for every provider.
	Windows []usageWindow `json:"windows"`
	// SameAs is the account this one is a second sign-in to (same subscription); it is never rotated to.
	SameAs string `json:"sameAs,omitempty"`
	// Shared is a login copied from another Mac: usable while TokenEnds is in the future.
	Shared bool `json:"shared,omitempty"`
	// TokenEnds is when the account's access token ends (unix seconds; Grok); null when unknown.
	TokenEnds *int64 `json:"tokenEnds"`
}

func (p *pool) status() []accountStatus {
	list := p.list()
	dups := sameAs(list)
	p.mu.Lock()
	now := p.now()
	out := []accountStatus{}
	for _, acc := range list {
		s := accountStatus{ID: acc.ID, Provider: p.provider.name(), Email: acc.Email, Label: accountLabel(acc), Plan: acc.Plan,
			ActiveFor: []string{}, Windows: []usageWindow{}}
		s.SameAs = dups[acc.ID]
		if until := p.spentUntil[acc.ID]; until.After(now) {
			unix := until.Unix()
			s.SpentUntil = &unix
		}
		if !acc.AuthEnds.IsZero() {
			unix := acc.AuthEnds.Unix()
			s.AuthEnds = &unix
		}
		for client, id := range p.active {
			if id == acc.ID {
				s.ActiveFor = append(s.ActiveFor, client)
			}
		}
		sort.Strings(s.ActiveFor)
		s.Shared = acc.Shared
		if !acc.TokenEnds.IsZero() {
			unix := acc.TokenEnds.Unix()
			s.TokenEnds = &unix
		}
		out = append(out, s)
	}
	p.mu.Unlock()
	// Usage may be read from disk (or, for some providers, asked for), so not under the lock.
	for i, acc := range list {
		if dups[acc.ID] != "" {
			continue // its usage is the first sign-in's; asking with a stale login only gets a 401
		}
		values, windows := p.provider.usage(acc)
		out[i].Usage = values
		if windows != nil {
			out[i].Windows = windows
		}
	}
	return out
}

// MARK: Shared by providers

// loginRenewal lets each named login be refreshed at most once per `every`, so a failing refresh
// is not retried on every request. The zero value is ready to use.
type loginRenewal struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// renew runs refresh (the provider's own tool, named by through) on a named login's dir unless it
// ran within every; true when it ran and worked. The default login is never refreshed from here.
func (g *loginRenewal) renew(provider, through string, acc account, now time.Time, every time.Duration, refresh func(dir string) error) bool {
	if acc.Dir == "" || refresh == nil {
		return false
	}
	g.mu.Lock()
	if last, ok := g.last[acc.ID]; ok && now.Sub(last) < every {
		g.mu.Unlock()
		return false
	}
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	g.last[acc.ID] = now
	g.mu.Unlock()
	if err := refresh(acc.Path); err != nil {
		log.Printf("%s account %s: refresh failed: %v", provider, acc.ID, err)
		return false
	}
	log.Printf("%s account %s refreshed through %s", provider, acc.ID, through)
	return true
}

// usageCache is each account's windows as last read from its provider's server, so a status asks
// that server at most once per `every` per account. The zero value is ready to use.
type usageCache struct {
	mu   sync.Mutex
	byID map[string]cachedUsage
}

type cachedUsage struct {
	read    time.Time
	windows []usageWindow
}

// put keeps windows seen now (in a reply's headers, say: fresher than a read).
func (c *usageCache) put(id string, now time.Time, windows []usageWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]cachedUsage{}
	}
	c.byID[id] = cachedUsage{read: now, windows: windows}
}

// get is the account's windows: the kept ones when read within every, else read now. A failed read
// keeps the last windows, returns its error, and is not tried again before every has passed.
func (c *usageCache) get(id string, now time.Time, every time.Duration, read func() ([]usageWindow, error)) ([]usageWindow, error) {
	c.mu.Lock()
	cached, ok := c.byID[id]
	if ok && now.Sub(cached.read) < every {
		c.mu.Unlock()
		return cached.windows, nil
	}
	if c.byID == nil {
		c.byID = map[string]cachedUsage{}
	}
	c.byID[id] = cachedUsage{read: now, windows: cached.windows}
	c.mu.Unlock()
	windows, err := read()
	if err != nil {
		return cached.windows, err
	}
	c.put(id, now, windows)
	return windows, nil
}

// accountLabel is the one rule for naming an account to a person: its email, else its id.
func accountLabel(acc account) string {
	if acc.Email != "" {
		return acc.Email
	}
	return acc.ID
}

// fetchUsage is the body of a GET for an account's usage at url with headers set (credentials
// among them); an error for any status but 200. Every provider reads its usage endpoint through it.
func fetchUsage(client *http.Client, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", "limitbreak")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return raw, nil
}
