#!/bin/bash
# Builds the server's Go binaries: linux/amd64 by default (the Lightsail
# server), or linux/arm64 with --arch arm64 (the Mac mini's container, which
# runs natively on Apple silicon, never under emulation), stamped with the build
# identity cmd/engine records in every envelope's strategy version
# (scripts/build-stamp.sh: `git describe --always --dirty`, with a dirty
# tree's own tree hash appended; through the Makefile's LDFLAGS). The
# engine refuses -mode dry-run, paper or live on an unstamped build. The
# server has no Go, so the reporter, the dashboard and the owner's tools
# operator and journal-query are built here too, from the same commit.
#
#   deploy/build-engine.sh            writes dist/ti-engine, dist/ti-reporter,
#                                     dist/ti-dashboard, dist/operator and
#                                     dist/journal-query for linux/amd64, and
#                                     prints the platform and the stamp
#   deploy/build-engine.sh --arch arm64
#                                     the same five for linux/arm64, from the
#                                     same commit with the same stamp
#
# The architecture changes nothing but GOARCH: the default build is the one
# this script always made, byte for byte, and both carry the one stamp of
# the commit. Each run replaces dist/'s binaries with that architecture's.
#
# A dirty working tree is refused: its stamp names a commit the binary was
# not built from. TI_ALLOW_DIRTY=1 overrides that for a local experiment;
# never install such a binary on the server.
#
# Install on the server (owner step, see docs/runbooks/lightsail-deployment.md):
#   sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine
# and, with dist/ copied beside deploy/, `sudo deploy/provision.sh services`
# installs the other four.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
usage() { echo "usage: deploy/build-engine.sh [--arch amd64|arm64]" >&2; exit 1; }
goarch=amd64
case $# in
  0) ;;
  2) test "$1" = --arch || usage; goarch=$2 ;;
  *) usage ;;
esac
case $goarch in
  amd64 | arm64) ;;
  *) printf 'build-engine: unsupported architecture %s (amd64 or arm64)\n' "$goarch" >&2; exit 1 ;;
esac
stamp=$(cd "$repo" && scripts/build-stamp.sh)
case $stamp in
  *-dirty-*)
    if test "${TI_ALLOW_DIRTY:-0}" != 1; then
      printf 'build-engine: the working tree is dirty (%s); commit first\n' "$stamp" >&2
      exit 1
    fi
    ;;
esac
mkdir -p "$repo/dist"
cd "$repo"
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 make --no-print-directory engine BUILD_STAMP="$stamp"
mv bin/ti-engine dist/ti-engine
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query; do
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build \
    -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=$stamp" \
    -o "dist/${tool#*:}" "./cmd/${tool%%:*}"
done
printf 'build-engine: dist/ti-engine, ti-reporter, ti-dashboard, operator and journal-query for linux/%s stamped %s\n' "$goarch" "$stamp"
