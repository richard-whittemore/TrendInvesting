#!/bin/bash
# Runs inside the second rehearsal container, in which systemd is PID 1 (root,
# Ubuntu 24.04, systemd 255), with / mounted shared so that systemd can hand a
# unit its credentials (LoadCredential=) as on the server.
#
# First the locked secrets (ADR 0028; docs/manual/server.md,
# "Unlocking the secrets after a boot"): the secret files are encrypted and
# nothing is decrypted, so systemd refuses to start every installed unit that
# loads a secret, naming the decrypted file it asserts, and a timer whose
# firing is refused keeps firing; then ti-unlock decrypts them into a
# root-only tmpfs under /run and the units read them from there.
#
# Then the failed-unit page as systemd itself starts it. The installed
# ti-unit-failed@.service runs the built reporter against stand-in channels
# on loopback, reading the channels from the credential systemd hands it from
# the unlocked secrets, unchanged but for a drop-in that writes down the
# instance and invocation it was given. The engine is a stand-in unit with
# the real engine unit's OnFailure= line, so systemd expands %n into the
# hook's name: ti-unit-failed@ti-engine@boost.service.service, whose %i is
# ti-engine@boost.service (docs/manual/server.md, "A failed unit"; ADR 0019,
# "Alerting").
set -euo pipefail
set +x

