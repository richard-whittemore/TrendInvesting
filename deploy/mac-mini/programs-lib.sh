#!/bin/bash
# Helpers for deploy/mac-mini/programs-inside.sh, sourced. Every wait is
# bounded, and a program started here is stopped and reaped on every path,
# success or failure, so a failed rehearsal in an existing container leaves
# nothing running (fail closed: a check that cannot finish reports it,
# never hangs). Tested by deploy/mac-mini/test-programs-lib.sh.

# The one background program running, its process ID; empty when none.
background_pid=

# _running PID: whether PID is a background job of this shell that has not
# exited (a job that exited is no longer listed, though not yet reaped).
_running() { jobs -rp | grep -qx "$1"; }

# background_start OUT CMD...: starts CMD in the background, its output and
# errors to OUT, and tracks it.
background_start() {
  local out=$1
  shift
  "$@" >"$out" 2>&1 &
  background_pid=$!
}

# wait_ready TEXT OUT SECONDS: succeeds once OUT holds TEXT; fails at once if
# the tracked program exits first, or after SECONDS.
wait_ready() {
  local text=$1 out=$2 limit=$3 i
  for ((i = 0; i < limit * 10; i++)); do
    grep -q "$text" "$out" 2>/dev/null && return 0
    _running "$background_pid" || return 1
    sleep 0.1
  done
  grep -q "$text" "$out" 2>/dev/null
}

# background_stop SECONDS: sends the tracked program SIGTERM and waits up to
# SECONDS for it to exit, then sends SIGKILL; reaps it and clears the
# tracking. Returns the program's exit status, or 124 if it had to be
# killed; 0 when nothing is tracked.
background_stop() {
  local limit=${1:-5} pid=$background_pid i status=0
  test -n "$pid" || return 0
  background_pid=
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < limit * 10; i++)); do
    _running "$pid" || break
    sleep 0.1
  done
  if _running "$pid"; then
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    return 124
  fi
  wait "$pid" || status=$?
  return "$status"
}

# install_state PATH...: one line naming every file and directory under each
# PATH, with its type, size, mode, modification time and link target (a
# missing PATH is named as missing), as a SHA-256: equal before and after a
# step only if the step changed nothing there.
install_state() {
  python3 -I -c '
import hashlib, os, sys
lines = []
for root in sys.argv[1:]:
    if not os.path.lexists(root):
        lines.append("missing " + root)
        continue
    stack = [root]
    while stack:
        path = stack.pop()
        st = os.lstat(path)
        link = os.readlink(path) if os.path.islink(path) else ""
        lines.append("{} {:o} {} {} {}".format(path, st.st_mode, st.st_size, st.st_mtime_ns, link))
        if os.path.isdir(path) and not os.path.islink(path):
            stack.extend(os.path.join(path, name) for name in os.listdir(path))
print(hashlib.sha256("\n".join(sorted(lines)).encode()).hexdigest())
' "$@"
}
