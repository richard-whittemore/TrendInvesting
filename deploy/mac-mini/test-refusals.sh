#!/bin/bash
# Tests for deploy/mac-mini/up.sh's refusals. Before it touches Docker: a
# checkout under the Desktop, Documents or iCloud Drive (or another synced
# folder), one holding an iCloud sync duplicate ("name 2.ext" or "name 2"),
# however many, or one it cannot read through; a build folder in either
# state; a container name that Docker would read as a path; a dashboard port
# outside 1 to 65535; another up.sh already running for the same container.
# A duplicate would be copied into the container's programs, and a stalled
# sync can hang the copy (docs/manual/mac-mini.md, "Why the checkout is not
# on the Desktop").
#
# Once it reaches Docker: a container of the same name that up.sh did not make
# from this checkout, or whose mounts are not the four volumes, is neither
# changed nor removed; --recreate refuses while an engine or the adapter runs,
# and otherwise stops the container before removing it, never
# force-removing it: `docker stop -t 60` gives systemd's own shutdown 60 s
# and only then does Docker kill what is left; a degraded system is accepted only
# when every failed unit is one that fails until the first engine run, and
# the mount sharing systemd's credentials need is checked; the container is
# created without --privileged.
#
# Each case runs a copy of up.sh from a temporary checkout under a fake home,
# with a stand-in `docker` that records every call and answers as the FAKE_*
# variables say; a refusal before Docker must exit non-zero, say why, and
# leave the record empty. No Docker needed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset TI_MAC_NAME TI_DIST TI_MAC_DASHBOARD_PORT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

fake_home=$work/home
mkdir -p "$fake_home" "$work/bin"
# The stand-in docker: records the call and answers it. `docker info` names
# an x86_64 VM unless FAKE_ARCH says otherwise, and up.sh refuses x86_64, so
# a run that passes every refusal before Docker stops there by default.
#   FAKE_EXISTS=1         a container of the name exists
#   FAKE_LABELS           its labels, "<mac-mini label>|<checkout label>"
#   FAKE_MOUNTS           its mounts, "type name destination;" each (default: the four volumes)
#   FAKE_ACTIVE           the engine and adapter units it reports running
#   FAKE_LIST_FAILS=1     systemd in it does not answer `systemctl list-units`
#   FAKE_SYSTEM           what is-system-running says (default running)
#   FAKE_FAILED           the failed units it lists
#   FAKE_MOUNT_SHARED=1   mount-shared.service is not active
#   FAKE_PROPAGATION      the propagation of / (default shared)
cat >"$work/bin/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_CALLS"
n=${TI_MAC_NAME:-ti}
case "$*" in
  "info "*) echo "${FAKE_ARCH:-x86_64}" ;;
  "container inspect --format "*org.trend-investing.mac-mini*) printf '%s\n' "${FAKE_LABELS:-}" ;;
  "container inspect --format "*.Mounts*) printf '%s\n' "${FAKE_MOUNTS-volume $n-etc /etc/trend-investing;volume $n-state /var/lib/trend-investing;volume $n-opt /opt/trend-investing;volume $n-journal /var/log/journal;}" ;;
  "container inspect --format "*.State.Running*) echo true ;;
  "container inspect --format "*.Config.Image*) echo "${FAKE_IMAGE:-}" ;;
  "container inspect "*) test "${FAKE_EXISTS:-0}" = 1 ;;
  *"systemctl list-units --state=active"*) test "${FAKE_LIST_FAILS:-0}" = 0 || exit 1; test -z "${FAKE_ACTIVE:-}" || printf '%s\n' $FAKE_ACTIVE ;;
  *"systemctl list-units --state=failed"*) test -z "${FAKE_FAILED:-}" || printf '%s\n' $FAKE_FAILED ;;
  *"systemctl is-system-running"*) echo "${FAKE_SYSTEM:-running}" ;;
  *"systemctl is-active --quiet mount-shared.service"*) test "${FAKE_MOUNT_SHARED:-0}" = 0 ;;
  *"findmnt"*) echo "${FAKE_PROPAGATION:-shared}" ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$work/bin/docker"

