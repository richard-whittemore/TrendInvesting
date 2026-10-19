#!/bin/bash
# Tests for deploy/mac-mini/up.sh's refusals, which come before it touches
# Docker: a checkout under the Desktop, Documents or iCloud Drive (or another
# synced folder), one holding an iCloud sync duplicate ("name 2.ext" or
# "name 2"), a build folder in either state, and a container name that Docker
# would read as a path. The plan's risk table (Mac mini plan, section 7, "iCloud
# sync duplicates files or stalls") is why: a duplicate would be copied into
# the container's programs, and a stalled sync can hang the copy.
#
# Each case runs a copy of up.sh from a temporary checkout under a fake home,
# with a stand-in `docker` that records every call; a refusal must exit
# non-zero, say why, and leave the record empty (no image, volume or
# container was created or looked at). No Docker needed.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset TI_MAC_NAME TI_DIST TI_MAC_DASHBOARD_PORT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

fake_home=$work/home
mkdir -p "$fake_home" "$work/bin"
# The stand-in docker: records the call; `docker info` names an x86_64 VM,
# which up.sh refuses, so a run that passes every refusal stops there.
cat >"$work/bin/docker" <<'STUB'
#!/bin/bash
echo "docker $*" >>"$DOCKER_CALLS"
if test "${1:-}" = info; then echo x86_64; exit 0; fi
exit 1
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

# The positive control: a clean checkout in the home folder passes every
# refusal and reaches Docker, which the stand-in reports as x86_64.
if launch "$clean" TI_DIST="$work/dist"; then fail "the x86_64 Docker VM was accepted"; fi
grep -q '^docker info' "$work/calls" || fail "a clean checkout never reached Docker: $(cat "$work/out")"
grep -qi 'arm64' "$work/out" || fail "the x86_64 refusal does not say arm64: $(cat "$work/out")"
if grep -Eq '^docker (create|run|volume create|build)' "$work/calls"; then fail "something was created on an x86_64 VM: $(cat "$work/calls")"; fi

test "$failures" -eq 0 || exit 1
echo "mac-mini refusal tests ok"
