package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assistantLine is a transcript line for one content block of a reply.
func assistantLine(session, requestID, at, model string, output int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"entrypoint":"sdk-ts","isSidechain":false,"requestId":%q,`+
		`"message":{"id":"msg_%s","model":%q,"type":"message","content":[{"type":"text","text":"\"type\":\"assistant\""}],`+
		`"usage":{"input_tokens":2,"cache_creation_input_tokens":751,"cache_read_input_tokens":1000,"output_tokens":%d,"service_tier":"standard"}}}`,
		at, session, requestID, requestID, model, output)
}

func TestTranscriptReplyCountsARequestOnceAndSkipsSynthetic(t *testing.T) {
	set := replySet{}
	lines := []string{
		assistantLine("s1", "req_a", "2026-11-14T10:00:00.000Z", "claude-opus-5-5", 5),
		assistantLine("s1", "req_a", "2026-11-14T10:00:01.000Z", "claude-opus-5-5", 822), // same reply, later block
		assistantLine("s1", "req_a", "2026-11-14T10:00:02.000Z", "claude-opus-5-5", 5),
		assistantLine("s1", "req_b", "2026-11-14T10:01:00.000Z", "claude-opus-5-5", 10),
		assistantLine("s1", "req_s", "2026-11-14T10:02:00.000Z", "<synthetic>", 0),
		`{"type":"assistant","timestamp":"2026-11-14T10:03:00Z","sessionId":"s1","message":{"id":"msg_nousage","model":"m"}}`,
		`{"type":"user","timestamp":"2026-11-14T10:03:00Z","sessionId":"s1","message":{"content":"\"type\":\"assistant\""}}`,
		`{"type":"assistant","timestamp":"2026-11-14T10:04:00Z","sessionId":"s1","message":{"id":"msg_only","model":"m","usage":{"output_tokens":3}}}`,
	}
	for _, line := range lines {
		if key, rec, ok := transcriptReply([]byte(line)); ok {
			set.add(key, rec)
		}
	}
	if len(set) != 3 {
		t.Fatalf("%+v", set)
	}
	a := set["req_a"]
	if a.OutputTokens != 822 || a.CacheReadInputTokens != 1000 || a.Session != "s1" || a.Agent != "sdk" || !a.Backfilled ||
		a.Status != 200 || a.Provider != "claude" || a.Model != "claude-opus-5-5" || a.FiveHour != nil {
		t.Fatalf("%+v", a)
	}
	if _, ok := set["msg_only"]; !ok {
		t.Fatal("a reply without a request id is keyed by its message id")
	}
}

func TestTranscriptOriginAttributesSubagentsAndMeshClients(t *testing.T) {
	home := "/Users/x"
	for path, want := range map[string][3]string{
		home + "/.claude/projects/-p/s1.jsonl":                                                     {"shared", "", "s1"},
		home + "/.claude-work/projects/-p/s1/subagents/agent-abc.jsonl":                            {"shared", "", "s1"},
		home + "/" + meshACPDir + "/member_agent_e3/home/revisions/4/.claude/projects/-p/s2.jsonl": {"member_agent_e3", "sdk", "s2"},
	} {
		c, a, s := transcriptOrigin(home, path)
		if [3]string{c, a, s} != want {
			t.Fatal(path, c, a, s)
		}
	}
}

func TestSwitchLogLinesAndAccountAtTime(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	lines := []string{
		"2026/11/01 14:47:08 account work spent until 2026-11-01T18:00:00+01:00 for client shared; now default",
		"2026/11/01 17:21:26 account science spent until 2026-11-01T21:10:00+01:00 for client shared; now none (Claude's own login answers)",
		"2026/11/14 09:46:04 claude account dalma spent until 2026-11-14T12:40:00+01:00 for client member_agent_e3; now science",
		"2026/11/14 10:00:00 codex account a spent until 2026-11-14T12:40:00+01:00 for client shared; now b",
		"2026/11/14 10:00:00 grok account a spent until 2026-11-14T12:40:00+01:00 for client shared; now b",
		"2026/11/14 19:10:26 claude account science spent until 2026-11-16T19:00:00+01:00 for client shared; now work",
		"2026/11/14 19:10:27 limitbreak listening on 127.0.0.1:48617",
		"2026/11/14 20:00:00 client member_agent_e3 set to claude account dalma",
		"2026/11/14 20:00:01 client member_agent_e3 set to grok account x",
		"2026/11/13 19:49:27 client lb_probe set to account work",
	}
	var switches []accountSwitch
	for _, l := range lines {
		if s, ok := parseSwitchLine(l, loc); ok {
			switches = append(switches, s)
		}
	}
	if len(switches) != 6 || switches[1].Account != ownAccount || switches[0].Account != "default" ||
		!switches[0].At.Equal(time.Date(2026, 11, 1, 13, 47, 8, 0, time.UTC)) {
		t.Fatalf("%+v", switches)
	}
	h := newSwitchHistory(switches)
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct{ client, at, want string }{
		{"shared", "2026-11-01T13:47:07Z", "?"},       // a second before the first switch (14:47:08 CET)
		{"shared", "2026-11-01T13:47:08Z", "default"}, // at it
		{"shared", "2026-11-05T00:00:00Z", "own"},
		{"shared", "2026-11-14T18:10:26Z", "work"},       // 19:10:26 CET
		{"shared", "2026-11-14T09:00:00Z", "own"},        // the codex line changed nothing
		{"member_agent_e3", "2026-11-14T08:00:00Z", "?"}, // 09:00 CET: before its first switch
		{"member_agent_e3", "2026-11-14T09:00:00Z", "science"},
		{"member_agent_e3", "2026-11-14T19:30:00Z", "dalma"}, // set at 20:00 CET; the grok line changed nothing
		{"lb_probe", "2026-11-13T19:00:00Z", "work"},
		{"member_other", "2026-11-14T09:00:00Z", "?"},
	} {
		if got := h.accountAt(c.client, at(c.at)); got != c.want {
			t.Fatal(c, got)
		}
	}
}

func TestBackfillStart(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 14, 23, 30, 0, 0, loc)
	for in, want := range map[string]time.Time{
		"":           time.Date(2026, 11, 14, 0, 0, 0, 0, loc),
		"2026-11-10": time.Date(2026, 11, 10, 0, 0, 0, 0, loc),
		"24h":        now.Add(-24 * time.Hour),
		"7d":         now.Add(-7 * 24 * time.Hour),
	} {
		if got, err := backfillStart(in, now); err != nil || !got.Equal(want) {
			t.Fatal(in, got, err)
		}
	}
	if _, err := backfillStart("yesterday", now); err == nil {
		t.Fatal("bad --since accepted")
	}
}

func TestEachLineSkipsOverlongLines(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	var got []string
	if err := eachLine(strings.NewReader("a\r\n"+long+"\nb\n\nc"), 100_000, func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatal(got)
	}
}

func readBackfill(t *testing.T, home string) []requestRecord {
	t.Helper()
	raw, err := os.ReadFile(requestsBackfillPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var out []requestRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var rec requestRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func TestBackfillCommandStopsAtTheFirstLiveRecordAndRewritesIdempotently(t *testing.T) {
	home := t.TempDir()
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 14, 23, 30, 0, 0, loc)
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-p", "s1.jsonl"),
		assistantLine("s1", "req_yesterday", "2026-11-13T22:00:00.000Z", "m", 1), // 23:00 CET the day before
		assistantLine("s1", "req_1", "2026-11-14T09:00:00.000Z", "m", 1),
		assistantLine("s1", "req_1", "2026-11-14T09:00:00.000Z", "m", 7),
		assistantLine("s1", "req_late", "2026-11-14T21:30:00.000Z", "m", 1)) // after the first live record
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-p", "s1", "subagents", "agent-a.jsonl"),
		assistantLine("s1", "req_sub", "2026-11-14T10:00:00.000Z", "m", 2))
	writeTranscript(t, filepath.Join(home, meshACPDir, "member_agent_e3", "home", "revisions", "2", ".claude", "projects", "-q", "s2.jsonl"),
		assistantLine("s2", "req_mesh", "2026-11-14T11:00:00.000Z", "m", 3))
	writeTranscript(t, switchLogPath(home),
		"2026/11/14 09:30:00 claude account a spent until 2026-11-14T12:40:00+01:00 for client shared; now work")
	newRequestLog(home).append(requestRecord{Time: time.Date(2026, 11, 14, 21, 20, 0, 0, time.UTC), Provider: "claude",
		Client: "shared", Account: "work", Session: "s1"})

	var out bytes.Buffer
	if err := backfillCommand(home, []string{"--dry-run"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dry run: 3 replies, 2 sessions") || !strings.Contains(out.String(), "1 with an unknown account") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(requestsBackfillPath(home)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote", err)
	}
	for range 2 {
		out.Reset()
		if err := backfillCommand(home, nil, now, &out); err != nil {
			t.Fatal(err)
		}
		got := readBackfill(t, home)
		if len(got) != 3 {
			t.Fatalf("%s %+v", out.String(), got)
		}
		// 09:00Z is 10:00 CET, after shared's first switch (09:30 CET); the mesh client never switched.
		if got[0].Session != "s1" || got[0].OutputTokens != 7 || got[0].Account != "work" || got[0].Client != "shared" ||
			got[1].Session != "s1" || got[1].Account != "work" ||
			got[2].Session != "s2" || got[2].Client != "member_agent_e3" || got[2].Account != "?" || got[2].Agent != "sdk" {
			t.Fatalf("%+v", got)
		}
	}
	info, err := os.Stat(requestsBackfillPath(home))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode(), err)
	}
	// The live log is untouched, and blame reads both: the backfilled rows are marked.
	records, err := readRequests(home)
	if err != nil || len(records) != 4 || !earliestLive(records).Equal(time.Date(2026, 11, 14, 21, 20, 0, 0, time.UTC)) {
		t.Fatal(len(records), err)
	}
}

func TestBackfillWithoutLiveRecordsHasNoUpperCutoff(t *testing.T) {
	set := replySet{"a": {Time: time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC), Client: "shared", Backfilled: true}}
	got := backfillRecords(set, switchHistory{}, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), earliestLive(nil), nil)
	if len(got) != 1 || got[0].Account != "?" {
		t.Fatalf("%+v", got)
	}
}

func TestBlameMarksBackfilledRowsAndTheirRecordsDoNotMoveUtilization(t *testing.T) {
	filled := func(minutesAgo int, account, session string) requestRecord {
		r := rec(minutesAgo, account, session, nil, nil, tokenUsage{OutputTokens: 100})
		r.Backfilled = true
		return r
	}
	records := []requestRecord{
		rec(50, "work", "live", util(0.10), nil, tokenUsage{OutputTokens: 1}),
		filled(45, "work", "old"),
		filled(40, "?", "old"),
		rec(30, "work", "live", util(0.14), nil, tokenUsage{OutputTokens: 1}), // +4, all live's
		filled(20, "work", "mixed"),
		rec(10, "work", "mixed", util(0.15), nil, tokenUsage{OutputTokens: 1}), // +1
	}
	rows := blame(records, blameOptions{since: 24 * time.Hour, by: "session"}, blameNow, noTranscripts)
	got := map[string]blameRow{}
	for _, r := range rows {
		got[r.Key] = r
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if got["old"].Backfilled != 2 || got["old"].FiveHourRise != 0 || got["live"].Backfilled != 0 || !near(got["live"].FiveHourRise, 4) ||
		got["mixed"].Backfilled != 1 || !near(got["mixed"].FiveHourRise, 1) {
		t.Fatalf("%+v", got)
	}
	var out bytes.Buffer
	showBlame(&out, rows, "session")
	text := out.String()
	if !strings.Contains(text, "old*") || !strings.Contains(text, "mixed*") || strings.Contains(text, "live*") ||
		!strings.Contains(text, "* rebuilt from transcripts: account inferred from the switch log, no 5h/7d") {
		t.Fatal(text)
	}
	out.Reset()
	showBlame(&out, rows[:0], "session")
	if strings.Contains(out.String(), "rebuilt") {
		t.Fatal(out.String())
	}
	raw, _ := json.Marshal(got["old"])
	if !strings.Contains(string(raw), `"backfilled":2`) {
		t.Fatal(string(raw))
	}
}

func TestBackfillKeepsLaterRepliesOfSessionsTheProxyNeverSaw(t *testing.T) {
	cutoff := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)
	set := replySet{
		"early":  {Time: cutoff.Add(-time.Hour), Client: "shared", Session: "rc"},
		"rc":     {Time: cutoff.Add(time.Hour), Client: "shared", Session: "rc"},
		"logged": {Time: cutoff.Add(time.Hour), Client: "shared", Session: "proxied"},
		"none":   {Time: cutoff.Add(time.Hour), Client: "shared"},
	}
	got := backfillRecords(set, switchHistory{}, cutoff.Add(-24*time.Hour), cutoff, map[string]bool{"proxied": true})
	if len(got) != 2 || got[0].Account != "?" || got[1].Session != "rc" || got[1].Account != directAccount {
		t.Fatalf("%+v", got)
	}
}
