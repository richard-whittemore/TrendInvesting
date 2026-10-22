#!/bin/bash
# ti-release: put a tagged release of the server's five programs on the
# server, or put the previous build back. Pull-based: the owner runs it over
# Tailscale; nothing is pushed to the server and nothing deploys on a merge.
#
#   sudo ti-release v1.2.3     fetch release v1.2.3 from the public repository
#                              (no token), check its checksums and its build
#                              attestation, keep the build now installed as the
#                              previous one, install the five programs, restart
#                              the dashboard, and say what changed
#   sudo ti-release --rollback put the previous build back the same way
#   sudo deploy/release.sh --install
#                              copy this script to /usr/local/sbin/ti-release
#
# The release is built by .github/workflows/release.yml from the tagged commit
# with deploy/build-engine.sh, stamped (ADR 0016), and carries checksums.txt
# and a build attestation. The programs are installed the way
# `provision.sh services` installs them: /opt/trend-investing/bin, mode 0755,
# with operator and journal-query linked into /usr/local/bin.
#
# The engine is never restarted: a program replaced under a running process
# leaves that process on the build it started with, and a new engine build
# takes effect only at the next fresh run the owner approves (a run never
# resumes across builds, ADR 0027). The reporter is started afresh at each
# firing and needs no restart. Only the dashboard is restarted. While any
# engine unit is starting, stopping or reloading the script refuses, installing
# nothing.
#
# Two builds are kept under releases/: the current one and the previous one
# (the hand-installed build, on the first release). If the dashboard does not
# come back after an install, the previous build is put back before the script
# exits non-zero. The attestation is checked with `gh attestation verify` when
# gh is installed; without it only the checksums are checked, and those come
# from the same release, so they catch a damaged download, not a tampered
# release. Install gh on the server to close that gap.
#
# Settings for tests and rehearsals (the server needs none): TI_RELEASE_URL,
# TI_PREFIX, TI_LINK_DIR, TI_SYSTEMCTL, TI_GH, TI_RELEASE_NO_ROOT,
# TI_RELEASE_ANY_ARCH, TI_RELEASE_WAIT (seconds to wait for the dashboard).
set -euo pipefail

repo_slug=richard-whittemore/TrendInvesting
base_url=${TI_RELEASE_URL:-https://github.com/$repo_slug/releases/download}
prefix=${TI_PREFIX:-/opt/trend-investing}
link_dir=${TI_LINK_DIR:-/usr/local/bin}
systemctl=${TI_SYSTEMCTL:-systemctl}
gh=${TI_GH:-gh}
wait_s=${TI_RELEASE_WAIT:-15}
bin=$prefix/bin
releases=$prefix/releases
programs="ti-engine ti-reporter ti-dashboard operator journal-query"

die() { printf 'ti-release: %s\n' "$*" >&2; exit 1; }
log() { printf 'ti-release: %s\n' "$*"; }
usage() { echo "usage: ti-release <tag> | --rollback | --install" >&2; exit 1; }

sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
mode=
tag=
case ${1:-} in
  --install) test $# -eq 1 || usage; mode=install ;;
  --rollback) test $# -eq 1 || usage; mode=rollback ;;
  "" | -*) usage ;;
  *)
    test $# -eq 1 || usage
    mode=release
    tag=$1
    # A tag names a folder and a URL path: only a version tag is accepted.
    if ! [[ $tag =~ ^v[0-9][A-Za-z0-9._-]*$ ]] || [[ $tag == *..* ]]; then
      die "'$tag' is not a release tag (a version such as v1.2.3)"
    fi
    ;;
esac

if test "${TI_RELEASE_NO_ROOT:-0}" != 1 && test "$(id -u)" -ne 0; then
  die "run as root (sudo ti-release ...)"
fi

if test "$mode" = install; then
  install -d -m 0755 /usr/local/sbin
  install -m 0755 "$here/release.sh" /usr/local/sbin/ti-release
  log "installed /usr/local/sbin/ti-release"
  exit 0
fi

if test "${TI_RELEASE_ANY_ARCH:-0}" != 1 && test "$(uname -s)-$(uname -m)" != Linux-x86_64; then
  die "this server is $(uname -s)-$(uname -m); the release programs are linux/amd64"
fi

# Refuse while an engine unit is mid-restart: a release then could land between
# its stop and its start.
busy=$("$systemctl" list-units --plain --no-legend --state=activating,deactivating,reloading 'ti-engine@*' 2>/dev/null || true)
if test -n "$busy"; then
  printf 'ti-release: an engine unit is mid-restart; nothing installed. Wait until it is active or stopped:\n%s\n' "$busy" >&2
  exit 1
fi

install -d -m 0755 "$releases"
lock=$releases/.lock
mkdir "$lock" 2>/dev/null || die "another ti-release is running (or died: remove $lock)"
trap 'rmdir "$lock" 2>/dev/null || true; rm -rf "$releases/.staging"' EXIT

read_state() { cat "$releases/$1" 2>/dev/null || true; }
write_state() { printf '%s\n' "$2" >"$releases/$1.new" && mv "$releases/$1.new" "$releases/$1"; }

# check_dir DIR: every program is listed once in DIR/checksums.txt and matches.
check_dir() {
  local dir=$1 p want got
  test -f "$dir/checksums.txt" || { echo "no checksums.txt in $dir" >&2; return 1; }
  for p in $programs; do
    want=$(awk -v f="$p" '$2 == f { print $1 }' "$dir/checksums.txt")
    test -n "$want" || { echo "$p is not listed in checksums.txt" >&2; return 1; }
    test -f "$dir/$p" || { echo "$p is missing" >&2; return 1; }
    got=$(sha "$dir/$p")
    test "$want" = "$got" || { echo "$p does not match its checksum" >&2; return 1; }
  done
}

