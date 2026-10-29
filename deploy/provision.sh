#!/bin/bash
# Idempotent provisioning for the Trend Investing server (Ubuntu 24.04 LTS,
# x86_64, a Lightsail VM), or for the Mac mini's container (TI_TARGET below).
# Safe to run again after any partial failure.
#
#   sudo deploy/provision.sh base       users, packages, automatic updates, Tailscale binaries
#   sudo deploy/provision.sh clock      enable time syncing and refuse to finish unless the clock is good
#   sudo deploy/provision.sh gateway    IBC and IB Gateway (pinned, checksummed)
#   sudo deploy/provision.sh twsapi     IBKR's TWS API and the protobuf wheel it imports: fetched,
#                                       verified, installed into the adapter's environment
#                                       (not part of all)
#   sudo deploy/provision.sh adapter    the IBKR adapter's code and pin file (not part of all)
#   sudo deploy/provision.sh services   systemd units, directories, config stubs
#   sudo deploy/provision.sh firewall   close everything except Tailscale
#   sudo deploy/provision.sh ntfy       the owner's own ntfy server (pinned, checksummed), listening
#                                       on the Tailscale address only; run after Tailscale is
#                                       signed in (not part of all)
#   sudo deploy/provision.sh all        base, clock, gateway, services, then firewall
#                                       if Tailscale is already signed in
#
# Secrets: this script never reads, writes or prints a credential. It creates
# root-only stubs, names only, for the owner to fill in under
# /etc/trend-investing/, outside the repository, and has ti-secrets
# (deploy/secrets.sh) encrypt every secret file still in plain text, a stub
# or an earlier server's file, so none rests on disk unencrypted (ADR
# 0028). ti-secrets asks the owner for the passphrase at the
# terminal only when there is something to encrypt. Do not run it with
# `bash -x`.
#
# TI_REHEARSAL=1 (container rehearsal only): skip steps that need a running
# systemd, a kernel firewall or a network identity.
#
# TI_TARGET selects the machine: lightsail (the default) or mac-mini, the
# local rehearsal container with a real systemd that deploy/mac-mini/up.sh
# creates under Docker Desktop on the owner's Mac (docs/manual/mac-mini.md;
# docs/runbooks/mac-mini.md). There IB Gateway is IBKR's macOS app outside the
# container and nothing listens beyond the Mac's loopback, so the target
# accepts arm64, installs no Tailscale, firewall or Linux Gateway, masks the
# Gateway unit, makes systemd's journal persistent, creates the service users
# at fixed IDs, and checks the clock against a reference, at provisioning and
# every 15 minutes, instead of syncing it. Every other step is the same.
set -euo pipefail
set +x

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091
. "$here/versions.env"

rehearsal=${TI_REHEARSAL:-0}
target=${TI_TARGET:-lightsail}
etc=/etc/trend-investing
unit_dir=/etc/systemd/system
# Absolute, because the vendor installers run from "/" (cd / && runuser ...).
dl_cache=${TI_DOWNLOAD_CACHE-/var/cache/trend-investing}
# The Mac mini's volumes that hold files owned by the service users.
volume_roots=(/etc/trend-investing /var/lib/trend-investing /opt/trend-investing)

