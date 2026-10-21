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
  for a fraction of a share.

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
  journalled fill, commission, dividend and cash in lieu, against IBKR's one
  number. When they differ, the result lists those parts beside the
  difference. But two activities the journal has not recorded that cancel
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

## What can go wrong, and what the program does then

| What you see | What the program does | What you do |
| --- | --- | --- |
| Alert: `<account>: reconciliation <classification> (<id>)`, urgent, repeating until acknowledged; `<classification>` is `unexplained` or `unverifiable`, `<id>` the reconciliation's ID | Sent once per reconciliation that degrades or halts. Its text names what differs (or "evidence that could not be compared"), the account's whole state after it, what it still does and what it has stopped; on the public relay, no figures. It ends "Recovery is a person's: docs/runbooks/reconciliation.md." | Read the full text on PagerDuty or the dashboard. Follow `docs/runbooks/reconciliation.md`, "First, in every case". Acknowledge with `operator acknowledge` and the alert key the text ends with. |
| `reconciliation-unexplained`. Alert: `<account>: reconciliation unexplained (<id>)` | A difference no journalled event accounts for. The account goes **Degraded** on exactly what differs: a holding's instrument is frozen, unexplained cash refuses every buy. A later difference widens the scope; a later match never brings it back to Normal. | `docs/runbooks/reconciliation.md`, "Find the cause", then "Recover": explain each difference at IBKR, then start a fresh run whose approval clears the incident (`operator approve -clear-incident <id>`), or carries it in force if something is still unexplained ([restart](restart.md)). |
| `reconciliation-unverifiable`. Alert: `<account>: reconciliation unverifiable (<id>)` | The figures could not be compared, so the account goes **Halted**: no order is released, except, for the joint account, the one sale back to 98%. The private text lists why, for example `the broker's positions could not be read`, `the broker's total cash could not be read`, `no account snapshot has been recorded, so the journal has no book to compare`, `the observation as of ... precedes the book as of ...`, or `the broker's cash is in ...; the book's is in ...`. | Fix what could not be read (the adapter's link, `journalctl -u ti-adapter-boost`), then follow the runbook's "Recover". Leaving Halted is never automatic: it takes a fresh run you approve. |
| A dividend, interest charge, fee or fill the journal has not recorded | Cash (or that instrument) differs, so the account goes Degraded: on cash, buys stop. **No command records the missing event**: `operator` has four commands (the kill switch, an approval, a rebalance clearance and an acknowledgement), and none records a market or account event. | Check the item on IBKR's activity, write down what you found, and either approve a fresh run that clears the incident (`operator approve -clear-incident <id>`) or one that carries it in force (leave `-clear-incident` out; for cash its scope is `"scope_cash_components": ["total-cash"]`, so buys stay refused) (`docs/runbooks/reconciliation.md`, "Recover"). |
| Private text names `corporate action ... (... of ...) is not one a target-weight book can apply` | A delisting, merger or symbol change: the book can apply only fills, dividends and splits, so the reconciliation is unverifiable and the account goes Halted. | Check at IBKR what the account now holds for that stock. There is no way to record the action for the program: clear the incident in a fresh run once the holding is understood. For an IRA, the next rebalance sells what the rule no longer targets. |
| An account has no account snapshot yet (a start or reconnect before the run's first delivery) | The adapter sends no reconciliation: with no book to compare, it would be unverifiable and halt the account. The run's first delivery carries the first, right behind its account snapshot. | Nothing. |
| A run's first reading is not the book it was compared with (the previous run's last close, or your starting book) | The adapter sends its reconciliation alone and withholds its snapshots, so the engine, holding no book, finds it unverifiable and **Halted**; nothing of that reading is taken as the book, by this run or the next. | Clear it with a fresh run whose approval states the book you check in IBKR (`operator approve -log <the halted run's log> -account <account> -by owner -clear-incident <id> -positions SYM=<shares> -cash <total cash> -currency USD -out <approval file>`, typed right after reading the book in IBKR; [restart](restart.md)). Without a stated book, the fresh run is compared with the same book again and halts again. |
| The daily summary shows `cash drift` and `unexplained cash` | Information only: the drift since the previous close and that drift added up since the cash was last explained (an explained reconciliation, else the starting book). The program does not act on it, and the post-close reading is stamped at its read time instead of the close when the drift exceeds 1% of net liquidation (the session is then withheld). | A small posting day (interest, a dividend) is expected; a figure that keeps growing is a cash movement to find at IBKR before approving anything. |
| Margin interest on the joint account | Not compared: Boost's book states no cash, so the result says cash is not compared. Nothing is seen. | Check the interest on IBKR's monthly statement yourself. |
| A $12.30 fee and a $12.30 credit, both unrecorded | Not detected: cash is compared as one net total, and the two cancel. | The statement is the only check. |
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