# install_from DIR: replace each program present in DIR. Each is written under
# a temporary name and renamed over the old one, so a running process keeps the
# file it started from and no half-written program is ever executed.
install_from() {
  local dir=$1 p
  for p in $programs; do
    test -f "$dir/$p" || continue
    install -m 0755 "$dir/$p" "$bin/.$p.new"
    mv -f "$bin/.$p.new" "$bin/$p"
  done
  install -d -m 0755 "$link_dir"
  for p in operator journal-query; do
    if test -x "$bin/$p"; then ln -sfn "$bin/$p" "$link_dir/$p"; fi
  done
}

# restart_dashboard_verified: restart the dashboard and wait for it to be active.
restart_dashboard_verified() {
  local i=0
  "$systemctl" restart ti-dashboard || return 1
  while test "$i" -lt "$wait_s"; do
    if "$systemctl" is-active --quiet ti-dashboard; then return 0; fi
    i=$((i + 1))
    if test "$wait_s" -gt 3; then sleep 1; fi
  done
  return 1
}

# snapshot_installed: keep the build now in bin as releases/initial, for the
# first release on a server whose programs were installed by hand.
snapshot_installed() {
  local d=$releases/initial p
  rm -rf "$d"
  mkdir -p "$d"
  : >"$d/checksums.txt"
  for p in $programs; do
    test -f "$bin/$p" || continue
    cp "$bin/$p" "$d/$p"
    printf '%s  %s\n' "$(sha "$d/$p")" "$p" >>"$d/checksums.txt"
  done
  write_state current initial
}

prune() { # keep the two named builds, remove the rest
  local d name
  for d in "$releases"/*/; do
    name=$(basename "$d")
    if test "$name" != "$1" && test "$name" != "$2"; then rm -rf "$d"; fi
  done
}

hashes() { local p; for p in $programs; do printf '%s %s\n' "$p" "$(test -f "$bin/$p" && sha "$bin/$p" || echo none)"; done; }

report() { # FROM TO BEFORE_HASHES
  local p was now
  log "$1 -> $2"
  for p in $programs; do
    was=$(printf '%s\n' "$3" | awk -v f="$p" '$1 == f { print $2 }')
    now=$(test -f "$bin/$p" && sha "$bin/$p" || echo none)
    if test "$was" = "$now"; then log "  $p: unchanged"; else log "  $p: changed (${was:0:12} -> ${now:0:12})"; fi
  done
  log "the dashboard was restarted and is active; the reporter runs the new build at its next firing"
  log "the engine was not restarted: a running engine keeps its build, and a new engine build takes effect at the next fresh run you approve"
}

before=$(hashes)

if test "$mode" = release; then
  cur=$(read_state current)
  if test "$cur" = "$tag"; then
    log "$tag is already installed; nothing to do"
    exit 0
  fi
  command -v curl >/dev/null 2>&1 || die "curl is not installed"
  stage=$releases/.staging
  rm -rf "$stage"
  mkdir -p "$stage"
  log "fetching $tag"
  for f in $programs checksums.txt attestation.sigstore.json; do
    curl -fsSL --retry 3 -o "$stage/$f" "$base_url/$tag/$f" || die "could not fetch $base_url/$tag/$f; nothing installed"
  done
  check_dir "$stage" || die "$tag failed its checksums; nothing installed"
  if command -v "$gh" >/dev/null 2>&1; then
    for p in $programs; do
      "$gh" attestation verify "$stage/$p" --repo "$repo_slug" --bundle "$stage/attestation.sigstore.json" \
        --signer-workflow "$repo_slug/.github/workflows/release.yml" >/dev/null 2>&1 \
        || die "$p failed its build attestation; nothing installed"
    done
    log "checksums and build attestation verified"
  else
    log "WARNING: gh is not installed, so the build attestation is not verified; the checksums come from the same release and catch a damaged download only (install gh to verify)"
    log "attestation not verified; checksums verified"
  fi

  if test -z "$cur"; then snapshot_installed; cur=initial; fi
  test -d "$releases/$cur" || die "the current build's folder $releases/$cur is missing; nothing installed"
  rm -rf "${releases:?}/$tag"
  mv "$stage" "$releases/$tag"

  if ! install_from "$releases/$tag" || ! restart_dashboard_verified; then
    log "the dashboard did not come back on $tag; putting $cur back"
    install_from "$releases/$cur"
    restart_dashboard_verified || log "the dashboard is still not active on $cur: check journalctl -u ti-dashboard"
    rm -rf "${releases:?}/$tag"
    die "release $tag not installed: the dashboard did not come back; the previous build ($cur) was put back"
  fi
  write_state previous "$cur"
  write_state current "$tag"
  prune "$tag" "$cur"
  report "$cur" "$tag" "$before"
  exit 0
fi

# rollback
cur=$(read_state current)
prev=$(read_state previous)
test -n "$prev" && test -d "$releases/$prev" || die "no previous build to roll back to"
check_dir "$releases/$prev" || die "the previous build ($prev) is damaged; nothing changed"
install_from "$releases/$prev"
if ! restart_dashboard_verified; then
  die "rolled back to $prev but the dashboard did not come back: check journalctl -u ti-dashboard; the current build ($cur) is still kept in $releases/$cur"
fi
write_state previous "$cur"
write_state current "$prev"
report "$cur" "$prev" "$before"
