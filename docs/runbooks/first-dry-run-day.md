# Runbook: starting Boost's first dry-run day on the server

*For the owner. One ordered checklist, run on the Lightsail server over
Tailscale SSH, that takes Boost from "programs installed, secrets unlocked,
Gateway logged in" to a dry-run engine that decides Monday's session after
its close. Dry-run sends nothing to IBKR, ever.*

Every command in a block below is run by `deploy/rehearse.sh` inside its
systemd container, exactly as written, against stand-ins (a synthetic Massive,
a scripted gateway, invented figures), and the lines in each "expect" block
are checked there (a line beginning `~` is a pattern, for times and counts). The container proves the commands, their order and their
output lines; it cannot prove the real Massive key, the real Gateway, the
tailnet or the phone prompt, which are marked **[real server only]**.

The plan, for a start on Saturday evening, 2026-10-24 (New York time):

| When | What |
|---|---|
| Saturday evening | Steps 0 to 8 below; the warm-up in step 5 takes about an hour and a half |
| Sunday 09:00 | IBC logs the Gateway in again from cold: **approve the phone prompt** ("Sunday, and the Gateway's cold restart") |
| Monday 11:30 | Look at the dashboard: the run is up, no decision yet (step 8) |
| Monday 16:00 close, 17:05 | The data job writes Monday's file; the adapter delivers it; the engine decides the session in dry-run ("Monday after the close") |

## 0. Check where the server stands

Nothing here changes anything.

<!-- rehearsed: preflight -->
```sh
sudo ti-secrets status | grep -E '^(secrets|accounts/boost)\.env'
sudo grep -c '^MASSIVE_API_KEY=.' /run/trend-investing/secrets/secrets.env
sudo grep -c '^IBKR_ACCOUNT_BOOST=DU' /run/trend-investing/secrets/accounts/boost.env
systemctl is-active ti-gateway
ss -ltn 'sport = :4002' | grep -c LISTEN
test -f /opt/trend-investing/adapter/adapter/ibkr/cli.py && echo adapter-code-installed
test -x /opt/trend-investing/bin/ti-data && test -x /opt/trend-investing/bin/ti-engine && echo programs-installed
test ! -e /var/lib/trend-investing/engines/boost/run.env && echo no-run-env
systemctl is-active ti-data.timer ti-reporter@boost.timer || true
```

<!-- expect: preflight -->
```text
accounts/boost.env       encrypted, unlocked
secrets.env              encrypted, unlocked
1
1
active
1
adapter-code-installed
programs-installed
no-run-env
inactive
inactive
```

If `adapter-code-installed` is missing, install it from the checkout you
provisioned from: `sudo deploy/provision.sh adapter`. If a `1` is a `0`, the
secret is missing: `sudo ti-secrets edit secrets.env` (the Massive key) or
`sudo ti-secrets edit accounts/boost.env` (`IBKR_ACCOUNT_BOOST=DU...`, no
quotes, no spaces), then start this step again. Never paste a key or the
account number into a chat, a ticket or this repository.

## 1. Name the paper account in Boost's configuration

The adapter reads the account `IBKR_ACCOUNT_BOOST` names from IB Gateway and
hands the engine a reconciliation for it; the engine refuses a reconciliation
for any account but the one its configuration names. The shipped dry-run
configuration names the placeholder `joint`, so, as for the paper copy
(`docs/runbooks/first-paper-order.md`, A5), the account number goes into
`/etc/trend-investing/engines/boost.config` on the server. The number is read
from the unlocked account file and never printed. It stays in `/etc` only:
never put it in the repository. Do not run `sudo deploy/provision.sh services`
again afterwards: it copies the shipped file back over this one.

<!-- rehearsed: account -->
```sh
account=$(sudo sed -n 's/^IBKR_ACCOUNT_BOOST=//p' /run/trend-investing/secrets/accounts/boost.env)
sudo sed -i "s/\"account_id\": \"joint\"/\"account_id\": \"$account\"/" /etc/trend-investing/engines/boost.config
unset account
sudo grep -c '"account_id": "DU' /etc/trend-investing/engines/boost.config
sudo grep -c '"mode": "dry-run"' /etc/trend-investing/engines/boost.config
```

<!-- expect: account -->
```text
1
1
```

## 2. Approve the first run with its starting book

