#!/bin/bash
# ti-release: put a tagged release of the server's five programs on the
# server, or put the previous build back. Pull-based: the owner runs it over
# Tailscale; nothing is pushed to the server and nothing deploys on a merge.
#
#   sudo ti-release v1.2.3     fetch release v1.2.3 from the public repository
#                              (no token), check its checksums, that each
#                              program is a Linux program for this machine,
#                              and its build attestation, keep the build now
#                              installed as the previous one, install the five
#                              programs, restart the dashboard, and say what
#                              changed
#   sudo ti-release --rollback put the previous build back the same way
#   sudo deploy/release.sh --install
#                              copy this script to /usr/local/sbin/ti-release
#   ti-release --unverified v1.2.3
#                              the rehearsal container only (it works only
#                              where TI_REHEARSAL=1, which the rehearsal sets;
#                              the server refuses it): install without
#                              checking the build attestation, with a warning
#
# The release is built by .github/workflows/release.yml from the tagged commit
# with deploy/build-engine.sh, stamped (ADR 0016), and carries checksums.txt
# and a build attestation. The programs are installed the way
# `provision.sh services` installs them: refused unless each is a Linux ELF
# program for this machine, then /opt/trend-investing/bin, mode 0755, with
# operator and journal-query linked into /usr/local/bin.
#
# Fail closed (AGENTS.md rule 5): without gh the release is refused, since the
# checksums come from the same release and catch a damaged download, not a
# tampered release. The attestation must be signed by this repository's
# release workflow on a GitHub-hosted runner, for the requested tag's ref and
# for the commit the tag names (read from the GitHub API), so an older
# release's programs and attestation cannot be served under a newer tag. That
# commit must also be on main, asked of the API (compare main...commit is
# identical or behind): the workflow's own on-main check runs from the tagged
# commit, which a moved tag controls. An answer that cannot be read is a
# refusal. The owner protects v* tags with a repository ruleset as well
# (docs/runbooks/lightsail-deployment.md).
#
# The engine is never restarted: a program replaced under a running process
# leaves that process on the build it started with, and a new engine build
# takes effect only at the next fresh run the owner approves (a run never
# resumes across builds, ADR 0027). The reporter is started afresh at each
# firing and needs no restart. Only the dashboard is restarted, and it must be
# active and stay active for three checks a second apart. While any engine
# unit is starting, stopping or reloading, or systemd cannot be asked, the
# script refuses, installing nothing; it asks again just before the swap.
#
# Two builds are kept under releases/, each with its checksums.txt: the
# current one and the previous one (the hand-installed build, on the first
# release). A kept build is checked against its checksums before it is put
# back. If any step of an install fails, or the dashboard does not stay up,
# the build that was current is put back (every program it has; none removed)
# and the script exits non-zero, recording nothing.
#
# Settings for tests and rehearsals (the server needs none): TI_RELEASE_URL,
# TI_RELEASE_API, TI_PREFIX, TI_LINK_DIR, TI_SYSTEMCTL, TI_GH,
# TI_RELEASE_NO_ROOT, TI_RELEASE_ANY_ARCH, TI_RELEASE_WAIT (seconds to wait
# for the dashboard), TI_RELEASE_SETTLE (seconds between the settle checks),
# TI_RELEASE_MAX_BYTES (the size cap of each download).
set -euo pipefail

repo_slug=richard-whittemore/TrendInvesting
base_url=${TI_RELEASE_URL:-https://github.com/$repo_slug/releases/download}
api_url=${TI_RELEASE_API:-https://api.github.com}
prefix=${TI_PREFIX:-/opt/trend-investing}
link_dir=${TI_LINK_DIR:-/usr/local/bin}
systemctl=${TI_SYSTEMCTL:-systemctl}
gh=${TI_GH:-gh}
wait_s=${TI_RELEASE_WAIT:-15}
settle_s=${TI_RELEASE_SETTLE:-1}
max_bytes=${TI_RELEASE_MAX_BYTES:-209715200}
max_time=300
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
unverified=0
if test "${1:-}" = --unverified; then
  test "${TI_REHEARSAL:-0}" = 1 || die "--unverified is for the rehearsal container only; this is not it, so nothing was installed"
  unverified=1
  shift
  test $# -eq 1 || usage
fi
case ${1:-} in
  --install) test "$unverified" = 0 && test $# -eq 1 || usage; mode=install ;;
  --rollback) test "$unverified" = 0 && test $# -eq 1 || usage; mode=rollback ;;
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

