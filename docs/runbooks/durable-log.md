# Runbook: the durable log, a refused restart and a build upgrade

This is the durable-log runbook (ADR 0027). #338 adds the remaining entries
(a damaged log, a diverging restart, a torn tail). This one entry is here
because the owner decided it on 2026-10-16 (#337).

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
