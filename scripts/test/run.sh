#!/usr/bin/env bash
# run.sh — plain-sh test runner (no bats). Executes every test_*.sh in this
# directory, prints per-case PASS/FAIL and a summary, exits nonzero on any
# failure. Works on macOS bash 3.2 and Linux bash.
#
# Usage: bash scripts/test/run.sh
set -u

self_dir="$(cd "$(dirname "$0")" && pwd)"

total_pass=0
total_fail=0
failed_files=""
found=0

for f in "$self_dir"/test_*.sh; do
  [ -e "$f" ] || continue
  found=1
  name="$(basename "$f")"
  printf '== %s\n' "$name"
  out="$(bash "$f" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | sed 's/^/   /'
  p="$(printf '%s\n' "$out" | grep -c '^PASS: ')" || true
  fl="$(printf '%s\n' "$out" | grep -c '^FAIL: ')" || true
  total_pass=$((total_pass + p))
  total_fail=$((total_fail + fl))
  if [ "$rc" -ne 0 ] && [ "$fl" -eq 0 ]; then
    # The file died outside of case accounting (syntax error, crashed lib...).
    printf '   FAIL: %s exited %d without reporting a failing case\n' "$name" "$rc"
    total_fail=$((total_fail + 1))
  fi
  if [ "$rc" -ne 0 ]; then
    failed_files="$failed_files $name"
  fi
done

if [ "$found" -eq 0 ]; then
  echo "ERROR: no test_*.sh files found in $self_dir" >&2
  exit 1
fi

printf -- '----\n'
printf '%d passed, %d failed\n' "$total_pass" "$total_fail"
if [ "$total_fail" -ne 0 ]; then
  printf 'failing files:%s\n' "$failed_files"
  exit 1
fi
