#!/usr/bin/env bash
# lib.sh — shared helpers for the plain-sh test harness (no bats).
# Sourced by every test_*.sh. Must stay bash 3.2 compatible (macOS system bash).
#
# Model: each test file sources this lib, defines case_* functions, and runs
# them via `t_case <name> <fn>`. A case runs in a subshell with `set -e`; any
# failing command or assertion fails the case. `t_case` prints "PASS: <name>"
# or "FAIL: <name>" (plus the captured case output), and `t_done` at the end
# of the file exits nonzero if any case failed. Every case calls `t_sandbox`
# for an isolated mktemp HOME/prefix; a single trap removes everything.

set -u

T_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURES_DIR="$T_DIR/fixtures"
INSTALL_SH="$(cd "$T_DIR/.." && pwd)/install.sh"

# URLs hardcoded in install.sh (the shim keys fixtures by exact URL).
API_BASE="https://api.github.com/repos/provectus/slack-mcp-server"
DL_BASE="https://github.com/provectus/slack-mcp-server/releases/download"
UPDATE_SH_URL="https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/update.sh"

T_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/slack-mcp-shtest.XXXXXX")"
trap 'rm -rf "$T_ROOT"' EXIT

t_pass=0
t_fail=0
t_n=0

# --- case runner --------------------------------------------------------

# t_case <name> <function> — run one case in a `set -e` subshell.
#
# The subshell MUST run as a standalone command with its status read from $?
# afterwards: were it the condition of an `if` (or on either side of &&/||),
# bash would ignore `set -e` throughout the case — even though the case
# re-enables it — and bare failing assertions could no longer fail the case.
t_case() {
  local name="$1"
  shift
  t_n=$((t_n + 1))
  local log="$T_ROOT/case-$t_n.log"
  local rc
  (
    set -e
    "$@"
  ) >"$log" 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf 'PASS: %s\n' "$name"
    t_pass=$((t_pass + 1))
  else
    printf 'FAIL: %s\n' "$name"
    sed 's/^/    | /' "$log"
    t_fail=$((t_fail + 1))
  fi
}

# t_done — file exit status: 0 iff no case failed.
t_done() {
  [ "$t_fail" -eq 0 ]
}

# --- sandbox ------------------------------------------------------------

# t_sandbox — fresh isolated environment for one case. Sets (globals within
# the case subshell): WORK, HOME, PREFIX, SHIMDIR, ITMP, INSTALL,
# CURL_SHIM_MAP, CURL_SHIM_LOG.
#
# install.sh is copied to $WORK/scripts/install.sh so that the presence of a
# sibling update.sh (which install_updater prefers over downloading) is
# controlled by the test, not by the state of the repo checkout.
t_sandbox() {
  WORK="$(mktemp -d "$T_ROOT/case.XXXXXX")"
  HOME="$WORK/home"
  PREFIX="$WORK/prefix"
  SHIMDIR="$WORK/shim"
  ITMP="$WORK/itmp" # TMPDIR handed to install.sh, to assert its cleanup
  mkdir -p "$HOME" "$PREFIX" "$SHIMDIR" "$ITMP" "$WORK/assets" "$WORK/scripts"
  CURL_SHIM_MAP="$WORK/curl.map"
  CURL_SHIM_LOG="$WORK/curl.log"
  : >"$CURL_SHIM_MAP"
  : >"$CURL_SHIM_LOG"
  cp "$T_DIR/curl_shim.sh" "$SHIMDIR/curl"
  chmod +x "$SHIMDIR/curl"
  cp "$INSTALL_SH" "$WORK/scripts/install.sh"
  INSTALL="$WORK/scripts/install.sh"
  export HOME CURL_SHIM_MAP CURL_SHIM_LOG
}

# --- release fixtures ---------------------------------------------------

host_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    *) return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) return 1 ;;
  esac
  printf '%s-%s\n' "$os" "$arch"
}

