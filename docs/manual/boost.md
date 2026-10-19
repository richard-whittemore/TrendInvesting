# Boost: the joint account's rule, end to end

*For the owner. What the Boost rule decides, when it turns on and off, how
its one order is sized and priced, what happens after an order is refused,
what one Boost day looks like from the data to IBKR and back, and what can
go wrong.*

Words in **bold** on first use, and the terms net liquidation, mark, latch,
broker snapshot and kill switch, are explained in the [glossary](glossary.md).

## What it is and why

Boost runs the joint taxable margin account (plan section 1). It holds SPY
almost all the time, and borrows for a short extra step after a sharp
one- or two-day drop in an uptrend. It is the backtested rule in
`research/edge-search/boost/template.py` (BOOST_SIZE 1.5, RSI_ENTRY 10,
TREND_DAYS 200, EXIT_SMA 5), restated in plan section 2, "Boost", and in
ADR 0026, point 2, "Boost". The owner chose margin Boost, not a leveraged
fund, on 2026-10-14 (plan decision D3).

The rule lives in `internal/targetweight/boost` (the arithmetic) and
`internal/targetweight/handler.go` (the session-by-session run). It knows
nothing about IBKR: it reads recorded inputs and writes recorded decisions
(ADR 0026, point 1).

## The rule in plain words

Every session, after the close, the engine reads SPY's closing price
(split- and dividend-adjusted, so a dividend is not mistaken for a fall)
and updates three readings:

- **RSI(2)**: a 0 to 100 gauge of the last two days' moves, smoothed the
  way QuantConnect smooths it (Wilder's method). Low means SPY has just
  fallen hard.
- **The 200-day average**: the mean of the last 200 closes, today's
  included. A close above it means the long trend is up.
- **The 5-day average**: the mean of the last 5 closes, today's included.

Then it decides whether the boost is on. This on/off state is called the
**latch**.

| The latch is | It changes when | To |
| --- | --- | --- |
| off | RSI(2) is below 10 **and** the close is above the 200-day average (both strictly, both readings ready) | on |
| on | the close is above the 5-day average (strictly) | off |

Two things follow from the table. First, once on, the boost stays on even
if SPY then closes below its 200-day average: only the 5-day exit turns it
off (`TestLatchedBoostSurvivesACloseBelowTheTrendMean`). Second, the latch
is state, not something worked out afresh each day: a restart takes it
from the record, never from prices (plan section 2; ADR 0026, point 5).

The **target** follows the latch:

| Latch | SPY target, as a share of the account's net liquidation |
| --- | --- |
| off | 98% (`invested_fraction` 0.98) |
| on | 147% (0.98 x `boost_size` 1.5) |

147% is Boost's own target. **150% is a different number**: the hard
limit on exposure that any buy must not exceed ([below](#the-hard-limits-for-this-account)).

The figures are in the account's configuration,
`deploy/etc/engines/boost.config`, under `boost`. Changing any of them
changes the configuration hash, so it is a new run, never a silent edit
(ADR 0016).

## The one order

The engine decides after the close and the order is for the next session
(plan section 2: "decide after the close, trade at the next open"). The
order planner (`internal/orderplan`, `PlanBoost`) makes at most **one**
order a session:

- **When.** Only when the target changes (98% to 147%, or back), or on the
  run's first decision (there is no target in force yet, so the first
  decision buys to 98% from cash), or after a refusal that left the account
  Normal (next section). On any other session no order is planned.
- **How many shares.** target x net liquidation / SPY's raw (unadjusted)
  close of the session just decided, called the **previous close** because
  the order is for the session after it, minus the shares held, rounded **toward zero** to whole shares. Rounding
  toward zero never buys above the target and never sells below it. A sell
  is also reduced by the shares already being sold by an earlier sell that
  has not finished, so two sells never together go below the target. A
  difference that rounds to no share plans nothing.
- **What kind of order.** A whole-share **limit** order, good for the
  session only (a DAY order), at the edge of the price band: that previous
  close **plus 5%** for a buy (rounded down to the cent), **minus
  5%** for a sell (rounded up to the cent). It is marketable, but can never
  fill worse than the band (ADR 0026, point 2, "Order type and price";
  `price_band` 0.05).
- **Its name.** A client order ID made from the decision, the instrument and
  the side (`tw-` and 24 hex digits). IBKR shows it as the order's
  reference (`orderRef`). It is a label: what stops an order going twice is
  that it is written down before it is sent ([re-delivery](redelivery.md)).

Then the order goes through the hard limits (the risk gate) before
anything can be sent ([states-and-kill-switch](states-and-kill-switch.md)).

## A refused order is planned again (re-plan)

Normally Boost orders only when its target changes. But if the gate refuses
the order **and the account stays Normal** (the kill switch was on, or
IBKR's latest snapshot was too old to check a sell against), the next
session that decides plans the difference between what the account holds
and the target in force, even though the target has not changed (owner's
decision of 2026-10-18; ADR 0026, amendment of that date; rules version
1.16.0). So a buy the kill switch refused goes at the first decided close
after you release the switch.

A refusal that moves the account to Degraded or Halted does **not** set up a
re-plan: those states follow their own rules, and the way out is a fresh
run you approve, whose first decision plans to the target from scratch.

Each decision records whether a re-plan is pending after it
(`replan_pending`), so a run restarted from that decision picks it up.

## Before the first decision: the warm-up

The 200-day average needs 200 closes before it is ready. A run's
configuration names `warm_up_start`, the first session whose close feeds the
readings; sessions before the run's first decision session (`-as-of`) only
feed the readings and decide nothing, as QuantConnect's warm-up does. The
daily data job's one-time warm-up delivers that history
([market data](market-data.md), "The one-time warm-up").

The shipped dry-run file says 2024-01-02, which is outside the two years of
history Massive's plan reaches back, so the data job cannot fetch a warm-up
for that file. A run fed by the data job needs a date inside the window;
that is a different configuration (a new hash), so it starts as a new run.
A paper run uses a date inside the window
(`docs/runbooks/first-paper-order.md`, step A5, recommends 2025-01-02).

## The hard limits for this account

From `deploy/etc/engines/boost.config`, `limits` and `execution`:

| Limit | Value | In plain words |
| --- | --- | --- |
| `max_exposure` | 1.5 | A buy is refused if it would leave SPY above 150% of net liquidation, valued at the previous close. A sell is never refused by it, and nothing is sold because the market moved the account above 150% |
| `max_orders_per_decision` | 1 | At most one order that is not risk-reducing per decision |
| `max_order_fraction` | 1 | No single order larger than 100% of net liquidation (Boost's step from 98% to 147% is about 49%) |
| `price_band` | 0.05 | The order's reference price (the raw previous close) must be within 5% of IBKR's latest recorded price for SPY (the **mark**); also sets the limit price above |
| `excess_liquidity_warning`, `_urgent`, `_stale_minutes` | 0.40, 0.20, 30 | The margin-cushion alarm ([states-and-kill-switch](states-and-kill-switch.md#the-margin-cushion-alarm)) |
| `execution.max_snapshot_age_minutes` | 30 | How old IBKR's latest snapshot may be when a sell is checked against it |

A **risk-reducing** sell (one that moves the account toward 98% or lower)
is exempt from the order count, the order size and the price band, so one
late mark cannot block Boost's exit (ADR 0026, points 3 and 4). The
[momentum chapter](momentum.md#the-hard-limits-in-plain-words) explains each
limit once for both strategies.

## One Boost day, from the data to IBKR and back

This is the path a normal paper day will take. **Stops here today** marks
the first step that does not work on `main` yet: up to that point the
pieces run and are tested; from there on, they are built but do not yet
receive what they need, or are built only in part.

| # | When (New York) | What happens | Where it is explained | Built? |
| --- | --- | --- | --- | --- |
| 1 | 16:00 | The market closes (13:00 on an early close). | [market data](market-data.md) | — |
| 2 | 17:05 (retries 18:05, 20:05) | `ti-data` fetches the day's whole-market bars and recent splits and dividends from Massive, records them, and writes the engine's input for the session: SPY's bar, then "the session closed", to `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<date>.jsonl`. | [market data](market-data.md), "The daily data job" | Built |
| 3 | after 17:05 | The adapter (`ti-adapter-boost`), while its link to IB Gateway is up and matched to IBKR's orders, picks up the file, takes one **broker snapshot** (IBKR's holdings with IBKR's price for each, net liquidation, Excess Liquidity and open orders: `account.broker.snapshot`), records the snapshot and then every line of the file in its outbox, and sends them over the engine's socket, each numbered in the engine's one series. | [IBKR adapter](ibkr-adapter.md), [paper trading](paper-trading.md) | Built |
| 4 | at the input | The engine writes each input to its durable log, flushed to disk, updates RSI(2) and the averages and moves the latch. **Stops here today.** To decide, it also needs an **account snapshot** as of the close (`account.snapshot`: SPY held and net liquidation), and to release a buy, IBKR's price for SPY recorded at or after the close (the **mark**, `market.mark.recorded`). Nothing produces either yet: the broker snapshot of step 3 is a different record, read by the hard limits on sells, the sale back to 98% and the margin-cushion alarm. So every session is withheld for want of an account snapshot (see the first rows of "What can go wrong") and no order is planned. | [engine](engine.md), [paper trading](paper-trading.md), "Not built yet" | The decision is built and tested against recorded inputs and `make demo-boost-dry-run`; its account snapshot and mark are **not produced** |
| 5 | same evening | With an order released, the engine hands it to the adapter. The adapter writes "about to send", hands it to IB Gateway as a DAY limit order under its client order ID, and writes "sent". The real IBKR client places it only in paper, on a `DU` account, with `"paper_sending_enabled": true` in the adapter's configuration. The shipped configurations are dry-run (every order says `send: false`). | [paper trading](paper-trading.md), [re-delivery](redelivery.md) | Built; not reached while step 4 withholds |
| 6 | next session | IBKR works the order. Each status change and each fill (with IBKR's commission) comes back; the adapter records them and sends them to the engine as inputs. | [paper trading](paper-trading.md), [order lifecycle](order-lifecycle.md) | Built |
| 7 | every 5 minutes | The reporter (`ti-reporter@<account>`) reads the durable log and sends any alert, and the daily summary. | [alerts and dashboard](alerts-and-dashboard.md) | Built and installed; its alerts need your ntfy and PagerDuty entries in `secrets.env` ([server](server.md)) |
| 8 | next morning, before the open | The adapter reads IBKR's positions and cash; the engine compares them with its book. | [reconciliation](reconciliation.md) | Engine side built; the adapter builds the record but does not send it yet (with no account snapshot there is no book to compare, and the check would halt the account) |

In dry-run, step 5 never sends; see the [engine](engine.md), "What dry-run
simulates, and what it does not", for what that leaves out.

## What you see in the record

Each decided or withheld session writes one `strategy.targets.decided`
record. For Boost it carries the close, RSI(2), both averages (and whether
each was ready), the latch before and after, the target in force and
whether a re-plan is pending. An order adds `strategy.order.planned`, then
`risk.order.released` or `risk.order.refused`. `journal-query orders` and
`journal-query refusals` list them ([journal-query](journal-query.md)).

## What can go wrong, and what the program does then

Alerts come from the reporter ([alerts and dashboard](alerts-and-dashboard.md));
`<account>` is the account the configuration names: `joint` in the shipped
dry-run file, your paper account (DU…) in a paper run. An **urgent**
alert pages you; a **routine** one only informs. A withheld session sends
one alert when a run of withheld sessions with the same reason starts, not
one per session.

| What you see | What the program does | What you do |
| --- | --- | --- |
| `margin-exposure`. Alert: `<account>: order refused (margin-exposure)` (urgent), its text naming the exposure after the order and the 1.5 limit; then `<account>: engine degraded (risk-limit-breached)`, which repeats until you acknowledge it | The risk gate refused a buy that would have left SPY above 150% of net liquidation, valued at the previous close. Nothing was sent. The account is Degraded on SPY: no buy goes, and only the one sale back to 98% may still sell SPY. Because the account is no longer Normal, the buy is **not** planned again. Boost's own target, 147%, is below the limit, so this means the account's figures are not what the plan expected (for example net liquidation fell between the snapshot and the decision, or the account already held more SPY than the engine's book) | Compare SPY held and net liquidation at IBKR with the order's figures (`journal-query refusals`). Follow `docs/runbooks/halted-account.md`. The way out is a fresh run you approve ([restart](restart.md)); its first decision plans to the target from scratch |
| Alert `<account>: decision withheld (missing-bars)` (urgent): a session closed with no SPY bar | That session is withheld, and so is **every later session of the run**: the readings are never computed across a gap (plan section 2, "Boost"). The latch is not recomputed. The reason is explained once, in [engine](engine.md), "What can go wrong" | Find why the bar was missing ([market data](market-data.md)). The run cannot recover by itself: stop it and start a new run (a fresh `TI_RUN`, its own warm-up and your approval: `docs/runbooks/durable-log.md`) |
| Alert `<account>: decision withheld (stale-account-snapshot)` (urgent): no account snapshot as of the close, or one taken after it. **On `main` today this is every paper session**, since nothing produces the account snapshot yet (step 4 above) | The session is withheld and no order is planned. The latch still advances, as the backtest's does; a change of target withheld one session is ordered at the next that decides. See [engine](engine.md), "What can go wrong" | Today: nothing; it is a known gap ([paper trading](paper-trading.md), "Not built yet"). Once the account snapshot is built: check the adapter and the Gateway (`docs/runbooks/gateway-login.md`) |
| Alert `<account>: order refused (missing-mark)` with `<account>: no recorded mark for SPY`, or `<account>: order refused (price-band)` (urgent) | A buy is refused: no mark at or after the close, or the previous close more than 5% from the mark. The account goes Degraded on SPY. A risk-reducing sell is not checked against the mark. Explained in [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong" | `docs/runbooks/halted-account.md` |
| `<account>: order refused (kill-switch)` (urgent), sent for the first refusal of each engagement only | Every order is refused except the sale back to 98%; the account stays Normal; the order is planned again at the first decided close after you release the switch | Nothing more, once released. Check it with `journal-query orders`. The kill switch itself: [states-and-kill-switch](states-and-kill-switch.md), [operator](operator.md) |
| `<account>: order refused (stale-broker-snapshot)` (urgent), once per outage; for the exit, `<account>: sale of SPY withheld (stale-broker-snapshot)` | IBKR's latest broker snapshot was older than 30 minutes (`execution.max_snapshot_age_minutes`) when a sell was checked. The sell is refused, the state does not change, and it is planned again at the next decision. Explained in [states-and-kill-switch](states-and-kill-switch.md), "What can go wrong" | Check the Gateway and the adapter (`journalctl -u ti-adapter-boost`). The program can still sell: the exit is planned again at the next decision, and a withheld sale back to 98% is decided again from the next good snapshot. So if the exit matters now and you sell by hand, first stop the engine and the adapter, as [states-and-kill-switch](states-and-kill-switch.md), "Selling by hand safely", says |
| The exit is refused because the account is Halted, Degraded on SPY or under the kill switch | The one sale back to 98% goes instead ([states-and-kill-switch](states-and-kill-switch.md), "The one sale back to 98%"), with its own sent, filled and failed alerts | Follow the alert; do not sell by hand while that sale is working |
| `journal-query orders` shows the DAY order expired, or IBKR cancelled it, with part or all unfilled | The cancellation is recorded and ends the order. Boost does **not** order the rest by itself: it plans again only when the target changes, or after a refusal that left the account Normal. The account stays off its target until then | Decide whether to buy or sell the rest by hand, or wait for the next change of target. Before you trade by hand, stop the engine and the adapter, cancel anything working in IBKR's app and confirm in IBKR's app that no order is open on SPY ([states-and-kill-switch](states-and-kill-switch.md), "Selling by hand safely"). The kill switch is not enough: it cancels nothing already at IBKR, and it does not stop the sale back to 98%, which a Boost exit it refuses fires. A fresh run would plan to the target from scratch |
| The adapter refuses an order for good and you settled it with `relay.py resolve ... --never-sent` | The order stays released in the log. A never-sent **sell** keeps counting as being sold, so every later exit in the run is smaller, or nothing, as in dry-run. A never-sent **buy** is not planned again (the target in force has not changed) | After settling it, reconcile and start a fresh run you approve: its first decision plans to the target from scratch ([re-delivery](redelivery.md), [paper trading](paper-trading.md)) |
| The engine crashed (`systemctl status ti-engine@boost` shows it failed) while the boost was on | Resumed from its log, it carries on in the same latch, neither entering nor exiting by mistake (`TestARestartWhileBoostedNeitherEntersNorExits`) | Resume it (`docs/runbooks/durable-log.md`, "Restarting an engine") |
| A fresh run is refused, naming the previous log's latch | The engine refuses an approval whose latch is not the one the previous log ended with, so a forgotten latch never switches the boost off | Let `operator approve` write the approval from the previous log ([operator](operator.md), [restart](restart.md)) |

## What backs this

- Plan section 2, "Boost"; plan decision D3; ADR 0026, point 2, "Boost",
  "Order type and price" and "Order identity", point 3 (the limits), point 4
  (risk-reducing), point 5 (state from the record), and its amendment of
  2026-10-18 (re-plan after a refusal); ADR 0019 (the sale back to 98%).
- Parity: Go's RSI(2), both averages, the latch and the target matched
  QuantConnect on every one of 7,166 sessions, within the thresholds in
  [parity](parity.md) (`docs/research/parity-2026-10.md`).
- Tests of the rule, in `internal/targetweight/boost`:
  `TestEntryNeedsRSIStrictlyBelowTheThreshold`,
  `TestEntryIsBlockedAtOrUnderTheTrendMean`,
  `TestEntryNeedsReadyIndicators`,
  `TestExitNeedsACloseStrictlyAboveTheExitMean`,
  `TestLatchedBoostSurvivesACloseBelowTheTrendMean`, `TestTargets`,
  `TestFirstDecisionFromCashIsNinetyEightPercentWithNoBoost`,
  `TestRSIIsWilderSmoothed`, `TestRSIWithNoLossIsOneHundred`,
  `TestMeansIncludeTheCloseAndWaitForTheirLength`; the golden cases
  `TestGoldenBoost` and `TestGoldenBoostExitThenNextDayReentry` (they run
  only with the QuantConnect logs, kept outside the repository).
- Tests of the order, in `internal/orderplan`:
  `TestFirstDecisionBuysToNinetyEightPercentFromCash`,
  `TestBoostEntryRoundsTowardZero`, `TestBoostExitRoundsTowardZero`,
  `TestNoOrderWithoutAStateChange`,
  `TestARefusedOrderIsReplannedAtTheNextDecision`,
  `TestNoOrderWhenTheRoundedQuantityIsZero`,
  `TestLimitPricesStayInsideTheBandOnCents`,
  `TestClientOrderIDIsDeterministicPerTagInstrumentAndPhase`,
  `TestAnExitIsNetOfSellsAlreadyWorking`.
- Tests of the run, in `internal/targetweight`:
  `TestBoostRunDecidesEnterAndExit`,
  `TestAMissingBarWithholdsAndNeverComputesAcrossTheGap`,
  `TestAStaleSnapshotWithholdsButTheLatchAdvances`,
  `TestAMissingMarkRefusesAndDegrades`,
  `TestARestartWhileBoostedNeitherEntersNorExits`,
  `TestARecoveredLatchIsNeverRecomputed`,
  `TestARefusalThatLeavesTheEngineNormalIsReplannedAtTheNextDecision`,
  `TestAWithheldSessionKeepsTheReplanForTheNextDecidedOne`,
  `TestARunRecoveredFromARefusedDecisionReplansIt`,
  `TestJournalIsByteIdenticalOnRerunAndReplays`.
