# The Mac mini: a rehearsal container

*For the owner. Paper trading runs on the Lightsail server
([server](server.md)), not on your Mac. The Mac mini's container is a local
rehearsal environment: a small Ubuntu machine with a real systemd, set up by
the same provisioner and units as the server, where you can try the
provisioning, the units, a restart and your own steps before doing them on
the server. This chapter says what runs in it, where its state is kept, why
the code you install is not the copy on your Desktop, what a restart does,
what the container is allowed to do on your Mac, and what can go wrong. The
step-by-step setup is the runbook `docs/runbooks/mac-mini.md`. Everything
the server chapter says about the units, the configuration and the secrets
holds inside the container, except where this chapter says otherwise.*

## What it is and why

A rehearsal on your own Mac costs nothing and can be thrown away. It works
like this:

- **One container** called `ti`. It is a small Ubuntu 24.04 machine for
  Apple silicon (arm64), run by Docker Desktop. It is set up by the same
  `deploy/provision.sh` and the same systemd units as the Lightsail server,
  with one switch, `TI_TARGET=mac-mini`. systemd really runs inside it, as
  the first program (PID 1), so every unit starts, stops and is restricted
  as it would be on the server.
- **No IB Gateway inside.** On the server the Gateway runs under IBC as
  `ti-gateway.service`. In the container that unit is **masked** (nothing can
  start it). If you rehearse against IBKR's paper servers, the Gateway is
  IBKR's own macOS app, logged in by hand on the Mac's port 4002; inside the
  container the Mac is reachable as `host.docker.internal`, and the adapter's
  configuration (`/etc/trend-investing/adapters/boost.json`) is created with
  that host name.

| | Lightsail server | Mac mini container |
| --- | --- | --- |
| What it is for | paper trading, later live | rehearsal only |
| IB Gateway | under IBC, `ti-gateway.service` | not inside; `ti-gateway.service` is masked |
| Tailscale and the firewall | yes | not installed: nothing listens beyond the Mac's own loopback |
| Time | a time service on the server | no time service inside; the clock is compared with an outside website when `up.sh` runs and every 15 minutes ("The clock", below) |
| systemd's journal | the server's disk | kept on disk in its own volume, so why a unit stopped survives a restart |
| The service users | created by the provisioner | created at fixed IDs (`ti-gateway` 2001, `ti-trader` 2002, and `ti-operator`, the operator inboxes' writer, 2003 in `ti-trader`'s group), so they still own their files after the container is created again |

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

The user accounts (`/etc/passwd`) are not in a volume: they are on the
container's own disk and are created again when the container is. That is
why the three service users have fixed IDs (`ti-gateway` 2001, `ti-trader`
2002 and `ti-operator` 2003, in `ti-trader`'s group 2002): the files in the volumes are owned
by those numbers, so a container created again, even from a changed image,
gives them back to the same users. If a changed image already has another
account at one of those IDs, provisioning stops and names it.

**A volume is not a backup.** If Docker Desktop is reset or uninstalled, its
virtual machine and every volume go with it. For a rehearsal that is a
nuisance, not a loss: nothing in the container is the record of a real run.

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
folder (`~/Library/CloudStorage`). It refuses a checkout or build folder
holding any file or folder named like a sync duplicate (`* 2.*` or `* 2`),
says how many and names the first five. It also refuses one with a folder it
cannot read, since that folder cannot be checked. Each refusal comes before
it touches Docker, so nothing is created.

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
  launcher says so) or the container itself is damaged. It refuses while
  the engine or the adapter runs, naming the unit: removing the container
  would end the engine without its own shutdown, which is a crash. Stop the
  adapter, then the engine, first. Otherwise it stops the container the way
  `docker stop` does, letting systemd shut every unit down in order (up to
  60 seconds), and only then removes it.

It is careful with what it did not make:

- **Only its own container.** The container is labelled with its name and
  the checkout it was made from. A container called `ti` that up.sh did not
  make, or made from another checkout, or whose mounts are not exactly the
  four volumes, is neither provisioned nor removed: up.sh stops and says
  which label or mount is wrong.
- **Only its own volumes.** The four volumes carry the same two labels, and
  up.sh reuses existing ones only if they name this checkout, whether or not
  a container uses them now. Volumes kept from another checkout's container,
  or made by an earlier up.sh before it recorded the checkout, are never
  adopted silently: up.sh stops and names them.
