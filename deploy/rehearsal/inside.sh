#!/bin/bash
# Runs inside the rehearsal container (root, Ubuntu 24.04, no systemd running).
set -euo pipefail
set +x

fail() { echo "REHEARSAL FAIL: $*" >&2; exit 1; }
pass() { echo "REHEARSAL ok: $*"; }
mode_of() { stat -c %a "$1"; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends systemd sudo >/dev/null

# Work from a writable copy, as on the server, with the data step's built
# file beside it as dist/ti-data.
cp -a /deploy /root/deploy
cp -a /dist /root/dist
p=/root/deploy/provision.sh

"$p" base
"$p" clock
"$p" gateway
"$p" services
pass "first full pass"

# The clock check passed above against this container's host. With a reference
# clock an hour off (the skew is measured to about a second, so -3598 to -3602
# all count), the step must refuse (this container has no NTP service
# of its own, so the reference decides).
stubs=$(mktemp -d)
cat >"$stubs/curl" <<'STUB'
#!/bin/bash
printf 'HTTP/2 200\r\nDate: %s\r\n\r\n' "$(date -u -d '+1 hour' '+%a, %d %b %Y %H:%M:%S GMT')"
STUB
chmod +x "$stubs/curl"
if PATH="$stubs:$PATH" "$p" clock >/dev/null 2>"$stubs/err"; then fail "a clock an hour off was accepted"; fi
grep -Eq 'skew of -(359[89]|360[012])s' "$stubs/err" || fail "the clock step failed for another reason: $(cat "$stubs/err")"
rm -rf "$stubs"
pass "the clock step passes on this host's clock and refuses a skewed one"

# Idempotence: a second pass changes nothing and must not touch owner-entered stubs.
printf 'TWS_USERID=sentinel\n' >>/etc/trend-investing/ibkr-login.env
"$p" base
"$p" clock
"$p" gateway
"$p" services
grep -q sentinel /etc/trend-investing/ibkr-login.env || fail "re-run overwrote the owner's login file"
pass "second pass idempotent, owner stub preserved"
sed -i '/sentinel/d' /etc/trend-investing/ibkr-login.env

# The owner's own ntfy server (provision.sh ntfy): the pinned binary is
# fetched and verified, installed, and configured to listen on the Tailscale
# address only. An address that is not a tailnet one is refused with nothing
# written, so the server can never be made to listen on a public interface; a
# download that does not match its pin is refused and nothing installed.
cache=${TI_DOWNLOAD_CACHE:-/var/cache/trend-investing}
cp -a /root/deploy /root/deploy-badntfy
sed -i 's/^NTFY_SHA256=.*/NTFY_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' /root/deploy-badntfy/versions.env
grep -q '^NTFY_SHA256=0\{64\}$' /root/deploy-badntfy/versions.env || fail "could not break the ntfy pin"
rm -f "$cache"/ntfy_*.tar.gz
if TI_NTFY_ADDRESS=100.64.0.7 /root/deploy-badntfy/provision.sh ntfy >/dev/null 2>&1; then fail "an ntfy download that does not match its pin was accepted"; fi
test ! -e /usr/local/bin/ntfy || fail "a mismatched ntfy download was installed"
for bad in 0.0.0.0 127.0.0.1 203.0.113.5 100.128.0.1 100.63.255.255 "100.64.0.1; x" 100.64.999.1 100.64.0.256; do
  if TI_NTFY_ADDRESS=$bad "$p" ntfy >/dev/null 2>&1; then fail "the ntfy step accepted the address '$bad'"; fi
done
test ! -e /etc/ntfy/server.yml || fail "an address that is not Tailscale's left a configuration"
TI_NTFY_ADDRESS=100.64.0.7 "$p" ntfy
test -x /usr/local/bin/ntfy || fail "ntfy not installed"
grep -qx 'listen-http: "100.64.0.7:2586"' /etc/ntfy/server.yml || fail "the ntfy server does not listen on the Tailscale address: $(cat /etc/ntfy/server.yml)"
grep -qx 'base-url: "http://100.64.0.7:2586"' /etc/ntfy/server.yml || fail "the ntfy server's base URL is not the Tailscale address"
test "$(grep -c '^listen-http:' /etc/ntfy/server.yml)" = 1 || fail "more than one listen address"
# The iOS app gets instant notifications from a self-hosted server only
# through the upstream relay, which carries the message ID alone.
grep -qx 'upstream-base-url: "https://ntfy.sh"' /etc/ntfy/server.yml || fail "the ntfy server names no upstream relay for the iOS app: $(cat /etc/ntfy/server.yml)"
id ti-ntfy >/dev/null || fail "no ti-ntfy user"
test "$(getent passwd ti-ntfy | cut -d: -f7)" = /usr/sbin/nologin || fail "ti-ntfy has a login shell"
test -f /etc/systemd/system/ti-ntfy.service || fail "the ntfy unit is not installed"
TI_NTFY_ADDRESS=100.64.0.7 "$p" ntfy
pass "the ntfy server is fetched against its pin (a mismatch is refused), listens on the Tailscale address only, and runs as its own user"

# Users and permissions.
for u in ti-gateway ti-trader; do
  id "$u" >/dev/null || fail "missing user $u"
  test "$(getent passwd "$u" | cut -d: -f7)" = /usr/sbin/nologin || fail "$u has a login shell"
  test "$(id -u "$u")" -ne 0 || fail "$u is uid 0"
done
for f in ibkr-login.env secrets.env accounts/boost.env accounts/trad-ira.env accounts/roth-ira.env; do
  test "$(mode_of "/etc/trend-investing/$f")" = 600 || fail "$f is not mode 600"
  test "$(stat -c %U:%G "/etc/trend-investing/$f")" = root:root || fail "$f is not root-owned"
done
pass "users are non-root, nologin; secret stubs are root:root 0600"

# IBKR's execution times name "US/Eastern", which Ubuntu 24.04 ships only in
# tzdata-legacy; the adapter cannot read an execution without it.
python3 -c 'from zoneinfo import ZoneInfo; ZoneInfo("US/Eastern")' || fail "the US/Eastern zone IBKR's execution times name is missing"
pass "the US/Eastern zone is installed"

# Pinned artefacts are really installed.
if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
  test -s /var/cache/trend-investing/ibgateway-10.50.1f.sh || fail "gateway installer not downloaded"
  echo "REHEARSAL note: gateway installer verified but not executed on $(uname -m)"
else
  test -x /var/lib/ti-gateway/Jts/ibgateway/1050/ibgateway || fail "gateway not installed"
fi
test -f /opt/ibc/scripts/ibcstart.sh || fail "IBC not installed"
test -x /usr/local/bin/tailscaled || fail "tailscaled not installed"
if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" != 1; then
  test "$(stat -c %U /var/lib/ti-gateway/Jts/ibgateway/1050/ibgateway)" = ti-gateway || fail "gateway files not owned by ti-gateway"
fi
pass "gateway, IBC and tailscale installed from checksummed downloads"

# A corrupt cached download is discarded and fetched again; a download whose
# checksum differs from the pin is refused.
cache=${TI_DOWNLOAD_CACHE:-/var/cache/trend-investing}
echo corrupt >"$cache/IBCLinux-3.24.2.zip"
rm -f /opt/ibc/.version-3.24.2
"$p" gateway
echo "cc097ca1dfa75413a5fbe02e4743050af15f3308eb6a97c769b3cff9850c80c5  $cache/IBCLinux-3.24.2.zip" | sha256sum -c --status - || fail "corrupt cache was not replaced"
cp -a /root/deploy /root/deploy-badpin
sed -i 's/^IBC_SHA256=.*/IBC_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' /root/deploy-badpin/versions.env
rm -f /opt/ibc/.version-3.24.2 "$cache/IBCLinux-3.24.2.zip"
if /root/deploy-badpin/provision.sh gateway >/dev/null 2>&1; then fail "a download that does not match its pin was accepted"; fi
test ! -e /opt/ibc/.version-3.24.2 || fail "a mismatched download was installed"
"$p" gateway
pass "corrupt cache replaced; checksum mismatch refused and nothing installed"

# The adapter's ibapi (provision.sh twsapi), for real. The protobuf wheel it
# imports is pinned beside it and verified like every download: a wheel whose
# checksum differs from the pin is refused with nothing installed (no
# environment, no install record), before and after a good install.
venv=/opt/trend-investing/adapter-venv
record=/opt/trend-investing/twsapi/INSTALLED
cp -a /root/deploy /root/deploy-badpb
sed -i 's/^PROTOBUF_SHA256=.*/PROTOBUF_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' /root/deploy-badpb/versions.env
grep -q '^PROTOBUF_SHA256=0\{64\}$' /root/deploy-badpb/versions.env || fail "could not break the protobuf pin"
rm -f "$cache"/protobuf-*.whl
err=$(mktemp)
if /root/deploy-badpb/provision.sh twsapi >/dev/null 2>"$err"; then fail "a protobuf wheel that does not match its pin was accepted"; fi
grep -q 'checksum mismatch for .*protobuf-5\.29\.5-py3-none-any\.whl' "$err" || fail "the twsapi step failed for another reason: $(cat "$err")"
test ! -e "$venv" && test ! -e "$record" || fail "a mismatched protobuf wheel left an environment or an install record"
"$p" twsapi
if test "${TI_REHEARSAL_SKIP_INSTALLER:-0}" = 1; then
  test -s "$cache/protobuf-5.29.5-py3-none-any.whl" || fail "protobuf wheel not downloaded"
  echo "REHEARSAL note: ibapi not imported on $(uname -m): the TWS API installer is x86_64 only and was not executed"
else
  # Imported for real, in the adapter's own environment and nowhere else.
  "$venv/bin/python3" -I -c 'import google.protobuf as g, ibapi, ibapi.client, ibapi.contract, ibapi.order, ibapi.wrapper
assert g.__version__ == "5.29.5", g.__version__
assert ibapi.VERSION == {"major": 1051, "minor": 1}, ibapi.VERSION' || fail "ibapi does not import in the adapter's environment"
  grep -qx 'PROTOBUF_VERSION=5.29.5' "$record" || fail "the install record names no protobuf version"
  # The adapter's own start-up check, run in that environment against the record.
  (cd /tmp && "$venv/bin/python3" -I -c 'import sys; sys.path.insert(0, "/"); from ibapi_pin import check_installed
print(check_installed(sys.argv[1], sys.argv[2]))' "$record" /root/deploy/versions.env) || fail "the adapter's pin check refuses the installed ibapi"
  cp "$record" "$record.good"
  err=$(mktemp)
if /root/deploy-badpb/provision.sh twsapi >/dev/null 2>"$err"; then fail "a protobuf wheel that does not match its pin was accepted"; fi
grep -q 'checksum mismatch for .*protobuf-5\.29\.5-py3-none-any\.whl' "$err" || fail "the twsapi step failed for another reason: $(cat "$err")"
  cmp -s "$record" "$record.good" || fail "a refused protobuf wheel changed the install record"
  rm -f "$record.good"
  "$venv/bin/python3" -I -c 'import ibapi' || fail "a refused protobuf wheel broke the installed environment"
fi
pass "the pinned protobuf wheel is verified, a mismatch is refused with nothing installed, ibapi imports in the adapter's environment"

# Credential rendering: a fake secret must reach the config file only, mode 0600.
# shellcheck disable=SC2016
fake_pw='fakePW!with$pecial&chars\\n#x'
cred=$(mktemp -d)
printf 'TWS_USERID=fakeuser\nTWS_PASSWORD=%s\nTWS_TRADING_MODE=paper\nTWS_READONLY_API=yes\n' "$fake_pw" >"$cred/ibkr-login"
run=$(mktemp -d)
out=$(CREDENTIALS_DIRECTORY=$cred RUNTIME_DIRECTORY=$run IBC_TEMPLATE=/opt/ibc/config.ini \
  /usr/local/libexec/trend-investing/render-ibc-config.sh 2>&1)
case $out in *fakePW*|*fakeuser*) fail "renderer printed a credential" ;; esac
test "$(mode_of "$run/config.ini")" = 600 || fail "rendered config not 0600"
grep -qxF "IbPassword=$fake_pw" "$run/config.ini" || fail "password not rendered verbatim"
grep -qx 'IbLoginId=fakeuser' "$run/config.ini" || fail "login id not rendered"
grep -qx 'TradingMode=paper' "$run/config.ini" || fail "trading mode not rendered"
grep -qx 'ReadOnlyApi=yes' "$run/config.ini" || fail "read-only API not rendered"
grep -qx 'AutoRestartTime=11:45 PM' "$run/config.ini" || fail "nightly restart not rendered"
grep -qx 'ColdRestartTime=09:00' "$run/config.ini" || fail "Sunday cold restart not rendered"
grep -qx 'ReloginAfterSecondFactorAuthenticationTimeout=yes' "$run/config.ini" || fail "2FA relogin not rendered"
if grep -rqF fakePW /etc/trend-investing /var/log 2>/dev/null; then fail "fake secret found on disk outside the runtime dir"; fi
pass "renderer writes credentials only to a 0600 file and prints none"

# Refusals: empty login, bad mode.
printf 'TWS_USERID=\nTWS_PASSWORD=\n' >"$cred/ibkr-login"
CREDENTIALS_DIRECTORY=$cred RUNTIME_DIRECTORY=$run /usr/local/libexec/trend-investing/render-ibc-config.sh >/dev/null 2>&1 && fail "empty login accepted"
printf 'TWS_USERID=a\nTWS_PASSWORD=b\nTWS_TRADING_MODE=prod\n' >"$cred/ibkr-login"
CREDENTIALS_DIRECTORY=$cred RUNTIME_DIRECTORY=$run /usr/local/libexec/trend-investing/render-ibc-config.sh >/dev/null 2>&1 && fail "bad mode accepted"
pass "renderer refuses an empty login and an unknown trading mode"

# The gateway's start renders first, so an empty login ends the unit's main
# process with 78, which the unit does not restart on (no 30-second loop
# before the owner fills the login in).
printf 'TWS_USERID=\nTWS_PASSWORD=\n' >"$cred/ibkr-login"
status=0
CREDENTIALS_DIRECTORY=$cred RUNTIME_DIRECTORY=$run GATEWAY_MAJOR=1050 \
  /usr/local/libexec/trend-investing/start-gateway.sh >/dev/null 2>&1 || status=$?
test "$status" = 78 || fail "start-gateway exited $status on an empty login, want 78"
grep -qx 'RestartPreventExitStatus=64 65 78' /etc/systemd/system/ti-gateway.service || fail "ti-gateway restarts on a configuration exit"
pass "an empty login stops the gateway with 78, which the unit does not restart on"

# The data step is installed and runs under the server's python3. Given a
# durable log the engine wrote for the shipped Boost configuration
# (adapter/marketdata/tests/testdata/boost-fixture.log, copied beside the
# built file), everything but the key is in place, so the refusal exercised
# is the missing Massive key: exit 1, the key's name in the message, and
# nothing written.
test -x /opt/trend-investing/bin/ti-data || fail "the data step was not installed"
cmp /root/dist/ti-data /opt/trend-investing/bin/ti-data || fail "the installed data step is not the built one"
/opt/trend-investing/bin/ti-data --help | grep -q -- --backfill-from || fail "the data step does not run"
grep -qx 'MASSIVE_API_KEY=' /etc/trend-investing/secrets.env || fail "the secrets stub has no MASSIVE_API_KEY line"
scratch=$(mktemp -d)
cp /root/dist/boost-fixture.log "$scratch/run.log"
status=0
/opt/trend-investing/bin/ti-data --data "$scratch/data" --config /etc/trend-investing/engines/boost.config \
  --engine-log "$scratch/run.log" --secrets-file /etc/trend-investing/secrets.env >/dev/null 2>"$scratch/err" || status=$?
test "$status" = 1 || fail "the data step exited $status with no key, want 1"
grep -q 'ti-data refused: .*MASSIVE_API_KEY' "$scratch/err" || fail "the data step's refusal does not name the missing key"
test ! -e "$scratch/data" || fail "the data step wrote something while refusing"
pass "the data step is installed, runs, and refuses a missing key with nothing written"

# The reporter, the dashboard and the owner's two tools are installed from
# the build beside deploy/, and the tools are on sudo's PATH for ti-trader,
# as the runbooks run them.
for b in ti-reporter ti-dashboard operator journal-query; do
  cmp "/root/dist/$b" "/opt/trend-investing/bin/$b" || fail "$b is not installed from the build"
done
# Each tool exits 1 with its usage when given no command.
for b in operator journal-query; do
  out=$(sudo -u ti-trader "$b" 2>&1 || true)
  case $out in "usage: $b"*) ;; *) fail "sudo -u ti-trader $b does not run the installed tool: $out" ;; esac
