# Runbook: the Lightsail server (setup, restart, reconnect)

Provisioning for the live server in `docs/live-trading/plan.md` section 3: one Ubuntu 24.04 LTS VM running one IB Gateway (kept logged in by IBC), a shared market-data step, three engines (one per account, each with its own adapter client ID, ADR 0026 section 8), a reporter and a read-only dashboard, reachable only over Tailscale. Everything is in `deploy/`.

> **Status:** provisioning only. The engine is built and runs Boost in dry-run (`deploy/build-engine.sh`, below); the IBKR adapter is built and tested against the fake gateway only (`ti-adapter-boost.service`, skipped until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and its code; `docs/manual/paper-trading.md`, "Starting the adapter"); the data step, the reporter and the dashboard are not built. Their systemd units exist as placeholders (`ti-data`, `ti-reporter`, `ti-dashboard`) and are skipped, not failed, until the binary is installed; the change that ships each binary fixes its arguments. Nothing here has run against a real IBKR login. Trading mode defaults to **paper** and the API to **read-only** until the limited-live gate (AGENTS.md rule 5).

**Owner steps** are marked **[OWNER]**. Agents never sign up, pay or enter credentials.

## What is on the server

| Piece | Where | Notes |
|---|---|---|
| Users | `ti-gateway` (Gateway and IBC), `ti-trader` (data step, engines, reporter, dashboard) | System users, no login shell. Engines never run as the user that can see the IBKR login. |
| IBKR login | `/etc/trend-investing/ibkr-login.env`, root:root 0600 | Outside the repository. Handed to `ti-gateway.service` only, through systemd `LoadCredential`. IBC's `config.ini` is rendered from it into a tmpfs on each start, so the password is never on disk beside the code, never in the journal, never committed. |
| Other secrets | `/etc/trend-investing/secrets.env`, root:root 0600 | Account numbers, ntfy and Healthchecks URLs. Names only in `deploy/etc/secrets.env.example`. |
| Engine settings | `/etc/trend-investing/engines/<account>.env` | Client IDs only: data step 10, `boost` 11, `trad-ira` 12, `roth-ira` 13. Each must stay unique. |
| Gateway, IBC | `/var/lib/ti-gateway/Jts/ibgateway/1050`, `/opt/ibc` | Pinned and checksummed in `deploy/versions.env`. |
| State | `/var/lib/trend-investing/{data,engines/<account>,reports}` | One journal and state directory per engine. |
| Firewall | `ufw`: deny inbound, allow only `tailscale0` | Lightsail's own firewall should also close port 22 once Tailscale works. |
| Updates | `unattended-upgrades`, security pocket only | No automatic reboot (a reboot logs the Gateway out). |

Schedule (New York time, `TZ` set in each unit): Gateway restarts nightly at 11:45 PM and does a full re-login every Sunday at 09:00 (`deploy/provision.sh` writes both to `/etc/trend-investing/gateway.env`). The data step runs weekdays 16:30; the reporter 17:30. ADR 0025 point 5 requires the nightly restart to fall after the day's reconciliation and outside trading hours: **confirm 11:45 PM against the real reconciliation time when the engines exist**, then change `TI_GATEWAY_RESTART_TIME`.

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
7. **[OWNER] Enter the IBKR login.** On the server: `sudo -e /etc/trend-investing/ibkr-login.env`, fill in `TWS_USERID` and `TWS_PASSWORD` (a paper-trading login first). Leave `TWS_TRADING_MODE=paper` and `TWS_READONLY_API=yes`. Never paste these into an issue, a PR, a chat with an agent or a shell command line. Enter account numbers and alert URLs in `/etc/trend-investing/secrets.env` the same way.
8. **[OWNER] Start the Gateway and approve 2FA:** `sudo systemctl start ti-gateway`. Approve the IBKR Mobile prompt on your phone within 3 minutes. Check `journalctl -u ti-gateway -n 50` shows a login and no credential.
9. Publish the dashboard to the tailnet only, once its binary is installed: `sudo tailscale serve --bg 8080` (**[OWNER]**, tailnet configuration).

When the data step, the reporter or the dashboard binary exists, install it at the path in its unit, `sudo systemctl daemon-reload`, and start it. Check each with `systemctl status`.

