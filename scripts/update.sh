#!/usr/bin/env bash
# update.sh — updater for slack-mcp-server, installed by install.sh as
# `slack-mcp-update` next to the binary.
#
# Usage:
#   slack-mcp-update [--check] [--bin <path>]
#
# Compares the installed binary's --version against the latest pv-v* release
# of provectus/slack-mcp-server. When an update exists it prints WARNING
# blocks for every `CONFIG-CHANGE:` line found in the release notes of the
# releases in (installed, latest], then (unless --check) downloads the new
# binary to a same-directory staging file, verifies its sha256 against the
# release checksums.txt, probes it, atomically swaps it into place (with
# .bak rollback on any failure), and restarts the background service when
# one is configured (launchd on macOS, systemd user unit on Linux).
#
# Output ends with machine-readable lines (each on its own line):
#   INSTALLED=<tag>
#   LATEST=<tag>
#   RESULT=up-to-date|updated|update-available|error
#   CONFIG_CHANGES=<n>
#
# Exit codes: 0 up-to-date/updated, 10 update available (--check only), 1 error.
#
# Must stay bash 3.2 compatible (macOS system bash) and shellcheck-clean.

set -euo pipefail

REPO_API_URL="https://api.github.com/repos/provectus/slack-mcp-server"
BINARY_NAME="slack-mcp-server"
SERVICE_LABEL="com.slack-mcp-server"

# Globals consulted by emit_machine / fail_update.
TMP_DIR=""
STAGING=""
BAK_PATH=""
BIN_PATH=""
SWAP_STARTED=0
INSTALLED_TAG=""
LATEST_TAG=""
CONFIG_COUNT=0

log() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

usage() {
  cat <<'EOF'
slack-mcp-update — updater for slack-mcp-server

Usage:
  slack-mcp-update [--check] [--bin <path>]

Options:
  --check       Report whether an update is available; change nothing.
  --bin <path>  Path to the installed slack-mcp-server binary
                (default: slack-mcp-server next to this script).
  -h, --help    Show this help

Environment:
  GITHUB_TOKEN  Sent as Authorization header to GitHub when set (higher rate limits)

Output ends with machine-readable lines:
  INSTALLED=<tag>  LATEST=<tag>
  RESULT=up-to-date|updated|update-available|error  CONFIG_CHANGES=<n>

Exit codes:
  0 up-to-date or updated   10 update available (--check only)   1 error
EOF
}

# emit_machine <result> — print the machine-readable block.
emit_machine() {
  printf 'INSTALLED=%s\n' "${INSTALLED_TAG:-unknown}"
  printf 'LATEST=%s\n' "${LATEST_TAG:-unknown}"
  printf 'RESULT=%s\n' "$1"
  printf 'CONFIG_CHANGES=%s\n' "${CONFIG_COUNT:-0}"
}

# die <message> — error out: message, machine block with RESULT=error, exit 1.
die() {
  err "$1"
  emit_machine error
  exit 1
}

# fail_update <message> — die, first rolling back a started swap and
# removing the staging file.
fail_update() {
  if [ "$SWAP_STARTED" -eq 1 ] && [ -n "$BAK_PATH" ] && [ -f "$BAK_PATH" ]; then
    if mv -f "$BAK_PATH" "$BIN_PATH"; then
      warn "restored the previous binary from ${BAK_PATH}"
    else
      warn "could not restore ${BAK_PATH} to ${BIN_PATH} — fix manually"
    fi
  fi
  if [ -n "$STAGING" ]; then
    rm -f "$STAGING"
  fi
  die "$1"
}

