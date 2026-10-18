#!/bin/bash
# Idempotent provisioning for the Trend Investing server (Ubuntu 24.04 LTS,
# x86_64, a Lightsail VM). Safe to run again after any partial failure.
#
#   sudo deploy/provision.sh base       users, packages, automatic updates, Tailscale binaries
#   sudo deploy/provision.sh gateway    IBC and IB Gateway (pinned, checksummed)
#   sudo deploy/provision.sh twsapi     IBKR's TWS API: fetched, verified, its ibapi installed into the
#                                       adapter's environment (not part of all)
#   sudo deploy/provision.sh adapter    the IBKR adapter's code and pin file (not part of all)
#   sudo deploy/provision.sh services   systemd units, directories, config stubs
#   sudo deploy/provision.sh firewall   close everything except Tailscale
#   sudo deploy/provision.sh all        base, gateway, services, then firewall
#                                       if Tailscale is already signed in
#
# Secrets: this script never reads, writes or prints a credential. It creates
# empty root-only stubs for the owner to fill in under /etc/trend-investing/,
# outside the repository. Do not run it with `bash -x`.
#
# TI_REHEARSAL=1 (container rehearsal only): skip steps that need a running
# systemd, a kernel firewall or a network identity.
set -euo pipefail
set +x

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091
. "$here/versions.env"

rehearsal=${TI_REHEARSAL:-0}
etc=/etc/trend-investing
dl_cache=${TI_DOWNLOAD_CACHE:-/var/cache/trend-investing}

log() { printf 'provision: %s\n' "$*"; }
die() { printf 'provision: %s\n' "$*" >&2; exit 1; }

need_root() { test "$(id -u)" -eq 0 || die "run as root (sudo)"; }

check_platform() {
  # TI_REHEARSAL_ANY_ARCH=1 lets the rehearsal run on an arm64 Docker host,
  # where the x86_64 installer cannot be executed (see TI_REHEARSAL_SKIP_INSTALLER).
  test "$(uname -m)" = x86_64 || test "${TI_REHEARSAL_ANY_ARCH:-0}" = 1 || die "IB Gateway's Linux installer is x86_64 only; this is $(uname -m)"
  # shellcheck disable=SC1091
  . /etc/os-release
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
  apt-get install -y -qq --no-install-recommends \
    ca-certificates curl unzip xvfb xauth ufw unattended-upgrades \
    libxtst6 libxrender1 libxi6 libxext6 fontconfig python3 tzdata iptables >/dev/null

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

step_gateway() {
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
# Runs the verified installer unprivileged, installs the Python client it
# bundles (source/pythonclient) as root under $twsapi_root/<version>, then
# into the adapter's own
# environment, $adapter_venv, offline (--no-index, --no-deps: ibapi has no
# dependencies), and only then writes the install record the adapter checks
# at start against the pin installed with its code (adapter/ibkr/ibapi_pin.py):
# a different or missing record and the adapter refuses to start.
twsapi_root=/opt/trend-investing/twsapi
adapter_venv=/opt/trend-investing/adapter-venv
adapter_root=/opt/trend-investing/adapter
step_twsapi() {
  log "TWS API ${TWSAPI_VERSION}"
  local installer="$dl_cache/tws-api-${TWSAPI_VERSION}-linux-x64.sh"
  fetch "$TWSAPI_URL" "$TWSAPI_SHA256" "$installer"
  if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
    log "rehearsal: checksum verified, installer not executed (not x86_64)"
    return 0
  fi
  log "the adapter's Python environment"
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -qq --no-install-recommends python3-venv python3-setuptools python3-wheel >/dev/null
  local target="$twsapi_root/$TWSAPI_VERSION"
  rm -rf "$target" "$twsapi_root/INSTALLED"
  install -d -m 0755 "$twsapi_root"
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
  install -d -o root -g root -m 0755 "$target"
  cp -R "$unpacked" "$target/pythonclient"
  chown -R root:root "$target"
  chmod -R u=rwX,go=rX "$target"
  rm -rf "$scratch"
  local client="$target/pythonclient"
  # A fresh environment each time: nothing of an earlier ibapi remains. The
  # system site packages give pip the setuptools and wheel it builds with,
  # offline.
  rm -rf "$adapter_venv"
  python3 -m venv --system-site-packages "$adapter_venv"
  "$adapter_venv/bin/pip" install --quiet --no-index --no-deps --no-build-isolation "$client"
  "$adapter_venv/bin/python3" -c 'import ibapi.client, ibapi.contract, ibapi.order, ibapi.wrapper' \
    || die "ibapi did not import from $adapter_venv after installing it; not recording it"
  printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\n' "$TWSAPI_VERSION" "$TWSAPI_SHA256" >"$twsapi_root/INSTALLED.tmp"
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

step_services() {
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
  local dist=${TI_DIST:-$here/../dist} b
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
  install -m 0644 "$here"/systemd/*.service "$here"/systemd/*.timer /etc/systemd/system/
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
    systemctl enable ti-gateway.service
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
    log "enabled; started ti-data.timer, ti-reporter@boost.timer and ti-dashboard; start the gateway with: systemctl start ti-gateway (after the owner enters the IBKR login)"
  fi
}

# Replaces the whole UFW rule set: `ufw --force reset` removes every existing
# rule, including any added by hand, before the Tailscale-only policy is
# applied. Re-add a manual rule after each run, or add it here.
step_firewall() {
  if test "$rehearsal" = 1; then log "rehearsal: skipping firewall"; return 0; fi
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
  need_root
  check_platform
  case ${1:-} in
    base) step_base ;;
    gateway) step_gateway ;;
    twsapi) step_twsapi ;;
    adapter) step_adapter ;;
    services) step_services ;;
    firewall) step_firewall ;;
    all) step_base; step_gateway; step_services; step_firewall ;;
    *) die "usage: provision.sh base|gateway|twsapi|adapter|services|firewall|all" ;;
  esac
  log "done: ${1}"
}

main "$@"
