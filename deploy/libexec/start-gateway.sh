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
# login ("Unrecognized Username or Password"): its user guide and config.ini
# document only the second-factor timeouts and LoginDialogDisplayTimeout (the
# dialog failing to appear). It leaves the dialog open and never exits, so the
# Gateway sat logged out for hours with the unit active. This script therefore
# reads IBC's output for that dialog and, on the first one in a run, counts it,
# then stops ITS OWN child, the pid it started, so systemd starts the unit
# again after RestartSec. That child is xvfb-run: it gets SIGTERM, and when it
# does not die within STOP_DEADLINE it gets SIGKILL; the rest of the control
# group (IBC's java, Xvfb) is finished by systemd's KillMode=control-group when
# this script exits (deploy/systemd/ti-gateway.service sets it explicitly).
# Only that one pid is ever signalled: never a group, pid 0 or -1, no killall
# or pkill (docs/development.md, "Ending processes").
#
# Lockout rule. IBKR's Client Portal documentation says "After ten failed login
# attempts in 24 hours, your Portal log in will be disabled" (IBKR Guides, "Log
# In and Log Out", https://www.ibkrguides.com/clientportal/gettingstarted/login.htm;
# the knowledge-base FAQ "Troubleshooting client portal login failures",
# article 1132, gives the same ten attempts in 24 hours). Those pages describe
# the Portal login; IBKR does not publish a separate figure for the Gateway's
# API login, so this assumes the same one. The owner's own failed web attempts
# count against it too, so the bound is 3 refused logins within 24 hours, well
# under 10.
#
# The count is a file of epoch-second lines, written durably (the file and its
# directory synced) BEFORE the child is stopped, and emptied by a completed
# login. At the bound the script exits 77, which the unit does not restart on;
# a start while the bound holds, or while the file cannot be trusted
# (malformed, not a regular file, unwritable), exits 77 at once without
# launching IBC, since an unknown history cannot authorise another try. The
# owner clears it by deleting the file after checking the login on the website
# (docs/manual/server.md, "The Gateway is logged out").
#
# Exit codes: 75 = a refused login was seen and the bound is not reached
# (systemd starts it again); 77 = the bound is reached or the count cannot be
# trusted; 64, 65, 78 = the renderer's; otherwise IBC's own status.
#
# Inputs (overridable for the rehearsal): TI_GATEWAY_RENDER, TI_GATEWAY_IBC (a
# command run instead of xvfb-run + ibcstart.sh), TI_GATEWAY_STATE_FILE,
# TI_GATEWAY_LOGIN_MAX_FAILURES, TI_GATEWAY_LOGIN_WINDOW (seconds),
# TI_GATEWAY_LOGIN_FAILURE_PATTERN (an extended regex),
# TI_GATEWAY_STOP_DEADLINE (seconds between SIGTERM and SIGKILL).
set -euo pipefail
set +x

"${TI_GATEWAY_RENDER:-/usr/local/libexec/trend-investing/render-ibc-config.sh}" || exit $?

state=${TI_GATEWAY_STATE_FILE:-/var/lib/ti-gateway/login-failures}
max=${TI_GATEWAY_LOGIN_MAX_FAILURES:-3}
window=${TI_GATEWAY_LOGIN_WINDOW:-86400}
failure=${TI_GATEWAY_LOGIN_FAILURE_PATTERN:-Unrecognized Username or Password}
deadline=${TI_GATEWAY_STOP_DEADLINE:-15}
decision="$state.decision"
fifo="$state.fifo"
eof_marker="__TI_GATEWAY_END_OF_OUTPUT__"

# flush FILE...: forces the files (or directory) to disk; where `sync` takes no
# file argument, everything is flushed.
flush() { sync "$@" 2>/dev/null || sync; }

