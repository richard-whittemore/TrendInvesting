#!/bin/bash
# Tests for deploy/provision.sh's two targets (TI_TARGET): the Lightsail
# server, the default, and the Mac mini's rehearsal container (mac-mini). Each
# case sources a copy of the provisioner, replaces every command that would
# change the machine with a stand-in that records its arguments, runs `main`
# with one step, and checks what was called. No root, no Docker and no network
# needed.
#
# The test must never change the machine it runs on, whatever permissions it
# has. Commands that only change the machine (install, systemctl, useradd and
# the rest) are recorded and never run. The file commands the provisioner
# also uses on its own temporary files (rm, cp, mv, mkdir, touch, sed -i...)
# run only when every absolute path they are given is inside the test's own
# tree; any other path is refused, recorded as REFUSED, and ends the case at
# once with status 97, which every case is checked for. Where a step must
# stop part way (the Gateway's unpacking), its stand-in stops it explicitly
# (status 75) rather than relying on `set -e`, which Bash ignores inside a
# subshell tested with `||`. The provisioner writes no file by redirection to
# a literal absolute path (checked below), since a redirection cannot be
# intercepted.
#
# What the Mac mini target does differently is in docs/manual/mac-mini.md:
# arm64 accepted only with it; no Tailscale, firewall or Linux Gateway; the
# Gateway unit masked rather than enabled; a persistent journal; an editor;
# the clock checked against a reference rather than synced, at provisioning
# and every 15 minutes; the service users at fixed IDs.
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
# exit status is returned. Set FAKE_SYSTEMD=1 for a running systemd,
# TEST_REHEARSAL=1 for TI_REHEARSAL=1, FAKE_PASSWD to the stand-in user
# database's lines ("name:x:uid:gid::home:shell"; by default both service
# users exist) and TEST_LEAK to a command run before `main` (the guard's own
# test). A command refused by the guard fails the case unless TEST_LEAK is set.
default_passwd='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin
ti-trader:x:2002:2002::/var/lib/trend-investing:/usr/sbin/nologin'
scenario() {
  local target=$1 arch=$2 step=$3
  : >"$work/calls"
  rm -rf "${work:?}/etc" "${work:?}/units" "${work:?}/tmp" "${work:?}/cwd"
  mkdir -p "$work/etc/adapters" "$work/units" "$work/tmp" "$work/cwd"
  printf '%s\n' "${FAKE_PASSWD-$default_passwd}" | sed '/^$/d' >"$work/passwd"
  sed 's/^\([^:]*\):x:[0-9]*:\([0-9]*\):.*/\1:x:\2:/' "$work/passwd" >"$work/group"
  if test -n "${FAKE_BOOST_JSON:-}"; then printf '%s\n' "$FAKE_BOOST_JSON" >"$work/etc/adapters/boost.json"; fi
  local status=0
  (
    cd "$work/cwd"
    export TMPDIR="$work/tmp"
    export TEST_CALLS="$work/calls"
    if test -n "$target"; then export TI_TARGET=$target; fi
    if test "${TEST_REHEARSAL:-0}" = 1; then export TI_REHEARSAL=1; fi
    # shellcheck source=/dev/null
    . "$work/deploy/provision.sh"
    # The machine's identity.
    uname() { echo "$arch"; }
    read_os_release() { ID=ubuntu VERSION_ID=24.04; }
    systemd_running() { test "${FAKE_SYSTEMD:-0}" = 1; }
    record() { echo "$*" >>"$TEST_CALLS"; }
    # The user database is a stand-in: $work/passwd and $work/group.
    id() {
      case $# in
        0) echo "uid=0(root)" ;;
        1) if test "$1" = -u; then echo 0; else awk -F: -v n="$1" '$1 == n {f = 1} END {exit !f}' "$work/passwd"; fi ;;
        *) local field=3; test "$1" = -g && field=4
           awk -F: -v n="$2" -v f="$field" '$1 == n {print $f; found = 1} END {exit !found}' "$work/passwd" ;;
      esac
    }
    getent() {
      local db=$work/$1
      awk -F: -v k="$2" '$1 == k || $3 == k {print; f = 1} END {exit f ? 0 : 2}' "$db"
    }
    useradd() {
      record useradd "$@"
      local uid=99 gid=99 name
      while test $# -gt 1; do
        case $1 in --uid | -u) uid=$2; shift ;; --gid | -g) gid=$2; shift ;; esac
        shift
      done
      name=$1
      echo "$name:x:$uid:$gid::/nonexistent:/usr/sbin/nologin" >>"$work/passwd"
    }
    groupadd() {
      record groupadd "$@"
      local gid=99
      while test $# -gt 1; do
        case $1 in --gid | -g) gid=$2; shift ;; esac
        shift
      done
      echo "$1:x:$gid:" >>"$work/group"
    }
    # Every command that only changes the machine is recorded, never run.
    install() { record install "$@"; }
    systemctl() { record systemctl "$@"; }
    apt-get() { record apt-get "$@"; }
    usermod() { record usermod "$@"; return 1; }
    userdel() { record userdel "$@"; return 1; }
    groupdel() { record groupdel "$@"; return 1; }
    chmod() { record chmod "$@"; }
    chown() { record chown "$@"; }
    chgrp() { record chgrp "$@"; }
    ln() { record ln "$@"; }
    tar() { record tar "$@"; }
    systemd-tmpfiles() { record systemd-tmpfiles "$@"; }
    fetch() { record fetch "$@"; }
    ufw() { record ufw "$@"; }
    tailscale() { record tailscale "$@"; return 1; }
    ip() { record ip "$@"; return 1; }
    mount() { record mount "$@"; return 1; }
    umount() { record umount "$@"; return 1; }
    curl() { record curl "$@"; return 1; }
    python3() { record python3 "$@"; return 1; }
    sudo() { record sudo "$@"; return 1; }
    docker() { record docker "$@"; return 1; }
    timedatectl() { record timedatectl "$@"; return 1; }
    # Unpacking IBC, or running a vendor installer, is where the Gateway case
    # stops, explicitly.
    unzip() { record unzip "$@"; record "STOPPED at unzip"; exit 75; }
    runuser() { record runuser "$@"; record "STOPPED at runuser"; exit 75; }
    # Temporary folders are made inside the test's tree (macOS's mktemp may
    # not follow TMPDIR).
    mktemp() {
      if test "${1:-}" = -d; then command mktemp -d "$work/tmp/tmp.XXXXXX"; else command mktemp "$work/tmp/tmp.XXXXXX"; fi
    }
    # File commands run only inside the test's tree.
    inside_work() {
      local a p
      for a in "$@"; do
        p=$a
        case $p in --*=*) p=${p#*=} ;; esac
        case $p in
          /dev/null | /dev/stdin | "$work" | "$work"/*) ;;
          /*) return 1 ;;
        esac
      done
    }
    guarded() {
      local cmd=$1
      shift
      if inside_work "$@"; then
        command "$cmd" "$@"
      else
        record "REFUSED $cmd $*"
        echo "test-target: refused '$cmd $*': outside the test's tree" >&2
        exit 97
      fi
    }
    rm() { guarded rm "$@"; }
    rmdir() { guarded rmdir "$@"; }
    cp() { guarded cp "$@"; }
    mv() { guarded mv "$@"; }
    mkdir() { guarded mkdir "$@"; }
    touch() { guarded touch "$@"; }
    tee() { guarded tee "$@"; }
    truncate() { guarded truncate "$@"; }
    dd() { guarded dd "$@"; }
    sed() {
      local a
      for a in "$@"; do
        case $a in -i*) guarded sed "$@"; return ;; esac
      done
      command sed "$@"
    }
    etc=$work/etc
    unit_dir=$work/units
    if test -n "${TEST_LEAK:-}"; then eval "$TEST_LEAK"; fi
    main "$step"
  ) >"$work/out" 2>&1 || status=$?
  if test -z "${TEST_LEAK:-}" && grep -q '^REFUSED' "$work/calls"; then
    fail "provision.sh $step (TI_TARGET=${target:-unset}) ran a file command outside the test's tree: $(grep '^REFUSED' "$work/calls")"
  fi
  return "$status"
}
called() { grep -qF -- "$1" "$work/calls"; }
said() { grep -qF -- "$1" "$work/out"; }

# --- The guard itself: a file command outside the test's tree is refused,
# ends the case, and changes nothing. The canary is outside $work.
canary=$(mktemp -d)
trap 'rm -rf "$work" "$canary"' EXIT
echo keep >"$canary/keep"
for leak in "rm -rf $canary/keep" "cp /dev/null $canary/keep" "mv $canary/keep $canary/moved" \
  "mkdir $canary/made" "touch $canary/touched" "sed -i s/keep/gone/ $canary/keep" \
  "rm -rf --one-file-system /opt/ibc-test-target-canary"; do
  status=0
  TEST_LEAK=$leak scenario mac-mini aarch64 services || status=$?
  test "$status" = 97 || fail "the guard did not end the case on '$leak' (status $status)"
  called "REFUSED ${leak%% *}" || fail "the guard did not record '$leak' as refused: $(cat "$work/calls")"
  if called "systemctl"; then fail "the case went on after '$leak' was refused"; fi
done
test "$(cat "$canary/keep" 2>/dev/null)" = keep || fail "a refused command changed the canary"
test ! -e "$canary/moved" && test ! -e "$canary/made" && test ! -e "$canary/touched" || fail "a refused command created a file outside the test's tree: $(ls "$canary")"
# A path inside the test's tree is allowed.
TEST_LEAK="mkdir -p $work/tmp/allowed" scenario mac-mini aarch64 services || fail "the guard refused a path inside the test's tree: $(cat "$work/out")"
# No redirection to a literal absolute path other than /dev/null, which no
# stand-in could intercept.
if grep -nE '>>?[[:space:]]*/' "$here/../provision.sh" | grep -vE '^[0-9]+:[[:space:]]*#' | grep -vE '>>?[[:space:]]*/dev/(null|stderr)'; then
  fail "provision.sh writes by redirection to a literal absolute path (above); use \$etc, \$unit_dir or another variable the test redirects"
fi

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

status=0
scenario "" x86_64 gateway || status=$?
test "$status" = 75 || fail "Lightsail's gateway step did not stop at its stand-in (status $status): $(cat "$work/out")"
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

# --- The Mac mini's service users have fixed IDs. /etc/passwd is in the
# container's own disk, not a volume, so a container created again from a
# changed image creates them again; the IDs must be the ones that own the
# files in the volumes, whatever the image's own system users took.
FAKE_PASSWD='' FAKE_SYSTEMD=1 scenario mac-mini aarch64 base || fail "Mac mini base with no service users failed: $(cat "$work/out")"
called "groupadd --system --gid 2002 ti-trader" || fail "Mac mini does not create ti-trader's group at GID 2002: $(grep -E 'useradd|groupadd' "$work/calls")"
called "useradd --system --uid 2002 --gid 2002" || fail "Mac mini does not create ti-trader at UID 2002: $(grep -E 'useradd|groupadd' "$work/calls")"
called "useradd --system --uid 2001 --gid 2001" || fail "Mac mini does not create ti-gateway at UID 2001: $(grep -E 'useradd|groupadd' "$work/calls")"
grep -q '^ti-trader:x:2002:2002:' "$work/passwd" || fail "ti-trader is not 2002:2002 after a Mac mini base: $(cat "$work/passwd")"
# A changed image whose own system user already holds 2002: refused, naming
# it, before any user is created.
shifted='ti-shift:x:2002:2002::/nonexistent:/usr/sbin/nologin'
if FAKE_PASSWD=$shifted FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted UID 2002 held by another account"; fi
said "ti-shift" || fail "the refusal of a taken UID does not name its holder: $(cat "$work/out")"
if called "useradd --system --uid 2002"; then fail "Mac mini created ti-trader over another account's UID"; fi
# A ti-trader already there at another UID: refused, since the volumes' files
# are owned by 2002.
moved='ti-gateway:x:2001:2001::/var/lib/ti-gateway:/usr/sbin/nologin
ti-trader:x:998:998::/var/lib/trend-investing:/usr/sbin/nologin'
if FAKE_PASSWD=$moved FAKE_SYSTEMD=1 scenario mac-mini aarch64 base; then fail "Mac mini accepted ti-trader at UID 998"; fi
said "998" || fail "the refusal of ti-trader at another UID does not say which: $(cat "$work/out")"
# Lightsail is unchanged: the users are created as before, at no fixed ID.
FAKE_PASSWD='' TEST_REHEARSAL=1 scenario "" x86_64 base || fail "Lightsail base with no service users failed: $(cat "$work/out")"
called "useradd --system --user-group --home-dir /var/lib/trend-investing" || fail "Lightsail no longer creates ti-trader as before: $(grep useradd "$work/calls")"
if called "--uid"; then fail "Lightsail now creates a service user at a fixed ID"; fi

# --- The Mac mini's clock is checked every 15 minutes, not only when
# provisioning runs (ADR 0019: a clock behind IBKR's withholds every sell).
scenario mac-mini aarch64 services || fail "Mac mini services failed: $(cat "$work/out")"
called "/mac-mini/systemd/ti-clock-check.service" || fail "Mac mini does not install the clock check's unit"
called "/mac-mini/systemd/ti-clock-check.timer" || fail "Mac mini does not install the clock check's timer"
called "systemctl enable --now ti-clock-check.timer" || fail "Mac mini does not start the clock check's timer"
grep -q '^Environment=TI_CLOCK_VERIFY=1' "$work/deploy/mac-mini/systemd/ti-clock-check.service" || fail "the clock check's unit does not compare with a reference"
grep -q '^OnUnitActiveSec=15min' "$work/deploy/mac-mini/systemd/ti-clock-check.timer" || fail "the clock check's timer is not every 15 minutes"
scenario "" x86_64 services || fail "Lightsail services failed: $(cat "$work/out")"
if called "ti-clock-check"; then fail "Lightsail installs the Mac mini's clock check"; fi

test "$failures" -eq 0 || exit 1
echo "target tests ok"
