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

# The secret files rest on disk only encrypted (ADR 0028).
# The owner types the passphrase; here it comes on a test-only descriptor,
# which ti-secrets honours only where TI_REHEARSAL=1, as this container sets.
test "${TI_REHEARSAL:-}" = 1 || fail "the rehearsal container does not set TI_REHEARSAL"
# ti-secrets refuses to run while any swap is on, and Docker's virtual
# machine has some: the swap list it reads is a stand-in with none
# (TI_PROC_SWAPS, also honoured only where TI_REHEARSAL=1).
printf 'Filename\tType\tSize\tUsed\tPriority\n' >/root/ti-no-swaps
export TI_PROC_SWAPS=/root/ti-no-swaps
passphrase="rehearsal passphrase $(head -c 9 /dev/urandom | od -An -tx1 | tr -d ' \n')"
with_pass() { TI_PASSPHRASE_FD=7 "$@" 7< <(yes "$passphrase"); }
# A server provisioned before the secrets were encrypted: its secrets.env is
# in plain text, with a marker the first pass must migrate off the disk.
marker=marker-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
install -d -m 0755 /etc/trend-investing
install -m 0600 /dev/null /etc/trend-investing/secrets.env
sed "s/^TI_IBKR_FLEX_TOKEN=\$/TI_IBKR_FLEX_TOKEN=$marker/" /deploy/etc/secrets.env.example >/etc/trend-investing/secrets.env
grep -qx "TI_IBKR_FLEX_TOKEN=$marker" /etc/trend-investing/secrets.env || fail "could not write the earlier server's secrets file"
unlocked=/run/trend-investing/secrets

"$p" base
"$p" clock
"$p" gateway
with_pass "$p" services
pass "first full pass"
test ! -e /etc/trend-investing/secrets.env || fail "the first pass left the earlier server's secrets.env on disk in plain text"

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

