#!/bin/bash
# The Mac mini's container, for real: run on the Mac mini (Apple silicon,
# Docker Desktop running). It makes a throwaway container and volumes named
# ti-test-<pid> with deploy/mac-mini/up.sh, from a copy of deploy/ outside the
# synced folders, and checks against the running systemd (Mac mini plan,
# section 5, tests T1, T2, T3 and T10):
#
#   T1   systemd is PID 1; every ti-* unit verifies; the Gateway is masked; the
#        timers and the dashboard are enabled and start under their hardening;
#        the engine and the adapter are installed and stopped.
#   T2   sentinels in each volume (root 0600, ti-trader 0750), the durable log
#        the engine wrote, the owner's filled-in files and a journal line are
#        byte-identical, with the same owners and modes, after a container
#        restart and after a full remove-and-recreate; journal-query still
#        reads the log; a second provisioning pass changes nothing.
#   T3   after each restart the timers and the dashboard are back, the engine
#        (running before) and the adapter are stopped, and the reporter fires
#        within 5 minutes.
#   T10  every mount is a named volume inside Docker's VM; nothing is under
#        the Desktop, Documents or iCloud Drive.
#
# And the container leaves the Docker VM's binfmt handlers alone: they are
# listed before it is created and after every boot and must not change, and
# an amd64 container must still run on the Mac (Rosetta). If amd64 is already
# broken before the test (a restart of Docker Desktop mends it), set
# TI_TEST_AMD64_BASELINE_BROKEN=1 to run the rest; the listing is still
# compared and the amd64 check is reported as not run.
#
# It also runs the clock step in the container (passes, and refuses a
# reference an hour off) and the rehearsal's checks of the owner's tools.
# It needs the network (the image's packages, the clock references) and takes
# about 15 minutes, most of it waiting for the reporter's timer. The owner's
# container (ti) and its volumes are never touched. TI_TEST_KEEP=1 keeps the
# test's container and volumes for a look afterwards.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
name=ti-test-$$
port=$((18000 + $$ % 1000))
work=$(mktemp -d)
cleanup() {
  if test "${TI_TEST_KEEP:-0}" != 1; then
    docker rm --force "$name" >/dev/null 2>&1 || true
    for v in etc state opt journal; do docker volume rm "$name-$v" >/dev/null 2>&1 || true; done
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "TEST FAIL: $*" >&2; exit 1; }
inside() { docker exec "$name" /root/deploy/mac-mini/container-checks.sh "$@"; }
up() { env TI_MAC_NAME="$name" TI_DIST="$work/dist" TI_MAC_DASHBOARD_PORT="$port" "$work/checkout/deploy/mac-mini/up.sh" "$@"; }

