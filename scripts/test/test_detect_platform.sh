#!/usr/bin/env bash
# Unit tests for install.sh's sourceable functions (detect_platform,
# validate_tag) and its flag validation (exit 2 paths).
# shellcheck source=lib.sh
. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

# Sourcing install.sh does not run main() thanks to its BASH_SOURCE guard.
# shellcheck source=../install.sh
source_install() {
  . "$INSTALL_SH"
}

case_platform_mappings() {
  source_install
  [ "$(detect_platform Darwin arm64)" = "darwin-arm64" ]
  [ "$(detect_platform Darwin x86_64)" = "darwin-amd64" ]
  [ "$(detect_platform Linux aarch64)" = "linux-arm64" ]
  [ "$(detect_platform Linux x86_64)" = "linux-amd64" ]
  [ "$(detect_platform Linux amd64)" = "linux-amd64" ]
}

case_unsupported_os_rc2() {
  source_install
  local rc=0
  detect_platform SunOS x86_64 2>/dev/null || rc=$?
  assert_rc 2 "$rc"
}

case_unsupported_arch_rc2() {
  source_install
  local rc=0
  detect_platform Darwin i386 2>/dev/null || rc=$?
  assert_rc 2 "$rc"
}

case_validate_tag() {
  source_install
  validate_tag pv-v1.0.0
  validate_tag pv-v10.22.333
  local bad
  for bad in v1.0.0 pv-v1.0 pv-v1.0.0-rc1; do
    if validate_tag "$bad"; then
      echo "ASSERT: validate_tag accepted invalid tag: $bad"
      return 1
    fi
  done
}

case_unknown_flag_rc2() {
  t_sandbox
  local rc=0
  run_install "$WORK/out" --bogus || rc=$?
  assert_rc 2 "$rc"
  assert_contains "$WORK/out" "unknown option: --bogus"
}

case_invalid_pin_rc2() {
  t_sandbox
  local rc=0
  run_install "$WORK/out" --version 1.0.0 || rc=$?
  assert_rc 2 "$rc"
  assert_contains "$WORK/out" "invalid --version '1.0.0'"
}

t_case platform_mappings case_platform_mappings
t_case unsupported_os_rc2 case_unsupported_os_rc2
t_case unsupported_arch_rc2 case_unsupported_arch_rc2
t_case validate_tag case_validate_tag
t_case unknown_flag_rc2 case_unknown_flag_rc2
t_case invalid_pin_rc2 case_invalid_pin_rc2
t_done