# resolve_path <path> — print <path> with symlinks in the final component
# resolved and the directory made absolute (no realpath on older macOS).
resolve_path() {
  local p="$1" i=0 target dir
  while [ -L "$p" ] && [ "$i" -lt 40 ]; do
    target="$(readlink "$p")"
    case "$target" in
      /*) p="$target" ;;
      *) p="$(dirname "$p")/$target" ;;
    esac
    i=$((i + 1))
  done
  dir="$(cd "$(dirname "$p")" && pwd)"
  printf '%s/%s\n' "$dir" "$(basename "$p")"
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

# semver_cmp <a> <b> — numeric compare of X.Y.Z versions; prints lt|eq|gt.
semver_cmp() {
  local a1 a2 a3 b1 b2 b3
  IFS=. read -r a1 a2 a3 <<<"$1"
  IFS=. read -r b1 b2 b3 <<<"$2"
  local x y
  for x in "$a1:$b1" "$a2:$b2" "$a3:$b3"; do
    y="${x#*:}"
    x="${x%%:*}"
    if [ "$x" -lt "$y" ]; then
      echo lt
      return 0
    fi
    if [ "$x" -gt "$y" ]; then
      echo gt
      return 0
    fi
  done
  echo eq
}

# version_in_range <ver> <installed-or-empty> <latest> — rc 0 iff
# ver is in (installed, latest]. Empty installed = unknown/older than all.
version_in_range() {
  if [ -n "$2" ]; then
    [ "$(semver_cmp "$1" "$2")" = "gt" ] || return 1
  fi
  [ "$(semver_cmp "$1" "$3")" != "gt" ]
}

# fetch <url> <output-file> — curl with optional $GITHUB_TOKEN auth. rc = curl rc.
fetch() {
  local url="$1"
  local out="$2"
  local -a args=(-fsSL --retry 2 --connect-timeout 15 -o "$out")
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

# json_unescape — stdin: content of a JSON string (\n-escaped); stdout:
# unescaped text (real newlines). Handles \\ so "C:\\path\\node" stays intact.
json_unescape() {
  awk 'BEGIN { esc = sprintf("%c", 1) }
  {
    gsub(/\\\\/, esc)
    gsub(/\\r/, "")
    gsub(/\\n/, "\n")
    gsub(/\\t/, "\t")
    gsub(/\\"/, "\"")
    gsub(esc, "\\\\")
    print
  }'
}

# probe_version_line <binary> — prints line 1 of `<binary> --version` iff it
# starts with "slack-mcp-server "; rc 1 otherwise.
probe_version_line() {
  local out
  out="$("$1" --version 2>/dev/null | head -n 1)" || true
  case "$out" in
    "${BINARY_NAME} "*)
      printf '%s\n' "$out"
      return 0
      ;;
  esac
  return 1
}

# expected_sha256 <checksums.txt> <asset-name> — prints the recorded hash
# (handles both "hash  name" and "hash *name"); empty output if absent.
expected_sha256() {
  awk -v n="$2" '$2 == n || $2 == "*" n { print $1; exit }' "$1"
}

# file_sha256 <file> — prints the file's sha256; rc 1 if no tool available.
file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    err "no sha256 tool found (need sha256sum or shasum)"
    return 1
  fi
}

# scan_config_changes <releases.json> <installed-ver-or-empty> <latest-ver> —
# prints "tag<TAB>note" for every `CONFIG-CHANGE:` note in the release notes
# of pv-v releases whose version is in (installed, latest].
scan_config_changes() {
  if command -v jq >/dev/null 2>&1; then
    scan_config_changes_jq "$@"
  else
    scan_config_changes_fallback "$@"
  fi
}

scan_config_changes_jq() {
  local json="$1" installed="$2" latest="$3"
  local tag ver
  jq -r '.[] | .tag_name // empty' "$json" 2>/dev/null |
    while IFS= read -r tag; do
      validate_tag "$tag" || continue
      ver="${tag#pv-v}"
      version_in_range "$ver" "$installed" "$latest" || continue
      jq -r --arg t "$tag" 'map(select(.tag_name == $t)) | .[0].body // ""' \
        "$json" 2>/dev/null |
        tr -d '\r' |
        grep '^CONFIG-CHANGE:' |
        sed 's/^CONFIG-CHANGE:[[:space:]]*//' |
        while IFS= read -r note; do
          printf '%s\t%s\n' "$tag" "$note"
        done || true
    done
}