fail() {
  echo "REHEARSAL FAIL: $*" >&2
  journalctl -u 'ti-unit-failed@*' -u 'ti-engine@*' -u ti-gateway -u ti-adapter-boost -u ti-data -u 'ti-data@*' -u ti-warmup -u ti-warmup-drill -u 'ti-reporter@*' -u ti-probe-massive --no-pager -o short-monotonic >&2 || true
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
install -D -m 0755 /dist/ti-data /opt/trend-investing/bin/ti-data
install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/data /var/lib/trend-investing/engines/boost /var/lib/trend-investing/engines/trad-ira
install -d -m 0755 /etc/trend-investing
install -d -m 0700 /etc/trend-investing/accounts

# --- The locked secrets. ti-secrets and ti-unlock as provision.sh installs
# them; the passphrase on the rehearsal's test-only descriptor, which
# ti-secrets honours only with TI_REHEARSAL=1.
install -m 0755 /deploy/secrets.sh /usr/local/sbin/ti-secrets
ln -sfn ti-secrets /usr/local/sbin/ti-unlock
passphrase="rehearsal passphrase $(head -c 9 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# ti-secrets refuses to run while any swap is on, and Docker's virtual
# machine has some: the swap list it reads is a stand-in with none.
printf 'Filename\tType\tSize\tUsed\tPriority\n' >/root/ti-no-swaps
with_pass() { TI_REHEARSAL=1 TI_PROC_SWAPS=/root/ti-no-swaps TI_PASSPHRASE_FD=7 "$@" 7< <(yes "$passphrase"); }
marker=marker-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')
on_disk() { find / -xdev -type f -print0 2>/dev/null | xargs -0 grep -lF -- "$1" 2>/dev/null || true; }
unlocked=/run/trend-investing/secrets
# The secrets in plain text, as on a server provisioned before they were
# encrypted, with a marker; migrated as provision.sh services migrates them.
printf 'TI_NTFY_TOPIC=rehearsal\nTI_NTFY_SERVER=http://127.0.0.1:8099\nTI_PUSHOVER_APP_TOKEN=fake\nTI_PUSHOVER_USER_KEY=fake\nTI_PUSHOVER_URL=http://127.0.0.1:8099/1/messages.json\nTI_GITHUB_ISSUES_REPO=owner/repo\nTI_GITHUB_API_URL=http://127.0.0.1:8099/gh\nTI_GITHUB_API_ALLOW_HOST=127.0.0.1:8099\nTI_GITHUB_ISSUES_TOKEN=%s\n' "$marker" >/etc/trend-investing/secrets.env
chmod 0600 /etc/trend-investing/secrets.env
for f in ibkr-login.env accounts/boost.env accounts/trad-ira.env accounts/roth-ira.env; do
  install -m 0600 "/deploy/etc/$f.example" "/etc/trend-investing/$f"
done
with_pass ti-secrets migrate >/dev/null || fail "ti-secrets migrate failed"
test ! -e /etc/trend-investing/secrets.env || fail "the migration left secrets.env in plain text"
test -z "$(on_disk "$marker")" || fail "the migrated secret is on disk: $(on_disk "$marker")"

# Every unit that loads a secret, installed as provision.sh installs it, with
# what its Condition= lines want in place (stand-ins), so that only the
# locked secrets stand in its way.
for u in ti-gateway.service ti-engine@.service ti-adapter-boost.service ti-data.service ti-data@.service ti-reporter@.service ti-flex@.service ti-unit-failed@.service; do
  cp "/deploy/systemd/$u" /etc/systemd/system/
done
install -D -m 0755 /dist/ti-engine /opt/trend-investing/bin/ti-engine
install -D -m 0644 /deploy/etc/engines/boost.env /etc/trend-investing/engines/boost.env
install -D -m 0644 /deploy/etc/engines/boost.config /etc/trend-investing/engines/boost.config
install -D -m 0755 /dev/null /opt/trend-investing/adapter-venv/bin/python3
install -D -m 0644 /dev/null /opt/trend-investing/twsapi/INSTALLED
install -D -m 0644 /dev/null /opt/trend-investing/adapter/adapter/ibkr/cli.py
install -D -m 0644 /dev/null /opt/trend-investing/adapter/adapter/ibkr/flex.py
install -D -m 0644 /deploy/etc/adapters/boost.json.example /etc/trend-investing/adapters/boost.json
systemctl daemon-reload
# refused UNIT FILE: systemctl start UNIT fails on its assertion of the
# decrypted FILE ("Assertion failed on job for UNIT", and in its journal
# "Starting requested but asserts failed"); the unit never ran, so it is not
# failed either.
refused() {
  if systemctl start "$1" 2>/dev/null; then fail "$1 started with the secrets locked"; fi
  test "$(systemctl show -p AssertResult --value "$1")" = no || fail "$1 was not refused by its assertion: $(systemctl status "$1" --no-pager 2>&1 | head -n 8)"
  # Read whole before matching: grep -q stopping early would fail the
  # writer on a closed pipe, and pipefail would count that.
  local said
  said=$(journalctl -u "$1" --no-pager -o cat)
  grep -q 'Starting requested but asserts failed' <<<"$said" || fail "$1's refusal is not in its journal"
  said=$(systemctl cat "$1")
  grep -qxF "AssertPathExists=$unlocked/$2" <<<"$said" || fail "$1 does not assert $unlocked/$2"
  test "$(systemctl show -p ActiveState --value "$1")" != failed || fail "$1 is failed, not refused"
}
refused ti-gateway.service ibkr-login.env
refused ti-engine@boost.service secrets.env
refused ti-adapter-boost.service accounts/boost.env
refused ti-data.service secrets.env
refused ti-data@trad-ira.service secrets.env
refused ti-reporter@boost.service secrets.env
refused ti-flex@trad-ira.service secrets.env
refused ti-unit-failed@ti-engine@boost.service.service secrets.env
pass "with the secrets locked, systemd refuses to start the Gateway, the engine, the adapter, the data job and its IRA instance, the reporter, the Flex reader and the failed-unit page, each on its assertion of the decrypted file it loads"

# A timer whose firing is refused keeps firing: the reporter's timer fires
# every 5 minutes, so a stand-in timer every 2 seconds fires a unit with the
# reporter's own assertion; once unlocked, its next firing runs.
assertion=$(grep -x 'AssertPathExists=.*' /deploy/systemd/ti-reporter@.service)
cat >/etc/systemd/system/ti-probe-locked.service <<UNIT
[Unit]
$assertion
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo fired >>$probe/locked-fired'
UNIT
cat >/etc/systemd/system/ti-probe-locked.timer <<UNIT
[Timer]
OnCalendar=*:*:0/2
AccuracySec=100ms
UNIT
systemctl daemon-reload
systemctl start ti-probe-locked.timer
sleep 5
test ! -e "$probe/locked-fired" || fail "a firing ran with the secrets locked"
grep -q 'asserts failed' <<<"$(journalctl -u ti-probe-locked.service --no-pager -o cat)" || fail "the stand-in timer's firings were not refused by the assertion"
test "$(systemctl show -p ActiveState --value ti-probe-locked.timer)" = active || fail "the stand-in timer stopped after a refused firing"

# A wrong passphrase unlocks nothing.
if TI_REHEARSAL=1 TI_PROC_SWAPS=/root/ti-no-swaps TI_PASSPHRASE_FD=7 ti-unlock >/dev/null 2>&1 7< <(yes "not the passphrase"); then fail "ti-unlock accepted a wrong passphrase"; fi
test -z "$(find "$unlocked" -mindepth 1 2>/dev/null)" || fail "a wrong passphrase decrypted something"

# --- The unlock: the files decrypted into a root-only tmpfs; the next timer
# firing runs; the marker is under /run only.
with_pass ti-unlock >/dev/null || fail "ti-unlock failed"
mountpoint -q "$unlocked" || fail "$unlocked is not a mount of its own"
test "$(stat -f -c %T "$unlocked")" = tmpfs || fail "$unlocked is not a tmpfs"
test "$(stat -c %U:%G:%a "$unlocked")" = root:root:700 || fail "$unlocked is not root:root 0700"
for _ in $(seq 50); do test -e "$probe/locked-fired" && break; sleep 0.1; done
test -e "$probe/locked-fired" || fail "the stand-in timer did not fire once the secrets were unlocked"
systemctl stop ti-probe-locked.timer
test -z "$(on_disk "$marker")" || fail "after the unlock the secret is on disk: $(on_disk "$marker")"
grep -qxF "TI_GITHUB_ISSUES_TOKEN=$marker" "$unlocked/secrets.env" || fail "the unlocked secrets.env does not hold the secret"
pass "a refused timer firing leaves the timer running; a wrong passphrase unlocks nothing; ti-unlock decrypts into a root-only tmpfs, the next firing runs, and the secret is nowhere on disk"

# The stand-in channels: every POST's path, and its body on one line.
cat >"$probe/channels.py" <<'PY'
import http.server
import sys

posts = sys.argv[1]


sys.path.insert(0, "/deploy/rehearsal")
import github_standin


def github_log(line, body):
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
        self.wfile.write(b'{"status":1,"request":"rehearsal"}')

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
# Pushover's messages are form posts: the pages are the posts to its endpoint,
# and the text of a page is its form decoded.
pushover_pages() { grep -c '^/1/messages.json$' "$probe/posts" || true; }
decoded_bodies() { python3 -c 'import sys, urllib.parse; sys.stdout.write(urllib.parse.unquote_plus(sys.stdin.read()))' <"$probe/posts.bodies"; }
last_page() { grep 'priority=2' "$probe/posts.bodies" | tail -1 | python3 -c 'import sys, urllib.parse; sys.stdout.write(urllib.parse.unquote_plus(sys.stdin.read()).replace("\n", " "))'; }
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
{ test "$(ntfy_pages)" = 1 && test "$(pushover_pages)" = 1; } || fail "the first failed run did not page each channel once"
test "$(grep -c '^POST /gh/repos/owner/repo/issues$' "$probe/posts.github" || true)" = 1 || fail "the first failed run did not open one issue in the alerts repository"
decoded_bodies | grep -q "$engine failed" || fail "the page does not name $engine"
decoded_bodies | grep -q 'marker-A' || fail "the page's reason is not the failed run's own line"
grep -q "unit-failed:$engine/$run1" "$reports/unit-failures.json" || fail "the record is not keyed by the failed run"
# The reporter's firing asks systemd the same, as ti-trader, over D-Bus.
answer=$(runuser -u ti-trader -- systemctl show -p ActiveState -p InvocationID "$engine")
{ grep -qx 'ActiveState=failed' <<<"$answer" && grep -qx "InvocationID=$run1" <<<"$answer"; } ||
  fail "systemctl show, as ti-trader, does not name the failed run: $answer"
pass "systemd accepts $hook, starts it on the engine's failure with %i = $engine and the failed run's invocation, and it pages both channels with the run's own line"

# Run 2: restarted from failed, it fails again writing nothing, before any
# reporter firing saw it well: the same failure episode, so no second page and
# no resolve of the first incident (ADR 0019, "Alerting": each incident once).
touch "$probe/silent"
fail_engine
wait_hook 2
run2=$(run_of)
test "$run2" != "$run1" || fail "the restarted engine kept its invocation"
test "$(sed -n 2p "$reports/hook-instances")" = "$engine $run2" || fail "the second hook run was given '$(sed -n 2p "$reports/hook-instances")'"
test "$(systemctl show -p Result --value "$hook")" = success || fail "$hook did not succeed on the second failed run"
{ test "$(ntfy_pages)" = 1 && test "$(pushover_pages)" = 1; } ||
  fail "a second failed run of one episode paged again (ntfy $(ntfy_pages), Pushover $(pushover_pages))"
pass "a unit that fails again before any firing saw it well is one episode: no second page"

# A reporter firing that samples the engine not failed ends the episode
# (cmd/reporter's TestAFailureAfterTheUnitWasSeenWellPagesAgain); no engine log
# runs here, so the record is cleared in its place before each new episode.
new_episode() {
  systemctl reset-failed "$engine"
  rm -f "$reports/unit-failures.json"
}

# Run 3: a new episode, a run that writes nothing: the reason is systemd's
# line about this run, not the first run's.
new_episode
fail_engine
wait_hook 3
{ test "$(ntfy_pages)" = 2 && test "$(pushover_pages)" = 2; } || fail "a failure after the episode ended did not page again"
second=$(last_page)
case $second in
  *marker-A*) fail "the second page's reason is the first run's line" ;;
  *"No entries"*) fail "the second page's reason is journalctl's own notice: $second" ;;
  *"Failed with result 'exit-code'"*) ;;
  *) fail "the second page's reason is not systemd's line about its run: $second" ;;
