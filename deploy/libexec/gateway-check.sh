#!/bin/bash
# Tells whether IB Gateway is logged in to IBKR, from the journals, for the
# reporter's Gateway check (cmd/reporter -gateway-check; ADR 0019, "Alerting").
# Runs as ti-trader, which is in the systemd-journal group. It fails closed:
# a journal it cannot read is never reported as a login.
#
# Output contract (the reporter pages with this text):
#   exit 0  "gateway: logged in ..." (IBC's newest login event is a completed
#           login)
#   exit 1  "gateway: logged out: <evidence>" (IBC's newest login event is a
#           refused-login dialog, a login screen with no completed login after
#           it, or the retry stop; or an adapter has reported IBKR's servers
#           lost, codes 1100/2110, for at least TI_GATEWAY_LINK_LOSS_AFTER)
#   exit 2  "gateway: could not tell: <why>" (journalctl missing, or failing)
#   exit 4  "gateway: no login evidence ..." (the journal reads but holds no
#           IBC login event: a fresh server, or the evidence has rotated away;
#           the reporter keeps an open outage open and opens none from this)
# An adapter's loss of IBKR counts only once it has lasted (default 3 hours):
# IBKR's scheduled server resets and weekend maintenance drop the link for a
# while without the Gateway being logged out; a login failure shows in IBC's
# own journal at once.
# Inputs (overridable for the rehearsal):
#   TI_JOURNALCTL              the journal reader
#   TI_GATEWAY_ADAPTER_UNITS   space-separated adapter units whose link events count
#   TI_GATEWAY_LINK_LOSS_AFTER journalctl --until value for that age (default -3h)
set -euo pipefail

journalctl=${TI_JOURNALCTL:-journalctl}
link_after=${TI_GATEWAY_LINK_LOSS_AFTER:--3h}
out=""

cannot_tell() { echo "gateway: could not tell: $1"; exit 2; }

# newest UNIT PATTERN [JOURNALCTL OPTION...]: prints the newest matching line
# (nothing when none matches). journalctl exits 1 when -g matches nothing;
# any other failure, or an unreadable journal, ends the check with exit 2.
newest() {
  local unit=$1 pattern=$2 got rc=0
  shift 2
  got=$("$journalctl" -u "$unit" -q -o short-iso --no-pager --since=-14d -g "$pattern" "$@" 2>&1) || rc=$?
  case $rc in
    0) ;;
    1) if test -n "$got"; then cannot_tell "journalctl failed for $unit: $got"; fi ;;
    *) cannot_tell "journalctl failed for $unit (status $rc): $got" ;;
  esac
  printf '%s\n' "$got" | grep -v '^-- ' | tail -n 1 || true
}

# The journal must be readable at all before its silence means anything.
rc=0
"$journalctl" -u ti-gateway.service -q -o cat --no-pager --since=-14d -n 1 >/dev/null 2>&1 || rc=$?
test "$rc" -eq 0 || cannot_tell "journalctl cannot read the ti-gateway journal (status $rc); is this user in the systemd-journal group?"

line=$(newest ti-gateway.service 'Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped')
case $line in
  *'Login has completed'*) ;;
  '') echo "gateway: no login evidence: the ti-gateway journal holds no login event in the last 14 days"; exit 4 ;;
  *) out="IBC's newest login event: $line" ;;
esac

for unit in ${TI_GATEWAY_ADAPTER_UNITS:-}; do
  line=$(newest "$unit" 'ibkr link state=(connectivity-lost|connected)')
  case $line in
    *'state=connectivity-lost'*)
      before=$(newest "$unit" 'ibkr link state=(connectivity-lost|connected)' --until="$link_after")
      case $before in
        *'state=connectivity-lost'*) out="${out:+$out; }$unit has reported IBKR's servers lost (1100/2110) for over ${link_after#-}: $line" ;;
      esac ;;
  esac
done

if test -n "$out"; then
  echo "gateway: logged out: $out"
  exit 1
fi
echo "gateway: logged in (IBC's newest login event is a completed login)"
