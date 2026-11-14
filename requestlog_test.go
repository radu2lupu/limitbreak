package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-opus-4","usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello, usage"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40}}` + "\r\n\r\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{},"usage":{"input_tokens":12,"output_tokens":77}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// chunked reads its data n bytes at a time, like a reply arriving in pieces.
type chunked struct {
	data []byte
	n    int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	k := min(c.n, len(c.data), len(p))
	copy(p, c.data[:k])
	c.data = c.data[k:]
	return k, nil
}

func scanAll(t *testing.T, data string, chunk int, sse bool) (string, usageTally) {
	t.Helper()
	var got []usageTally
	s := newUsageScanner(io.NopCloser(&chunked{data: []byte(data), n: chunk}), sse, func(u usageTally) { got = append(got, u) })
	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if len(got) != 1 {
		t.Fatalf("done ran %d times", len(got))
	}
	return string(out), got[0]
}

func TestUsageScannerReadsTheSameUsageWhateverTheReadBoundaries(t *testing.T) {
	want := usageTally{model: "claude-opus-4", usage: tokenUsage{InputTokens: 12, CacheCreationInputTokens: 300, CacheReadInputTokens: 4000, OutputTokens: 77}}
	for _, chunk := range []int{1, 7, 64, len(testStream)} {
		out, got := scanAll(t, testStream, chunk, true)
		if out != testStream {
			t.Fatalf("chunk %d: bytes out differ from bytes in", chunk)
		}
		if got != want {
			t.Fatalf("chunk %d: %+v", chunk, got)
		}
	}
}

func TestUsageScannerTakesTheLastDeltasOutputCount(t *testing.T) {
	stream := strings.Replace(testStream, `"output_tokens":77`, `"output_tokens":0`, 1)
	if _, got := scanAll(t, stream, 5, true); got.usage.OutputTokens != 40 {
		t.Fatal("a zero count does not override", got)
	}
}

func TestUsageScannerParsesAJSONReplyAtTheEnd(t *testing.T) {
	reply := `{"id":"msg_1","type":"message","model":"claude-haiku-4","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":9,"output_tokens":3}}`
	out, got := scanAll(t, reply, 3, false)
	if out != reply || got.model != "claude-haiku-4" || got.usage != (tokenUsage{InputTokens: 5, CacheReadInputTokens: 9, OutputTokens: 3}) {
		t.Fatal(got)
	}
	if _, got := scanAll(t, `{"type":"error","error":{"type":"rate_limit_error"}}`, 3, false); got != (usageTally{}) {
		t.Fatal(got)
	}
}

func TestUsageScannerIgnoresAnOverlongLineAndKeepsGoing(t *testing.T) {
	long := `data: {"type":"message_delta","usage":{"output_tokens":999},"pad":"` + strings.Repeat("x", maxScannedLine) + "\"}\n\n"
	stream := strings.Replace(testStream, "event: message_stop\n", long+"event: message_stop\n", 1)
	out, got := scanAll(t, stream, 4096, true)
	if out != stream {
		t.Fatal("bytes out differ")
	}
	if got.usage.OutputTokens != 77 {
		t.Fatal("the overlong line was read", got)
	}
}

func TestUsageScannerReportsWhatWasSeenWhenClosedEarly(t *testing.T) {
	var got []usageTally
	s := newUsageScanner(io.NopCloser(strings.NewReader(testStream)), true, func(u usageTally) { got = append(got, u) })
	buf := make([]byte, strings.Index(testStream, "event: content_block_delta"))
	io.ReadFull(s, buf)
	s.Close()
	s.Close()
	if len(got) != 1 || got[0].usage.InputTokens != 12 || got[0].usage.OutputTokens != 1 {
		t.Fatal(got)
	}
}

func TestAgentOfTellsSDKFromCLI(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.1.325 (external, sdk-ts, agent-sdk/0.3.276)": "sdk",
		"claude-cli/2.1.325 (external, cli)":                       "cli",
		"curl/8.7.1":                                               "curl",
		"":                                                         "",
	}
	for ua, want := range cases {
		if got := agentOf(ua); got != want {
			t.Fatal(ua, got)
		}
	}
}

func TestRequestFactsReadsModelAndSessionFromTheBody(t *testing.T) {
	model, session := requestFacts([]byte(`{"model":"claude-opus-4","metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"abc-123\"}"}}`))
	if model != "claude-opus-4" || session != "abc-123" {
		t.Fatal(model, session)
	}
	if _, session := requestFacts([]byte(`{"metadata":{"user_id":"user_x_account_y_session_def-456"}}`)); session != "def-456" {
		t.Fatal(session)
	}
}

