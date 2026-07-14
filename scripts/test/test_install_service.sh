#!/usr/bin/env bash
# test_install_service.sh — coverage for install.sh --with-service: macOS
# LaunchAgent plist rendering + bootstrap, Linux systemd user unit rendering
# + enable, the sibling run-with-tokens.sh copy path, and the token-file-
# missing skip (warning, nothing rendered, exit 0).
#
# Every case runs with launchctl, systemctl AND uname replaced by PATH stubs
# (s_sandbox) against a temp $HOME, so install.sh can never see the host
# platform or touch the real service managers. As a belt-and-suspenders
# check, the last case asserts the PID of the user's live launchd service
# (queried via /bin/launchctl directly) did not change across the file.
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

# URLs hardcoded in install.sh's service mode.
RUN_WITH_TOKENS_URL="https://raw.githubusercontent.com/provectus/slack-mcp-server/master/run-with-tokens.sh"
TOKENS_SETUP_URL="https://github.com/provectus/slack-mcp-server/blob/master/SLACK_TOKENS_SETUP.md"
RUN_WITH_TOKENS_SRC="$(cd "$T_DIR/../.." && pwd)/run-with-tokens.sh"

# real_service_pid — PID of the user's live launchd service, via the REAL
# launchctl (absolute path; the stubs never shadow this). Empty when absent
# or not on macOS.
real_service_pid() {
  [ "$(/usr/bin/uname -s 2>/dev/null || uname -s)" = "Darwin" ] || return 0
  [ -x /bin/launchctl ] || return 0
  /bin/launchctl print "gui/$(id -u)/com.slack-mcp-server" 2>/dev/null |
    sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*/\1/p' | head -n 1
}
REAL_PID_BEFORE="$(real_service_pid)"

# s_sandbox — t_sandbox plus the launchctl/systemctl/uname stubs and a fixed
# fake platform (darwin-arm64 by default; override UNAME_SHIM_*/S_ASSET
# before staging). Also sets the expected rendered-file paths.
s_sandbox() {
  t_sandbox
  SERVICE_SHIM_LOG="$WORK/service.log"
  : >"$SERVICE_SHIM_LOG"
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/launchctl"
  cp "$T_DIR/service_shim.sh" "$SHIMDIR/systemctl"
  cp "$T_DIR/uname_shim.sh" "$SHIMDIR/uname"
  chmod +x "$SHIMDIR/launchctl" "$SHIMDIR/systemctl" "$SHIMDIR/uname"
  UNAME_SHIM_S=Darwin
  UNAME_SHIM_M=arm64
  S_ASSET="slack-mcp-server-darwin-arm64"
  PLIST="$HOME/Library/LaunchAgents/com.slack-mcp-server.plist"
  UNIT="$HOME/.config/systemd/user/slack-mcp-server.service"
  RUN_SCRIPT="$HOME/.local/share/slack-mcp-server/run-with-tokens.sh"
  BIN="$PREFIX/slack-mcp-server"
}

# s_stage_latest <tag> — build the $S_ASSET release asset (shim platform, not
# host platform), its checksums.txt and the release JSON, and map
# /releases/latest plus both download URLs.
s_stage_latest() {
  local tag="$1"
  sed "s/@TAG@/$tag/g" "$FIXTURES_DIR/fake-binary.tpl" >"$WORK/assets/$S_ASSET"
  chmod +x "$WORK/assets/$S_ASSET"
  printf '%s  %s\n' "$(sha256_of "$WORK/assets/$S_ASSET")" "$S_ASSET" \
    >"$WORK/assets/checksums.txt"
  sed "s/@TAG@/$tag/g" "$FIXTURES_DIR/release.tpl.json" >"$WORK/release-$tag.json"
  map_url "$API_BASE/releases/latest" "$WORK/release-$tag.json"
  map_url "$DL_BASE/$tag/$S_ASSET" "$WORK/assets/$S_ASSET"
  map_url "$DL_BASE/$tag/checksums.txt" "$WORK/assets/checksums.txt"
}

# s_write_tokens — create a minimal ~/.ssh/slack_tokens in the sandbox HOME.
s_write_tokens() {
  mkdir -p "$HOME/.ssh"
  printf 'SLACK_MCP_XOXP_TOKEN=xoxp-test\n' >"$HOME/.ssh/slack_tokens"
}

