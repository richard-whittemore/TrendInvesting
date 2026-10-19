#!/bin/bash
# Names the time-syncing daemon the server already has (chrony, ntpsec, ntp,
# openntpd or systemd-timesyncd).
#   exit 0: that daemon is running (its systemd unit name is printed, without .service).
#   exit 2: none is running but one is installed (its unit name is printed).
#   exit 1: none is installed (nothing is printed).
# time-sync.sh keeps a daemon it finds, started if need be, rather than
# replacing it: installing systemd-timesyncd would remove the others.
set -euo pipefail
units="chrony chronyd ntpsec ntp ntpd openntpd systemd-timesyncd"
for unit in $units; do
  if systemctl is-active --quiet "$unit" 2>/dev/null; then
    echo "$unit"
    exit 0
  fi
done
for unit in $units; do
  if test -n "$(systemctl list-unit-files --no-legend "$unit.service" 2>/dev/null)"; then
    echo "$unit"
    exit 2
  fi
done
exit 1