Even in dry-run the engine decides only from a book it can trust: the
adapter compares its first reading of the account with the book you state,
and with none the engine halts on that first reading ("reconciliation
unverifiable"; `docs/manual/boost.md`). In IBKR (Client Portal), read the paper
account's holdings and its **total cash** (`TotalCashValue`, in USD). Replace
`100000.00` with that figure, digits and a decimal point only. If the account
holds SPY, replace `-positions ""` with `-positions SPY=<shares>`. Approve right
after you read it, with no order working.

<!-- rehearsed: approve -->
```sh
cash=100000.00
account=$(sudo sed -n 's/^IBKR_ACCOUNT_BOOST=//p' /run/trend-investing/secrets/accounts/boost.env)
sudo -u ti-trader operator approve -first-run -account "$account" -by owner -positions "" -cash "$cash" -currency USD -out /var/lib/trend-investing/engines/boost/first-run-approval.json
unset account
```

<!-- expect: approve -->
```text
wrote /var/lib/trend-investing/engines/boost/first-run-approval.json: start the first run with -approval /var/lib/trend-investing/engines/boost/first-run-approval.json (docs/runbooks/first-paper-order.md)
```

It also prints the book it recorded ("holdings none, total cash ... USD");
check that against IBKR once more before going on.

## 3. Write the run file

`TI_RUN` names this run's durable log and journal; it must be new (a log is
never reused), and a letter-and-digit name with dashes such as
`run-2026-10-24` is the convention. `TI_AS_OF` is the first session the
engine decides: sessions closing before it only warm the indicators. Monday
2026-10-26 closes at 16:00 New York time, 20:00 UTC (daylight time lasts until
1 November), so `2026-10-26T20:00:00Z` makes Friday's session warm-up only and
Monday's the first decided; a Saturday or Sunday start changes nothing here.
`TI_RESTART_ARGS` hands the engine the approval from step 2.

<!-- rehearsed: run-env -->
```sh
sudo -u ti-trader tee /var/lib/trend-investing/engines/boost/run.env >/dev/null <<'EOF2'
TI_RUN=run-2026-10-24
TI_AS_OF=2026-10-26T20:00:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/boost/first-run-approval.json
EOF2
sudo cat /var/lib/trend-investing/engines/boost/run.env
```

<!-- expect: run-env -->
```text
TI_RUN=run-2026-10-24
TI_AS_OF=2026-10-26T20:00:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/boost/first-run-approval.json
```

## 4. Start the engine

<!-- rehearsed: engine -->
```sh
sudo systemctl start ti-engine@boost
sleep 3
systemctl is-active ti-engine@boost
sudo journalctl -u ti-engine@boost -n 3 --no-pager -o cat
ls /var/lib/trend-investing/engines/boost/
```

<!-- expect: engine -->
```text
active
Started ti-engine@boost.service - Trend Investing engine for account boost (own adapter client ID, config, durable log).
recorded the owner's approval of a fresh run after  at sequence 2; the adapter's first input carries sequence 3
listening on /run/ti-engine-boost/engine.sock
run-2026-10-24.log
```

The engine's own journal, `/var/lib/trend-investing/engines/boost/run-2026-10-24.journal`,
appears when it stops, not now. If the unit is not `active`, read
`sudo journalctl -u ti-engine@boost -n 20`; the engine's refusals are listed in
`docs/manual/engine.md`. Do not start it a second time with the same `TI_RUN`:
a log is never reused.

## 5. The one-time warm-up

The engine needs history before its first decision (its 200-day average and
its RSI). The data job fetches it once from Massive, from the configuration's
`warm_up_start`, 2025-01-02, which the free plan's two-year window still covers
(`docs/manual/market-data.md`, "The one-time warm-up"). The data job asks
for one session at a time at the free plan's 5 requests a minute, about 12.5
seconds each, so the 450 or so sessions take **about an hour and a half**.
It runs as its own transient unit, so closing your terminal does not stop it,
and the unit reads the Massive key from the unlocked secrets, never from
`/etc/trend-investing/secrets.env` (which no longer exists in plain text).
Start it, then wait for it to end:

<!-- rehearsed: warmup -->
```sh
sudo systemd-run --unit=ti-warmup --collect -p User=ti-trader -p Group=ti-trader \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/boost.config \
  --engine-log /var/lib/trend-investing/engines/boost/run-2026-10-24.log \
  --backfill-from 2025-01-02
sleep 2
while systemctl is-active --quiet ti-warmup; do sleep 30; done
sudo journalctl -u ti-warmup --no-pager -o cat
ls /var/lib/trend-investing/data/streams/boost/run-2026-10-24/
```