# Idempotence: a second pass changes nothing, asks for no passphrase (it is
# given none) and must not touch the owner's encrypted files.
# The owner names the IRA's account in its configuration on the server, as
# the runbook does (a rehearsal ID, never a real one); the second pass must
# keep that edit byte for byte and put the shipped file beside it (docs/live-trading/lessons-2026-10.md
# L7).
secret_sums() { sha256sum /etc/trend-investing/*.gpg /etc/trend-investing/accounts/*.gpg; }
ira_config=/etc/trend-investing/engines/trad-ira.config
grep -q '"account_id": "trad-ira"' "$ira_config" || fail "the first pass did not install the shipped trad-ira.config"
sed -i 's/"account_id": "trad-ira"/"account_id": "DUREHEARS1"/' "$ira_config"
edited=$(sha256sum "$ira_config")
# A settings file the owner edited by hand (a line the shipped one lacks)
# survives too, the shipped one beside it.
echo "TI_OWNER_MARKER=kept" >>/etc/trend-investing/engines/trad-ira.env
edited_env=$(sha256sum /etc/trend-investing/engines/trad-ira.env)
before=$(secret_sums)
"$p" base
"$p" clock
"$p" gateway
"$p" services </dev/null
test "$(secret_sums)" = "$before" || fail "re-run changed the owner's encrypted secret files"
test "$(sha256sum "$ira_config")" = "$edited" || fail "re-run changed the account ID the owner named in trad-ira.config"
cmp -s /root/deploy/etc/engines/trad-ira.config "$ira_config.example" || fail "re-run did not put the shipped trad-ira.config beside the owner's as trad-ira.config.example"
test "$(sha256sum /etc/trend-investing/engines/trad-ira.env)" = "$edited_env" || fail "re-run changed the trad-ira.env the owner edited (its marker line)"
cmp -s /root/deploy/etc/engines/trad-ira.env /etc/trend-investing/engines/trad-ira.env.example || fail "re-run did not put the shipped trad-ira.env beside the owner's as trad-ira.env.example"
pass "second pass idempotent, owner's encrypted files, edited account configuration and edited settings file preserved, no passphrase asked"

# The encrypted files, ti-unlock and ti-secrets (deploy/rehearsal/inside-secrets.sh).
TI_TEST_PASS=$passphrase TI_TEST_MARKER=$marker bash /root/deploy/rehearsal/inside-secrets.sh

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
  test ! -e "/etc/trend-investing/$f" || fail "$f is on disk in plain text"
  test "$(mode_of "/etc/trend-investing/$f.gpg")" = 600 || fail "$f.gpg is not mode 600"
  test "$(stat -c %U:%G "/etc/trend-investing/$f.gpg")" = root:root || fail "$f.gpg is not root-owned"
done
pass "users are non-root, nologin; the secret files are encrypted only, root:root 0600"

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
runuser -u ti-gateway -- test -r /opt/ibc/config.ini || fail "IBC config.ini is not readable by ti-gateway"
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
grep -qx 'LoginDialogDisplayTimeout=60' "$run/config.ini" || fail "login dialog timeout not rendered"
grep -qx 'AcceptIncomingConnectionAction=reject' "$run/config.ini" || fail "incoming API connections are not rejected"
grep -qx 'TrustedTwsApiClientIPs=' "$run/config.ini" || fail "trusted API client addresses not empty"
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
grep -qx 'RestartPreventExitStatus=64 65 77 78' /etc/systemd/system/ti-gateway.service || fail "ti-gateway restarts on a configuration exit"
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
grep -qx 'MASSIVE_API_KEY=' "$unlocked/secrets.env" || fail "the unlocked secrets file has no MASSIVE_API_KEY line"
scratch=$(mktemp -d)
cp /root/dist/boost-fixture.log "$scratch/run.log"
status=0
/opt/trend-investing/bin/ti-data --data "$scratch/data" --config /etc/trend-investing/engines/boost.config \
  --engine-log "$scratch/run.log" --secrets-file "$unlocked/secrets.env" >/dev/null 2>"$scratch/err" || status=$?
test "$status" = 1 || fail "the data step exited $status with no key, want 1"
grep -q 'ti-data refused: .*MASSIVE_API_KEY' "$scratch/err" || fail "the data step's refusal does not name the missing key"
test ! -e "$scratch/data" || fail "the data step wrote something while refusing"
pass "the data step is installed, runs, and refuses a missing key with nothing written"

# The reporter, the dashboard and the owner's two tools are installed from
# the build beside deploy/, and the tools are on sudo's PATH for ti-trader,
# as the runbooks run them.
for b in ti-reporter ti-dashboard operator journal-query ti-gateway-start universe-rank; do
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
pass "reporter, dashboard, operator, journal-query and universe-rank installed; the tools run under sudo -u ti-trader"

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
printf '{"keys":[],"baselines":{"ntfy":"2000-01-01T00:00:00Z","pushover":"2000-01-01T00:00:00Z","github-issues":"2000-01-01T00:00:00Z"}}' >"$rep/reports/boost.reporter.json"
cp "$rep/reports/boost.reporter.json" "$rep/reports/boost.reporter.json.seed"
chown -R ti-trader:ti-trader "$rep"
chmod 0750 "$rep/engines" "$rep/engines/boost" "$rep/reports"
python3 - "$rep/posts" <<'PY' &
import http.server
import sys

posts = sys.argv[1]


sys.path.insert(0, "/deploy/rehearsal")
import github_standin


def github_log(line, body):
    # The alerts repository's requests are kept apart from the other channels'.
    with open(posts + ".github", "a") as f:
        f.write(line + "\n")


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if not github_standin.handle(self, "GET", b"", github_log):
            self.send_response(404)
            self.end_headers()

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        github_standin.handle(self, "PATCH", body, github_log)

    def do_DELETE(self):
        github_standin.handle(self, "DELETE", b"", github_log)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if github_standin.handle(self, "POST", body, github_log):
            return
        with open(posts, "a") as f:
            f.write(self.path + "\n")
        with open(posts + ".bodies", "ab") as f:
            f.write(body.replace(b"\n", b" ") + b"\n")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":1,"request":"rehearsal","receipt":"rehearsalreceipt"}')

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", 8099), Handler).serve_forever()
PY
fake=$!
for _ in $(seq 50); do
  python3 -c 'import socket; socket.create_connection(("127.0.0.1", 8099), 1)' 2>/dev/null && break
  sleep 0.1
done
# The stand-in channels go into the encrypted secrets file as the owner puts
# them there (ti-secrets edit, its editor given the lines from a tmpfs), and
# the reporter is handed the unlocked copy its unit names in LoadCredential=,
# as systemd would hand it: a private copy, the unit's user's, read-only.
install -d -m 0700 /dev/shm/ti-edit
printf 'TI_NTFY_TOPIC=rehearsal\nTI_NTFY_SERVER=http://127.0.0.1:8099\nTI_PUSHOVER_APP_TOKEN=fake\nTI_PUSHOVER_USER_KEY=fake\nTI_PUSHOVER_URL=http://127.0.0.1:8099/1/messages.json\nTI_GITHUB_ISSUES_TOKEN=fakeghtoken\nTI_GITHUB_ISSUES_REPO=owner/repo\nTI_GITHUB_API_URL=http://127.0.0.1:8099/gh\nTI_GITHUB_API_ALLOW_HOST=127.0.0.1:8099\nTI_HEARTBEAT_URL=http://127.0.0.1:8099/hb\nMASSIVE_API_KEY=\n' >/dev/shm/ti-edit/content
EDITOR=/root/ti-editor.sh with_pass ti-secrets edit secrets.env >/dev/null || fail "ti-secrets edit could not set the reporter's channels"
rm -rf /dev/shm/ti-edit
source=$(sed -n 's/^LoadCredential=secrets://p' /etc/systemd/system/ti-reporter@.service)
test "$source" = "$unlocked/secrets.env" || fail "the reporter's unit loads its secrets from $source, not the unlocked copy"
install -o ti-trader -g ti-trader -m 0400 "$source" "$rep/creds/secrets"
line=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-reporter@.service)
line=${line//%i/boost}
line=${line//%d/$rep/creds}
# The unit file holds these as literal ${...} text; the backslash matches them verbatim.
line=${line//"\${TI_STATE_DIR}"/$rep/engines/boost}
line=${line//"\${TI_REPORTS_DIR}"/$rep/reports}
line=${line//"\${TI_RUN}"/run-rehearsal}
# The engine has run: its run.env, where the unit reads it, is the rehearsal's.
line=${line//"/var/lib/trend-investing/engines/boost/run.env"/$rep/engines/boost/run.env}
printf 'TI_RUN=run-rehearsal\n' >"$rep/engines/boost/run.env"
chown ti-trader:ti-trader "$rep/engines/boost/run.env"
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
echo NRestarts=0
echo SubState=running
echo InvocationID=0123456789abcdef0123456789abcdef
SH
chmod 0755 "$sysd/systemctl"
# Nor does a journal: the Gateway check reads ti-gateway's and the adapter's,
# so a stand-in journalctl answers with a completed login and a restored link.
cat >"$sysd/journalctl" <<'SH'
#!/bin/sh
echo "2026-01-01T00:00:00+0000 host ibc[1]: Login has completed"
echo "2026-01-01T00:00:00+0000 host adapter[2]: ibkr link state=connected reason=restored"
SH
chmod 0755 "$sysd/journalctl"
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $line >/dev/null || fail "the reporter, run as its unit runs it (as ti-trader), failed"
{ grep -qx 'show -p ActiveState -p InvocationID ti-engine@boost.service' "$sysd/calls" && grep -qx 'show -p ActiveState -p InvocationID ti-adapter-boost.service' "$sysd/calls"; } ||
  fail "the reporter did not ask systemd for the engine's and the adapter's state: $(cat "$sysd/calls")"
test -s "$rep/reports/boost.reporter.json" || fail "the reporter wrote no state file"
test "$(stat -c %U "$rep/reports/boost.reporter.json")" = ti-trader || fail "the reporter's state file is not ti-trader's"
grep -q '^/rehearsal$' "$rep/posts" || fail "the reporter reached no ntfy topic"
grep -q '^/1/messages.json$' "$rep/posts" || fail "the reporter reached no Pushover endpoint"
# The alerts repository's stand-in took an issue for a rehearsed alert, and
# only the reporter's own run made it (the unit names github-issues).
issues_opened() {
  if test -e "$rep/posts.github"; then grep -c '^POST /gh/repos/owner/repo/issues$' "$rep/posts.github" || true; else echo 0; fi
}
test "$(issues_opened)" -ge 1 || fail "the reporter opened no issue in the alerts repository (stand-in saw: $(cat "$rep/posts.github" 2>/dev/null || echo nothing))"
opened=$(issues_opened)
# Comments the reporter wrote on existing issues: a repeat of an alert must not
# arrive as a comment either.
comments_posted() {
  if test -e "$rep/posts.github"; then grep -c '^POST /gh/repos/owner/repo/issues/[0-9]*/comments$' "$rep/posts.github" || true; else echo 0; fi
}
commented=$(comments_posted)
# The heartbeat pings (POST /hb) are not alerts: counted apart.
alerts_sent() { { grep -v '^/hb$' "$rep/posts" || true; } | wc -l; }
sent=$(alerts_sent)
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $line >/dev/null || fail "the reporter's second run failed"
test "$(issues_opened)" = "$opened" || fail "the reporter's second run opened another issue"
test "$(comments_posted)" = "$commented" || fail "the reporter's second run commented on an issue again"
test "$(alerts_sent)" = "$sent" || fail "the reporter's second run sent its alerts again"
test "$(grep -c '^/hb$' "$rep/posts")" = 2 || fail "the reporter did not ping the outside heartbeat once per clean firing (stand-in endpoint saw $(grep -c '^/hb$' "$rep/posts"))"
pass "the reporter runs as its unit runs it, as ti-trader, delivers to ntfy, Pushover and the alerts repository's issues once, keeps its state, and pings the outside heartbeat once per firing"

