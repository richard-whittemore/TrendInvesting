#!/bin/bash
# Packs the daily data job (adapter/marketdata, standard library only) as one
# executable file, a Python zip application run by the server's python3.
#
#   deploy/build-data.sh [OUT]        writes OUT (default dist/ti-data)
#
# Install on the server: copy it beside deploy/ as dist/ti-data and run
# `sudo deploy/provision.sh services`, which installs it as
# /opt/trend-investing/bin/ti-data (docs/runbooks/lightsail-deployment.md).
# The tests are left out; nothing else is added.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$repo/dist/ti-data}
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/marketdata" "$(dirname "$out")"
cp "$repo"/adapter/marketdata/*.py "$stage/marketdata/"
cat >"$stage/__main__.py" <<'PY'
import sys

from marketdata import tidata

sys.exit(tidata.main(sys.argv[1:]))
PY
python3 -m zipapp "$stage" --python "/usr/bin/env python3" --output "$out"
chmod 0755 "$out"
printf 'build-data: %s\n' "$out"