# checkout DIR: a minimal checkout at DIR holding a copy of the launcher.
checkout() {
  mkdir -p "$1/deploy/mac-mini"
  cp "$here/up.sh" "$here/Dockerfile" "$1/deploy/mac-mini/"
}

# launch DIR [ENV=VALUE...]: runs DIR's up.sh; output in $work/out, docker
# calls in $work/calls; returns its exit status.
launch() {
  local dir=$1
  shift
  : >"$work/calls"
  local status=0
  env HOME="$fake_home" PATH="$work/bin:$PATH" DOCKER_CALLS="$work/calls" "$@" \
    "$dir/deploy/mac-mini/up.sh" >"$work/out" 2>&1 || status=$?
  return "$status"
}

# refused DESCRIPTION WORD DIR [ENV=VALUE...]: the launcher must refuse, name
# WORD in its message, and call docker not at all.
refused() {
  local what=$1 word=$2 dir=$3
  shift 3
  if launch "$dir" "$@"; then
    fail "$what: accepted"
    return
  fi
  grep -qiF -- "$word" "$work/out" || fail "$what: the refusal does not say '$word': $(cat "$work/out")"
  test ! -s "$work/calls" || fail "$what: docker was called before the refusal: $(cat "$work/calls")"
}

for place in "Desktop" "Documents" "Library/Mobile Documents/com~apple~CloudDocs" "Library/CloudStorage/OneDrive-Personal"; do
  co="$fake_home/$place/TrendInvesting"
  checkout "$co"
  refused "a checkout under ~/$place" "$place" "$co"
done
# A folder that is a link into the Desktop is still the Desktop.
checkout "$fake_home/Desktop/linked"
ln -s "$fake_home/Desktop/linked" "$fake_home/TrendInvesting-link"
refused "a checkout reached through a link into the Desktop" "Desktop" "$fake_home/TrendInvesting-link"

clean="$fake_home/TrendInvesting-deploy"
checkout "$clean"

# Duplicates anywhere in the checkout, files and folders.
mkdir -p "$clean/adapter/ibkr"
touch "$clean/adapter/ibkr/cli 2.py"
refused "a checkout holding a '* 2.*' file" "cli 2.py" "$clean"
rm "$clean/adapter/ibkr/cli 2.py"
mkdir "$clean/adapter/ibkr 2"
refused "a checkout holding a '* 2' folder" "ibkr 2" "$clean"
rmdir "$clean/adapter/ibkr 2"

# The build folder copied into the container: neither synced nor duplicated.
mkdir -p "$fake_home/Desktop/dist"
refused "a build folder on the Desktop" "Desktop" "$clean" TI_DIST="$fake_home/Desktop/dist"
mkdir -p "$work/dist"
touch "$work/dist/ti-data 2"
refused "a build folder holding a duplicate" "ti-data 2" "$clean" TI_DIST="$work/dist"
rm "$work/dist/ti-data 2"

# A name with a slash or a space is refused: it names volumes, never a folder.
refused "a container name with a slash" "TI_MAC_NAME" "$clean" TI_MAC_NAME="$fake_home/Desktop/x"
refused "a container name with a space" "TI_MAC_NAME" "$clean" TI_MAC_NAME="my ti"

# The dashboard's port: a number from 1 to 65535, checked before Docker (a
# bad one would otherwise fail `docker create` after --recreate had removed
# the container).
for bad in 0 65536 99999 00 8o88; do
  refused "the dashboard port '$bad'" "TI_MAC_DASHBOARD_PORT" "$clean" TI_MAC_DASHBOARD_PORT="$bad"
done

