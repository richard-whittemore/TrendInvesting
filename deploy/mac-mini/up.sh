#!/bin/bash
# Creates the Mac mini's rehearsal container, or re-attaches to it, and
# provisions it. The container is a local rehearsal environment with a real
# systemd, for trying the provisioner, the units and the owner's steps on the
# Mac before they are done on the Lightsail server, where paper trading runs
# (docs/manual/mac-mini.md; the steps are docs/runbooks/mac-mini.md). Run it
# on the Mac, from a checkout outside the synced folders
# (~/TrendInvesting-deploy), never from one on the Desktop:
#
#   deploy/mac-mini/up.sh              create the container, or start it and re-provision
#   deploy/mac-mini/up.sh --recreate   stop and remove the container (never its volumes) and create it again
#
# The container is one long-running arm64 Ubuntu 24.04 machine with systemd as
# PID 1 (the image is deploy/mac-mini/Dockerfile), restarted by Docker unless
# you stop it. Its state is in four named volumes inside Docker's own Linux VM,
# never in a Mac folder:
#
#   <name>-etc      /etc/trend-investing      configuration and root-only secrets
#   <name>-state    /var/lib/trend-investing  data, the engines' logs and journals, reporter state
#   <name>-opt      /opt/trend-investing      installed programs
#   <name>-journal  /var/log/journal          systemd's log, so why a unit stopped survives
#
# The dashboard's port is published on the Mac's loopback only, and the Mac
# itself (where IB Gateway runs) is host.docker.internal inside. deploy/ and
# the built programs (dist/, or TI_DIST) are copied in with `docker cp`, never
# bind-mounted, and provision.sh runs inside with TI_TARGET=mac-mini. Running
# it again changes nothing but what the checkout changed.
#
# Before it touches Docker it refuses a checkout or build folder under the
# Desktop, Documents, iCloud Drive or another synced folder, one holding an
# iCloud sync duplicate ("name 2.ext" or "name 2"), which would otherwise be
# copied into the programs, or one it cannot read all of; a bad name or port;
# and a second up.sh for the same container while one runs (a lock in
# ~/.trend-investing). It changes or removes an existing container only if
# up.sh made it from this checkout (its labels) with the four volumes as its
# only mounts; it removes one only after systemd has stopped it, and never
# while an engine or the adapter runs.
#
#   TI_MAC_NAME             container name and volume prefix (default ti)
#   TI_DIST                 the built programs to install (default <checkout>/dist)
#   TI_MAC_DASHBOARD_PORT   the Mac loopback port for the dashboard (default 8088)
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../.." && pwd -P)
name=${TI_MAC_NAME:-ti}
dist=${TI_DIST:-$repo/dist}
port=${TI_MAC_DASHBOARD_PORT:-8088}
recreate=0
case ${1:-} in
  '') ;;
  --recreate) recreate=1 ;;
  *) printf 'usage: up.sh [--recreate]\n' >&2; exit 2 ;;
esac

say() { printf 'up: %s\n' "$*"; }
refuse() { printf 'up: %s\n' "$*" >&2; exit 1; }

scratch=$(mktemp -d)
# The lock links this run holds (take_lock), each removed at exit only while
# it still names this process.
held=()
cleanup() {
  local link
  rm -rf "$scratch"
  for link in ${held[@]+"${held[@]}"}; do
    if test "$(readlink "$link" 2>/dev/null)" = "${self:-}"; then rm -f "$link"; fi
  done
}
trap cleanup EXIT

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

