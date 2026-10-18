#!/bin/bash
# Builds the server's Go binaries: linux/amd64, stamped with the build
# identity cmd/engine records in every envelope's strategy version
# (scripts/build-stamp.sh: `git describe --always --dirty`, with a dirty
# tree's own tree hash appended; through the Makefile's LDFLAGS). The
# engine refuses -mode dry-run, paper or live on an unstamped build. The
# server has no Go, so the reporter, the dashboard and the owner's tools
# operator and journal-query are built here too, from the same commit.
#
#   deploy/build-engine.sh            writes dist/ti-engine, dist/ti-reporter,
#                                     dist/ti-dashboard, dist/operator and
#                                     dist/journal-query, and prints the stamp
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
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make --no-print-directory engine BUILD_STAMP="$stamp"
mv bin/ti-engine dist/ti-engine
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query; do
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOTOOLCHAIN=local go build \
    -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=$stamp" \
    -o "dist/${tool#*:}" "./cmd/${tool%%:*}"
done
printf 'build-engine: dist/ti-engine, ti-reporter, ti-dashboard, operator and journal-query stamped %s\n' "$stamp"
