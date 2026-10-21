#!/bin/bash
# The Mac mini's rehearsal container, for real: run on the Mac (Apple
# silicon, Docker Desktop running). It makes a throwaway container and volumes
# named ti-test-<pid> with deploy/mac-mini/up.sh, from a copy of deploy/
# outside the synced folders, and checks against the running systemd
# (docs/manual/mac-mini.md, "What backs this", says what each part backs):
#
#   T1   systemd is PID 1; every ti-* unit verifies; the Gateway is masked; the
#        timers and the dashboard are enabled and start under their hardening;
#        the engine and the adapter are installed and stopped.
#   T2   the selected durable files: sentinels in each volume (root 0600,
#        ti-trader 0750), the durable log the engine wrote and the owner's
#        filled-in files keep their bytes, owners (by number) and modes, and a
#        journal line written before stays readable, after a container
#        restart, after a full remove-and-recreate and after a re-creation
#        onto a changed image whose own system user takes an ID first;
#        journal-query still reads the log; a second provisioning pass
#        changes nothing. Not compared: the rest of the volumes, which the
#        services and systemd write to as they run (the journal, the
#        reporter's state).
#   T3   after each restart the timers and the dashboard are back, the engine
#        (running before) and the adapter are stopped, and the reporter fires
#        within 5 minutes.
#   T10  every mount is a named volume inside Docker's VM; nothing is under
#        the Desktop, Documents or iCloud Drive; the container is not
#        privileged, drops every default capability and adds back only eleven
#        (SYS_ADMIN, SYS_CHROOT, CHOWN, FOWNER, DAC_OVERRIDE, SETUID, SETGID,
#        SETPCAP, AUDIT_WRITE, KILL, FSETID), whose bounding set the container's PID 1 shows.
#
# And up.sh refuses --recreate while the engine runs, leaving it running;
# provision.sh reown moves files of an old UID and GID in the volumes to the
# fixed IDs, links themselves, and leaves root's alone, and refuses an old ID
# an account or group in the container holds now.
#
# And the container leaves the Docker VM's binfmt handlers alone: they are
# listed before it is created and after every boot and must not change, and
# an amd64 container must still run on the Mac (Rosetta). If amd64 is already
# broken before the test (a restart of Docker Desktop mends it), set
# TI_TEST_AMD64_BASELINE_BROKEN=1 to run the rest; the listing is still
# compared and the amd64 check is reported as not run.
#
# It also runs the clock step and the clock check's unit in the container
# (passes, and refuses a reference an hour off) and the rehearsal's checks of
# the owner's tools. The reporter's ntfy and PagerDuty stand-in runs on the
# Mac, reached as host.docker.internal, so it is there before the reporter's
# timer can fire after each boot.
# It needs the network (the image's packages, the clock references) and takes
# about 15 minutes, most of it waiting for the reporter's timer. The owner's
# container (ti) and its volumes are never touched. TI_TEST_KEEP=1 keeps the
# test's container and volumes for a look afterwards.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
name=ti-test-$$
port=$((18000 + $$ % 1000))
sink_port=$((19000 + $$ % 1000))
work=$(mktemp -d)
sink_pid=
shifted_image=
cleanup() {
  if test -n "$sink_pid"; then kill "$sink_pid" 2>/dev/null || true; fi
  if test "${TI_TEST_KEEP:-0}" != 1; then
    # The test's own container only: its stand-in engine holds nothing.
    docker rm --force "$name" >/dev/null 2>&1 || true
    # The foreign-volume step's container exists only if that refusal failed;
    # it would restart by itself, so it is stopped and removed before its volumes.
    docker stop -t 5 "$name-x" >/dev/null 2>&1 || true
    docker rm --force "$name-x" >/dev/null 2>&1 || true
    for v in etc state opt journal; do docker volume rm "$name-$v" "$name-x-$v" >/dev/null 2>&1 || true; done
    if test -n "$shifted_image"; then docker image rm "$shifted_image" >/dev/null 2>&1 || true; fi
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "TEST FAIL: $*" >&2; exit 1; }
# docker exec passes the container's environment, not this one's: what a
# check reads from here is passed by name.
inside() { docker exec --env "ALLOWED=${ALLOWED:-}" --env "SINK_PORT=$sink_port" "$name" /root/deploy/mac-mini/container-checks.sh "$@"; }
up() { env TI_MAC_NAME="$name" TI_DIST="$work/dist-link" TI_MAC_DASHBOARD_PORT="$port" "$work/checkout/deploy/mac-mini/up.sh" "$@"; }

