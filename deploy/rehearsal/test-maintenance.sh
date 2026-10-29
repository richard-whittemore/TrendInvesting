#!/bin/bash
# Tests for deploy/maintenance.sh (the server's `ti-maintenance`): the
# marker it writes, its default and its 30-minute maximum, `end`, `status`,
# and a marker that is past its expiry reading as closed and being removed.
# The marker is a temporary file and the time is fixed with
# TI_MAINTENANCE_NOW; no root, no systemd.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
script=$repo/deploy/maintenance.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export TI_MAINTENANCE_FILE=$work/maintenance.json TI_MAINTENANCE_NO_ROOT=1
failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
expect() { if test "$2" != "$3"; then fail "$1: want '$2', got '$3'"; fi; }
run() { TI_MAINTENANCE_NOW=$1 bash "$script" "${@:2}"; }
t0=1793200000 # a fixed instant
field() { sed -n 's/.*"'"$1"'":\([0-9]*\).*/\1/p' "$TI_MAINTENANCE_FILE"; }

# The default is 15 minutes.
run $t0 start >"$work/out" 2>&1 || fail "start with no duration failed: $(cat "$work/out")"
expect "default expiry" $((t0 + 900)) "$(field until_unix)"
expect "opened" "$t0" "$(field opened_unix)"
grep -q '"until":"' "$TI_MAINTENANCE_FILE" || fail "the marker has no until time"
# The marker is read by the reporter user, which is not the writer.
perm=$(stat -c %a "$TI_MAINTENANCE_FILE" 2>/dev/null || stat -f %Lp "$TI_MAINTENANCE_FILE")
expect "marker mode" 644 "$perm"
# The status says it is open, and until when.
run $((t0 + 60)) status >"$work/out"
grep -q 'maintenance window open until' "$work/out" || fail "status does not say it is open: $(cat "$work/out")"
# Durations: minutes, seconds, a bare number of minutes; 30 minutes is the most.
run $t0 start 10m >/dev/null; expect "10m" $((t0 + 600)) "$(field until_unix)"
run $t0 start 90s >/dev/null; expect "90s" $((t0 + 90)) "$(field until_unix)"
run $t0 start 7 >/dev/null; expect "7 minutes" $((t0 + 420)) "$(field until_unix)"
run $t0 start 30m >/dev/null; expect "30m" $((t0 + 1800)) "$(field until_unix)"
for bad in 31m 1801s 45 0 0m -5m abc 10x 1h "" 18446744073709551617 18446744073709551617s 99999999999999999999m 000000000000000000045; do
  rm -f "$TI_MAINTENANCE_FILE"
  if run $t0 start "$bad" >"$work/out" 2>&1; then fail "start '$bad' was accepted"; fi
  test ! -e "$TI_MAINTENANCE_FILE" || fail "start '$bad' wrote a marker"
done
rm -f "$TI_MAINTENANCE_FILE"
run $t0 start 31m >"$work/out" 2>&1 || true
grep -q 'at most 30 minutes' "$work/out" || fail "the refusal does not say the maximum: $(cat "$work/out")"
# An expired marker reads as closed and is removed by status.
run $t0 start 5m >/dev/null
run $((t0 + 301)) status >"$work/out"
grep -q 'maintenance window closed' "$work/out" || fail "an expired marker was reported open: $(cat "$work/out")"
test -e "$TI_MAINTENANCE_FILE" || fail "status removed an expired marker: a reader never removes one, so a start in flight is never undone"
# end removes it, and ending nothing is not an error.
run $t0 start 5m >/dev/null
run $t0 end >/dev/null
test ! -e "$TI_MAINTENANCE_FILE" || fail "end left the marker"
run $t0 end >/dev/null || fail "end with no window failed"
# status with no marker at all (after end, or before the first start) says
# closed and succeeds; is-open agrees by its exit status.
rm -f "$TI_MAINTENANCE_FILE"
run $t0 status >"$work/out" 2>&1 || fail "status with no marker failed: $(cat "$work/out")"
grep -q 'maintenance window closed' "$work/out" || fail "status with no marker does not say closed: $(cat "$work/out")"
if run $t0 is-open >/dev/null 2>&1; then fail "is-open succeeded with no marker"; fi
run $t0 start 5m >/dev/null
run $((t0 + 10)) is-open >/dev/null || fail "is-open failed inside a window"
if run $((t0 + 400)) is-open >/dev/null 2>&1; then fail "is-open succeeded past the expiry"; fi
run $t0 end >/dev/null
# status judges a marker as the reporter does: the same files, the same answers
# (internal/maintenance/testdata/markers, read by marker_test.go too).
for f in "$repo"/internal/maintenance/testdata/markers/*.json; do
  cp "$f" "$TI_MAINTENANCE_FILE"
  run 1793214000 status >"$work/out" 2>&1 || fail "status failed on $(basename "$f"): $(cat "$work/out")"
  case $(basename "$f") in
    open-*) grep -q 'maintenance window open until' "$work/out" || fail "status calls $(basename "$f") closed: $(cat "$work/out")" ;;
    *) grep -q 'maintenance window closed' "$work/out" || fail "status calls $(basename "$f") open: $(cat "$work/out")" ;;
  esac
  case $(basename "$f") in
    open-*) run 1793214000 is-open >/dev/null 2>&1 || fail "is-open calls $(basename "$f") closed" ;;
    *) if run 1793214000 is-open >/dev/null 2>&1; then fail "is-open calls $(basename "$f") open"; fi ;;
  esac
done
rm -f "$TI_MAINTENANCE_FILE"
# No half-written marker is left behind.
run $t0 start 5m >/dev/null
for f in "$work"/.maintenance.*; do test ! -e "$f" || fail "a half-written marker was left: $f"; done
# The marker is made in a folder only root can write, and published under a
# name no one can guess, with no chown: the reports folder belongs to the
# reporter's user, and root must not follow a path it could swap.
if grep -v "^ *#" "$script" | grep -q chown; then fail "maintenance.sh changes ownership of a path in the reports folder"; fi
# Not root and not the test escape: refused.
if env -u TI_MAINTENANCE_NO_ROOT bash "$script" start 5m >/dev/null 2>&1 && test "$(id -u)" -ne 0; then fail "start ran without root"; fi
test "$failures" -eq 0 || { echo "$failures failure(s)" >&2; exit 1; }
echo "test-maintenance: ok"
