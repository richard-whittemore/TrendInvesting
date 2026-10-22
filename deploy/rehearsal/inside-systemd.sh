#!/bin/bash
# Runs inside the second rehearsal container, in which systemd is PID 1 (root,
# Ubuntu 24.04, systemd 255): the failed-unit page as systemd itself starts
# it. The installed ti-unit-failed@.service runs the built reporter against
# stand-in channels on loopback, unchanged but for a drop-in that writes down
# the instance and invocation it was given and points -secrets-file at a copy
# of the secrets, since systemd's credential directory (%d) is not mounted in
# a Docker container: LoadCredential= itself is rehearsed in inside.sh's
# verify and on the server. The engine is a stand-in unit with the real
# engine unit's OnFailure= line, so systemd expands %n into the hook's name:
# ti-unit-failed@ti-engine@boost.service.service, whose %i is
# ti-engine@boost.service (docs/manual/server.md, "A failed unit"; ADR 0019,
# "Alerting").
set -euo pipefail
set +x

fail() {
  echo "REHEARSAL FAIL: $*" >&2
  journalctl -u 'ti-unit-failed@*' -u 'ti-engine@*' --no-pager -o short-monotonic >&2 || true
  exit 1
}
pass() { echo "REHEARSAL ok: $*"; }
note() { echo "REHEARSAL note: $*"; }

engine=ti-engine@boost.service
hook=ti-unit-failed@ti-engine@boost.service.service
reports=/var/lib/trend-investing/reports
probe=/run/ti-probe
install -d "$probe"

useradd --system --no-create-home --shell /usr/sbin/nologin ti-trader
install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing "$reports"
install -D -m 0755 /dist/ti-reporter /opt/trend-investing/bin/ti-reporter
install -d -m 0755 /etc/trend-investing
printf 'TI_NTFY_TOPIC=rehearsal\nTI_NTFY_SERVER=http://127.0.0.1:8099\nTI_PAGERDUTY_ROUTING_KEY=fake\nTI_PAGERDUTY_URL=http://127.0.0.1:8099/v2/enqueue\n' >/etc/trend-investing/secrets.env
chmod 0600 /etc/trend-investing/secrets.env
install -d -o ti-trader -g ti-trader -m 0700 "$probe/creds"
install -o ti-trader -g ti-trader -m 0400 /etc/trend-investing/secrets.env "$probe/creds/secrets"

# The stand-in channels: every POST's path, and its body on one line.
cat >"$probe/channels.py" <<'PY'
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
: >"$probe/posts"
: >"$probe/posts.bodies"
systemd-run --quiet --unit=ti-probe-channels python3 "$probe/channels.py" "$probe/posts"
for _ in $(seq 50); do
  python3 -c 'import socket; socket.create_connection(("127.0.0.1", 8099), 1)' 2>/dev/null && break
  sleep 0.1
done

