#!/bin/bash
# Makes sure a time daemon is running and gives it time to synchronise (ADR 0019:
# a clock behind IBKR's withholds every sell). A daemon already running (chrony,
# ntp, ntpsec or systemd-timesyncd) is kept: installing systemd-timesyncd would
# remove it. Otherwise systemd-timesyncd is installed, enabled and started.
# It does not judge the result; clock-check.sh does.
#
#   TI_CLOCK_WAIT_TRIES   reads of timedatectl before giving up (default 30)
#   TI_CLOCK_WAIT_SLEEP   seconds between reads (default 2)
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
log() { printf 'time-sync: %s\n' "$*"; }

if running=$("$here/time-daemon.sh"); then
  log "keeping the running time daemon ($running)"
else
  log "no time daemon is running; installing systemd-timesyncd"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends systemd-timesyncd >/dev/null
  systemctl enable systemd-timesyncd.service
  systemctl start systemd-timesyncd.service || log "systemd-timesyncd did not start (normal in a container: the host keeps time)"
fi
tries=${TI_CLOCK_WAIT_TRIES:-30}
for _ in $(seq 1 "$tries"); do
  test "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" = yes && break
  sleep "${TI_CLOCK_WAIT_SLEEP:-2}"
done
