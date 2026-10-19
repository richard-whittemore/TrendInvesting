# The server: what runs where

*For the owner. The Lightsail server: which programs run on it, under which
users, when each starts, where its configuration and secrets live, what is
backed up, what you start by hand, and what can go wrong. The step-by-step
setup is `docs/runbooks/lightsail-deployment.md`; this chapter is what you
need to know to look after it.*

## What it is and why

One virtual machine at AWS Lightsail: Ubuntu 24.04 LTS, x86_64 (IBKR ships
IB Gateway for Linux x64 only), US East, the 2 GB plan (plan sections 3 and
8). Everything that trades runs on it: one IB Gateway shared by all three
accounts, the market-data step, one engine (and, later, one adapter) per
account, the reporter and the dashboard. It is reachable only over
Tailscale, your private network: the firewall refuses everything else.
Everything that sets it up is in `deploy/`, run by `deploy/provision.sh`.

One machine keeps the cost at about $170 a year (plan section 8). The price
is that a reporter on the same machine cannot notice the machine itself
dying: that needs an outside watcher, which is not built (below).

## Users

| User | Runs | Can see |
| --- | --- | --- |
| `ti-gateway` | IB Gateway and IBC | the IBKR login, handed to it alone |
| `ti-trader` | the data step, the engines, the Boost adapter, the reporter, the dashboard, and the `operator` tool when you run it | the market data, the engines' logs, the secrets file, the Boost account's file; never the IBKR login |

Both are system users with no login shell. You log in as `ubuntu` and use
`sudo`.

## The units and timers

All times are New York time (each unit sets `TZ=America/New_York`).

| Unit | What it does | When it runs | Today |
| --- | --- | --- | --- |
| `ti-gateway.service` | IB Gateway under IBC, logged in to IBKR | Starts at boot (enabled). Restarts itself 30 seconds after IBC exits (for example when the weekly phone approval times out), except when the login file is empty or malformed (a wrong password is not caught: see the table below). IBC restarts the Gateway nightly at 11:45 PM and logs in again from cold every Sunday at 09:00 (`/etc/trend-investing/gateway.env`) | Installed; not yet run against a real IBKR login |
| `ti-data.timer` and `ti-data.service` | The daily data job: records the day's market data from Massive and writes the Boost engine's input ([market data](market-data.md), "The daily data job") | Monday to Friday at 17:05, retried at 18:05 and 20:05. A firing missed while the server was off runs when it starts again. A run is stopped after an hour | Built and installed by `provision.sh services` when `dist/ti-data` is beside `deploy/`. It fails, with a message, until the Boost engine has been started once |
| `ti-engine@boost.service` (and `@trad-ira`, `@roth-ira`) | One engine per account ([engine](engine.md)) | **Never by itself.** Not started at boot, never restarted by systemd (`Restart=no`): every start is yours ([restart](restart.md)) | Boost in dry-run. The IRA instances are skipped: they have no configuration file |
| `ti-reporter@boost.timer` and `ti-reporter@boost.service` | The reporter: alerts and the daily summary ([alerts and dashboard](alerts-and-dashboard.md)). One instance per account; only Boost's timer is enabled, since the IRA engines have no configuration yet | **Every 5 minutes, around the clock, seven days a week**, so an alert reaches you within 5 minutes, overnight and at weekends too. A firing missed while the server was down runs at boot. A run is stopped after 4 minutes | Built and installed by `provision.sh services`. It reads the log named by the Boost engine's `run.env` at every firing, and fails, with a message, until the engine has been started once and the `secrets.env` entries below are filled in |
| `ti-dashboard.service` | The read-only dashboard, on loopback port 8088 | Starts at boot (enabled) | Built and installed by `provision.sh services`; it follows the Boost engine's current log. It fails, with a message, until `run.env` exists |
| `tailscaled` | Tailscale | At boot | Installed by `provision.sh base` |

`ti-adapter-boost.service` carries the IBKR adapter ([IBKR adapter](ibkr-adapter.md)): never started at boot or restarted by systemd, every start is yours, after the engine ([paper trading](paper-trading.md), "Starting the adapter"). It is skipped until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and the adapter's code, and `provision.sh services` has created its configuration (`/etc/trend-investing/adapters/boost.json`).

