# Momentum: the IRAs' rule, the order planner and the hard limits

*For the owner. How the momentum rule picks and weights the IRAs' stocks
each month, how a decision becomes orders, which hard limits every order
must pass, and what can go wrong. The finer points have their own
chapters: [momentum universe](momentum-universe.md),
[minimum lot](momentum-minimum-lot.md) and [glide path](glide-path.md).
This chapter puts the whole rule in one place.*

Terms such as net liquidation, mark, broker snapshot, Degraded and the kill
switch are explained in the [glossary](glossary.md).

## Where this stands

The rule, the planner and the limits are built and tested in Go
(`internal/targetweight/momentum`, `internal/targetweight/momentum_run.go`,
`internal/orderplan`, `internal/risklimit`), and parity with the backtest
was checked for the rule itself ([parity](parity.md); the glide path, the
minimum lot and the IRA buy timing are not yet compared). **The
Traditional IRA's engine starts on the server in dry-run** (since
2026-10-28): the engine program composes a momentum configuration
(`cmd/engine/compose.go`), `deploy/etc/engines/trad-ira.config` is its
dry-run configuration (placeholders for the account and your birth month,
which you name on the server), and a fresh IRA run is approved with the
rebalance its previous run left in progress, or none, and holds until you
clear it ([restart](restart.md); `docs/runbooks/missed-rebalance.md`). The daily data job
writes the Traditional IRA's compact stream each weekday morning
(`ti-data@trad-ira`, [market data](market-data.md), "A job for the IRAs"),
and **its own adapter, `ti-adapter@trad-ira`, delivers it** with IBKR's
account snapshot and a mark and contract for every name the engine can
decide ([IBKR adapter](ibkr-adapter.md), "The IRAs' adapter"), so the engine
decides a real month-end in dry-run (`docs/runbooks/first-dry-run-day.md`,
"The Traditional IRA"). **The IRAs cannot trade yet**: no IRA order is sent
before the paper gate, the IRA's reporter and dashboard view are still to
come, and the Roth IRA has no configuration. The configuration's hard limits (`max_orders_per_decision`
40, `max_order_fraction` 0.6) and its cash rule and share quantity
(`settled-cash`, `minimum-lot`: decisions D5 and D7) are dry-run values for
you to confirm before paper. So the "one IRA month" walk-through below stops
at sending.

## What it is and why

Each IRA runs the momentum "combination" the owner chose on 2026-10-14
(plan decision D1): `research/edge-search/momentum/defensive_v2.py` as the
`F_COMBO` variant configures it (`research/edge-search/build_variants.py`;
plan section 2, "Momentum, combination"). In one sentence: once a month,
buy the 25 large US stocks that rose most over the past year (leaving out
the last month), no more than 3 from one sector, more of the steadier ones
and less of the jumpy ones, and move half the stock money into a bond fund
when the market's own trend turns down. A bond sleeve sized by the owner's
age sits beside it ([glide path](glide-path.md)).

## The rule, step by step

**1. When.** At the close of each month's last trading session. The
orders are for the next session, the first of the new month (plan section
2; `TestMomentumRunDecidesOnTheLastSessionOfEachMonth`). Every month
records one decision, decided or withheld with a reason, so a month is
never skipped silently (ADR 0026, point 1).

