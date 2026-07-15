#!/usr/bin/env bash
# Updater matrix: --with-updater installs slack-mcp-update, --no-updater
# doesn't, non-TTY default installs it with a notice, and a sibling update.sh
# is preferred over downloading.
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

case_with_updater_downloads() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  map_updater
  run_install "$WORK/out" --with-updater || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }
  assert_exec "$PREFIX/slack-mcp-update"
  assert_contains "$PREFIX/slack-mcp-update" "slack-mcp-update fixture"
  assert_contains "$CURL_SHIM_LOG" "$UPDATE_SH_URL"
}

case_no_updater_skips() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  run_install "$WORK/out" --no-updater || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }
  assert_no_file "$PREFIX/slack-mcp-update"
  assert_not_contains "$CURL_SHIM_LOG" "$UPDATE_SH_URL"
}

case_notty_default_installs_with_notice() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  map_updater
  # no --with-updater/--no-updater, detached from any controlling terminal
  run_install_notty "$WORK/out" || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "No TTY: installing the updater by default"
  assert_exec "$PREFIX/slack-mcp-update"
}

case_sibling_update_sh_preferred() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  # a sibling update.sh next to install.sh must be copied, not downloaded
  printf '#!/bin/sh\necho sibling updater\n' >"$WORK/scripts/update.sh"
  run_install "$WORK/out" --with-updater || {
    echo "install failed (rc=$?); output:"
    cat "$WORK/out"
    return 1
  }
  assert_exec "$PREFIX/slack-mcp-update"
  assert_contains "$PREFIX/slack-mcp-update" "sibling updater"
  assert_not_contains "$CURL_SHIM_LOG" "$UPDATE_SH_URL"
}

t_case with_updater_downloads case_with_updater_downloads
t_case no_updater_skips case_no_updater_skips
t_case notty_default_installs_with_notice case_notty_default_installs_with_notice
t_case sibling_update_sh_preferred case_sibling_update_sh_preferred
t_done
