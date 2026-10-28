# The morning reconciliation

## What it does and why it exists

The engine keeps its own record of what each account holds. IBKR keeps the
real one. They can drift apart: a fill report goes missing, a dividend posts,
someone trades by hand, or IBKR liquidates a position. The morning check
built so far catches only some of these (see "What the morning check covers,
and what it does not"). Every decision the
engine makes rests on what it believes you hold, so a difference it cannot
explain is a reason to stop taking on new risk (ADR 0019).

At every start of the IBKR adapter, at every reconnect, and with every
delivery of market data, the adapter reads the account's positions and cash
from IBKR and records them in the journal. A delivery's goes ahead of that
day's account snapshot, so the engine compares IBKR's holdings with the book
it holds before the new snapshot replaces it: shares no fill explains are
found, never taken over. A run's first delivery carries it right behind the
first account snapshot, which is the engine's first book. A start's or a
reconnect's is read only once every trade IBKR replayed has its commission
and its fill has reached the engine. Before the engine holds any account
snapshot there is no book to compare, so the adapter sends none at a start
or reconnect then. At a start or reconnect the adapter also reads IBKR's
open-order list, each time to its end, until two reads in a row agree, and
then the account, whose own list must show the same working orders (if an
order filled, was cancelled or was placed meanwhile, both are read again, up
to four account reads), before it compares anything or sends any order: an
order still working at IBKR is matched to the engine's order, never reported
missing because one read was cut short. After each of these reads the
adapter waits until IBKR has sent no open-order message for 2 seconds; a
read during or after which an order or an end-of-list mark arrived that no
list was waiting for may have been cut short, so it counts as no read and
the list is read again. Messages that never pause for 2 seconds within 10
drop the link, which is made again and matched again; a link dropped during
three reconciles (these reads and the account read) in a row, with none
completed between them, stops the adapter (`ReconcileIncomplete`). An
execution that arrives during these reads and is not among the executions
just read (an order placed by hand that filled at once) has everything read
again. After three
disagreements in a row, or four account reads that never agree, the adapter
stops (`ReconcileIncomplete`) and nothing is sent; start it again once
nothing is being placed, filled or cancelled at IBKR. The engine compares them with its own book and decides one of four
things:

| Result | What it means | What happens |
| --- | --- | --- |
| **matched** | IBKR's figures equal the book, and nothing has changed since it | nothing; no alert |
| **explained** | they differ, but journalled events account for every difference exactly | nothing; no alert |
| **unexplained** | a difference no journalled event accounts for | the account goes **Degraded** on exactly what differs, and you get one urgent alert |
| **unverifiable** | IBKR's figures could not be read, or cannot be compared | the account goes **Halted**, and you get one urgent alert |
| **pending-cash-read** | only the cash differs, on a day the Flex reader has not yet read (a deposit or withdrawal it has not yet recorded may explain it) | the account's state does not change, and you get one reconciliation alert. **More cash** than the book (like a deposit): buys are not stopped, but they are sized without the new cash until the reader's record explains it, so a credit IBKR reverses is never invested. **Less cash** (like a withdrawal): every buy waits (refused `pending-cash-decrease`) until it is settled, and the first refused buy sends one more urgent page, the later ones none; sells and exits go on. The difference is checked again at each later reconciliation (below, "A deposit or withdrawal you make") |

A matching reconciliation changes nothing and sends nothing.

## What the morning check covers, and what it does not

The book is replaced by every close's account snapshot, and that snapshot
already contains the session's fills. Two checks keep a difference from
slipping into it:

- **The reconciliation with each delivery** compares IBKR's holdings and cash
  with **the previous close's snapshot, plus the events the journal recorded
  since that close**. It is taken when the session's market data is
  delivered (normally the morning after the close), from the same reading as
  the new snapshot, and goes ahead of it, so it covers the whole session as
  well as the night before. It catches a manual trade placed overnight, before the open
  or during the session, a dividend, split or fee posted overnight, IBKR
  liquidating a position, a fill whose report the engine never received, and a
  fill the journal recorded that IBKR does not show.
- **The close-snapshot check** (built 2026-10-21, ADR 0019 amended): before a
  new account snapshot becomes the book, the engine itself compares the
  snapshot's holdings with the book in force plus the fills, dividends and
  splits journalled since. A holding they do not explain (shares bought or
  sold by hand during the session, a fill the engine was never given, or a
  fill the journal recorded that IBKR does not show) is an unexplained
  difference: the account goes **Degraded** on that stock and you get the
  reconciliation alert, its ID `close-snapshot:<snapshot ID>`. A delisting,
  merger or symbol change journalled since the book cannot be applied, so the
  check is unverifiable and the account goes **Halted**, as for any
  unverifiable reconciliation. This does not rely on the adapter: it holds
  for every snapshot after a run's first, stamped at the close or at the read
  time, whether or not a reconciliation came with it. When the delivery's
  reconciliation already found the same holdings against the same book, with
  no fill or corporate action journalled after it or dated between the
  reconciliation's reading and the snapshot's (whichever came first), the
  check sends nothing more: one difference is one alert. A Halt is sent
  unless the account is already Halted for the same change. It compares holdings only, never cash.

What neither catches:

- **A difference within one session that leaves the holdings as they were**,
  such as a sale and a repurchase of the same shares by another program. It
  shows only in cash, which is compared as one net total (below) and, for the
  joint account, not at all.
- **The run's first book.** The engine has nothing to compare a run's first
  snapshot with; the adapter compares that reading with the previous run's
  last close or the starting book you approved, and halts the account if it
  differs ("What can go wrong").
- **A difference that remains in the close snapshot after arising between the
  checks while the market is open.** The after-fill, pre-order and scheduled
  reconciliations ADR 0019 asks for are not built yet (see the last section),
  so such a difference is found at that session's close, not when it happens.

## What "the book" is

The book is the last account snapshot the engine read: the holdings (and, for
an IRA, the cash) IBKR reported at the previous close, which is what the
engine sized its orders from. The reconciliation starts from that and applies
every event the journal recorded since:

- **a fill** of one of our orders: its shares, and its cost or proceeds and
  commission;
- **a dividend**: the cash IBKR posted;
- **a split**: the holding converted at the split's ratio, and any cash paid
  for a fraction of a share;
- **a deposit or withdrawal** the Flex reader recorded (below): the cash in
  or out, and nothing else. One dated before the book is already in it; so
  is one dated the evening of a close, after 4 p.m., since the close's
  reading is taken after the close (the next morning).

Anything else that changes a holding (a delisting, a merger, a symbol change)
is not something the book can apply yet, so a reconciliation that meets one is
unverifiable.

The rules are strict on purpose:

- **Every share and every cent counts.** There is no "small enough to
  ignore". A difference of one share or one cent, in either direction, is a
  difference.
- **The engine never copies IBKR's figures to make a difference go away.** It
  only accepts a difference that a recorded event explains.
- **Timing.** An event stamped in the same second as a snapshot counts as
  coming after it, so it is never counted on both sides. A fill stamped in the
  same second as the morning read is therefore not in that read; if IBKR's
  figure does include it, the result is a difference and the account is
  Degraded. That is the safe side to err on.
- **Cash.** Cash is compared in whole cents. Each fill's value (its quantity
  times its price, worked out exactly) and its commission are each rounded to
  the cent, with a half cent rounded away from zero: $0.145 is 15 cents. The
  joint account's book (Boost's account snapshot) carries no cash yet, so its
  cash is not compared; the result says so. Its positions are.
- **Cash is one net total.** IBKR's total cash (`TotalCashValue`) is a single
  figure, so the engine can compare only the whole: the book's cash plus every
  journalled fill, commission, dividend, cash in lieu, deposit and withdrawal,
  against IBKR's one number. When they differ, the result lists those parts
  beside the difference (`deposits and withdrawals` only when there was one). But two activities the journal has not recorded that cancel
  each other out (a $12.30 fee and a $12.30 credit, say) leave the total
  unchanged, so they are **not detected**, and a recorded dividend does not
  prove that nothing else moved. Every result that compares cash says this in
  its detail. Matching each cash activity to one of IBKR's own activity rows
  is not built yet.

## What Degraded means here

The scope is exactly what differs:

- **A holding that differs** (SPY 140 at IBKR where the book says 147, a
  position IBKR shows that the book does not, or one the book has that IBKR no
  longer shows): that instrument is frozen. No order goes for it, except, for
  the joint account, the one sale back to 98% described in
  [states-and-kill-switch](states-and-kill-switch.md). Sells on other
  instruments carry on.
- **Cash that differs**: no instrument is frozen. Every buy is refused, since
  buys are sized from cash, and sells carry on.

A later difference widens the scope; it never narrows it. A later matching
reconciliation does **not** bring the account back to Normal. Only you can,
through the recovery procedure in `docs/runbooks/reconciliation.md`: a
fresh run you approve, clearing the incident by its ID.

## What you see

The journal records, for every reconciliation:

- `account.reconciliation`: what IBKR reported, recorded as an input, so a
  replay never asks IBKR again;
- `risk.reconciliation.result`: the engine's verdict, with every component
  listed (unchanged ones too), its book, expected and observed figures, the
  difference, and the events that explained it. A close-snapshot check that
  found a difference is recorded the same way, its ID `close-snapshot:` and
  the snapshot's ID, its input the `account.snapshot`, and cash not
  compared;
