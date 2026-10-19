# Runbook: the Mac mini (deploy/mac-mini)

*Steps for the Mac mini's container. What it is and why: the manual chapter
[mac-mini](../manual/mac-mini.md). This runbook grows with the Mac mini
plan's slices. Today it has its first step: creating the container, or
re-attaching to it.*

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

   Expect `running`, or `degraded` with only `ti-reporter@boost` failed
   before the Boost engine has run once and `secrets.env` is filled in. Expect
   `ti-data.timer` and `ti-reporter@boost.timer` active, `ti-gateway` masked,
   and `ti-engine@boost` and `ti-adapter-boost` inactive.
6. **Fill in the secrets inside the container** (never on the Mac, never in
   a chat):

   ```
   docker exec -it ti nano /etc/trend-investing/secrets.env
   docker exec -it ti nano /etc/trend-investing/accounts/boost.env
   ```

**If the launcher says the image changed**, or the container is damaged:
`deploy/mac-mini/up.sh --recreate` removes the container and creates it
again, keeping all four volumes. **Never** run `docker volume rm ti-...` or
reset Docker Desktop: the volumes hold the configuration, the secrets and
the durable logs, and no backup exists yet.

## Checking the launcher itself

On the Mac mini, from any clean checkout (the data job's build refuses uncommitted changes):

```
deploy/mac-mini/test-container.sh
```

It uses a throwaway container and volumes named `ti-test-<number>` and
removes them at the end. It never touches `ti`. It takes about 15 minutes
and needs the network. It must end with `REAL CONTAINER TEST COMPLETE`.
