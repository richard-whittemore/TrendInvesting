#!/bin/bash
# Tests for the Gateway's bounded login retry (deploy/libexec/start-gateway.sh)
# and its health check (deploy/libexec/gateway-check.sh), with stand-ins for
# IBC and journalctl: no real Gateway, no network. The retry bound is what keeps
# a refused login from being retried until IBKR locks the login (10 failed
# attempts in 24 hours).
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
start=${TI_START_GATEWAY:-$here/../libexec/start-gateway.sh}
check=${TI_GATEWAY_CHECK:-$here/../libexec/gateway-check.sh}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}

# A renderer that does nothing, and an IBC that logs the refused-login dialog,
# as IBC 3.24.2 did, then sits there; it counts its launches.
cat >"$work/render.sh" <<'R'
#!/bin/sh
exit 0
R
cat >"$work/ibc-refused.sh" <<R
#!/bin/sh
echo x >>"$work/launches"
echo "IBC: detected dialog entitled: Unrecognized Username or Password; event=Opened"
exec sleep 300
R
cat >"$work/ibc-ok.sh" <<R
#!/bin/sh
echo x >>"$work/launches"
echo "IBC: Login has completed"
exec sleep 1
R
chmod +x "$work/render.sh" "$work/ibc-refused.sh" "$work/ibc-ok.sh"
launches() { grep -c x "$work/launches" 2>/dev/null || echo 0; }

export TI_GATEWAY_RENDER="$work/render.sh" TI_GATEWAY_STATE_FILE="$work/login-failures"
start_with() { # IBC_STANDIN: prints the exit status
  local s=0
  TI_GATEWAY_IBC=$1 "$start" >"$work/out" 2>&1 || s=$?
  echo "$s"
}

# A refused login stops IBC (its own child) and asks systemd to start it
# again: 75, twice; the third refusal reaches the bound: 77; a fourth start
# does not launch IBC at all.
expect "first refused login restarts" 75 "$(start_with "$work/ibc-refused.sh")"
expect "second refused login restarts" 75 "$(start_with "$work/ibc-refused.sh")"
expect "third refused login stops the retries" 77 "$(start_with "$work/ibc-refused.sh")"
expect "IBC was launched 3 times" 3 "$(launches)"
expect "a start at the bound is refused" 77 "$(start_with "$work/ibc-refused.sh")"
expect "a start at the bound launches no IBC" 3 "$(launches)"
grep -q 'login retries stopped' "$work/out" || expect "the refusal says retries stopped" yes no

# One refused login that IBC logs three times is one refusal, signalled once.
rm -f "$work/login-failures" "$work/launches"
cat >"$work/ibc-noisy.sh" <<R
#!/bin/sh
echo x >>"$work/launches"
for i in 1 2 3; do echo "IBC: detected dialog entitled: Unrecognized Username or Password; event=Opened \$i"; done
exec sleep 300
R
chmod +x "$work/ibc-noisy.sh"
expect "a refusal logged three times restarts, not stops" 75 "$(start_with "$work/ibc-noisy.sh")"
expect "a refusal logged three times is counted once" 1 "$(grep -c . "$work/login-failures")"

# An IBC that ignores SIGTERM (as a wrapper that traps it would) is still
# stopped: its own pid is killed after the deadline, and the retry goes on.
rm -f "$work/login-failures"
cat >"$work/ibc-deaf.sh" <<R
#!/bin/sh
trap '' TERM
echo "IBC: detected dialog entitled: Unrecognized Username or Password; event=Opened"
while :; do sleep 1; done
R
chmod +x "$work/ibc-deaf.sh"
s=0; TI_GATEWAY_STOP_DEADLINE=2 TI_GATEWAY_IBC="$work/ibc-deaf.sh" "$start" >"$work/out" 2>&1 || s=$?
expect "an IBC that ignores SIGTERM is still stopped and retried" 75 "$s"