# run_install_svc <outfile> [args...] — run install.sh --with-service with
# all stubs first on PATH; rc lands in $SRC (never fails the case by itself).
run_install_svc() {
  local out="$1"
  shift
  SRC=0
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    SERVICE_SHIM_LOG="$SERVICE_SHIM_LOG" SERVICE_SHIM_PRESENT=0 \
    UNAME_SHIM_S="$UNAME_SHIM_S" UNAME_SHIM_M="$UNAME_SHIM_M" \
    bash "$INSTALL" --prefix "$PREFIX" --no-updater --with-service "$@" \
    >"$out" 2>&1 || SRC=$?
}

# --- 1. macOS: plist rendered with real paths, bootout+bootstrap+kickstart ---

case_macos_service_render() {
  s_sandbox
  s_write_tokens
  s_stage_latest pv-v1.2.3
  map_url "$RUN_WITH_TOKENS_URL" "$RUN_WITH_TOKENS_SRC"

  run_install_svc "$WORK/out"
  assert_rc 0 "$SRC" || {
    cat "$WORK/out"
    return 1
  }
  assert_exec "$BIN"

  # run script downloaded (no sibling in $WORK) and executable
  assert_exec "$RUN_SCRIPT"
  assert_contains "$CURL_SHIM_LOG" "$RUN_WITH_TOKENS_URL"

  # plist rendered with real sandbox paths, no template placeholders
  assert_file "$PLIST"
  assert_contains "$PLIST" "<string>com.slack-mcp-server</string>"
  assert_contains "$PLIST" "<string>/bin/bash</string>"
  assert_contains "$PLIST" "<string>$RUN_SCRIPT</string>"
  assert_contains "$PLIST" "<string>$HOME/.local/share/slack-mcp-server</string>"
  assert_contains "$PLIST" "<key>RunAtLoad</key>"
  assert_contains "$PLIST" "<key>KeepAlive</key>"
  assert_contains "$PLIST" "<string>$HOME/Library/Logs/slack-mcp-server.log</string>"
  assert_contains "$PLIST" "<string>$HOME/Library/Logs/slack-mcp-server.err.log</string>"
  assert_contains "$PLIST" "<string>$HOME</string>"
  assert_contains "$PLIST" "<key>SLACK_MCP_BIN</key>"
  assert_contains "$PLIST" "<string>$BIN</string>"
  assert_not_contains "$PLIST" "/path/to"
  assert_not_contains "$PLIST" "YOUR_USERNAME"

  # the rendered plist is valid plist XML (plutil on macOS; skip elsewhere)
  if command -v plutil >/dev/null 2>&1; then
    plutil -lint "$PLIST" >/dev/null || {
      echo "ASSERT: plutil -lint rejected the rendered plist"
      plutil -lint "$PLIST" || true
      return 1
    }
  fi

  # bootout (ignored failure) -> bootstrap -> kickstart, all against the stub
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl bootout gui/$(id -u)/com.slack-mcp-server"
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl bootstrap gui/$(id -u) $PLIST"
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl kickstart -k gui/$(id -u)/com.slack-mcp-server"
  assert_not_contains "$SERVICE_SHIM_LOG" "systemctl"
  assert_no_file "$UNIT"
  assert_contains "$WORK/out" "Service started: com.slack-mcp-server (launchd)"
  assert_empty_dir "$ITMP"
}

# --- 2. Linux: unit rendered, daemon-reload + enable --now, linger note ------

