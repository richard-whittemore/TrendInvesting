#!/bin/bash
# Tests for deploy/provision.sh's two targets (TI_TARGET): the Lightsail
# server, the default, and the Mac mini's container (mac-mini). Each case
# sources a copy of the provisioner, replaces every command that would change
# the machine with a stand-in that records its arguments, runs `main` with one
# step, and checks what was called. No root, no Docker and no network needed.
#
# The Mac mini target is the plan's option A, approved by the owner on
# 2026-10-18 (Mac_Mini_Paper_Deployment_Plan_2026-10-18.md, section 5, item 1):
# arm64 accepted only with it; no Tailscale, firewall or Linux Gateway; the
# Gateway unit masked rather than enabled; a persistent journal; an editor;
# the clock checked against a reference rather than synced in the container.
# The stand-ins are called by the sourced provisioner, and the variables they
# set are read by it, which shellcheck cannot see.
# shellcheck disable=SC2034,SC2329
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The caller's environment must not decide any case.
unset TI_TARGET TI_REHEARSAL TI_REHEARSAL_ANY_ARCH TI_REHEARSAL_SKIP_INSTALLER TI_CLOCK_VERIFY

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

# A copy of deploy/, whose clock scripts record that they ran (and the clock
# check fails when FAKE_CLOCK_BAD=1).
cp -R "$here/.." "$work/deploy"
cat >"$work/deploy/libexec/time-sync.sh" <<'STUB'
#!/bin/bash
echo "time-sync.sh" >>"$TEST_CALLS"
STUB
cat >"$work/deploy/libexec/clock-check.sh" <<'STUB'
#!/bin/bash
echo "clock-check.sh TI_CLOCK_VERIFY=${TI_CLOCK_VERIFY:-}" >>"$TEST_CALLS"
test "${FAKE_CLOCK_BAD:-0}" != 1
STUB
chmod +x "$work/deploy/libexec/"*.sh

# scenario TARGET ARCH STEP: runs `provision.sh STEP` in a subshell with
# TI_TARGET=TARGET ("" for unset) on a machine reporting ARCH. Its output is
# in $work/out, the commands it called in $work/calls, one per line; its
# exit status is returned. Set FAKE_SYSTEMD=1 for a running systemd and
# TEST_REHEARSAL=1 for TI_REHEARSAL=1.
scenario() {
  local target=$1 arch=$2 step=$3
  : >"$work/calls"
  rm -rf "${work:?}/etc" "${work:?}/units"
  mkdir -p "$work/etc/adapters" "$work/units"
  if test -n "${FAKE_BOOST_JSON:-}"; then printf '%s\n' "$FAKE_BOOST_JSON" >"$work/etc/adapters/boost.json"; fi
  local status=0
  (
    export TEST_CALLS="$work/calls"
    if test -n "$target"; then export TI_TARGET=$target; fi
    if test "${TEST_REHEARSAL:-0}" = 1; then export TI_REHEARSAL=1; fi
    # shellcheck source=/dev/null
    . "$work/deploy/provision.sh"
    # The machine's identity.
    uname() { echo "$arch"; }
    read_os_release() { ID=ubuntu VERSION_ID=24.04; }
    systemd_running() { test "${FAKE_SYSTEMD:-0}" = 1; }
    id() { if test "${1:-}" = -u; then echo 0; fi; }
    # Every command that changes the machine is recorded instead.
    record() { echo "$*" >>"$TEST_CALLS"; }
    install() { record install "$@"; }
    systemctl() { record systemctl "$@"; }
    apt-get() { record apt-get "$@"; }
    useradd() { record useradd "$@"; }
    chmod() { record chmod "$@"; }
    chown() { record chown "$@"; }
    ln() { record ln "$@"; }
    tar() { record tar "$@"; }
    systemd-tmpfiles() { record systemd-tmpfiles "$@"; }
    fetch() { record fetch "$@"; }
    # unzip ends the Gateway step once its download is recorded, before it
    # would replace /opt/ibc.
    unzip() { record unzip "$@"; return 1; }
    ufw() { record ufw "$@"; }
    tailscale() { record tailscale "$@"; return 1; }
    ip() { record ip "$@"; return 1; }
    etc=$work/etc
    unit_dir=$work/units
    main "$step"
  ) >"$work/out" 2>&1 || status=$?
  return "$status"
}
called() { grep -qF -- "$1" "$work/calls"; }
said() { grep -qF -- "$1" "$work/out"; }

# --- An unknown target is refused before anything is done.
if scenario nowhere x86_64 services; then fail "an unknown TI_TARGET was accepted"; fi
said "TI_TARGET" || fail "the refusal of an unknown target does not name TI_TARGET: $(cat "$work/out")"
test ! -s "$work/calls" || fail "an unknown target changed something: $(cat "$work/calls")"

# --- Lightsail (the default): unchanged.
if scenario "" aarch64 services; then fail "Lightsail accepted arm64"; fi
said "x86_64 only" || fail "Lightsail's arm64 refusal changed: $(cat "$work/out")"
test ! -s "$work/calls" || fail "Lightsail's arm64 refusal changed something first"
if scenario lightsail aarch64 services; then fail "TI_TARGET=lightsail accepted arm64"; fi

TEST_REHEARSAL=1 scenario "" x86_64 base || fail "Lightsail base failed: $(cat "$work/out")"
called "fetch https://pkgs.tailscale.com" || fail "Lightsail base no longer fetches Tailscale"
called "ufw" || fail "Lightsail base no longer installs ufw"
if called "journald.conf.d"; then fail "Lightsail base changed the journal's settings"; fi

