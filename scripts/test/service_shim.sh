#!/usr/bin/env bash
# service_shim.sh — test stub standing in for BOTH launchctl and systemctl.
# The updater tests copy it to <shimdir>/launchctl and <shimdir>/systemctl and
# put <shimdir> first on PATH, so scripts under test can never reach the real
# service managers (a stray restart of the user's live service must be
# impossible in the harness).
#
# Environment:
#   SERVICE_SHIM_LOG      every invocation is appended as "<tool> <args...>"
#   SERVICE_SHIM_PRESENT  "1" = a service is configured: presence queries
#                         (launchctl print / systemctl --user is-enabled)
#                         succeed; anything else = absent, they fail (rc 1)
set -u

tool="${0##*/}"
printf '%s %s\n' "$tool" "$*" >>"${SERVICE_SHIM_LOG:?SERVICE_SHIM_LOG not set}"

cmd="${1:-}"
[ "$cmd" = "--user" ] && cmd="${2:-}"

case "$cmd" in
  print | is-enabled)
    # presence query: succeed only when the test declares a service configured
    [ "${SERVICE_SHIM_PRESENT:-0}" = "1" ]
    ;;
  *)
    # kickstart / restart / anything else: log-only success
    exit 0
    ;;
esac