esac
pass "a failure after the episode ended pages again, keyed by its new run, with systemd's line about that run as the reason"

# Run 4: a new episode, a start refused for a missing EnvironmentFile (the
# engine's run.env): the run writes nothing at all.
new_episode
rm -f "$probe/silent" "$probe/engine.env"
fail_engine
wait_hook 4
third=$(last_page)
case $third in
  *"Failed to load environment files"*) ;;
  *) fail "a refused start's page does not give systemd's reason: $third" ;;
esac
pass "a start refused for a missing environment file pages with systemd's own reason"

# The exact name, started by hand: the hook takes the same failed run and
# sends nothing again.
pages=$(wc -l <"$probe/posts")
systemctl start "$hook" || fail "systemctl start $hook failed"
test "$(sed -n 5p "$reports/hook-instances" | cut -d' ' -f1)" = "$engine" || fail "the hook started by name was not given $engine"
test "$(wc -l <"$probe/posts")" = "$pages" || fail "the hook started again for the same failed run paged again"
pass "systemctl start $hook runs with %i = $engine and pages nothing new for a run already paged"

# The data job's failure pages like the engine's: a stand-in ti-data.service
# that refuses, carrying the real unit's OnFailure= line, so a 403 that
# persists past the last morning firing (a refusal, exit 1) reaches the hook.
# The real unit is put back afterwards.
data_hook=ti-unit-failed@ti-data.service.service
ondata=$(grep -x 'OnFailure=.*' /deploy/systemd/ti-data.service) || fail "the data job's unit names no OnFailure="
cat >/etc/systemd/system/ti-data.service <<UNIT
[Unit]
Description=Stand-in data job (rehearsal): fails as the real one can
$ondata

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "ti-data: HTTP 403 for session 2026-10-23, marker-D" >&2; exit 1'
UNIT
systemctl daemon-reload
rm -f "$reports/unit-failures.json"
before=$(instances)
data_pushover=$(pushover_pages)
systemctl start ti-data.service 2>/dev/null && fail "the stand-in data job did not fail"
for _ in $(seq 150); do
  if test "$(instances)" -gt "$before" && test "$(systemctl show -p ActiveState --value "$data_hook")" != activating; then break; fi
  sleep 0.1