scenario "" x86_64 services || fail "Lightsail services failed: $(cat "$work/out")"
called "systemctl enable ti-gateway.service" || fail "Lightsail no longer enables the Gateway unit"
if called "systemctl mask"; then fail "Lightsail masks a unit"; fi
called "/systemd/ti-gateway.service" || fail "Lightsail no longer installs the Gateway unit"
called "systemctl enable --now ti-data.timer ti-reporter@boost.timer" || fail "Lightsail no longer starts the timers"
grep -q '"host": "127.0.0.1"' "$work/etc/adapters/boost.json" || fail "Lightsail's adapter configuration does not point at 127.0.0.1"

scenario "" x86_64 firewall || fail "Lightsail firewall failed: $(cat "$work/out")"
said "Tailscale is not signed in" || fail "Lightsail's firewall step no longer checks Tailscale: $(cat "$work/out")"

FAKE_SYSTEMD=1 scenario "" x86_64 clock || fail "Lightsail clock failed: $(cat "$work/out")"
called "time-sync.sh" || fail "Lightsail's clock step no longer syncs time where systemd runs"
called "clock-check.sh TI_CLOCK_VERIFY=" || fail "Lightsail's clock step no longer checks the clock"
if called "clock-check.sh TI_CLOCK_VERIFY=1"; then fail "Lightsail's clock step forces a reference check"; fi

if scenario "" x86_64 gateway; then :; fi
called "fetch https://github.com/IbcAlpha" || fail "Lightsail's gateway step no longer fetches IBC: $(cat "$work/calls")"

# --- The Mac mini.
if scenario mac-mini x86_64 services; then fail "the Mac mini target accepted x86_64"; fi
said "arm64" || fail "the Mac mini's x86_64 refusal does not say arm64: $(cat "$work/out")"
test ! -s "$work/calls" || fail "the Mac mini's x86_64 refusal changed something first"

FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base failed: $(cat "$work/out")"
if called "tailscale"; then fail "Mac mini base fetches Tailscale"; fi
if called "ufw"; then fail "Mac mini base installs ufw"; fi
called "nano" || fail "Mac mini base installs no editor"
called "journald.conf.d" || fail "Mac mini base does not make the journal persistent"
called "systemd-tmpfiles --create --prefix /var/log/journal" || fail "Mac mini base does not prepare /var/log/journal"
called "systemctl restart systemd-journald.service" || fail "Mac mini base does not restart journald with its new setting"
grep -q 'Storage=persistent' "$work/deploy/provision.sh" || fail "the provisioner never sets Storage=persistent"

scenario mac-mini aarch64 services || fail "Mac mini services failed: $(cat "$work/out")"
called "systemctl mask ti-gateway.service" || fail "Mac mini does not mask the Gateway unit"
if called "systemctl enable ti-gateway.service"; then fail "Mac mini enables the Gateway unit"; fi
if called "/systemd/ti-gateway.service"; then fail "Mac mini installs the Gateway unit, which masking cannot replace"; fi
called "systemctl enable --now ti-data.timer ti-reporter@boost.timer" || fail "Mac mini does not start the timers"
called "systemctl enable ti-dashboard.service" || fail "Mac mini does not enable the dashboard"
called "systemctl disable ti-engine@boost.service" || fail "Mac mini does not keep the engines disabled"
called "systemctl disable ti-adapter-boost.service" || fail "Mac mini does not keep the adapter disabled"
grep -q '"host": "host.docker.internal"' "$work/etc/adapters/boost.json" || fail "Mac mini's adapter configuration does not point at host.docker.internal: $(cat "$work/etc/adapters/boost.json")"
grep -q '"port": 4002' "$work/etc/adapters/boost.json" || fail "Mac mini's adapter configuration lost the paper port"

owner='{"host": "owner-set", "port": 4002}'
FAKE_BOOST_JSON=$owner scenario mac-mini aarch64 services || fail "Mac mini services with an owner's file failed"
test "$(cat "$work/etc/adapters/boost.json")" = "$owner" || fail "Mac mini rewrote the owner's adapter configuration"

scenario mac-mini aarch64 firewall || fail "Mac mini firewall failed: $(cat "$work/out")"
if called "ufw" || called "ip " || called "tailscale"; then fail "Mac mini's firewall step touched the firewall or Tailscale: $(cat "$work/calls")"; fi
said "Mac mini" || fail "Mac mini's firewall step does not say why it skips"

scenario mac-mini aarch64 gateway || fail "Mac mini gateway failed: $(cat "$work/out")"
test ! -s "$work/calls" || fail "Mac mini's gateway step installed something: $(cat "$work/calls")"

FAKE_SYSTEMD=1 scenario mac-mini aarch64 clock || fail "Mac mini clock failed: $(cat "$work/out")"
if called "time-sync.sh"; then fail "Mac mini syncs time in the container, against the Mac's clock"; fi
called "clock-check.sh TI_CLOCK_VERIFY=1" || fail "Mac mini does not check the clock against a reference"
if FAKE_SYSTEMD=1 FAKE_CLOCK_BAD=1 scenario mac-mini aarch64 clock; then fail "Mac mini's clock step finished with a bad clock"; fi

FAKE_SYSTEMD=1 scenario mac-mini aarch64 all || fail "Mac mini all failed: $(cat "$work/out")"
if called "fetch"; then fail "Mac mini all downloaded something (Tailscale or the Gateway): $(grep fetch "$work/calls")"; fi
called "clock-check.sh TI_CLOCK_VERIFY=1" || fail "Mac mini all does not run the clock step"
called "systemctl mask ti-gateway.service" || fail "Mac mini all does not run the services step"

test "$failures" -eq 0 || exit 1
echo "target tests ok"
