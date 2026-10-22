# Runbook: the Mac mini (deploy/mac-mini)

*Steps for the Mac mini's rehearsal container: a local copy of the server
with a real systemd, for trying the provisioner, the units and your own
steps before doing them on the Lightsail server, where paper trading runs
(`docs/runbooks/lightsail-deployment.md`). What it is and why: the manual
chapter [mac-mini](../manual/mac-mini.md). Its steps: building the programs and creating the container, or
re-attaching to it; and, outside the container, the alert triage job.*

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
   docker exec -it ti nano /etc/trend-investing/secrets.env
   docker exec -it ti nano /etc/trend-investing/accounts/boost.env
   ```

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

## 2. The alert triage job

Not part of the container: it runs on the Mac itself, under launchd, as a
macOS user of its own. What it does, what it can and cannot do, its cost cap
and its off switch: the manual chapter
[alerts and dashboard](../manual/alerts-and-dashboard.md), "Alerts analysed
by Claude". **Owner steps** are marked **[OWNER]**; agents never create
tokens or enter them.

1. **[OWNER] A macOS user of its own.** System Settings, Users & Groups, Add
   User: a Standard user named `ti-triage`. Never sign it in to iCloud,
   never give it an SSH key, never run `gh auth login` as it: what it holds
   is all the job, and the code its sessions write, can reach. Keep the
   Mac mini awake (System Settings, Energy: prevent automatic sleeping) and
   on the tailnet.
2. **Its tools.** Go is the system's (`/usr/local/go`). Install Claude Code
   for that user, from the official installer, as that user
   (`sudo -iu ti-triage`, then the install command from Claude Code's
   documentation); it lands in `/Users/ti-triage/.local/bin`.
3. **Its own clone, outside the synced folders** (read-only `https`, no
   credential):

   ```
   sudo -iu ti-triage git clone https://github.com/richard-whittemore/TrendInvesting.git /Users/ti-triage/TrendInvesting-triage
   ```

4. **[OWNER] The triage token.** On GitHub: Settings, Developer settings,
   Personal access tokens, Fine-grained tokens, Generate new token. Name it
   `ti-triage`, set an expiry (90 days), "Only select repositories" with the
   one repository, and under "Repository permissions" set **Contents**,
   **Issues** and **Pull requests** to "Read and write" and leave every other
   permission at "No access" (Metadata read-only is added by GitHub). It
   needs Contents to push a fix's branch, which is why `main` must stay
   protected: check that the repository's ruleset for `main` requires a pull
   request, so this token can never push to `main` itself. Put it in a file
   only `ti-triage` can read, typed on the terminal, never on a command line:

   ```
   sudo -u ti-triage sh -c 'umask 077; mkdir -p /Users/ti-triage/.config/ti-triage; cat > /Users/ti-triage/.config/ti-triage/github-token'
   ```

   Paste the token, press Return, then Ctrl-D. If the reporter's token
   (`TI_GITHUB_ISSUES_TOKEN` on the server) belongs to a different GitHub
   user than this one, add `-alert-author <that login>` to the plist (step
   7). Put the expiry in your calendar; to rotate, replace the file's line
   and delete the old token.
5. **[OWNER] The Claude token.** As yourself, run `claude setup-token` and
   sign in with the subscription the job should use; it prints a long-lived
   token. Store it the same way, in
   `/Users/ti-triage/.config/ti-triage/claude-token`. The job hands it only
   to its sessions, never to the gate's commands or to git.
6. **Build the program** from the clone, at `main`, with no changes:

   ```
   sudo -iu ti-triage sh -c 'cd /Users/ti-triage/TrendInvesting-triage && git fetch origin && git checkout --detach origin/main && git status --short && mkdir -p /Users/ti-triage/bin /Users/ti-triage/triage && go build -o /Users/ti-triage/bin/ti-triage ./cmd/triage'
   ```

   `git status --short` must print nothing. To upgrade later, run the same
   line again.
7. **[OWNER] The read-only access path.** The job needs no key for the
   server: it reads the dashboard's `/api/journal-slice` over the tailnet,
   which answers reads only, with no credential at all. Check two things:
   - the server runs a dashboard build that serves it: from the Mac mini,
     `curl -sS https://<server>.<tailnet>.ts.net/api/journal-slice | head`
     prints `## <log>: journal-query refusals` (a release with this change,
     `docs/runbooks/lightsail-deployment.md`, "Upgrading");
   - the tailnet lets nothing on the Mac mini log in to the server by
     itself. Tailscale SSH trusts the machine, not the macOS user, so in the
     tailnet's policy file make sure no SSH rule with `"action": "accept"`
     admits the Mac mini to the server; keep `"check"` (which asks you to
     sign in again in a browser) or allow SSH to the server from your laptop
     only. This is the step that makes "no write path to the server" true
     for every process on the Mac mini, not only for the job.
8. **Install the launchd job.** Edit the copy's `-slice-url` to the
   server's tailnet name, then:

   ```
   cp /Users/ti-triage/TrendInvesting-triage/deploy/mac-mini/triage/com.trendinvesting.triage.plist /tmp/ti-triage.plist
   nano /tmp/ti-triage.plist        # replace <server>.<tailnet>
   plutil -lint /tmp/ti-triage.plist
   sudo install -o root -g wheel -m 644 /tmp/ti-triage.plist /Library/LaunchDaemons/com.trendinvesting.triage.plist
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.trendinvesting.triage.plist
   ```

   It runs at once and then every five minutes. To use the golden scenarios
   in the gate's `make check`, add `TI_GOLDEN_DIR` to the plist's
   `EnvironmentVariables`. To change a setting (such as `-daily-cap`), edit
   the installed plist, then `sudo launchctl bootout
   system/com.trendinvesting.triage` and bootstrap it again.
9. **Check it.** `sudo cat /Users/ti-triage/triage/triage.log` shows each
   poll's error, if any; a quiet log is a poll that found nothing new.
   `sudo launchctl print system/com.trendinvesting.triage` shows its last
   exit status. The first poll records when the job began: only alert
   issues opened after it are analysed. The next alert issue should have its
   comment within about five minutes.

**Switching it off:** `sudo -u ti-triage touch /Users/ti-triage/triage/OFF`;
delete the file to switch it on again. The manual chapter has the rest.

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
