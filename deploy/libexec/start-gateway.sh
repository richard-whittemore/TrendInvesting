#!/bin/bash
# Starts IB Gateway under IBC: a thin hand-off to ti-gateway-start
# (cmd/gateway-start), which renders IBC's config.ini
# (render-ibc-config.sh), starts xvfb-run and IBC as its own child inside a
# virtual display, and bounds the retry of a refused login. Runs as ti-gateway
# from ti-gateway.service. The renderer's configuration exits (64, 65, 78: the
# owner has not filled in the login, or filled it in wrongly) come through
# unchanged, and the unit does not restart on them; exit 77 is the retry bound
# and is not restarted on either (ti-gateway.service). This script signals no
# process: the program it runs signals only its own child.
set -euo pipefail
set +x
exec /opt/trend-investing/bin/ti-gateway-start "$@"
