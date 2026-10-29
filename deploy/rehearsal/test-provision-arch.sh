#!/bin/bash
# Tests for deploy/provision.sh's architecture rules: which TWS API pin each
# target installs from (the Lightsail server, the default, always IBKR's
# x86_64 installer; the Mac mini's arm64 container, TI_TARGET=mac-mini, the
# same release's pinned Mac/Unix archive: owner decision 2026-10-18, no
# emulation), that the Mac mini fetches no amd64 Tailscale, that both targets
# install tzdata-legacy (IBKR's "US/Eastern"), and that `services` refuses a
# Go program in dist/ built for another architecture than the machine's,
# installing nothing. Each case sources a copy of the provisioner, replaces
# every command that would change the machine with a stand-in that records
# its arguments, runs `main` with one step, and checks what was called. No
# root, no Docker and no network needed.
# The stand-ins are called by the sourced provisioner, and the variables they
# set are read by it, which shellcheck cannot see.
# shellcheck disable=SC2034,SC2329
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The caller's environment must not decide any case.
unset TI_TARGET TI_REHEARSAL TI_REHEARSAL_ANY_ARCH TI_REHEARSAL_SKIP_INSTALLER TI_DIST
# The harness's own switches are the caller's to set per case, never inherited.
unset FAKE_ANY_ARCH FAKE_SKIP FAKE_DIST FAKE_NO_CLOCK FAKE_LIVE

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

cp -R "$here/.." "$work/deploy"
# shellcheck disable=SC1091
. "$here/../versions.env"

# scenario TARGET ARCH STEP: runs `provision.sh STEP` in a subshell with
# TI_TARGET=TARGET ("" for unset) on a machine reporting ARCH. Its output is
# in $work/out, the commands it called in $work/calls, one per line; its
# exit status is returned. FAKE_ANY_ARCH=1 sets TI_REHEARSAL_ANY_ARCH=1,
# FAKE_SKIP=1 TI_REHEARSAL_SKIP_INSTALLER=1, and FAKE_DIST the dist/ folder.
# FAKE_LIVE=1 runs without TI_REHEARSAL, so the systemctl branches execute
# against the stand-ins. The systemd unit folder is always $work/units, never
# the host's /etc/systemd/system.
scenario() {
  local target=$1 arch=$2 step=$3
  : >"$work/calls"
  local status=0
  (
    export TEST_CALLS="$work/calls"
    if test -n "$target"; then export TI_TARGET=$target; fi
    if test "${FAKE_ANY_ARCH:-0}" = 1; then export TI_REHEARSAL_ANY_ARCH=1; fi
    if test "${FAKE_SKIP:-0}" = 1; then export TI_REHEARSAL_SKIP_INSTALLER=1; fi
    export TI_REHEARSAL=1 TI_DIST=${FAKE_DIST:-$work/no-dist}
    # shellcheck source=/dev/null
    . "$work/deploy/provision.sh"
    uname() { echo "$arch"; }
    read_os_release() { ID=ubuntu VERSION_ID=24.04; }
    systemd_running() { false; }
    id() { if test "${1:-}" = -u; then echo 0; fi; }
    record() { echo "$*" >>"$TEST_CALLS"; }
    install() { record install "$@"; }
    systemctl() { record systemctl "$@"; }
    apt-get() { record apt-get "$@"; }
    useradd() { record useradd "$@"; }
    chmod() { record chmod "$@"; }
    chown() { record chown "$@"; }
    ln() { record ln "$@"; }
    rm() { record rm "$@"; }
    mv() { record mv "$@"; }
    tar() { record tar "$@"; }
    systemd-tmpfiles() { record systemd-tmpfiles "$@"; }
    fetch() { record fetch "$@"; }
    # The TWS API step ends at its first unpacking command, once the pin it
    # fetched is recorded.
    python3() { record python3 "$@"; return 1; }
    runuser() { record runuser "$@"; return 1; }
    # The service users are test-target.sh's subject, not this test's.
    ensure_user() { record ensure_user "$@"; }
    ensure_operator_user() { record ensure_operator_user "$@"; }
    # The clock check reads the machine's time daemon, which the stand-ins do not model.
    if test "${FAKE_NO_CLOCK:-0}" = 1; then step_clock() { record step_clock; }; fi
    etc=$work/etc
    unit_dir=$work/units
    if test "${FAKE_LIVE:-0}" = 1; then rehearsal=0; fi
    twsapi_root=$work/twsapi
    adapter_venv=$work/venv
    main "$step"
  ) >"$work/out" 2>&1 || status=$?
  return "$status"
}
called() { grep -qF -- "$1" "$work/calls"; }
said() { grep -qF -- "$1" "$work/out"; }

# elf FILE MACHINE: a file with an ELF header naming MACHINE (62 x86-64,
# 183 AArch64), as `go build` writes for linux/amd64 and linux/arm64.
elf() {
  printf '\177ELF\002\001\001' >"$1"
  head -c 11 /dev/zero >>"$1"
  case $2 in
    62) printf '\076\000' >>"$1" ;;
    183) printf '\267\000' >>"$1" ;;
  esac
  head -c 44 /dev/zero >>"$1"
}
# dist DIR MACHINE: the five Go programs, all for MACHINE.
dist() {
  mkdir -p "$1"
  local b
  for b in ti-engine ti-reporter ti-dashboard operator journal-query; do elf "$1/$b" "$2"; done
}

