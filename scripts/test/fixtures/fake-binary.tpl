#!/bin/sh
# Fixture release "binary": answers --version like the real slack-mcp-server.
# @TAG@ is substituted by the test lib when the asset is staged.
if [ "${1:-}" = "--version" ]; then
  echo "slack-mcp-server @TAG@"
  echo "commit: 0000000"
  echo "built:  1970-01-01T00:00:00Z"
  exit 0
fi
echo "fixture binary: unexpected invocation: $*" >&2
exit 64