done
test "$(instances)" -gt "$before" || fail "systemd did not start $data_hook when ti-data failed"
data_run=$(systemctl show -p InvocationID --value ti-data.service)
test "$(tail -n 1 "$reports/hook-instances")" = "ti-data.service $data_run" ||
  fail "the data job's hook was given '$(tail -n 1 "$reports/hook-instances")', want 'ti-data.service $data_run'"
test "$(systemctl show -p Result --value "$data_hook")" = success || fail "$data_hook did not succeed"
test "$(pushover_pages)" = "$((data_pushover + 1))" || fail "the failed data job did not page Pushover once"
decoded_bodies | grep -q 'ti-data.service failed' || fail "the page does not name ti-data.service"
decoded_bodies | grep -q 'marker-D' || fail "the page's reason is not the failed data job's own line"
systemctl reset-failed ti-data.service
cp /deploy/systemd/ti-data.service /etc/systemd/system/ti-data.service
rm -f "$reports/unit-failures.json"
systemctl daemon-reload
pass "a failing data job starts $data_hook with %i = ti-data.service and the failed run's invocation, and it pages with the run's own line"

# The IRA's instance of the data job pages the same way: a stand-in
# ti-data@.service with the real template's OnFailure= line.
ira_data_hook=ti-unit-failed@ti-data@trad-ira.service.service
ondata=$(grep -x 'OnFailure=.*' /deploy/systemd/ti-data@.service) || fail "the IRA data job's unit names no OnFailure="
cat >/etc/systemd/system/ti-data@.service <<UNIT
[Unit]
Description=Stand-in IRA data job (rehearsal): fails as the real one can
$ondata

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "ti-data: HTTP 403 for session 2026-10-23, marker-I" >&2; exit 1'
UNIT
systemctl daemon-reload
rm -f "$reports/unit-failures.json"
before=$(instances)
data_pushover=$(pushover_pages)
systemctl start ti-data@trad-ira.service 2>/dev/null && fail "the stand-in IRA data job did not fail"
for _ in $(seq 150); do
  if test "$(instances)" -gt "$before" && test "$(systemctl show -p ActiveState --value "$ira_data_hook")" != activating; then break; fi
  sleep 0.1
