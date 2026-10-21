#!/bin/bash
# Tests for deploy/libexec/clock-check.sh, the server's clock check (ADR 0019:
# a negative snapshot age, the server's clock behind IBKR's, is stale with no
# tolerance). timedatectl, systemd-detect-virt and curl are stand-ins on PATH.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check="$here/../libexec/clock-check.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The caller's environment must not decide any case.
for v in $(compgen -e | grep -E '^(TI_CLOCK|FAKE)_' || true); do unset "$v"; done
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}

stubs="$work/bin"
mkdir -p "$stubs"
# timedatectl show -p NTPSynchronized --value: answers $FAKE_SYNC (unset: no systemd).
cat >"$stubs/timedatectl" <<'STUB'
#!/bin/bash
if test -n "${FAKE_SYNC_AFTER:-}"; then
  n=$(cat "$FAKE_COUNT" 2>/dev/null || echo 0); echo $((n + 1)) >"$FAKE_COUNT"
  test "$n" -ge "$FAKE_SYNC_AFTER" && echo yes || echo no
  exit 0
fi
test -n "${FAKE_SYNC:-}" || exit 1
echo "$FAKE_SYNC"
STUB
# systemd-detect-virt --container: succeeds when FAKE_CONTAINER=1.
cat >"$stubs/systemd-detect-virt" <<'STUB'
#!/bin/bash
test "${FAKE_CONTAINER:-0}" = 1
STUB
# curl -sI URL: a response whose Date header is the real time plus FAKE_OFFSET
# seconds, or a failure when FAKE_OFFLINE=1. FAKE_DELAY seconds is the round
# trip (the server stamps the Date halfway through it); a URL containing
# "slow" takes FAKE_SLOW_DELAY instead.
cat >"$stubs/curl" <<'STUB'
#!/bin/bash
test "${FAKE_OFFLINE:-0}" != 1 || exit 6
delay=${FAKE_DELAY:-0}
case "${*: -1}" in *slow*) delay=${FAKE_SLOW_DELAY:-$delay} ;; esac
sleep "$(python3 -c "print($delay/2)")"
epoch=$(( $(date +%s) + ${FAKE_OFFSET:-0} ))
epoch=$((epoch - ${FAKE_AGE:-0}))
age=; test -z "${FAKE_AGE:-}" || age="Age: $FAKE_AGE"$'\r\n'
raw=${FAKE_AGE_RAW:-}
case "${*: -1}" in *"${FAKE_BADAGE_URL:-@@}"*) ;; *) test -z "${FAKE_BADAGE_URL:-}" || raw= ;; esac
test -z "$raw" || age="Age: $raw"$'\r\n'
printf 'HTTP/2 200\r\nDate: %s\r\n%sServer: x\r\n\r\n' "$(python3 -I -c 'import email.utils,sys;print(email.utils.formatdate(int(sys.argv[1]),usegmt=True))' "$epoch")" "$age"
sleep "$(python3 -c "print($delay/2)")"
STUB
# systemctl is-active --quiet UNIT: succeeds for the units listed in FAKE_ACTIVE;
# list-unit-files UNIT.service names the units in FAKE_INSTALLED; start UNIT.service
# fails for those in FAKE_START_FAIL.
cat >"$stubs/systemctl" <<'STUB'
#!/bin/bash
echo "systemctl $*" >>"${FAKE_LOG:-/dev/null}"
if test "$1" = list-unit-files; then
  for unit in ${FAKE_INSTALLED:-}; do test "$unit.service" = "${*: -1}" && echo "$unit.service enabled"; done
  exit 0
fi
if test "$1" = start; then
  for unit in ${FAKE_START_FAIL:-}; do test "$unit.service" = "${*: -1}" && exit 1; done
  exit 0
