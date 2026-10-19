#!/bin/bash
# Creates the Mac mini's container, or re-attaches to it, and provisions it
# (Mac mini plan, option A, approved by the owner on 2026-10-18; section 5,
# item 4). Run it on the Mac, from the checkout kept for builds and installs
# (~/TrendInvesting-deploy), never from one on the Desktop:
#
#   deploy/mac-mini/up.sh              create the container, or start it and re-provision
#   deploy/mac-mini/up.sh --recreate   remove the container (never its volumes) and create it again
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
# Desktop, Documents, iCloud Drive or another synced folder, and one holding an
# iCloud sync duplicate ("name 2.ext" or "name 2"), which would otherwise be
# copied into the programs (plan section 7, "iCloud sync duplicates files").
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

# refuse_duplicates WHAT PATH: any "name 2.ext" or "name 2", file or folder.
refuse_duplicates() {
  local found
  found=$(find "$2" \( -name '* 2.*' -o -name '* 2' \) -print 2>/dev/null | head -n 5)
  if test -n "$found"; then
    refuse "refusing the $1 at $2: it holds what look like iCloud sync duplicates, which would be copied into the container: $(printf '%s' "$found" | tr '\n' ';') Delete them, or take a fresh checkout outside the synced folders."
  fi
}

case $name in
  '' | *[!a-z0-9-]* | -*) refuse "TI_MAC_NAME must be lower-case letters, digits and hyphens (it names Docker volumes, never a folder), not '$name'" ;;
esac
case $port in
  '' | *[!0-9]*) refuse "TI_MAC_DASHBOARD_PORT must be a port number, not '$port'" ;;
esac
refuse_synced checkout "$repo"
refuse_synced "build folder" "$dist"
refuse_duplicates checkout "$repo"
if test -d "$dist"; then refuse_duplicates "build folder" "$dist"; fi

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

exists=0
docker container inspect "$name" >/dev/null 2>&1 && exists=1
if test "$exists" = 1 && test "$recreate" = 1; then
  say "removing the container $name (its volumes are kept)"
  docker rm --force "$name" >/dev/null
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
  # --privileged: systemd as PID 1 exits at once without it under Docker
  # Desktop, and the units' hardening (ProtectSystem=strict, PrivateTmp,
  # RuntimeDirectory) needs mount namespaces. The privilege is inside
  # Docker's own Linux VM, not the Mac (docs/manual/mac-mini.md).
  docker create --name "$name" --hostname "$name" --platform linux/arm64 \
    --privileged --restart unless-stopped --stop-timeout 60 \
    --tmpfs /run --tmpfs /run/lock \
    --env container_uuid="$uuid" \
    --add-host host.docker.internal:host-gateway \
    --publish "127.0.0.1:$port:8088" \
    --mount "type=volume,source=$name-etc,target=/etc/trend-investing" \
    --mount "type=volume,source=$name-state,target=/var/lib/trend-investing" \
    --mount "type=volume,source=$name-opt,target=/opt/trend-investing" \
    --mount "type=volume,source=$name-journal,target=/var/log/journal" \
    --label org.trend-investing.mac-mini="$name" \
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
  running | degraded) ;;
  *) refuse "systemd in $name did not finish starting (state '${state:-none}'); see: docker logs $name" ;;
esac

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
