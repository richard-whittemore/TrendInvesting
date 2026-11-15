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
export work
cat >"$work/scenario.sh" <<'SCENARIO'
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
    if test "${FAIL_BUILD_INSTALL:-0}" = 1 && test "$dest" = "$work/root/opt/trend-investing/bin/.BUILD.new"; then return 1; fi
    command cp "$src" "$dest"
    command chmod "${2}" "$dest"
  }
  date() { echo 20261115T120000Z; }
  mv() { record mv "$@"; guard "$@"; command mv "$@"; }
  cp() {
    record cp "$@"; guard "$@"
    if test "${FAIL_BACKUP_COPY:-0}" = 1 && test "${*: -2:1}" = "$bin/ti-engine"; then
      printf partial >"${*: -1}"
      return 1
    fi
    command cp "$@"
  }
  rm() { guard "$@"; command rm "$@"; }
  ln() { record ln "$@"; }
  runuser() { record runuser "$@"; }
  systemctl() {
    record systemctl "$@"
    local arg verb= engine=0 now=0
    for arg in "$@"; do
      case $arg in
        ti-engine*) engine=1 ;;
        --now) now=1 ;;
        start|stop|restart|try-restart|reload|reload-or-restart|try-reload-or-restart|kill|isolate|enable|disable) verb=$arg ;;
      esac
    done
    if test "$engine" = 1; then
      case $verb in
        enable|disable) test "$now" != 1 || { record forbidden-engine-state-change; return 97; } ;;
        '') ;;
        *) record forbidden-engine-state-change; return 97 ;;
      esac
    fi
  }
  : >"$work/calls"
  step_services
SCENARIO
run_scenario() {
  set +e
  bash -e "$work/scenario.sh" >"$work/out" 2>&1
  status=$?
  set -e
  if grep -q '^forbidden-engine-state-change$' "$work/calls"; then
    fail 'services changed an engine unit running state'
  fi
}
run_services() {
  run_scenario
  if test "$status" != 0; then
    cat "$work/out" >&2
    fail 'services failed'
  fi
}
check_units() {
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
chmod 0600 "$bin/ti-engine"
ln "$bin/ti-engine" "$work/identical-engine"
export TI_DATA_BUILD=$work/override/ti-data
run_services
test ! -e "$bin/ti-engine.bak" || fail 'identical engine was backed up'
test "$bin/ti-engine" -ef "$work/identical-engine" || fail 'identical engine inode replaced'
test -x "$bin/ti-engine" || fail 'identical engine executable mode not restored'
grep -Fxq "chown root:root $bin/ti-engine" "$work/calls" || fail 'identical engine ownership not restored'
grep -q "ti-engine already at new-build" "$work/out" || fail 'identical engine was not logged'
if grep -E "(install|cp|mv).*($bin/\.ti-engine.new|$bin/ti-engine$)" "$work/calls"; then fail 'identical engine was staged or renamed'; fi
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
run_scenario
test "$status" != 0 || fail 'failed engine install succeeded'
grep -qx 'keep engine' "$bin/ti-engine" || fail 'failed install replaced engine'
grep -qx 'keep-build' "$bin/BUILD" || fail 'failed install changed BUILD'
unset FAIL_ENGINE_INSTALL

# Both staged files precede any backup or engine replacement (server.md).
export FAIL_BUILD_INSTALL=1
run_scenario
test "$status" != 0 || fail 'failed BUILD staging succeeded'
grep -qx 'keep engine' "$bin/ti-engine" || fail 'BUILD staging failure replaced engine'
grep -qx 'keep-build' "$bin/BUILD" || fail 'BUILD staging failure changed BUILD'
if grep -q '^cp ' "$work/calls"; then fail 'backup preceded successful BUILD staging'; fi
unset FAIL_BUILD_INSTALL

for identity in missing '' 'bad/build' "$(printf '%065d' 0)"; do
  printf '%s\n' "$identity" >"$work/dist/BUILD"
  if test "$identity" = missing; then rm "$work/dist/BUILD"; fi
  run_scenario
  test "$status" != 0 || fail "accepted incoming BUILD: $identity"
  grep -Fq "ti-engine in $work/dist has no valid BUILD; deploy/build-engine.sh writes it" "$work/out" || fail 'missing identity diagnostic'
  grep -qx 'keep engine' "$bin/ti-engine" || fail 'invalid identity replaced engine'
  grep -qx 'keep-build' "$bin/BUILD" || fail 'invalid identity changed BUILD'
  if grep -q '^install ' "$work/calls"; then fail 'invalid identity installed files'; fi
done
printf 'new-build\n' >"$work/dist/BUILD"
printf '%065d\n' 0 >"$bin/BUILD"
run_scenario
test "$status" != 0 || fail 'accepted overlong installed BUILD'
grep -qx 'keep engine' "$bin/ti-engine" || fail 'invalid installed identity replaced engine'
printf 'keep-build\n' >"$bin/BUILD"

for target in "$work/dist/BUILD" "$bin/BUILD"; do
  for malformed in nul trailing-nul extra-newline embedded-newline non-ascii; do
    printf 'keep engine\n' >"$bin/ti-engine"
    printf 'new-build\n' >"$work/dist/BUILD"
    printf 'keep-build\n' >"$bin/BUILD"
    case $malformed in
      nul) printf 'valid\000suffix\n' >"$target" ;;
      trailing-nul) printf 'valid\000' >"$target" ;;
      extra-newline) printf 'valid\n\n' >"$target" ;;
      embedded-newline) printf 'valid\nother' >"$target" ;;
      non-ascii) printf 'valid\377\n' >"$target" ;;
    esac
    run_scenario
    test "$status" != 0 || fail "accepted $malformed in $target"
    grep -qx 'keep engine' "$bin/ti-engine" || fail 'malformed BUILD replaced engine'
    if grep -q '^install ' "$work/calls"; then fail 'malformed BUILD installed files'; fi
  done
