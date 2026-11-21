# limitbreak

<p align="center"><img src="assets/limitbreak.gif" alt="Arcade-style battle: an account's limit gauge fills, LIMIT BREAK, the next account takes over" width="640"></p>

```
   ★ L I M I T   B R E A K ★
   ADA    [██████████] LIMIT   → spent
   GRACE  [░░░░░░░░░░] ready   → takes the hit
```

Use several Claude accounts from **one** Claude Code install, and switch between them automatically when an account's usage limit is hit, without restarting the session or losing its history.

A small local proxy (a single Go binary, standard library only, a few MB of RAM) sits between `claude` and Anthropic's API. It sends each request on the active account's own login. When Anthropic answers that the account's 5-hour or weekly window is spent, the proxy remembers that account until its reset time and **replays the same request on the next account** before Claude Code ever sees the error. To the session nothing happened.

```
claude ──ANTHROPIC_BASE_URL──▶ proxy 127.0.0.1:48617 ──▶ api.anthropic.com
                                 │  active account's token
                                 └─ 429 "rejected"? mark spent, retry on the next account
```

Try it without touching any real login: `limitbreak video` plays a simulated switch in your terminal as a little arcade battle.

## Quick start

```sh
git clone https://github.com/radu2lupu/limitbreak && cd limitbreak
./install.sh                                   # builds and starts the background service
echo 'source "$HOME/.local/bin/limitbreak-shell.sh"' >> ~/.zshenv  # or ~/.bashrc
limitbreak add                       # sign in another account (browser), repeat per account
```

Open a new terminal and run `claude` as usual. When one account's limit is hit, the next one answers.