# --- The TWS API pin each target installs from.
if scenario mac-mini aarch64 twsapi; then :; fi
called "fetch $TWSAPI_ARCHIVE_URL $TWSAPI_ARCHIVE_SHA256" || fail "the Mac mini does not fetch the pinned archive: $(cat "$work/out")"
if called "fetch $TWSAPI_URL "; then fail "the Mac mini fetches the x86_64 installer"; fi
called "twsapi-archive.py" || fail "the Mac mini does not unpack the archive with twsapi-archive.py: $(cat "$work/calls")"
if called "runuser"; then fail "the Mac mini runs the x86_64 installer"; fi

if scenario "" x86_64 twsapi; then :; fi
called "fetch $TWSAPI_URL $TWSAPI_SHA256" || fail "Lightsail does not fetch the pinned installer: $(cat "$work/out")"
if called "$TWSAPI_ARCHIVE_URL"; then fail "Lightsail fetches the Mac/Unix archive"; fi

# Lightsail rehearsed on an arm64 Docker host still installs Lightsail's pin.
if FAKE_ANY_ARCH=1 scenario "" aarch64 twsapi; then :; fi
called "fetch $TWSAPI_URL $TWSAPI_SHA256" || fail "Lightsail on an arm64 rehearsal host does not fetch the installer: $(cat "$work/calls")"
if called "$TWSAPI_ARCHIVE_URL"; then fail "the archive path is reachable without TI_TARGET=mac-mini"; fi

if scenario "" aarch64 twsapi; then fail "Lightsail accepted arm64 for twsapi"; fi
said "x86_64 only" || fail "Lightsail's arm64 refusal changed: $(cat "$work/out")"
if scenario mac-mini x86_64 twsapi; then fail "the Mac mini target accepted x86_64"; fi
said "arm64" || fail "the Mac mini's x86_64 refusal does not say arm64: $(cat "$work/out")"

FAKE_SKIP=1 scenario "" x86_64 twsapi || fail "Lightsail's rehearsal skip failed: $(cat "$work/out")"
said "installer not executed" || fail "the skip does not say the installer was not executed: $(cat "$work/out")"
if said "not x86_64"; then fail "the skip on x86_64 says 'not x86_64': $(cat "$work/out")"; fi
if called "runuser"; then fail "the skip ran the installer"; fi

# --- base: tzdata-legacy everywhere; no amd64 Tailscale on the Mac mini.
scenario "" x86_64 base || fail "Lightsail base failed: $(cat "$work/out")"
called "tzdata-legacy" || fail "Lightsail base does not install tzdata-legacy"
called "fetch $TAILSCALE_URL" || fail "Lightsail base no longer fetches Tailscale"
scenario mac-mini aarch64 base || fail "Mac mini base failed: $(cat "$work/out")"
called "tzdata-legacy" || fail "Mac mini base does not install tzdata-legacy"
if called "tailscale"; then fail "Mac mini base fetches the amd64 Tailscale tarball: $(cat "$work/calls")"; fi

# --- The Mac mini runs no Linux Gateway: `gateway` fetches and runs nothing,
# `all` is base, clock, services (no Gateway unit installed) and no firewall.
scenario mac-mini aarch64 gateway || fail "Mac mini gateway failed: $(cat "$work/out")"
said "no Linux Gateway" || fail "the Mac mini's gateway step does not say it installs none: $(cat "$work/out")"
if called "fetch"; then fail "the Mac mini gateway step fetches something: $(cat "$work/calls")"; fi
if called "runuser"; then fail "the Mac mini gateway step runs an installer"; fi
if called "systemctl"; then fail "the Mac mini gateway step touches systemd"; fi

dist "$work/arm64all" 183
dist "$work/amd64" 62
FAKE_NO_CLOCK=1 FAKE_DIST=$work/arm64all scenario mac-mini aarch64 all || fail "Mac mini all failed: $(cat "$work/out")"
if called "fetch $GATEWAY_URL" || called "fetch $IBC_URL"; then fail "Mac mini all fetches the Gateway or IBC: $(cat "$work/calls")"; fi
if called "runuser"; then fail "Mac mini all runs an installer"; fi
if called "ti-gateway.service"; then fail "Mac mini all installs the Gateway unit: $(cat "$work/calls")"; fi
if called "fetch $TAILSCALE_URL"; then fail "Mac mini all fetches Tailscale"; fi
FAKE_DIST=$work/amd64 scenario "" x86_64 services || fail "Lightsail services failed"
called "$work/deploy/systemd/ti-gateway.service" || fail "Lightsail services no longer installs the Gateway unit: $(cat "$work/calls")"