done
printf 'keep engine\n' >"$bin/ti-engine"
printf 'new-build' >"$work/dist/BUILD"
printf 'keep-build\n' >"$bin/BUILD"
export FAIL_BACKUP_COPY=1
run_scenario
test "$status" != 0 || fail 'failed backup copy succeeded'
test ! -e "$bin/ti-engine.keep-build.bak" || fail 'failed copy published partial backup'
grep -qx 'keep engine' "$bin/ti-engine" || fail 'failed backup replaced engine'
unset FAIL_BACKUP_COPY
rm -f "$bin/ti-engine.keep-build.bak"

printf 'earlier backup\n' >"$bin/ti-engine.keep-build.bak"
run_services
grep -qx 'earlier backup' "$bin/ti-engine.keep-build.bak" || fail 'differing backup overwritten'
grep -qx 'keep engine' "$bin/ti-engine.keep-build.20261115T120000Z.bak" || fail 'timestamped backup missing'
grep -Fq 'installed ti-engine new-build over keep-build (backup ti-engine.keep-build.20261115T120000Z.bak)' "$work/out" || fail 'installation summary missing'
python3 - "$work/calls" "$bin" <<'PYCODE' || fail 'engine staging and replacement order differs'
from pathlib import Path
import sys
calls = Path(sys.argv[1]).read_text().splitlines()
b = sys.argv[2]
engine_stage = next(i for i, c in enumerate(calls) if c.startswith('install ') and c.endswith(b + '/.ti-engine.new'))
build_stage = next(i for i, c in enumerate(calls) if c.startswith('install ') and c.endswith(b + '/.BUILD.new'))
backup = next(i for i, c in enumerate(calls) if c.startswith('cp '))
engine_move = calls.index(f'mv -f {b}/.ti-engine.new {b}/ti-engine')
build_move = calls.index(f'mv -f {b}/.BUILD.new {b}/BUILD')
backup_move = calls.index(f'mv -f {b}/.ti-engine.bak.new {b}/ti-engine.keep-build.20261115T120000Z.bak')
assert calls[backup].endswith(b + '/.ti-engine.bak.new')
assert engine_stage < build_stage < backup < backup_move < engine_move < build_move
assert Path(b + '/BUILD').read_bytes() == b'new-build\n'
PYCODE