**2. The universe.** Every US common stock (Massive's listing type `CS`)
whose raw close that session is at least $5, ranked by that one session's
dollar volume (close x volume, unadjusted); the top **500** are kept. Ties
in dollar volume go by ticker. How the market's data reaches the engine,
and why the 500 are chosen by the engine's own code, is in
[momentum universe](momentum-universe.md); the data step itself is in
[market data](market-data.md).

**3. The score ("12-1 momentum").** For each of the 500, from its split-
and dividend-adjusted closes: the close 21 sessions ago divided by the
close 252 sessions ago, minus one. That is roughly the past year's return
without the last month. A stock needs 253 closes; one with fewer is
`short-history`. A stock not held with a missing session in its window is
`gap` and is not ranked that month. A stock **you hold** with a missing
session is scored across it, from its last 253 closes within the last 260
sessions, as the backtest did (owner's decision of 2026-10-18, rules version
1.15.0; [momentum universe](momentum-universe.md), "A stock you hold with a
missing day"). This applies to a run fed the compact universe stream, the
one the IRAs are meant to use; a run fed whole-market bars still leaves out
every stock with a gap.

**4. The volatility.** The population standard deviation of the stock's
last 252 daily returns. A stock whose returns were all identical has no
volatility to divide by; it is `zero-volatility` and left out.

**5. Ranking and the sector cap.** Highest score first; equal scores by
ticker. Walk down the list and pick until there are **25**, taking at most
**3** from any one sector. A stock passed over for its sector is
`sector-capped`; one after the book is full is `beyond-top-n`. A stock with
**no sector label** (neither iShares nor Nasdaq labels it) is picked like
any other and counts toward the 25 but toward no sector's 3, as the backtest
did (owner's decision of 2026-10-18, rules version 1.13.0). If more than
20% of the 500 have no label in one month, the labels have failed, and the
month is withheld as `labels-missing` ([momentum universe](momentum-universe.md),
"Stocks with no sector label").

**6. The weights.** Each pick gets a weight in proportion to one over its
volatility, and the weights add up to **98%** of the momentum book (2% is
kept as cash).

**7. Risk-off.** If SPY's last close is strictly below the mean of its last
200 closes (that close included), the market is "risk-off": the picks are
scaled down to half (they add up to 49%) and the other **49% goes to IEF**,
an intermediate Treasury fund (`risk_off_fraction` 0.5). With fewer than
200 SPY closes the month is withheld (`insufficient-trend-history`); a
missing SPY close in the window withholds it as `missing-bars`. The
backtest treated both as risk-on; live trading refuses to guess (ADR 0026,
point 8).

**8. Whole shares (D7-L).** A pick one share of which costs more than its
target value is skipped, and the next eligible stock takes its place; a pick
you already hold is kept instead, with no order that month
(`held-one-lot-short`). If nothing is affordable the month is withheld
(`all-unaffordable`). All of it is in [minimum lot](momentum-minimum-lot.md).

**9. The bond sleeve.** The whole book above is scaled to (1 - b) of the
account, and 98% of b goes half to IEF and half to SHY, where b is the bond
share for the owner's age that month. IEF held by both sleeves is one
target, the sum ([glide path](glide-path.md)).

The decision record (`strategy.targets.decided`) lists every one of the
500 with its rank, score, volatility, sector, status and weight, the
risk-off reading, the glide figures and the minimum-lot figures, so the
month can be read back in full from the run's journal (one JSON line per
record). `journal-query` has no report of decisions; its `orders` and
`refusals` reports list what followed ([journal-query](journal-query.md)).
The dashboard shows each decision's outcome, withheld reason and targets,
rounded to whole percent ([alerts and dashboard](alerts-and-dashboard.md)).

## From targets to orders: the order planner

The planner (`internal/orderplan`, `PlanIRA`) knows no strategy rule. It
turns the month's targets into orders, valued at the account's net
liquidation and each stock's raw close of the decision session (plan
section 5; ADR 0026, point 2):

1. It takes every instrument held or targeted, in alphabetical order.
2. **Held, with no target:** sell the whole holding.
3. **Not held, with a target:** buy target x net liquidation / price whole
   shares.
4. **Held, with a target:** trade only when the holding's weight is **more
   than 25% of its target away** from it (`rebalance_band` 0.25, strictly
   more), and then trade back to the target. So a stock targeted at 4% is
   left alone between 3% and 5%.
5. Every quantity is rounded **toward zero** to whole shares. A trade that
   rounds to nothing is recorded as `strategy.order.skipped` with its reason,
   never dropped silently.
6. No sell is larger than the holding minus every sell already working.
7. **Sells first.** The sells are sent at the decision. The **buys** are
   planned only once every sell has finished (filled, cancelled or refused),
   or at once if there were no sells, because an IRA never borrows: the buys
   are paid from the cash actually there (plan section 5, "IRA execution").
8. **The buy budget** is the account's total cash (both IRAs are margin
   IRAs that never borrow, so unsettled sale proceeds count: plan decision
   D5, cash rule `total-cash`), plus what the sells raised net of
   commission, minus anything a newer account snapshot shows has gone.
9. **Each buy is costed at its worst case**: its shares at the limit price
   (the previous close plus the 5% band) plus a **$2.00** fee allowance per
   order (`execution.max_fees_per_order`; $2.00 is the plan's value,
   pending paper trading; plan section 8). If the
   buys do not fit the budget, every buy is scaled down by the same factor
   and rounded down again; a buy left with no share is recorded as skipped.
   This scaling is not in the backtest.

