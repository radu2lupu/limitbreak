package main

// The request log answers "which session used the limits": one JSON line per Claude inference reply,
// with the session that asked, the account that answered (`own` for the client's own login), the
// tokens the reply counted and the utilization its headers stated. It holds no prompt text, no
// token and no header but the utilization ones.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// sessionHeader names the Claude Code session a request belongs to (and its transcript file).
	sessionHeader = "X-Claude-Code-Session-Id"
	// ownAccount is the account of a request that went on the client's own login.
	ownAccount = "own"
	// maxScannedLine caps what the usage scanner buffers: one SSE line, or the head of a JSON reply.
	// Usage sits in short event lines; a longer line is passed on and not read.
	maxScannedLine = 1 << 20
	// requestLogMaxBytes is the size past which the log moves to requests.jsonl.1 (replacing an older
	// one), so the two together stay near twice this.
	requestLogMaxBytes = 20 << 20
)

// tokenUsage is Anthropic's `usage` object, the same shape in a JSON reply, in a stream's
// message_start and in its message_delta events.
type tokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// requestRecord is one line of the request log.
type requestRecord struct {
	// Time is when the reply's headers arrived: when its utilization was read.
	Time     time.Time `json:"time"`
	Provider string    `json:"provider"`
	Client   string    `json:"client"`
	Account  string    `json:"account"`
	Session  string    `json:"session,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Model    string    `json:"model,omitempty"`
	Status   int       `json:"status"`
	tokenUsage
	// The reply's 5-hour and 7-day utilization (0..1); absent when its headers did not say.
	FiveHour *float64 `json:"five_hour_utilization,omitempty"`
	SevenDay *float64 `json:"seven_day_utilization,omitempty"`
}

func requestsPath(home string) string {
	return filepath.Join(filepath.Dir(usagePath(home)), "requests.jsonl")
}

// MARK: Decisions (pure)

// agentOf is the kind of client a User-Agent names: `sdk` for an Agent SDK session, `cli` for the
// terminal app, else the product token (`curl`).
func agentOf(userAgent string) string {
	switch {
	case strings.Contains(userAgent, "sdk-"):
		return "sdk"
	case strings.Contains(userAgent, "cli)") || strings.Contains(userAgent, ", cli"):
		return "cli"
	}
	product, _, _ := strings.Cut(userAgent, " ")
	product, _, _ = strings.Cut(product, "/")
	return product
}

// requestFacts are the model and session a request body states; the session is in
// `metadata.user_id`, a JSON string holding `session_id` (older clients: `…_session_<id>`).
func requestFacts(body []byte) (model, session string) {
	var req struct {
		Model    string `json:"model"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", ""
	}
	var id struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(req.Metadata.UserID), &id) == nil && id.SessionID != "" {
		return req.Model, id.SessionID
	}
	if _, after, ok := strings.Cut(req.Metadata.UserID, "_session_"); ok {
		return req.Model, after
	}
	return req.Model, ""
}

// usageTally is what a reply has said so far about its model and tokens.
type usageTally struct {
	model string
	usage tokenUsage
}

// apply takes in one JSON document: a stream event (message_start carries `message.usage`,
// message_delta a top-level `usage` whose output count is cumulative) or a whole JSON reply. The
// latest non-zero value of each count wins, so the final delta's output count is the one kept.
func (t *usageTally) apply(raw []byte) {
	var doc struct {
		Model   string      `json:"model"`
		Usage   *tokenUsage `json:"usage"`
		Message *struct {
			Model string      `json:"model"`
			Usage *tokenUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return
	}
	t.merge(doc.Model, doc.Usage)
	if doc.Message != nil {
		t.merge(doc.Message.Model, doc.Message.Usage)
	}
}

func (t *usageTally) merge(model string, u *tokenUsage) {
	if model != "" {
		t.model = model
	}
	if u == nil {
		return
	}
	for _, f := range []struct {
		to   *int64
		from int64
	}{
		{&t.usage.InputTokens, u.InputTokens},
		{&t.usage.CacheCreationInputTokens, u.CacheCreationInputTokens},
		{&t.usage.CacheReadInputTokens, u.CacheReadInputTokens},
		{&t.usage.OutputTokens, u.OutputTokens},
	} {
		if f.from != 0 {
			*f.to = f.from
		}
	}
}

// MARK: Scanning a reply as it passes

// usageScanner is a reply body that reads usage out of the bytes going by and changes nothing:
// each Read hands back exactly what the reply's Read gave, when it gave it. An event stream is read
// line by line (its `data:` lines); any other reply keeps its first maxScannedLine bytes and is
// parsed once it ends. done runs once, when the body ends or is closed (a client that hung up gets
// what was seen).
type usageScanner struct {
	body     io.ReadCloser
	sse      bool
	line     []byte
	overlong bool
	head     []byte
	tally    usageTally
	once     sync.Once
	done     func(usageTally)
}

func newUsageScanner(body io.ReadCloser, sse bool, done func(usageTally)) *usageScanner {
	return &usageScanner{body: body, sse: sse, done: done}
}

func (s *usageScanner) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	s.scan(p[:n])
	// Bytes that came with the end are delivered first; the record waits for the next Read or Close.
	if err == io.EOF && n == 0 {
		s.finish()
	}
	return n, err
}

