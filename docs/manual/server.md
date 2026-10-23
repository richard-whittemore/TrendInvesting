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
all of this with: `git pull`, `sudo deploy/provision.sh services` (installs the changed `render-ibc-config.sh` into `/usr/local/libexec/trend-investing`, and reinstalls `ti-data`, `ti-reporter`, `ti-dashboard`, `operator` and `journal-query` from the checkout's `dist/` for each one present; it does not touch `ti-engine`. That `dist/` holds the builds copied from the Mac at the last install, so it is safe to run now; for later upgrades use releases, `ti-release`), `sudo deploy/provision.sh gateway` (repairs `/opt/ibc` modes), `sudo deploy/provision.sh twsapi` (rebuilds the adapter environment with open modes), `sudo deploy/provision.sh firewall`, then `sudo systemctl restart ti-gateway` (the Gateway logs in again; after a cold
start expect an IBKR Mobile prompt). `services` copies the repository's engine
files over `/etc/trend-investing/engines/` (see the caution under the file table):
that costs nothing while the server is in dry-run with those files unedited, but
not once they are edited for paper. To check it, run a pair of probes a few minutes apart. **Step A, on the server (the control):** `python3 deploy/ibapi-probe.py 127.0.0.1 4002` must print `EXPOSED`; that shows the Gateway is up and the probe works. Anything else: stop, the Gateway is not ready; rerun after the Gateway log shows `Login has completed`. **Step B, from the Mac, within a few minutes of A:** `python3 deploy/ibapi-probe.py trend-investing-1 4002` passes on anything except `EXPOSED`: `NOT-SERVED refused`, `NOT-SERVED closed` or `NOT-SERVED silent` (exit 0) all mean no API was served to the tailnet. `EXPOSED` (exit 1) fails: stop and report it. `ERROR` (exit 2: a name that did not resolve, no usable address, or a usage error) is not a pass: fix it and rerun. The probe sends only the API's opening handshake and closes. Each `provision.sh` step is safe to repeat.
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

All times are New York time (each unit sets `TZ=America/New_York`).

| Unit | What it does | When it runs | Today |
| --- | --- | --- | --- |
| `ti-gateway.service` | IB Gateway under IBC, logged in to IBKR | Starts at boot (enabled). Restarts itself 30 seconds after IBC exits (for example when the weekly phone approval times out), except when the login file is empty or malformed (a wrong password is not caught: see the table below). IBC restarts the Gateway nightly at 11:45 PM and logs in again from cold every Sunday at 09:00 (`/etc/trend-investing/gateway.env`) | Installed; not yet run against a real IBKR login |
| `ti-data.timer` and `ti-data.service` | The daily data job: records the day's market data from Massive and writes the Boost engine's input ([market data](market-data.md), "The daily data job") | Monday to Friday at 17:05, retried at 18:05 and 20:05. A firing missed while the server was off runs when it starts again. A run is stopped after an hour | Built and installed by `provision.sh services` when `dist/ti-data` is beside `deploy/`. It fails, with a message, until the Boost engine has been started once |
| `ti-engine@boost.service` (and `@trad-ira`, `@roth-ira`) | One engine per account ([engine](engine.md)) | **Never by itself.** Not started at boot, never restarted by systemd (`Restart=no`): every start is yours ([restart](restart.md)) | Boost in dry-run. The IRA instances are skipped: they have no configuration file |
| `ti-reporter@boost.timer` and `ti-reporter@boost.service` | The reporter: alerts and the daily summary ([alerts and dashboard](alerts-and-dashboard.md)). One instance per account; only Boost's timer is enabled, since the IRA engines have no configuration yet | **Every 5 minutes, around the clock, seven days a week**, so an alert reaches you within 5 minutes, overnight and at weekends too. A firing missed while the server was down runs at boot. A run is stopped after 4 minutes | Built and installed by `provision.sh services`. It reads the log named by the Boost engine's `run.env` at every firing, and fails, with a message, until the engine has been started once and the `secrets.env` entries below are filled in |
| `ti-unit-failed@.service` | The page that a unit failed: the engine, the adapter and the reporter each name it in `OnFailure=`, so systemd starts one instance, named after the failed unit (for the engine, `ti-unit-failed@ti-engine@boost.service.service`), the moment that unit fails. It sends one urgent page through the reporter's channels naming the unit, the failed run's last journal line and the manual row to read ("A failed unit", below) | Only when one of those three units fails; never by itself otherwise | Installed by `provision.sh services` with the other units |
| `ti-dashboard.service` | The read-only dashboard, on loopback port 8088 | Starts at boot (enabled) | Built and installed by `provision.sh services`; it finds the Boost engine's current run, the newest link of its chain of runs, on every request and follows the chain of runs back to the first, so a restart or an upgrade of the engine needs no dashboard restart. It shows a message, not a figure, until the engine has run once |
| `ti-ntfy.service` | Your own ntfy server: the only place the daily summary goes ("Your own ntfy server", below). Listens on the server's Tailscale address, port 2586, and nowhere else | Starts at boot once `provision.sh ntfy` has enabled it; tried again every 10 seconds if the Tailscale address is not up yet | Installed by `provision.sh ntfy`, which you run after joining Tailscale. Not part of `provision.sh all` |
| `tailscaled` | Tailscale | At boot | Installed by `provision.sh base` |