# synced_root PATH: prints the synced folder PATH is in, if any. Both PATH and
# each folder are compared as given and with links resolved, without regard
# to case (the Mac's disk ignores it).
synced_root() {
  local path=$1 physical root rootp candidate
  physical=$(cd "$path" 2>/dev/null && pwd -P) || physical=$path
  for root in "$HOME/Desktop" "$HOME/Documents" "$HOME/Library/Mobile Documents" "$HOME/Library/CloudStorage"; do
    rootp=$(cd "$root" 2>/dev/null && pwd -P) || rootp=$root
    for candidate in "$path" "$physical"; do
      case "$(lower "$candidate")/" in
        "$(lower "$root")"/* | "$(lower "$rootp")"/*) printf '%s' "${root#"$HOME"/}"; return 0 ;;
      esac
    done
  done
  return 1
}

# refuse_synced WHAT PATH
refuse_synced() {
  local root
  if root=$(synced_root "$2"); then
    refuse "refusing the $1 at $2: it is under ~/$root, which iCloud or another service syncs. Sync can stall a copy or leave duplicate files that would be installed as programs. Use a checkout of its own in your home folder, such as ~/TrendInvesting-deploy (docs/manual/mac-mini.md)."
  fi
}

# refuse_duplicates WHAT PATH: any "name 2.ext" or "name 2", file or folder,
# and any folder the scan cannot read through. The whole list is written to a
# file and only its first five lines shown, so that no cut of the output ends
# the scan early (under pipefail that would end up.sh without a word).
refuse_duplicates() {
  local list=$scratch/duplicates status=0 count
  find "$2" \( -name '* 2.*' -o -name '* 2' \) -print >"$list" 2>"$list.err" || status=$?
  if test "$status" -ne 0; then
    refuse "refusing the $1 at $2: could not read all of it, so it cannot be checked for iCloud sync duplicates: $(sed -n '1,3p' "$list.err" | tr '\n' ';') Fix the permissions, or take a fresh checkout outside the synced folders."
  fi
  if test -s "$list"; then
    count=$(wc -l <"$list" | tr -d ' ')
    refuse "refusing the $1 at $2: it holds $count what look like iCloud sync duplicates, which would be copied into the container: $(sed -n '1,5p' "$list" | tr '\n' ';') Delete them, or take a fresh checkout outside the synced folders."
  fi
}

# take_lock: one up.sh at a time for this container. The lock is a symbolic
# link naming this process by its ID and start time, made in one step
# (symlink(2) fails if it exists), so of two runs only one makes it. A holder
# is live only if a process with that ID is running and started at that time:
# an ID handed out again, to an unrelated process, after the holder died
# (after a restart above all) is not the holder. One left by a process no
# longer running is taken over, and the takeover is one at a time too: it is
# held by a second link, <lock>.takeover, taken by the same rule. Only its
# holder replaces a stale lock, by renaming a new link over it (rename(2)
# replaces in one step, so the lock never goes missing for a newcomer to take
# meanwhile), and it checks again, holding it, that the lock's holder is still
# the dead one. So no run ever removes or replaces a lock another living run
# holds. A takeover link left by a taker that died part way is stale in turn
# and taken over through <lock>.takeover.takeover; the chain ends there, and a
# stale link at its end is refused with the links to remove by hand, so a run
# never goes on without end.
take_lock() {
  mkdir -p "$HOME/.trend-investing"
  self="$$:$(start_of $$)"
  take_link "$HOME/.trend-investing/up-$name.lock" 0
}

# start_of PID: the process's start time as ps prints it, in the C locale and
# UTC, so the text is the same whatever the time zone or language of the shell
# that reads it (a lock written under one and read under another must still
# name the same start).
start_of() {
  LC_ALL=C TZ=UTC0 ps -o lstart= -p "$1" | sed 's/^ *//;s/ *$//'
}

# holder_runs HOLDER: HOLDER is "PID:START" (a bare PID is checked by the
# process only); true if that process is running and started at START.
holder_runs() {
  local pid=${1%%:*} start now
  test -n "$pid" && ps -p "$pid" >/dev/null 2>&1 || return 1
  case $1 in
    *:*)
      start=${1#*:}
      now=$(start_of "$pid" 2>/dev/null)
      test "$now" = "$start"
      ;;
  esac
}

