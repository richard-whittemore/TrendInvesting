# Runbook: the Mac mini (deploy/mac-mini)

*Steps for the Mac mini's rehearsal container: a local copy of the server
with a real systemd, for trying the provisioner, the units and your own
steps before doing them on the Lightsail server, where paper trading runs
(`docs/runbooks/lightsail-deployment.md`). What it is and why: the manual
chapter [mac-mini](../manual/mac-mini.md). Its steps: building the programs and creating the container, or
re-attaching to it.*

## Before you start

- The Mac mini, signed in, with Docker Desktop installed and running (Docker
  Desktop's setting "Start Docker Desktop when you sign in" on).
- Git, and Go for the builds (`CONTRIBUTING.md`).
- About 2 GB free for the image and the volumes.

## 1. Build the programs, create the container, or re-attach to it

1. **A checkout of its own, outside the synced folders.** Once:

   ```
   git clone https://github.com/richard-whittemore/TrendInvesting.git ~/TrendInvesting-deploy
   ```

   Never use the copy on the Desktop: the launcher refuses it, and anything
   under `~/Documents`, iCloud Drive or `~/Library/CloudStorage`, and any
   checkout holding a sync duplicate (`name 2.py`, `folder 2`).
2. **Bring it to the version you mean to run**, with no changes of your own:

   ```
   cd ~/TrendInvesting-deploy
   git fetch origin && git checkout --detach origin/main
   git status --short        # must print nothing
   ```

3. **Build the programs for arm64**, into a folder of their own, never
   `dist/` (Lightsail's) and outside the checkout (an untracked folder in it
   makes the tree dirty, and the build refuses):

   ```
   deploy/build-engine.sh --arch arm64 --out "$HOME/ti-arm64"
   deploy/build-data.sh "$HOME/ti-arm64/ti-data"
   ```

   Each prints the stamp. To rehearse them first, run
   `deploy/mac-mini/rehearse-programs.sh`: it builds into a scratch folder
   of its own and ends with `programs rehearsal passed`. Then tell the
   launcher where they are: `export TI_DIST="$HOME/ti-arm64"`. It installs
   whatever that folder holds and names each program it does not find.
4. **Run the launcher:**

   ```
   deploy/mac-mini/up.sh
   ```

   The first time it builds the image, creates the volumes `ti-etc`,
   `ti-state`, `ti-opt` and `ti-journal`, creates and starts the container
   `ti`, and provisions it. A later run starts the container if it is stopped
   and provisions it again; nothing you filled in is overwritten. It ends
   with `up: ti is up`.
5. **Check it:**

   ```
   docker exec ti systemctl is-system-running
   docker exec ti systemctl list-units 'ti-*' --all
   ```

   Expect `running`, or `degraded` with only these units failed, each for its
   own reason:
   - `ti-reporter@boost` and `ti-dashboard` may fail until the Boost engine
     has run once (there is no log yet).
   - `ti-data` may fail until its Massive key is filled in: `MASSIVE_API_KEY`
     in `/etc/trend-investing/secrets.env`.

   "Degraded accepted" means up.sh lets `degraded` through only when every
   failed unit is one of `ti-reporter@*.service`, `ti-dashboard.service` or
   `ti-data.service`. It checks the unit names, not the reasons above, so
   check the reason yourself. Any other failed unit (`mount-shared`,
   `systemd-journald`, `ti-clock-check`...) stops up.sh with the list.
   Expect `ti-data.timer`, `ti-reporter@boost.timer` and
   `ti-clock-check.timer` active, `ti-gateway` masked, and `ti-engine@boost`
   and `ti-adapter-boost` inactive.
6. **Fill in the secrets inside the container** (never on the Mac, never in
   a chat). It is a rehearsal: use the paper account's details, or stand-ins,
   never a live account's:

   ```
   docker exec -it ti ti-secrets edit secrets.env
   docker exec -it ti ti-secrets edit accounts/boost.env
   ```

   The secret files are kept encrypted, as on the server: the first
   provisioning asked you to choose a passphrase, and each edit asks for it.
   After every start of the container, unlock them, or every unit that needs
   a secret refuses to start: `docker exec -it ti ti-unlock`
   (`docs/manual/server.md`, "Unlocking the secrets after a boot").
   Every command that asks for the passphrase refuses while any swap is on,
   and Docker Desktop's virtual machine has swap on unless you turn it off:
   `docker exec ti cat /proc/swaps` must list only its heading line. Turn it
   off in Docker Desktop's settings (Resources) before provisioning
   (`docs/manual/server.md`, "Swap must stay off").

7. **Install the TWS API for the adapter.** Not part of `up.sh`. It fetches
   IBKR's Mac/Unix archive (the Linux installer is x86_64 only), checks its
   SHA-256 against the pin (a mismatch stops it with nothing installed),
   installs its Python client and the pinned protobuf into the adapter's
   environment, and writes the record the adapter checks at start:

   ```
   docker exec --env TI_TARGET=mac-mini ti /root/deploy/provision.sh twsapi
   docker exec --env TI_TARGET=mac-mini ti /root/deploy/provision.sh adapter
   ```

**If the launcher says the image changed**, or the container is damaged:
stop the adapter and then the engine if they run
(`docker exec ti systemctl stop ti-adapter-boost`, then
`docker exec ti systemctl stop ti-engine@boost`), then run
`deploy/mac-mini/up.sh --recreate`. It refuses while either runs; otherwise
it stops the container (systemd shuts its units down, up to 60 seconds),
removes it and creates it again, keeping all four volumes. Do not run
`docker volume rm ti-...` or reset Docker Desktop unless you mean to start
the rehearsal again from nothing: the volumes hold its configuration, its
secrets and its durable logs, and nothing backs them up.

**If up.sh says another up.sh is working on the container**, wait for it.
If no such process exists any more, run up.sh again: it takes the lock over.

## Checking the launcher itself

On the Mac mini, from any clean checkout (the data job's build refuses uncommitted changes):

```
deploy/mac-mini/test-container.sh
```

It uses a throwaway container and volumes named `ti-test-<number>`, and a
stand-in for ntfy and PagerDuty on the Mac's loopback, and removes them at
the end. It never touches `ti`. It takes about 20 minutes and needs the
network. It must end with `REAL CONTAINER TEST COMPLETE`. If Intel (amd64)
containers already fail on this Mac before the test (quitting and reopening
Docker Desktop usually mends it), run it with
`TI_TEST_AMD64_BASELINE_BROKEN=1`: it then prints `amd64 check NOT RUN` and
compares only Docker's list of program handlers.
