# Runbook: the gateway will not log in

The IBKR Gateway runs on the server under IBC (`ti-gateway`), and the
engines' adapter talks to IBKR through it (ADR 0025). Setup and the
reconnect drill are in `lightsail-deployment.md`; this runbook is what to
do when it will not log in, and what the engines do meanwhile.

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
Rerunning it changes nothing. If old and new files both exist, migration
refuses and names them; preserve both and resolve which file to keep before
retrying. The old name is no longer accepted by `ti-secrets edit`. A legacy
plain-text login is renamed too, then follows the normal encryption migration.

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
The optional file is encrypted even if the editor closes without a change;
an editor error creates nothing. Live mode requires an explicit
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
They write the selector atomically and print the restart instruction; they
never restart a service. If secrets are locked, run `sudo ti-unlock` (which
starts an enabled, stopped Gateway). For a running Gateway, apply the
selection when ready with `sudo systemctl restart ti-gateway` and approve
the IB Key prompt on the phone for the live login.

A root `ExecStartPre=+` runs `stage-gateway-login.sh` before Gateway starts.
It validates the selector and copies exactly the selected
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
