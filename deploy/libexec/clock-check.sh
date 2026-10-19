#!/bin/bash
# Checks that the server's clock keeps time. ADR 0019 treats a negative
# broker-snapshot age (this clock behind IBKR's) as stale with no tolerance,
# so a clock that drifts behind withholds every sell (stale-broker-snapshot).
#
# On a VM the clock must be synchronised by an NTP service: timedatectl must
# report NTPSynchronized=yes. In a container the host keeps time and
# timedatectl may report no NTP service; there the clock is compared with the
# Date header of an HTTPS reference and must be within
# TI_CLOCK_TOLERANCE_SECONDS (default 2) of it. The header is whole seconds,
# so the measurement is good to about a second; a container that cannot
# reach any reference fails, because an unchecked clock is not a checked one.
#
# Run it by hand any time: /usr/local/libexec/trend-investing/clock-check.sh
# Exit 0: the clock is good. Exit 1: it is not, and the reason is on stderr.
#
#   TI_CLOCK_TOLERANCE_SECONDS   container tolerance in whole seconds (default 2)
#   TI_CLOCK_CONTAINER_MARKERS  files whose presence means a container, besides systemd-detect-virt
#   TI_CLOCK_REFERENCES          space-separated HTTPS URLs, tried in order
set -euo pipefail
set +x

tolerance=${TI_CLOCK_TOLERANCE_SECONDS:-2}
references=${TI_CLOCK_REFERENCES:-https://www.cloudflare.com https://www.google.com https://www.apple.com}
container_markers=${TI_CLOCK_CONTAINER_MARKERS-/.dockerenv /run/.containerenv}

say() { printf 'clock: %s\n' "$*"; }
refuse() { printf 'clock: %s\n' "$*" >&2; exit 1; }
case $tolerance in '' | *[!0-9]*) refuse "TI_CLOCK_TOLERANCE_SECONDS must be whole seconds, not '$tolerance'" ;; esac

synchronised=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
if test "$synchronised" = yes; then
  say "ok, synchronised by the system's NTP service"
  exit 0
fi

in_container=0
systemd-detect-virt --container --quiet 2>/dev/null && in_container=1
for marker in $container_markers; do test -e "$marker" && in_container=1; done
if test "$in_container" = 0; then
  refuse "not synchronised: timedatectl does not report NTPSynchronized=yes (got '${synchronised:-no answer}'). Check 'timedatectl status' and 'systemctl status systemd-timesyncd'. While it is wrong the engine withholds every sell."
fi

# In a container: compare with a reference.
for url in $references; do
  header=$(curl -sI --max-time 5 "$url" 2>/dev/null | tr -d '\r' | grep -i '^date:' | head -n 1 || true)
  test -n "$header" || continue
  reference=$(python3 -I -c 'import email.utils,sys;print(int(email.utils.parsedate_to_datetime(sys.argv[1]).timestamp()))' "${header#*: }" 2>/dev/null) || continue
  skew=$(($(date +%s) - reference))
  magnitude=${skew#-}
  if test "$magnitude" -gt "$tolerance"; then
    refuse "skew of ${skew}s against $url (this clock minus the reference), more than the ${tolerance}s tolerance. The host's clock is wrong: turn on automatic date and time on the host. While it is wrong the engine may withhold every sell."
  fi
  say "ok, in a container: skew ${skew}s against $url, within ${tolerance}s"
  exit 0
done
refuse "in a container with no NTP service, and no reference clock could be read (tried: $references). Check the network, then run this again."
