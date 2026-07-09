#!/usr/bin/env bash
# Run slack-mcp-server with env vars loaded from ~/.ssh/slack_tokens (SSE transport).
# Used by LaunchAgent for system-start. See SLACK_TOKENS_SETUP.md for token file format.

set -e
# LaunchAgent may not set HOME; ensure it's set for token file path
export HOME="${HOME:-$(eval echo ~$(id -un))}"
REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
TOKENS_FILE="${HOME}/.ssh/slack_tokens"
BINARY="${REPO_DIR}/build/slack-mcp-server"

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
