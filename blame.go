package main

// `limitbreak blame` reads the request log (requestlog.go) and ranks what used the accounts' limits:
// by Claude Code session (with its project and title, from its transcript), project, account or client.

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// usageWeights price each kind of token against an uncached input token, by the API's price ratios
// (cache write 1.25×, cache read 0.1×, output 5×). Subscription limits are not published, but they
// roughly track cost, so the weighted total ranks what used a limit better than a raw token count.
var usageWeights = struct{ Input, CacheWrite, CacheRead, Output float64 }{1, 1.25, 0.1, 5}

func weighted(u tokenUsage) float64 {
	return float64(u.InputTokens)*usageWeights.Input + float64(u.CacheCreationInputTokens)*usageWeights.CacheWrite +
		float64(u.CacheReadInputTokens)*usageWeights.CacheRead + float64(u.OutputTokens)*usageWeights.Output
}

const (
	// transcriptReadLimit is how much of a session's transcript is read for its project and title.
	transcriptReadLimit = 2 << 20
	titleWidth          = 60
)

type blameOptions struct {
	since   time.Duration
	account string
	by      string
}

// blameRow is one group's share of the usage.
type blameRow struct {
	Key     string `json:"key"`
	Project string `json:"project,omitempty"`
	Title   string `json:"title,omitempty"`
	// Requests counts replies; the token counts are their sums.
	Requests int `json:"requests"`
	tokenUsage
	Weighted float64 `json:"weighted"`
	// FiveHourRise and SevenDayRise are the percentage points of the windows attributed to the group
	// (see utilizationRises).
	FiveHourRise float64 `json:"five_hour_rise"`
	SevenDayRise float64 `json:"seven_day_rise"`
	// Backfilled counts the replies rebuilt from transcripts (no utilization, account inferred).
	Backfilled int `json:"backfilled"`
}

// transcriptInfo is what a session's transcript says about it; "?" where it says nothing.
type transcriptInfo struct {
	Project string
	Title   string
}

// MARK: Decisions (pure)

// utilizationRises is, for each record (records in time order), how many percentage points the
// account's 5-hour and 7-day utilization rose since that account's previous record that stated it.
// A drop means the window reset, and is nobody's. The headers move in 1% steps and every session
// on an account shares its window, so a rise lands on whichever request first saw it: over many
// requests this is a fair share, for one request it is approximate.
func utilizationRises(records []requestRecord) (fiveHour, sevenDay []float64) {
	fiveHour, sevenDay = make([]float64, len(records)), make([]float64, len(records))
	last5, last7 := map[string]float64{}, map[string]float64{}
	rise := func(last map[string]float64, account string, v *float64) float64 {
		if v == nil {
			return 0
		}
		prev, seen := last[account]
		last[account] = *v
		if seen && *v > prev {
			return (*v - prev) * 100
		}
		return 0
	}
	for i, rec := range records {
		fiveHour[i] = rise(last5, rec.Account, rec.FiveHour)
		sevenDay[i] = rise(last7, rec.Account, rec.SevenDay)
	}
	return fiveHour, sevenDay
}