# A counter that cannot be trusted refuses to launch: malformed, a directory.
rm -f "$work/launches"
echo abc >"$work/login-failures"
expect "a malformed counter refuses to start" 77 "$(start_with "$work/ibc-refused.sh")"
grep -q 'unreadable or malformed' "$work/out" || expect "the refusal says the counter is malformed" yes no
expect "a malformed counter launches no IBC" 0 "$(launches)"
rm -f "$work/login-failures"; mkdir "$work/login-failures"
expect "a counter that is a directory refuses to start" 77 "$(start_with "$work/ibc-refused.sh")"
rmdir "$work/login-failures"
# A counter that cannot be written fails closed: no retry is authorised.
if test "$(id -u)" -ne 0; then
  mkdir "$work/ro"; chmod 0500 "$work/ro"
  s=0; TI_GATEWAY_STATE_FILE="$work/ro/login-failures" TI_GATEWAY_IBC="$work/ibc-refused.sh" "$start" >"$work/out" 2>&1 || s=$?
  chmod 0700 "$work/ro"
  expect "a counter that cannot be written stops the retries" 77 "$s"
fi

# Refusals older than the window do not count.
old=$(( $(date +%s) - 90000 ))
printf '%s\n%s\n%s\n' "$old" "$old" "$old" >"$work/login-failures"
expect "refusals older than 24 hours are forgotten" 75 "$(start_with "$work/ibc-refused.sh")"

# A completed login empties the count.
printf '%s\n%s\n' "$(date +%s)" "$(date +%s)" >"$work/login-failures"
expect "a completed login exits with IBC's status" 0 "$(start_with "$work/ibc-ok.sh")"
expect "a completed login resets the count" 0 "$(grep -c . "$work/login-failures" || true)"

# The health check, against a stand-in journalctl that honours -u, -g (an
# extended regex; exit 1 and no output when nothing matches), -n, -q (without
# it a query with no entries prints journalctl's own marker), --until (the
# journal as it stood then is the unit's ".old" file) and JOURNAL_FAIL.
cat >"$work/journalctl" <<'J'
#!/bin/bash
unit="" pattern="" quiet="" lines="" old=""
while test $# -gt 0; do
  case $1 in
    -u) unit=$2; shift ;;
    -g) pattern=$2; shift ;;
    -q|--quiet) quiet=1 ;;
    -n) lines=$2; shift ;;
    --until=*) old=.old ;;
  esac
  shift
done
if test -n "${JOURNAL_FAIL:-}"; then echo "Failed to open journal: Permission denied" >&2; exit 1; fi
file="$JOURNAL_DIR/$unit$old"
out=$(cat "$file" 2>/dev/null || true)
if test -n "$pattern"; then
  out=$(printf '%s\n' "$out" | grep -E -- "$pattern" || true)
  if test -z "$out"; then test -n "$quiet" || echo "-- No entries --"; exit 1; fi
