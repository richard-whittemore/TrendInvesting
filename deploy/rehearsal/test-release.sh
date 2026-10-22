#!/bin/bash
# Tests for deploy/release.sh (the server's `ti-release`): the checksum and
# attestation checks that gate an install, the refusal while an engine unit is
# mid-restart, the install that restarts the dashboard and never the engine,
# the previous build kept, the verify that puts the previous build back when
# the dashboard does not come up, and --rollback. The release is a local fake
# release folder reached through file:// and the server is a temporary prefix
# with a stand-in systemctl and gh that record every call; no network, no root,
# no systemd. The fake programs are small text files, not ELF.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
script=$repo/deploy/release.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unset TI_RELEASE_URL TI_PREFIX TI_LINK_DIR TI_SYSTEMCTL TI_GH

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
expect() { # description expected actual
  if test "$2" != "$3"; then fail "$1: want '$2', got '$3'"; fi
}
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

programs="ti-engine ti-reporter ti-dashboard operator journal-query"

# make_release TAG [DASHBOARD_BODY]: a fake release folder with correct checksums.
make_release() {
  local tag=$1 dash=${2:-} d=$work/rel/$1 p
  mkdir -p "$d"
  for p in $programs; do printf '#!/bin/sh\necho %s %s\n' "$p" "$tag" >"$d/$p"; done
  if test -n "$dash"; then printf '#!/bin/sh\necho %s\n' "$dash" >"$d/ti-dashboard"; fi
  : >"$d/checksums.txt"
  for p in $programs; do printf '%s  %s\n' "$(sha "$d/$p")" "$p" >>"$d/checksums.txt"; done
  printf '{"fake":"bundle"}\n' >"$d/attestation.sigstore.json"
}
make_release v1.0.0
make_release v1.1.0
make_release v1.2.0 BAD
make_release v1.3.0

# The stand-in systemctl. Engine units listed as mid-restart when
# FAKE_ENGINE_BUSY is set; the dashboard is active unless the installed
# dashboard program says BAD.
mkdir -p "$work/stub"
cat >"$work/stub/systemctl" <<'STUB'
#!/bin/bash
echo "$*" >>"$FAKE_CTL_LOG"
case $1 in
  list-units) if test -n "${FAKE_ENGINE_BUSY:-}"; then echo "ti-engine@boost.service loaded deactivating stop"; fi ;;
  is-active) ! grep -q BAD "$TI_PREFIX/bin/ti-dashboard" ;;
  restart | daemon-reload) ;;
  *) echo "unexpected systemctl $*" >&2; exit 9 ;;
esac
STUB
cat >"$work/stub/gh" <<'STUB'
#!/bin/bash
echo "$*" >>"$FAKE_GH_LOG"
test -z "${FAKE_GH_FAIL:-}"
STUB
chmod +x "$work/stub/systemctl" "$work/stub/gh"

# fresh_server: a prefix with an earlier hand-installed build, like a server
# provisioned before the first release.
fresh_server() {
  export TI_PREFIX=$work/server TI_LINK_DIR=$work/server/usr-local-bin
  rm -rf "$TI_PREFIX"
  mkdir -p "$TI_PREFIX/bin" "$TI_LINK_DIR"
  local p
  for p in $programs; do printf '#!/bin/sh\necho %s hand\n' "$p" >"$TI_PREFIX/bin/$p"; chmod 0755 "$TI_PREFIX/bin/$p"; done
  : >"$work/ctl.log"
  : >"$work/gh.log"
}
export TI_RELEASE_URL=file://$work/rel TI_RELEASE_NO_ROOT=1 TI_RELEASE_ANY_ARCH=1
export TI_SYSTEMCTL=$work/stub/systemctl TI_GH=$work/stub/gh
export FAKE_CTL_LOG=$work/ctl.log FAKE_GH_LOG=$work/gh.log

# run ARGS...: the script's status; its output is in $work/out.
run() { local s=0; "$script" "$@" >"$work/out" 2>&1 || s=$?; echo "$s"; }
installed() { cat "$TI_PREFIX/bin/$1"; }
state() { cat "$TI_PREFIX/releases/$1" 2>/dev/null || echo none; }
nonzero() { test "$1" -ne 0 && echo nonzero || echo "$1"; }