# Many duplicates: the scan's output is cut to five, without the cut ending
# the scan's pipe early and the refusal with it.
mkdir "$clean/many"
for i in $(seq 1 3000); do : >"$clean/many/file-$i 2.txt"; done
refused "a checkout holding 3000 duplicates" "sync duplicates" "$clean"
rm -rf "$clean/many"
# A folder the scan cannot read through: refused, saying so, rather than
# passed unseen or ended in silence. The scan's failure is made by a
# stand-in find, which fails as find does on a folder it cannot open, so the
# case holds when the suite runs as root too (root reads a mode 000 folder).
mkdir -p "$work/badfind"
cat >"$work/badfind/find" <<'STUB'
#!/bin/bash
echo "find: '$1/locked': Permission denied" >&2
exit 1
STUB
chmod +x "$work/badfind/find"
refused "a checkout whose scan fails" "could not read" "$clean" PATH="$work/badfind:$work/bin:$PATH"
# And for real, where the permission takes effect: not as root.
if test "$(id -u)" != 0; then
  mkdir -p "$clean/locked/inner"
  chmod 000 "$clean/locked"
  refused "a checkout with a folder it cannot read" "could not read" "$clean"
  chmod 755 "$clean/locked"
  rm -rf "$clean/locked"
else
  echo "test-refusals: running as root, so a mode 000 folder is readable; its case is covered by the stand-in find above"
fi

# One up.sh at a time for a container: a second one, while the first holds
# the lock, refuses before Docker. A lock left by a process no longer running
# is taken over.
# The lock is a link naming the process that holds it.
# A holder is its process ID and start time: a process now running under the
# ID of one that died (IDs are handed out again, after a restart above all)
# started at another time, so its link is stale.
self="$$:$(LC_ALL=C TZ=UTC0 ps -o lstart= -p $$ | sed 's/^ *//;s/ *$//')"
reused="$$:Thu Jan  1 00:00:00 1970"
lock=$fake_home/.trend-investing/up-ti.lock
mkdir -p "$fake_home/.trend-investing"
ln -s "$self" "$lock"
refused "a second up.sh while one runs" "another up.sh" "$clean" TI_DIST="$work/dist"
test "$(readlink "$lock")" = "$self" || fail "a refused up.sh changed the lock it did not hold"
# The start time is read the same way whatever the time zone or language of
# the shell that runs up.sh, so a live holder is never taken for a stale one.
: >"$work/calls"
# The language case needs an installed locale that makes ps print the start
# time differently from C; pick one that does, and say which path ran.
c_form=$(LC_ALL=C ps -o lstart= -p $$)
alt_locale=""
for cand in $(locale -a 2>/dev/null | grep -v -e '^C' -e '^POSIX' -e '^en_'); do
  # A candidate whose ps fails or prints nothing shows no start time to compare.
  start=$(LC_ALL=$cand ps -o lstart= -p $$ 2>/dev/null) || continue
  test -n "$start" || continue
  if test "$start" != "$c_form"; then alt_locale=$cand; break; fi
done
if test -n "$alt_locale"; then
  echo "test-refusals: lock start time checked under time zone Pacific/Auckland and locale $alt_locale"
  tz_env=(TZ=Pacific/Auckland LC_ALL="$alt_locale" LANG="$alt_locale")
else
  echo "test-refusals: no installed locale prints the start time differently from C; lock start time checked under the time zone alone"
  tz_env=(TZ=Pacific/Auckland)
