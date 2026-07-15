#!/usr/bin/env bash
# Run slack-mcp-server with env vars loaded from ~/.ssh/slack_tokens (SSE transport).
# Used by LaunchAgent for system-start. See SLACK_TOKENS_SETUP.md for token file format.

set -e
# LaunchAgent may not set HOME; ensure it's set for token file path
export HOME="${HOME:-$(eval echo ~$(id -un))}"
REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
TOKENS_FILE="${HOME}/.ssh/slack_tokens"

# Resolve the server binary: $SLACK_MCP_BIN > `current` pin symlink >
# ~/.local/bin > <repo>/build. Prints the chosen path on stdout; returns 1
# if an explicit override (env var or pin) exists but is unusable — a stale
# pin must fail loudly, not silently fall back.
resolve_binary() {
  if [[ -n "${SLACK_MCP_BIN:-}" ]]; then
    if [[ -x "$SLACK_MCP_BIN" ]]; then
      echo "$SLACK_MCP_BIN"
      return 0
    fi
    echo "Error: SLACK_MCP_BIN is set to '$SLACK_MCP_BIN' but it is not an executable file." >&2
    echo "Fix the path or unset SLACK_MCP_BIN to fall back to auto-detection." >&2
    return 1
  fi
  local pin="${HOME}/.local/share/slack-mcp-server/current"
  if [[ -e "$pin" || -L "$pin" ]]; then
    if [[ -x "$pin" ]]; then
      echo "$pin"
      return 0
    fi
    echo "Error: binary pin '$pin' exists but does not point to an executable file." >&2
    echo "Repoint it (make service-local / make service-release) or remove it to fall back to auto-detection." >&2
    return 1
  fi
  if [[ -x "${HOME}/.local/bin/slack-mcp-server" ]]; then
    echo "${HOME}/.local/bin/slack-mcp-server"
    return 0
  fi
  # May not exist yet; the build-if-missing fallback below applies only to this path.
  echo "${REPO_DIR}/build/slack-mcp-server"
}

BINARY="$(resolve_binary)"
echo "Info: using slack-mcp-server binary: $BINARY" >&2

if [[ ! -f "$TOKENS_FILE" ]]; then
  echo "Error: tokens file not found: $TOKENS_FILE" >&2
  echo "Create it with your Slack tokens. See SLACK_TOKENS_SETUP.md" >&2
  exit 1
fi

# Export KEY=value lines (skip comments and empty lines); trim \r for Windows line endings
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line//$'\r'/}"
  line="${line%%#*}"
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [[ -z "$line" ]] && continue
  if [[ "$line" == *=* ]]; then
    export "$line"
  fi
done < "$TOKENS_FILE"

# Build binary if missing (only when go is in PATH, e.g. from LaunchAgent)
if [[ ! -x "$BINARY" ]]; then
  cd "$REPO_DIR"
  if command -v go &>/dev/null; then
    make build
  else
    echo "Error: binary not found and go not in PATH. Run once from your terminal: cd $REPO_DIR && make build" >&2
    exit 1
  fi
fi

exec "$BINARY" -t sse