# The failed-unit page: the hook unit's own command line, for the engine's
# failure, as ti-trader against the same stand-in channels, with the failed
# run's name in MONITOR_UNIT and MONITOR_INVOCATION_ID as systemd hands it (no
# systemd runs here; deploy/rehearsal/inside-systemd.sh starts the hook under
# a real one). It pages both channels once for the failure episode and sends
# nothing on a second run for the same failed run; the record sits beside the reporter's state.
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
test "$(issues_opened)" = "$((opened + 1))" || fail "the failed-unit hook did not open one issue in the alerts repository"
hook_comments=$(comments_posted)
grep -q 'ti-engine%40boost.service+failed' "$rep/posts.bodies" || fail "the hook's page does not name the failed unit"
test -s "$rep/reports/unit-failures.json" || fail "the hook wrote no record of the page"
grep -q 'unit-failed:ti-engine@boost.service/0123456789abcdef0123456789abcdef' "$rep/reports/unit-failures.json" || fail "the hook's record is not keyed by the failed run"
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH=/usr/bin:/bin "${failed_run[@]}" $hook >/dev/null || fail "the failed-unit hook's second run failed"
test "$(alerts_sent)" = "$((before + 2))" || fail "the failed-unit hook paged again for the same failure"
test "$(issues_opened)" = "$((opened + 1))" || fail "the failed-unit hook opened another issue for the same failure"
test "$(comments_posted)" = "$hook_comments" || fail "the failed-unit hook commented on an issue again for the same failure"
for unit in ti-engine@.service ti-adapter-boost.service ti-reporter@.service ti-data.service ti-data@.service; do
  grep -qx 'OnFailure=ti-unit-failed@%n.service' "/etc/systemd/system/$unit" || fail "$unit does not hand its failure to the hook"
