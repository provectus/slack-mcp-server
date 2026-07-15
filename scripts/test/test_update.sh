#!/usr/bin/env bash
# test_update.sh — coverage for scripts/update.sh (slack-mcp-update):
# up-to-date, --check (exit 10), successful apply (binary really replaced,
# service restart via stubs), CONFIG-CHANGE warnings, rollback injections
# (failed download, truncated download / checksum mismatch, bad probe), the
# required end-to-end scenario, and the jq-less config-change scan fallback.
#
# Every case runs with launchctl, systemctl AND uname replaced by PATH stubs
# (u_sandbox), so update.sh can never see the host platform or touch the real
# service managers — service activity is asserted via the stub log only.
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

UPDATE_SH="$(cd "$T_DIR/.." && pwd)/update.sh"
RELEASES_LIST_URL="$API_BASE/releases?per_page=100"

# --- updater sandbox and helpers ------------------------------------------

# u_sandbox — t_sandbox plus the launchctl/systemctl/uname stubs and a fixed
# fake platform (darwin-arm64; override the UNAME_SHIM_*/U_ASSET globals
# before staging to test another platform). Sets BIN to the "installed"
# binary path inside the sandbox.
u_sandbox() {
  t_sandbox
  SERVICE_SHIM_LOG="$WORK/service.log"
  : >"$SERVICE_SHIM_LOG"
  SERVICE_SHIM_PRESENT=0
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/launchctl"
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/systemctl"
  cp "$T_DIR/uname_shim.sh" "$SHIMDIR/uname"
  chmod +x "$SHIMDIR/launchctl" "$SHIMDIR/systemctl" "$SHIMDIR/uname"
  UNAME_SHIM_S=Darwin
  UNAME_SHIM_M=arm64
  U_ASSET="slack-mcp-server-darwin-arm64"
  BIN="$PREFIX/slack-mcp-server"
}

# u_install_binary <tag> — place a fake installed binary at $BIN.
u_install_binary() {
  sed "s/@TAG@/$1/g" "$FIXTURES_DIR/fake-binary.tpl" >"$BIN"
  chmod +x "$BIN"
}

# u_stage_latest <tag> [bad-banner] — build the $U_ASSET release asset, its
# checksums.txt and the release JSON, and map /releases/latest plus both
# download URLs to them.
u_stage_latest() {
  local tag="$1"
  local tpl="$FIXTURES_DIR/fake-binary.tpl"
  [ "${2:-}" = "bad-banner" ] && tpl="$FIXTURES_DIR/fake-binary-bad.tpl"
  sed "s/@TAG@/$tag/g" "$tpl" >"$WORK/assets/$U_ASSET"
  printf '%s  %s\n' "$(sha256_of "$WORK/assets/$U_ASSET")" "$U_ASSET" \
    >"$WORK/assets/checksums.txt"
  sed "s/@TAG@/$tag/g" "$FIXTURES_DIR/release.tpl.json" >"$WORK/release-$tag.json"
  map_url "$API_BASE/releases/latest" "$WORK/release-$tag.json"
  map_url "$DL_BASE/$tag/$U_ASSET" "$WORK/assets/$U_ASSET"
  map_url "$DL_BASE/$tag/checksums.txt" "$WORK/assets/checksums.txt"
}

# u_map_releases <tag=body>... — write a releases-list JSON array (one object
# per argument; body is a JSON-escaped string, so use \r\n for newlines) and
# map the per_page=100 listing URL to it.
u_map_releases() {
  local out="$WORK/releases-list.json" sep="" e tag body
  printf '[' >"$out"
  for e in "$@"; do
    tag="${e%%=*}"
    body="${e#*=}"
    printf '%s{"tag_name":"%s","name":"%s","body":"%s"}' \
      "$sep" "$tag" "$tag" "$body" >>"$out"
    sep=","
  done
  printf ']\n' >>"$out"
  map_url "$RELEASES_LIST_URL" "$out"
}

# run_update <outfile> [args...] — run update.sh --bin "$BIN" with all stubs
# first on PATH; rc lands in $URC (never fails the case by itself).
run_update() {
  local out="$1"
  shift
  URC=0
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    SERVICE_SHIM_LOG="$SERVICE_SHIM_LOG" \
    SERVICE_SHIM_PRESENT="$SERVICE_SHIM_PRESENT" \
    UNAME_SHIM_S="$UNAME_SHIM_S" UNAME_SHIM_M="$UNAME_SHIM_M" \
    "$BASH" "$UPDATE_SH" --bin "$BIN" "$@" >"$out" 2>&1 || URC=$?
}