# take_link PATH DEPTH: holds PATH for this process (adding it to $held) or
# refuses. DEPTH 0 is the lock itself, 1 and 2 the takeover links above it.
take_link() {
  local path=$1 depth=$2 holder top=$HOME/.trend-investing/up-$name.lock
  local hint="if no up.sh runs (pgrep -fl up.sh), remove $path by hand"
  if ln -s "$self" "$path" 2>/dev/null; then held+=("$path"); return; fi
  holder=$(readlink "$path" 2>/dev/null || true)
  if test -n "$holder" && holder_runs "$holder"; then
    if test "$depth" = 0; then
      refuse "another up.sh (process ${holder%%:*}) is working on the container $name; wait for it to finish (the lock is $path); $hint"
    fi
    refuse "another up.sh (process ${holder%%:*}) is taking over the stale lock on $name; wait for it, then run up.sh again; $hint"
  fi
  if test "$depth" -ge 2; then
    refuse "the lock on $name and the links taking it over were all left by runs no longer running ($top, $top.takeover, $path); if no up.sh runs, remove them by hand and run up.sh again"
  fi
  take_link "$path.takeover" $((depth + 1))
  holder=$(readlink "$path" 2>/dev/null || true)
  if test -z "$holder"; then
    # Released meanwhile: taken as a fresh link is, or refused if a newcomer
    # made it first.
    ln -s "$self" "$path" 2>/dev/null || refuse "another up.sh took the lock on $name first; wait for it to finish (the lock is $top)"
  elif holder_runs "$holder"; then
    refuse "another up.sh (process ${holder%%:*}) is working on the container $name or taking over its lock; wait for it to finish (the lock is $top); if no up.sh runs (pgrep -fl up.sh), remove $path by hand"
  else
    if test "$depth" = 0; then say "taking over the lock left by process ${holder%%:*}, which is no longer running"; fi
    rm -f "$path.$$"
    ln -s "$self" "$path.$$"
    mv -f "$path.$$" "$path"
  fi
  held+=("$path")
  rm -f "$path.takeover"
}

case $name in
  '' | *[!a-z0-9-]* | -*) refuse "TI_MAC_NAME must be lower-case letters, digits and hyphens (it names Docker volumes, never a folder), not '$name'" ;;
esac
case $port in
  '' | *[!0-9]* | 0* | ??????*) refuse "TI_MAC_DASHBOARD_PORT must be a port number from 1 to 65535, not '$port'" ;;
esac
test "$port" -le 65535 || refuse "TI_MAC_DASHBOARD_PORT must be a port number from 1 to 65535, not '$port'"
refuse_synced checkout "$repo"
refuse_synced "build folder" "$dist"
refuse_duplicates checkout "$repo"
if test -d "$dist"; then refuse_duplicates "build folder" "$dist"; fi
take_lock

# --- Docker, from here on.
arch=$(docker info --format '{{.Architecture}}')
case $arch in
  aarch64 | arm64) ;;
  *) refuse "Docker's VM is $arch; the Mac mini's container is arm64 (Apple silicon) only" ;;
esac

image="trend-investing-mac-mini:$(shasum -a 256 "$here/Dockerfile" | cut -c1-12)"
if ! docker image inspect "$image" >/dev/null 2>&1; then
  say "building the image $image (Ubuntu 24.04 arm64 with systemd)"
  docker build --platform linux/arm64 --tag "$image" - <"$here/Dockerfile" >/dev/null
fi

for v in etc state opt journal; do
  if ! docker volume inspect "$name-$v" >/dev/null 2>&1; then
    docker volume create --label org.trend-investing.mac-mini="$name" "$name-$v" >/dev/null
    say "created the volume $name-$v"
  fi
done

