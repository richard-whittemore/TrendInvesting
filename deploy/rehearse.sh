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
# One platform is pinned for the container and for the Go tools: it is
# TI_REHEARSAL_PLATFORM, else DOCKER_DEFAULT_PLATFORM, else the daemon's own,
# and the tools are cross-compiled for it, whatever the host. Only
# TI_REHEARSAL_PLATFORM enables the installer step: DOCKER_DEFAULT_PLATFORM
# alone pins the platform but the installer is still skipped.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=/dev/null
. "$repo/deploy/rehearsal/platform.sh"
platform=$(rehearsal_platform)
goarch=$(rehearsal_goarch "$platform")
args=(create -e TI_REHEARSAL=1 --platform "$platform")
if test -z "${TI_REHEARSAL_PLATFORM:-}"; then
  args+=(-e TI_REHEARSAL_ANY_ARCH=1 -e TI_REHEARSAL_SKIP_INSTALLER=1)
fi
# Refuse before building anything if the container is not the architecture
# the tools are built for.
rehearsal_arch_matches "$goarch" "$(docker run --rm --platform "$platform" ubuntu:24.04 uname -m)"
# The data step's file and the Go tools (the reporter, the dashboard,
# operator and journal-query), installed by provision.sh from beside
# deploy/, and a durable log the engine wrote for the shipped Boost
# configuration, which the rehearsal runs the data step and the reporter
# against. The Go tools are built for the container's pinned platform, as
# deploy/build-engine.sh builds them for the server's. The engine is built the
# same way, stamped (it refuses dry-run on an unstamped build), so the
# rehearsal runs the real engine against the operator inbox the provisioner
# created.
dist=$(mktemp -d)
"$repo/deploy/build-data.sh" "$dist/ti-data" >/dev/null
cp "$repo/adapter/marketdata/tests/testdata/boost-fixture.log" "$dist/boost-fixture.log"
(cd "$repo" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build \
  -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=rehearsal" \
  -o "$dist/ti-engine" ./cmd/engine)
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query; do
  (cd "$repo" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$dist/${tool#*:}" "./cmd/${tool%%:*}")
done
id=$(docker "${args[@]}" ubuntu:24.04 bash /deploy/rehearsal/inside.sh)
trap 'docker rm -f "$id" >/dev/null 2>&1 || true; rm -rf "$dist"' EXIT
docker cp "$repo/deploy" "$id:/deploy"
docker cp "$dist" "$id:/dist"
docker start -a "$id"
