#!/bin/bash
# Idempotent provisioning for the Trend Investing server (Ubuntu 24.04 LTS,
# x86_64, a Lightsail VM), or for the Mac mini's container (TI_TARGET below).
# Safe to run again after any partial failure.
#
#   sudo deploy/provision.sh base       users, packages, automatic updates, Tailscale binaries
#   sudo deploy/provision.sh clock      enable time syncing and refuse to finish unless the clock is good
#   sudo deploy/provision.sh gateway    IBC and IB Gateway (pinned, checksummed)
#   sudo deploy/provision.sh twsapi     IBKR's TWS API: fetched, verified, its ibapi installed into the
#                                       adapter's environment (not part of all)
#   sudo deploy/provision.sh adapter    the IBKR adapter's code and pin file (not part of all)
#   sudo deploy/provision.sh services   systemd units, directories, config stubs
#   sudo deploy/provision.sh firewall   close everything except Tailscale
#   sudo deploy/provision.sh all        base, clock, gateway, services, then firewall
#                                       if Tailscale is already signed in
#
# Secrets: this script never reads, writes or prints a credential. It creates
# empty root-only stubs for the owner to fill in under /etc/trend-investing/,
# outside the repository. Do not run it with `bash -x`.
#
# TI_REHEARSAL=1 (container rehearsal only): skip steps that need a running
# systemd, a kernel firewall or a network identity.
#
# TI_TARGET selects the machine: lightsail (the default) or mac-mini, the
# long-running arm64 Ubuntu 24.04 container under Docker Desktop on the Mac
# (docs/manual/mac-mini.md; docs/runbooks/mac-mini-paper.md). There IB Gateway
# is IBKR's macOS app outside the container and nothing listens beyond the
# Mac's loopback, so the target accepts arm64, installs no Tailscale, firewall
# or Linux Gateway (the gateway step does nothing and the Gateway's unit is
# not installed; services masks it), makes systemd's journal persistent, and
# installs the TWS API from its pinned Mac/Unix archive. Every other step is
# the same.
set -euo pipefail
set +x

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091
. "$here/versions.env"

rehearsal=${TI_REHEARSAL:-0}
ti_target=${TI_TARGET:-lightsail}
etc=/etc/trend-investing
dl_cache=${TI_DOWNLOAD_CACHE:-/var/cache/trend-investing}

log() { printf 'provision: %s\n' "$*"; }
die() { printf 'provision: %s\n' "$*" >&2; exit 1; }

need_root() { test "$(id -u)" -eq 0 || die "run as root (sudo)"; }

# The distribution's ID and VERSION_ID, read into the caller's shell.
read_os_release() {
  # shellcheck disable=SC1091
  . /etc/os-release
}

# Whether systemd is the running init (it creates this directory at boot).
systemd_running() { test -d /run/systemd/system; }

mac_mini() { test "$ti_target" = mac-mini; }

check_target() {
  case $ti_target in
    lightsail | mac-mini) ;;
    *) die "TI_TARGET must be lightsail (the default) or mac-mini, not '$ti_target'" ;;
  esac
}

check_platform() {
  if mac_mini; then
    # The Mac mini is Apple silicon: its container is arm64, and runs no x86_64
    # installer (the Gateway is the Mac app).
    test "$(uname -m)" = aarch64 || die "the Mac mini target is an arm64 (aarch64) container; this is $(uname -m)"
  else
    # TI_REHEARSAL_ANY_ARCH=1 lets the rehearsal run on an arm64 Docker host,
    # where the x86_64 installer cannot be executed (see TI_REHEARSAL_SKIP_INSTALLER).
    test "$(uname -m)" = x86_64 || test "${TI_REHEARSAL_ANY_ARCH:-0}" = 1 || die "IB Gateway's Linux installer is x86_64 only; this is $(uname -m)"
  fi
  read_os_release
  test "${ID:-}" = ubuntu || die "Ubuntu LTS only (found ${ID:-unknown})"
  case ${VERSION_ID:-} in 24.04) ;; *) die "pinned and rehearsed on Ubuntu 24.04 only (found ${VERSION_ID:-unknown})" ;; esac
}