func readLog(t *testing.T, home string) []requestRecord {
	t.Helper()
	records, err := readRequests(home)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestTheProxyLogsEachRepliedRequestWithItsSessionAccountTokensAndUtilization(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.37")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.12")
		f := w.(http.Flusher)
		for _, part := range strings.SplitAfter(testStream, "\n\n") {
			w.Write([]byte(part))
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	home := makeHome(t, map[string]string{"default": "t-default"})
	acc := newClaudeAccounts(home, credentialsFile, "")
	acc.claude = ""
	px := newProxy(newPool(acc, up.URL))
	px.requests = newRequestLog(home)
	server := httptest.NewServer(px)
	t.Cleanup(server.Close)

	req, _ := http.NewRequest("POST", server.URL+"/_c/agent_a/v1/messages?beta=true", strings.NewReader(`{"model":"asked-model"}`))
	req.Header.Set("Authorization", "Bearer claude-own-login")
	req.Header.Set(sessionHeader, "5e55-1d")
	req.Header.Set("User-Agent", "claude-cli/2.1.325 (external, sdk-ts, agent-sdk/0.3.276)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != testStream {
		t.Fatal("the client got a different stream", string(body))
	}
	// A request no account serves is the client's own login's, logged as `own`; a non-inference one is not logged.
	post(t, server.URL, "/api/oauth/profile")
	acc2 := newClaudeAccounts(t.TempDir(), credentialsFile, "")
	px.pools[0] = newPool(acc2, up.URL)
	post(t, server.URL, "/v1/messages")

	records := readLog(t, home)
	if len(records) != 2 {
		t.Fatal(records)
	}
	r := records[0]
	if r.Account != "default" || r.Client != "agent_a" || r.Session != "5e55-1d" || r.Agent != "sdk" || r.Provider != "claude" ||
		r.Model != "claude-opus-4" || r.Status != 200 || r.tokenUsage != (tokenUsage{12, 300, 4000, 77}) ||
		r.FiveHour == nil || *r.FiveHour != 0.37 || r.SevenDay == nil || *r.SevenDay != 0.12 || time.Since(r.Time) > time.Minute {
		t.Fatalf("%+v", r)
	}
	if own := records[1]; own.Account != ownAccount || own.Client != sharedClient || own.OutputTokens != 77 {
		t.Fatalf("%+v", own)
	}
	raw, _ := os.ReadFile(requestsPath(home))
	if bytes.Contains(raw, []byte("t-default")) || bytes.Contains(raw, []byte("claude-own-login")) || bytes.Contains(raw, []byte("hello")) {
		t.Fatal("a token or reply text in the log", string(raw))
	}
	if info, _ := os.Stat(requestsPath(home)); info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

func TestASpentReplyThatWasRetriedIsNotLoggedButTheOneThatAnsweredIs(t *testing.T) {
	up := newUpstream(t, "t-default")
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	server, _ := start(t, home, up)
	server.Config.Handler.(*proxy).requests = newRequestLog(home)
	post(t, server.URL, "/v1/messages")
	records := readLog(t, home)
	if len(records) != 1 || records[0].Account != "work" || records[0].Status != 200 {
		t.Fatalf("%+v", records)
	}
}

func TestTheLogMovesAsideOnceItWouldPassItsSize(t *testing.T) {
	home := t.TempDir()
	l := newRequestLog(home)
	l.maxBytes = 300
	for i := 0; i < 5; i++ {
		l.append(requestRecord{Time: time.Unix(int64(1_800_000_000+i), 0).UTC(), Provider: "claude", Account: "a", Session: strings.Repeat("s", 40)})
	}
	count := func(path string) int {
		f, err := os.Open(path)
		if err != nil {
			return 0
		}
		defer f.Close()
		n := 0
		for s := bufio.NewScanner(f); s.Scan(); n++ {
		}
		return n
	}
	main, old := count(requestsPath(home)), count(requestsPath(home)+".1")
	if main+old > 5 || main == 0 || old == 0 {
		t.Fatal(main, old)
	}
	if info, _ := os.Stat(requestsPath(home)); info.Size() > 300 {
		t.Fatal(info.Size())
	}
	// Reading takes the rotated file first, so records stay in order.
	records := readLog(t, home)
	if len(records) != main+old || !records[len(records)-1].Time.Equal(time.Unix(1_800_000_004, 0)) {
		t.Fatal(records)
	}
}

func TestASpentReplyHandedBackToTheClientIsLoggedWithNoTokens(t *testing.T) {
	up := newUpstream(t, "t-default", "t-work")
	home := makeHome(t, map[string]string{"default": "t-default", "work": "t-work"})
	server, _ := start(t, home, up)
	server.Config.Handler.(*proxy).requests = newRequestLog(home)
	if status, _ := post(t, server.URL, "/v1/messages"); status != 429 {
		t.Fatal(status)
	}
	records := readLog(t, home)
	if len(records) != 1 || records[0].Status != 429 || records[0].tokenUsage != (tokenUsage{}) {
		t.Fatalf("%+v", records)
	}
}
