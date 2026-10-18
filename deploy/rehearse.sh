#!/bin/bash
# Local rehearsal of provision.sh in a clean Ubuntu 24.04 container.
# Needs Docker. Downloads the pinned artefacts (about 350 MB) and verifies
# their checksums; sends nothing to IBKR and uses no real credential. See
# docs/runbooks/lightsail-deployment.md for what this does and does not prove.
#
# The deploy directory is copied in with `docker cp`, not bind-mounted: Docker
# Desktop's file sharing can hang on a bind mount under an iCloud-synced Desktop.
#
# Apple-silicon Docker cannot start linux/amd64 containers reliably, so the
# default is the host's native platform and the x86_64 Gateway installer is
# downloaded and checksum-verified but not executed. Set
# TI_REHEARSAL_PLATFORM=linux/amd64 on an x86_64 host to run the installer too.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
platform=${TI_REHEARSAL_PLATFORM:-}
args=(create -e TI_REHEARSAL=1)
if test -n "$platform"; then
  args+=(--platform "$platform")
else
  args+=(-e TI_REHEARSAL_ANY_ARCH=1 -e TI_REHEARSAL_SKIP_INSTALLER=1)
fi
# The data step's file, installed by provision.sh from beside deploy/, and
# a durable log the engine wrote for the shipped Boost configuration, which
# the rehearsal runs it against.
dist=$(mktemp -d)
"$repo/deploy/build-data.sh" "$dist/ti-data" >/dev/null
cp "$repo/adapter/marketdata/tests/testdata/boost-fixture.log" "$dist/boost-fixture.log"
id=$(docker "${args[@]}" ubuntu:24.04 bash /deploy/rehearsal/inside.sh)
trap 'docker rm -f "$id" >/dev/null 2>&1 || true; rm -rf "$dist"' EXIT
docker cp "$repo/deploy" "$id:/deploy"
docker cp "$dist" "$id:/dist"
docker start -a "$id"