- `strategy.engine.state`, only when the state moved: Degraded with its scope,
  or Halted, with the reason `reconciliation-unexplained` or
  `reconciliation-unverifiable`.

A replay of the same journal gives the same verdict and the same state change,
byte for byte.

The alert (see [alerts-and-dashboard](alerts-and-dashboard.md)) is one per
mismatch, urgent, on every channel you named (Pushover and ntfy). The
engine-state change the same reconciliation causes is part of that one
incident, not a second alert. A reconciliation that matches or is explained
sends nothing. It says what
differs, the state the account is now in, what it still does and what it has
stopped doing. The state is the account's whole state, not just this
reconciliation's: an account already Halted is told it is still Halted, and a
Degraded account's scope includes what was already frozen. It repeats (every 30 minutes by default) until you acknowledge it
with `operator acknowledge` ([operator](operator.md)). On the public ntfy
relay the alert names what is in doubt but carries no figure; the figures
go to Pushover, your own ntfy server and the dashboard, which shows the
full alert. `journal-query refusals` shows the engine-state change and its
detail, but no `journal-query` report lists the reconciliation result's
figures themselves ([journal-query](journal-query.md)). A reconciliation
that could not be verified says only that on the public relay, and sends you
to the private channel for the reasons, which can carry amounts.

## What the adapter sends

The adapter reads positions, the account values (net liquidation, total cash,
Excess Liquidity, from IBKR's account updates: [the IBKR adapter](ibkr-adapter.md),
"How the balances are read") and every open order in one pass, then asks IBKR the time.
The open orders in the records below are the list taken in that one-pass
read. The rule of two agreeing reads (and the stop after three disagreements)
belongs to the start's and reconnect's reconciliation read, described at the
top of this page, not to this read. The adapter asks for one list at a time,
so an earlier list's late orders are never read as a later one's, and takes
the list and end-of-list mark IB Gateway sends unasked as a session starts
before it asks for anything. Whether a live Gateway always sends that mark in
time is still to be confirmed on the first live Gateway ([the IBKR adapter](ibkr-adapter.md), "Reports").
From that one read it builds the records the engine reads:

- the engine's **account snapshot** (`account.snapshot`), positions and net
  liquidation, the **book** the strategy decides from;
- the **broker snapshot** (`account.broker.snapshot`), which the risk gate
  sizes sells and the sale back to 98% from, and the margin alarm watches;
- the **reconciliation** (`account.reconciliation`), positions and total cash
  for the comparison, stamped with the true read time.

After a session's close, the account and broker snapshots are stamped as of
that close when the reading can stand for it (your decision of 2026-10-18:
nothing working at IBKR, no execution since the close, read on the close's
own date or, from your decision of 2026-10-27, the next morning with the
Flex reader's executions proof, below), else at the read time ([paper trading](paper-trading.md), "A
paper day"). The reconciliation always carries the read time, so it is
never earlier than the book it is compared with.

Its main loop sends all three with every delivery of market data, the
reconciliation ahead of the snapshots once the engine holds a book, and a
reconciliation alone at every start and reconnect once it does.

Every field is always stated. A list with nothing in it is sent as an empty
list. Something IBKR did not deliver, or delivered in a form the engine cannot
represent (a fractional share, an option, a balance in another currency, an
order with an unknown status), is named as **unreadable** and carries no
value; it is never sent as zero. Times are whole seconds. If IBKR could not
answer the time after the read, the read is not bounded by IBKR's clock and
every component is unreadable.

Two conservative choices, both of which can only make a sale smaller:

- an open order's remaining quantity is its total quantity, since IBKR's
  open-order report does not carry what has filled;
- an order IBKR holds back (`Inactive`) counts as one that may still execute.

## Deposits and withdrawals, read from IBKR

**In short: deposit any time; nothing to do.** The reader records it the
next weekday morning (from 5:30 a.m.) once IBKR's statement lists it,
sometimes a day later, and the strategy invests it at the next decision after
the reader has recorded it (until then buys are sized without it). A deposit
of more than 1% of the account's value (a typical IRA contribution is), if
the reader has not recorded it before the session's morning reading, makes
that reading stamped at its read time, so the engine skips that session's
decision and you get that page too; that is expected, and it is still nothing to do. A
withdrawal is the same, except that buys wait until the reader has recorded it
(sells and exits go on); the next decision then sizes from what is left. (Keep
a withdrawal within the account's cash: IBKR refuses one larger, and the
program treats a recorded one as a difference.)

### A deposit or withdrawal you make

What happens to an IRA's reconciliation when you move money (built
2026-10-21, ADR 0019 amended; the joint account's book carries no cash, so
there nothing changes and the next decision simply sees the new net
liquidation):

1. **During the day**, the money lands at IBKR. The Flex reader runs only
   the next weekday morning (from 5:30 a.m.), and IBKR may list a day's
   transfer only a day later, so a reconciliation before the reader has
   recorded it finds IBKR's cash higher (or lower) than the book and nothing
   recorded to explain it.
   Because the reader has not yet read that day, this is a **pending cash
   read**, not a difference: one reconciliation alert, `<account>: reconciliation
   pending-cash-read (<id>)`, and the account's state does not change. What
   it does to buys depends on which way the cash moved:
   - **more cash than the book** (a deposit): buys carry on, but they are
     sized from the cash in IBKR's snapshot that the decision used, less
     the new cash, so money IBKR reverses or posted in error is never
     invested. The new cash is taken off whenever the snapshot's cash could
     include it: the reconciliation that found it was read no later than
     the snapshot's cash. A close's snapshot is labelled with the close but
     read after it (the next morning, when the day's file arrives),
     alongside its reconciliation, so it counts as holding everything up to
     midnight New York time on the close's day; a reconciliation of that
     day is taken off, and one on a later day is not. If the engine cannot tell
     how long the snapshot's cash holds, it takes the new cash off. The
     deposit is invested at the first decision after the reader's record
     explains it;
   - **less cash than the book** (a withdrawal): every buy on the account
     waits, refused with the reason `pending-cash-decrease`, so no order is
     sized on cash that may not be there; sells and exits go on, as while
     Degraded. Each refused buy is recorded, but only the first one pages you
     (`<account>: order refused (pending-cash-decrease)`), once for the whole
     pending difference. Buys go again as soon as the difference is settled.
   A reconnect that reads the account again before the book changes finds
   the same difference, even after a trade has filled: its result says so,
   it sends no second alert, and it is settled with the first.
   The alert's text says how much, which way, on what day, the state the
   account stays in, whether buys continue, and when it becomes Degraded,
   for example: "trad-ira: cash differs from the book by 500.00 USD (an
   increase, like a deposit) on 2026-10-21; the Flex reader has not covered
   that day yet; the account stays Normal; buys continue, sized without the
   increase until the reader explains it; it becomes Degraded if unexplained
   after the Flex reader covers 2026-10-21 or more than 2 sessions close
   since, whichever comes first." For a withdrawal it says "(a decrease,
   like a withdrawal)" and "buys are withheld until the reader explains it,
   and sells and exits continue". The public relay's copy leaves the amount
   out.
2. **The next morning**, the Flex reader runs first (5:30), and records the
   transfer if IBKR's statement already shows it; the adapter's reading then
   finds it explained. If the statement does not show it yet, the adapter
   compares the reading after the close with the previous close with the
   transfer unrecorded, so a deposit
   or withdrawal of more than 1% of the account's net liquidation is more
   than the adapter's drift bound: the reading is stamped at its read time
   (`not_as_of_close`), the engine withholds that session as stale, and the
   reporter pages that as well. A smaller one is within the bound: for a
   deposit, that session's decision goes ahead but sizes its buys without the
   new cash, still unexplained; for a withdrawal, its sells go and its buys
   wait, as step 1 says. A later morning's reading records the transfer in
   the ledger once IBKR's statement shows it, and the engine journals it
   before IBKR's next reading reaches it.
3. **At the first reconciliation after the reader has covered the day**
   (usually the next morning, sometimes the one after), the engine checks the
   pending difference again against what the reader has recorded since.
   Explained to the cent, it is settled: a `risk.reconciliation.result`
   saying `explained`, with the transfer's ID, and no alert. That is the
   usual end. The strategy invests a deposit at the next decision after the
   reader has recorded it, usually the following morning.
4. **If the record never explains it**: once the reader's status says it has
   read that day (`covered_to` on or after it) and the difference remains, or
   once more than **two sessions** have closed since it, whatever the reader
   says, the account goes **Degraded** on cash as for any unexplained
   difference (buys stop; you get the reconciliation alert). A difference
   never waits longer than that, so a reader set aside for a week cannot hide
   one.

A deposit the reader has already recorded when the reconciliation runs
(made a day or more before) is simply part of the expected cash: the result
is `explained`, and nothing is sent.

What still goes Degraded at once, as before: a holding that differs; a cash
difference when no Flex reader status has been recorded (the reader not set
up for the account); one on a day the reader has already read; and a
recorded withdrawal larger than the cash before it, which the result names
(the cash is counted in time order, so a deposit and a withdrawal on the same
day are never mistaken for an overdraft, and a withdrawal IBKR dates by day
alone counts after that day's sales, so one the day's sales paid for is not
either, while a deposit IBKR dates by day alone counts from the start of its
day). Only a transfer in the account's own
currency explains a difference.