# run_update_jqless <outfile> [args...] — same, on the restricted PATH with
# jq hidden (stubs still first via the shim dir).
run_update_jqless() {
  local out="$1"
  shift
  local rpath
  rpath="$(restricted_path_without_jq)"
  if env PATH="$rpath" "$BASH" -c 'command -v jq' >/dev/null 2>&1; then
    echo "ASSERT: jq is still visible on the restricted PATH"
    return 1
  fi
  URC=0
  env -u GITHUB_TOKEN PATH="$rpath" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    SERVICE_SHIM_LOG="$SERVICE_SHIM_LOG" \
    SERVICE_SHIM_PRESENT="$SERVICE_SHIM_PRESENT" \
    UNAME_SHIM_S="$UNAME_SHIM_S" UNAME_SHIM_M="$UNAME_SHIM_M" \
    "$BASH" "$UPDATE_SH" --bin "$BIN" "$@" >"$out" 2>&1 || URC=$?
}

# u_assert_probe <tag> — the installed binary reports <tag> on --version.
u_assert_probe() {
  local line
  line="$("$BIN" --version | head -n 1)"
  [ "$line" = "slack-mcp-server $1" ] || {
    echo "ASSERT: expected the binary to report 'slack-mcp-server $1', got: $line"
    return 1
  }
}

# u_assert_bin_unchanged <sha256> — the installed binary is byte-identical to
# the pre-run snapshot.
u_assert_bin_unchanged() {
  [ "$(sha256_of "$BIN")" = "$1" ] || {
    echo "ASSERT: installed binary changed (sha256 differs from pre-run snapshot)"
    return 1
  }
}

# u_assert_no_leftovers — no .bak / .new next to the binary, updater temp dir
# cleaned up.
u_assert_no_leftovers() {
  assert_no_file "$BIN.bak"
  assert_no_file "$PREFIX/.slack-mcp-server.new"
  assert_empty_dir "$ITMP"
}

# u_assert_no_service_calls — the launchctl/systemctl stubs were never hit.
u_assert_no_service_calls() {
  [ ! -s "$SERVICE_SHIM_LOG" ] || {
    echo "ASSERT: expected no launchctl/systemctl calls — stub log:"
    cat "$SERVICE_SHIM_LOG"
    return 1
  }
}

# --- 1. up-to-date ---------------------------------------------------------

case_up_to_date() {
  u_sandbox
  u_install_binary pv-v1.2.3
  u_stage_latest pv-v1.2.3
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out"
  assert_rc 0 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "RESULT=up-to-date"
  assert_contains "$WORK/out" "INSTALLED=pv-v1.2.3"
  assert_contains "$WORK/out" "LATEST=pv-v1.2.3"
  assert_contains "$WORK/out" "CONFIG_CHANGES=0"
  u_assert_bin_unchanged "$pre"
  u_assert_no_leftovers
  u_assert_no_service_calls
  # nothing beyond the latest-release query was fetched
  assert_not_contains "$CURL_SHIM_LOG" "releases?per_page"
  assert_not_contains "$CURL_SHIM_LOG" "/releases/download/"
}

# --- 2. update available, --check -----------------------------------------

case_check_update_available() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases \
    'pv-v1.2.3=routine notes\r\nno directives here' \
    'pv-v1.0.0=old notes'
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "RESULT=update-available"
  assert_contains "$WORK/out" "INSTALLED=pv-v1.0.0"
  assert_contains "$WORK/out" "LATEST=pv-v1.2.3"
  assert_contains "$WORK/out" "CONFIG_CHANGES=0"
  assert_contains "$WORK/out" "Update available: pv-v1.0.0 -> pv-v1.2.3"
  u_assert_bin_unchanged "$pre"
  u_assert_no_leftovers
  u_assert_no_service_calls
  # check mode must not download anything
  assert_not_contains "$CURL_SHIM_LOG" "/releases/download/"
}

# --- 3. successful apply ----------------------------------------------------

case_apply_no_service() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases 'pv-v1.2.3=routine notes'
  SERVICE_SHIM_PRESENT=0
  run_update "$WORK/out"
  assert_rc 0 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "Checksum OK"
  assert_contains "$WORK/out" "RESULT=updated"
  u_assert_probe pv-v1.2.3 # binary actually replaced
  u_assert_no_leftovers
  assert_contains "$WORK/out" "No background service detected"
  # stub reported no service: presence was queried, restart never attempted
  assert_contains "$SERVICE_SHIM_LOG" "launchctl print gui/"
  assert_not_contains "$SERVICE_SHIM_LOG" "kickstart"
}

