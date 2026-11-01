# The server: what runs where

*For the owner. The Lightsail server: which programs run on it, under which
users, when each starts, where its configuration and secrets live, what is
backed up, what you start by hand, and what can go wrong. The step-by-step
setup is `docs/runbooks/lightsail-deployment.md`; this chapter is what you
need to know to look after it. Paper trading runs here. The Mac mini's
container is only a local rehearsal of this server, set up by the same
provisioner and units: [mac-mini](mac-mini.md) says what differs there.*

## What it is and why

One virtual machine at AWS Lightsail: Ubuntu 24.04 LTS, x86_64 (IBKR ships
IB Gateway for Linux x64 only), US East, the 2 GB plan (plan sections 3 and
8). Everything that trades runs on it: one IB Gateway shared by all three
accounts, the market-data step, one engine and one IBKR adapter per account
(only Boost's exist today), the reporter and the dashboard. It is reachable only over
Tailscale, your private network: the firewall refuses everything else. The
Gateway's API port (4002 for paper, 4001 for live) is for programs on the
server itself, the engines and adapters, and the Gateway refuses every other
client: the rendered IBC settings reject incoming connections from any address
but the server's own and trust none. The firewall also denies those two ports,
but that is a second layer, not the guard: Tailscale's own rules let tailnet
traffic through before the firewall looks. On a server set up earlier, apply
all of this with: `git pull`, `sudo deploy/provision.sh services` (installs the changed `render-ibc-config.sh` into `/usr/local/libexec/trend-investing`. `services` reinstalls the five programs (`ti-data`, `ti-reporter`, `ti-dashboard`, `operator`, `journal-query`) found in the checkout's `dist/`, and never `ti-engine`. `dist/` on this server holds the builds copied from the Mac at the first install and has not been changed since. That holds until the first `ti-release`; after that, run `services` only with `dist/` moved aside (or do upgrades only through `ti-release`). If you are unsure, move `dist/` aside before running `services` (the programs already installed then stay)), `sudo deploy/provision.sh gateway` (repairs `/opt/ibc` modes), `sudo deploy/provision.sh twsapi` (rebuilds the adapter environment with open modes), `sudo deploy/provision.sh firewall`, then `sudo systemctl restart ti-gateway` (the Gateway logs in again; after a cold
start expect an IBKR Mobile prompt). `services` copies the repository's engine
`.env` files over `/etc/trend-investing/engines/` and keeps every `.config`
already there (see the caution under the file table). Then check, as the owner: the Gateway log shows `Login has completed`, and `sudo grep -E '^(AcceptIncomingConnectionAction|TrustedTwsApiClientIPs)=' /run/ti-gateway/config.ini` prints `AcceptIncomingConnectionAction=reject` and `TrustedTwsApiClientIPs=` (an empty list). The guards are: the IBC files' modes; those two settings (the real guard); the Gateway's read-only API, which holds in dry-run only (once paper sending is on, the IBC `reject` setting is the only guard inside the Gateway, so add the Tailscale access rules that close ports 4001 and 4002 to tailnet devices before turning read-only off); and the firewall's deny of ports 4001 and 4002, a second layer that Tailscale's own input rules may bypass. Closing the port at the network level for tailnet devices is done with Tailscale access rules, a separate owner step. Each `provision.sh` step is safe to repeat.
Everything that sets it up is in `deploy/`, run by `deploy/provision.sh`.

One machine keeps the cost at about $170 a year (plan section 8). The price
is that a reporter on the same machine cannot notice the machine itself
dying: that needs an outside watcher, a Healthchecks check that pages you
when the reporter's pings stop ("The outside heartbeat", below).

## Users

| User | Runs | Can see |
| --- | --- | --- |
| `ti-gateway` | IB Gateway and IBC | the IBKR login, handed to it alone |
| `ti-ntfy` | your own ntfy server (below) | its own cache folder, `/var/lib/ntfy`, and nothing else |
| `ti-trader` | the data step, the engines, the Boost adapter, the reporter, the dashboard, and the `operator` and `journal-query` tools when you run them | the market data, the engines' logs; a private copy of the secrets file (the data step, the engines, the reporter) or of the Boost account's file (the Boost adapter), each handed only to the units that need it; never the IBKR login |

All are system users with no login shell. You log in as `ubuntu` and use
`sudo`.

## The units and timers

Times below are New York time unless marked UTC (the memory timer uses UTC).

| Unit | What it does | When it runs | Today |
| --- | --- | --- | --- |
| `ti-gateway.service` | IB Gateway under IBC, logged in to IBKR | Starts at boot (enabled). Restarts itself 30 seconds after IBC exits (for example when the weekly phone approval times out), except when the login file is empty or malformed, or after 3 login attempts of any kind within 24 hours ("The Gateway is logged out"). IBC restarts the Gateway nightly at 11:45 PM and logs in again from cold every Sunday at 09:00 (`/etc/trend-investing/gateway.env`) | Installed; not yet run against a real IBKR login |
| `ti-data.timer` and `ti-data.service` | The daily data job: records the day's market data from Massive and writes the Boost engine's input ([market data](market-data.md), "The daily data job") | Monday to Friday at 06:05, 07:05 and 08:05, for the previous session: Massive's free plan serves a session's prices only the next morning, and the adapter's morning reading stands for the session's close only with the Flex reader's executions proof, else the session is withheld ([market data](market-data.md), "The free plan's timing"; [reconciliation](reconciliation.md), "The executions proof"). A 403 at the first two firings is a quiet wait; one that persists at 08:05 fails the unit and pages you ("The free plan's timing" in [market data](market-data.md)). A firing missed while the server was off runs when it starts again. A run is stopped after an hour | Built and installed by `provision.sh services` when `dist/ti-data` is beside `deploy/`. Until the Boost engine has been started once (no `run.env` and no run log in its folder) a firing waits quietly and succeeds; it refuses, and the unit fails, for a real fault, including a `run.env` lost after a run (a run log or a run stream folder is the sign) ("The reporter before the first run", below) |
| `ti-engine@boost.service` (and `@trad-ira`, `@roth-ira`) | One engine per account ([engine](engine.md)) | **Never by itself.** Not started at boot, never restarted by systemd (`Restart=no`): every start is yours ([restart](restart.md)) | Boost in dry-run. `@trad-ira` starts in dry-run once you name its account and birth month ([momentum](momentum.md), "Starting the Traditional IRA's engine in dry-run"); `@roth-ira` has its configuration and starts in dry-run once you name its account and birth month |
| `ti-reporter@boost.timer` and `ti-reporter@boost.service` | The reporter: alerts and the daily summary ([alerts and dashboard](alerts-and-dashboard.md)). One instance per account; Boost's and both IRA reporter timers are enabled | **Every 5 minutes, around the clock, seven days a week**, so an alert reaches you within 5 minutes, overnight and at weekends too. A firing missed while the server was down runs at boot. A run is stopped after 4 minutes | Built and installed by `provision.sh services`. It reads the log named by the Boost engine's `run.env` at every firing, until the engine has been started once it logs that it is waiting for the account's first run, skips only the reading of the log, and otherwise runs as usual ("The reporter before the first run", below); it fails, with a message, until the `secrets.env` entries below are filled in |
| `ti-memory.timer` | Triggers the memory sampler ([Measuring the server](#measuring-the-server)) | Every five minutes in UTC; the sampler appends only within the selected week | Installed; the owner enables it for the measurement week |
| `ti-memory.service` | Records server total memory, unit counters and installed schedules | On each memory timer firing or a manual start; stopped after two minutes | Requires `memory.env`; failed samples page once and a good run resolves the episode |
| `ti-unit-failed@.service` | The page that a unit failed: the engine, the adapter, the reporter, the data jobs, the Flex readers and the memory sampler each name it in `OnFailure=`, so systemd starts one instance, named after the failed unit (for the engine, `ti-unit-failed@ti-engine@boost.service.service`), the moment that unit fails. It sends one urgent page through the reporter's channels naming the unit, the failed run's last journal line and the manual row to read ("A failed unit", below) | Only when one of those units fails; never by itself otherwise | Installed by `provision.sh services` with the other units |
| `ti-dashboard.service` | The read-only dashboard, on loopback port 8088 | Starts at boot (enabled) | Built and installed by `provision.sh services`; it finds the Boost engine's current run, the newest link of its chain of runs, on every request and follows the chain of runs back to the first, so a restart or an upgrade of the engine needs no dashboard restart. It shows a message, not a figure, until the engine has run once |
| `ti-ntfy.service` | Your own ntfy server: the only place the daily summary goes ("Your own ntfy server", below). Listens on the server's Tailscale address, port 2586, and nowhere else | Starts at boot once `provision.sh ntfy` has enabled it; tried again every 10 seconds if the Tailscale address is not up yet | Installed by `provision.sh ntfy`, which you run after joining Tailscale. Not part of `provision.sh all` |
| `tailscaled` | Tailscale | At boot | Installed by `provision.sh base` |

`ti-adapter-boost.service` carries the IBKR adapter ([IBKR adapter](ibkr-adapter.md)): never started at boot or restarted by systemd (`Restart=no`), every start is yours, after the engine ([paper trading](paper-trading.md), "Starting the adapter"). It reads the same `run.env` as the engine, reads the engine's log, talks to the engine over its socket, and delivers the data job's stream for the run, so it refuses to start without the engine's log or that run's stream folder, which the data job's warm-up writes ([market data](market-data.md), "The one-time warm-up"). It is skipped, not failed, until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and the adapter's code, and `provision.sh services` has created its configuration (`/etc/trend-investing/adapters/boost.json`, created once and never overwritten, in dry-run with paper sending off). Its reason for stopping is in `journalctl -u ti-adapter-boost`. Each IRA's adapter is an instance of the template `ti-adapter@.service` (`ti-adapter@trad-ira`, client ID 12, configuration `/etc/trend-investing/adapters/trad-ira.json`, created once in dry-run with paper sending off), with the same rules: never started at boot, never restarted, started by you after its engine and its warm-up; skipped until the same installs and its configuration exist; the template refuses to run as `ti-adapter@boost` ([IBKR adapter](ibkr-adapter.md), "The IRAs' adapter").

Updates: `unattended-upgrades` applies security updates only and never
reboots by itself, because a reboot logs the Gateway out.

## Upgrading the programs

The server has no Go and does not update itself. To put a fix on it, tag the merged commit on your Mac (`git tag -a v1.2.3 -m "v1.2.3"` and push the tag); GitHub builds the five programs and publishes them with their checksums and a build attestation. Then, on the server over Tailscale, run `sudo ti-release v1.2.3`. It downloads that release, checks it, keeps the build now installed as the previous one, installs the new one, restarts the dashboard and prints what changed. If the dashboard does not come back, it puts the previous build back and tells you.

**Installing a release** is quiet by design. Once the release is fetched and checked, `ti-release` opens a maintenance window sized to its own worst case (the wait below plus 5 minutes, never more than about 7), waits until it is 1 to 3 minutes past a 5-minute mark on the clock (the reporter fires at the marks, up to a minute late, and a firing can run for minutes, so no time is certain to be clear of one; it asks the time, divides the seconds since the epoch by 300 and waits for the 60th second of the next 5-minute cycle if it is not already between the 60th and the 239th, which makes a collision unlikely, and the window makes one harmless), installs the programs, restarts the dashboard, checks that it stays up, and closes the window as it exits, whether the release worked or not. If you have already opened a window by hand (`ti-maintenance start`), `ti-release` uses it as it is: it neither replaces, extends nor ends it, and says so. While the window is open a unit that fails because of the install is recorded and not paged, the heartbeat still pings, and every capital-safety alert still pages ([alerts and dashboard](alerts-and-dashboard.md), "Maintenance windows"). A release that does not verify puts the previous build back and closes the window too, so whatever is still broken pages at the reporter's next firing, at most 5 minutes later. You will see "ti-release: waiting N seconds for a quiet moment after a 5-minute mark" and the window's end time in its output; the dashboard says "Maintenance until ..." meanwhile. `ti-release` needs `ti-maintenance` beside it: `sudo deploy/release.sh --install` puts both in `/usr/local/sbin`, and a `ti-release` older than this change must be installed again that way once before its next run; without `ti-maintenance` it installs nothing and says so. The marker is `/run/ti-maintenance/maintenance.json`, in a folder that only root can write: the reporter, the failed-unit hook and the dashboard only read it, so none of them can be made to follow a path that another user swapped, and no unit file or permission changes. It is on `/run`, so a reboot ends any window. It has one fixed layout that `ti-maintenance` writes and the reporter and `ti-maintenance status` both insist on; a file in any other layout (a typo, a damaged copy) counts as no window, and the reporter says so once in its output, never as a page. Each window has an id that `ti-maintenance start` prints; `ti-release` ends only the window with the id it opened (`ti-maintenance end --id <id>`), and just before it replaces anything it needs at least 120 seconds of window left: if there is less (a window you opened, or its own), it waits for that one to end and then opens one of its own for 300 seconds, so it never installs with none and never has two. `ti-maintenance start` refuses while a window is open (`start --replace` replaces it), and every `ti-maintenance` command takes a lock (`flock`, which the server has; the command refuses without it), waiting at most 10 seconds and then failing with "maintenance lock busy", so a start that arrives while another command is closing a window is not undone by it and a stalled command never blocks `ti-release` for ever. A window's length counts from when the lock was got. If `start` is refused because someone opened a window meanwhile, `ti-release` looks at how much of it is left and waits it out or opens its own, up to four times, and refuses to install if none holds; it checks that no engine unit is restarting again after any wait, right before the swap. To cut a window short, `sudo ti-maintenance end`; `ti-maintenance status` says whether one is open, judging the marker as the reporter does.

Two things it never does. It never restarts an engine: a running engine keeps the build it started with, and a new engine build takes effect only at the next fresh run you approve. And nothing is ever deployed because something was merged: only a tag you push and a command you run.

`sudo ti-release --rollback` puts the previous build back. It refuses while an engine unit is starting or stopping; wait and run it again. The step-by-step is in `docs/runbooks/lightsail-deployment.md`, "Upgrading: tag, release, `ti-release`".

`ti-release` checks, before it installs anything, that each program matches the release's checksums, that each is a Linux program for this server, and that GitHub's build attestation says it was built by this repository's release workflow from exactly the commit the tag names, so an older release cannot be passed off under a newer tag. That check needs `gh`, which the first install puts on the server; without `gh`, `ti-release` refuses and installs nothing. It also refuses while any engine unit is starting or stopping (it asks again just before it swaps the programs), and treats a dashboard that comes up and then stops within a few seconds as not come back. If any step of the install fails, it puts the build that was current back and records nothing.

**If a release fails part way.** `ti-release` normally puts the previous build back by itself and says so. In the rare case it cannot, it leaves a copy of a build in the releases folder under a name starting with a dot. Only `.replaced` stops `ti-release`: it holds a build that could not be kept after a release, and while it is there `ti-release` prints an ALERT naming the build's tag and installs nothing. `.rescue-<tag>` holds a build that could not be moved back into its own folder; its ALERT names it, it is the only copy so nothing deletes it, and it never blocks a run. To see which build is installed and whether anything is kept, run `ti-release --status`: it prints the current and previous tags and any kept copy, and changes nothing. To clear a copy, follow the ALERT's tag and the runbook's "Upgrading" section, which shows how to move it back by hand or aside. Once `.replaced` is clear, `ti-release` runs again, and `sudo ti-release --rollback` is available to put the previous build back.

## Where the configuration lives

In the paths and unit names below, `<instance>` is the engine instance: the
name of its configuration file (`boost`, `trad-ira`, `roth-ira`), as in
`ti-engine@boost`. It is not the account id the `-account` flag takes: the
Boost instance's account id is `joint`.

| File | What it holds | Who writes it |
| --- | --- | --- |
| `/etc/trend-investing/engines/<instance>.env` | the account's label (`TI_ACCOUNT`), its adapter client ID (Boost 11, Traditional IRA 12, Roth IRA 13) and the engine's start mode (`TI_MODE`, `dry-run`) | installed from `deploy/etc/engines/` by `provision.sh services` when there is none, then kept; the shipped one beside it as `<instance>.env.example` |
| `/etc/trend-investing/engines/<instance>.config` | the account's configuration: strategy, account, mode, rule figures, hard limits. Hashed into every run (ADR 0016). All three accounts have templates. Each ships with placeholders (`joint`, `trad-ira`, `roth-ira`; each IRA's birth month `YYYY-MM`), never your account or birth month, which you name on the server. Each IRA reads its own glide anchors and retirement horizon from this file | installed from `deploy/etc/engines/` by `provision.sh services` only when there is none; then yours, kept by every later run (the shipped one goes beside it as `<instance>.config.example`) |
| `/var/lib/trend-investing/engines/<instance>/run.env` | one engine start: `TI_RUN` (the run's name, never reused), `TI_AS_OF` (its first decided session) and `TI_RESTART_ARGS` (empty, `-approval <file>` or `-resume <log>`) | you, before every start (`docs/runbooks/durable-log.md`, "Restarting an engine"; example in `deploy/etc/engines/run.env.example`) |
| `/etc/trend-investing/gateway.env` | the Gateway's restart times | `provision.sh services` |
| `/etc/trend-investing/adapters/boost.json` | the Boost adapter's settings: the Gateway's address and port (4002), client ID 11, mode and the paper sending switch (`paper_sending_enabled`), timeouts; no account number | created once from `deploy/etc/adapters/boost.json.example` by `provision.sh services`, never overwritten; you edit it for paper |

**Caution:** `provision.sh services` installs an account's settings file
(`*.env`) and its configuration (`*.config`) only when there is none: one
already there, with a `TI_MODE` you set to `paper` or the account you named,
is kept byte for byte, and the shipped file is put beside it as
`<instance>.env.example` or `<instance>.config.example` to compare. A line the
shipped settings file adds (such as the IRA adapter's `TI_ADAPTER_ACCOUNT_KEY`)
is yours to copy by hand; `docs/runbooks/first-dry-run-day.md`, "The
Traditional IRA", gives the lines (rehearsed by `deploy/rehearse.sh` and
`deploy/rehearsal/test-engine-configs.sh`).

## Where the secrets live

Every secret file is kept on the disk **only encrypted**, as
`/etc/trend-investing/<name>.gpg` (root only, mode 0600), with a passphrase
you chose. The named files on the disk cannot be read without it: not by
someone who gets a copy of the disk or a Lightsail snapshot, and not by the
server itself. That holds for copies taken after the move to encrypted files,
provided swap stays off ("Swap must stay off", below) and no secret is written to the disk
in plain text by hand; on a server that once kept them in plain text, read
"Shredding is not a guarantee" below.
After every boot you type the passphrase once (`sudo ti-unlock`, below), and
the files are decrypted into memory, in `/run/trend-investing/secrets/`
(root only, a separate in-memory file system, gone at the next boot); the
programs are handed their private copies from there.

Why a passphrase, not something automatic: Lightsail has no hardware key
store (TPM), so any key the server could use by itself would have to sit on
the same disk as the files, and a snapshot would carry both.

| File | What it holds | Who reads it |
| --- | --- | --- |
| `ibkr-login.env` | the IBKR username and password, the trading mode (`paper`) and the read-only switch (`TWS_READONLY_API=yes`). Read-only means the Gateway refuses every order from the adapter, paper included; you set it to `no` yourself, as a step of the first-paper-order runbook | only `ti-gateway.service`, through systemd's credential hand-over. IBC's settings file is written from it into memory on each start, never to disk beside the code |
| `secrets.env` | `IBKR_ACCOUNT_TRAD_IRA`, `IBKR_ACCOUNT_ROTH_IRA`, `MASSIVE_API_KEY`, `TI_NTFY_TOPIC`, `TI_NTFY_SERVER`, `TI_PUSHOVER_APP_TOKEN`, `TI_PUSHOVER_USER_KEY`, `TI_PAGERDUTY_ROUTING_KEY` (unused: the server pages through Pushover), `TI_GITHUB_ISSUES_TOKEN`, `TI_GITHUB_ISSUES_REPO`, `TI_HEARTBEAT_URL`, and the Flex reader's `TI_IBKR_FLEX_TOKEN`, `TI_IBKR_FLEX_QUERY_ID` and `TI_IBKR_FLEX_TRADES_QUERY_ID` | handed as a private credential file (`LoadCredential`) to the data job, the engines, the reporter and the Flex readers; the dashboard is given none |
| `accounts/boost.env` | `IBKR_ACCOUNT_BOOST`: the Boost account's number, alone | handed as a private copy to the Boost adapter (`ti-adapter-boost.service`) and to the Boost account's Flex reader (`ti-flex@boost.service`) |
| `accounts/trad-ira.env` | `IBKR_ACCOUNT_TRAD_IRA`: the Traditional IRA's number, alone | handed as a private copy to that account's adapter (`ti-adapter@trad-ira.service`) and its Flex reader (`ti-flex@trad-ira.service`) |
| `accounts/roth-ira.env` | `IBKR_ACCOUNT_ROTH_IRA`: the Roth IRA's number, alone | handed as a private copy to its adapter (`ti-adapter@roth-ira.service`, client ID 13) and Flex reader (`ti-flex@roth-ira.service`) |

An existing server's `secrets.env` keeps the layout it was provisioned with, because provisioning never overwrites it, so the owner may tidy it by hand to match `deploy/etc/secrets.env.example`.

What each program actually reads today:

- `ti-data` reads **only** `MASSIVE_API_KEY` from that file.
- Each account's Flex reader reads that account's number from its own
  account file (`accounts/<account>.env`), and no other account's. No
  program reads the `IBKR_ACCOUNT_TRAD_IRA` or `IBKR_ACCOUNT_ROTH_IRA` lines
  of `secrets.env`. The Boost adapter reads `IBKR_ACCOUNT_BOOST` from its own
  file and from no other. The account an engine trades is the `account_id`
  in its configuration file.
- The reporter reads `TI_NTFY_TOPIC`, `TI_NTFY_SERVER` (your own server's
  URL, which `provision.sh ntfy` prints; empty means the public ntfy.sh relay,
  which never gets the daily summary), `TI_PUSHOVER_APP_TOKEN` and
  `TI_PUSHOVER_USER_KEY` from that file
  (`-secrets-file`), through its unit's credential. While one of them is empty the
  reporter sets that channel aside (ntfy for the topic, Pushover for the token
  or the user key) and says so in its output; the other channels still
  deliver, including the GitHub issues, and its unit exits non-zero, so it
  shows as failed. Fill them in with `sudo ti-secrets edit secrets.env`; the
  next firing uses them. Every instance reads `TI_HEARTBEAT_URL` from its
  private credential, the address of your check at the outside heartbeat service;
  only Boost pings it. The `trad-ira` and `roth-ira` instance drop-ins separately
  set `TI_HEARTBEAT=off`. The URL is optional for the run, but without
  it nothing outside the server notices the server dying
  ("The outside heartbeat", below).
- The reporter's `github-issues` channel reads `TI_GITHUB_ISSUES_TOKEN`, a
  fine-grained token with **Issues read and write on the one repository and
  no other permission**, and `TI_GITHUB_ISSUES_REPO`, that repository as
  `owner/name`. The reporter's unit and the failed-unit hook both name
  `github-issues` in `-channel`, beside `ntfy` and `pushover`, so nothing is
  edited on the server to turn it on. While either value is empty the channel
  is set aside: the run says "the github-issues channel needs
  TI_GITHUB_ISSUES_TOKEN" (or "... TI_GITHUB_ISSUES_REPO as owner/name"), ntfy
  and Pushover still deliver, and the unit exits non-zero, so a server
  without the alerts repository shows as failed until you fill both in. Fill
  them in before installing the unit files on a running server: until then
  every firing fails, sends no heartbeat ping (Healthchecks pages) and the
  failed-unit page repeats hourly. The
  daily summary never goes to this channel. How to create and rotate the
  token:
  [alerts and dashboard](alerts-and-dashboard.md), "Alerts as GitHub Issues".

Never paste a secret into an issue, a pull request, a chat with an agent or
a command line.

**Editing a file:** `sudo ti-secrets edit secrets.env` (or `ibkr-login.env`,
`accounts/boost.env`, `accounts/trad-ira.env`, `accounts/roth-ira.env`). It
asks for the passphrase, decrypts the file into memory, opens it in the
editor, and when you save and quit encrypts it again (the old encrypted file
is replaced only once the new one is checked), updates the unlocked copy if
the secrets are unlocked, and removes the plain text. Each unit reads the new
value at its next start: the reporter at its next firing, the Gateway after
`sudo systemctl restart ti-gateway`. A wrong passphrase writes nothing, and
quitting without a change writes nothing either (except that an unlocked
copy that is not the encrypted file is made again from it). `sudo ti-secrets
status` lists each file as encrypted or not and unlocked or locked, and shows
no secret.

The editor is `$SUDO_EDITOR`, `$VISUAL` or `$EDITOR` if you set one, else
Ubuntu's `editor` (nano, unless you chose another), `nano` or `vi`. It runs
with its home and temporary folder in the in-memory folder, so the swap,
backup and history files an editor keeps are in memory and removed with the
plain text. vim (and vi, nvim) is also given `-n` (no swap file), no viminfo,
and that folder as its `directory`, `backupdir` and `undodir`; nano keeps no
backup unless told to (`-B`, or `set backup` in a nanorc file), so it is given
`--ignorercfiles`, which keeps any nanorc from turning backups or history on.
Another editor is safe only if it keeps its files in its home
folder, its temporary folder or beside the file: do not point it at a folder
of your own for backups. Core dumps are off for the command and the editor.

If an edit is cut short after the new encrypted file is written (a crash, a
lost connection), the file's unlocked copy is missing, never the old value:
its units refuse to start until `sudo ti-unlock`, or the same edit quit
without a change, makes it again. If a command is killed before it cleans
up, it can leave an encrypted temporary file beside the file, named
`.<name>.gpg.tmp.` and six characters (for example
`/etc/trend-investing/.secrets.env.gpg.tmp.Ab12Cd`): the next `ti-secrets`
or `ti-unlock` removes it and names it, and `sudo ti-secrets passphrase`
refuses while one is still there, so no copy under an old passphrase
outlives a change. Only one `ti-secrets` or `ti-unlock` runs
at a time; a second says "another ti-secrets or ti-unlock is running" and
changes nothing (an edit holds that lock until you quit the editor).

## Unlocking the secrets after a boot

**After every reboot, run `sudo ti-unlock` and type the passphrase.** It
decrypts every secret file into memory and then starts the Gateway, which
sends its IBKR Mobile prompt: approve it on your phone. A wrong passphrase
unlocks nothing and changes nothing; just run it again.

**If you forget to unlock:** every unit that needs a secret refuses to start:
`sudo systemctl start <unit>` says "Assertion failed on job for <unit>", and
`journalctl -u <unit>` says "Starting requested but asserts failed" (the
unit asserts its decrypted file, `AssertPathExists=/run/trend-investing/secrets/...`,
which `systemctl cat <unit>` shows). The Gateway stays down; the engines and the adapter cannot be
started; the data job and the Flex reader skip their firings. The reporter
cannot run either, so it cannot page you itself, but its pings to the outside
heartbeat stop, and about 20 minutes after the boot Healthchecks pages you
through Pushover ("The outside heartbeat", below). Unlock, and the next
firing of each timer runs normally; the data job's next firing catches up
every session it missed.

**Changing the passphrase:** `sudo ti-secrets passphrase`. It asks for the
current one, then the new one twice (at least 12 characters). It writes a new
copy of every file under the new passphrase and checks each by decrypting
it, and only then renames them over the old files, one after another. The
old passphrase then opens nothing. A journal,
`/etc/trend-investing/.passphrase-change` (file names only, no secret),
records each step.

**If a passphrase change is cut short** (a crash or a lost connection during
the renames), some files are under the old passphrase and some under the
new. `ti-unlock`, `edit` and `migrate` then refuse, and `status` says so. Run
`sudo ti-secrets passphrase --resume`: it asks for the old passphrase, then
the new one twice, opens each file with whichever it is under, and finishes
the change, so every file is under the new one. A change that stops with an
error while the new copies are being written and checked has renamed no file
and removes its journal. From the moment the renames begin the journal
stays, even before the first file is renamed; and a change stopped abruptly
(a crash, a power loss, the command killed) can leave the journal at any
point, before any rename included. Whenever the journal is there, run
`--resume`.

**Never lose the passphrase.** It is stored nowhere, on the server or
anywhere else, and there is no recovery: without it the encrypted files can
never be read, and you would have to fill every secret in again (the IBKR
login, the account numbers, the Massive key, the alert keys, the Flex token).
Keep it in your password manager, and never put it in a file, a command line,
an issue or a chat.

`sudo ti-secrets lock` removes the decrypted copies without a reboot; a
program already running keeps the copy it was handed until it stops.

**Moving from the earlier plain-text files.** `sudo deploy/provision.sh
services` finds any secret file still in plain text, encrypts it, checks the
encrypted copy decrypts to the same text, and only then shreds the plain
text. The first time, it asks you to choose the passphrase. If a plain-text
file and its encrypted copy are both there and differ (a migration cut
short, or a file put back by hand), it stops and changes nothing: `sudo
ti-secrets diff <name>` shows how they differ, then keep the right one as
the message says and run it again. It prints both, secrets included, so it
refuses unless its output is the terminal (not redirected to a file and not
piped). It exits as `diff` does: 0 when they are the same, 1 when they
differ, and 2 or more when the comparison itself failed, which it says.

**Shredding is not a guarantee.** On a journaling file system (Ubuntu's
ext4) on a cloud volume, which is SSD and copy-on-write storage underneath,
`shred` may overwrite new blocks while the old ones, holding the plain text,
stay on the volume, and so in every later snapshot of it. The real
protection is that, after the migration, plain text never touches the disk
again: it lives only in memory. So, **on a server provisioned before the
secrets were encrypted** (one whose secret files were once on its disk in
plain text), delete every Lightsail snapshot taken before the migration,
since they hold the plain text, and rotate the secrets that were on the disk
in plain text (the IBKR password, the Massive key, the Pushover token and user key,
the PagerDuty routing key, the GitHub token, the Flex token), since anyone who later gets the
volume or a snapshot of it may read the old values. A server provisioned
after this change never had them in plain text and needs neither step.

**Swap must stay off.** The in-memory folder is mounted `noswap` (Ubuntu
24.04's kernel has it), so the decrypted files are never paged to disk; if
that mount fails, nothing is decrypted. But the passphrase and the plain
text also pass through the memory of `ti-secrets`, gpg and the editor, which
the kernel can page to a swap file or partition on the disk. So while any
swap is on, `ti-unlock` and `ti-secrets unlock`, `edit`, `passphrase`,
`migrate` and `diff` refuse before they ask for the passphrase, name the
swap and change nothing (`status` and `lock` still run). A Lightsail Ubuntu
instance has no swap unless you add some.

To check: `swapon --show` prints nothing when swap is off (or `cat
/proc/swaps` lists only its heading line). To turn it off: `sudo swapoff
-a`, then remove the swap line from `/etc/fstab` (on Ubuntu often
`/swap.img none swap sw 0 0`) so it stays off after a reboot, and remove the
file (`sudo rm /swap.img`), which can hold pages from before. If
`/proc/swaps` cannot be read, the same commands refuse too.



## Your own ntfy server

The daily summary carries your balances, so it goes to a push server that
only you can reach: ntfy, running on the Lightsail box, listening only on the
server's Tailscale address. Pushover never gets the summary (a page means an
incident), and the public ntfy.sh relay never does either, because anyone who
learns a topic name can read it there. Incident alerts also go to this server
when `ntfy` is in the reporter's channels.

**Setting it up (once, after Tailscale works):**

1. On the server: `sudo deploy/provision.sh ntfy`. It downloads the pinned
   ntfy release, checks its SHA-256 and stops on a mismatch, installs it,
   makes the `ti-ntfy` user, writes `/etc/ntfy/server.yml` so the server
   listens on this machine's Tailscale address only (it refuses any address
   outside Tailscale's `100.64.0.0/10`, so a slip cannot open it to the
   internet), then enables and starts `ti-ntfy.service`. If Tailscale is not
   signed in yet it installs the program, says it is waiting, and writes
   nothing else: sign in (`sudo tailscale up --ssh`) and run it again.
2. It prints the line for the secrets file, `TI_NTFY_SERVER=http://100.x.y.z:2586`.
   Put that line in `secrets.env` (`sudo ti-secrets edit secrets.env`), with an
   unguessable `TI_NTFY_TOPIC=` beside it. The topic is the only secret the
   server uses: the server asks for no sign-in, because nothing but your own
   tailnet reaches it.
3. On your phone: install Tailscale, sign in to the same tailnet, and keep it
   **on all the time**: on an iPhone, turn on "VPN On Demand" in the
   Tailscale app's settings; on Android, turn on "Always-on VPN" for Tailscale
   in the system's VPN settings. The phone fetches every message from your
   server over Tailscale, so with Tailscale off it shows nothing new from
   this server, the daily summary included.
4. In the ntfy app, subscribe to that topic on that server (the app's "use
   another server" option, with the same URL). On an iPhone, also set the
   app's Default Server to that same exact URL: ntfy delivers only when the
   two match.
5. **Test a delivery end to end** before you rely on it. In the ntfy app,
   also subscribe to a throwaway topic on the same server, say `check-1234`
   (not your secret topic). On the server run
   `curl -d "test from the server" http://100.x.y.z:2586/check-1234` with your
   server's address. The phone must show the notification within a few
   seconds, with the phone locked and on mobile data. Then unsubscribe from
   the throwaway topic. If nothing arrives, see "What can go wrong" below.

How the phone is woken. The iOS ntfy app cannot keep a connection open to
your server in the background, so `provision.sh ntfy` sets the server's
`upstream-base-url` to `https://ntfy.sh`: for each message your server asks
ntfy.sh to wake the app. That request carries only the message's ID and a
scrambled (SHA-256) form of the topic's address: **never the message, its
title or the topic's name**. The woken app then fetches the message itself
from your server over Tailscale. The server still listens on the tailnet
only; nothing reaches it from the internet. (Android's app keeps its own
connection and does not need the upstream.)

The next reporter firing (within 5 minutes) sends the day's summary once the
log records that day's session closed. A message the server has taken counts
as delivered to the reporter: whether the phone then shows it depends on the
phone keeping Tailscale on (step 3).

Checking it: `systemctl status ti-ntfy`, and `journalctl -u ti-ntfy`. The
server keeps messages for 12 hours, so a phone that was off the tailnet for a
few hours fetches what it missed when it returns; one off for longer misses
the messages older than 12 hours. Incident alerts go to Pushover as well, so
they do not depend on this server. It keeps nothing else and holds no
account.

**Without it** the daily summary is not sent, and the reporter says so once,
as a routine message on the incident channels and in its output; incident
alerts are unaffected. Read the summary on the dashboard
meanwhile ([alerts and dashboard](alerts-and-dashboard.md), "Where alerts
go").

## The pager

Urgent alerts reach your phone through Pushover. It is a one-time purchase of
the Pushover app (no subscription), and an **emergency** message, which is
what every page is sent as, rings again every 60 seconds until you
acknowledge it in the app or 30 minutes pass. It replaced PagerDuty, whose
sign-up refuses personal email addresses. The reporter's `pagerduty` channel is
still in the program but the server's units no longer name it. What is sent,
and the rules for repeats, are in [alerts and dashboard](alerts-and-dashboard.md),
"Where alerts go".

**Set it up (once) [OWNER]:**

1. Sign up at pushover.net and install the Pushover app on your iPhone (the
   purchase is made in the app, after its free trial). Log in to it.
2. On the iPhone, when the app or iOS asks whether Pushover may send
   **Critical Alerts**, allow it. A critical alert sounds even when the phone is
   silenced or on Do Not Disturb, which is what an emergency page needs; in
   Settings, Notifications, Pushover, confirm that Critical Alerts is on.
3. On pushover.net, open **Create an Application/API Token**, name the
   application **Trend server**, and create it. Its **API Token** is
   `TI_PUSHOVER_APP_TOKEN`.
4. On the pushover.net dashboard, copy **Your User Key**. It is
   `TI_PUSHOVER_USER_KEY`.
5. Put both on the server: `sudo ti-secrets edit secrets.env`, as the lines
   `TI_PUSHOVER_APP_TOKEN=<the token>` and `TI_PUSHOVER_USER_KEY=<the key>`.
   They are secrets: they are never printed or logged. The next firing uses them;
   until both are filled the reporter sets the Pushover channel aside and says
   so, ntfy and the GitHub issues still deliver, and its unit exits non-zero.
6. Test a page. This sends one emergency message that rings for a minute,
   reading the two values from the unlocked file without printing them:

   ```
   sudo sh -c 'f=/run/trend-investing/secrets/secrets.env; t=$(sed -n "s/^TI_PUSHOVER_APP_TOKEN=//p" $f); u=$(sed -n "s/^TI_PUSHOVER_USER_KEY=//p" $f); printf "token=%s&user=%s&priority=2&retry=30&expire=60&title=Trend+server+test&message=A+test+page" "$t" "$u" | curl -sS --data @- https://api.pushover.net/1/messages.json'
   ```

   Pushover answers `{"status":1,...}`. Your phone should ring; acknowledge it
   in the app. If it answers `"status":0`, read its `errors`: the token or the
   user key is wrong.

A page that is not acknowledged on the phone is not the same as one the
journal records as acknowledged: acknowledging in the Pushover app silences that
message only, and the reporter sends the next one at its repeat interval until
the acknowledgement is recorded ([operator](operator.md), "Acknowledging an
alert").

## The reporter before the first run

(The data job, `ti-data`, waits the same way: with no `run.env`, no run log
in the engine's folder and no run stream folder in its own output, its firing logs `ti-data: waiting for the account's
first run` and succeeds, writing nothing and sending no heartbeat of its own.
With either present, a missing or empty `run.env` is a refusal and the unit
fails.)

On a freshly provisioned server no engine has been started, so there is no
`run.env` and no log to report on. The reporter's firing does not fail then:
it logs one line, `reporter: waiting for the account's first run: ... run.env
is missing or empty (start the engine once)`, and skips only the reading of the
log: there is none. Everything else runs as on any firing. The channels'
secrets are checked, so an empty one is set aside, named and fails the unit;
the trading units are checked, and a failed one is paged and holds back the
heartbeat; the host's clock is checked; and an earlier failure page of the
reporter's own is resolved. When nothing is wrong it pings the outside
heartbeat ("The outside heartbeat", below), because the reporter is alive and
the server is up; there is simply nothing to report yet, and a check that went
down here would page you for a server that is well.
The timer keeps firing every 5 minutes; the first firing after you write
`run.env` (`docs/runbooks/durable-log.md`, "Restarting an engine") reads the
engine's log as usual.

From then on a `run.env` that names a log that is missing or unreadable is a
fault as before: the firing fails, the failure is paged, and no heartbeat is
sent. A `run.env` that exists but cannot be read, is not a regular file or is
over 64 KiB is also a fault, and so is a link to nothing: only a path with nothing at it counts as missing.

The wait is for an account that has never run, and only for that. The reporter
keeps its own record of a run: the first time it reads a journal named by
`run.env` it writes a small marker file, `<account>.ran` (`boost.ran`), in
`/var/lib/trend-investing/reports`, beside its state file, and the program never
removes it. If `run.env` is missing or empty and any sign of a run exists (that
marker, or a `.log` file in the engine's folder), or the engine's folder exists
but cannot be listed, the firing sends an urgent alert, "the run's settings
file is missing", fails, and sends no heartbeat ping. A stray `.log` file that
is later deleted leaves nothing behind: the next firing waits again. The
firing waits only when there is no marker and the engine's folder either does
not exist or exists, can be listed and holds no `.log` file; anything else is
the alert. The reporter needs `-account` whenever `-run-env` is given and
refuses to start without it (the shipped unit passes it). An account that ran
before the marker existed gets its marker at its next firing that reads its
journal, and its `.log` file covers it until then. The data job refuses when a `.log` file is in the engine's folder or the
job's own output already holds a run's stream folder
(`/var/lib/trend-investing/data/streams/boost/<run>`); it does not read the
reporter's marker. Restore `run.env` as the runbook describes
(`docs/runbooks/durable-log.md`, "Restarting an engine"). The secrets
must still be unlocked and filled in: a firing without its secrets file fails
and sends no ping.

## The outside heartbeat

A reporter on the server cannot page you that the server died, so the
Boost reporter also pings an outside service, Healthchecks, at the end of every
firing that finishes without a fault (every 5 minutes, around the clock),
including the firings that wait for the account's first run. The
service pages you through Pushover when the pings stop. You decided this is
required before paper trading.

What is sent: an empty request to your check's address, with nothing in it:
no balance, account number, alert text or secret. The address is the check's
key, so it lives in `secrets.env` as `TI_HEARTBEAT_URL` and is never
printed or logged. A ping that fails never fails the reporter's run: the
reporter logs one line ("the heartbeat ping was not sent: ...", without the
address) and pings again at the next firing. No ping is sent for a firing that
failed, so a reporter that cannot deliver, or cannot read the log, also goes
quiet and is noticed from outside; nor for a firing that found the engine or
the adapter failed, even when its page was delivered, so a trading unit left
failed for 20 minutes also takes the check down.

Only `ti-reporter@boost` pings this check. The reporter reads
`TI_HEARTBEAT_URL` from the private credential file (`%d/secrets`), never
prints it, and needs no Boost drop-in. Provisioning writes both
`ti-reporter@trad-ira.service.d/heartbeat.conf` and
`ti-reporter@roth-ira.service.d/heartbeat.conf`, each with
`Environment=TI_HEARTBEAT=off`: a second pinger would mask the loss of the
first. This switch suppresses the ping even with a URL in the credential.
The instance logs `reporter: heartbeat: not this instance` once per firing,
as information, never an alert. The stop-Boost-timer drill below therefore
still pages even while the IRA reporter is healthy.

**Set it up (once) [OWNER]:**

1. Sign up at healthchecks.io. The free plan is enough for one check.
2. Add a check named `trend-investing-reporter` with a **period of 5
   minutes** (the reporter's timer) and a **grace time of 15 minutes**, so
   the check is down 20 minutes after the last ping, on the fourth missed
   firing, and one slow firing does not page you.
3. Copy the check's ping address (`https://hc-ping.com/<uuid>`) and put it on the
   server: `sudo ti-secrets edit secrets.env`, as the line
   `TI_HEARTBEAT_URL=<the address>`. The next firing uses it.
4. In Healthchecks, under Integrations, add **Pushover** (it sends you to
   Pushover to authorise Healthchecks for your account). Set the priority for
   **"down" to Emergency**, so the page rings until you acknowledge it, and
   leave the "up" priority at a normal one. Tick the integration on the check,
   and use its test button once to hear the page.
5. Test it: `sudo systemctl stop ti-reporter@boost.timer`. After about
   20 minutes (the 5-minute period plus the 15 minutes' grace) Pushover
   pages you that the check is down. Then `sudo systemctl start
   ti-reporter@boost.timer`: the next firing pings and the check recovers.

How to tell it from an engine alert: a heartbeat page comes from
Healthchecks, names your check, and says nothing about an account or an
engine. It means the reporter has not finished a clean firing for 20 minutes,
for one of four reasons: the server is down or unreachable, the timer is
stopped, every firing is failing, or the engine or the adapter has been failed
for 20 minutes (a firing that finds a trading unit failed sends no ping, even
when it delivered everything). The last is the likeliest: one stopped engine
gives you two pages for one fault, first `<host>: <unit> failed` from the
reporter, then, 20 minutes later, this one from Healthchecks. Act on the unit
page ("A failed unit" below); the Healthchecks page clears by itself at the
first clean firing after the unit is running again. If you have no unit page,
check the server, the timer and the reporter's firings (`journalctl -u
ti-reporter@boost`). Every page the reporter itself sends names an account or a
unit.

## Where the records live

| Folder | What is in it |
| --- | --- |
| `/var/lib/trend-investing/data/` | the data step's recordings (never overwritten), `streams/boost/<TI_RUN>/` (the Boost engine's daily input files) and, when you need it, `quarantine/` |
| `/var/lib/trend-investing/engines/<instance>/` | each run's durable log (`<TI_RUN>.log`) and end-of-run journal (`<TI_RUN>.journal`), `run.env`, your approval files, `quarantine/` for a log set aside, `operator/`, the operator inbox the provisioner creates (`ti-operator:ti-trader`, `0750`; [operator](operator.md), "Who can write an instruction"), and `operator-notes/`, the engine's own notes on it |
| `/var/lib/trend-investing/reports/` | the reporter's own state file, `<instance>.reporter.json` (the only folder its unit may write) |
| `/var/lib/trend-investing/adapters/<instance>/` | the adapter's pending file (`pending.json`) and the files beside it (the only folder its unit may write) |
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

## Memory

The server has 2 GB and no swap by design, shared by the IB Gateway (the
largest), the dashboard, the engine and the data job. The data job keeps only
the configured instrument's bars from each recorded session and holds at most one
day's recordings (two when both the grouped and universe recordings exist),
so its peak is about 53 MB whole-process (about 10 MB of
Python objects; about 59 MB and 16 MB for days with both recordings) whether it is the warm-up (two years, about 456 sessions) or
a daily firing, measured on synthetic sessions of 10,000 instruments; the
budget is 100 MB ([market data](market-data.md), "Memory the data job
needs"). If a job is ever killed for memory anyway (`journalctl -k | grep -i
"out of memory"`), pause the Gateway and the dashboard around the warm-up:
`sudo systemctl stop ti-gateway ti-dashboard`, run the warm-up, then `sudo
systemctl start ti-gateway ti-dashboard` (the Gateway logs in again and may
ask for the phone approval).

## Measuring the server

Boost lesson L1 requires a week of evidence with the Gateway, all three
engines and adapters, data jobs, reporters and dashboard sharing the 2 GB
server before the IRAs go live. This measures the existing server; the owner
decides whether its figures require a larger instance (ADR 0026). Run the
accounts in dry-run; the paper and restart gates remain separate.

`deploy/memory.py` uses only Python's standard library and `systemctl`.
`ti-memory.timer` samples every five minutes, including weekends, into
`/var/lib/trend-investing/memory/<week-start>.jsonl`. Provisioning installs
the script, units, directory and `MemoryAccounting=yes` service drop-ins;
it **does not enable the measurement timer** or choose a week. Existing
evidence is append-only. Keep each week's JSONL and report, failed weeks
included; never truncate or replace one to obtain a passing report.

For an existing server, install by hand from the staged repository root:

```bash
sudo install -m 0755 deploy/memory.py /usr/local/libexec/trend-investing/memory.py
sudo install -m 0644 deploy/systemd/ti-memory.service deploy/systemd/ti-memory.timer /etc/systemd/system/
sudo install -d -o ti-trader -g ti-trader -m 0750 /var/lib/trend-investing/memory
for file in deploy/systemd/*.service; do
  unit=$(basename "$file")
  sudo install -d -m 0755 "/etc/systemd/system/$unit.d"
  printf '[Service]\nMemoryAccounting=yes\n' | sudo tee "/etc/systemd/system/$unit.d/memory.conf" >/dev/null
done
sudo systemctl daemon-reload
```

Choose the first of seven **UTC dates** in `memory.env`. An arbitrary start
date is allowed. Start at its midnight for the fullest coverage; starting
later on that date records a partial first day explicitly. For example,
replace `YYYY-MM-DD` below with the chosen date. This file holds no secret:

```bash
printf 'TI_MEMORY_WEEK_START=YYYY-MM-DD\n' | sudo tee /etc/trend-investing/memory.env >/dev/null
sudo chmod 0644 /etc/trend-investing/memory.env
sudo systemctl enable --now ti-memory.timer
sudo systemctl start ti-memory.service
sudo systemctl status ti-memory.service --no-pager
sudo systemctl list-timers ti-memory.timer --no-pager
```

On that same day, after the selected UTC week has begun, check that the
JSONL has a new record and check the `active_state` of all three engines,
all three adapters and the Gateway. Missing/inactive services are recorded,
not assumed to be running. An active unit with unavailable counters fails
the sample and pages through `OnFailure`; check `MemoryAccounting` and, for
an already running unit, use `sudo systemctl set-property --runtime <unit>
MemoryAccounting=yes`, then retry. The sampler records all loaded `ti-*`
services plus an explicit list of the accounts' services and the Gateway,
so even an inactive or unloaded required unit appears.

The record includes each unit's `MemoryCurrent`, `MemoryPeak`, active/load
state, the server's `MemTotal` and `MemAvailable` from `/proc/meminfo`, and
the installed timer text including instance drop-ins. Schedule changes
during the week remain visible in the report's `schedules`: each entry records
`since` (UTC), the installed `schedule` text and the number of `samples`
taken under it. A change and a return to an earlier schedule are separate
entries in timestamp order. The Flex
starts are Boost 05:30/06:30/07:30, Traditional IRA 05:45/06:45/07:45 and
Roth IRA 06:00/07:00/08:00 New York. Roth data runs at
06:30/07:30/08:30, thirty minutes after its Flex starts, before the 09:00
proof cutoff. The stagger separates starts; a slow Flex request can outlive
its fifteen-minute window, and each adapter still requires the executions
proof (Boost lesson L3).

After the seven dates, substitute the same start date and save the report:

```bash
sudo systemctl disable --now ti-memory.timer
sudo -u ti-trader python3 /usr/local/libexec/trend-investing/memory.py report \
  --week-start YYYY-MM-DD --record /var/lib/trend-investing/memory/YYYY-MM-DD.jsonl
```

Copy stdout to a new report file and keep it beside the JSONL. The sampler
appends nothing outside the selected week even if the owner leaves its
timer enabled. The report exits non-zero with `memory: FAIL: missing samples
for <date>` if any UTC day has no sample; malformed/truncated records also
fail. It states counts per day (288 expected), first/last observations and
the largest gap: a day with one observation meets the missing-day check,
but is visibly incomplete and is not enough evidence for a capacity decision.

All figures are bytes. Typical use is the median of available
`MemoryCurrent` observations per unit. `peak_bytes` includes the largest
cgroup `MemoryPeak` seen: this is a lifetime high-water mark and can predate
the week, and a short-lived job can finish between samples without leaving
a readable counter. `sampled_peak_bytes` is the highest observed
`MemoryCurrent`; neither claims to capture every short spike. Unavailable
counters are null with observation counts and unit states, never invented
zeros. Server peak and median use `MemTotal - MemAvailable` at each sample;
they include the whole host, and are never a sum of non-simultaneous unit
peaks. Compare these figures and coverage with `capacity_bytes`, and retain
the schedule under which they were taken before making the owner's sizing
decision.

## The clock

The program compares the server's clock with IBKR's. If the server's clock
runs behind IBKR's, the account reading looks as if it comes from the future,
the engine treats it as stale (ADR 0019 allows no tolerance for that), and it
blocks every sell until the clock is right. A drifting clock therefore shows
up first as a withheld sale, so the server is set up to keep its clock right
from the start.

`sudo deploy/provision.sh clock` (also part of `all`) makes sure a time
service is running and then runs the check, and it refuses to finish if the
check fails. A time service the server already has (chrony, ntp, ntpsec, openntpd or
`systemd-timesyncd`) is kept, turned on for the next boot and, if it was
installed but stopped, started; if it will not start the step refuses and names
it, so you can start or remove it yourself. The one exception is
`systemd-timesyncd` inside a container: there a failure to start is logged and
the outside check below decides, while any other daemon that will not start
still refuses (on a server of its own, `systemd-timesyncd` refuses too). Only when none is installed is
`systemd-timesyncd` installed and turned on. On a server of its own the clock must be reported
synchronised by `timedatectl`. Inside the Mac mini's container the Mac keeps
time and the container usually shows no time service of its own, so there the
check compares the clock with the time an outside website reports, and fails
if they differ by more than 2 seconds. It reads the local clock before and
after the request and compares the website's time with the midpoint, so a slow
connection does not count against the clock; a request that takes longer than
2 seconds (`TI_CLOCK_MAX_ROUNDTRIP_SECONDS`) is thrown away and the next
website tried. Only HTTPS websites are used. If a website's cache says its
time is old (an `Age` header, common behind a CDN), that many seconds are added
back, but only when `Age` is a whole number up to an hour; any other `Age`
makes that website unusable. A fresh answer has no `Age`, or an `Age` of 0, and
nothing is added. The limit can be changed with
`TI_CLOCK_TOLERANCE_SECONDS`. On a slow home link every website may take more
than 2 seconds; `sudo` drops environment variables, so raise the limit in the
file `/etc/trend-investing/clock.conf`, which the check reads (the clock step
creates the folder `/etc/trend-investing`, owned by root, if it is missing, so
this works on a fresh server; the file should be owned by root and readable by
all, which `tee` gives you):
`echo TI_CLOCK_MAX_ROUNDTRIP_SECONDS=5 | sudo tee -a /etc/trend-investing/clock.conf`.
`tee -a` appends, so lines already in the file are kept; if a key is written
twice, the last line for it wins.
The tolerance can be set there too, as `TI_CLOCK_TOLERANCE_SECONDS`, but only
to a whole number of seconds from 1 to 60; anything else is refused. A value set in the
environment of a run by hand overrides the file, and a value that is not a
positive number is refused. If the container cannot
reach any reference, that also fails, because a clock that could not be
checked is not a checked clock. On the Mac mini, a failure means the Mac's own
"Set time and date automatically" is off or wrong.

When the server reports "synchronised" inside a container, that is the
report of the kernel the container runs on. On a Mac that is the Linux virtual
machine that Docker runs, not macOS itself, so the report says nothing
about the Mac's own clock; it is accepted because the container shares that
virtual machine's clock. To check that claim against an outside website too, run the
check with `TI_CLOCK_VERIFY=1` in front of it.

The `clock` step installs the check before it runs it, so you can run it
yourself at any time after that step (even if the step failed), and after any sale is withheld with the
reason `stale-broker-snapshot`:

```
/usr/local/libexec/trend-investing/clock-check.sh
```

It prints `clock: ok ...` or says what is wrong and exits with an error. The
reporter runs this same check at every firing (every 5 minutes) and pages you
once for the whole server when two checks in a row fail, with the skew it
measured and the remedy, and resolves the page when a check passes; the daily summary
ends with the last check's time and result ([alerts and
dashboard](alerts-and-dashboard.md), "Alerts this chapter explains"). Running it
by hand is how you confirm the fix.

## The Gateway is logged out

IB Gateway can sit logged out of IBKR while its unit still shows `active`:
the nightly restart at 11:45 PM logs in again, IBKR sometimes answers a login
that worked minutes before with "Unrecognized Username or Password", and IBC
leaves that dialog open for hours. Nothing reaches IBKR meanwhile, and no unit
has failed to tell you. Two things now cover it.

**The reporter distinguishes logout from connectivity loss.** The owning
reporter reads the Gateway and adapter journals every five minutes. A refused
login, login dialog or stopped retries means logged out. A completed login
followed only by 1100/2110 means IBKR connectivity lost; it does not mean a bad
password. A plain API reconnect proves nothing; 1102 (also 1101 restoration)
or successful reconciliation ends the connectivity incident. Missing journal
access fails the firing. Rotated evidence does not prove recovery. An inactive
adapter or link evidence from before its last start does not establish a current
loss; the Gateway’s own login state then decides.

Both are **operational**, following the [paging invariant](alerts-and-dashboard.md#paging-invariant):
ntfy and an alerts issue first, Pushover after `TI_ESCALATE_AFTER` (default 30m)
if still open, immediately near a session once detection grace has passed.
A connectivity loss is observed at the next firing; there is no three-hour blind interval. Outside session-near,
the shared IBKR schedule holds Pushover until its window ends and the delay
then expires. If loss outlasts maintenance, the notice asks for
`sudo systemctl restart ti-gateway`; the reporter never runs that command.

The newest `gateway-start:` line being `gateway-start: reset wait: login refused at`
produces a reset-wait notice only on the night it was written, from 23:40 to
that night’s first 01:00 ET, marked scheduled IBKR interruption,
with no Pushover. At 01:00 ET, or after another start state, ordinary logout
handling resumes. An `interrupted by stop` line ends the wait. Both logout
and connectivity loss retain the ten-minute detection grace,
counted after the restart windows in `/etc/trend-investing/gateway.env`:
five minutes before to 45 minutes after the nightly restart (23:40–00:30 by
default), and five minutes before to 30 minutes after the Sunday cold restart
(08:55–09:30). The IBKR schedule is a separate paging rule. The incident record
is `host-gateway.json` in `/var/lib/trend-investing/reports/`.

Install the non-secret policy for **both** the timer and the unit-failure hook:

```sh
sudo install -m 0644 deploy/etc/reporter.env.example /etc/trend-investing/reporter.env
# Install the shared adapter/reporter schedule only if it is not already configured:
sudo test -e /etc/trend-investing/ibkr-maintenance.json || sudo install -m 0644 deploy/etc/ibkr-maintenance.json.example /etc/trend-investing/ibkr-maintenance.json
```

Edit `TI_ESCALATE_AFTER` in `reporter.env`; `0` preserves immediate paging
outside scheduled windows. Both entry points read the file on each invocation,
so no systemd unit edit or daemon reload is needed. The process environment
can override the file; `TI_REPORTER_CONFIG` selects another policy file and
`TI_IBKR_MAINTENANCE_FILE` selects another schedule. Do not put credentials in
this file. A malformed schedule or delay fails the firing and disables quieting
for that firing, preserving immediate safety delivery. A missing schedule
means no windows. The shipped example gives daily 23:40–01:00,
Saturday 00:00–24:00 and Sunday 00:00–14:00 America/New_York, merged into
one weekend window from Friday 23:40 to Sunday 14:00. Retain owner edits when upgrading.

**The retry is bounded.** IBC 3.24.2 has no setting for a refused login: its
user guide and `config.ini` cover only the second-factor timeouts and
`LoginDialogDisplayTimeout`. The Gateway's start program (`ti-gateway-start`, run by `start-gateway.sh`) reads IBC's output for
the refused-login dialog. Between **23:40 and 01:00 America/New_York**, a
refusal can be treated as IBKR's nightly session reset, usually around 00:10 ET
and sometimes longer on Friday nights. There are **at most 4 uncounted reset refusals in any rolling 24 hours**,
then the **three counted refusals** stop retries: at most seven refused attempts in any rolling 24 hours
without an accepted login, below the assumed lockout at ten. The counted limit
can be lowered, but cannot be configured above three. The fifth reset refusal
counts even before 01:00 ET. The repeated 01:00 hour on the autumn DST night is
outside the reset window; the cutoff is the first 01:00 America/New_York.

Before cleanup or journal output, the window file records the reset wait's
end time and the timestamps of consumed reset allowances, retaining the
provisional attempt. Allowances expire individually after 24 elapsed hours;
a new calendar night, an idle unit, or either DST change never renews them.
A header written by an earlier build, without the timestamps, is refused as
malformed (exit 77): delete `/var/lib/ti-gateway/login-failures` and start the
unit; nothing is migrated.
The program kills IBC's own process group and polls until it is gone, with a
bounded deadline, before draining output or relaunching. If the group cannot be
confirmed gone, retries stop with 77 and the attempt stays counted. Only when
returning the reset outcome does it remove that start's provisional attempt;
older attempts stay counted. It waits up to 15 minutes, capped at 01:00 ET.
Before sleeping, the journal reports the actual remaining duration and deadline:
"gateway-start: reset wait: login refused at HH:MM ET inside IBKR's nightly reset hour;
waiting Nm until HH:MM ET (consumed reset allowances M of 4 in 24h)". At 00:58 this is 2m until
01:00 ET, not 15 minutes. Durations round up to whole minutes, or whole seconds
under a minute; deadlines include seconds when needed. Cleanup that crosses
the cutoff reports 0s; the
next start is counted. A stop during the wait logs
"gateway-start: reset wait: interrupted by stop". A restart or crash **resumes the remaining wait**
before another IBC launch; a crash before the final discount keeps the attempt
counted as well. The label reports consumed reset allowances, since an
interruption or crash can leave both an allowance consumed and its attempt
counted. Journal errors and output drain timeouts cannot skip the wait. A
permanent safety stop (exit 77, including untrusted IBC output) outranks a reset
retry and retains the attempt; any persisted cooldown remains on disk.
A persisted wait is honoured only when its refusal lies in a 23:40–01:00 ET
night and its deadline is the 15-minute-or-cutoff one the rule would have set.
A refusal up to two minutes ahead of the clock (an ordinary time correction)
keeps its deadline, which can only lengthen the wait; one further ahead, an
inconsistent pair, a history that does not account for its refusal, or a
clock that has stepped back past a recorded refusal while IBC ran, all stop
the unit with exit 77 and leave the file for you to inspect: correct the clock
or delete the file, then start the unit.
An accepted login clears counted attempts, but preserves rolling reset
allowances. The unit is
`Type=simple`, with no explicit `TimeoutStartSec` or watchdog: its start is
complete before the wait, so no readiness notification or timeout change is
needed. This changes the retry bound's treatment of resets; the separate
logged-out page timing above still applies.

Outside that reset window, on the first refusal of a run it stops its own
`xvfb-run` process (SIGTERM, then SIGKILL after 15 seconds; systemd ends the
rest of the unit's processes when the program exits) and lets systemd start it
again 30 seconds later. IBKR's documentation says "After ten failed login
attempts in 24 hours, your Portal log in will be disabled" (IBKR Guides, "Log
In and Log Out", ibkrguides.com/clientportal/gettingstarted/login.htm; the
FAQ "Troubleshooting client portal login failures", article 1132, says the
same). That is the Client Portal login; IBKR publishes no separate figure for
the Gateway's, so the program assumes the same. It counts attempts: every
start of IBC is one, written to disk before IBC is launched, whether or not IBC
got as far as a login (a start that fails, such as a missing `xvfb-run`, costs
one too); the nightly reset exception above removes that start's attempt
only after confirmed cleanup, when returning the reset outcome. A completed login, or IBKR's prompt for the second factor (which
means the password was accepted), clears the count; a refusal after such a login
in the same run is a new counted attempt unless a reset exemption remains,
written when it is seen (if the program dies between seeing it and writing it,
the next start counts anyway; this accepted-login relogin edge can add one
refusal outside the pre-launch accounting). The rolling reset allowance is separate:
at most 4 uncounted reset refusals in any rolling 24 hours, then the three counted refusals. Three
attempts (two retries) within 24 hours stop the unit and do not restart it
(exit code 77); a start while 3 still count refuses at once with "login retries
stopped" without touching IBKR. Every actual IBC launch is provisionally counted, including your own
`systemctl restart ti-gateway`, a reboot or a second-factor timeout. Restarting
during a persisted reset wait resumes it without launching IBC or consuming
another attempt. A qualifying reset refusal can discount its launch only as
described above. A counter file it cannot
read, trust (invalid attempt times or reset metadata) or write also refuses, since an unknown
history cannot authorise another try; so do IBC output that ends while IBC
runs and a line over 64 KiB. The count is the file
`/var/lib/ti-gateway/login-failures`. A journal that errs or takes nothing for
30 seconds stops IBC with exit 75, so systemd starts it again: that restart
counts as an attempt unless the durable reset policy applies. A journal wedged
while IBC runs cannot lift either bound; so does an output reader that makes no progress after IBC ends, or that has not
finished 65 seconds (2 x the journal bound + 5 s) after IBC ends: it delivers what
it can within those bounds, then exits 75 (a durable reset outcome takes precedence over restartable failures), and what is left (a late line of an
orphan, lines still buffered) is dropped.
Failed logins you make on IBKR's website, or with the wrong paper password
elsewhere, count against IBKR's 10 too, which is why the program stops at 3.
A start whose IBC command or display wrapper cannot be launched exits 70, which
systemd retries every 30 seconds until the 3 are used (it counts). A stop signal interrupts even a blocked renderer, with bounded cleanup of
its own process group before the supervisor exits. A renderer
that cannot run, or whose output the journal will not take, stops the start
before any attempt is counted (exit 70, or 75 for the journal): systemd retries
it every 30 seconds without end and it never reaches 77. A start that keeps
failing is seen through systemd's restart count: every firing the reporter asks
systemd for `ti-gateway.service`'s restart count and state (`systemctl show`),
and keeps a baseline: the restart count when the unit was last seen quiet (active,
with the count unchanged) at two firings in a row. It counts the Gateway as
logged out when the count stands 3 or more above that baseline: a
loop that restarts every 30 seconds makes about ten a firing, and one that
restarts at least every ten minutes never lets the baseline move. That holds by
construction: the longest cycle of a failing start is 155 seconds with the
default settings, and `TI_GATEWAY_SINK_TIMEOUT` and `TI_GATEWAY_STOP_DEADLINE`
are capped at 60 seconds, which keeps the worst cycle under six minutes. A fall in the count (a start
by hand, `reset-failed`) re-bases and counts as neither. The usual 10 minutes
outside the restart windows then page you. Such an outage closes after two
good firings in a row with the unit active and no new restarts (a firing
where systemd could not be asked breaks the streak); with a silent journal
that is all the reporter can tell, namely that the start loop has ended, not
that IBKR has accepted the login. An outage the journal evidences (a refused
login, a login screen, a lost link) is never closed that way: it needs a
completed login or a restored link newer than its start, and an outage the
restart rule opened that later gains journal evidence is the journal's from
then on. This does not depend on the journal, so the 10-minute promise holds
even when the journal takes nothing; a single restart pages nothing. If
`systemctl` itself fails, a page the journal evidences still goes out and the
failure is the firing's error.
Each such start logs "cannot start IBC", as does every exit that leaves the
unit down without contacting IBKR (the renderer's 64, 65 and 78); the health
check reads that line as logged out when the journal works. The reporter also
samples `ti-gateway.service` as it does the engine and the adapter, so a unit
that is failed and stays down pages at its next firing as a failed unit ("A
failed unit").

**What you do when paged:**

1. Log in to the paper account on IBKR's website. If that is refused, the
   password changed or the login is locked: fix it there (IBKR phones, not
   this program), and `sudo ti-secrets edit ibkr-login.env` if the password
   changed.
2. Never stay logged in anywhere else with the paper username (the app, the
   website, another Gateway): a second session makes IBKR log the Gateway
   out. Log out of those.
3. `sudo rm -f /var/lib/ti-gateway/login-failures` if the page or the
   journal says retries stopped or the count cannot be trusted, then `sudo systemctl restart ti-gateway` and
   approve the phone prompt if one comes. Check `journalctl -u ti-gateway`
   shows `Login has completed`. The page resolves within 5 minutes.

## Closing the Gateway's ports to your other devices

Tailscale lets every device on your tailnet reach every port on the server
unless a rule says otherwise, and the server's own firewall cannot stop that.
The Gateway's API ports (4001 live, 4002 paper) should be reachable from the
server alone. Tailscale's access rules are enforced before traffic reaches the
server, so they close those ports to your Mac, your phone and any device you
add later. Do this before the Read-Only API is turned off (first-paper-order
runbook, B4 step 2): after that, the Gateway's `reject` setting would be the
only guard inside it.

The policy is `deploy/tailscale/policy.hujson`. It lets your own devices reach
each other, and reach the server on three ports only: 22 (SSH), 443 (the
dashboard, which `tailscale serve` publishes) and 2586 (your ntfy server). The
Mac mini (the alert triage job) is tagged `tag:trend-mac-mini` and may reach
443 only. It keeps Tailscale's default SSH rule for your own devices, and adds
one for the tagged server, so `ssh ubuntu@trend-investing-1` keeps working as
ubuntu (the server joined with `tailscale up --ssh`). A device with a tag stops
belonging to a user, so the tagged server is not covered by the rules for your
own devices; it has its own.

The server's public SSH port is closed, so Tailscale is your only way in. Take
these steps so you cannot lock yourself out:

0. Open the Tailscale admin console in your browser and keep it open, with
   `deploy/tailscale/rescue.hujson` to hand. That is your protection: pasting a
   policy there does not depend on SSH. An open SSH session is not: every login
   to the server is a Tailscale SSH login, and Tailscale ends a session the
   moment a policy change revokes the access that allowed it, so it dies in
   exactly the case it would be meant to cover. Open one only for convenience.
1. In the Tailscale admin console, open **Access controls** and replace the
   policy with the contents of `deploy/tailscale/policy.hujson`. The default
   policy allows everything; replacing it is the point. If you added rules of
   your own, merge them in by hand, and keep nothing that admits
   `tag:trend-server` on any port but 22, 443 and 2586. Save. What changes: the
   allow-everything rule is gone, and the policy now defines the two tags. The
   server is not tagged yet, so it is still one of your own devices: your
   devices still reach it on every port, including 4001 and 4002, and SSH to it
   still works (the default SSH rule is kept). The Gateway's ports are not
   closed until step 2.
2. Open **Machines**, find `trend-investing-1`, choose **Edit ACL tags** and add
   `tag:trend-server`. Save. What changes: the server leaves your own devices
   and the policy's rules for the tag apply to it, so 4001 and 4002 are closed
   to your devices. Its row in the machine list no longer shows you as its
   owner; that is expected. The Mac mini is not tagged yet (the alert triage job
   is not set up), and the policy's lines for `tag:trend-mac-mini` do nothing
   until it is; it is tagged when that job is set up
   (`docs/runbooks/mac-mini.md`, "2. The alert triage job").
3. Open a NEW terminal and check `ssh ubuntu@trend-investing-1` logs you in.
   If it does not, paste
   `deploy/tailscale/rescue.hujson` into Access controls (below) and try again.
4. Still in **Machines**, choose the **...** menu on `trend-investing-1` and
   **Disable key expiry**. Tagging in the console does not change a key's
   expiry: if it was on when the server joined, the server leaves the tailnet on
   that date and SSH, the dashboard and ntfy all go dark with the public door
   closed. Never re-authenticate the server as a user (`tailscale up` signing in
   again): that drops the tag, and the rules above stop applying to it.
5. From the Mac, check each of these:
   - `ssh ubuntu@trend-investing-1` logs you in (in the new terminal).
   - The dashboard address still opens in a browser.
   - The ntfy app on your iPhone still receives a test message (server manual,
     "Your own ntfy server", step 5).
   - `nc -vz -G 5 trend-investing-1 4002` now fails. With Tailscale filtering, a
     blocked port is not refused, it simply times out, so "timed out" is the
     right result. (Before this change it said the connection succeeded.)

**If SSH stops working**, do not restore the previous policy: the default
policy has no SSH rule for a tagged server, so it would not bring SSH back.
Open the admin console's Access controls page, paste the contents of
`deploy/tailscale/rescue.hujson` and save, then try `ssh ubuntu@trend-investing-1`
again. The rescue policy restores SSH and nothing else: it grants the same
ports as `policy.hujson` (22, 443 and 2586 to your devices, 443 to the Mac
mini), so 4001 and 4002 stay closed, and its SSH rules are the most permissive
safe ones (your devices as ubuntu or root, with no re-check on the server).
Paste `policy.hujson` again once you have found the cause.

## What you start by hand

- **Once:** create the instance, join Tailscale (`sudo tailscale up --ssh`),
  close the public door (`provision.sh firewall`), enter the IBKR login and
  the secrets, start the Gateway the first time and approve the phone
  prompt (`docs/runbooks/lightsail-deployment.md`).
- **Every engine start**, after a crash or not: write `run.env`, and for a
  fresh run an approval, then `sudo systemctl start ti-engine@<instance>`
  ([restart](restart.md)).
- **The adapter**, after its engine: `sudo systemctl start ti-adapter-boost`
  ([paper trading](paper-trading.md), "Starting the adapter"); an IRA's, after
  its engine and warm-up: `sudo systemctl start ti-adapter@trad-ira`, then the
  same-day probe ([IBKR adapter](ibkr-adapter.md), "The same-day probe").
- **Once per Boost run:** the data job's warm-up, after the engine has
  started once ([market data](market-data.md), "The one-time warm-up").
- **The first dry-run day, in order:** [the first dry-run day runbook](../runbooks/first-dry-run-day.md).
- **After every boot:** `sudo ti-unlock`, then approve the Gateway's phone
  prompt ("Unlocking the secrets after a boot").
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

Run `sudo ti-unlock` and type the passphrase: until then nothing that needs
a secret starts ("Unlocking the secrets after a boot"). It starts the
Gateway, which sends a phone prompt: approve it. The dashboard starts by
itself; the data job's and the reporter's timers run at their next firing
after the unlock. **Every engine and the adapter stay stopped.**
A reboot is not a crash, so each engine's next start is a fresh run you
approve ([restart](restart.md)). Schedule reboots for a Sunday, after the
cold re-login, so one phone prompt serves both.

## Reading the logs and the kill switch on the server

`journal-query` and `operator` are installed on the server, in
`/usr/local/bin`. Run `journal-query`, which only reads, and `operator
approve` as the engine's user, `sudo -u ti-trader ...`. Run the
instructions (`operator kill-switch`, `rebalance-cleared` and `acknowledge`)
as `ti-operator`, the user that writes them: `sudo -u ti-operator operator
...`. The provisioner creates `ti-operator` (a system user with no login
shell, in the engine's group `ti-trader`) and each account's operator inbox,
`/var/lib/trend-investing/engines/<instance>/operator`, owned by `ti-operator`
in the group `ti-trader`, mode `0750`; the engine unit passes it, so `sudo -u
ti-operator operator kill-switch -inbox
/var/lib/trend-investing/engines/<instance>/operator ...` is read by the engine
before its next input ([operator](operator.md), "The operator inbox"). The
engine reads the inbox through its group and cannot write it, and the
adapter, the data job, the reporter and the dashboard, which also run as
`ti-trader`, cannot reach it (each unit's `InaccessiblePaths=`) and can write
nothing under `/var/lib/trend-investing/engines`, so no unit that reads data
from outside can write an instruction ([operator](operator.md), "Who can
write an instruction"). The engine checks the inbox's owner and mode each
time it starts: it refuses to start if the folder is missing, if its group
or others can write it, if others can read it, if root or the engine's own
user owns it, or if the engine cannot list the folder or read a record in it
(for instance, its group is not one the engine's user is in). So a container or any other host that runs
the engine (the Mac mini's) must create it the same way: owned by whichever
user that host's engine does not run as (on the server, `ti-operator`), in a
group the engine's user is in, mode `0750`. The recorded switch only works while the engine
runs; to stop the program trading outright, also stop it (`sudo systemctl stop
ti-engine@<instance>`). Also stop the adapter
(`sudo systemctl stop ti-adapter-boost`): an order the engine released that the
adapter has not sent is never sent by the fresh run you start next: its adapter
stops at start naming the earlier run's input ("awaits its reply from an
earlier engine run"), which you settle with the relay's `resolve --run-ended`
command, run on the adapter's machine ([paper trading](paper-trading.md), that
row). `--never-sent` is only for an order the adapter refuses before it wrote
"about to send" ([redelivery](redelivery.md)). Cancel anything
already at IBKR in IBKR's own app.
  Do not treat stopping the Gateway (`sudo systemctl stop ti-gateway`) as a
  kill switch: it only delays. An order the engine has released waits in the
  adapter and goes out once the Gateway is back the same day, and an order already at IBKR keeps
  working until you cancel it in IBKR's app.

## A failed unit

The engine, the adapter, the reporter and the data job each name
`ti-unit-failed@%n.service` in `OnFailure=`. When one fails, systemd starts that unit under the failed
unit's name (for the engine, `ti-unit-failed@ti-engine@boost.service.service`;
the rehearsal starts it under a real systemd), which runs `ti-reporter
notify-unit-failed <unit>`: one urgent page on Pushover, your own ntfy and, once its token and repository are filled in, the private alerts repository, naming the unit, the reason (cleaned of addresses, account numbers and
secrets) and the manual row to read. You are paged within a minute of the
stop, not after two sessions of silence.

- One page per failure, not per failed run. Every start of a unit is a new
  run with its own invocation ID (`systemctl show -p InvocationID --value
  <unit>`), which systemd hands the hook, and the page is keyed by the run
  that began the failure. A failure lasts until the unit is seen well: a
  reporter firing finds the engine or the adapter not failed, or, for the
  reporter itself, a firing finishes without a fault. Every run that fails
  before that (a unit you restart that fails again, a reporter that fails on
  each 5-minute firing) is the same failure: it is not paged again and its
  page is not sent again. While it goes
  on, at most once an hour, a reminder `<host>: <unit> still failing` is sent
  (by a later failed run, or by a reporter firing that finds the unit still
  failed), saying since when and, once two or more runs have failed, how
  many, as a reminder of the same failure. A failure after the unit was seen well pages
  again. The record is `unit-failures.json` in
  `/var/lib/trend-investing/reports/`.
- The data job is the exception to reminders and to the heartbeat. Its timer
  runs it again at the next firing, so a failed data job is paged once by the
  hook and never reminded: a retry that fails again is the same failure, with
  no second page. If the hook could not deliver its page (a channel was down),
  the reporter's next firing sends it once; it never reminds. The reporter
  does not name the data job in a state-at-start page. The data job being
  failed never holds back the outside heartbeat (only the engine, the
  adapter or the Gateway being failed does), and a page the reporter cannot deliver for it
  is written to the journal and tried again at the next firing without failing
  the firing, so a data job failure never silences the heartbeat that would
  tell you the server itself went down. The page ends when the reporter's firing
  sees the data job inactive: after a later run ends well (a retry still
  running does not end it), and also after a reboot or `systemctl
  reset-failed ti-data`, which clear the failed state without any run. A run
  that fails after that pages again. Until then no further page comes, so
  after you fix the cause, `sudo systemctl start ti-data` and wait for it to
  finish: that run ending well is what closes the incident. Started on an
  evening it is a quiet wait that writes no stream (market data chapter,
  "The free plan's timing"): the next trading day's morning firing records
  the session, or you run `ti-data --session <date>` once Massive serves it.
- The reason is the failed run's own last journal line. A run that wrote
  nothing (a start systemd refused, such as a missing `run.env`) is explained
  by systemd's own line about that run, such as "Failed to load environment
  files: No such file or directory"; it is never a line from an earlier run.
- The reporter's every firing asks systemd once, at its start, for the engine's
  and the adapter's state and run (`systemctl show`). A failed run that the hook
  did not page (the hook could not reach a channel, the unit failed before the
  hook was installed, or you started a unit that was already failed and
  systemd refused the start, which systemd does not hand to the hook) is paged
  at that firing (unless its failure was already paged), and the
  state-at-start page names every failed trading unit, so a reporter
  restarted after the failure still tells you. The page of a unit that was not
  failed at the firing's start is resolved; a page sent, or a failed run of
  the same failure recorded, after the firing started is left for the next
  firing, so a unit that fails again while a firing runs keeps its failure record
  open.
- A failure of the reporter itself is paged by the hook too, once, however
  many firings fail after it (with the hourly reminder), and resolved by the
  next firing that finishes without a fault.
- If the hook itself fails (`systemctl status ti-unit-failed@<unit>.service`),
  no channel took the page; the reporter's next firing tries again.

## What can go wrong, and what happens then

| What you see | What the program does | What you do |
| --- | --- | --- |
| The server has rebooted | The secrets are locked: every unit that needs one refuses to start; the dashboard starts; every engine and the adapter stay stopped | `sudo ti-unlock`; approve the Gateway's phone prompt; start each engine as a fresh run you approve, then the adapter ([restart](restart.md)) |
| `sudo systemctl start <unit>` says "Assertion failed on job for <unit>", or its journal says "Starting requested but asserts failed"; or Healthchecks pages that the reporter's check is down soon after a boot | The secrets have not been unlocked since the boot. systemd refuses to start the Gateway, the engines, the adapter, the data job, the reporter, the Flex reader and the failed-unit page; the timers keep firing and are refused each time; nothing reaches IBKR | `sudo ti-unlock` ("Unlocking the secrets after a boot"); then start what you start by hand |
| `ti-unlock` (or `ti-secrets`) says "the passphrase is wrong, or ... is damaged; nothing changed" | Nothing is unlocked or changed | Type it again from your password manager. If the right one is refused, the file is damaged: restore it from a snapshot taken after the move to encrypted files |
| `ti-unlock` (or `ti-secrets`) says "swap is on (...)" or "cannot read /proc/swaps" | Nothing is read, decrypted or changed | Turn swap off and keep it off ("Swap must stay off", in "Unlocking the secrets after a boot"), then run it again |
| `ti-secrets passphrase` says "an encrypted temporary file left by a command cut short is still there" | Nothing is changed | Remove the file it names (`sudo rm -r <file>`), then run it again |
| The passphrase is lost | The encrypted files can never be read; there is no recovery | Fill every secret in again: remove the `.gpg` files (`sudo rm /etc/trend-investing/*.gpg /etc/trend-investing/accounts/*.gpg`), run `sudo deploy/provision.sh services`, which asks for a new passphrase, then enter each secret with `sudo ti-secrets edit` |
| `provision.sh services` stops with "verification failed, nothing replaced and no plain text removed", or with "both exist and differ" | The plain-text file is kept and no encrypted copy replaces anything | For the first, run it again; if it keeps failing, see `journalctl` and the disk space. For the second, keep the right one as the message says, then run it again |
| The Sunday phone prompt is missed | IBC exits, the unit starts it again after 30 seconds, and a new prompt arrives each time; nothing reaches IBKR meanwhile | Approve the next prompt (`docs/runbooks/gateway-login.md`) |
| `ti-gateway` failed and stays down (the page `<host>: ti-gateway.service failed`); `journalctl -u ti-gateway` names the login file, or says "login retries stopped" | Exit 64, 65 or 78: the login file is empty, or holds an unknown key or value (for example a trading mode other than `paper` or `live`). Exit 77: the program stopped the retries (3 login attempts within 24 hours without an accepted login, or a counter it could not trust, or IBC output it could not watch), so IBKR cannot lock the login. IBKR's nightly session reset around 00:10 ET (longer on Friday nights) can also refuse a password that just worked. From 23:40 to 01:00 ET the unit waits it out internally: the journal says "gateway-start: reset wait: login refused at HH:MM ET inside IBKR's nightly reset hour; waiting Nm until HH:MM ET (consumed reset allowances M of 4 in 24h)". There are at most 4 uncounted reset refusals in any rolling 24 hours, then the three counted refusals (at most seven in any rolling 24 hours without an accepted login). Restarting resumes the remaining wait; journal failures cannot skip it. At the fifth reset refusal or 01:00 ET, whichever comes first, counted retries resume; if the bound is then reached, exit 77 stops the unit and the next reporter firing pages the failed unit (the separate logged-out page can arrive earlier, as explained above). The unit does not restart on any of these exits, because none fixes itself. The reporter's next firing pages it (the Gateway has no `OnFailure=` hook), and holds back the outside heartbeat while it stays down | For the login file: fill it in or correct it with `sudo ti-secrets edit ibkr-login.env`, then `sudo systemctl start ti-gateway` (`docs/runbooks/gateway-login.md`). After a nightly reset, recover by hand after 01:00 ET: `sudo systemctl start ti-gateway.service`, wait for "Login has completed" in the journal, then `sudo systemctl start ti-adapter-boost.service`. If the journal says "login retries stopped", check the paper login and clear the counter as below before starting. For "login retries stopped": check the paper login on IBKR's website, delete `/var/lib/ti-gateway/login-failures`, then `sudo systemctl start ti-gateway` ("The Gateway is logged out", above) |
| `<host>: IB Gateway is logged out` (or `journalctl -u ti-gateway` says "Unrecognized Username or Password" or "login retries stopped") | IBKR refused the login, typically after the nightly restart, and IBC left the dialog open. During the nightly reset window (23:40 to 01:00 ET), there are at most 4 uncounted reset refusals in any rolling 24 hours, of up to 15 minutes each, capped at 01:00 ET; restarts resume a persisted wait. After those, or outside the window, the start program stops IBC and systemd starts it again, at most 3 login attempts of any kind within 24 hours (two retries; IBKR locks at 10); then the unit stops with exit 77 and stays down. Class: operational. After the ten-minute detection grace outside restart windows, paging follows the escalation and shared schedule rules above. A current, uninterrupted reset-wait line from this night before 01:00 ET produces a quiet notice instead. Connectivity loss from a logged-in Gateway has its own notice, not a password page | "The Gateway is logged out", above: check the paper login on IBKR's website, log out of any other session with the paper username, delete `/var/lib/ti-gateway/login-failures`, `sudo systemctl restart ti-gateway` |
| `<host>: IB Gateway reset wait` | Class: operational. The newest gateway-start line records a refused-login reset wait on this night between 23:40 and the first 01:00 ET; an interrupted wait does not count. ntfy and the alerts repository receive one scheduled IBKR interruption notice; Pushover receives none during the wait | Let the bounded reset wait finish. At 01:00 ET ordinary logout detection and paging resume |
| `<host>: IBKR connectivity lost` | Class: operational. IBC shows a completed login but an active adapter has 1100/2110 newer than its last start without restoration. After the ten-minute grace, session-near pages immediately; otherwise ntfy and the alerts repository receive a notice, with Pushover delayed and held through scheduled IBKR maintenance | Wait during maintenance. If it outlasts the window and the Gateway remains logged in, `sudo systemctl restart ti-gateway` and confirm Login has completed and 1102. A later restoration or reconciliation resolves the incident |
| The IBKR password is wrong | Outside the nightly reset window (23:40 to 01:00 ET), the refused login is retried at most twice (3 attempts in all; any start counts, so after a good login fewer), then the unit stops, so IBKR cannot lock the login; you are paged | Correct the file (`sudo ti-secrets edit ibkr-login.env`), delete `/var/lib/ti-gateway/login-failures`, start it again (`docs/runbooks/gateway-login.md`) |
| The journal of `ti-data` or `ti-reporter@boost` says "waiting for the account's first run" (the unit is not failed) | No engine run has been started, so there is nothing to record or report. The data job writes nothing; the reporter sends no alert and, if nothing else is wrong, pings the heartbeat. Nothing is paged ("The reporter before the first run", below) | Start the Boost engine once (`docs/runbooks/durable-log.md`, "Restarting an engine") |
| A unit failed because its program is missing from `/opt/trend-investing/bin/` (`provision.sh services` printed "no .../dist/...: ... is not installed") | Nothing of that program runs | Build it on the Mac (`deploy/build-engine.sh`, `deploy/build-data.sh`), copy `dist/` beside `deploy/`, run `sudo deploy/provision.sh services` again |
| `ti-engine@<instance>`, `ti-adapter-boost` or `ti-adapter@<instance>` was skipped, not failed (`systemctl status` says its start condition was not met) | It did not start: the engine has no binary, `.env` or `.config` for that account; the adapter's `ibapi`, code or configuration is not installed | Install what is missing (`provision.sh twsapi`, `provision.sh adapter`, `provision.sh services`) before starting it |
| The memory sample fails (you are paged: "ti-memory.service failed") | Nothing complete is appended: a fault before the write adds no row, and a half-written last row is refused by the report, which names it; `journalctl -u ti-memory.service` says why | Check `memory.env`, memory accounting and the record directory permissions, then `sudo systemctl start ti-memory.service`. Its timer retries every five minutes; one page covers the episode, the reporter pages if the hook could not, and a good run resolves it. Keep any measurement gaps visible |
| The data job fails (you are paged: "ti-data.service failed") | The unit shows failed; `journalctl -u ti-data` says why (a 403 that is still there at 08:05 reads "Massive still refuses session ..."); no file is written for the session | Fix the cause and `sudo systemctl start ti-data` ([market data](market-data.md)); you are paged once, not reminded, and the incident ends when a run ends well, so wait for that run to finish ("A failed unit") |
| The end-to-end test (step 5 of "Your own ntfy server") shows nothing on the phone | The phone is not on the tailnet (Tailscale off, or not set to stay on), the app is subscribed on another server, or the server cannot reach ntfy.sh to wake an iPhone (`journalctl -u ti-ntfy` names the upstream) | Turn Tailscale on and set it to stay on (step 3); check the subscription's server URL is exactly `TI_NTFY_SERVER`; check the server reaches the internet (`curl -sI https://ntfy.sh`) |
| `ti-ntfy` fails, or the summary stops reaching your phone while Pushover pages still do; `journalctl -u ti-ntfy` names it | Your own ntfy server is down or has no Tailscale address yet. The unit tries again every 10 seconds; the reporter retries the summary at every firing and the run ends with an error naming `ntfy:`. Incident pages go to Pushover as well and are not held back | `sudo systemctl status ti-ntfy`; if Tailscale was signed out, `sudo tailscale up --ssh` and `sudo systemctl restart ti-ntfy`; if the configuration is missing, `sudo deploy/provision.sh ntfy` |
| `provision.sh ntfy` printed "SKIPPED the ntfy server's configuration: Tailscale is not signed in yet" | The program is installed; the server is not configured or started | `sudo tailscale up --ssh`, then `sudo deploy/provision.sh ntfy` again |
| `provision.sh ntfy` stopped with "is not a Tailscale address" | The address it was given is outside `100.64.0.0/10`; it configures nothing, so the server cannot be exposed | Do not set `TI_NTFY_ADDRESS` by hand on the server; sign in to Tailscale and run it again |
| The reporter fails every firing | Nothing reaches your phone; the error is in `journalctl -u ti-reporter@boost` | [alerts and dashboard](alerts-and-dashboard.md), "What can go wrong" |
| A page from Healthchecks (through Pushover) that your check `trend-investing-reporter` is down | The reporter has not finished a clean firing for 20 minutes (period 5 minutes plus 15 minutes' grace). Nothing on the server sent this page; it works even when the server is dead | Check the server is up and reachable over Tailscale (the dashboard, or `ssh`). If it is: `systemctl list-timers ti-reporter@boost.timer`, then `journalctl -u ti-reporter@boost -n 50` for the run's error. If the server is dead, start it from the Lightsail console, then read "After a reboot" |
| The reporter's journal says "the heartbeat ping was not sent: ..." | The reporter's run is unaffected; it will ping again at the next firing. The reason is the kind of failure only (an address that is not an absolute URL, the service not reached or not answering in time, or the status it answered), never the address | If the pages from Healthchecks come anyway, check `TI_HEARTBEAT_URL` in `secrets.env` is the check's full ping address and that the server can reach the internet. A single line during a Healthchecks outage needs nothing |
| No page from Healthchecks when you stop the timer (the test above) | The check is not wired: the address is missing or wrong, the period or grace is wrong, or the Pushover integration is not ticked on the check | Healthchecks' page for the check shows the last ping; fix the address, the 5-minute period, the 15-minute grace, or the integration |
| A page from the server, titled with the unit's name and ending "failed", that a trading unit failed | The engine, the adapter or the reporter stopped and systemd does not restart them; or the data job failed, which its timer runs again at the next firing. See "A failed unit" below | Read the manual row the page names, then `journalctl -u <unit>` |
| `provision.sh firewall` printed "SKIPPED firewall: Tailscale is not signed in yet" | The firewall is not changed, so the public door is still open | `sudo tailscale up --ssh`, then `sudo deploy/provision.sh firewall` again |
| The disk fills | An engine that cannot write its log stops deciding and releases nothing more (ADR 0027); the data job writes nothing; the reporter cannot save its state | Free space, then resume or start as `docs/runbooks/durable-log.md` says |
| The server's clock drifts behind IBKR's | The engine counts the account reading as stale and blocks every sell (`stale-broker-snapshot`); the sale back to 98% is withheld and you are alerted to that. The reporter's own clock check pages you first, within 10 minutes of the drift ("The server's clock failed its check") | Run the clock check ("The clock", above); fix the time syncing (or, on the Mac mini, the Mac's automatic date and time), then wait for the next close |
| `provision.sh services` is run again after paper edits | The repository's dry-run `.env` files come back, so `TI_MODE` says `dry-run` and the paper engine refuses to start. Every `.config` already on the server is kept (the shipped one is put beside it as `.config.example`), and so is the adapter's `boost.json` | Set `TI_MODE` again |
| A disk is lost | Only a Lightsail snapshot, if any, can restore it | `docs/runbooks/durable-log.md`, "A log that cannot be read" |

## What is pinned

Every download is fixed in `deploy/versions.env` by version and SHA-256, and provisioning stops on a mismatch.

| Pin | What it is | Installed by |
|---|---|---|
| IBC 3.24.2 | The Gateway's login controller | `provision.sh gateway` |
| IB Gateway 10.50.1f | IBKR's Gateway | `provision.sh gateway` |
| TWS API 10.51.01 | IBKR's Python client, `ibapi` | `provision.sh twsapi`, into the adapter's own environment |
| protobuf 5.29.5 | The Python package `ibapi` needs (PyPI's pure-Python wheel, one file for both machine types). **Proposed to the owner 2026-10-20 as a new runtime dependency (`docs/dependency-policy.md`); not final until ratified.** | `provision.sh twsapi`, before `ibapi`, offline |
| Tailscale 1.104.1 | The private network | `provision.sh base` |
| ntfy 2.29.0 | Your own push server for the daily summary (Linux amd64 release, checked against the SHA-256 in the release's `checksums.txt`) | `provision.sh ntfy` |

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
