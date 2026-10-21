# Runbook: the gateway will not log in

The IBKR Gateway runs on the server under IBC (`ti-gateway`), and the
engines' adapter talks to IBKR through it (ADR 0025). Setup and the
reconnect drill are in `lightsail-deployment.md`; this runbook is what to
do when it will not log in, and what the engines do meanwhile.

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
4. **Bad credentials**: do not retry in a loop (IBKR locks the login).
   `sudo systemctl stop ti-gateway`, **[OWNER]** correct
   `/etc/trend-investing/ibkr-login.env`, start it again.
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
