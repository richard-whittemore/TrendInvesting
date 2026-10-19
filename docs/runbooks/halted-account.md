# Runbook: a halted or degraded account

When the risk gate takes an account out of Normal (ADR 0019, as amended;
ADR 0026, point 3) it records a `strategy.engine.state` decision and the
reporter sends an urgent alert ([alerts-and-dashboard](../manual/alerts-and-dashboard.md)).
**Degraded** freezes the instruments it names: no buys, and sells only on
other instruments. **Halted** freezes the whole account. In both, the
margin account's one sale back to 98% may still go. What each state stops is in
[states-and-kill-switch](../manual/states-and-kill-switch.md). Leaving either
is never automatic: it is this procedure, and it ends in a fresh run you
approve.

## First, in every case

1. **Do not trade by hand while a sale of the program's is working.** If you
   must act at IBKR first, record the kill switch so the program sends
   nothing new but the sale back to 98%:
   `sudo -u ti-operator operator kill-switch -inbox <inbox> -account <account> -state engaged -by owner -detail "halted: investigating"`
   (always run the tool as `ti-trader`, the engine's user, so the engine
   can read what it writes).
2. **Read what happened.** On the server:
   - `journal-query refusals <durable log>`: the refusal that moved the
     account, its reason and detail, and the `engine-state` row naming the
     state and its scope;
   - `journal-query orders <durable log>`: every order planned, released,
     refused, and what IBKR reported of it;
   - `journal-query operator <durable log>`: the kill switch and any approval
     the run started from.
3. **Note the incident's ID**: the ID of the first `strategy.engine.state`
   record out of Normal in this run, or the incident the run's own approval
   carried in force. `operator approve` reads it for you (below).
4. **Check the sale back to 98%** (margin account only): whether it was
   planned and released (an order with sale figures in `journal-query
   orders`), or withheld (`sale-withheld` in `journal-query refusals`). A
   withhold with reason `broker-values-unreadable` or `unknown-working-sell`
   counts as the attempt: the program will not try again, so you act by
   hand. No alert tells you so yet
   ([alerts-and-dashboard](../manual/alerts-and-dashboard.md), "Alerts the
   reporter does not send").

## Find the cause

| Reason in the refusal | Usual cause | Where to look |
| --- | --- | --- |
| `undecided-order` (Halted) | an order that names no recorded decision: a defect in this program's order path | stop here and open an issue; do not clear until it is understood |
| `broker-holding`, `sell-exceeds-holding` (Degraded) | a sell larger than the holding: a hand trade, a corporate action, or a fill the journal has not seen | IBKR's positions against `journal-query positions` |
| `order-size`, `order-count`, `margin-exposure`, `ira-exposure`, `insufficient-cash` (Degraded) | an order outside a hard limit (ADR 0026, point 3) | the decision and the account snapshot it was sized from |
| `price-band`, `missing-mark`, `unvalued-holding` (Degraded) | a price the gate cannot trust: a gap, a stale or missing mark | the bar and mark for that session |
| `reconciliation-unexplained`, `reconciliation-unverifiable` (an `engine-state` row) | IBKR's positions or cash differ from the journal, or could not be compared | `reconciliation.md`, *Find the cause* |

## Recover

1. **Wait for the end of a session**, with no order of the program's still
   working at IBKR (cancel any you decide to end, and wait for IBKR to show
   it cancelled).
2. **Reconcile** positions, cash and open orders against the journal
   (`reconciliation.md`, *Recover*). Explain every difference; never edit a
   number to make it match.
3. **Stop the engine** (`sudo systemctl stop ti-engine@<account>`). It writes
   its journal.
4. **Write the approval** from the stopped run's log. To leave the incident
   behind, clear it by its ID:

   ```
   sudo -u ti-trader operator approve -log <latest run>.log -account <account> -by owner \
     -clear-incident <incident id> -out approval-<new run>.json
   ```

   To restart but keep the account restricted (you have not finished
   reconciling), leave out `-clear-incident`: the approval carries the
   incident in force, in the same state and scope, with whether its sale was
   attempted, and the new run starts under it.
5. If the tool warns the kill switch was engaged, and you still want it on,
   record it again now (`sudo -u ti-operator operator kill-switch ... -state engaged`): a fresh
   run starts with it released.
6. **Start the fresh run** as `durable-log.md`, "Start a fresh run after an
   earlier one", steps 4 and 5, with `-approval` the file from step 4.
7. **Watch the first session** against IBKR before leaving it unattended.

## What can go wrong

- **The approval is refused for the sale attempt**: the log records an
  attempt of the incident's sale and the approval says it was not attempted.
  Write the approval with the tool (it reads the attempt from the log), or,
  if you sold by hand, an approval saying attempted is always accepted.
- **The approval is refused for the incident**: you named an incident that is
  not the one the log ended in, or tried to carry it looser (Degraded when
  the log ended Halted, or on fewer instruments). A restart never loosens
  the state (ADR 0019, amended 2026-09-24).
- **Halted again at the first session**: the cause is still there. Stop,
  and start again from *Find the cause*.

## Before live money

Rehearse this in paper trading (plan.md section 9, phase 4) before an
account goes live: a broker difference that degrades the account, handled
end to end through `operator approve` and a fresh run.
