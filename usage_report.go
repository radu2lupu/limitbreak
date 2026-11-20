package main

// `GET /_usage` is every account's fuel: each account's status (its windows, whether it is spent,
// which clients are on it) for Claude, Codex, Grok and OpenCode (opencode.go). A Claude account also
// lists the sessions that used it over the period, from the request log (blame.go); an OpenCode one
// its total from OpenCode's own stats. Hi, computer draws its Usage page from it. Like `/_status` it holds no
// token and no prompt text.

import (
	"cmp"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	// usageReportSessions is how many sessions each account lists by name; the rest are summed.
	usageReportSessions = 5
	// usageReportTTL is how long a report is reused: reading the log and transcripts takes a while.
	usageReportTTL    = 60 * time.Second
	defaultUsageSince = 7 * 24 * time.Hour
)

// usageSession is one session's share of an account over the period.
type usageSession struct {
	Session  string  `json:"session"`
	Title    string  `json:"title"`
	Project  string  `json:"project"`
	Client   string  `json:"client,omitempty"`
	Weighted float64 `json:"weighted"`
	// Backfilled is true when some of its replies were rebuilt from transcripts (account inferred).
	Backfilled bool `json:"backfilled,omitempty"`
}

// usageAccount is an account's status with what used it.
type usageAccount struct {
	accountStatus
	// Weighted is the account's weighted total over the period (see weighted).
	Weighted float64 `json:"weighted"`
	// Sessions are its heaviest sessions, heaviest first; Rest is the weighted total of the others.
	Sessions []usageSession `json:"sessions"`
	Rest     float64        `json:"rest"`
	// NoLimit is true when the provider has no usage limit for the account (OpenCode Zen is paid as
	// you go), so it has no window to draw; an account with no windows otherwise just was not read.
	NoLimit bool `json:"noLimit,omitempty"`
}

// usageReport is the body of `GET /_usage`.
type usageReport struct {
	// Since is the start of the period (unix seconds); Generated is when the report was made.
	Since     int64          `json:"since"`
	Generated int64          `json:"generated"`
	Accounts  []usageAccount `json:"accounts"`
	// Own is what went on clients' own logins; Unrecorded is what was rebuilt with no account known.
	Own        float64 `json:"own"`
	Unrecorded float64 `json:"unrecorded"`
}

// MARK: Decisions (pure)

// accountSessions is, per account, each session's weighted total since cutoff, heaviest first.
// A session that moved between accounts counts on each for the replies that account answered.
func accountSessions(records []requestRecord, cutoff time.Time) map[string][]usageSession {
	type key struct{ account, session string }
	sums := map[key]*usageSession{}
	for _, rec := range records {
		if rec.Time.Before(cutoff) {
			continue
		}
		k := key{accountKey(rec.Provider, cmp.Or(rec.Account, "?")), cmp.Or(rec.Session, "?")}
		s := sums[k]
		if s == nil {
			s = &usageSession{Session: k.session, Client: rec.Client}
			sums[k] = s
		}
		s.Weighted += weighted(rec.tokenUsage)
		s.Backfilled = s.Backfilled || rec.Backfilled
	}
	out := map[string][]usageSession{}
	for k, s := range sums {
		out[k.account] = append(out[k.account], *s)
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Weighted != list[j].Weighted {
				return list[i].Weighted > list[j].Weighted
			}
			return list[i].Session < list[j].Session
		})
	}
	return out
}

// buildUsageReport joins the accounts' status with their sessions (Claude, Codex and Grok replies are
// logged; an account with none, such as OpenCode's, is listed with none). info resolves a listed session's
// transcript and name a client's agent name; each is asked only for the sessions that are listed.
func buildUsageReport(status []accountStatus, records []requestRecord, since time.Duration, now time.Time,
	info func(session string) transcriptInfo, name func(client string) string) usageReport {
	cutoff := now.Add(-since)
	bySession := accountSessions(records, cutoff)
	total := func(list []usageSession) float64 {
		sum := 0.0
		for _, s := range list {
			sum += s.Weighted
		}
		return sum
	}
	report := usageReport{Since: cutoff.Unix(), Generated: now.Unix(), Accounts: []usageAccount{},
		Own: total(bySession[ownAccount]), Unrecorded: total(bySession["?"])}
	for _, st := range status {
		list := bySession[accountKey(st.Provider, st.ID)]
		acc := usageAccount{accountStatus: st, Weighted: total(list), Sessions: []usageSession{}}
		for i, s := range list {
			if i >= usageReportSessions {
				acc.Rest += s.Weighted
				continue
			}
			ti := transcriptInfo{Project: "?", Title: "?"}
			if s.Session != "?" {
				ti = info(s.Session)
			}
			s.Title, s.Project = ti.Title, ti.Project
			if n := name(s.Client); n != "" {
				s.Client = n
			}
			acc.Sessions = append(acc.Sessions, s)
		}
		report.Accounts = append(report.Accounts, acc)
	}
	return report
}

// MARK: Serving

// usageReports makes `/_usage` reports for one home, reusing one for usageReportTTL per period.
type usageReports struct {
	home string
	// others are accounts limitbreak does not proxy (OpenCode's), added after the proxied ones.
	others func(since time.Duration) []usageAccount
	mu     sync.Mutex
	kept   map[time.Duration]keptReport
}

type keptReport struct {
	made   time.Time
	report usageReport
}

func (u *usageReports) serve(w http.ResponseWriter, r *http.Request, status func() []accountStatus) {
	since := defaultUsageSince
	if s := r.URL.Query().Get("since"); s != "" {
		d, err := parseSince(s)
		if err != nil || d == 0 {
			apiError(w, http.StatusBadRequest, "bad since: use a duration like 5h or 7d")
			return
		}
		since = d
	}
	// One report at a time: a second caller waits for the first and reuses its report.
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	kept, ok := u.kept[since]
	if !ok || now.Sub(kept.made) >= usageReportTTL {
		records, err := readRequests(u.home)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "request log unreadable")
			return
		}
		kept = keptReport{made: now, report: buildUsageReport(status(), records, since, now,
			func(id string) transcriptInfo { return sessionTranscript(u.home, id) },
			func(client string) string { return agentName(u.home, client) })}
		if u.others != nil {
			kept.report.Accounts = append(kept.report.Accounts, u.others(since)...)
		}
		if u.kept == nil {
			u.kept = map[time.Duration]keptReport{}
		}
		u.kept[since] = kept
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(kept.report)
}