func (s *usageScanner) Close() error {
	err := s.body.Close()
	s.finish()
	return err
}

func (s *usageScanner) finish() {
	s.once.Do(func() {
		if s.sse {
			if !s.overlong && len(s.line) > 0 {
				s.event(s.line)
			}
		} else {
			s.tally.apply(s.head)
		}
		s.done(s.tally)
	})
}

func (s *usageScanner) scan(b []byte) {
	if !s.sse {
		if room := maxScannedLine - len(s.head); room > 0 {
			s.head = append(s.head, b[:min(room, len(b))]...)
		}
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		chunk := b
		if i >= 0 {
			chunk = b[:i]
		}
		if !s.overlong {
			if len(s.line)+len(chunk) > maxScannedLine {
				s.overlong, s.line = true, s.line[:0]
			} else {
				s.line = append(s.line, chunk...)
			}
		}
		if i < 0 {
			return
		}
		if !s.overlong {
			s.event(s.line)
		}
		s.line, s.overlong = s.line[:0], false
		b = b[i+1:]
	}
}

// event reads one complete SSE line; only `data:` lines that mention usage are parsed.
func (s *usageScanner) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	s.tally.apply(bytes.TrimSpace(data))
}

func isEventStream(h http.Header) bool {
	media, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	return media == "text/event-stream"
}

// MARK: Recording

// requestMeta is what is known about a logged request before its reply: who sent it. Nil for a
// request that is not logged (not inference, or not the Claude provider, whose replies carry
// Anthropic's usage).
type requestMeta struct {
	log                              *requestLog
	provider, client, session, agent string
	// body is the request, read for its model or session only when the reply and headers did not say.
	body []byte
	now  func() time.Time
}

func (p *proxy) requestMeta(pl *pool, client, path string, r *http.Request, body []byte) *requestMeta {
	if p.requests == nil || !pl.provider.spends(path) {
		return nil
	}
	if _, ok := pl.provider.(*claudeAccounts); !ok {
		return nil
	}
	return &requestMeta{log: p.requests, provider: pl.provider.name(), client: client,
		session: r.Header.Get(sessionHeader), agent: agentOf(r.UserAgent()), body: body, now: pl.now}
}

// watch makes resp's body record the request once it has been relayed. A nil meta does nothing.
func (m *requestMeta) watch(resp *http.Response, accountID string) {
	if m == nil {
		return
	}
	rec := requestRecord{Time: m.now().UTC().Truncate(time.Millisecond), Provider: m.provider, Client: m.client,
		Account: accountID, Session: m.session, Agent: m.agent, Status: resp.StatusCode}
	if v, ok := usageHeader(resp.Header, "five_hour_utilization"); ok {
		rec.FiveHour = &v
	}
	if v, ok := usageHeader(resp.Header, "seven_day_utilization"); ok {
		rec.SevenDay = &v
	}
	resp.Body = newUsageScanner(resp.Body, isEventStream(resp.Header), func(t usageTally) {
		rec.Model, rec.tokenUsage = t.model, t.usage
		if rec.Model == "" || rec.Session == "" {
			model, session := requestFacts(m.body)
			if rec.Model == "" {
				rec.Model = model
			}
			if rec.Session == "" {
				rec.Session = session
			}
		}
		m.log.append(rec)
	})
}

// requestLog appends records to requests.jsonl, one writer at a time. A failure is logged and never
// reaches the request.
type requestLog struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
}

func newRequestLog(home string) *requestLog {
	return &requestLog{path: requestsPath(home), maxBytes: requestLogMaxBytes}
}

func (l *requestLog) append(rec requestRecord) {
	raw, err := json.Marshal(rec)
	if err != nil {
		log.Printf("request log encode: %v", err)
		return
	}
	raw = append(raw, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err = os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
		log.Printf("request log directory: %v", err)
		return
	}
	if info, err := os.Stat(l.path); err == nil && info.Size() > 0 && info.Size()+int64(len(raw)) > l.maxBytes {
		if err = os.Rename(l.path, l.path+".1"); err != nil {
			log.Printf("request log rotate: %v", err)
		}
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("request log open: %v", err)
		return
	}
	if _, err = f.Write(raw); err != nil {
		log.Printf("request log write: %v", err)
	}
	if err = f.Close(); err != nil {
		log.Printf("request log close: %v", err)
	}
}
