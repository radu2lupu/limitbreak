# Logins from Coffer

Status: implemented and tested on branch `logins-from-coffer` (Limit Break and Coffer), not pushed, not deployed. Admin steps below are the owner's; none has been run.
Decided by Radu, 2026-11-21: "each device should have its own limitbreak running. And they should access the
creds from coffer or somethng."

## Problem

Every Mac runs its own Limit Break and signs in on its own. To use one login on a second Mac the first Mac
pushed a copy over SSH (`share`, and a serve-time loop for Grok). That has three faults:

- Macs push logins to each other, so each needs SSH access to the others and the other must be awake.
- The copy is token-only (Grok) or a 1-year token (Claude); a Mac that holds the real login is a special owner.
- Rotating logins (Grok's refresh token, Codex's) cannot simply be copied: two Macs that each spend the same
  refresh token sign each other out.

## Design

Coffer is the one shared store of provider logins. Each Mac keeps a local copy in the login dir the provider
already uses and keeps working when Coffer is unreachable.

**Paths.** `limitbreak/<provider>/<account>` holds the login (provider `claude`, `codex` or `grok`; account is
a named account, never the default one). `limitbreak/lease/<provider>/<account>` holds the refresh lease.

**Value.** Grok and Codex: the provider's own `auth.json`, whole, refresh token included (Coffer is the place a
refresh token may live). Claude: the 1-year token record (`{token, expiresAt, createdAt}`); a Claude sign-in's
refresh token has a hard end and is never shared. OpenCode keys are static API keys that Limit Break only reads
for usage; they are not synced.

**Local copy and state.** `~/.grok-<name>`, `~/.codex-<name>`, `~/.claude-<name>` as before. The file
`~/.local/state/limitbreak/coffer-sync.json` records, per path, the Coffer version last synced and a
fingerprint of the login synced then (a hash, no secret). A path in that file is a *synced account*; that is
what `GET /_status` reports as `"shared": true` (Grok), so the app keeps reading what it reads now.

**Sync loop** (every 5 minutes in `limitbreak serve`, and `limitbreak sync` once):

1. List `limitbreak/<provider>/*`. Every path is a candidate; the Mac installs one it has no local dir for.
   What a Mac may read is decided by its Coffer rules, not by Limit Break. A dir that is a login of its own
   (not tracked) is never overwritten.
2. For each synced account compare three things: Coffer's version, the version last synced, and the local
   fingerprint.
   - Coffer moved, local did not: install Coffer's login (written to a temp file and renamed).
   - Local moved (grok refreshed it, or `limitbreak renew`), Coffer did not: write it back with
     `PUT ?if-version=<last synced>`.
   - Both moved: the login whose access token ends later wins; a tie goes to Coffer. A winning local login is
     written back with the version just read.
   - Coffer unreachable: nothing changes; the next tick retries. A local change stays pending.
3. A write that loses the compare-and-swap (409) writes nothing; the next tick reads again.

**Refresh ownership (Grok, Codex).** A refresh token is spent by exactly one Mac at a time:

1. Take the lease: read `limitbreak/lease/...` (version `v`, `{holder, expires}`), and if it is free or
   expired write `{me, now+3min}` with `if-version=v`. Losing the race means someone else holds it; do nothing.
2. Sync the account (so a refresh another Mac already did is picked up). If the login is no longer due, stop.
3. Run the provider's own refresh on the local copy (`grok models`, `codex app-server`), then write the result
   back with `if-version`. Release the lease.

The proxy's request-time refresh and the loop's proactive refresh (Grok under 3 h left, Codex under 24 h) both
go through the same guarded function, so there is one path. No lease, no refresh: with Coffer unreachable a
rotating login is not refreshed (its token is good for hours; the cost is that an account whose token ends
during a long Coffer outage stops until Coffer is back). The lease is a client-clock expiry with a TTL much
larger than clock skew and a refresh (1 min timeout); it is a courtesy that keeps two Macs from trying at once,
the compare-and-swap on the login itself is what stops two Macs from both writing back.

**Coffer change.** A conditional write: `PUT /v1/secrets/<path>?if-version=N`, 409 when the latest version
(a tombstone counts) is not N, `coffer put --if-version`. Coffer branch `logins-from-coffer`, CH-0004.

**Replaced.** `share <host>` / `unshare <host>` over SSH, the serve-time Grok push, `import-grok` and
`import-token`, `grok-shares.json`. `limitbreak share [name...] [--provider P]` now publishes a named local
login to Coffer; `unshare` stops tracking it (the Coffer copy stays; delete it with the admin command below).
`grok-placeholder` stays, and so do `shared` and `tokenEnds` in `/_status`.

**Client.** Limit Break keeps a ~150-line Coffer HTTP client of its own (`coffer.go`) instead of importing the
Coffer module: Limit Break is `go 1.22` with no dependencies and is built with `CGO_ENABLED=0` inside the app's
build script, while Coffer is `go 1.25` with SQLite and its own `internal/` packages. The wire contract is three
requests (get, put with `if-version`, list) and is checked by tests against a fake Coffer that speaks it, and by
one run against a real Coffer built from the branch in a temp dir.

