#!/bin/bash
# Tells whether IB Gateway is logged in to IBKR, from the journals, for the
# reporter's Gateway check (cmd/reporter -gateway-check; ADR 0019, "Alerting").
# Runs as ti-trader, which is in the systemd-journal group.
#
# Output contract (the reporter pages with this text):
#   exit 0  "gateway: logged in ..." (the newest login event is a completed
#           login, or the journals show none)
#   exit 1  "gateway: logged out: <journal line>" (IBC's newest login event is
#           a refused-login dialog, a login screen with no completed login
#           after it, or the retry bound's stop; or an adapter's newest link
#           event is the loss of IBKR's servers, codes 1100/2110)
# Inputs (overridable for the rehearsal):
#   TI_JOURNALCTL              the journal reader
#   TI_GATEWAY_ADAPTER_UNITS   space-separated adapter units whose link events count
set -euo pipefail

journalctl=${TI_JOURNALCTL:-journalctl}
out=""

# The newest line of a unit's journal matching a pattern. journalctl exits 1
# when nothing matches, which is not an error here.
newest() { # UNIT PATTERN
  "$journalctl" -u "$1" -o short-iso --no-pager --since=-14d -g "$2" 2>/dev/null | tail -n 1 || true
}

line=$(newest ti-gateway.service 'Unrecognized Username or Password|LoginState is LOGGED_OUT|Login has completed|login retries stopped')
case $line in
  *'Login has completed'*) ;;
  '') ;;
  *) out="IBC's newest login event: $line" ;;
esac

for unit in ${TI_GATEWAY_ADAPTER_UNITS:-}; do
  line=$(newest "$unit" 'ibkr link state=(connectivity-lost|connected)')
  case $line in
    *'state=connectivity-lost'*) out="${out:+$out; }$unit reports IBKR's servers lost (1100/2110): $line" ;;
  esac
done

if test -n "$out"; then
  echo "gateway: logged out: $out"
  exit 1
fi
echo "gateway: logged in (no login failure after the newest completed login)"
