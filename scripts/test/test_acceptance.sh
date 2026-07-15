#!/usr/bin/env bash
# test_acceptance.sh — whole-feature acceptance tests for
# 002-agent-friendly-install. Verifies functional-spec.md's R1..R5 acceptance
# criteria as a feature, not per-slice. Run via scripts/test/run.sh alongside
# the slice-level suites.
#
# @layer: e2e
# @spec: 002-agent-friendly-install
#
# --- Criteria -> coverage map -----------------------------------------------
#
# R1 (published ready-to-run versions):
#   - Live "tag push fires the workflow / assets appear on the release" is
#     NOT locally testable (needs a real GitHub Actions run) — per tech-spec
#     Testing Strategy §4, this is manual/post-release smoke on pv-v* tags.
#   - What IS testable statically: the workflow's trigger pattern, asset
#     list, and absence of the original project's distribution channels.
#     Covered here by case_r1_release_pipeline_contract (below), a static
#     grep-based check of .github/workflows/release.yaml and the Makefile —
#     no existing test touches these files.
#   - The config-changing-release *convention* (CONFIG-CHANGE: note, scanned
#     and surfaced by the updater) is exercised for real in
#     case_whole_feature_install_check_update_rollback below (stage 2) and,
#     at slice level, throughout scripts/test/test_update.sh's
#     case_config_change_warning_* cases — not duplicated here beyond the
#     one flow needed for the whole-feature narrative.
#
# R2 (one-command install):
#   - Exercised whole-feature style below: a real install.sh run producing a
#     working, --version-probing binary, chained into the real installed
#     updater. Slice-level negative paths (unsupported platform exit 2,
#     download failure exit 3, checksum mismatch exit 4, probe failure
#     exit 5, unknown-flag/invalid-pin exit 2) are already covered
#     end-to-end by test_detect_platform.sh and test_install_failures.sh —
#     referenced, not duplicated, here.
#   - The interactive-prompt / --with-updater / --no-updater / non-TTY
#     default matrix is covered by test_updater.sh.
#
# R3 (optional background-service setup):
#   - Full plist/unit rendering, token-file-missing warning, and real-service
#     non-interference are covered by test_install_service.sh. This file
#     reuses --with-service once, inside the whole-feature flow, specifically
#     to prove the install-time service integrates with a *subsequent real
#     update* (restart-on-update) — a combination no other test exercises.
#
# R4 (version check & update script):
#   - up-to-date / update-available / updated / rollback-on-error and their
#     exit codes (0/10/1) are covered exhaustively by test_update.sh (against
#     a hand-placed fixture binary). The whole-feature flow below drives the
#     same contract but through the binary and updater that install.sh
#     itself produced, via the updater's real sibling-discovery (no --bin
#     flag) — the missing link between "install.sh works" and "update.sh
#     works" as a continuous user/agent journey.
#
# R5 (configuration-change warning):
#   - Warning-present and warning-absent cases (including the boundary and
#     mid-line-mention negatives) are covered by test_update.sh. The
#     whole-feature flow below carries one config-changing release through
#     both --check and apply so the warning's presence across the *whole*
#     journey (not just an isolated updater invocation) is proven.
#
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

REPO_ROOT="$(cd "$T_DIR/../.." && pwd)"
RELEASE_WORKFLOW="$REPO_ROOT/.github/workflows/release.yaml"
MAKEFILE="$REPO_ROOT/Makefile"
RELEASES_LIST_URL="$API_BASE/releases?per_page=100"
UPDATE_SH_REAL="$(cd "$T_DIR/.." && pwd)/update.sh"
RUN_WITH_TOKENS_REAL="$(cd "$T_DIR/../.." && pwd)/run-with-tokens.sh"
RUN_WITH_TOKENS_URL="https://raw.githubusercontent.com/provectus/slack-mcp-server/master/run-with-tokens.sh"

# =============================================================================
# R1 — static release-pipeline assertions (grep-based; no network, no CI run)
# =============================================================================