**Do not approve a fresh run while a cash read is pending.** A fresh run
starts with nothing pending, so the difference would never be decided:
`operator approve` refuses, naming the pending difference, and the engine
refuses such an approval too. Wait for it to be settled, or, if you must
restart now, check the account in IBKR after the transfer and state that book
in the approval (`-positions`, `-cash`, `-currency`, as for a first run); the
book you state replaces the pending difference.


The adapter reads the account's balances, not the reasons they move. A deposit
or withdrawal you make changes IBKR's cash without any fill, so on its own it
shows only as cash drift (above, and the daily summary's `cash drift` and
`unexplained cash`). To record it, a small program, the **Flex reader**, asks
IBKR once a day for the account's cash transactions and writes down each
deposit and withdrawal.

**What is read.** IBKR's Flex Web Service gives a saved report, a *Flex
query*, to a program holding a *Flex token*. The reader asks for yours, reads
its **Cash Transactions** section, and sorts each row by IBKR's type:
deposits and withdrawals, dividends, broker interest paid and received,
commission adjustments, and other fees, each with IBKR's own transaction ID,
its date, amount and currency. The deposits and withdrawals go to the cash
ledger; the commissions, fees and broker interest go to a second ledger, the
costs ledger (below). Which times are a session's close comes from
the NYSE calendar the adapter and the data job share
(`adapter/marketdata/nyse_calendar.py`), which holds 2022 to 2030; past its
last year the reader refuses rather than guess (below, "What can go wrong").

**What is written.** Each deposit or withdrawal not already recorded becomes
one cash movement (`account.cash-movement`, version 2) in the account's
**ledger**, `/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl`
(`boost`, `trad-ira` or `roth-ira`): the amount (positive in, negative out),
the date, the currency, IBKR's transaction ID, and the account's equity
before it. A posting whose type the reader does not know is counted in the
status file (`unknown_types`) and named in its log, never dropped without a
word. Dividends and withholding tax are read but not written.

**The costs ledger.** Each commission, fee and broker-interest posting not
already recorded becomes one cost (`account.cost`, version 1) in
`/var/lib/trend-investing/flex/<instance>/costs.jsonl`, in the same folder as
the cash ledger, under the same lock: its kind (`commission`, `fee`,
`interest-paid` or `interest-received`), the amount as IBKR states it
(negative is money out of the account), the date, the currency and IBKR's
transaction ID. It follows the cash ledger's rules: a posting whose
transaction ID is already in it is never written again, a posting IBKR later
states with another amount, kind, date or currency is recorded again as a
restatement (below), never changed, and a ledger
line that is not as the reader writes it (or not UTF-8 text), or a file that
does not end with a whole line, is never appended to.

The costs are read **in their own step, after the cash ledger is written**,
so nothing in the costs can stop a deposit or withdrawal being recorded: a
cost IBKR restates to an impossible amount (not a finite number), a damaged costs ledger, or a costs status file that
cannot be written, sets aside the costs alone. The cash ledger, its period
and its status file are written as usual (`state` "read"), and only then do
the costs start: they keep their own status file beside the costs ledger
(`costs.jsonl.status.json`), "running" from the moment the costs step
starts, then "read" with the costs written and how
far they are recorded (`covered_to`), or "set-aside" (or "coverage-gap")
with the reason. The unit still fails, so you see it, and the reporter's
costs line says "costs incomplete" until a good reading clears it. The costs
also keep their own period (`costs.jsonl.coverage.json`), checked as the
cash ledger's is: the first costs reading (an installation whose cash ledger
was read before the costs were, or a lost costs period file) must start on
or before the New York day you approved the first run, or the costs alone
are set aside as a coverage gap until a statement reaching back to that day
is read. A reading that gets no statement at all (a rejected token, a
service error, a damaged cash ledger) sets aside both. A reading stopped
before its costs step leaves the previous costs status as it was, so the
costs fall behind and the reporter says so ("behind as of <day>").

**A cost IBKR restates.** When IBKR states a cost already in the costs
ledger with another amount, kind, date or currency, the reader never
changes the line it wrote: it adds a restatement line for the same
transaction (`ibkr-flex:<ID>:restated:1`, then `:restated:2`, and so on)
holding what IBKR now states, with a note naming the old and the new value
("restated: was fee -10.0 USD on …, now fee -11.0 USD on …"). The restatement is
written with the reading's other costs and the reading goes on, so the period
still advances; the costs status says how many were restated ("restated") and
the unit's log names each, once. A cost IBKR restates to zero is a reversal: it
is written as a restatement line with an amount of 0 (a zero amount is accepted
on a restatement line only, never on a new cost) and from then on counts at
zero, so the earlier amount no longer counts. For a fee or interest paid, or an
unmatched commission, the total falls by that amount; for interest received, the
net cost rises by it; a commission already counted on a fill still counts once,
from the fill. A cost IBKR restates to an amount that is not a finite number
is not written at all: the costs are set aside with the transaction named. The reporter and the dashboard count each cost once, at its
latest value. The costs text counts every restatement line, so a cost restated
a second time raises the count ("N restatement(s) by IBKR (latest <day>)",
where the day is the New York posting date (IBKR's as-of date) of the restatement
line last in the file, not the day IBKR restated it, so it can be older than the
reading and can move back when an older cost is restated), says how many
reversed a cost to zero, and says how many the last reading recorded ("N
restated in the last reading", from the costs status).

A cost IBKR dates before the New York day you
approved the first run is not written, as it belongs to the account before the
book you stated; a posting of nothing is not written. Nothing reads the costs
ledger to trade: only the reporter and the dashboard total it (alerts and
dashboard chapter, "Costs: commissions, fees and interest").

**The reader writes every commission, and the reporter counts each once.**
A trade's own commission comes from its fill: IBKR reports it with each
execution, and the engine's log keeps it on the fill. Cash Transactions
normally carries no per-trade commissions; the only commission type the
reader takes from it is "Commission Adjustments", IBKR's later corrections
of a commission, and those carry IBKR's numeric transaction ID, not an
execution ID. The reader cannot see the engine's log, so it writes every
commission posting it finds. The reporter and the dashboard leave out a
commission posting whose transaction ID is a logged fill's execution ID, or
a correction of that execution (the same ID but its last dot-separated
part), and count the fill's; and a corrected execution logged twice (the
same ID but its last part) is counted once, at its latest commission: the
one with the higher last part, whatever order the log holds them in, so the
original execution delivered again after its correction never replaces the
corrected commission. **This
matching is an assumption until the first live statement is checked**
against the owner's activity statement: it guards against IBKR listing a
trade's commission in Cash Transactions under its execution ID
(`0001f4e8.65a1b2c3.01.01`), which IBKR's documentation does not show. If
the first live statement shows per-trade commissions there under a numeric
ID, they would be counted twice, and the matching has to change before the
totals are relied on. **A further assumption:** a "Commission Adjustment"
under a numeric transaction ID that corrects a logged execution's commission
cannot be matched to that execution (nothing in it names it), so it is
counted as an adjustment, on top of the fill's commission; if IBKR also
redelivers the corrected execution with its new commission, that correction
would be counted twice. The first live statement settles this too. Interest
and fees have no fill, so they are always the ledger's.

**The equity before a transfer** comes from the engine's close snapshots
(the reading after each close), found back through the chain of runs:

- normally, the last close snapshot before the transfer, plus any transfers
  between that snapshot and this one;
- for a transfer IBKR times **after the close** (4 p.m. New York, 1 p.m. on
  an early-close day such as the day after Thanksgiving: the session's
  official close, from the same NYSE calendar the adapter stamps its close
  snapshot by), that day's close snapshot, less the transfer (and any later
  that evening): the snapshot is read after the data step (the next
  morning, from 6:05 a.m.), so it already holds the transfer. The record says so:
  `"equity_before_basis": "same-day-snapshot-less-amount"`;
- for a transfer **after you approved the first run but before its first
  close snapshot**, the first snapshot after it, less the transfer (and any
  others up to that snapshot). The market's move between the transfer and
  that close is in the figure, so it is an approximation, and the record
  says so: `"equity_before_basis": "next-snapshot-less-amount"`. Until that
  first snapshot exists the reading is set aside (below), and the first
  reading after it writes the transfer.

Both approximations take the close snapshot as read after the transfer, as
the next morning's reading is; a transfer IBKR times later on the close's own
day is counted as held by it. What the records are for, the running record of money in and out,
needs only each transfer's amount and date, which are exact.

- **Each transfer once.** The ledger is the record of what has been read: a
  transfer whose transaction ID is already in it is never written again,
  however many days IBKR's report repeats it. If IBKR ever states a recorded
  transfer with another amount, date or currency, nothing is changed and the
  reader stops (below).
- **Only this account.** A report for another account, or with a row for
  another account, is refused whole.