case_apply_restarts_launchd() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases 'pv-v1.2.3=routine notes'
  SERVICE_SHIM_PRESENT=1
  run_update "$WORK/out"
  assert_rc 0 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "RESULT=updated"
  assert_contains "$WORK/out" "Service restarted: com.slack-mcp-server (launchd)"
  u_assert_probe pv-v1.2.3
  u_assert_no_leftovers
  # the kickstart went to the stub (and only the stub — it logs every call)
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl kickstart -k gui/$(id -u)/com.slack-mcp-server"
}

case_apply_restarts_systemd() {
  u_sandbox
  UNAME_SHIM_S=Linux
  UNAME_SHIM_M=x86_64
  U_ASSET="slack-mcp-server-linux-amd64"
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases 'pv-v1.2.3=routine notes'
  SERVICE_SHIM_PRESENT=1
  run_update "$WORK/out"
  assert_rc 0 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "RESULT=updated"
  assert_contains "$WORK/out" "Service restarted: slack-mcp-server (systemd user)"
  u_assert_probe pv-v1.2.3
  u_assert_no_leftovers
  assert_contains "$SERVICE_SHIM_LOG" "systemctl --user restart slack-mcp-server"
  assert_not_contains "$SERVICE_SHIM_LOG" "launchctl"
}

# --- 4. CONFIG-CHANGE warnings ----------------------------------------------

# Releases fixture: config change in pv-v1.1.0 (inside (1.0.0, 1.2.0]); the
# one in pv-v0.9.0 is at or below the installed version and must NOT count.
u_map_releases_with_config_change() {
  u_map_releases \
    'pv-v1.2.0=improvements\r\nnothing else' \
    'pv-v1.1.0=feature work\r\nCONFIG-CHANGE: re-run install.sh --with-service\r\nmore notes' \
    'pv-v0.9.0=ancient\r\nCONFIG-CHANGE: out-of-range note must not count'
}

case_config_change_warning_check() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.0
  u_map_releases_with_config_change
  run_update "$WORK/out" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service"
  assert_contains "$WORK/out" "CONFIG_CHANGES=1"
  assert_not_contains "$WORK/out" "out-of-range note must not count"
  u_assert_no_leftovers
}

case_config_change_warning_apply() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.0
  u_map_releases_with_config_change
  run_update "$WORK/out"
  assert_rc 0 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service"
  assert_contains "$WORK/out" "CONFIG_CHANGES=1"
  assert_contains "$WORK/out" "RESULT=updated"
  u_assert_probe pv-v1.2.0
  u_assert_no_leftovers
}

case_no_config_change_control() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.0
  # a mid-line mention must not count either: the scan is line-anchored
  u_map_releases \
    'pv-v1.2.0=routine notes' \
    'pv-v1.1.0=see the CONFIG-CHANGE: convention docs for details'
  run_update "$WORK/out" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" "CONFIG_CHANGES=0"
  assert_not_contains "$WORK/out" "WARNING ("
}

# --- 5. rollback injections --------------------------------------------------

case_rollback_failed_download() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases 'pv-v1.2.3=routine notes'
  unmap_url "$DL_BASE/pv-v1.2.3/$U_ASSET" # the binary download fails
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out"
  assert_rc 1 "$URC"
  assert_contains "$WORK/out" "ERROR: failed to download"
  assert_contains "$WORK/out" "RESULT=error"
  u_assert_bin_unchanged "$pre"
  u_assert_probe pv-v1.0.0
  u_assert_no_leftovers
  u_assert_no_service_calls
}

case_rollback_truncated_download_checksum_mismatch() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  u_map_releases 'pv-v1.2.3=routine notes'
  # serve a truncated copy of the real asset; checksums.txt still lists the
  # full file's hash, so the truncation must be caught by the checksum gate
  head -c 16 "$WORK/assets/$U_ASSET" >"$WORK/assets/truncated"
  unmap_url "$DL_BASE/pv-v1.2.3/$U_ASSET"
  map_url "$DL_BASE/pv-v1.2.3/$U_ASSET" "$WORK/assets/truncated"
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out"
  assert_rc 1 "$URC"
  assert_contains "$WORK/out" "ERROR: checksum mismatch for $U_ASSET"
  assert_contains "$WORK/out" "RESULT=error"
  u_assert_bin_unchanged "$pre"
  u_assert_probe pv-v1.0.0
  u_assert_no_leftovers
  u_assert_no_service_calls
}

