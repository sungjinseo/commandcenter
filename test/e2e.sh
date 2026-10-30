#!/usr/bin/env bash
# Runs every end-to-end check: the real-client smoke test of each format and
# the memory load test. Prints a summary and fails if any check failed.
# --ha adds the high availability smoke test (test/ha/ha-smoke.sh: PostgreSQL
# and two nodes, a kill -9 and a rolling restart; about 6 minutes).
set -uo pipefail
cd "$(dirname "$0")/.."
checks=(maven-smoke npm-smoke pypi-smoke go-smoke cargo-smoke ui-smoke ldap-smoke mail-smoke load)
[ "${1:-}" = --ha ] && checks+=(ha-smoke)
declare -A result duration memory
failed=0
for c in "${checks[@]}"; do
  script="test/smoke/$c.sh"
  [ "$c" = load ] && script="test/load/load.sh"
  [ "$c" = ha-smoke ] && script="test/ha/ha-smoke.sh"
  log="$(mktemp)"
  start=$(date +%s)
  if bash "$script" >"$log" 2>&1; then result[$c]=OK; else result[$c]=FAIL; failed=1; fi
  duration[$c]=$(( $(date +%s) - start ))s
  # Load test: its own max-rss figure. Smoke tests: the docker stats usage
  # ("<name> 9.1MiB / 7.6GiB" -> "9.1MiB").
  mem="$(grep -Eo 'max-rss=[0-9.]+[KMG]i?B' "$log" | tail -1)"
  [ -n "$mem" ] || mem="$(sed -nE 's/^commandcenter[^ ]* ([0-9.]+[KMG]iB) \/ .*/\1/p' "$log" | tail -1)"
  memory[$c]="$mem"
  if [ "${result[$c]}" = FAIL ]; then
    echo "---- $c output (last 40 lines) ----"; tail -40 "$log"
  fi
  rm -f "$log"
done
printf '\n%-12s %-6s %-8s %s\n' CHECK RESULT TIME MEMORY
for c in "${checks[@]}"; do
  printf '%-12s %-6s %-8s %s\n' "$c" "${result[$c]}" "${duration[$c]}" "${memory[$c]}"
done
exit $failed