<!-- expect: warmup -->
```text
Started ti-warmup.service - /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data --config /etc/trend-investing/engines/boost.config --engine-log /var/lib/trend-investing/engines/boost/run-2026-10-24.log --backfill-from 2025-01-02.
/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-23.jsonl
ti-warmup.service: Deactivated successfully.
boost-2026-10-23.jsonl
```

While it runs, `sudo journalctl -fu ti-warmup` shows it (Ctrl-C leaves it
running). It prints the build, then, at the end, the path of the one file it
wrote, named after the last settled session, Friday 2026-10-23. It is
resumable: if it stops (a Massive outage, a 429 streak) read why with
`sudo journalctl -u ti-warmup`, and run the same command again; it fetches only
what is missing. Do not run it for any other date: "Any other date is refused".

## 6. Start the adapter, in dry-run

The adapter carries the data job's files to the engine, behind a reading of
the account from IB Gateway, and (in paper) the engine's orders back. Its
shipped configuration, `/etc/trend-investing/adapters/boost.json`, is
`"mode": "dry-run"` and `"paper_sending_enabled": false`: it reads the account
and sends nothing, whatever the Gateway's API setting. A read-only Gateway is
fine. Do not edit that file.

<!-- rehearsed: adapter -->
```sh
sudo grep -E '"(mode|paper_sending_enabled)"' /etc/trend-investing/adapters/boost.json
sudo systemctl start ti-adapter-boost
for _ in $(seq 60); do sudo journalctl -u ti-adapter-boost --no-pager -o cat | grep -q 'session-closed:2026-10-23' && break; sleep 5; done
systemctl is-active ti-adapter-boost
sudo journalctl -u ti-adapter-boost --no-pager -o cat | grep -E 'adapter starting|link connected|reconciled|stream queued|stopped|refused'
sudo journalctl -u ti-adapter-boost --no-pager -o cat | grep 'session-closed:2026-10-23'
sudo grep -c '"state":"halted"' /var/lib/trend-investing/engines/boost/run-2026-10-24.log || true
```

<!-- expect: adapter -->
```text
  "mode": "dry-run",
  "paper_sending_enabled": false,
active
~ ibkr adapter starting mode=dry-run client_id=11 sending=off
~ ibkr link connected
~ ibkr reconciled client_id=11 positions=0 open_orders=0 executions=0
~ ibkr stream queued run=run-2026-10-24 files=boost-2026-10-23.jsonl inputs=[0-9]+
~ input=marketdata:session-closed:2026-10-23 type=market.session.closed answered with
0
```