# A retry in the same second preserves an existing timestamped backup too.
printf 'another engine\n' >"$bin/ti-engine"
printf 'keep-build\n' >"$bin/BUILD"
run_scenario
test "$status" != 0 || fail 'timestamp collision succeeded'
grep -qx 'keep engine' "$bin/ti-engine.keep-build.20261115T120000Z.bak" || fail 'timestamped backup overwritten'
grep -qx 'another engine' "$bin/ti-engine" || fail 'timestamp collision replaced engine'

rm "$bin/ti-engine" "$bin/BUILD"
run_services
grep -Fq 'installed ti-engine new-build over none (backup none)' "$work/out" || fail 'fresh installation summary missing'

# Exercise the build entry point with local stand-ins for Go compilation.
mkdir -p "$work/build-repo/deploy" "$work/build-repo/scripts" "$work/build-tools"
cp "$here/../build-engine.sh" "$work/build-repo/deploy/"
printf '#!/bin/bash\necho test-clean-tree\n' >"$work/build-repo/scripts/build-stamp.sh"
printf '#!/bin/bash\nmkdir -p bin\necho engine >bin/ti-engine\n' >"$work/build-tools/make"
cat >"$work/build-tools/go" <<'GO'
#!/bin/bash
while test "$1" != -o; do shift; done
test ! -e "$BUILD_OUTPUT/BUILD" || { echo 'BUILD visible during compilation' >>"$BUILD_OUTPUT/observed"; }
if test "${FAIL_GO_BUILD:-0}" = 1; then exit 1; fi
echo tool >"$2"
GO
chmod +x "$work/build-repo/scripts/build-stamp.sh" "$work/build-tools/"*
git -C "$work/build-repo" init -q
git -C "$work/build-repo" add .
git -C "$work/build-repo" -c user.name=Test -c user.email=test@example.com commit -qm fixture
export BUILD_OUTPUT=$work/build-output
set +e
PATH="$work/build-tools:$PATH" bash -e "$work/build-repo/deploy/build-engine.sh" --out "$work/build-output" >"$work/build-out" 2>&1
status=$?
set -e
test "$status" = 0 || fail 'build of a clean tree failed'
printf 'test-clean-tree\n' >"$work/expected-build"
cmp -s "$work/expected-build" "$work/build-output/BUILD" || fail 'build did not write binary stamp to selected output BUILD'

for failing_tool in make go; do
  printf 'stale-build\n' >"$BUILD_OUTPUT/BUILD"
  if test "$failing_tool" = make; then
    cat >"$work/build-tools/make" <<'MAKE'
#!/bin/bash
test ! -e "$BUILD_OUTPUT/BUILD" || echo visible >"$BUILD_OUTPUT/observed"
exit 1
MAKE
  else
    printf '#!/bin/bash\nmkdir -p bin\necho engine >bin/ti-engine\n' >"$work/build-tools/make"
  fi
  set +e
  FAIL_GO_BUILD=1 PATH="$work/build-tools:$PATH" bash -e "$work/build-repo/deploy/build-engine.sh" --out "$BUILD_OUTPUT" >"$work/build-out" 2>&1
  status=$?
  set -e
  test "$status" != 0 || fail "failed $failing_tool build succeeded"
  test ! -e "$BUILD_OUTPUT/BUILD" || fail "failed $failing_tool build retained stale BUILD"
  test ! -e "$BUILD_OUTPUT/observed" || fail 'BUILD existed during compilation'
  rm -f "$BUILD_OUTPUT/observed"
done

if test "$failures" -gt 0; then echo "$failures provisioning program test(s) failed" >&2; exit 1; fi
echo 'provision programs tests passed'