host_asset() {
  printf 'slack-mcp-server-%s\n' "$(host_platform)"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# map_url <url> <fixture-path> — serve <fixture-path> for <url>.
map_url() {
  printf '%s\t%s\n' "$1" "$2" >>"$CURL_SHIM_MAP"
}

# unmap_url <url> — remove a mapping so the shim fails that URL (rc 22).
unmap_url() {
  local pat
  pat="$(printf '%s\t' "$1")"
  grep -vF "$pat" "$CURL_SHIM_MAP" >"$CURL_SHIM_MAP.tmp" || true
  mv "$CURL_SHIM_MAP.tmp" "$CURL_SHIM_MAP"
}

# stage_release <tag> [bad-banner] — build the host-platform asset, its
# checksums.txt, and the release JSON under $WORK, and map:
#   $API_BASE/releases/tags/<tag>  -> release JSON
#   $DL_BASE/<tag>/<asset>         -> asset
#   $DL_BASE/<tag>/checksums.txt   -> checksums.txt
# Pass "bad-banner" to stage a binary whose --version output fails the probe
# (its checksum is still correct).
stage_release() {
  local tag="$1"
  local tpl="$FIXTURES_DIR/fake-binary.tpl"
  [ "${2:-}" = "bad-banner" ] && tpl="$FIXTURES_DIR/fake-binary-bad.tpl"
  local asset
  asset="$(host_asset)"
  sed "s/@TAG@/$tag/g" "$tpl" >"$WORK/assets/$asset"
  chmod +x "$WORK/assets/$asset"
  printf '%s  %s\n' "$(sha256_of "$WORK/assets/$asset")" "$asset" \
    >"$WORK/assets/checksums.txt"
  sed "s/@TAG@/$tag/g" "$FIXTURES_DIR/release.tpl.json" >"$WORK/release-$tag.json"
  map_url "$API_BASE/releases/tags/$tag" "$WORK/release-$tag.json"
  map_url "$DL_BASE/$tag/$asset" "$WORK/assets/$asset"
  map_url "$DL_BASE/$tag/checksums.txt" "$WORK/assets/checksums.txt"
}

# map_latest <tag> — serve the staged release JSON for the /releases/latest URL.
map_latest() {
  map_url "$API_BASE/releases/latest" "$WORK/release-$1.json"
}

# map_updater — serve the update.sh fixture for the raw-master download URL.
map_updater() {
  map_url "$UPDATE_SH_URL" "$FIXTURES_DIR/update-sh.fixture"
}

# --- running the installer ----------------------------------------------

# run_install <outfile> [args...] — run install.sh with the curl shim first
# on PATH and TMPDIR pointed at $ITMP. --prefix "$PREFIX" is always passed.
run_install() {
  local out="$1"
  shift
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    bash "$INSTALL" --prefix "$PREFIX" "$@" >"$out" 2>&1
}

# run_install_notty <outfile> [args...] — same, but detached from any
# controlling terminal (perl POSIX::setsid), so install.sh's /dev/tty read
# fails and its non-interactive default kicks in even when the harness runs
# from a real terminal.
run_install_notty() {
  local out="$1"
  shift
  env -u GITHUB_TOKEN PATH="$SHIMDIR:$PATH" HOME="$HOME" TMPDIR="$ITMP" \
    CURL_SHIM_MAP="$CURL_SHIM_MAP" CURL_SHIM_LOG="$CURL_SHIM_LOG" \
    perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV or die "exec: $!"' -- \
    bash "$INSTALL" --prefix "$PREFIX" "$@" >"$out" 2>&1 </dev/null
}

# restricted_path_without_jq — print a PATH of the shim dir plus symlinks to
# only the external tools the scripts under test need, with jq deliberately
# absent. The shim dir stays first, so any stubs a test installed there
# (curl, launchctl, systemctl, uname) still win over the real tools.
restricted_path_without_jq() {
  local tooldir="$WORK/tools" t p
  mkdir -p "$tooldir"
  for t in bash sh env uname mktemp rm sed head grep mkdir chmod mv cp cat \
    dirname basename readlink awk tr id sha256sum shasum; do
    p="$(command -v "$t" 2>/dev/null)" || continue
    ln -sf "$p" "$tooldir/$t"
  done
  printf '%s:%s\n' "$SHIMDIR" "$tooldir"
}

# --- assertions ---------------------------------------------------------

assert_file() {
  [ -f "$1" ] || {
    echo "ASSERT: expected file: $1"
    return 1
  }
}

assert_no_file() {
  [ ! -e "$1" ] || {
    echo "ASSERT: expected no file at: $1"
    return 1
  }
}

assert_exec() {
  [ -x "$1" ] || {
    echo "ASSERT: expected executable: $1"
    return 1
  }
}

# assert_rc <expected> <actual>
assert_rc() {
  [ "$2" -eq "$1" ] || {
    echo "ASSERT: expected rc $1, got $2"
    return 1
  }
}

# assert_contains <file> <literal-substring>
assert_contains() {
  grep -qF -- "$2" "$1" || {
    echo "ASSERT: expected to find: $2"
    echo "--- in $1:"
    cat "$1"
    return 1
  }
}

# assert_not_contains <file> <literal-substring>
assert_not_contains() {
  ! grep -qF -- "$2" "$1" || {
    echo "ASSERT: expected NOT to find: $2"
    echo "--- in $1:"
    cat "$1"
    return 1
  }
}

assert_empty_dir() {
  [ -z "$(find "$1" -mindepth 1 2>/dev/null)" ] || {
    echo "ASSERT: expected empty dir: $1 — contents:"
    find "$1" -mindepth 1
    return 1
  }
}
