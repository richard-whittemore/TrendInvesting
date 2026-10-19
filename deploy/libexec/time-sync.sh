#!/bin/bash
# Makes sure a time daemon is running and gives it time to synchronise (ADR 0019:
# a clock behind IBKR's withholds every sell). A daemon already running (chrony,
# ntp, ntpsec, openntpd or systemd-timesyncd), running or installed but stopped,
# is kept, enabled and started: installing systemd-timesyncd would remove it.
# Only where none is installed is systemd-timesyncd installed, enabled and started.
# It does not judge the result; clock-check.sh does.
#
#   TI_CLOCK_WAIT_TRIES   reads of timedatectl before giving up (default 30)
#   TI_CLOCK_WAIT_SLEEP   seconds between reads (default 2)
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log() { printf 'time-sync: %s\n' "$*"; }

container_markers=${TI_CLOCK_CONTAINER_MARKERS-/.dockerenv /run/.containerenv}
in_container() {
  systemd-detect-virt --container --quiet 2>/dev/null && return 0
  local marker
  for marker in $container_markers; do test -e "$marker" && return 0; done
  return 1
}
# The unit is what systemctl needs; the display name is for messages.
display() { case $1 in chronyd) echo chrony ;; ntpd) echo ntp ;; *) echo "$1" ;; esac; }
# A systemd-timesyncd that cannot start is normal in a container (the host
# keeps time; clock-check.sh decides) and fatal on a VM, on every run.
timesyncd_start_failed() {
  if in_container; then
    log "systemd-timesyncd did not start (normal in a container: the host keeps time)"
    return 0
  fi
  printf 'time-sync: systemd-timesyncd would not start. Check "systemctl status systemd-timesyncd", then run this again.\n' >&2
  exit 1
}

status=0
running=$("$here/time-daemon.sh") || status=$?
case $status in
  0)
    log "keeping the running time daemon ($(display "$running")); enabling it for the next boot"
    systemctl enable "$running.service" || log "could not enable $(display "$running")"
    ;;
  2)
    log "$(display "$running") is installed but not running; starting it rather than replacing it"
    systemctl enable "$running.service" || log "could not enable $(display "$running")"
    systemctl start "$running.service" || {
      if test "$running" = systemd-timesyncd; then
        timesyncd_start_failed
      else
        printf 'time-sync: %s is installed but would not start. Start it ("systemctl status %s") or remove it, then run this again.\n' "$(display "$running")" "$running" >&2
        exit 1
      fi
    }
    ;;
  1)
    log "no time daemon is installed; installing systemd-timesyncd"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends systemd-timesyncd >/dev/null
    systemctl enable systemd-timesyncd.service
    systemctl start systemd-timesyncd.service || timesyncd_start_failed
    ;;
  *)
    printf 'time-sync: could not tell which time daemon is installed (time-daemon.sh exited %s); not installing one over it.\n' "$status" >&2
    exit 1
    ;;
esac
tries=${TI_CLOCK_WAIT_TRIES:-30}
for _ in $(seq 1 "$tries"); do
  test "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" = yes && break
  sleep "${TI_CLOCK_WAIT_SLEEP:-2}"
done