# refuse_if_engine_busy: refuses while an engine unit is mid-restart, since a
# release then could land between its stop and its start; a systemctl that
# fails counts as busy (fail closed).
refuse_if_engine_busy() {
  local busy
  if ! busy=$("$systemctl" list-units --plain --no-legend --state=activating,deactivating,reloading 'ti-engine@*' 2>/dev/null); then
    die "could not ask systemd whether an engine unit is mid-restart; nothing installed"
  fi
  if test -n "$busy"; then
    printf 'ti-release: an engine unit is mid-restart; nothing installed. Wait until it is active or stopped:\n%s\n' "$busy" >&2
    exit 1
  fi
}
refuse_if_engine_busy

install -d -m 0755 "$releases"
lock=$releases/.lock
mkdir "$lock" 2>/dev/null || die "another ti-release is running (or died: remove $lock)"
trap 'rmdir "$lock" 2>/dev/null || true; rm -rf "$releases/.staging" "$releases/.replaced"' EXIT

read_state() { cat "$releases/$1" 2>/dev/null || true; }
write_state() { printf '%s\n' "$2" >"$releases/$1.new" && mv "$releases/$1.new" "$releases/$1"; }

# listed DIR: the programs DIR/checksums.txt lists, in install order.
listed() {
  local p
  for p in $programs; do
    if awk -v f="$p" '$2 == f { found = 1 } END { exit !found }' "$1/checksums.txt"; then printf '%s\n' "$p"; fi
  done
}

# check_dir DIR: every program DIR/checksums.txt lists is present and
# matches, and all five are listed; only the hand-installed build (initial)
# may lack one, so a lost line cannot mix two builds.
check_dir() {
  local dir=$1 p want got n=0
  test -f "$dir/checksums.txt" || { echo "no checksums.txt in $dir" >&2; return 1; }
  for p in $programs; do
    want=$(awk -v f="$p" '$2 == f { print $1 }' "$dir/checksums.txt")
    if test -z "$want"; then
      test "$(basename "$dir")" != initial || continue
      echo "$p is not listed in checksums.txt" >&2
      return 1
    fi
    test -f "$dir/$p" || { echo "$p is missing" >&2; return 1; }
    got=$(sha "$dir/$p")
    test "$want" = "$got" || { echo "$p does not match its checksum" >&2; return 1; }
    n=$((n + 1))
  done
  test "$n" -gt 0 || { echo "checksums.txt in $dir lists no program" >&2; return 1; }
}

# elf_machine FILE: FILE's ELF e_machine (62 x86-64, 183 AArch64), the two
# little-endian bytes at offset 18; empty if FILE is not an ELF file.
elf_machine() {
  test "$(od -An -tx1 -N4 "$1" | tr -d ' \n')" = 7f454c46 || return 0
  od -An -tu2 -j18 -N2 "$1" | tr -d ' \n'
}

# require_native_programs DIR: every program in DIR is an ELF file for this
# machine's architecture: the check `provision.sh services` makes before it
# installs anything (its require_native_programs; elf_machine is the same
# function, kept identical by deploy/rehearsal/test-release.sh).
require_native_programs() {
  local dir=$1 want p got
  case $(uname -m) in
    x86_64) want=62 ;;
    aarch64 | arm64) want=183 ;;
    *) die "no Go build for $(uname -m); nothing installed" ;;
  esac
  for p in $programs; do
    got=$(elf_machine "$dir/$p")
    test "$got" = "$want" || die "$p in $tag is not a Linux program for this machine ($(uname -m); its ELF machine is ${got:-none, not ELF}); nothing installed"
  done
}