# fetch URL SHA256 DEST: download to a cache, verify, then place. A cached
# file that fails its checksum is deleted and fetched again once.
fetch() {
  local url=$1 sha=$2 dest=$3
  mkdir -p "$dl_cache"
  if test -f "$dest" && echo "$sha  $dest" | sha256sum -c --status -; then
    return 0
  fi
  rm -f "$dest"
  curl -fsSL --retry 3 --output "$dest.part" "$url"
  if ! echo "$sha  $dest.part" | sha256sum -c --status -; then
    rm -f "$dest.part"
    die "checksum mismatch for $url (expected $sha); not installing. See the runbook, 'Bumping a pinned version'."
  fi
  mv "$dest.part" "$dest"
}

ensure_user() {
  local name=$1 home=$2
  if ! id "$name" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$home" --create-home --shell /usr/sbin/nologin "$name"
  fi
  install -d -o "$name" -g "$name" -m 0750 "$home"
}

step_base() {
  log "packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  # tzdata-legacy, on both targets: IBKR states execution times in
  # "US/Eastern", a name Ubuntu 24.04 moved out of tzdata; without it the
  # adapter refuses to start (adapter/ibkr/cli.py, require_zones), as it
  # could not read an execution report (adapter/ibkr/orders.py,
  # parse_execution_time). A server provisioned before this package was
  # listed gets it by running this step again.
  if mac_mini; then
    # No Gateway, firewall or X display in the container; sudo for running
    # the owner's tools as ti-trader, and nano for editing the configuration
    # and secret files in place.
    apt-get install -y -qq --no-install-recommends \
      ca-certificates curl unzip unattended-upgrades python3 tzdata tzdata-legacy sudo nano systemd >/dev/null
  else
    apt-get install -y -qq --no-install-recommends \
      ca-certificates curl unzip xvfb xauth ufw unattended-upgrades \
      libxtst6 libxrender1 libxi6 libxext6 fontconfig python3 tzdata tzdata-legacy iptables >/dev/null
  fi

  log "service users (non-root, no login shell)"
  ensure_user ti-gateway /var/lib/ti-gateway
  ensure_user ti-trader /var/lib/trend-investing

  log "automatic security updates"
  install -m 0644 /dev/stdin /etc/apt/apt.conf.d/20auto-upgrades <<'CONF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
CONF
  # Security pocket only. No automatic reboot: a reboot restarts the gateway,
  # which may need the owner's phone. Reboots are a Sunday owner step.
  install -m 0644 /dev/stdin /etc/apt/apt.conf.d/52trend-investing-unattended <<'CONF'
Unattended-Upgrade::Allowed-Origins {
        "${distro_id}:${distro_codename}-security";
        "${distro_id}ESMApps:${distro_codename}-apps-security";
        "${distro_id}ESM:${distro_codename}-infra-security";
};
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
CONF

  if mac_mini; then
    step_journal
    log "Mac mini: no Tailscale (nothing listens beyond the Mac's loopback)"
    return 0
  fi

  log "tailscale ${TAILSCALE_VERSION}"
  local tgz="$dl_cache/tailscale_${TAILSCALE_VERSION}_amd64.tgz"
  fetch "$TAILSCALE_URL" "$TAILSCALE_SHA256" "$tgz"
  local tmp
  tmp=$(mktemp -d)
  tar -xzf "$tgz" -C "$tmp" --strip-components=1
  install -m 0755 "$tmp/tailscale" "$tmp/tailscaled" /usr/local/bin/
  if test "$rehearsal" != 1; then
    # tailscaled's own unit, from the pinned tarball, pointed at /usr/local/bin.
    sed 's|/usr/sbin/tailscaled|/usr/local/bin/tailscaled|' "$tmp/systemd/tailscaled.service" >/etc/systemd/system/tailscaled.service
    install -m 0644 "$tmp/systemd/tailscaled.defaults" /etc/default/tailscaled
    systemctl daemon-reload
    systemctl enable --now tailscaled
  fi
  rm -rf "$tmp"
}

