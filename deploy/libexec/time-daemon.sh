#!/bin/bash
# Prints the name of the time-syncing daemon that is running (chrony, ntpsec,
# ntp or systemd-timesyncd) and exits 0, or prints nothing and exits 1 when
# none is. provision.sh clock keeps a working daemon rather than replacing it
# with systemd-timesyncd.
set -euo pipefail
for unit in chrony chronyd ntpsec ntp ntpd systemd-timesyncd; do
  if systemctl is-active --quiet "$unit" 2>/dev/null; then
    case $unit in chronyd) echo chrony ;; ntpd) echo ntp ;; *) echo "$unit" ;; esac
    exit 0
  fi
done
exit 1