fi
if test -z "$out"; then test -n "$quiet" || echo "-- No entries --"; exit 0; fi
if test -n "$lines"; then printf '%s\n' "$out" | tail -n "$lines"; else printf '%s\n' "$out"; fi
J
chmod +x "$work/journalctl"
export TI_JOURNALCTL="$work/journalctl" JOURNAL_DIR="$work/journal"
mkdir -p "$JOURNAL_DIR"
# status prints the check's exit status: 0 logged in, 1 logged out, 2 could
# not tell, 4 the journal holds no login event.
status() { local s=0; "$check" >"$work/check-out" 2>&1 || s=$?; echo "$s"; }
cat >"$JOURNAL_DIR/ti-gateway.service" <<'J'
2026-10-27T23:44:00 ibc[1]: Login has completed
2026-10-27T23:45:30 ibc[1]: Login dialog WINDOW_OPENED: LoginState is LOGGED_OUT
2026-10-27T23:45:40 ibc[1]: detected dialog entitled: Unrecognized Username or Password; event=Opened
J
expect "a refused-login dialog is logged out" 1 "$(status)"
grep -q '^gateway: logged out' "$work/check-out" || expect "logged-out output" yes no
echo '2026-10-27T23:46:00 ibc[1]: some unrelated IBC line' >>"$JOURNAL_DIR/ti-gateway.service"
expect "an unrelated line after a refusal is still logged out" 1 "$(status)"
echo '2026-10-27T23:50:00 ibc[1]: Login has completed' >>"$JOURNAL_DIR/ti-gateway.service"
echo '2026-10-27T23:50:05 ibc[1]: some unrelated IBC line' >>"$JOURNAL_DIR/ti-gateway.service"
expect "a completed login followed by noise is logged in" 0 "$(status)"
echo '2026-10-28T00:15:00 ibc[1]: Login dialog WINDOW_OPENED: LoginState is LOGGED_OUT' >>"$JOURNAL_DIR/ti-gateway.service"
expect "LOGGED_OUT with no completed login after it is logged out" 1 "$(status)"
echo '2026-10-28T00:20:00 start-gateway: login retries stopped: 3 refused logins' >>"$JOURNAL_DIR/ti-gateway.service"
expect "the retry stop is logged out" 1 "$(status)"
# The journal cannot be read: could not tell, never logged in.
JOURNAL_FAIL=1 expect "an unreadable journal could not be told" 2 "$(JOURNAL_FAIL=1 status)"
expect "a missing journalctl could not be told" 2 "$(TI_JOURNALCTL=$work/none status)"
# A readable journal with no login event (or nothing at all) is neither: 4,
# and the marker journalctl prints for no entries is not an event.
echo '2026-10-27T10:00:00 ibc[1]: some unrelated IBC line' >"$JOURNAL_DIR/ti-gateway.service"
expect "a journal with no login event is no evidence" 4 "$(status)"
: >"$JOURNAL_DIR/ti-gateway.service"
expect "an empty journal is no evidence, not logged in or out" 4 "$(status)"
grep -q 'No entries' "$work/check-out" && expect "the no-entries marker is not reported as an event" absent present
# An adapter's loss of IBKR (1100/2110) counts only once it has lasted 3 hours
# (IBKR's scheduled resets and weekend maintenance are shorter), judged by the
# journal as it stood 3 hours ago.
echo '2026-10-28T09:00:00 ibc[1]: Login has completed' >"$JOURNAL_DIR/ti-gateway.service"
export TI_GATEWAY_ADAPTER_UNITS="ti-adapter-boost.service"
echo '2026-10-28T10:00:00 adapter: ibkr link state=connectivity-lost reason=gateway-lost-ibkr client_id=11 attempt=None code=2110' >"$JOURNAL_DIR/ti-adapter-boost.service"
echo '2026-10-28T06:00:00 adapter: ibkr link state=connected reason=restored client_id=11 attempt=None code=1102' >"$JOURNAL_DIR/ti-adapter-boost.service.old"
expect "a link lost for under 3 hours is not a logout" 0 "$(status)"
echo '2026-10-28T06:30:00 adapter: ibkr link state=connectivity-lost reason=gateway-lost-ibkr client_id=11 attempt=None code=2110' >"$JOURNAL_DIR/ti-adapter-boost.service.old"
expect "a link lost for over 3 hours is a logout" 1 "$(status)"
echo '2026-10-28T10:05:00 adapter: ibkr link state=connected reason=restored client_id=11 attempt=None code=1102' >>"$JOURNAL_DIR/ti-adapter-boost.service"
expect "a restored link is logged in" 0 "$(status)"

# The rendered config pins the login dialog timeout (render-ibc-config.sh).
render="$here/../libexec/render-ibc-config.sh"
mkdir -p "$work/cred" "$work/run"
printf 'TWS_USERID=u\nTWS_PASSWORD=p\n' >"$work/cred/ibkr-login"
printf '%s\n' 'IbLoginId=' 'IbPassword=' 'TradingMode=live' 'ReadOnlyApi=no' 'AutoRestartTime=' 'ColdRestartTime=' \
  'ReloginAfterSecondFactorAuthenticationTimeout=no' 'AcceptNonBrokerageAccountWarning=no' 'CommandServerPort=7462' \
  'TrustedTwsApiClientIPs=' 'AcceptIncomingConnectionAction=manual' 'LoginDialogDisplayTimeout=20' >"$work/template.ini"
CREDENTIALS_DIRECTORY=$work/cred RUNTIME_DIRECTORY=$work/run IBC_TEMPLATE=$work/template.ini "$render" >/dev/null
grep -qx 'LoginDialogDisplayTimeout=60' "$work/run/config.ini" || expect "login dialog timeout rendered" LoginDialogDisplayTimeout=60 "$(grep '^LoginDialogDisplayTimeout' "$work/run/config.ini" || echo missing)"

test "$failures" -eq 0 || exit 1
echo "gateway retry tests ok"
