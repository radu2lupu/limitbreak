# Source this from ~/.zshenv (or ~/.zshrc / ~/.bashrc). `claude` then goes through the account proxy
# when it is running on this host, and runs as before when it is not.
#
# Over SSH on macOS the login Keychain is out of reach, so Claude Code cannot read its own sign-in and
# stops before sending anything. The proxy replaces every request's credentials with the account's
# token anyway, so in that case `claude` gets a placeholder bearer token that never leaves this host.
# Claude.ai connectors are not available in such a session; local sessions are unchanged.
claude() {
  local port="${LIMITBREAK_PORT:-48617}"
  if ! curl -s -m 1 -o /dev/null "http://127.0.0.1:${port}/_status"; then
    command claude "$@"
  elif [ -n "$SSH_CONNECTION" ] && [ -z "$ANTHROPIC_AUTH_TOKEN" ] && [ "$(uname)" = Darwin ] &&
    ! security show-keychain-info >/dev/null 2>&1; then
    ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" ANTHROPIC_AUTH_TOKEN=limitbreak command claude "$@"
  else
    ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" command claude "$@"
  fi
}
