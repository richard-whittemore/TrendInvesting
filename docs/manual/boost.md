# Boost: the joint account's rule, end to end

*For the owner. What the Boost rule decides, when it turns on and off, how
its one order is sized and priced, what happens after an order is refused,
what one Boost day looks like from the data to IBKR and back, and what can
go wrong.*

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
([market data](market-data.md#the-one-time-warm-up)).

The shipped dry-run file says 2024-01-02, which is outside Massive's
two-year window. The data job therefore refuses to fetch a warm-up for that
file. A run fed by the data job needs a date inside the window; that is a
different configuration, so it starts as a new run. A paper run uses a date inside Massive's
two-year window (the first-paper-order runbook, step A5, recommends
2025-01-02).

## The hard limits for this account

From `deploy/etc/engines/boost.config`, `limits` and `execution`:

| Limit | Value | In plain words |
| --- | --- | --- |
| `max_exposure` | 1.5 | A buy is refused if it would leave SPY above 150% of net liquidation, valued at the previous close (the account snapshot's net liquidation is valued at that close too: its cash plus SPY at the official close). A sell is never refused by it, and nothing is sold because the market moved the account above 150% |
| `max_orders_per_decision` | 1 | At most one order that is not risk-reducing per decision |
| `max_order_fraction` | 1 | No single order larger than 100% of net liquidation (Boost's step from 98% to 147% is about 49%) |
| `price_band` | 0.05 | The order's reference price (the raw previous close) must be within 5% of **IBKR's last price** for SPY, read after the close and recorded beside the mark (`broker_last`); when IBKR gives no price, it is compared with the mark itself (the delivered bar's official close), which is the reference price, so it always passes. The order's figures say which was used (`mark_source`: `broker-last` or `official-close`). Plainly: **the band has teeth only when IBKR provides a price**; without IBKR market data the limit price is the only bound on the fill. This is the orchestrator's reading of your decision, put to you (ADR 0026, point 3, amended 2026-10-18). Also sets the limit price above |
| `excess_liquidity_warning`, `_urgent`, `_stale_minutes` | 0.40, 0.20, 30 | The margin-cushion alarm ([states-and-kill-switch](states-and-kill-switch.md#the-margin-cushion-alarm)) |
| `execution.max_snapshot_age_minutes` | 30 | How old IBKR's latest snapshot may be when a sell is checked against it |

A **risk-reducing** sell (one that moves the account toward 98% or lower)
is exempt from the order count, the order size and the price band, so one
late mark cannot block Boost's exit (ADR 0026, points 3 and 4). The
[momentum chapter](momentum.md#the-hard-limits-in-plain-words) explains each
limit once for both strategies.

## One Boost day, from the data to IBKR and back

This is the path a normal paper day takes. Every step is built; the whole
day runs in `deploy/rehearse-paper.sh` with the real engine against a
stand-in for IB Gateway, and is still to be run against IBKR's paper
servers ([first paper order](../runbooks/first-paper-order.md)).

| # | When (New York) | What happens | Where it is explained | Built? |
| --- | --- | --- | --- | --- |
| 1 | 16:00 | The market closes (13:00 on an early close). | [market data](market-data.md) | — |
| 2 | 17:05 (retries 18:05, 20:05) | `ti-data` fetches the day's whole-market bars and recent splits and dividends from Massive, records them, and writes the engine's input for the session: SPY's bar, then "the session closed", to `/var/lib/trend-investing/data/streams/boost/<TI_RUN>/boost-<date>.jsonl`. | [market data](market-data.md), "The daily data job" | Built |
| 3 | after 17:05 | The adapter reads the file and the account at IBKR once. That reading is the account as of the close when it is taken on the close's own date with nothing working at IBKR, nothing executed since, and the holdings and cash those of the previous close adjusted by the run's fills (cash within 0.5% of net liquidation; your decision of 2026-10-18), and it is then valued at the close (cash plus SPY at the official close); otherwise it is stamped at its read time and step 4 withholds the session. It sends, numbered in the engine's one series: the reconciliation of that reading (against the engine's book, before it is replaced), the account snapshot (SPY held and net liquidation), the broker snapshot, SPY's bar, the mark (the bar's official close, IBKR's last price beside it, which the price band compares with when it is set), then "the session closed". | [IBKR adapter](ibkr-adapter.md), [paper trading](paper-trading.md#a-paper-day) | Built |
| 4 | at the input | The engine writes each input to its durable log, flushed to disk, then decides: updates RSI(2) and the averages, moves the latch, plans at most one order and checks it against the hard limits. A released order is written down before anything is told about it. | [engine](engine.md), this chapter, [states-and-kill-switch](states-and-kill-switch.md) | Built (tested against recorded inputs and `make demo-boost-dry-run`) |
| 5 | same evening | The adapter writes "about to send", hands the order to IB Gateway as a DAY limit order under its client order ID, and writes "sent". | [paper trading](paper-trading.md), [re-delivery](redelivery.md) | Built; the real IBKR client places it only in paper with sending switched on; not yet run against IBKR's paper servers |
| 6 | next session | IBKR works the order. Each status change and each fill (with IBKR's commission) comes back; the adapter records them as the engine's inputs. | [paper trading](paper-trading.md), [order lifecycle](order-lifecycle.md) | Built |
| 7 | every 5 minutes | The reporter reads the durable log and sends any alert, and the daily summary. | [alerts and dashboard](alerts-and-dashboard.md) | Built and installed; its alerts need your ntfy and PagerDuty entries in `secrets.env` ([server](server.md)) |
| 8 | at every start and reconnect of the adapter | The adapter reads IBKR's positions and cash; the engine compares them with its book. | [reconciliation](reconciliation.md) | Built |

In dry-run, step 5 never sends (every order says `send: false`); see the
[engine](engine.md), "What dry-run simulates", for what that leaves out.

## What you see in the record

Each decided or withheld session writes one `strategy.targets.decided`
record. For Boost it carries the close, RSI(2), both averages (and whether
each was ready), the latch before and after, the target in force and
whether a re-plan is pending. An order adds `strategy.order.planned`, then
`risk.order.released` or `risk.order.refused`. `journal-query orders` and
`journal-query refusals` list them ([journal-query](journal-query.md)).

## What can go wrong, and what the program does then

| What happens | What the program does | What you do |
| --- | --- | --- |
| A session closes with no SPY bar | That session is withheld (`missing-bars`), and so is **every later session of the run**: the readings are never computed across a gap (plan section 2, "Boost"). The latch is not recomputed. | Find why the bar was missing. The run cannot recover by itself: stop it and start a new run (a fresh `TI_RUN`, its own warm-up and your approval: `docs/runbooks/durable-log.md`). |
| No account snapshot as of the close, or one taken after it | The session is withheld (`stale-account-snapshot`). The latch still advances, as the backtest's does; a change of target withheld one session is ordered at the next that decides. | Check the adapter and the Gateway (`docs/runbooks/gateway-login.md`). |
| No mark at or after the close, or the previous close is more than 5% from the mark | A buy is refused (`missing-mark` or `price-band`) and the account goes Degraded on SPY. A risk-reducing sell is not checked against the mark. | `docs/runbooks/halted-account.md`. |
| The kill switch is on | Every order is refused except the sale back to 98%; the account stays Normal; the order is re-planned at the first decided close after you release the switch. | Nothing more, once released. Check it with `journal-query orders`. |
| IBKR's latest snapshot is too old when a sell is checked | The sell is refused (`stale-broker-snapshot`), no state change, one urgent alert per outage, re-planned at the next decision. | Check the Gateway. |
| The exit is refused because the account is Halted, Degraded on SPY or under the kill switch | The one sale back to 98% goes instead ([states-and-kill-switch](states-and-kill-switch.md#the-one-sale-back-to-98)). | Follow the alert; do not sell by hand while that sale is working. |
| The DAY order expires, or IBKR cancels it, with part or all unfilled | The cancellation is recorded and ends the order. Boost does **not** order the rest by itself: it plans again only when the target changes, or after a refusal. The account stays off its target until then. | Decide whether to buy or sell the rest by hand (engage the kill switch first, so nothing of the program's is working against yours), or wait for the next change of target. A fresh run would plan to the target from scratch. |
| An order is never sent (the adapter refused it for good and you settled it `--never-sent`) | The order stays released in the log. A never-sent **sell** keeps counting as being sold, so every later exit in the run is smaller, or nothing, as in dry-run. A never-sent **buy** is not re-planned (the target in force has not changed). | After settling it, reconcile and start a fresh run you approve: its first decision plans to the target from scratch ([IBKR adapter](ibkr-adapter.md)). |
| The engine crashes while the boost is on | Resumed from its log, it carries on in the same latch, neither entering nor exiting by mistake (`TestARestartWhileBoostedNeitherEntersNorExits`). | Resume it (`docs/runbooks/durable-log.md`). |
| A fresh run after a stop | The approval states the latch in force; the engine refuses an approval whose latch is not the one the previous log ended with. | `operator approve` reads it for you ([operator](operator.md)). |

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