- **The first book is the one you stated** when you approved the first run
  (the starting book, `operator approve ... -positions ... -cash ...`), and
  the approval's time is its cutoff. A transfer dated **before** that time is
  already in that book, so it is not written: the reader's log names it and
  the status file counts it (`in_first_book`). A transfer at or after that
  time is always written, never skipped. A transfer IBKR dates by **day
  alone on the approval's own day** (IBKR usually dates deposits and
  withdrawals so) depends on when you approved:
  - **before that day's close** (4 p.m. New York, 1 p.m. on an early close):
    it is taken as after the approval and written. You read the book in IBKR
    before the day's business, so the book cannot hold it. Its equity before
    is the first close snapshot after it less the transfers up to it
    (`next-snapshot-less-amount`), and the status file counts it
    (`date_only_on_approval_day`). If such a transfer was in fact made before
    you read the book, it is in both the book and the ledger: tell the
    developer.
  - **after that day's close** (an evening approval, as after a restart or a
    recovery, or on a weekend or holiday): the day's business may be in the
    book you read or not, so the reader cannot tell. That reading writes
    nothing and stops as an **ambiguous transfer**, naming the transfer and
    its amount (below, "What can go wrong"), until you record which side it
    is on. Every later reading honours what you recorded. To avoid this,
    move no money on the day you approve a first run.
- **Times.** The reader dates a posting by its Date/Time field alone
  (`dateTime`). IBKR's documentation gives it in New York time; that is
  taken as so until the first live statement has been checked against your
  activity statement. A date with no time is read as the start of that New
  York day, so the equity before it is the previous close's (on the first
  run's approval day, see the first book, above).
- **A transfer IBKR adds a day late.** It is written with the right equity
  before it. If a transfer from the same stretch between snapshots was
  already written and its figure should have counted this one (a transfer
  later the same day before the close, or earlier the same evening after
  it), that earlier record is **not** changed: records are never rewritten.
  The new record says so in its `note` ("a later transfer in the same
  interval was written first ..." or "an earlier transfer ..."), the
  reader's log names it, and the status file counts it (`out_of_order`).
  The earlier record's `equity_before` stays off by this transfer's amount.
- **No day is skipped.** Each reading keeps the days its statement covered
  (`cash-movements.jsonl.coverage.json`, beside the ledger) up to the day
  **before** its last day: IBKR may post a transfer for that last day after
  the reading, so the next statement must start on or before it
  and read it again (a transfer read twice is still written once). A
  statement that starts later would leave days unread, and a transfer on them
  would be lost once it dropped out of the query's window, so that reading
  writes nothing and stops as a **coverage gap** (below). The **first
  reading** must reach back to the New York day you approved the first run,
  since every transfer from then on is to be written: a 30-day query does
  when you turn the reader on no later than 29 days after that approval day
  (the first 30-day statement then still starts on or before it). Otherwise the
  first reading is a coverage gap naming both days, and you run the reader
  once with a query whose period starts on the approval day (below). A
  coverage file that is lost is treated the same way, as a first reading:
  first put it back, unchanged, from the server's nightly backup (the
  coverage file is part of the backup set once the nightly backup is built,
  which is not built yet; until then, from a Lightsail snapshot if you took
  one, see the server chapter, "Backups"), and the next reading goes on from it.
  Without a copy, the fallback is the custom-range reading below, which only
  reaches as far back as IBKR lets a custom period go: 365 days, from IBKR's
  documentation and not yet verified against the live Client Portal. When the
  approval day is further back than that, no reading can reach it: tell the
  developer rather than making the file by hand.