# Fallback without jq: walk "tag_name"/"body" string fields in order (GitHub
# emits tag_name before body within each release object), unescape the
# \n-escaped body, then grep it. Escaped quotes inside bodies are consumed
# by the (\\.|[^"\\])* field match, so they cannot break the pairing.
scan_config_changes_fallback() {
  local json="$1" installed="$2" latest="$3"
  local cur_tag="" line value ver
  grep -oE '"(tag_name|body)"[[:space:]]*:[[:space:]]*"(\\.|[^"\\])*"' "$json" 2>/dev/null |
    while IFS= read -r line; do
      value="$(printf '%s\n' "$line" |
        sed 's/^"[a-z_]*"[[:space:]]*:[[:space:]]*"//; s/"$//')"
      case "$line" in
        '"tag_name"'*)
          cur_tag="$value"
          ;;
        '"body"'*)
          validate_tag "$cur_tag" || continue
          ver="${cur_tag#pv-v}"
          version_in_range "$ver" "$installed" "$latest" || continue
          printf '%s\n' "$value" | json_unescape |
            tr -d '\r' |
            grep '^CONFIG-CHANGE:' |
            sed 's/^CONFIG-CHANGE:[[:space:]]*//' |
            while IFS= read -r note; do
              printf '%s\t%s\n' "$cur_tag" "$note"
            done || true
          ;;
      esac
    done || true
}

# restart_service — restart the background service if one is configured;
# absence of a service is an info line, never an error.
restart_service() {
  local uid
  case "$(uname -s)" in
    Darwin)
      uid="$(id -u)"
      if launchctl print "gui/${uid}/${SERVICE_LABEL}" >/dev/null 2>&1; then
        if launchctl kickstart -k "gui/${uid}/${SERVICE_LABEL}" >/dev/null 2>&1; then
          log "Service restarted: ${SERVICE_LABEL} (launchd)"
        else
          warn "failed to restart launchd service ${SERVICE_LABEL}; restart it manually"
        fi
      else
        log "No background service detected; skipping restart."
      fi
      ;;
    Linux)
      if systemctl --user is-enabled "$BINARY_NAME" >/dev/null 2>&1; then
        if systemctl --user restart "$BINARY_NAME" >/dev/null 2>&1; then
          log "Service restarted: ${BINARY_NAME} (systemd user)"
        else
          warn "failed to restart systemd user service ${BINARY_NAME}; restart it manually"
        fi
      else
        log "No background service detected; skipping restart."
      fi
      ;;
    *)
      log "No background service detected; skipping restart."
      ;;
  esac
}

