#!/bin/bash
# shellcheck disable=SC2154  # probe, unlocked and the helpers come from inside-systemd.sh, which sources this
# Sourced by inside-systemd.sh once the secrets are unlocked (it uses that
# script's fail, pass, note, with_pass, unlocked, probe and reports). It
# runs the commands of docs/runbooks/first-dry-run-day.md, as written in that
# file, in the systemd container, against stand-ins: a synthetic Massive on
# loopback (massive-standin.py), the adapter's scripted fake gateway in place
# of ibapi and IB Gateway (gateway-adapter.py), and a clock moved to the
# evening of the start. The engine, the data job, the reporter, the dashboard
# and the adapter's own code are the built ones, run by their installed
# units. Nothing here is a real price, key or account.

doc=/first-dry-run-day.md
test -f "$doc" || fail "the runbook is not in the container at $doc"

# block KIND NAME: the fenced block after the marker "<!-- KIND: NAME -->".
block() {
  awk -v marker="<!-- $1: $2 -->" '
    $0 == marker { want = 1; next }
    want && /^```/ { if (inside) exit; inside = 1; next }
    inside { print }' "$doc"
}

# The owner: an ordinary login on the server, not root and not in
# ti-trader's group, with sudo as the Lightsail image gives its login. Every
# block of the runbook runs as this user, so a command that needs sudo and
# does not say so fails here as it would for the owner.
useradd --create-home --shell /bin/bash owner
printf 'owner ALL=(ALL) NOPASSWD:ALL\n' >/etc/sudoers.d/owner
chmod 0440 /etc/sudoers.d/owner

# run_block COMMANDS: runs them as the owner in a shell of their own that
# stops at the first failing command (bash -e), outside any errexit-
# suppressing context, so its exit status is the block's.
run_block() {
  (cd / && runuser -u owner -- bash -e -c "$1")
}

# The runner's own checks, before it is trusted with the runbook: a failing
# command ends a block, the owner is unprivileged, and a ti-trader-only path
# is refused without sudo and read with it.
status=0
selftest=$(run_block 'false; echo after-the-failure' 2>&1) || status=$?
test "$status" != 0 || fail "run_block let a block go on, and exit 0, after a failing command"
! grep -q after-the-failure <<<"$selftest" || fail "run_block ran a command after a failing one"
test "$(run_block 'id -un')" = owner || fail "the runbook's blocks do not run as the owner"
install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/engines/selftest
status=0
run_block 'ls /var/lib/trend-investing/engines/selftest' >/dev/null 2>&1 || status=$?
test "$status" != 0 || fail "the owner can list a ti-trader-only directory without sudo"
run_block 'sudo ls /var/lib/trend-investing/engines/selftest' >/dev/null || fail "the owner cannot list it with sudo"
rmdir /var/lib/trend-investing/engines/selftest

# remap_expr: a sed program applied to a block and its expected lines, for
# the drill at the end, which runs the recovery section's commands for a
# halted run of another name.
remap_expr='s/^$//'
remap() { sed -e "$remap_expr"; }

# step NAME [STATUS [EXPECT]]: runs the runbook's NAME block as the owner,
# as written, and requires every line of the expected block (EXPECT, else
# NAME), in order, to appear in the output: a line as a whole, or, marked
# "~ ", as a pattern. Each match consumes the output through the line it
# matched. The output is kept in $out.
out=
step() {
  local cmds want line status=0 rest found newrest o pat mode
  cmds=$(block rehearsed "$1" | remap)
  test -n "$cmds" || fail "the runbook has no command block 'rehearsed: $1'"
  want=$(block expect "${3:-$1}" | remap)
  test -n "$want" || fail "the runbook has no expected output block 'expect: ${3:-$1}'"
  out=$(run_block "$cmds" 2>&1) || status=$?
  printf 'REHEARSAL step %s output:\n%s\n' "$1" "$out" | sed 's/^/    /'
  if test "$status" != "${2:-0}"; then fail "step $1 exited $status, want ${2:-0}"; fi
  rest=$out
  while IFS= read -r line; do
    test -n "$line" || continue
    mode=lit
    pat=$line
    if [[ $line == "~ "* ]]; then
      mode=re
      pat=${line:2}
    fi
    found=
    newrest=
    while IFS= read -r o || test -n "$o"; do
      if test -z "$found"; then
        if test "$mode" = re; then
          if [[ $o =~ $pat ]]; then found=1; fi
        elif test "$o" = "$pat"; then
          found=1
        fi
        continue
      fi
      newrest+=$o$'\n'
    done <<<"$rest"
    test -n "$found" || fail "step $1: the runbook promises '$pat', which the output lacks (or has out of order)"
    rest=$newrest
  done <<<"$want"
}

# --- Stand-ins for what provision.sh and the owner already did.
useradd --system --gid ti-trader --no-create-home --shell /usr/sbin/nologin ti-operator
for d in data engines flex adapters; do install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/$d; done
install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/engines/boost /var/lib/trend-investing/adapters/boost /var/lib/trend-investing/flex/boost
install -d -o ti-operator -g ti-trader -m 0750 /var/lib/trend-investing/engines/boost/operator
install -D -m 0755 /dev/null /usr/local/libexec/trend-investing/clock-check.sh
printf '#!/bin/sh\nexit 0\n' >/usr/local/libexec/trend-investing/clock-check.sh
for b in ti-data ti-reporter ti-dashboard operator journal-query; do
  case $b in
    operator | journal-query) install -m 0755 "/dist/$b" "/usr/local/bin/$b" ;;
    *) install -D -m 0755 "/dist/$b" "/opt/trend-investing/bin/$b" ;;
  esac
done
cp /deploy/systemd/ti-dashboard.service /deploy/systemd/ti-data.timer /deploy/systemd/ti-reporter@.timer /etc/systemd/system/
cp /deploy/systemd/ti-engine@.service /etc/systemd/system/
rm -rf /etc/systemd/system/ti-engine@.service.d
systemctl reset-failed || true

# ibapi is not installed here: provision.sh twsapi's record is written as it
# writes it, and gateway-adapter.py stands in for the adapter environment's
# python3 (the real adapter code, the fake gateway).
# shellcheck source=/dev/null
. /deploy/versions.env
artefact=installer
pinned_sha=$TWSAPI_SHA256
if test "$(uname -m)" != x86_64; then
  artefact=archive
  pinned_sha=$TWSAPI_ARCHIVE_SHA256
fi
printf 'TWSAPI_VERSION=%s\nTWSAPI_SHA256=%s\nTWSAPI_ARTEFACT=%s\nPROTOBUF_VERSION=%s\nPROTOBUF_SHA256=%s\n' \
  "$TWSAPI_VERSION" "$pinned_sha" "$artefact" "$PROTOBUF_VERSION" "$PROTOBUF_SHA256" >/opt/trend-investing/twsapi/INSTALLED
rm -f /opt/trend-investing/adapter/adapter/ibkr/cli.py /opt/trend-investing/adapter/adapter/ibkr/flex.py
install -d /opt/rehearsal/tests
install -m 0644 /adapter/ibkr/tests/fake_tws.py /opt/rehearsal/tests/
install -m 0755 /deploy/rehearsal/gateway-adapter.py /opt/rehearsal/gateway-adapter.py
printf '#!/bin/sh\nexec /usr/bin/python3 /opt/rehearsal/gateway-adapter.py "$@"\n' >/opt/trend-investing/adapter-venv/bin/python3
chmod 0755 /opt/trend-investing/adapter-venv/bin/python3
chmod -R go+rX /opt/rehearsal

# The adapter's configuration as provision.sh services creates it: dry-run,
# sending off. Its code is not installed yet, as on a server where
# provision.sh adapter was never run: the runbook's check says so.
test ! -e /opt/trend-investing/adapter/adapter/ibkr/cli.py || fail "the adapter's code is still installed"

# The synthetic Massive: HTTPS on loopback, a throwaway certificate trusted
# through SSL_CERT_FILE, api.massive.com resolved to loopback, and the
# data job's 12.5-second request spacing skipped (its sleeps only; the
# stand-in cannot be rate limited). The job's own command line is unchanged.
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=api.massive.com -addext subjectAltName=DNS:api.massive.com \
  -keyout "$probe/massive.key" -out "$probe/massive.crt" 2>/dev/null
chmod 0644 "$probe/massive.crt"
: >"$probe/massive-requests"
chmod 0644 "$probe/massive-requests"
echo '127.0.0.1 api.massive.com' >>/etc/hosts
systemd-run --quiet --unit=ti-probe-massive python3 /deploy/rehearsal/massive-standin.py "$probe/massive.crt" "$probe/massive.key" "$probe/massive-requests"
install -d /opt/rehearsal/site
# The clock: every Python program started from now on (the data job, the
# adapter) reads datetime.now() shifted by the number of seconds in
# TI_REHEARSAL_CLOCK_OFFSET, which clock_at sets for the units about to be
# started. The data job's request spacing is skipped too.
cat >/opt/rehearsal/site/sitecustomize.py <<'PYSITE'
import datetime
import os
import sys
import time

_real = datetime.datetime
_offset = datetime.timedelta(seconds=int(os.environ.get("TI_REHEARSAL_CLOCK_OFFSET", "0")))


class _Shifted(_real):
    @classmethod
    def now(cls, tz=None):
        return _real.now(tz) + _offset


datetime.datetime = _Shifted
if os.path.basename(sys.argv[0]) == "ti-data":
    time.sleep = lambda seconds: None
PYSITE
chmod -R go+rX /opt/rehearsal
systemctl set-environment "PYTHONPATH=/opt/rehearsal/site" "SSL_CERT_FILE=$probe/massive.crt"
# clock_at "DATE TIME" (New York): the clock the units started from now on
# see, ticking from there.
clock_at() {
  local target now
  target=$(TZ=America/New_York date -d "$1" +%s)
  now=$(date +%s)
  systemctl set-environment "TI_REHEARSAL_CLOCK_OFFSET=$((target - now))"
}
clock_at "2026-10-24 19:00"

# IB Gateway's stand-in: a unit of the Gateway's name that listens on its
# port (the adapter's connection itself is the scripted fake).
cat >/etc/systemd/system/ti-gateway.service <<UNIT
[Unit]
Description=Stand-in IB Gateway (rehearsal): listens on 4002
[Service]
Type=simple
ExecStart=/usr/bin/python3 /deploy/rehearsal/listen.py
UNIT
systemctl daemon-reload
systemctl start ti-gateway.service || { journalctl -u ti-gateway --no-pager -o cat | tail -n 8 >&2; fail "the stand-in Gateway did not start"; }

# The secrets as the owner has them by now: the Massive key (synthetic) and
# the Boost account (invented, as fake_tws.py's).
cat >/root/ti-editor.sh <<'ED'
#!/bin/sh
cat /dev/shm/ti-edit/content >"$1"
ED
chmod 0755 /root/ti-editor.sh
install -d -m 0700 /dev/shm/ti-edit
{ cat "$unlocked/secrets.env"; printf 'MASSIVE_API_KEY=synthetic-key-0123456789\n'; } >/dev/shm/ti-edit/content
EDITOR=/root/ti-editor.sh with_pass ti-secrets edit secrets.env >/dev/null || fail "ti-secrets edit secrets.env failed"
printf 'IBKR_ACCOUNT_BOOST=DU0000001\n' >/dev/shm/ti-edit/content
EDITOR=/root/ti-editor.sh with_pass ti-secrets edit accounts/boost.env >/dev/null || fail "ti-secrets edit accounts/boost.env failed"
rm -rf /dev/shm/ti-edit

# --- Step 0: where the server stands. The adapter's code is the one thing
# the owner may need to install; do what the runbook says for it.
systemctl stop ti-data.timer ti-reporter@boost.timer 2>/dev/null || true
out=$(run_block "$(block rehearsed preflight)" 2>&1) || true
if grep -qxF 'adapter-code-installed' <<<"$out"; then fail "the adapter's code was reported installed before provision.sh adapter ran"; fi
TI_REHEARSAL_ANY_ARCH=1 /deploy/provision.sh adapter >/dev/null || fail "provision.sh adapter failed"
test -f /opt/trend-investing/adapter/adapter/ibkr/cli.py || fail "provision.sh adapter did not install the adapter's code"
step preflight
pass "step 0: the preflight reports the server as the owner described it, and the adapter's code is installed by provision.sh adapter"

# --- Steps 1 to 4: the account in the configuration, the first run's
# approval, the run file, then the engine, started by its own unit.
step account
# Run again it changes nothing and says the same; a configuration that
# already names another account stops the step before it changes anything.
step account
config=/etc/trend-investing/engines/boost.config
sed -i 's/"account_id": "DU0000001"/"account_id": "DU0000009"/' "$config"
step account 1 account-refused
grep -q '"account_id": "DU0000009"' "$config" || fail "the account step changed a configuration that names another account"
sed -i 's/"account_id": "DU0000009"/"account_id": "DU0000001"/' "$config"
step approve
test -s /var/lib/trend-investing/engines/boost/first-run-approval.json || fail "operator approve wrote no approval"
step run-env
test "$(stat -c %U:%G /var/lib/trend-investing/engines/boost/run.env)" = ti-trader:ti-trader || fail "run.env is not the engine user's"
step engine
pass "steps 1 to 4: the account is named in the configuration, the first run is approved with its starting book, run.env is written and the real engine unit starts the dry-run engine"

# The warm-up command as market-data.md gave it before (the plain-text path)
# is refused: the file is not there, only its encrypted copy.
status=0
systemd-run --wait --collect -p User=ti-trader -p Group=ti-trader -p LoadCredential=secrets:/etc/trend-investing/secrets.env \
  /bin/true >/dev/null 2>&1 || status=$?
test "$status" != 0 || fail "a credential loaded from the plain-text /etc/trend-investing/secrets.env, which must not exist"
test ! -e /etc/trend-investing/secrets.env || fail "secrets.env is in plain text"

# --- Step 3: the one-time warm-up, with the encrypted secrets, against the
# synthetic Massive on the Saturday evening's clock.
# The wait has a deadline. A warm-up that never ends (a stand-in for it: a
# unit of its name that sleeps) is given up on after the deadline, which the
# rehearsal shortens to six seconds, with the message the runbook promises.
engines=/var/lib/trend-investing/engines/boost
systemd-run --quiet --unit=ti-warmup --collect /bin/sleep 300
remap_expr='s/10800/6/;s/sleep 30/sleep 1/'
step warmup-wait 1 warmup-late
! grep -q 'Deactivated successfully' <<<"$out" || fail "the late warm-up was reported as finished"
remap_expr='s/^$//'
systemctl stop ti-warmup.service
step warmup
step warmup-wait
test -s /var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-23.jsonl || fail "the warm-up wrote no stream file"
fetched=$(grep -c '^/v2/aggs/grouped/' "$probe/massive-requests")
test "$fetched" -gt 440 || fail "the warm-up fetched only $fetched sessions"
pass "step 3: the warm-up command, run with the encrypted secrets unlocked, records $fetched sessions from the stand-in Massive and writes the run's first file"

# --- Step 6: the adapter, dry-run, started by its own unit; it reads the
# account from the fake gateway and delivers the warm-up.
step adapter
pass "step 6: the adapter unit starts in dry-run with sending off, reconciles against the approved book, and delivers the warm-up; the engine stays out of Halted"

# Readiness counts only this start's acknowledgements. The adapter is
# restarted on a run.env whose log does not exist, so the new invocation
# refuses to start; the journal still holds the first invocation's
# acknowledgement of the session, which must not make it ready.
systemctl stop ti-adapter-boost.service
cp -p "$engines/run.env" /root/run.env.keep
sed -i 's/^TI_RUN=.*/TI_RUN=run-no-such-log/' "$engines/run.env"
remap_expr='s/seq 60/seq 2/;s/sleep 5/sleep 1/'
step adapter 1 adapter-not-ready
remap_expr='s/^$//'
! grep -q 'market.session.closed' <<<"$out" || fail "the readiness check showed an earlier start's acknowledgement"
cp -p /root/run.env.keep "$engines/run.env"
systemctl reset-failed ti-adapter-boost.service || true
systemctl start ti-adapter-boost.service
for _ in $(seq 30); do systemctl is-active --quiet ti-adapter-boost && break; sleep 1; done
sleep 20 # the failed start's page, if it pages, is sent before the weekend's firings are counted
pass "step 6: an earlier start's acknowledgement does not make a start that never connected ready"

# --- Step 7: the two timers, and what the weekend's firings do. A page is
# a POST to Pushover's endpoint, so none may be added by any firing here.
grouped() { grep -c '^/v2/aggs/grouped/' "$probe/massive-requests" || true; }
before_requests=$(grouped)
before_pages=$(pushover_pages)
step timers
test "$(grouped)" = "$before_requests" || fail "Saturday's firings fetched a session although Friday's file was written"
test "$(pushover_pages)" = "$before_pages" || fail "a Saturday firing paged"
pass "step 7: both timers start, and the Saturday firings succeed, fetch no session and page nothing"

# Sunday 09:05 (after the Gateway's cold login): the same, again.
clock_at "2026-10-25 09:05"
# Only the data job: it is Python, and the clock above moves what it reads.
# The reporter is a Go program with no clock to move, so it cannot be made
# to see a Sunday here; its weekend firings are not rehearsed (step 7 above
# ran it once, on the container's own clock).
systemctl start ti-data.service
test "$(systemctl show -p Result --value ti-data.service)" = success || fail "ti-data.service did not succeed on Sunday"
test "$(grouped)" = "$before_requests" || fail "Sunday's data job fetched a session"
test "$(pushover_pages)" = "$before_pages" || fail "a Sunday firing paged"
pass "Sunday's firing of the data job succeeds quietly: nothing to record, no page"

# Monday 17:06: the data job's first evening firing writes Monday's file.
clock_at "2026-10-26 17:06"
systemctl start ti-data.service
test "$(systemctl show -p Result --value ti-data.service)" = success || fail "Monday's data job did not succeed: $(journalctl -u ti-data.service --no-pager -o cat | tail -n 3)"
monday=/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-26.jsonl
test -s "$monday" || fail "Monday's firing wrote no file for 2026-10-26"
grep -qxF "$monday" <<<"$(journalctl -u ti-data.service --no-pager -o cat | grep '^/var/lib' | tail -n 1)" || fail "Monday's firing did not print the file it wrote"
test "$(grouped)" -gt "$before_requests" || fail "Monday's firing fetched no session"
test "$(pushover_pages)" = "$before_pages" || fail "Monday's firing paged"
pass "Monday's 17:06 firing of the data job writes boost-2026-10-26.jsonl beside Friday's, and pages nothing"
clock_at "2026-10-24 19:00"

# --- Step 8: the dashboard, on loopback.
step dashboard
pass "step 8: the dashboard unit starts on loopback and answers read-only"

# --- If the engine halts on its first reading. A drill, on the run name the
# recovery section does not use: the section's commands, as written, with
# 2026-10-25 in place of 2026-10-24 (so its new name is run-2026-10-25b). The
# earlier run is stopped; a fresh run is approved with a wrong book, the
# adapter reads the account and the engine halts; then the section is followed.
systemctl stop ti-adapter-boost.service ti-engine@boost.service ti-data.timer ti-reporter@boost.timer
runuser -u ti-trader -- operator approve -log "$engines/run-2026-10-24.log" -account DU0000001 -by owner \
  -positions "" -cash 90000.00 -currency USD -out "$engines/approval-run-2026-10-25.json" >/dev/null || fail "the drill's wrong-book approval was refused"
printf 'TI_RUN=run-2026-10-25\nTI_AS_OF=2026-10-26T19:58:00Z\nTI_RESTART_ARGS=-approval %s/approval-run-2026-10-25.json\n' "$engines" >"$engines/run.env"
chown ti-trader:ti-trader "$engines/run.env"
systemctl start ti-engine@boost.service
for _ in $(seq 20); do systemctl is-active --quiet ti-engine@boost && break; sleep 1; done
systemctl is-active --quiet ti-engine@boost || fail "the drill's engine did not start: $(journalctl -u ti-engine@boost -n 5 --no-pager -o cat)"
systemd-run --quiet --wait --collect --unit=ti-warmup-drill -p User=ti-trader -p Group=ti-trader \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data --config /etc/trend-investing/engines/boost.config \
  --engine-log "$engines/run-2026-10-25.log" --backfill-from 2025-01-02 || fail "the drill's warm-up failed"
systemctl start ti-adapter-boost.service
for _ in $(seq 120); do grep -q '"state":"halted"' "$engines/run-2026-10-25.log" && break; sleep 1; done
grep -q '"state":"halted"' "$engines/run-2026-10-25.log" || fail "the engine did not halt on a first reading that is not the approved book: $(journalctl -u ti-adapter-boost -n 8 --no-pager -o cat)"
note "the drill's engine halted on its first reading, as a mistyped book makes it"

remap_expr='s/run-2026-10-24/run-2026-10-25/g'
step recover-stop
step recover-approve
step recover-run-env
step engine 0 engine-again
before_recovery=$(grouped)
step warmup
step warmup-wait
refetched=$(($(grouped) - before_recovery))
test "$refetched" -lt 5 || fail "the second warm-up fetched $refetched sessions; the runbook says it fetches only the missing days"
step adapter
pass "recovery: the halted run is stopped, a new run name, approval with the book and run.env follow, and steps 4 to 6 again start a run that stays out of Halted; the second warm-up fetched $refetched sessions (the days missing from the recordings)"
remap_expr='s/^$//'
