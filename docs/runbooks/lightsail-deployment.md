# Runbook: the Lightsail server (setup, restart, reconnect)

Provisioning for the live server in `docs/live-trading/plan.md` section 3: one Ubuntu 24.04 LTS VM running one IB Gateway (kept logged in by IBC), a shared market-data step, three engines (one per account, each with its own adapter client ID, ADR 0026 section 8), a reporter and a read-only dashboard, reachable only over Tailscale. Everything is in `deploy/`.

> **Status:** provisioning only. The engine is built and runs Boost in dry-run (`deploy/build-engine.sh`, below); the data step is built (`deploy/build-data.sh`, below; it writes the Boost engine's daily input, which nothing delivers to the engine yet); the reporter, the dashboard and the owner's tools `operator` and `journal-query` are built beside the engine (`deploy/build-engine.sh`) and installed by `provision.sh services`; the broker adapter's connection, account and sending layers are built and tested against a stand-in gateway, but it has no start command or unit yet (`docs/manual/ibkr-adapter.md`). The engine's unit is skipped, not failed, until its binary is installed; the reporter's and the dashboard's fail with a message until theirs are, and until the Boost engine's `run.env` exists. Nothing here has run against a real IBKR login. Trading mode defaults to **paper** and the API to **read-only** until the limited-live gate (AGENTS.md rule 5).

**Owner steps** are marked **[OWNER]**. Agents never sign up, pay or enter credentials.

## What is on the server

| Piece | Where | Notes |
|---|---|---|
| Users | `ti-gateway` (Gateway and IBC), `ti-trader` (data step, engines, reporter, dashboard) | System users, no login shell. Engines never run as the user that can see the IBKR login. |
| IBKR login | `/etc/trend-investing/ibkr-login.env`, root:root 0600 | Outside the repository. Handed to `ti-gateway.service` only, through systemd `LoadCredential`. IBC's `config.ini` is rendered from it into a tmpfs on each start, so the password is never on disk beside the code, never in the journal, never committed. |
| Other secrets | `/etc/trend-investing/secrets.env`, root:root 0600 | Account numbers, the Massive key, and the reporter's `TI_NTFY_TOPIC`, `TI_NTFY_SERVER` and `TI_PAGERDUTY_ROUTING_KEY`. Names only in `deploy/etc/secrets.env.example`. Handed to each unit as a private credential file (`LoadCredential`), never in its environment. The outside heartbeat service is not built, so there is no line for it. |
| Programs | `/opt/trend-investing/bin/`: `ti-engine`, `ti-data`, `ti-reporter`, `ti-dashboard`, `operator`, `journal-query` | The server has no Go: each is built on the Mac and copied over (below). `operator` and `journal-query` are linked into `/usr/local/bin`, so `sudo -u ti-trader operator …` and `sudo -u ti-trader journal-query …` work as the runbooks write them. |
| Engine settings | `/etc/trend-investing/engines/<account>.env` | The account's label (`TI_ACCOUNT`), its client ID (`boost` 11, `trad-ira` 12, `roth-ira` 13; each must stay unique) and, for Boost, the start mode (`TI_MODE`). The data step uses Massive, not IB Gateway, and has none. |
| Gateway, IBC | `/var/lib/ti-gateway/Jts/ibgateway/1050`, `/opt/ibc` | Pinned and checksummed in `deploy/versions.env`. |
| State | `/var/lib/trend-investing/{data,engines/<account>,reports}` | One journal and state directory per engine. |
| Firewall | `ufw`: deny inbound, allow only `tailscale0` | Lightsail's own firewall should also close port 22 once Tailscale works. |
| Updates | `unattended-upgrades`, security pocket only | No automatic reboot (a reboot logs the Gateway out). |

Schedule (New York time, `TZ` set in each unit): Gateway restarts nightly at 11:45 PM and does a full re-login every Sunday at 09:00 (`deploy/provision.sh` writes both to `/etc/trend-investing/gateway.env`). The data step runs weekdays 17:05, after the close plus its 60 settle minutes, and again at 18:05 and 20:05 (a no-op once the day is written); the Boost account's reporter (`ti-reporter@boost.timer`) every 5 minutes, around the clock, seven days a week, which is what makes its 30-minute repeats and 10-minute acknowledgement bound true at every hour (`docs/manual/alerts-and-dashboard.md`, "When the reporter runs"). ADR 0025 point 5 requires the nightly restart to fall after the day's reconciliation and outside trading hours: **confirm 11:45 PM against the real reconciliation time when the engines exist**, then change `TI_GATEWAY_RESTART_TIME`.

## Setup

1. **[OWNER] Create the Lightsail instance.** Ubuntu 24.04 LTS, x86_64 (not ARM: IBKR ships the Gateway for Linux x64 only), US East, the 2 GB plan (plan.md section 8), with your SSH key. Enable snapshots. This is a purchase; agents do not make it.
2. **[OWNER] Create the Tailscale account** and install Tailscale on your phone and laptop.
3. SSH in as `ubuntu` over the public address and fetch the repository (read-only checkout is enough): `git clone https://github.com/richard-whittemore/TrendInvesting.git`.
4. Run the first three stages. Each is safe to repeat:

   ```sh
   cd TrendInvesting
   sudo deploy/provision.sh base      # packages, users, auto-updates, Tailscale binaries
   sudo deploy/provision.sh gateway   # IBC and IB Gateway, checksum-verified
   sudo deploy/provision.sh services  # units, directories, empty root-only secret stubs
   ```

   Do not run these with `bash -x`.
5. **[OWNER] Join the tailnet:** `sudo tailscale up --ssh`, approve in the browser, then confirm you can reach the server by its tailnet name from your laptop.
6. Close the public door: `sudo deploy/provision.sh firewall`. It refuses (and says so) until Tailscale is signed in, so it cannot lock you out. Each run resets UFW, removing every existing rule (any added by hand included), then allows inbound on Tailscale only; re-add a manual rule after each run. Then **[OWNER]** remove the port 22 rule in Lightsail's Networking tab.
7. **[OWNER] Enter the IBKR login.** On the server: `sudo -e /etc/trend-investing/ibkr-login.env`, fill in `TWS_USERID` and `TWS_PASSWORD` (a paper-trading login first). Leave `TWS_TRADING_MODE=paper` and `TWS_READONLY_API=yes`. Never paste these into an issue, a PR, a chat with an agent or a shell command line. Enter account numbers and the reporter's ntfy topic (and your own ntfy server, if you run one) and PagerDuty routing key in `/etc/trend-investing/secrets.env` the same way. A file created by an earlier provisioner has `NTFY_URL=` and `HEALTHCHECK_URL=` lines, which nothing reads: replace them with the `TI_NTFY_TOPIC=`, `TI_NTFY_SERVER=` and `TI_PAGERDUTY_ROUTING_KEY=` lines of `deploy/etc/secrets.env.example`.
8. **[OWNER] Start the Gateway and approve 2FA:** `sudo systemctl start ti-gateway`. Approve the IBKR Mobile prompt on your phone within 3 minutes. Check `journalctl -u ti-gateway -n 50` shows a login and no credential.
9. Publish the dashboard to the tailnet only, once its binary is installed: `sudo tailscale serve --bg 8088` (**[OWNER]**, tailnet configuration). 8088 is the port the dashboard's unit listens on, on loopback.

**The data step.** Build it on the Mac from a clean checkout with `deploy/build-data.sh` (one Python file, `dist/ti-data`, standard library only; it refuses a dirty tree, is byte for byte the same for one commit, and `ti-data --version` prints the commit stamp), copy it to the server as `dist/ti-data` beside the `deploy/` folder, and run `sudo deploy/provision.sh services`, which installs it as `/opt/trend-investing/bin/ti-data`. Put the Massive key on the `MASSIVE_API_KEY=` line of `/etc/trend-investing/secrets.env` (`sudo -e`; a file created by an earlier provisioner has no such line: add it). Until the Boost engine's `run.env` exists the unit fails at each firing with a message naming it (never a silent skip); once it exists, run the warm-up once (`docs/manual/market-data.md`, "The daily data job") before the timer's first run. Install a new ti-data build only together with a fresh engine run and its warm-up, or after confirming it reproduces the current run's files: a build that encodes a delivered session differently stops the job (`docs/manual/market-data.md`, "A file already written differs").

**The engine, the reporter, the dashboard and the two tools.** Build them on the Mac from a clean checkout with `deploy/build-engine.sh` (linux/amd64, stamped by `scripts/build-stamp.sh` from `git describe --always --dirty`, and it refuses a dirty tree; the engine refuses dry-run, paper and live on an unstamped build). It writes `dist/ti-engine`, `dist/ti-reporter`, `dist/ti-dashboard`, `dist/operator` and `dist/journal-query`. Copy `dist/` to the server beside the `deploy/` folder, `sudo install -m 0755 dist/ti-engine /opt/trend-investing/bin/ti-engine`, and run `sudo deploy/provision.sh services`, which installs the other four, names any that is missing, and restarts the dashboard so it runs the new build. After an upgrade, check it did: `systemctl status ti-dashboard` shows it started after the install (if not, `sudo systemctl restart ti-dashboard`). The reporter needs no restart: it is started afresh at each firing and runs the new build at the next one.

**The reporter and the dashboard.** `provision.sh services` enables and starts `ti-reporter@boost.timer` (and `ti-data.timer`), and enables and restarts `ti-dashboard.service`, so they run without a reboot; it first stops and disables an earlier provisioner's single `ti-reporter.timer` and removes it. Both read the Boost engine's current log, named by `TI_RUN` in `/var/lib/trend-investing/engines/boost/run.env`, so both fail with a message naming that file until the engine has been started once. The reporter reads `run.env` again at every firing and follows a fresh run's new log by itself; the dashboard reads it when it starts, so after a fresh run `sudo systemctl restart ti-dashboard`. The reporter fails, naming the line, until `secrets.env` holds its ntfy topic and PagerDuty key. Check each with `systemctl status ti-reporter@boost ti-dashboard` and `journalctl -u ti-reporter@boost`; `systemctl list-timers ti-reporter@boost.timer` shows the next firing. The IRA reporters are not enabled: the IRA engines have no configuration yet.

**The engine's unit.** `provision.sh services` installs `boost.env` (`TI_MODE=dry-run`) and `boost.config` (mode `dry-run`). The engine units are **not enabled**, and `provision.sh services` disables any engine instance an earlier provisioner enabled: every start is an explicit owner step, written down in `docs/runbooks/durable-log.md`, "Restarting an engine". The unit runs the engine with `-mode`, `-config`, `-socket /run/ti-engine-<account>/engine.sock` (a 0700 directory per engine), `-log`, `-out` and `-as-of`, from the account's `run.env`, and never restarts it by itself (`Restart=no`).

## Restart drill

Rehearse before any engine trades. Record the result in #329.

1. `sudo systemctl restart ti-gateway`. Expect a re-login within about two minutes. If the session was fresh, no phone prompt; after a cold start expect one **[OWNER]**.
2. Confirm the Gateway is up: `systemctl is-active ti-gateway` and `journalctl -u ti-gateway --since "5 min ago"`.
3. A Gateway restart does not restart an engine: the engine keeps running, and its adapter reconnects (the adapter is not built yet). Confirm each engine that was running is still `active` and that its journal shows a reconcile after reconnecting, not a resume from stale state (ADR 0019 and 0025 point 5: a restart must not cross an open order).
4. `sudo reboot`, then repeat 2. This one needs your phone **[OWNER]**. Schedule reboots for Sunday, after the weekly cold restart, so one prompt serves both. The engines stay stopped after a reboot (they are not enabled): start each by `docs/runbooks/durable-log.md`, "Restarting an engine" (a reboot is not a crash, so each is a fresh run the owner approves).
5. Kill one engine (`sudo systemctl kill --signal=SIGKILL ti-engine@boost`). Confirm the other two stayed `active` and that boost stays **stopped** (`Restart=no`): a fault touches one account only (ADR 0026 section 8), and only the owner restarts it. Resume it as a crash by the runbook.

## Reconnect drill: the Gateway will not log in

1. `journalctl -u ti-gateway -n 100` and the IBC log in `/var/log/ti-gateway/`. Look for a 2FA timeout, bad credentials or "existing session".
2. **2FA timed out:** the unit restarts IBC and sends a new prompt every 30 s plus the timeout. **[OWNER]** approve it on the phone.
3. **Existing session detected** (you logged in to IBKR from the app or website): IBC is set to `manual`, so it waits rather than fighting for the session. Log out of the other session, then `sudo systemctl restart ti-gateway`.
4. **Bad credentials:** do not retry in a loop (IBKR locks the login). `sudo systemctl stop ti-gateway`, **[OWNER]** correct `/etc/trend-investing/ibkr-login.env`, start again.
5. Until the Gateway is back, engines hold: they must not trade without it. Follow `docs/runbooks/reconciliation.md` if any account left Normal. Never edit numbers to repair a state.

## Bumping a pinned version

Every download in `deploy/versions.env` is verified by SHA-256 and provisioning stops on a mismatch.

- **IBC:** release and asset at `https://github.com/IbcAlpha/IBC/releases`. The release publishes no checksum, so compute it yourself from the downloaded asset and say so in the PR.
- **IB Gateway:** IBKR's stable installer link is a moving target and publishes no checksum; the version is read from `https://download2.interactivebrokers.com/installers/ibgateway/stable-standalone/version.json`. When the file changes, provisioning fails closed. ADR 0025 point 5: repeat the paper-account execution-coverage test on the new version first, then change `GATEWAY_BUILD`, `GATEWAY_MAJOR` and `GATEWAY_SHA256` together.
- **TWS API (`ibapi`):** IBKR's download page, `https://download2.interactivebrokers.com/installers/tws-api/index.html`. Only the "Latest" Mac/Linux build includes the Python API, so the pin is Latest; its link is a moving target and publishes no checksum, so compute the SHA-256 of the downloaded installer and say so in the PR. `sudo deploy/provision.sh twsapi` fetches and verifies it, as its own step (not part of `all`, so it never blocks the Gateway or the services); it is not yet installed into the adapter's environment (that step comes with the paper-account test). Because the link moves, `provision.sh twsapi` stops with a checksum mismatch whenever IBKR publishes a new build there, even though nothing in `versions.env` changed. That is the fail-closed stop working, not a fault: the build the adapter was tested on is no longer at that link. Do not edit the checksum to match; treat it as a version bump (below). ADR 0025 point 5: repeat the paper-account coverage test on the new version first, then change `TWSAPI_VERSION` and `TWSAPI_SHA256` together.
- **Tailscale:** `https://pkgs.tailscale.com/stable/`, with the checksum from the `.sha256` file beside the tarball.

## What was rehearsed, and what was not

Run `deploy/rehearse.sh` (Docker, a clean Ubuntu 24.04 container; about 350 MB of downloads). On 2026-10-16 it passed on an Apple-silicon Mac, in a native arm64 container (and again on 2026-10-18 with the reporter, dashboard and tool checks below), because linux/amd64 containers would not start under Docker Desktop there. Set `TI_REHEARSAL_PLATFORM=linux/amd64` on an x86_64 host to run the Gateway installer as well. It checks:

- the full provisioning pass runs twice and the second pass changes nothing and does not overwrite an owner-entered secret stub;
- the real pinned IBC, IB Gateway and Tailscale downloads match their checksums; IBC and Tailscale are installed;
- a corrupt cached download is not trusted;
- the service users are non-root with no login shell; secret files are root:root 0600;
- the credential renderer writes a fake password (with special characters) verbatim to a 0600 file, prints no credential, and leaves no copy under `/etc` or `/var/log`; it refuses an empty login and an unknown trading mode; the gateway's start exits 78 on an empty login, which the unit does not restart on;
- the reporter, the dashboard, `operator` and `journal-query` are installed from the build, and both tools run as `sudo -u ti-trader <tool>`; the old single reporter's timer, service and timer link are removed;
- the reporter, run with its unit's own command line (expanded as systemd would) as its unit's user `ti-trader`, on files owned as on the server, against a durable log the engine wrote, reads its secrets from the credential file only, delivers to a stand-in ntfy and PagerDuty on loopback, writes its state file, and sends nothing again on a second run;
- `systemd-analyze verify` is clean on every unit, the reporter's timer and its calendar included: any diagnostic at all fails the rehearsal, and a deliberately broken unit (an invalid `LoadCredential=` name) shows the check can fail;
- the three engine client IDs are distinct and differ from the data step's.

**Not rehearsed** (needs the real server or the owner): executing the x86_64 Gateway installer (the arm64 container downloaded and checksum-verified it only), so the install directory layout IBC expects, and the file ownership, are unconfirmed; a running systemd (the container has none, so enabling and starting units, `LoadCredential`, the timers and `Restart=` were checked only by `systemd-analyze verify`; in particular `provision.sh services` starting the reporter's timer, restarting the dashboard, and stopping and disabling the old single reporter before removing it are skipped in the rehearsal and checked only by `cmd/reporter`'s unit test of the script); `ufw` and Tailscale sign-in; the Gateway actually starting, IBC logging in and the 2FA prompt (no IBKR login was used, nothing was sent to IBKR); the nightly and Sunday restarts; the restart and reconnect drills above; unattended-upgrades actually applying an update; Lightsail itself. The reporter's timer firing under a real systemd, and its `LoadCredential` hand-over, are checked only by `systemd-analyze verify` and by running its command line by hand; the adapter integration is unproven until the adapter exists.