// blame groups the records since now-since (of one account when asked) and ranks the groups by
// weighted total. info resolves a session's transcript; it is asked once per session.
func blame(records []requestRecord, opts blameOptions, now time.Time, info func(session string) transcriptInfo) []blameRow {
	sorted := append([]requestRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	// Rises are worked out over every record, so the first one in the period has its predecessor.
	rise5, rise7 := utilizationRises(sorted)
	cutoff := now.Add(-opts.since)
	infos := map[string]transcriptInfo{}
	sessionInfo := func(id string) transcriptInfo {
		if id == "" {
			return transcriptInfo{Project: "?", Title: "?"}
		}
		if got, ok := infos[id]; ok {
			return got
		}
		infos[id] = info(id)
		return infos[id]
	}
	groups := map[string]*blameRow{}
	for i, rec := range sorted {
		if (opts.since > 0 && rec.Time.Before(cutoff)) || (opts.account != "" && rec.Account != opts.account) {
			continue
		}
		var key string
		switch opts.by {
		case "project":
			key = sessionInfo(rec.Session).Project
		case "account":
			key = rec.Account
		case "client":
			key = rec.Client
		default:
			key = rec.Session
		}
		if key == "" {
			key = "?"
		}
		row := groups[key]
		if row == nil {
			row = &blameRow{Key: key}
			if opts.by == "session" && rec.Session != "" {
				si := sessionInfo(rec.Session)
				row.Project, row.Title = si.Project, si.Title
			}
			groups[key] = row
		}
		row.Requests++
		row.InputTokens += rec.InputTokens
		row.CacheCreationInputTokens += rec.CacheCreationInputTokens
		row.CacheReadInputTokens += rec.CacheReadInputTokens
		row.OutputTokens += rec.OutputTokens
		row.Weighted += weighted(rec.tokenUsage)
		row.FiveHourRise += rise5[i]
		row.SevenDayRise += rise7[i]
		if rec.Backfilled {
			row.Backfilled++
		}
	}
	rows := make([]blameRow, 0, len(groups))
	for _, row := range groups {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Weighted != rows[j].Weighted {
			return rows[i].Weighted > rows[j].Weighted
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

// parseSince is a Go duration, or whole days as `<n>d`.
func parseSince(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad --since %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad --since %q: use a duration like 24h, 90m or 7d", s)
	}
	return d, nil
}

// humanize is a count for a person: 950, 9.5k, 340k, 1.2M, 3.1B.
func humanize(n float64) string {
	for _, unit := range []struct {
		size   float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "k"}} {
		if n >= unit.size {
			v := n / unit.size
			if v < 10 {
				return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit.suffix
			}
			return fmt.Sprintf("%.0f", v) + unit.suffix
		}
	}
	return fmt.Sprintf("%.0f", n)
}

// points is a rise in percentage points, `-` for none.
func points(v float64) string {
	if v <= 0 {
		return "-"
	}
	return "+" + strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

// oneLine is text on one line, cut to width runes.
func oneLine(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= width {
		return text
	}
	return string([]rune(text)[:width-1]) + "…"
}

// promptText is the person's words in a user message's content (a string or text blocks); empty for
// tool results and for wrappers Claude Code injects (`<command-name>`, `<system-reminder>`, …).
func promptText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<") && strings.TrimSpace(b.Text) != "" {
				text = b.Text
				break
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(text), "<") {
		return ""
	}
	return strings.TrimSpace(text)
}

// transcriptFacts reads a transcript's head: its cwd (the project) and its title, which is the
// latest title the session was given (by the person, else Claude Code's), else a summary line, else
// its first prompt.
func transcriptFacts(head []byte) (cwd, title string) {
	var custom, ai, summary, prompt string
	for _, line := range bytes.Split(head, []byte{'\n'}) {
		// Most lines are long and say nothing wanted once the cwd and first prompt are known.
		if cwd != "" && prompt != "" && !bytes.Contains(line, []byte(`-title"`)) && !bytes.Contains(line, []byte(`"summary"`)) {
			continue
		}
		var l struct {
			Type        string `json:"type"`
			Cwd         string `json:"cwd"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
			Summary     string `json:"summary"`
			IsMeta      bool   `json:"isMeta"`
			Message     *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if cwd == "" {
			cwd = l.Cwd
		}
		switch l.Type {
		case "custom-title":
			custom = cmp.Or(l.CustomTitle, custom)
		case "ai-title":
			ai = cmp.Or(l.AITitle, ai)
		case "summary":
			if summary == "" {
				summary = l.Summary
			}
		case "user":
			if prompt == "" && !l.IsMeta && l.Message != nil {
				prompt = promptText(l.Message.Content)
			}
		}
	}
	return cwd, cmp.Or(custom, ai, summary, prompt)
}

// tildePath shows a path under home as ~/….
func tildePath(home, path string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+string(filepath.Separator))) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// MARK: Reading

var sessionID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// meshACPDir holds Hi, computer's agents (Hi, computer ships limitbreak): each runs with its own
// home, one per revision, under <client>/home/revisions/<n>, and reaches the proxy as /_c/<client>.
const meshACPDir = "Library/Application Support/Hi, computer/mesh-acp"

// claudeConfigGlobs are the Claude config dirs under home whose projects/ hold session transcripts.
var claudeConfigGlobs = []string{".claude", ".claude-*", meshACPDir + "/*/home/revisions/*/.claude"}

// sessionTranscript is what the transcript of session id says, under any Claude config dir of home.
func sessionTranscript(home, id string) transcriptInfo {
	unknown := transcriptInfo{Project: "?", Title: "?"}
	if !sessionID.MatchString(id) {
		return unknown
	}
	var matches []string
	for _, dir := range claudeConfigGlobs {
		found, _ := filepath.Glob(filepath.Join(home, dir, "projects", "*", id+".jsonl"))
		matches = append(matches, found...)
	}
	if len(matches) == 0 {
		return unknown
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return unknown
	}
	defer f.Close()
	head, _ := io.ReadAll(io.LimitReader(f, transcriptReadLimit))
	cwd, title := transcriptFacts(head)
	info := unknown
	if cwd != "" {
		info.Project = tildePath(home, cwd)
	}
	if title != "" {
		info.Title = oneLine(title, titleWidth)
	}
	return info
}

// readRequests is every record in the request log, the backfilled ones and the rotated file's
// first; bad lines are skipped.
func readRequests(home string) ([]requestRecord, error) {
	var out []requestRecord
	for _, path := range []string{requestsBackfillPath(home), requestsPath(home) + ".1", requestsPath(home)} {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), maxScannedLine)
		for scanner.Scan() {
			var rec requestRecord
			if json.Unmarshal(scanner.Bytes(), &rec) == nil && !rec.Time.IsZero() {
				out = append(out, rec)
			}
		}
		err = scanner.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MARK: Command

func blameCommand(home string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("blame", flag.ContinueOnError)
	since := flags.String("since", "24h", "how far back to look (24h, 90m, 7d)")
	account := flags.String("account", "", "only this account (`own` is the client's own login)")
	by := flags.String("by", "session", "group by session, project, account or client")
	asJSON := flags.Bool("json", false, "print the rows as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	switch *by {
	case "session", "project", "account", "client":
	default:
		return fmt.Errorf("bad --by %q: use session, project, account or client", *by)
	}
	period, err := parseSince(*since)
	if err != nil {
		return err
	}
	records, err := readRequests(home)
	if err != nil {
		return err
	}
	rows := blame(records, blameOptions{since: period, account: *account, by: *by}, time.Now(),
		func(id string) transcriptInfo { return sessionTranscript(home, id) })
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "no requests recorded in the last %s (log: %s)\n", *since, tildePath(home, requestsPath(home)))
		return nil
	}
	showBlame(out, rows, *by)
	return nil
}

func showBlame(out io.Writer, rows []blameRow, by string) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	numbers := "REQ\tINPUT\tCACHE W\tCACHE R\tOUTPUT\tWEIGHTED\t5H\t7D"
	if by == "session" {
		fmt.Fprintln(tw, "SESSION\tPROJECT\tTITLE\t"+numbers)
	} else {
		fmt.Fprintln(tw, strings.ToUpper(by)+"\t"+numbers)
	}
	rebuilt := false
	for _, row := range rows {
		key := row.Key
		if by == "session" && len(key) > 8 {
			key = key[:8]
		}
		if row.Backfilled > 0 {
			key += "*"
			rebuilt = true
		}
		if by == "session" {
			key += "\t" + cmp.Or(row.Project, "?") + "\t" + cmp.Or(row.Title, "?")
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", key, row.Requests,
			humanize(float64(row.InputTokens)), humanize(float64(row.CacheCreationInputTokens)),
			humanize(float64(row.CacheReadInputTokens)), humanize(float64(row.OutputTokens)),
			humanize(row.Weighted), points(row.FiveHourRise), points(row.SevenDayRise))
	}
	tw.Flush()
	if rebuilt {
		fmt.Fprintln(out, "* rebuilt from transcripts: account inferred from the switch log, no 5h/7d")
	}
}
