#!/bin/bash
# Tests for deploy/maintenance.sh (the server's `ti-maintenance`): the
# marker it writes, its default and its 30-minute maximum, `end`, `status`,
# and a marker that is past its expiry reading as closed and staying where it
# is (no reader removes a marker). The one canonical layout is judged over the
# fixtures that marker_test.go judges too.
# The marker is a temporary file and the time is fixed with
# TI_MAINTENANCE_NOW; no root, no systemd.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
script=$repo/deploy/maintenance.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export TI_MAINTENANCE_FILE=$work/maintenance.json TI_MAINTENANCE_NO_ROOT=1
# The script needs flock(1); a machine without it (the Mac the tests run on)
# gets the stand-in beside this file.
if ! command -v flock >/dev/null 2>&1; then PATH=$here/stubs:$PATH; export PATH; fi
failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
expect() { if test "$2" != "$3"; then fail "$1: want '$2', got '$3'"; fi; }
run() { TI_MAINTENANCE_NOW=$1 bash "$script" "${@:2}"; }
t0=1793200000 # a fixed instant
field() { sed -n 's/.*"'"$1"'":\([0-9]*\).*/\1/p' "$TI_MAINTENANCE_FILE"; }

# The default is 15 minutes.
run $t0 start --replace >"$work/out" 2>&1 || fail "start with no duration failed: $(cat "$work/out")"
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
run $t0 start --replace 10m >/dev/null; expect "10m" $((t0 + 600)) "$(field until_unix)"
run $t0 start --replace 90s >/dev/null; expect "90s" $((t0 + 90)) "$(field until_unix)"
run $t0 start --replace 7 >/dev/null; expect "7 minutes" $((t0 + 420)) "$(field until_unix)"
run $t0 start --replace 30m >/dev/null; expect "30m" $((t0 + 1800)) "$(field until_unix)"
for bad in 31m 1801s 45 0 0m -5m abc 10x 1h "" 18446744073709551617 18446744073709551617s 99999999999999999999m 000000000000000000045; do
  rm -f "$TI_MAINTENANCE_FILE"
  if run $t0 start --replace "$bad" >"$work/out" 2>&1; then fail "start '$bad' was accepted"; fi
  test ! -e "$TI_MAINTENANCE_FILE" || fail "start '$bad' wrote a marker"