`ti-adapter-boost.service` carries the IBKR adapter ([IBKR adapter](ibkr-adapter.md)): never started at boot or restarted by systemd (`Restart=no`), every start is yours, after the engine ([paper trading](paper-trading.md), "Starting the adapter"). It reads the same `run.env` as the engine, reads the engine's log, talks to the engine over its socket, and delivers the data job's stream for the run, so it refuses to start without the engine's log or that run's stream folder, which the data job's warm-up writes ([market data](market-data.md), "The one-time warm-up"). It is skipped, not failed, until `provision.sh twsapi` and `provision.sh adapter` have installed `ibapi` and the adapter's code, and `provision.sh services` has created its configuration (`/etc/trend-investing/adapters/boost.json`, created once and never overwritten, in dry-run with paper sending off). Its reason for stopping is in `journalctl -u ti-adapter-boost`.

Updates: `unattended-upgrades` applies security updates only and never
reboots by itself, because a reboot logs the Gateway out.

## Upgrading the programs

The server has no Go and does not update itself. To put a fix on it, tag the merged commit on your Mac (`git tag -a v1.2.3 -m "v1.2.3"` and push the tag); GitHub builds the five programs and publishes them with their checksums and a build attestation. Then, on the server over Tailscale, run `sudo ti-release v1.2.3`. It downloads that release, checks it, keeps the build now installed as the previous one, installs the new one, restarts the dashboard and prints what changed. If the dashboard does not come back, it puts the previous build back and tells you.

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
| `/etc/trend-investing/engines/<instance>.env` | the account's label (`TI_ACCOUNT`), its adapter client ID (Boost 11, Traditional IRA 12, Roth IRA 13) and, for Boost, the engine's start mode (`TI_MODE`) | copied from `deploy/etc/engines/` by `provision.sh services` |
| `/etc/trend-investing/engines/<instance>.config` | the account's configuration: strategy, account, mode, rule figures, hard limits. Hashed into every run (ADR 0016). Only `boost.config` exists | the same |
| `/var/lib/trend-investing/engines/<instance>/run.env` | one engine start: `TI_RUN` (the run's name, never reused), `TI_AS_OF` (its first decided session) and `TI_RESTART_ARGS` (empty, `-approval <file>` or `-resume <log>`) | you, before every start (`docs/runbooks/durable-log.md`, "Restarting an engine"; example in `deploy/etc/engines/run.env.example`) |
| `/etc/trend-investing/gateway.env` | the Gateway's restart times | `provision.sh services` |
| `/etc/trend-investing/adapters/boost.json` | the Boost adapter's settings: the Gateway's address and port (4002), client ID 11, mode and the paper sending switch (`paper_sending_enabled`), timeouts; no account number | created once from `deploy/etc/adapters/boost.json.example` by `provision.sh services`, never overwritten; you edit it for paper |

**Caution:** `provision.sh services` copies the repository's `*.env` and
`*.config` files over `/etc/trend-investing/engines/` every time it runs.
Once you have edited them for paper (the paper account, mode `paper`, the
warm-up start), running it again puts the dry-run files back. The engine
then refuses to start in paper, which is safe, but your edits are gone.

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
| `secrets.env` | `IBKR_ACCOUNT_TRAD_IRA`, `IBKR_ACCOUNT_ROTH_IRA`, `MASSIVE_API_KEY`, `TI_NTFY_TOPIC`, `TI_NTFY_SERVER`, `TI_PAGERDUTY_ROUTING_KEY`, `TI_GITHUB_ISSUES_TOKEN`, `TI_GITHUB_ISSUES_REPO`, `TI_HEARTBEAT_URL` | handed as a private credential file (`LoadCredential`) to the data job, the engines and the reporter; the dashboard is given none |
| `accounts/boost.env` | `IBKR_ACCOUNT_BOOST`: the Boost account's number, alone | handed as a private copy to the Boost adapter (`ti-adapter-boost.service`) and to the Boost account's Flex reader (`ti-flex@boost.service`) |
| `accounts/trad-ira.env` | `IBKR_ACCOUNT_TRAD_IRA`: the Traditional IRA's number, alone | handed as a private copy to that account's Flex reader only (`ti-flex@trad-ira.service`) |
| `accounts/roth-ira.env` | `IBKR_ACCOUNT_ROTH_IRA`: the Roth IRA's number, alone | handed as a private copy to that account's Flex reader only (`ti-flex@roth-ira.service`) |

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
  which never gets the daily summary) and `TI_PAGERDUTY_ROUTING_KEY` from that file
  (`-secrets-file`), through its unit's credential. It refuses to run, and
  its unit fails, while the topic or the routing key is empty: fill them in
  with `sudo ti-secrets edit secrets.env`; the next firing uses
  them. It also reads `TI_HEARTBEAT_URL`, the address of your check at the
  outside heartbeat service; that one is optional for the run, but without
  it nothing outside the server notices the server dying
  ("The outside heartbeat", below).
