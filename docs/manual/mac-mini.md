# The Mac mini: what runs where

*For the owner. The first paper runs happen on your Mac mini, not on the
Lightsail server. This chapter says what runs on the Mac and what runs in
its one container, where the state is kept, why the code you install is not
the copy on your Desktop, what a restart does, and what can go wrong. The
step-by-step setup is the runbook `docs/runbooks/mac-mini.md`. Everything
the server chapter ([server](server.md)) says about the units, the
configuration and the secrets holds inside the container, except where this
chapter says otherwise.*

## What it is and why

You approved option A of the Mac mini plan on 2026-10-18
(`Mac_Mini_Paper_Deployment_Plan_2026-10-18.md`, sections 1 and 3, on your
Desktop and not in the repository). It works like this:

- **IB Gateway runs as IBKR's own macOS app.** You log in to it by hand. It
  listens on the Mac's port 4002 (paper).
- **Everything else runs in one container** called `ti`. It is a small
  Ubuntu 24.04 machine for Apple silicon (arm64), run by Docker Desktop. It
  holds the data job, the engine, the adapter, the reporter and the
  dashboard. It is set up by the same `deploy/provision.sh` and the same
  systemd units as the Lightsail server, with one switch,
  `TI_TARGET=mac-mini`. systemd really runs inside it, as the first
  program (PID 1), so every unit starts, stops and is restricted exactly as
  it would be on the server.

Inside the container the Mac is reachable as `host.docker.internal`. The
adapter's configuration (`/etc/trend-investing/adapters/boost.json`) is
created with that host name, so the adapter reaches the Gateway app on the
Mac's port 4002.

| | Lightsail server | Mac mini |
| --- | --- | --- |
| IB Gateway | under IBC, `ti-gateway.service` | IBKR's macOS app, logged in by you; `ti-gateway.service` is **masked** (nothing can start it) |
| Tailscale and the firewall | yes | not installed: nothing listens beyond the Mac's own loopback |
| Time syncing | a time service on the server | the Mac keeps the time; the container's clock check compares it with an outside website every time |
| systemd's journal | the server's disk | kept on disk in its own volume, so why a unit stopped survives a restart |

## The four volumes

The container's state lives in four Docker **volumes**. A volume is storage
kept inside Docker's own Linux virtual machine, not in a folder of your Mac,
and it outlives the container: removing the container and creating it again
leaves every volume as it was.

| Volume | Mounted at | What it holds |
| --- | --- | --- |
| `ti-etc` | `/etc/trend-investing` | the configuration and the root-only secret files (`secrets.env`, `accounts/boost.env`, the engines' settings, the adapter's configuration) |
| `ti-state` | `/var/lib/trend-investing` | the market data, each engine's durable logs and journals, `run.env`, the adapter's files, the reporter's state |
| `ti-opt` | `/opt/trend-investing` | the installed programs |
| `ti-journal` | `/var/log/journal` | systemd's own log (`journalctl`) |

There are no other mounts: no Mac folder is shared into the container. The
code is copied in by the launcher, never linked. The dashboard's port is
published on the Mac's own loopback address only (`127.0.0.1:8088`), so
nothing on your home network can reach it.

**A volume is not a backup.** If Docker Desktop is reset or uninstalled, its
virtual machine and every volume go with it. Nightly backups to a folder on
the Mac are not built yet (Mac mini plan, section 5, item 7).

## Why the checkout is not on the Desktop

Your Desktop and Documents folders are synced by iCloud. Two things go wrong
with code kept there:

- iCloud keeps both copies when two edits meet, and names one `name 2.py`.
  Copied into the container, a stray `cli 2.py` beside `cli.py` would be
  installed as part of the adapter.
- A sync in progress can stall a copy, or Docker Desktop's file sharing,
  for minutes.

So the code you install lives in a checkout of its own in your home folder,
`~/TrendInvesting-deploy`, used only for builds and installs. The launcher,
`deploy/mac-mini/up.sh`, refuses to run from a checkout under `~/Desktop`,
`~/Documents`, iCloud Drive (`~/Library/Mobile Documents`) or another synced
folder (`~/Library/CloudStorage`), and refuses a checkout or build folder
holding any file or folder named like a sync duplicate (`* 2.*` or `* 2`). It
says which file and stops before it touches Docker, so nothing is created.

## The launcher

`deploy/mac-mini/up.sh` is one command. Run it from `~/TrendInvesting-deploy`:

- **The first time** it builds the container's image (Ubuntu 24.04 for arm64
  with systemd; `deploy/mac-mini/Dockerfile`), creates the four volumes and
  the container, starts it, copies `deploy/` and the built programs in, and
  runs `provision.sh all` inside with `TI_TARGET=mac-mini`. On the Mac mini
  `all` is the `base`, `clock` and `services` steps; the Gateway and
  firewall steps say they have nothing to do.