step "a checkout outside the synced folders, and the programs built for arm64"
mkdir -p "$work/checkout" "$work/dist"
# Only deploy/ is copied in; a copy, not a link, so up.sh sees no synced path.
(cd "$repo" && tar -cf - deploy) | (cd "$work/checkout" && tar -xf -)
"$repo/deploy/build-data.sh" "$work/dist/ti-data" >/dev/null
cp "$repo/adapter/marketdata/tests/testdata/boost-fixture.log" "$work/dist/boost-fixture.log"
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query; do
  (cd "$repo" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$work/dist/${tool#*:}" "./cmd/${tool%%:*}")
done
echo "checkout $work/checkout, container $name, dashboard port $port"

image="trend-investing-mac-mini:$(shasum -a 256 "$repo/deploy/mac-mini/Dockerfile" | cut -c1-12)"
docker image inspect "$image" >/dev/null 2>&1 || docker build --platform linux/arm64 --tag "$image" - <"$repo/deploy/mac-mini/Dockerfile" >/dev/null
# The VM's binfmt handlers, each with its registration time and settings, seen
# from a throwaway container that starts no systemd.
binfmt() {
  docker run --rm --privileged --platform linux/arm64 --entrypoint /bin/sh "$image" -c \
    'mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc 2>/dev/null; cd /proc/sys/fs/binfmt_misc && for f in *; do case $f in register) ;; *) echo "== $f $(stat -c %Y "$f")"; cat "$f";; esac; done'
}
amd64_image=ubuntu@sha256:a2d43bffc46670d7108bb3c0468c67c5557c4c92e83e1e34cbff1322ad1b94dc
amd64() {
  if test "${TI_TEST_AMD64_BASELINE_BROKEN:-0}" = 1; then
    echo "amd64 check NOT RUN (TI_TEST_AMD64_BASELINE_BROKEN=1)"
    return 0
  fi
  test "$(docker run --rm --platform linux/amd64 "$amd64_image" uname -m 2>&1)" = x86_64 || fail "an amd64 container no longer runs on the Mac ($1)"
  echo "an amd64 container runs on the Mac ($1)"
}
binfmt >"$work/binfmt-0"
amd64 "before the test"

step "create (up.sh)"
up
ALLOWED="ti-reporter@boost.service ti-dashboard.service" inside boot
binfmt_unchanged() {
  binfmt >"$work/binfmt-now"
  diff "$work/binfmt-0" "$work/binfmt-now" || fail "the Docker VM's binfmt handlers changed ($1; above)"
  echo "the Docker VM's binfmt handlers are unchanged ($1)"
  amd64 "$1"
}
binfmt_unchanged "after the container's first boot"

step "T10: every mount is a named volume in Docker's VM"
mounts=$(docker container inspect --format '{{range .Mounts}}{{.Type}} {{.Name}} {{.Destination}} {{.Source}}{{"\n"}}{{end}}' "$name" | sed '/^$/d' | sort)
echo "$mounts"
want=$(printf 'volume %s-etc /etc/trend-investing\nvolume %s-journal /var/log/journal\nvolume %s-opt /opt/trend-investing\nvolume %s-state /var/lib/trend-investing\n' "$name" "$name" "$name" "$name")
test "$(echo "$mounts" | cut -d' ' -f1-3)" = "$want" || fail "the mounts are not exactly the four named volumes"
echo "$mounts" | while read -r _ _ _ src; do
  case $src in /var/lib/docker/volumes/*) ;; *) fail "a volume's source is $src, not in Docker's VM" ;; esac
  case $src in "$HOME"/*) fail "a volume's source is under the home folder: $src" ;; esac
done
binds=$(docker container inspect --format '{{json .HostConfig.Binds}}' "$name")
test "$binds" = null || test "$binds" = '[]' || fail "the container has bind mounts: $binds"
published=$(docker container inspect --format '{{json .HostConfig.PortBindings}}' "$name")
test "$published" = "{\"8088/tcp\":[{\"HostIp\":\"127.0.0.1\",\"HostPort\":\"$port\"}]}" || fail "the dashboard is not published on the Mac's loopback only: $published"
docker exec "$name" getent hosts host.docker.internal >/dev/null || fail "host.docker.internal does not resolve in the container"
test "$(docker container inspect --format '{{.HostConfig.RestartPolicy.Name}}' "$name")" = unless-stopped || fail "the restart policy is not unless-stopped"
docker exec "$name" grep -q '"host": "host.docker.internal"' /etc/trend-investing/adapters/boost.json || fail "the adapter's configuration does not point at host.docker.internal"
test "$(docker exec "$name" uname -m)" = aarch64 || fail "the container is not arm64"
echo "four named volumes in Docker's VM, no bind mount, dashboard on 127.0.0.1:$port only, host.docker.internal resolves, restart unless-stopped, arm64"

step "the test's files, then T1 against the running systemd"
inside fixtures
inside units
inside tools
inside boot
inside clock

step "T2: a second provisioning pass (up.sh again) changes nothing"
inside provision-manifest >"$work/provision-1"
inside state-manifest >"$work/state-1"
up
inside provision-manifest >"$work/provision-2"
diff "$work/provision-1" "$work/provision-2" || fail "the second pass changed what provisioning owns (above)"
inside state-manifest >"$work/state-2"
diff "$work/state-1" "$work/state-2" || fail "the second pass changed the state or the owner's files (above)"
docker exec "$name" grep -q '^# filled in by the owner' /etc/trend-investing/secrets.env || fail "the owner's secrets were overwritten"
docker exec "$name" grep -q '"mark_timeout": 16' /etc/trend-investing/adapters/boost.json || fail "the owner's adapter setting was overwritten"
echo "second pass: provisioning unchanged ($(wc -l <"$work/provision-1") lines), state and owner's files unchanged"
docker exec "$name" systemctl is-active --quiet ti-engine@boost.service || fail "the engine stand-in stopped before the restart"

step "T2, T3: docker restart"
docker restart "$name" >/dev/null
inside boot
binfmt_unchanged "after docker restart"
inside after-restart
inside state-manifest >"$work/state-3"
diff "$work/state-1" "$work/state-3" || fail "the state changed across the restart (above)"
echo "state byte-identical across the restart"

step "T2, T3: remove and recreate (up.sh --recreate)"
old=$(docker container inspect --format '{{.Id}}' "$name")
up --recreate
test "$(docker container inspect --format '{{.Id}}' "$name")" != "$old" || fail "the container was not recreated"
inside boot
binfmt_unchanged "after the re-creation"
inside after-restart
inside state-manifest >"$work/state-4"
diff "$work/state-1" "$work/state-4" || fail "the state changed across the re-creation (above)"
inside provision-manifest >"$work/provision-4"
diff "$work/provision-1" "$work/provision-4" || fail "provisioning a fresh container differs from the first (above)"
inside tools
echo "state byte-identical across the re-creation; provisioning identical; journal-query reads the log"

step "REAL CONTAINER TEST COMPLETE"