done
for u in ti-reporter.timer ti-reporter.service timers.target.wants/ti-reporter.timer; do
  test ! -e "/etc/systemd/system/$u" || fail "the old single reporter's $u is still installed"
done
pass "reporter, dashboard, operator and journal-query installed; the tools run under sudo -u ti-trader"

# The reporter, run with its unit's own command line (systemd is not running
# here, so the line is expanded as systemd would: %i, %d, and the unit's and
# run.env's variables), against the engine's durable log and a stand-in for
# both channels on loopback. Its secrets come only from the credential file.
# It runs as the unit's user, ti-trader, on files owned as on the server, so
# the check fails if that user cannot read the log or write its state. It
# must deliver, write its state file, and send nothing new the second time.
rep=$(mktemp -d)
mkdir -p "$rep/engines/boost" "$rep/reports" "$rep/creds"
cp /root/dist/boost-fixture.log "$rep/engines/boost/run-rehearsal.log"
# A channel with no record starts at the run's own time and is sent none of
# the log's earlier alerts, and this fixture log is old: so the state file
# begins with both channels recorded long before it, as on a server whose
# reporter has run since before the log's events.
printf '{"keys":[],"baselines":{"ntfy":"2000-01-01T00:00:00Z","pagerduty":"2000-01-01T00:00:00Z"}}' >"$rep/reports/boost.reporter.json"
chown -R ti-trader:ti-trader "$rep"
chmod 0750 "$rep/engines" "$rep/engines/boost" "$rep/reports"
python3 - "$rep/posts" <<'PY' &
import http.server
import sys

