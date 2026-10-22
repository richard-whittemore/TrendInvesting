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
macOS user of its own, and the code its sessions write runs only as a
second user, in a sandbox. What it does, what the session and the gate can
and cannot do, its cost cap and its off switch: the manual chapter
[alerts and dashboard](../manual/alerts-and-dashboard.md), "Alerts analysed
by Claude". **Owner steps** are marked **[OWNER]**; agents never create
tokens or enter them.

1. **[OWNER] A macOS user of its own.** System Settings, Users & Groups, Add
   User: a Standard user named `ti-triage`. Never sign it in to iCloud,
   never give it an SSH key, never run `gh auth login` as it. Close its home
   to every other user (macOS opens new homes to the `staff` group):

   ```
   sudo chmod 700 /Users/ti-triage
   ```

   Keep the Mac mini awake (System Settings, Energy: prevent automatic
   sleeping) and on the tailnet.
2. **[OWNER] The gate's user, group and sudo rule.** The gate runs the code
   a session wrote (`go test`, `make check`) as `ti-triage-gate`, a user
   that cannot log in and cannot read `ti-triage`'s home or tokens:

   ```
   sudo sysadminctl -addUser ti-triage-gate -fullName "TrendInvesting triage gate" -shell /usr/bin/false -home /var/empty -password "$(openssl rand -base64 24)"
   sudo dseditgroup -o create ti-triage-gate
   sudo dseditgroup -o edit -a ti-triage-gate -t user ti-triage-gate
   echo 'ti-triage ALL=(ti-triage-gate) NOPASSWD: /usr/bin/sandbox-exec, /bin/kill' | sudo tee /etc/sudoers.d/ti-triage-gate
   sudo chmod 440 /etc/sudoers.d/ti-triage-gate
   sudo visudo -cf /etc/sudoers.d/ti-triage-gate
   ```

   The password is random and never used. The rule lets `ti-triage` run
   two programs as the gate user and nothing else: `sandbox-exec` (every
   gate command runs inside it, with a profile the job builds that denies
   the network, loopback aside, and writes outside the gate's run folder
   and the shared temporary folders) and `kill` (to end the gate user's
   processes after each command). `visudo -c` must say "parsed OK".