# @regression
case_r1_release_pipeline_contract() {
  # Trigger: pv-v* tags only. Upstream's v* tags must stay inert — assert the
  # trigger block contains the fork pattern and nothing that would also match
  # a bare upstream "v1.2.3" tag.
  assert_file "$RELEASE_WORKFLOW"
  assert_contains "$RELEASE_WORKFLOW" "tags:"
  assert_contains "$RELEASE_WORKFLOW" "pv-v*"
  assert_not_contains "$RELEASE_WORKFLOW" "- 'v*'"
  assert_not_contains "$RELEASE_WORKFLOW" "- \"v*\""

  # Assets: ready-to-run programs for every supported platform (6 binaries
  # via the wildcard) plus checksums and license — nothing DXT/npm/docker.
  assert_contains "$RELEASE_WORKFLOW" "make build-all-platforms"
  assert_contains "$RELEASE_WORKFLOW" "build/slack-mcp-server-*"
  assert_contains "$RELEASE_WORKFLOW" "checksums.txt"
  assert_contains "$RELEASE_WORKFLOW" "LICENSE"
  assert_not_contains "$RELEASE_WORKFLOW" "npm"
  assert_not_contains "$RELEASE_WORKFLOW" "dxt"
  assert_not_contains "$RELEASE_WORKFLOW" "docker"

  # The Makefile's cross-compile matrix actually spans macOS, Linux, Windows
  # x2 arches (the "each supported platform" the workflow's wildcard covers).
  assert_contains "$MAKEFILE" "OSES = darwin linux windows"
  assert_contains "$MAKEFILE" "ARCHS = amd64 arm64"

  # release target pushes to the fork remote (the historical bug this spec
  # fixes was pushing tags to origin=upstream) and validates the tag format.
  assert_contains "$MAKEFILE" "git push fork"
  assert_contains "$MAKEFILE" "pv-v"

  # Original project's distribution channels: removed, not merely unused.
  assert_no_file "$REPO_ROOT/.github/workflows/release-image.yaml"
  assert_no_file "$REPO_ROOT/npm"
  assert_no_file "$REPO_ROOT/manifest-dxt.json"
  assert_not_contains "$MAKEFILE" "npm-publish"
  assert_not_contains "$MAKEFILE" "npm-copy-binaries"
  assert_not_contains "$MAKEFILE" "build-dxt"
}

# =============================================================================
# R2 + R3 + R4 + R5 — whole-feature journey: install -> check -> update
# (with a config-changing release in range) -> rollback on a broken release.
# =============================================================================

# af_sandbox — t_sandbox plus: launchctl/systemctl/uname PATH stubs pinned to
# a fixed platform (darwin-arm64, so the flow is deterministic regardless of
# which OS actually runs the harness); a real sibling scripts/update.sh and
# repo-root run-with-tokens.sh (so install.sh installs and later chains into
# the ACTUAL scripts under test, not stand-in fixtures); and a prepared
# ~/.ssh/slack_tokens so --with-service can complete.
af_sandbox() {
  t_sandbox
  SERVICE_SHIM_LOG="$WORK/service.log"
  : >"$SERVICE_SHIM_LOG"
  AF_SERVICE_PRESENT=0
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/launchctl"
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/systemctl"
  cp "$T_DIR/uname_shim.sh" "$SHIMDIR/uname"
  chmod +x "$SHIMDIR/launchctl" "$SHIMDIR/systemctl" "$SHIMDIR/uname"
  UNAME_SHIM_S=Darwin
  UNAME_SHIM_M=arm64
  AF_ASSET="slack-mcp-server-darwin-arm64"
  BIN="$PREFIX/slack-mcp-server"
  UPDATER="$PREFIX/slack-mcp-update"
  PLIST="$HOME/Library/LaunchAgents/com.slack-mcp-server.plist"
  cp "$UPDATE_SH_REAL" "$WORK/scripts/update.sh"
  cp "$RUN_WITH_TOKENS_REAL" "$WORK/run-with-tokens.sh"
  mkdir -p "$HOME/.ssh"
  printf 'SLACK_MCP_XOXP_TOKEN=xoxp-test\n' >"$HOME/.ssh/slack_tokens"
}

# af_stage_latest <tag> [bad-banner] — (re)build the darwin-arm64 asset,
# checksums.txt and release JSON for <tag>, and map /releases/latest plus
# both download URLs to it. Safe to call more than once per case: it unmaps
# the previous /releases/latest mapping first (the curl shim matches the
# FIRST mapped line for a URL, so a stale mapping would otherwise win).
af_stage_latest() {
  local tag="$1" bad="${2:-}"
  local dir="$WORK/assets/$tag"
  mkdir -p "$dir"
  local tpl="$FIXTURES_DIR/fake-binary.tpl"
  [ "$bad" = "bad-banner" ] && tpl="$FIXTURES_DIR/fake-binary-bad.tpl"
  sed "s/@TAG@/$tag/g" "$tpl" >"$dir/$AF_ASSET"
  chmod +x "$dir/$AF_ASSET"
  printf '%s  %s\n' "$(sha256_of "$dir/$AF_ASSET")" "$AF_ASSET" >"$dir/checksums.txt"
  sed "s/@TAG@/$tag/g" "$FIXTURES_DIR/release.tpl.json" >"$dir/release.json"
  unmap_url "$API_BASE/releases/latest"
  map_url "$API_BASE/releases/latest" "$dir/release.json"
  map_url "$DL_BASE/$tag/$AF_ASSET" "$dir/$AF_ASSET"
  map_url "$DL_BASE/$tag/checksums.txt" "$dir/checksums.txt"
}