fresh_server
expect "no argument is refused" nonzero "$(nonzero "$(run)")"
for bad in ../x main v1/../../x "v1 0" ""; do
  expect "tag '$bad' is refused" nonzero "$(nonzero "$(run "$bad")")"
done
expect "nothing downloaded for a bad tag" none "$(state current)"

# An engine unit mid-restart: refused, nothing installed or restarted.
expect "an engine mid-restart is refused" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY=1 run v1.0.0)")"
grep -qi "mid-restart" "$work/out" || fail "the refusal does not say why: $(cat "$work/out")"
grep -q "ti-engine@boost" "$work/out" || fail "the refusal does not name the unit"
expect "no program installed" "echo ti-dashboard hand" "$(installed ti-dashboard | tail -1)"
if grep -q '^restart' "$work/ctl.log"; then fail "something was restarted despite the refusal"; fi

# A corrupted program: checksum mismatch, bin untouched.
make_release v0.9.0
printf 'tampered\n' >>"$work/rel/v0.9.0/operator"
expect "a checksum mismatch is refused" nonzero "$(nonzero "$(run v0.9.0)")"
grep -q "operator" "$work/out" || fail "the mismatch does not name the program"
expect "bin untouched after a mismatch" "echo operator hand" "$(installed operator | tail -1)"
expect "no release recorded after a mismatch" none "$(state current)"

# A program missing from checksums.txt.
make_release v0.9.1
grep -v journal-query "$work/rel/v0.9.1/checksums.txt" >"$work/c" && mv "$work/c" "$work/rel/v0.9.1/checksums.txt"
expect "an unlisted program is refused" nonzero "$(nonzero "$(run v0.9.1)")"

# An attestation that does not verify.
expect "a failed attestation is refused" nonzero "$(nonzero "$(FAKE_GH_FAIL=1 run v1.0.0)")"
expect "bin untouched after a failed attestation" "echo ti-dashboard hand" "$(installed ti-dashboard | tail -1)"

# The first release.
: >"$work/ctl.log"
expect "the first release installs" 0 "$(run v1.0.0)"
for p in $programs; do expect "$p is v1.0.0" "echo $p v1.0.0" "$(installed "$p" | tail -1)"; done
test -x "$TI_PREFIX/bin/ti-engine" || fail "ti-engine is not executable"
expect "current is v1.0.0" v1.0.0 "$(state current)"
expect "the hand-installed build is the previous" initial "$(state previous)"
expect "the hand-installed build is kept" "echo ti-dashboard hand" "$(tail -1 "$TI_PREFIX/releases/initial/ti-dashboard")"
test -L "$TI_LINK_DIR/operator" && test -L "$TI_LINK_DIR/journal-query" || fail "operator and journal-query are not linked"
grep -q '^restart ti-dashboard' "$work/ctl.log" || fail "the dashboard was not restarted"
if grep -E '^(restart|stop|start|kill).*ti-engine' "$work/ctl.log"; then fail "the engine was touched"; fi
if grep -E '^(restart|stop|start).*ti-reporter' "$work/ctl.log"; then fail "the reporter was restarted"; fi
grep -q "attestation verify" "$work/gh.log" || fail "gh attestation verify was not run"
grep -q "next fresh run" "$work/out" || fail "the output does not say the engine takes the build at the next fresh run"
grep -q "v1.0.0" "$work/out" || fail "the output does not name the new tag"
grep -qi "changed" "$work/out" || fail "the output does not say what changed"

# The same tag again changes nothing.
: >"$work/ctl.log"
expect "the same tag again succeeds" 0 "$(run v1.0.0)"
grep -qi "already" "$work/out" || fail "the repeat does not say it is already installed"
if grep -q '^restart' "$work/ctl.log"; then fail "a repeat restarted the dashboard"; fi

# Without gh the install proceeds on the checksums and says it did not verify
# the attestation.
fresh_server
expect "without gh the install proceeds" 0 "$(TI_GH=$work/stub/no-such-gh run v1.0.0)"
grep -qi "attestation not verified" "$work/out" || fail "the missing gh is not reported: $(cat "$work/out")"

