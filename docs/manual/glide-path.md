# The glide path: how the IRAs move into bonds as you age

## What it does and why it exists

The momentum strategy that runs the IRAs is all stocks when you are young.
As you get closer to retirement you want less of the account riding on it.
The glide path does that. Each month it works out what share of the account
belongs in bonds, from your age, and moves that share out of the momentum
book and into a fixed pair of bond funds.

That share is called **b**, the bond share. It is a number between 0 and 1:
0 means no bond sleeve at all, 0.5 means half the account is meant for bonds.

The schedule is the plan's (docs/live-trading/plan.md section 2) and matches
the research that tested it (research/edge-search/momentum/glide_path.py).

## What b is at each age

The schedule is a set of corners, called anchors. Between two corners b rises
in a straight line, a little each month. Before the first corner b is 0.
After the last corner it stays flat.

| Age | b |
| --- | --- |
| 38 or younger | 0 |
| 53 | 0.308 |
| 65 (the retirement age the schedule assumes) | 0.5 |
| 72 and older | 0.7 |

For example, between 53 and 65 b rises by 0.192 over 144 months, which is
about 0.0013 a month. At 53 years and one month b is 0.308 plus one month's
step.

The anchors, the retirement age and the bond funds are all in the
configuration's `glide` section, so changing any of them changes the
configuration's hash and is recorded with every run.

## How your age is counted

You give a birth **year and month** only, as `YYYY-MM` (for example
`1972-02`). There is no day.

- Your age is counted in **whole months**, from your birth month to the month
  of the rebalance. With no day, it goes up by one month on the first of
  every month.
- The date used is the **rebalance session**: the first trading day of the
  new month, the day the orders are meant to go in. The decision is made at
  the close of the month's last trading day, but it uses your age on the
  rebalance day, the same as the research.
- Every momentum decision records three glide figures: the rebalance session,
  your age in months, and b. They are recorded whether the decision traded or
  was withheld, so you can always see what the run believed.

Your birth value is personal. It has no default anywhere, it is never stored
in the repository (every value there is made up), and error messages never
print it. It comes only from the server's configuration. The configuration
record at the top of each journal does contain it, so journals of real IRA
runs stay on the server and are never committed.

## The two sleeves

Each month the account is split into two parts, called sleeves.

- **The momentum sleeve** is the momentum strategy's own book, including the
  IEF it holds when the market trend is down (risk-off). The whole book is
  scaled down to `1 - b` of the account.
- **The bond sleeve** holds `0.98 x b` of the account, split half IEF and half
  SHY. The 0.98 is the same 2% cash buffer the momentum book keeps, so the
  account is never asked to hold more than 98% in positions.

With b = 0 (38 or younger) there is no bond sleeve and the momentum book is
used exactly as the rule decided it.

A worked example at b = 0.308, in a risk-on month where the momentum book
holds five names at 0.196 each:

- each momentum name: `0.196 x (1 - 0.308) = 0.1356`
- IEF: `0.98 x 0.308 x 0.5 = 0.1509`
- SHY: the same, `0.1509`

The targets add up to 0.98 of the account.

## IEF in both sleeves is one target, summed

In a risk-off month the momentum book itself holds IEF. The bond sleeve also
holds IEF. The account does not get two IEF orders. The two amounts are
**added into one IEF target**, and the decision records how much of it came
from each sleeve (its `shares`: momentum first, then bond).

Suppose the risk-off momentum book holds IEF at 0.49 and b = 0.308. The
momentum sleeve's IEF is `0.49 x 0.692 = 0.3391`, the bond sleeve's is
`0.1509`, and the one IEF target is `0.49`.

The order planner sees only the summed targets. Its rebalance band (a held
fund is traded only when its weight is more than 25% of its target away from
it) applies to the sum. So a small monthly step in b does not trade SHY every
month, and IEF held by both sleeves in two risk-off months in a row is not
resized just because the split between the sleeves moved.

## What can go wrong

Every one of these stops a trade rather than guessing.

- **A malformed birth value** (a day included, a month of 13, the wrong
  shape) is refused when the configuration is checked. The run does not
  start.
- **An implausible age.** A birth value that makes you younger than 18 or
  older than 110 is refused as a likely typo. The bounds are checked at the
  configured first decision time, when the run starts, and again at every
  decision's rebalance session, where a run that reaches an age above 110
  stops with an error instead of deciding.
- **A missing final bar for a bond fund.** SHY and IEF need their final bar
  of the month like every other fund that is held or targeted. Whenever b is
  above 0 the bond sleeve targets both, so if either has no final bar the
  decision is withheld with the reason `missing-bars` and nothing trades that
  month.
- **A month-end session that never closed.** The next close records that
  month's decision as withheld (`missing-bars`), still with its glide figures
  for the rebalance day it would have used. If that age is implausible, the
  run stops there too.
- **A momentum book set above 98% invested.** The bond sleeve is fixed at
  98% of b, so a momentum book invested above 98% could not be combined with
  it. Such a configuration is refused when the run starts. A book invested
  below 98% is accepted, and its bond sleeve stays at 98% of b.
- **A restart does not remember the book.** A restarted run does not yet
  read back the last decision's book, b or sleeve shares from its journal; it
  starts from the next decision.

## Where to look

- The schedule and the sleeve arithmetic: `internal/targetweight/glide`.
- Where each decision applies them: `applyGlide` in
  `internal/targetweight/momentum_run.go`.
- The configuration fields: `GlideConfiguration` in
  `internal/event/target_weight_configuration.go`.
- The recorded figures: `GlideFigures` and `SleeveShare` in
  `internal/event/target_weight_decisions.go`.
- The rules: ADR 0026 (point 6 for the bond sleeve) and plan.md section 2.