- The reporter's `github-issues` channel reads `TI_GITHUB_ISSUES_TOKEN`, a
  fine-grained token with **Issues read and write on the one repository and
  no other permission**, and `TI_GITHUB_ISSUES_REPO`, that repository as
  `owner/name`. Both are optional until `github-issues` is in the reporter's
  `-channel`; the channel is set aside while either is empty and the others
  still deliver. How to create and rotate the token:
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
through PagerDuty ("The outside heartbeat", below). Unlock, and the next
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
in plain text (the IBKR password, the Massive key, the PagerDuty routing
key, the GitHub token, the Flex token), since anyone who later gets the
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
server's Tailscale address. PagerDuty never gets the summary (a page means an
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
the messages older than 12 hours. Incident alerts go to PagerDuty as well, so
they do not depend on this server. It keeps nothing else and holds no
account.

**Without it** the daily summary is not sent, and the reporter says so once,
as a routine message on the incident channels and in its output; incident
alerts are unaffected. Read the summary on the dashboard
meanwhile ([alerts and dashboard](alerts-and-dashboard.md), "Where alerts
go").

## The outside heartbeat

A reporter on the server cannot page you that the server died, so the
reporter also pings an outside service, Healthchecks, at the end of every
firing that finishes without a fault (every 5 minutes, around the clock). The
service pages you through PagerDuty when the pings stop. You decided this is
required before paper trading.

What is sent: an empty request to your check's address, with nothing in it:
no balance, account number, alert text or secret. The address is the check's
key, so it lives only in `secrets.env` as `TI_HEARTBEAT_URL` and is never
printed or logged. A ping that fails never fails the reporter's run: the
reporter logs one line ("the heartbeat ping was not sent: ...", without the
address) and pings again at the next firing. No ping is sent for a firing that
failed, so a reporter that cannot deliver, or cannot read the log, also goes
quiet and is noticed from outside; nor for a firing that found the engine or
the adapter failed, even when its page was delivered, so a trading unit left
failed for 20 minutes also takes the check down.

**Set it up (once) [OWNER]:**

1. Sign up at healthchecks.io. The free plan is enough for one check.
2. Add a check named `trend-investing-reporter` with a **period of 5
   minutes** (the reporter's timer) and a **grace time of 15 minutes**, so
   the check is down 20 minutes after the last ping, on the fourth missed
   firing, and one slow firing does not page you.
3. Copy the check's ping address (`https://hc-ping.com/<uuid>`) and put it on the
   server: `sudo ti-secrets edit secrets.env`, as the line
   `TI_HEARTBEAT_URL=<the address>`. The next firing uses it.
4. In Healthchecks, under Integrations, add **PagerDuty** (it asks you to
   sign in to PagerDuty and pick the same service your reporter pages) and
   tick it on the check.
5. Test it: `sudo systemctl stop ti-reporter@boost.timer`. After about
   20 minutes (the 5-minute period plus the 15 minutes' grace) PagerDuty
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

## What you start by hand

- **Once:** create the instance, join Tailscale (`sudo tailscale up --ssh`),
  close the public door (`provision.sh firewall`), enter the IBKR login and
  the secrets, start the Gateway the first time and approve the phone
  prompt (`docs/runbooks/lightsail-deployment.md`).
- **Every engine start**, after a crash or not: write `run.env`, and for a
  fresh run an approval, then `sudo systemctl start ti-engine@<instance>`
  ([restart](restart.md)).
- **The adapter**, after its engine: `sudo systemctl start ti-adapter-boost`
  ([paper trading](paper-trading.md), "Starting the adapter").
- **Once per Boost run:** the data job's warm-up, after the engine has
  started once ([market data](market-data.md), "The one-time warm-up").
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

The engine, the adapter and the reporter each name `ti-unit-failed@%n.service`
in `OnFailure=`. When one fails, systemd starts that unit under the failed
unit's name (for the engine, `ti-unit-failed@ti-engine@boost.service.service`;
the rehearsal starts it under a real systemd), which runs `ti-reporter
notify-unit-failed <unit>`: one urgent page on PagerDuty and your own ntfy
server naming the unit, the reason (cleaned of addresses, account numbers and
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
  PagerDuty incident is neither resolved nor triggered anew. While it goes
  on, at most once an hour, a reminder `<host>: <unit> still failing` is sent
  (by a later failed run, or by a reporter firing that finds the unit still
  failed), saying since when and, once two or more runs have failed, how
  many, under the same incident. A failure after the unit was seen well pages
  again. The record is `unit-failures.json` in
  `/var/lib/trend-investing/reports/`.
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
  firing, so a unit that fails again while a firing runs keeps its incident
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
| `ti-gateway` failed and stays down; `journalctl -u ti-gateway` names the login file | The login file is empty, or holds an unknown key or value (for example a trading mode other than `paper` or `live`). The unit does not restart on these, because none fixes itself | Fill it in or correct it with `sudo ti-secrets edit ibkr-login.env`, then `sudo systemctl start ti-gateway` (`docs/runbooks/gateway-login.md`) |
| The IBKR password is wrong | That check does not catch it: if IBC exits, the unit starts it again every 30 seconds, and repeated failed logins can make IBKR lock the login | Stop the Gateway at once (`sudo systemctl stop ti-gateway`), correct the file, start it again (`docs/runbooks/gateway-login.md`) |
| `ti-data` or `ti-reporter@boost` failed: ".../engines/boost/run.env is missing or empty: start the Boost engine once first ..." | That unit does not run. The data job writes nothing; the reporter sends nothing | Start the Boost engine once (`docs/runbooks/durable-log.md`, "Restarting an engine") |
| A unit failed because its program is missing from `/opt/trend-investing/bin/` (`provision.sh services` printed "no .../dist/...: ... is not installed") | Nothing of that program runs | Build it on the Mac (`deploy/build-engine.sh`, `deploy/build-data.sh`), copy `dist/` beside `deploy/`, run `sudo deploy/provision.sh services` again |
| `ti-engine@<instance>` or `ti-adapter-boost` was skipped, not failed (`systemctl status` says its start condition was not met) | It did not start: the engine has no binary, `.env` or `.config` for that account (the IRA engines have no configuration yet); the adapter's `ibapi`, code or configuration is not installed | Install what is missing (`provision.sh twsapi`, `provision.sh adapter`, `provision.sh services`) before starting it |
| The data job fails | The unit shows failed; `journalctl -u ti-data` says why; no file is written for the session | Fix the cause and `sudo systemctl start ti-data` ([market data](market-data.md)) |
| The end-to-end test (step 5 of "Your own ntfy server") shows nothing on the phone | The phone is not on the tailnet (Tailscale off, or not set to stay on), the app is subscribed on another server, or the server cannot reach ntfy.sh to wake an iPhone (`journalctl -u ti-ntfy` names the upstream) | Turn Tailscale on and set it to stay on (step 3); check the subscription's server URL is exactly `TI_NTFY_SERVER`; check the server reaches the internet (`curl -sI https://ntfy.sh`) |
| `ti-ntfy` fails, or the summary stops reaching your phone while PagerDuty pages still do; `journalctl -u ti-ntfy` names it | Your own ntfy server is down or has no Tailscale address yet. The unit tries again every 10 seconds; the reporter retries the summary at every firing and the run ends with an error naming `ntfy:`. Incident pages go to PagerDuty as well and are not held back | `sudo systemctl status ti-ntfy`; if Tailscale was signed out, `sudo tailscale up --ssh` and `sudo systemctl restart ti-ntfy`; if the configuration is missing, `sudo deploy/provision.sh ntfy` |
| `provision.sh ntfy` printed "SKIPPED the ntfy server's configuration: Tailscale is not signed in yet" | The program is installed; the server is not configured or started | `sudo tailscale up --ssh`, then `sudo deploy/provision.sh ntfy` again |
| `provision.sh ntfy` stopped with "is not a Tailscale address" | The address it was given is outside `100.64.0.0/10`; it configures nothing, so the server cannot be exposed | Do not set `TI_NTFY_ADDRESS` by hand on the server; sign in to Tailscale and run it again |
| The reporter fails every firing | Nothing reaches your phone; the error is in `journalctl -u ti-reporter@boost` | [alerts and dashboard](alerts-and-dashboard.md), "What can go wrong" |
| A page from Healthchecks (through PagerDuty) that your check `trend-investing-reporter` is down | The reporter has not finished a clean firing for 20 minutes (period 5 minutes plus 15 minutes' grace). Nothing on the server sent this page; it works even when the server is dead | Check the server is up and reachable over Tailscale (the dashboard, or `ssh`). If it is: `systemctl list-timers ti-reporter@boost.timer`, then `journalctl -u ti-reporter@boost -n 50` for the run's error. If the server is dead, start it from the Lightsail console, then read "After a reboot" |
| The reporter's journal says "the heartbeat ping was not sent: ..." | The reporter's run is unaffected; it will ping again at the next firing. The reason is the kind of failure only (an address that is not an absolute URL, the service not reached or not answering in time, or the status it answered), never the address | If the pages from Healthchecks come anyway, check `TI_HEARTBEAT_URL` in `secrets.env` is the check's full ping address and that the server can reach the internet. A single line during a Healthchecks outage needs nothing |
| No page from Healthchecks when you stop the timer (the test above) | The check is not wired: the address is missing or wrong, the period or grace is wrong, or the PagerDuty integration is not ticked on the check | Healthchecks' page for the check shows the last ping; fix the address, the 5-minute period, the 15-minute grace, or the integration |
| A page from the server, titled with the unit's name and ending "failed", that a trading unit failed | The engine, the adapter or the reporter stopped and systemd does not restart them. See "A failed unit" below | Read the manual row the page names, then `journalctl -u <unit>` |
| `provision.sh firewall` printed "SKIPPED firewall: Tailscale is not signed in yet" | The firewall is not changed, so the public door is still open | `sudo tailscale up --ssh`, then `sudo deploy/provision.sh firewall` again |
| The disk fills | An engine that cannot write its log stops deciding and releases nothing more (ADR 0027); the data job writes nothing; the reporter cannot save its state | Free space, then resume or start as `docs/runbooks/durable-log.md` says |
| The server's clock drifts behind IBKR's | The engine counts the account reading as stale and blocks every sell (`stale-broker-snapshot`); the sale back to 98% is withheld and you are alerted to that. The reporter's own clock check pages you first, within 10 minutes of the drift ("The server's clock failed its check") | Run the clock check ("The clock", above); fix the time syncing (or, on the Mac mini, the Mac's automatic date and time), then wait for the next close |
| `provision.sh services` is run again after paper edits | The repository's dry-run `.env` and `.config` files come back; the paper engine refuses to start. The adapter's `boost.json` is kept | Redo the paper edits |
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
