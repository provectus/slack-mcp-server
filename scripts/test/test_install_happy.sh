#!/usr/bin/env bash
# Happy-path install of "latest" into a temp --prefix.
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

case_happy_latest() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  map_updater

  run_install "$WORK/out" --with-updater || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }

  local dest="$PREFIX/slack-mcp-server"
  assert_file "$dest"
  assert_exec "$dest"
  assert_contains "$WORK/out" "Checksum OK"
  # the post-install probe ran and reported the fixture banner
  assert_contains "$WORK/out" "Installed: $dest (slack-mcp-server pv-v1.2.3)"
  assert_contains "$WORK/out" "Updater installed: $PREFIX/slack-mcp-update"
  # temp prefix is not on PATH -> exact export hint printed
  assert_contains "$WORK/out" "export PATH=\"$PREFIX:\$PATH\""
  # machine-readable final line
  local last
  last="$(tail -n 1 "$WORK/out")"
  [ "$last" = "INSTALLED=pv-v1.2.3" ] || {
    echo "ASSERT: expected final line INSTALLED=pv-v1.2.3, got: $last"
    cat "$WORK/out"
    return 1
  }
  # "latest" endpoint was queried
  assert_contains "$CURL_SHIM_LOG" "$API_BASE/releases/latest"
  # installed binary really is the probed fixture
  [ "$("$dest" --version | head -n 1)" = "slack-mcp-server pv-v1.2.3" ]
  # install.sh cleaned up its own temp dir
  assert_empty_dir "$ITMP"
}

t_case happy_latest case_happy_latest
t_done