fi
if launch "$clean" TI_DIST="$work/dist" "${tz_env[@]}"; then fail "a launcher in another time zone and language ran with a live lock held"; fi
grep -qiF "another up.sh" "$work/out" || fail "a launcher in another time zone and language did not see the live lock: $(cat "$work/out")"
test "$(readlink "$lock")" = "$self" || fail "a launcher in another time zone and language replaced a live lock"
gone=$(sh -c 'echo $$')
rm -f "$lock"
ln -s "$gone" "$lock"
if launch "$clean" TI_DIST="$work/dist"; then :; fi
grep -q '^docker info' "$work/calls" || fail "a stale lock (process $gone) was not taken over: $(cat "$work/out")"
test ! -L "$lock" || fail "up.sh left its lock behind"
test ! -L "$lock.takeover" || fail "up.sh left its takeover lock behind"
# Taking over a stale lock is itself one at a time: while another up.sh
# (here, this test) is taking it over, a second refuses and leaves the lock
# alone, so two runs can never both end up holding it.
rm -f "$lock"
ln -s "$gone" "$lock"
ln -s "$self" "$lock.takeover"
refused "a stale lock another up.sh is taking over" "taking over" "$clean" TI_DIST="$work/dist"
test "$(readlink "$lock")" = "$gone" || fail "an up.sh that lost the takeover changed the lock: $(readlink "$lock")"
test "$(readlink "$lock.takeover")" = "$self" || fail "an up.sh that lost the takeover changed the takeover lock"
rm -f "$lock" "$lock.takeover"
# A taker that died part way (killed, or the Mac lost power) leaves its
# takeover link naming a process no longer running: that link is itself
# stale, and taken over the same way, so it never blocks every later run.
ln -s "$gone" "$lock"
ln -s "$gone" "$lock.takeover"
: >"$work/calls"
if launch "$clean" TI_DIST="$work/dist"; then :; fi
grep -q '^docker info' "$work/calls" || fail "a stale takeover link (process $gone) blocked the lock's takeover: $(cat "$work/out")"
for left in "$lock" "$lock.takeover" "$lock.takeover.takeover"; do
  test ! -L "$left" || fail "up.sh left $left behind after taking over a stale takeover link"
done
# ...and taking over that link is one at a time too: while another up.sh
# holds the link above it, a second refuses and changes none of them.
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
ln -s "$gone" "$lock"
ln -s "$gone" "$lock.takeover"
ln -s "$self" "$lock.takeover.takeover"
refused "a stale takeover link another up.sh is taking over" "taking over" "$clean" TI_DIST="$work/dist"
test "$(readlink "$lock")" = "$gone" && test "$(readlink "$lock.takeover")" = "$gone" && test "$(readlink "$lock.takeover.takeover")" = "$self" \
  || fail "an up.sh that lost the takeover of a stale takeover link changed a link"
# The chain is bounded: when every link up to the last is stale, up.sh
# refuses and names the links to remove, rather than going on without end.
rm -f "$lock.takeover.takeover"
ln -s "$gone" "$lock.takeover.takeover"
refused "a chain of stale takeover links" "by hand" "$clean" TI_DIST="$work/dist"
test "$(readlink "$lock")" = "$gone" || fail "an up.sh refusing a chain of stale links changed the lock"
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
# The ID of a dead run, reused by a live process that is not that run, does
# not hold a link: the lock, a takeover link, and a chain ending in one.
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
ln -s "$reused" "$lock"
: >"$work/calls"
if launch "$clean" TI_DIST="$work/dist"; then :; fi
grep -q '^docker info' "$work/calls" || fail "a lock whose process ID was reused was not taken over: $(cat "$work/out")"
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
ln -s "$gone" "$lock"
ln -s "$reused" "$lock.takeover"
: >"$work/calls"
if launch "$clean" TI_DIST="$work/dist"; then :; fi
grep -q '^docker info' "$work/calls" || fail "a takeover link whose process ID was reused blocked the takeover: $(cat "$work/out")"
for left in "$lock" "$lock.takeover" "$lock.takeover.takeover"; do
  test ! -L "$left" || fail "up.sh left $left behind after taking over a link of a reused ID"
done
# A refusal because a holder lives says how to clear it when it is not an up.sh.
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
ln -s "$gone" "$lock"
ln -s "$self" "$lock.takeover"
refused "a takeover link held by a live run" "if no up.sh runs" "$clean" TI_DIST="$work/dist"
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"
ln -s "$self" "$lock"
refused "a lock held by a live run" "if no up.sh runs" "$clean" TI_DIST="$work/dist"
rm -f "$lock" "$lock.takeover" "$lock.takeover.takeover"

