#!/bin/bash
# Runs inside an arm64 Ubuntu 24.04 container, as root, for
# deploy/mac-mini/rehearse-programs.sh: the checkout's committed tree at
# /src, the arm64 builds at /dist, TI_EXPECT_STAMP the stamp the linux/amd64
# build of the same commit carries. Proves, stopping at the first failure:
#
#  1. the container is arm64 and every Go program is an arm64 binary that
#     runs here natively; the engine, started in dry-run, writes that stamp
#     into its durable log's strategy version, and journal-query reads the
#     journal it wrote;
#  2. the paper rehearsal (deploy/rehearse-paper.sh, unchanged) passes;
#  3. provision.sh twsapi on arm64 refuses a tampered download and a
#     download that is not the pinned file, with nothing installed;
#  4. it installs ibapi from the pinned Mac/Unix archive, records the pinned
#     version and the archive's pinned SHA-256, and, after provision.sh
#     adapter, the adapter's own pin check passes from its environment;
#  5. no x86_64 program runs in the container, and the adapter's environment
#     holds no x86_64 file.
set -euo pipefail
set +x

fail() { echo "PROGRAMS FAIL: $*" >&2; exit 1; }
pass() { echo "PROGRAMS ok: $*"; }
test -n "${TI_EXPECT_STAMP:-}" || fail "TI_EXPECT_STAMP is not set"

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
/dist/ti-engine -mode dry-run -config /src/deploy/etc/engines/boost.config -as-of 2024-01-02T21:00:00Z \
  -socket "$work/engine.sock" -log "$work/run.log" -out "$work/run.journal" >"$work/engine.out" 2>&1 &
engine=$!
for _ in $(seq 100); do
  grep -q "listening on" "$work/engine.out" && break
  kill -0 "$engine" 2>/dev/null || fail "the engine stopped: $(cat "$work/engine.out")"
  sleep 0.1
done
grep -q "listening on" "$work/engine.out" || fail "the engine did not start"
kill -TERM "$engine"
wait "$engine" || fail "the engine did not stop cleanly: $(cat "$work/engine.out")"
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

# 3. The TWS API step refuses what is not the pinned file, installing nothing.
p=/src/deploy/provision.sh
twsapi=/opt/trend-investing/twsapi
venv=/opt/trend-investing/adapter-venv
nothing_installed() { test ! -e "$twsapi/INSTALLED" && test ! -e "$twsapi/10.51.01" && test ! -e "$venv"; }
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
nothing_installed || fail "a tampered download left something installed"
cp -a /src/deploy "$work/deploy-badpin"
sed -i 's/^TWSAPI_ARCHIVE_SHA256=.*/TWSAPI_ARCHIVE_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' \
  "$work/deploy-badpin/versions.env"
if TI_DOWNLOAD_CACHE="$(mktemp -d)" "$work/deploy-badpin/provision.sh" twsapi >/dev/null 2>"$work/err"; then
  fail "a download that is not the pinned file was accepted"
fi
grep -q "checksum mismatch" "$work/err" || fail "the mismatched download failed for another reason: $(cat "$work/err")"
nothing_installed || fail "a mismatched download left something installed"
pass "provision.sh twsapi on arm64 refuses a tampered and a mismatched download; nothing installed"

# 4. The pinned archive installed, recorded and checked by the adapter.
"$p" twsapi
# shellcheck disable=SC1091
. /src/deploy/versions.env
printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\n' "$TWSAPI_VERSION" "$TWSAPI_ARCHIVE_SHA256" >"$work/expected"
cmp -s "$work/expected" "$twsapi/INSTALLED" || fail "the install record is not the pinned version and archive SHA-256: $(cat "$twsapi/INSTALLED")"
"$p" adapter
checked=$(cd /opt/trend-investing/adapter && "$venv/bin/python3" -c 'from adapter.ibkr import ibapi_pin
print(ibapi_pin.check_installed("/opt/trend-investing/twsapi/INSTALLED", "/opt/trend-investing/adapter/versions.env"))')
test "$checked" = "$TWSAPI_VERSION" || fail "the adapter's pin check returned $checked"
pass "ibapi from TWS API $TWSAPI_VERSION's archive installed and recorded; the adapter's pin check passes"

# 5. Nothing x86_64.
for exe in /proc/[0-9]*/exe; do
  real=$(readlink -f "$exe" 2>/dev/null) || continue
  test "$(elf_machine "$real" 2>/dev/null || true)" != 62 || fail "an x86_64 program is running: $real"
done
while IFS= read -r -d '' f; do
  test "$(elf_machine "$f")" != 62 || fail "the adapter's environment holds an x86_64 file: $f"
done < <(find "$venv" -type f -print0)
pass "no x86_64 program runs here and the adapter's environment holds no x86_64 file"
echo "programs rehearsal passed"