Updates: `unattended-upgrades` applies security updates only and never
reboots by itself, because a reboot logs the Gateway out.

## Where the configuration lives

| File | What it holds | Who writes it |
| --- | --- | --- |
| `/etc/trend-investing/engines/<account>.env` | the account's label (`TI_ACCOUNT`), its adapter client ID (Boost 11, Traditional IRA 12, Roth IRA 13) and, for Boost, the engine's start mode (`TI_MODE`) | copied from `deploy/etc/engines/` by `provision.sh services` |
| `/etc/trend-investing/engines/<account>.config` | the account's configuration: strategy, account, mode, rule figures, hard limits. Hashed into every run (ADR 0016). Only `boost.config` exists | the same |
| `/var/lib/trend-investing/engines/<account>/run.env` | one engine start: `TI_RUN` (the run's name, never reused), `TI_AS_OF` (its first decided session) and `TI_RESTART_ARGS` (empty, `-approval <file>` or `-resume <log>`) | you, before every start (`docs/runbooks/durable-log.md`, "Restarting an engine"; example in `deploy/etc/engines/run.env.example`) |
| `/etc/trend-investing/gateway.env` | the Gateway's restart times | `provision.sh services` |

**Caution:** `provision.sh services` copies the repository's `*.env` and
`*.config` files over `/etc/trend-investing/engines/` every time it runs.
Once you have edited them for paper (the paper account, mode `paper`, the
warm-up start), running it again puts the dry-run files back. The engine
then refuses to start in paper, which is safe, but your edits are gone.

## Where the secrets live

| File | What it holds | Who reads it |
| --- | --- | --- |
| `/etc/trend-investing/ibkr-login.env` (root only, mode 0600) | the IBKR username and password, the trading mode (`paper`) and the read-only switch (`TWS_READONLY_API=yes`). Read-only means the Gateway refuses every order from the adapter, paper included; you set it to `no` yourself, as a step of the first-paper-order runbook | only `ti-gateway.service`, through systemd's credential hand-over. IBC's settings file is written from it into memory on each start, never to disk beside the code |
| `/etc/trend-investing/secrets.env` (root only, mode 0600) | `IBKR_ACCOUNT_TRAD_IRA`, `IBKR_ACCOUNT_ROTH_IRA`, `MASSIVE_API_KEY`, `TI_NTFY_TOPIC`, `TI_NTFY_SERVER`, `TI_PAGERDUTY_ROUTING_KEY` | handed as a private credential file (`LoadCredential`) to the data job, the engines and the reporter; the dashboard is given none |
| `/etc/trend-investing/accounts/boost.env` (root only, mode 0600, in a root-only folder, mode 0700) | `IBKR_ACCOUNT_BOOST`: the Boost account's number, alone | handed as a private copy to the Boost adapter only (`ti-adapter-boost.service`) |

What each program actually reads today:

- `ti-data` reads **only** `MASSIVE_API_KEY` from that file.
- **No program reads** `IBKR_ACCOUNT_TRAD_IRA` or `IBKR_ACCOUNT_ROTH_IRA` yet.
  The Boost adapter reads `IBKR_ACCOUNT_BOOST` from its own file and from no
  other. The account an engine trades is the `account_id` in its
  configuration file.
- The reporter reads `TI_NTFY_TOPIC` (and optionally `TI_NTFY_SERVER`, empty
  for the public ntfy.sh relay) and `TI_PAGERDUTY_ROUTING_KEY` from that file
  (`-secrets-file`), through its unit's credential. It refuses to run, and
  its unit fails, while the topic or the routing key is empty: fill them in
  with `sudo -e /etc/trend-investing/secrets.env`; the next firing uses
  them. There is no line for the outside heartbeat, which is not built.

Never paste a secret into an issue, a pull request, a chat with an agent or
a command line. Edit these files with `sudo -e`.

## Where the records live

| Folder | What is in it |
| --- | --- |
| `/var/lib/trend-investing/data/` | the data step's recordings (never overwritten), `streams/boost/<TI_RUN>/` (the Boost engine's daily input files) and, when you need it, `quarantine/` |
| `/var/lib/trend-investing/engines/<account>/` | each run's durable log (`<TI_RUN>.log`) and end-of-run journal (`<TI_RUN>.journal`), `run.env`, your approval files, `quarantine/` for a log set aside, and the operator inbox once you create it ([operator](operator.md), "The operator inbox": the provisioner does not create it yet) |
| `/var/lib/trend-investing/reports/` | the reporter's own state file (the only folder its unit may write) |
| `/var/lib/ti-gateway/`, `/var/log/ti-gateway/` | the Gateway's own files and IBC's log |
| `/opt/trend-investing/bin/` | the installed programs: `ti-engine`, `ti-reporter`, `ti-dashboard`, `operator` and `journal-query` (from `deploy/build-engine.sh`) and `ti-data` (from `deploy/build-data.sh`). `operator` and `journal-query` are linked into `/usr/local/bin` |