- **The engine reads the ledger, and never writes it.** Before each of the
  adapter's inputs it journals every record and the reader's latest status it
  has not yet journalled in the run (`account.cash-movement` and
  `account.flex-status`), so a replay reads them from the journal. Each line
  is checked as the reader writes it; a ledger with one line that is not is
  not read at all (the engine's log says which line), so a cash difference it
  would have explained is a difference. The ledger is read only by its own
  account's engine: one in another account's folder stops the engine at its
  start. A record can explain cash; it cannot place an order. The adapter's
  cash-drift check subtracts the recorded transfers before applying its 1%
  bound; a transfer the reader missed still shows as drift.
- **The SPY comparison reads it too.** The daily summary and the dashboard
  read the ledger directly (their `-cash-ledger` flag) and mirror each deposit
  and withdrawal in the SPY shadow account, so a transfer is never counted as
  a gain or a loss of the strategy (alerts and dashboard chapter, "How the
  comparison with SPY works"). Until the reader has written the ledger, or
  recorded a reading in its status file, they say the comparison assumes no
  transfer. They also read the status file's `covered_to` and say
  "transfers recorded through <day>", adding that deposits or
  withdrawals from the last two trading sessions may not be recorded yet when
  the summary's day is later (a transfer is in the summary two trading
  sessions after it posts: a Friday transfer in Tuesday's), and they take a
  ledger only from the account's own `flex/<instance>` folder.

**When.** `ti-flex@<instance>.timer` (one per account: `ti-flex@boost`,
`ti-flex@trad-ira`, `ti-flex@roth-ira`) runs it each weekday morning at
5:30, 6:30 and 7:30 New York time, each before one of the data job's
firings (6:05, 7:05, 8:05), because the morning delivery needs its
executions proof (below). IBKR refreshes an Activity statement once a day,
at close of business, so a morning reading holds the day before; IBKR may
still add a day's transfer to the report only after its own end-of-day
processing, so a transfer can appear a day late; the query covers the last
30 days (below), so the next reading writes it. A later firing the same
morning reads again and writes nothing twice.

**The IRA readers fire later than Boost's.** Every account's reader uses the
one Flex token, and only one reading holds it at a time (another waits at
most 4 minutes, then sets itself aside), so the IRA readers, when you turn
them on, must not fire at Boost's 5:30, 6:30 and 7:30. Give each its own
times with a drop-in before enabling its timer, 15 and 30 minutes later:
`sudo systemctl edit ti-flex@trad-ira.timer` and put

```
[Timer]
OnCalendar=
OnCalendar=Mon..Fri 05:45 America/New_York
OnCalendar=Mon..Fri 06:45 America/New_York
OnCalendar=Mon..Fri 07:45 America/New_York
```

then the same for `ti-flex@roth-ira.timer` with 06:00, 07:00 and 08:00
(the empty `OnCalendar=` clears the shared times first). Every reader, once
it holds the token, waits a full 61 seconds before its first request to
IBKR, so its requests never share a minute with the reader's before it,
even one stopped straight after a request. That costs each reading a
minute.

**The executions proof.** The adapter delivers a session's file the next
morning (Massive's free plan serves a day's prices only then) and reads the
account at that moment. IBKR's own list of executions, which the adapter
asks for, reaches back only to midnight, so it cannot show a trade between
the close and midnight (an after-hours trade, or one in IBKR's overnight
session, which starts at 8 p.m.). The Flex reader closes that gap: every
reading also writes
`/var/lib/trend-investing/flex/<instance>/executions.json`, the
**executions proof**, with every execution IBKR's statement lists for the
account (its execution or trade ID and time), the statement's first and
last day, when it was asked for, and `covered_until`, the moment up to
which the statement lists every execution: the start of the statement's
last day, and no later than an hour before it was asked for (IBKR: a new
execution is typically in a Trade Confirmation report within 5 to 10
minutes). The morning reading stands for the close only when the proof
reaches the midnight that starts the reading's day and lists nothing at or
after the close ([paper trading](paper-trading.md), "A paper day"; ADR 0019,
amended 2026-10-27). IBKR's own list starts at midnight in IB Gateway's
time zone, which its unit sets to New York's (`TZ=America/New_York` in
`ti-gateway.service`; do not change it, and keep the Gateway login's time
zone New York's). The adapter checks it at every connect and reconnect,
whether or not anything has executed: it reads the connection time the
Gateway gives (for example `20261027 07:10:01 US/Eastern`, or `20261028
09:49:19 EST`). Its zone must be a New York name (`US/Eastern`,
`America/New_York`, `EST5EDT`) or an Eastern label (`EST`, `EDT`, `Eastern
Standard Time`, `Eastern Daylight Time`). IBKR's label does not follow
daylight saving time (on 2026-10-28 a Gateway on New York time said `EST`
in summer time), so the label is not taken to say the season: instead the
time it states, read on New York's clock, must be within 5 minutes of the
adapter's own clock when it connected. A Gateway on a fixed `EST` all year
is an hour off in summer, and one in another zone is hours off; either
withholds the morning reading, and the message says how far off it was and
which way (for example `60 minutes 3 seconds behind`). In the hour repeated
when the clocks go back, the reading that matches the adapter's clock is
taken. The check holds only until New York's clocks next change: a link
that stays up (or that the Gateway restores without reconnecting) across a
change is not trusted after it, and withholds the morning reading until it
connects again (the Gateway's nightly restart does that). Every
execution's time must be in one of the same zones (the clocks are not
compared: an execution can be hours old). While the link's check holds it
is read on New York's clock whatever its label; until then, the adapter
records it at the time it heard of it (or the Gateway's time, if later),
so a fill after the close is never recorded as one before it, even by a
Gateway whose clock is slow. A fill recorded that way may carry a time
later than it happened: by as long as the adapter took to hear of it, or,
when the Gateway's time is the later, by as much as the Gateway's clock is
fast (hours, for a Gateway on another zone's clock). On an evening reading
over such a link, an execution listed since the close is withheld with the
Gateway's clock named first as the cause. Because a Gateway up to 5 minutes
slow still passes the check, an execution or fill stamped within five
minutes before that session's close (from 3:55 p.m. on a normal day,
12:55 p.m. on a 1 p.m. early close) is taken as at the close, and the
reading is withheld; the reason names each one and says whether it is
after the close or within those five minutes. A fill in the journal in
that window keeps that session's reading withheld for good (the journal
is never rewritten), on the evening reading as well as the next morning's;
the check reads every earlier run's log back through the chain of
approvals, however old. A log it cannot read withholds the reading too,
but only until it can be read.
An
execution at a time New York skipped when the clocks went forward stops the
adapter (send the journal line to the developer). Any other zone, a
connection time New York skipped, or a time it cannot read, withholds the
morning reading, the page and `journalctl -u ti-adapter-boost` quoting the
Gateway's text exactly as it gave it (send that to the developer if it is
New York time in a form not listed here); once the
Gateway is set right and the adapter reconnects, it is trusted again. IBKR's
Python library hands the connection time over as raw bytes rather than
text; the adapter turns them into text first. Whether it arrives as bytes
or as text, a time with any character outside plain ASCII is refused. If
the message quotes the connection time as `None`, the adapter had no
session or the Gateway's time was not plain ASCII text; that is a fault to send to the developer, not a Gateway
setting to change. A fill recorded with a
time that has no zone withholds it too.

**Only a Trade Confirmation query can prove the evening.** An Activity
statement (your cash query) is refreshed once a day, and IBKR's day for
securities closes at 8:20 p.m.: a trade after that, or in the overnight
session (dated the next trade day), is in the next day's statement, which
is ready only around the next midnight. So an Activity statement read the
next morning never reaches the midnight, and its Trades section, if you add
one, cannot prove the evening: every session would be withheld. The proof
comes from a separate **Trade Confirmation** Flex query (below, step 4),
read with the same token in the same reading, right after the cash query.
Without one (`TI_IBKR_FLEX_TRADES_QUERY_ID` empty) the reader writes
`"state": "none"` (a Trades section in the cash query is never used), and
the cash reading still succeeds; sessions delivered the next morning are
then withheld, each page saying why.

| `executions.json` field | When | What it says |
| --- | --- | --- |
| `state` | always | `"read"`, `"none"` (no trades query named) or `"set-aside"` |
| `account_id` | always | the account the statement was checked against; the adapter refuses a proof for any other |
| `checked_at` | always | when the reading started (UTC) |
| `query`, `section` | `read` | always `trades` and `TradeConfirms` |
| `from_date`, `to_date` | `read` | the statement's first and last day (`yyyyMMdd`) |
| `fetched_at` | `read` | when the reader asked IBKR for that statement |
| `covered_until` | `read` | every execution up to this moment is listed |
| `executions` | `read` | each execution's `id` and `at` (UTC) |
| `reason`, `code` | `none`, `set-aside` | why, and IBKR's error code (`set-aside` only) |

A trades query IBKR refuses, a statement with no Trade Confirmations
section, a summary row in it (the query must list executions, not
summaries), any element in the section that is not a trade confirmation,
an execution dated by day alone (keep the default date/time format, with
the time) or one of another account sets the proof aside alone: the cash
ledger and the costs are still written, the proof says `"set-aside"` with
why, and the unit fails so you see it. Once a reading holds the reader's
lock it rewrites the proof as `"set-aside"` ("a Flex reading began at ...
and has not finished") first, before its status file and before it asks
IBKR for anything, and again, whole, when it ends. So a reading stopped
part way (its unit's 90-minute limit, a reboot, a kill) leaves that
set-aside proof, and the morning delivery waits for the next firing, then
withholds. If the proof itself cannot be written, the deposits,
withdrawals and costs are still recorded, the earlier proof is deleted
(the morning delivery then finds none), and the unit fails, its journal
saying `the executions proof cannot be written`. A reading leaves the
earlier proof in place in these cases only:

- its command refused before reading (`refused:`: a missing argument, a
  secrets or account file it cannot read, a token or query ID line not
  filled in as it needs);
- it cannot open its lock file (the folder missing or not writable): a
  Python error naming the path;
- the proof can neither be written nor deleted (the folder not writable),
  which the journal says.

The adapter still checks any proof's account, period, fetch time and
coverage, so an earlier proof covers only the days it was read for.

**The trades query's dates.** The portal fixes a Trade Confirmation query's
period at *Today*, so the reader asks IBKR for the dates itself each
reading: the 7 New York calendar days before its own day through that day
(the Flex Web Service's `fd` and `td`). If the statement IBKR returns does
not cover them, the proof says `"set-aside"` with "IBKR ignored the
requested dates" and the two periods, and the unit fails. After the first
morning reading, `sudo cat /var/lib/trend-investing/flex/boost/executions.json`
should show `from_date` a week back (or the first session after it, when
that day was a holiday: IBKR's statement has nothing to cover before it),
`to_date` as that day and `covered_until` as that day's midnight (05:00 or
04:00 UTC); tell the developer if it does not. On a weekday holiday the
statement may end on the session before; it then covers only to that
session's start, so a file delivered that morning waits for the proof
until 09:00 and is then withheld, which is expected.

**What you set up once, in IBKR's Client Portal.** These steps, menu names
and formats are from IBKR's documentation, not yet verified against a live
statement; if a screen differs, follow what IBKR shows and tell the
developer.

1. Create the Flex query: **Performance & Reports → Flex Queries → Activity
   Flex Query → +**. Give it a name, tick the **Cash Transactions** section
   and, inside it, select all fields (or at least Account ID, Currency, Type,
   Date/Time, Amount, Transaction ID, Level of Detail). Set **Format** to
   XML, **Period** to *Last 30 Calendar Days*, and leave the date
   format (`yyyyMMdd`), the time format (`HHmmss`) and the date/time
   separator (`;`) at their defaults: the reader refuses any other. Save it
   and note its **Query ID**. Why 30 days: while the reader is set aside
   (an expired token, an outage, a refusal waiting for the developer) it
   writes nothing, and each statement holds only the query's window. A
   window shorter than the longest such stretch would let a transfer drop
   out unread; the reader then stops as a coverage gap rather than lose it,
   but 30 days gives you a month to put things right before that happens.
2. Create the token: **Performance & Reports → Flex Queries → Flex Web
   Service Configuration**, turn the service on and **generate a token**.
   Note the token and its expiry date; IBKR ends a token at its expiry, and
   you generate a new one then.
3. Put both in the server's secrets file, never anywhere else: `sudo ti-secrets
   edit secrets.env`, the lines `TI_IBKR_FLEX_TOKEN=` and
   `TI_IBKR_FLEX_QUERY_ID=` (no quotes, no spaces). The token is a password
   to your account's reports: never paste it into a chat, an issue or a
   command line.
4. For the morning delivery's executions proof, a second query:
   **Performance & Reports → Flex Queries → Trade Confirmation Flex Query →
   +**. Give it a name, tick **Trade Confirmations** and, inside it, select
   all fields (or at least Account ID, Symbol, Quantity, Date/Time, Trade
   ID, Exec ID, Level of Detail). Set **Format** to XML. The **Period** is
   greyed out at *Today*: leave it, the reader asks IBKR for the last 7 days
   itself (above, "The trades query's dates"). Leave the date format
   (`yyyyMMdd`), the time format (`HHmmss`) and the separator (`;`) at
   their defaults. Save it,
   note its **Query ID**, and put it in the secrets file beside the others:
   `sudo ti-secrets edit secrets.env`, the line
   `TI_IBKR_FLEX_TRADES_QUERY_ID=`. The same Flex token reads it. Adding a
   Trades section to the cash query instead does not work (above, "Only a
   Trade Confirmation query can prove the evening").
5. For an IRA account, its number in its own file, the same shape as
   `accounts/boost.env`: fill in `IBKR_ACCOUNT_TRAD_IRA=` with
   `sudo ti-secrets edit accounts/trad-ira.env`, or `IBKR_ACCOUNT_ROTH_IRA=`
   with `sudo ti-secrets edit accounts/roth-ira.env`. The provisioner creates
   both, encrypted, with the line named only, so an IRA reader started before you
   fill it in fails saying which line is empty (`systemctl status
   ti-flex@trad-ira` shows the reader's message). Do not delete the file: a
   missing one fails the unit before the reader runs, with only systemd's
   "Failed to set up credentials".
6. Turn the reader on (`docs/runbooks/lightsail-deployment.md`, "The Flex
   reader"). For an IRA reader, first give its timer later times than
   Boost's (above, "The IRA readers fire later than Boost's").

**When it cannot read.** The reader *sets itself aside* for that reading:
it writes nothing to the ledger, records why in the status file beside the
ledger (`cash-movements.jsonl.status.json`, `"state": "set-aside"`, with
IBKR's error code when IBKR gave one), and its unit shows as failed
(`systemctl --failed`, `journalctl -u ti-flex@<instance>`). Trading carries on.
While it is set aside, a cash difference stays a pending cash read for at most
two sessions and then degrades the account on cash (above, "A deposit or
withdrawal you make"), and the executions proof says `"set-aside"`, so the
morning delivery withholds its session after 9:00. The next firing tries again, and a good reading
puts the status back to `"read"`. The reporter does not page you about it yet (see "What is not
built yet").

**The status file.** `sudo cat
/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl.status.json`
shows one JSON object, rewritten whole at each reading that gets as far as
writing it:

| Field | When | What it says |
| --- | --- | --- |
| `state` | always | `"running"` from the moment a reading starts; `"read"`, `"set-aside"`, `"coverage-gap"` or `"ambiguous-transfer"` when it ends |
| `checked_at` | always | when the reading started (UTC) |
| `written` | `read` | how many cash movements this reading added to the ledger |
| `period` | `read` | the statement's first and last day, as IBKR states them (`yyyyMMdd`) |
| `covered_to` | `read` | the last complete day every reading so far has covered, the day before the latest statement's last day, kept in `cash-movements.jsonl.coverage.json`; the next statement must start on or before the day after it, so that last day is read again |
| `postings` | `read` | how many detail rows the Cash Transactions section held |
| `in_first_book` | `read` | how many transfers were dated before the first run's approval, so not written |
| `date_only_on_approval_day` | `read` | how many transfers IBKR dated by day alone on the approval's day, approved before that day's close, so taken as after it and written |
| `resolved` | `read` | how many transfers were placed by a decision you recorded with `--resolve` (those in the starting book are counted in `in_first_book` too) |
| `out_of_order` | `read` | how many transfers this reading wrote with a `note` that a record from the same stretch was written first |
| `unknown_types` | `read` | each IBKR type the reader does not classify, with how many rows had it (`{}` when none) |
| `reason` | `set-aside`, `coverage-gap`, `ambiguous-transfer` | why, in words |
| `code` | `set-aside`, `coverage-gap`, `ambiguous-transfer` | IBKR's error code, or `null` when IBKR gave none |
| `ambiguous` | `ambiguous-transfer` | the transaction IDs this reading set aside as ambiguous, the only ones `--resolve` accepts a new decision for; gone after the next good reading |

A reading writes `"running"` before it asks IBKR for anything, so if it is
stopped part way (its unit's 90-minute limit, a reboot, a kill), the file
keeps saying `"running"` with that start time, never the previous reading's
`"read"`. A `"running"` whose `checked_at` is more than 90 minutes old means
the reading was stopped: `journalctl -u ti-flex@<instance>` says why, and
the next firing reads again. A reading that stops before it writes `"running"` leaves the file as the last
reading left it, possibly an older `"read"`, so for a failed unit whose status file says `"read"` or has no
`checked_at` of this morning, read the journal, not the file. Every such stop is one of these, and
`journalctl -u ti-flex@<instance>` names it:

- The unit never reached the reader: `run.env` missing or empty (the start step prints which file); the secrets
  file or the account's file deleted (systemd's credential setup, status `243/CREDENTIALS`, which names no line);
  the flex directory missing (systemd cannot set up the unit's file access, status `226/NAMESPACE`).
- argparse rejected the command line before the reader ran, with exit status 2: an instance name that is not
  `boost`, `trad-ira` or `roth-ira` (`invalid choice`), or a missing `--ledger` (`the following arguments are
  required`). Under the unit, an unknown instance stops earlier still, at systemd's credential setup (`243/CREDENTIALS`,
  no `accounts/<name>.env`).
- The reader refused before it opened the ledger's lock, printing `refused:` (exit status 1): a missing
  `--secrets`, `--account-secrets`, `--instance` or `--engine-log`; a secrets file it cannot read; the Flex token or
  query ID line missing, empty, repeated, or quoted or holding a space; the account's line missing or empty, which is
  the expected first failure of a new instance (fill it in).
- The reader could not open its lock file (the flex directory missing or not writable): it stops with a Python error naming the path.
- Another reading holds the lock: this one waits for it, and writes `"running"` only once it has the lock; if the
  unit's 90-minute limit ends the wait first, the file keeps whatever it said.

A refusal that prints `refused:` is also the quick test: the status file was not touched. Every other set-aside,
including each row below that names a status file, is written to it.
Nothing is written to the ledger until a
reading has checked everything, so a stopped reading leaves it as it was.

## What can go wrong, and what the program does then

| What you see | What the program does | What you do |
| --- | --- | --- |
| Alert: `<account>: reconciliation <classification> (<id>)`, urgent, repeating until acknowledged; `<classification>` is `unexplained`, `unverifiable` or `pending-cash-read`, `<id>` the reconciliation's ID | Sent once per reconciliation that degrades or halts, and once per pending cash read: a later reading of the same pending difference, and its decision, send nothing more. Its text names what differs (or "evidence that could not be compared"), the account's whole state after it, what it still does and what it has stopped; on the public relay, no figures. It ends "Recovery is a person's: docs/runbooks/reconciliation.md." | Read the full text on Pushover or the dashboard. Follow `docs/runbooks/reconciliation.md`, "First, in every case". Acknowledge with `operator acknowledge` and the alert key the text ends with. |
| `reconciliation-unexplained`. Alert: `<account>: reconciliation unexplained (<id>)` | A difference no journalled event accounts for. The account goes **Degraded** on exactly what differs: a holding's instrument is frozen, unexplained cash refuses every buy. A later difference widens the scope; a later match never brings it back to Normal. | `docs/runbooks/reconciliation.md`, "Find the cause", then "Recover": explain each difference at IBKR, then start a fresh run whose approval clears the incident (`operator approve -clear-incident <id>`), or carries it in force if something is still unexplained ([restart](restart.md)). |
| `reconciliation-unverifiable`. Alert: `<account>: reconciliation unverifiable (<id>)` | The figures could not be compared, so the account goes **Halted**: no order is released, except, for the joint account, the one sale back to 98%. The private text lists why, for example `the broker's positions could not be read`, `the broker's total cash could not be read`, `no account snapshot has been recorded, so the journal has no book to compare`, `the observation as of ... precedes the book as of ...`, or `the broker's cash is in ...; the book's is in ...`. | Fix what could not be read (the adapter's link, `journalctl -u ti-adapter-boost`), then follow the runbook's "Recover". Leaving Halted is never automatic: it takes a fresh run you approve. |
| `pending-cash-read`. Alert: `<account>: reconciliation pending-cash-read (<id>)` | Only the cash differs, on a day the Flex reader has not yet read, so a deposit or withdrawal it has not recorded yet may explain it. **No state changes.** More cash than the book: buys are not stopped, but are sized without the new cash until it is explained. Less cash: every buy is refused (`pending-cash-decrease`) until it is settled, the first refusal paging once; sells and exits go on. The private text says how far the reader has read. At each later reconciliation the difference is checked against the transfers recorded since: explained to the cent, it is settled (a result `explained` for it, no alert); once the reader has read its day with the difference still there, or after more than two sessions, the account goes **Degraded** on cash (the `unexplained` row below, same `<id>`). The alert's text says so: the amount and direction, the day, that the account stays in its state (Normal, or the Degraded or Halted state already in force), whether buys continue, and when it becomes Degraded | If you moved money that day, nothing: it clears at the first reconciliation after the reader has covered the day, usually the next morning, sometimes the one after. Acknowledge the alert. If you did not, check IBKR's activity for what moved the cash. Do not approve a fresh run while a cash read is pending: `operator approve` refuses one unless you state the book you checked in IBKR after the transfer |
| A pending cash read becomes Degraded: the private text says the Flex reader `has covered <day> and its records do not explain it`, or that `<n> sessions have closed since it, more than the 2 sessions a difference may wait for the Flex reader` (the cap is `event.PendingCashReadSessions`) | The difference was not a transfer the reader recorded, or the reader was set aside too long: the account is Degraded on cash, buys stop | Check `ti-flex@<instance>`'s status file; if the reader was set aside, fix it (rows below) and check the ledger now holds the transfer. Then recover as for any unexplained difference (`docs/runbooks/reconciliation.md`) |
| The private text names `withdrawal ibkr-flex:<id> of <amount> is more than the cash on hand before it, ..., which IBKR refuses` | A recorded withdrawal larger than the account's cash: IBKR would have refused it, so the record and the account disagree. Degraded on cash at once, never pending | Check the withdrawal in IBKR's activity statement and tell the developer; recover as for any unexplained difference |
| The engine's log says `the Flex ledger ... is refused, so none of its transfers is delivered` (naming the line) | A ledger line is not as the reader writes it, so no record of that ledger reaches the engine; a cash difference a record would have explained is a difference. Said once until it changes | Put the line back as the reader wrote it (as for the reader's own refusal above); never edit the ledger by hand |
| The engine will not start: `the Flex ledger ... is instance <a>'s, but this engine's state folder is <b>'s` | `-cash-ledger` names another account's ledger. The records carry no account, so each account's engine reads only its own folder | Correct the unit (`ti-engine@.service` passes `${TI_FLEX_DIR}`, `/var/lib/trend-investing/flex/%i`) |
| The engine will not start: `the Flex ledger's folder: ... no such file or directory` | The account's Flex folder is missing (the provisioner creates it) | Run the provisioner's services step again (`docs/manual/server.md`) |
| A dividend, interest charge, fee or fill the journal has not recorded | Cash (or that instrument) differs, so the account goes Degraded: on cash, buys stop. **No command records the missing event**: `operator` has four commands (the kill switch, an approval, a rebalance clearance and an acknowledgement), and none records a market or account event. | Check the item on IBKR's activity, write down what you found, and either approve a fresh run that clears the incident (`operator approve -clear-incident <id>`) or one that carries it in force (leave `-clear-incident` out; for cash its scope is `"scope_cash_components": ["total-cash"]`, so buys stay refused) (`docs/runbooks/reconciliation.md`, "Recover"). |
| Private text names `corporate action ... (... of ...) is not one a target-weight book can apply` | A delisting, merger or symbol change: the book can apply only fills, dividends and splits, so the reconciliation is unverifiable and the account goes Halted. | Check at IBKR what the account now holds for that stock. There is no way to record the action for the program: clear the incident in a fresh run once the holding is understood. For an IRA, the next rebalance sells what the rule no longer targets. |
| The adapter's link to IB Gateway is lost (IBKR's message 1100, the connection closing, or both) while orders are working | Nothing is sent while the link is down. When it is back the adapter reads the positions, the cash and IBKR's whole open-order list again, matches the working orders to the engine's, and, once the engine holds a book, records a reconciliation for the engine to compare. A working order is matched, not reported missing: the reconnect itself changes nothing in the account's state. The adapter waits on a closed connection not at all, and for Gateway to restore IBKR's servers at most `restore_timeout` ([the IBKR adapter](ibkr-adapter.md), "What can go wrong") | Nothing, unless the adapter stops: then the stop's own row in [the IBKR adapter](ibkr-adapter.md) |
| An account has no account snapshot yet (a start or reconnect before the run's first delivery) | The adapter sends no reconciliation: with no book to compare, it would be unverifiable and halt the account. The run's first delivery carries the first, right behind its account snapshot. | Nothing. |
| A run's first reading is not the book it was compared with (the previous run's last close, or your starting book) | The adapter sends its reconciliation alone and withholds its snapshots, so the engine, holding no book, finds it unverifiable and **Halted**; nothing of that reading is taken as the book, by this run or the next. | Clear it with a fresh run whose approval states the book you check in IBKR (`operator approve -log <the halted run's log> -account <account> -by owner -clear-incident <id> -positions SYM=<shares> -cash <total cash> -currency USD -out <approval file>`, typed right after reading the book in IBKR; [restart](restart.md)). Without a stated book, the fresh run is compared with the same book again and halts again. |
| The daily summary shows `cash drift` and `unexplained cash` | Information only: the drift since the previous close and that drift added up since the cash was last explained (an explained reconciliation, else the starting book). The program does not act on it, and the post-close reading is stamped at its read time instead of the close when the drift exceeds 1% of net liquidation (the session is then withheld). | A small posting day (interest, a dividend) is expected; a figure that keeps growing is a cash movement to find at IBKR before approving anything. |
| Margin interest on the joint account | Not compared: Boost's book states no cash, so the result says cash is not compared. Nothing is seen. | Check the interest on IBKR's monthly statement yourself. |
| A $12.30 fee and a $12.30 credit, both unrecorded | Not detected: cash is compared as one net total, and the two cancel. | The statement is the only check. |
| `ti-flex@<instance>` failed; its status file says IBKR's Flex Web Service **rejected the token or the query** (error 1012 the token expired, 1015 the token is invalid, 1014 the query is invalid, 1011, 1013 or 1016 the service, its IP address or the account is not allowed; these codes are from IBKR's documentation, not yet verified against a live statement) | The Flex reader is set aside for this reading: no request for the statement is made, nothing is written to the ledger, and the token is never printed. Trading is unaffected. It tries again at the next firing | In Client Portal, check the Flex Web Service is on and the token not expired (generate a new one if it is), and the query ID; correct `TI_IBKR_FLEX_TOKEN` or `TI_IBKR_FLEX_QUERY_ID` with `sudo ti-secrets edit secrets.env`, then `sudo systemctl start ti-flex@<instance>` and check `journalctl -u ti-flex@<instance>` says it read |
| `ti-flex@<instance>` failed; its status file says the Flex Web Service **gave no statement** (busy, error 1009; too many requests from the token, 1018, still answered after the reader asked again 10, 30 and 60 seconds later; not ready, 1003 to 1008; still generating after 30 tries 10 seconds apart, 1019; the codes from IBKR's documentation, not yet verified against a live statement; or IBKR could not be reached, or answered with a redirect, which the reader never follows since the token would go with it) | Set aside for this reading, nothing written; the next firing asks again. The reader keeps IBKR's limit of 1 request a second and 10 a minute for the token across both queries, every poll counted, and starts the trades query 5 seconds after the cash query. Every account's reader shares the one token, so only one reading holds it at a time (a lock on `/var/lib/trend-investing/flex`; the reader that takes it waits 61 seconds before its first request, so a reader stopped straight after a request still leaves its minute to the next); another account's reader waits for it at most 4 minutes, then sets itself aside, asking IBKR nothing, with "another Flex reader on this server held the Flex token". So a 1018 should come only when something off the server uses the same token | Nothing, if the next firing reads. If it keeps failing, check the server's network and IBKR's system status; for 1018, that no other program (a second server, a portfolio tool) asks IBKR's Flex Web Service with the same token |
| `ti-flex@<instance>` failed; its status file says the Flex statement **is for another account**, or a row is | The whole statement is refused and nothing of it is written; neither account number is printed | Make the Flex query cover this account (Client Portal, the query's account selection); for one query covering several accounts this account must be among them |
| `ti-flex@<instance>` failed; its status file says the statement **has no Cash Transactions section** | Refused: without the section the reader cannot tell "nothing moved" from "not asked for" | Edit the Flex query in Client Portal and tick **Cash Transactions** (above, "What you set up once") |
| `ti-flex@<instance>` failed; its status file says the statement **is not XML**, or a row has no transaction ID, type, currency, a number for its amount, or a date as `yyyyMMdd[;HHmmss]` | Refused whole; nothing written | Set the query's format to XML and its date, time and separator formats back to the defaults (`yyyyMMdd`, `HHmmss`, `;`) |
| `ti-flex@<instance>` failed; its status file says **transaction N is in the ledger as one amount, but IBKR now states another** | Refused: a recorded cash movement is never changed or written twice | Check the transfer in IBKR's activity statement. Leave the ledger as it is and tell the developer: a corrected transfer needs its own recorded correction, which is not built |
| `ti-flex@<instance>` failed; its status file says **the ledger line N is not a cash movement as this reader writes it**, or the ledger **does not end with a whole line** | The ledger is never appended to while a line in it cannot be read: each line's fields and their types are checked (an amount and an equity before as numbers, the currency as three capital letters) | Put back the line as the reader wrote it (from a backup or the server's snapshot); never edit the ledger by hand |
| `ti-flex@<instance>` failed: "the secrets file ... does not hold exactly one filled-in TI_IBKR_FLEX_TOKEN line" (or `TI_IBKR_FLEX_QUERY_ID`, or the account's line) | Refused before anything is asked of IBKR | Fill the line in (above, "What you set up once"), or turn the reader's timer off until you have: `sudo systemctl disable --now ti-flex@<instance>.timer` |
| `ti-flex@<instance>` failed; its status file says **transfer N ... is after the first run's approval, but no close snapshot is committed yet** | Set aside: the equity before it cannot be known until the first close snapshot. Nothing written | Nothing: the first reading after the first close snapshot writes it. If it persists past that close, check the engine and the adapter ran the close |
| `ti-flex@<instance>` failed; its status file says the transfer **is in** another currency, is **of nothing**, or **would take the equity before it to zero or below** | Refused: none of these is a cash movement the engine can apply. Nothing written | Check the transfer in IBKR's activity statement and tell the developer |
| `ti-flex@<instance>` failed; its status file says **the account snapshot ... is at version N, which this reader does not read** | Refused rather than read as if no snapshot were there, which would misdate every transfer. Nothing written | Tell the developer: the reader must be updated with the adapter |
| `ti-flex@<instance>` failed; its status file says **the chain of runs ... ends without a first run's approval stating the starting book** | Refused: without the starting book's time the reader cannot tell a transfer in the first book from one after it | Tell the developer; check `run.env` names the account's current run |
| `ti-flex@<instance>` failed; its status file says `"state": "coverage-gap"`: **the Flex statement starts on D2, but the last reading covered up to D1** (and names the day the next statement must start on or before, the day after D1) | Nothing written, and the kept days not moved on: a transfer between D1 and D2 would otherwise be lost. Every later reading stops the same way until a statement reaches back | In Client Portal, set the Flex query's period to reach back to the day it names (a custom date range, or *Last 365 Calendar Days*), run `sudo systemctl start ti-flex@<instance>` and check it read; then set the period back to *Last 30 Calendar Days*. A transfer already in the ledger is never written twice |
| `ti-flex@<instance>` failed; its status file says `"state": "coverage-gap"`: **the Flex statement starts on D2, but no earlier reading is kept ... and the first run was approved on D1** | The first reading (or the first after the coverage file was lost) starts after the approval day, so a transfer between D1 and D2 would be in no statement. Nothing written, no coverage kept | In Client Portal, set the Flex query's period to a custom date range starting on D1 and ending today, run `sudo systemctl start ti-flex@<instance>` and check it read; then set the period back to *Last 30 Calendar Days*. If the coverage file was lost, put it back from the nightly backup once it exists, or until then from a Lightsail snapshot if you took one (see the server chapter, "Backups"); a custom period reaches back 365 days at most (IBKR's documentation, unverified), so if the approval day is further back than that, tell the developer. Turning the reader on no later than 29 days after the first run's approval day avoids this |
| `ti-flex@<instance>` failed; its status file says `"state": "coverage-gap"`: **the Flex statement ends on D1, but a current one ends on D2 or the session before** (the period was left as a custom range, or is not Last 30 Calendar Days) | Nothing written, the coverage file not moved on: transfers after D1 are in no statement, and every later reading would return the same window. D2 is the last NYSE session before the reading's New York day; a statement ending on D2, the session before it, or yesterday's date is read, so a weekend or a market holiday never trips it | In Client Portal, check the Flex query's period and set it back to *Last 30 Calendar Days*, run `sudo systemctl start ti-flex@<instance>` and check it read. If the period already is Last 30 Calendar Days (IBKR ended it earlier than expected after several closed days), the next reading clears it; tell the developer if it repeats |
| `ti-flex@<instance>` failed; its status file says `"state": "ambiguous-transfer"`: **the first run was approved at T, after that session's close, and IBKR dates transfer N of AMOUNT by that day alone** | IBKR gives no time for the transfer, and you approved after the day's close, so the reader cannot tell whether the book you stated holds it. Nothing written, the account number never shown; every reading stops the same way until you decide. Trading is unaffected | Check in IBKR's activity when the transfer reached the account against when you read the book. Then record it, once: `sudo -u ti-trader env -C /opt/trend-investing/adapter /opt/trend-investing/adapter-venv/bin/python3 -m adapter.ibkr flex --ledger /var/lib/trend-investing/flex/<instance>/cash-movements.jsonl --resolve N=in-first-book` if the book you read already held it, or `--resolve N=after-approval` if it came after; then `sudo systemctl start ti-flex@<instance>` and check it read. The decision is kept in `cash-movements.jsonl.resolutions.json` and never changed (a second, different `--resolve` for the same transfer is refused). `--resolve` also refuses a transfer already in the ledger ("already journalled as ...") and any transfer the last reading did not name as ambiguous, saying what the reader is waiting on; and if a reading is running it waits up to 10 seconds, then says "a reading is running, try again in a few minutes" (try again once it has finished). Avoid it: move no money on the day you approve a first run |
| `ti-flex@<instance>` failed; its status file says **the snapshot as of ... is in a year the NYSE calendar does not hold** (or the first run's approval is), or **the reading's day, D, is in a year the NYSE calendar does not hold** (D as `yyyymmdd`) | Refused rather than guess whether the snapshot is a session's close, or whether the statement is current. Nothing written. The snapshot case: from the first close snapshot past the calendar's last year (2030), every reading that has a new transfer is refused this way. The reading's-day case: the reader looks up the last session before its own day, so from 2 January 2031 every reading is refused this way, whether or not it has a new transfer. Until the calendar is extended; the market-data job stops at the same year | Tell the developer to extend `adapter/marketdata/nyse_calendar.py` (checked against NYSE's published holidays and early closes) and install the new build; the next reading then writes the transfers, none lost while they are within the query's window (or a custom-period reading reaches back) |
| `ti-flex@<instance>` failed; its status file says **the resolutions file ... is not as this reader writes it** | Refused: your recorded decisions on ambiguous transfers are not known | Put the file back from a backup or the server's snapshot; never edit it by hand. Without one, record each decision again with `--resolve` (above) |
| `ti-flex@<instance>` failed; its status file says the Flex statement **does not state its period** | Refused: without its first and last day the reader cannot check that no day was skipped | Keep the query's date format at the default (`yyyyMMdd`); tell the developer if it is |
| `ti-flex@<instance>` failed; its status file says **the coverage file ... is not as this reader writes it** | Refused: the days the last reading covered are not known, so no day can be checked | Put the file back from a backup or the server's snapshot; never edit it by hand. Without one, tell the developer |
| A ledger record has a `note` that a later (or earlier) transfer in the same interval was written first; the status file's `out_of_order` is above 0 | Information only: the transfer reached IBKR's report a day late. It is written, with the right figure; the record written before it keeps an `equity_before` that leaves this transfer out, and is not rewritten | Nothing; keep it in mind when reading that record's `equity_before` |
| The reader's log says "transfer N is dated by day alone on the first run's approval day" | Information only: the approval came before that day's close, so it is written as after the approval (above, "The first book"); counted in `date_only_on_approval_day` | Check the transfer was made after you read the book in IBKR; if it was before, tell the developer |
| `ti-flex@<instance>` failed; the cash status file says `"state": "read"` but the costs status file (`costs.jsonl.status.json`) says `"set-aside"` or `"coverage-gap"`: **cost N is stated as … which is not a cost this reader can write**, **the costs status cannot be written** (in the unit's log), **the costs ledger ... line N is not a cost as this reader writes it** (or is not UTF-8 text, or does not end with a whole line), or the first costs reading **starts after the first run's approval day** | The costs alone are set aside: the deposits and withdrawals are still written and the cash status is "read", so the cash checks are unaffected. No new cost is written; the daily summary's costs line says "costs incomplete" | A cost restated to an amount that is not a number: check it in IBKR's activity statement. (A restatement, including one to zero, sets nothing aside; it is recorded, said in the unit's log and counted in the daily costs text.) A costs status that cannot be written: check the folder `/var/lib/trend-investing/flex/<instance>/` is writable by the unit and that `costs.jsonl.status.json` is a file, then `sudo systemctl start ti-flex@<instance>`. A damaged line: put it back from a backup or the server's snapshot; never edit the ledger by hand. A first reading that does not reach back: set the Flex query's period to a custom range from the approval day, `sudo systemctl start ti-flex@<instance>`, then set it back, as for the cash coverage gap above |
| The status file says `"state": "running"` with a `checked_at` more than 90 minutes old | The reading was stopped part way (above, "The status file"); the ledger is as it was | Read `journalctl -u ti-flex@<instance>`; the next firing reads again |
| The status file's `unknown_types` is not `{}` | Information only: those rows are read but not classified, so not written | Tell the developer the type's name, so the reader can learn it |
| The reader's log says "transfer N not written: ... before the first run's approval" | Information only: that transfer is already in the starting book you stated; the status file counts it (`in_first_book`) | Nothing |
| A fill stamped in the same second as the morning read | It counts as after the read. If IBKR's figure includes it, the result is a difference and the account goes Degraded on that instrument: the safe side. | Check the fill at IBKR, then recover as for any difference. |
| A trade during a session that the engine did not place (one you made by hand in IBKR, or another program's), a fill report the engine was never given, or a journalled fill IBKR does not show. Alert: `<account>: reconciliation unexplained (close-snapshot:<snapshot ID>)` | The session's close snapshot holds shares that the book and the fills journalled since it do not explain. The engine records the check (`risk.reconciliation.result`, its input the snapshot) and the account goes **Degraded** on that stock: no buys, the stock frozen, the snapshot still taken as the book the next decision sizes from. A delisting, merger or symbol change journalled since the book cannot be applied, so the check is unverifiable and the account goes **Halted**, as for any unverifiable reconciliation. If the delivery's reconciliation from the same reading already reported the same holdings, and no fill or corporate action is dated between the reconciliation's reading and the snapshot's (whichever came first), nothing more is sent: you get that reconciliation's alert instead. The same difference, with the same shares, is not reported again after a fill of another stock; once a later reconciliation finds the stock matched, the same difference recurring is reported anew. A Halt is sent unless the account is already Halted for the same change. Nothing is recorded for a snapshot the fills explain. | Find the trade in IBKR's trade log for the session. Then follow `docs/runbooks/reconciliation.md`, "Recover", as for any unexplained difference: a fresh run whose approval clears the incident, or carries it in force. Halted: recover as `docs/runbooks/halted-account.md`, "Recover", says: after a session ends with none of the program's orders working, reconcile against IBKR, stop the engine, and start a fresh run whose approval clears the incident (`-clear-incident`). Stating the book (`-positions`) is only for a run's first reading that was not the book. |
| A difference that arises while the market is open and remains in the close snapshot | Found at that session's close, by the delivery's reconciliation or the close-snapshot check (above), not when it happens: the after-fill and pre-order reconciliations are not built. A difference reversed before the close, such as a holding-neutral trade, is not seen by either check. | Compare IBKR with the journal yourself when anything looks wrong during the day. |

## What is not built yet

- The reconciliations ADR 0019 also asks for after every fill, before every
  order and on a timer (the start, reconnect and daily-delivery ones are
  sent, and every close snapshot is checked); until they exist, a difference
  that arises during a session (a lost fill report among them) is found only
  at that session's close. Also
  the comparison of open orders; the joint account's cash; matching each cash
  activity to IBKR's activity rows rather than comparing one net total. These
  are tracked as follow-up issues.
- Built: the engine's reconciliation, the daily summary and the dashboard all
  read the ledger's deposits and withdrawals (see the sections above).
  Not built: a page from the reporter when the Flex reader itself is set aside
  (today you see a failed unit only, and after two sessions a pending cash read
  becomes Degraded). Built since: the reader records the commissions, fees and
  broker interest it parses in the costs ledger, and the reporter and the
  dashboard total them (alerts and dashboard chapter, "Costs"). That narrows
  what the cash-drift bound has to allow for, since a fee or an interest
  posting is now a recorded posting rather than an unknown one; the bound
  itself is unchanged. Still not recorded: the dividends and withholding tax
  the reader parses. An IRA's reader runs only once
  that IRA's engine runs (its `run.env`) and you fill in its account file.
