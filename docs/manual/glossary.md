# Glossary

*For the owner. Every term the manual uses, in plain words, with the chapter
that explains it. Terms in **bold** inside a definition have their own
entry.*

**Account names.** One account goes by several names, and each command
takes a particular one:

| Name | Example | Used by |
| --- | --- | --- |
| The configuration's `account_id` | `joint` in the shipped dry-run file; your IBKR paper account number (`DU…`) in a paper configuration | the engine, and every `operator` command's `-account` (an instruction for any other name is refused or has no effect), and an approval's `account_id` |
| The server's account label | `boost`, `trad-ira`, `roth-ira` | the service names (`ti-engine@boost`, `ti-adapter-boost`, `ti-reporter@boost`), the state folder (`/var/lib/trend-investing/engines/boost/`), the `.env` and `.config` file names, `TI_ACCOUNT` in the `.env` file |
| The IBKR account number | `DU…` for paper; a live number never begins `DU` | IBKR, the adapter's configuration, and the paper configuration's `account_id` |
| The plan's name | "the joint account", "the Traditional IRA", "the Roth IRA" | the chapters and the plan |

In an alert's title, `<account>` is the first of these (the name in the
configuration, such as `joint`), read from the log. The reporter's own
`-account` setting, which the server sets to the second, is only used when
the log cannot name its account.

**Acknowledged** (an order). IBKR has reported on the order (accepted,
refused, cancelled or filled), and the engine read the report. The third of
an order's states after **committed** and **released**. See
[order lifecycle](order-lifecycle.md).

**Acknowledgement** (an alert). Your record, made with `operator
acknowledge`, that you have seen a repeating alert. It stops the repeats
and changes nothing else. See [operator](operator.md).

**Adapter.** The Python program between IB Gateway and an engine: it reads
IBKR's figures and reports for the engine and sends its released orders.
See [IBKR adapter](ibkr-adapter.md).

**Alert.** A message from the reporter. An **urgent** one pages you; a
**routine** one only informs. Most are sent once; reconciliation and
engine-state alerts repeat every 30 minutes until you **acknowledge** them.
See [alerts and dashboard](alerts-and-dashboard.md).

**Alert key.** The name at the end of each alert's text, which an
**acknowledgement** must quote exactly. See
[alerts and dashboard](alerts-and-dashboard.md).

**Approval.** Your recorded permission for a **fresh run**, naming the
previous log, the **latch** and the **incident** in force. Written by
`operator approve`. See [restart](restart.md).

**b** (the bond share). The share of an IRA meant for bonds at your age,
from 0 to 0.7. See [glide path](glide-path.md).

**Band.** Two different bands: the **price band** (5%), how far an order's
reference price may be from the **mark**, which also sets the limit price;
and the **rebalance band** (25%), how far an IRA holding may drift from its
target before it is traded. See [momentum](momentum.md).

**Book.** What the engine believes an account holds: the last **account
snapshot** plus the fills and other events recorded since. The morning
**reconciliation** compares it with IBKR's figures. See
[reconciliation](reconciliation.md).

**Broker snapshot** (`account.broker.snapshot`). One read of IBKR's
positions, net liquidation, total cash, **Excess Liquidity** and open
orders. The risk gate sizes sells from it. Different from the **account
snapshot** (`account.snapshot`), the positions and value as of a close that
the strategy decides from. See [states and kill switch](states-and-kill-switch.md).

**Build stamp.** The git commit a program was built from, recorded in every
decision. An unstamped build ("dev") cannot run dry-run, paper or live. See
[engine](engine.md), "Build identity".

**Client ID.** The number an adapter connects to IB Gateway under: one per
engine (Boost 11, Traditional IRA 12, Roth IRA 13). Not the **client order
ID**. See [IBKR adapter](ibkr-adapter.md).