fi
test "$1" = is-active || exit 0
for unit in ${FAKE_ACTIVE:-}; do test "$unit" = "${*: -1}" && exit 0; done
exit 3
STUB
cat >"$stubs/apt-get" <<'STUB'
#!/bin/bash
echo "apt-get $*" >>"${FAKE_LOG:-/dev/null}"
STUB
chmod +x "$stubs"/*

run() { # env assignments...; prints the exit status
  local status=0
  env PATH="$stubs:$PATH" TI_CLOCK_CONTAINER_MARKERS= TI_CLOCK_CONFIG="$work/none.conf" "$@" "$check" >"$work/out" 2>"$work/err" || status=$?
  echo "$status"
}

expect "a VM synchronised by NTP passes" 0 "$(run FAKE_SYNC=yes FAKE_CONTAINER=0)"
expect "a VM not synchronised fails, whatever an outside clock says" 1 "$(run FAKE_SYNC=no FAKE_CONTAINER=0)"
grep -q 'not synchronised' "$work/err" || expect "the VM refusal says so" yes no
expect "a VM with no timedatectl answer fails" 1 "$(run FAKE_CONTAINER=0)"

expect "a container whose host is synchronised passes (no NTP service shown)" 0 "$(run FAKE_SYNC=no FAKE_CONTAINER=1 FAKE_OFFSET=0)"
expect "a container with the clock 1 second off passes" 0 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=-1)"
expect "a container with the clock 1 minute behind fails" 1 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=60)"
grep -q 'skew' "$work/err" || expect "the container refusal names the skew" yes no
expect "a container with the clock 1 minute ahead fails" 1 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=-60)"
expect "a container that cannot reach a reference fails closed" 1 "$(run FAKE_CONTAINER=1 FAKE_OFFLINE=1)"
expect "a wider tolerance admits a 5 second skew" 0 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=5 TI_CLOCK_TOLERANCE_SECONDS=10)"
expect "the default tolerance refuses a 5 second skew" 1 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=5)"

# The output contract the reporter reads (the script's header, "Output"): a
# measured skew is "skew <N>s" on a pass and "skew of <N>s" on a failure, and
# a failure with no measurement begins "clock: not synchronised" or
# "clock: no reference clock could be measured".
run FAKE_CONTAINER=1 FAKE_OFFSET=60 >/dev/null
grep -Eq '^clock: skew of -[0-9]+s against ' "$work/err" || expect "a refused skew is 'clock: skew of <N>s against'" yes "$(cat "$work/err")"
run FAKE_CONTAINER=1 FAKE_OFFSET=0 >/dev/null
grep -Eq '^clock: ok, skew -?[0-9]+s against ' "$work/out" || expect "a passing skew is 'clock: ok, skew <N>s against'" yes "$(cat "$work/out")"
run FAKE_CONTAINER=1 FAKE_OFFLINE=1 >/dev/null
grep -q '^clock: no reference clock could be measured' "$work/err" || expect "no reference reads 'clock: no reference clock could be measured'" yes "$(cat "$work/err")"
run FAKE_SYNC=no FAKE_CONTAINER=0 >/dev/null
grep -q '^clock: not synchronised' "$work/err" || expect "a VM refusal reads 'clock: not synchronised'" yes "$(cat "$work/err")"

# The request's own latency must not bias the skew: the local clock is read
# either side of the request and compared with the reference at the midpoint.
expect "a 3 second round trip does not bias a 1 second tolerance" 0 \
  "$(run FAKE_CONTAINER=1 FAKE_DELAY=3 TI_CLOCK_TOLERANCE_SECONDS=1 TI_CLOCK_MAX_ROUNDTRIP_SECONDS=10)"
expect "a round trip over the bound is rejected, failing closed" 1 \
  "$(run FAKE_CONTAINER=1 FAKE_DELAY=2 TI_CLOCK_MAX_ROUNDTRIP_SECONDS=1)"
grep -q 'round trip' "$work/err" || expect "the refusal names the round trip" yes no
expect "a slow reference is skipped for the next one" 0 \
  "$(run FAKE_CONTAINER=1 FAKE_SLOW_DELAY=3 TI_CLOCK_MAX_ROUNDTRIP_SECONDS=2 TI_CLOCK_REFERENCES='https://slow.test https://fast.test')"

# NTPSynchronized=yes in a container is the host kernel's claim (the container
# shares the host clock) and is accepted; TI_CLOCK_VERIFY=1 also checks it
# against the outside reference.
expect "a container reporting synchronised passes without an outside check" 0 \
  "$(run FAKE_SYNC=yes FAKE_CONTAINER=1 FAKE_OFFSET=60)"
expect "TI_CLOCK_VERIFY=1 checks a synchronised claim and finds a 1 minute skew" 1 \
  "$(run FAKE_SYNC=yes FAKE_CONTAINER=1 FAKE_OFFSET=60 TI_CLOCK_VERIFY=1)"
grep -q 'skew' "$work/err" || expect "the verify refusal names the skew" yes no
expect "TI_CLOCK_VERIFY=1 passes when the outside clock agrees" 0 \
  "$(run FAKE_SYNC=yes FAKE_CONTAINER=1 FAKE_OFFSET=0 TI_CLOCK_VERIFY=1)"

# TI_CLOCK_MAX_ROUNDTRIP_SECONDS is validated like the tolerance.
for bad in abc -1 0 0.0 nan inf 1e400; do
  expect "a round-trip bound of '$bad' is refused" 1 "$(run FAKE_CONTAINER=1 TI_CLOCK_MAX_ROUNDTRIP_SECONDS="$bad")"
  grep -q 'TI_CLOCK_MAX_ROUNDTRIP_SECONDS' "$work/err" || expect "the refusal names the round-trip variable ('$bad')" yes no
done
expect "a fractional round-trip bound is accepted" 0 "$(run FAKE_CONTAINER=1 TI_CLOCK_MAX_ROUNDTRIP_SECONDS=2.5)"

# The bound can be raised in a way that survives sudo: a file the script reads.
printf '# slow home link\nTI_CLOCK_MAX_ROUNDTRIP_SECONDS=5\n' >"$work/clock.conf"
expect "a round trip of 3s over the default bound is refused" 1 "$(run FAKE_CONTAINER=1 FAKE_DELAY=3)"
grep -q '/etc/trend-investing/clock.conf' "$work/err" || expect "the round-trip refusal says how to raise the bound" yes no
expect "the config file raises the bound" 0 "$(run FAKE_CONTAINER=1 FAKE_DELAY=3 TI_CLOCK_CONFIG="$work/clock.conf")"
expect "the environment overrides the config file" 1 \
  "$(run FAKE_CONTAINER=1 FAKE_DELAY=3 TI_CLOCK_CONFIG="$work/clock.conf" TI_CLOCK_MAX_ROUNDTRIP_SECONDS=1)"
printf 'TI_CLOCK_MAX_ROUNDTRIP_SECONDS=oops\n' >"$work/bad.conf"
expect "a bad value in the config file is refused" 1 "$(run FAKE_CONTAINER=1 TI_CLOCK_CONFIG="$work/bad.conf")"

# The tolerance is bounded like the round-trip value: a finite positive whole
# number of seconds, at most 60, short; and a comparison that errors fails.
for bad in 0 61 99999999999999999999 1e3 -1 abc 0005000; do
  expect "a tolerance of '$bad' is refused" 1 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=3600 TI_CLOCK_TOLERANCE_SECONDS="$bad")"
  grep -q 'TI_CLOCK_TOLERANCE_SECONDS' "$work/err" || expect "the refusal names the tolerance variable ('$bad')" yes no
  grep -q 'ok, skew' "$work/out" && expect "a tolerance of '$bad' never reports ok" no yes
done
printf 'TI_CLOCK_TOLERANCE_SECONDS=99999999999999999999\n' >"$work/hugetol.conf"
expect "a huge tolerance in the config file is refused, not passed" 1 \
  "$(run FAKE_CONTAINER=1 FAKE_OFFSET=3600 TI_CLOCK_CONFIG="$work/hugetol.conf")"
expect "a tolerance of 60 is accepted" 0 "$(run FAKE_CONTAINER=1 FAKE_OFFSET=30 TI_CLOCK_TOLERANCE_SECONDS=60)"

# The Age header is checked: a non-negative whole number up to an hour.
expect "a negative Age makes the reference unusable" 1 "$(run FAKE_CONTAINER=1 FAKE_AGE_RAW=-30)"
expect "a non-numeric Age makes the reference unusable" 1 "$(run FAKE_CONTAINER=1 FAKE_AGE_RAW=abc)"
grep -q 'Age' "$work/err" || expect "the refusal names the Age header" yes no
expect "an enormous Age makes the reference unusable" 1 "$(run FAKE_CONTAINER=1 FAKE_AGE_RAW=999999)"
expect "a bad Age on one reference falls through to the next" 0 \
  "$(run FAKE_CONTAINER=1 FAKE_AGE_RAW=-5 TI_CLOCK_REFERENCES='https://slow.test https://fast.test' FAKE_BADAGE_URL=slow)"
expect "Age 0 is a fresh response and passes" 0 "$(run FAKE_CONTAINER=1 FAKE_AGE=0)"

# The time daemon already running (libexec/time-daemon.sh) is kept.
daemon() { env PATH="$stubs:$PATH" "$@" "$here/../libexec/time-daemon.sh" 2>/dev/null || true; }
expect "chrony running is detected" chrony "$(daemon FAKE_ACTIVE='chrony')"
expect "ntpsec running is detected" ntpsec "$(daemon FAKE_ACTIVE='ntpsec')"
expect "ntp running is detected" ntp "$(daemon FAKE_ACTIVE='ntp')"
expect "timesyncd running is detected" systemd-timesyncd "$(daemon FAKE_ACTIVE='systemd-timesyncd')"
expect "openntpd running is detected" openntpd "$(daemon FAKE_ACTIVE='openntpd')"
expect "no daemon running prints nothing" "" "$(daemon FAKE_ACTIVE='')"
# An installed daemon that is not running is still a daemon: exit 2, named.
expect "installed chrony, stopped, is named" chrony "$(daemon FAKE_ACTIVE='' FAKE_INSTALLED='chrony')"
expect "installed openntpd, stopped, is named" openntpd "$(daemon FAKE_ACTIVE='' FAKE_INSTALLED='openntpd')"
st=0; env PATH="$stubs:$PATH" FAKE_INSTALLED=ntp "$here/../libexec/time-daemon.sh" >/dev/null 2>&1 || st=$?
expect "installed but stopped exits 2" 2 "$st"
st=0; env PATH="$stubs:$PATH" "$here/../libexec/time-daemon.sh" >/dev/null 2>&1 || st=$?
expect "nothing installed exits 1" 1 "$st"

# A cached response: its Date is Age seconds old, so the reference is Date + Age.
expect "a cached response 30 seconds old (Age header) passes" 0 "$(run FAKE_CONTAINER=1 FAKE_AGE=30)"
# Only HTTPS references are evidence: an on-path attacker can rewrite plain HTTP.
expect "an http reference is refused" 1 "$(run FAKE_CONTAINER=1 TI_CLOCK_REFERENCES=http://example.test)"
grep -q 'HTTPS' "$work/err" || expect "the refusal names HTTPS" yes no
expect "an http reference is skipped for an https one" 0 \
  "$(run FAKE_CONTAINER=1 TI_CLOCK_REFERENCES='http://example.test https://example.test')"

# libexec/time-sync.sh: where no time daemon runs it installs, enables and
# starts systemd-timesyncd and waits for the first synchronisation; a daemon
# already running is kept.
sync_run() { # env assignments...; prints the stub call log
  : >"$work/log"; rm -f "$work/count"
  env PATH="$stubs:$PATH" FAKE_LOG="$work/log" FAKE_COUNT="$work/count" TI_CLOCK_WAIT_SLEEP=0 "$@" "$here/../libexec/time-sync.sh" >/dev/null 2>&1 || true
  cat "$work/log"
}
log_has() { grep -q "$2" <<<"$1" && echo yes || echo no; }
out=$(sync_run FAKE_ACTIVE= FAKE_SYNC=yes)
expect "no daemon: timesyncd is installed" yes "$(log_has "$out" 'apt-get install.*systemd-timesyncd')"
expect "no daemon: the package lists are refreshed first" yes "$(log_has "$out" 'apt-get update')"
expect "no daemon: timesyncd is enabled" yes "$(log_has "$out" 'systemctl enable systemd-timesyncd')"
expect "no daemon: timesyncd is started" yes "$(log_has "$out" 'systemctl start systemd-timesyncd')"
out=$(sync_run FAKE_ACTIVE=chrony FAKE_SYNC=yes)
expect "chrony running: nothing is installed" no "$(log_has "$out" 'apt-get install')"
expect "chrony running: it is enabled for the next boot" yes "$(log_has "$out" 'systemctl enable chrony')"
expect "chrony running: timesyncd is not enabled" no "$(log_has "$out" 'systemctl enable systemd-timesyncd')"
out=$(sync_run FAKE_ACTIVE=systemd-timesyncd FAKE_SYNC=yes)
expect "timesyncd running but disabled: it is enabled, not reinstalled" yes "$(log_has "$out" 'systemctl enable systemd-timesyncd')"
expect "timesyncd running: no install" no "$(log_has "$out" 'apt-get install')"
out=$(sync_run FAKE_ACTIVE= FAKE_INSTALLED=chrony FAKE_SYNC=yes)
expect "chrony installed but stopped: no install" no "$(log_has "$out" 'apt-get install')"
expect "chrony installed but stopped: it is started" yes "$(log_has "$out" 'systemctl start chrony')"
expect "chrony installed but stopped: it is enabled" yes "$(log_has "$out" 'systemctl enable chrony')"
out=$(sync_run FAKE_ACTIVE= FAKE_INSTALLED=openntpd FAKE_SYNC=yes)
expect "openntpd installed but stopped: no install" no "$(log_has "$out" 'apt-get install')"
sync_run FAKE_ACTIVE= FAKE_INSTALLED=chrony FAKE_START_FAIL=chrony FAKE_SYNC=yes >/dev/null
st=0; env PATH="$stubs:$PATH" FAKE_LOG="$work/log" FAKE_COUNT="$work/count" TI_CLOCK_WAIT_SLEEP=0 FAKE_ACTIVE= FAKE_INSTALLED=chrony FAKE_START_FAIL=chrony FAKE_SYNC=yes "$here/../libexec/time-sync.sh" >/dev/null 2>"$work/err" || st=$?
expect "an installed daemon that will not start is refused" 1 "$st"
grep -q 'chrony' "$work/err" || expect "the refusal names the daemon" yes no
expect "an installed daemon that will not start is not replaced" no "$(log_has "$(cat "$work/log")" 'apt-get install')"
sync_run FAKE_ACTIVE= FAKE_SYNC_AFTER=3 >/dev/null
expect "it waits until synchronised (4 reads)" 4 "$(cat "$work/count")"
sync_run FAKE_ACTIVE= FAKE_SYNC_AFTER=999 TI_CLOCK_WAIT_TRIES=5 >/dev/null
expect "it gives up after the wait (5 reads)" 5 "$(cat "$work/count")"

# The unit is started by its real name (chronyd.service, ntpd.service); the
# display name is for messages only.
expect "chronyd running is named by its unit" chronyd "$(daemon FAKE_ACTIVE='chronyd')"
expect "ntpd installed, stopped, is named by its unit" ntpd "$(daemon FAKE_ACTIVE='' FAKE_INSTALLED='ntpd')"
out=$(sync_run FAKE_ACTIVE= FAKE_INSTALLED=chronyd FAKE_SYNC=yes)
expect "only chronyd.service exists: it is started by that name" yes "$(log_has "$out" 'systemctl start chronyd.service')"
expect "only chronyd.service exists: it is enabled by that name" yes "$(log_has "$out" 'systemctl enable chronyd.service')"
out=$(sync_run FAKE_ACTIVE=ntpd FAKE_SYNC=yes)
expect "ntpd.service running: it is enabled by that name" yes "$(log_has "$out" 'systemctl enable ntpd.service')"

# Only exit 1 from time-daemon.sh means none is installed; any other status
# fails closed and never installs timesyncd over a possibly installed daemon.
stubdir="$work/lx"
mkdir -p "$stubdir"
cp "$here/../libexec/time-sync.sh" "$stubdir/"
for code in 126 127 3 139; do
  printf '#!/bin/bash\nexit %s\n' "$code" >"$stubdir/time-daemon.sh"
  chmod +x "$stubdir/time-daemon.sh" "$stubdir/time-sync.sh"
  : >"$work/log"; st=0
  env PATH="$stubs:$PATH" FAKE_LOG="$work/log" TI_CLOCK_WAIT_SLEEP=0 "$stubdir/time-sync.sh" >/dev/null 2>"$work/err" || st=$?
  expect "time-daemon.sh exiting $code fails closed" 1 "$st"
  grep -q "$code" "$work/err" || expect "the refusal names status $code" yes no
  expect "time-daemon.sh exiting $code installs nothing" no "$(log_has "$(cat "$work/log")" 'apt-get install')"
done

# A systemd-timesyncd that cannot start: tolerated in a container on every run
# (the clock check decides), fatal on a VM on every run.
sync_status() { # env assignments...; prints time-sync.sh's exit status
  : >"$work/log"; local st=0
  env PATH="$stubs:$PATH" FAKE_LOG="$work/log" FAKE_COUNT="$work/count" TI_CLOCK_WAIT_SLEEP=0 TI_CLOCK_WAIT_TRIES=1 TI_CLOCK_CONTAINER_MARKERS= "$@" "$here/../libexec/time-sync.sh" >/dev/null 2>"$work/err" || st=$?
  echo "$st"
}
expect "container, first run, timesyncd will not start: tolerated" 0 \
  "$(sync_status FAKE_ACTIVE= FAKE_CONTAINER=1 FAKE_START_FAIL=systemd-timesyncd)"
expect "container, re-run, timesyncd installed but will not start: tolerated" 0 \
  "$(sync_status FAKE_ACTIVE= FAKE_INSTALLED=systemd-timesyncd FAKE_CONTAINER=1 FAKE_START_FAIL=systemd-timesyncd)"
expect "VM, first run, timesyncd will not start: fatal" 1 \
  "$(sync_status FAKE_ACTIVE= FAKE_CONTAINER=0 FAKE_START_FAIL=systemd-timesyncd)"
expect "VM, re-run, timesyncd installed but will not start: fatal" 1 \
  "$(sync_status FAKE_ACTIVE= FAKE_INSTALLED=systemd-timesyncd FAKE_CONTAINER=0 FAKE_START_FAIL=systemd-timesyncd)"
expect "container, chrony that will not start is still fatal" 1 \
  "$(sync_status FAKE_ACTIVE= FAKE_INSTALLED=chrony FAKE_CONTAINER=1 FAKE_START_FAIL=chrony)"

# The clock step creates /etc/trend-investing before the check reads clock.conf.
grep -q 'install -d -m 0755 /etc/trend-investing' "$here/../provision.sh" || expect "step_clock creates /etc/trend-investing" yes no
clock_body=$(awk '/^step_clock\(\)/,/^}/' "$here/../provision.sh")
dir_line=$(printf '%s\n' "$clock_body" | grep -nF 'install -d -m 0755 /etc/trend-investing' | head -1 | cut -d: -f1)
check_line=$(printf '%s\n' "$clock_body" | grep -nF 'clock-check.sh" ||' | head -1 | cut -d: -f1)
order=no
if test -n "$dir_line" && test -n "$check_line" && test "$dir_line" -lt "$check_line"; then order=yes; fi
expect "inside step_clock the directory is created before the clock check" yes "$order"

# The reporter runs the installed check as ti-trader, the engine's user: the
# check and its directory are installed readable and executable by all.
grep -q 'install -d -m 0755 /usr/local/libexec/trend-investing' "$here/../provision.sh" || expect "step_clock creates the libexec directory 0755" yes no
# shellcheck disable=SC2016 # the literal text of provision.sh
grep -q 'install -m 0755 "$here"/libexec/clock-check.sh /usr/local/libexec/trend-investing/' "$here/../provision.sh" || expect "step_clock installs the check 0755" yes no

test "$failures" -eq 0 || exit 1
echo "clock tests ok"
