# Runbook: starting Boost's first dry-run day on the server

*For the owner. One ordered checklist, run on the Lightsail server over
Tailscale SSH, that takes Boost from "programs installed, secrets unlocked,
Gateway logged in" to a dry-run engine that decides Monday's session after
its close. Dry-run sends nothing to IBKR, ever.*

> **`sudo deploy/provision.sh services` keeps the `boost.config` you edit in
> step 1.** It installs an engine configuration only when the server has
> none, and puts the shipped one beside yours as `boost.config.example`
> ([server](../manual/server.md), "Caution"). A checkout from before
> 2026-10-28 copied the shipped file, with the account `joint`, back over
> yours; if a `services` from such a checkout ran after step 1, do step 1
> again (it is safe to repeat) and restart nothing else.
>
> **It also reinstalls every Go program in the checkout's `dist/`** over
> `/opt/trend-investing/bin` and restarts the dashboard on it. On the live
> server, once a `ti-release` has installed newer builds, move `dist/` aside
> (or leave it empty) before `services`, else a stale `dist/` downgrades them
> (`docs/runbooks/lightsail-deployment.md`). The settings files
> (`engines/*.env`) are kept as `boost.config` is.

**How to use the blocks.** Paste one block at a time into your login shell on
the server, then compare what it printed with the "expect" block under it.
Stop at the first mismatch: do not paste the next block. A block that has to
stop when one of its checks fails begins `( set -euo pipefail` and ends with
`)`: it is a subshell with errexit on, so a failed check ends that block and
leaves your shell open, and the lines after it do not run. (A plain `false`
in an ordinary login shell stops nothing; that is why the wrapper is there.)

