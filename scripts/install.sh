#!/usr/bin/env bash
# install.sh — one-command installer for slack-mcp-server (macOS/Linux).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/install.sh | bash
#   bash scripts/install.sh [options]
#
# Downloads the latest (or pinned) fork release binary for this platform,
# verifies its sha256 against the release checksums.txt, installs it to
# $PREFIX (default ~/.local/bin), and optionally installs the updater
# (slack-mcp-update) alongside it. With --with-service it also installs
# run-with-tokens.sh to ~/.local/share/slack-mcp-server/ and renders +
# activates the background service (launchd LaunchAgent on macOS, systemd
# user unit on Linux); a missing ~/.ssh/slack_tokens skips the service with
# a warning (the binary install still succeeds).
#
# Exit codes:
#   0  success (including updater-download warning)
#   2  unsupported platform or bad flags
#   3  download / GitHub API failure
#   4  checksum mismatch
#   5  post-install --version probe failure

set -euo pipefail

REPO_API_URL="https://api.github.com/repos/provectus/slack-mcp-server"
UPDATE_SH_URL="https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/update.sh"
RUN_WITH_TOKENS_URL="https://raw.githubusercontent.com/provectus/slack-mcp-server/master/run-with-tokens.sh"
TOKENS_SETUP_URL="https://github.com/provectus/slack-mcp-server/blob/master/SLACK_TOKENS_SETUP.md"
BINARY_NAME="slack-mcp-server"
UPDATER_NAME="slack-mcp-update"
SERVICE_LABEL="com.slack-mcp-server"
TMP_DIR=""

log() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

usage() {
  cat <<'EOF'
slack-mcp-server installer

Usage:
  curl -fsSL https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/install.sh | bash
  bash install.sh [options]

Options:
  --version <pv-vX.Y.Z>  Install a specific release (default: latest)
  --prefix <dir>         Install directory (default: $HOME/.local/bin)
  --with-updater         Install slack-mcp-update without prompting
  --no-updater           Skip slack-mcp-update without prompting
  --with-service         Also set up the background service (launchd on macOS,
                         systemd user unit on Linux); needs ~/.ssh/slack_tokens
  -h, --help             Show this help

Environment:
  PREFIX        Same as --prefix
  GITHUB_TOKEN  Sent as Authorization header to GitHub when set (higher rate limits)

Exit codes:
  0 success   2 unsupported platform / bad flags   3 download or API failure
  4 checksum mismatch   5 post-install probe failure
EOF
}

# detect_platform [os] [arch] — prints "<os>-<arch>" (darwin|linux, amd64|arm64).
# Args default to uname output; overridable for tests. Unsupported -> stderr, rc 2.
# shellcheck disable=SC2120  # args are passed by the test suite, not by this script
detect_platform() {
  local os_raw="${1:-$(uname -s)}"
  local arch_raw="${2:-$(uname -m)}"
  local os arch
  case "$os_raw" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    *)
      err "unsupported operating system: ${os_raw} (supported: macOS, Linux)"
      return 2
      ;;
  esac
  case "$arch_raw" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *)
      err "unsupported architecture: ${arch_raw} (supported: x86_64/amd64, aarch64/arm64)"
      return 2
      ;;
  esac
  printf '%s-%s\n' "$os" "$arch"
}

