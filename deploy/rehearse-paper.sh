#!/bin/bash
# The paper rehearsal: one whole synthetic paper day. The data job (ti-data)
# writes the run's warm-up and the day's stream from synthetic Massive
# answers; the IBKR adapter's start command, with --streams, delivers each
# behind a broker snapshot to an engine stand-in on a real socket, with a
# paper configuration and sending switched on; the stand-in releases one
# order on the day's close, which goes out once to the fake gateway, is
# filled and journalled; a reconnect and a restart send nothing twice.
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
exec python3 "$repo/adapter/ibkr/tests/rehearse_paper.py"