# af_tamper_latest_asset <tag> — corrupt the already-staged asset for <tag>
# so its sha256 no longer matches its own checksums.txt (checksums.txt was
# already written by af_stage_latest, so this must run after it).
af_tamper_latest_asset() {
  printf '# tampered\n' >>"$WORK/assets/$1/$AF_ASSET"
}

# af_map_releases_list <tag=body> ... — releases-list.json for the
# CONFIG-CHANGE scan (\r\n inside body becomes real newlines via update.sh's
# unescape). Safe to call more than once per case (unmaps first).
af_map_releases_list() {
  local out="$WORK/releases-list.json" sep="" e tag body
  unmap_url "$RELEASES_LIST_URL"
  printf '[' >"$out"
  for e in "$@"; do
    tag="${e%%=*}"
    body="${e#*=}"
    printf '%s{"tag_name":"%s","name":"%s","body":"%s"}' "$sep" "$tag" "$tag" "$body" >>"$out"
    sep=","
  done
  printf ']\n' >>"$out"
  map_url "$RELEASES_LIST_URL" "$out"
}

# af_run_install <outfile> [args...] — run the real install.sh; rc in $IRC.
af_run_install() {
  local out="$1"
  shift
  IRC=0
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    SERVICE_SHIM_LOG="$SERVICE_SHIM_LOG" SERVICE_SHIM_PRESENT="$AF_SERVICE_PRESENT" \
    UNAME_SHIM_S="$UNAME_SHIM_S" UNAME_SHIM_M="$UNAME_SHIM_M" \
    bash "$INSTALL" --prefix "$PREFIX" "$@" >"$out" 2>&1 || IRC=$?
}

# af_run_update <outfile> [args...] — run the ACTUAL installed
# $PREFIX/slack-mcp-update directly, with no --bin, so its sibling-discovery
# of $PREFIX/slack-mcp-server is what is under test; rc in $URC.
af_run_update() {
  local out="$1"
  shift
  URC=0
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    SERVICE_SHIM_LOG="$SERVICE_SHIM_LOG" SERVICE_SHIM_PRESENT="$AF_SERVICE_PRESENT" \
    UNAME_SHIM_S="$UNAME_SHIM_S" UNAME_SHIM_M="$UNAME_SHIM_M" \
    "$UPDATER" "$@" >"$out" 2>&1 || URC=$?
}

# af_assert_probe <tag> — $BIN --version reports exactly <tag> on line 1.
af_assert_probe() {
  local line
  line="$("$BIN" --version | head -n 1)"
  [ "$line" = "slack-mcp-server $1" ] || {
    echo "ASSERT: expected 'slack-mcp-server $1', got: $line"
    return 1
  }
}

# af_service_log_lines — current line count of the launchctl/systemctl stub
# log, for before/after deltas (asserting check-only touches no service).
af_service_log_lines() {
  wc -l <"$SERVICE_SHIM_LOG" | tr -d ' '
}

