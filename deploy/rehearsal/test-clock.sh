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
printf 'HTTP/2 200\r\nDate: %s\r\n%sServer: x\r\n\r\n' "$(python3 -I -c 'import email.utils,sys;print(email.utils.formatdate(int(sys.argv[1]),usegmt=True))' "$epoch")" "$age"
sleep "$(python3 -c "print($delay/2)")"
STUB
# systemctl is-active --quiet UNIT: succeeds for the units listed in FAKE_ACTIVE.
cat >"$stubs/systemctl" <<'STUB'
#!/bin/bash
echo "systemctl $*" >>"${FAKE_LOG:-/dev/null}"
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
  env PATH="$stubs:$PATH" TI_CLOCK_CONTAINER_MARKERS= "$@" "$check" >"$work/out" 2>"$work/err" || status=$?
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

# The time daemon already running (libexec/time-daemon.sh) is kept.
daemon() { env PATH="$stubs:$PATH" "$@" "$here/../libexec/time-daemon.sh" 2>/dev/null || true; }
expect "chrony running is detected" chrony "$(daemon FAKE_ACTIVE='chrony')"
expect "ntpsec running is detected" ntpsec "$(daemon FAKE_ACTIVE='ntpsec')"
expect "ntp running is detected" ntp "$(daemon FAKE_ACTIVE='ntp')"
expect "timesyncd running is detected" systemd-timesyncd "$(daemon FAKE_ACTIVE='systemd-timesyncd')"
expect "no daemon running prints nothing" "" "$(daemon FAKE_ACTIVE='')"

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
expect "chrony running: nothing is enabled" no "$(log_has "$out" 'systemctl enable')"
sync_run FAKE_ACTIVE= FAKE_SYNC_AFTER=3 >/dev/null
expect "it waits until synchronised (4 reads)" 4 "$(cat "$work/count")"
sync_run FAKE_ACTIVE= FAKE_SYNC_AFTER=999 TI_CLOCK_WAIT_TRIES=5 >/dev/null
expect "it gives up after the wait (5 reads)" 5 "$(cat "$work/count")"

test "$failures" -eq 0 || exit 1
echo "clock tests ok"
