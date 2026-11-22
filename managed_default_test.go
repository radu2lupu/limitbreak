package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func accountIDs(list []account) string {
	var ids []string
	for _, acc := range list {
		ids = append(ids, acc.ID)
	}
	return strings.Join(ids, ",")
}

func ownDefault(t *testing.T, home, provider, name string) {
	t.Helper()
	if err := writeSyncState(home, map[string]syncEntry{loginPath(provider, name): {Version: 1, Default: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCodexDoesNotServeTheDefaultLogin(t *testing.T) {
	home, _ := codexHome(t)
	a := newCodexAccounts(home, "", "http://127.0.0.1:1")
	if got := accountIDs(a.list()); got != "default,work" {
		t.Fatalf("unmanaged: %s", got)
	}
	a.managed = true
	if got := accountIDs(a.list()); got != "work" {
		t.Fatalf("managed: %s", got)
	}
}

func TestManagedCodexServesAnOwnedDefaultUnderItsCofferName(t *testing.T) {
	home, _ := codexHome(t)
	ownDefault(t, home, "codex", "radu-pool")
	a := newCodexAccounts(home, "", "http://127.0.0.1:1")
	a.managed = true
	list := a.list()
	if accountIDs(list) != "radu-pool,work" || list[0].Dir != filepath.Join(home, ".codex") || list[0].Shared {
		t.Fatalf("%+v", list)
	}
}

func TestManagedCodexSwapsTheClientsLoginOnEveryRequest(t *testing.T) {
	home, workToken := codexHome(t)
	rig := startCodex(t, home, nil)
	rig.codex.managed = true
	resp, _ := sendCodex(t, rig.server.URL, "agent_a")
	seen := rig.up.seen[0]
	if resp.StatusCode != 200 || seen.auth != "Bearer "+workToken || seen.account != "acct-work" {
		t.Fatalf("%d %+v", resp.StatusCode, seen)
	}
}

func TestManagedCodexOwnedDefaultGetsItsOwnTokenFromDisk(t *testing.T) {
	home := t.TempDir()
	token := writeCodexLogin(t, filepath.Join(home, ".codex"), "mine", "acct-mine", time.Now().Add(240*time.Hour))
	ownDefault(t, home, "codex", "radu-pool")
	rig := startCodex(t, home, nil)
	rig.codex.managed = true
	sendCodex(t, rig.server.URL, "agent_a")
	if seen := rig.up.seen[0]; seen.auth != "Bearer "+token || seen.account != "acct-mine" {
		t.Fatalf("%+v", seen)
	}
}

func TestManagedGrokDoesNotServeTheDefaultLogin(t *testing.T) {
	home, _, _ := grokHome(t)
	a := newGrokAccounts(home, "", "http://127.0.0.1:1")
	if got := accountIDs(a.list()); got != "default,work" {
		t.Fatalf("unmanaged: %s", got)
	}
	a.managed = true
	if got := accountIDs(a.list()); got != "work" {
		t.Fatalf("managed: %s", got)
	}
	ownDefault(t, home, "grok", "radu-pool")
	if got := accountIDs(a.list()); got != "radu-pool,work" {
		t.Fatalf("owned: %s", got)
	}
}

func TestManagedGrokSwapsTheClientsLoginOnEveryRequest(t *testing.T) {
	home, own, work := grokHome(t)
	rig := startGrok(t, home, nil)
	rig.grok.managed = true
	sendGrok(t, rig.server.URL, "agent_a", "GET", "/v1/models", own)
	if len(rig.up.seen) == 0 || rig.up.seen[0].auth != "Bearer "+work {
		t.Fatalf("%+v", rig.up.seen)
	}
}