The logs are the evidence: never edit or delete one (ADR 0018). An IRA's
configuration holds your birth month, and the first record of every IRA log
repeats it, so IRA logs stay on the server and are never committed.

## Backups

Plan section 7 says the logs and journals are "copied off it nightly" and
the server is snapshotted weekly. **Not built yet**: nothing in `deploy/`
copies anything off the server or takes a snapshot. The only backup today is
a Lightsail snapshot, if you turned snapshots on when you created the
instance (setup step 1). Where another chapter says "restore it from the
backup" (for a recording or a data file), that is what it means today. The
gap is tracked in the issue "Review L10: server secrets per unit, IBC
trusted IPs, UMask, backups".

## The clock

The program compares the server's clock with IBKR's. If the server's clock
runs behind IBKR's, the account reading looks as if it comes from the future,
the engine treats it as stale (ADR 0019 allows no tolerance for that), and it
blocks every sell until the clock is right. A drifting clock therefore shows
up first as a withheld sale, so the server is set up to keep its clock right
from the start.

`sudo deploy/provision.sh clock` (also part of `all`) turns on the system's
time syncing (`systemd-timesyncd`) and then runs the check, and it refuses to
finish if the check fails. On a server of its own the clock must be reported
synchronised by `timedatectl`. Inside the Mac mini's container the Mac keeps
time and the container usually shows no time service of its own, so there the
check compares the clock with the time an outside website reports, and fails
if they differ by more than 2 seconds (it measures to the second; the limit
can be changed with `TI_CLOCK_TOLERANCE_SECONDS`). If the container cannot
reach any reference, that also fails, because a clock that could not be
checked is not a checked clock. On the Mac mini, a failure means the Mac's own
"Set time and date automatically" is off or wrong.

Run the check yourself at any time, and after any sale is withheld with the
reason `stale-broker-snapshot`:

```
/usr/local/libexec/trend-investing/clock-check.sh
```

It prints `clock: ok ...` or says what is wrong and exits with an error. The
reporter does not report the clock; this check is the way to see it.

## What you start by hand

- **Once:** create the instance, join Tailscale (`sudo tailscale up --ssh`),
  close the public door (`provision.sh firewall`), enter the IBKR login and
  the secrets, start the Gateway the first time and approve the phone
  prompt (`docs/runbooks/lightsail-deployment.md`).
- **Every engine start**, after a crash or not: write `run.env`, and for a
  fresh run an approval, then `sudo systemctl start ti-engine@<account>`
  ([restart](restart.md)).
- **Once per Boost run:** the data job's warm-up, after the engine has
  started once ([market data](market-data.md), "The one-time warm-up").
- **Every Sunday:** approve the Gateway's phone prompt.
- **Installing a build:** copy it from the Mac and `sudo install` it
  (`docs/runbooks/lightsail-deployment.md`). A new data job build only with
  a fresh engine run, or after checking it reproduces the current run's
  files.
- **The dashboard on the tailnet:** `sudo tailscale serve --bg 8088`, the
  port its unit listens on.

## After a reboot

The Gateway starts by itself and sends a phone prompt: approve it. The data
job's timer catches up any firing it missed. **Every engine stays stopped.**
A reboot is not a crash, so each engine's next start is a fresh run you
approve ([restart](restart.md)). Schedule reboots for a Sunday, after the
cold re-login, so one phone prompt serves both.

## Reading the logs and the kill switch on the server

