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
#   TI_CLOCK_MAX_ROUNDTRIP_SECONDS  longest request accepted as a measurement (default 2)
#   TI_CLOCK_VERIFY             1: compare with a reference even when synchronised
#   TI_CLOCK_REFERENCES          space-separated HTTPS URLs, tried in order
#   TI_CLOCK_CONFIG             file of KEY=value lines (default /etc/trend-investing/clock.conf)
#
# sudo drops the caller's environment, so the two limits can also be set in the
# config file, which is read as data (never sourced) and which the environment
# overrides:
#   TI_CLOCK_MAX_ROUNDTRIP_SECONDS=5
#   TI_CLOCK_TOLERANCE_SECONDS=2
set -euo pipefail
set +x

config=${TI_CLOCK_CONFIG-/etc/trend-investing/clock.conf}
setting() { # KEY default -> the environment's value, else the config file's last one, else the default
  local env_value=${!1-} file_value=
  if test -n "$env_value"; then printf '%s' "$env_value"; return; fi
  if test -n "$config" && test -r "$config"; then
    file_value=$(sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$config" | tail -n 1 | sed 's/[[:space:]]*$//')
  fi
  printf '%s' "${file_value:-$2}"
}

tolerance=$(setting TI_CLOCK_TOLERANCE_SECONDS 2)
references=${TI_CLOCK_REFERENCES:-https://www.cloudflare.com https://www.google.com https://www.apple.com}
container_markers=${TI_CLOCK_CONTAINER_MARKERS-/.dockerenv /run/.containerenv}

say() { printf 'clock: %s\n' "$*"; }
refuse() { printf 'clock: %s\n' "$*" >&2; exit 1; }
case $tolerance in '' | *[!0-9]*) refuse "TI_CLOCK_TOLERANCE_SECONDS must be whole seconds, not '$tolerance'" ;; esac

max_roundtrip=$(setting TI_CLOCK_MAX_ROUNDTRIP_SECONDS 2)
# A finite positive number: digits with an optional fraction, not zero, and short
# enough that it cannot overflow to infinity.
case $max_roundtrip in
  '' | *[!0-9.]* | *.*.* | . | .* | *. | ???????????*) refuse "TI_CLOCK_MAX_ROUNDTRIP_SECONDS must be a positive number of seconds, not '$max_roundtrip'" ;;
esac
case ${max_roundtrip//[0.]/} in '') refuse "TI_CLOCK_MAX_ROUNDTRIP_SECONDS must be a positive number of seconds, not '$max_roundtrip'" ;; esac
verify=${TI_CLOCK_VERIFY:-0}

synchronised=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
# In a container NTPSynchronized=yes describes the host kernel's status. That is
# accepted, because a container shares the host's clock; TI_CLOCK_VERIFY=1
# still compares with an outside reference so the owner can check the claim.
if test "$synchronised" = yes; then
  if test "$verify" != 1; then
    say "ok, synchronised by the system's NTP service"
    exit 0
  fi
  say "synchronised by the system's NTP service; checking against a reference (TI_CLOCK_VERIFY=1)"
  in_container=1
fi

in_container=${in_container:-0}
systemd-detect-virt --container --quiet 2>/dev/null && in_container=1
for marker in $container_markers; do test -e "$marker" && in_container=1; done
if test "$in_container" = 0; then
  refuse "not synchronised: timedatectl does not report NTPSynchronized=yes (got '${synchronised:-no answer}'). Check 'timedatectl status' and 'systemctl status systemd-timesyncd'. While it is wrong the engine withholds every sell."
fi

# Compare with a reference. The local clock is read immediately before and
# after the request and compared with the reference Date (whole seconds, so the
# true time is taken as the header plus half a second) at the midpoint, so the
# request's latency does not bias the skew. A measurement whose round trip
# exceeds TI_CLOCK_MAX_ROUNDTRIP_SECONDS is rejected and the next reference
# tried. One python invocation does the whole measurement.
# A cache between here and the site may answer with an old Date and say how old
# in an Age header (a CDN; one asked for a fresh copy normally answers without
# Age or with Age 0, and then nothing is added). The Age is added to the Date
# only when it is a whole number of at most an hour; any other Age makes the
# reference unusable.
measure() { # url -> "skew" on stdout, or a failure with the reason on stdout
  python3 -I - "$1" "$max_roundtrip" <<'PY'
import email.utils, subprocess, sys, time
url, bound = sys.argv[1], float(sys.argv[2])
before = time.time()
if not url.startswith("https://"):
    print("not HTTPS"); sys.exit(4)
try:
    # A fresh response is asked for; a cache that answers anyway says how old
    # the Date is in an Age header, which is added back below.
    out = subprocess.run(["curl", "-sI", "--proto", "=https", "-H", "Cache-Control: no-cache", "--max-time", "5", url], capture_output=True, text=True, timeout=10).stdout
except Exception:
    print("unreachable"); sys.exit(2)
after = time.time()
fields = {l.split(":", 1)[0].strip().lower(): l.split(":", 1)[1].strip() for l in out.splitlines() if ":" in l}
header = fields.get("date", "")
age_text = fields.get("age", "")
if age_text and not (age_text.isascii() and age_text.isdigit() and int(age_text) <= 3600):
    print("unusable Age header '%s'" % age_text[:20]); sys.exit(5)
age = int(age_text) if age_text else 0
try:
    reference = email.utils.parsedate_to_datetime(header).timestamp() + age + 0.5
except Exception:
    print("unreachable"); sys.exit(2)
if after - before > bound:
    print("round trip %.1fs exceeds %gs" % (after - before, bound)); sys.exit(3)
print(round((before + after) / 2 - reference))
PY
}

reasons=
refused_plain=
badage=
for url in $references; do
  status=0
  result=$(measure "$url") || status=$?
  if test "$status" -ne 0; then
    test "$status" -eq 3 && reasons="$reasons $url: $result;"
    test "$status" -eq 5 && badage="$badage $url ($result);"
    test "$status" -eq 4 && refused_plain="$refused_plain $url"
    continue
  fi
  skew=$result
  magnitude=${skew#-}
  if test "$magnitude" -gt "$tolerance"; then
    refuse "skew of ${skew}s against $url (this clock minus the reference), more than the ${tolerance}s tolerance. The host's clock is wrong: turn on automatic date and time on the host. While it is wrong the engine may withhold every sell."
  fi
  say "ok, skew ${skew}s against $url, within ${tolerance}s"
  exit 0
done
refuse "no reference clock could be measured (tried: $references).${reasons:+ Rejected, round trip too long:$reasons To allow a slower link, put TI_CLOCK_MAX_ROUNDTRIP_SECONDS=5 on a line of /etc/trend-investing/clock.conf (the sudo-proof way), or set it in the environment of a run by hand.}${badage:+ Not used, unusable Age header:$badage}${refused_plain:+ Not used, since only HTTPS references count as evidence:$refused_plain.} Check the network, then run this again."
