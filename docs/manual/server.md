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
| `ti-trader` | the data step, the engines, the reporter, the dashboard, and the `operator` tool when you run it | the market data, the engines' logs, the other secrets file; never the IBKR login |

Both are system users with no login shell. You log in as `ubuntu` and use
`sudo`.

## The units and timers

All times are New York time (each unit sets `TZ=America/New_York`).

| Unit | What it does | When it runs | Today |
| --- | --- | --- | --- |
| `ti-gateway.service` | IB Gateway under IBC, logged in to IBKR | Starts at boot (enabled). Restarts itself 30 seconds after IBC exits (for example when the weekly phone approval times out), except when the login file is empty or malformed (a wrong password is not caught: see the table below). IBC restarts the Gateway nightly at 11:45 PM and logs in again from cold every Sunday at 09:00 (`/etc/trend-investing/gateway.env`) | Installed; not yet run against a real IBKR login |
| `ti-data.timer` and `ti-data.service` | The daily data job: records the day's market data from Massive and writes the Boost engine's input ([market data](market-data.md), "The daily data job") | Monday to Friday at 17:05, retried at 18:05 and 20:05. A firing missed while the server was off runs when it starts again. A run is stopped after an hour | Built and installed by `provision.sh services` when `dist/ti-data` is beside `deploy/`. It fails, with a message, until the Boost engine has been started once |
| `ti-engine@boost.service` (and `@trad-ira`, `@roth-ira`) | One engine per account ([engine](engine.md)) | **Never by itself.** Not started at boot, never restarted by systemd (`Restart=no`): every start is yours ([restart](restart.md)) | Boost in dry-run. The IRA instances are skipped: they have no configuration file |
| `ti-reporter.timer` and `ti-reporter.service` | The reporter: alerts and the daily summary ([alerts and dashboard](alerts-and-dashboard.md)) | Monday to Friday at 17:30, **once a day** | **Not working as shipped.** The binary is not installed, so the unit is skipped. When it is installed, the unit passes none of the settings the reporter requires (`-journal`, `-state`, `-channel`), and the reporter refuses to run without all three (with no `-channel` it falls back to stdout, which alone is refused) |
| `ti-dashboard.service` | The read-only dashboard | Starts at boot once installed | **Not working as shipped.** Not installed; the unit sets `TI_DASHBOARD_ADDR=127.0.0.1:8080`, which the program does not read (it listens on `-addr`, default `127.0.0.1:8088`), and passes no `-journal`, without which it refuses to start |
| `tailscaled` | Tailscale | At boot | Installed by `provision.sh base` |

`ti-adapter-boost.service` carries the IBKR adapter ([IBKR adapter](ibkr-adapter.md)): never started at boot or restarted by systemd, every start is yours, after the engine ([paper trading](paper-trading.md), "Starting the adapter"). It is skipped until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and the adapter's code, and `provision.sh services` has created its configuration (`/etc/trend-investing/adapters/boost.json`). The `operator` and `journal-query` tools are not on the server yet.

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
| `/etc/trend-investing/secrets.env` (root only, mode 0600) | `IBKR_ACCOUNT_BOOST`, `IBKR_ACCOUNT_TRAD_IRA`, `IBKR_ACCOUNT_ROTH_IRA`, `MASSIVE_API_KEY`, `NTFY_URL`, `HEALTHCHECK_URL` | handed as a file to the data job, the engines, the reporter and the dashboard |

What each program actually reads today:

- `ti-data` reads **only** `MASSIVE_API_KEY` from that file.
- **No program reads** the `IBKR_ACCOUNT_*` lines: the account an engine
  trades is the `account_id` in its configuration file.
- **No program reads** `NTFY_URL` or `HEALTHCHECK_URL`.
- The reporter reads its channels' secrets from **environment variables**,
  `TI_NTFY_TOPIC` (and optionally `TI_NTFY_SERVER`) and
  `TI_PAGERDUTY_ROUTING_KEY` (and optionally `TI_PAGERDUTY_URL`), which
  nothing on the server sets. **Not built yet**: a place on the server to
  enter them that the reporter's unit passes on. Until it exists, the
  reporter, once installed, could not reach your phone.

Never paste a secret into an issue, a pull request, a chat with an agent or
a command line. Edit these files with `sudo -e`.

## Where the records live

| Folder | What is in it |
| --- | --- |
| `/var/lib/trend-investing/data/` | the data step's recordings (never overwritten), `streams/boost/<TI_RUN>/` (the Boost engine's daily input files) and, when you need it, `quarantine/` |
| `/var/lib/trend-investing/engines/<account>/` | each run's durable log (`<TI_RUN>.log`) and end-of-run journal (`<TI_RUN>.journal`), `run.env`, your approval files, `quarantine/` for a log set aside, and the operator inbox once you create it ([operator](operator.md), "The operator inbox": the provisioner does not create it yet) |
| `/var/lib/trend-investing/reports/` | the reporter's own state file (the only folder its unit may write) |
| `/var/lib/ti-gateway/`, `/var/log/ti-gateway/` | the Gateway's own files and IBC's log |
| `/opt/trend-investing/bin/` | the installed programs: `ti-engine` (from `deploy/build-engine.sh`) and `ti-data` (from `deploy/build-data.sh`) |

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
- **The dashboard on the tailnet:** `sudo tailscale serve`, once its unit
  works (the runbook's port, 8080, does not match the program's 8088 today).

## After a reboot

The Gateway starts by itself and sends a phone prompt: approve it. The data
job's timer catches up any firing it missed. **Every engine stays stopped.**
A reboot is not a crash, so each engine's next start is a fresh run you
approve ([restart](restart.md)). Schedule reboots for a Sunday, after the
cold re-login, so one phone prompt serves both.

## Reading the logs without the tools on the server

`journal-query` and `operator` are not installed on the server, and the
server has no Go to run them from a checkout. Until they are deployed
(tracked in the issue "Deploy the operator tool and each engine's operator
inbox"):

- To **read** a log, copy it to the Mac over Tailscale (`scp`), into a
  folder outside the repository and outside iCloud, and run `go run
  ./cmd/journal-query ...` there. It only reads.
- The **operator** commands the runbooks show (`sudo -u ti-trader operator
  ...`) cannot be run on the server today. **Today there is no recorded
  kill switch on the server.** The operator tool, and with it
  `operator kill-switch`, will be there once the pending change that
  installs it is merged; until then, to stop the program trading, stop the
  engine (`sudo systemctl stop ti-engine@<account>`). No adapter runs on the
  server yet, so stopping the engine is the whole step today. Once the
  adapter exists, it is also stopped, and an order that must not go is
  settled `--never-sent` with the relay's `resolve` command, run on the
  adapter's machine (the first-paper-order runbook shows it). Cancel anything already at
  IBKR in IBKR's own app.
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
| The machine dies, or the reporter never runs | **Nothing tells you.** The outside heartbeat (Healthchecks.io in plan section 7) is not built, and the reporter is not yet working on the server | Check the dashboard or the server yourself until both exist (tracked in the issue "External heartbeat and a recorded clock input for the margin alarm's 30-minute rule") |
| The disk fills | An engine that cannot write its log stops deciding and releases nothing more (ADR 0027); the data job writes nothing | Free space, then resume or start as `docs/runbooks/durable-log.md` says |
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