Every command in a block below is run by `deploy/rehearse.sh` inside its
systemd container, as an ordinary login named `owner` with sudo (not as root,
so a command that needs `sudo` and lacks it fails there), exactly as written,
in a shell without errexit, as your login shell is (so a block stops only if
it stops itself), against stand-ins (a synthetic Massive,
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
| Monday 16:00 close; Tuesday 05:30 to 06:05 | The Flex reader writes its executions proof; the data job writes Monday's file; the adapter delivers it; the engine decides the session in dry-run ("Monday after the close") |

## 0. Check where the server stands

Nothing here changes anything except the `systemctl stop` line, which stops the data
job's and the reporter's timers if provisioning started them. Their units wait
quietly until `run.env` exists (step 3), so they do no harm; they are stopped
so that nothing fires while the warm-up and the first start are under way, and
step 7 starts them again.

<!-- rehearsed: preflight -->
```sh
( set -euo pipefail
sudo ti-secrets status | grep -E '^(secrets|accounts/boost)\.env'
sudo grep -c '^MASSIVE_API_KEY=.' /run/trend-investing/secrets/secrets.env
sudo grep -c '^IBKR_ACCOUNT_BOOST=DU' /run/trend-investing/secrets/accounts/boost.env
systemctl is-active ti-gateway
ss -ltn 'sport = :4002' | grep -c LISTEN
test -f /opt/trend-investing/adapter/adapter/ibkr/cli.py || { echo "STOP: adapter code missing: run sudo deploy/provision.sh adapter"; exit 1; }
echo adapter-code-installed
{ test -x /opt/trend-investing/bin/ti-data && test -x /opt/trend-investing/bin/ti-engine; } || { echo "STOP: programs missing: from the checkout, with the programs copied from the Mac into dist/, install ti-data with sudo deploy/provision.sh services (only before step 1; see the warning at the top) and ti-engine with sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine, then start this step again"; exit 1; }
echo programs-installed
sudo test ! -e /var/lib/trend-investing/engines/boost/run.env || { echo "STOP: run.env already exists: this is not a first start; do not go on (see 'If the engine halts on its first reading')"; exit 1; }
echo no-run-env
sudo systemctl stop ti-data.timer ti-reporter@boost.timer
systemctl is-active ti-data.timer ti-reporter@boost.timer || true
)
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

The block stops at the first line that fails, so the last line it printed
is the one to look at; the three checks near the end print a line starting
`STOP:` that says what to do. If it says the adapter code is missing, install it from the checkout you
provisioned from: `sudo deploy/provision.sh adapter`. If a `1` is a `0`, the
secret is missing: `sudo ti-secrets edit secrets.env` (the Massive key) or
`sudo ti-secrets edit accounts/boost.env` (`IBKR_ACCOUNT_BOOST=DU...`, no
quotes, no spaces), then start this step again. Never paste a key or the
account number into a chat, a ticket or this repository.

## 1. Name the paper account in Boost's configuration

> `sudo deploy/provision.sh services` keeps the file you edit here: it
> installs a configuration only when there is none. If a checkout from
> before 2026-10-28 ran it after this step and put the account `joint` back,
> run this step again and restart nothing else.

The adapter reads the account `IBKR_ACCOUNT_BOOST` names from IB Gateway and
hands the engine a reconciliation for it; the engine refuses a reconciliation
for any account but the one its configuration names. The shipped dry-run
configuration names the placeholder `joint`, so, as for the paper copy
(`docs/runbooks/first-paper-order.md`, A5), the account number goes into
`/etc/trend-investing/engines/boost.config` on the server. The number is read
from the unlocked account file and never printed. **The paper account number
then sits in `boost.config` in plain text, in `/etc` only** (never put it in
the repository): the paper runbook's copy does the same, and a paper account
number is an identifier, not a credential; it opens nothing without the
Gateway's login. The block stops with a message if the configuration already
names a different account than `accounts/boost.env` (an earlier attempt with
another number): correct whichever is wrong and run it again. It is safe to
run twice, and it checks the result by comparing the two values exactly.

<!-- rehearsed: account -->
```sh
sudo bash -eu <<'EOF2'
config=/etc/trend-investing/engines/boost.config
account=$(sed -n 's/^IBKR_ACCOUNT_BOOST=//p' /run/trend-investing/secrets/accounts/boost.env)
if ! printf '%s\n' "$account" | grep -qE '^DU[A-Z0-9]{5,9}$' || ! printf '%s\n' "$account" | grep -q '[0-9]'; then
  echo "STOP: accounts/boost.env has no IBKR_ACCOUNT_BOOST=DU... line (DU, then 5 to 9 capital letters and digits with at least one digit, no quotes or spaces)."
  exit 1
fi
current=$(sed -n 's/^ *"account_id": *"\([^"]*\)".*/\1/p' "$config")
if [ "$current" = joint ]; then
  sed -i "s/\"account_id\": \"joint\"/\"account_id\": \"$account\"/" "$config"
elif [ "$current" != "$account" ]; then
  echo "STOP: boost.config already names a different account than accounts/boost.env. Change nothing; find out which is wrong."
  exit 1
fi
now=$(sed -n 's/^ *"account_id": *"\([^"]*\)".*/\1/p' "$config")
[ "$now" = "$account" ] || { echo "STOP: boost.config does not name the account of accounts/boost.env."; exit 1; }
echo account-named
grep -c '"mode": "dry-run"' "$config"
EOF2
```

<!-- expect: account -->
```text
account-named
1
```

If the account in the file is the wrong one:

<!-- expect: account-invalid -->
```text
~ STOP: accounts/boost.env has no IBKR_ACCOUNT_BOOST=DU
```

<!-- expect: account-refused -->
```text
~ STOP: boost.config already names a different account
```

## 2. Approve the first run with its starting book

Even in dry-run the engine decides only from a book it can trust: the
adapter compares its first reading of the account with the book you state,
and with none the engine halts on that first reading ("reconciliation
unverifiable"; `docs/manual/boost.md`). In IBKR (Client Portal), read the paper
account's holdings and its **total cash** (`TotalCashValue`, in USD). Replace
`100000.00` with that figure, digits and a decimal point only. If the account
holds anything, replace `-positions ""` with every holding, as
`-positions SPY=<shares>` or `-positions QQQ=5,SPY=10` (one `SYMBOL=shares` for
each holding, none left out); keep `""` only when it holds nothing. Approve right
after you read it, with no order working.

<!-- rehearsed: approve -->
```sh
( set -euo pipefail
cash=100000.00
account=$(sudo sed -n 's/^IBKR_ACCOUNT_BOOST=//p' /run/trend-investing/secrets/accounts/boost.env)
sudo -u ti-trader operator approve -first-run -account "$account" -by owner -positions "" -cash "$cash" -currency USD -out /var/lib/trend-investing/engines/boost/first-run-approval.json
)
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
( set -euo pipefail
sudo -u ti-trader tee /var/lib/trend-investing/engines/boost/run.env >/dev/null <<'EOF2'
TI_RUN=run-2026-10-24
TI_AS_OF=2026-10-26T20:00:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/boost/first-run-approval.json
EOF2
sudo cat /var/lib/trend-investing/engines/boost/run.env
)
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
( set -euo pipefail
sudo systemctl start ti-engine@boost
sleep 3
systemctl is-active ti-engine@boost
sudo journalctl -u ti-engine@boost -n 3 --no-pager -o cat
sudo ls /var/lib/trend-investing/engines/boost/
)
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
a log is never reused. (If the unit is not `active` the block stops at that
line and prints `inactive` or `failed`; the journal command is the one to run
by hand then.)

## 5. The one-time warm-up

The engine needs history before its first decision (its 200-day average and
its RSI). The data job fetches it once from Massive, from the configuration's
`warm_up_start`, 2025-01-02, which the free plan's two-year window still covers
(`docs/manual/market-data.md`, "The one-time warm-up"). The data job asks
for one session at a time at the free plan's 5 requests a minute, about 12.5
seconds each, so the 450 or so sessions take **about an hour and a half**.
Massive's free plan serves the previous session from the morning (the data
job takes its 403 as "not served yet" until 08:00 New York), so any start
from about 08:15 on the day after a session records it: this Saturday
evening's warm-up records up to Friday, and Monday's session, the first
decided, arrives in its own file on Tuesday morning. The data job's own
firings write nothing for the run until the warm-up has made its stream
folder, so there is no file to wait for before it.
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
```

<!-- expect: warmup -->
```text
~ Running as unit: ti-warmup\.service
```

Then wait for it, for at most 3 hours (twice what it should take):

<!-- rehearsed: warmup-wait -->
```sh
( set -euo pipefail
sleep 2
deadline=$((SECONDS + 10800))
late=0
while systemctl is-active --quiet ti-warmup; do
  if [ "$SECONDS" -ge "$deadline" ]; then late=1; break; fi
  sleep 30
done
if [ "$late" = 1 ]; then
  echo "STILL RUNNING after 3 hours (it should take about 90 minutes): see the paragraph below this block"
  exit 1
fi
sudo journalctl -u ti-warmup --no-pager -o cat
sudo ls /var/lib/trend-investing/data/streams/boost/run-2026-10-24/
)
```

<!-- expect: warmup-wait -->
```text
Started ti-warmup.service - /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data --config /etc/trend-investing/engines/boost.config --engine-log /var/lib/trend-investing/engines/boost/run-2026-10-24.log --backfill-from 2025-01-02.
/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-23.jsonl
ti-warmup.service: Deactivated successfully.
boost-2026-10-23.jsonl
```

If it is late you see this instead, and the unit may still be running:

<!-- expect: warmup-late -->
```text
~ STILL RUNNING after 3 hours
```

A late warm-up is wedged or very slow (a Massive outage, a 429 streak). Read
`sudo journalctl -u ti-warmup -n 30 --no-pager`; if it is still making
requests, run the wait block again; if it is stuck, `sudo systemctl stop
ti-warmup` (your own unit) and run the warm-up block again: it fetches only
what is missing.

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
( set -euo pipefail
sudo grep -E '"(mode|paper_sending_enabled)"' /etc/trend-investing/adapters/boost.json
sudo systemctl start ti-adapter-boost
invocation=$(systemctl show -p InvocationID --value ti-adapter-boost)
ready=0
for _ in $(seq 60); do
  seen=$(sudo journalctl _SYSTEMD_INVOCATION_ID="$invocation" --no-pager -o cat || true)
  if grep -q 'session-closed:2026-10-23' <<<"$seen"; then ready=1; break; fi
  sleep 5
done
if [ "$ready" != 1 ]; then
  echo "NOT READY: this start of the adapter has not delivered the warm-up after 5 minutes; read sudo journalctl -u ti-adapter-boost -n 30"
  exit 1
fi
systemctl is-active ti-adapter-boost
grep -E 'adapter starting|link connected|reconciled|stream queued|stopped|refused' <<<"$seen"
grep 'session-closed:2026-10-23' <<<"$seen"
sudo grep -c '"state":"halted"' /var/lib/trend-investing/engines/boost/run-2026-10-24.log || true
)
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

The readiness check looks only at this start of the adapter (its journal
invocation), so an acknowledgement left by an earlier start, or by an earlier
run, never makes it ready. A start that does not connect, or does not deliver,
prints this instead, and the block ends there (its subshell exits; the lines
after it do not run, so a unit that is still `active` is never mistaken for
ready). Do not go on to step 7:

<!-- expect: adapter-not-ready -->
```text
~ NOT READY: this start of the adapter has not delivered
```

The adapter connects to the Gateway, reads the account, hands the engine the
reconciliation against the book you approved, then delivers the warm-up file
to the engine, one input at a time. That takes a minute or two (about 900
inputs). `halted` must stay `0`: a `1` means the engine halted on its first
reading (the book you typed is not what the adapter read): follow "If the
engine halts on its first reading", below. If the unit is `failed`, `sudo journalctl -u
ti-adapter-boost -n 30` says why; the common causes are the Gateway not
listening on 4002 or the account number in `accounts/boost.env` not being the
one the Gateway is logged in to (`docs/manual/ibkr-adapter.md`, "Every stop,
and the command that settles it").

## 7. Turn the two stopped timers back on

You stopped `ti-data.timer` and `ti-reporter@boost.timer` by hand so that
nothing fired during the warm-up and the first start (before `run.env` exists their
units only wait quietly). It exists now, and the engine and the adapter
are up. Start them, then run each unit once by hand to see what a weekend
firing does: the data job finds Friday's file already written and fetches
nothing new to record; the reporter reads the log and sends nothing.

<!-- rehearsed: timers -->
```sh
( set -euo pipefail
sudo systemctl start ti-data.timer ti-reporter@boost.timer
systemctl is-active ti-data.timer ti-reporter@boost.timer
sudo systemctl start ti-data.service ti-reporter@boost.service
systemctl show -p ActiveState -p Result ti-data.service
systemctl show -p ActiveState -p Result ti-reporter@boost.service
sudo journalctl -u ti-data.service --no-pager -o cat | grep '^/var/lib' | tail -n 1
systemctl list-timers ti-data.timer --no-pager
systemctl list-timers ti-reporter@boost.timer --no-pager
)
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