# fetch URL OUT: a download with a connect timeout, a time limit of five
# minutes per attempt (up to four attempts with the retries) and a size cap,
# so a stalled or oversized transfer cannot hold the lock for long.
fetch() {
  curl -fsSL --retry 3 --connect-timeout 20 --max-time "$max_time" --max-filesize "$max_bytes" -o "$2" "$1"
}

# tag_commit: the commit the tag names, from the GitHub API, following an
# annotated tag to its commit.
tag_commit() {
  local out=$releases/.staging/api.json kind sha i
  fetch "$api_url/repos/$repo_slug/git/ref/tags/$tag" "$out" || return 1
  for i in 1 2 3; do
    read -r kind sha < <(python3 -I -c 'import json, sys; o = json.load(open(sys.argv[1]))["object"]; print(o["type"], o["sha"])' "$out") || return 1
    [[ $sha =~ ^[0-9a-f]{40}$ ]] || return 1
    if test "$kind" = commit; then printf '%s\n' "$sha"; return 0; fi
    test "$kind" = tag || return 1
    fetch "$api_url/repos/$repo_slug/git/tags/$sha" "$out" || return 1
  done
  return 1
}

# commit_on_main COMMIT: the API says COMMIT is main's head or an ancestor of
# it (compare main...COMMIT is identical or behind); anything else, or an
# answer that cannot be read, is no.
commit_on_main() {
  local out=$releases/.staging/compare.json status
  fetch "$api_url/repos/$repo_slug/compare/main...$1" "$out" || return 1
  status=$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["status"])' "$out" 2>/dev/null) || return 1
  test "$status" = identical || test "$status" = behind
}

# install_from DIR: install every program DIR/checksums.txt lists. Each is
# written under a temporary name and renamed over the old one, so a running
# process keeps the file it started from and no half-written program is ever
# executed; a program the build lacks is left as it is. Every step is checked
# (this runs where set -e is off) and the installed files are compared with
# the checksums, so a failed copy or rename returns non-zero.
install_from() {
  local dir=$1 p
  for p in $(listed "$dir"); do
    install -m 0755 "$dir/$p" "$bin/.$p.new" || return 1
    mv -f "$bin/.$p.new" "$bin/$p" || { rm -f "$bin/.$p.new"; return 1; }
  done
  install -d -m 0755 "$link_dir" || return 1
  for p in operator journal-query; do
    if test -x "$bin/$p"; then ln -sfn "$bin/$p" "$link_dir/$p" || return 1; fi
  done
  for p in $(listed "$dir"); do
    test "$(sha "$bin/$p")" = "$(awk -v f="$p" '$2 == f { print $1 }' "$dir/checksums.txt")" || { echo "$bin/$p does not match the build just installed" >&2; return 1; }
  done
}

# restart_dashboard_verified: restart the dashboard, wait for it to be active,
# then require it to stay active for three checks a second apart (its unit is
# Type=simple, so systemd reports it active before a startup error ends it).
restart_dashboard_verified() {
  local i=0 _
  "$systemctl" restart ti-dashboard || return 1
  while ! "$systemctl" is-active --quiet ti-dashboard; do
    i=$((i + 1))
    test "$i" -lt "$wait_s" || return 1
    if test "$wait_s" -gt 3; then sleep 1; fi
  done
  for _ in 1 2 3; do
    sleep "$settle_s"
    "$systemctl" is-active --quiet ti-dashboard || return 1
  done
}

# put_back NAME: re-check the kept build NAME against its checksums and put it
# back; says so loudly if it cannot.
put_back() {
  if ! check_dir "$releases/$1"; then
    log "ALERT: the kept build $1 failed its checksums and was NOT put back; the programs in $bin may be a mix of builds: run sudo ti-release <a good tag>"
    return 1
  fi
  if ! install_from "$releases/$1"; then
    log "ALERT: putting $1 back failed partway; the programs in $bin may be a mix of builds: run sudo ti-release <a good tag>"
    return 1
  fi
  restart_dashboard_verified || log "the dashboard is still not active on $1: check journalctl -u ti-dashboard"
}

