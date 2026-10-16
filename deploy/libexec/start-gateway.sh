#!/bin/bash
# Starts IB Gateway under IBC inside a virtual display. ibcstart.sh reads its
# credentials from the rendered config.ini, not from arguments, so nothing
# secret appears in the process list. Runs as ti-gateway from ti-gateway.service.
set -euo pipefail
set +x

major=${GATEWAY_MAJOR:?set by the unit environment file}
home=/var/lib/ti-gateway
exec xvfb-run -a --server-args="-screen 0 1024x768x24" \
  /opt/ibc/scripts/ibcstart.sh "$major" --gateway \
  --tws-path="$home/Jts" \
  --tws-settings-path="$home/Jts" \
  --ibc-path=/opt/ibc \
  --ibc-ini="${RUNTIME_DIRECTORY:?no RuntimeDirectory}/config.ini" \
  --on2fatimeout=restart
