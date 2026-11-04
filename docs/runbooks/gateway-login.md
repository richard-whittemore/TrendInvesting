# Runbook: the gateway will not log in

Boost's IBKR Gateway runs on the server under IBC (`ti-gateway`). The
Traditional IRA uses its own `ti-gateway@trad-ira` on port 4003 (ADR 0025). Setup and the
reconnect drill are in `lightsail-deployment.md`; this runbook is what to
do when it will not log in, and what the engines do meanwhile.

## The Traditional IRA's Gateway

The instance stages `/run/trend-investing/secrets/ibkr-login-trad-ira.env`
directly; `ti-gateway-login` changes only the joint service's selection.
Start/stop it with `sudo systemctl start ti-gateway@trad-ira.service` or
`sudo systemctl stop ti-gateway@trad-ira.service`; check
`sudo journalctl -u ti-gateway@trad-ira.service --since today` and port 4003.
Its counter and reset-wait state are in
`/var/lib/ti-gateway-trad-ira/login-failures`; never clear the joint's counter
to recover this login. Its healer reservation is under
`/var/lib/ti-gateway-heal/trad-ira/`. See
[The Traditional IRA's Gateway](../manual/server.md#the-traditional-iras-gateway)
for installation, adapter port migration and status checks. Reporter Gateway
checks still cover the joint only. The procedures below describe the joint
selector; use the instance unit and paths above for Traditional IRA diagnosis.

## Login files and selection

Owner decision, 2026-11-02: each IBKR login has its own encrypted file
(ADR 0028). Account files under `accounts/` remain separate.

| Selector | Login file | Purpose |
| --- | --- | --- |
| `joint` | `ibkr-login-joint.env` | Boost's joint paper login |
| `trad-ira` | `ibkr-login-trad-ira.env` | Traditional IRA paper login |
| `roth-ira` | `ibkr-login-roth-ira.env` | Roth IRA paper login |
| `live` | `ibkr-login-live.env` | The owner's real login, reaching the joint account and both IRAs |

`/etc/trend-investing/gateway-login` is non-secret and contains exactly one
of those words, with an optional final newline. Provisioning creates it once
as `joint` and never overwrites it. It is separate from `gateway.env`, which
provisioning rewrites. A missing or invalid selector refuses Gateway startup
with exit 64; it never falls back to another login.

**Existing joint file: no owner command is needed after installation.**
Provisioning's `services` step renames `ibkr-login.env.gpg` to
`ibkr-login-joint.env.gpg`, and its unlocked copy if present, without asking
for a passphrase. `sudo ti-secrets migrate` also performs this rename.
Rerunning it completes a partial rename, including an unlocked copy still
under the old name. Complementary encrypted and on-disk plaintext names
require a passphrase to verify that their contents match before renaming.
If both encrypted copies or both plaintext copies exist, migration refuses
and names them. Preserve both and resolve which file to keep before retrying.
Ordinary unlocks, edits and new passphrase rotations require migration first.
Resume an interrupted rotation with `sudo ti-secrets passphrase --resume`.
Its worklist includes the legacy login, preserving the shared passphrase.
The old name is no longer accepted by `ti-secrets edit`. A legacy plain-text
login is renamed too, then follows the normal encryption migration.

The services step runs migration before installing any Gateway files. A
refused migration stops the step with a nonzero exit before installing the
renderer, staging script, unit or selector. After migration, the staging
script, renderer and unit are installed together, immediately followed by
`daemon-reload`. Provisioning leaves Gateway restarts to the owner and reports
a failed Gateway so the owner can resolve the error and restart it.

Create optional login files on their first edit:

```sh
sudo SUDO_EDITOR=nano ti-secrets edit ibkr-login-trad-ira.env
sudo SUDO_EDITOR=nano ti-secrets edit ibkr-login-roth-ira.env
sudo SUDO_EDITOR=nano ti-secrets edit ibkr-login-live.env
```

Each command verifies the passphrase against an existing encrypted secret
before opening the editor or creating a file. The paper templates have blank
`TWS_USERID` and `TWS_PASSWORD`, `TWS_TRADING_MODE=paper` and
`TWS_READONLY_API=yes`, with no second-factor line. The live template has
blank credentials, `TWS_TRADING_MODE=live`, `TWS_READONLY_API=yes` and
`TWS_SECOND_FACTOR_DEVICE=IB Key`. Fill credentials in the editor only.
Closing an unchanged first-edit template creates nothing. Save an actual change
to create the optional encrypted file. An editor error creates nothing.
Live mode requires an explicit
`TWS_READONLY_API=yes` or `no`; leave it `yes` until the trading gates permit
orders. Selecting a login does not authorize orders or change adapter accounts.

Show or switch the selection without editing any secret:

```sh
sudo ti-gateway-login
sudo ti-gateway-login joint
sudo ti-gateway-login trad-ira
sudo ti-gateway-login roth-ira
sudo ti-gateway-login live
```

The commands with an argument are alternatives: choose the login to use.
They durably replace the selector, syncing the temporary file before its
rename and the parent directory afterwards. A selection whose encrypted
login file is missing is refused, with the command to create it.
The selection applies at the Gateway's next start of any kind, including an
owner restart, the healer, `ti-unlock` or a systemd restart after an IBC exit.
Select only immediately before a planned restart. For live, restart at once
with `sudo systemctl restart ti-gateway`, approve the IB Key prompt, and
switch back to joint afterwards. If secrets are locked, run `sudo ti-unlock`,
which starts an enabled, stopped Gateway.

A root `ExecStartPre=+` runs `stage-gateway-login.sh` before Gateway starts.
It validates the selector and requires `TWS_TRADING_MODE=live` for `live`
and `TWS_TRADING_MODE=paper` for `joint`, `trad-ira` and `roth-ira`. A mismatch
exits 64 with a message naming the selected file. Live also requires an
explicit `TWS_READONLY_API`. It copies exactly the selected
`/run/trend-investing/secrets/ibkr-login-<selection>.env` into the unit's
`RuntimeDirectory` as `login.env`, mode 0600 and owned by `ti-gateway`.
The source open refuses symbolic links. A missing, empty, linked or unreadable
selected file refuses with exit 64, naming both the file and selector.
`InaccessiblePaths=/run/trend-investing/secrets` hides the unlocked directory
from Gateway. The unit's `secrets.env` assertion refuses startup while locked.
The renderer reads the staged `login.env`. systemd removes the runtime
directory when the unit stops.
A file whose username or password is blank
refuses with exit 78. Trading mode and read-only status come from that file;
`TWS_SECOND_FACTOR_DEVICE` becomes IBC's `SecondFactorDevice`, empty when
absent. Rendered credentials stay in memory and are never logged.

Systemd permits at most three starts in ten minutes. Repeated staging or
rendering failures end in a failed unit and trigger the failed-unit page.
The renderer's protected exit statuses still stop retries immediately.
After correcting a configuration failure, use
`sudo systemctl reset-failed ti-gateway` and `sudo systemctl restart ti-gateway`.

## What the engines do while it is down

Nothing reaches IBKR and nothing comes back. With no bars, account figures or
broker snapshots, the engines cannot decide: a session without its bar or
snapshot is withheld (`missing-bars`, `stale-account-snapshot`), a sell
checked against an old broker snapshot is refused (`stale-broker-snapshot`,
one urgent alert per outage), and with no Excess Liquidity reading for the
stale period while the market is open the margin alarm turns urgent. The
sale that alarm fires is withheld until a good snapshot arrives
(`margin-warning.md`). Nothing is sent that was not checked against a fresh
IBKR view.

## Steps

1. `journalctl -u ti-gateway -n 100` and the IBC log in
   `/var/log/ti-gateway/`. Look for a 2FA timeout, bad credentials or
   "existing session".
2. **2FA timed out** (the weekly Sunday prompt, or a restart): the unit
   restarts IBC and sends a new prompt every 30 s plus the timeout.
   **[OWNER]** approve it on the phone.
3. **Existing session detected** (you logged in to IBKR from the app or the
   website): IBC waits rather than fight for the session. Log out of the
   other session, then `sudo systemctl restart ti-gateway`.
4. **Bad credentials**: do not retry in a loop (IBKR locks the login; the
   start program stops after 3 login attempts of any kind in 24 hours and you are paged;
   see `docs/manual/server.md`, "The Gateway is logged out").
   `sudo systemctl stop ti-gateway`, **[OWNER]** correct
   the selected login: `sudo ti-gateway-login` shows it. For `joint`, use
   `sudo SUDO_EDITOR=nano ti-secrets edit ibkr-login-joint.env`; for another
   selection use its filename from the table above. Delete
   `/var/lib/ti-gateway/login-failures`, start it again.
5. **A long outage during market hours with the joint account boosted**:
   check the account in IBKR's own app. If Excess Liquidity is falling, act
   there (the program cannot), and record the kill switch so the program
   sends nothing new when the gateway returns until you have looked:
   `sudo -u ti-operator operator kill-switch ... -state engaged` (as the
   user that writes the owner's instructions, which the engine reads).
6. **When it is back**, the engines keep running (a gateway restart does not
   restart an engine): confirm each is `active`, and that its first broker
   snapshot after the gap is fresh (the stale episode closes). If any account
   left Normal, follow `halted-account.md`; if reconciliation differs,
   `reconciliation.md`. Release the kill switch when you are satisfied.

Never edit numbers to repair a state.