It does the same for Codex on ChatGPT sign-ins, see [Codex](#codex), and for Grok Build, see [Grok](#grok).

## What it does and does not touch

- Only `/v1/messages*` requests are sent on an account's token. Everything else (claude.ai connectors, MCP, artifacts, profile) goes through untouched with Claude Code's own credentials, so those keep working.
- It never sets `CLAUDE_CODE_OAUTH_TOKEN` (that makes Claude Code drop its connectors and local MCP servers).
- Accounts are the logins Claude Code already holds: the default one plus every `~/.claude-<name>` config directory that has a login. An account's token is its 1-year token from `limitbreak renew` when it has one, else the sign-in token read from the macOS Keychain item Claude Code keeps per config dir, or from `<dir>/.credentials.json`. Tokens are never logged or exposed by `GET /_status`, and sign-in tokens are never copied.
- An expired token is refreshed by running one tiny `claude -p` under that account's directory, so Claude Code stays the only thing that refreshes a login.
- If no account can serve a request, it passes through with Claude Code's own login: never worse than no proxy.
- Only a window rejection (`429` with `anthropic-ratelimit-unified-status: rejected`) switches accounts. Ordinary rate limits go back to Claude unchanged.
- A running session stays on the account it is on until that account is spent: the prompt cache is per account, and a move re-sends its whole context as a cache write. When the client's active account changes, only new sessions (and sessions idle for over an hour, whose cache has expired) take the new one.
- Every prompt-cache breakpoint in a Claude request is sent with a 1-hour lifetime (`"ttl":"1h"`), so a pause of more than 5 minutes does not re-write the context. Claude Code already does this for a subscription's main conversation, not for subagents and background requests. Nothing else in the request changes.

## Install

Needs Go 1.22+ to build, macOS or Linux. Prebuilt binaries are attached to each [release](../../releases).

```sh
git clone https://github.com/radu2lupu/limitbreak
cd limitbreak
./install.sh          # builds, installs ~/.local/bin/limitbreak, starts a launchd / systemd user service
```

Then add the printed line to `~/.zshenv` (zsh reads it for every shell, including `ssh host claude -p …`) or `~/.bashrc`:

```sh
source "$HOME/.local/bin/limitbreak-shell.sh"
```

That defines a `claude` function that points Claude Code at the proxy when it is running and runs plain `claude` when it is not. Use `./install.sh --binary <prebuilt>` on a host without Go.

## Add accounts

```sh
limitbreak add            # signs in a new account in your browser, names it account-N
limitbreak add work       # or pick the name
limitbreak list           # accounts by provider, who they are, which one is active (*)
limitbreak remove work    # sign it out and delete its login from this machine
```

`add` runs Claude Code's own browser sign-in into its own config directory (`~/.claude-<name>`), checks the login landed, tells you which email it is, and warns if you added the same account twice. The running proxy picks it up within 30 seconds; no restart. The default account is whatever plain `claude auth login` signs in. Raw status for scripts: `curl -s http://127.0.0.1:48617/_status`.

Every switch is logged (`~/Library/Logs/limitbreak.log` on macOS, the journal on Linux): the account that ran out, when it resets, and the one now serving.

`limitbreak list` also shows the latest usage headers observed for each account, with their observation time. The proxy stores these snapshots in `~/.local/state/limitbreak/usage.json`; they survive restarts. Values are only as fresh as the last response for that account and may be absent if Anthropic did not send them. The headers are undocumented and may change.

### Logins that renew themselves

A Claude sign-in ends about 30 days after `claude auth login`, and refreshing does not move that date. To stop signing in every account once a month, run this once:

```sh
limitbreak renew          # every account without a 1-year token (or one ending within 30 days)
limitbreak renew work     # just this one
limitbreak renew --all    # every account, even those with a fresh token
```

For each account it runs Claude Code's `claude setup-token` and you authorize once in the browser. The sign-in page opens with the account's email already filled in (as `claude auth login --email` does). If the browser is already signed in to claude.ai as someone else, that session may win, so check it matches the email `renew` prints. Each account then gets a 1-year token, kept in the Keychain (item `limitbreak-token`) on macOS, or in `<dir>/.limitbreak-token.json` (mode 600) elsewhere and when the Keychain is locked (a Mac reached over SSH). The proxy uses it in place of the sign-in.

To use the same tokens on another machine of yours, keep them in Coffer: `limitbreak share founder` publishes the account's token and `limitbreak sync` (or the running proxy, every 5 minutes) installs it on the other Mac as `~/.claude-founder`, after Anthropic accepts it. See [Logins from Coffer](#logins-from-coffer). Unlike a sign-in, a 1-year token never rotates, so two machines holding it do not log each other out. A same-named dir another tool keeps is left alone.

`limitbreak list` shows when each account's auth ends. The running proxy warns 7 days before any auth ends, in its log and, on macOS, as a notification. If Anthropic rejects an account's token (401), the proxy skips that account for a few minutes and logs `run "limitbreak renew <name>"`; Claude never sees the error.

## Which sessions used the limits

The proxy appends one JSON line per model reply it serves (Claude `/v1/messages`, Codex and Grok model turns) to `~/.local/state/limitbreak/requests.jsonl` (mode 600): the time, client (`/_c/<client>` or `shared`), the account that answered (`own` when it went on Claude Code's own login because no account could take it), the provider, the session id (`X-Claude-Code-Session-Id`, Codex's `session_id`, Grok's `x-grok-conv-id`), the agent (`sdk` or `cli`), model, status, the reply's token counts (input, cache write and how much of it is kept for an hour, cache read, output; Codex and Grok counts are put in the same terms, their cached input as cache read) and the 5-hour and 7-day utilization from its headers. It records no prompt or reply text and no token. A retried spent reply is not logged; the reply the client got is. Past 20 MB the file moves to `requests.jsonl.1` (replacing an older one).

```sh
limitbreak blame                       # last 24h, by session
limitbreak blame --since 7d --by project
limitbreak blame --account own         # what burned your own login
limitbreak blame --by account --json   # also: --by client, --by model; every other grouping shows its models
```

```
SESSION   PROJECT        TITLE                          REQ  INPUT  CACHE W  CACHE R  OUTPUT  WEIGHTED  5H   7D
d7ecca82  ~/Code/app     Gallery scrubber tab labels    212  41k    1.9M     88M      610k    14M       +23  +4
f18dfb1d  ~/Code/other   Auto planning status check     40   6k     300k     9.1M     52k     1.5M      +3   -
```

A session's project and title come from its transcript (`~/.claude*/projects/*/<session>.jsonl`, or a Hi, computer agent's own home): the title it was given, else Claude Code's own title, else its first prompt. `WEIGHTED` counts tokens at the API's price ratios (input 1, cache write 1.25, cache read 0.1, output 5), which roughly track what the limits count. `5H` and `7D` are the percentage points the account's windows rose on that group's replies; a fall is a window reset and counts for no one. Utilization moves in 1% steps and sessions on one account share its window, so these are approximate.

`limitbreak backfill [--since YYYY-MM-DD|24h|7d] [--dry-run]` (default: since local midnight) rebuilds records for the replies before the log began from Claude Code's transcripts (the same dirs, subagent transcripts counted to their parent session), each reply once, and only up to the first record the proxy logged itself, so nothing counts twice. After that cutoff it still keeps the replies of sessions the proxy never logged (Remote Control, which refuses any `ANTHROPIC_BASE_URL`), with the account `direct`; run it with `--since` covering a report's period before the report. It replaces `requests.backfill.jsonl` beside the log, so it can be rerun; `blame` reads it too and marks those rows `*`. Their account is inferred from the switch log (`?` before a client's first logged switch; Hi, computer agents are their own `/_c/<client>`, everything else `shared`). A client can also move without a line of its own and a request that bypassed the proxy looks the same, so treat it as a guess; they have no 5h/7d.

`GET /_usage?since=7d` (default 7d; any `--since` value) is the same for an app: every account's `/_status` entry (Claude, Codex and Grok, each with its windows) plus its weighted total over the period, its five heaviest sessions (title, project, client, weighted, whether rebuilt) and the weighted `rest`, and the totals that went on clients' own logins (`own`) and with no account recorded (`unrecorded`). Codex and Grok accounts get their sessions too (their records are keyed `codex:<id>` and `grok:<id>`, so they never mix with a Claude account of the same id). [OpenCode](#opencode)'s accounts are added after them. A report is reused for 60 seconds. Hi, computer's Usage page is drawn from it.

## Codex

The same proxy rotates Codex between ChatGPT sign-ins. Point Codex at it:

```sh
codex -c openai_base_url="http://127.0.0.1:48617/backend-api/codex"
# or in ~/.codex/config.toml: openai_base_url = "http://127.0.0.1:48617/backend-api/codex"
```

- Accounts are `~/.codex` (the default, Codex's own login) and every `~/.codex-<name>` whose `auth.json` is a ChatGPT sign-in. `limitbreak add --provider codex [name]` runs `codex login` into a new `~/.codex-<name>`; `limitbreak remove --provider codex <name>` deletes that dir.
- On the default account a request keeps Codex's own credentials; limitbreak never refreshes or rewrites `~/.codex`. On another account it swaps `Authorization` and `chatgpt-account-id` for that login's. The body (zstd) and its encoding pass as they are; replies stream back as they arrive.
- A `429` whose body says `usage_limit_reached` marks the account spent until its `resets_at` and the same request goes again on the next account. A reply whose `x-codex-…-used-percent` header reaches 100 marks it spent ahead, so the next request goes elsewhere without being refused first. Any other `429` goes back to Codex unchanged.
- A named login is refreshed by Codex itself, never by limitbreak calling the sign-in server: a day before its access token ends, or after a `401`, limitbreak runs `CODEX_HOME=<dir> codex app-server` and asks it for the account with `refreshToken`. A `401` on the default account goes back to Codex, which refreshes its own login.
- Codex first tries a WebSocket; the proxy answers `426` and Codex carries on over HTTP (it logs one `failed to connect to Responses WebSocket` line per session).
- Usage per account comes from `GET /backend-api/wham/usage`, which spends nothing, at most once every 5 minutes, and from the `x-codex-*` headers of each reply.
- Replaying a turn on another ChatGPT account has not been tried yet with two subscriptions; encrypted reasoning from one account may not be readable by another.

## Grok

The same proxy rotates Grok Build (`grok`, 1.0.77 here) between Grok sign-ins. Point grok at it:

```sh
GROK_CLI_CHAT_PROXY_BASE_URL="http://127.0.0.1:48617/grok/v1" grok
# or source limitbreak-shell.sh, whose grok() does this when the proxy is up
# an app gives each agent its own client: http://127.0.0.1:48617/_c/<client>/grok/v1
```

`grok agent stdio` (ACP) honours the same variable.

What grok's traffic looks like (seen through a capture proxy, grok 1.0.77 on a SuperGrok browser sign-in):

- Every backend request goes to that base, not just the chat: `GET /v1/models`, `GET /v1/settings`, `GET /v1/feedback/config`, `POST /v1/storage`, `POST /v1/traces` (OTLP protobuf), `POST /v1/sessions/<id>/signals` and `/turn-deltas`, and the model turns, `POST /v1/responses` (an OpenAI Responses-style stream, `text/event-stream`, request body `Content-Encoding: zstd`). Without the variable they go to `https://cli-chat-proxy.grok.com/v1`. A `GET /` without credentials is a reachability check (404 upstream).
- Auth on every one of them: `Authorization: Bearer <session token>` (the `key` in `~/.grok/auth.json`, a JWT from `https://auth.x.ai` that lasts about 6 hours) plus `X-Xai-Token-Auth: xai-grok-cli`. Identity rides along in `X-Email`, `X-Userid`, `X-Grok-User-Id` and, on traces, `X-Teamid`. The variable works with an ordinary browser sign-in, not only an OIDC one: the browser sign-in is stored as `auth_mode: "oidc"` with issuer `https://auth.x.ai`.
- Replies carry request rate limits (`x-ratelimit-limit-requests`, `-remaining-requests`, `-tokens`), not the plan's usage. The plan's usage is `GET /v1/billing?format=credits`: `config.currentPeriod` (`type` `USAGE_PERIOD_TYPE_WEEKLY`, `start`, `end`) and `config.creditUsagePercent`. It spends nothing.

How limitbreak handles it:

- Accounts are `~/.grok` (the default, grok's own login) and every `~/.grok-<name>` with a login in its `auth.json`. Each named one is a separate grok home (`GROK_HOME`), so its sign-in, lock files and refreshes never touch `~/.grok`. `limitbreak add --provider grok [name]` runs `GROK_HOME=~/.grok-<name> grok login --oauth`; `limitbreak remove --provider grok <name>` deletes that dir (only its `auth.json` when limitbreak did not make it).
- Clients reach Grok under `/grok`, because its `/v1/…` paths are also Claude's; the prefix comes off before forwarding. Only the model turns (`/v1/responses`, `/v1/chat/completions`, `/v1/messages`) are put on an account. Everything else keeps grok's own login. (A Mac with no login of its own is the exception: see [below](#a-mac-with-no-grok-sign-in-of-its-own).)
- On the default account a turn keeps grok's own credentials, and limitbreak never writes `~/.grok` itself (grok refreshes it with its refresh token; limitbreak runs grok only to refresh a named login). On another account the turn gets that login's token, `X-Xai-Token-Auth` and, where grok sent them, the identity headers above. Body and encoding pass as they are; replies stream back as they arrive.
- A named login is refreshed by grok itself, never by limitbreak calling the sign-in server: an hour before its token ends, or after a `401`, limitbreak runs `GROK_HOME=<dir> GROK_AUTH_EARLY_INVALIDATION_SECS=86400 grok models` (with no proxy or API key in its environment). The wide window makes grok refresh before it lists models, and the refresh counts only if the token's end moved. Not yet run against a real second login. A login unused for a week may need signing in again (grok's sign-ins are said to last 7 days).
### Logins from Coffer

Every Mac runs its own Limit Break. [Coffer](https://github.com/radu2lupu/coffer) is the one shared store of the logins: `limitbreak/<provider>/<account>` for `claude` (the 1-year token), `codex` and `grok` (the whole `auth.json`). Each Mac keeps a copy in the dir the provider already uses (`~/.grok-<name>`, `~/.codex-<name>`, `~/.claude-<name>`), so it keeps working when Coffer is down. Only named accounts sync; the default `~/.grok` and `~/.codex` belong to the tools themselves, and sharing one would give two homes the same rotating refresh token. Design and failure modes: `ROADMAP/logins-from-coffer.md`.

```sh
limitbreak share [name...] [--provider P]    # publish named local logins to Coffer (all of them with no name)
limitbreak unshare [name...] [--provider P]  # stop syncing this Mac's copy; Coffer keeps its copy
limitbreak sync                              # one sync pass now, and what it did
```

- **Configuration.** The address is `LIMITBREAK_COFFER_ADDR` or the first line of `~/.config/limitbreak/coffer` (for example `http://100.119.127.103:8795`). The token is the file named by `LIMITBREAK_COFFER_TOKEN_FILE`, else the login Keychain item with service `coffer.limitbreak` and account `token`. With either missing, sync is off and the Mac behaves as one with no Coffer.
- **Sync** runs every 5 minutes inside `limitbreak serve`. Per account it compares Coffer's version, the version last synced and the local login: Coffer moved, so install it; the local login moved (a refresh), so write it back with `PUT ?if-version=<last synced>`; both moved, so the login whose access token ends later wins. A write that loses the compare-and-swap writes nothing and the next pass reads again. A login that is not tracked and differs from Coffer's is refused, and so is a dir another tool keeps. State is `~/.local/state/limitbreak/coffer-sync.json` (versions and fingerprints, no secret).
- **One Mac refreshes at a time.** A Grok or Codex refresh token is spent once, so a refresh (the request-time one after a `401` or under an hour left, and the loop's under 3 hours for Grok, under 24 hours for Codex) first takes a 3-minute lease in Coffer (`limitbreak/lease/<provider>/<account>`), pulls any newer login, refreshes only if it is still due, writes the result back and lets the lease go. With Coffer unreachable a synced rotating login is not refreshed; the token is good for hours and the next pass catches up.
- `GET /_status` marks a Grok account synced this way `"shared": true` and gives `tokenEnds` (unix seconds, the access token's end).
- To move an existing login: sign it in under a name on one Mac (`limitbreak add --provider grok work`), `limitbreak share --provider grok work`, then `limitbreak sync` on each other Mac. Admin steps (an identity per Mac, rules pinned to its tailnet IP) are in the roadmap file.
### A Mac with no Grok sign-in of its own

When `~/.grok` holds no login, every Grok request on that Mac (models, settings, storage, sessions, traces, not only model turns) is put on an account: the shared one, with its token and the identity headers. A Mac that has its own login keeps the rule above (only model turns). With no account to go on (nothing shared yet, every one spent) the request goes on as the client made it.

grok itself will not send a request without an `auth.json` (`grok models` with none sends nothing and says it is not authenticated), so the smallest fix is a stand-in: `limitbreak grok-placeholder <grok-home>` writes an `auth.json` there with a token of the shape grok checks (expiring in 2100, nobody's, no refresh token). grok starts on it, sends it as the bearer token, and Limit Break replaces the `Authorization` and identity headers on every request. The placeholder is never listed as an account, and the command refuses to overwrite a real login. An app that gives each agent its own `GROK_HOME` runs it on that dir when the Mac has no login of its own and `GET /_status` shows a usable shared Grok account, then starts `grok agent stdio` with `GROK_HOME` set and `GROK_CLI_CHAT_PROXY_BASE_URL=http://127.0.0.1:48617/_c/<client>/grok/v1`.

`GET /_status` marks such an account `"shared": true` and gives `tokenEnds` (unix seconds, its access token's end); the account is usable while `tokenEnds` is in the future. Every Grok account has `tokenEnds`; `shared` is absent otherwise.

## OpenCode

limitbreak does not proxy OpenCode; it only reads its accounts for `GET /_usage`. They are OpenCode's own keys in `~/.local/share/opencode/auth.json`:

- `opencode-go`, the Go subscription: its rolling 5-hour, weekly and monthly windows come from `GET https://opencode.ai/zen/go/v1/usage` with the key (it spends nothing), at most once every 5 minutes. A window at 100% marks the account spent until it resets.
- `opencode`, Zen: paid as you go, so it has no window and is reported with `noLimit: true`. OpenCode keeps this key for its free models too, so Zen is listed only when `opencode stats` shows it cost money over the period; a Zen key used only for free models is left out.

Other providers OpenCode holds keys for (OpenRouter, say) are not listed. Each account's weighted total is its provider's tokens from `opencode stats --json --models --days N`, reasoning counted as output. `opencode stats` takes a few seconds, so it runs in the background every 5 minutes and a report uses the last totals read. It counts calendar days (`--days 0` is today, used for any period under a day), so the totals are approximate, and it does not split a provider by session: the whole total is the account's `rest`.

## Options

`limitbreak serve --port 48617 --upstream https://api.anthropic.com --codex-upstream https://chatgpt.com --grok-upstream https://cli-chat-proxy.grok.com --opencode-usage https://opencode.ai/zen/go/v1/usage --home ~ --credentials auto|keychain|file`

A request path may begin with `/_c/<client>/` to give a client (an agent, a tool) its own active account; the prefix is stripped before forwarding. `/backend-api/…` paths are Codex's, `/grok/…` paths Grok's (the `/grok` comes off too), everything else Claude's.

Loopback API for apps that show accounts (JSON; fields are only ever added):

- `GET /_status`: every account with `id`, `provider` (`claude`, `codex` or `grok`), `email`, `label` (the name to show: the email, else the id), `plan` (`max`, `plus`, … or empty), `spentUntil` (unix seconds or null), `activeFor` (client names), `authEnds` (unix seconds or null), `tokenEnds` (unix seconds or null; the access token of a Grok account), `shared` (true on a Grok login synced through Coffer, see [Logins from Coffer](#logins-from-coffer)), `windows` and, for Claude, `usage` (the stored usage values by name, such as `seven_day_utilization`, each with `value` and `observed`). `windows` is the same for every provider: `[{"id":"five_hour","label":"5h","usedPercent":42,"resetsAt":1800000000,"windowMinutes":300,"observed":1799990000}]` (Codex: `primary`, `secondary`; Grok: `weekly`, its billing period; times in unix seconds, `resetsAt` may be null). No token is ever in it.
- `PUT /_c/<client>/_active` with `{"provider":"codex","id":"work"}` (`provider` defaults to `claude`): that client's next request for that provider goes to `work` (it still moves on when `work` is spent; sessions already running stay where they are). The active account is per client per provider. `404` for an unknown provider or id. Replies with the status list.

For apps, `add` and `remove` speak JSON, one event per line on stdout:

```sh
limitbreak add --provider codex work --json
{"event":"open_url","url":"https://auth.openai.com/oauth/authorize?…"}
{"event":"added","provider":"codex","id":"work","email":"me@example.com"}
limitbreak remove --provider codex work --yes --json
{"event":"removed","provider":"codex","id":"work","email":"me@example.com"}
```

`open_url` is the sign-in page (the sign-in may have opened it already); `device_code` (`code`) is a code to type on that page; `need_code` asks for a pasted code, which `add` then reads as one line from stdin; `added` may carry `sameAs` when the new login is the same subscription as another account; `error` (`message`) ends a failed run, which exits 1. Without `--json` and `--provider` both commands behave as before.

## Caveats, read these

- **Terms of service.** This uses the official `claude` binary with accounts you own, but spreading load across subscription accounts may conflict with Anthropic's terms for your plan. Read them; you are responsible for your use. Not affiliated with or endorsed by Anthropic.
- **Don't copy a login between machines.** Refresh tokens rotate; two machines holding the same one will log each other out. Sign in once per machine. `limitbreak share` puts a 1-year Claude token, or a named Grok or Codex login (refresh token included, in Coffer, with a lease so one Mac refreshes at a time), into Coffer; never a Claude sign-in's refresh token.
- **Headless hosts.** On macOS over SSH the Keychain is locked, so Claude Code cannot read its own sign-in. Run the proxy as a user service in the login session (what `install.sh` does); over SSH the `claude` function then passes a placeholder bearer token that the proxy replaces with the account's token, so `ssh host claude -p …` works on the subscription. Such a session has no claude.ai connectors, and fails if no account can serve the request.
- The reset time is read from Anthropic's response headers, which are not a documented contract and may change.

## Uninstall

macOS: `launchctl bootout gui/$(id -u)/dev.limitbreak; rm ~/Library/LaunchAgents/dev.limitbreak.plist`.
Linux: `systemctl --user disable --now limitbreak`.
Then remove `~/.local/bin/limitbreak*` and the `source` line from your shell profile. Accounts you added stay as ordinary `~/.claude-<name>` logins; `limitbreak remove <name>` deletes one.

## Develop

```sh
go vet ./... && go test ./...
```

## License

MIT, see [LICENSE](LICENSE).
