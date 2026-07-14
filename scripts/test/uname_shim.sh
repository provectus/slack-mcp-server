#!/usr/bin/env bash
# uname_shim.sh — test stub standing in for uname. The updater tests copy it
# to <shimdir>/uname so the platform seen by update.sh (detect_platform and
# the restart_service OS switch) is chosen by the test, not by the host.
#
# Environment (both required):
#   UNAME_SHIM_S  printed for `uname -s` (and any other invocation)
#   UNAME_SHIM_M  printed for `uname -m`
set -u

case "${1:-}" in
  -m) printf '%s\n' "${UNAME_SHIM_M:?UNAME_SHIM_M not set}" ;;
  *) printf '%s\n' "${UNAME_SHIM_S:?UNAME_SHIM_S not set}" ;;
esac
