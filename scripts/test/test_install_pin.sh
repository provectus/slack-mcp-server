#!/usr/bin/env bash
# --version pv-vX.Y.Z pin: resolves the tag-specific API URL, never "latest".
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

case_pin_resolves_tag_url() {
  t_sandbox
  stage_release pv-v1.0.0
  # NOTE: /releases/latest is deliberately NOT mapped — hitting it would fail.

  run_install "$WORK/out" --version pv-v1.0.0 --no-updater || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }

  assert_contains "$CURL_SHIM_LOG" "$API_BASE/releases/tags/pv-v1.0.0"
  assert_not_contains "$CURL_SHIM_LOG" "$API_BASE/releases/latest"
  assert_contains "$WORK/out" "INSTALLED=pv-v1.0.0"
  assert_exec "$PREFIX/slack-mcp-server"
  [ "$("$PREFIX/slack-mcp-server" --version | head -n 1)" = "slack-mcp-server pv-v1.0.0" ]
}

t_case pin_resolves_tag_url case_pin_resolves_tag_url
t_done