`journal-query` and `operator` are installed on the server, in
`/usr/local/bin`: run them as the engine's user, `sudo -u ti-trader
journal-query ...` and `sudo -u ti-trader operator ...`. `journal-query` only
reads. The engine's operator inbox is not created by the provisioner yet
([operator](operator.md), "The operator inbox"): until you create it and pass
it to the engine, `operator kill-switch` has nowhere to be read from. **Until
then there is no recorded kill switch on the server:** to stop the program
trading, stop the engine (`sudo systemctl stop ti-engine@<account>`). No
adapter runs on the server yet, so stopping the engine is the whole step
today. Once the adapter exists, it is also stopped, and an order that must not
go is settled `--never-sent` with the relay's `resolve` command, run on the
adapter's machine (the first-paper-order runbook shows it). Cancel anything
already at IBKR in IBKR's own app.
  Do not treat stopping the Gateway (`sudo systemctl stop ti-gateway`) as a
  kill switch: it only delays. An order the engine has released waits in the
  adapter and goes out once the Gateway is back the same day, and an order already at IBKR keeps
  working until you cancel it in IBKR's app.

## What can go wrong, and what happens then

| What happens | What happens on the server | What you do |
| --- | --- | --- |
| The server reboots | Gateway back after a phone approval; timers resume; engines stay stopped | Approve the prompt; start each engine as a fresh run |
| The Sunday phone prompt is missed | IBC exits, the unit restarts it every 30 seconds, and a new prompt arrives each time; nothing reaches IBKR meanwhile | Approve the next prompt (`docs/runbooks/gateway-login.md`) |
| The IBKR password is wrong | The unit does not restart on an empty or malformed login file, but a wrong password is not caught by that check: if IBC exits, the unit starts it again every 30 seconds, and repeated failed logins can make IBKR lock the login | Stop the Gateway at once (`sudo systemctl stop ti-gateway`), correct the file, start it again (`docs/runbooks/gateway-login.md`) |
| The data job fails | The unit shows failed; `journalctl -u ti-data` says why; no file is written for the session | Fix the cause and `sudo systemctl start ti-data` ([market data](market-data.md)) |
| The machine dies, or the reporter never runs | **Nothing outside the server tells you.** The outside heartbeat (Healthchecks.io in plan section 7) is not built, and a reporter on the machine cannot notice the machine dying | Check the dashboard or the server yourself until it exists (tracked in the issue "External heartbeat and a recorded clock input for the margin alarm's 30-minute rule") |
| The disk fills | An engine that cannot write its log stops deciding and releases nothing more (ADR 0027); the data job writes nothing | Free space, then resume or start as `docs/runbooks/durable-log.md` says |
| The server's clock drifts behind IBKR's | The engine counts the account reading as stale and blocks every sell (`stale-broker-snapshot`); the sale back to 98% is withheld and you are alerted to that | Run the clock check ("The clock", above); fix the time syncing (or, on the Mac mini, the Mac's automatic date and time), then wait for the next close |
| `provision.sh services` is run again after paper edits | The dry-run files come back; the paper engine refuses to start | Redo the paper edits |
| A disk is lost | Only a Lightsail snapshot, if any, can restore it | `docs/runbooks/durable-log.md`, "A log that cannot be read" |

## What backs this

- Plan sections 3 (architecture), 7 (operations) and 8 (costs); ADR 0025,
  point 6 (IB Gateway under IBC, credentials only on the server); ADR 0026,
  point 8 (one engine and one client ID per account); ADR 0027 (the restart
  model: systemd never restarts an engine).
- `deploy/provision.sh`, `deploy/systemd/`, `deploy/etc/` and
  `deploy/versions.env` (every download pinned by its SHA-256).
- `deploy/rehearse.sh` checks the provisioning in a clean Ubuntu container
  (it runs twice and the second pass changes nothing; secrets are root-only;
  every unit passes `systemd-analyze verify`; the engines' client IDs are
  distinct). What was and was not rehearsed is listed in
  `docs/runbooks/lightsail-deployment.md`; no real server has run yet.
- `TestTheServiceFileStartsTheEngineWithExplicitSettings` (`cmd/engine`)
  pins the engine unit's settings; `adapter/marketdata/tests/test_tidata.py`
  checks every firing of the data job's timer against the settle time.