# snapshot_installed: keep the build now in bin, with its checksums, as
# releases/initial, for the first release on a server whose programs were
# installed by hand.
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
  if test "$unverified" = 0; then
    command -v "$gh" >/dev/null 2>&1 || die "gh is not installed, so the build attestation cannot be checked; nothing installed. Install gh as in docs/runbooks/lightsail-deployment.md"
  fi
  stage=$releases/.staging
  rm -rf "$stage"
  mkdir -p "$stage"
  log "fetching $tag"
  for f in $programs checksums.txt attestation.sigstore.json; do
    fetch "$base_url/$tag/$f" "$stage/$f" || die "could not fetch $base_url/$tag/$f (or it is over $max_bytes bytes); nothing installed"
  done
  check_dir "$stage" || die "$tag failed its checksums; nothing installed"
  require_native_programs "$stage"
  if test "$unverified" = 0; then
    commit=$(tag_commit) || die "could not read the commit $tag names from $api_url; nothing installed"
    commit_on_main "$commit" || die "$tag names commit $commit, which the API does not show to be on main (or its answer could not be read); nothing installed"
    for p in $programs; do
      "$gh" attestation verify "$stage/$p" --repo "$repo_slug" --bundle "$stage/attestation.sigstore.json" \
        --signer-workflow "$repo_slug/.github/workflows/release.yml" --source-ref "refs/tags/$tag" \
        --source-digest "$commit" --deny-self-hosted-runners >/dev/null 2>"$stage/gh.err" \
        || {
          # gh's message says why (an older gh, a login prompt, no route to the
          # trust root); a token-shaped string is masked before it is shown.
          head -c 2000 "$stage/gh.err" | sed -E 's/(gh[pousr]_|github_pat_)[A-Za-z0-9_]+/\1***/g; s/^/gh: /' >&2
          die "$p failed its build attestation for $tag (commit $commit); nothing installed"
        }
    done
    log "checksums and build attestation verified ($tag, commit $commit)"
  else
    log "WARNING: --unverified: the build attestation is NOT checked; the checksums come from the same release and catch a damaged download only. Never use this on the trading server."
    log "attestation not verified; checksums verified"
  fi

  if test -z "$cur"; then snapshot_installed; cur=initial; fi
  test -d "$releases/$cur" || die "the current build's folder $releases/$cur is missing; nothing installed"
  check_dir "$releases/$cur" || die "the kept copy of the current build ($cur) failed its checksums, so it could not be put back; nothing installed"

  refuse_if_engine_busy
  if ! install_from "$stage" || ! restart_dashboard_verified; then
    log "$tag did not install, or the dashboard did not stay up on it; putting $cur back"
    put_back "$cur" || true
    die "release $tag not installed; the previous build ($cur) was put back"
  fi
  # Keep the new build; a folder of the same tag (the previous build, when its
  # tag is released again) is replaced only now that the release succeeded.
  # If keeping it fails, the old build is put back and nothing is recorded.
  rm -rf "$releases/.replaced"
  kept=1
  if test -d "$releases/$tag" && ! mv "$releases/$tag" "$releases/.replaced"; then kept=0; fi
  if test "$kept" = 1 && ! mv "$stage" "$releases/$tag"; then
    kept=0
    if test -d "$releases/.replaced"; then mv "$releases/.replaced" "$releases/$tag" || true; fi
  fi
  if test "$kept" = 0; then
    log "$tag was installed but could not be kept under $releases; putting $cur back"
    put_back "$cur" || true
    die "release $tag not recorded; the previous build ($cur) was put back"
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
test -n "$cur" && test -d "$releases/$cur" || die "the current build's folder is missing, so it could not be put back; nothing changed"
check_dir "$releases/$cur" || die "the kept copy of the current build ($cur) failed its checksums, so it could not be put back; nothing changed"
refuse_if_engine_busy
if ! install_from "$releases/$prev" || ! restart_dashboard_verified; then
  log "rolling back to $prev failed, or the dashboard did not stay up on it; putting $cur back"
  put_back "$cur" || true
  die "not rolled back: $prev did not install or the dashboard did not come back (check journalctl -u ti-dashboard); the current build ($cur) was put back"
fi
write_state previous "$cur"
write_state current "$prev"
report "$cur" "$prev" "$before"