case_linux_service_render() {
  s_sandbox
  UNAME_SHIM_S=Linux
  UNAME_SHIM_M=x86_64
  S_ASSET="slack-mcp-server-linux-amd64"
  s_write_tokens
  s_stage_latest pv-v1.2.3
  map_url "$RUN_WITH_TOKENS_URL" "$RUN_WITH_TOKENS_SRC"

  run_install_svc "$WORK/out"
  assert_rc 0 "$SRC" || {
    cat "$WORK/out"
    return 1
  }
  assert_exec "$BIN"
  assert_exec "$RUN_SCRIPT"

  assert_file "$UNIT"
  assert_contains "$UNIT" "[Unit]"
  assert_contains "$UNIT" "Description="
  assert_contains "$UNIT" "ExecStart=/bin/bash $RUN_SCRIPT"
  assert_contains "$UNIT" "Environment=SLACK_MCP_BIN=$BIN"
  assert_contains "$UNIT" "Restart=on-failure"
  assert_contains "$UNIT" "WantedBy=default.target"
  assert_not_contains "$UNIT" "/path/to"

  # unit passes systemd's own verifier when available (Linux CI; not macOS).
  # Offline --user verify needs XDG_RUNTIME_DIR, which headless CI runners
  # without a user session leave unset — point it at a sandbox dir so the
  # verifier can initialize; bad units still fail verify under this mode.
  if command -v systemd-analyze >/dev/null 2>&1; then
    local xdg_rt="${XDG_RUNTIME_DIR:-}"
    if [ -z "$xdg_rt" ]; then
      xdg_rt="$WORK/xdg-runtime"
      mkdir -p "$xdg_rt"
      chmod 700 "$xdg_rt"
    fi
    XDG_RUNTIME_DIR="$xdg_rt" systemd-analyze --user verify "$UNIT" || {
      echo "ASSERT: systemd-analyze verify rejected the rendered unit"
      return 1
    }
  fi

  assert_contains "$SERVICE_SHIM_LOG" "systemctl --user daemon-reload"
  assert_contains "$SERVICE_SHIM_LOG" "systemctl --user enable --now slack-mcp-server"
  assert_not_contains "$SERVICE_SHIM_LOG" "launchctl"
  assert_no_file "$PLIST"
  assert_contains "$WORK/out" "Service started: slack-mcp-server (systemd user)"
  assert_contains "$WORK/out" "loginctl enable-linger"
}

# --- 3. checkout run: sibling run-with-tokens.sh copied, not downloaded ------

case_sibling_run_script_preferred() {
  s_sandbox
  s_write_tokens
  s_stage_latest pv-v1.2.3
  # install.sh sits in $WORK/scripts/, so the "repo root" sibling is $WORK/
  cp "$RUN_WITH_TOKENS_SRC" "$WORK/run-with-tokens.sh"
  # RUN_WITH_TOKENS_URL deliberately unmapped: a download attempt would fail

  run_install_svc "$WORK/out"
  assert_rc 0 "$SRC" || {
    cat "$WORK/out"
    return 1
  }
  assert_exec "$RUN_SCRIPT"
  assert_not_contains "$CURL_SHIM_LOG" "$RUN_WITH_TOKENS_URL"
  assert_file "$PLIST"
  assert_contains "$SERVICE_SHIM_LOG" \
    "launchctl kickstart -k gui/$(id -u)/com.slack-mcp-server"
}

# --- 4. missing token file: binary installed, warning, nothing rendered ------

case_token_file_missing() {
  s_sandbox
  # no s_write_tokens: $HOME/.ssh/slack_tokens does not exist
  s_stage_latest pv-v1.2.3
  map_url "$RUN_WITH_TOKENS_URL" "$RUN_WITH_TOKENS_SRC"

  run_install_svc "$WORK/out"
  assert_rc 0 "$SRC" || {
    cat "$WORK/out"
    return 1
  }

  # binary install still succeeded, machine-readable line intact
  assert_exec "$BIN"
  assert_contains "$WORK/out" "Installed: $BIN"
  assert_contains "$WORK/out" "INSTALLED=pv-v1.2.3"

  # warning points to the token setup guide
  assert_contains "$WORK/out" "token file not found: $HOME/.ssh/slack_tokens"
  assert_contains "$WORK/out" "$TOKENS_SETUP_URL"

  # no half-configured service: nothing rendered, nothing downloaded,
  # no service-manager calls
  assert_no_file "$PLIST"
  assert_no_file "$UNIT"
  assert_no_file "$RUN_SCRIPT"
  assert_not_contains "$CURL_SHIM_LOG" "$RUN_WITH_TOKENS_URL"
  [ ! -s "$SERVICE_SHIM_LOG" ] || {
    echo "ASSERT: expected no launchctl/systemctl calls — stub log:"
    cat "$SERVICE_SHIM_LOG"
    return 1
  }
}

# --- 5. the user's real launchd service was never touched --------------------

case_real_service_untouched() {
  local after
  after="$(real_service_pid)"
  [ "$after" = "$REAL_PID_BEFORE" ] || {
    echo "ASSERT: real com.slack-mcp-server PID changed: '$REAL_PID_BEFORE' -> '$after'"
    return 1
  }
}

t_case macos_service_render case_macos_service_render
t_case linux_service_render case_linux_service_render
t_case sibling_run_script_preferred case_sibling_run_script_preferred
t_case token_file_missing case_token_file_missing
t_case real_service_untouched case_real_service_untouched
t_done