# --- Once Docker is reached (an arm64 VM).
co=$(cd "$clean" && pwd -P)
mine="ti|$co"
mutated() { grep -Eq '^docker (rm|stop|kill|start|cp|create|restart|exec ti (rm|/root))' "$work/calls"; }
# refused_after WHAT WORD [ARGS...] [-- ENV=VALUE...]: the launcher reaches
# Docker, refuses, names WORD, and changes nothing.
refused_after() {
  local what=$1 word=$2
  shift 2
  local args=()
  while test $# -gt 0 && test "$1" != --; do args+=("$1"); shift; done
  test $# -gt 0 && shift
  : >"$work/calls"
  local status=0
  env HOME="$fake_home" PATH="$work/bin:$PATH" DOCKER_CALLS="$work/calls" FAKE_ARCH=arm64 TI_DIST="$work/dist" "$@" \
    "$clean/deploy/mac-mini/up.sh" ${args[@]+"${args[@]}"} >"$work/out" 2>&1 || status=$?
  if test "$status" = 0; then fail "$what: accepted"; return; fi
  grep -qiF -- "$word" "$work/out" || fail "$what: the refusal does not say '$word': $(cat "$work/out")"
  if mutated; then fail "$what: the container was changed before the refusal: $(cat "$work/calls")"; fi
}
# run_after [ARGS...] [-- ENV=VALUE...]: the same, expected to succeed.
run_after() {
  local args=()
  while test $# -gt 0 && test "$1" != --; do args+=("$1"); shift; done
  test $# -gt 0 && shift
  : >"$work/calls"
  env HOME="$fake_home" PATH="$work/bin:$PATH" DOCKER_CALLS="$work/calls" FAKE_ARCH=arm64 TI_DIST="$work/dist" "$@" \
    "$clean/deploy/mac-mini/up.sh" ${args[@]+"${args[@]}"} >"$work/out" 2>&1
}

# A container of the name that up.sh did not make, or made from another
# checkout, or whose mounts are not the four volumes: never adopted, never
# removed.
refused_after "an unlabelled container" "not one up.sh made" -- FAKE_EXISTS=1 FAKE_LABELS="|"
refused_after "an unlabelled container, --recreate" "not one up.sh made" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="|"
# One made by an earlier up.sh, before it recorded the checkout: the refusal
# gives the command that removes it, keeping its volumes.
refused_after "a container from the launcher before the checkout label" "docker rm ti" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="ti|"
grep -qF "volumes are kept" "$work/out" || fail "the refusal of a container from the earlier launcher does not say the volumes are kept: $(cat "$work/out")"
refused_after "a container made from another checkout" "/elsewhere" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="ti|/elsewhere"
refused_after "a container with a fifth mount" "mounts" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="$mine" \
  FAKE_MOUNTS="volume ti-etc /etc/trend-investing;volume ti-state /var/lib/trend-investing;volume ti-opt /opt/trend-investing;volume ti-journal /var/log/journal;bind  /data;"
refused_after "a container missing its state volume" "mounts" -- FAKE_EXISTS=1 FAKE_LABELS="$mine" \
  FAKE_MOUNTS="volume ti-etc /etc/trend-investing;volume ti-opt /opt/trend-investing;volume ti-journal /var/log/journal;"

