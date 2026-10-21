#!/bin/bash
# Runs inside a Mac mini container that deploy/mac-mini/up.sh made, for
# deploy/mac-mini/test-container.sh (which says what each part proves). Each
# subcommand checks one thing against the running systemd and exits non-zero
# with the reason if it does not hold; `fixtures` writes the test's own files.
# Never run it in the owner's container: `fixtures` installs a stand-in
# engine and points secrets.env at the test's stand-in on the Mac.
set -euo pipefail
set +x

fail() { echo "CHECK FAIL: $*" >&2; exit 1; }
pass() { echo "CHECK ok: $*"; }

state=/var/lib/trend-investing
boost=$state/engines/boost
log=$boost/run-rehearsal.log
units_enabled="ti-data.timer ti-reporter@boost.timer ti-clock-check.timer ti-dashboard.service"
units_stopped="ti-engine@boost.service ti-adapter-boost.service"

# The four volumes, and in each a root-only file and a service user's file.
volumes="/etc/trend-investing $state /opt/trend-investing /var/log/journal"

wait_system() {
  local s=
  for _ in $(seq 1 90); do
    s=$(systemctl is-system-running 2>/dev/null || true)
    case $s in running | degraded) echo "$s"; return 0 ;; esac
    sleep 1
  done
  echo "${s:-none}"
}

# The stand-in for ntfy and PagerDuty runs on the Mac (test-container.sh
# starts it), reached as host.docker.internal on SINK_PORT, so it is there
# from the container's first boot, across restarts and re-creations, before
# the reporter's timer can fire. It records each request's path; GET /posts
# lists them and GET /reset clears them.
sink=http://host.docker.internal:${SINK_PORT:-0}
sink_posts() { curl -fsS --connect-timeout 2 --max-time 10 "$sink/posts"; }

case ${1:-} in
  # T1: systemd is PID 1 and the system is up. ALLOWED names the units that
  # may have failed (before the test's files exist, the reporter fails: no
  # run.env yet).
  boot)
    test "$(cat /proc/1/comm)" = systemd || fail "PID 1 is $(cat /proc/1/comm), not systemd"
    # Checked once the boot has finished: straight after a restart the
    # early units may not have run yet.
    s=$(wait_system)
    # Shared mount propagation, as systemd sets it on a machine of its own:
    # without it the credentials systemd hands a unit (LoadCredential) never
    # appear in /run/credentials, and the reporter and the data job cannot
    # read their secrets.
    p=$(findmnt -n -o PROPAGATION /)
    test "$p" = shared || fail "/ has $p mount propagation, not shared"
    # binfmt_misc is the Docker VM's, shared by every container: systemd's
    # binfmt units would clear it and so remove Docker Desktop's Rosetta
    # handler, after which no amd64 container runs on the Mac.
    for u in systemd-binfmt.service proc-sys-fs-binfmt_misc.automount proc-sys-fs-binfmt_misc.mount; do
      e=$(systemctl is-enabled "$u" 2>&1 || true)
      test "$e" = masked || fail "$u is $e, not masked: it would rewrite the Docker VM's binfmt handlers"
    done
    failed=$(systemctl list-units --state=failed --no-legend --plain | awk '{print $1}' | sort | tr '\n' ' ')
    for u in $failed; do
      case " ${ALLOWED:-} " in *" $u "*) ;; *) fail "unit $u failed (system $s); journalctl -u $u: $(journalctl -u "$u" -n 5 --no-pager -o cat)" ;; esac
    done
    if test -z "${ALLOWED:-}"; then test "$s" = running || fail "systemctl is-system-running says $s"; fi
    pass "PID 1 is systemd; is-system-running: $s; failed: ${failed:-none}"
    ;;

  # The test's own files: a stand-in engine (it records what systemd handed
  # it), a durable log the engine wrote (the rehearsal's Boost fixture), the
  # run.env naming it, secrets pointing at the loopback stand-in, an owner's
  # edit to the adapter's configuration, and the sentinels.
  fixtures)
    cat >/opt/trend-investing/bin/ti-engine <<'ENGINE'