case_rollback_bad_probe() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3 bad-banner # checksum valid, --version banner wrong
  u_map_releases 'pv-v1.2.3=routine notes'
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out"
  assert_rc 1 "$URC"
  assert_contains "$WORK/out" "ERROR: downloaded binary failed the --version probe"
  assert_contains "$WORK/out" "RESULT=error"
  u_assert_bin_unchanged "$pre"
  u_assert_probe pv-v1.0.0
  u_assert_no_leftovers
  u_assert_no_service_calls
}

case_rollback_stale_version_stamp() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.3
  # re-stamp the served asset with an older version banner; checksum stays
  # valid because checksums.txt is regenerated from the re-stamped asset.
  sed "s/@TAG@/pv-v1.2.2/g" "$FIXTURES_DIR/fake-binary.tpl" >"$WORK/assets/$U_ASSET"
  printf '%s  %s\n' "$(sha256_of "$WORK/assets/$U_ASSET")" "$U_ASSET" \
    >"$WORK/assets/checksums.txt"
  u_map_releases 'pv-v1.2.3=routine notes'
  local pre
  pre="$(sha256_of "$BIN")"
  run_update "$WORK/out"
  assert_rc 1 "$URC"
  assert_contains "$WORK/out" "ERROR: downloaded binary reports version 'pv-v1.2.2' instead of pv-v1.2.3"
  assert_contains "$WORK/out" "RESULT=error"
  u_assert_bin_unchanged "$pre"
  u_assert_probe pv-v1.0.0
  u_assert_no_leftovers
  u_assert_no_service_calls
}

# --- 6. required end-to-end scenario (tech spec §4) --------------------------
# Installed old pv-v tag -> newer latest with a config-changing release in
# between -> --check first, then apply, in one flowing test.

case_e2e_check_then_apply() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.0
  u_map_releases_with_config_change
  SERVICE_SHIM_PRESENT=1
  local pre
  pre="$(sha256_of "$BIN")"

  # step 1: --check reports, warns, changes nothing
  run_update "$WORK/out-check" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out-check"
    return 1
  }
  assert_contains "$WORK/out-check" "RESULT=update-available"
  assert_contains "$WORK/out-check" "INSTALLED=pv-v1.0.0"
  assert_contains "$WORK/out-check" "LATEST=pv-v1.2.0"
  assert_contains "$WORK/out-check" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service"
  assert_contains "$WORK/out-check" "CONFIG_CHANGES=1"
  u_assert_bin_unchanged "$pre"
  u_assert_no_service_calls

  # step 2: apply swaps the binary, warns again, restarts the (stub) service
  run_update "$WORK/out-apply"
  assert_rc 0 "$URC" || {
    cat "$WORK/out-apply"
    return 1
  }
  assert_contains "$WORK/out-apply" "RESULT=updated"
  assert_contains "$WORK/out-apply" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service"
  assert_contains "$WORK/out-apply" "CONFIG_CHANGES=1"
  assert_contains "$WORK/out-apply" "Updated: pv-v1.0.0 -> pv-v1.2.0"
  u_assert_probe pv-v1.2.0
  u_assert_no_leftovers
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl kickstart -k gui/$(id -u)/com.slack-mcp-server"
}

# --- 7. jq-less config-change scan (body \n-unescaping fallback) -------------

case_jqless_config_change_scan() {
  u_sandbox
  u_install_binary pv-v1.0.0
  u_stage_latest pv-v1.2.0
  u_map_releases_with_config_change
  run_update_jqless "$WORK/out" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out"
    return 1
  }
  assert_contains "$WORK/out" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service"
  assert_contains "$WORK/out" "CONFIG_CHANGES=1"
  assert_not_contains "$WORK/out" "out-of-range note must not count"
  assert_contains "$WORK/out" "RESULT=update-available"
}

t_case up_to_date case_up_to_date
t_case check_update_available case_check_update_available
t_case apply_no_service case_apply_no_service
t_case apply_restarts_launchd case_apply_restarts_launchd
t_case apply_restarts_systemd case_apply_restarts_systemd
t_case config_change_warning_check case_config_change_warning_check
t_case config_change_warning_apply case_config_change_warning_apply
t_case no_config_change_control case_no_config_change_control
t_case rollback_failed_download case_rollback_failed_download
t_case rollback_truncated_download_checksum_mismatch case_rollback_truncated_download_checksum_mismatch
t_case rollback_bad_probe case_rollback_bad_probe
t_case rollback_stale_version_stamp case_rollback_stale_version_stamp
t_case e2e_check_then_apply case_e2e_check_then_apply
t_case jqless_config_change_scan case_jqless_config_change_scan
t_done