# --- The Mac mini's services step with the systemctl branches executing: a
# stray regular-file Gateway unit is deleted, the Gateway is masked and never
# enabled or installed, and every other unit is still installed.
mkdir -p "$work/units"
echo stray >"$work/units/ti-gateway.service"
FAKE_LIVE=1 FAKE_DIST=$work/arm64all scenario mac-mini aarch64 services || fail "Mac mini live services failed: $(cat "$work/out")"
called "rm -f $work/units/ti-gateway.service" || fail "the Mac mini does not delete a regular-file Gateway unit: $(cat "$work/calls")"
called "systemctl mask ti-gateway.service" || fail "the Mac mini does not mask the Gateway: $(cat "$work/calls")"
if called "systemctl enable ti-gateway.service"; then fail "the Mac mini enables the Gateway: $(cat "$work/calls")"; fi
if called "systemd/ti-gateway.service"; then fail "the Mac mini installs the Gateway unit: $(cat "$work/calls")"; fi
for u in ti-dashboard.service ti-data.timer ti-reporter@.service; do
  called "install -m 0644 $work/deploy/systemd/$u $work/units/" || fail "the Mac mini skipped installing $u: $(cat "$work/calls")"
done
called "systemctl enable ti-dashboard.service" || fail "the Mac mini does not enable the dashboard: $(cat "$work/calls")"
# A link (what mask leaves) is not a file to delete.
rm -f "$work/units/ti-gateway.service"
ln -s /dev/null "$work/units/ti-gateway.service"
FAKE_LIVE=1 FAKE_DIST=$work/arm64all scenario mac-mini aarch64 services || fail "Mac mini live services (masked) failed: $(cat "$work/out")"
if called "rm -f $work/units/ti-gateway.service"; then fail "the Mac mini deletes the masking link: $(cat "$work/calls")"; fi
called "systemctl mask ti-gateway.service" || fail "the Mac mini does not re-mask the Gateway: $(cat "$work/calls")"
rm -f "$work/units/ti-gateway.service"
FAKE_LIVE=1 FAKE_DIST=$work/amd64 scenario "" x86_64 services || fail "Lightsail live services failed: $(cat "$work/out")"
called "systemctl enable ti-gateway.service" || fail "Lightsail does not enable the Gateway: $(cat "$work/calls")"
for u in ti-data@roth-ira.timer ti-reporter@roth-ira.timer; do
  called "$u" || fail "Roth timer not enabled: $u"
done
called "systemctl disable ti-adapter-boost.service ti-adapter@trad-ira.service ti-adapter@roth-ira.service" || fail "Roth adapter not disabled"
for instance in boost trad-ira roth-ira; do
  called "install -m 0644 $work/deploy/systemd/ti-flex@$instance.timer.d/stagger.conf $work/units/ti-flex@$instance.timer.d/" || fail "missing Flex stagger for $instance"
done
called "install -m 0644 $work/deploy/systemd/ti-data@roth-ira.timer.d/stagger.conf $work/units/ti-data@roth-ira.timer.d/" || fail "missing Roth data stagger"
if grep -E 'systemctl (enable|start).*ti-memory' "$work/calls" >/dev/null; then fail "measurement enabled without owner"; fi
if called "systemctl mask"; then fail "Lightsail masks a unit: $(cat "$work/calls")"; fi

# --- services refuses a Go program built for another architecture.
FAKE_DIST=$work/amd64 scenario "" x86_64 services || fail "Lightsail services refused an x86_64 dist/: $(cat "$work/out")"
called "install -m 0755 $work/amd64/ti-reporter /opt/trend-investing/bin/ti-reporter" || fail "Lightsail services did not install the reporter"

dist "$work/arm64" 183
if FAKE_DIST=$work/arm64 scenario "" x86_64 services; then fail "Lightsail services accepted an arm64 dist/"; fi
said "ti-engine" || fail "the refusal does not name the program: $(cat "$work/out")"
said "x86_64" || fail "the refusal does not name the server's architecture: $(cat "$work/out")"
if called "/opt/trend-investing/bin/"; then fail "a refused dist/ installed something: $(cat "$work/calls")"; fi

dist "$work/mixed" 62
elf "$work/mixed/journal-query" 183
if FAKE_DIST=$work/mixed scenario "" x86_64 services; then fail "Lightsail services accepted one arm64 program"; fi
said "journal-query" || fail "the refusal does not name the arm64 program: $(cat "$work/out")"
if called "/opt/trend-investing/bin/"; then fail "a dist/ with one arm64 program installed the others: $(cat "$work/calls")"; fi

dist "$work/notelf" 62
echo '#!/bin/sh' >"$work/notelf/operator"
if FAKE_DIST=$work/notelf scenario "" x86_64 services; then fail "Lightsail services accepted a Go program that is not ELF"; fi
said "operator" || fail "the refusal does not name the non-ELF program: $(cat "$work/out")"

FAKE_DIST=$work/arm64 scenario mac-mini aarch64 services || fail "Mac mini services refused an arm64 dist/: $(cat "$work/out")"
if FAKE_DIST=$work/amd64 scenario mac-mini aarch64 services; then fail "Mac mini services accepted an x86_64 dist/"; fi

test "$failures" -eq 0 || exit 1
echo "provision architecture tests ok"
