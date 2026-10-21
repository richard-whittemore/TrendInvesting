#!/bin/bash
# Runs inside an arm64 Ubuntu 24.04 container, as root, for
# deploy/mac-mini/rehearse-programs.sh: the checkout's committed tree at
# /src, the arm64 builds at /dist, TI_EXPECT_STAMP the stamp the linux/amd64
# build of the same commit carries, TI_TARGET=mac-mini (provision.sh's Mac
# mini target, which accepts arm64 and installs the TWS API from its pinned
# Mac/Unix archive). Checks, stopping at the first failure:
#
#  1. the container is arm64 and every Go program is an arm64 binary that
#     runs here natively; the engine, started in dry-run, writes that stamp
#     into its durable log's strategy version, and journal-query reads the
#     journal it wrote;
#  2. the paper rehearsal (deploy/rehearse-paper.sh, unchanged) passes;
#  3. provision.sh twsapi refuses a tampered download and a download that is
#     not the pinned file, changing nothing already installed;
#  4. it unpacks ibapi from the pinned Mac/Unix archive and, if ibapi
#     imports, records the pinned version and the archive's pinned SHA-256,
#     and, after provision.sh adapter, the adapter's own pin check passes
#     from its environment. Today ibapi 10.51.01 does not import (its
#     protobuf 5.29.5 is not pinned: adapter/ibkr/README.md), so the step
#     stops with "ibapi did not import" and records nothing; the rehearsal
#     checks that fail-closed stop, says so, and does not claim the install
#     record or the pin check;
#  5. no x86_64 ELF file is on the container's disk outside /proc and /sys
#     (none was installed, none left behind), and none is running once every
#     step has finished. It cannot see a short-lived x86_64 helper that ran
#     and was deleted during a step; that the Mac mini's path runs none is
#     shown by deploy/rehearsal/test-provision-arch.sh (the archive path
#     runs no installer).
#
# Re-runnable in the same container: every wait is bounded, the engine is
# stopped and reaped on every path, and section 3 compares what is installed
# before and after each refused download rather than requiring nothing.
set -euo pipefail
set +x

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/mac-mini/programs-lib.sh
. "$here/programs-lib.sh"
trap 'background_stop 5 || true' EXIT
fail() { echo "PROGRAMS FAIL: $*" >&2; exit 1; }
pass() { echo "PROGRAMS ok: $*"; }
test -n "${TI_EXPECT_STAMP:-}" || fail "TI_EXPECT_STAMP is not set"
test "${TI_TARGET:-}" = mac-mini || fail "TI_TARGET is not mac-mini"
# shellcheck disable=SC1091
. /src/deploy/versions.env

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# The packages provision.sh base installs that these checks use, tzdata-legacy
# among them (IBKR's "US/Eastern" execution times).
apt-get install -y -qq --no-install-recommends ca-certificates curl python3 tzdata tzdata-legacy >/dev/null

# ELF e_machine of a file: 62 x86-64, 183 AArch64; empty if not ELF.
elf_machine() {
  python3 -I -c 'import sys
with open(sys.argv[1], "rb") as f:
    head = f.read(20)
print(int.from_bytes(head[18:20], "little") if head[:4] == b"\x7fELF" else "")' "$1"
}

# 1. The arm64 programs.
test "$(uname -m)" = aarch64 || fail "the container is $(uname -m), not aarch64"
for b in ti-engine ti-reporter ti-dashboard operator journal-query; do
  test "$(elf_machine "/dist/$b")" = 183 || fail "/dist/$b is not an arm64 binary"
done
for b in ti-reporter ti-dashboard; do
  out=$("/dist/$b" -h 2>&1 || true)
  case $out in "Usage of"*) ;; *) fail "$b does not run here: $out" ;; esac
done
for b in operator journal-query; do
  out=$("/dist/$b" 2>&1 || true)
  case $out in "usage: $b"*) ;; *) fail "$b does not run here: $out" ;; esac
done
work=$(mktemp -d)
background_start "$work/engine.out" /dist/ti-engine -mode dry-run -config /src/deploy/etc/engines/boost.config \
  -as-of 2024-01-02T21:00:00Z -socket "$work/engine.sock" -log "$work/run.log" -out "$work/run.journal"
wait_ready "listening on" "$work/engine.out" 10 || fail "the engine did not start within 10 s: $(cat "$work/engine.out")"
status=0
background_stop 10 || status=$?
test "$status" -ne 124 || fail "the engine did not stop within 10 s of SIGTERM (killed): $(cat "$work/engine.out")"
test "$status" -eq 0 || fail "the engine did not stop cleanly (status $status): $(cat "$work/engine.out")"
version=$(python3 -I -c 'import json, sys
with open(sys.argv[1]) as f:
    print(json.loads(f.readline())["strategy_version"])' "$work/run.log")
case $version in
  *"+$TI_EXPECT_STAMP") ;;
  *) fail "the engine's log names $version, not the stamp $TI_EXPECT_STAMP" ;;
esac
/dist/journal-query totals "$work/run.journal" >"$work/totals" || fail "journal-query did not read the engine's journal"
pass "the five Go programs are arm64 and run here; the engine's log names $version"

