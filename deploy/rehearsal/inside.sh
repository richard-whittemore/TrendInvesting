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
"$p" gateway
"$p" services
pass "first full pass"

# Idempotence: a second pass changes nothing and must not touch owner-entered stubs.
printf 'TWS_USERID=sentinel\n' >>/etc/trend-investing/ibkr-login.env
"$p" base
"$p" gateway
"$p" services
grep -q sentinel /etc/trend-investing/ibkr-login.env || fail "re-run overwrote the owner's login file"
pass "second pass idempotent, owner stub preserved"
sed -i '/sentinel/d' /etc/trend-investing/ibkr-login.env

# Users and permissions.
for u in ti-gateway ti-trader; do
  id "$u" >/dev/null || fail "missing user $u"
  test "$(getent passwd "$u" | cut -d: -f7)" = /usr/sbin/nologin || fail "$u has a login shell"
  test "$(id -u "$u")" -ne 0 || fail "$u is uid 0"
done
for f in ibkr-login.env secrets.env; do
  test "$(mode_of "/etc/trend-investing/$f")" = 600 || fail "$f is not mode 600"
  test "$(stat -c %U:%G "/etc/trend-investing/$f")" = root:root || fail "$f is not root-owned"
done
pass "users are non-root, nologin; secret stubs are root:root 0600"

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

# Unit files. The placeholder binaries do not exist; stand-ins let
# systemd-analyze check the rest of each unit.
for b in ti-engine ti-reporter ti-dashboard; do
  install -m 0755 /dev/null "/opt/trend-investing/bin/$b"
done
cd /etc/systemd/system
verify_log=$(mktemp)
verify_status=0
systemd-analyze verify ti-gateway.service ti-data.service ti-data.timer \
  ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service \
  ti-reporter.service ti-reporter.timer ti-dashboard.service >"$verify_log" 2>&1 || verify_status=$?
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

# Client IDs are unique. The data step uses Massive, not IB Gateway, and
# holds none.
ids=$(grep -h '^TI_ADAPTER_CLIENT_ID=' /etc/trend-investing/engines/*.env | sort)
test "$(echo "$ids" | uniq | wc -l)" -eq 3 || fail "engine client IDs not unique"
grep -q 'TI_ADAPTER_CLIENT_ID' /etc/systemd/system/ti-data.service && fail "the data step names an IB Gateway client ID"
pass "three distinct engine client IDs, and none for the data step"
echo "REHEARSAL COMPLETE"
