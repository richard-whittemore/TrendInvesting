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

# Refusals older than the window do not count.
old=$(( $(date +%s) - 90000 ))
printf '%s\n%s\n%s\n' "$old" "$old" "$old" >"$work/login-failures"
expect "refusals older than 24 hours are forgotten" 75 "$(start_with "$work/ibc-refused.sh")"

# A completed login empties the count.
printf '%s\n%s\n' "$(date +%s)" "$(date +%s)" >"$work/login-failures"
expect "a completed login exits with IBC's status" 0 "$(start_with "$work/ibc-ok.sh")"
expect "a completed login resets the count" 0 "$(grep -c . "$work/login-failures" || true)"

# The health check, against a stand-in journalctl.
cat >"$work/journalctl" <<'J'
#!/bin/sh
unit=""
while test $# -gt 0; do case $1 in -u) unit=$2; shift ;; esac; shift; done
cat "$JOURNAL_DIR/$unit" 2>/dev/null
J
chmod +x "$work/journalctl"
export TI_JOURNALCTL="$work/journalctl" JOURNAL_DIR="$work/journal"
mkdir -p "$JOURNAL_DIR"
status() { local s=0; "$check" >"$work/check-out" 2>&1 || s=$?; echo "$s"; }
cat >"$JOURNAL_DIR/ti-gateway.service" <<'J'
2026-10-27T23:44:00 ibc[1]: Login has completed
2026-10-27T23:45:30 ibc[1]: Login dialog WINDOW_OPENED: LoginState is LOGGED_OUT
2026-10-27T23:45:40 ibc[1]: detected dialog entitled: Unrecognized Username or Password; event=Opened
J
expect "a refused-login dialog is logged out" 1 "$(status)"
grep -q '^gateway: logged out' "$work/check-out" || expect "logged-out output" yes no
echo '2026-10-27T23:50:00 ibc[1]: Login has completed' >>"$JOURNAL_DIR/ti-gateway.service"
expect "a later completed login is logged in" 0 "$(status)"
echo '2026-10-28T00:15:00 ibc[1]: Login dialog WINDOW_OPENED: LoginState is LOGGED_OUT' >>"$JOURNAL_DIR/ti-gateway.service"
expect "LOGGED_OUT with no completed login after it is logged out" 1 "$(status)"
: >"$JOURNAL_DIR/ti-gateway.service"
expect "an empty journal is not a logout" 0 "$(status)"
export TI_GATEWAY_ADAPTER_UNITS="ti-adapter-boost.service"
echo '2026-10-28T10:00:00 adapter: ibkr link state=connectivity-lost reason=gateway-lost-ibkr client_id=11 attempt=None code=2110' >"$JOURNAL_DIR/ti-adapter-boost.service"
expect "an adapter reporting 2110 is logged out" 1 "$(status)"
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
