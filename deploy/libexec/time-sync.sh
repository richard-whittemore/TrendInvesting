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

status=0
running=$("$here/time-daemon.sh") || status=$?
case $status in
  0)
    log "keeping the running time daemon ($running); enabling it for the next boot"
    systemctl enable "$running.service" || log "could not enable $running"
    ;;
  2)
    log "$running is installed but not running; starting it rather than replacing it"
    systemctl enable "$running.service" || log "could not enable $running"
    systemctl start "$running.service" || {
      printf 'time-sync: %s is installed but would not start. Start it ("systemctl status %s") or remove it, then run this again.\n' "$running" "$running" >&2
      exit 1
    }
    ;;
  *)
    log "no time daemon is installed; installing systemd-timesyncd"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends systemd-timesyncd >/dev/null
    systemctl enable systemd-timesyncd.service
    systemctl start systemd-timesyncd.service || log "systemd-timesyncd did not start (normal in a container: the host keeps time)"
    ;;
esac
tries=${TI_CLOCK_WAIT_TRIES:-30}
for _ in $(seq 1 "$tries"); do
  test "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" = yes && break
  sleep "${TI_CLOCK_WAIT_SLEEP:-2}"
done
