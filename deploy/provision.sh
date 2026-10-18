#!/bin/bash
# Idempotent provisioning for the Trend Investing server (Ubuntu 24.04 LTS,
# x86_64, a Lightsail VM). Safe to run again after any partial failure.
#
#   sudo deploy/provision.sh base       users, packages, automatic updates, Tailscale binaries
#   sudo deploy/provision.sh gateway    IBC, IB Gateway and the TWS API (pinned, checksummed)
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
    libxtst6 libxrender1 libxi6 libxext6 fontconfig python3 iptables >/dev/null

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

  # IBKR's TWS API (the adapter's ibapi), fetched and verified against its
  # pinned checksum (ADR 0025 point 4, amended 2026-10-16). It is not yet
  # installed into the adapter's environment: that step is written and run
  # with the paper-account test, the first use of the real client.
  log "TWS API ${TWSAPI_VERSION} (fetched and verified, not installed)"
  fetch "$TWSAPI_URL" "$TWSAPI_SHA256" "$dl_cache/tws-api-${TWSAPI_VERSION}-linux-x64.sh"

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
  write_stub "$etc/ibkr-login.env" "$here/etc/ibkr-login.env.example"
  write_stub "$etc/secrets.env" "$here/etc/secrets.env.example"
  install -d -o ti-trader -g ti-trader -m 0750 \
    /var/lib/trend-investing/data /var/lib/trend-investing/reports /var/lib/trend-investing/engines
  local a
  for a in boost trad-ira roth-ira; do
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/engines/$a"
  done

  log "systemd units"
  install -m 0644 "$here"/systemd/*.service "$here"/systemd/*.timer /etc/systemd/system/
  if test "$rehearsal" != 1; then
    systemctl daemon-reload
    systemctl enable ti-gateway.service ti-data.timer ti-reporter.timer ti-dashboard.service
    # Engines are not enabled: every engine start is the owner's explicit step
    # (docs/runbooks/durable-log.md, "Restarting an engine"), so a reboot
    # leaves them stopped. An earlier provisioner enabled them, and those
    # links outlive it, so each instance is disabled too (ADR 0027, the
    # restart model). Disabling stops nothing that is running.
    systemctl disable ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service
    # The gateway starts only once the owner has filled
    # the login file; the unit's own check refuses an empty one. The data
    # step, reporter and dashboard are skipped (a unit condition) until their
    # binaries are installed.
    log "enabled; start with: systemctl start ti-gateway (after the owner enters the IBKR login)"
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
    services) step_services ;;
    firewall) step_firewall ;;
    all) step_base; step_gateway; step_services; step_firewall ;;
    *) die "usage: provision.sh base|gateway|services|firewall|all" ;;
  esac
  log "done: ${1}"
}

main "$@"