done
pass "the failed-unit hook runs as its unit runs it, as ti-trader, pages both channels once per failure episode and not again, and the engine, adapter, reporter and data job units name it in OnFailure="

# The alerts repository not yet set up: the credential file with the token
# empty, as secrets.env.example ships it, and a state file recorded as above.
# The reporter sets only that channel aside by name and exits non-zero, so the
# unit fails and no heartbeat is pinged; ntfy and Pushover still deliver, and
# nothing reaches the alerts repository.
mkdir "$rep/creds-empty"
grep -v '^TI_GITHUB_ISSUES_TOKEN=' "$rep/creds/secrets" >"$rep/creds-empty/secrets"
echo 'TI_GITHUB_ISSUES_TOKEN=' >>"$rep/creds-empty/secrets"
cp "$rep/reports/boost.reporter.json.seed" "$rep/reports/empty.reporter.json"
empty_line=${line//"$rep/creds/"/"$rep/creds-empty/"}
empty_line=${empty_line//"$rep/reports/boost.reporter.json"/"$rep/reports/empty.reporter.json"}
chown -R ti-trader:ti-trader "$rep/creds-empty" "$rep/reports/empty.reporter.json"
chmod 0400 "$rep/creds-empty/secrets"
github_writes=$(wc -l <"$rep/posts.github")
ntfy_posts=$(grep -c '^/rehearsal$' "$rep/posts" || true)
pushover_posts=$(grep -c '^/1/messages.json$' "$rep/posts" || true)
pings=$(grep -c '^/hb$' "$rep/posts")
# shellcheck disable=SC2086
if empty_out=$(sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $empty_line 2>&1); then
  fail "the reporter with an empty GitHub token exited zero"
fi
case $empty_out in *"github-issues channel needs TI_GITHUB_ISSUES_TOKEN"*) ;; *) fail "the reporter with an empty GitHub token did not name the channel and the missing value" ;; esac
test "$(grep -c '^/rehearsal$' "$rep/posts" || true)" -gt "$ntfy_posts" || fail "the reporter with an empty GitHub token delivered nothing to ntfy"
test "$(grep -c '^/1/messages.json$' "$rep/posts" || true)" -gt "$pushover_posts" || fail "the reporter with an empty GitHub token delivered nothing to Pushover"
test "$(wc -l <"$rep/posts.github")" = "$github_writes" || fail "the reporter with an empty GitHub token wrote to the alerts repository"
test "$(grep -c '^/hb$' "$rep/posts")" = "$pings" || fail "the reporter with an empty GitHub token pinged the heartbeat"
pass "with the GitHub token empty the reporter sets only that channel aside by name, still delivers to ntfy and Pushover, exits non-zero and pings no heartbeat"

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
# failure in a row, once for the host. Each run's check is aged past the timer period so the
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
clock_pages() { grep -c 'priority=2.*clock-drift%3A\|clock-drift%3A.*priority=2' "$rep/posts.bodies" || true; }
# The real check's own count (it may fail here with no network) is cleared.
rm -f "$clock_state"
touch "$stand/fail"
for n in 1 2; do
  test ! -e "$clock_state" || age_check
  # shellcheck disable=SC2086
  sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $stand_line >/dev/null || fail "the reporter with a failing stand-in clock check failed to run"
  if test "$n" = 1 && test "$(clock_pages)" != 0; then fail "one failing clock check paged"; fi
