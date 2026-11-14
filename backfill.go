package main

// `limitbreak backfill` rebuilds request-log records (requestlog.go) for the replies before the
// proxy started logging, from Claude Code's transcripts, so `blame` can rank earlier sessions too.
// The account is inferred from the switch log (pool.spend's and pool.setActive's lines), so it is a
// best guess: a client also moves without a line of its own (after a restart, or off an account
// another client found spent), and a request that bypassed the proxy looks the same as one that
// went through it. The records carry `backfilled` and no utilization.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// maxTranscriptLine is the longest transcript line read; a longer one (a big tool result) is skipped.
const maxTranscriptLine = 16 << 20

// unknownAccount is the account of a reply sent before its client's first logged switch.
const unknownAccount = "?"

// switchLogPath is the service's stderr on macOS (install.sh), where the pool logs each switch.
func switchLogPath(home string) string {
	return filepath.Join(home, "Library", "Logs", "limitbreak.log")
}

// MARK: Decisions (pure)

// switchLine and setLine match what pool.spend and pool.setActive log (time, provider, client,
// account); builds before the provider name was added omit it.
var (
	switchLine = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) (?:(\w+) )?account \S+ spent until \S+ for client (\S+); now (\S+)`)
	setLine    = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) client (\S+) set to (?:(\w+) )?account (\S+)$`)
)

// accountSwitch is one logged switch: from At, Client is on Account.
type accountSwitch struct {
	At      time.Time
	Client  string
	Account string
}

// parseSwitchLine reads one log line (its timestamp in loc); ok only for a Claude switch, by a
// spent account or by a request to set the client's account. `none` (no account could take it) is
// the client's own login.
func parseSwitchLine(line string, loc *time.Location) (accountSwitch, bool) {
	var stamp, provider, client, account string
	if m := switchLine.FindStringSubmatch(line); m != nil {
		stamp, provider, client, account = m[1], m[2], m[3], m[4]
	} else if m := setLine.FindStringSubmatch(line); m != nil {
		stamp, client, provider, account = m[1], m[2], m[3], m[4]
	} else {
		return accountSwitch{}, false
	}
	if provider != "" && provider != "claude" {
		return accountSwitch{}, false
	}
	at, err := time.ParseInLocation("2006/01/02 15:04:05", stamp, loc)
	if err != nil {
		return accountSwitch{}, false
	}
	if account == "none" {
		account = ownAccount
	}
	return accountSwitch{At: at, Client: client, Account: account}, true
}

// switchHistory is each client's switches in time order.
type switchHistory map[string][]accountSwitch

func newSwitchHistory(switches []accountSwitch) switchHistory {
	h := switchHistory{}
	for _, s := range switches {
		h[s.Client] = append(h[s.Client], s)
	}
	for _, list := range h {
		sort.SliceStable(list, func(i, j int) bool { return list[i].At.Before(list[j].At) })
	}
	return h
}

// accountAt is the account client was on at t: the latest switch at or before t, else unknown.
func (h switchHistory) accountAt(client string, t time.Time) string {
	list := h[client]
	i := sort.Search(len(list), func(i int) bool { return list[i].At.After(t) })
	if i == 0 {
		return unknownAccount
	}
	return list[i-1].Account
}

// transcriptOrigin is what a transcript's path says: the client that sent its requests (`shared`,
// or a Hi, computer agent's own /_c/<client>), its default agent and its session.
func transcriptOrigin(home, path string) (client, agent, session string) {
	client = "shared"
	if rel, ok := strings.CutPrefix(path, filepath.Join(home, meshACPDir)+string(filepath.Separator)); ok {
		client, _, _ = strings.Cut(rel, string(filepath.Separator))
		agent = "sdk"
	}
	// <session>.jsonl, or <session>/subagents/agent-*.jsonl: a subagent is its parent's.
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		session = filepath.Base(filepath.Dir(dir))
	} else {
		session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return client, agent, session
}

// transcriptReply reads one transcript line: ok for an assistant reply with usage. The key is its
// request id (else its message id); a reply is written once per content block under the same key.
// Client, Account and the path's agent and session are the caller's.
func transcriptReply(line []byte) (key string, rec requestRecord, ok bool) {
	if !bytes.Contains(line, []byte(`"type":"assistant"`)) {
		return "", rec, false
	}
	var l struct {
		Type       string    `json:"type"`
		Timestamp  time.Time `json:"timestamp"`
		SessionID  string    `json:"sessionId"`
		Entrypoint string    `json:"entrypoint"`
		RequestID  string    `json:"requestId"`
		Message    struct {
			ID    string      `json:"id"`
			Model string      `json:"model"`
			Usage *tokenUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil ||
		l.Message.Model == "<synthetic>" || l.Timestamp.IsZero() {
		return "", rec, false
	}
	key = l.RequestID
	if key == "" {
		key = l.Message.ID
	}
	if key == "" {
		return "", rec, false
	}
	rec = requestRecord{Time: l.Timestamp.UTC(), Provider: "claude", Session: l.SessionID, Model: l.Message.Model,
		Status: 200, tokenUsage: *l.Message.Usage, Backfilled: true}
	if l.Entrypoint != "" {
		// sdk-ts, sdk-cli → sdk; cli → cli; anything else (claude-desktop) as it is.
		rec.Agent = agentOf(l.Entrypoint)
	}
	return key, rec, true
}

// replySet keeps one record per reply key: the line with the most output tokens (the last block).
type replySet map[string]requestRecord

func (s replySet) add(key string, rec requestRecord) {
	if old, ok := s[key]; !ok || rec.OutputTokens > old.OutputTokens {
		s[key] = rec
	}
}

// backfillRecords is the set's replies in [since, until) (no upper bound when until is zero), each
// with the account its client was on, in time order.
func backfillRecords(set replySet, history switchHistory, since, until time.Time) []requestRecord {
	var out []requestRecord
	for _, rec := range set {
		if rec.Time.Before(since) || (!until.IsZero() && !rec.Time.Before(until)) {
			continue
		}
		rec.Account = history.accountAt(rec.Client, rec.Time)
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].Session < out[j].Session
	})
	return out
}

