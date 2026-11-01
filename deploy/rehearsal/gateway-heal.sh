# shellcheck shell=bash
# Container-only stand-ins exercise systemd's OnFailure expansion and the real
# healer policy (docs/manual/server.md, Gateway healer; ADR 0025). No Gateway,
# account, credentials or broker connection exists in this drill.
rehearse_gateway_heal() {
  local hook=ti-gateway-heal@ti-adapter-boost.service.service n said
  install -d -m 0700 /var/lib/ti-gateway/heal
  for n in boost trad-ira roth-ira; do
    install -d -o ti-trader -g ti-trader -m 0750 "/var/lib/trend-investing/adapters/$n"
  done
  install -m 0755 /dist/ti-gateway-heal-rehearsal /opt/trend-investing/bin/ti-gateway-heal
  install -m 0644 /deploy/systemd/ti-gateway-heal@.service /etc/systemd/system/
  printf '{"windows":[]}\n' >/etc/trend-investing/ibkr-maintenance.json
  cat >/etc/systemd/system/ti-gateway.service <<'UNIT'
[Unit]
Description=Gateway stand-in for the isolated recovery drill
[Service]
Type=simple
ExecStart=/bin/sh -c 'echo start >>/run/heal-gateway-starts; echo "Login has completed"; exec sleep infinity'
UNIT
  # Both real hooks are retained: recovery must coexist with today's page.
  {
    printf '[Unit]\nDescription=Adapter remedy stand-in\n'
    grep '^OnFailure=' /deploy/systemd/ti-adapter-boost.service
    cat <<'UNIT'
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo start >>/run/heal-adapter-starts; if test -e /run/heal-adapter-good; then exit 0; fi; touch /run/heal-adapter-good; echo "adapter stopped: MaintenanceExpired: IBKR connectivity did not return by the end of its scheduled window; a Gateway restart is the remedy" >&2; exit 3'
UNIT
  } >/etc/systemd/system/ti-adapter-boost.service
  systemctl daemon-reload
  systemctl start ti-gateway.service || fail "Gateway stand-in did not start"
  systemctl start ti-adapter-boost.service 2>/dev/null && fail "adapter stand-in did not exit 3"
  for _ in $(seq 300); do
    if test -f /run/heal-adapter-starts && test "$(wc -l </run/heal-adapter-starts)" -eq 2 &&
      test "$(systemctl show -p ActiveState --value "$hook")" = inactive; then break; fi
    sleep 0.1
  done
  test "$(wc -l </run/heal-gateway-starts)" -eq 2 || fail "healer did not restart Gateway exactly once"
  test "$(wc -l </run/heal-adapter-starts)" -eq 2 || fail "healer did not start adapter after login"
  test "$(stat -c '%u:%a' /var/lib/ti-gateway/heal/last-restart.json)" = 0:600 || fail "restart state is not root-only"
  said=$(journalctl -u "$hook" --no-pager -o cat)
  for n in restart login-completed adapter-start; do
    grep -q "gateway-heal: row=eligible step=$n" <<<"$said" || fail "healer omitted $n"
  done
  rm /run/heal-adapter-good
  systemctl start ti-adapter-boost.service 2>/dev/null && fail "second adapter stand-in did not exit 3"
  for _ in $(seq 300); do
    said=$(journalctl -u "$hook" --no-pager -o cat)
    if grep -Eq 'gateway-heal: row=(rate-limit|same-incident) step=no-action' <<<"$said"; then break; fi
    sleep 0.1
  done
  grep -Eq 'gateway-heal: row=(rate-limit|same-incident) step=no-action' <<<"$said" || fail "second failure was not refused"
  test "$(wc -l </run/heal-gateway-starts)" -eq 2 || fail "second failure restarted Gateway"
  test "$(wc -l </run/heal-adapter-starts)" -eq 3 || fail "second failure restarted adapter"
  systemctl stop ti-gateway.service "$hook"
  systemctl reset-failed ti-adapter-boost.service
  cp /deploy/systemd/ti-gateway.service /deploy/systemd/ti-adapter-boost.service /etc/systemd/system/
  install -m 0755 /dist/ti-gateway-heal /opt/trend-investing/bin/ti-gateway-heal
  cp /deploy/etc/ibkr-maintenance.json.example /etc/trend-investing/ibkr-maintenance.json
  systemctl daemon-reload
  pass "Gateway healer: one restart, fresh login, adapter start; second failure does nothing"
}