**Configuration (per Mac).** Address: `LIMITBREAK_COFFER_ADDR` or the file `~/.config/limitbreak/coffer`
(e.g. `http://100.119.127.103:8795`). Token: `LIMITBREAK_COFFER_TOKEN_FILE`, else the login Keychain item with
service `coffer.limitbreak`, account `token`. No address or no token: sync is off and Limit Break behaves as a
Mac with no Coffer.

## Failure modes

| What breaks | Result |
| --- | --- |
| Coffer unreachable | Local logins keep working; no rotating refresh; local changes stay pending and are written back later. |
| A Mac crashes after refreshing, before write-back | Its local file holds the new login; the first tick back sees a local change and writes it back (CAS). Others cannot refresh meanwhile only while the lease lasts (3 min); after that a second Mac may spend the old, already-rotated token and fail. Recovery: sign in again on one Mac and `limitbreak share`. |
| Two Macs both write back | One wins the CAS; the other reads again and installs the winner's login. |
| Clock skew above the lease TTL | Two Macs may refresh at once; the second write-back conflicts and the first login wins. |
| Coffer rule missing | 403: that Mac installs and publishes nothing; `limitbreak sync` prints the reason. |
| Account removed in Coffer | The local copy stays and keeps being tracked; nothing is pushed back. |

## Verification

- `go vet ./... && go test ./...` in `limitbreak` and `coffer`.
- Tests with a fake Coffer and two Macs (separate home dirs): publish and install, one refresh across two Macs,
  lease held and expired, Coffer down then back, write-back of a local change, both-changed conflict, crash
  between install and state write, refusal to touch a foreign dir, Claude token install with a rejecting
  validator, and 16 concurrent lease takers with one winner.
- One run of two Limit Break processes against a real Coffer built from the branch, in a temp dir with its own
  identities and a temp port (not the Mini's Coffer, not any Mac's live Limit Break).

## Admin steps (the owner's session; nothing here has been run)

Values go in on stdin from a file or the Keychain, never on a command line. `MINI` runs the commands on the
Mini as `_coffer`; `IP_<mac>` is that Mac's tailnet IP.

1. **Deploy Coffer with the conditional put** (the branch `logins-from-coffer` of `~/Code/coffer`), the way the
   README's "Deploy" section describes, and restart the daemon. Without it every write-back is a 404/405 and
   Limit Break stays read-only.
2. **One identity per Mac** (prints the token once; pipe it straight into that Mac's Keychain):

   ```sh
   ssh mini
   sudo -n -u _coffer /usr/local/bin/coffer identity add --dir /var/db/coffer limitbreak@silicon-baby
   sudo -n -u _coffer /usr/local/bin/coffer identity add --dir /var/db/coffer limitbreak@mini
   sudo -n -u _coffer /usr/local/bin/coffer identity add --dir /var/db/coffer limitbreak@dalma
   ```

   Store each token on its own Mac, from stdin:
   `security add-generic-password -U -s coffer.limitbreak -a token -w "$(cat)"` (paste, Ctrl-D), then
   `mkdir -p ~/.config/limitbreak && printf 'http://100.119.127.103:8795' > ~/.config/limitbreak/coffer`.
3. **Rules** (read, write and list on `limitbreak/*`, pinned to the Mac's tailnet IP; a Mac that should not get
   a login gets a narrower pattern, e.g. `limitbreak/grok/*` only):

   ```sh
   for m in silicon-baby mini dalma; do
     sudo -n -u _coffer /usr/local/bin/coffer rule add --dir /var/db/coffer \
       --subject "limitbreak@$m" --pattern 'limitbreak/*' --ops read,write,list --ip "$(eval echo \$IP_$m)"
   done
   ```
4. **First import of each existing login.** A login must be a *named* account (`~/.grok-<name>`,
   `~/.codex-<name>`, `~/.claude-<name>`). The default `~/.grok` and `~/.codex` belong to the tools themselves;
   copying one to a named dir would give two homes the same refresh token, so sign the account in under a name
   (`limitbreak add --provider grok <name>`) on one Mac, then publish it from that Mac with the Mac's own
   identity (no admin write needed):

   ```sh
   limitbreak share --provider grok <name>     # likewise --provider codex, and claude after `limitbreak renew <name>`
   limitbreak sync                             # on each other Mac: installs it as ~/.grok-<name>
   ```

   If the owner prefers an admin write for a login already in a file:
   `ssh mini 'sudo -n -u _coffer /usr/local/bin/coffer put --dir /var/db/coffer limitbreak/grok/<name>' < ~/.grok-<name>/auth.json`
   (the file goes in on stdin; the admin put needs no `--if-version`. `limitbreak share` on a Mac that holds the
   identical login then only tracks it, and `limitbreak sync` on any other Mac installs it).
5. **Delete a login** that should leave Coffer: `rule rm --dir /var/db/coffer <id>` removes a Mac's access to it; the
   secret itself is removed with `DELETE /v1/secrets/limitbreak/<provider>/<account>` through an identity with write
   (there is no `coffer delete` command). A delete is a tombstone version, so a later conditional publish of the same
   path conflicts for ever: put it again with the unconditional admin `coffer put` (step 4's second form), after
   which the Macs track it as any other version.
6. Remove the SSH pushes' leftovers on each Mac when convenient: `~/.local/state/limitbreak/grok-shares.json`
   and any `~/.grok-shared` that was only a pushed copy.