Every order is a whole-share DAY limit order at the band's edge, with a
client order ID made from the decision, the instrument and the side, as for
Boost ([boost](boost.md#the-one-order)).

While any order of a rebalance is still working, the next month's decision
is withheld (`rebalance-in-progress`) and nothing more is sent, until IBKR
reports the order finished or you clear it
(`docs/runbooks/missed-rebalance.md`; [operator](operator.md), "A
rebalance clearance").

## The hard limits in plain words

Every planned order, Boost's and the IRAs', goes through the risk gate
(`internal/risklimit`) before it can be released. The limits live outside
the strategy so that a defect in a rule cannot break them (ADR 0026, point
3). Each limit's value is in the account's configuration, under `limits`,
and is part of its hash. Values are set so that **every order in the
backtests passes**; a value that refuses a backtested order is a
configuration error.

| Limit | What it checks | Joint account (Boost) | IRAs | On a breach |
| --- | --- | --- | --- | --- |
| Exposure (`max_exposure`) | An order that **increases** exposure must not leave holdings, valued at the previous close, above this share of net liquidation. A sell is never refused by it | 1.5 (150%) | at most 1 (100%): an IRA configuration above 1 is refused | refused; Degraded on the instrument (`margin-exposure`, `ira-exposure`) |
| Orders per decision (`max_orders_per_decision`) | Orders that are not risk-reducing at one decision time | 1 | from the IRA configuration (none exists yet) | the plan's buys refused; Degraded (`order-count`) |
| Order size (`max_order_fraction`) | One order's value as a share of net liquidation | 1 (100%) | from the IRA configuration | refused; Degraded (`order-size`) |
| Price band (`price_band`) | The order's reference price must be within this share of IBKR's latest recorded price (the mark), recorded at or after the close | 0.05 | from the IRA configuration | refused; Degraded (`price-band`, or `missing-mark` with no mark) |
| Cash (IRAs only) | A buy's worst-case cost must fit the cash still available | — | always | refused; Degraded (`insufficient-cash`) |
| Holding | A sell must not exceed the shares held, net of sells already working, in the engine's book and in IBKR's latest snapshot | always | always | refused; Degraded (`sell-exceeds-holding`, `broker-holding`) |
| Snapshot age (`execution.max_snapshot_age_minutes`) | IBKR's latest snapshot must be no older than this when a sell is checked against it | 30 | from the IRA configuration (required; 1 to 390) | refused, **no state change**, re-planned at the next decision (`stale-broker-snapshot`) |
| A recorded decision | Every order must name a decision recorded in this run | always | always | refused; **Halted** (`undecided-order`): a defect in the program |
| Kill switch | Your own stop | always | always | refused, no state change (`kill-switch`); only the joint account's sale back to 98% still goes |

A **risk-reducing** sell (one that moves the account toward 98% invested
or lower: Boost's exit, a momentum or bond sell) is exempt from the order
count, the order size and the price band, but never from the holding check,
the recorded-decision check or the kill switch (ADR 0026, points 3 and 4).
What Degraded and Halted then stop is in
[states-and-kill-switch](states-and-kill-switch.md); the reasons and what
to do are in `docs/runbooks/halted-account.md`.

The adapter is meant to check the same limits again before it calls IBKR
(ADR 0026, point 3). **Not built yet**: the adapter's sender checks the
order's shape, account and mode, not these limits (`adapter/ibkr/sender.py`).

## One IRA month, end to end

**Stops here today** marks the first step not on `main`.

| # | When | What happens | Built? |
| --- | --- | --- | --- |
| 1 | each session, the next morning | The data job records the whole market, the latest session's universe and sector labels, and writes SPY's, IEF's, SHY's and every holding's bars, the holdings read from the engine's own log ([market data](market-data.md), "A job for the IRAs"). | Built for the Traditional IRA: `ti-data@trad-ira`, weekday mornings at 06:15, 07:15 and 08:15 New York |
| 2 | the morning after the last session of the month | The same job writes the compact stream, exactly as `encode --ranker`: bars for the named instruments, the universe snapshot, and the 500 ranked stocks' price histories ([momentum universe](momentum-universe.md)). | Built |
| 3 | the same morning | The IRA's own adapter, `ti-adapter@trad-ira` (client ID 12), reads the account once and delivers that stream behind it: the account snapshot the engine reads (version 4: positions, net liquidation, IBKR's settled and total cash, from the same reading), and, before the session's close, a mark for every name the engine can decide, each ranked stock at its universe close and SPY, IEF, SHY and the holdings at their bars, each with the IBKR contract the adapter resolved for it by name; a name IBKR cannot resolve to one US-dollar stock gets no mark ([IBKR adapter](ibkr-adapter.md), "The IRAs' adapter" and "Contracts and marks for an IRA"). The morning reading stands for the close only with the IRA's own Flex executions proof (`ti-flex@trad-ira`, 05:45, 06:45, 07:45). | Built; the order to start the engine, its warm-up, the adapter and the same-day probe is `docs/runbooks/first-dry-run-day.md`, "The Traditional IRA" |
| 4 | at the close | The engine decides the month and plans the sells, checked against the limits; any order for a name without a mark as of the close is refused (`missing-mark`) before it is released. | Built and tested on recorded inputs, and against the real IRA adapter with the scripted Gateway |
| 5 | next session | In dry-run (now): nothing is sent; the engine records a synthetic cancel for each order it releases, so the buys are planned at the same close from the snapshot's settled cash. In paper (later): the adapter sends the sells; when every sell has finished, the engine plans and releases the buys from the cash realised. | Dry-run built and rehearsed: the IRA adapter runs and the engine decides a dry-run month. **Stops here today**: no IRA order is sent before the paper gate (AGENTS.md rule 5); sending one will need the adapter's sender to take the contract the journal records for a pick, which today names a contract only from `con_ids` |
| 6 | after | Fills come back; the reporter and the dashboard show the month. | As for Boost ([boost](boost.md#one-boost-day-from-the-data-to-ibkr-and-back)) |

## What can go wrong, and what the program does then

Every one of these refuses or withholds rather than guess. Alerts come from
the reporter ([alerts and dashboard](alerts-and-dashboard.md)); `<account>`
is the account the IRA's configuration names. An **urgent** alert pages
you; a **routine** one only informs. A withheld month sends one alert when
a run of withheld months with the same reason starts, and its text says
held positions and orders are unchanged. The IRA's reporter is not running yet, so none of these alerts reaches you
for the IRA until it is; until then the same-day probe (`docs/runbooks/first-dry-run-day.md`, I-8) names the
latest decision and any `missing-mark` refusal.

| What you see | What the program does | What you do |
| --- | --- | --- |
| The Traditional IRA's engine will not start, saying `glide birth value must be YYYY-MM with a month 01-12` | The shipped `trad-ira.config` names the placeholder `YYYY-MM`, never your birth month, so the engine refuses rather than decide from a made-up age. Nothing is recorded | Name your birth year and month on the server ("Starting the Traditional IRA's engine in dry-run", step 2) |
| `ira-exposure`. Alert: `<account>: order refused (ira-exposure)` (urgent), its text naming the exposure after the order and the limit; then `<account>: engine degraded (risk-limit-breached)`, which repeats until you acknowledge it | The risk gate refused a buy that would have left the account's holdings, valued at the previous close, above `max_exposure` (at most 1: an IRA never borrows). Nothing was sent for it. The account is Degraded on that instrument: no buy goes, and the instrument is frozen | Compare holdings, cash and net liquidation at IBKR with the refusal's figures (`journal-query refusals`). Follow `docs/runbooks/halted-account.md`; the way out is a fresh run you approve ([restart](restart.md)) |
| `insufficient-cash`. Alert: `<account>: order refused (insufficient-cash)` (urgent), naming the buy's worst-case cost and the cash available; then `<account>: engine degraded (risk-limit-breached)`, repeating until acknowledged | The gate refused a buy whose worst-case cost (its shares at the limit price plus the fee allowance) exceeds the cash still available after the buys released before it. The planner already scales buys to fit the budget, so this means the cash changed between the plan and the check. Degraded on that instrument | Check cash at IBKR against the last account snapshot. Follow `docs/runbooks/halted-account.md` |
| `no-eligible-names`. Alert: `<account>: decision withheld (no-eligible-names)` (routine) | The month is withheld: no stock in the ranked 500 could be picked (every one short of history, with a gap, or without volatility), and none was skipped as unaffordable. The book is kept; nothing is ordered | Check the month's data step: the universe snapshot and the ranked stocks' price histories ([momentum universe](momentum-universe.md), [market data](market-data.md)). The next month decides as usual |
| `insufficient-trend-history`. Alert: `<account>: decision withheld (insufficient-trend-history)` (routine), saying SPY has fewer than 200 closes | The month is withheld: the risk-off test needs 200 SPY closes. The backtest treated this as risk-on; live trading refuses to guess (ADR 0026, point 5). The book is kept | Feed the run a stream that starts at least 200 sessions before its first decision. A run already started cannot gain history it was not given: start a fresh run with a longer history ([restart](restart.md)) |
| Alert `<account>: decision withheld (missing-bars)` (urgent): a held or targeted instrument, or SPY in its 200-close window, has no bar, or the month's last session never closed | The month is withheld; nothing is ordered. Explained in [engine](engine.md), "What can go wrong" | Fix the feed. The next month decides as usual; there is no mid-month catch-up (`docs/runbooks/missed-rebalance.md`) |
| Alert `<account>: decision withheld (stale-account-snapshot)` (urgent) | No account snapshot as of the close: withheld. See [engine](engine.md), "What can go wrong" | Check the adapter and the Gateway (`docs/runbooks/gateway-login.md`) |
| Alert `<account>: decision withheld (missing-universe)`, `(histories-missing)` or `(labels-missing)` (urgent) | The month is withheld, the book kept. Explained in [momentum universe](momentum-universe.md), "What can go wrong" | As that chapter says |
| Alert `<account>: decision withheld (all-unaffordable)` (urgent), or a trade recorded as skipped | No pick could be bought in whole shares, or a trade rounded to no share. Explained in [minimum lot](momentum-minimum-lot.md), "What can go wrong" | As that chapter says |
| `rebalance-in-progress`. Alert `<account>: decision withheld (rebalance-in-progress)` (routine): last month's rebalance is still working | The month's decision is withheld and nothing more is sent until IBKR reports every order of the rebalance finished or you clear it. An order settled `--never-sent` never finishes, so it holds the next rebalance until you clear it ([paper trading](paper-trading.md), the row "The link stayed down past the end of the order's day") | `docs/runbooks/missed-rebalance.md`; [operator](operator.md), "A rebalance clearance" |
| The sells finish but `journal-query orders` shows smaller buys than planned, or a buy recorded as skipped | The buys were scaled down to fit the cash; a buy left with no share is recorded as skipped | Nothing; the next month decides afresh |
| `journal-query orders` shows a sell failed or was cancelled part-filled | The buys wait for every sell to finish, then are budgeted from the cash actually raised; nothing is ever borrowed | Check the order at IBKR |
| Any other refusal: `<account>: order refused (order-count)`, `(order-size)`, `(price-band)`, `(missing-mark)`, `(sell-exceeds-holding)`, `(broker-holding)`, `(stale-broker-snapshot)`, `(undecided-order)` or `(kill-switch)` | Refused; the state that follows (Degraded, Halted or none) is in the hard-limits table above. Explained in [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong" | `docs/runbooks/halted-account.md` |
| A fresh run started while a rebalance was working | `operator approve` states the rebalance in progress in the approval, the engine checks it against the previous log, and the fresh run sends nothing for it and withholds every month as `rebalance-in-progress` until you clear it (ADR 0026, point 5) | `docs/runbooks/missed-rebalance.md`, "A restart mid-rebalance" |

## Starting the Traditional IRA's engine in dry-run

Dry-run sends nothing to IBKR, ever. On the server, over Tailscale SSH, from
the checkout, once the build with this slice is installed (`sudo ti-release
<tag>`):

1. `git pull`, then `sudo deploy/provision.sh services`. It installs
   `/etc/trend-investing/engines/trad-ira.config` (there is none yet) and
   keeps your `boost.config`, putting the shipped one beside it as
   `boost.config.example`. Check: `sudo grep -c '"account_id": "joint"'
   /etc/trend-investing/engines/boost.config` prints `0`.
2. Name your birth year and month (the glide path counts your age from it;
   the repository holds only `YYYY-MM`), and, if `accounts/trad-ira.env`
   holds the IRA's paper number, the account, as Boost's step 1 does
   (`docs/runbooks/first-dry-run-day.md`): `sudo sed -i
   's/"birth": "YYYY-MM"/"birth": "1975-06"/'
   /etc/trend-investing/engines/trad-ira.config` with your own year and
   month. A dry-run may keep the placeholder account `trad-ira`; a paper run
   may not. Never put either value in the repository.
3. Write the run's start: `sudo -u ti-trader tee
   /var/lib/trend-investing/engines/trad-ira/run.env` with
   `TI_RUN=demo-<today>`, `TI_AS_OF=2025-01-02T21:00:00Z` (the synthetic
   month's first close) and an empty `TI_RESTART_ARGS=`.
4. `sudo systemctl start ti-engine@trad-ira`, then `sudo journalctl -u
   ti-engine@trad-ira -n 3` shows `listening on /run/ti-engine-trad-ira/engine.sock`.
5. Replay the synthetic month (`cmd/engine/testdata/momentum-month.jsonl`,
   invented prices only) as the adapter would: `sudo install -m 0644
   cmd/engine/testdata/momentum-month.jsonl deploy/feed-month.py /tmp/`, then
   `sudo -u ti-trader python3 /tmp/feed-month.py --socket
   /run/ti-engine-trad-ira/engine.sock --log
   /var/lib/trend-investing/engines/trad-ira/demo-<today>.log --config
   /etc/trend-investing/engines/trad-ira.config --month
   /tmp/momentum-month.jsonl`. Expect one line,
   `2025-01-31T21:00:00Z strategy.targets.decided withheld histories-missing`:
   the month's decision is recorded withheld, since the real rule wants 200
   SPY closes and the 500 ranked stocks' histories a month does not hold,
   and nothing is planned or sent. It prints no account number, and it
   refuses, before connecting, a configuration or a log that is not
   dry-run, or a log of another account: invented prices must never become
   a paper or live account's facts.
6. `sudo systemctl stop ti-engine@trad-ira`, then `sudo journalctl -u
   ti-engine@trad-ira -n 3` shows `wrote .../demo-<today>.journal`: the run
   stopped cleanly. `sudo grep -c '"withheld_reason":"histories-missing"'
   /var/lib/trend-investing/engines/trad-ira/demo-<today>.log` prints `1`:
   the durable log holds the withheld month, and `sudo -u ti-trader
   journal-query orders` on it lists no order.
7. Move the demo's `demo-<today>.log`, `.journal` and `run.env` unchanged into
   `/var/lib/trend-investing/engines/trad-ira/demo/` (the engine reads only
   the folder's root), so the IRA's real first run is a first run, not a
   fresh run after the demo.

The same steps are rehearsed in a container by `deploy/rehearse.sh`, and in
`cmd/engine` by `TestTheShippedIRAConfigurationRecordsTheMonthWithheld`.

## What backs this

- Plan sections 2 ("Momentum, combination"; "Bond sleeve and glide path"),
  4 (universe and sectors), 5 ("IRA execution", hard limits) and 10
  (decisions D1, D2, D5, D7, D8); ADR 0026, points 1 to 5 and 8, and its
  amendments of 2026-10-17 and 2026-10-18.
- The rule, in `internal/targetweight/momentum`:
  `TestRankUniverseKeepsCommonStockAtOrAboveTheFloorByDollarVolume`,
  `TestScoreNeeds253ClosesNot252`,
  `TestScoreLeavesOutANameWithAGapInItsWindow`,
  `TestScoreMarksZeroVolatility`,
  `TestTrendRiskOffOnlyStrictlyBelowTheMeanIncludingTheLastClose`,
  `TestTrendWithheldOnShortOrGappedSPY`,
  `TestSelectBookRanksCapsAndExcludes`,
  `TestSelectBookPicksNamesWithNoSectorAsTheBacktestDoes`,
  `TestSelectBookRiskOffMovesHalfToTheBond`,
  `TestSelectBookWeightsSumTo98Percent`, `TestDecideRecordsEveryRankedName`,
  `TestDecideRiskOff`, `TestDecideWithholds`, `TestIsDecisionSession`, and
  the golden months `TestGoldenMonths` (they run only with the QuantConnect
  logs, kept outside the repository). The backtest's own behaviour, kept
  for parity: `TestBacktestBookKeepsTiesInInputOrderAndLeavesNoSectorUncapped`,
  `TestBacktestBookWeightsAZeroVolatilityAsOne`,
  `TestBacktestTrendTreatsAShortHistoryAsRiskOnAndDropsMissingCloses`.
- The engine program and the approval, in `cmd/engine`:
  `TestAMomentumConfigurationIsComposed`,
  `TestADryRunEngineDecidesASyntheticIRAMonthAndStopsCleanly`,
  `TestTheMomentumMonthFixtureIsTheGeneratedOne`,
  `TestAMomentumRunInLiveModeIsRefused`,
  `TestTheShippedIRAConfigurationRecordsTheMonthWithheld`,
  `TestTheTradIRAEngineStartsAsItsUnitStartsIt`,
  `TestAFreshIRARunStartsOnlyFromTheRebalanceItsPreviousLogEndsWith`,
  `TestAMomentumFirstRunRefusesABoostShapedApproval`; in
  `internal/targetweight`: `TestAFreshIRARunMidRebalanceHoldsUntilCleared`,
  `TestAClearanceMustNameTheRebalanceTheApprovalStated`,
  `TestAMomentumRunReadsOnlyAnIRAApproval`, `TestBoostRefusesAnIRAApproval`,
  `TestAFreshIRARunResumesTheIncidentItsApprovalCarries`,
  `TestAMomentumRunRecordsTheApprovalItAccepted`,
  `TestRulesVersionDeclaresTheIRAApproval`; in `internal/restart`:
  `TestTheRebalanceInProgressIsReadFromTheLog`,
  `TestTheEngineRefusesAnApprovalOfAnotherRebalance`,
  `TestRecoveryReadsTheApprovalTheRunAccepted`,
  `TestRecoveryReadsRecordsNotTheRunningBuildsRules`,
  `TestAnAcceptanceThatCannotBeReadBackIsRefused`; in `internal/event`:
  `TestARunRecordsTheApprovalItAccepted`,
  `TestAnIRAApprovalStatesTheRebalanceInProgress`,
  `TestDecodeAnIRAApprovalRequiresTheRebalanceKey`; in `cmd/operator`:
  `TestAnIRAApprovalStatesTheRebalanceInProgress`; provisioning:
  `deploy/rehearsal/test-engine-configs.sh` and `deploy/rehearse.sh`.
- The run, in `internal/targetweight`:
  `TestMomentumRunDecidesOnTheLastSessionOfEachMonth`,
  `TestMomentumBuysAtOnceWithoutSellsThenSellsFirst`,
  `TestMomentumBuysAreBudgetedUnderTheCashRule`,
  `TestMomentumBuyPhaseWaitsForAPartlyFilledSellToBeCancelled`,
  `TestBuysAreBudgetedFromNoMoreCashThanALaterSnapshotExplains`,
  `TestAWithheldRebalancePlansNothing`,
  `TestAMissedDecisionSessionIsWithheldAtTheNextClose`,
  `TestMomentumFailsClosed`, `TestMomentumJournalIsByteIdenticalAndReplays`.
- The planner, in `internal/orderplan`:
  `TestIRABandAtExactlyAQuarterOfTheTargetDoesNotTrade`,
  `TestIRAEntriesExitsAndSellsFirstInInstrumentOrder`,
  `TestIRAPlanWithNoSellsHasOnlyBuys`,
  `TestIRANeverSellsBeyondTheHoldingNetOfWorkingSells`,
  `TestIRARoundsTowardZero`, `TestBuysAreCostedAtTheBandEdgePlusFees`,
  `TestScaleBuys`, `TestBudgetCarriedThroughReleasesAndPartialFills`,
  `TestBudgetLosesCashANewerSnapshotCannotExplain`.
- The limits, in `internal/risklimit`: `TestMarginExposureAtBelowAndAbove`,
  `TestASellIsNeverRefusedByTheExposureCheck`,
  `TestMaxOrderSizeAtBelowAndAbove`, `TestMaxOrderCountAtBelowAndAbove`,
  `TestPriceBandAtBelowAndAbove`, `TestMissingMarkIsRefused`,
  `TestAnUntaggedOrderIsRefusedAndHalts`,
  `TestARiskReducingSellIsExemptFromMarkBandAndSize`,
  `TestIRAExposureIsAccountWideAtBelowAndAbove`,
  `TestIRABuyCostAtBelowAndAboveTheCashAvailable`,
  `TestIRANeverBuysOnMargin`,
  `TestASellBeyondTheHoldingNetOfWorkingSellsIsRefused`.