done
test "$(clock_pages)" = 1 || fail "two failing clock checks in a row did not page once on Pushover"
grep -q -- '-42s' "$rep/posts.bodies" || fail "the clock page lacks the measured skew"
rm "$stand/fail"
age_check
# shellcheck disable=SC2086
sudo -u ti-trader env -i PATH="$sysd:/usr/bin:/bin" $stand_line >/dev/null || fail "the reporter with a passing stand-in clock check failed"
test "$(clock_pages)" = 1 || fail "a passing clock check paged again ($(clock_pages) pages)"
grep -qx '/1/receipts/rehearsalreceipt/cancel.json' "$rep/posts" || fail "a passing clock check did not cancel the Pushover emergency"
rm -rf "$stand"
kill "$fake"
pass "the reporter runs the installed clock check as ti-trader, pages a failing stand-in once, on the second failure, and cancels the page on a pass"

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
systemd-analyze verify ti-gateway.service ti-data.service ti-data.timer ti-data@trad-ira.service ti-data@trad-ira.timer \
  ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service \
  ti-reporter@boost.service ti-reporter@boost.timer \
  ti-unit-failed@ti-engine@boost.service.service ti-unit-failed@ti-adapter-boost.service.service ti-unit-failed@ti-reporter@boost.service.service \
  ti-unit-failed@ti-data@trad-ira.service.service \
  ti-dashboard.service ti-adapter-boost.service ti-adapter@trad-ira.service ti-ntfy.service \
  ti-unit-failed@ti-adapter@trad-ira.service.service \
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
for unit in ti-adapter-boost.service ti-adapter@.service ti-data.service ti-data@.service ti-reporter@.service ti-unit-failed@.service ti-dashboard.service ti-flex@.service; do
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

