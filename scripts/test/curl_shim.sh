#!/usr/bin/env bash
# curl_shim.sh — test stub standing in for curl. The test lib copies it to
# <shimdir>/curl and puts <shimdir> first on PATH, so scripts under test hit
# fixtures instead of the network.
#
# Environment (both required):
#   CURL_SHIM_MAP  file of "<url>\t<fixture-path>" lines
#   CURL_SHIM_LOG  every requested URL is appended here (one per line)
#
# An unmapped URL or missing fixture behaves like `curl -f` on an HTTP error:
# nothing is written to -o, exit code 22.
set -u

out=""
url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o | -H | --retry | --connect-timeout | --max-time) # flags with a value
      [ "$1" = "-o" ] && out="$2"
      shift 2
      ;;
    -*) # value-less flags (-f, -s, -S, -L, combined -fsSL, ...)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done

printf '%s\n' "$url" >>"${CURL_SHIM_LOG:?CURL_SHIM_LOG not set}"

tab="$(printf '\t')"
fixture=""
while IFS="$tab" read -r m_url m_file; do
  if [ "$m_url" = "$url" ]; then
    fixture="$m_file"
    break
  fi
done <"${CURL_SHIM_MAP:?CURL_SHIM_MAP not set}"

if [ -z "$fixture" ] || [ ! -f "$fixture" ]; then
  printf 'curl-shim: no fixture mapped for %s\n' "$url" >&2
  exit 22
fi

if [ -n "$out" ]; then
  cp "$fixture" "$out"
else
  cat "$fixture"
fi