log() { printf 'provision: %s\n' "$*"; }
die() { printf 'provision: %s\n' "$*" >&2; exit 1; }
test -n "$dl_cache" || die "TI_DOWNLOAD_CACHE is set but empty"
case $dl_cache in /*) ;; *) dl_cache=$PWD/$dl_cache ;; esac

need_root() { test "$(id -u)" -eq 0 || die "run as root (sudo)"; }

# The distribution's ID and VERSION_ID, read into the caller's shell.
read_os_release() {
  # shellcheck disable=SC1091
  . /etc/os-release
}

# Whether systemd is the running init (it creates this directory at boot).
systemd_running() { test -d /run/systemd/system; }

mac_mini() { test "$target" = mac-mini; }

check_target() {
  case $target in
    lightsail | mac-mini) ;;
    *) die "TI_TARGET must be lightsail (the default) or mac-mini, not '$target'" ;;
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

# The user that writes the owner's instructions into the operator inboxes
# (ADR 0026, kill-switch amendment): not the engine's user, and no unit runs
# as it, so no network-facing unit can write an instruction. Its group is
# the engine's, so it reaches the engines' state directories (0750), and
# the engine reads its records through that group. The base and services
# steps both ensure it, so a server provisioned before it existed gains it
# on a services upgrade. The tool gives each record the inbox's group, which
# the process may do for any group it belongs to; the step checks the
# primary group is ti-trader anyway, so a record's default group is the
# engine's even where the tool did not set it, and so the user is a member
# on a server where it was made another way. Another primary group is set,
# or the step stops naming the mismatch.
#
# ensure_operator_user [UID]: with UID (the Mac mini, whose /etc/passwd is
# made anew with each container while the inboxes it owns are in a volume),
# ti-operator is created at that UID, and an account already holding it, or a
# ti-operator at another UID, is refused as ensure_user refuses its own.
ensure_operator_user() {
  local want=${1:-} holder uid
  if ! id ti-operator >/dev/null 2>&1; then
    if test -n "$want"; then
      holder=$(getent passwd "$want" | cut -d: -f1) || true
      test -z "$holder" || die "UID $want is held by the account '$holder' in this image, so ti-operator cannot be created at it; the operator inboxes in the volumes are owned by $want. Remove that account from the image (deploy/mac-mini/Dockerfile) and run up.sh --recreate"
      useradd --system --uid "$want" --gid ti-trader --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ti-operator
    else
      useradd --system --gid ti-trader --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ti-operator
    fi
  elif test -n "$want"; then
    uid=$(id -u ti-operator)
    test "$uid" = "$want" || die "ti-operator is UID $uid here, not $want: this container was provisioned before the IDs were fixed. Re-creating it does not re-own the files already in the volumes, which stay $uid. Run up.sh --recreate, then re-own them in the new container: docker exec --env TI_TARGET=mac-mini CONTAINER /root/deploy/provision.sh reown ti-operator $uid (CONTAINER is ti unless TI_MAC_NAME is set), then run up.sh again (docs/manual/mac-mini.md, 'What can go wrong')"
  fi
  local group
  group=$(id -gn ti-operator)
  if test "$group" != ti-trader; then
    log "ti-operator's primary group is $group; setting it to ti-trader"
    usermod -g ti-trader ti-operator || die "ti-operator's primary group is $group, not ti-trader, and usermod -g ti-trader ti-operator failed; fix it by hand and run again"
    group=$(id -gn ti-operator)
    test "$group" = ti-trader || die "ti-operator's primary group is still $group, not ti-trader; fix it with usermod -g ti-trader ti-operator and run again"
  fi
}

# ensure_user NAME HOME [ID]: the service user NAME, with its own group, and
# its home. With ID, the user and its group are created at that UID and GID,
# and a user already there at another ID, or another account holding ID, is
# refused rather than adopted.
ensure_user() {
  local name=$1 home=$2 want=${3:-} holder
  if ! id "$name" >/dev/null 2>&1; then
    if test -n "$want"; then
      holder=$(getent passwd "$want" | cut -d: -f1) || true
      test -z "$holder" || die "UID $want is held by the account '$holder' in this image, so $name cannot be created at it; the files in the volumes are owned by $want. Remove that account from the image (deploy/mac-mini/Dockerfile) and run up.sh --recreate"
      # A group of this name at this GID is one an earlier run made before
      # it was cut off: reused, so that a re-run converges.
      holder=$(getent group "$want" | cut -d: -f1) || true
      if test "$holder" != "$name"; then
        test -z "$holder" || die "GID $want is held by the group '$holder' in this image, so $name's group cannot be created at it. Remove that group from the image (deploy/mac-mini/Dockerfile) and run up.sh --recreate"
        groupadd --system --gid "$want" "$name"
      fi
      useradd --system --uid "$want" --gid "$want" --home-dir "$home" --create-home --shell /usr/sbin/nologin "$name"
    else
      useradd --system --user-group --home-dir "$home" --create-home --shell /usr/sbin/nologin "$name"
    fi
  elif test -n "$want"; then
    # A container provisioned before the IDs were fixed: its files in the
    # volumes are owned by the old IDs, and a re-creation, which only makes
    # /etc/passwd anew, leaves them so. The remedy names the re-owning pass,
    # by number, which needs no account to exist.
    local uid gid
    uid=$(id -u "$name")
    gid=$(id -g "$name")
    test "$uid:$gid" = "$want:$want" || die "$name is $uid:$gid here, not $want:$want: this container was provisioned before the IDs were fixed. Re-creating it does not re-own the files already in the volumes, which stay $uid:$gid. Run up.sh --recreate, then re-own them in the new container: docker exec --env TI_TARGET=mac-mini CONTAINER /root/deploy/provision.sh reown $name $uid $gid (CONTAINER is ti unless TI_MAC_NAME is set), then run up.sh again (docs/manual/mac-mini.md, 'What can go wrong')"
  fi
  install -d -o "$name" -g "$name" -m 0750 "$home"
}

step_base() {
  log "packages"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  if mac_mini; then
    # No Gateway, firewall or X display in the container; sudo for running
    # the owner's tools as ti-trader, and nano for editing the configuration
    # and secret files in place.
    apt-get install -y -qq --no-install-recommends \
      ca-certificates curl unzip unattended-upgrades python3 tzdata tzdata-legacy sudo nano gnupg >/dev/null
  else
    apt-get install -y -qq --no-install-recommends \
      ca-certificates curl unzip xvfb xauth ufw unattended-upgrades \
      libxtst6 libxrender1 libxi6 libxext6 fontconfig python3 tzdata tzdata-legacy iptables gnupg >/dev/null
  fi

  log "service users (non-root, no login shell)"
  if mac_mini; then
    # /etc/passwd is on the container's own disk, not in a volume, so a
    # container created again (up.sh --recreate, onto a changed image) creates
    # these users again. Fixed IDs, above the range the image's packages take
    # (system users are 100 to 999, the image's own user 1000), keep them the
    # owners of their files in the volumes.
    ensure_user ti-gateway /var/lib/ti-gateway 2001
    ensure_user ti-trader /var/lib/trend-investing 2002
    ensure_operator_user 2003
  else
    ensure_user ti-gateway /var/lib/ti-gateway
    ensure_user ti-trader /var/lib/trend-investing
    ensure_operator_user
  fi

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
    sed 's|/usr/sbin/tailscaled|/usr/local/bin/tailscaled|' "$tmp/systemd/tailscaled.service" >"$unit_dir/tailscaled.service"
    install -m 0644 "$tmp/systemd/tailscaled.defaults" /etc/default/tailscaled
    systemctl daemon-reload
    systemctl enable --now tailscaled
  fi
  rm -rf "$tmp"
}

# systemd's journal kept on disk, in /var/log/journal (a volume of its own on
# the Mac mini), so the reason a unit stopped survives a restart of the
# container and its removal and re-creation (docs/manual/mac-mini.md).
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
  if mac_mini; then
    # Docker's Linux VM keeps the time (Docker Desktop resynchronises it with
    # the Mac) and the container shares it, so no time daemon runs here even
    # though systemd does; the check always compares the clock with an outside
    # reference (TI_CLOCK_VERIFY=1), since "synchronised" inside the container
    # would describe the VM. ti-clock-check.timer repeats it every 15 minutes.
    log "Mac mini: the Mac keeps the time; checking the clock against a reference"
    TI_CLOCK_VERIFY=1 "$here/libexec/clock-check.sh" || die "the clock is not good: turn on the Mac's automatic date and time, then run 'provision.sh clock' again"
    return 0
  fi
  if test "$rehearsal" != 1 && systemd_running; then
    log "time syncing"
    "$here/libexec/time-sync.sh"
  fi
  log "checking the clock"
  "$here/libexec/clock-check.sh" || die "the clock is not good; fix it and run 'provision.sh clock' again"
}

# fix_ibc_modes DIR: the IBC tree is root's and readable by every service
# user: ti-gateway runs IBC's scripts and reads config.ini, whatever modes the
# zip or root's umask gave the files. The template holds no secrets; the
# credentials are injected into the unit's runtime copy.
fix_ibc_modes() {
  local dir=$1
  chown -R root:root "$dir"
  chmod -R u=rwX,go=rX "$dir"
  chmod 0755 "$dir"/scripts/*.sh "$dir"/*.sh
}

# install_ibc_tree SRC DEST: replaces DEST with SRC's files, then fixes modes.
install_ibc_tree() {
  local src=$1 dest=$2
  rm -rf "$dest"
  install -d -m 0755 "$dest"
  cp -a "$src"/. "$dest"/
  fix_ibc_modes "$dest"
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
    install_ibc_tree "$tmp" /opt/ibc
    touch "/opt/ibc/.version-${IBC_VERSION}"
    rm -rf "$tmp"
  else
    # Repairs a tree installed with the zip's own modes, without unpacking it.
    fix_ibc_modes /opt/ibc
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
    (cd / && runuser -u ti-gateway -- env HOME=/var/lib/ti-gateway sh "$installer" -q -dir "$target" >/dev/null)
    test -x "$target/ibgateway" || die "Gateway installer finished but $target/ibgateway is missing"
    touch "$target/.installed-${GATEWAY_SHA256}"
    chown ti-gateway:ti-gateway "$target/.installed-${GATEWAY_SHA256}"
  fi
}

# verify_adapter_venv VENV PINNED_PROTOBUF: the twsapi step's last gate, before
# it records INSTALLED. -I: the venv's own packages only, nothing from the
# current directory. The modules must import; the interpreter must be a real
# venv (sys.prefix is not sys.base_prefix, as the adapter checks at start);
# google.protobuf and ibapi must resolve under the realpath of VENV itself;
# and google.protobuf must be the pinned version. The venv is made with
# --system-site-packages, so a system google/__init__.py (a regular package)
# can shadow the venv's namespace google; the location checks run before the
# version check so that is reported as the shadow it is, not as a version
# mismatch (ibapi_pin.py; ADR 0025).
verify_adapter_venv() {
  local venv=$1 want=$2 py="$1/bin/python3" out real prefix base version pb ib
  real=$(cd "$venv" && pwd -P) || die "$venv does not exist; not recording it"
  "$py" -I -c 'import google.protobuf, ibapi.client, ibapi.contract, ibapi.order, ibapi.wrapper' \
    || die "ibapi did not import from $venv after installing it; not recording it"
  out=$("$py" -I -c 'import os, sys, google.protobuf, ibapi
print(os.path.realpath(sys.prefix)); print(os.path.realpath(sys.base_prefix))
print(google.protobuf.__version__)
print(os.path.realpath(google.protobuf.__file__)); print(os.path.realpath(ibapi.__file__))') \
    || die "could not read where protobuf and ibapi resolve in $venv; not recording it"
  { read -r prefix; read -r base; read -r version; read -r pb; read -r ib; } <<<"$out"
  test "$prefix" != "$base" || die "$venv is not a venv (its interpreter's sys.prefix is the base prefix $base); not recording it"
  case $pb in "$real"/*) ;; *) die "google.protobuf resolves to $pb, outside $venv; not recording it" ;; esac
  case $ib in "$real"/*) ;; *) die "ibapi resolves to $ib, outside $venv; not recording it" ;; esac
  test "$version" = "$want" || die "protobuf $version in $venv, pinned $want; not recording it"
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
# environment, $adapter_venv, offline (--no-index, --no-deps: ibapi declares
# no dependencies, but imports protobuf, so the pinned pure-Python protobuf
# wheel is fetched and verified beside the installer, before anything is
# installed, and installed first), and only then writes the install record
# the adapter checks
# at start against the pin installed with its code (adapter/ibkr/ibapi_pin.py):
# a different or missing record and the adapter refuses to start.
twsapi_root=/opt/trend-investing/twsapi
adapter_venv=/opt/trend-investing/adapter-venv
adapter_root=/opt/trend-investing/adapter
step_twsapi() {
  log "TWS API ${TWSAPI_VERSION}"
  local installer pinned_sha=$TWSAPI_SHA256 artefact=installer
  if mac_mini; then
    artefact=archive
    installer="$dl_cache/twsapi-macunix-${TWSAPI_VERSION}.zip"
    pinned_sha=$TWSAPI_ARCHIVE_SHA256
    fetch "$TWSAPI_ARCHIVE_URL" "$TWSAPI_ARCHIVE_SHA256" "$installer"
  else
    installer="$dl_cache/tws-api-${TWSAPI_VERSION}-linux-x64.sh"
    fetch "$TWSAPI_URL" "$TWSAPI_SHA256" "$installer"
  fi
  # Both downloads are verified before any file is removed or installed.
  local protobuf="$dl_cache/protobuf-${PROTOBUF_VERSION}-py3-none-any.whl"
  fetch "$PROTOBUF_URL" "$PROTOBUF_SHA256" "$protobuf"
  if ! mac_mini && test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
    log "rehearsal: checksum verified, installer not executed (TI_REHEARSAL_SKIP_INSTALLER)"
    return 0
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
    # root, under $tree. -q: unattended; -dir: where to put the tree. The
    # install4j installer writes under HOME, so it gets a scratch one.
    local scratch
    scratch=$(mktemp -d)
    chmod 0755 "$scratch"
    install -d -o nobody -g nogroup -m 0700 "$scratch/home" "$scratch/tree"
    install -m 0644 "$installer" "$scratch/installer.sh"
    (cd / && runuser -u nobody -- env HOME="$scratch/home" sh "$scratch/installer.sh" -q -dir "$scratch/tree" >/dev/null)
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
  "$adapter_venv/bin/pip" install --quiet --no-index --no-deps "$protobuf"
  "$adapter_venv/bin/pip" install --quiet --no-index --no-deps --no-build-isolation "$client"
  verify_adapter_venv "$adapter_venv" "$PROTOBUF_VERSION"
  # Root's umask must not close the environment to the adapter's user.
  chmod -R u=rwX,go=rX "$adapter_venv"
  printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\nTWSAPI_ARTEFACT=%s\nPROTOBUF_VERSION=%s\nPROTOBUF_SHA256=%s\n' \
    "$TWSAPI_VERSION" "$pinned_sha" "$artefact" "$PROTOBUF_VERSION" "$PROTOBUF_SHA256" >"$twsapi_root/INSTALLED.tmp"
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
  # The NYSE calendar, the table of inputs the data job writes and the
  # reader of the engine's durable log, in the same layout, so the adapter
  # states the market's hours from the same committed calendar, refuses any
  # input type the data job does not write (delivery.py) and reads the log
  # exactly as the data job does (relay.py).
  install -d -m 0755 "$adapter_root.new/adapter/marketdata"
  install -m 0644 "$here/../adapter/marketdata/nyse_calendar.py" "$adapter_root.new/adapter/marketdata/"
  install -m 0644 "$here/../adapter/marketdata/kinds.py" "$adapter_root.new/adapter/marketdata/"
  install -m 0644 "$here/../adapter/marketdata/engine_log.py" "$adapter_root.new/adapter/marketdata/"
  install -m 0644 "$here/versions.env" "$adapter_root.new/versions.env"
  rm -rf "$adapter_root"
  mv "$adapter_root.new" "$adapter_root"
}

# write_stub PATH TEMPLATE: root-only, names only, created once when neither
# PATH nor its encrypted copy PATH.gpg exists, and never overwritten, so a
# re-run cannot erase what the owner entered. secrets_migrate then encrypts it.
write_stub() {
  local path=$1 template=$2
  if ! test -e "$path" && ! test -e "$path.gpg"; then
    install -m 0600 -o root -g root "$template" "$path"
    log "created $path (owner step: fill it in with sudo ti-secrets edit)"
  fi
  if test -e "$path"; then
    chmod 0600 "$path"
    chown root:root "$path"
  fi
}

# secrets_migrate: every secret file still in plain text encrypted, checked
# by decrypting it, then shredded (deploy/secrets.sh, "migrate"); a check
# that fails stops provisioning with the plain text kept.
secrets_migrate() { "$here/secrets.sh" migrate; }

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
  for b in ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start universe-rank; do
    test -f "$dist/$b" || continue
    got=$(elf_machine "$dist/$b")
    test "$got" = "$want" || die "$dist/$b is not a Linux program for this machine ($machine; its ELF machine is ${got:-none, not ELF}): build it with deploy/build-engine.sh for this machine and copy dist/ again; nothing installed"
  done
}

# install_engine_settings SRC DEST: installs the engines' settings from SRC
# into DEST. A settings file (*.env: client ID, mode, the adapter's account
# line name; no secret, no account) and a configuration (*.config, which
# names its account, which the owner substitutes on the server and never in
# the repository, and is hashed into its engine's run, ADR 0016) are each
# installed only when DEST has none: an existing one is kept byte for byte,
# an owner's edit included, and the shipped one is installed beside it as
# <name>.env.example or <name>.config.example, to compare; a line the
# shipped file adds is the owner's to copy (docs/runbooks/first-dry-run-day.md,
# "The Traditional IRA"; docs/live-trading/lessons-2026-10.md L7: an earlier
# provisioner copied the shipped configuration over the server's, resetting
# Boost's account to the placeholder).
install_engine_settings() {
  local src=$1 dest=$2 f name
  for f in "$src"/*.env; do
    name=$(basename "$f")
    if test -e "$dest/$name"; then
      install -m 0644 "$f" "$dest/$name.example"
      log "kept $dest/$name; the shipped one is $dest/$name.example (copy any line it adds by hand)"
    else
      install -m 0644 "$f" "$dest/$name"
    fi
  done
  for f in "$src"/*.config; do
    name=$(basename "$f")
    if test -e "$dest/$name"; then
      install -m 0644 "$f" "$dest/$name.example"
      log "kept $dest/$name, which names the server's account; the shipped one is $dest/$name.example"
    else
      install -m 0644 "$f" "$dest/$name"
      log "installed $dest/$name; name its account on the server (docs/runbooks/first-dry-run-day.md)"
    fi
  done
}

step_services() {
  local dist=${TI_DIST:-$here/../dist} b
  require_native_programs "$dist"
  log "directories and config"
  install -d -m 0755 "$etc" "$etc/engines" /usr/local/libexec/trend-investing /opt/trend-investing/bin
  install -m 0755 "$here"/libexec/*.sh /usr/local/libexec/trend-investing/
  install_engine_settings "$here/etc/engines" "$etc/engines"
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
  # The reporter, the dashboard, the owner's two tools and universe-rank (the
  # Go ranker the IRAs' data job runs: ti-data@.service), built for the
  # server beside ti-engine by deploy/build-engine.sh (the server has no Go)
  # and copied to dist/ beside deploy/. A missing one is named here, and its
  # unit fails when it runs, so the gap is seen. The owner records instructions
  # as `sudo -u ti-operator operator ...` and runs `sudo -u ti-trader
  # journal-query ...` and `sudo -u ti-trader operator approve ...`: sudo's
  # PATH includes /usr/local/bin, where each is linked.
  local dist=${TI_DIST:-$here/../dist} b
  for b in ti-reporter ti-dashboard operator journal-query ti-gateway-start universe-rank; do
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
  # Each IRA's file too, named only: its Flex reader's unit loads it as a
  # credential, and a missing file would end the start at systemd's credential
  # setup ("Failed to set up credentials") before anything says why; present
  # and empty, the reader names the line to fill in (ti-flex@.service).
  write_stub "$etc/accounts/trad-ira.env" "$here/etc/accounts/trad-ira.env.example"
  write_stub "$etc/accounts/roth-ira.env" "$here/etc/accounts/roth-ira.env.example"
  # The owner's secrets tools: ti-unlock after each boot, ti-secrets to edit
  # a file or change the passphrase (docs/manual/server.md).
  install -m 0755 "$here/secrets.sh" /usr/local/sbin/ti-secrets
  ln -sfn ti-secrets /usr/local/sbin/ti-unlock
  secrets_migrate
  if mac_mini; then ensure_operator_user 2003; else ensure_operator_user; fi
  install -d -o ti-trader -g ti-trader -m 0750 \
    /var/lib/trend-investing/data /var/lib/trend-investing/reports /var/lib/trend-investing/engines \
    /var/lib/trend-investing/flex
  local a
  for a in boost trad-ira roth-ira; do
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/engines/$a"
    # The operator inbox (ti-engine@.service passes it as -operator). The owner
    # records into it as ti-operator (sudo -u ti-operator operator ...); the
    # engine reads it through its group and cannot write it, and no other user
    # can (ADR 0026, kill-switch amendment; docs/manual/operator.md).
    install -d -o ti-operator -g ti-trader -m 0750 "/var/lib/trend-investing/engines/$a/operator"
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/adapters/$a"
    # The Flex reader's ledger of cash movements and its status file
    # (ti-flex@<instance>.service; docs/manual/reconciliation.md).
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/flex/$a"
  done
  # The adapter's configuration carries no secret (the account number comes
  # from accounts/boost.env): created once from the example, never overwritten, so
  # the owner's paper settings survive a re-run.
  # On the Mac mini the Gateway listens on the Mac, which the container
  # reaches as host.docker.internal; the adapter accepts any host name.
  install -d -m 0755 "$etc/adapters"
  if ! test -e "$etc/adapters/boost.json"; then
    if mac_mini; then
      sed 's/"host": "127.0.0.1"/"host": "host.docker.internal"/' "$here/etc/adapters/boost.json.example" >"$etc/adapters/boost.json.new"
    else
      cat "$here/etc/adapters/boost.json.example" >"$etc/adapters/boost.json.new"
    fi
    chmod 0644 "$etc/adapters/boost.json.new"
    mv "$etc/adapters/boost.json.new" "$etc/adapters/boost.json"
    log "created $etc/adapters/boost.json (owner step: set paper mode before the paper test)"
  fi
  # The Traditional IRA's adapter (ti-adapter@trad-ira): its own client ID
  # (12), the account snapshot the momentum engine reads (version 4) and a
  # contract and mark for every name it can decide (ADR 0026 points 2 and 3).
  # Created once, never overwritten, as Boost's is.
  if ! test -e "$etc/adapters/trad-ira.json"; then
    if mac_mini; then
      sed 's/"host": "127.0.0.1"/"host": "host.docker.internal"/' "$here/etc/adapters/trad-ira.json.example" >"$etc/adapters/trad-ira.json.new"
    else
      cat "$here/etc/adapters/trad-ira.json.example" >"$etc/adapters/trad-ira.json.new"
    fi
    chmod 0644 "$etc/adapters/trad-ira.json.new"
    mv "$etc/adapters/trad-ira.json.new" "$etc/adapters/trad-ira.json"
    log "created $etc/adapters/trad-ira.json (dry-run, sending off)"
  fi

  log "systemd units"
  # The reporter is one instance per engine (ti-reporter@<instance>); an
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
    if test -e "$unit_dir/ti-reporter.timer"; then
      systemctl disable ti-reporter.timer
    fi
  fi
  rm -f "$unit_dir/ti-reporter.service" "$unit_dir/ti-reporter.timer" \
    "$unit_dir/timers.target.wants/ti-reporter.timer"
  # On the Mac mini the Gateway's unit file is not installed: masking puts a
  # link to /dev/null in its place, which a real file there would block.
  local unit
  for unit in "$here"/systemd/*.service "$here"/systemd/*.timer; do
    if mac_mini && test "$(basename "$unit")" = ti-gateway.service; then continue; fi
    install -m 0644 "$unit" "$unit_dir/"
  done
  # The Traditional IRA's Flex reader fires at its own staggered times, 15
  # minutes after Boost's (docs/live-trading/lessons-2026-10.md L3): a drop-in
  # for its instance of the timer, replaced on every run as the units are.
  install -d -m 0755 "$unit_dir/ti-flex@trad-ira.timer.d"
  install -m 0644 "$here/systemd/ti-flex@trad-ira.timer.d/stagger.conf" "$unit_dir/ti-flex@trad-ira.timer.d/"
  if mac_mini && test -f "$unit_dir/ti-gateway.service" && ! test -L "$unit_dir/ti-gateway.service"; then
    rm -f "$unit_dir/ti-gateway.service"
  fi
  # On the Mac mini no time daemon runs in the container (step_clock), so the
  # clock is compared with a reference every 15 minutes too, not only when
  # provisioning runs (ADR 0019); a failed check is a failed unit.
  if mac_mini; then
    install -m 0644 "$here/mac-mini/systemd/ti-clock-check.service" "$here/mac-mini/systemd/ti-clock-check.timer" "$unit_dir/"
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
    # The Traditional IRA's data job too: until its engine's run.env exists
    # each firing is a quiet wait (ti-data@.service; docs/live-trading/
    # lessons-2026-10.md L8: a timer that should run unattended is enabled).
    systemctl enable --now ti-data.timer ti-data@trad-ira.timer ti-reporter@boost.timer
    if mac_mini; then
      systemctl enable --now ti-clock-check.timer
    fi
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
    # The adapters are started by the owner too, each after its engine and,
    # for an IRA, after its warm-up; never enabled: the engine is never
    # started at boot, so an adapter started then would only fail.
    systemctl disable ti-adapter-boost.service ti-adapter@trad-ira.service
    # The gateway starts only once the owner has filled
    # the login file; the unit's own check refuses an empty one. The data
    # step, the reporter and the dashboard fail, each with a message, until
    # their files are installed and the Boost engine's run.env exists; the
    # reporter also until secrets.env holds its channel secrets.
    if mac_mini; then
      log "enabled; started ti-data.timer, ti-data@trad-ira.timer, ti-reporter@boost.timer and ti-dashboard; ti-gateway is masked (the Gateway is the Mac app)"
    else
      log "enabled; started ti-data.timer, ti-data@trad-ira.timer, ti-reporter@boost.timer and ti-dashboard; start the gateway with: systemctl start ti-gateway (after the owner enters the IBKR login)"
    fi
  fi
}

# Replaces the whole UFW rule set: `ufw --force reset` removes every existing
# rule, including any added by hand, before the Tailscale-only policy is
# applied. Re-add a manual rule after each run, or add it here. The Gateway's
# API ports stay closed to the tailnet; only the server itself reaches them.
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
  # The Gateway's API ports (4002 paper, 4001 live) are for the server's own
  # engines and adapters on 127.0.0.1, which ufw does not filter. The denies
  # come first, so the tailscale0 allow below never reaches them; the reset
  # above keeps a rerun from stacking duplicates. This is a second layer, not
  # the guard: tailscaled's own input chain accepts tailscale0 traffic before
  # ufw's chains, so the Gateway refuses non-local API clients itself
  # (render-ibc-config.sh: AcceptIncomingConnectionAction=reject).
  ufw deny in proto tcp to any port 4001 >/dev/null
  ufw deny in proto tcp to any port 4002 >/dev/null
  ufw allow in on tailscale0 >/dev/null
  ufw --force enable >/dev/null
  log "also close port 22 in the Lightsail firewall tab (owner step)"
}

# The owner's own ntfy server, which takes the daily summary (it carries
# balances, so it never goes to the public ntfy.sh relay or to Pushover). It
# names ntfy.sh as its upstream, the relay the iOS app is woken through,
# which is told only a message's ID and a hash of its topic. It
# listens on this machine's Tailscale address and nowhere else: the address
# is read from Tailscale, or given as TI_NTFY_ADDRESS (the rehearsal), and
# must be a Tailscale one (100.64.0.0/10), so the server cannot be made to
# listen on a public interface by a mistake here. The server needs no
# secret: the topic the reporter publishes to is in secrets.env
# (TI_NTFY_TOPIC), and the server's URL is TI_NTFY_SERVER, which this step
# prints and does not write. The Mac mini has none: paper trading runs on the
# Lightsail box.
step_ntfy() {
  if mac_mini; then
    log "Mac mini: no ntfy server (the owner's server runs on the Lightsail box)"
    return 0
  fi
  log "ntfy ${NTFY_VERSION}"
  local tgz="$dl_cache/ntfy_${NTFY_VERSION}_linux_amd64.tar.gz" tmp addr
  fetch "$NTFY_URL" "$NTFY_SHA256" "$tgz"
  tmp=$(mktemp -d)
  tar -xzf "$tgz" -C "$tmp" --strip-components=1
  install -m 0755 "$tmp/ntfy" /usr/local/bin/ntfy
  rm -rf "$tmp"
  addr=${TI_NTFY_ADDRESS:-}
  if test -z "$addr" && command -v tailscale >/dev/null 2>&1; then
    addr=$(tailscale ip -4 2>/dev/null | head -n 1) || addr=
  fi
  if test -z "$addr"; then
    log "SKIPPED the ntfy server's configuration: Tailscale is not signed in yet (owner step: sudo tailscale up --ssh). Re-run 'provision.sh ntfy' after."
    return 0
  fi
  # 100.64.0.0/10: 100.64.x.x to 100.127.x.x, each of the last two octets
  # 0 to 255.
  local octet='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
  if ! [[ $addr =~ ^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.${octet}\.${octet}$ ]]; then
    die "'$addr' is not a Tailscale address (100.64.0.0/10); the ntfy server listens on the tailnet only, so it is not configured"
  fi
  ensure_user ti-ntfy /var/lib/ntfy
  install -d -m 0755 /etc/ntfy
  install -m 0644 /dev/stdin /etc/ntfy/server.yml <<CONF
# Written by provision.sh ntfy; rewritten at each run. Listens on the
# Tailscale address only. No sign-in is needed: only your own devices on the
# tailnet reach it, and the topic is the secret.
base-url: "http://${addr}:2586"
listen-http: "${addr}:2586"
cache-file: "/var/lib/ntfy/cache.db"
cache-duration: "12h"
auth-default-access: "read-write"
# The iOS app shows a self-hosted server's messages at once only when the
# server asks ntfy.sh to wake it: the request carries the message's ID and a
# hash of the topic, never the message or the topic itself, and the phone
# then fetches the message from this server over Tailscale.
upstream-base-url: "https://ntfy.sh"
CONF
  install -m 0644 "$here/systemd/ti-ntfy.service" "$unit_dir/"
  if test "$rehearsal" != 1; then
    systemctl daemon-reload
    systemctl enable ti-ntfy.service
    systemctl restart ti-ntfy.service
  fi
  log "ntfy listens on http://${addr}:2586 (the tailnet only). Owner step: with sudo ti-secrets edit secrets.env set TI_NTFY_SERVER=http://${addr}:2586 and an unguessable TI_NTFY_TOPIC, and subscribe to that topic on that server in the ntfy app on a phone that keeps Tailscale on (always-on VPN); then send a test message to check delivery end to end (docs/manual/server.md, \"Your own ntfy server\")."
}

# reown NAME OLD_UID [OLD_GID] [--force-id] (the Mac mini only): after a container
# provisioned before the service users had fixed IDs is re-created, its
# volumes' files still carry the old numbers. Each file in the volumes owned
# by OLD_UID goes to NAME's fixed UID, and each of group OLD_GID to its fixed
# GID, links themselves and not their targets, within each volume. Only
# once NAME is at its fixed ID, so the pass cannot hand files to a number no
# account holds. ti-operator has no group of its own (it is ti-trader's).
# An old ID that root (0) or a service user's fixed ID (2001 to 2003) holds
# is always refused: those files are that account's own (root's secret stubs
# and installed programs among them), and handing them to a service user
# would expose them. An old ID that only collides with another account the
# new image has (a system account its packages added, at the ID the old
# service user had) is refused too, naming the account, unless --force-id says
# the files at that ID are the old service user's: the pass only ever touches
# the provisioner's own volumes, so they are left-behind files.
step_reown() {
  mac_mini || die "reown is for the Mac mini's container only (TI_TARGET=mac-mini)"
  local name=${1:-} old_uid=${2:-} old_gid=${3:-} want arg
  local -a rest=()
  force_id=0
  for arg in "$@"; do
    if test "$arg" = --force-id; then force_id=1; else rest+=("$arg"); fi
  done
  set -- ${rest[@]+"${rest[@]}"}
  name=${1:-} old_uid=${2:-} old_gid=${3:-}
  case $name in
    ti-gateway) want=2001 ;;
    ti-trader) want=2002 ;;
    ti-operator) want=2003; test -z "$old_gid" || die "ti-operator has no group of its own; give only its old UID" ;;
    *) die "usage: provision.sh reown ti-gateway|ti-trader|ti-operator OLD_UID [OLD_GID] [--force-id]" ;;
  esac
  test "$(id -u "$name" 2>/dev/null || echo none)" = "$want" \
    || die "$name is not at its fixed UID $want in this container: run up.sh --recreate first, then reown"
  old_uid=$(reown_old_id UID passwd "$old_uid") || exit 1
  if test -n "$old_gid"; then old_gid=$(reown_old_id GID group "$old_gid") || exit 1; fi
  local root owned=0 grouped=0 n
  for root in "${volume_roots[@]}"; do
    test -d "$root" || continue
    # find's own failure fails the pipeline (pipefail), so a listing that
    # stopped part way is reported, never counted as nothing left to re-own.
    n=$(find "$root" -xdev -uid "$old_uid" -print0 | reown_listed chown "$want") \
      || die "could not list or re-own the files of UID $old_uid under $root (see find's message above); some of them may have been changed already, and running reown again is safe once the cause is fixed"
    owned=$((owned + n))
    if test -n "$old_gid"; then
      n=$(find "$root" -xdev -gid "$old_gid" -print0 | reown_listed chgrp "$want") \
        || die "could not list or re-group the files of GID $old_gid under $root (see find's message above); some of them may have been changed already, and running reown again is safe once the cause is fixed"
      grouped=$((grouped + n))
    fi
  done
  log "re-owned $owned file(s) from UID $old_uid to $name ($want)${old_gid:+, and $grouped from GID $old_gid to $want}"
}

# reown_old_id KIND DB ID: prints ID as a plain number (leading zeros taken
# out), or dies if it is not a number up to 4294967294 or if root, a fixed
# service ID (2001 to 2003) holds it now, or if another entry of DB (passwd or
# group) holds it and force_id is not 1 (when it is, the holder is named and
# the ID accepted).
reown_old_id() {
  local kind=$1 db=$2 id=$3 holder
  case $id in '' | *[!0-9]*) die "the old $kind must be a number, not '$id'" ;; esac
  id=${id#"${id%%[!0]*}"}
  id=${id:-0}
  test "${#id}" -le 10 && test "$id" -le 4294967294 || die "the old $kind must be at most 4294967294, not '$3'"
  case $id in
    0 | 2001 | 2002 | 2003) die "the old $kind $id is held now (by root or a service user's fixed ID); its files are that account's own, not files left behind" ;;
  esac
  if holder=$(getent "$db" "$id"); then
    if test "${force_id:-0}" != 1; then
      die "the old $kind $id is held now by '${holder%%:*}' in this container, which the new image may have given that number; if the files at $id under /var/lib/trend-investing, /opt/trend-investing and /etc/trend-investing are the old service user's, run the same reown again with --force-id to move them, otherwise remove or renumber '${holder%%:*}' in the image and run up.sh --recreate"
    fi
    log "the old $kind $id is held by '${holder%%:*}' in this container; --force-id: its files in the volumes are taken as the old service user's" >&2
  fi
  printf '%s\n' "$id"
}

# reown_listed CMD ID: runs CMD -h ID on each NUL-separated path read from
# its input, and prints how many.
reown_listed() {
  local f n=0
  while IFS= read -r -d '' f; do
    "$1" -h "$2" "$f" || return 1
    n=$((n + 1))
  done
  printf '%s\n' "$n"
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
    ntfy) step_ntfy ;;
    reown) shift; step_reown "$@" ;;
    all) step_base; step_clock; step_gateway; step_services; step_firewall ;;
    *) die "usage: provision.sh base|clock|gateway|twsapi|adapter|services|firewall|ntfy|all, or reown NAME OLD_UID [OLD_GID] [--force-id] on the Mac mini" ;;
  esac
  log "done: ${1}"
}

# Run as a program; sourced (by deploy/rehearsal/test-target.sh), it only
# defines the steps.
if test "${BASH_SOURCE[0]}" = "$0"; then
  main "$@"
fi