`ti-data.timer` fires Monday to Friday at 06:05, 07:05 and 08:05 New York
time, for the session before (Massive's free plan serves a session the next
morning), and never on a weekend. `ti-reporter@boost.timer` fires every 5 minutes, all
week. On Saturday and Sunday there is no new session, so the data job's
weekend runs (if any, such as a catch-up after a reboot) find the last
settled session, Friday's, already written, record nothing and succeed; the
reporter's weekend firings find nothing to report and succeed (the
rehearsal cannot move the reporter's clock, so the reporter's weekend firings
are not rehearsed: it is run once, on the container's own clock; only the data
job's weekend firings are rehearsed). A failure of
either pages you through the reporter's channels like any failed unit
(`docs/manual/server.md`, "A failed unit"), so no page is the expected
state.

## 8. The dashboard

`ti-dashboard` listens on loopback only. It starts with the server; check it,
and check that Tailscale publishes it to your tailnet and to nobody else.

<!-- rehearsed: dashboard -->
```sh
( set -euo pipefail
sudo systemctl start ti-dashboard
sleep 3
systemctl is-active ti-dashboard
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088/
curl -s http://127.0.0.1:8088/ | grep -c 'Read-only'
)
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

## If the engine halts on its first reading

You see `halted` `1` in step 6, or the page "reconciliation unverifiable".
The book you approved is not the book the adapter read, and a run's log is
never reused, so you start a fresh run. Nothing is lost but time: the new
warm-up fetches only the sessions the data job has not already recorded
(they are on disk), so it takes seconds rather than 90 minutes (the rehearsal
checks this: fewer than 5 requests). Read the holdings and **total cash** in
IBKR again and use them in the approval block below.

Every command in this section uses three names. Set them once, in the shell
you use for the rest of this section (a block that finds one of them unset
stops at once; set all three again if you open a new shell):

- `halted` is the run that just halted: the first run, `run-2026-10-24`.
- `next` is the new run's name, which must be new: `run-2026-10-24b`.
- `asof` is the new run's first decided session, one minute earlier than the
  halted run's `TI_AS_OF` (which was `2026-10-26T20:00:00Z`).

<!-- rehearsed: recover-vars -->
```sh
halted=run-2026-10-24
next=run-2026-10-24b
asof=2026-10-26T19:59:00Z
```

For a second recovery (the run `next` halted too), paste this block instead
of the one above, then continue with the stop block below (the one that stops the adapter and the engine). Nothing else
changes: no command below has a run name written in it. Then repeat steps 4,
5 and 6 with the blocks of this section (step 4's block has no run name), and
read `next` for the run name in step 7 and in "Monday after the close".

<!-- rehearsed: recover-vars-2 -->
```sh
halted=run-2026-10-24b
next=run-2026-10-24c
asof=2026-10-26T19:58:00Z
```

<!-- rehearsed: recover-stop -->
```sh
( set -euo pipefail
sudo systemctl stop ti-adapter-boost
sudo systemctl stop ti-engine@boost
sudo systemctl stop ti-data.timer ti-reporter@boost.timer
sudo ls /var/lib/trend-investing/engines/boost/
)
```

<!-- expect: recover-stop -->
```text
run-2026-10-24.log
```

The first `operator approve` below only reads the incident's ID from the
stopped log; the second clears it and states the book as IBKR shows it now.
Replace `100000.00` with that figure, and put every holding in `-positions`
as in step 2 if it holds any.

<!-- rehearsed: recover-approve -->
```sh
( set -euo pipefail
: "${halted:?set halted first}" "${next:?set next first}"
sudo bash -eu -s -- "$halted" "$next" <<'EOF2'
halted=$1
next=$2
cash=100000.00
account=$(sed -n 's/^IBKR_ACCOUNT_BOOST=//p' /run/trend-investing/secrets/accounts/boost.env)
dir=/var/lib/trend-investing/engines/boost
set -o pipefail
look=$(runuser -u ti-trader -- operator approve -log "$dir/$halted.log" -account "$account" -by owner -positions "" -cash "$cash" -currency USD -out "$dir/incident-look-$next.json" 2>&1) || { printf '%s\n' "$look"; echo "STOP: operator could not read $halted.log: check the name in halted"; exit 1; }
incident=$(printf '%s\n' "$look" | sed -n 's/.*carries incident \([^ ]*\) in force.*/\1/p')
test -n "$incident" || { echo "STOP: no incident in force in $halted.log; it did not halt. Do not start a fresh run."; exit 1; }
runuser -u ti-trader -- operator approve -log "$dir/$halted.log" -account "$account" -by owner -clear-incident "$incident" -positions "" -cash "$cash" -currency USD -out "$dir/approval-$next.json"
EOF2
)
```

<!-- expect: recover-approve -->
```text
~ the approval clears incident .* \(halted\)
~ the fresh run starts from the book you checked in IBKR.*holdings none, total cash 100000 USD
~ wrote /var/lib/trend-investing/engines/boost/approval-run-2026-10-24b.json: start the fresh run with -approval
```

<!-- rehearsed: recover-run-env -->
```sh
( set -euo pipefail
: "${next:?set next first}" "${asof:?set asof first}"
sudo -u ti-trader tee /var/lib/trend-investing/engines/boost/run.env >/dev/null <<EOF2
TI_RUN=$next
TI_AS_OF=$asof
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/boost/approval-$next.json
EOF2
sudo cat /var/lib/trend-investing/engines/boost/run.env
)
```

<!-- expect: recover-run-env -->
```text
TI_RUN=run-2026-10-24b
TI_AS_OF=2026-10-26T19:59:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/boost/approval-run-2026-10-24b.json
```

`asof` is one minute earlier than in step 3, on purpose: the engine refuses a
fresh run whose start is the same as an earlier log's ("declare a new start
with -as-of"). 19:59 changes nothing that matters: Friday's session still only
warms the indicators, and Monday's, closing at 20:00 UTC, is still the first
one decided.

Then step 4 as written (start the engine). The engine's third line reads
"recorded the owner's approval of a fresh run after ..." with the halted
run's log name; what must hold is `active`:

<!-- expect: engine-again -->
```text
active
~ recorded the owner's approval of a fresh run after
listening on /run/ti-engine-boost/engine.sock
run-2026-10-24b.log
```

Then the warm-up (step 5), with `next` in place of the run name, and its wait,
bounded as before:

<!-- rehearsed: recover-warmup -->
```sh
( set -euo pipefail
: "${next:?set next first}"
sudo systemd-run --unit=ti-warmup --collect -p User=ti-trader -p Group=ti-trader \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/boost.config \
  --engine-log "/var/lib/trend-investing/engines/boost/$next.log" \
  --backfill-from 2025-01-02
)
```

(It prints `Running as unit: ti-warmup.service`, as in step 5.)

<!-- rehearsed: recover-warmup-wait -->
```sh
( set -euo pipefail
: "${next:?set next first}"
sleep 2
deadline=$((SECONDS + 10800))
late=0
while systemctl is-active --quiet ti-warmup; do
  if [ "$SECONDS" -ge "$deadline" ]; then late=1; break; fi
  sleep 30
done
if [ "$late" = 1 ]; then
  echo "STILL RUNNING after 3 hours (it should take about 90 minutes): see the paragraph below this block"
  exit 1
fi
sudo journalctl -u ti-warmup --no-pager -o cat
sudo ls "/var/lib/trend-investing/data/streams/boost/$next/"
)
```

<!-- expect: recover-warmup-wait -->
```text
Started ti-warmup.service - /opt/trend-investing/bin/ti-data --data /var/lib/trend-investing/data --config /etc/trend-investing/engines/boost.config --engine-log /var/lib/trend-investing/engines/boost/run-2026-10-24b.log --backfill-from 2025-01-02.
ti-warmup.service: Deactivated successfully.
boost-2026-10-23.jsonl
```

(Here "STILL RUNNING" means the same as in step 5; the paragraph under that
block applies.) Then the adapter (step 6), with `next` in place of the run
name:

<!-- rehearsed: recover-adapter -->
```sh
( set -euo pipefail
: "${next:?set next first}"
sudo grep -E '"(mode|paper_sending_enabled)"' /etc/trend-investing/adapters/boost.json
sudo systemctl start ti-adapter-boost
invocation=$(systemctl show -p InvocationID --value ti-adapter-boost)
ready=0
for _ in $(seq 60); do
  seen=$(sudo journalctl _SYSTEMD_INVOCATION_ID="$invocation" --no-pager -o cat || true)
  if grep -q 'session-closed:2026-10-23' <<<"$seen"; then ready=1; break; fi
  sleep 5
done
if [ "$ready" != 1 ]; then
  echo "NOT READY: this start of the adapter has not delivered the warm-up after 5 minutes; read sudo journalctl -u ti-adapter-boost -n 30"
  exit 1
fi
systemctl is-active ti-adapter-boost
grep -E 'adapter starting|link connected|reconciled|stream queued|stopped|refused' <<<"$seen"
grep 'session-closed:2026-10-23' <<<"$seen"
sudo grep -c '"state":"halted"' "/var/lib/trend-investing/engines/boost/$next.log" || true
)
```

<!-- expect: recover-adapter -->
```text
  "mode": "dry-run",
  "paper_sending_enabled": false,
