#!/bin/bash
# Starts IB Gateway under IBC inside a virtual display. ibcstart.sh reads its
# credentials from the rendered config.ini, not from arguments, so nothing
# secret appears in the process list. Runs as ti-gateway from ti-gateway.service.
#
# The config is rendered here, in the unit's main process, rather than in an
# ExecStartPre: systemd applies RestartPreventExitStatus= only to the main
# process, so the renderer's configuration exits (64, 65, 78: the owner has
# not filled in the login, or filled it in wrongly) stop the unit instead of
# restarting it every 30 seconds.
#
# Bounded retry of a refused login. IBC 3.24.2 has no setting for a refused
# login ("Unrecognized Username or Password"): it leaves the dialog open and
# never exits, so the Gateway sits logged out for hours with the unit active.
# This script therefore watches IBC's output for that dialog and, on it, stops
# ITS OWN child (the pid it started, nothing else) so systemd starts the
# unit again after RestartSec. IBKR locks a login after 10 consecutive failed
# attempts within 24 hours (IBKR Knowledge Base, "login ... locked"), so the
# bound is MAX_FAILURES (3) refused logins within WINDOW (24 hours), counted in
# a state file that a completed login empties. At the bound the script exits
# 77, which the unit does not restart on, and a start while the bound still
# holds exits 77 at once without launching IBC. The owner clears it by
# deleting the state file (docs/manual/server.md, "The Gateway is logged out").
#
# Exit codes: 75 = a refused login was seen and the bound is not reached
# (systemd starts it again); 77 = the bound is reached; 64, 65, 78 = the
# renderer's; otherwise IBC's own status.
#
# Inputs (overridable for the rehearsal): TI_GATEWAY_RENDER, TI_GATEWAY_IBC (a
# command run instead of xvfb-run + ibcstart.sh), TI_GATEWAY_STATE_FILE,
# TI_GATEWAY_LOGIN_MAX_FAILURES, TI_GATEWAY_LOGIN_WINDOW (seconds),
# TI_GATEWAY_LOGIN_FAILURE_PATTERN (an extended regex).
set -euo pipefail
set +x

"${TI_GATEWAY_RENDER:-/usr/local/libexec/trend-investing/render-ibc-config.sh}" || exit $?

state=${TI_GATEWAY_STATE_FILE:-/var/lib/ti-gateway/login-failures}
max=${TI_GATEWAY_LOGIN_MAX_FAILURES:-3}
window=${TI_GATEWAY_LOGIN_WINDOW:-86400}
failure=${TI_GATEWAY_LOGIN_FAILURE_PATTERN:-Unrecognized Username or Password}
decision="$state.decision"
pidfile="$state.pid"

# failures_in_window: the number of recorded refusals no older than WINDOW.
failures_in_window() {
  local now n=0 at
  now=$(date +%s)
  if test -f "$state"; then
    while read -r at || test -n "$at"; do
      case $at in ''|*[!0-9]*) continue ;; esac
      if test $((now - at)) -lt "$window"; then n=$((n + 1)); fi
    done <"$state"
  fi
  echo "$n"
}

if test "$(failures_in_window)" -ge "$max"; then
  echo "start-gateway: login retries stopped: $max refused logins within the last $((window / 3600)) hours; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete $state and start the unit" >&2
  exit 77
fi
rm -f "$decision" "$pidfile"

# IBC's command. A small shell records its own pid for the watcher, then execs
# the command, so the recorded pid is IBC's (or xvfb-run's) own.
if test -n "${TI_GATEWAY_IBC:-}"; then
  cmd=("$TI_GATEWAY_IBC")
else
  major=${GATEWAY_MAJOR:?set by the unit environment file}
  home=/var/lib/ti-gateway
  cmd=(xvfb-run -a --server-args="-screen 0 1024x768x24"
    /opt/ibc/scripts/ibcstart.sh "$major" --gateway
    --tws-path="$home/Jts"
    --tws-settings-path="$home/Jts"
    --ibc-path=/opt/ibc
    --ibc-ini="${RUNTIME_DIRECTORY:?no RuntimeDirectory}/config.ini"
    --on2fatimeout=restart)
fi

# watch passes IBC's output through to the journal and reacts to its login
# events. It runs in a subshell started before the child's pid is known, so it
# reads the pid the child recorded and hands its verdict to the main script in
# the decision file. It signals only that one pid, and only a plain positive
# one above 1 (fixrules rule 10: never pid 0, -1 or a group).
watch() {
  local line n child
  while IFS= read -r line || test -n "$line"; do
    printf '%s\n' "$line"
    case $line in
      *'Login has completed'*) : >"$state" ;;
    esac
    if [[ $line =~ $failure ]]; then
      date +%s >>"$state"
      n=$(failures_in_window)
      if test "$n" -ge "$max"; then echo bound >"$decision"; else echo retry >"$decision"; fi
      echo "start-gateway: IBC reports a refused login ($n of $max within the window); stopping IBC" >&2
      child=$(cat "$pidfile" 2>/dev/null || true)
      case $child in
        ''|*[!0-9]*|0|1) ;;
        *) kill -TERM "$child" 2>/dev/null || true ;;
      esac
    fi
  done
}

sh -c 'echo "$$" >"$1"; shift; exec "$@"' _ "$pidfile" "${cmd[@]}" > >(watch) 2>&1 &
child=$!
status=0
wait "$child" || status=$?
# Let the watcher finish writing its verdict before reading it.
sleep 0.2
case $(cat "$decision" 2>/dev/null || true) in
  bound) echo "start-gateway: login retries stopped: the bound of $max refused logins is reached; not retrying" >&2; exit 77 ;;
  retry) exit 75 ;;
esac
exit "$status"
