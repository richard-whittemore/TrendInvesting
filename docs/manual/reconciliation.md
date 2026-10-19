# The morning reconciliation

## What it does and why it exists

The engine keeps its own record of what each account holds. IBKR keeps the
real one. They can drift apart: a fill report goes missing, a dividend posts,
someone trades by hand, or IBKR liquidates a position. The morning check
built so far catches only some of these (see "What the morning check covers,
and what it does not"). Every decision the
engine makes rests on what it believes you hold, so a difference it cannot
explain is a reason to stop taking on new risk (ADR 0019).

As designed, every morning before the day's first decision the IBKR adapter
reads the account's positions and cash from IBKR and records them in the
journal. The engine then compares them with its own book and decides one of
four things. **This does not run yet**: the engine's side and the alert are
built and tested, and the adapter can build the record, but its main loop
does not send it (see "What is not built yet"). Until it does, no
reconciliation happens and no reconciliation alert is ever sent.

"Reconciled" also means two other things in this program: the adapter's
own check of IBKR's open orders when it connects, and the
`journal-query reconcile` report. The [glossary](glossary.md)
("Reconciled, reconciliation") tells the three apart. This chapter is about
the engine's morning reconciliation.

| Result | What it means | What happens |
| --- | --- | --- |
| **matched** | IBKR's figures equal the book, and nothing has changed since it | nothing; no alert |
| **explained** | they differ, but journalled events account for every difference exactly | nothing; no alert |
| **unexplained** | a difference no journalled event accounts for | the account goes **Degraded** on exactly what differs, and you get one urgent alert |
| **unverifiable** | IBKR's figures could not be read, or cannot be compared | the account goes **Halted**, and you get one urgent alert |

A matching reconciliation changes nothing and sends nothing.

## What the morning check covers, and what it does not

The book is replaced by every close's account snapshot, and that snapshot
already contains the session's fills. So the morning check compares **the
previous close's snapshot, plus the events the journal recorded since that
close**, with IBKR's view the next morning. It covers only what changed
between that close and the morning read:

- **It catches** a manual trade placed overnight or before the open, a
  dividend, split or fee posted overnight, IBKR liquidating a position
  overnight, and a fill the journal recorded after the close that IBKR does
  not show (or the reverse).
- **It does not catch** anything that happens during a session. Orders sent
  at a close execute in the next session, before that session's own close
  snapshot. A fill whose report the engine never received is therefore taken
  into the next close's snapshot, and no check compares the morning book plus
  the session's fills with that snapshot. A missing fill report, or a
  difference that arises while the market is open, is caught only by the
  after-fill and pre-order reconciliations ADR 0019 asks for, which are not
  built yet (see the last section).
- **The book is not itself verified.** ADR 0019 asks that the starting point
  be a previously verified reconciliation or an opening baseline you approved.
  As built, the starting point is simply the last snapshot read, so a
  difference already inside that snapshot is not seen.

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
  difference, and the events that explained it;
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
From that one read it can build two records (`adapter/ibkr/snapshot.py`):

- the **broker snapshot** (`account.broker.snapshot`), which the risk gate
  sizes sells and the sale back to 98% from, and the margin alarm watches;
- the **reconciliation** (`account.reconciliation`), positions and total cash
  for the morning comparison.

Today its main loop sends only the first: a broker snapshot ahead of each
day's market data. The reconciliation record is built and tested but never
sent, so the engine never receives one.

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
| Margin interest on the joint account | Never compared: Boost's book carries no cash, so its cash is not reconciled and the result says so. Nothing is seen. | Check the interest on IBKR's monthly statement yourself. |
| A $12.30 fee and a $12.30 credit, both unrecorded | Not detected: cash is compared as one net total, and the two cancel. | The statement is the only check. |
| A fill stamped in the same second as the morning read | It counts as after the read. If IBKR's figure includes it, the result is a difference and the account goes Degraded on that instrument: the safe side. | Check the fill at IBKR, then recover as for any difference. |
| A fill report lost during a session, or a difference that arises while the market is open | Not caught by the morning check: the next close's snapshot absorbs it ("What the morning check covers"). | Compare IBKR with the journal yourself when anything looks wrong. |
| Today: no reconciliation alert at all | Nothing runs: the adapter does not send the morning read. A difference between IBKR and the engine's book is not detected by the program. | Before every fresh run, compare IBKR's positions, cash and open orders with the journal yourself (`docs/runbooks/reconciliation.md`, "If the system itself is silent", step 3). |

## What is not built yet

- Sending the morning read: the adapter builds the `account.reconciliation`
  record but its main loop never sends it, so the morning reconciliation
  does not run. The engine side and the alert are built and tested against
  recorded inputs.
- The account snapshot the book starts from: for Boost, nothing produces
  the account snapshot as of the close yet ([engine](engine.md)), so even
  a delivered reconciliation would find no book and be unverifiable.
- Delivering dividends and splits to the engine: the data job uses them to
  adjust prices, but no program sends them to the engine as events, so a
  posted dividend could not explain a cash difference yet.
- The reconciliations ADR 0019 also asks for at startup, after every fill,
  before every order and on a timer; until they exist, a difference that
  arises during a session (a lost fill report among them) is not caught.
  Also the comparison of open orders; the joint account's cash; matching
  each cash activity to IBKR's activity rows rather than comparing one net
  total; any command that records a missed event; and the outside heartbeat
  monitor.
- `-operator` in the engine's service file on the server: without it, an
  `operator acknowledge` does not reach the engine, so a repeating alert
  cannot be acknowledged there ([engine](engine.md), "Your instructions
  while it runs").