#!/bin/bash
# A stand-in for the engine: records what its unit handed it, then waits.
{
  echo "user=$(id -un)"
  echo "runtime_mode=$(stat -c %a "$RUNTIME_DIRECTORY")"
  echo "credential=$(test -r "$CREDENTIALS_DIRECTORY/secrets" && echo readable || echo missing)"
  if touch /usr/standin-probe 2>/dev/null; then echo "usr=writable"; else echo "usr=read-only"; fi
  echo "args=$*"
} >"$TI_STATE_DIR/standin-proof"
exec sleep infinity
ENGINE
    chmod 0755 /opt/trend-investing/bin/ti-engine
    install -d /opt/trend-investing/adapter-venv/bin
    install -m 0755 /dev/null /opt/trend-investing/adapter-venv/bin/python3
    install -o ti-trader -g ti-trader -m 0640 /root/dist/boost-fixture.log "$log"
    printf 'TI_RUN=run-rehearsal\nTI_AS_OF=2026-10-20T20:00:00Z\nTI_RESTART_ARGS=\n' >"$boost/run.env"
    chown ti-trader:ti-trader "$boost/run.env"
    chmod 0640 "$boost/run.env"
    test "${SINK_PORT:-0}" != 0 || fail "SINK_PORT is not set: run this from test-container.sh"
    sed -i -e 's|^TI_NTFY_TOPIC=.*|TI_NTFY_TOPIC=rehearsal|' \
      -e "s|^TI_NTFY_SERVER=.*|TI_NTFY_SERVER=$sink|" \
      -e "s|^TI_PAGERDUTY_ROUTING_KEY=.*|TI_PAGERDUTY_ROUTING_KEY=fake\\nTI_PAGERDUTY_URL=$sink/v2/enqueue|" \
      /etc/trend-investing/secrets.env
    echo '# filled in by the owner (test sentinel)' >>/etc/trend-investing/secrets.env
    sed -i 's/"mark_timeout": 15/"mark_timeout": 16/' /etc/trend-investing/adapters/boost.json
    for v in $volumes; do
      head -c 256 /dev/urandom | base64 >"$v/sentinel-root"
      chown root:root "$v/sentinel-root"
      chmod 0600 "$v/sentinel-root"
      head -c 256 /dev/urandom | base64 >"$v/sentinel-trader"
      chown ti-trader:ti-trader "$v/sentinel-trader"
      chmod 0750 "$v/sentinel-trader"
    done
    token=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
    echo "$token" >/etc/trend-investing/journal-token
    logger -t ti-test "journal sentinel $token"
    journalctl --flush
    pass "fixtures written"
    ;;

  # T1: every unit verifies, and the enabled ones and the engine start under
  # their hardening, as systemd reports it and as the processes see it.
  units)
    verify_log=$(mktemp)
    status=0
    (cd /etc/systemd/system && systemd-analyze verify ti-data.service ti-data.timer \
      ti-engine@boost.service ti-engine@trad-ira.service ti-engine@roth-ira.service \
      ti-reporter@boost.service ti-reporter@boost.timer ti-dashboard.service ti-adapter-boost.service \
      ti-clock-check.service ti-clock-check.timer) >"$verify_log" 2>&1 || status=$?
    # The Gateway's name is masked here, which verify refuses, so its file
    # from deploy/ is checked under another name.
    gw=$(mktemp -d)
    cp /root/deploy/systemd/ti-gateway.service "$gw/ti-gateway-file.service"
    systemd-analyze verify "$gw/ti-gateway-file.service" >>"$verify_log" 2>&1 || status=$?
    rm -rf "$gw"
    cat "$verify_log"
    if test "$status" -ne 0 || test -s "$verify_log"; then fail "systemd-analyze verify exited $status or reported problems (above)"; fi
    pass "systemd-analyze verify clean on every ti-* unit, the Gateway's from deploy/"

    test "$(systemctl is-enabled ti-gateway.service 2>&1 || true)" = masked || fail "ti-gateway is $(systemctl is-enabled ti-gateway.service 2>&1 || true), not masked"
    test "$(readlink /etc/systemd/system/ti-gateway.service)" = /dev/null || fail "ti-gateway's mask is not a link to /dev/null"
    pass "ti-gateway is masked"
    for u in $units_enabled; do
      test "$(systemctl is-enabled "$u")" = enabled || fail "$u is not enabled"
    done
    for u in $units_stopped; do
      test -f "/etc/systemd/system/${u/@boost/@}" || fail "$u's unit file is not installed"
      e=$(systemctl is-enabled "$u" 2>&1 || true)
      test "$e" = disabled || fail "$u is $e, not disabled"
    done
    pass "timers and dashboard enabled; engine and adapter installed and disabled"

    for u in ti-data.service ti-reporter@boost.service ti-dashboard.service ti-engine@boost.service ti-adapter-boost.service ti-clock-check.service; do
      got=
      for prop in ProtectSystem ProtectHome NoNewPrivileges PrivateTmp; do
        got="$got $prop=$(systemctl show "$u" -p "$prop" --value)"
      done
      test "$got" = " ProtectSystem=strict ProtectHome=yes NoNewPrivileges=yes PrivateTmp=yes" || fail "$u's effective hardening is$got"
    done
    pass "systemd applies ProtectSystem=strict, ProtectHome, NoNewPrivileges and PrivateTmp to every ti-* service"

    # The dashboard, live.
    systemctl restart ti-dashboard.service
    for _ in $(seq 30); do systemctl is-active --quiet ti-dashboard.service && curl -fsS --connect-timeout 2 --max-time 10 -o /dev/null http://127.0.0.1:8088/ && break; sleep 1; done
    systemctl is-active --quiet ti-dashboard.service || fail "ti-dashboard is not active: $(journalctl -u ti-dashboard -n 5 --no-pager -o cat)"
    curl -fsS --connect-timeout 2 --max-time 10 -o /dev/null http://127.0.0.1:8088/ || fail "the dashboard does not answer on 127.0.0.1:8088"
    pid=$(systemctl show ti-dashboard.service -p MainPID --value)
    grep -q '^NoNewPrivs:[[:space:]]*1' "/proc/$pid/status" || fail "the dashboard's process may gain privileges"
    awk '$5 == "/" {print $6}' "/proc/$pid/mountinfo" | grep -q '^ro' || fail "the dashboard sees / writable"
    test "$(stat -c %U "/proc/$pid")" = ti-trader || fail "the dashboard does not run as ti-trader"
    pass "ti-dashboard runs as ti-trader, answers on loopback, with / read-only and no new privileges in its namespace"

    # The reporter, fired by hand: its credential is handed over and used.
    curl -fsS --connect-timeout 2 --max-time 10 -o /dev/null "$sink/reset" || fail "the stand-in on the Mac does not answer at $sink"
    systemctl start ti-reporter@boost.service || fail "ti-reporter@boost failed: $(journalctl -u ti-reporter@boost -n 10 --no-pager -o cat)"
    posts=$(sink_posts) || fail "the stand-in on the Mac does not list its requests"
    grep -q '^/rehearsal$' <<<"$posts" || fail "the reporter reached no ntfy stand-in: $posts"
    grep -q '^/v2/enqueue$' <<<"$posts" || fail "the reporter reached no PagerDuty stand-in: $posts"
    test "$(stat -c %U "$state/reports/boost.reporter.json")" = ti-trader || fail "the reporter's state is not ti-trader's"
    pass "ti-reporter@boost runs under its unit, reads its secrets through LoadCredential and delivers to both stand-ins"

    # The data job: it runs, reads its credential and refuses the empty key.
    if systemctl start ti-data.service 2>/dev/null; then fail "ti-data succeeded with no Massive key"; fi
    journalctl -u ti-data.service --no-pager -o cat | grep -q 'ti-data refused: .*MASSIVE_API_KEY' || fail "ti-data failed for another reason: $(journalctl -u ti-data -n 10 --no-pager -o cat)"
    systemctl reset-failed ti-data.service
    pass "ti-data runs under its unit and refuses the missing key, naming it"

    # The engine (a stand-in): started by hand, it is handed its credential
    # and a private runtime folder 0700, and cannot write /usr.
    rm -f "$boost/standin-proof"
    systemctl start ti-engine@boost.service
    for _ in $(seq 20); do test -s "$boost/standin-proof" && break; sleep 0.5; done
    proof=$(cat "$boost/standin-proof" 2>/dev/null || true)
    case $proof in *"user=ti-trader"*) ;; *) fail "the engine stand-in did not run as ti-trader: $proof" ;; esac
    case $proof in *"runtime_mode=700"*) ;; *) fail "the engine's runtime folder is not 0700: $proof" ;; esac
    case $proof in *"credential=readable"*) ;; *) fail "the engine was not handed its secrets: $proof" ;; esac
    case $proof in *"usr=read-only"*) ;; *) fail "the engine could write /usr: $proof" ;; esac
    case $proof in *"-socket /run/ti-engine-boost/engine.sock"*) ;; *) fail "the engine's socket is not in its runtime folder: $proof" ;; esac
    systemctl is-active --quiet ti-engine@boost.service || fail "the engine stand-in is not running"
    rm -f "$boost/standin-proof"
    pass "ti-engine@boost (stand-in) runs as ti-trader with its credential, /run/ti-engine-boost 0700 and / read-only"
    ;;

  # The rehearsal's checks of the owner's tools, as on main: both run as
  # ti-trader, and journal-query reads the durable log.
  tools)
    for b in operator journal-query; do
      out=$(sudo -u ti-trader "$b" 2>&1 || true)
      case $out in "usage: $b"*) ;; *) fail "sudo -u ti-trader $b does not run the installed tool: $out" ;; esac
    done
    sudo -u ti-trader journal-query orders "$log" >/dev/null || fail "journal-query cannot read $log"
    pass "operator and journal-query run as ti-trader; journal-query reads the durable log"
    ;;

  # T2: what must survive, one line each (content hash, owner by number and
  # name, mode, path), the journal-query reading, and the journal sentinel.
  state-manifest)
    files="$log /etc/trend-investing/secrets.env /etc/trend-investing/adapters/boost.json /etc/trend-investing/journal-token"
    for v in $volumes; do files="$files $v/sentinel-root $v/sentinel-trader"; done
    for f in $files; do
      test -f "$f" || fail "$f is missing"
      printf '%s %s %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$(stat -c '%u:%g %U:%G %a' "$f")" "$f"
    done
    echo "journal-query $(sudo -u ti-trader journal-query orders "$log" | sha256sum | cut -d' ' -f1)"
    token=$(cat /etc/trend-investing/journal-token)
    journalctl -t ti-test -o cat --no-pager | grep -qx "journal sentinel $token" || fail "the journal no longer holds the sentinel written before (machine $(cat /etc/machine-id))"
    echo "journal sentinel present"
    ;;

  # What provisioning owns: a second pass must leave every line the same.
  provision-manifest)
    paths="/etc/trend-investing /opt/trend-investing /usr/local/libexec/trend-investing /usr/local/bin /etc/systemd/system /etc/systemd/journald.conf.d"
    # shellcheck disable=SC2086
    {
      find $paths -printf '%y %m %U:%G %p -> %l\n'
      find $paths -type f -exec sha256sum {} +
    } | sort
    getent passwd ti-trader ti-gateway ti-operator
    ;;

  # T3: after a restart, the timers and the dashboard are back, the engine
  # and the adapter are stopped, and the reporter's timer fires within 5
  # minutes of boot.
  after-restart)
    for u in ti-data.timer ti-reporter@boost.timer ti-clock-check.timer; do
      systemctl is-active --quiet "$u" || fail "$u is not active after the restart"
    done
    for _ in $(seq 30); do systemctl is-active --quiet ti-dashboard.service && break; sleep 1; done
    systemctl is-active --quiet ti-dashboard.service || fail "ti-dashboard is not back: $(journalctl -u ti-dashboard -b -n 5 --no-pager -o cat)"
    for u in $units_stopped; do
      a=$(systemctl is-active "$u" 2>&1 || true)
      test "$a" = inactive || fail "$u is $a after the restart, not inactive"
    done
    test "$(systemctl show ti-engine@boost.service -p ExecMainStartTimestampMonotonic --value)" = 0 || fail "the engine was started during boot"
    pass "timers and dashboard back; engine and adapter stopped (the engine was running before)"
    # Monotonic times count from the Docker VM's boot, which the container
    # shares; the container's own boot is when its systemd started.
    booted=$(systemctl show -p UserspaceTimestampMonotonic --value)
    fired=0
    for _ in $(seq 1 340); do
      if test "$(systemctl show ti-reporter@boost.service -p ExecMainStartTimestampMonotonic --value)" != 0; then fired=1; break; fi
      sleep 1
    done
    test "$fired" = 1 || fail "the reporter did not fire within 5 minutes of the restart"
    first=$((($(systemctl show ti-reporter@boost.service -p ExecMainStartTimestampMonotonic --value) - booted) / 1000000))
    test "$first" -le 300 || fail "the reporter first fired $first s after boot, more than 5 minutes"
    for _ in $(seq 60); do
      a=$(systemctl show ti-reporter@boost.service -p ActiveState --value)
      case $a in activating | deactivating) sleep 1 ;; *) break ;; esac
    done
    r=$(systemctl show ti-reporter@boost.service -p Result --value)
    test "$r" = success || fail "the reporter's first firing after the restart ended '$r': $(journalctl -u ti-reporter@boost -b -n 10 --no-pager -o cat)"
    pass "the reporter's timer fired $first s after the container's boot and the run succeeded"
    s=$(wait_system)
    test "$s" = running || fail "is-system-running says $s after the restart: $(systemctl list-units --state=failed --no-legend --plain)"
    pass "is-system-running: running"
    ;;

  # The clock step passes in the container, and fails closed when the
  # reference says the clock is an hour off.
  clock)
    TI_TARGET=mac-mini /root/deploy/provision.sh clock || fail "the clock step failed in the container"
    # The timer's unit, under its own hardening.
    systemctl start ti-clock-check.service || fail "ti-clock-check failed: $(journalctl -u ti-clock-check -n 5 --no-pager -o cat)"
    journalctl -u ti-clock-check.service --no-pager -o cat | grep -q 'clock: ok, skew' || fail "ti-clock-check did not compare with a reference: $(journalctl -u ti-clock-check -n 5 --no-pager -o cat)"
    stubs=$(mktemp -d)
    cat >"$stubs/curl" <<'STUB'
#!/bin/bash
printf 'HTTP/2 200\r\nDate: %s\r\n\r\n' "$(date -u -d '+1 hour' '+%a, %d %b %Y %H:%M:%S GMT')"
STUB
    chmod +x "$stubs/curl"
    if PATH="$stubs:$PATH" TI_TARGET=mac-mini /root/deploy/provision.sh clock >/dev/null 2>"$stubs/err"; then fail "a clock an hour off was accepted"; fi
    grep -Eq 'skew of -(359[89]|360[012])s' "$stubs/err" || fail "the clock step failed for another reason: $(cat "$stubs/err")"
    rm -rf "$stubs"
    pass "the clock step and ti-clock-check.service pass against a reference, and the step refuses one an hour off"
    ;;

  *) fail "usage: container-checks.sh boot|fixtures|units|tools|state-manifest|provision-manifest|after-restart|clock" ;;
esac
