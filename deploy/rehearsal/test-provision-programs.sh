#!/bin/bash
# Services install the staged build without interrupting an engine's run
# (docs/manual/server.md, Installing a build). All writes stay in a copied
# deployment tree, with stand-ins for installation and systemd.
# shellcheck disable=SC2034,SC2329
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(cd "$(mktemp -d)" && pwd -P)
trap 'rm -rf "$work"' EXIT
unset TI_DIST TI_DATA_BUILD TI_TARGET TI_REHEARSAL
mkdir -p "$work/source"
cp -R "$here/.." "$work/source/deploy"
# Redirect literal host paths too, including the final binary and its backup.
python3 - "$work" <<'PY'
import pathlib, re, sys
work = pathlib.Path(sys.argv[1])
p = work / 'source/deploy/provision.sh'
s = p.read_text()
s = re.sub(r'(?<![a-zA-Z0-9_}])/(opt|var|usr|run|etc)/',
           lambda m: str(work / 'root') + m.group(), s)
p.write_text(s)
# Read the actual output names from the build entry points.
build = (work / 'source/deploy/build-engine.sh').read_text()
programs = re.findall(r'mv bin/([^ ]+) "\$out/[^\"]+"', build)
tools = re.search(r'for tool in (.*); do', build).group(1).split()
programs += [tool.split(':')[1] for tool in tools]
assert len(programs) == 8, programs
(work / 'programs').write_text('\n'.join(programs) + '\n')
PY
mkdir -p "$work/dist" "$work/override" "$work/root/opt/trend-investing/bin" "$work/root/etc/systemd/system"
bin=$work/root/opt/trend-investing/bin
while IFS= read -r program; do
  printf '\177ELF\002\001\001' >"$work/dist/$program"
  head -c 11 /dev/zero >>"$work/dist/$program"
  printf '\076\000new-%s' "$program" >>"$work/dist/$program"
done <"$work/programs"
printf 'staged data\n' >"$work/dist/ti-data"
printf 'override data\n' >"$work/override/ti-data"
mkdir -p "$work/source/dist"
printf 'fallback data\n' >"$work/source/dist/ti-data"
printf 'new-build\n' >"$work/dist/BUILD"

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
scenario() (
  # shellcheck source=/dev/null
  . "$work/source/deploy/provision.sh"
  etc=$work/root/etc/trend-investing
  unit_dir=$work/root/etc/systemd/system
  rehearsal=0
  uname() { echo x86_64; }
  secrets_migrate() { :; }
  init_gateway_login() { :; }
  set_gateway_heap() { :; }
  ensure_operator_user() { :; }
  chown() { record chown "$@"; }
  record() { printf '%s\n' "$*" >>"$work/calls"; }
  guard() {
    local path
    for path in "$@"; do
      case $path in
        -*) ;;
        "$work"/*) ;;
        *) echo "unsafe test write: $path" >&2; exit 97 ;;
      esac
    done
  }
  install() {
    record install "$@"
    local dest=${*: -1} src=${*: -2:1}
    guard "$dest"
    if test "${1:-}" = -d; then
      # Only the final directory needs materializing for this harness.
      command mkdir -p "$dest"
      return
    fi
    command mkdir -p "$(dirname "$dest")"
    if test -d "$dest"; then dest=$dest/$(basename "$src"); fi
    if test "${FAIL_ENGINE_INSTALL:-0}" = 1 && test "$src" = "$work/dist/ti-engine"; then return 1; fi
    command cp "$src" "$dest"
    command chmod "${2}" "$dest"
  }
  mv() { record mv "$@"; guard "$@"; command mv "$@"; }
  cp() { record cp "$@"; guard "$@"; command cp "$@"; }
  rm() { guard "$@"; command rm "$@"; }
  ln() { record ln "$@"; }
  runuser() { record runuser "$@"; }
  systemctl() { record systemctl "$@"; }
  : >"$work/calls"
  step_services
)
run_services() {
  if scenario >"$work/out" 2>&1; then return 0; fi
  cat "$work/out" >&2
  fail 'services failed'
}
check_units() {
  if grep -E '^systemctl (start|stop|restart|try-restart|reload-or-restart|reload-or-try-restart|enable.*--now|disable.*--now).*ti-engine@' "$work/calls"; then
    fail 'services changed an engine unit running state'
  fi
  grep -q '^systemctl restart ti-dashboard.service$' "$work/calls" || fail 'live systemctl branch was not exercised'
}

export TI_DIST=$work/dist
printf 'old engine\n' >"$bin/ti-engine"
printf 'old-build\n' >"$bin/BUILD"
# A hard link models the old inode held open by a running engine.
ln "$bin/ti-engine" "$work/open-engine"
run_services
while IFS= read -r program; do
  cmp -s "$work/dist/$program" "$bin/$program" || fail "$program was not installed from TI_DIST"
done <"$work/programs"
cmp -s "$work/dist/ti-data" "$bin/ti-data" || fail 'ti-data was not installed from TI_DIST'
cmp -s "$work/dist/BUILD" "$bin/BUILD" || fail 'bin/BUILD was not updated'
printf 'old engine\n' >"$work/expected-old"
cmp -s "$work/expected-old" "$bin/ti-engine.old-build.bak" || fail 'stamped engine backup missing'
cmp -s "$work/expected-old" "$work/open-engine" || fail 'old engine inode was overwritten'
if grep -Fxq "install -m 0755 $work/dist/ti-engine $bin/ti-engine" "$work/calls"; then fail 'engine installed directly over running binary'; fi
grep -E "^mv .*${bin}/.* ${bin}/ti-engine$" "$work/calls" >/dev/null || fail 'engine was not renamed into place'
check_units

rm -f "$bin/BUILD"
export TI_DATA_BUILD=$work/override/ti-data
run_services
cmp -s "$work/dist/ti-engine" "$bin/ti-engine.bak" || fail 'unstamped engine backup missing'
cmp -s "$TI_DATA_BUILD" "$bin/ti-data" || fail 'explicit TI_DATA_BUILD was ignored'
check_units
unset TI_DATA_BUILD TI_DIST
# The copied provisioner defaults to its sibling dist directory.
run_services
cmp -s "$work/source/dist/ti-data" "$bin/ti-data" || fail 'unset TI_DIST lost the old data fallback'
check_units

export TI_DIST=$work/dist
printf 'keep engine\n' >"$bin/ti-engine"
printf 'keep-build\n' >"$bin/BUILD"
export FAIL_ENGINE_INSTALL=1
if scenario >"$work/out" 2>&1; then fail 'failed engine install succeeded'; fi
grep -qx 'keep engine' "$bin/ti-engine" || fail 'failed install replaced engine'
grep -qx 'keep-build' "$bin/BUILD" || fail 'failed install changed BUILD'
unset FAIL_ENGINE_INSTALL

if test "$failures" -gt 0; then echo "$failures provisioning program test(s) failed" >&2; exit 1; fi
echo 'provision programs tests passed'