active
~ ibkr adapter starting mode=dry-run client_id=11 sending=off
~ ibkr link connected
~ ibkr reconciled client_id=11 positions=0 open_orders=0 executions=0
~ ibkr stream queued run=run-2026-10-24b files=boost-2026-10-23.jsonl inputs=[0-9]+
~ input=marketdata:session-closed:2026-10-23 type=market.session.closed answered with
0
```

`halted` must be `0` again. Finish with step 7's first command, which starts
the two timers again. The halted run's files (`$halted.log`, here
`run-2026-10-24.log`, its stream folder and the `incident-look` file) stay where they are as evidence;
never delete or edit them.

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

After a recovery, read the new run's name for `run-2026-10-24` in the two paths below.

- Before Tuesday 05:30, turn on the Flex reader, or Monday's session is
  withheld at 09:00 for want of its executions proof: create the Activity
  cash query, the Trade Confirmation query and the Flex token
  (`docs/manual/reconciliation.md`, "What you set up once", steps 1, 2 and
  4); fill in `TI_IBKR_FLEX_TOKEN=`, `TI_IBKR_FLEX_QUERY_ID=` and
  `TI_IBKR_FLEX_TRADES_QUERY_ID=` with `sudo ti-secrets edit secrets.env`;
  run `sudo systemctl start ti-flex@boost`; check `sudo journalctl -u
  ti-flex@boost -n 20` says it read and `sudo cat
  /var/lib/trend-investing/flex/boost/executions.json` shows `"state":
  "read"`; then `sudo systemctl enable --now ti-flex@boost.timer`.
- 16:00 the market closes. Tuesday at 05:30 the Flex reader writes its
  executions proof (`docs/manual/reconciliation.md`, "The executions proof";
  it needs the Trade Confirmation query set up), and the data job fires at
  06:05 (and again at 07:05 and 08:05 if the first attempt fails; once the
  file exists they find it and fetch nothing). It writes
  `/var/lib/trend-investing/data/streams/boost/run-2026-10-24/boost-2026-10-26.jsonl`.
- The adapter, still running, delivers that file to the engine behind a
  reading of the account taken that morning, which stands for Monday's close
  with the proof, and the engine decides the session `TI_AS_OF` named: in
  dry-run it journals what it would have done and sends nothing. Without the
  proof by 09:00 the session is withheld (`stale-account-snapshot`) and the
  page says which proof was missing. From about 06:10 Tuesday, `sudo grep -c
  '"type":"strategy.targets.decided"'
  /var/lib/trend-investing/engines/boost/run-2026-10-24.log` prints `1`
  and the dashboard's "targets the strategy decided" card shows the decision.
- If the file is not there by 08:30 Tuesday: `sudo journalctl -u
  ti-data.service -n 20`, then `sudo systemctl start ti-data.service` once
  the cause is gone.

## The Traditional IRA: its engine, warm-up, adapter and same-day probe

*After Boost's day is running. Dry-run only: the IRA's adapter, like
Boost's, sends nothing to IBKR, ever.*

The Traditional IRA's momentum engine needs four things running, in this
order, and the order matters (`docs/manual/market-data.md`, "A job for the
IRAs"): **the engine** (its durable log is the run's identity), then **the
one-time warm-up** of the IRA's data job (it reads that log and writes the
run's stream folder), then **the IRA's adapter**, `ti-adapter@trad-ira`
(client ID 12; it refuses to start until the stream folder exists), then
**the daily firings** (`ti-data@trad-ira.timer`, already enabled by
provisioning). The IRA's Flex reader is turned on before the adapter (I-6):
a morning start of the adapter waits for its executions proof.

Install first, from the checkout (each is an explicit step; never a
timer's doing):

- `sudo deploy/provision.sh services`. What it **installs**: the adapter
  template `ti-adapter@.service` and every other unit file (replaced), the
  Flex reader's staggered times for the IRA (the drop-in),
  `/etc/trend-investing/adapters/trad-ira.json` once, `gateway.env`
  (rewritten every run), and **every Go program present in the checkout's
  `dist/`** (`ti-data`, `ti-reporter`, `ti-dashboard`, `operator`,
  `journal-query`, `ti-gateway-start`, `universe-rank`; never `ti-engine`)
  over `/opt/trend-investing/bin`. So on the live server run it with `dist/`
  moved aside, or with a `dist/` that holds only the builds you mean to
  install: a stale `dist/` downgrades the programs a `ti-release` installed
  (`docs/runbooks/lightsail-deployment.md`). What it **keeps**,
  byte for byte: every engine configuration (`engines/*.config`), every
  settings file already on the server (`engines/*.env`; the shipped one is
  put beside it as `<name>.env.example`), the adapters' configurations and
  the secrets. What it **does to running units**: it enables and starts
  `ti-data.timer`, `ti-data@trad-ira.timer` and `ti-reporter@boost.timer`
  (already running on a Boost server, so no change), restarts
  `ti-dashboard` (on whatever `ti-dashboard` it just installed), and disables (never stops) the engines and adapters, which
  are never enabled at boot. Boost's engine and adapter keep running.
- `sudo deploy/provision.sh adapter` (the adapter's code).
- The engine and reporter binaries built on the Mac
  (`sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine`
  and the same for `ti-reporter`), at :01 to :03 past a 5-minute mark (the
  reporter fires every 5 minutes) or with `ti-reporter@boost.timer` stopped;
  a running Boost engine is not touched by a new binary until it is next
  started.

Because `services` keeps a `trad-ira.env` the server already has, add by
hand the two lines the IRA's adapter and engine need if it lacks them
(compare with `trad-ira.env.example` beside it), then check:

```sh
f=/etc/trend-investing/engines/trad-ira.env
sudo grep -q '^TI_ADAPTER_ACCOUNT_KEY=' "$f" || echo 'TI_ADAPTER_ACCOUNT_KEY=IBKR_ACCOUNT_TRAD_IRA' | sudo tee -a "$f"
sudo grep -q '^TI_MODE=' "$f" || echo 'TI_MODE=dry-run' | sudo tee -a "$f"
sudo grep -E '^TI_(ADAPTER_CLIENT_ID|ADAPTER_ACCOUNT_KEY|MODE)=' "$f"
```

It must print `TI_ADAPTER_CLIENT_ID=12`, `TI_ADAPTER_ACCOUNT_KEY=IBKR_ACCOUNT_TRAD_IRA`
and `TI_MODE=dry-run` (in that file's order).

Every block below except the warm-up and the Flex reader is run by
`deploy/rehearse.sh` as written, as for Boost's steps. The warm-up block is
not: the rehearsal's container has no Massive, sector sources or two years
of recordings, so a stand-in writes the run's first file there
(`deploy/rehearsal/ira-stream.py`), and the warm-up command itself is
rehearsed on its own, on synthetic recordings (`deploy/rehearsal/inside.sh`).
Nor is the Flex reader's: it needs IBKR's Flex Web Service.

### I-0. Where the server stands

<!-- rehearsed: ira-preflight -->
```sh
( set -euo pipefail
sudo ti-secrets status | grep -E '^accounts/trad-ira\.env'
sudo grep -c '^IBKR_ACCOUNT_TRAD_IRA=DU' /run/trend-investing/secrets/accounts/trad-ira.env
test -f /etc/systemd/system/ti-adapter@.service || { echo "STOP: the IRA adapter's unit is not installed: run sudo deploy/provision.sh services"; exit 1; }
echo ira-adapter-unit-installed
grep -E '"(client_id|account_snapshot_version|resolve_contracts)"' /etc/trend-investing/adapters/trad-ira.json
grep -E '^TI_ADAPTER_(CLIENT_ID|ACCOUNT_KEY)=' /etc/trend-investing/engines/trad-ira.env
sudo test ! -e /var/lib/trend-investing/engines/trad-ira/run.env || { echo "STOP: the IRA's run.env already exists: this is not its first start"; exit 1; }
echo no-ira-run-env
)
```

<!-- expect: ira-preflight -->
```text
accounts/trad-ira.env    encrypted, unlocked
1
ira-adapter-unit-installed
  "client_id": 12,
  "account_snapshot_version": 6,
  "resolve_contracts": true
TI_ADAPTER_CLIENT_ID=12
TI_ADAPTER_ACCOUNT_KEY=IBKR_ACCOUNT_TRAD_IRA
no-ira-run-env
```

If the `1` is a `0`, put the IRA's paper account number in its own file:
`sudo ti-secrets edit accounts/trad-ira.env` (`IBKR_ACCOUNT_TRAD_IRA=DU...`,
no quotes, no spaces). It goes there and nowhere else: never in the
repository, a chat or a ticket (`docs/live-trading/lessons-2026-10.md` L12).

### I-1. Name the account and your birth month in the IRA's configuration

Enable **fractional trading** on the IRA account in IBKR and keep **dividend
reinvestment (DRIP) off**. The owner selected D7-F (ADR 0026):
`execution.share_quantity` must be `"fractional"` and
`execution.fractional_increment` must be `"0.0001"`. The shipped template
sets both; the block below also updates an existing installed template
and sets the adapter's `account_snapshot_version` to 6. Provisioning never
overwrites an installed adapter configuration; if the preflight above
shows 4, complete this update before starting the adapter.
Holdings and fills may have up to four decimal places. A fifth decimal
place is refused with its reason, never rounded by the adapter.

The shipped `trad-ira.config` names the placeholder account `trad-ira` and the
placeholder birth `YYYY-MM` (the glide path's age; the engine refuses to
start on the placeholder). Replace `1975-06` below with your year and month
of birth before you paste the block. As in step 1, the account number is
read from the unlocked file and never printed; it is then in
`/etc/trend-investing/engines/trad-ira.config` on the server only.

<!-- rehearsed: ira-account -->
```sh
sudo bash -eu <<'EOF2'
config=/etc/trend-investing/engines/trad-ira.config
birth=1975-06
account=$(sed -n 's/^IBKR_ACCOUNT_TRAD_IRA=//p' /run/trend-investing/secrets/accounts/trad-ira.env)
if ! printf '%s\n' "$account" | grep -qE '^DU[A-Z0-9]{5,9}$' || ! printf '%s\n' "$account" | grep -q '[0-9]'; then
  echo "STOP: accounts/trad-ira.env has no IBKR_ACCOUNT_TRAD_IRA=DU... line."
  exit 1
fi
printf '%s\n' "$birth" | grep -qE '^(19|20)[0-9]{2}-(0[1-9]|1[0-2])$' || { echo "STOP: birth is not YYYY-MM."; exit 1; }
current=$(sed -n 's/^ *"account_id": *"\([^"]*\)".*/\1/p' "$config")
if [ "$current" = trad-ira ]; then
  sed -i "s/\"account_id\": \"trad-ira\"/\"account_id\": \"$account\"/" "$config"