# @regression
case_whole_feature_install_check_update_rollback() {
  af_sandbox

  # --- 1. R2 + R3: one-command install, with the updater and the service ---
  af_stage_latest pv-v1.0.0
  af_run_install "$WORK/out-install" --with-updater --with-service
  assert_rc 0 "$IRC" || {
    cat "$WORK/out-install"
    return 1
  }

  assert_exec "$BIN"
  af_assert_probe pv-v1.0.0
  assert_contains "$WORK/out-install" "INSTALLED=pv-v1.0.0"

  # updater is the REAL script (sibling-preferred, never downloaded)
  assert_exec "$UPDATER"
  assert_contains "$UPDATER" "updater for slack-mcp-server"
  assert_not_contains "$CURL_SHIM_LOG" "$UPDATE_SH_URL"

  # background service actually configured via the (stubbed) real flow
  assert_file "$PLIST"
  assert_contains "$WORK/out-install" "Service started: com.slack-mcp-server (launchd)"
  assert_contains "$SERVICE_SHIM_LOG" "launchctl kickstart -k gui/$(id -u)/com.slack-mcp-server"
  assert_not_contains "$CURL_SHIM_LOG" "$RUN_WITH_TOKENS_URL"

  local svc_lines_after_install
  svc_lines_after_install="$(af_service_log_lines)"

  # --- 2. R4: freshly installed at "latest" -> check-only says so ----------
  AF_SERVICE_PRESENT=1
  af_run_update "$WORK/out-check1" --check
  assert_rc 0 "$URC" || {
    cat "$WORK/out-check1"
    return 1
  }
  assert_contains "$WORK/out-check1" "RESULT=up-to-date"
  assert_contains "$WORK/out-check1" "INSTALLED=pv-v1.0.0"
  assert_contains "$WORK/out-check1" "LATEST=pv-v1.0.0"
  assert_contains "$WORK/out-check1" "CONFIG_CHANGES=0"
  af_assert_probe pv-v1.0.0
  [ "$(af_service_log_lines)" = "$svc_lines_after_install" ] || {
    echo "ASSERT: check-only (up-to-date) must not touch the service"
    return 1
  }

  # --- 3. R4 + R5: a newer release ships, with a config-changing release --
  # in the (installed, latest] range. --check reports and warns; changes
  # nothing.
  af_stage_latest pv-v1.2.0
  af_map_releases_list \
    'pv-v1.2.0=routine improvements\r\nnothing else' \
    'pv-v1.1.0=feature work\r\nCONFIG-CHANGE: re-run install.sh --with-service after updating\r\nmore notes' \
    'pv-v1.0.0=old notes, at the floor: must not count'

  af_run_update "$WORK/out-check2" --check
  assert_rc 10 "$URC" || {
    cat "$WORK/out-check2"
    return 1
  }
  assert_contains "$WORK/out-check2" "RESULT=update-available"
  assert_contains "$WORK/out-check2" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service after updating"
  assert_contains "$WORK/out-check2" "CONFIG_CHANGES=1"
  af_assert_probe pv-v1.0.0 # check-only changed nothing
  [ "$(af_service_log_lines)" = "$svc_lines_after_install" ] || {
    echo "ASSERT: --check must not touch the service"
    return 1
  }

  # --- 4. R4 + R5: apply — binary really replaced, warning still shown, ---
  # service restarted onto the new version.
  af_run_update "$WORK/out-apply"
  assert_rc 0 "$URC" || {
    cat "$WORK/out-apply"
    return 1
  }
  assert_contains "$WORK/out-apply" "RESULT=updated"
  assert_contains "$WORK/out-apply" \
    "WARNING (pv-v1.1.0): re-run install.sh --with-service after updating"
  assert_contains "$WORK/out-apply" "CONFIG_CHANGES=1"
  assert_contains "$WORK/out-apply" "Updated: pv-v1.0.0 -> pv-v1.2.0"
  assert_contains "$WORK/out-apply" "Service restarted: com.slack-mcp-server (launchd)"
  af_assert_probe pv-v1.2.0 # the actually-installed binary now reports the new version
  assert_no_file "$BIN.bak"
  assert_no_file "$PREFIX/.slack-mcp-server.new"

  # --- 5. R4 negative: a broken next release must roll back cleanly -------
  local pre_sha
  pre_sha="$(sha256_of "$BIN")"
  af_stage_latest pv-v1.3.0
  af_tamper_latest_asset pv-v1.3.0 # checksum in checksums.txt no longer matches
  af_map_releases_list 'pv-v1.3.0=patch release, no config changes'

  af_run_update "$WORK/out-broken"
  assert_rc 1 "$URC"
  assert_contains "$WORK/out-broken" "ERROR: checksum mismatch for $AF_ASSET"
  assert_contains "$WORK/out-broken" "RESULT=error"
  [ "$(sha256_of "$BIN")" = "$pre_sha" ] || {
    echo "ASSERT: installed binary changed despite a failed update"
    return 1
  }
  af_assert_probe pv-v1.2.0 # still the last GOOD version, not bricked
  assert_no_file "$BIN.bak"
  assert_no_file "$PREFIX/.slack-mcp-server.new"
  assert_empty_dir "$ITMP"
}

t_case r1_release_pipeline_contract case_r1_release_pipeline_contract
t_case whole_feature_install_check_update_rollback case_whole_feature_install_check_update_rollback
t_done