step "a checkout outside the synced folders, and the programs built for arm64"
mkdir -p "$work/checkout" "$work/dist"
# The build folder is reached through a symbolic link, as a TI_DIST may be: the programs must still arrive.
ln -s "$work/dist" "$work/dist-link"
# Only deploy/ is copied in; a copy, not a link, so up.sh sees no synced path.
(cd "$repo" && tar -cf - deploy) | (cd "$work/checkout" && tar -xf -)
"$repo/deploy/build-data.sh" "$work/dist/ti-data" >/dev/null
cp "$repo/adapter/marketdata/tests/testdata/boost-fixture.log" "$work/dist/boost-fixture.log"
for tool in reporter:ti-reporter dashboard:ti-dashboard operator:operator journal-query:journal-query; do
  (cd "$repo" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 GOTOOLCHAIN=local go build -o "$work/dist/${tool#*:}" "./cmd/${tool%%:*}")
done
echo "checkout $work/checkout, container $name, dashboard port $port"

step "the stand-in for ntfy and PagerDuty, on the Mac's loopback port $sink_port"
cat >"$work/sink.py" <<'SINK'
import http.server, sys

posts = []

class Handler(http.server.BaseHTTPRequestHandler):
    # A connection that sends nothing, or a body shorter than it declares, is
    # dropped after 5 s rather than holding this one-at-a-time server.
    timeout = 5

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        posts.append(self.path)
        self.send_response(202)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"success"}')

    def do_GET(self):
        if self.path == "/reset":
            posts.clear()
        body = "".join(p + "\n" for p in posts).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
SINK
python3 -I "$work/sink.py" "$sink_port" &
sink_pid=$!
# Every request to it is bounded, so a port held by something that never
# answers fails the test instead of hanging it.
for _ in $(seq 50); do curl -fsS --connect-timeout 1 --max-time 2 -o /dev/null "http://127.0.0.1:$sink_port/posts" 2>/dev/null && break; sleep 0.1; done
curl -fsS --connect-timeout 2 --max-time 10 -o /dev/null "http://127.0.0.1:$sink_port/posts" || fail "the stand-in did not start"

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
test "$(docker container inspect --format '{{.HostConfig.Privileged}}' "$name")" = false || fail "the container is privileged"
test "$(docker container inspect --format '{{json .HostConfig.CapDrop}}' "$name")" = '["ALL"]' || test "$(docker container inspect --format '{{json .HostConfig.CapDrop}}' "$name")" = '["CAP_ALL"]' || fail "the container does not drop every default capability"
caps=$(docker container inspect --format '{{range .HostConfig.CapAdd}}{{.}} {{end}}' "$name" | tr ' ' '\n' | sed -e '/^$/d' -e 's/^CAP_//' | sort | tr '\n' ' ')
test "$caps" = "AUDIT_WRITE CHOWN DAC_OVERRIDE FOWNER FSETID KILL SETGID SETPCAP SETUID SYS_ADMIN SYS_CHROOT " || fail "the container's added capabilities are '$caps', not the eleven systemd needs"
test "$(docker container inspect --format '{{.HostConfig.CgroupnsMode}}' "$name")" = private || fail "the container has no cgroup namespace of its own"
echo "four named volumes in Docker's VM, no bind mount, dashboard on 127.0.0.1:$port only, host.docker.internal resolves, restart unless-stopped, arm64, not privileged (all capabilities dropped, eleven added, private cgroup namespace)"

