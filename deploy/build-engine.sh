#!/bin/bash
# Builds the server's Go binaries: linux/amd64 by default (the Lightsail
# server), or linux/arm64 with --arch arm64 (the Mac mini's container, which
# runs natively on Apple silicon, never under emulation), stamped with the build
# identity cmd/engine records in every envelope's strategy version
# (scripts/build-stamp.sh: `git describe --always --dirty`, with a dirty
# tree's own tree hash appended; through the Makefile's LDFLAGS). The
# engine refuses -mode dry-run, paper or live on an unstamped build. The
# server has no Go, so the reporter, the dashboard, the owner's tools
# operator and journal-query, and universe-rank, the Go ranker the IRAs' data
# job runs (ti-data@.service), are built here too, from the same commit.
#
#   deploy/build-engine.sh            writes dist/ti-engine, dist/ti-reporter,
#                                     dist/ti-dashboard, dist/operator,
#                                     dist/journal-query and dist/universe-rank
#                                     for linux/amd64, and prints the platform
#                                     and the stamp
#   deploy/build-engine.sh --arch arm64
#                                     the same six for linux/arm64, from the
#                                     same commit with the same stamp
#   deploy/build-engine.sh ... --out DIR
#                                     writes the six into DIR instead of
#                                     dist/, which is left as it was
#
# The architecture changes nothing but GOARCH: the default build is the one
# this script always made, byte for byte, and both carry the one stamp of
# the commit. Each run replaces its directory's binaries with that
# architecture's; dist/ is what the Lightsail runbook copies to the server,
# so a build for another machine goes elsewhere with --out.
#
# A dirty working tree is refused: its stamp names a commit the binary was
# not built from. TI_ALLOW_DIRTY=1 overrides that for a local experiment;
# never install such a binary on the server.
#
# Install on the server (owner step, see docs/runbooks/lightsail-deployment.md):
# with dist/ copied beside deploy/, `sudo deploy/provision.sh services`
# refuses, installing nothing, if any of the six is not built for the
# server's architecture, and installs the other five; then
#   sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
usage() { echo "usage: deploy/build-engine.sh [--arch amd64|arm64] [--out DIR]" >&2; exit 1; }
goarch=amd64
out=
while test $# -gt 0; do
  case $1 in
    --arch) test $# -ge 2 || usage; goarch=$2; shift 2 ;;
    --out) test $# -ge 2 && test -n "$2" || usage; out=$2; shift 2 ;;
    *) usage ;;
  esac
done
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
out=${out:-$repo/dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$repo"
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 make --no-print-directory engine BUILD_STAMP="$stamp"
mv bin/ti-engine "$out/ti-engine"
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query universe-rank:universe-rank; do
  GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build \
    -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=$stamp" \
    -o "$out/${tool#*:}" "./cmd/${tool%%:*}"
done
printf 'build-engine: %s/ti-engine, ti-reporter, ti-dashboard, operator, journal-query and universe-rank for linux/%s stamped %s\n' "$out" "$goarch" "$stamp"
