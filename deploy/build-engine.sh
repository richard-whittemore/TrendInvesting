#!/bin/bash
# Builds the server's engine binary: linux/amd64, stamped with the build
# identity cmd/engine records in every envelope's strategy version
# (`git describe --always --dirty`, through the Makefile's LDFLAGS). The
# engine refuses -mode dry-run, paper or live on an unstamped build.
#
#   deploy/build-engine.sh            writes dist/ti-engine and prints its stamp
#
# A dirty working tree is refused: its stamp names a commit the binary was
# not built from. TI_ALLOW_DIRTY=1 overrides that for a local experiment;
# never install such a binary on the server.
#
# Install on the server (owner step, see docs/runbooks/lightsail-deployment.md):
#   sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
stamp=$(git -C "$repo" describe --always --dirty)
case $stamp in
  *-dirty)
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
printf 'build-engine: dist/ti-engine stamped %s\n' "$stamp"