cp /deploy/systemd/ti-unit-failed@.service /etc/systemd/system/
install -d /etc/systemd/system/ti-unit-failed@.service.d
execstart=$(sed -n 's/^ExecStart=//p' /etc/systemd/system/ti-unit-failed@.service)
test "$(grep -o '%d/secrets' <<<"$execstart" | wc -l)" = 1 || fail "the hook's ExecStart does not read %d/secrets once: $execstart"
cat >/etc/systemd/system/ti-unit-failed@.service.d/rehearsal.conf <<UNIT
[Service]
ExecStartPre=/bin/sh -c 'echo "%i \$MONITOR_INVOCATION_ID" >>/var/lib/trend-investing/reports/hook-instances'
ReadOnlyPaths=$probe/creds
ExecStart=
ExecStart=${execstart//%d/$probe/creds}
UNIT
onfailure=$(grep -x 'OnFailure=.*' /deploy/systemd/ti-engine@.service) || fail "the engine unit names no OnFailure="
cat >/etc/systemd/system/ti-engine@.service <<UNIT
[Unit]
Description=Stand-in engine %i (rehearsal): fails as the real one can
$onfailure

[Service]
Type=simple
EnvironmentFile=$probe/engine.env
ExecStart=/bin/sh -c 'if test -e $probe/silent; then exit 1; fi; echo "engine stopping: marker-A from the first run"; exit 1'
UNIT
systemctl daemon-reload

instances() { if test -e "$reports/hook-instances"; then wc -l <"$reports/hook-instances"; else echo 0; fi; }
ntfy_pages() { grep -c '^/rehearsal$' "$probe/posts" || true; }
pd_events() { grep -o '"event_action": *"[a-z]*"' "$probe/posts.bodies" | grep -c "$1" || true; }
# wait_hook n: the hook has started n times and is no longer running.
wait_hook() {
  for _ in $(seq 150); do
    if test "$(instances)" -ge "$1" && test "$(systemctl show -p ActiveState --value "$hook")" != activating; then
      return 0
    fi
    sleep 0.1
  done
  fail "the hook did not run a ${1}th time"
}
fail_engine() { systemctl start "$engine" 2>/dev/null || true; }
run_of() { systemctl show -p InvocationID --value "$engine"; }

# Run 1: the engine starts, writes its own line, and fails.
touch "$probe/engine.env"
fail_engine
wait_hook 1
run1=$(run_of)
systemctl list-units --all --no-legend "ti-unit-failed@*" | grep -q "$hook" || fail "systemd did not start $hook"
test "$(systemctl show -p Result --value "$hook")" = success || fail "$hook did not succeed"
test "$(sed -n 1p "$reports/hook-instances")" = "$engine $run1" ||
  fail "the hook was given '$(sed -n 1p "$reports/hook-instances")', want '$engine $run1' (%i and MONITOR_INVOCATION_ID)"
{ test "$(ntfy_pages)" = 1 && test "$(pd_events trigger)" = 1; } || fail "the first failed run did not page each channel once"
grep -q "$engine failed" "$probe/posts.bodies" || fail "the page does not name $engine"
grep -q 'marker-A' "$probe/posts.bodies" || fail "the page's reason is not the failed run's own line"
grep -q "unit-failed:$engine/$run1" "$reports/unit-failures.json" || fail "the record is not keyed by the failed run"
# The reporter's firing asks systemd the same, as ti-trader, over D-Bus.
answer=$(runuser -u ti-trader -- systemctl show -p ActiveState -p InvocationID "$engine")
{ grep -qx 'ActiveState=failed' <<<"$answer" && grep -qx "InvocationID=$run1" <<<"$answer"; } ||
  fail "systemctl show, as ti-trader, does not name the failed run: $answer"
pass "systemd accepts $hook, starts it on the engine's failure with %i = $engine and the failed run's invocation, and it pages both channels with the run's own line"

# Run 2: restarted from failed, it fails again writing nothing, before any
# reporter firing: a second page, the first incident resolved first, and the
# reason systemd's line about this run, not the first run's.
touch "$probe/silent"
fail_engine
wait_hook 2
run2=$(run_of)
test "$run2" != "$run1" || fail "the restarted engine kept its invocation"
test "$(sed -n 2p "$reports/hook-instances")" = "$engine $run2" || fail "the second hook run was given '$(sed -n 2p "$reports/hook-instances")'"
{ test "$(ntfy_pages)" = 2 && test "$(pd_events trigger)" = 2 && test "$(pd_events resolve)" = 1; } ||
  fail "the second failed run did not page again with the first incident resolved (ntfy $(ntfy_pages), triggers $(pd_events trigger), resolves $(pd_events resolve))"
second=$(grep '"event_action": *"trigger"' "$probe/posts.bodies" | tail -1)
case $second in
  *marker-A*) fail "the second page's reason is the first run's line" ;;
  *"Failed with result 'exit-code'"*) ;;
  *) fail "the second page's reason is not systemd's line about its run: $second" ;;
esac
pass "a unit that fails again before any firing pages again, keyed by its new run, with systemd's line about that run as the reason"

# Run 3: after reset-failed, a start refused for a missing EnvironmentFile
# (the engine's run.env): the run writes nothing at all.
systemctl reset-failed "$engine"
rm -f "$probe/silent" "$probe/engine.env"
fail_engine
wait_hook 3
third=$(grep '"event_action": *"trigger"' "$probe/posts.bodies" | tail -1)
case $third in
  *"Failed to load environment files"*) ;;
  *) fail "a refused start's page does not give systemd's reason: $third" ;;
esac
pass "a start refused for a missing environment file pages with systemd's own reason"

# The exact name, started by hand: the hook takes the same failed run and
# sends nothing again.
pages=$(wc -l <"$probe/posts")
systemctl start "$hook" || fail "systemctl start $hook failed"
test "$(sed -n 4p "$reports/hook-instances" | cut -d' ' -f1)" = "$engine" || fail "the hook started by name was not given $engine"
test "$(wc -l <"$probe/posts")" = "$pages" || fail "the hook started again for the same failed run paged again"
pass "systemctl start $hook runs with %i = $engine and pages nothing new for a run already paged"

# A start refused while the unit is already failed: systemd 255 does not
# start OnFailure= for it, so only the reporter's firing, which keys by the
# new invocation, can page it.
before=$(instances)
run3=$(run_of)
fail_engine
sleep 2
if test "$(instances)" = "$before" && test "$(run_of)" != "$run3"; then
  note "a start refused while $engine was already failed did not start the hook (new invocation $(run_of)); the reporter's next firing pages it"
else
  note "systemd started the hook for a start refused while $engine was already failed"
fi
echo "REHEARSAL (systemd) complete"
