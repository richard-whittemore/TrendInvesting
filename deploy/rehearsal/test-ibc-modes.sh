#!/bin/bash
# Tests for install_ibc_tree and fix_ibc_modes in deploy/provision.sh: the
# IBC tree is root's and readable by ti-gateway, whatever modes the zip or the
# umask gave its files; the gateway step repairs an existing tree without
# downloading again (the template holds no secrets).
# Unprivileged: chown is a stand-in that records its arguments; the stand-in
# zip's tree holds a 0600 config.ini, as the real one came out on the server.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'chmod -R u+rwX "$work" 2>/dev/null || true; rm -rf "$work"' EXIT
failures=0
expect() { # description expected actual
  if test "$2" != "$3"; then
    echo "FAIL: $1: want '$2', got '$3'" >&2
    failures=$((failures + 1))
  fi
}
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

fns=$(awk '/^(fix_ibc_modes|install_ibc_tree)\(\)/,/^}/' "$here/../provision.sh")
test -n "$fns" || { echo "FAIL: provision.sh has no install_ibc_tree" >&2; exit 1; }

src="$work/src"
mkdir -p "$src/scripts" "$src/sub"
echo 'LogToConsole=yes' >"$src/config.ini"
echo '#!/bin/sh' >"$src/scripts/ibcstart.sh"
echo '#!/bin/sh' >"$src/gatewaystart.sh"
echo 'x' >"$src/sub/data.txt"
chmod 0600 "$src/config.ini" "$src/scripts/ibcstart.sh" "$src/gatewaystart.sh" "$src/sub/data.txt"
chmod 0700 "$src/sub" "$src/scripts"

run() { # DEST FUNCTION ARGS...; chown is recorded, not run
  local dest=$1; shift
  bash -c '
    chown() { echo "chown $*" >>"$CHOWN_LOG"; }
    eval "$1"; shift; "$@"' _ "$fns" "$@" >"$work/out" 2>&1 || { cat "$work/out" >&2; return 1; }
}
dest="$work/ibc"
export CHOWN_LOG="$work/chown"
: >"$CHOWN_LOG"

( umask 077; run "$dest" install_ibc_tree "$src" "$dest" )
expect "config.ini is 0644" 644 "$(mode "$dest/config.ini")"
expect "a subfolder is 0755" 755 "$(mode "$dest/sub")"
expect "scripts folder is 0755" 755 "$(mode "$dest/scripts")"
expect "a plain file is 0644" 644 "$(mode "$dest/sub/data.txt")"
expect "a script is 0755" 755 "$(mode "$dest/scripts/ibcstart.sh")"
expect "the start script is 0755" 755 "$(mode "$dest/gatewaystart.sh")"
grep -q 'chown -R root:root' "$CHOWN_LOG" || expect "the tree is given to root" yes no
if test "$(id -u)" -ne 0; then
  # A user other than the owner can read it: the owner here stands for root,
  # so the check is on the "other" bits.
  expect "others can read config.ini" 4 "$(( $(mode "$dest/config.ini") % 10 & 4 ))"
fi

# A rerun goes through step_gateway itself: with the version marker present it
# must repair the tree in place and neither download nor unpack again. The
# function is run with its paths moved into the test's tree and every outside
# effect recorded.
chmod 0600 "$dest/config.ini"
chmod 0700 "$dest/sub"
gw="$work/gw"
mkdir -p "$gw/Jts/ibgateway/1050"
touch "$dest/.version-9.9.9" "$gw/Jts/ibgateway/1050/.installed-gwsha"
step=$(awk '/^step_gateway\(\)/,/^}/' "$here/../provision.sh" | sed -e "s#/opt/ibc#$dest#g" -e "s#/var/lib/ti-gateway#$gw#g")
test -n "$step" || { echo "FAIL: provision.sh has no step_gateway" >&2; exit 1; }
export EFFECTS="$work/effects"
: >"$EFFECTS"
bash -c '
  log() { :; }
  mac_mini() { return 1; }
  fetch() { echo "fetch $*" >>"$EFFECTS"; }
  unzip() { echo "unzip $*" >>"$EFFECTS"; }
  chown() { echo "chown $*" >>"$CHOWN_LOG"; }
  IBC_VERSION=9.9.9 IBC_URL=u IBC_SHA256=s GATEWAY_BUILD=b GATEWAY_MAJOR=1050 GATEWAY_SHA256=gwsha
  dl_cache=/nonexistent
  eval "$1"; fix_fns=$2; eval "$fix_fns"; step_gateway' _ "$step" "$fns" >"$work/out" 2>&1 \
  || { cat "$work/out" >&2; echo "FAIL: step_gateway failed on an existing tree" >&2; failures=$((failures + 1)); }
expect "step_gateway repairs config.ini on a rerun" 644 "$(mode "$dest/config.ini")"
expect "step_gateway repairs a folder on a rerun" 755 "$(mode "$dest/sub")"
expect "step_gateway unpacks nothing on a rerun" "" "$(grep '^unzip' "$work/effects" || true)"

# The rendered config makes the Gateway itself refuse API clients that are not
# on the server: Tailscale's own firewall rules accept the tailnet before ufw
# looks, so the firewall is not the guard. (BindAddress is IBC's command
# server's address, not the API's, so it is not set.)
render="$here/../libexec/render-ibc-config.sh"
cred="$work/cred"; run_dir="$work/run"
mkdir -p "$cred" "$run_dir"
printf 'TWS_USERID=u\nTWS_PASSWORD=p\n' >"$cred/ibkr-login"
printf '%s\n' '# comment' 'IbLoginId=' 'IbPassword=' 'TradingMode=live' 'ReadOnlyApi=no' \
  'AutoRestartTime=' 'ColdRestartTime=' 'ReloginAfterSecondFactorAuthenticationTimeout=no' \
  'AcceptNonBrokerageAccountWarning=no' 'CommandServerPort=7462' \
  'TrustedTwsApiClientIPs=100.64.0.1' 'AcceptIncomingConnectionAction=manual' 'LoginDialogDisplayTimeout=20' >"$work/template.ini"
CREDENTIALS_DIRECTORY=$cred RUNTIME_DIRECTORY=$run_dir IBC_TEMPLATE=$work/template.ini "$render" >/dev/null
grep -qx 'AcceptIncomingConnectionAction=reject' "$run_dir/config.ini" || expect "incoming API connections are rejected" reject "$(grep '^AcceptIncomingConnectionAction' "$run_dir/config.ini" || echo missing)"
grep -qx 'TrustedTwsApiClientIPs=' "$run_dir/config.ini" || expect "no trusted API client addresses" "TrustedTwsApiClientIPs=" "$(grep '^TrustedTwsApiClientIPs' "$run_dir/config.ini" || echo missing)"

test "$failures" -eq 0 || exit 1
echo "ibc modes tests ok"