# validate_tag <tag> — rc 0 iff tag matches pv-vX.Y.Z.
validate_tag() {
  [[ "$1" =~ ^pv-v[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

# fetch <url> <output-file> — curl with optional $GITHUB_TOKEN auth. rc = curl rc.
fetch() {
  local url="$1"
  local out="$2"
  local -a args=(-fsSL --retry 2 --connect-timeout 15 --max-time 300 -o "$out")
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    args+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi
  curl "${args[@]}" "$url"
}

# parse_tag_name <release.json> — prints tag_name; rc 1 if absent.
parse_tag_name() {
  local json="$1"
  local tag=""
  if command -v jq >/dev/null 2>&1; then
    tag="$(jq -r '.tag_name // empty' "$json" 2>/dev/null)" || true
  else
    tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$json" | head -n 1)" || true
  fi
  [[ -n "$tag" ]] || return 1
  printf '%s\n' "$tag"
}

# parse_asset_url <release.json> <asset-name> — prints browser_download_url; rc 1 if absent.
parse_asset_url() {
  local json="$1"
  local name="$2"
  local url=""
  if command -v jq >/dev/null 2>&1; then
    url="$(jq -r --arg n "$name" \
      '[.assets[]? | select(.name == $n) | .browser_download_url][0] // empty' \
      "$json" 2>/dev/null)" || true
  else
    url="$(grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*"' "$json" |
      sed 's/.*"\(https[^"]*\)".*/\1/' |
      grep "/${name}\$" | head -n 1)" || true
  fi
  [[ -n "$url" ]] || return 1
  printf '%s\n' "$url"
}

# verify_checksum <dir> <file> — checks <dir>/<file> against <dir>/checksums.txt.
verify_checksum() {
  local dir="$1"
  local file="$2"
  local -a checker
  if command -v sha256sum >/dev/null 2>&1; then
    checker=(sha256sum -c) # linux
  elif command -v shasum >/dev/null 2>&1; then
    checker=(shasum -a 256 -c) # darwin
  else
    err "no sha256 tool found (need sha256sum or shasum)"
    return 1
  fi
  local pattern="^[0-9a-fA-F]{64}[[:space:]]+\\*?${file}\$"
  if ! grep -E "$pattern" "$dir/checksums.txt" >"$dir/checksums.one"; then
    err "no entry for ${file} in checksums.txt"
    return 1
  fi
  (cd "$dir" && "${checker[@]}" checksums.one >/dev/null 2>&1)
}

# install_updater <prefix> <tmpdir> — installs update.sh as <prefix>/slack-mcp-update.
# Prefers a sibling update.sh when running from a checkout; downloads otherwise.
# A missing download is a warning, not a failure (updater ships in a later release).
install_updater() {
  local prefix="$1"
  local tmpdir="$2"
  local dest="${prefix}/${UPDATER_NAME}"
  local self="${BASH_SOURCE[0]:-}"
  local sibling=""
  if [[ -n "$self" && -f "$self" ]]; then
    sibling="$(cd "$(dirname "$self")" && pwd)/update.sh"
  fi
  if [[ -n "$sibling" && -f "$sibling" ]]; then
    cp "$sibling" "$dest"
  elif fetch "$UPDATE_SH_URL" "$tmpdir/update.sh"; then
    mv -f "$tmpdir/update.sh" "$dest"
  else
    warn "could not download update.sh; updater not installed (re-run install.sh once it ships)"
    return 0
  fi
  chmod +x "$dest"
  log "Updater installed: ${dest}"
}

# install_run_script <share-dir> <tmpdir> — installs run-with-tokens.sh into
# <share-dir>. Prefers the checkout's copy (repo root, one level above this
# script) when present; downloads from raw master otherwise. rc 1 when the
# download fails.
install_run_script() {
  local share_dir="$1"
  local tmpdir="$2"
  local dest="${share_dir}/run-with-tokens.sh"
  local self="${BASH_SOURCE[0]:-}"
  local sibling=""
  if [[ -n "$self" && -f "$self" ]]; then
    sibling="$(cd "$(dirname "$self")/.." && pwd)/run-with-tokens.sh"
  fi
  mkdir -p "$share_dir"
  if [[ -n "$sibling" && -f "$sibling" ]]; then
    cp "$sibling" "$dest"
  elif fetch "$RUN_WITH_TOKENS_URL" "$tmpdir/run-with-tokens.sh"; then
    mv -f "$tmpdir/run-with-tokens.sh" "$dest"
  else
    return 1
  fi
  chmod +x "$dest"
  log "Run script installed: ${dest}"
}

# setup_service_darwin <run-script> <share-dir> <bin-path> — render the
# LaunchAgent plist with real paths (never the repo's placeholder template)
# and (re)bootstrap it. launchctl is resolved via PATH.
setup_service_darwin() {
  local run_script="$1"
  local share_dir="$2"
  local bin_path="$3"
  local plist="${HOME}/Library/LaunchAgents/${SERVICE_LABEL}.plist"
  mkdir -p "${HOME}/Library/LaunchAgents" "${HOME}/Library/Logs"
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${SERVICE_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>${run_script}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${share_dir}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${HOME}/Library/Logs/slack-mcp-server.log</string>
  <key>StandardErrorPath</key>
  <string>${HOME}/Library/Logs/slack-mcp-server.err.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>${HOME}</string>
    <key>PATH</key>
    <string>/usr/local/bin:/usr/bin:/bin</string>
    <key>SLACK_MCP_BIN</key>
    <string>${bin_path}</string>
  </dict>
</dict>
</plist>
EOF
  log "LaunchAgent rendered: ${plist}"
  local uid
  uid="$(id -u)"
  # Idempotent re-bootstrap: bootout fails harmlessly when nothing is loaded.
  launchctl bootout "gui/${uid}/${SERVICE_LABEL}" >/dev/null 2>&1 || true
  if ! launchctl bootstrap "gui/${uid}" "$plist"; then
    warn "launchctl bootstrap failed; load manually: launchctl bootstrap gui/${uid} ${plist}"
    return 0
  fi
  if launchctl kickstart -k "gui/${uid}/${SERVICE_LABEL}"; then
    log "Service started: ${SERVICE_LABEL} (launchd)"
  else
    warn "launchctl kickstart failed; start manually: launchctl kickstart -k gui/${uid}/${SERVICE_LABEL}"
  fi
}

# setup_service_linux <run-script> <bin-path> — render the systemd user unit
# and enable + start it. systemctl is resolved via PATH.
setup_service_linux() {
  local run_script="$1"
  local bin_path="$2"
  local unit_dir="${HOME}/.config/systemd/user"
  local unit="${unit_dir}/${BINARY_NAME}.service"
  mkdir -p "$unit_dir"
  cat >"$unit" <<EOF
[Unit]
Description=Slack MCP server (SSE transport)

[Service]
ExecStart=/bin/bash "${run_script}"
Environment="SLACK_MCP_BIN=${bin_path}"
Restart=on-failure

[Install]
WantedBy=default.target
EOF
  log "systemd user unit rendered: ${unit}"
  if systemctl --user daemon-reload && systemctl --user enable --now "$BINARY_NAME"; then
    log "Service started: ${BINARY_NAME} (systemd user)"
  else
    warn "systemctl --user setup failed; run manually: systemctl --user enable --now ${BINARY_NAME}"
  fi
  log "NOTE: to start at boot without an active login session, run:"
  log "  loginctl enable-linger $(id -un)"
}

# setup_service <bin-path> <tmpdir> — --with-service entry point. A missing
# ~/.ssh/slack_tokens skips everything service-related (no files rendered, no
# launchctl/systemctl calls) with a warning; the binary install already
# succeeded, so this always returns 0.
setup_service() {
  local bin_path="$1"
  local tmpdir="$2"
  local tokens_file="${HOME}/.ssh/slack_tokens"
  if [[ ! -f "$tokens_file" ]]; then
    warn "token file not found: ${tokens_file}"
    warn "service setup skipped — create it per SLACK_TOKENS_SETUP.md (${TOKENS_SETUP_URL}), then re-run: install.sh --with-service"
    return 0
  fi
  local share_dir="${HOME}/.local/share/slack-mcp-server"
  if ! install_run_script "$share_dir" "$tmpdir"; then
    warn "could not download run-with-tokens.sh; service not configured (re-run install.sh --with-service)"
    return 0
  fi
  local run_script="${share_dir}/run-with-tokens.sh"
  case "$(uname -s)" in
    Darwin) setup_service_darwin "$run_script" "$share_dir" "$bin_path" ;;
    Linux) setup_service_linux "$run_script" "$bin_path" ;;
  esac
}

main() {
  local pin_tag=""
  local prefix="${PREFIX:-$HOME/.local/bin}"
  local updater=""
  local with_service=0

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -h | --help)
        usage
        return 0
        ;;
      --version)
        if [[ $# -lt 2 ]]; then
          err "--version requires a value (e.g. pv-v1.0.0)"
          return 2
        fi
        pin_tag="$2"
        shift 2
        ;;
      --prefix)
        if [[ $# -lt 2 ]]; then
          err "--prefix requires a directory"
          return 2
        fi
        prefix="$2"
        shift 2
        ;;
      --with-updater)
        updater="yes"
        shift
        ;;
      --no-updater)
        updater="no"
        shift
        ;;
      --with-service)
        with_service=1
        shift
        ;;
      *)
        err "unknown option: $1"
        usage >&2
        return 2
        ;;
    esac
  done

  if [[ -n "$pin_tag" ]] && ! validate_tag "$pin_tag"; then
    err "invalid --version '${pin_tag}' (expected pv-vX.Y.Z, e.g. pv-v1.0.0)"
    return 2
  fi

  local platform
  # shellcheck disable=SC2119  # no args: use the real uname os/arch
  platform="$(detect_platform)" || return 2

  local release_url="${REPO_API_URL}/releases/latest"
  if [[ -n "$pin_tag" ]]; then
    release_url="${REPO_API_URL}/releases/tags/${pin_tag}"
  fi

  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "${TMP_DIR:-}"' EXIT

  log "Resolving release: ${release_url}"
  if ! fetch "$release_url" "$TMP_DIR/release.json"; then
    err "failed to query the GitHub releases API (${release_url})"
    return 3
  fi

  local tag
  if ! tag="$(parse_tag_name "$TMP_DIR/release.json")"; then
    err "could not parse tag_name from the API response"
    return 3
  fi

  local asset="${BINARY_NAME}-${platform}"
  local bin_url sums_url
  if ! bin_url="$(parse_asset_url "$TMP_DIR/release.json" "$asset")"; then
    err "release ${tag} has no asset ${asset}"
    return 3
  fi
  if ! sums_url="$(parse_asset_url "$TMP_DIR/release.json" "checksums.txt")"; then
    err "release ${tag} has no checksums.txt asset"
    return 3
  fi

  log "Downloading ${asset} (${tag})"
  if ! fetch "$bin_url" "$TMP_DIR/$asset"; then
    err "failed to download ${bin_url}"
    return 3
  fi
  if ! fetch "$sums_url" "$TMP_DIR/checksums.txt"; then
    err "failed to download ${sums_url}"
    return 3
  fi

  if ! verify_checksum "$TMP_DIR" "$asset"; then
    err "checksum verification failed for ${asset} (${tag})"
    return 4
  fi
  log "Checksum OK"

  mkdir -p "$prefix"
  local dest="${prefix}/${BINARY_NAME}"
  chmod +x "$TMP_DIR/$asset"

  local probe
  probe="$("$TMP_DIR/$asset" --version 2>/dev/null | head -n 1)" || true
  if [[ "$probe" != "${BINARY_NAME} "* ]]; then
    err "downloaded binary failed the --version probe; existing installation unchanged"
    return 5
  fi
  mv -f "$TMP_DIR/$asset" "$dest"
  log "Installed: ${dest} (${probe})"

  local want_updater="$updater"
  if [[ -z "$want_updater" ]]; then
    local ans=""
    if [[ -r /dev/tty ]] &&
      IFS= read -r -p "Install the updater (slack-mcp-update)? [Y/n] " ans </dev/tty; then
      case "$ans" in
        [nN]*) want_updater="no" ;;
        *) want_updater="yes" ;;
      esac
    else
      log "No TTY: installing the updater by default (pass --no-updater to skip)."
      want_updater="yes"
    fi
  fi
  if [[ "$want_updater" == "yes" ]]; then
    install_updater "$prefix" "$TMP_DIR"
  fi

  if [[ "$with_service" -eq 1 ]]; then
    setup_service "$dest" "$TMP_DIR"
  fi

  case ":$PATH:" in
    *":${prefix}:"*) ;;
    *)
      log "NOTE: ${prefix} is not in your PATH. Add it with:"
      log "  export PATH=\"${prefix}:\$PATH\""
      ;;
  esac

  log "INSTALLED=${tag}"
}

# Allow test harnesses to source this file without executing main.
if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then
  main "$@"
fi