# systemd's journal kept on disk, in /var/log/journal (a volume of its own on
# the Mac mini), so the reason a unit stopped survives a restart of the
# container and its removal and re-creation (Mac mini plan, section 5, item 1).
step_journal() {
  log "persistent journal"
  install -d -m 0755 /etc/systemd/journald.conf.d
  install -m 0644 /dev/stdin /etc/systemd/journald.conf.d/50-trend-investing.conf <<'CONF'
[Journal]
Storage=persistent
CONF
  install -d -m 2755 /var/log/journal
  systemd-tmpfiles --create --prefix /var/log/journal
  if test "$rehearsal" != 1 && systemd_running; then
    systemctl restart systemd-journald.service
  fi
}

# The clock (ADR 0019: a clock behind IBKR's makes every snapshot stale and
# withholds every sell). The check is installed first, so the owner can run it
# from /usr/local/libexec/trend-investing after this step even if it fails.
# Where systemd runs, libexec/time-sync.sh keeps a running time daemon or
# installs systemd-timesyncd, and waits for a first sync; the check
# (libexec/clock-check.sh) then refuses unless timedatectl reports the clock
# synchronised or, in a container, unless the clock is within tolerance of an
# outside reference. Re-run safe.
step_clock() {
  install -d -m 0755 /usr/local/libexec/trend-investing
  install -d -m 0755 /etc/trend-investing # holds clock.conf, which the check reads
  install -m 0755 "$here"/libexec/clock-check.sh /usr/local/libexec/trend-investing/
  if test "$rehearsal" != 1 && test -d /run/systemd/system; then
    log "time syncing"
    "$here/libexec/time-sync.sh"
  fi
  log "checking the clock"
  "$here/libexec/clock-check.sh" || die "the clock is not good; fix it and run 'provision.sh clock' again"
}

