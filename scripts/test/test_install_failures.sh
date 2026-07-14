#!/usr/bin/env bash
# Failure paths: API/download failure (3), checksum mismatch (4), probe
# failure (5, with no binary left behind).
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

case_api_failure_rc3() {
  t_sandbox
  # nothing mapped: the releases API call itself fails
  local rc=0
  run_install "$WORK/out" --no-updater || rc=$?
  assert_rc 3 "$rc"
  assert_contains "$WORK/out" "failed to query the GitHub releases API"
  assert_no_file "$PREFIX/slack-mcp-server"
}

case_binary_download_failure_rc3() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  unmap_url "$DL_BASE/pv-v1.2.3/$(host_asset)"
  local rc=0
  run_install "$WORK/out" --no-updater || rc=$?
  assert_rc 3 "$rc"
  assert_contains "$WORK/out" "failed to download"
  assert_no_file "$PREFIX/slack-mcp-server"
}

case_checksums_download_failure_rc3() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  unmap_url "$DL_BASE/pv-v1.2.3/checksums.txt"
  local rc=0
  run_install "$WORK/out" --no-updater || rc=$?
  assert_rc 3 "$rc"
  assert_contains "$WORK/out" "failed to download"
  assert_no_file "$PREFIX/slack-mcp-server"
}

case_checksum_mismatch_rc4() {
  t_sandbox
  stage_release pv-v1.2.3
  map_latest pv-v1.2.3
  # tamper with the served asset after checksums.txt was generated
  printf '# tampered\n' >>"$WORK/assets/$(host_asset)"
  local rc=0
  run_install "$WORK/out" --no-updater || rc=$?
  assert_rc 4 "$rc"
  assert_contains "$WORK/out" "checksum verification failed"
  assert_no_file "$PREFIX/slack-mcp-server"
}

case_probe_failure_rc5_no_binary_left() {
  t_sandbox
  stage_release pv-v1.2.3 bad-banner # checksum valid, --version banner wrong
  map_latest pv-v1.2.3
  local rc=0
  run_install "$WORK/out" --no-updater || rc=$?
  assert_rc 5 "$rc"
  assert_contains "$WORK/out" "failed the --version probe"
  assert_no_file "$PREFIX/slack-mcp-server"
}

t_case api_failure_rc3 case_api_failure_rc3
t_case binary_download_failure_rc3 case_binary_download_failure_rc3
t_case checksums_download_failure_rc3 case_checksums_download_failure_rc3
t_case checksum_mismatch_rc4 case_checksum_mismatch_rc4
t_case probe_failure_rc5_no_binary_left case_probe_failure_rc5_no_binary_left
t_done
