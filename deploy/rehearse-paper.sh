#!/bin/bash
# The paper rehearsal: synthetic paper days with the real engine. It builds
# the engine (make engine, stamped from git) and runs it in paper mode
# (Boost) on a real socket; the data job (ti-data) writes the run's warm-up
# and each day's stream from synthetic Massive answers; the IBKR adapter's
# start command, with --streams, delivers each behind one account reading
# as of the day's close (the owner's decision of 2026-10-18: the account and
# broker snapshots, then the bar, the mark from its official close and the
# close), with a paper configuration and sending switched on. The engine
# decides the day and releases one order, which goes out once to the fake
# gateway, is accepted and reconciled across a reconnect and a restart;
# the next day it fills after the close, so that day's reading stands at
# its read time and the engine withholds; a crash resume takes the next
# day; and a holding no order explains takes the engine out of Normal.
# Exits non-zero at the first check that fails or if the adapter stops.
# Nothing reaches Massive or IBKR, and no ibapi is needed. Run it from a
# checkout before the first real paper order
# (docs/runbooks/first-paper-order.md; docs/manual/paper-trading.md).
#
#   deploy/rehearse-paper.sh            with a synthetic paper account
#   deploy/rehearse-paper.sh --account  asks for your paper account (DU...)
#                                       at a hidden prompt, so a typo is
#                                       found here; it is never printed
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
case ${1:-} in
  "") TI_REHEARSAL_ACCOUNT= ;;
  --account)
    printf 'paper account (DU...): '
    read -r -s TI_REHEARSAL_ACCOUNT
    echo
    ;;
  *) echo "usage: deploy/rehearse-paper.sh [--account]" >&2; exit 2 ;;
esac
export TI_REHEARSAL_ACCOUNT
make --no-print-directory -C "$repo" engine
export TI_REHEARSAL_ENGINE="$repo/bin/ti-engine"
exec python3 "$repo/adapter/ibkr/tests/rehearse_paper.py"