# failures_in_window prints the number of recorded refusals no older than
# WINDOW. It returns 1 for a counter it cannot trust: not a regular file,
# unreadable, or any line that is not a plain epoch-second number.
failures_in_window() {
  local now n=0 at
  now=$(date +%s)
  if test -e "$state" || test -L "$state"; then
    test -f "$state" && test -r "$state" || return 1
    while IFS= read -r at || test -n "$at"; do
      case $at in ''|*[!0-9]*) return 1 ;; esac
      test "${#at}" -le 12 || return 1
      if test $((now - 10#$at)) -lt "$window"; then n=$((n + 1)); fi
    done <"$state"
  fi
  echo "$n"
}

# write_count CONTENT: replaces the counter with CONTENT, durably.
write_count() {
  local tmp="$state.tmp"
  printf '%s' "$1" >"$tmp" && flush "$tmp" && mv -f "$tmp" "$state" && flush "$(dirname "$state")"
}

if ! n=$(failures_in_window); then
  echo "start-gateway: login retries stopped: $state is unreadable or malformed, so the number of refused logins is unknown; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete $state and start the unit" >&2
  exit 77
fi
if test "$n" -ge "$max"; then
  echo "start-gateway: login retries stopped: $n refused logins within the last $((window / 3600)) hours; not starting IBC so the IBKR login cannot be locked. Check the paper login on the IBKR website, then delete $state and start the unit" >&2
  exit 77
fi
# A counter that cannot be written cannot bound anything.
if ! { rm -f "$decision" "$fifo" "$state.tmp" && : >"$state.tmp" && rm -f "$state.tmp"; } 2>/dev/null; then
  echo "start-gateway: login retries stopped: $(dirname "$state") is not writable, so refused logins could not be counted; not starting IBC so the IBKR login cannot be locked" >&2
  exit 77
fi

# IBC's command.
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
# events. It writes the counter durably BEFORE it records its verdict in the
# decision file, and it reacts to the first refusal of a run only (IBC logs the
# dialog several times). It signals nothing: the main script stops the child.
watch() {
  local line n seen="" existing
  while IFS= read -r line || test -n "$line"; do
    test "$line" != "$eof_marker" || break
    printf '%s\n' "$line"
    case $line in
      *'Login has completed'*) write_count "" || true ;;
    esac
    if test -z "$seen" && [[ $line =~ $failure ]]; then
      seen=1
      existing=$(cat "$state" 2>/dev/null || true)
      if n=$(failures_in_window) && write_count "${existing:+$existing$'\n'}$(date +%s)"$'\n'; then
        n=$((n + 1))
        if test "$n" -ge "$max"; then echo bound >"$decision"; else echo retry >"$decision"; fi
        echo "start-gateway: IBC reports a refused login ($n of $max within the window); stopping IBC" >&2
      else
        echo "start-gateway: the refused-login count cannot be saved; stopping IBC and not retrying" >&2
        echo bound >"$decision"
      fi
    fi
  done
}

# The fifo is held open here (read-write, so this never blocks) until the end,
# so the watcher can be told, in order after everything IBC wrote, that the
# output is over even when an orphan of IBC still holds the pipe.
mkfifo -m 0600 "$fifo"
exec 3<>"$fifo"
watch <"$fifo" 3>&- &
watcher=$!
"${cmd[@]}" >"$fifo" 2>&1 3>&- &
child=$!
case $child in [1-9]|[1-9][0-9]*) ;; *) echo "start-gateway: no usable child pid" >&2; exit 70 ;; esac
test "$child" -gt 1 || exit 70

stopping=""
while kill -0 "$child" 2>/dev/null; do
  now=$(date +%s)
  if test -z "$stopping" && test -e "$decision"; then
    stopping=$now
    kill -TERM "$child" 2>/dev/null || true
  elif test -n "$stopping" && test $((now - stopping)) -ge "$deadline"; then
    kill -KILL "$child" 2>/dev/null || true
    break
  fi
  sleep 0.2
done
status=0
wait "$child" || status=$?

# Everything IBC wrote is read, then the watcher ends at the marker.
printf '%s\n' "$eof_marker" >&3
exec 3>&-
for _ in $(seq 1 50); do
  kill -0 "$watcher" 2>/dev/null || break
  sleep 0.2
done
if kill -0 "$watcher" 2>/dev/null; then
  kill -TERM "$watcher" 2>/dev/null || true
  rm -f "$fifo"
  echo "start-gateway: the output watcher did not finish; the refused-login count may be incomplete, so not retrying" >&2
  exit 77
fi
rm -f "$fifo"

case $(cat "$decision" 2>/dev/null || true) in
  bound) echo "start-gateway: login retries stopped: not retrying" >&2; exit 77 ;;
  retry) exit 75 ;;
esac
exit "$status"