done
rm -f "$TI_MAINTENANCE_FILE"
run $t0 start --replace 31m >"$work/out" 2>&1 || true
grep -q 'at most 30 minutes' "$work/out" || fail "the refusal does not say the maximum: $(cat "$work/out")"
# An expired marker reads as closed and is left in place.
run $t0 start --replace 5m >/dev/null
run $((t0 + 301)) status >"$work/out"
grep -q 'maintenance window closed' "$work/out" || fail "an expired marker was reported open: $(cat "$work/out")"
test -e "$TI_MAINTENANCE_FILE" || fail "status removed an expired marker: a reader never removes one, so a start in flight is never undone"
# end removes it, and ending nothing is not an error.
run $t0 start --replace 5m >/dev/null
run $t0 end >/dev/null
test ! -e "$TI_MAINTENANCE_FILE" || fail "end left the marker"
run $t0 end >/dev/null || fail "end with no window failed"
# start never overwrites a window that is open: it refuses (status 3) and says
# which, unless asked to --replace it; an expired one is replaced freely.
run $t0 start --replace 10m >"$work/out"
id=$(sed -n 's/^id //p' "$work/out")
set +e; run $((t0 + 1)) start 5m >"$work/out" 2>&1; rc=$?; set -e
expect "start over an open window" 3 "$rc"
grep -q "already open" "$work/out" || fail "the refusal does not say a window is open: $(cat "$work/out")"
grep -q "\"id\":\"$id\"" "$TI_MAINTENANCE_FILE" || fail "the refused start replaced the open window"
run $((t0 + 700)) start 5m >/dev/null || fail "start over an expired window was refused"
# left says how many seconds remain, 0 when none.
run $t0 start --replace 10m >/dev/null
expect "seconds left" 590 "$(run $((t0 + 10)) left)"
expect "seconds left when expired" 0 "$(run $((t0 + 700)) left)"
rm -f "$TI_MAINTENANCE_FILE"
expect "seconds left with no marker" 0 "$(run $t0 left)"
# end --id and a start that arrives between its check and its removal: the
# lock makes the start wait, so the new window is not the one removed. The
# hook (test mode only) starts it in the background right after the check.
run $t0 start --replace 10m >"$work/out"
id=$(sed -n 's/^id //p' "$work/out")
TI_MAINTENANCE_AFTER_CHECK="( TI_MAINTENANCE_NOW=$t0 bash '$script' start --replace 20m >/dev/null 2>&1 & ); sleep 1" run $t0 end --id "$id" >/dev/null || fail "end --id failed"
sleep 2
test -e "$TI_MAINTENANCE_FILE" || fail "a window started while end --id ran was removed by it"
expect "the new window's length" $((t0 + 1200)) "$(field until_unix)"
if grep -q "\"id\":\"$id\"" "$TI_MAINTENANCE_FILE"; then fail "the old window is still there"; fi
rm -f "$TI_MAINTENANCE_FILE"
# The lock has a time limit: a command that cannot get it in time says so and
# fails, so a stalled holder never blocks ti-release's cleanup for ever.
run $t0 start --replace 5m >/dev/null
( exec 9>>"$work/.lock"; flock 9; sleep 3 ) &
holder=$!
sleep 0.5
set +e; TI_MAINTENANCE_LOCK_WAIT=1 run $t0 end >"$work/out" 2>&1; rc=$?; set -e
test "$rc" -ne 0 || fail "end got the lock while another command held it"
grep -q "maintenance lock busy" "$work/out" || fail "a lock timeout does not say so: $(cat "$work/out")"
test -e "$TI_MAINTENANCE_FILE" || fail "end removed the marker without the lock"
wait "$holder"
# A window's expiry counts from when the lock is got, not from when the
# command began: the clock is the real one here, and the holder keeps the lock
# for 2 seconds.
rm -f "$TI_MAINTENANCE_FILE"
( exec 9>>"$work/.lock"; flock 9; sleep 2 ) &
holder=$!
sleep 0.3
began=$(date +%s)
TI_MAINTENANCE_NOW='' bash "$script" start --replace 1m >/dev/null || fail "start after waiting for the lock failed"
wait "$holder"
[[ $(sed -n 's/.*"opened_unix":\([0-9]*\).*/\1/p' "$TI_MAINTENANCE_FILE") -ge $((began + 1)) ]] || fail "the window opened before the lock was got: $(cat "$TI_MAINTENANCE_FILE")"
rm -f "$TI_MAINTENANCE_FILE"
# Without flock the script refuses and says what is missing; it has no fallback.
bash_bin=$(command -v bash)
set +e; TI_MAINTENANCE_FLOCK=no-such-flock "$bash_bin" "$script" status >"$work/out" 2>&1; rc=$?; set -e
{ test "$rc" -ne 0 && grep -q "flock" "$work/out"; } || fail "a missing flock is not refused with a message: $(cat "$work/out")"
# status with no marker at all (after end, or before the first start) says
# closed and succeeds; is-open agrees by its exit status.
rm -f "$TI_MAINTENANCE_FILE"
run $t0 status >"$work/out" 2>&1 || fail "status with no marker failed: $(cat "$work/out")"
grep -q 'maintenance window closed' "$work/out" || fail "status with no marker does not say closed: $(cat "$work/out")"
if run $t0 is-open >/dev/null 2>&1; then fail "is-open succeeded with no marker"; fi
run $t0 start --replace 5m >/dev/null
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
run $t0 start --replace 5m >/dev/null
for f in "$work"/.maintenance.*; do test ! -e "$f" || fail "a half-written marker was left: $f"; done
# The marker is written by root into a folder only root can write, with no
# chown, chmod or install -m on a path another user could swap, and the file
# is created exclusively with the mode the umask gives.
if grep -v "^ *#" "$script" | grep -Eq 'chown|chmod|install -m|install .*-m '; then fail "maintenance.sh changes the owner or mode of a path by name"; fi
perm=$(stat -c %a "$TI_MAINTENANCE_FILE" 2>/dev/null || stat -f %Lp "$TI_MAINTENANCE_FILE")
expect "marker mode from the umask" 644 "$perm"
# The marker is the canonical layout, byte for byte, and names a window.
run $t0 start --replace 5m >"$work/out"
expect "the marker's layout" "{\"id\":\"$(sed -n 's/^id //p' "$work/out")\",\"opened\":\"2026-10-28T15:06:40Z\",\"until\":\"2026-10-28T15:11:40Z\",\"opened_unix\":$t0,\"until_unix\":$((t0 + 300))}" "$(cat "$TI_MAINTENANCE_FILE")"
id=$(sed -n 's/^id //p' "$work/out")
[[ $id =~ ^[0-9a-f]{16}$ ]] || fail "start did not print a 16-digit hexadecimal id: $(cat "$work/out")"
# end --id ends only the window with that id.
run $t0 end --id 0000000000000000 >/dev/null || fail "end --id of another window failed"
test -e "$TI_MAINTENANCE_FILE" || fail "end --id of another window ended this one"
run $t0 end --id "$id" >/dev/null || fail "end --id failed"
test ! -e "$TI_MAINTENANCE_FILE" || fail "end --id left its own window"
run $t0 end --id "$id" >/dev/null || fail "end --id with no window failed"
if run $t0 end --id >/dev/null 2>&1; then fail "end --id with no id was accepted"; fi
run $t0 start --replace 5m >/dev/null
# A new marker is always in a folder that exists afterwards.
rm -rf "$work/sub"
TI_MAINTENANCE_FILE=$work/sub/maintenance.json run $t0 start 5m >/dev/null || fail "start did not make the marker's folder"
test -s "$work/sub/maintenance.json" || fail "start made no marker in a new folder"
# Not root and not the test escape: refused.
if env -u TI_MAINTENANCE_NO_ROOT bash "$script" start 5m >/dev/null 2>&1 && test "$(id -u)" -ne 0; then fail "start ran without root"; fi
test "$failures" -eq 0 || { echo "$failures failure(s)" >&2; exit 1; }
echo "test-maintenance: ok"
