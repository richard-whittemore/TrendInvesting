# Runbook: the Mac mini (deploy/mac-mini)

*Steps for the Mac mini's rehearsal container: a local copy of the server
with a real systemd, for trying the provisioner, the units and your own
steps before doing them on the Lightsail server, where paper trading runs
(`docs/runbooks/lightsail-deployment.md`). What it is and why: the manual
chapter [mac-mini](../manual/mac-mini.md). Today this runbook has one step:
creating the container, or re-attaching to it.*

## Before you start

- The Mac mini, signed in, with Docker Desktop installed and running (Docker
  Desktop's setting "Start Docker Desktop when you sign in" on).
- Git, and Go for the builds (`CONTRIBUTING.md`).
- About 2 GB free for the image and the volumes.

## 1. Create the container, or re-attach to it

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

3. **Build the programs into `dist/`**, as the server runbook does
   (`docs/runbooks/lightsail-deployment.md`). The arm64 builds are not in
   `deploy/build-engine.sh` yet; until they are, `up.sh` installs whatever
   `dist/` holds and names each program it does not find.
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

   Expect `running`, or `degraded` with only `ti-reporter@boost`,
   `ti-dashboard` or `ti-data` failed before the Boost engine has run once
   and `secrets.env` is filled in (up.sh refuses any other failed unit).
   Expect `ti-data.timer`, `ti-reporter@boost.timer` and
   `ti-clock-check.timer` active, `ti-gateway` masked, and `ti-engine@boost`
   and `ti-adapter-boost` inactive.
6. **Fill in the secrets inside the container** (never on the Mac, never in
   a chat). It is a rehearsal: use the paper account's details, or stand-ins,
   never a live account's:

   ```
   docker exec -it ti nano /etc/trend-investing/secrets.env
   docker exec -it ti nano /etc/trend-investing/accounts/boost.env
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