// earliestLive is the time of the first record the proxy logged itself; zero when there is none.
func earliestLive(records []requestRecord) time.Time {
	var first time.Time
	for _, rec := range records {
		if !rec.Backfilled && (first.IsZero() || rec.Time.Before(first)) {
			first = rec.Time
		}
	}
	return first
}

// backfillStart is where --since starts: local midnight of now's day when empty, a date
// (YYYY-MM-DD, local), or a duration back from now (24h, 7d).
func backfillStart(since string, now time.Time) (time.Time, error) {
	if since == "" {
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	}
	if day, err := time.ParseInLocation("2006-01-02", since, now.Location()); err == nil {
		return day, nil
	}
	back, err := parseSince(since)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad --since %q: use a date (2026-11-14) or a duration (24h, 7d)", since)
	}
	return now.Add(-back), nil
}

// backfillSummary is the one line the command prints.
func backfillSummary(records []requestRecord, since, cutoff time.Time) string {
	sessions, unknown := map[string]bool{}, 0
	for _, rec := range records {
		sessions[rec.Session] = true
		if rec.Account == unknownAccount {
			unknown++
		}
	}
	upTo := "none (no live record)"
	if !cutoff.IsZero() {
		upTo = cutoff.Local().Format("2006-01-02 15:04:05")
	}
	span := "-"
	if len(records) > 0 {
		span = records[0].Time.Local().Format("2006-01-02 15:04") + " – " + records[len(records)-1].Time.Local().Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%d replies, %d sessions, %s (since %s, cutoff %s), %d with an unknown account",
		len(records), len(sessions), span, since.Local().Format("2006-01-02 15:04"), upTo, unknown)
}

// MARK: Reading

// eachLine calls fn with every line of r (without its newline), skipping lines over maxLine. The
// slice is reused: fn must not keep it.
func eachLine(r io.Reader, maxLine int, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	overlong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !overlong {
			if len(line)+len(chunk) > maxLine+1 {
				overlong, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if trimmed := bytes.TrimRight(line, "\r\n"); !overlong && len(trimmed) > 0 {
			fn(trimmed)
		}
		line, overlong = line[:0], false
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readSwitchHistory is the switch log's history; an absent log is an empty one.
func readSwitchHistory(path string, loc *time.Location) (switchHistory, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return switchHistory{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var switches []accountSwitch
	err = eachLine(f, maxScannedLine, func(line []byte) {
		if s, ok := parseSwitchLine(string(line), loc); ok {
			switches = append(switches, s)
		}
	})
	return newSwitchHistory(switches), err
}

// transcriptFiles is every session and subagent transcript under home changed at or after since.
func transcriptFiles(home string, since time.Time) []string {
	var out []string
	for _, root := range claudeConfigGlobs {
		for _, pattern := range []string{"projects/*/*.jsonl", "projects/*/*/subagents/*.jsonl"} {
			found, _ := filepath.Glob(filepath.Join(home, root, pattern))
			for _, path := range found {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && !info.ModTime().Before(since) {
					out = append(out, path)
				}
			}
		}
	}
	return out
}

// scanTranscript adds the replies of one transcript file to set.
func scanTranscript(home, path string, set replySet) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	client, agent, session := transcriptOrigin(home, path)
	return eachLine(f, maxTranscriptLine, func(line []byte) {
		key, rec, ok := transcriptReply(line)
		if !ok {
			return
		}
		rec.Client = client
		if agent != "" {
			rec.Agent = agent
		}
		if rec.Session == "" {
			rec.Session = session
		}
		set.add(key, rec)
	})
}

// writeRecordsAtomically replaces path with records, one JSON line each (mode 600).
func writeRecordsAtomically(path string, records []requestRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	for _, rec := range records {
		if err = enc.Encode(rec); err != nil {
			tmp.Close()
			return err
		}
	}
	if err = w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// MARK: Command

func backfillCommand(home string, args []string, now time.Time, out io.Writer) error {
	flags := flag.NewFlagSet("backfill", flag.ContinueOnError)
	sinceFlag := flags.String("since", "", "from when: a date (2026-11-14), or a duration back (24h, 7d); default local midnight today")
	dryRun := flags.Bool("dry-run", false, "print the summary only; write nothing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	since, err := backfillStart(*sinceFlag, now)
	if err != nil {
		return err
	}
	live, err := readRequests(home)
	if err != nil {
		return err
	}
	cutoff := earliestLive(live)
	history, err := readSwitchHistory(switchLogPath(home), now.Location())
	if err != nil {
		return err
	}
	set := replySet{}
	for _, path := range transcriptFiles(home, since) {
		if err := scanTranscript(home, path, set); err != nil {
			fmt.Fprintf(out, "skipped %s: %v\n", tildePath(home, path), err)
		}
	}
	records := backfillRecords(set, history, since, cutoff)
	summary := backfillSummary(records, since, cutoff)
	if *dryRun {
		fmt.Fprintln(out, "dry run:", summary)
		return nil
	}
	if err := writeRecordsAtomically(requestsBackfillPath(home), records); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s; wrote %s\n", summary, tildePath(home, requestsBackfillPath(home)))
	return nil
}
