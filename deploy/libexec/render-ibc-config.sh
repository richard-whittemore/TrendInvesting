#!/bin/bash
# Renders IBC's config.ini for this run from the pinned template and the
# owner's login file, into the service's tmpfs runtime directory (mode 0600).
# The password therefore never reaches the disk, the repository or the logs.
# This script never prints a credential value and must never run under
# `set -x`. Runs as ti-gateway from start-gateway.sh, ti-gateway.service's ExecStart.
#
# Inputs (all overridable for the container rehearsal):
#   CREDENTIALS_DIRECTORY   systemd's private credential directory
#   RUNTIME_DIRECTORY       the service's tmpfs directory
#   IBC_TEMPLATE            the pinned IBC config.ini
#   TI_GATEWAY_RESTART_TIME nightly restart, 12-hour clock, New York time
#   TI_GATEWAY_COLD_RESTART Sunday full re-login (weekly 2FA), 24-hour, New York time
set -euo pipefail
set +x
umask 077

template=${IBC_TEMPLATE:-/opt/ibc/config.ini}
login_file="${CREDENTIALS_DIRECTORY:?not run under systemd LoadCredential}/ibkr-login"
out="${RUNTIME_DIRECTORY:?no RuntimeDirectory}/config.ini"

userid="" password="" mode="paper" readonly_api="yes"
while IFS= read -r line || test -n "$line"; do
  case $line in
    ''|'#'*) continue ;;
  esac
  key=${line%%=*}
  value=${line#*=}
  case $key in
    TWS_USERID) userid=$value ;;
    TWS_PASSWORD) password=$value ;;
    TWS_TRADING_MODE) mode=$value ;;
    TWS_READONLY_API) readonly_api=$value ;;
    *) echo "render-ibc-config: unknown key in login file: $key" >&2; exit 64 ;;
  esac
done <"$login_file"

if test -z "$userid" || test -z "$password"; then
  echo "render-ibc-config: TWS_USERID and TWS_PASSWORD are not set in /etc/trend-investing/ibkr-login.env (owner step)" >&2
  exit 78
fi
case $mode in paper|live) ;; *) echo "render-ibc-config: TWS_TRADING_MODE must be paper or live" >&2; exit 64 ;; esac
case $readonly_api in yes|no) ;; *) echo "render-ibc-config: TWS_READONLY_API must be yes or no" >&2; exit 64 ;; esac

# Values go to awk through the environment, never the command line (ps).
IBC_V_IbLoginId=$userid \
IBC_V_IbPassword=$password \
IBC_V_TradingMode=$mode \
IBC_V_ReadOnlyApi=$readonly_api \
IBC_V_AutoRestartTime=${TI_GATEWAY_RESTART_TIME:-11:45 PM} \
IBC_V_ColdRestartTime=${TI_GATEWAY_COLD_RESTART:-09:00} \
IBC_V_ReloginAfterSecondFactorAuthenticationTimeout=yes \
IBC_V_AcceptNonBrokerageAccountWarning=yes \
IBC_V_CommandServerPort=0 \
awk '
  BEGIN { n = 0 }
  {
    eq = index($0, "=")
    if (eq > 1 && substr($0, 1, 1) != "#") {
      key = substr($0, 1, eq - 1)
      env = "IBC_V_" key
      if (env in ENVIRON) { print key "=" ENVIRON[env]; seen[key] = 1; next }
    }
    print
  }
  END {
    for (e in ENVIRON) if (index(e, "IBC_V_") == 1) {
      k = substr(e, 7)
      if (!(k in seen)) { print "render-ibc-config: template has no " k > "/dev/stderr"; exit 65 }
    }
  }' "$template" >"$out"
chmod 0600 "$out"
echo "render-ibc-config: wrote config for trading mode $mode, read-only API $readonly_api"
