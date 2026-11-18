package main

// OpenCode is not proxied: limitbreak only reads its accounts for `GET /_usage`. They are OpenCode's
// own keys in `~/.local/share/opencode/auth.json`: "opencode-go", the Go subscription, whose rolling
// 5-hour, weekly and monthly windows come from `GET https://opencode.ai/zen/go/v1/usage` (it spends
// nothing), and "opencode", Zen, which is paid as you go and has no window. What each one used over
// the period comes from `opencode stats --json --models`, summed by provider.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	opencodeProvider   = "opencode"
	defaultOpencodeURL = "https://opencode.ai/zen/go/v1/usage"
	// opencodeUsageEvery is how often the Go windows are read; opencodeStatsEvery how often the
	// token totals are (`opencode stats` takes a few seconds, so it runs in the background).
	opencodeUsageEvery = 5 * time.Minute
	opencodeStatsEvery = 5 * time.Minute
)

// opencodeKinds are the OpenCode keys shown, in order. limited is whether the key has windows.
var opencodeKinds = []struct {
	id, label, plan string
	limited         bool
}{
	{"opencode-go", "OpenCode Go", "go", true},
	{"opencode", "OpenCode Zen", "zen", false},
}

// MARK: Decisions (pure)

// opencodeWindows are the windows in a `GET /zen/go/v1/usage` reply: rolling (5 hours), weekly
// and monthly, each a percent used and when it resets.
func opencodeWindows(raw []byte, now time.Time) ([]usageWindow, error) {
	type window struct {
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resetsAt"`
	}
	var doc struct {
		Usage map[string]window `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := []usageWindow{}
	for _, w := range []struct {
		id      string
		minutes int
	}{{"rolling", 5 * 60}, {"weekly", 7 * 24 * 60}, {"monthly", 30 * 24 * 60}} {
		got, ok := doc.Usage[w.id]
		if !ok || got.Percent == nil {
			continue
		}
		reset := int64(0)
		if at, err := time.Parse(time.RFC3339Nano, got.ResetsAt); err == nil {
			reset = at.Unix()
		}
		out = append(out, newWindow(w.id, *got.Percent, reset, w.minutes, now))
	}
	return out, nil
}

// opencodeSpentUntil is when a used-up window frees the account: the latest reset among the windows
// at 100%; nil when none is used up.
func opencodeSpentUntil(windows []usageWindow) *int64 {
	var until *int64
	for _, w := range windows {
		if w.UsedPercent >= 100 && w.ResetsAt != nil && (until == nil || *w.ResetsAt > *until) {
			at := *w.ResetsAt
			until = &at
		}
	}
	return until
}

// opencodeTokens is each provider's tokens in an `opencode stats --json --models` reply. Reasoning
// counts as output.
func opencodeTokens(raw []byte) (map[string]tokenUsage, error) {
	var doc struct {
		Models []struct {
			Model struct {
				ProviderID string `json:"providerID"`
			} `json:"model"`
			Tokens struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]tokenUsage{}
	for _, m := range doc.Models {
		u := out[m.Model.ProviderID]
		u.InputTokens += m.Tokens.Input
		u.CacheCreationInputTokens += m.Tokens.Cache.Write
		u.CacheReadInputTokens += m.Tokens.Cache.Read
		u.OutputTokens += m.Tokens.Output + m.Tokens.Reasoning
		out[m.Model.ProviderID] = u
	}
	return out, nil
}

// opencodeDays is the `opencode stats --days` value for a period: whole days, rounded up, and 0
// (today) for anything under a day. Its days are calendar days, so the totals are approximate.
func opencodeDays(since time.Duration) int {
	if since < 24*time.Hour {
		return 0
	}
	return int(math.Ceil(since.Hours() / 24))
}

// MARK: Accounts

type opencodeAccounts struct {
	// auth is OpenCode's auth.json; usageURL the Go usage address.
	auth, usageURL string
	client         *http.Client
	now            func() time.Time
	// stats runs `opencode stats --json --models --days <days>`; injected for tests.
	stats func(days int) ([]byte, error)

	usages usageCache
	mu     sync.Mutex
	tokens map[int]keptTokens
}

type keptTokens struct {
	read    time.Time
	running bool
	byKey   map[string]tokenUsage
}

func newOpencodeAccounts(home, usageURL string) *opencodeAccounts {
	bin := findInstalled("opencode", filepath.Join(home, ".local", "bin", "opencode"), filepath.Join(home, ".opencode", "bin", "opencode"))
	return &opencodeAccounts{auth: filepath.Join(home, ".local", "share", "opencode", "auth.json"), usageURL: usageURL,
		client: &http.Client{Timeout: 4 * time.Second}, now: time.Now, stats: opencodeStats(bin)}
}

func opencodeStats(bin string) func(days int) ([]byte, error) {
	return func(days int) ([]byte, error) {
		if bin == "" {
			return nil, errors.New("opencode is not installed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return exec.CommandContext(ctx, bin, "stats", "--json", "--models", "--days", fmt.Sprint(days)).Output()
	}
}

// keys is each OpenCode key by provider id; empty when OpenCode has none.
func (o *opencodeAccounts) keys() map[string]string {
	raw, err := os.ReadFile(o.auth)
	if err != nil {
		return nil
	}
	var doc map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := map[string]string{}
	for id, entry := range doc {
		if entry.Type == "api" && entry.Key != "" {
			out[id] = entry.Key
		}
	}
	return out
}

// report is OpenCode's accounts for `/_usage`: each key's windows and what it used over since. The
// token totals are the last ones read; a stale read starts a new one in the background, so the
// first report after a start may show none.
func (o *opencodeAccounts) report(since time.Duration) []usageAccount {
	keys := o.keys()
	out := []usageAccount{}
	if len(keys) == 0 {
		return out
	}
	tokens := o.tokensFor(opencodeDays(since))
	for _, kind := range opencodeKinds {
		key, ok := keys[kind.id]
		if !ok {
			continue
		}
		st := accountStatus{ID: kind.id, Provider: opencodeProvider, Label: kind.label, Plan: kind.plan,
			ActiveFor: []string{}, Windows: []usageWindow{}}
		if kind.limited {
			windows, err := o.usages.get(kind.id, o.now(), opencodeUsageEvery, func() ([]usageWindow, error) { return o.readUsage(key) })
			if err != nil {
				log.Printf("opencode %s: usage read failed: %v", kind.id, err)
			}
			if windows != nil {
				st.Windows = windows
			}
			st.SpentUntil = opencodeSpentUntil(st.Windows)
		}
		used := weighted(tokens[kind.id])
		// OpenCode's stats do not split a provider by session: the whole period is one share.
		out = append(out, usageAccount{accountStatus: st, Weighted: used, Sessions: []usageSession{}, Rest: used, NoLimit: !kind.limited})
	}
	return out
}

func (o *opencodeAccounts) readUsage(key string) ([]usageWindow, error) {
	raw, err := fetchUsage(o.client, o.usageURL, map[string]string{"Authorization": "Bearer " + key})
	if err != nil {
		return nil, err
	}
	return opencodeWindows(raw, o.now())
}

// tokensFor is the last token totals read for days, starting a read in the background when they
// are older than opencodeStatsEvery and none is running.
func (o *opencodeAccounts) tokensFor(days int) map[string]tokenUsage {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.tokens == nil {
		o.tokens = map[int]keptTokens{}
	}
	kept := o.tokens[days]
	if !kept.running && o.now().Sub(kept.read) >= opencodeStatsEvery {
		kept.running = true
		o.tokens[days] = kept
		go o.readTokens(days)
	}
	return kept.byKey
}

func (o *opencodeAccounts) readTokens(days int) {
	raw, err := o.stats(days)
	var byKey map[string]tokenUsage
	if err == nil {
		byKey, err = opencodeTokens(raw)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.tokens[days]
	kept.running, kept.read = false, o.now()
	if err != nil {
		log.Printf("opencode stats: %v", err)
	} else {
		kept.byKey = byKey
	}
	o.tokens[days] = kept
}