step "the test's files, then T1 against the running systemd"
inside fixtures
inside units
inside stop
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

step "up.sh --recreate refuses while the engine runs"
old=$(docker container inspect --format '{{.Id}}' "$name")
if up --recreate >"$work/recreate-out" 2>&1; then cat "$work/recreate-out"; fail "up.sh --recreate removed the container while the engine ran"; fi
cat "$work/recreate-out"
grep -q 'ti-engine@boost.service' "$work/recreate-out" || fail "the refusal does not name the engine"
test "$(docker container inspect --format '{{.Id}}' "$name")" = "$old" || fail "the container changed although --recreate was refused"
docker exec "$name" systemctl is-active --quiet ti-engine@boost.service || fail "the engine stopped although --recreate was refused"
echo "refused, naming the engine; the container and the engine are untouched"

step "up.sh reuses volumes only when this checkout made them"
for v in etc state opt journal; do
  test "$(docker volume inspect --format '{{index .Labels "org.trend-investing.checkout"}}' "$name-$v")" = "$(cd "$work/checkout" && pwd -P)" || fail "the volume $name-$v is not labelled with the checkout that made it"
done
# Volumes of another checkout, kept after its container was removed, under a
# name of their own so the container above is not involved.
for v in etc state opt journal; do
  docker volume create --label org.trend-investing.mac-mini="$name-x" --label org.trend-investing.checkout=/elsewhere "$name-x-$v" >/dev/null
done
if env TI_MAC_NAME="$name-x" TI_DIST="$work/dist-link" TI_MAC_DASHBOARD_PORT="$((port + 1))" "$work/checkout/deploy/mac-mini/up.sh" >"$work/volumes-out" 2>&1; then cat "$work/volumes-out"; fail "up.sh provisioned over another checkout's volumes"; fi
cat "$work/volumes-out"
grep -q "docker volume rm $name-x-etc" "$work/volumes-out" || fail "the refusal does not give the owner's two choices"
if docker container inspect "$name-x" >/dev/null 2>&1; then fail "a container was created on another checkout's volumes"; fi

step "T2, T3: docker restart"
# The ordered shutdown sends the engine (ti-trader) SIGTERM and ends well
# inside the 60 s stop timeout, not by Docker's SIGKILL at its end.
docker exec "$name" rm -f /var/lib/trend-investing/engines/boost/term-marker
t0=$(date +%s)
docker restart "$name" >/dev/null
took=$(($(date +%s) - t0))
test "$took" -le 40 || fail "docker restart took ${took}s: the ordered shutdown ran into the stop timeout"
echo "docker restart took ${took}s"
inside boot
binfmt_unchanged "after docker restart"
inside after-restart
inside marker
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

step "T2: re-creation onto a changed image whose own system user takes an ID first"
# /etc/passwd is on the container's own disk: the service users are created
# again on the new image. The image's packages are stood in for by one more
# system user, which takes the ID the first service user would have taken
# had it no fixed one.
printf '\n# The container test only: a system user added by the image.\nRUN useradd --system --user-group ti-shift\n' >>"$work/checkout/deploy/mac-mini/Dockerfile"
shifted_image="trend-investing-mac-mini:$(shasum -a 256 "$work/checkout/deploy/mac-mini/Dockerfile" | cut -c1-12)"
inside provision-manifest | grep -E '^ti-(trader|gateway|operator):' >"$work/users-1"
up --recreate
test "$(docker container inspect --format '{{.Config.Image}}' "$name")" = "$shifted_image" || fail "the container is not on the changed image"
shift_id=$(docker exec "$name" id -u ti-shift) || fail "the changed image has no ti-shift"
echo "the changed image's ti-shift took UID $shift_id"
inside boot
binfmt_unchanged "after the re-creation onto a changed image"
inside provision-manifest | grep -E '^ti-(trader|gateway|operator):' >"$work/users-2"
diff "$work/users-1" "$work/users-2" || fail "the service users' IDs changed on the changed image (above)"
grep -q '^ti-operator:x:2003:2002:' "$work/users-2" || fail "ti-operator is not 2003 in ti-trader's group: $(cat "$work/users-2")"
inside state-manifest >"$work/state-5"
diff "$work/state-1" "$work/state-5" || fail "the state changed across the re-creation onto a changed image (above)"
inside tools
echo "service users at the same IDs ($(cut -d: -f1,3,4 "$work/users-2" | tr '\n' ' ')), state byte-identical, journal-query reads the log"

