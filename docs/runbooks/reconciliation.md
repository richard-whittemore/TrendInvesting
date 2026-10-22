# Runbook: when reconciliation fails

This is the procedure for when IBKR's figures and the engine's record of an
account disagree: the morning reconciliation found a difference, or the
alert says an account went **Degraded** or **Halted** and names a
reconciliation. It is written for the three live accounts, the joint
account (Boost) and the two IRAs (momentum), under ADR 0019 as amended and
ADR 0026, points 3 to 5. Read the whole alert first: it says the state the
account is now in, what it still does and what it has stopped doing.

**None of these accounts has stop orders at IBKR.** Earlier versions of
this runbook were written for a strategy that kept a stop order per
position; that does not apply here. The program's protection is that it
stops ordering, not an order resting at the broker.

> **Status.** The engine's side is built and tested against recorded
> inputs: the comparison, the states and the alert
> ([reconciliation](../manual/reconciliation.md)). The adapter's morning
> reading is built and delivered to the engine: the adapter sends IBKR's
> positions and cash with every day's file, and at every start and
> reconnect once the engine holds a book (a fresh Boost run holds none until
> its first delivered file), so a reconciliation runs by itself from then on
> (Boost; the IRAs' inputs are not built). Rehearse this runbook in paper trading before live money
> (below).

## First, in every case

1. **Read the alert.** It names what differs (an instrument, the cash, or
   "could not be compared") and the account's whole state. On the public
   ntfy relay it carries no figures: read them on PagerDuty, the dashboard,
   or with `journal-query`. Two lines of the alert's text are left over
   from the old stop-order strategy: "restoring a missing Protective Stop"
   and "check the protection of the named positions at the broker". These
   accounts have no stops: ignore those two lines. (Correcting the alert
   text belongs to the reporter follow-up issue on alerts.)
2. **Know what the state means for this account:**

   | State | What still goes | What has stopped |
   | --- | --- | --- |
   | **Degraded** on an instrument | sells of other instruments, sized from IBKR's latest holding; for the joint account, the one sale back to 98% | every buy; every order on the named instrument |
   | **Degraded** on `total-cash` (only the cash differs) | every sell | every buy |
   | **Halted** | for the joint account only, the one sale back to 98% when Boost's exit fires or the margin alarm is urgent | every other order |

   Neither state ends by itself: a later matching reconciliation does not
   bring the account back to Normal. Only a fresh run you approve does
   ([Recover](#recover)).
3. **Acknowledge the alert** so it stops repeating:
   `sudo -u ti-operator operator acknowledge -inbox <inbox> -account <account> -alert '<alert key>' -by owner`
   ([operator](../manual/operator.md), "Acknowledging an alert").
   Acknowledging changes nothing else.
4. **Do not trade by hand while a sale of the program's is working.** If
   you must act at IBKR first, stop the program sending anything new: the
   kill switch (`sudo -u ti-operator operator kill-switch ... -state engaged`),
   and to stop everything, also stop the adapter, then the engine (stopping the
   Gateway only delays: a released order goes out once it is back the same
   day). The kill switch does not cancel an order already at IBKR: cancel
   one in IBKR's own app.
5. **If the alert or `journal-query refusals` says CONTAINMENT REQUIRED**
   (`unknown-working-sell`): a sell order the program did not send is
   working at IBKR, and the program will not sell over it. At IBKR, cancel
   it, or confirm it is yours and leave it, and make sure the working sells
   together are no more than the shares held; **wait for each cancellation
   to be confirmed.** Write down what you did and why.
6. **Never "fix" the account by editing a number, a log or a file.** The
   engine never copies IBKR's figures to make a difference go away (ADR
   0019). Every repair is something done at IBKR and recorded, or a fresh
   run you approve.

## Find the cause

Read the engine's verdict on the server, from the account's durable log
(`/var/lib/trend-investing/engines/<instance>/<TI_RUN>.log`):

- `grep '"risk.reconciliation.result"' <log> | tail -n 1` shows the last
  verdict: each component (each instrument, and the cash), the book, the
  expected and observed figures, the difference and the events that
  explained it.
- `journal-query refusals <log>` shows the state change and its scope.
  Run it on the server as `sudo -u ti-trader journal-query ...`
  ([server](../manual/server.md)). `journal-query reconcile` fails, saying so
  in words, on a log that is empty, cut short, compared nothing or has fills no
  snapshot has covered; that is not a clean result ([journal-query](../manual/journal-query.md)).

Then compare with IBKR's own records: its activity statement and trade,
order and cash history in Client Portal. Each kind of difference has a
signature:

| What differs | Likely cause | Where to look at IBKR |
| --- | --- | --- |
| A holding, by the size of one of our orders | a fill IBKR made that the engine never recorded (a lost report, or a trade you acknowledged with `relay.py acknowledge-late-fill`, which is never recorded as a fill) | trade history for that instrument |
| A holding the book does not have, or a different count with no order of ours | a trade placed by hand, outside the program | order history: who placed it |
| A holding gone, or much smaller | IBKR sold it (a margin liquidation, on the joint account), or a corporate action | IBKR's messages; the corporate-actions notice |
| The share count changed with no trade | a split, merger, symbol change or delisting the book did not apply | the corporate-actions notice |
| Only the cash differs (IRAs) | a dividend, interest, a fee, a transfer or a commission the journal did not record | cash activity |
| **Halted, unverifiable** | IBKR's figures could not be read, there was no account snapshot yet to compare with, or a corporate action other than a split or dividend | the Gateway (`docs/runbooks/gateway-login.md`); the corporate-actions notice |

Two things the morning check never sees, so check them yourself:

- **The joint account's cash.** Boost's book carries no cash yet, so its
  cash is not compared, and **margin interest on the joint account is never
  compared**. Nothing will tell you about an unexpected charge: read the
  joint account's monthly statement.
- **A difference that arises during a session** (a fill report lost while
  the market is open). The checks after each fill and before each order
  that ADR 0019 asks for are not built (only the start, reconnect and daily-delivery reconciliations are).

## Recover

**No command records a missed event today.** There is no way to tell the
program "this dividend, fee, interest charge, fill or corporate action
happened": the `operator` tool has four commands (the kill switch, an
approval, a rebalance clearance and an acknowledgement), and none of them
records a market or account event. What you can do is check the difference
at IBKR, write down what you found, and either clear the incident or carry
it into a fresh run. A fresh run's book starts from the first account
snapshot it reads, so a difference you have explained is not seen again;
that is why the approval is your attestation and must follow a real check.

1. **Explain every difference at IBKR**, item by item, and write down what
   you found (in the issue for the incident, or your own notes; the
   approval itself holds no free text). If you cannot explain one, do not
   clear: go to step 5 and carry the incident.
2. **Wait for the end of a session** with no order of the program's still
   working at IBKR. Cancel any you decide to end and wait for IBKR to show
   it cancelled. For an IRA in the middle of a rebalance, record the
   clearance in the old run first (`docs/runbooks/missed-rebalance.md`):
   a fresh run does not know the earlier run's working orders.
3. **Stop the engine**: `sudo systemctl stop ti-engine@<instance>`. It writes
   its journal. Keep the log and the journal: they are evidence, never
   edited or deleted (ADR 0018).
4. **To leave the incident behind** (every difference explained), write the
   approval clearing it by its ID. `operator` is installed on the server
   ([server](../manual/server.md), "Reading the logs and the kill switch on
   the server"), so write it there, as `ti-trader`, with `-out` naming a
   file in the account's state directory:

   ```
   sudo -u ti-trader operator approve -log <latest run>.log -account <account> -by owner \
     -clear-incident <incident id> -out /var/lib/trend-investing/engines/<instance>/approval-<new run>.json
   ```

   `operator approve` reads the incident's ID, the latch and the sale
   attempt from the log ([operator](../manual/operator.md), "An approval").
   Check that the file name the tool printed matches the log you approved.
5. **To restart but stay restricted** (something is not yet explained),
   leave out `-clear-incident`: the approval carries the incident in force,
   in the same state and scope (`total-cash` included), and the new run
   starts under it. Buys stay refused until you clear it in a later run.
6. If the tool warns that the kill switch was engaged, and you still want
   it on: a fresh run starts with it released. Record it again with
   `sudo -u ti-operator operator kill-switch ... -state engaged` into the
   account's inbox, which the server's service file passes to the engine
   as `-operator` (`docs/runbooks/durable-log.md`, "Start a fresh run after
   an earlier one").
7. **Start the fresh run**: `docs/runbooks/durable-log.md`, "Start a fresh
   run after an earlier one", steps 4 and 5, with `-approval` the file from
   step 4 or 5.
8. **Watch the first session** against IBKR before leaving it unattended.
   If the next reconciliation fails again, its alert names what is still
   unexplained: go back to *Find the cause*.

Some cases need one more step before step 4:

- **A missed dividend, fee or interest charge (IRAs):** the account is
  Degraded on `total-cash`; every buy is refused, sells continue. Check the
  item on IBKR's cash activity, then clear. Two items that cancel each other
  out (a fee and a credit of the same amount) leave the total unchanged and
  are never detected: the statement is the only check.
- **A fill IBKR made that the engine never recorded:** check the trade at
  IBKR. For an IRA, an order with no final report also holds the next month
  back (`rebalance-in-progress`): clear it only after IBKR shows the order
  finished (`docs/runbooks/missed-rebalance.md`).
- **A trade by hand:** decide whether to keep it or undo it at IBKR (with
  the kill switch on, and nothing of the program's working). The next
  decision trades from what IBKR then holds.
- **A corporate action other than a split or dividend** (a merger, a
  delisting, a symbol change): the account is Halted, unverifiable. Check
  what IBKR now holds for that stock. There is no way to record the action
  for the program; clear once the holding is understood. For an IRA, the
  next rebalance sells what the rule no longer targets.

## If the system itself is silent

If the reporter's "no sign of life from the engine" alert fires, or you
notice nothing has come for a day, the engine is down or stuck and cannot
report its own state. If the whole server is down, the outside heartbeat
(Healthchecks, `docs/manual/server.md`, "The outside heartbeat") pages you
when the reporter's pings stop; if you have not set that up yet, nothing will
tell you.

1. Look at each account in IBKR's own app. These accounts have no stop
   orders at IBKR, so nothing there protects them: for the joint account,
   check Excess Liquidity first, and act in IBKR's app if it is falling
   (`docs/runbooks/margin-warning.md`).
2. On the server, `systemctl status ti-engine@<instance>` says whether the
   engine crashed or was stopped. A crash is resumed; anything else is a
   fresh run you approve (`docs/runbooks/durable-log.md`, "Restarting an
   engine"). Either way the engine comes back **in the state it was in**:
   a Degraded or Halted account stays so.
3. The adapter sends its reconciliation at a start only once the engine
   holds a book: a fresh Boost run holds none until its first delivered
   file, and with no market data delivered nothing is compared at all. Until
   then, compare IBKR's positions and cash with the journal yourself. The
   adapter does stop at start on an open order or a trade no released order
   explains, but the engine's reconciliation does not compare open orders:
   check those against the journal yourself.

## Before live money

- Rehearse each row of *Find the cause* in paper trading, by deliberately
  creating that difference, and time the recovery through `operator
  approve` and a fresh run.
- Install and set up the reporter on the server so that alerts reach both
  channels, and prove that a Degraded or Halted alert repeats until it is
  acknowledged, at the interval the server actually runs the reporter
  ([server](../manual/server.md)).
- Prove the outside heartbeat pages when the reporter stops (the test in
  `docs/manual/server.md`, "The outside heartbeat"), and that a stopped engine
  or adapter unit pages within minutes (`systemctl kill --signal=SIGKILL
  ti-engine@boost` in paper).

## What is not built yet

- The reconciliations after each fill, before each order and on a timer
  (ADR 0019); the start, reconnect and daily-delivery ones are built.
- The comparison of open orders, and of the joint account's cash.
- Any command that records a missed fill, dividend, fee, interest charge or
  corporate action.
- Matching each cash item with IBKR's own activity rows, rather than one net
  total.
- The alert text without the stop-order lines, pointing here and to
  `docs/runbooks/halted-account.md`.