# check_own: the existing container is one up.sh made, for this name, from
# this checkout, and its only mounts are the four volumes; otherwise nothing
# of it is changed or removed.
check_own() {
  local labels made from mounts want
  labels=$(docker container inspect --format '{{index .Config.Labels "org.trend-investing.mac-mini"}}|{{index .Config.Labels "org.trend-investing.checkout"}}' "$name")
  labels=${labels//<no value>/}
  made=${labels%%|*}
  from=${labels#*|}
  if test "$made" = "$name" && test -z "$from"; then
    refuse "the container $name was made by an earlier up.sh, before it recorded the checkout it was made from, so it cannot be told apart from another checkout's. Nothing was changed. If it is your own rehearsal container: stop its adapter, then its engine (docker exec $name systemctl stop ti-adapter-boost ti-engine@boost), then stop and remove it: docker stop -t 60 $name && docker rm $name (its volumes are kept). Then run up.sh again, which creates it anew with the same volumes"
  fi
  if test "$made" != "$name" || test "$from" != "$repo"; then
    refuse "the container $name is not one up.sh made from this checkout: its labels name '${made:-none}' from '${from:-no checkout}', and this is $name from $repo. Nothing was changed. Use another TI_MAC_NAME, or run up.sh from the checkout that made it."
  fi
  mounts=$(docker container inspect --format '{{range .Mounts}}{{.Type}} {{.Name}} {{.Destination}};{{end}}' "$name" | tr ';' '\n' | sed '/^$/d' | LC_ALL=C sort)
  want=$(printf 'volume %s-etc /etc/trend-investing\nvolume %s-state /var/lib/trend-investing\nvolume %s-opt /opt/trend-investing\nvolume %s-journal /var/log/journal\n' "$name" "$name" "$name" "$name" | LC_ALL=C sort)
  if test "$mounts" != "$want"; then
    refuse "the container $name's mounts are not the four volumes $name-etc, $name-state, $name-opt and $name-journal ($(printf '%s' "$mounts" | tr '\n' ';')). Nothing was changed: files kept outside those volumes would be lost."
  fi
}

exists=0
docker container inspect "$name" >/dev/null 2>&1 && exists=1
if test "$exists" = 1; then check_own; fi
if test "$exists" = 1 && test "$recreate" = 1; then
  if test "$(docker container inspect --format '{{.State.Running}}' "$name")" = true; then
    # Removing the container ends every process in it at once, which for an
    # engine is a crash: its last group may be uncommitted (ADR 0027). The
    # owner stops the engine and the adapter first (docs/manual/restart.md).
    if ! running=$(docker exec "$name" systemctl list-units --state=active,activating,deactivating,reloading --no-legend --plain 'ti-engine@*' 'ti-adapter-*' | awk '{print $1}' | tr '\n' ' '); then
      refuse "cannot ask systemd in $name which units run, so it is not removed: an engine might be running. Nothing was changed. See: docker logs $name"
    fi
    running=${running% }
    if test -n "$running"; then
      refuse "refusing to remove $name while $running runs: stop it first (docker exec $name systemctl stop ${running%% *}; the adapter before its engine), then run up.sh --recreate again"
    fi
    # systemd shuts its units down in order on the image's STOPSIGNAL; the
    # bound is the container's --stop-timeout.
    say "stopping the container $name (systemd stops its units, within 60 s)"
    docker stop -t 60 "$name" >/dev/null
  fi
  say "removing the container $name (its volumes are kept)"
  docker rm "$name" >/dev/null
  exists=0
fi
if test "$exists" = 1; then
  current=$(docker container inspect --format '{{.Config.Image}}' "$name")
  if test "$current" != "$image"; then
    say "note: $name runs the image $current, not $image (the Dockerfile changed); run 'up.sh --recreate' to move to it, keeping every volume"
  fi
else
  # systemd's machine ID, from the name, so the journal in <name>-journal stays
  # this machine's across a re-creation.
  uuid=$(printf 'trend-investing mac mini %s' "$name" | shasum -a 256 | cut -c1-32)
  # Named volumes by --mount type=volume, which Docker never reads as a Mac
  # folder (a -v whose name had a slash in it would be a bind mount).
  # Not --privileged: systemd as PID 1 needs CAP_SYS_ADMIN (mount namespaces
  # for the units' ProtectSystem, PrivateTmp and RuntimeDirectory, and its
  # credentials) and a cgroup namespace of its own, whose cgroup tree the
  # image's entrypoint makes writable. Docker's default seccomp filter stays,
  # and no other capability is added (no clock, no module, no raw device).
  # /proc/sys read-only, the masked paths and the device rules are only
  # Docker's defaults: root inside holds CAP_SYS_ADMIN and can remount them,
  # changing the VM's kernel settings. The container is a rehearsal tool, not
  # a boundary (docs/manual/mac-mini.md, "What the container may do").
  docker create --name "$name" --hostname "$name" --platform linux/arm64 \
    --cap-add SYS_ADMIN --cgroupns private \
    --restart unless-stopped --stop-timeout 60 \
    --tmpfs /run --tmpfs /run/lock \
    --env container_uuid="$uuid" \
    --add-host host.docker.internal:host-gateway \
    --publish "127.0.0.1:$port:8088" \
    --mount "type=volume,source=$name-etc,target=/etc/trend-investing" \
    --mount "type=volume,source=$name-state,target=/var/lib/trend-investing" \
    --mount "type=volume,source=$name-opt,target=/opt/trend-investing" \
    --mount "type=volume,source=$name-journal,target=/var/log/journal" \
    --label org.trend-investing.mac-mini="$name" \
    --label org.trend-investing.checkout="$repo" \
    "$image" >/dev/null
  say "created the container $name"
fi

if test "$(docker container inspect --format '{{.State.Running}}' "$name")" != true; then
  docker start "$name" >/dev/null
fi
state=
for _ in $(seq 1 90); do
  state=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
  case $state in running | degraded) break ;; esac
  sleep 1
done
case $state in
  running) ;;
  degraded)
    # Accepted only when every failed unit is one that fails, naming why,
    # until the owner's first steps: the reporter and the dashboard until the
    # Boost engine has run once, the data job until its key is filled in. Any
    # other failed unit (mount-shared, journald, the clock check...) is a
    # machine that is not fit to provision.
    failed=$(docker exec "$name" systemctl list-units --state=failed --no-legend --plain | awk '{print $1}' | tr '\n' ' ')
    failed=${failed% }
    say "systemd in $name is degraded; failed: ${failed:-none listed}"
    bad=
    for u in $failed; do
      case $u in ti-reporter@*.service | ti-dashboard.service | ti-data.service) ;; *) bad="$bad $u" ;; esac
    done
    if test -n "$bad"; then
      refuse "refusing to provision $name: these units failed:$bad. See: docker exec $name journalctl -u ${bad# } -n 20"
    fi
    ;;
  *) refuse "systemd in $name did not finish starting (state '${state:-none}'); see: docker logs $name" ;;