step "re-owning volumes provisioned before the fixed IDs (provision.sh reown)"
# Files a ti-trader at a dynamic UID left in the volumes, and one of root's
# beside them: the step moves the first to 2002:2002 and leaves root's alone.
docker exec "$name" sh -c 'install -d -o 1998 -g 1998 -m 0750 /var/lib/trend-investing/reown-test &&
  install -o 1998 -g 1998 -m 0640 /dev/null /var/lib/trend-investing/reown-test/run.env &&
  ln -s /etc/hostname /var/lib/trend-investing/reown-test/link && chown -h 1998:1998 /var/lib/trend-investing/reown-test/link &&
  install -o 1998 -g 1998 -m 0640 /dev/null /etc/trend-investing/reown-test.env &&
  install -o root -g root -m 0600 /dev/null /var/lib/trend-investing/reown-test/root-own'
docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh reown ti-trader 1998 1998
owners=$(docker exec "$name" stat -c '%u:%g %n' /var/lib/trend-investing/reown-test /var/lib/trend-investing/reown-test/run.env /etc/trend-investing/reown-test.env /var/lib/trend-investing/reown-test/root-own)
owners=$owners$'\n'$(docker exec "$name" stat -c '%u:%g %n' /var/lib/trend-investing/reown-test/link /etc/hostname)
printf '%s\n' "$owners"
test "$(printf '%s\n' "$owners" | grep -c '^2002:2002 ')" = 4 || fail "reown did not move every file of 1998 to 2002:2002 (above)"
printf '%s\n' "$owners" | grep -q '^0:0 /var/lib/trend-investing/reown-test/root-own$' || fail "reown changed root's file (above)"
printf '%s\n' "$owners" | grep -q '^0:0 /etc/hostname$' || fail "reown followed a link out of the volumes (above)"
# An old ID an account holds now is refused before any file changes: root's
# 0 always, and the image's own system account at UID 1 and group 1 (daemon)
# unless --force-id says its files at that ID are the old service user's.
for ids in "0 0" "1" "1998 1"; do
  # shellcheck disable=SC2086 # IDS is the step's one or two arguments.
  if docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh reown ti-trader $ids; then
    fail "reown accepted the old ID(s) $ids, which an account holds now"
  fi
done
test "$(docker exec "$name" stat -c '%u:%g' /var/lib/trend-investing/reown-test/root-own)" = 0:0 || fail "a refused reown changed root's file"
if docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh reown ti-trader 0 --force-id; then
  fail "reown --force-id accepted root's UID 0"
fi
refusal=$(docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh reown ti-trader 1 2>&1 || true)
case $refusal in *daemon*--force-id*) ;; *) fail "the refusal of a colliding ID does not name the account and the way forward: $refusal" ;; esac
docker exec "$name" sh -c 'install -o 1 -g 1 -m 0640 /dev/null /var/lib/trend-investing/reown-test/held-id'
docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh reown ti-trader 1 --force-id
test "$(docker exec "$name" stat -c '%u' /var/lib/trend-investing/reown-test/held-id)" = 2002 || fail "reown --force-id did not move the files of the colliding UID"
test "$(docker exec "$name" stat -c '%u:%g' /var/lib/trend-investing/reown-test/root-own)" = 0:0 || fail "reown --force-id changed root's file"
docker exec "$name" rm -rf /var/lib/trend-investing/reown-test /etc/trend-investing/reown-test.env
echo "reown moved the old UID's files to 2002:2002, links themselves, left root's alone, and refused IDs held now (a system account's only with --force-id)"

step "REAL CONTAINER TEST COMPLETE"
