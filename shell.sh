# Source this from ~/.zshenv (or ~/.zshrc / ~/.bashrc). `claude` then goes through the account proxy
# when it is running on this host, and runs as before when it is not.
#
# When the proxy serves only Coffer's accounts (its /_status reply carries X-Limitbreak-Placeholder-Login),
# `claude` starts on a placeholder login (CLAUDE_CODE_OAUTH_TOKEN) instead of this Mac user's own sign-in:
# the proxy replaces every request's credentials with the pool account's token, so the Keychain login is
# never read or used. That also covers SSH sessions, where the login Keychain is out of reach. Claude.ai
# connectors and Remote Control are not available in such a session. A token or API key you set yourself
# is kept. When the proxy is down, or an older one that does not send the header, `claude` runs exactly as
# before (no base URL, no placeholder).
claude() {
  local port="${LIMITBREAK_PORT:-48617}" reply
  if ! reply="$(curl -s -m 1 -D - -o /dev/null "http://127.0.0.1:${port}/_status")"; then
    command claude "$@"
  elif [ -z "$CLAUDE_CODE_OAUTH_TOKEN$ANTHROPIC_AUTH_TOKEN$ANTHROPIC_API_KEY" ] &&
    printf '%s' "$reply" | grep -qi '^x-limitbreak-placeholder-login: *1'; then
    ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" CLAUDE_CODE_OAUTH_TOKEN=limitbreak-placeholder command claude "$@"
  else
    ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" command claude "$@"
  fi
}

# Grok Build likewise, so its turns are pooled and logged (Codex reads openai_base_url from its config).
grok() {
  local port="${LIMITBREAK_PORT:-48617}"
  if curl -s -m 1 -o /dev/null "http://127.0.0.1:${port}/_status"; then
    GROK_CLI_CHAT_PROXY_BASE_URL="${GROK_CLI_CHAT_PROXY_BASE_URL:-http://127.0.0.1:${port}/grok/v1}" command grok "$@"
  else
    command grok "$@"
  fi
}
