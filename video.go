package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runVideo shows a switch without touching any real login or the network: two made-up accounts, and
// a stand-in for Anthropic that reports the first account's window as spent.
func runVideo(out io.Writer) error {
	home, err := os.MkdirTemp("", "limitbreak-demo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	far := float64(neverExpires)
	for name, dir := range map[string]string{"ada@example.com": ".claude", "grace@example.com": ".claude-grace"} {
		path := filepath.Join(home, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		claudeJSON := filepath.Join(path, ".claude.json")
		if dir == ".claude" {
			claudeJSON = filepath.Join(home, ".claude.json")
		}
		creds, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "token-of-" + name, "expiresAt": far}})
		doc, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{"emailAddress": name}})
		if err := os.WriteFile(filepath.Join(path, ".credentials.json"), creds, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(claudeJSON, doc, 0o600); err != nil {
			return err
		}
	}
	reset := time.Now().Add(3 * time.Hour).Unix()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := strings.TrimPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer token-of-"), "")
		if who == "ada@example.com" {
			w.Header().Set(spentHeader, "rejected")
			w.Header().Set(resetHeader, fmt.Sprint(reset))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(w, `{"content":"answered on %s"}`, who)
	}))
	defer anthropic.Close()

	proxy := httptest.NewServer(newProxy(newPool(newClaudeAccounts(home, credentialsFile, ""), anthropic.URL)))
	defer proxy.Close()

	log.SetOutput(io.Discard)
	t := newTheme(out)
	t.say(t.dim("╔══════════════════════════════════════════╗"))
	t.say(t.dim("║ ") + t.cyan("BATTLE") + t.dim("  claude vs. the 5-hour window     ║"))
	t.say(t.dim("╚══════════════════════════════════════════╝"))
	t.say("")
	t.say(t.cyan("ADA   ") + "ada@example.com    " + t.gauge(100) + t.red("  LIMIT"))
	t.say(t.cyan("GRACE ") + "grace@example.com  " + t.gauge(0) + t.dim("  ready"))
	for i := 1; i <= 2; i++ {
		t.say("")
		sentAs := "ADA"
		if i == 2 {
			sentAs = "GRACE, now active"
		}
		t.say(t.yellow(fmt.Sprintf("» request %d", i)) + t.dim(" sent to Anthropic as "+sentAs))
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if i == 1 {
			t.say(t.red("  ✖ 429 window spent") + t.dim("  (proxy catches it, claude never sees it)"))
			t.flash("★ L I M I T   B R E A K ! ★")
			t.say(t.dim("  same request replayed as GRACE"))
		}
		t.say(t.green(fmt.Sprintf("  ✔ %d", resp.StatusCode)) + " " + string(body))
	}
	t.say("")
	t.say(t.dim("Claude saw two ordinary 200s. No restart, no lost history."))
	t.say(t.dim("(simulated: no real login or network was used)"))
	return nil
}

// theme draws the demo in colour on a terminal and plainly everywhere else (NO_COLOR, pipes).
type theme struct {
	out   io.Writer
	color bool
	pause bool
}

func newTheme(out io.Writer) theme {
	f, ok := out.(*os.File)
	tty := false
	if ok {
		if info, err := f.Stat(); err == nil {
			tty = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return theme{out: out, color: tty && os.Getenv("NO_COLOR") == "", pause: tty}
}

func (t theme) paint(code, s string) string {
	if !t.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (t theme) cyan(s string) string   { return t.paint("96", s) }
func (t theme) red(s string) string    { return t.paint("91", s) }
func (t theme) green(s string) string  { return t.paint("92", s) }
func (t theme) yellow(s string) string { return t.paint("93", s) }
func (t theme) dim(s string) string    { return t.paint("2", s) }

func (t theme) say(s string) {
	fmt.Fprintln(t.out, s)
	if t.pause {
		time.Sleep(350 * time.Millisecond)
	}
}

// gauge is the limit meter: ten cells, filled in proportion.
func (t theme) gauge(percent int) string {
	filled := percent / 10
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
	colour := "92"
	if percent >= 100 {
		colour = "91"
	} else if percent >= 60 {
		colour = "93"
	}
	return "[" + t.paint(colour, bar) + "]"
}

// flash is the banner: it blinks between two colours on a terminal and prints once elsewhere.
func (t theme) flash(banner string) {
	if !t.pause {
		fmt.Fprintln(t.out, "  "+banner)
		return
	}
	for i := 0; i < 6; i++ {
		colour := "93"
		if i%2 == 1 {
			colour = "95"
		}
		fmt.Fprintf(t.out, "\r  \x1b[1;%sm%s\x1b[0m", colour, banner)
		time.Sleep(140 * time.Millisecond)
	}
	fmt.Fprintln(t.out)
}