main() {
  local check=0 bin=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -h | --help)
        usage
        return 0
        ;;
      --check)
        check=1
        shift
        ;;
      --bin)
        if [[ $# -lt 2 ]]; then
          usage >&2
          die "--bin requires a path"
        fi
        bin="$2"
        shift 2
        ;;
      *)
        usage >&2
        die "unknown option: $1"
        ;;
    esac
  done

  # --- binary discovery -------------------------------------------------
  if [[ -z "$bin" ]]; then
    local self
    self="$(resolve_path "${BASH_SOURCE[0]:-$0}")"
    bin="$(dirname "$self")/${BINARY_NAME}"
  fi
  case "$bin" in
    /*) ;;
    *) bin="$(pwd)/$bin" ;;
  esac
  if [[ ! -f "$bin" || ! -x "$bin" ]]; then
    die "binary not found or not executable: ${bin} (use --bin <path>)"
  fi
  BIN_PATH="$bin"

  # --- installed version ------------------------------------------------
  local ver_line installed_ver=""
  if ! ver_line="$(probe_version_line "$bin")"; then
    die "could not read the installed version (${bin} --version probe failed)"
  fi
  INSTALLED_TAG="$(printf '%s\n' "$ver_line" | awk '{print $2}')"
  if validate_tag "$INSTALLED_TAG"; then
    installed_ver="${INSTALLED_TAG#pv-v}"
  else
    log "NOTE: installed version '${INSTALLED_TAG}' is not a pv-v release tag;"
    log "      assuming it is older than the latest release and proceeding."
  fi
  log "Installed: ${INSTALLED_TAG} (${bin})"

  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "${TMP_DIR:-}"' EXIT

  # --- latest release ---------------------------------------------------
  if ! fetch "${REPO_API_URL}/releases/latest" "$TMP_DIR/latest.json"; then
    die "failed to query the GitHub releases API (${REPO_API_URL}/releases/latest)"
  fi
  if ! LATEST_TAG="$(parse_tag_name "$TMP_DIR/latest.json")"; then
    die "could not parse tag_name from the API response"
  fi
  if ! validate_tag "$LATEST_TAG"; then
    die "latest release has unexpected tag '${LATEST_TAG}' (expected pv-vX.Y.Z)"
  fi
  local latest_ver="${LATEST_TAG#pv-v}"
  log "Latest:    ${LATEST_TAG}"

  if [[ -n "$installed_ver" ]] &&
    [[ "$(semver_cmp "$latest_ver" "$installed_ver")" != "gt" ]]; then
    log "Already up to date."
    emit_machine up-to-date
    return 0
  fi

  # --- config-change scan over (installed, latest] ------------------------
  if ! fetch "${REPO_API_URL}/releases?per_page=100" "$TMP_DIR/releases.json"; then
    die "failed to list releases for the config-change scan (${REPO_API_URL}/releases)"
  fi
  local changes=""
  changes="$(scan_config_changes "$TMP_DIR/releases.json" "$installed_ver" "$latest_ver")" || true
  CONFIG_COUNT=0
  if [[ -n "$changes" ]]; then
    CONFIG_COUNT="$(printf '%s\n' "$changes" | grep -c '')"
    local w_tag w_note
    while IFS=$'\t' read -r w_tag w_note; do
      printf 'WARNING (%s): %s\n' "$w_tag" "$w_note"
    done <<<"$changes"
  fi

  log "Update available: ${INSTALLED_TAG} -> ${LATEST_TAG}"

  if [[ "$check" -eq 1 ]]; then
    log "Run slack-mcp-update (without --check) to apply."
    emit_machine update-available
    return 10
  fi

  # --- apply: download to same-dir staging, verify, probe, swap ----------
  local platform
  # shellcheck disable=SC2119  # no args: use the real uname os/arch
  if ! platform="$(detect_platform)"; then
    die "cannot update on this platform"
  fi
  local asset="${BINARY_NAME}-${platform}"
  local bin_url sums_url
  if ! bin_url="$(parse_asset_url "$TMP_DIR/latest.json" "$asset")"; then
    die "release ${LATEST_TAG} has no asset ${asset}"
  fi
  if ! sums_url="$(parse_asset_url "$TMP_DIR/latest.json" "checksums.txt")"; then
    die "release ${LATEST_TAG} has no checksums.txt asset"
  fi

  local bin_dir
  bin_dir="$(dirname "$bin")"
  STAGING="${bin_dir}/.${BINARY_NAME}.new"
  rm -f "$STAGING"

  log "Downloading ${asset} (${LATEST_TAG})"
  if ! fetch "$bin_url" "$STAGING"; then
    fail_update "failed to download ${bin_url}"
  fi
  if ! fetch "$sums_url" "$TMP_DIR/checksums.txt"; then
    fail_update "failed to download ${sums_url}"
  fi

  local expected actual
  expected="$(expected_sha256 "$TMP_DIR/checksums.txt" "$asset")"
  if [[ -z "$expected" ]]; then
    fail_update "no entry for ${asset} in checksums.txt"
  fi
  if ! actual="$(file_sha256 "$STAGING")"; then
    fail_update "cannot compute sha256 of the downloaded binary"
  fi
  if [[ "$actual" != "$expected" ]]; then
    fail_update "checksum mismatch for ${asset} (${LATEST_TAG})"
  fi
  log "Checksum OK"

  chmod +x "$STAGING"
  if ! probe_version_line "$STAGING" >/dev/null; then
    fail_update "downloaded binary failed the --version probe"
  fi

  BAK_PATH="${bin}.bak"
  if ! mv -f "$bin" "$BAK_PATH"; then
    fail_update "failed to move the current binary aside"
  fi
  SWAP_STARTED=1
  if ! mv -f "$STAGING" "$bin"; then
    fail_update "failed to move the new binary into place"
  fi
  local new_line
  if ! new_line="$(probe_version_line "$bin")"; then
    fail_update "swapped binary failed the --version re-probe"
  fi
  rm -f "$BAK_PATH"
  SWAP_STARTED=0
  STAGING=""

  log "Updated: ${INSTALLED_TAG} -> ${LATEST_TAG} (${new_line})"
  restart_service
  emit_machine updated
  return 0
}

# Allow test harnesses to source this file without executing main.
if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then
  main "$@"
fi