# --recreate while an engine or the adapter runs: refused, naming the unit.
refused_after "--recreate while the engine runs" "ti-engine@boost.service" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_ACTIVE="ti-engine@boost.service"
refused_after "--recreate while the adapter runs" "ti-adapter-boost.service" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_ACTIVE="ti-adapter-boost.service"
# systemd not answering which units run: refused, saying so, before anything
# is stopped.
refused_after "--recreate when systemd does not list its units" "cannot ask systemd" --recreate -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_LIST_FAILS=1
# Otherwise it stops the container (systemd shuts down within the bound),
# then removes it; nothing is killed or force-removed.
run_after --recreate -- FAKE_EXISTS=1 FAKE_LABELS="$mine" || fail "--recreate of up.sh's own stopped-engine container failed: $(cat "$work/out")"
stop_line=$(grep -n '^docker stop -t 60 ti$' "$work/calls" | cut -d: -f1 || true)
rm_line=$(grep -n '^docker rm ti$' "$work/calls" | cut -d: -f1 || true)
test -n "$stop_line" || fail "--recreate does not stop the container with a 60 s bound first: $(cat "$work/calls")"
test -n "$rm_line" && test -n "$stop_line" && test "$stop_line" -lt "$rm_line" || fail "--recreate does not remove the container after stopping it: $(cat "$work/calls")"
if grep -Eq '^docker (kill|rm (-f|--force))' "$work/calls"; then fail "--recreate kills or force-removes the container: $(cat "$work/calls")"; fi
# The container it creates: labelled with its name and this checkout, and
# given only the capabilities systemd needs (all others dropped), not --privileged.
create=$(grep '^docker create' "$work/calls" || true)
case $create in *--privileged*) fail "the container is created --privileged: $create" ;; esac
case $create in *"--cap-drop ALL"*) ;; *) fail "the container does not drop Docker's default capabilities (--cap-drop ALL): $create" ;; esac
added=$(grep -o -- '--cap-add [A-Z_]*' <<<"$create" | cut -d' ' -f2 | sort | tr '\n' ' ')
test "$added" = "CHOWN DAC_OVERRIDE FOWNER SETGID SETPCAP SETUID SYS_ADMIN SYS_CHROOT " || fail "the container's added capabilities are '$added', not the eight systemd needs: $create"
case $create in *"--cgroupns private"*) ;; *) fail "the container is not created with --cgroupns private: $create" ;; esac
case $create in *"--label org.trend-investing.mac-mini=ti"*"--label org.trend-investing.checkout=$co"*) ;; *) fail "the container is not labelled with its name and checkout: $create" ;; esac
# Its own container, reused: provisioned.
run_after -- FAKE_EXISTS=1 FAKE_LABELS="$mine" || fail "re-provisioning up.sh's own container failed: $(cat "$work/out")"
grep -q 'provision.sh all' "$work/calls" || fail "up.sh's own container was not provisioned: $(cat "$work/calls")"

# A degraded system: accepted only when every failed unit is one that fails
# until the first engine run, and listed either way.
refused_after "a degraded system with mount-shared failed" "mount-shared.service" -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_SYSTEM=degraded FAKE_FAILED="mount-shared.service"
refused_after "a degraded system with journald failed" "systemd-journald.service" -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_SYSTEM=degraded FAKE_FAILED="ti-reporter@boost.service systemd-journald.service"
run_after -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_SYSTEM=degraded FAKE_FAILED="ti-reporter@boost.service ti-dashboard.service" || fail "a degraded system with only the reporter and dashboard failed was refused: $(cat "$work/out")"
grep -qF "ti-reporter@boost.service" "$work/out" || fail "the failed units of a degraded system are not listed: $(cat "$work/out")"
# The mount sharing systemd's credentials need, whatever the state says.
refused_after "mount-shared.service not active" "mount-shared.service" -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_MOUNT_SHARED=1
refused_after "/ not shared" "propagation" -- FAKE_EXISTS=1 FAKE_LABELS="$mine" FAKE_PROPAGATION=private

# The positive control: a clean checkout in the home folder passes every
# refusal and reaches Docker, which the stand-in reports as x86_64.
if launch "$clean" TI_DIST="$work/dist"; then fail "the x86_64 Docker VM was accepted"; fi
grep -q '^docker info' "$work/calls" || fail "a clean checkout never reached Docker: $(cat "$work/out")"
grep -qi 'arm64' "$work/out" || fail "the x86_64 refusal does not say arm64: $(cat "$work/out")"
if grep -Eq '^docker (create|run|volume create|build)' "$work/calls"; then fail "something was created on an x86_64 VM: $(cat "$work/calls")"; fi

test "$failures" -eq 0 || exit 1
echo "mac-mini refusal tests ok"