done
test "$(instances)" -gt "$before" || fail "systemd did not start $ira_data_hook when ti-data@trad-ira failed"
ira_run=$(systemctl show -p InvocationID --value ti-data@trad-ira.service)
test "$(tail -n 1 "$reports/hook-instances")" = "ti-data@trad-ira.service $ira_run" ||
  fail "the IRA data job's hook was given '$(tail -n 1 "$reports/hook-instances")', want 'ti-data@trad-ira.service $ira_run'"
test "$(systemctl show -p Result --value "$ira_data_hook")" = success || fail "$ira_data_hook did not succeed"
test "$(pushover_pages)" = "$((data_pushover + 1))" || fail "the failed IRA data job did not page Pushover once"
decoded_bodies | grep -q 'ti-data@trad-ira.service failed' || fail "the page does not name ti-data@trad-ira.service"
decoded_bodies | grep -q 'marker-I' || fail "the page's reason is not the failed IRA data job's own line"
decoded_bodies | grep -q 'The data job fails' || fail "the IRA data job's page does not give the data job's manual row"
systemctl reset-failed ti-data@trad-ira.service
cp /deploy/systemd/ti-data@.service /etc/systemd/system/ti-data@.service
rm -f "$reports/unit-failures.json"
systemctl daemon-reload
pass "a failing IRA data job starts $ira_data_hook with %i = ti-data@trad-ira.service and the failed run's invocation, and it pages with the run's own line and the data job's manual row"

