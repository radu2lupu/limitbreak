package main

// Coffer is the one shared store of provider logins (see ROADMAP/logins-from-coffer.md). This file is
// the one owner of how Limit Break talks to it: where its address and token come from, and the three
// requests it needs (get, put with a compare-and-swap on the version, list). It is a small client of
// its own rather than an import of the Coffer module, which is go 1.25 with SQLite and `internal/`
// packages, while Limit Break is go 1.22, has no dependencies and is built with CGO off inside the app.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cofferAddrEnv       = "LIMITBREAK_COFFER_ADDR"
	cofferTokenFileEnv  = "LIMITBREAK_COFFER_TOKEN_FILE"
	cofferVersionHeader = "X-Coffer-Version"
	// cofferMaxValue is the most a login read from Coffer may be; a Grok or Codex auth.json is a few KB.
	cofferMaxValue = 256 * 1024
)

var (
	errCofferNotFound = errors.New("not in Coffer")
	errCofferConflict = errors.New("Coffer holds a different version than expected")
)

// cofferError is a request Coffer answered with an error status. It never holds a value or a token.
type cofferError struct {
	Status  int
	Message string
}

func (e *cofferError) Error() string { return fmt.Sprintf("coffer: %s (HTTP %d)", e.Message, e.Status) }

// cofferStore is what the login sync needs of Coffer; tests run it against a fake Coffer server.
type cofferStore interface {
	// get is the latest value of path and its version; errCofferNotFound when there is none.
	get(path string) ([]byte, int64, error)
	// putIf writes value only when the latest version of path is exactly expect (0: never written);
	// errCofferConflict when it is not. It returns the new version.
	putIf(path string, value []byte, expect int64) (int64, error)
	// list is the paths matching pattern the caller may list.
	list(pattern string) ([]string, error)
}

type cofferClient struct {
	base  string
	token string
	hc    *http.Client
}

func newCofferClient(addr, token string) (*cofferClient, error) {
	addr = strings.TrimRight(strings.TrimSpace(addr), "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return nil, fmt.Errorf("coffer address %q: want http://host:port", addr)
	}
	if token == "" {
		return nil, errors.New("no coffer token")
	}
	return &cofferClient{base: addr, token: token, hc: &http.Client{Timeout: 15 * time.Second}}, nil
}

func cofferURL(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/v1/secrets/" + strings.Join(segs, "/")
}

func (c *cofferClient) do(method, p string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+p, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		// The URL has no secret in it, but the error text of a transport failure is not ours to forward
		// verbatim into logs that outlive this process: say where and why in a few words.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("coffer unreachable at %s: %w", c.base, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
	if e.Error == "" {
		e.Error = http.StatusText(resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w (%s)", errCofferNotFound, e.Error)
	case http.StatusConflict:
		return nil, errCofferConflict
	}
	return nil, &cofferError{Status: resp.StatusCode, Message: e.Error}
}

func (c *cofferClient) get(path string) ([]byte, int64, error) {
	resp, err := c.do(http.MethodGet, cofferURL(path), nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	val, err := io.ReadAll(io.LimitReader(resp.Body, cofferMaxValue+1))
	if err != nil {
		return nil, 0, err
	}
	if len(val) > cofferMaxValue {
		return nil, 0, fmt.Errorf("coffer: %s is larger than %d bytes", path, cofferMaxValue)
	}
	version, _ := strconv.ParseInt(resp.Header.Get(cofferVersionHeader), 10, 64)
	return val, version, nil
}

func (c *cofferClient) putIf(path string, value []byte, expect int64) (int64, error) {
	if expect < 0 {
		return 0, errors.New("coffer: expected version must be 0 or more")
	}
	resp, err := c.do(http.MethodPut, cofferURL(path)+"?if-version="+strconv.FormatInt(expect, 10), value)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var v struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v); err != nil {
		return 0, fmt.Errorf("coffer: bad response: %w", err)
	}
	return v.Version, nil
}

func (c *cofferClient) list(pattern string) ([]string, error) {
	resp, err := c.do(http.MethodGet, "/v1/list?pattern="+url.QueryEscape(pattern), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&v); err != nil {
		return nil, fmt.Errorf("coffer: bad response: %w", err)
	}
	return v.Paths, nil
}

// MARK: Configuration

// cofferConfigFile is where a Mac says which Coffer to use when its environment cannot (a LaunchAgent).
func cofferConfigFile(home string) string {
	return filepath.Join(home, ".config", "limitbreak", "coffer")
}

// cofferAddress is the Coffer to use: $LIMITBREAK_COFFER_ADDR, else the config file; empty when neither.
func cofferAddress(home string) string {
	if v := strings.TrimSpace(os.Getenv(cofferAddrEnv)); v != "" {
		return v
	}
	raw, _ := os.ReadFile(cofferConfigFile(home))
	return strings.TrimSpace(string(raw))
}

// cofferTokenFile is where a Mac keeps its Coffer token when its login Keychain is locked to a LaunchAgent
// (a Mac nobody is logged in to, such as the Mini): mode 0600, beside the address file.
func cofferTokenFile(home string) string {
	return filepath.Join(home, ".config", "limitbreak", "coffer-token")
}

// cofferToken is this Mac's Coffer identity token: the file named by $LIMITBREAK_COFFER_TOKEN_FILE, else
// the token file beside the address file. It is never logged.
func cofferToken(home string) string {
	if path := os.Getenv(cofferTokenFileEnv); path != "" {
		raw, _ := os.ReadFile(path)
		return strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile(cofferTokenFile(home)); err == nil {
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token
		}
	}
	return ""
}

// openCoffer is the client for this Mac's Coffer; nil with a reason when it has no address or token
// (sync is then off, which is how a Mac without Coffer behaves).
func openCoffer(home string) (*cofferClient, error) {
	addr := cofferAddress(home)
	if addr == "" {
		return nil, fmt.Errorf("no Coffer address (set %s or write it to %s)", cofferAddrEnv, cofferConfigFile(home))
	}
	token := cofferToken(home)
	if token == "" {
		return nil, fmt.Errorf("no Coffer token (%s, %s)", cofferTokenFileEnv, cofferTokenFile(home))
	}
	return newCofferClient(addr, token)
}