esac
# What the units' credentials (LoadCredential) depend on, whatever the state:
# a credential mounted by systemd's helper reaches /run/credentials only when
# / has shared propagation, which mount-shared.service sets at boot.
docker exec "$name" systemctl is-active --quiet mount-shared.service \
  || refuse "mount-shared.service is not active in $name, so systemd cannot hand a unit its secrets; see: docker exec $name journalctl -u mount-shared.service"
propagation=$(docker exec "$name" findmnt -n -o PROPAGATION /)
test "$propagation" = shared \
  || refuse "/ in $name has '$propagation' mount propagation, not shared, so systemd cannot hand a unit its secrets; run up.sh --recreate onto the current image"

say "copying deploy/ and the built programs in"
docker exec "$name" rm -rf /root/deploy /root/dist
docker cp "$repo/deploy" "$name:/root/deploy"
if test -d "$dist"; then
  docker cp "$dist" "$name:/root/dist"
else
  say "no build folder at $dist: the programs are not installed (deploy/build-engine.sh and deploy/build-data.sh build them)"
fi

say "provisioning (TI_TARGET=mac-mini)"
docker exec --env TI_TARGET=mac-mini "$name" /root/deploy/provision.sh all

say "$name is up. Its units: docker exec $name systemctl list-units 'ti-*'"
