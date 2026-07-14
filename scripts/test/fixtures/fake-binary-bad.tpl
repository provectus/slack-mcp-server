#!/bin/sh
# Fixture release "binary" with a wrong --version banner: must fail the
# installer's post-install probe (which requires a "slack-mcp-server " prefix).
if [ "${1:-}" = "--version" ]; then
  echo "unexpected-banner @TAG@"
  exit 0
fi
exit 64
