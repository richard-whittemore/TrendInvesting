#!/bin/bash
# The Mac mini's programs, rehearsed: builds the server's Go programs for
# linux/amd64 and linux/arm64 from this checkout's commit, checks from the
# files themselves that both carry the one stamp, packs the data job, and
# runs deploy/mac-mini/programs-inside.sh in an arm64 Ubuntu 24.04
# container, under provision.sh's Mac mini target (TI_TARGET=mac-mini, in
# both modes below): the programs run natively, the engine names the stamp,
# the paper rehearsal passes, provision.sh twsapi refuses a tampered or
# unpinned download and unpacks ibapi from the pinned Mac/Unix archive, and
# no x86_64 file is installed. Until ibapi's protobuf is pinned, ibapi does
# not import, so the run ends "programs rehearsal passed up to the ibapi
# import" and the install record and the adapter's pin check are not
# verified (programs-inside.sh, section 4). Needs Docker and Go; downloads
# the pinned archive (about 11 MB). Sends nothing to IBKR or Massive and uses
# no credential.
#
#   deploy/mac-mini/rehearse-programs.sh                  in a throwaway container
#   deploy/mac-mini/rehearse-programs.sh --container NAME in an existing arm64
#                                                         container (the Mac
#                                                         mini's own, before
#                                                         its first run)
#
# A dirty tree is refused (deploy/build-engine.sh): the stamp would name a
# commit the programs were not built from. The committed tree is copied in
# with `git archive`, never the working directory, so no iCloud duplicate
# (`* 2.py`) can enter the container. Both builds go to a scratch folder of
# their own, removed at the end: dist/ is what the Lightsail runbook copies
# to the x86_64 server, and this script never writes there.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
container=
case ${1:-} in
  "") ;;
  --container) container=${2:?usage: rehearse-programs.sh [--container NAME]} ;;
  *) echo "usage: deploy/mac-mini/rehearse-programs.sh [--container NAME]" >&2; exit 2 ;;
esac

# The stamp a build records, read from the binary (`go version -m`).
stamp_of() {
  go version -m "$1" | awk -F'\t' '$2 == "build" && index($3, "-ldflags=") == 1 { print $3 }' |
    sed -n 's/.*buildinfo\.Version=\([^ "]*\).*/\1/p'
}

builds=$(mktemp -d)
created=
cleanup() {
  if test -n "$created"; then docker rm -f "$created" >/dev/null 2>&1 || true; fi
  rm -rf "$builds"
}
trap cleanup EXIT
"$repo/deploy/build-engine.sh" --out "$builds/amd64"
amd64=$(stamp_of "$builds/amd64/ti-engine")
"$repo/deploy/build-engine.sh" --arch arm64 --out "$builds/arm64"
arm64=$(stamp_of "$builds/arm64/ti-engine")
if test -z "$amd64" || test "$amd64" != "$arm64"; then
  echo "rehearse-programs: the amd64 build is stamped '$amd64' and the arm64 build '$arm64'" >&2
  exit 1
fi
"$repo/deploy/build-data.sh" "$builds/arm64/ti-data" >/dev/null
echo "rehearse-programs: linux/amd64 and linux/arm64 builds both stamped $arm64"

if test -z "$container"; then
  container=$(docker create --platform linux/arm64 ubuntu:24.04 sleep infinity)
  created=$container
  docker start "$container" >/dev/null
fi
test "$(docker exec "$container" uname -m)" = aarch64 || { echo "rehearse-programs: $container is not arm64" >&2; exit 1; }
docker exec "$container" rm -rf /src /dist
git -C "$repo" archive --prefix=src/ HEAD | docker cp - "$container:/"
docker exec "$container" mkdir /dist
for f in ti-engine ti-reporter ti-dashboard operator journal-query ti-data; do
  docker cp "$builds/arm64/$f" "$container:/dist/$f"
done
docker exec -e TI_TARGET=mac-mini -e TI_EXPECT_STAMP="$arm64" "$container" bash /src/deploy/mac-mini/programs-inside.sh