The adapter connects to the Gateway, reads the account, hands the engine the
reconciliation against the book you approved, then delivers the warm-up file
to the engine, one input at a time. That takes a minute or two (about 900
inputs). `halted` must stay `0`: a `1` means the engine halted on its first
reading (the book you typed is not what the adapter read; start again with the
book as IBKR shows it, `docs/manual/boost.md`, the first alert row under
"What can go wrong"). If the unit is `failed`, `sudo journalctl -u
ti-adapter-boost -n 30` says why; the common causes are the Gateway not
listening on 4002 or the account number in `accounts/boost.env` not being the
one the Gateway is logged in to (`docs/manual/ibkr-adapter.md`, "Every stop,
and the command that settles it").

## 7. Turn the two stopped timers back on

You stopped `ti-data.timer` and `ti-reporter@boost.timer` by hand because their
units fail until `run.env` exists. It exists now, and the engine and the adapter
are up. Start them, then run each unit once by hand to see what a weekend
firing does: the data job finds Friday's file already written and fetches
nothing new to record; the reporter reads the log and sends nothing.

<!-- rehearsed: timers -->
```sh
sudo systemctl start ti-data.timer ti-reporter@boost.timer
systemctl is-active ti-data.timer ti-reporter@boost.timer
sudo systemctl start ti-data.service ti-reporter@boost.service
systemctl show -p ActiveState -p Result ti-data.service
systemctl show -p ActiveState -p Result ti-reporter@boost.service
sudo journalctl -u ti-data.service --no-pager -o cat | grep '^/var/lib' | tail -n 1
systemctl list-timers ti-data.timer ti-reporter@boost.timer --no-pager
```

<!-- expect: timers -->
```text
active
active
Result=success
ActiveState=inactive
Result=success
ActiveState=inactive
/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-23.jsonl
~ ti-data\.timer +ti-data\.service
~ ti-reporter@boost\.timer +ti-reporter@boost\.service
```

`ti-data.timer` fires Monday to Friday at 17:05, 18:05 and 20:05 New York time
and never on a weekend. `ti-reporter@boost.timer` fires every 5 minutes, all
week. On Saturday and Sunday there is no new session, so the data job's
weekend runs (if any, such as a catch-up after a reboot) find the last
settled session, Friday's, already written, record nothing and succeed; the
reporter's weekend firings find nothing to report and succeed. A failure of
either pages you through the reporter's channels like any failed unit
(`docs/manual/server.md`, "A failed unit"), so no page is the expected
state.

## 8. The dashboard

`ti-dashboard` listens on loopback only. It starts with the server; check it,
and check that Tailscale publishes it to your tailnet and to nobody else.

<!-- rehearsed: dashboard -->
```sh
sudo systemctl start ti-dashboard
sleep 3
systemctl is-active ti-dashboard
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088/
curl -s http://127.0.0.1:8088/ | grep -c 'Read-only'
```

<!-- expect: dashboard -->
```text
active
200
1
```

**[real server only]** Then, on the server:

```sh
sudo tailscale serve status
```

It should show your server's `https://trend-investing-1.<your tailnet>.ts.net`
name, "(tailnet only)", proxying `/` to `http://127.0.0.1:8088`. If it prints
nothing (the dashboard was never published), run `sudo tailscale serve --bg 8088`
once and check again. Never `tailscale funnel`: that publishes to the whole
internet. Then open the address from your phone or laptop (on Tailscale). The
page should say "Read-only", name the Boost account, and show its engine state
as normal.

## Sunday, and the Gateway's cold restart

IBC restarts the Gateway every night at 23:45 New York time and logs in again
**from cold every Sunday at 09:00**, which needs your phone's approval
(`docs/runbooks/gateway-login.md`, `docs/manual/server.md`). With the engine
and the adapter running through it:

- The Gateway drops the adapter's connection. The adapter says
  `link state=connectivity-lost` or `link state=disconnected` in
  `sudo journalctl -u ti-adapter-boost`, then retries with growing pauses, up
  to 10 attempts (about eight minutes), and matches IBKR's orders and balances
  again before it does anything on the new connection
  (`docs/manual/ibkr-adapter.md`, "The Gateway's nightly restart or Sunday
  re-login"). Dry-run sends nothing, so there is nothing to duplicate.
- **Approve the phone prompt when it comes, soon after 09:00.** If you do not,
  the Gateway keeps asking (`ti-gateway` restarts itself 30 seconds after IBC
  exits) and the adapter, which gives up after its attempts, stops with `gave
  up after N attempts`; the reporter pages that. Nothing is lost: approve the
  prompt, wait until `ss -ltn 'sport = :4002'` shows the port listening, and
  start it again with `sudo systemctl start ti-adapter-boost`. A restarted
  adapter picks up where the engine's log says it was.
- The nightly 23:45 restart (tonight's included) is the same drop and
  reconnect without a phone prompt.
- The engine and the data job are not affected by the Gateway.
- Not rehearsed against a real Gateway (only against the scripted one):
  `adapter/ibkr/tests/test_connection.py` covers the lost-link and reconnect
  paths.

## Monday after the close

- 11:30 New York: the dashboard (step 8) shows Boost's engine state as normal
  and no decided target yet. That is right: Monday's session is decided after
  its close, not before. Also check `systemctl is-active ti-engine@boost
  ti-adapter-boost ti-gateway` prints `active` three times, and that your phone
  has had no page.
- 16:00 the market closes. The data job fires at 17:05 (and again at 18:05 and
  20:05 if the first attempt fails; once the file exists they find it and
  fetch nothing). It writes
  `/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-26.jsonl`.
- The adapter, still running, delivers that file to the engine behind a
  reading of the account taken after the close, and the engine decides the
  session `TI_AS_OF` named: in dry-run it journals what it would have done and
  sends nothing. From about 17:10, `sudo grep -c
  '"type":"strategy.targets.decided"'
  /var/lib/trend-investing/engines/boost/run-2026-10-24.log` prints `1`
  and the dashboard's "targets the strategy decided" card shows the decision.
- If the file is not there by 17:30: `sudo journalctl -u ti-data.service -n
  20`, then `sudo systemctl start ti-data.service` once the cause (usually
  Massive not having published Monday's bars yet) is gone.