3. **[OWNER] The gate's folder.** It holds the gate's runs, the module cache
   and Go's vulnerability database; the job owns it, the gate user may only
   read it and write in its `runs` folder:

   ```
   sudo install -d -o ti-triage -g ti-triage-gate -m 750 /Users/Shared/ti-triage-gate
   ```

   The job fills the module cache itself, as `ti-triage`, with network,
   before each session (`go mod download`, then `go mod tidy -diff`, which
   reads every module `make check`'s own `go mod tidy -diff` will), in
   `/Users/Shared/ti-triage-gate/modcache`; the gate reads it offline
   (`GOFLAGS=-mod=readonly GOPROXY=off`) and cannot change it. Before the
   gate's `make check` it fetches Go's vulnerability database
   (`https://vuln.go.dev/vulndb.zip`) into
   `/Users/Shared/ti-triage-gate/vulndb`, which `make vuln` reads through
   `GOVULNDB`, since the gate has no network. Nothing to do here but create
   the folder. To use the golden scenarios in the gate, copy the golden
   folder where the gate user can read it
   (`sudo install -d -o ti-triage -g ti-triage-gate -m 750 /Users/Shared/ti-golden`,
   then `sudo cp -R <golden>/. /Users/Shared/ti-golden/ && sudo chown -R ti-triage:ti-triage-gate /Users/Shared/ti-golden && sudo chmod -R g+rX,o-rwx /Users/Shared/ti-golden`)
   and set `TI_GOLDEN_DIR` to it in the plist (step 10).
4. **Its tools.** Go from Homebrew (`/opt/homebrew/bin/go`), at the version
   `go.mod` names: the gate builds with `GOTOOLCHAIN=local`, so an older Go
   fails every check (`/usr/local/go`, if present, comes after it on the
   plist's `PATH`). Install Claude Code for `ti-triage`, from the official
   installer, as that user (`sudo -iu ti-triage`, then the install command
   from Claude Code's documentation); it lands in
   `/Users/ti-triage/.local/bin`. Its sandbox on macOS is built in; the job
   asks it to fail rather than run unsandboxed.
5. **Its own clone, outside the synced folders** (read-only `https`, no
   credential):

   ```
   sudo -iu ti-triage git clone https://github.com/richard-whittemore/TrendInvesting.git /Users/ti-triage/TrendInvesting-triage
   ```

6. **[OWNER] The GitHub tokens.** The alert issues live in a private
   repository of their own (the alerts repository); the code and the fix
   PRs in the public one. The reporter's `TI_GITHUB_ISSUES_REPO` on the
   server must name that same private alerts repository. GitHub gives a
   fine-grained token its permissions on every repository it selects, so
   the least each needs takes two tokens. On GitHub: Settings, Developer
   settings, Personal access tokens, Fine-grained tokens, Generate new
   token, twice, each with an expiry (90 days) and "Only select
   repositories":
   - `ti-triage-alerts`: the alerts repository only; under "Repository
     permissions" **Issues** "Read and write", everything else "No access"
     (Metadata read-only is added by GitHub);
   - `ti-triage-code`: the code repository only; **Contents** and **Pull
     requests** "Read and write", everything else "No access". It needs
     Contents to push a fix's branch, which is why `main` must stay
     protected: check that the repository's ruleset for `main` requires a
     pull request, so this token can never push to `main` itself.

   Put each in a file only `ti-triage` can read, in a folder no other user
   can read, typed on the terminal, never on a command line:

   ```
   sudo -u ti-triage sh -c 'umask 077; mkdir -p /Users/ti-triage/.config/ti-triage; cat > /Users/ti-triage/.config/ti-triage/github-token'
   sudo -u ti-triage sh -c 'umask 077; cat > /Users/ti-triage/.config/ti-triage/code-token'
   ```

   Paste the token (the alerts one first, then the code one), press Return,
   then Ctrl-D. The job reads both into memory when each poll starts; the
   session's commands cannot read the folder (Claude Code's sandbox denies
   it) and the gate user cannot either. If the reporter's token
   (`TI_GITHUB_ISSUES_TOKEN` on the server) belongs to a different GitHub
   user than the alerts token, add `-alert-author <that login>` to the
   plist (step 10). Put the expiries in your calendar; to rotate, replace a
   file's line and delete the old token. One token selecting both
   repositories also works (leave out `-code-token-file`), but it then
   holds Contents and Pull requests on the alerts repository and Issues on
   the code one as well.
7. **[OWNER] The Claude token.** As yourself, run `claude setup-token` and
   sign in with the subscription the job should use; it prints a long-lived
   token. Store it the same way, in
   `/Users/ti-triage/.config/ti-triage/claude-token`. Its reach: it makes
   model calls billed to that subscription, nothing more (it opens no
   repository, server or broker). The job hands it to its sessions only, in
   their environment, where a command the session runs could read it but
   not send it anywhere (no network in the sandbox); the job removes its
   text from every comment and every push; the gate never has it.
8. **Build the program** from the clone, at `main`, with no changes:

   ```
   sudo -iu ti-triage sh -c 'cd /Users/ti-triage/TrendInvesting-triage && git fetch origin && git checkout --detach origin/main && git status --short && mkdir -p /Users/ti-triage/bin /Users/ti-triage/triage && go build -o /Users/ti-triage/bin/ti-triage ./cmd/triage'
   ```

   `git status --short` must print nothing. To upgrade later, run the same
   line again; a session it interrupts is analysed again at the next poll.
9. **[OWNER] The read-only access path.** The job needs no key for the
   server: it reads the dashboard's `/api/journal-slice` over the tailnet,
   which answers reads only, with no credential and no figure. Check three
   things:
   - the server runs a dashboard build that serves it: from the Mac mini,
     `curl -sS https://<server>.<tailnet>.ts.net/api/journal-slice | head`
     prints `## <log>: covers ...` (a release with this change,
     `docs/runbooks/lightsail-deployment.md`, "Upgrading");
   - the tailnet's policy admits to the dashboard only your own devices and
     the Mac mini (`docs/runbooks/lightsail-deployment.md`, step 12, has the
     grant);
   - the tailnet lets nothing on the Mac mini log in to the server by
     itself. Tailscale SSH trusts the machine, not the macOS user, so in the
     tailnet's policy file make sure no SSH rule with `"action": "accept"`
     admits the Mac mini to the server; keep `"check"` (which asks you to
     sign in again in a browser) or allow SSH to the server from your laptop
     only. This is the step that makes "no write path to the server" true
     for every process on the Mac mini, not only for the job.
10. **Install the launchd job.** Edit the copy's `-alerts-repo` to the
    private alerts repository and `-slice-url` to the server's tailnet
    name, then:

    ```
    cp /Users/ti-triage/TrendInvesting-triage/deploy/mac-mini/triage/com.trendinvesting.triage.plist /tmp/ti-triage.plist
    nano /tmp/ti-triage.plist        # replace <alerts-repo> and <server>.<tailnet>
    plutil -lint /tmp/ti-triage.plist
    sudo install -o root -g wheel -m 644 /tmp/ti-triage.plist /Library/LaunchDaemons/com.trendinvesting.triage.plist
    sudo launchctl bootstrap system /Library/LaunchDaemons/com.trendinvesting.triage.plist
    ```

    It runs at once and then every five minutes. To use the golden
    scenarios in the gate's `make check`, add `TI_GOLDEN_DIR` (step 3's
    folder) to the plist's `EnvironmentVariables`. To change a setting (such
    as `-daily-cap`), edit the installed plist, then `sudo launchctl bootout
    system/com.trendinvesting.triage` and bootstrap it again.
11. **Check it.** `sudo cat /Users/ti-triage/triage/triage.log` shows each
    poll's errors and the job's notices. A quiet log does not prove the job
    runs, nor that it analysed nothing: `sudo launchctl print
    system/com.trendinvesting.triage` shows its runs and last exit status,
    and the issues' comments show what it analysed. The first poll records
    when the job began: only alert issues opened after it are analysed. The
    next alert issue should have its comment within about five minutes; a
    comment "triage failed: gate unavailable" means steps 2 or 3 need
    another look.

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
