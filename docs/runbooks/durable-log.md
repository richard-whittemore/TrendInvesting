# Runbook: the durable log, a refused restart and a build upgrade

This is the durable-log runbook (ADR 0027). #338 adds the remaining entries
(a damaged log, a diverging restart, a torn tail). The two entries here
follow the owner's decisions of 2026-10-16: no resume across builds, and
the restart model.

## Upgrading the build

A run does not resume across builds. Every decision carries the strategy
version, which includes the build (ADR 0016), so a new build cannot reproduce
the old log's bytes, and `engine -resume` refuses it with an error naming this
procedure. An upgrade is always a new run.

1. **Wait for the end of a session.** Do not upgrade with a session in
   progress or an order working that the old engine placed and has not seen
   settle.
2. **Stop the old engine** with SIGINT or SIGTERM, so it writes its journal.
3. **Keep the old durable log and journal.** They are recorded evidence and
   are never edited or deleted (ADR 0018).
4. **Install the new build** and its pinned inputs.
5. **Reconcile.** Start the new run from a reconciled baseline under ADR 0019's
   recovery procedure (`reconciliation.md`, *Recover*): the broker's positions,
   cash and open orders are compared and the baseline is journalled before any
   trading decision.
6. **Start a fresh run** with `-mode` as before, a new `-log` path, and no
   `-resume`. The new log begins with its own configuration input.
7. **Check** the new run's first session against the broker before leaving it
   unattended.

If `-resume` is refused with "does not resume across builds", you are at step
1. If it is refused for a different configuration, that is not an upgrade:
resume with the `-config` and `-as-of` the logged run started with.

## Restarting an engine

systemd never restarts an engine (`Restart=no`), and a reboot leaves every
engine stopped. Every start is this owner step. First decide which case it is
(ADR 0027, amendment of 2026-10-17):

- **A crash** (the process died: `systemctl status ti-engine@<account>`
  shows it failed or was killed, and its journal was not written): **resume**.
- **Anything else** (you stopped it, a reboot, leaving Halted, a build
  upgrade, a cleared rebalance): a **fresh run**, which starts only after your
  recorded approval.

The engine reads its settings from
`/var/lib/trend-investing/engines/<account>/run.env` (the example is
`deploy/etc/engines/run.env.example`). Edit it with `sudo -u ti-trader -e`.

### Resume after a crash

1. Keep `TI_AS_OF` as it was. Set `TI_RUN` to a new name (a log is never
   reused) and `TI_RESTART_ARGS=-resume /var/lib/trend-investing/engines/<account>/<crashed TI_RUN>.log`.
2. `sudo systemctl start ti-engine@<account>`, then
   `journalctl -u ti-engine@<account> -n 20`. Expect `resumed N record(s)`,
   and possibly a torn tail of one input, which the adapter sends again.
3. A refusal naming another build or configuration means this is not a
   crash restart: follow "Upgrading the build" or start a fresh run. A
   refusal naming a later log means the log you named is not the crashed
   run: only the latest log in the state directory is resumed. A refusal
   saying the log is a resume abandoned partway means an earlier resume
   stopped while copying (refused, or crashed): resume the log it names,
   the crashed run, and leave the partial copy where it is as evidence;
   the engine skips it when finding the latest log (ADR 0027 §6). A refusal
   saying the log holds nothing past its configuration means a fresh run
   crashed before recording its approval: add
   `-approval <the approval it was started with>` to `TI_RESTART_ARGS`
   beside `-resume`, and start again. If that start crashes too, resume
   the newest log with the same approval: the approval still names the run
   before the first attempt.

### Start a fresh run after an earlier one

1. Wait for the end of a session; reconcile under ADR 0019's recovery
   procedure (`reconciliation.md`, *Recover*).
2. Find the latest log and its final record hash: the `record_hash` of the
   last commit line, `grep '"commit"' <latest>.log | tail -n 1`.
3. Write the approval, for example `approval-<new TI_RUN>.json` in the
   account's state directory:

   ```json
   {"account_id": "joint", "previous_log": "run-2026-10-20.log",
    "previous_log_final_hash": "sha256:<64 hex digits>",
    "incident_id": "", "approved_by": "owner",
    "approved_at": "2026-10-21T13:00:00Z", "boost": {"latched": false},
    "incident_in_force": null}
   ```

   Every key is required (`null` where there is none); one left out, or one
   the engine does not know, is refused. `boost.latched` is the state in
   force: the `latched_after` of the latest `strategy.targets.decided` in the
   previous log, or, if that run decided nothing, the latch its own approval
   stated; the engine refuses any other value. `incident_id` names the
   incident you are clearing, or is empty; it must be the ID of the incident
   the previous log ended in (the `incident_id` its own approval carried in
   force, or else the `id` of its first `strategy.engine.state` record out
   of normal), and must be empty if that log ended normal. `incident_in_force` is `null`
   unless the new run continues an incident; then it is
   `{"incident_id": "<id>", "state": "degraded"|"halted",
   "scope_instrument_ids": ["SPY"], "sale_attempted": true|false,
   "sale_client_order_id": "<id, only if the sale was sent>"}`: the state
   and scope the incident holds the account in (degraded names its
   instruments, in ascending order; halted names none: `[]`), from the last
   `strategy.engine.state` records in the previous log, or, if it recorded
   none since its start, the `incident_in_force` its own approval carried
   (a continued incident records no new state); the new run starts
   in them (ADR 0019, amended 2026-09-24). And whether that incident's one
   sale back to 98% was attempted (ADR 0019, amended 2026-10-15). A run
   approved with `true` never attempts it again. If the previous log ended
   degraded or halted, the approval must either clear that incident
   (`incident_id`) or carry it in force, under that incident's ID, at least
   as tight; the engine refuses one that would loosen it or names another
   incident. If the latest log holds nothing past its configuration (a
   start cut short), the approval still names it and its final record hash,
   but take the latch and incident from the latest earlier log that holds
   more: the engine checks them against that log.
4. Set `TI_RUN` to a new name, `TI_AS_OF` to the first session the new run
   decides, and `TI_RESTART_ARGS=-approval <that file>`. If the latest log
   holds nothing past its configuration, `TI_AS_OF` must differ from its
   run's: the engine refuses to repeat that start (resume that log instead).
5. `sudo systemctl start ti-engine@<account>`. Expect `recorded the owner's
   approval ... the adapter's first input carries sequence 3`. A refusal
   names what does not match: a later log than the one approved, a final hash
   that is not the log's, a latch the log does not end with, an incident
   state looser than the one the log ended in, an incident ID that is not
   the log's, a repeated start, or another account. Every log in the state
   directory is read to find the latest run, so a log the engine cannot
   recover (ADR 0027 §5) stops every start until it is dealt with.
