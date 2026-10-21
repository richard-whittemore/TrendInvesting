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
or reconnect then. The engine compares them with its own book and decides one of four
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
  delivered (normally the evening after the close), from the same reading as
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
  reading is taken after the close.

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
mismatch, urgent, on every channel you named (PagerDuty and ntfy). The
engine-state change the same reconciliation causes is part of that one
incident, not a second alert. A reconciliation that matches or is explained
sends nothing. It says what
differs, the state the account is now in, what it still does and what it has
stopped doing. The state is the account's whole state, not just this
reconciliation's: an account already Halted is told it is still Halted, and a
Degraded account's scope includes what was already frozen. It repeats (every 30 minutes by default) until you acknowledge it
with `operator acknowledge` ([operator](operator.md)). On the public ntfy
relay the alert names what is in doubt but carries no figure; the figures
go to PagerDuty, your own ntfy server and the dashboard, which shows the
full alert. `journal-query refusals` shows the engine-state change and its
detail, but no `journal-query` report lists the reconciliation result's
figures themselves ([journal-query](journal-query.md)). A reconciliation
that could not be verified says only that on the public relay, and sends you
to the private channel for the reasons, which can carry amounts.

## What the adapter sends

The adapter reads positions, the account summary (net liquidation, total cash,
Excess Liquidity) and every open order in one pass, then asks IBKR the time.
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
own date), else at the read time ([paper trading](paper-trading.md), "A
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

**In short: deposit any time; nothing to do.** The reader records it that
evening, and the strategy invests it at the next decision after the reader has
recorded it, usually the following evening (until then buys are sized
without it). A deposit of more than 1% of the
account's value (a typical IRA contribution is) also makes that evening's
reading stamped at its read time, so the engine skips that evening's decision
and you get that page too; that is expected, and it is still nothing to do. A
withdrawal is the same, except that buys wait until the reader has recorded it
(sells and exits go on); the next decision then sizes from what is left. (Keep
a withdrawal within the account's cash: IBKR refuses one larger, and the
program treats a recorded one as a difference.)

### A deposit or withdrawal you make

What happens to an IRA's reconciliation when you move money (built
2026-10-21, ADR 0019 amended; the joint account's book carries no cash, so
there nothing changes and the next decision simply sees the new net
liquidation):

1. **During the day**, the money lands at IBKR. The Flex reader has not run
   yet (it runs at 9:30 p.m.), so the evening's reconciliation finds IBKR's
   cash higher (or lower) than the book and nothing recorded to explain it.
   Because the reader has not yet read that day, this is a **pending cash
   read**, not a difference: one reconciliation alert, `<account>: reconciliation
   pending-cash-read (<id>)`, and the account's state does not change. What
   it does to buys depends on which way the cash moved:
   - **more cash than the book** (a deposit): buys carry on, but they are
     sized from the book's cash, IBKR's less the new cash, so money IBKR
     reverses or posted in error is never invested; the deposit is invested
     at the first decision after the reader's record explains it;
   - **less cash than the book** (a withdrawal): every buy on the account
     waits, refused with the reason `pending-cash-decrease`, so no order is
     sized on cash that may not be there; sells and exits go on, as while
     Degraded. Each refused buy is recorded, but only the first one pages you
     (`<account>: order refused (pending-cash-decrease)`), once for the whole
     pending difference. Buys go again as soon as the difference is settled.
   A reconnect that reads the account again before the book changes finds
   the same difference, even after a trade has filled: its result says so,
   it sends no second alert, and it is settled with the first.
   The alert's private text says which, and the account's state as it is.
