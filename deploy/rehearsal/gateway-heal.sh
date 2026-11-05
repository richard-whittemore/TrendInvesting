# shellcheck shell=bash
# Container-only stand-ins exercise systemd's OnFailure expansion and the real
# healer policy (docs/manual/server.md, Gateway healer; ADR 0025). No Gateway,
# account, credentials or broker connection exists in this drill.
rehearse_gateway_heal_account() {
  local account=$1 gateway=ti-gateway.service adapter=ti-adapter-boost.service gateway_state=/var/lib/ti-gateway heal_state=/var/lib/ti-gateway-heal prefix=/run/heal
  if test "$account" != boost; then
    gateway=ti-gateway@$account.service
    adapter=ti-adapter@$account.service
    gateway_state=/var/lib/ti-gateway-$account
    heal_state=/var/lib/ti-gateway-heal/$account
    prefix=/run/heal-$account
  fi
  local hook=ti-gateway-heal@$adapter.service n said page_hook=ti-unit-failed@$adapter.service page_before failed_invocation
  page_before=$(instances)
  install -d -m 0700 "$heal_state"
  # The Gateway's own state folder, where gateway-start keeps its login counter.
  install -d -m 0750 "$gateway_state"
  # The files the healer reads on the server, as provisioning lays them down:
  # the Gateway's restart times and IBKR's schedule (a missing one fails closed).
  install -d -m 0755 /etc/trend-investing
  local gateway_env=/etc/trend-investing/gateway.env
  if test "$account" != boost; then gateway_env=/etc/trend-investing/gateway-$account.env; fi
  test -e "$gateway_env" ||
    printf 'GATEWAY_MAJOR=1050\nTI_GATEWAY_RESTART_TIME=11:45 PM\nTI_GATEWAY_COLD_RESTART=09:00\n' >"$gateway_env"
  test -e /etc/trend-investing/ibkr-maintenance.json ||
    install -m 0644 /deploy/etc/ibkr-maintenance.json.example /etc/trend-investing/ibkr-maintenance.json
  for n in boost trad-ira roth-ira; do
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/adapters/$n"
    install -o ti-trader -g ti-trader -m 0600 /dev/null "/var/lib/trend-investing/adapters/$n/pending.json.lock"
    printf '{}\n' >"/var/lib/trend-investing/adapters/$n/pending.json.handover"
    chown ti-trader:ti-trader "/var/lib/trend-investing/adapters/$n/pending.json.handover"
    chmod 0600 "/var/lib/trend-investing/adapters/$n/pending.json.handover"
  done
  install -m 0755 /dist/ti-gateway-heal-rehearsal /opt/trend-investing/bin/ti-gateway-heal
  install -m 0644 /deploy/systemd/ti-gateway-heal@.service /etc/systemd/system/
  printf '{"windows":[]}\n' >/etc/trend-investing/ibkr-maintenance.json
  # One shared fake clock moves both Now and systemd invocation timestamps;
  # the second failure must be later than the first restart on every date.
  python3 - <<'PY_CLOCK' >/run/heal-clock-offset-seconds
from datetime import datetime, timedelta, timezone
now = datetime.now(timezone.utc)
target = now.replace(hour=17, minute=0, second=0, microsecond=0)
while target <= now or target.weekday() >= 5:
    target += timedelta(days=1)
print(int((target - now).total_seconds()))
PY_CLOCK
  # The real sandbox must read and flock another user's 0600 files but may
  # never write order evidence (ADR 0026 point 8).
  install -d "/etc/systemd/system/$hook.d"
  cat >"/etc/systemd/system/$hook.d/sandbox.conf" <<SANDBOX
[Service]
ExecStartPre=/bin/sh -c 'cat /var/lib/trend-investing/adapters/$account/pending.json.handover >/dev/null || exit 1; if echo changed >>/var/lib/trend-investing/adapters/$account/pending.json.handover; then exit 1; fi'
SANDBOX
  # gateway-start writes the provisional attempt before launch and settles it
  # durably before publishing the accepted line (server.md, Gateway healer).
  # Not under /run: the container mounts it noexec (status 203 at start).
  install -d -m 0755 /usr/local/libexec
  cat >"/usr/local/libexec/heal-gateway-standin-$account" <<GATEWAY
#!/bin/sh
set -eu
echo start >>${prefix}-gateway-starts
date +%s >$gateway_state/login-failures
sync $gateway_state/login-failures
sleep 1
: >$gateway_state/login-failures
sync $gateway_state/login-failures
echo 'IBC: Login has completed'
exec sleep infinity
GATEWAY
  chmod 0755 "/usr/local/libexec/heal-gateway-standin-$account"
  cat >"/etc/systemd/system/$gateway" <<UNIT
[Unit]
Description=Gateway stand-in for the isolated recovery drill
[Service]
Type=simple
ExecStart=/usr/local/libexec/heal-gateway-standin-$account
UNIT
  # Both real hooks are retained: recovery must coexist with today's page.
  {
    printf '[Unit]\nDescription=Adapter remedy stand-in\n'
    grep '^OnFailure=' /deploy/systemd/ti-adapter-boost.service
    cat <<UNIT
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo start >>${prefix}-adapter-starts; if test -e ${prefix}-adapter-good; then exit 0; fi; touch ${prefix}-adapter-good; echo "ibkr link state=connectivity-lost reason=gateway-lost-ibkr" >&2; sleep 1; echo "adapter stopped: MaintenanceExpired: IBKR connectivity did not return by the end of its scheduled window; a Gateway restart is the remedy" >&2; exit 3'
UNIT
  } >"/etc/systemd/system/$adapter"
  systemctl daemon-reload
  systemctl start "$gateway" || fail "Gateway stand-in did not start"
  for _ in $(seq 100); do
    if journalctl -u "$gateway" --no-pager -o cat | grep -q 'Login has completed'; then break; fi
    sleep 0.1
  done
  systemctl start "$adapter" 2>/dev/null && fail "adapter stand-in did not exit 3"
  failed_invocation=$(systemctl show -p InvocationID --value "$adapter")
  for _ in $(seq 300); do
    if test -f "${prefix}-adapter-starts" && test "$(wc -l <"${prefix}-adapter-starts")" -eq 2 &&
      test "$(systemctl show -p ActiveState --value "$hook")" = inactive; then break; fi
    sleep 0.1
  done
  test "$(wc -l <"${prefix}-gateway-starts")" -eq 2 || fail "healer did not restart Gateway exactly once"
  test "$(wc -l <"${prefix}-adapter-starts")" -eq 2 || fail "healer did not start adapter after login"
  test "$(stat -c '%u:%a' "$heal_state/last-restart.json")" = 0:600 || fail "restart state is not root-only"
  said=$(journalctl -u "$hook" --no-pager -o cat)
  for n in restart login-completed adapter-start; do
    grep -q "gateway-heal: row=eligible step=$n" <<<"$said" || fail "healer omitted $n"
  done
  test "$(systemctl show -p CapabilityBoundingSet --value "$hook")" = cap_dac_read_search || fail "healer kept extra capabilities"
  test "$(systemctl show -p ReadOnlyPaths --value "$hook")" = /var/lib/trend-investing/adapters || fail "adapter evidence is not read-only"
  test "$(cat "/var/lib/trend-investing/adapters/$account/pending.json.handover")" = '{}' || fail "sandbox changed order evidence"
  for _ in $(seq 100); do
    if test "$(instances)" -gt "$page_before" && test "$(systemctl show -p Result --value "$page_hook")" = success; then break; fi
    sleep 0.1
  done
  test "$(instances)" -gt "$page_before" || fail "failed-unit page hook did not run"
  test "$(systemctl show -p Result --value "$page_hook")" = success || fail "failed-unit page hook did not succeed"
  grep -Fxq "$adapter $failed_invocation" /var/lib/trend-investing/reports/hook-instances || fail "page hook did not receive the failed adapter"
  # Give the second failure a distinct time on the same fake clock before
  # exercising the cooldown (server.md, Gateway healer).
  sleep 2
  rm "${prefix}-adapter-good"
  systemctl start "$adapter" 2>/dev/null && fail "second adapter stand-in did not exit 3"
  for _ in $(seq 300); do
    said=$(journalctl -u "$hook" --no-pager -o cat)
    if grep -q 'gateway-heal: row=rate-limit step=no-action' <<<"$said"; then break; fi
    sleep 0.1
  done
  grep -q 'gateway-heal: row=rate-limit step=no-action' <<<"$said" || fail "second failure was not refused"
  test "$(wc -l <"${prefix}-gateway-starts")" -eq 2 || fail "second failure restarted Gateway"
  test "$(wc -l <"${prefix}-adapter-starts")" -eq 3 || fail "second failure restarted adapter"
  systemctl stop "$gateway" "$hook"
  systemctl reset-failed "$adapter"
  rm -r "/etc/systemd/system/$hook.d"
  if test "$account" = boost; then
    cp /deploy/systemd/ti-gateway.service /deploy/systemd/ti-adapter-boost.service /etc/systemd/system/
  else
    rm "/etc/systemd/system/$gateway" "/etc/systemd/system/$adapter"
  fi
  install -m 0755 /dist/ti-gateway-heal /opt/trend-investing/bin/ti-gateway-heal
  cp /deploy/etc/ibkr-maintenance.json.example /etc/trend-investing/ibkr-maintenance.json
  systemctl daemon-reload
  pass "Gateway healer: one restart, fresh login, adapter start; second failure does nothing"
}

rehearse_gateway_heal() {
  rehearse_gateway_heal_account boost
  # Preserve joint evidence while the IRA performs its full recovery drill
  # (server.md, Gateway healer). No IRA trigger may touch this state.
  cp /var/lib/ti-gateway-heal/last-restart.json /run/heal-joint-reservation-before
  cp /var/lib/ti-gateway/login-failures /run/heal-joint-counter-before
  local starts
  starts=$(wc -l </run/heal-gateway-starts)
  rehearse_gateway_heal_account trad-ira
  cmp -s /run/heal-joint-reservation-before /var/lib/ti-gateway-heal/last-restart.json || fail "IRA changed joint reservation"
  cmp -s /run/heal-joint-counter-before /var/lib/ti-gateway/login-failures || fail "IRA changed joint login counter"
  test "$(wc -l </run/heal-gateway-starts)" -eq "$starts" || fail "IRA restarted joint Gateway"
  pass "IRA recovery leaves joint Gateway and accounting untouched"
}