- **One at a time.** Two `up.sh` runs for the same container at once would
  copy over each other's files. Each run holds a lock
  (`~/.trend-investing/up-ti.lock`) while it works; a second run stops and
  names the first. A lock records the run's process ID and start time, so an
  ID given to another program after the run died does not hold it. A lock left by a run that no longer exists is taken over,
  by one run only: a run that finds another already taking it over stops
  too. A run killed while taking over (or a Mac that lost power then) leaves
  its takeover link, `up-ti.lock.takeover`, naming a process that no longer
  exists; that link is taken over the same way, so it does not block later
  runs. Only if the lock and both links above it were all left by dead runs
  does up.sh stop and name the three to remove by hand.
- **A bad port is refused first.** `TI_MAC_DASHBOARD_PORT` must be a number
  from 1 to 65535, checked before anything in Docker is touched.
- **A machine that did not start well is not provisioned.** systemd
  reporting `degraded` is accepted only when every failed unit is one that
  fails, saying why, until your first steps: `ti-reporter@boost` and
  `ti-dashboard` until the Boost engine has run once, `ti-data` until its
  key is filled in. Any other failed unit, `mount-shared`, `systemd-journald`
  or `ti-clock-check` among them, stops up.sh with the list. It also checks
  that mount sharing is on, which systemd needs to hand a unit its secrets.

## What the container may do on your Mac

The container is **not privileged**. systemd as its first program needs a
few things a normal container lacks, and it is given only those:

- **Every default capability is dropped, and eleven are added back.** The
  eleven are `CAP_SYS_ADMIN` (systemd gives each unit its own view of the
  files, a read-only system, private temporary folders, a private folder
  for the engine's socket, and hands a unit its secrets), `CAP_SYS_CHROOT`
  (the same private views), `CAP_CHOWN` and `CAP_FOWNER` (the units' runtime
  folders), `CAP_DAC_OVERRIDE` (handing a unit its secrets), `CAP_SETUID`
  and `CAP_SETGID` (running a unit as `ti-trader`), `CAP_SETPCAP`
  (systemd's journal and login services fail without it),
  `CAP_AUDIT_WRITE` (`sudo`, which runs the operator tools, refuses without
  it), `CAP_FSETID` (systemd keeps the journal folders
  setgid, mode 2755, as on any machine; the change of mode drops that bit
  without it) and `CAP_KILL` (how systemd, which is `root`, sends the stop signal to
  a unit that runs as `ti-trader`: without it `systemctl stop` of the engine
  and the container's orderly shutdown end in a forced kill after the
  timeout). The list was found by removing each in turn until the container
  no longer booted, started its units, stopped them (the engine stopped by
  hand and the container stopped and restarted with the engine running) or
  ran the operator tools, and the journal folder kept its mode; Docker's other defaults (`NET_BIND_SERVICE`,
  `MKNOD`, `NET_RAW`, `SETFCAP`) are not needed and are gone.
- **A cgroup namespace of its own** (`--cgroupns private`), whose cgroup
  tree the image makes writable at each start. That tree is the container's
  own; the rest of the virtual machine's is hidden from it.

Docker's default system-call filter stays on, and the container gets no
other capability: it cannot set the clock, load a kernel module or reach a
device directly. By default it also sees none of the virtual machine's disks
or devices, and `/proc/sys`, where the kernel's settings are, is read-only.

Those defaults are not a wall, and the container is not a security boundary.
`CAP_SYS_ADMIN` is the capability to mount and remount file systems, the very
thing the image itself does to make its cgroup tree writable. Any program
running as root inside the container can therefore remount `/proc/sys`
writable and change kernel settings of Docker's Linux virtual machine, which
every container on this Mac shares; it can likewise undo the hidden paths
and the device rules. A compromised root program inside has a much better
chance of breaking out into that virtual machine than in an ordinary
container. From there it could reach every container and volume on this Mac
and the Mac folders Docker Desktop shares into that virtual machine. The
container test checks that it is not privileged, drops every default
capability, adds back only those eleven, and that the container's capability
limit is exactly them; it does not, and cannot, check that root inside stays within these
defaults.

So the container is a rehearsal tool on a machine you control, and its
safety rests on what runs in it, not on Docker: everything in it is code you
installed from your own checkout, the services run as `ti-trader`, not root,
no root program runs in it that you did not put there, and nothing in it
listens beyond the Mac's loopback. No real account's credentials belong in
it.

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

## The clock

No time service runs in the container. Its clock is the clock of Docker's
Linux virtual machine, which Docker Desktop keeps in step with the Mac; the
Mac keeps time with its own "Set time and date automatically". A clock
behind IBKR's makes every account reading stale, and the engine then
withholds every sell (ADR 0019). So the clock is compared with an outside
website, as the server chapter's "The clock" describes:

- when `up.sh` provisions (the `clock` step; up.sh stops if it fails), and
- every 15 minutes after that, by `ti-clock-check.timer`.

A failed check fails the unit `ti-clock-check`. You see it in
`docker exec ti systemctl --failed`, with the reason in
`docker exec ti journalctl -u ti-clock-check`, and the next `up.sh` stops on
it. The reporter does not send an alert for it: a clock that drifts between
checks is seen only there. After the Mac wakes from sleep, the virtual
machine's clock can lag until Docker Desktop corrects it, so do not let the
Mac sleep during a rehearsal you care about.

## What a restart does

A restart of the Mac, of Docker Desktop, or of the container (`docker
restart ti`) has the server's rules:

- The container comes back by itself (Docker's restart policy "unless
  stopped"; Docker Desktop must start when you sign in).
- The data job's timer, the reporter's timer, the clock check's timer and
  the dashboard come back. The reporter fires within 5 minutes. A data job
  firing missed while the Mac was off runs when it is back.
- **The engine and the adapter stay stopped.** systemd never starts them.
  A restart is not a crash, so the engine's next start is a fresh run you
  approve ([restart](restart.md)), then the adapter after it.
- Your files keep their contents, owners and modes. The container test
  (`deploy/mac-mini/test-container.sh`, last run on 2026-10-20) checked a
  chosen set of them byte for byte: a sentinel file in each volume, the
  durable log, `secrets.env`, the adapter's configuration. It checked them
  after a restart, after `up.sh --recreate`, and after a re-creation onto a
  changed image. A journal line written before each stayed readable, and
  `journal-query` read the durable log afterwards. That is not "nothing
  changes": the services and systemd write to the volumes as they run (new
  journal lines, the reporter's state), and the test does not compare those.

## What is not built yet

- The programs for arm64 (`deploy/build-engine.sh` builds for the Lightsail
  server's x86_64) and the adapter's installation in the container. Until
  then `up.sh` installs only what is in the build folder.
- The dashboard on the Mac's loopback. The port is published, but the
  dashboard listens on the container's own loopback, so the Mac cannot reach
  it yet.
- The `ti` command for `edit`, `start`, `stop`, `status` and `logs`. Until
  it exists, use `docker exec -it ti ...`, as the runbook shows.
- **The kill switch.** The kill switch is an instruction you record with
  `operator kill-switch`. The engine reads it from its operator inbox and
  then refuses every order, routine sells included, except a margin
  account's sale back to 98% ([operator](operator.md), "The kill switch").
  The container has no operator inbox yet. It arrives with the change that
  creates the inbox in the engines' service file, on the server and here.
  Until then there is no kill switch in the container. **Stopping the engine
  is not the kill switch**
  (`docker exec ti systemctl stop ti-engine@boost`): it records nothing, it
  leaves every order already resting at IBKR where it is, and there is no
  sale back to 98%. The engine's next start is a fresh run that needs your
  approval ([restart](restart.md)). To pull orders that are resting, cancel
  them in IBKR itself.
- An alert for a failed clock check, nightly backups, and the outside
  heartbeat.

## What can go wrong

| What you see | What the program does | What you do |
| --- | --- | --- |
| `up: refusing the checkout at ...: it is under ~/Desktop` (or Documents, iCloud Drive, CloudStorage) | Stops before it touches Docker; nothing is created or changed | Run it from `~/TrendInvesting-deploy`, a checkout of its own in your home folder (`docs/runbooks/mac-mini.md`) |
| `up: refusing the checkout at ...: it holds N what look like iCloud sync duplicates` | Names the first five and stops before it touches Docker | Delete them, or take a fresh checkout outside the synced folders, then run it again |
| `up: refusing the checkout at ...: could not read all of it` | Stops before it touches Docker | Fix the folder's permissions it names, or take a fresh checkout |
| `up: another up.sh (process N) is working on the container ti` | Stops before it touches Docker | Wait for the other run to finish; if no such process exists, run it again (the lock is taken over) |
| `up: another up.sh (process N) is taking over the stale lock on ti` | Stops before it touches Docker; the lock is left alone | Wait for the other run, then run it again. A takeover link left by a run that was killed is taken over by the next run by itself, also when another program has since been given its process ID. If no `up.sh` runs (`pgrep -fl up.sh`), the message names the link to remove by hand |
| `up: the lock on ti and the links taking it over were all left by runs no longer running` | Stops before it touches Docker | Check that no `up.sh` runs (`pgrep -fl up.sh`), remove the three links it names, then run it again |
| `up: TI_MAC_DASHBOARD_PORT must be a port number from 1 to 65535` | Stops before it touches Docker | Set a port in that range, or leave it unset (8088) |
| `up: Docker's VM is x86_64` | Stops: the container is for Apple silicon only | Run it on the Mac mini |
| `up: the container ti was made by an earlier up.sh, before it recorded the checkout` | Changes nothing and removes nothing: it cannot tell this container from another checkout's | If it is your own rehearsal container: stop its adapter, then its engine, then `docker stop -t 60 ti && docker rm ti` (the volumes are kept), and run `up.sh` again, which creates it anew on the same volumes |
| `up: the container ti is not one up.sh made from this checkout` | Changes nothing and removes nothing | Use another `TI_MAC_NAME`, or run up.sh from the checkout that made it; remove the other container yourself only if you know what it holds |
| `up: the container ti's mounts are not the four volumes` | Changes nothing and removes nothing: files kept outside the volumes would be lost | Find out what the extra or missing mount is before removing the container by hand |
| `up: the volume ti-etc is not one up.sh made from this checkout` | Changes nothing and removes nothing: provisioning would overwrite the programs and configuration of the rehearsal that owns it | If the path it names is this checkout's old place, move the checkout (or a fresh clone) back there and run `up.sh` again; the volumes are kept. Otherwise run up.sh from the checkout that made it, or, if that rehearsal's contents may go, remove its volumes once no container uses them (`docker volume rm ti-etc ti-state ti-opt ti-journal`) |
| `up: the volume ti-etc was made by an earlier up.sh, before it recorded the checkout` | Changes nothing and removes nothing: it cannot be told apart from another checkout's | If it is your own rehearsal's and its contents may go: with no container, remove the four volumes (`docker volume rm ti-etc ti-state ti-opt ti-journal`); if the container `ti` exists, first stop its adapter and then its engine (`docker exec ti systemctl stop ti-adapter-boost` && `docker exec ti systemctl stop ti-engine@boost`), then `docker stop -t 60 ti && docker rm ti` (never `docker rm -f`), then remove the four volumes, or keep them and use another `TI_MAC_NAME`. Then run `up.sh` again, which creates them anew |
| `up: cannot inspect the volume ti-etc` | Changes nothing and creates nothing: Docker failed for a reason other than the volume not existing (the message shows it) | Fix what Docker reports (for example, start Docker Desktop), then run `up.sh` again |
| `up: refusing to remove ti while ti-engine@boost.service runs` | `--recreate` changes nothing | Stop the adapter, then the engine, then run `up.sh --recreate` again |
| `up: cannot ask systemd in ti which units run` | `--recreate` changes nothing: an engine might be running | Read `docker logs ti`; once `docker exec ti systemctl list-units` answers, check the engine and adapter are stopped and run `up.sh --recreate` again |
| `up: systemd in ti did not finish starting` | Stops before copying anything in | Read `docker logs ti`; run `up.sh` again, or `up.sh --recreate` (the volumes are kept) |
| `up: refusing to provision ti: these units failed: ...` | Stops before copying anything in; the list names each failed unit | Read `docker exec ti journalctl -u <unit>` for each; fix the cause, then run `up.sh` again |
| `provision: the clock is not good` during `up.sh` | The clock step refuses, and `up.sh` stops before the services step | Turn on the Mac's "Set time and date automatically", then run `up.sh` again ([server](server.md), "The clock") |
| `ti-clock-check` failed (`systemctl --failed` inside) | The engine is not stopped by it; a clock behind IBKR's makes it withhold every sell | Read `journalctl -u ti-clock-check`; fix the Mac's time (or wait for Docker Desktop to correct it after sleep); `systemctl start ti-clock-check` to check again |
| `provision: UID 2002 (or 2001, 2003) is held by the account '...' in this image` | Stops before creating the service users | A changed image took the ID; remove that account from the image, then `up.sh --recreate` |
| `provision: ti-trader is 998:998 here, not 2002:2002` (or `ti-operator is UID 997 here, not 2003`) | Stops before changing anything: the container was provisioned before the IDs were fixed | `up.sh --recreate` gives the users their fixed IDs, but does not re-own the files already in the volumes, which stay at the old IDs. After it, run the `reown` step the message gives, inside the container (`docker exec --env TI_TARGET=mac-mini ti /root/deploy/provision.sh reown ti-trader 998 998`): it moves every file in the volumes of the old UID and GID to the fixed ones and says how many. It always refuses an old ID that root (0) or a service user (2001 to 2003) holds, since those files are that account's own. An old ID that only collides with another account the new image has (a system account its packages added at the number the old service user had) is refused too, naming the account; if the files at that ID are the old service user's, run the same command again with `--force-id` (`... reown ti-trader 995 995 --force-id`) and it moves them, or remove or renumber that account in the image and run `up.sh --recreate` again. If it cannot list a volume it stops with `could not list`, never a count. Then run `up.sh` again |
| `up: note: ti runs the image ..., not ...` | Keeps the running container and provisions it as before | Run `up.sh --recreate` when it suits you; every volume is kept |
| After a Mac or Docker Desktop restart, the engine and the adapter are stopped | Nothing: this is the rule, and the timers and dashboard are back | Start the engine as a fresh approved run and then the adapter ([restart](restart.md)) |
| `systemctl is-system-running` inside says `degraded`, with `ti-reporter@boost` failed | The reporter fails every 5 minutes until the Boost engine has run once and `secrets.env` holds its channels | Expected before the first run; otherwise read `journalctl -u ti-reporter@boost` ([alerts and dashboard](alerts-and-dashboard.md)) |
| An Intel (amd64) container fails on this Mac with `exec format error` | Not this container: its image never touches Docker's program handlers. An older image, or another systemd container, may have cleared them | Quit and reopen Docker Desktop, which registers Rosetta's handler again; check that `ti` runs the current image (`up.sh` says so if not) |
| The reporter or the data job fails, saying its secrets file cannot be read | systemd could not hand the unit its secrets; the unit fails and does nothing | Check that `ti` runs the current image, which turns mount sharing on at boot; run `up.sh --recreate` if it does not |
| Docker Desktop is reset or uninstalled | The virtual machine and every volume are gone | Run `up.sh` again for a fresh rehearsal container; nothing in it was a real run's record |

## What backs this

- `deploy/provision.sh` (`TI_TARGET=mac-mini`), `deploy/mac-mini/up.sh`,
  `deploy/mac-mini/Dockerfile` and the clock check's units in
  `deploy/mac-mini/systemd/`.
- `deploy/rehearsal/test-target.sh` checks the provisioner's two targets:
  arm64 accepted only on the Mac mini; the Gateway masked, not enabled;
  Tailscale and the firewall skipped there and unchanged on Lightsail; the
  service users at fixed IDs, and refused when another account holds one;
  the clock check's timer on the Mac mini only. It never changes the machine
  it runs on: every path a file command is given, relative ones and `dd`'s
  and `sed`'s in-place forms and `cp -t/dir` included (a `sed` script, even
  one starting with `/`, is not a path and is not refused), is resolved against the test's own
  temporary folder, and one outside it is refused and fails the test; the
  test proves that refusal works for each of those shapes. A redirection
  cannot be refused that way, so the test also checks that the provisioner
  redirects only to `/dev/null` or under the folders it sets inside its own:
  never to a literal path, quoted or not, `~` or a command substitution (`$(...)` or backticks).
  `deploy/mac-mini/test-refusals.sh` checks every refusal of the launcher
  listed above, the order of stop and removal on `--recreate`, and that the
  container is created without `--privileged`. Both run in `make check`.
- `deploy/mac-mini/test-container.sh`, run on the Mac mini, makes a
  throwaway container with the launcher and checks it against the running
  systemd. Every unit verifies. The timers, the dashboard and a stand-in
  engine start with their restrictions and their secrets. The chosen files
  keep their bytes, owners and modes after a restart, a re-creation and a
  re-creation onto a changed image, and the service users keep their IDs.
  The `reown` step moves files left at an old UID and GID in the volumes to
  the fixed IDs, the links themselves and not what they point to, and
  leaves root's files alone.
  `--recreate` is refused while the engine runs. The reporter fires within 5
  minutes. The engine stays stopped. The clock step and the clock check's
  unit pass and the step refuses a clock an hour off. Docker's program
  handlers are left alone: they are listed before the container is made and
  after every boot, and must not change. It also checks that an Intel
  (amd64) container still runs, unless that was already broken before the
  test; then it says "amd64 check NOT RUN" and compares only the listing.
  In the run of 2026-10-20 the amd64 check ran and passed before the test and after every boot, the re-creation onto a changed image included.