step_gateway() {
  if mac_mini; then
    log "Mac mini: no Linux Gateway (the Gateway is IBKR's macOS app, logged in by hand)"
    return 0
  fi
  log "IBC ${IBC_VERSION}"
  local zip="$dl_cache/IBCLinux-${IBC_VERSION}.zip"
  fetch "$IBC_URL" "$IBC_SHA256" "$zip"
  if ! test -f "/opt/ibc/.version-${IBC_VERSION}"; then
    local tmp
    tmp=$(mktemp -d)
    unzip -q "$zip" -d "$tmp"
    rm -rf /opt/ibc
    install -d -m 0755 /opt/ibc
    cp -a "$tmp"/. /opt/ibc/
    chmod 0755 /opt/ibc/scripts/*.sh /opt/ibc/*.sh
    touch "/opt/ibc/.version-${IBC_VERSION}"
    rm -rf "$tmp"
  fi

  log "IB Gateway ${GATEWAY_BUILD}"
  local target=/var/lib/ti-gateway/Jts/ibgateway/${GATEWAY_MAJOR}
  if ! test -f "$target/.installed-${GATEWAY_SHA256}"; then
    local installer="$dl_cache/ibgateway-${GATEWAY_BUILD}.sh"
    fetch "$GATEWAY_URL" "$GATEWAY_SHA256" "$installer"
    install -d -o ti-gateway -g ti-gateway -m 0750 /var/lib/ti-gateway/Jts
    # -q: unattended; -dir: where IBC expects <tws-path>/ibgateway/<major>.
    # Installed as the service user so nothing in the Gateway tree is root's.
    # HOME must be writable: the install4j installer writes there.
    if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
      log "rehearsal: checksum verified, installer not executed (not x86_64)"
      return 0
    fi
    runuser -u ti-gateway -- env HOME=/var/lib/ti-gateway sh "$installer" -q -dir "$target" >/dev/null
    test -x "$target/ibgateway" || die "Gateway installer finished but $target/ibgateway is missing"
    touch "$target/.installed-${GATEWAY_SHA256}"
    chown ti-gateway:ti-gateway "$target/.installed-${GATEWAY_SHA256}"
  fi
}

# IBKR's TWS API (the adapter's ibapi), fetched and verified against its
# pinned checksum (ADR 0025 point 4, amended 2026-10-16); a mismatch stops
# the step before anything is installed. Its own step, run with the
# paper-account test and not by `all`: the pin is IBKR's moving "latest"
# link, so once IBKR replaces the file the checksum fails, and that must not
# block the Gateway or the services.
#
# On Lightsail it runs the verified x86_64 installer unprivileged; on the
# Mac mini's arm64 container (TI_TARGET=mac-mini), where that installer
# cannot run (it starts its own x86_64 Java), it fetches the same release's
# pinned Mac/Unix archive instead and unpacks it with twsapi-archive.py
# (owner decision 2026-10-18: no emulation). Either way it installs the Python client
# (source/pythonclient) as root under $twsapi_root/<version>, then
# into the adapter's own
# environment, $adapter_venv, offline (--no-index, --no-deps), checks that
# ibapi imports there, and only then writes the install record, naming the
# pinned file it installed from, that the adapter checks
# at start against the pin installed with its code (adapter/ibkr/ibapi_pin.py):
# a different or missing record and the adapter refuses to start. TWS API
# 10.51.01's ibapi imports protobuf (5.29.5), which nothing installs yet, so
# today the import check fails and nothing is recorded (adapter/ibkr/README.md).
twsapi_root=/opt/trend-investing/twsapi
adapter_venv=/opt/trend-investing/adapter-venv
adapter_root=/opt/trend-investing/adapter
step_twsapi() {
  log "TWS API ${TWSAPI_VERSION}"
  local installer pinned_sha=$TWSAPI_SHA256
  if mac_mini; then
    installer="$dl_cache/twsapi-macunix-${TWSAPI_VERSION}.zip"
    pinned_sha=$TWSAPI_ARCHIVE_SHA256
    fetch "$TWSAPI_ARCHIVE_URL" "$TWSAPI_ARCHIVE_SHA256" "$installer"
  else
    installer="$dl_cache/tws-api-${TWSAPI_VERSION}-linux-x64.sh"
    fetch "$TWSAPI_URL" "$TWSAPI_SHA256" "$installer"
    if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
      log "rehearsal: checksum verified, installer not executed (TI_REHEARSAL_SKIP_INSTALLER)"
      return 0
    fi
  fi
  log "the adapter's Python environment"
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -qq --no-install-recommends python3-venv python3-setuptools python3-wheel >/dev/null
  local tree="$twsapi_root/$TWSAPI_VERSION"
  rm -rf "$tree" "$twsapi_root/INSTALLED"
  install -d -m 0755 "$twsapi_root"
  if mac_mini; then
    # Verifies the archive against its pin again, then unpacks only its one
    # source/pythonclient, as $tree/pythonclient; refuses with nothing
    # written otherwise.
    python3 -I "$here/twsapi-archive.py" "$installer" "$pinned_sha" "$tree" \
      || die "the TWS API ${TWSAPI_VERSION} archive was refused; not installing"
  else
    # The vendor installer runs unprivileged (as nobody), into a scratch
    # directory it owns, as step_gateway runs the Gateway's installer as its
    # service user; only the Python client it unpacks is then installed, as
    # root, under $target. -q: unattended; -dir: where to put the tree. The
    # install4j installer writes under HOME, so it gets a scratch one.
    local scratch
    scratch=$(mktemp -d)
    chmod 0755 "$scratch"
    install -d -o nobody -g nogroup -m 0700 "$scratch/home" "$scratch/tree"
    install -m 0644 "$installer" "$scratch/installer.sh"
    runuser -u nobody -- env HOME="$scratch/home" sh "$scratch/installer.sh" -q -dir "$scratch/tree" >/dev/null
    local unpacked
    unpacked=$(find "$scratch/tree" -type d -name pythonclient -path '*/source/*' | head -n 2)
    if test "$(printf '%s\n' "$unpacked" | grep -c .)" != 1; then
      rm -rf "$scratch"
      die "the TWS API ${TWSAPI_VERSION} tree has not exactly one source/pythonclient; not installing"
    fi
    if ! test -f "$unpacked/setup.py" && ! test -f "$unpacked/pyproject.toml"; then
      rm -rf "$scratch"
      die "the TWS API ${TWSAPI_VERSION} pythonclient has no setup.py or pyproject.toml; not installing"
    fi
    install -d -o root -g root -m 0755 "$tree"
    cp -R "$unpacked" "$tree/pythonclient"
    chown -R root:root "$tree"
    chmod -R u=rwX,go=rX "$tree"
    rm -rf "$scratch"
  fi
  local client="$tree/pythonclient"
  # A fresh environment each time: nothing of an earlier ibapi remains. The
  # system site packages give pip the setuptools and wheel it builds with,
  # offline.
  rm -rf "$adapter_venv"
  python3 -m venv --system-site-packages "$adapter_venv"
  "$adapter_venv/bin/pip" install --quiet --no-index --no-deps --no-build-isolation "$client"
  "$adapter_venv/bin/python3" -c 'import ibapi.client, ibapi.contract, ibapi.order, ibapi.wrapper' \
    || die "ibapi did not import from $adapter_venv after installing it; not recording it"
  printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\n' "$TWSAPI_VERSION" "$pinned_sha" >"$twsapi_root/INSTALLED.tmp"
  chmod 0644 "$twsapi_root/INSTALLED.tmp"
  mv "$twsapi_root/INSTALLED.tmp" "$twsapi_root/INSTALLED"
  log "ibapi from TWS API ${TWSAPI_VERSION} installed into $adapter_venv; recorded in $twsapi_root/INSTALLED"
}

# The IBKR adapter's code (adapter/ibkr, without its tests) and the pin file
# it checks the installed ibapi against, from this checkout, under
# $adapter_root, which the adapter's unit runs from (python3 -m adapter.ibkr).
# Replaced whole, so no file of an earlier version remains. Its own step,
# not run by `all`: install the code you mean to run, then start the unit.
step_adapter() {
  local src="$here/../adapter/ibkr"
  test -f "$src/cli.py" || die "no adapter code at $src; run this from a checkout of the repository"
  local checkout
  checkout=$(cd "$here/.." && pwd)
  log "IBKR adapter code from $(git -c safe.directory="$checkout" -C "$checkout" describe --always --dirty 2>/dev/null || echo 'an unversioned tree')"
  rm -rf "$adapter_root.new"
  install -d -m 0755 "$adapter_root.new/adapter/ibkr"
  install -m 0644 "$src"/*.py "$adapter_root.new/adapter/ibkr/"
  # The NYSE calendar and the table of inputs the data job writes, in the
  # same layout, so the adapter states the market's hours from the same
  # committed calendar and refuses any input type the data job does not
  # write (delivery.py).
  install -d -m 0755 "$adapter_root.new/adapter/marketdata"
  install -m 0644 "$here/../adapter/marketdata/nyse_calendar.py" "$adapter_root.new/adapter/marketdata/"
  install -m 0644 "$here/../adapter/marketdata/kinds.py" "$adapter_root.new/adapter/marketdata/"
  install -m 0644 "$here/versions.env" "$adapter_root.new/versions.env"
  rm -rf "$adapter_root"
  mv "$adapter_root.new" "$adapter_root"
}

# write_stub PATH TEMPLATE: root-only, empty, created once and never
# overwritten, so a re-run cannot erase what the owner entered.
write_stub() {
  local path=$1 template=$2
  if ! test -e "$path"; then
    install -m 0600 -o root -g root "$template" "$path"
    log "created $path (owner step: fill it in)"
  fi
  chmod 0600 "$path"
  chown root:root "$path"
}

# elf_machine FILE: FILE's ELF e_machine (62 x86-64, 183 AArch64), the two
# little-endian bytes at offset 18; empty if FILE is not an ELF file.
elf_machine() {
  test "$(od -An -tx1 -N4 "$1" | tr -d ' \n')" = 7f454c46 || return 0
  od -An -tu2 -j18 -N2 "$1" | tr -d ' \n'
}

# require_native_programs DIST: refuses, before anything is installed, if a
# Go program in DIST is not an ELF file for this machine's architecture
# (dist/ built for the other machine: deploy/build-engine.sh builds
# linux/amd64 for Lightsail, linux/arm64 with --arch arm64 for the Mac mini).
# ti-engine is checked too, though the owner installs it by hand.
require_native_programs() {
  local dist=$1 machine want b got
  machine=$(uname -m)
  case $machine in
    x86_64) want=62 ;;
    aarch64) want=183 ;;
    *) die "no Go build for $machine; not installing" ;;
  esac
  for b in ti-engine ti-reporter ti-dashboard operator journal-query; do
    test -f "$dist/$b" || continue
    got=$(elf_machine "$dist/$b")
    test "$got" = "$want" || die "$dist/$b is not a Linux program for this machine ($machine; its ELF machine is ${got:-none, not ELF}): build it with deploy/build-engine.sh for this machine and copy dist/ again; nothing installed"
  done
}

step_services() {
  local dist=${TI_DIST:-$here/../dist} b
  require_native_programs "$dist"
  log "directories and config"
  install -d -m 0755 "$etc" "$etc/engines" /usr/local/libexec/trend-investing /opt/trend-investing/bin
  install -m 0755 "$here"/libexec/*.sh /usr/local/libexec/trend-investing/
  # Engine settings carry client IDs and modes only, no secrets; repo-managed,
  # overwritten. Each configuration is hashed into its engine's run (ADR 0016).
  install -m 0644 "$here"/etc/engines/*.env "$here"/etc/engines/*.config "$etc/engines/"
  # Non-secret gateway settings. Restart times are New York time (the unit sets TZ).
  install -m 0644 /dev/stdin "$etc/gateway.env" <<CONF
GATEWAY_MAJOR=${GATEWAY_MAJOR}
TI_GATEWAY_RESTART_TIME=11:45 PM
TI_GATEWAY_COLD_RESTART=09:00
CONF
  # The data step (deploy/build-data.sh), when its file is beside deploy/;
  # without it the unit fails when the timer fires, so the gap is seen.
  local data_job=${TI_DATA_BUILD:-$here/../dist/ti-data}
  if test -f "$data_job"; then
    install -m 0755 "$data_job" /opt/trend-investing/bin/ti-data
    log "installed the data step from $data_job"
  else
    log "no $data_job: the data step is not installed (deploy/build-data.sh builds it)"
  fi
  # The reporter, the dashboard and the owner's two tools, built for the
  # server beside ti-engine by deploy/build-engine.sh (the server has no Go)
  # and copied to dist/ beside deploy/. A missing one is named here, and its
  # unit fails when it runs, so the gap is seen. The owner runs the tools as
  # `sudo -u ti-trader operator ...` and `sudo -u ti-trader journal-query ...`:
  # sudo's PATH includes /usr/local/bin, where each is linked.
  for b in ti-reporter ti-dashboard operator journal-query; do
    if test -f "$dist/$b"; then
      install -m 0755 "$dist/$b" "/opt/trend-investing/bin/$b"
      log "installed $b from $dist"
    else
      log "no $dist/$b: $b is not installed (deploy/build-engine.sh builds it)"
    fi
  done
  for b in operator journal-query; do
    if test -x "/opt/trend-investing/bin/$b"; then
      ln -sfn "/opt/trend-investing/bin/$b" "/usr/local/bin/$b"
    fi
  done
  write_stub "$etc/ibkr-login.env" "$here/etc/ibkr-login.env.example"
  write_stub "$etc/secrets.env" "$here/etc/secrets.env.example"
  # Each adapter's account number in a file of its own, handed only to that
  # adapter (ADR 0026 point 8: one engine per account).
  install -d -o root -g root -m 0700 "$etc/accounts"
  write_stub "$etc/accounts/boost.env" "$here/etc/accounts/boost.env.example"
  install -d -o ti-trader -g ti-trader -m 0750 \
    /var/lib/trend-investing/data /var/lib/trend-investing/reports /var/lib/trend-investing/engines
  local a
  for a in boost trad-ira roth-ira; do
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/engines/$a"
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/adapters/$a"
  done
  # The adapter's configuration carries no secret (the account number comes
  # from accounts/boost.env): created once from the example, never overwritten, so
  # the owner's paper settings survive a re-run.
  install -d -m 0755 "$etc/adapters"
  if ! test -e "$etc/adapters/boost.json"; then
    install -m 0644 "$here/etc/adapters/boost.json.example" "$etc/adapters/boost.json"
    log "created $etc/adapters/boost.json (owner step: set paper mode before the paper test)"
  fi

  log "systemd units"
  # The reporter is one instance per account (ti-reporter@<account>); an
  # earlier provisioner's single unit and its timer link are removed. It is
  # stopped and disabled first: systemd keeps a running unit loaded after its
  # file is gone, firing a service that no longer exists.
  if test "$rehearsal" != 1; then
    # Stopped when active even if its file is already gone (the unit stays
    # loaded); disabled only while its file exists. The test sits in an if,
    # so its failure does not trip set -e.
    if systemctl is-active --quiet ti-reporter.timer; then systemctl stop ti-reporter.timer; fi
    # A oneshot still starting is "activating", which is-active does not
    # report as active, so the service is stopped unconditionally; stopping a
    # unit that is not loaded fails, which must not trip set -e.
    systemctl stop ti-reporter.service || true
    if test -e /etc/systemd/system/ti-reporter.timer; then
      systemctl disable ti-reporter.timer
    fi
  fi
  rm -f /etc/systemd/system/ti-reporter.service /etc/systemd/system/ti-reporter.timer \
    /etc/systemd/system/timers.target.wants/ti-reporter.timer
  # On the Mac mini the Gateway's unit file is not installed: masking puts a
  # link to /dev/null in its place, which a real file there would block.
  local unit
  for unit in "$here"/systemd/*.service "$here"/systemd/*.timer; do
    if mac_mini && test "$(basename "$unit")" = ti-gateway.service; then continue; fi
    install -m 0644 "$unit" /etc/systemd/system/
  done
  if mac_mini && test -f /etc/systemd/system/ti-gateway.service && ! test -L /etc/systemd/system/ti-gateway.service; then
    rm -f /etc/systemd/system/ti-gateway.service
  fi
  if test "$rehearsal" != 1; then
    systemctl daemon-reload
    # The Boost account's reporter only: the IRA engines have no
    # configuration yet, and an enabled IRA reporter would fail every firing.
    # `enable` only links them for the next boot, so the timers and the
    # dashboard are started now too: on a server that is not rebooted the
    # reporter, the owner's one eye on the engine, would otherwise never fire
    # (ADR 0019, "Alerting"). The dashboard is restarted, so it runs the
    # build just installed; the reporter picks a new build up at its next
    # firing. Until the Boost engine's run.env exists the dashboard fails,
    # naming it, and restarts by itself; that is logged here, not fatal.
    if mac_mini; then
      # The Gateway is the Mac app; masked, nothing can start this unit (an
      # engine's or the adapter's Wants= on it is then ignored).
      systemctl mask ti-gateway.service
    else
      systemctl enable ti-gateway.service
    fi
    systemctl enable --now ti-data.timer ti-reporter@boost.timer
    systemctl enable ti-dashboard.service
    if ! systemctl restart ti-dashboard.service; then
      log "ti-dashboard did not start (see journalctl -u ti-dashboard); it fails until the Boost engine has run once"
    fi
    # Engines are not enabled: every engine start is the owner's explicit step
    # (docs/runbooks/durable-log.md, "Restarting an engine"), so a reboot
    # leaves them stopped. An earlier provisioner enabled them, and those
    # links outlive it, so each instance is disabled too (ADR 0027, the
    # restart model). Disabling stops nothing that is running.
    systemctl disable ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service
    # The adapter is started by the owner too, after its engine; never enabled.
    systemctl disable ti-adapter-boost.service
    # The gateway starts only once the owner has filled
    # the login file; the unit's own check refuses an empty one. The data
    # step, the reporter and the dashboard fail, each with a message, until
    # their files are installed and the Boost engine's run.env exists; the
    # reporter also until secrets.env holds its channel secrets.
    if mac_mini; then
      log "enabled; started ti-data.timer, ti-reporter@boost.timer and ti-dashboard; ti-gateway is masked (the Gateway is the Mac app)"
    else
      log "enabled; started ti-data.timer, ti-reporter@boost.timer and ti-dashboard; start the gateway with: systemctl start ti-gateway (after the owner enters the IBKR login)"
    fi
  fi
}

# Replaces the whole UFW rule set: `ufw --force reset` removes every existing
# rule, including any added by hand, before the Tailscale-only policy is
# applied. Re-add a manual rule after each run, or add it here.
step_firewall() {
  if test "$rehearsal" = 1; then log "rehearsal: skipping firewall"; return 0; fi
  if mac_mini; then
    log "Mac mini: no firewall in the container (the dashboard is published on the Mac's loopback only)"
    return 0
  fi
  # Refuse to close the public SSH door before the Tailscale door works, or
  # the server locks the owner out.
  if ! ip link show tailscale0 >/dev/null 2>&1 || ! tailscale ip -4 >/dev/null 2>&1; then
    log "SKIPPED firewall: Tailscale is not signed in yet (owner step: sudo tailscale up --ssh). Re-run 'provision.sh firewall' after."
    return 0
  fi
  log "firewall: deny all inbound except Tailscale"
  ufw --force reset >/dev/null
  ufw default deny incoming >/dev/null
  ufw default allow outgoing >/dev/null
  ufw allow in on tailscale0 >/dev/null
  ufw --force enable >/dev/null
  log "also close port 22 in the Lightsail firewall tab (owner step)"
}

main() {
  check_target
  need_root
  check_platform
  case ${1:-} in
    base) step_base ;;
    clock) step_clock ;;
    gateway) step_gateway ;;
    twsapi) step_twsapi ;;
    adapter) step_adapter ;;
    services) step_services ;;
    firewall) step_firewall ;;
    all) step_base; step_clock; step_gateway; step_services; step_firewall ;;
    *) die "usage: provision.sh base|clock|gateway|twsapi|adapter|services|firewall|all" ;;
  esac
  log "done: ${1}"
}

# Run as a program; sourced (by deploy/rehearsal/test-provision-arch.sh), it
# only defines the steps.
if test "${BASH_SOURCE[0]}" = "$0"; then
  main "$@"
fi