# A second release keeps the first as the previous build; the hand-installed
# one is dropped.
expect "the second release installs" 0 "$(run v1.1.0)"
expect "current is v1.1.0" v1.1.0 "$(state current)"
expect "previous is v1.0.0" v1.0.0 "$(state previous)"
test ! -e "$TI_PREFIX/releases/initial" || fail "the older build was not pruned"

# A release whose dashboard does not come back: reported, and the previous
# build is put back, so nothing is left half-installed.
: >"$work/ctl.log"
expect "a failed verify is nonzero" nonzero "$(nonzero "$(run v1.2.0)")"
grep -qi "dashboard" "$work/out" || fail "the verify failure does not name the dashboard"
for p in $programs; do expect "$p is back to v1.1.0" "echo $p v1.1.0" "$(installed "$p" | tail -1)"; done
expect "current is still v1.1.0" v1.1.0 "$(state current)"
expect "previous is still v1.0.0" v1.0.0 "$(state previous)"
expect "the dashboard was restarted twice" 2 "$(grep -c '^restart ti-dashboard' "$work/ctl.log")"

# Rollback.
: >"$work/ctl.log"
expect "rollback succeeds" 0 "$(run --rollback)"
for p in $programs; do expect "$p rolled back to v1.0.0" "echo $p v1.0.0" "$(installed "$p" | tail -1)"; done
expect "current is v1.0.0 after rollback" v1.0.0 "$(state current)"
expect "previous is v1.1.0 after rollback" v1.1.0 "$(state previous)"
grep -q '^restart ti-dashboard' "$work/ctl.log" || fail "rollback did not restart the dashboard"
if grep -E '^(restart|stop|start|kill).*ti-engine' "$work/ctl.log"; then fail "rollback touched the engine"; fi
grep -q "next fresh run" "$work/out" || fail "rollback does not say the engine keeps its build until the next fresh run"

# Rollback refuses a damaged previous build, and while an engine is mid-restart.
printf 'x\n' >>"$TI_PREFIX/releases/v1.1.0/ti-reporter"
expect "rollback to a damaged build is refused" nonzero "$(nonzero "$(run --rollback)")"
expect "bin untouched after the refused rollback" "echo ti-reporter v1.0.0" "$(installed ti-reporter | tail -1)"
expect "an engine mid-restart blocks rollback" nonzero "$(nonzero "$(FAKE_ENGINE_BUSY=1 run --rollback)")"

# Rollback with no previous build.
fresh_server
expect "rollback with nothing to roll back to is refused" nonzero "$(nonzero "$(run --rollback)")"

# A rollback whose dashboard does not come back is reported.
fresh_server
run v1.2.0 >/dev/null || true
run v1.1.0 >/dev/null
run v1.2.0 >/dev/null || true
# current is v1.1.0, previous is the hand build; make the previous bad.
printf '#!/bin/sh\necho BAD\n' >"$TI_PREFIX/releases/initial/ti-dashboard"
(cd "$TI_PREFIX/releases/initial" && : >checksums.txt && for p in $programs; do printf '%s  %s\n' "$(sha "$p")" "$p" >>checksums.txt; done)
expect "a rollback whose dashboard does not return fails" nonzero "$(nonzero "$(run --rollback)")"
grep -qi "dashboard" "$work/out" || fail "the rollback failure does not name the dashboard"

# The release workflow: tags only (no deploy on a merge), every action pinned
# by commit SHA, the attestation step present, and valid to actionlint when
# it is installed.
wf=$repo/.github/workflows/release.yml
grep -Eq '^on:' "$wf" || fail "the release workflow has no trigger"
if grep -Eq 'branches:|pull_request|workflow_dispatch|schedule:' "$wf"; then fail "the release workflow runs on more than a version tag"; fi
grep -Eq 'tags: \["v\*"\]' "$wf" || fail "the release workflow is not triggered by a v* tag"
grep -q 'actions/attest-build-provenance@' "$wf" || fail "the release workflow does not attest the build"
if grep -E '^\s*(- )?uses:' "$wf" | grep -Ev '@[0-9a-f]{40} #'; then fail "an action in the release workflow is not pinned by SHA"; fi
if command -v actionlint >/dev/null 2>&1; then
  (cd "$repo" && actionlint .github/workflows/release.yml) || fail "actionlint rejects the release workflow"
fi

if test "$failures" -ne 0; then echo "$failures failure(s)" >&2; exit 1; fi
echo "test-release: ok"
