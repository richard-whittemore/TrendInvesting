#!/bin/bash
# Tests for deploy/libexec/clock-check.sh, the server's clock check (ADR 0019:
# a negative snapshot age, the server's clock behind IBKR's, is stale with no
# tolerance). timedatectl, systemd-detect-virt and curl are stand-ins on PATH.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check="$here/../libexec/clock-check.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
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
test -n "${FAKE_SYNC:-}" || exit 1
echo "$FAKE_SYNC"
STUB
# systemd-detect-virt --container: succeeds when FAKE_CONTAINER=1.
cat >"$stubs/systemd-detect-virt" <<'STUB'
#!/bin/bash
test "${FAKE_CONTAINER:-0}" = 1
STUB
# curl -sI URL: a response whose Date header is the real time plus FAKE_OFFSET
# seconds, or a failure when FAKE_OFFLINE=1.
cat >"$stubs/curl" <<'STUB'
#!/bin/bash
test "${FAKE_OFFLINE:-0}" != 1 || exit 6
epoch=$(( $(date +%s) + ${FAKE_OFFSET:-0} ))
printf 'HTTP/2 200\r\nDate: %s\r\nServer: x\r\n\r\n' "$(python3 -I -c 'import email.utils,sys;print(email.utils.formatdate(int(sys.argv[1]),usegmt=True))' "$epoch")"
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

test "$failures" -eq 0 || exit 1
echo "clock tests ok"
