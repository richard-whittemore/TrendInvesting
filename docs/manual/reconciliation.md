# The morning reconciliation

## What it does and why it exists

The engine keeps its own record of what each account holds. IBKR keeps the
real one. They can drift apart: a fill report goes missing, a dividend posts,
someone trades by hand, or IBKR liquidates a position. Every decision the
engine makes rests on what it believes you hold, so a difference it cannot
explain is a reason to stop taking on new risk (ADR 0019).

Every morning, before the day's first decision, the IBKR adapter reads the
account's positions and cash from IBKR and records them in the journal. The
engine then compares them with its own book and decides one of four things:

| Result | What it means | What happens |
| --- | --- | --- |
| **matched** | IBKR's figures equal the book, and nothing has changed since it | nothing; no alert |
| **explained** | they differ, but journalled events account for every difference exactly | nothing; no alert |
| **unexplained** | a difference no journalled event accounts for | the account goes **Degraded** on exactly what differs, and you get one urgent alert |
| **unverifiable** | IBKR's figures could not be read, or cannot be compared | the account goes **Halted**, and you get one urgent alert |

A matching reconciliation changes nothing and sends nothing.

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
- **Cash.** Cash is compared in whole cents, each fill's value and commission
  rounded to the cent. The joint account's book (Boost's account snapshot)
  carries no cash yet, so its cash is not compared; the result says so. Its
  positions are.

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
through the recovery procedure in `docs/runbooks/reconciliation.md`.

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
mismatch, on every channel you named (PagerDuty and ntfy). It says what
differs, the state the account is now in, what it still does and what it has
stopped doing. It repeats until you acknowledge it. On the public ntfy relay
the alert names what is in doubt but carries no figure; the figures go to
PagerDuty, your own ntfy server, the dashboard and `journal-query`.

## What the adapter sends

The adapter reads positions, the account summary (net liquidation, total cash,
Excess Liquidity) and every open order in one pass, then asks IBKR the time.
From that one read it builds two records:

- the **broker snapshot** (`account.broker.snapshot`), which the risk gate
  sizes sells and the sale back to 98% from, and the margin alarm watches;
- the **reconciliation** (`account.reconciliation`), positions and total cash
  for the morning comparison.

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

## What can go wrong

- **No account snapshot yet.** With nothing to compare against, the
  reconciliation is unverifiable and the account goes Halted.
- **IBKR cannot be read.** Unverifiable, Halted. The alert names what could not
  be read.
- **A dividend, interest charge or fee the journal has not recorded.** The cash
  differs, so the account goes Degraded on cash: buys stop until you record the
  missing event and approve a restart. Margin interest on the joint account is
  not compared yet (its book carries no cash).
- **A fill in flight at the moment of the read.** The account can go Degraded
  on that instrument. The morning read is before the open precisely so that
  nothing is filling.

## What is not built yet

- The adapter's records are not yet sent over the transport, so the morning
  reconciliation does not run on its own yet. The engine side and the alert
  are built and tested against recorded inputs.
- The reconciliations ADR 0019 also asks for at startup, after every fill,
  before every order and on a timer; the comparison of open orders; the joint
  account's cash; and the outside heartbeat monitor. These are tracked as
  follow-up issues.
