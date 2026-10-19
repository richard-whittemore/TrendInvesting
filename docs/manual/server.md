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
accounts, the market-data step, one engine and one IBKR adapter per account
(only Boost's exist today), the reporter and the dashboard. It is reachable only over
Tailscale, your private network: the firewall refuses everything else.
Everything that sets it up is in `deploy/`, run by `deploy/provision.sh`.

One machine keeps the cost at about $170 a year (plan section 8). The price
is that a reporter on the same machine cannot notice the machine itself
dying: that needs an outside watcher, which is not built (below).

## Users

| User | Runs | Can see |
| --- | --- | --- |
| `ti-gateway` | IB Gateway and IBC | the IBKR login, handed to it alone |
| `ti-trader` | the data step, the engines, the Boost adapter, the reporter, the dashboard, and the `operator` and `journal-query` tools when you run them | the market data, the engines' logs; a private copy of the secrets file (the data step, the engines, the reporter) or of the Boost account's file (the Boost adapter), each handed only to the units that need it; never the IBKR login |

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

`ti-adapter-boost.service` carries the IBKR adapter ([IBKR adapter](ibkr-adapter.md)): never started at boot or restarted by systemd (`Restart=no`), every start is yours, after the engine ([paper trading](paper-trading.md), "Starting the adapter"). It reads the same `run.env` as the engine, reads the engine's log, talks to the engine over its socket, and delivers the data job's stream for the run, so it refuses to start without the engine's log or that run's stream folder, which the data job's warm-up writes ([market data](market-data.md), "The one-time warm-up"). It is skipped, not failed, until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and the adapter's code, and `provision.sh services` has created its configuration (`/etc/trend-investing/adapters/boost.json`, created once and never overwritten, in dry-run with paper sending off). Its reason for stopping is in `journalctl -u ti-adapter-boost`.

Updates: `unattended-upgrades` applies security updates only and never
reboots by itself, because a reboot logs the Gateway out.

## Where the configuration lives

| File | What it holds | Who writes it |
| --- | --- | --- |
| `/etc/trend-investing/engines/<account>.env` | the account's label (`TI_ACCOUNT`), its adapter client ID (Boost 11, Traditional IRA 12, Roth IRA 13) and, for Boost, the engine's start mode (`TI_MODE`) | copied from `deploy/etc/engines/` by `provision.sh services` |
| `/etc/trend-investing/engines/<account>.config` | the account's configuration: strategy, account, mode, rule figures, hard limits. Hashed into every run (ADR 0016). Only `boost.config` exists | the same |
| `/var/lib/trend-investing/engines/<account>/run.env` | one engine start: `TI_RUN` (the run's name, never reused), `TI_AS_OF` (its first decided session) and `TI_RESTART_ARGS` (empty, `-approval <file>` or `-resume <log>`) | you, before every start (`docs/runbooks/durable-log.md`, "Restarting an engine"; example in `deploy/etc/engines/run.env.example`) |
| `/etc/trend-investing/gateway.env` | the Gateway's restart times | `provision.sh services` |
| `/etc/trend-investing/adapters/boost.json` | the Boost adapter's settings: the Gateway's address and port (4002), client ID 11, mode and the paper sending switch (`paper_sending_enabled`), timeouts; no account number | created once from `deploy/etc/adapters/boost.json.example` by `provision.sh services`, never overwritten; you edit it for paper |

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
| `/var/lib/trend-investing/reports/` | the reporter's own state file, `<account>.reporter.json` (the only folder its unit may write) |
| `/var/lib/trend-investing/adapters/<account>/` | the adapter's pending file (`pending.json`) and the files beside it (the only folder its unit may write) |
| `/var/lib/ti-gateway/`, `/var/log/ti-gateway/` | the Gateway's own files and IBC's log |
| `/opt/trend-investing/bin/` | the installed programs: `ti-engine`, `ti-reporter`, `ti-dashboard`, `operator` and `journal-query` (from `deploy/build-engine.sh`) and `ti-data` (from `deploy/build-data.sh`). `operator` and `journal-query` are linked into `/usr/local/bin` |
| `/opt/trend-investing/adapter/`, `/opt/trend-investing/adapter-venv/`, `/opt/trend-investing/twsapi/` | the adapter's code (`provision.sh adapter`), its Python environment and IBKR's TWS API with its install record (`provision.sh twsapi`) |

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
gap is tracked in the open issue on server secrets per unit, IBC trusted
IPs, UMask and backups.

## What you start by hand

- **Once:** create the instance, join Tailscale (`sudo tailscale up --ssh`),
  close the public door (`provision.sh firewall`), enter the IBKR login and
  the secrets, start the Gateway the first time and approve the phone
  prompt (`docs/runbooks/lightsail-deployment.md`).
- **Every engine start**, after a crash or not: write `run.env`, and for a
  fresh run an approval, then `sudo systemctl start ti-engine@<account>`
  ([restart](restart.md)).
- **The adapter**, after its engine: `sudo systemctl start ti-adapter-boost`
  ([paper trading](paper-trading.md), "Starting the adapter").
- **Once per Boost run:** the data job's warm-up, after the engine has
  started once ([market data](market-data.md), "The one-time warm-up").
- **Every Sunday:** approve the Gateway's phone prompt.
- **Installing a build:** build on the Mac with `deploy/build-engine.sh`
  (and `deploy/build-data.sh`), copy `dist/` beside `deploy/`, `sudo install`
  the engine, and run `sudo deploy/provision.sh services`, which installs the
  reporter, the dashboard, `operator`, `journal-query` and `ti-data` and
  restarts the dashboard (`docs/runbooks/lightsail-deployment.md`). A new data job build only with
  a fresh engine run, or after checking it reproduces the current run's
  files.
- **The dashboard on the tailnet:** `sudo tailscale serve --bg 8088`, the
  port its unit listens on.

## After a reboot

The Gateway starts by itself and sends a phone prompt: approve it. The data
job's and the reporter's timers catch up any firing they missed, and the
dashboard starts. **Every engine and the adapter stay stopped.**
A reboot is not a crash, so each engine's next start is a fresh run you
approve ([restart](restart.md)). Schedule reboots for a Sunday, after the
cold re-login, so one phone prompt serves both.

## Reading the logs and the kill switch on the server

`journal-query` and `operator` are installed on the server, in
`/usr/local/bin`: run them as the engine's user, `sudo -u ti-trader
journal-query ...` and `sudo -u ti-trader operator ...`. `journal-query` only
reads. The engine's operator inbox is not created by the provisioner, and the
engine unit does not pass `-operator`: until you create the folder
(`/var/lib/trend-investing/engines/<account>/operator`, owned by `ti-trader`)
and add `-operator` with it to `TI_RESTART_ARGS` in `run.env` before the
engine's start ([operator](operator.md), "The operator inbox"),
`operator kill-switch` and `operator acknowledge` write files nothing reads.
**Until then there is no recorded kill switch on the server:** to stop the
program trading, stop the adapter (`sudo systemctl stop ti-adapter-boost`)
and the engine (`sudo systemctl stop ti-engine@<account>`). An order that
must not go is then settled `--never-sent` with the relay's `resolve`
command, run on the server (`docs/runbooks/first-paper-order.md` shows it).
Cancel anything already at IBKR in IBKR's own app.
  Do not treat stopping the Gateway (`sudo systemctl stop ti-gateway`) as a
  kill switch: it only delays. An order the engine has released waits in the
  adapter and goes out once the Gateway is back the same day, and an order already at IBKR keeps
  working until you cancel it in IBKR's app.

## What can go wrong, and what happens then

| What you see | What the program does | What you do |
| --- | --- | --- |
| The server has rebooted | The Gateway starts and sends a phone prompt; the timers catch up; the dashboard starts; every engine and the adapter stay stopped | Approve the prompt; start each engine as a fresh run you approve, then the adapter ([restart](restart.md)) |
| The Sunday phone prompt is missed | IBC exits, the unit starts it again after 30 seconds, and a new prompt arrives each time; nothing reaches IBKR meanwhile | Approve the next prompt (`docs/runbooks/gateway-login.md`) |
| `ti-gateway` failed and stays down; `journalctl -u ti-gateway` names the login file | The login file is empty, or holds an unknown key or value (for example a trading mode other than `paper` or `live`). The unit does not restart on these, because none fixes itself | Fill it in or correct it with `sudo -e /etc/trend-investing/ibkr-login.env`, then `sudo systemctl start ti-gateway` (`docs/runbooks/gateway-login.md`) |
| The IBKR password is wrong | That check does not catch it: if IBC exits, the unit starts it again every 30 seconds, and repeated failed logins can make IBKR lock the login | Stop the Gateway at once (`sudo systemctl stop ti-gateway`), correct the file, start it again (`docs/runbooks/gateway-login.md`) |
| `ti-data`, `ti-reporter@boost` or `ti-dashboard` failed: ".../engines/boost/run.env is missing or empty: start the Boost engine once first ..." | That unit does not run. The data job writes nothing; the reporter sends nothing | Start the Boost engine once (`docs/runbooks/durable-log.md`, "Restarting an engine"); then `sudo systemctl restart ti-dashboard` |
| A unit failed because its program is missing from `/opt/trend-investing/bin/` (`provision.sh services` printed "no .../dist/...: ... is not installed") | Nothing of that program runs | Build it on the Mac (`deploy/build-engine.sh`, `deploy/build-data.sh`), copy `dist/` beside `deploy/`, run `sudo deploy/provision.sh services` again |
| `ti-engine@<account>` or `ti-adapter-boost` was skipped, not failed (`systemctl status` says its start condition was not met) | It did not start: the engine has no binary, `.env` or `.config` for that account (the IRA engines have no configuration yet); the adapter's `ibapi`, code or configuration is not installed | Install what is missing (`provision.sh twsapi`, `provision.sh adapter`, `provision.sh services`) before starting it |
| The data job fails | The unit shows failed; `journalctl -u ti-data` says why; no file is written for the session | Fix the cause and `sudo systemctl start ti-data` ([market data](market-data.md)) |
| The reporter fails every firing | Nothing reaches your phone; the error is in `journalctl -u ti-reporter@boost` | [alerts and dashboard](alerts-and-dashboard.md), "What can go wrong" |
| The machine dies, or the reporter never runs | **Nothing outside the server tells you.** The outside heartbeat (Healthchecks.io in plan section 7) is not built, and nothing reads a `HEALTHCHECK_URL` line. A reporter on the machine cannot notice the machine dying | Check the dashboard or the server yourself until it exists |
| `provision.sh firewall` printed "SKIPPED firewall: Tailscale is not signed in yet" | The firewall is not changed, so the public door is still open | `sudo tailscale up --ssh`, then `sudo deploy/provision.sh firewall` again |
| The disk fills | An engine that cannot write its log stops deciding and releases nothing more (ADR 0027); the data job writes nothing; the reporter cannot save its state | Free space, then resume or start as `docs/runbooks/durable-log.md` says |
| `provision.sh services` is run again after paper edits | The repository's dry-run `.env` and `.config` files come back; the paper engine refuses to start. The adapter's `boost.json` is kept | Redo the paper edits |
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
  pins the engine unit's settings; `TestTheServiceFileRunsTheReporterOnTheEnginesLog`,
  `TestTheTimerMakesTheRepeatAndAcknowledgementBoundsTrue`,
  `TestTheProvisionerInstallsAndEnablesTheReporter` and
  `TestTheProvisionerStartsWhatItEnablesAndStopsWhatItRemoves` (`cmd/reporter`)
  and `TestTheServiceFileServesTheBoostLogOnTheOnePort` (`cmd/dashboard`) pin
  the reporter's and the dashboard's units, timer and installation; `adapter/marketdata/tests/test_tidata.py`
  checks every firing of the data job's timer against the settle time.
