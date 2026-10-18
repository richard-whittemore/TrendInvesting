#!/bin/bash
# Packs the daily data job (adapter/marketdata, standard library only) as one
# executable file, a Python zip application run by the server's python3.
#
#   deploy/build-data.sh [OUT]        writes OUT (default dist/ti-data)
#
# Stamped, as deploy/build-engine.sh stamps the engine, with
# scripts/build-stamp.sh (`git describe --always --dirty`): the file prints
# it with --version and at the start of every run, so the job on the server
# names the commit it was built from. A dirty working tree is refused: its
# stamp names a commit the file was not built from. TI_ALLOW_DIRTY=1
# overrides that for a local experiment; never install such a file.
#
# Reproducible: the zip is written with every entry's time fixed and the
# entries in name order and stored uncompressed (no zlib version enters the
# bytes), so two builds of one commit are byte for byte the
# same and a checksum of the installed file identifies the commit.
#
# Install on the server: copy it beside deploy/ as dist/ti-data and run
# `sudo deploy/provision.sh services`, which installs it as
# /opt/trend-investing/bin/ti-data (docs/runbooks/lightsail-deployment.md).
# The tests are left out; nothing else is added.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${1:-$repo/dist/ti-data}
stamp=$(cd "$repo" && scripts/build-stamp.sh)
case $stamp in
  *-dirty*)
    if test "${TI_ALLOW_DIRTY:-0}" != 1; then
      printf 'build-data: the working tree is dirty (%s); commit first\n' "$stamp" >&2
      exit 1
    fi
    ;;
esac
mkdir -p "$(dirname "$out")"
python3 - "$repo/adapter/marketdata" "$stamp" "$out" <<'PY'
import os
import sys
import zipfile

source, stamp, out = sys.argv[1:]
entries = {"__main__.py": "import sys\n\nfrom marketdata import tidata\n\nsys.exit(tidata.main(sys.argv[1:]))\n",
           "marketdata/_build.py": "STAMP = {!r}\n".format(stamp)}
for name in os.listdir(source):
    if name.endswith(".py"):
        with open(os.path.join(source, name), encoding="utf-8") as f:
            entries["marketdata/" + name] = f.read()
tmp = out + ".tmp"
with open(tmp, "wb") as f:
    f.write(b"#!/usr/bin/env python3\n")
    with zipfile.ZipFile(f, "w", zipfile.ZIP_STORED) as z:
        for name in sorted(entries):
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.external_attr = 0o644 << 16
            info.compress_type = zipfile.ZIP_STORED
            z.writestr(info, entries[name].encode("utf-8"))
os.chmod(tmp, 0o755)
os.replace(tmp, out)
PY
printf 'build-data: %s stamped %s\n' "$out" "$stamp"