# 2. The paper rehearsal, unchanged.
(cd /src && deploy/rehearse-paper.sh) | tee "$work/paper.out"
grep -q "^rehearsal passed" "$work/paper.out" || fail "the paper rehearsal did not pass"
pass "the paper rehearsal passed inside the container"

# 3. The TWS API step refuses what is not the pinned file, changing nothing
# installed (an earlier run in this container may have left a tree).
p=/src/deploy/provision.sh
twsapi=/opt/trend-investing/twsapi
venv=/opt/trend-investing/adapter-venv
before=$(install_state "$twsapi" "$venv")
unchanged() { test "$(install_state "$twsapi" "$venv")" = "$before"; }
stubs=$(mktemp -d)
cat >"$stubs/curl" <<'STUB'
#!/bin/bash
# A tampered download: whatever was asked for, the bytes are not IBKR's.
while test $# -gt 0; do
  case $1 in --output) shift; echo tampered >"$1" ;; esac
  shift
done
STUB
chmod +x "$stubs/curl"
if PATH="$stubs:$PATH" TI_DOWNLOAD_CACHE="$(mktemp -d)" "$p" twsapi >/dev/null 2>"$work/err"; then
  fail "a tampered download was accepted"
fi
grep -q "checksum mismatch" "$work/err" || fail "the tampered download failed for another reason: $(cat "$work/err")"
unchanged || fail "a tampered download changed what is installed"
cp -a /src/deploy "$work/deploy-badpin"
sed -i 's/^TWSAPI_ARCHIVE_SHA256=.*/TWSAPI_ARCHIVE_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' \
  "$work/deploy-badpin/versions.env"
if TI_DOWNLOAD_CACHE="$(mktemp -d)" "$work/deploy-badpin/provision.sh" twsapi >/dev/null 2>"$work/err"; then
  fail "a download that is not the pinned file was accepted"
fi
grep -q "checksum mismatch" "$work/err" || fail "the mismatched download failed for another reason: $(cat "$work/err")"
unchanged || fail "a mismatched download changed what is installed"
pass "provision.sh twsapi on arm64 refuses a tampered and a mismatched download; nothing installed changed"

# 4. The pinned archive unpacked; recorded and checked by the adapter only
# if ibapi imports.
installed=0
if "$p" twsapi 2>"$work/err"; then
  installed=1
else
  grep -q "ibapi did not import" "$work/err" || fail "provision.sh twsapi failed for another reason: $(cat "$work/err")"
  test ! -e "$twsapi/INSTALLED" || fail "ibapi did not import, yet an install record was written"
  test -f "$twsapi/$TWSAPI_VERSION/pythonclient/setup.py" || test -f "$twsapi/$TWSAPI_VERSION/pythonclient/pyproject.toml" \
    || fail "the archive's Python client was not unpacked"
  pass "the pinned archive's Python client unpacked; ibapi did not import (protobuf not pinned) and nothing was recorded"
fi
if test "$installed" = 1; then
  printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\n' "$TWSAPI_VERSION" "$TWSAPI_ARCHIVE_SHA256" >"$work/expected"
  cmp -s "$work/expected" "$twsapi/INSTALLED" || fail "the install record is not the pinned version and archive SHA-256: $(cat "$twsapi/INSTALLED")"
  "$p" adapter
  checked=$(cd /opt/trend-investing/adapter && "$venv/bin/python3" -c 'from adapter.ibkr import ibapi_pin
print(ibapi_pin.check_installed("/opt/trend-investing/twsapi/INSTALLED", "/opt/trend-investing/adapter/versions.env"))')
  test "$checked" = "$TWSAPI_VERSION" || fail "the adapter's pin check returned $checked"
  pass "ibapi from TWS API $TWSAPI_VERSION's archive installed and recorded; the adapter's pin check passes"
fi

# 5. Nothing x86_64 on disk, and nothing running once every step finished
# (the engine was stopped and reaped in section 1).
found=$(python3 -I -c '
import os, sys
for top, dirs, files in os.walk("/"):
    if top == "/":
        dirs[:] = [d for d in dirs if d not in ("proc", "sys", "dev")]
    for name in files:
        path = os.path.join(top, name)
        try:
            if os.path.islink(path) or not os.path.isfile(path):
                continue
            with open(path, "rb") as f:
                head = f.read(20)
        except OSError:
            continue
        if head[:4] == b"\x7fELF" and int.from_bytes(head[18:20], "little") == 62:
            print(path)
')
test -z "$found" || fail "x86_64 files are on the container's disk: $found"
for exe in /proc/[0-9]*/exe; do
  real=$(readlink -f "$exe" 2>/dev/null) || continue
  test "$(elf_machine "$real" 2>/dev/null || true)" != 62 || fail "an x86_64 program is running: $real"
done
pass "no x86_64 file is on the container's disk and none is running"
if test "$installed" = 1; then
  echo "programs rehearsal passed"
else
  echo "programs rehearsal passed up to the ibapi import: ibapi does not import until protobuf is pinned, so the install record and the adapter's pin check are not yet verified"
fi
