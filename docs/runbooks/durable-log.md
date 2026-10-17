# Runbook: the durable log, a refused restart and a build upgrade

This is the durable-log runbook (ADR 0027). #338 adds the remaining entries
(a diverging restart, a torn tail). The entries here follow the owner's
decisions of 2026-10-16: no resume across builds, and the restart model;
"An unrecoverable log" is the step for a log whose content the engine cannot
recover; "A log that cannot be read" is the step for one it cannot open or
read.

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
   run: only the latest run in the state directory is resumed. A refusal
   saying the log is a resume abandoned partway means an earlier resume
   stopped while copying (refused, or crashed), even if it copied only the
   configuration: resume the log it names, the crashed run, and leave the
   partial copy where it is as evidence; the engine skips it when finding
   the latest run (ADR 0027 §6). A refusal saying the log holds no
   committed record means a fresh run crashed before its first record, or
   a resume crashed before copying anything: nothing in the log tells which,
   and it holds nothing to resume. If you were starting a fresh run, start
   it again ("Start a fresh run after an earlier one") with the same
   `TI_AS_OF` and approval and a new `TI_RUN`; only if the latest run the
   refusal names itself crashed, resume that log. Leave the empty log where
   it is; the engine skips it when finding the latest run. A refusal saying
   the log holds nothing past its configuration means a fresh run crashed
   before recording its approval: add
   `-approval <the approval it was started with>` to `TI_RESTART_ARGS`
   beside `-resume`, and start again. If that start crashes too, it leaves
   a copy the engine treats as an abandoned attempt: resume the same
   cut-short log again, with the same approval.

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
   decides, and `TI_RESTART_ARGS=-approval <that file>`. `TI_AS_OF` must
   differ from every earlier run's in the state directory: the engine
   refuses to repeat a start (if the latest log holds nothing past its
   configuration, resume that log instead).
5. `sudo systemctl start ti-engine@<account>`. Expect `recorded the owner's
   approval ... the adapter's first input carries sequence 3`. A refusal
   names what does not match: a later log than the one approved, a final hash
   that is not the log's, a latch the log does not end with, an incident
   state looser than the one the log ended in, an incident ID that is not
   the log's, a carried-in-force incident when the log ended normal, a
   repeated start, or another account. Every log in the state directory is
   read to find the latest run, so a log the engine cannot recover (ADR
   0027 §5) stops every start until it is set aside: see "An unrecoverable
   log"; a log it cannot open or read stops every start until the fault is
   fixed or, if it is a superseded log, it is set aside: see "A log that
   cannot be read".

## A log that cannot be read

A log the engine cannot open or read (its owner or mode after a manual
copy, or a disk fault) refuses every start with a message saying it "could
not be read". The engine never skips it: until it is read, nothing tells
whether it is the crashed run. The message says which case it is.

1. **A permission fault** ("not permitted to read it"): fix the file's owner
   or mode, never quarantine it:
   `sudo chown ti-trader: /var/lib/trend-investing/engines/<account>/<name>.log`
   and `sudo chmod 600` on it. Start again.
2. **A disk fault on a log that is or may be the latest run** ("it is or may
   be the latest run", given when no log holding a record was modified after
   it): fix the disk the error names and start again, leaving the log where
   it is. Never quarantine it: it may be the intact crashed run. If the disk
   cannot be recovered, the crashed run cannot be resumed: start a fresh run
   from a reconciled baseline (ADR 0019's recovery procedure), with an
   approval naming the latest run you can still read.
3. **A disk fault on a superseded log** (the message names a newer log
   holding a record, and the quarantine folder): fix the disk if you can and
   start again. If the fault persists, confirm the log is not the crashed
   run: the newer log is the latest run unless it is a resume of this one
   abandoned while copying it. Check the service journal for a start after
   the log was last written that finished its resume or recorded an
   approval:

   ```sh
   sudo journalctl -u ti-engine@<account> \
     --since "$(date -r /var/lib/trend-investing/engines/<account>/<name>.log '+%F %T')" \
     | grep -E "resumed [0-9]+ record\(s\) from|recorded the owner's approval of a fresh run after"
   ```

   A line here comes from a start that completed its copy (`resumed`) or
   began a fresh run (`recorded the owner's approval`) after the log was last
   written, so a later run supersedes it. With such a line, follow "An
   unrecoverable log" step 2 to move it, unchanged, into the quarantine
   folder (a move needs no read), then start again. With no line, treat it as
   case 2: leave it where it is.
4. If the engine then reads the log and finds its content unrecoverable,
   follow "An unrecoverable log".

## An unrecoverable log

Every `*.log` in the root of the account's state directory,
`/var/lib/trend-investing/engines/<account>/`, is read to find the latest
run. A log whose content the engine cannot recover (ADR 0027 §5: a damaged file, an old
log whose bytes rotted after it was superseded and still read back, or a stray file ending in
`.log`; rot the disk reports as a read error is "A log that cannot be read") refuses every start, resume and fresh run alike, with a message
ending "move `<name>`, unchanged, into `<state directory>/quarantine`".

1. **Do not edit or delete the file.** It is recorded evidence (ADR 0018).
2. **Move it, unchanged, into the quarantine folder** beside the logs:

   ```sh
   sudo -u ti-trader mkdir -p /var/lib/trend-investing/engines/<account>/quarantine
   sudo -u ti-trader mv /var/lib/trend-investing/engines/<account>/<name>.log \
     /var/lib/trend-investing/engines/<account>/quarantine/
   ```

   The engine reads only the state directory's root, never the quarantine
   folder, so the file is kept and no longer counted as a run.
3. **Decide the restart again** ("Restarting an engine"): with the file set
   aside, the latest run may be a different log. If the quarantined log was
   the crashed run itself, it cannot be resumed: start a fresh run from a
   reconciled baseline (ADR 0019's recovery procedure), with an approval
   naming the latest run left in the directory.