posts = sys.argv[1]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with open(posts, "a") as f:
            f.write(self.path + "\n")
        with open(posts + ".bodies", "ab") as f:
            f.write(body.replace(b"\n", b" ") + b"\n")
        self.send_response(202)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"success"}')

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", 8099), Handler).serve_forever()
PY
fake=$!
for _ in $(seq 50); do
  python3 -c 'import socket; socket.create_connection(("127.0.0.1", 8099), 1)' 2>/dev/null && break
  sleep 0.1
done
printf 'TI_NTFY_TOPIC=rehearsal\nTI_NTFY_SERVER=http://127.0.0.1:8099\nTI_PAGERDUTY_ROUTING_KEY=fake\nTI_PAGERDUTY_URL=http://127.0.0.1:8099/v2/enqueue\nTI_HEARTBEAT_URL=http://127.0.0.1:8099/hb\n' >"$rep/creds/secrets"
chown ti-trader:ti-trader "$rep/creds/secrets"
chmod 0400 "$rep/creds/secrets"
line=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-reporter@.service)
line=${line//%i/boost}
line=${line//%d/$rep/creds}
# The unit file holds these as literal ${...} text; the backslash matches them verbatim.
line=${line//"\${TI_STATE_DIR}"/$rep/engines/boost}
line=${line//"\${TI_REPORTS_DIR}"/$rep/reports}
line=${line//"\${TI_RUN}"/run-rehearsal}
case $line in *'$'* | *%*) fail "the reporter's ExecStart has a variable the rehearsal does not expand: $line" ;; esac
# No systemd runs here, so the reporter's systemctl (asked for each -units
# unit's state and invocation) is a stand-in that answers every unit active
# and records what it was asked; a systemctl that cannot answer fails the
# firing, so the stand-in is first on PATH.
sysd=$(mktemp -d)
chmod 0755 "$sysd"
install -m 0666 /dev/null "$sysd/calls"
cat >"$sysd/systemctl" <<SH
#!/bin/sh
echo "\$*" >>$sysd/calls
echo ActiveState=active
echo InvocationID=0123456789abcdef0123456789abcdef
SH
chmod 0755 "$sysd/systemctl"
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $line >/dev/null || fail "the reporter, run as its unit runs it (as ti-trader), failed"
{ grep -qx 'show -p ActiveState -p InvocationID ti-engine@boost.service' "$sysd/calls" && grep -qx 'show -p ActiveState -p InvocationID ti-adapter-boost.service' "$sysd/calls"; } ||
  fail "the reporter did not ask systemd for the engine's and the adapter's state: $(cat "$sysd/calls")"
test -s "$rep/reports/boost.reporter.json" || fail "the reporter wrote no state file"
test "$(stat -c %U "$rep/reports/boost.reporter.json")" = ti-trader || fail "the reporter's state file is not ti-trader's"
grep -q '^/rehearsal$' "$rep/posts" || fail "the reporter reached no ntfy topic"
grep -q '^/v2/enqueue$' "$rep/posts" || fail "the reporter reached no PagerDuty endpoint"
# The heartbeat pings (POST /hb) are not alerts: counted apart.
alerts_sent() { { grep -v '^/hb$' "$rep/posts" || true; } | wc -l; }
sent=$(alerts_sent)
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $line >/dev/null || fail "the reporter's second run failed"
test "$(alerts_sent)" = "$sent" || fail "the reporter's second run sent its alerts again"
test "$(grep -c '^/hb$' "$rep/posts")" = 2 || fail "the reporter did not ping the outside heartbeat once per clean firing (stand-in endpoint saw $(grep -c '^/hb$' "$rep/posts"))"
pass "the reporter runs as its unit runs it, as ti-trader, delivers to both channels once, keeps its state, and pings the outside heartbeat once per firing"

# The failed-unit page: the hook unit's own command line, for the engine's
# failure, as ti-trader against the same stand-in channels, with the failed
# run's name in MONITOR_UNIT and MONITOR_INVOCATION_ID as systemd hands it (no
# systemd runs here; deploy/rehearsal/inside-systemd.sh starts the hook under
# a real one). It pages both channels once and sends nothing on a second run
# for the same failed run; the record sits beside the reporter's state.
hook=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-unit-failed@.service)
hook=${hook//%i/ti-engine@boost.service}
hook=${hook//%d/$rep/creds}
hook=${hook//\/var\/lib\/trend-investing\/reports/$rep/reports}
case $hook in *'$'* | *%*) fail "the hook's ExecStart has a variable the rehearsal does not expand: $hook" ;; esac
before=$(alerts_sent)
failed_run=(MONITOR_UNIT=ti-engine@boost.service MONITOR_INVOCATION_ID=0123456789abcdef0123456789abcdef)
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH=/usr/bin:/bin "${failed_run[@]}" $hook >/dev/null || fail "the failed-unit hook, run as its unit runs it (as ti-trader), failed"
test "$(alerts_sent)" = "$((before + 2))" || fail "the failed-unit hook did not page both channels once"
grep -q 'ti-engine@boost.service failed' "$rep/posts.bodies" || fail "the hook's page does not name the failed unit"
test -s "$rep/reports/unit-failures.json" || fail "the hook wrote no record of the page"
grep -q 'unit-failed:ti-engine@boost.service/0123456789abcdef0123456789abcdef' "$rep/reports/unit-failures.json" || fail "the hook's record is not keyed by the failed run"
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH=/usr/bin:/bin "${failed_run[@]}" $hook >/dev/null || fail "the failed-unit hook's second run failed"
test "$(alerts_sent)" = "$((before + 2))" || fail "the failed-unit hook paged again for the same failure"
for unit in ti-engine@.service ti-adapter-boost.service ti-reporter@.service; do
  grep -qx 'OnFailure=ti-unit-failed@%n.service' "/etc/systemd/system/$unit" || fail "$unit does not hand its failure to the hook"
done
pass "the failed-unit hook runs as its unit runs it, as ti-trader, pages both channels once per failed run and not again, and the three trading units name it in OnFailure="

# The clock check. The reporter ran the installed check as ti-trader above:
# the host clock state must hold what the check said, not that it could not
# run (missing, or not executable by ti-trader).
clock_state=$rep/reports/host-clock.json
test -s "$clock_state" || fail "the reporter wrote no host clock state"
python3 - "$clock_state" <<'PY' || fail "the installed clock check did not run as ti-trader: $(cat "$clock_state")"
import json, sys
out = json.load(open(sys.argv[1]))["last"]["Output"]
sys.exit(0 if out.startswith("clock: ") else 1)
PY
# Then -clock-check with a stand-in: a failing check pages on the second
# failure in a row, once for the host, and a passing one resolves the
# PagerDuty incident. Each run's check is aged past the timer period so the
# next run checks again.
stand=$(mktemp -d)
chmod 0755 "$stand"
printf '#!/bin/sh\nif test -e %s/fail; then echo "clock: skew of -42s against https://reference.test (this clock minus the reference), more than the 2s tolerance." >&2; exit 1; fi\necho "clock: ok, skew 0s against https://reference.test, within 2s"\n' "$stand" >"$stand/check.sh"
chmod 0755 "$stand/check.sh"
stand_line=${line//\/usr\/local\/libexec\/trend-investing\/clock-check.sh/$stand/check.sh}
age_check() {
  python3 - "$clock_state" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
s["last"]["At"] = "2000-01-01T00:00:00Z"
json.dump(s, open(sys.argv[1], "w"))
PY
}
clock_pages() { grep -c 'clock-drift:.*"event_action":"trigger"\|"event_action":"trigger".*clock-drift:' "$rep/posts.bodies" || true; }
# The real check's own count (it may fail here with no network) is cleared.
rm -f "$clock_state"
touch "$stand/fail"
for n in 1 2; do
  test ! -e "$clock_state" || age_check
  # shellcheck disable=SC2086
  sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $stand_line >/dev/null || fail "the reporter with a failing stand-in clock check failed to run"
  if test "$n" = 1 && test "$(clock_pages)" != 0; then fail "one failing clock check paged"; fi
done
test "$(clock_pages)" = 1 || fail "two failing clock checks in a row did not page once on PagerDuty"
grep -q -- '-42s' "$rep/posts.bodies" || fail "the clock page lacks the measured skew"
rm "$stand/fail"
age_check
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $stand_line >/dev/null || fail "the reporter with a passing stand-in clock check failed"
trigger_key=$(python3 - "$rep/posts.bodies" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    try:
        b = json.loads(line)
    except ValueError:
        continue
    if isinstance(b, dict) and b.get("event_action") == "trigger" and str(b.get("dedup_key", "")).startswith("clock-drift:"):
        print(b["dedup_key"])
        break
PY
)
test -n "$trigger_key" || fail "the clock page's dedup key could not be read from the posts"
grep -qF "\"event_action\":\"resolve\",\"dedup_key\":\"$trigger_key\"" "$rep/posts.bodies" || fail "a passing clock check did not resolve the PagerDuty incident"
rm -rf "$stand"
kill "$fake"
pass "the reporter runs the installed clock check as ti-trader, pages a failing stand-in on the second failure, and resolves it on a pass"

# Unit files. The engine's binary is installed by hand on the server; the
# rehearsal installs the stamped engine deploy/rehearse.sh built (the
# operator-inbox check below runs it).
install -m 0755 /root/dist/ti-engine /opt/trend-investing/bin/ti-engine
# The adapter's environment and code (provision.sh twsapi and adapter).
install -d /opt/trend-investing/adapter-venv/bin /opt/trend-investing/adapter
test -x /opt/trend-investing/adapter-venv/bin/python3 || install -m 0755 /dev/null /opt/trend-investing/adapter-venv/bin/python3
cd /etc/systemd/system
verify_log=$(mktemp)
verify_status=0
systemd-analyze verify ti-gateway.service ti-data.service ti-data.timer \
  ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service \
  ti-reporter@boost.service ti-reporter@boost.timer \
  ti-unit-failed@ti-engine@boost.service.service ti-unit-failed@ti-adapter-boost.service.service ti-unit-failed@ti-reporter@boost.service.service \
  ti-dashboard.service ti-adapter-boost.service ti-ntfy.service \
  ti-flex@boost.service ti-flex@boost.timer ti-flex@trad-ira.service ti-flex@trad-ira.timer \
  ti-flex@roth-ira.service ti-flex@roth-ira.timer >"$verify_log" 2>&1 || verify_status=$?
cat "$verify_log"
# Every line is a finding: no warning is expected in this container (the
# rehearsal of 2026-10-16 printed none), so nothing is filtered out, and a
# parser diagnostic such as an invalid LoadCredential= source cannot hide. If
# a container-only warning ever appears, allow it here by its exact full line
# (grep -vxF), never by a substring.
if test "$verify_status" -ne 0 || test -s "$verify_log"; then
  fail "systemd-analyze verify exited $verify_status or reported problems (above)"
fi
pass "systemd-analyze verify clean on all units"

# The check above can fail: a unit with an invalid credential name must
# produce a diagnostic, or an empty log would prove nothing.
broken=$(mktemp -d)
sed 's|^LoadCredential=ibkr-login:|LoadCredential=bad/name:|' ti-gateway.service >"$broken/ti-gateway.service"
grep -q '^LoadCredential=bad/name:' "$broken/ti-gateway.service" || fail "could not break the test unit"
systemd-analyze verify "$broken/ti-gateway.service" >"$verify_log" 2>&1 || true
cat "$verify_log"
grep -qi 'credential' "$verify_log" || fail "systemd-analyze verify is silent on an invalid LoadCredential= name"
pass "systemd-analyze verify reports an invalid LoadCredential= name"

# The operator inbox (ADR 0026, kill-switch amendment; docs/manual/operator.md,
# "Who can write an instruction"). The instruction's writer is a user of its
# own, ti-operator, with no login shell, in the engine's group; the
# provisioner creates one inbox per account, owned ti-operator:ti-trader, mode
# 0750, so the engine (ti-trader) reads it through its group and cannot write
# it.
test "$(getent passwd ti-operator | cut -d: -f7)" = /usr/sbin/nologin || fail "ti-operator is missing or has a login shell"
test "$(id -gn ti-operator)" = ti-trader || fail "ti-operator's group is not the engine's, ti-trader"
# A server provisioned before ti-operator existed gains it on a services
# upgrade: remove the user and rerun the step.
userdel ti-operator || fail "could not remove ti-operator to test the upgrade"
"$p" services >/dev/null 2>&1 || fail "the services step fails on a server without ti-operator"
test "$(id -gn ti-operator)" = ti-trader || fail "the services step did not create ti-operator"
# A ti-operator made another way, in a group of its own, gets the engine's
# group back from the services step.
groupadd ti-other || fail "could not create a stand-in group"
usermod -g ti-other ti-operator || fail "could not move ti-operator to the stand-in group"
"$p" services >/dev/null 2>&1 || fail "the services step fails on a ti-operator in another group"
test "$(id -gn ti-operator)" = ti-trader || fail "the services step left ti-operator outside ti-trader"
groupdel ti-other || fail "could not remove the stand-in group"
for a in boost trad-ira roth-ira; do
  inbox=/var/lib/trend-investing/engines/$a/operator
  test "$(stat -c %U:%G:%a "$inbox")" = ti-operator:ti-trader:750 || fail "the $a operator inbox is not ti-operator:ti-trader 0750"
done
# shellcheck disable=SC2016
grep -q -- '-operator ${TI_STATE_DIR}/operator' /etc/systemd/system/ti-engine@.service || fail "the engine unit does not pass -operator"
for unit in ti-adapter-boost.service ti-data.service ti-reporter@.service ti-unit-failed@.service ti-dashboard.service ti-flex@.service; do
  grep -q '^InaccessiblePaths=.*-/var/lib/trend-investing/engines/boost/operator' "/etc/systemd/system/$unit" || fail "$unit can reach the operator inbox"
done
pass "ti-operator writes the instructions: each account's inbox is ti-operator:ti-trader 0750, the engine unit passes it, and no network-facing unit can reach it"

# The engine, started with its unit's own command line (expanded as systemd
# would: %i, %t and the unit's variables), as ti-trader, refuses an inbox it
# could write to itself (its group's or others' to write, root's, or its own
# user's) and a missing one.
line=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-engine@.service)
line=${line//%i/boost}
line=${line//%t//run}
line=${line//"\${TI_MODE}"/dry-run}
line=${line//"\${TI_CONFIG}"/\/etc\/trend-investing\/engines\/boost.config}
line=${line//"\${TI_STATE_DIR}"/\/var\/lib\/trend-investing\/engines\/boost}
line=${line//"\${TI_FLEX_DIR}"/\/var\/lib\/trend-investing\/flex\/boost}
line=${line//"\${TI_RUN}"/run-rehearsal}
as_of=$(sed -n 's/^TI_AS_OF=//p' /root/deploy/etc/engines/run.env.example)
line=${line//"\${TI_AS_OF}"/$as_of}
# shellcheck disable=SC2016
line=${line//'$TI_RESTART_ARGS'/}
case $line in *'$'* | *%*) fail "the engine's ExecStart has a variable the rehearsal does not expand: $line" ;; esac
state=/var/lib/trend-investing/engines/boost
inbox=$state/operator
notes=$state/operator-notes
install -d -o ti-trader -g ti-trader -m 0700 /run/ti-engine-boost
refused_start() { # <what> <expected text>: the engine must refuse to start, saying so
  local out
  # shellcheck disable=SC2086
  if out=$(sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York timeout 20 $line 2>&1); then
    fail "the engine started with $1"
  fi
  case $out in *"$2"*) ;; *) fail "the engine's refusal of $1 does not say \"$2\": $out" ;; esac
}
chmod 0770 "$inbox"
refused_start "an inbox its group can write" "writable by its group or by others"
chmod 0750 "$inbox"
chown ti-trader "$inbox"
refused_start "an inbox its own user owns" "the engine's own user"
chown root "$inbox"
refused_start "an inbox root owns" "owned by root"
chown ti-operator "$inbox"
mv "$inbox" "$inbox.away"
refused_start "no operator inbox" "operator inbox"
mv "$inbox.away" "$inbox"
test "$(stat -c %U:%G:%a "$inbox")" = ti-operator:ti-trader:750 || fail "the inbox was not restored"
rm -rf "$state"/run-rehearsal.* "$notes"
pass "the engine refuses an operator inbox that is missing, writable by its group, or owned by root or by the engine's own user"

# The kill switch takes effect. A stand-in adapter feeds the real engine one
# Boost session at the run's as-of close (the bar, the account snapshot of
# $4,000 in cash, the mark and the close, with the run's own configuration
# hash and strategy version): with no instruction, Boost's entry to 98% is
# released; with the owner's kill switch engaged first, typed as the owner
# types it (sudo -u ti-operator operator ...), the same entry is refused for
# the kill switch and nothing is released. The owner's acknowledgement is
# journalled with its alert key. The engine notes each record delivered in
# its own operator-notes folder, refuses none, and writes nothing into the
# inbox; the engine's user cannot record an instruction.
feed=$(mktemp)
cat >"$feed" <<'FEED'
import hashlib
import json
import socket
import sys

log, at = sys.argv[1], sys.argv[2]
with open(log) as f:
    header = json.loads(f.readline())
sock = socket.socket(socket.AF_UNIX)
sock.settimeout(20)
sock.connect("/run/ti-engine-boost/engine.sock")
replies = sock.makefile()
sequence = 2  # the run's configuration is input 1


def send(kind, version, ident, payload):
    global sequence
    raw = json.dumps(payload, separators=(",", ":"))
    envelope = {"id": ident, "type": kind, "envelope_version": 1, "schema_version": version,
                "event_time": at, "recorded_at": at, "sequence": sequence, "source": "rehearsal-adapter",
                "strategy_version": header["strategy_version"], "configuration_hash": header["configuration_hash"],
                "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": json.loads(raw)}
    sock.sendall((json.dumps(envelope, separators=(",", ":")) + "\n").encode())
    frame = json.loads(replies.readline())
    reply = frame.get("envelope")
    if not reply or reply.get("type") != "engine.decisions":
        sys.exit("the engine refused %s: %s" % (ident, frame))
    # The decisions carry the last sequence this exchange recorded, after any
    # operator input recorded first; the next input continues it.
    sequence = reply["sequence"] + 1
    for d in reply["payload"].get("decisions") or []:
        print(d["type"], d["payload"].get("reason", d["payload"].get("side", "")))


def view(label):
    return {"view": label, "open": 400.0, "high": 400.0, "low": 400.0, "close": 400.0, "volume": 1e6}


send("market.bar.completed", 2, "bar:rehearsal", {"instrument_id": "SPY", "period_end": at, "split_adjusted": view("split-adjusted"),
     "raw": view("raw"), "adjusted": view("split-and-dividend-adjusted"), "adjusted_base": "2024-01-02"})
send("account.snapshot", 3, "snapshot:rehearsal", {"account_id": "joint", "as_of": at, "net_liquidation": 4000.0, "currency": "USD", "positions": []})
send("market.mark.recorded", 1, "mark:rehearsal", {"instrument_id": "SPY", "as_of": at, "price": 400.0, "source": "ibkr"})
send("market.session.closed", 1, "session:rehearsal", {"period_end": at, "instrument_ids": ["SPY"]})
FEED
chmod 0644 "$feed"
eng_out=$(mktemp)
start_engine() {
  # The output file is root's on purpose; the engine writes to the inherited descriptor.
  # shellcheck disable=SC2086,SC2024
  sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York $line >"$eng_out" 2>&1 &
  engine=$!
  for _ in $(seq 100); do
    test -S /run/ti-engine-boost/engine.sock && break
    sleep 0.1
  done
  test -S /run/ti-engine-boost/engine.sock || { cat "$eng_out"; fail "the engine did not listen on its socket"; }
}
stop_engine() {
  kill -TERM "$engine"
  wait "$engine" || true
}
start_engine
decided=$(sudo -u ti-trader python3 "$feed" "$state/run-rehearsal.log" "$as_of") || { cat "$eng_out"; fail "the stand-in adapter's session was refused: $decided"; }
stop_engine
case $decided in *"risk.order.released buy"*) ;; *) fail "with no kill switch, the session released nothing: $decided" ;; esac
rm -rf "$state"/run-rehearsal.* "$notes" /run/ti-engine-boost/engine.sock

alert='engine-state:halted:rehearsal@joint'
start_engine
if sudo -u ti-trader operator kill-switch -inbox "$inbox" -account joint -state released -by rehearsal -detail "the engine's user" >/dev/null 2>&1; then
  fail "the engine's user recorded an instruction"
fi
sudo -u ti-operator operator kill-switch -inbox "$inbox" -account joint -state engaged -by rehearsal -detail "rehearsal kill switch" >/dev/null || fail "operator kill-switch failed as ti-operator"
sudo -u ti-operator operator acknowledge -inbox "$inbox" -account joint -alert "$alert" -by rehearsal -detail "rehearsal acknowledgement" >/dev/null || fail "operator acknowledge failed as ti-operator"
decided=$(sudo -u ti-trader python3 "$feed" "$state/run-rehearsal.log" "$as_of") || { cat "$eng_out"; fail "the stand-in adapter's session was refused: $decided"; }
stop_engine
case $decided in *"risk.order.refused kill-switch"*) ;; *) cat "$eng_out"; fail "with the kill switch engaged, the entry was not refused for it: $decided" ;; esac
case $decided in *"risk.order.released"*) fail "an order was released with the kill switch engaged: $decided" ;; esac
for kind in kill-switch alert.acknowledged; do
  test -n "$(ls "$notes"/*-operator.$kind.json.delivered 2>/dev/null)" || fail "no delivered note for the operator.$kind record in $notes"
  grep -q "recorded operator input operator.$kind:joint:" "$eng_out" || fail "the engine did not log the operator.$kind input"
done
test -z "$(ls "$notes"/*.refused 2>/dev/null)" || fail "the engine refused an operator record: $(cat "$notes"/*.refused)"
if grep -q 'refused operator record' "$eng_out"; then fail "the engine refused an operator record: $(cat "$eng_out")"; fi
test "$(stat -c %U:%a "$notes")" = ti-trader:700 || fail "the engine's notes folder is not its own, 0700"
test "$(find "$inbox" -mindepth 1 | wc -l)" -eq 2 || fail "the inbox holds more than the owner's two records: $(ls -A "$inbox")"
query=$(sudo -u ti-trader journal-query orders "$state/run-rehearsal.log")
case $query in *refused*kill-switch*) ;; *) fail "journal-query orders does not show the entry refused for the kill switch: $query" ;; esac
query=$(sudo -u ti-trader journal-query operator "$state/run-rehearsal.log")
case $query in *engaged*"rehearsal kill switch"*) ;; *) fail "journal-query operator does not list the kill switch: $query" ;; esac
python3 - "$state/run-rehearsal.log" "$alert" <<'ACK' || fail "the durable log does not hold the acknowledgement of $alert"
import json
import sys

log, alert = sys.argv[1], sys.argv[2]
with open(log) as f:
    rows = [json.loads(line) for line in f][1:]
acks = [r["record"]["envelope"]["payload"] for r in rows if "record" in r and r["record"]["kind"] == "input"
        and r["record"]["envelope"]["type"] == "operator.alert.acknowledged"]
sys.exit(0 if [a for a in acks if a["alert_key"] == alert] else 1)
ACK
rm -rf "$state"/run-rehearsal.* "${inbox:?}"/* "$notes" /run/ti-engine-boost "$feed" "$eng_out"
pass "the owner's kill switch, recorded as ti-operator, makes the engine refuse the entry it otherwise releases; the acknowledgement is journalled with its alert key; the engine notes both in its own folder and writes nothing into the inbox"

# Client IDs are unique. The data step uses Massive, not IB Gateway, and
# holds none.
ids=$(grep -h '^TI_ADAPTER_CLIENT_ID=' /etc/trend-investing/engines/*.env | sort)
test "$(echo "$ids" | uniq | wc -l)" -eq 3 || fail "engine client IDs not unique"
grep -q 'TI_ADAPTER_CLIENT_ID' /etc/systemd/system/ti-data.service && fail "the data step names an IB Gateway client ID"
pass "three distinct engine client IDs, and none for the data step"

# The adapter's state directory and its configuration, created once and kept.
test "$(stat -c %U:%a /var/lib/trend-investing/adapters/boost)" = ti-trader:750 || fail "adapter state directory not ti-trader 0750"
for a in boost trad-ira roth-ira; do
  test "$(stat -c %U:%a "/var/lib/trend-investing/flex/$a")" = ti-trader:750 || fail "the $a Flex reader's ledger directory not ti-trader 0750"
done
# One templated Flex reader for every account; the Boost-only pair is gone.
test -f /etc/systemd/system/ti-flex@.service && test -f /etc/systemd/system/ti-flex@.timer || fail "ti-flex@ unit or timer not installed"
test ! -e /etc/systemd/system/ti-flex-boost.service && test ! -e /etc/systemd/system/ti-flex-boost.timer || fail "the Boost-only Flex reader unit is still installed"
grep -q '"paper_sending_enabled": false' /etc/trend-investing/adapters/boost.json || fail "adapter configuration not created from the example"
pass "adapter state directory and configuration in place, sending off"
echo "REHEARSAL COMPLETE"
