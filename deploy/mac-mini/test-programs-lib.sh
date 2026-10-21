#!/bin/bash
# Tests for deploy/mac-mini/programs-lib.sh, the helpers the programs
# rehearsal runs the engine and checks the TWS API step with: a start-up that
# times out and a shutdown that hangs are both bounded and leave nothing
# running, and the install state compares a tree before and after a step. No
# Docker needed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/mac-mini/programs-lib.sh
. "$here/programs-lib.sh"
work=$(mktemp -d)
trap 'background_stop; rm -rf "$work"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
gone() { ! kill -0 "$1" 2>/dev/null; }

# A program that is ready and stops on SIGTERM.
background_start "$work/good.out" bash -c 'echo "listening on x"; trap "exit 0" TERM; while :; do sleep 0.1; done'
pid=$background_pid
wait_ready "listening on" "$work/good.out" 5 || fail "a ready program was not seen as ready"
status=0
background_stop 5 || status=$?
test "$status" -eq 0 || fail "a program that stops on SIGTERM was reported as status $status"
gone "$pid" || fail "a stopped program is still running"
test -z "$background_pid" || fail "the tracked program is not cleared after it stopped"

# A program that ignores SIGTERM: stopped within the limit, killed, reported.
background_start "$work/stuck.out" bash -c 'trap "" TERM; echo "listening on x"; while :; do sleep 0.1; done'
pid=$background_pid
wait_ready "listening on" "$work/stuck.out" 5 || fail "the stuck program was not seen as ready"
start=$SECONDS
status=0
background_stop 1 || status=$?
test "$status" -ne 0 || fail "a program that ignored SIGTERM was reported as stopping cleanly"
test $((SECONDS - start)) -le 4 || fail "stopping a stuck program took $((SECONDS - start)) s, not about 1"
gone "$pid" || fail "a program that ignored SIGTERM is still running"

# A program that never becomes ready: the wait ends at its limit, and the
# stop that follows (as on any failure, from the EXIT trap) leaves nothing.
background_start "$work/silent.out" bash -c 'trap "" TERM; while :; do sleep 0.1; done'
pid=$background_pid
start=$SECONDS
if wait_ready "listening on" "$work/silent.out" 1; then fail "a program that never printed was seen as ready"; fi
test $((SECONDS - start)) -le 4 || fail "waiting for a silent program took $((SECONDS - start)) s, not about 1"
background_stop 1 || true
gone "$pid" || fail "a program that never became ready is still running after the stop"

# A program that exits before it is ready: the wait says so at once.
background_start "$work/dead.out" bash -c 'exit 3'
start=$SECONDS
if wait_ready "listening on" "$work/dead.out" 10; then fail "a program that exited was seen as ready"; fi
test $((SECONDS - start)) -le 4 || fail "a program that exited was waited on for $((SECONDS - start)) s"
background_stop 1 || true

# With nothing tracked, the stop does nothing and succeeds.
background_stop 1 || fail "a stop with nothing running failed"

# The install state: unchanged trees compare equal, missing paths included;
# any added, removed or changed file is a difference.
mkdir -p "$work/tree/a"
echo one >"$work/tree/a/f"
before=$(install_state "$work/tree" "$work/missing")
test "$before" = "$(install_state "$work/tree" "$work/missing")" || fail "an unchanged tree compared different"
echo two >"$work/tree/a/g"
test "$before" != "$(install_state "$work/tree" "$work/missing")" || fail "an added file was not seen"
rm "$work/tree/a/g"
before=$(install_state "$work/tree" "$work/missing")
mkdir "$work/missing"
test "$before" != "$(install_state "$work/tree" "$work/missing")" || fail "a path that appeared was not seen"
rmdir "$work/missing"
test "$before" = "$(install_state "$work/tree" "$work/missing")" || fail "removing the path that appeared did not restore the state"
printf 'one!\n' >"$work/tree/a/f"
test "$before" != "$(install_state "$work/tree" "$work/missing")" || fail "a changed file was not seen"

test "$failures" -eq 0 || exit 1
echo "programs-lib tests ok"