# The traditional IRA's engine starts in dry-run from its own settings and
# configuration, with its unit's own command line, and the synthetic month
# (cmd/engine/testdata/momentum-month.jsonl, copied in as dist/) is replayed
# into it by deploy/feed-month.py as the server's demonstration does: the
# month's decision is recorded withheld with its reason (the shipped rule
# wants 200 SPY closes and the ranked members' histories), nothing is sent,
# and SIGTERM stops the run cleanly, its journal written. The account was
# named above; the birth placeholder is named here as the owner names it, a
# rehearsal value. The shipped placeholder alone refuses to start.
ira_state=/var/lib/trend-investing/engines/trad-ira
ira_line=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-engine@.service)
ira_line=${ira_line//%i/trad-ira}
ira_line=${ira_line//%t//run}
ira_mode=$(sed -n 's/^TI_MODE=//p' /etc/trend-investing/engines/trad-ira.env)
test "$ira_mode" = dry-run || fail "trad-ira.env declares TI_MODE=$ira_mode, not dry-run"
ira_line=${ira_line//"\${TI_MODE}"/$ira_mode}
ira_line=${ira_line//"\${TI_CONFIG}"/$ira_config}
ira_line=${ira_line//"\${TI_STATE_DIR}"/$ira_state}
ira_line=${ira_line//"\${TI_FLEX_DIR}"/\/var\/lib\/trend-investing\/flex\/trad-ira}
ira_line=${ira_line//"\${TI_RUN}"/run-rehearsal}
ira_line=${ira_line//"\${TI_AS_OF}"/2025-01-02T21:00:00Z}
# shellcheck disable=SC2016
ira_line=${ira_line//'$TI_RESTART_ARGS'/}
case $ira_line in *'$'* | *%*) fail "the IRA engine's ExecStart has a variable the rehearsal does not expand: $ira_line" ;; esac
install -d -o ti-trader -g ti-trader -m 0700 /run/ti-engine-trad-ira
# shellcheck disable=SC2086
if out=$(sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York timeout 20 $ira_line 2>&1); then
  fail "the IRA engine started with its birth a placeholder"
fi
case $out in *birth*) ;; *) fail "the IRA engine's refusal of its placeholder birth does not name it: $out" ;; esac
rm -f "$ira_state"/run-rehearsal.*
sed -i 's/"birth": "YYYY-MM"/"birth": "1975-06"/' "$ira_config"
ira_out=$(mktemp)
# The output file is root's on purpose; the engine writes to the inherited descriptor.
# shellcheck disable=SC2086,SC2024
sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York $ira_line >"$ira_out" 2>&1 &
ira_engine=$!
for _ in $(seq 100); do
  test -S /run/ti-engine-trad-ira/engine.sock && break
  sleep 0.1
done
test -S /run/ti-engine-trad-ira/engine.sock || { cat "$ira_out"; fail "the IRA engine did not listen on its socket"; }
install -m 0644 /root/dist/momentum-month.jsonl /tmp/momentum-month.jsonl
install -m 0644 /root/deploy/feed-month.py /tmp/feed-month.py
decided=$(sudo -u ti-trader python3 /tmp/feed-month.py --socket /run/ti-engine-trad-ira/engine.sock --log "$ira_state/run-rehearsal.log" \
  --config "$ira_config" --month /tmp/momentum-month.jsonl) || { cat "$ira_out"; fail "the synthetic month was refused: $decided"; }
kill -TERM "$ira_engine"
wait "$ira_engine" || { cat "$ira_out"; fail "the IRA engine did not stop cleanly"; }
case $decided in *"strategy.targets.decided withheld"*) ;; *) fail "the month's decision was not recorded withheld: $decided" ;; esac
case $decided in *risk.order.released*) fail "the withheld month released an order: $decided" ;; esac
case $decided in *DUREHEARS1*) fail "the feed printed the account" ;; esac
test -s "$ira_state/run-rehearsal.journal" || { cat "$ira_out"; fail "the stopped IRA run wrote no journal"; }
grep -q "wrote $ira_state/run-rehearsal.journal" "$ira_out" || fail "the IRA engine did not report its journal: $(cat "$ira_out")"
rm -rf "$ira_state"/run-rehearsal.* "$ira_state/operator-notes" /tmp/momentum-month.jsonl /tmp/feed-month.py
pass "the traditional IRA's engine starts in dry-run from its own settings and configuration, refuses its placeholder birth, records the synthetic month's decision withheld and stops cleanly"

# The IRA's data job, run with ti-data@trad-ira's own command line (expanded
# as systemd would) as ti-trader, on synthetic recordings of the last 210
# sessions (deploy/rehearsal/ira-recordings.py; invented prices only): the
# engine is started once and stopped, so its durable log holds its
# configuration, as before the owner's warm-up; the warm-up then writes the
# run's first compact stream with the installed universe-rank, offline (every
# session, the universe, the sector labels and the action windows the job
# asks for are already recorded, so no request is made), and a second firing
# finds the file, re-encodes it, and prints the same path.
rm -rf "$ira_state"/run-rehearsal.* "$ira_state/operator-notes"
# shellcheck disable=SC2086,SC2024
sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York $ira_line >"$ira_out" 2>&1 &
ira_engine=$!
for _ in $(seq 100); do
  test -S /run/ti-engine-trad-ira/engine.sock && break
  sleep 0.1
done
test -S /run/ti-engine-trad-ira/engine.sock || { cat "$ira_out"; fail "the IRA engine did not listen on its socket for the data job's rehearsal"; }
kill -TERM "$ira_engine"
wait "$ira_engine" || { cat "$ira_out"; fail "the IRA engine did not stop cleanly"; }
ira_data=$(mktemp -d)
chown ti-trader:ti-trader "$ira_data"
install -m 0644 /root/deploy/rehearsal/ira-recordings.py /tmp/ira-recordings.py
base=$(sudo -u ti-trader python3 /tmp/ira-recordings.py /opt/trend-investing/bin/ti-data "$ira_data") || fail "the IRA's synthetic recordings were not written"
printf 'MASSIVE_API_KEY=rehearsal-not-a-key\n' >"$ira_data/secrets"
chown ti-trader:ti-trader "$ira_data/secrets"
chmod 0600 "$ira_data/secrets"
printf 'TI_RUN=run-rehearsal\nTI_AS_OF=2025-01-02T21:00:00Z\nTI_RESTART_ARGS=\n' >"$ira_state/run.env"
data_line=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-data@.service)
data_line=${data_line//%i/trad-ira}
data_line=${data_line//"%d/secrets"/$ira_data/secrets}
data_line=${data_line//"\${TI_DATA_DIR}"/$ira_data}
data_line=${data_line//"\${TI_RUN}"/run-rehearsal}
case $data_line in *'$'* | *%*) fail "the IRA data job's ExecStart has a variable the rehearsal does not expand: $data_line" ;; esac
# shellcheck disable=SC2086
warm=$(sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York $data_line --backfill-from "$base" 2>"$ira_out") || { cat "$ira_out"; fail "the IRA data job's warm-up failed"; }
case $warm in "$ira_data/streams/trad-ira/run-rehearsal/trad-ira-"*.jsonl) ;; *) cat "$ira_out"; fail "the IRA data job's warm-up wrote $warm" ;; esac
head -c 200 "$warm" | grep -q '"type":"market.session.calendar"' || fail "the IRA stream does not begin with its calendar"
grep -q "\"name\": \"$(basename "$warm")\"" "$(dirname "$warm")/manifest.json" || fail "the IRA job's manifest does not list the file it published"
test "$(grep -c '"type":"market.universe.snapshot"' "$warm")" = 1 || fail "the IRA stream does not hold the latest session's universe snapshot"
grep -q '"instrument_id":"SHY"' "$warm" || fail "the IRA stream has no SHY bars"
grep -q '"instrument_id":"R00","period_end"' "$warm" && fail "the IRA stream carries a bar for a stock it does not hold"
# shellcheck disable=SC2086
again=$(sudo -u ti-trader env -i PATH=/usr/bin:/bin TZ=America/New_York $data_line 2>"$ira_out") || { cat "$ira_out"; fail "the IRA data job's second firing failed"; }
test "$again" = "$warm" || fail "the second firing printed $again, not $warm"
rm -rf "$ira_data" "$ira_state"/run-rehearsal.* "$ira_state/operator-notes" "$ira_state/run.env" /tmp/ira-recordings.py
pass "the IRA's data job, run as ti-data@trad-ira runs it, writes the run's first compact stream with the installed universe-rank, and a second firing finds and re-verifies it"

# Client IDs are unique. The data step uses Massive, not IB Gateway, and
# holds none.
ids=$(grep -h '^TI_ADAPTER_CLIENT_ID=' /etc/trend-investing/engines/*.env | sort)
test "$(echo "$ids" | uniq | wc -l)" -eq 3 || fail "engine client IDs not unique"
grep -q 'TI_ADAPTER_CLIENT_ID' /etc/systemd/system/ti-data.service /etc/systemd/system/ti-data@.service && fail "the data step names an IB Gateway client ID"
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
cmp -s /root/deploy/etc/adapters/trad-ira.json.example /etc/trend-investing/adapters/trad-ira.json || fail "the IRA adapter's configuration not created from its example"
test "$(stat -c %U:%a /var/lib/trend-investing/adapters/trad-ira)" = ti-trader:750 || fail "the IRA adapter's state directory not ti-trader 0750"
# The Traditional IRA's Flex reader fires 15 minutes after Boost's (lessons L3).
test "$(grep -c '^OnCalendar=Mon\.\.Fri 0[567]:45 America/New_York$' /etc/systemd/system/ti-flex@trad-ira.timer.d/stagger.conf)" = 3 || fail "the IRA Flex reader's staggered times are not installed"
pass "adapter state directories and configurations in place (Boost's and the Traditional IRA's), sending off, and the IRA Flex reader's staggered times installed"
# ti-release, run against a local fake release (file://) on the programs
# installed above, with a stand-in systemctl (no systemd runs here) and no gh.
# Without gh it refuses, installing nothing; with --unverified (this container
# only) it installs, warning that the attestation was not checked. A release
# of different bytes installs over the installed ones and keeps them as the
# previous build; --rollback puts them back byte for byte; the engine is not
# restarted.
install -m 0755 /root/dist/ti-engine /opt/trend-investing/bin/ti-engine
rel=/root/fake-release
mkdir -p "$rel/v9.9.9"
for b in ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start; do
  { cat "/root/dist/$b"; printf 'rehearsal-release'; } >"$rel/v9.9.9/$b"
done
(cd "$rel/v9.9.9" && sha256sum ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start >checksums.txt && echo '{}' >attestation.sigstore.json)
ctl_stub=$(mktemp -d)
cat >"$ctl_stub/systemctl" <<'STUB'
#!/bin/bash
echo "$*" >>/root/release-ctl.log
exit 0
STUB
chmod +x "$ctl_stub/systemctl"
: >/root/release-ctl.log
export TI_RELEASE_URL=file:///root/fake-release TI_SYSTEMCTL="$ctl_stub/systemctl" TI_GH=/nonexistent/gh TI_RELEASE_ANY_ARCH=1
if /root/deploy/release.sh v9.9.9 >/root/release.out 2>&1; then fail "ti-release installed without gh"; fi
cmp "$rel/v9.9.9/ti-engine" /opt/trend-investing/bin/ti-engine >/dev/null 2>&1 && fail "ti-release without gh changed ti-engine"
test ! -e /opt/trend-investing/releases/current || fail "ti-release without gh recorded a build"
if env -u TI_REHEARSAL /root/deploy/release.sh --unverified v9.9.9 >/root/release.out 2>&1; then fail "ti-release --unverified ran without the rehearsal marker"; fi
test "${TI_REHEARSAL:-}" = 1 || fail "the rehearsal container does not set TI_REHEARSAL"
# Two minutes past a 5-minute mark: no wait for a quiet moment (the wait is
# covered by deploy/rehearsal/test-release.sh with a fixed clock).
export TI_RELEASE_NOW=120
marker_file=/run/ti-maintenance/maintenance.json
/root/deploy/release.sh --unverified v9.9.9 >/root/release.out 2>&1 || { cat /root/release.out; fail "ti-release --unverified v9.9.9 failed"; }
grep -q WARNING /root/release.out || fail "ti-release --unverified did not warn"
grep -q 'maintenance window open until' /root/release.out || fail "ti-release did not open a maintenance window: $(cat /root/release.out)"
test ! -e "$marker_file" || fail "ti-release left its maintenance window open: $(cat "$marker_file")"
for b in ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start; do
  cmp "$rel/v9.9.9/$b" "/opt/trend-investing/bin/$b" || fail "ti-release did not install $b"
done
test "$(cat /opt/trend-investing/releases/current)" = v9.9.9 || fail "ti-release did not record the current build"
grep -q '^restart ti-dashboard' /root/release-ctl.log || fail "ti-release did not restart the dashboard"
grep -Eq 'ti-engine@' /root/release-ctl.log && grep -E '^(restart|stop|start|kill).*ti-engine' /root/release-ctl.log && fail "ti-release touched the engine"
/root/deploy/release.sh --rollback >/root/release.out 2>&1 || { cat /root/release.out; fail "ti-release --rollback failed"; }
for b in ti-engine ti-reporter ti-dashboard operator journal-query ti-gateway-start; do
  cmp "/root/dist/$b" "/opt/trend-investing/bin/$b" || fail "rollback did not restore $b byte for byte"
done
grep -q 'maintenance window open until' /root/release.out || fail "ti-release --rollback did not open a maintenance window"
test ! -e "$marker_file" || fail "ti-release --rollback left its maintenance window open"
unset TI_RELEASE_NOW
pass "ti-release refuses without gh, installs a fake release with --unverified inside a maintenance window it opens and closes, restarts only the dashboard, and --rollback restores the previous build the same way"
for f in $(find / -xdev -type f -print0 2>/dev/null | xargs -0 grep -lF -- "$marker" 2>/dev/null || true); do
  fail "the migrated secret is on disk at the end: $f"
done
echo "REHEARSAL COMPLETE"
