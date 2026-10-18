#!/bin/bash
# The paper rehearsal: runs the IBKR adapter's start command end to end
# against the fake gateway and an engine stand-in on a real socket, with a
# paper configuration and sending switched on, and proves one order, its
# status and fills reaching the engine, a reconnect that does not resend,
# and a restart that sends and records nothing twice. Exits non-zero at the
# first check that fails or if the adapter stops. Nothing reaches IBKR, and
# no ibapi is needed. Run it from a checkout before the first real paper
# order (docs/runbooks/first-paper-order.md; docs/manual/paper-trading.md).
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