# The maintenance window, as the owner meets it (deploy/rehearsal/maintenance-window.sh).
. /deploy/rehearsal/maintenance-window.sh
rehearse_maintenance_window

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
# After the unlock the reporter's own unit is no longer refused (a start that
# exits 0 is not an assertion failure; a finished oneshot is unloaded, so its
# journal is what is read). The server is
# provisioned and no engine has run, so there is no run.env: the firing waits
# quietly for the account's first run, which is a success, not a failed unit
# (docs/manual/server.md, "The reporter before the first run").
test ! -e /var/lib/trend-investing/engines/boost/run.env || fail "a run.env exists before the reporter's first-run wait is checked"
systemctl start ti-reporter@boost.service 2>/dev/null || fail "the reporter's firing with no run.env failed: $(journalctl -u ti-reporter@boost.service --no-pager -o cat | tail -n 5)"
! systemctl is-failed --quiet ti-reporter@boost.service || fail "the reporter's unit is failed with no run.env"
waiting=$(journalctl -u ti-reporter@boost.service --no-pager -o cat)
grep -q "waiting for the account's first run" <<<"$waiting" || fail "the reporter did not say it is waiting for the first run: $waiting"
! grep -q 'missing or empty: start' <<<"$waiting" || fail "the reporter still fails its start step with no run.env: $waiting"
# The data job is the same: with no run.env its firing waits and succeeds.
systemctl start ti-data.service 2>/dev/null || fail "the data job's firing with no run.env failed: $(journalctl -u ti-data.service --no-pager -o cat | tail -n 5)"
! systemctl is-failed --quiet ti-data.service || fail "the data job's unit is failed with no run.env"
data_said=$(journalctl -u ti-data.service --no-pager -o cat)
grep -q "waiting for the account's first run" <<<"$data_said" || fail "the data job did not say it is waiting for the first run: $data_said"
! grep -q 'refused\|missing or empty: start' <<<"$data_said" || fail "the data job refused with no run.env: $data_said"
systemctl start ti-data@trad-ira.service 2>/dev/null || fail "the IRA data job's firing with no run.env failed: $(journalctl -u ti-data@trad-ira.service --no-pager -o cat | tail -n 5)"
! systemctl is-failed --quiet ti-data@trad-ira.service || fail "the IRA data job's unit is failed with no run.env"
data_said=$(journalctl -u ti-data@trad-ira.service --no-pager -o cat)
grep -q "waiting for the account's first run.*start the trad-ira engine once" <<<"$data_said" || fail "the IRA data job did not say it is waiting for the first run: $data_said"
test -z "$(on_disk "$marker")" || fail "the secret is on disk at the end: $(on_disk "$marker")"
pass "after the unlock the hook read its channels from the credential systemd handed it, the reporter's unit and the data job, no longer refused and with no run started, wait for the first run and succeed, and the secret is nowhere on disk"

# --- The owner's first dry-run day, step by step
# (docs/runbooks/first-dry-run-day.md).
. /deploy/rehearsal/first-day.sh
echo "REHEARSAL (systemd) complete"
