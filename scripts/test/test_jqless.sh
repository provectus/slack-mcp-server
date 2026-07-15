#!/usr/bin/env bash
# Happy install with jq hidden from PATH: exercises the sed/grep JSON
# fallback parsers in install.sh.
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

case_jqless_happy_install() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3

  local rpath
  rpath="$(restricted_path_without_jq)"
  # sanity: jq must not be visible on the restricted PATH
  if env PATH="$rpath" "$BASH" -c 'command -v jq' >/dev/null 2>&1; then
    echo "ASSERT: jq is still visible on the restricted PATH"
    return 1
  fi

  local rc=0
  env -u GITHUB_TOKEN PATH="$rpath" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    "$BASH" "$INSTALL" --prefix "$PREFIX" --no-updater >"$WORK/out" 2>&1 || rc=$?
  assert_rc 0 "$rc" || {
    cat "$WORK/out"
    return 1
  }
  assert_exec "$PREFIX/slack-mcp-server"
  assert_contains "$WORK/out" "Checksum OK"
  assert_contains "$WORK/out" "INSTALLED=pv-v1.2.3"
}

t_case jqless_happy_install case_jqless_happy_install
t_done
