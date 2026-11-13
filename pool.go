package main

// A provider is one kind of account (Claude, Codex; more later): where its logins live, how a request is
// put on one, how a reply says the account is spent, and what its usage is. A pool is one
// provider's accounts as the proxy rotates them: which one each client is on, and which are spent
// until when. What differs between providers is behind the interface; the rotation is the pool's.

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type provider interface {
	// name is the provider's id in the API ("claude", "codex").
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
	// unsaid). body is the start of a 429's body, nil for any other reply.
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

type pool struct {
	provider provider
	upstream string
	now      func() time.Time

	mu           sync.Mutex
	discovered   []account
	discoveredAt time.Time
	active       map[string]string
	spentUntil   map[string]time.Time
}

func newPool(p provider, upstream string) *pool {
	return &pool{provider: p, upstream: strings.TrimRight(upstream, "/"), now: time.Now,
		active: map[string]string{}, spentUntil: map[string]time.Time{}}
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

// current is the client's active account, moving on from a spent one; nil when none is signed in
// or all are spent.
func (p *pool) current(client string) *account {
	list := rotation(p.list())
	if len(list) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	want := p.active[client]
	for i := range list {
		if list[i].ID == want && !p.spentUntil[want].After(now) {
			return &list[i]
		}
	}
	usable := nextAccount(list, want, p.spentUntil, now)
	if usable != nil {
		p.active[client] = usable.ID
	}
	return usable
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
	// Provider is the kind of account: "claude" or "codex". Absent means "claude".
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

// accountLabel is the one rule for naming an account to a person: its email, else its id.
func accountLabel(acc account) string {
	if acc.Email != "" {
		return acc.Email
	}
	return acc.ID
}
