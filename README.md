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

It does the same for Codex on ChatGPT sign-ins, see [Codex](#codex).

## What it does and does not touch

- Only `/v1/messages*` requests are sent on an account's token. Everything else (claude.ai connectors, MCP, artifacts, profile) goes through untouched with Claude Code's own credentials, so those keep working.
- It never sets `CLAUDE_CODE_OAUTH_TOKEN` (that makes Claude Code drop its connectors and local MCP servers).
- Accounts are the logins Claude Code already holds: the default one plus every `~/.claude-<name>` config directory that has a login. An account's token is its 1-year token from `limitbreak renew` when it has one, else the sign-in token read from the macOS Keychain item Claude Code keeps per config dir, or from `<dir>/.credentials.json`. Tokens are never logged or exposed by `GET /_status`, and sign-in tokens are never copied.
- An expired token is refreshed by running one tiny `claude -p` under that account's directory, so Claude Code stays the only thing that refreshes a login.
- If no account can serve a request, it passes through with Claude Code's own login: never worse than no proxy.
- Only a window rejection (`429` with `anthropic-ratelimit-unified-status: rejected`) switches accounts. Ordinary rate limits go back to Claude unchanged.

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

To use the same tokens on another machine of yours, sign the same-named accounts in there once, install limitbreak, then from this one run `limitbreak share <ssh-host>`. Unlike a sign-in, a 1-year token never rotates, so two machines holding it do not log each other out. The other machine checks each token with Anthropic before storing it.

`limitbreak list` shows when each account's auth ends. The running proxy warns 7 days before any auth ends, in its log and, on macOS, as a notification. If Anthropic rejects an account's token (401), the proxy skips that account for a few minutes and logs `run "limitbreak renew <name>"`; Claude never sees the error.

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

## Options

`limitbreak serve --port 48617 --upstream https://api.anthropic.com --codex-upstream https://chatgpt.com --home ~ --credentials auto|keychain|file`

A request path may begin with `/_c/<client>/` to give a client (an agent, a tool) its own active account; the prefix is stripped before forwarding. `/backend-api/…` paths are Codex's, everything else Claude's.

Loopback API for apps that show accounts (JSON; fields are only ever added):

- `GET /_status`: every account with `id`, `provider` (`claude` or `codex`), `email`, `label` (the name to show: the email, else the id), `plan` (`max`, `plus`, … or empty), `spentUntil` (unix seconds or null), `activeFor` (client names), `authEnds` (unix seconds or null), `windows` and, for Claude, `usage` (the stored usage values by name, such as `seven_day_utilization`, each with `value` and `observed`). `windows` is the same for every provider: `[{"id":"five_hour","label":"5h","usedPercent":42,"resetsAt":1800000000,"windowMinutes":300,"observed":1799990000}]` (Codex: `primary`, `secondary`; times in unix seconds, `resetsAt` may be null). No token is ever in it.
- `PUT /_c/<client>/_active` with `{"provider":"codex","id":"work"}` (`provider` defaults to `claude`): that client's next request for that provider goes to `work` (it still moves on when `work` is spent). The active account is per client per provider. `404` for an unknown provider or id. Replies with the status list.

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
- **Don't copy a login between machines.** Refresh tokens rotate; two machines holding the same one will log each other out. Sign in once per machine.
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
