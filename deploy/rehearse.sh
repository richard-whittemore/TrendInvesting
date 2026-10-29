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
# The secrets rest on disk only encrypted (ADR 0028): no command in the docs
# may load a credential from the plain-text path.
if grep -rn 'LoadCredential=[a-z]*:/etc/trend-investing' "$repo/docs" "$repo/deploy/systemd" "$repo/deploy/mac-mini/systemd" >&2; then
  echo "REHEARSAL FAIL: a command or unit loads a credential from /etc/trend-investing (the unlocked copy is under /run/trend-investing/secrets)" >&2
  exit 1
fi
# shellcheck source=/dev/null
. "$repo/deploy/rehearsal/platform.sh"
platform=$(rehearsal_platform)
goarch=$(rehearsal_goarch "$platform")
# The secrets ti-unlock decrypts go on a tmpfs of their own, which
# ti-secrets mounts itself, noswap, as on the server: the container is given
# the capability to mount (CAP_SYS_ADMIN), since Docker's --tmpfs cannot set
# noswap.
args=(create -e TI_REHEARSAL=1 --cap-add SYS_ADMIN --platform "$platform")
if test -z "${TI_REHEARSAL_PLATFORM:-}"; then
  args+=(-e TI_REHEARSAL_ANY_ARCH=1 -e TI_REHEARSAL_SKIP_INSTALLER=1)
fi
# Refuse before building anything if the container is not the architecture
# the tools are built for.
rehearsal_arch_matches "$goarch" "$(docker run --rm --platform "$platform" ubuntu:24.04 uname -m)"
# The data step's file and the Go tools (the reporter, the dashboard,
# operator, journal-query and universe-rank, the IRAs' data job's ranker), installed by provision.sh from beside
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
# The synthetic month the IRA engine replays (cmd/engine/testdata; invented
# prices only), as the server's demonstration replays it.
cp "$repo/cmd/engine/testdata/momentum-month.jsonl" "$dist/momentum-month.jsonl"
(cd "$repo" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build \
  -ldflags "-X github.com/richard-whittemore/TrendInvesting/internal/buildinfo.Version=rehearsal" \
  -o "$dist/ti-engine" ./cmd/engine)
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query gateway-start:ti-gateway-start universe-rank:universe-rank; do
  (cd "$repo" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$dist/${tool#*:}" "./cmd/${tool%%:*}")
done
id=$(docker "${args[@]}" ubuntu:24.04 bash /deploy/rehearsal/inside.sh)
sd=""
trap 'docker rm -f "$id" ${sd:+"$sd"} >/dev/null 2>&1 || true; rm -rf "$dist"' EXIT
docker cp "$repo/deploy" "$id:/deploy"
docker cp "$dist" "$id:/dist"
# The adapter's pin check, run inside the container against the real install.
docker cp "$repo/adapter/ibkr/ibapi_pin.py" "$id:/ibapi_pin.py"
docker start -a "$id"

# A second container in which systemd is PID 1: the failed-unit hook as
# systemd itself names and starts it on a templated unit's failure
# (deploy/rehearsal/inside-systemd.sh). Not --privileged: it runs systemd as
# the Mac mini container does (deploy/mac-mini/up.sh, which gives the reason
# for each capability): every capability dropped and the same eleven added
# back, a cgroup namespace of its own whose tree is remounted writable before
# systemd starts, and Docker's default seccomp filter kept. / is mounted
# shared, as up.sh's mount-shared.service does, so that systemd can hand a
# unit its credentials (LoadCredential=) and the rehearsal reads them as the
# server does; gnupg is ti-secrets' tool, as provision.sh base installs it;
# openssl, iproute2 (ss) and tzdata serve the first-day rehearsal's stand-in
# Massive, the runbook's port check and the New York clock; sudo is the
# owner's, as the runbook types it.
caps=(--cap-drop ALL)
for cap in SYS_ADMIN SYS_CHROOT CHOWN FOWNER DAC_OVERRIDE SETUID SETGID SETPCAP AUDIT_WRITE KILL FSETID; do
  caps+=(--cap-add "$cap")
done
sd=$(docker create "${caps[@]}" --cgroupns=private --tmpfs /run --tmpfs /run/lock -e container=docker --platform "$platform" ubuntu:24.04 \
  bash -c 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends systemd dbus python3 gnupg openssl iproute2 curl tzdata tzdata-legacy sudo >/dev/null && mount -o remount,rw /sys/fs/cgroup && mount --make-rshared / && exec /lib/systemd/systemd')
docker cp "$repo/deploy" "$sd:/deploy"
docker cp "$dist" "$sd:/dist"
# The adapter's code (provision.sh adapter installs it from beside deploy/)
# and its scripted fake gateway, and the first-day runbook whose commands
# deploy/rehearsal/first-day.sh runs as written.
docker cp "$repo/adapter" "$sd:/adapter"
docker cp "$repo/docs/runbooks/first-dry-run-day.md" "$sd:/first-dry-run-day.md"
docker start "$sd" >/dev/null
booted=""
for _ in $(seq 120); do
  state=$(docker exec "$sd" systemctl is-system-running 2>/dev/null || true)
  case $state in running | degraded) booted=1 && break ;; esac
  sleep 2
done
test -n "$booted" || { echo "REHEARSAL FAIL: systemd did not boot in the second container" >&2; exit 1; }
docker exec "$sd" bash /deploy/rehearsal/inside-systemd.sh