- **Every later time** it starts the container if it is stopped, copies the
  checkout in again and provisions again. A second pass changes nothing
  unless the checkout changed, and it never overwrites a file you filled in
  (the secrets, the account file, the adapter's configuration).
- **`up.sh --recreate`** removes the container and creates it again from the
  current image, keeping every volume. Use it when the image changes (the
  launcher says so) or the container itself is damaged.

The container runs **privileged**. systemd as the container's first program
stops at once without it under Docker Desktop, and the units' restrictions
(a read-only system, private temporary folders, a private folder for the
engine's socket) need it. The privilege stays inside Docker's own Linux
virtual machine. It is not a privilege on your Mac.

Two settings in the image keep it from upsetting Docker itself:

- **It never touches the virtual machine's program handlers** (binfmt). They
  are shared by every container. Ubuntu's own systemd would clear them at
  boot, and that removes the handler (Rosetta) that lets Docker Desktop run
  Intel (amd64) containers on Apple silicon. The units that do this are
  masked in the image.
- **Mount sharing is turned on at boot**, as systemd does on a machine of
  its own. Docker starts containers without it, and without it systemd
  cannot hand a unit its secrets (the reporter and the data job would fail,
  saying their secrets file cannot be read).

## What a restart does

A restart of the Mac, of Docker Desktop, or of the container (`docker
restart ti`) has the server's rules:

- The container comes back by itself (Docker's restart policy "unless
  stopped"; Docker Desktop must start when you sign in).
- The data job's timer, the reporter's timer and the dashboard come back.
  The reporter fires within 5 minutes. A data job firing missed while the
  Mac was off runs when it is back.
- **The engine and the adapter stay stopped.** They are never started by
  systemd. A restart is not a crash, so the engine's next start is a fresh
  run you approve ([restart](restart.md)), then the adapter after it.
- Nothing in the volumes changes. The rehearsal on 2026-10-18 checked this
  byte for byte, owners and modes included, after a restart and after
  `up.sh --recreate`, and `journal-query` read the durable log afterwards.

The Gateway app does not restart by itself after a Mac restart: you log in
to it again.

## What is not built yet

- The programs for arm64 (`deploy/build-engine.sh` builds for the Lightsail
  server's x86_64) and the adapter's installation in the container (Mac mini
  plan, slice 2). Until then `up.sh` installs only what is in the build
  folder.
- The dashboard on the Mac's loopback. The port is published, but the
  dashboard listens on the container's own loopback, so the Mac cannot reach
  it yet (Mac mini plan, section 5, item 5).
- The `ti` command for `edit`, `start`, `stop`, `status` and `logs`. Until
  it exists, use `docker exec -it ti ...`, as the runbook shows.
- The operator inbox in the container. It is created the same way as on the
  server once the provisioner creates it there ([operator](operator.md),
  "The operator inbox"). Until then the kill switch is to stop the engine
  (`docker exec ti systemctl stop ti-engine@boost`).
- Nightly backups, and the outside heartbeat.

## What can go wrong

| What you see | What the program does | What you do |
| --- | --- | --- |
| `up: refusing the checkout at ...: it is under ~/Desktop` (or Documents, iCloud Drive, CloudStorage) | Stops before it touches Docker; nothing is created or changed | Run it from `~/TrendInvesting-deploy`, a checkout of its own in your home folder (`docs/runbooks/mac-mini.md`) |
| `up: refusing the checkout at ...: it holds what look like iCloud sync duplicates` | Names up to five such files and stops before it touches Docker | Delete them, or take a fresh checkout outside the synced folders, then run it again |
| `up: Docker's VM is x86_64` | Stops: the container is for Apple silicon only | Run it on the Mac mini |
| `up: systemd in ti did not finish starting` | Stops before copying anything in | Read `docker logs ti`; run `up.sh` again, or `up.sh --recreate` (the volumes are kept) |
| `provision: the clock is not good` during `up.sh` | The clock step refuses, and `up.sh` stops before the services step | Turn on the Mac's "Set time and date automatically", then run `up.sh` again ([server](server.md), "The clock") |
| `up: note: ti runs the image ..., not ...` | Keeps the running container and provisions it as before | Run `up.sh --recreate` when it suits you; every volume is kept |
| After a Mac or Docker Desktop restart, the engine and the adapter are stopped | Nothing: this is the rule, and the timers and dashboard are back | Log in to the Gateway app, then start the engine as a fresh approved run and then the adapter ([restart](restart.md)) |
| `systemctl is-system-running` inside says `degraded`, with `ti-reporter@boost` failed | The reporter fails every 5 minutes until the Boost engine has run once and `secrets.env` holds its channels | Expected before the first run; otherwise read `journalctl -u ti-reporter@boost` ([alerts and dashboard](alerts-and-dashboard.md)) |
| An Intel (amd64) container fails on this Mac with `exec format error` | Not this container: its image never touches Docker's program handlers. An older image, or another systemd container, may have cleared them | Quit and reopen Docker Desktop, which registers Rosetta's handler again; check that `ti` runs the current image (`up.sh` says so if not) |
| The reporter or the data job fails, saying its secrets file cannot be read | systemd could not hand the unit its secrets; the unit fails and does nothing | Check that `ti` runs the current image, which turns mount sharing on at boot; run `up.sh --recreate` if it does not |
| Docker Desktop is reset or uninstalled | The virtual machine and every volume are gone: configuration, secrets, durable logs | Nothing restores them yet (backups are not built); keep Docker Desktop's "reset" away from this Mac |

## What backs this

- The Mac mini plan, option A, approved by you on 2026-10-18: sections 3, 5
  (items 1 and 4) and 7, and its tests T1, T2, T3 and T10.
- `deploy/provision.sh` (`TI_TARGET=mac-mini`), `deploy/mac-mini/up.sh` and
  `deploy/mac-mini/Dockerfile`.
- `deploy/rehearsal/test-target.sh` checks the provisioner's two targets
  (arm64 accepted only on the Mac mini; the Gateway masked, not enabled;
  Tailscale and the firewall skipped there and unchanged on Lightsail).
  `deploy/mac-mini/test-refusals.sh` checks every refusal of the launcher.
  Both run in `make check`.
- `deploy/mac-mini/test-container.sh`, run on the Mac mini, makes a
  throwaway container with the launcher and checks it against the running
  systemd. Every unit verifies. The timers, the dashboard and a stand-in
  engine start with their restrictions and their secrets. The state is
  byte-identical after a restart and after a re-creation. The reporter fires
  within 5 minutes. The engine stays stopped. The clock check passes and
  refuses a clock an hour off. Docker's program handlers are left alone.