2. **That evening**, the adapter compares the post-close reading with the
   previous close. The reader has not recorded the transfer yet, so a deposit
   or withdrawal of more than 1% of the account's net liquidation is more
   than the adapter's drift bound: the reading is stamped at its read time
   (`not_as_of_close`), the engine withholds that session as stale, and the
   reporter pages that as well. A smaller one is within the bound: for a
   deposit, that evening's decision goes ahead but sizes its buys without the
   new cash, still unexplained; for a withdrawal, its sells go and its buys
   wait, as step 1 says. At 9:30 p.m. the reader
   records the transfer in the ledger, if IBKR's statement already shows it
   (it may show a day's transfer only the next evening), and the engine
   journals it before IBKR's next reading reaches it.
3. **At the first reconciliation after the reader has covered the day**
   (usually the next evening, sometimes the one after), the engine checks the
   pending difference again against what the reader has recorded since.
   Explained to the cent, it is settled: a `risk.reconciliation.result`
   saying `explained`, with the transfer's ID, and no alert. That is the
   usual end. The strategy invests a deposit at the next decision after the
   reader has recorded it, usually the following evening.
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
its date, amount and currency. Which times are a session's close comes from
the NYSE calendar the adapter and the data job share
(`adapter/marketdata/nyse_calendar.py`), which holds 2022 to 2030; past its
last year the reader refuses rather than guess (below, "What can go wrong").

**What is written.** Each deposit or withdrawal not already recorded becomes
one cash movement (`account.cash-movement`, version 2) in the account's
**ledger**, `/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl`
(`boost`, `trad-ira` or `roth-ira`): the amount (positive in, negative out),
the date, the currency, IBKR's transaction ID, and the account's equity
before it. The other postings are read but not written yet; they are for the
running totals of costs. A posting whose type the reader does not know is
counted in the status file (`unknown_types`) and named in its log, never
dropped without a word.

**The equity before a transfer** comes from the engine's close snapshots
(the reading after each close), found back through the chain of runs:

- normally, the last close snapshot before the transfer, plus any transfers
  between that snapshot and this one;
- for a transfer IBKR times **after the close** (4 p.m. New York, 1 p.m. on
  an early-close day such as the day after Thanksgiving: the session's
  official close, from the same NYSE calendar the adapter stamps its close
  snapshot by), that day's close snapshot, less the transfer (and any later
  that evening): the snapshot is read after the data step (5:05 p.m. or
  later), so it already holds the transfer. The record says so:
  `"equity_before_basis": "same-day-snapshot-less-amount"`;
- for a transfer **after you approved the first run but before its first
  close snapshot**, the first snapshot after it, less the transfer (and any
  others up to that snapshot). The market's move between the transfer and
  that close is in the figure, so it is an approximation, and the record
  says so: `"equity_before_basis": "next-snapshot-less-amount"`. Until that
  first snapshot exists the reading is set aside (below), and the first
  reading after it writes the transfer.

Both approximations take the close snapshot as read after the transfer; a
transfer IBKR times later that evening than the reading is counted as held
by it. What the records are for, the running record of money in and out,
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
  the evening's reading, so the next statement must start on or before it
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

