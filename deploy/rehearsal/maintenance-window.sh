# shellcheck shell=bash
# shellcheck disable=SC2154  # reports and engine are inside-systemd.sh's
# Sourced by inside-systemd.sh (and first-day.sh) in the rehearsal container
# in which systemd is PID 1. The maintenance window as the owner meets it
# (docs/manual/alerts-and-dashboard.md, "Maintenance windows"; ADR 0019,
# "Alerting"): the real ti-maintenance, the real OnFailure hook unit and the
# real reporter unit, with the stand-in channels of inside-systemd.sh.
# Needs its helpers: fail, pass, new_episode, fail_engine, wait_hook,
# instances, run_of, ntfy_pages, pushover_pages, decoded_bodies.

# rehearse_maintenance_window: a unit that fails inside a window and stays
# failed is recorded and pages once, at the firing after the window closes (a
# release inside a window is exactly this: ti-release opens and closes the
# window around its install, and deploy/rehearsal/inside.sh rehearses that);
# a window that expires on its own stops holding by itself.
rehearse_maintenance_window() {
  local marker=/run/ti-maintenance/maintenance.json n push0 ntfy0
  /deploy/release.sh --install >/dev/null || fail "release.sh --install failed"
  test -x /usr/local/sbin/ti-maintenance || fail "release.sh --install did not install ti-maintenance"

  # A window opened by the owner (root), read by the hook and the reporter as
  # ti-trader, with no permission changed on any other directory.
  new_episode
  n=$(($(instances) + 1))
  ntfy0=$(ntfy_pages)
  push0=$(pushover_pages)
  ti-maintenance start 10m >/dev/null || fail "ti-maintenance start failed"
  runuser -u ti-trader -- test -r "$marker" || fail "the reporter's user cannot read the window's marker"
  ti-maintenance status | grep -q 'maintenance window open until' || fail "ti-maintenance status does not show the window"

  # The engine fails inside the window: the hook records and pages no one.
  fail_engine
  wait_hook "$n"
  { test "$(ntfy_pages)" = "$ntfy0" && test "$(pushover_pages)" = "$push0"; } || fail "the failed unit's hook paged inside a window"
  grep -q 'held:' "$reports/unit-failures.json" || fail "the hook did not record the failure with the window noted: $(cat "$reports/unit-failures.json")"
  # A reporter firing inside the window still finds the unit failed and pages no one.
  systemctl start ti-reporter@boost.service || fail "the reporter's firing inside a window failed: $(journalctl -u ti-reporter@boost.service --no-pager -o cat | tail -n 5)"
  { test "$(ntfy_pages)" = "$ntfy0" && test "$(pushover_pages)" = "$push0"; } || fail "the reporter paged a failed unit inside a window"

  # The window closes with the unit still failed: the next firing pages once.
  ti-maintenance end >/dev/null || fail "ti-maintenance end failed"
  test ! -e "$marker" || fail "ti-maintenance end left the marker"
  systemctl start ti-reporter@boost.service || fail "the reporter's firing after the window failed"
  # Each channel takes the page, once, and the close sends no message of its own.
  { test "$(ntfy_pages)" = "$((ntfy0 + 1))" && test "$(pushover_pages)" = "$((push0 + 1))"; } ||
    fail "the unit still failed at the window's close did not page each channel once (ntfy $(ntfy_pages) from $ntfy0, Pushover $(pushover_pages) from $push0)"
  decoded_bodies | grep -q "$engine failed" || fail "the page at the window's close does not name $engine"
  systemctl start ti-reporter@boost.service || fail "the second reporter firing after the window failed"
  { test "$(ntfy_pages)" = "$((ntfy0 + 1))" && test "$(pushover_pages)" = "$((push0 + 1))"; } || fail "the unit paged again at the next firing"
  pass "a unit that fails inside a window and stays failed is recorded, not paged, and pages once at the firing after the window closes"

  # A window that expires on its own: nobody ends it, and it holds nothing after.
  new_episode
  n=$(($(instances) + 1))
  push0=$(pushover_pages)
  ti-maintenance start 2s >/dev/null || fail "ti-maintenance start 2s failed"
  sleep 4
  fail_engine
  wait_hook "$n"
  test "$(pushover_pages)" = "$((push0 + 1))" || fail "a failure after a window expired on its own was held"
  ti-maintenance status | grep -q 'maintenance window closed' || fail "ti-maintenance status calls the expired window open"
  test -e "$marker" || fail "the hook removed the expired marker: no reader removes one, so a start in flight is never undone"
  new_episode
  pass "a window that expires on its own holds nothing after its expiry, and no reader removes the marker"
}

# rehearse_capital_safety_in_window: the engine is Halted (the drill in
# first-day.sh); a reporter firing inside a window pages it all the same.
rehearse_capital_safety_in_window() {
  local push0 ntfy0
  push0=$(pushover_pages)
  ntfy0=$(ntfy_pages)
  ti-maintenance start 10m >/dev/null || fail "ti-maintenance start failed"
  systemctl start ti-reporter@boost.service || fail "the reporter's firing inside a window failed: $(journalctl -u ti-reporter@boost.service --no-pager -o cat | tail -n 5)"
  ti-maintenance end >/dev/null || fail "ti-maintenance end failed"
  test "$(pushover_pages)" -gt "$push0" || fail "a Halted engine did not page Pushover inside a window"
  test "$(ntfy_pages)" -gt "$ntfy0" || fail "a Halted engine did not page ntfy inside a window"
  decoded_bodies | grep -qi 'halted' || fail "the pages inside the window do not say the engine is Halted; the last was: $(last_page)"
  pass "a Halted engine pages inside a maintenance window, as a capital-safety alert always does"
}