**The engine.** Build it on the Mac from a clean checkout with `deploy/build-engine.sh` (linux/amd64, stamped by `scripts/build-stamp.sh` from `git describe --always --dirty`, and it refuses a dirty tree; the engine refuses dry-run, paper and live on an unstamped build). Copy `dist/ti-engine` to the server and `sudo install -m 0755 ti-engine /opt/trend-investing/bin/ti-engine`. `provision.sh services` installs `boost.env` (`TI_MODE=dry-run`) and `boost.config` (mode `dry-run`). The engine units are **not enabled**, and `provision.sh services` disables any engine instance an earlier provisioner enabled: every start is an explicit owner step, written down in `docs/runbooks/durable-log.md`, "Restarting an engine". The unit runs the engine with `-mode`, `-config`, `-socket /run/ti-engine-<account>/engine.sock` (a 0700 directory per engine), `-log`, `-out` and `-as-of`, from the account's `run.env`, and never restarts it by itself (`Restart=no`).

## Restart drill

Rehearse before any engine trades. Record the result in #329.

1. `sudo systemctl restart ti-gateway`. Expect a re-login within about two minutes. If the session was fresh, no phone prompt; after a cold start expect one **[OWNER]**.
2. Confirm the Gateway is up: `systemctl is-active ti-gateway` and `journalctl -u ti-gateway --since "5 min ago"`.
3. A Gateway restart does not restart an engine: the engine keeps running, and its adapter reconnects and matches IBKR again before sending anything (`adapter/ibkr/loop.py`). Confirm each engine that was running is still `active` and that its journal shows a reconcile after reconnecting, not a resume from stale state (ADR 0019 and 0025 point 5: a restart must not cross an open order).
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
- **TWS API (`ibapi`):** IBKR's download page, `https://download2.interactivebrokers.com/installers/tws-api/index.html`. Only the "Latest" Mac/Linux build includes the Python API, so the pin is Latest; its link is a moving target and publishes no checksum, so compute the SHA-256 of the downloaded installer and say so in the PR. `sudo deploy/provision.sh twsapi` fetches and verifies it, as its own step (not part of `all`, so it never blocks the Gateway or the services); once verified, the same step installs its Python client into the adapter's own environment (`/opt/trend-investing/adapter-venv`) and writes `/opt/trend-investing/twsapi/INSTALLED`, which the adapter checks against the pin installed with its code (`provision.sh adapter`) and refuses to start on a difference. Because the link moves, `provision.sh twsapi` stops with a checksum mismatch whenever IBKR publishes a new build there, even though nothing in `versions.env` changed. That is the fail-closed stop working, not a fault: the build the adapter was tested on is no longer at that link. Do not edit the checksum to match; treat it as a version bump (below). ADR 0025 point 5: repeat the paper-account coverage test on the new version first, then change `TWSAPI_VERSION` and `TWSAPI_SHA256` together.
- **Tailscale:** `https://pkgs.tailscale.com/stable/`, with the checksum from the `.sha256` file beside the tarball.

## What was rehearsed, and what was not

Run `deploy/rehearse.sh` (Docker, a clean Ubuntu 24.04 container; about 350 MB of downloads). On 2026-10-16 it passed on an Apple-silicon Mac, in a native arm64 container, because linux/amd64 containers would not start under Docker Desktop there. Set `TI_REHEARSAL_PLATFORM=linux/amd64` on an x86_64 host to run the Gateway installer as well. It checks:

- the full provisioning pass runs twice and the second pass changes nothing and does not overwrite an owner-entered secret stub;
- the real pinned IBC, IB Gateway and Tailscale downloads match their checksums; IBC and Tailscale are installed;
- a corrupt cached download is not trusted;
- the service users are non-root with no login shell; secret files are root:root 0600;
- the credential renderer writes a fake password (with special characters) verbatim to a 0600 file, prints no credential, and leaves no copy under `/etc` or `/var/log`; it refuses an empty login and an unknown trading mode; the gateway's start exits 78 on an empty login, which the unit does not restart on;
- `systemd-analyze verify` is clean on every unit: any diagnostic at all fails the rehearsal, and a deliberately broken unit (an invalid `LoadCredential=` name) shows the check can fail;
- the three engine client IDs are distinct and differ from the data step's.

**Not rehearsed** (needs the real server or the owner): executing the x86_64 Gateway installer (the arm64 container downloaded and checksum-verified it only), so the install directory layout IBC expects, and the file ownership, are unconfirmed; a running systemd (the container has none, so enabling and starting units, `LoadCredential`, the timers and `Restart=` were checked only by `systemd-analyze verify`); `ufw` and Tailscale sign-in; the Gateway actually starting, IBC logging in and the 2FA prompt (no IBKR login was used, nothing was sent to IBKR); the nightly and Sunday restarts; the restart and reconnect drills above; unattended-upgrades actually applying an update; Lightsail itself. The placeholder units and the engine/adapter integration are unproven until their binaries exist.