**Client order ID.** An order's name, made from its decision, instrument
and side (`tw-` and 24 hex digits). IBKR shows it as the order's
reference, `orderRef`. See [boost](boost.md#the-one-order).

**Commit, committed.** The engine writes each input and what it decided as
one group, flushed to disk, ending in a commit line. An order whose release
is on disk is **committed**: it may or may not have reached the adapter.
See [order lifecycle](order-lifecycle.md).

**Configuration.** One account's settings file: strategy, account, mode,
rule figures and hard limits (`/etc/trend-investing/engines/<account>.config`).
It is the first record of every run.

**Configuration hash.** A fingerprint (SHA-256) of the configuration and
its schema version. Any change gives a different hash, so a changed
configuration is always a new run (ADR 0016).

**D7-L** (minimum lot). The IRAs' whole-share rule: a pick one share of
which costs more than its target is replaced by the next stock. See
[minimum lot](momentum-minimum-lot.md).

**Dashboard.** A web page, `ti-dashboard`, listening on `127.0.0.1:8088` on
the server only, that shows the daily summary, one section per account. See
[alerts and dashboard](alerts-and-dashboard.md).

**Data job.** `ti-data`, the scheduled job that records the day's market data
and writes the files an engine's input **stream** is made from. See
[market data](market-data.md).

**Decision.** A record the engine writes from its inputs: a target
(`strategy.targets.decided`), a planned order, a release or refusal, a
change of state, a withheld sale. Unlike an **input**, a replay of the
log must produce it again byte for byte.

**Decision session.** For momentum, the last session of a month, at whose
close the month is decided. For Boost, every session.

**Degraded.** The state the risk gate puts an account in when an order
breaks a limit or the reconciliation finds a difference nothing recorded
explains: no buys, and nothing at all on the instruments it names (its
**scope**), except, for the joint account, the one sale back to 98%
on the instrument it names; sells elsewhere continue. Never left by itself. See
[states and kill switch](states-and-kill-switch.md).

**Dry-run.** The mode that decides and records everything and sends
nothing: every released order says `send: false`. See [engine](engine.md).

**Durable log.** The file a running engine writes as it goes
(`<TI_RUN>.log`): every input and decision, each group flushed to disk
before anything is sent (ADR 0027). It is what a crash is resumed from. See
[restart](restart.md).

**Excess Liquidity.** IBKR's figure for how much margin cushion the
account has left. The **margin-cushion alarm** watches it as a share of
net liquidation. See [states and kill switch](states-and-kill-switch.md#the-margin-cushion-alarm).

**Fake gateway.** A stand-in for IB Gateway, used in the adapter's tests,
that answers like IBKR with invented accounts and prices.

**Fee allowance.** The $2.00 per order the IRA planner adds to a buy's
worst-case cost. See [momentum](momentum.md).

**Fill.** One execution of an order at IBKR: some or all of its shares at a
price. A fill is a separate report from an order's status. See
[order lifecycle](order-lifecycle.md).

**Fresh run.** Any start that is not a crash **resume**: after you stopped
the engine, a reboot, a new build, leaving Degraded or Halted. It needs your
**approval**. See [restart](restart.md).

**Gap.** A session missing from a price series. Boost skips a session with
no SPY bar (that session is withheld, nothing trades) and computes its
readings over the closes it has; the next decision names the gap
(`gap_sessions`). Momentum leaves out a stock not held with a gap. See [boost](boost.md)
and [momentum universe](momentum-universe.md).

**Golden case.** A month (momentum) or a session (Boost) logged from
QuantConnect's own backtest, transcribed into a Go test that the Go rule
must reproduce. The logs hold market data, so they are kept outside the
repository and the tests skip without them (ADR 0026, point 7).

**Halted.** The state for something the gate cannot contain: an order that
names no recorded decision, or a reconciliation that could not be compared.
Nothing is sent except the joint account's one sale back to 98%. Never left
by itself. See [states and kill switch](states-and-kill-switch.md).

**Hand-over.** The adapter's "about to send" and "sent" marks for an order,
written on disk before and after it hands the order to IB Gateway, in the
file ending `.handover`. See [re-delivery](redelivery.md).

**IB Gateway, IBC.** IB Gateway is IBKR's own login-and-connection program;
IBC keeps it logged in. All the adapters connect through the one on the
server. See [IBKR adapter](ibkr-adapter.md).

**Incident.** The span from an account's first move out of Normal until
your recovery. Degraded to Halted is the same incident. Each incident gets
at most one **sale back to 98%**.

**Input.** A record the engine reads: a bar, a session close, an account
or broker snapshot, a mark, a fill, an IBKR report, your instruction. Each
is numbered in one series per run (its `sequence`) and written to the log
before the engine acts on it.

**Journal.** The record of a run, one line per event, each line
fingerprinted from the one before, so an edit anywhere breaks the chain
(ADR 0017). "Journal" also names the end-of-run file (`<TI_RUN>.journal`)
an engine writes when it stops; the **durable log** is the one it writes
while running. `journal-query` reads both. See
[journal-query](journal-query.md).

**Kill switch.** Your own stop: while engaged, every new order is refused
except the joint account's sale back to 98%. It does not cancel orders
already at IBKR. See [states and kill switch](states-and-kill-switch.md).

**Lapse, lapsed.** A withheld sale back to 98% that is given up: what fired it
ended before a good **broker snapshot** could size it, so the sale is not
tried again. The log records one more withheld sale with the reason `lapsed`;
it goes in the daily summary, not an urgent alert. See
[states and kill switch](states-and-kill-switch.md).

**Latch.** Whether Boost's boost is on. It turns on after a sharp drop in
an uptrend and off at a close above the 5-day average, and is taken from
the record after a restart, never recomputed. See [boost](boost.md).

**Limit order.** An order that may not trade at a worse price than its limit.
Every order the engines release is a whole-share DAY limit order. See
[paper trading](paper-trading.md).

**Live.** The mode that would send real orders to a real account. Refused
until the limited-live gate (plan section 9, Phase 5).

**Margin-cushion alarm.** The joint account's watch on **Excess
Liquidity**: warning below 40% of net liquidation, urgent below 20% (or no
reading for 30 minutes while the market is open), which fires the sale
back to 98%.

**Mark.** IBKR's latest recorded price for an instrument
(`market.mark.recorded`), recorded at or after the close. The **price band**
compares the order's reference price with it.

**MaxSnapshotAge.** How old IBKR's latest **broker snapshot** may be when a
sell is checked against it (30 minutes for Boost).

**Net liquidation.** The account's value if everything were sold: IBKR's
`NetLiquidation`.

**Normal.** The starting state: every order that passes the limits goes.

**Operator inbox.** The folder an engine reads your instructions from,
written by the `operator` tool. See [operator](operator.md).

**Order status.** What IBKR reports about an order that is not a **fill**:
submitted (working), cancel-pending, canceled or invalid (refused). See
[order lifecycle](order-lifecycle.md).

**Outbox.** The adapter's file of inputs it has taken but not yet sent to the
engine (IBKR's reports, and a day's market data behind its **broker
snapshot**), each sent once, in order. It sits beside the **pending file**.
See [re-delivery](redelivery.md).

**Paper.** The mode that sends orders, but only to an IBKR paper account
(numbers beginning `DU`). See [paper trading](paper-trading.md).

**Parity.** The check that the Go code makes the same decisions as the
QuantConnect backtests from the same data, within thresholds fixed in
advance (ADR 0026, point 7). See [parity](parity.md).

**Pending file.** The adapter's small file holding the input it is waiting
for the engine's reply to. The hand-over file (`.handover`), the file of your
resolve decisions (`.handover.decisions`), the lock (`.lock`) and the outbox
sit beside it with the same name plus an ending.
See [re-delivery](redelivery.md).

**Quarantine.** A folder beside the logs (or the recordings) where a file
the program cannot use is moved, unchanged, as evidence. The program never
reads it.

**Re-delivery.** The adapter asking the engine again for the reply to an
input, by sending the same input again; the engine answers from its log,
so an order is never lost or doubled. See [re-delivery](redelivery.md).

**Re-plan.** Boost planning again, at its next decided session, the
difference to its target after its last order did not get there: the gate
refused it while the account stayed Normal (`refused`), it ended with
shares unfilled (`part-filled`, or `unfilled` when none filled), or IBKR
rejected it (`rejected`, planned once more only; two orders rejected in a
row stop Boost's buys until a fresh run, while its sells still go). The
`unfilled` re-plan, the one retry of a rejection and the stop after two
were confirmed by the owner on 2026-10-18. The decision records the
cause (`replan`). See
[boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan).

**Reconciled, reconciliation.** Three different things carry this word:

| Meaning | What it is | Can it put the account in Degraded or Halted? |
| --- | --- | --- |
| (a) The adapter's matching | On every connect and reconnect, the adapter reads IBKR's positions, balances and open orders, and matches IBKR's open orders and today's trades against the orders the engine released, before it sends anything. Positions and balances are read, not compared. Its messages call this "reconciled" | No |
| (b) The morning reconciliation | The engine compares IBKR's positions and cash with its **book**: matched, explained, unexplained or unverifiable ([reconciliation](reconciliation.md)) | Yes |
| (c) `journal-query reconcile` | A read of a log that checks the recorded fills against the recorded account snapshots ([journal-query](journal-query.md)) | No: it only reads |

When a runbook says "reconcile the account" it means a fourth thing: you
compare IBKR's positions, cash and open orders with the journal yourself
(`docs/runbooks/reconciliation.md`).

**Refused.** The risk gate's answer to an order that breaks a limit or
meets a state that stops it (`risk.order.refused`, with a reason). A
refused order is never sent.

**Rejections in a row.** Boost's count of its orders IBKR rejected since
the last one IBKR accepted, each counted when IBKR's first word on it
arrives, however late (`venue_rejections`). At two Boost's buys stop until
a fresh run, while its sells still go; each decision names the orders
counted (`rejected_in_a_row`), so at the stop the two that stopped buys,
and the page names them too. See
[boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan).

**Release, released.** The risk gate's answer to an order that passes
(`risk.order.released`). In the order's lifecycle, **released** also means
a later input proves the adapter received it.

**Reporter.** `ti-reporter`, which reads an account's log every 5 minutes,
around the clock, and sends its **alerts** and the daily summary. It only
reads. See [alerts and dashboard](alerts-and-dashboard.md).

**Resume.** Restarting a crashed run from its own durable log, carrying on
exactly where it stopped. Needs no approval. See [restart](restart.md).

**Risk gate.** The hard-limit checks outside the strategy that every
planned order passes through, and the keeper of Normal, Degraded and
Halted (`internal/risklimit`). See [momentum](momentum.md#the-hard-limits-in-plain-words).

**Risk-off.** For momentum, SPY's close below its 200-day average: half of
the momentum book moves to IEF.

**Risk-reducing.** An order that moves the account toward 98% invested or
lower: exempt from the order count, order size and price band.

**RulesVersion.** A version number declared in the code that changes only
when a rule changes, so two builds with the same rules version replay each
other's logs exactly (ADR 0016). The target-weight strategies are at
**1.18.0**; a log written under an earlier one does not replay. Each
chapter's "since version x" notes say when a rule arrived.

**Run.** One start of an engine to its stop, with its own name (`TI_RUN`),
log and journal. A resume continues a run's history in a new log; a fresh
run starts a new one.

**Sale back to 98%.** The joint account's one automatic sale, once per incident
(under the kill switch, once per engagement; for the urgent alarm, once per
alarm), that brings SPY back to 98% when Boost's exit is refused or the margin
alarm is urgent. See [states and kill switch](states-and-kill-switch.md#the-one-sale-back-to-98).

**Scope.** What a Degraded account has frozen: the instruments named, and
`total-cash` when only the cash differs.

**Sequence.** An input's number in its run's one series. Only the program
that delivers inputs knows the next number, so the data files carry their
own line numbers, which the deliverer replaces.

**Session.** One NYSE trading day, closing at 4 p.m. New York (1 p.m. on an
early close), from the committed NYSE calendar.

**Sleeve.** One of an IRA's two parts: the momentum sleeve and the bond
sleeve. See [glide path](glide-path.md).

**Stale episode.** A stretch during which IBKR's latest snapshot is too old
for a sell to be checked against it; it carries one urgent alert, however
many sells it refuses.

**Stream.** The market-data files the **data job** writes for a run, which the
adapter hands to the engine in order, each input once, behind a fresh
**broker snapshot**. See [market data](market-data.md).

**Strategy version.** `<strategy>/<rules version>+<build>`, carried on every
record, for example `boost/1.18.0+<commit>`.

**Synthetic report.** In dry-run only, the engine's own "never sent,
cancelled" record for each order a momentum IRA releases, so the next month
is not held back. It is never an execution. See [engine](engine.md).

**Torn tail.** A partial group at the end of a log, left by a crash in the
middle of a write. A resume leaves it out; nothing from it was released.

**Undecided order.** An order Boost released that IBKR has neither
accepted nor rejected yet: no fill, and no report but "submitted". Each
decision lists them (`undecided_orders`), so IBKR's late word on any of
them counts after a restart as it would have without one. An order leaves
the list at its first word beyond "submitted" (a fill, an expiry or
cancellation, or a rejection), or, if the adapter never sent it,
when you record `operator rebalance-cleared` for it; a dry-run lists none
(`no_venue: "dry-run"`), since no order reaches IBKR. See
[boost](boost.md#a-refused-or-unfilled-order-is-planned-again-re-plan).

**Universe.** For momentum, the 500 US common stocks with the highest
dollar volume (at $5 or more) on the decision session. See
[momentum universe](momentum-universe.md).

**Unreadable.** A figure IBKR did not deliver, or delivered in a form the
engine cannot hold exactly. It is marked unreadable and never sent as zero.
See [reconciliation](reconciliation.md).

**Warm-up.** The history a run reads before its first decision, so its
averages are ready. For Boost, from the configuration's `warm_up_start`.

**Working** (an order). Open at IBKR and still able to fill. In a momentum run the engine
treats a released order as working until a report or your clearance ends it. See
[order lifecycle](order-lifecycle.md).

**Withheld.** A decision that sets no targets, with a reason
(`missing-bars`, `stale-account-snapshot`, `labels-missing` and so on), so a
session or month is never skipped silently. Also a sale back to 98% not
placed (`risk.sale.withheld`).