**When.** `ti-flex@<instance>.timer` (one per account: `ti-flex@boost`,
`ti-flex@trad-ira`, `ti-flex@roth-ira`) runs it on weekdays at 9:30 p.m. New
York time, after the close and the evening's data step. IBKR may add a day's
transfer to the report only after its own end-of-day processing, so a
transfer can appear one evening late; the query covers the last 30 days
(below), so the next evening's reading writes it.

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
3. Put both in the server's secrets file, never anywhere else: `sudo -e
   /etc/trend-investing/secrets.env`, the lines `TI_IBKR_FLEX_TOKEN=` and
   `TI_IBKR_FLEX_QUERY_ID=` (no quotes, no spaces). The token is a password
   to your account's reports: never paste it into a chat, an issue or a
   command line.
4. For an IRA account, its number in its own file, the same shape as
   `accounts/boost.env`: fill in `IBKR_ACCOUNT_TRAD_IRA=` in
   `/etc/trend-investing/accounts/trad-ira.env`, or `IBKR_ACCOUNT_ROTH_IRA=`
   in `roth-ira.env` (`sudo -e`). The provisioner creates both, root-owned,
   mode 0600, with the line named only, so an IRA reader started before you
   fill it in fails saying which line is empty (`systemctl status
   ti-flex@trad-ira` shows the reader's message). Do not delete the file: a
   missing one fails the unit before the reader runs, with only systemd's
   "Failed to set up credentials".
5. Turn the reader on (`docs/runbooks/lightsail-deployment.md`, "The Flex
   reader").

**When it cannot read.** The reader *sets itself aside* for that evening:
it writes nothing to the ledger, records why in the status file beside the
ledger (`cash-movements.jsonl.status.json`, `"state": "set-aside"`, with
IBKR's error code when IBKR gave one), and its unit shows as failed
(`systemctl --failed`, `journalctl -u ti-flex@<instance>`). Trading carries on.
While it is set aside, a cash difference stays a pending cash read for at most
two sessions and then degrades the account on cash (above, "A deposit or
withdrawal you make"). The next evening it tries again, and a good reading
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
stopped part way (its unit's 45-minute limit, a reboot, a kill), the file
keeps saying `"running"` with that start time, never the previous evening's
`"read"`. A `"running"` whose `checked_at` is more than 45 minutes old means
the reading was stopped: `journalctl -u ti-flex@<instance>` says why, and
the next firing reads again. A reading that stops before it writes `"running"` leaves the file as the last
reading left it, possibly an older `"read"`, so for a failed unit whose status file says `"read"` or has no
`checked_at` of tonight, read the journal, not the file. Every such stop is one of these, and
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
  unit's 45-minute limit ends the wait first, the file keeps whatever it said.

A refusal that prints `refused:` is also the quick test: the status file was not touched. Every other set-aside,
including each row below that names a status file, is written to it.
Nothing is written to the ledger until a
reading has checked everything, so a stopped reading leaves it as it was.

## What can go wrong, and what the program does then

| What you see | What the program does | What you do |
| --- | --- | --- |
| Alert: `<account>: reconciliation <classification> (<id>)`, urgent, repeating until acknowledged; `<classification>` is `unexplained`, `unverifiable` or `pending-cash-read`, `<id>` the reconciliation's ID | Sent once per reconciliation that degrades or halts, and once per pending cash read: a later reading of the same pending difference, and its decision, send nothing more. Its text names what differs (or "evidence that could not be compared"), the account's whole state after it, what it still does and what it has stopped; on the public relay, no figures. It ends "Recovery is a person's: docs/runbooks/reconciliation.md." | Read the full text on PagerDuty or the dashboard. Follow `docs/runbooks/reconciliation.md`, "First, in every case". Acknowledge with `operator acknowledge` and the alert key the text ends with. |
| `reconciliation-unexplained`. Alert: `<account>: reconciliation unexplained (<id>)` | A difference no journalled event accounts for. The account goes **Degraded** on exactly what differs: a holding's instrument is frozen, unexplained cash refuses every buy. A later difference widens the scope; a later match never brings it back to Normal. | `docs/runbooks/reconciliation.md`, "Find the cause", then "Recover": explain each difference at IBKR, then start a fresh run whose approval clears the incident (`operator approve -clear-incident <id>`), or carries it in force if something is still unexplained ([restart](restart.md)). |
| `reconciliation-unverifiable`. Alert: `<account>: reconciliation unverifiable (<id>)` | The figures could not be compared, so the account goes **Halted**: no order is released, except, for the joint account, the one sale back to 98%. The private text lists why, for example `the broker's positions could not be read`, `the broker's total cash could not be read`, `no account snapshot has been recorded, so the journal has no book to compare`, `the observation as of ... precedes the book as of ...`, or `the broker's cash is in ...; the book's is in ...`. | Fix what could not be read (the adapter's link, `journalctl -u ti-adapter-boost`), then follow the runbook's "Recover". Leaving Halted is never automatic: it takes a fresh run you approve. |
| `pending-cash-read`. Alert: `<account>: reconciliation pending-cash-read (<id>)` | Only the cash differs, on a day the Flex reader has not yet read, so a deposit or withdrawal it has not recorded yet may explain it. **No state changes.** More cash than the book: buys are not stopped, but are sized without the new cash until it is explained. Less cash: every buy is refused (`pending-cash-decrease`) until it is settled, the first refusal paging once; sells and exits go on. The private text says how far the reader has read. At each later reconciliation the difference is checked against the transfers recorded since: explained to the cent, it is settled (a result `explained` for it, no alert); once the reader has read its day with the difference still there, or after more than two sessions, the account goes **Degraded** on cash (the `unexplained` row below, same `<id>`). The alert text's account-state sentence may say "Degraded on instruments [] and cash []": the account is not; trust the engine state in `journal-query refusals` | If you moved money that day, nothing: it clears at the first reconciliation after the reader has covered the day, usually the next evening, sometimes the one after. Acknowledge the alert. If you did not, check IBKR's activity for what moved the cash. Do not approve a fresh run while a cash read is pending: `operator approve` refuses one unless you state the book you checked in IBKR after the transfer |
| A pending cash read becomes Degraded: the private text says the Flex reader `has covered <day> and its records do not explain it`, or that `<n> sessions have closed since it, more than the two sessions` | The difference was not a transfer the reader recorded, or the reader was set aside too long: the account is Degraded on cash, buys stop | Check `ti-flex@<instance>`'s status file; if the reader was set aside, fix it (rows below) and check the ledger now holds the transfer. Then recover as for any unexplained difference (`docs/runbooks/reconciliation.md`) |
| The private text names `withdrawal ibkr-flex:<id> of <amount> is more than the cash on hand before it, ..., which IBKR refuses` | A recorded withdrawal larger than the account's cash: IBKR would have refused it, so the record and the account disagree. Degraded on cash at once, never pending | Check the withdrawal in IBKR's activity statement and tell the developer; recover as for any unexplained difference |
| The engine's log says `the Flex ledger ... is refused, so none of its transfers is delivered` (naming the line) | A ledger line is not as the reader writes it, so no record of that ledger reaches the engine; a cash difference a record would have explained is a difference. Said once until it changes | Put the line back as the reader wrote it (as for the reader's own refusal above); never edit the ledger by hand |
| The engine will not start: `the Flex ledger ... is instance <a>'s, but this engine's state folder is <b>'s` | `-cash-ledger` names another account's ledger. The records carry no account, so each account's engine reads only its own folder | Correct the unit (`ti-engine@.service` passes `${TI_FLEX_DIR}`, `/var/lib/trend-investing/flex/%i`) |
| The engine will not start: `the Flex ledger's folder: ... no such file or directory` | The account's Flex folder is missing (the provisioner creates it) | Run the provisioner's services step again (`docs/manual/server.md`) |
| A dividend, interest charge, fee or fill the journal has not recorded | Cash (or that instrument) differs, so the account goes Degraded: on cash, buys stop. **No command records the missing event**: `operator` has four commands (the kill switch, an approval, a rebalance clearance and an acknowledgement), and none records a market or account event. | Check the item on IBKR's activity, write down what you found, and either approve a fresh run that clears the incident (`operator approve -clear-incident <id>`) or one that carries it in force (leave `-clear-incident` out; for cash its scope is `"scope_cash_components": ["total-cash"]`, so buys stay refused) (`docs/runbooks/reconciliation.md`, "Recover"). |
| Private text names `corporate action ... (... of ...) is not one a target-weight book can apply` | A delisting, merger or symbol change: the book can apply only fills, dividends and splits, so the reconciliation is unverifiable and the account goes Halted. | Check at IBKR what the account now holds for that stock. There is no way to record the action for the program: clear the incident in a fresh run once the holding is understood. For an IRA, the next rebalance sells what the rule no longer targets. |
| An account has no account snapshot yet (a start or reconnect before the run's first delivery) | The adapter sends no reconciliation: with no book to compare, it would be unverifiable and halt the account. The run's first delivery carries the first, right behind its account snapshot. | Nothing. |
| A run's first reading is not the book it was compared with (the previous run's last close, or your starting book) | The adapter sends its reconciliation alone and withholds its snapshots, so the engine, holding no book, finds it unverifiable and **Halted**; nothing of that reading is taken as the book, by this run or the next. | Clear it with a fresh run whose approval states the book you check in IBKR (`operator approve -log <the halted run's log> -account <account> -by owner -clear-incident <id> -positions SYM=<shares> -cash <total cash> -currency USD -out <approval file>`, typed right after reading the book in IBKR; [restart](restart.md)). Without a stated book, the fresh run is compared with the same book again and halts again. |
| The daily summary shows `cash drift` and `unexplained cash` | Information only: the drift since the previous close and that drift added up since the cash was last explained (an explained reconciliation, else the starting book). The program does not act on it, and the post-close reading is stamped at its read time instead of the close when the drift exceeds 1% of net liquidation (the session is then withheld). | A small posting day (interest, a dividend) is expected; a figure that keeps growing is a cash movement to find at IBKR before approving anything. |
| Margin interest on the joint account | Not compared: Boost's book states no cash, so the result says cash is not compared. Nothing is seen. | Check the interest on IBKR's monthly statement yourself. |
| A $12.30 fee and a $12.30 credit, both unrecorded | Not detected: cash is compared as one net total, and the two cancel. | The statement is the only check. |
| `ti-flex@<instance>` failed; its status file says IBKR's Flex Web Service **rejected the token or the query** (error 1012 the token expired, 1015 the token is invalid, 1014 the query is invalid, 1011, 1013 or 1016 the service, its IP address or the account is not allowed; these codes are from IBKR's documentation, not yet verified against a live statement) | The Flex reader is set aside for this reading: no request for the statement is made, nothing is written to the ledger, and the token is never printed. Trading is unaffected. It tries again at the next firing | In Client Portal, check the Flex Web Service is on and the token not expired (generate a new one if it is), and the query ID; correct `TI_IBKR_FLEX_TOKEN` or `TI_IBKR_FLEX_QUERY_ID` with `sudo -e /etc/trend-investing/secrets.env`, then `sudo systemctl start ti-flex@<instance>` and check `journalctl -u ti-flex@<instance>` says it read |
| `ti-flex@<instance>` failed; its status file says the Flex Web Service **gave no statement** (busy, error 1009 or 1018; not ready, 1003 to 1008; still generating after 30 tries 10 seconds apart, 1019; the codes from IBKR's documentation, not yet verified against a live statement; or IBKR could not be reached, or answered with a redirect, which the reader never follows since the token would go with it) | Set aside for this reading, nothing written; the next firing asks again | Nothing, if the next evening reads. If it keeps failing, check the server's network, and IBKR's system status |
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
| `ti-flex@<instance>` failed; its status file says `"state": "coverage-gap"`: **the Flex statement ends on D1, but a current one ends on D2 or the session before** (the period was left as a custom range, or is not Last 30 Calendar Days) | Nothing written, the coverage file not moved on: transfers after D1 are in no statement, and every later reading would return the same window. D2 is the last NYSE session before the reading's New York day; a statement ending on D2, the session before it, or yesterday's date is read, so a weekend or a market holiday never trips it | In Client Portal, check the Flex query's period and set it back to *Last 30 Calendar Days*, run `sudo systemctl start ti-flex@<instance>` and check it read. If the period already is Last 30 Calendar Days (IBKR ended it earlier than expected after several closed days), the next evening's reading clears it; tell the developer if it repeats |
| `ti-flex@<instance>` failed; its status file says `"state": "ambiguous-transfer"`: **the first run was approved at T, after that session's close, and IBKR dates transfer N of AMOUNT by that day alone** | IBKR gives no time for the transfer, and you approved after the day's close, so the reader cannot tell whether the book you stated holds it. Nothing written, the account number never shown; every reading stops the same way until you decide. Trading is unaffected | Check in IBKR's activity when the transfer reached the account against when you read the book. Then record it, once: `sudo -u ti-trader env -C /opt/trend-investing/adapter /opt/trend-investing/adapter-venv/bin/python3 -m adapter.ibkr flex --ledger /var/lib/trend-investing/flex/<instance>/cash-movements.jsonl --resolve N=in-first-book` if the book you read already held it, or `--resolve N=after-approval` if it came after; then `sudo systemctl start ti-flex@<instance>` and check it read. The decision is kept in `cash-movements.jsonl.resolutions.json` and never changed (a second, different `--resolve` for the same transfer is refused). `--resolve` also refuses a transfer already in the ledger ("already journalled as ...") and any transfer the last reading did not name as ambiguous, saying what the reader is waiting on; and if a reading is running it waits up to 10 seconds, then says "a reading is running, try again in a few minutes" (try again once it has finished). Avoid it: move no money on the day you approve a first run |
| `ti-flex@<instance>` failed; its status file says **the snapshot as of ... is in a year the NYSE calendar does not hold** (or the first run's approval is), or **the reading's day, D, is in a year the NYSE calendar does not hold** (D as `yyyymmdd`) | Refused rather than guess whether the snapshot is a session's close, or whether the statement is current. Nothing written. The snapshot case: from the first close snapshot past the calendar's last year (2030), every reading that has a new transfer is refused this way. The reading's-day case: the reader looks up the last session before its own day, so from 2 January 2031 every reading is refused this way, whether or not it has a new transfer. Until the calendar is extended; the market-data job stops at the same year | Tell the developer to extend `adapter/marketdata/nyse_calendar.py` (checked against NYSE's published holidays and early closes) and install the new build; the next reading then writes the transfers, none lost while they are within the query's window (or a custom-period reading reaches back) |
| `ti-flex@<instance>` failed; its status file says **the resolutions file ... is not as this reader writes it** | Refused: your recorded decisions on ambiguous transfers are not known | Put the file back from a backup or the server's snapshot; never edit it by hand. Without one, record each decision again with `--resolve` (above) |
| `ti-flex@<instance>` failed; its status file says the Flex statement **does not state its period** | Refused: without its first and last day the reader cannot check that no day was skipped | Keep the query's date format at the default (`yyyyMMdd`); tell the developer if it is |
| `ti-flex@<instance>` failed; its status file says **the coverage file ... is not as this reader writes it** | Refused: the days the last reading covered are not known, so no day can be checked | Put the file back from a backup or the server's snapshot; never edit it by hand. Without one, tell the developer |
| A ledger record has a `note` that a later (or earlier) transfer in the same interval was written first; the status file's `out_of_order` is above 0 | Information only: the transfer reached IBKR's report a day late. It is written, with the right figure; the record written before it keeps an `equity_before` that leaves this transfer out, and is not rewritten | Nothing; keep it in mind when reading that record's `equity_before` |
| The reader's log says "transfer N is dated by day alone on the first run's approval day" | Information only: the approval came before that day's close, so it is written as after the approval (above, "The first book"); counted in `date_only_on_approval_day` | Check the transfer was made after you read the book in IBKR; if it was before, tell the developer |
| The status file says `"state": "running"` with a `checked_at` more than 45 minutes old | The reading was stopped part way (above, "The status file"); the ledger is as it was | Read `journalctl -u ti-flex@<instance>`; the next firing reads again |
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
  activity to IBKR's activity rows rather than comparing one net total; and
  the outside heartbeat monitor. These are tracked as follow-up issues.
- The engine and the adapter read the Flex reader's ledger, but the reporter
  does not page when the reader is set aside (you see a failed unit only, and
  after two sessions a pending cash read becomes Degraded), its pending cash
  read alert's account-state sentence does not yet say "unchanged", the
  dashboard's comparison does not yet use the deposits and withdrawals, and
  the dividends, interest and fees it reads are not yet recorded. An IRA's
  reader runs only once that IRA's engine runs (its `run.env`) and you fill
  in its account file.