elif [ "$current" != "$account" ]; then
  echo "STOP: trad-ira.config already names a different account than accounts/trad-ira.env."
  exit 1
fi
sed -i "s/\"birth\": \"YYYY-MM\"/\"birth\": \"$birth\"/" "$config"
python3 - "$config" <<'PY'
import json
import sys
from pathlib import Path
path = Path(sys.argv[1])
config = json.loads(path.read_text())
config["execution"]["share_quantity"] = "fractional"
config["execution"]["fractional_increment"] = "0.0001"
path.write_text(json.dumps(config, indent=2) + "\n")
adapter_path = Path("/etc/trend-investing/adapters/trad-ira.json")
adapter = json.loads(adapter_path.read_text())
adapter["account_snapshot_version"] = 6
adapter_path.write_text(json.dumps(adapter, indent=2) + "\n")
PY
[ "$(sed -n 's/^ *"account_id": *"\([^"]*\)".*/\1/p' "$config")" = "$account" ] || { echo "STOP: the account was not named."; exit 1; }
echo account-named
grep -c '"birth": "YYYY-MM"' "$config" || true
grep -c '"mode": "dry-run"' "$config"
EOF2
```

<!-- expect: ira-account -->
```text
account-named
0
1
```

### I-2. Approve the IRA's first run with its starting book

The existing starting-book approval accepts whole shares only (ADR 0026,
D7-F amendment). Begin with cash or whole-share holdings. If the IRA already
holds a fraction, its approval needs the separate starting-book migration;
do not round that holding to make the approval pass.

In IBKR, read the IRA paper account's holdings and its total cash, and use
them here exactly as in step 2 (`-positions ""` only when it holds nothing).
`-strategy momentum` records the IRA's approval.

<!-- rehearsed: ira-approve -->
```sh
( set -euo pipefail
cash=100000.00
account=$(sudo sed -n 's/^IBKR_ACCOUNT_TRAD_IRA=//p' /run/trend-investing/secrets/accounts/trad-ira.env)
sudo -u ti-trader operator approve -first-run -strategy momentum -account "$account" -by owner -positions "" -cash "$cash" -currency USD -out /var/lib/trend-investing/engines/trad-ira/first-run-approval.json
)
```

<!-- expect: ira-approve -->
```text
wrote /var/lib/trend-investing/engines/trad-ira/first-run-approval.json: start the first run with -approval /var/lib/trend-investing/engines/trad-ira/first-run-approval.json (docs/runbooks/first-paper-order.md)
```

### I-3. The run file

`TI_AS_OF` is the close of the first month-end the engine decides; every
session before it only warms the histories. `2026-11-30T21:00:00Z` is
Monday 30 November's 16:00 close in New York (standard time, so 21:00 UTC):
the last session of November, decided from the reading the adapter takes on
the morning of Tuesday 1 December. To decide October's month-end instead,
use `2026-10-30T20:00:00Z` (Friday 30 October, still daylight time), decided
the morning of Monday 2 November. Name the run after the day you start it.

<!-- rehearsed: ira-run-env -->
```sh
( set -euo pipefail
sudo -u ti-trader tee /var/lib/trend-investing/engines/trad-ira/run.env >/dev/null <<'EOF2'
TI_RUN=run-ira-2026-10-29
TI_AS_OF=2026-11-30T21:00:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/trad-ira/first-run-approval.json
EOF2
sudo cat /var/lib/trend-investing/engines/trad-ira/run.env
)
```

<!-- expect: ira-run-env -->
```text
TI_RUN=run-ira-2026-10-29
TI_AS_OF=2026-11-30T21:00:00Z
TI_RESTART_ARGS=-approval /var/lib/trend-investing/engines/trad-ira/first-run-approval.json
```

### I-4. Start the IRA's engine

<!-- rehearsed: ira-engine -->
```sh
( set -euo pipefail
sudo systemctl start ti-engine@trad-ira
sleep 3
systemctl is-active ti-engine@trad-ira
sudo journalctl -u ti-engine@trad-ira -n 3 --no-pager -o cat
sudo ls /var/lib/trend-investing/engines/trad-ira/
)
```

<!-- expect: ira-engine -->
```text
active
~ Started ti-engine@trad-ira\.service
listening on /run/ti-engine-trad-ira/engine.sock
run-ira-2026-10-29.log
```

### I-5. The IRA's one-time warm-up (not rehearsed here; see above)

After the engine and before the adapter. It records what Boost's warm-up has
not, ranks the universe with the Go ranker and writes the run's first file;
`docs/manual/market-data.md`, "A job for the IRAs", "The one-time warm-up",
gives what it does and how long it takes. Then wait for it as for Boost's
(`sudo journalctl -fu ti-warmup-ira` until `Deactivated successfully`):

```sh
sudo systemd-run --unit=ti-warmup-ira --collect -p User=ti-trader -p Group=ti-trader -p RuntimeMaxSec=4h \
  -p LoadCredential=secrets:/run/trend-investing/secrets/secrets.env \
  /opt/trend-investing/bin/ti-data --engine momentum --instance trad-ira \
  --ranker /opt/trend-investing/bin/universe-rank --data /var/lib/trend-investing/data \
  --config /etc/trend-investing/engines/trad-ira.config \
  --engine-log /var/lib/trend-investing/engines/trad-ira/run-ira-2026-10-29.log \
  --backfill-from 2025-01-02
```

The last line of its journal is the file it wrote:
`/var/lib/trend-investing/data/streams/trad-ira/run-ira-2026-10-29/trad-ira-<last session>.jsonl`.

### I-6. Turn on the IRA's Flex reader (not rehearsed; see above)

Before the adapter: a morning reading of a session stands for its close
only with the IRA's own executions proof (ADR 0019, amended 2026-10-27), and
until 09:00 New York a morning start of the adapter waits for it rather than
deliver, so I-7 would not be ready in its five minutes without it. Its own
Flex reader writes it, firing at 05:45, 06:45 and 07:45, after Boost's (the
drop-in provisioning installs). Run it once, check it, then enable its timer,
with the same token and queries as Boost's ("Monday after the close"):

```sh
sudo systemctl start ti-flex@trad-ira
sudo journalctl -u ti-flex@trad-ira -n 20 --no-pager
sudo cat /var/lib/trend-investing/flex/trad-ira/executions.json
sudo systemctl enable --now ti-flex@trad-ira.timer
systemctl list-timers ti-flex@trad-ira.timer --no-pager
```

The journal says it read, `executions.json` shows `"state": "read"`, and the
timer's next firing is at 05:45, 06:45 or 07:45 New York.

### I-7. Start the IRA's adapter, in dry-run

`ti-adapter@trad-ira` connects with client ID 12 (Boost's is 11, on the same
Gateway), reads the account's positions and balances, its settled cash
among them, compares them with the book you approved, and delivers the
warm-up. The block waits for the warm-up's last session to reach the engine.

<!-- rehearsed: ira-adapter -->
```sh
( set -euo pipefail
sudo grep -E '"(client_id|mode|paper_sending_enabled|account_snapshot_version|resolve_contracts)"' /etc/trend-investing/adapters/trad-ira.json
run=$(sudo sed -n 's/^TI_RUN=//p' /var/lib/trend-investing/engines/trad-ira/run.env)
last=$(sudo ls /var/lib/trend-investing/data/streams/trad-ira/"$run"/ | grep '^trad-ira-.*\.jsonl$' | tail -n 1)
test -n "$last" || { echo "STOP: no stream file yet: run the warm-up (I-5) first"; exit 1; }
session=${last#trad-ira-}
session=${session%.jsonl}
sudo systemctl start ti-adapter@trad-ira
invocation=$(systemctl show -p InvocationID --value ti-adapter@trad-ira)
ready=0
for _ in $(seq 60); do
  seen=$(sudo journalctl _SYSTEMD_INVOCATION_ID="$invocation" --no-pager -o cat || true)
  if grep -q "session-closed:$session" <<<"$seen"; then ready=1; break; fi
  sleep 5
done
if [ "$ready" != 1 ]; then
  echo "NOT READY: this start of the IRA's adapter has not delivered the warm-up after 5 minutes; read sudo journalctl -u ti-adapter@trad-ira -n 30"
  exit 1
fi
systemctl is-active ti-adapter@trad-ira
grep -E 'adapter starting|link connected|reconciled|contracts resolved|stream queued|stopped|refused' <<<"$seen"
sudo grep -c '"state":"halted"' /var/lib/trend-investing/engines/trad-ira/"$run".log || true
)
```

<!-- expect: ira-adapter -->
```text
  "client_id": 12,
  "mode": "dry-run",
  "paper_sending_enabled": false,
  "account_snapshot_version": 6,
  "resolve_contracts": true
active
~ ibkr adapter starting mode=dry-run client_id=12 sending=off
~ ibkr link connected
~ ibkr reconciled client_id=12 positions=[0-9]+ open_orders=0 executions=0
~ ibkr contracts resolved=[0-9]+ of [0-9]+
~ ibkr stream queued run=run-ira-2026-10-29 files=trad-ira-[0-9-]+\.jsonl inputs=[0-9]+
0
```

`halted` must stay `0`; a `1` means the book you approved is not the one the
adapter read: follow "If the engine halts on its first reading" with the
IRA's names (`ti-engine@trad-ira`, `ti-adapter@trad-ira`, the folder
`engines/trad-ira`, the warm-up of I-5). A `contract for <name> not
resolved` warning in its journal is not a stop: that name gets no mark, so
the engine refuses any order for it (`missing-mark`) rather than size it on
a guessed contract (`docs/manual/ibkr-adapter.md`, "Contracts and marks for
an IRA").

### I-8. The same-day probe

Run it right after I-7, the same day (`docs/live-trading/lessons-2026-10.md`
L10), and after every later install of the adapter. It reads this start's
journal and the engine's log, talks to nothing, and prints one line per
check; it prints no account number. There are two forms, and which one to
use depends only on whether this start had a file to deliver:

- **With `--expect-batch`** (the block below): on the first day (after I-7),
  after a data-job firing has written a new file, and on a month-end
  morning. The start must have delivered a batch, and its outcome line must
  say `outcome=delivered`.
- **Without `--expect-batch`**: after a plain restart of the adapter, or a
  binary install, when no new stream file was waiting. The start must have
  said `ibkr stream no new file since <file>; caught up`, and the probe then
  passes with `probe: ok no new batch since start; caught up (...)`.

The probe reads `execution.share_quantity` from the engine configuration:
`fractional` requires the fractional cash snapshot version (currently 6);
`minimum-lot` requires version 4. The example output below is for fractional
shares; minimum-lot output reports version 4 and has no holdings line.

<!-- rehearsed: ira-probe -->
```sh
( set -euo pipefail
run=$(sudo sed -n 's/^TI_RUN=//p' /var/lib/trend-investing/engines/trad-ira/run.env)
snapshot_version=$(sudo python3 - /etc/trend-investing/engines/trad-ira.config <<'PYTHON'
import json
import sys
with open(sys.argv[1]) as source:
    rule = json.load(source)["execution"]["share_quantity"]
# ADR 0026 D7-F uses fractional cash snapshots; D7-L keeps whole-share cash snapshots.
print({"fractional": 6, "minimum-lot": 4}[rule])
PYTHON
)
invocation=$(systemctl show -p InvocationID --value ti-adapter@trad-ira)
sudo journalctl _SYSTEMD_INVOCATION_ID="$invocation" --no-pager -o cat >/tmp/ti-adapter-trad-ira.journal
cd /opt/trend-investing/adapter
sudo -u ti-trader /opt/trend-investing/adapter-venv/bin/python3 -m adapter.ibkr probe --journal /tmp/ti-adapter-trad-ira.journal \
  --engine-log /var/lib/trend-investing/engines/trad-ira/"$run".log --client-id 12 --snapshot-version "$snapshot_version" --expect-batch
)
```

<!-- expect: ira-probe -->
```text
~ probe: ok the adapter started: mode=dry-run client_id=12 sending=off ibapi=
~ probe: Gateway server version [0-9]+ \(contract details read from IBKR's (protobuf|text) messages\)
~ probe: ok connected to IB Gateway as client_id=12
~ probe: ok reconciled client_id=12: positions=[0-9]+ open_orders=0 executions=0
~ probe: ok queued run=run-ira-2026-10-29 files=trad-ira-[0-9-]+\.jsonl inputs=[0-9]+ \(delivered: [0-9]+ answered, [0-9]+ already committed\)
~ probe: ok account snapshot version 6 as of .*settled_cash .*total_cash
~ probe: ok holdings: .*
~ probe: ok reconciliation journalled
~ probe: ok [0-9]+ mark\(s\) at version 3
probe: ok no order released to be sent and none refused for a missing mark
~ probe: (no decision yet|latest decision)
probe: passed
```

Paste its output into the ticket's testing evidence as it stands (it carries
no account number). A `probe: FAIL` line says what is missing; the last line
then reads `probe: FAILED, <n> check(s)` and the command exits 1. The
manual's "The same-day probe" says what each check means and what to do.

### I-9. The daily firings

`ti-data@trad-ira.timer` is enabled by provisioning; once `run.env` exists its
06:15, 07:15 and 08:15 firings write each session's file and the adapter,
still running, delivers it behind that morning's reading, with the proof
the Flex reader (I-6) writes each morning before it.

### The first IRA month-end, in dry-run (the slice's acceptance)

For `TI_AS_OF=2026-11-30T21:00:00Z`:

| When (New York) | What happens | What to look at |
|---|---|---|
| Mon 30 Nov 16:00 | The month's last session closes | nothing |
| Tue 1 Dec 05:45 | `ti-flex@trad-ira` writes the executions proof | `sudo cat /var/lib/trend-investing/flex/trad-ira/executions.json` shows `"state": "read"` |
| Tue 1 Dec 06:15 | `ti-data@trad-ira` writes `trad-ira-2026-11-30.jsonl`: SPY, IEF, SHY and the holdings' bars, the month-end universe and the 500 ranked stocks' histories | `sudo journalctl -u ti-data@trad-ira -n 5 --no-pager`, last line the file |
| Tue 1 Dec, minutes after | The adapter reads the account once, resolves the contract of every name it will mark (each ranked stock, SPY, IEF, SHY and the holdings: about 500 names, at most 40 requests a second, a slow answer asked once more), asks IBKR's last price for each under the same pace, and delivers the file with a mark and contract for each, before the session's close | `sudo journalctl -u ti-adapter@trad-ira -n 40 --no-pager`: `contracts resolved=N of M`, then `stream queued`; each `contract for X not resolved` names a stock that gets no mark |
| Then | The engine decides the month, or withholds it with a stated reason; a dry-run records a synthetic cancel for each order it releases, so the buy phase is planned at the same close | the probe (I-8) again, with `--expect-batch`, on that morning's start or the adapter's journal since its start: its `latest decision` line |

The slice is accepted when that first month-end is decided, or withheld
with a stated reason (`stale-account-snapshot` without the proof by 09:00,
`missing-universe`, `histories-missing`, and so on;
`docs/manual/momentum.md`). The IRA's reporter and dashboard view are not
part of this slice, so no page comes for a withheld IRA month yet: run the
probe on the morning of 1 December and record its output.

## The Roth IRA and the measurement week

Use the same Gateway and login as Boost and Traditional IRA (ADR 0026;
owner decision 2026-10-28). The Roth adapter is client **13**, Traditional
IRA is **12**, Boost is **11**. All remain dry-run. From the staged repository
root on the server, install the new account's examples only when absent;
never replace another account's configuration. Record the existing files'
checksums before and after, without printing their contents:

```bash
before=$(sudo sha256sum /etc/trend-investing/engines/*.config /etc/trend-investing/engines/*.env)
sudo test -e /etc/trend-investing/engines/roth-ira.config || sudo install -m 0644 deploy/etc/engines/roth-ira.config.example /etc/trend-investing/engines/roth-ira.config
sudo test -e /etc/trend-investing/engines/roth-ira.env || sudo install -m 0644 deploy/etc/engines/roth-ira.env /etc/trend-investing/engines/roth-ira.env
sudo test -e /etc/trend-investing/adapters/roth-ira.json || sudo install -m 0644 deploy/etc/adapters/roth-ira.json.example /etc/trend-investing/adapters/roth-ira.json
for account in boost trad-ira roth-ira; do
  sudo install -d -m 0755 "/etc/systemd/system/ti-flex@$account.timer.d"
  sudo install -m 0644 "deploy/systemd/ti-flex@$account.timer.d/stagger.conf" "/etc/systemd/system/ti-flex@$account.timer.d/"
done
sudo install -d -m 0755 /etc/systemd/system/ti-data@roth-ira.timer.d
sudo install -m 0644 deploy/systemd/ti-data@roth-ira.timer.d/stagger.conf /etc/systemd/system/ti-data@roth-ira.timer.d/
sudo install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/engines/roth-ira /var/lib/trend-investing/adapters/roth-ira /var/lib/trend-investing/flex/roth-ira
sudo install -d -o ti-operator -g ti-trader -m 0750 /var/lib/trend-investing/engines/roth-ira/operator
printf '%s\n' "$before" | sudo sha256sum --check --status
sudo systemctl daemon-reload
sudo systemctl enable --now ti-data@roth-ira.timer ti-reporter@roth-ira.timer
```

Before starting any Roth engine or adapter, use `sudo ti-secrets edit
accounts/roth-ira.env` to set `IBKR_ACCOUNT_ROTH_IRA` on the server. Follow
I-1 through I-8 with `roth-ira` in every path and instance, `ROTH_IRA` in
the account key and `13` in the probe. Substitute the owner's birth month
and review the glide anchors and retirement horizon in **Roth's own**
configuration. Its account must match its own secret and differ from the
other two accounts; never copy their account values. Use a new Roth run
name and its own starting-book approval. Warm it up before starting its
adapter. Do not enable engines or adapters; each start remains the owner's
step under ADR 0027. Enable and start `ti-flex@roth-ira.timer` once the
account and shared Flex query credentials are ready (I-6). Restart any
already running Flex timers after installing their calendar drop-ins so
the new schedule is in force.

Roth's Flex starts at 06:00/07:00/08:00 New York; its data job starts at
06:30/07:30/08:30, after Flex's scheduled starts and before the 09:00 proof
cutoff. Slow Flex reads can run past their windows; the adapter still
requires the proof. Check `systemctl list-timers 'ti-flex@*' 'ti-data*'`.
The shared engine/adapter/Flex/data templates already page on failure and
the reporter's `-units` and `-resolve-units` lists cover Roth. Verify its
reporter runs and the heartbeat remains Boost's alone.

**Same day, before leaving the installation:** run I-8's probe for Roth,
expect `client_id=13` and sending off. Read Roth's engine configuration
instead of Traditional IRA's: `fractional` requires the fractional cash
snapshot version (currently 6), and `minimum-lot` requires version 4. Retain the
account-masked output. Start Roth's Flex reader by hand and confirm its
executions proof; start its data job and reporter and read their unit
results. Record a withheld decision's stated reason if one occurs. Do not
wait for the next morning to find that a unit cannot start (Boost lesson
L10).

Then follow [Measuring the server](../manual/server.md#measuring-the-server)
to install the memory recorder by hand, select seven UTC dates and enable
its timer. Check the first JSONL record that same day with all three
engines/adapters and the Gateway running. After the week, retain the JSONL,
the report's peak/median figures, counts/gaps and schedule, and the owner's
capacity decision. A missing day fails the report and requires a new
recorded week; keep the failed one. No server upgrade, paper orders or
restart drill is performed by these steps.
