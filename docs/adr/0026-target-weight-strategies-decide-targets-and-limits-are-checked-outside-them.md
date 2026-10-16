# ADR 0026: Target-weight strategies decide targets; orders and limits are decided outside them

- Status: Accepted (2026-10-16, by the owner)
- Date: 2026-10-15
- Relates to: ADR 0004, ADR 0012, ADR 0015, ADR 0016, ADR 0017, ADR 0019 (both amendments), ADR 0020, ADR 0022, ADR 0025

## In plain English

*A summary. Where it differs from the text below, the text governs.*

The live strategies say what share of an account each holding should be. They
send nothing. A separate step turns shares into orders, and a gate checks each
order against hard limits before it is journalled for sending. The adapter
checks again. A problem stops new risk; sales that reduce exposure continue,
as ADR 0019 allows.

## Context

ADR 0025's Consequences require this ADR before live strategy code. `docs/live-trading/plan.md`
makes it a Phase 2 deliverable (section 9) and asks it to settle the
risk-reducing definition (section 3).

The rules to reproduce are in `plan.md` section 2 and its sources:
- momentum, combination: `research/edge-search/momentum/defensive_v2.py` as
  configured by `F_COMBO` (`research/edge-search/build_variants.py:26-27,145-146`);
- bond sleeve and glide path: `research/edge-search/momentum/glide_path.py:59-88,177`;
- Boost: `research/edge-search/boost/template.py:33-37,52-55,89-100`.

These rebalancers have no Campaigns, Units or stops, which the Turtle reducer
(`internal/strategy`) is built on (AGENTS.md rule 4). Parts of the event
contract are Turtle-shaped too (Consequences).

## Decision

### 1. A target-weight strategy module decides targets, and only targets

- New domain package (name indicative): `internal/targetweight`, implementing
  the existing `replay.Handler` (`internal/replay/engine.go:30`). It shares no
  state with `internal/strategy`. Each account runs one configuration
  (`plan.md` section 1).
- **Inputs only:** completed bars, the universe snapshot, the account
  snapshot, the session calendar and its configuration. It never reads the
  broker, the network or the wall clock; `make lint` already enforces this in
  `internal/` (`.golangci.yml`, `forbidigo` and `depguard`).
- **Output.** At each decision time it emits one decision per account, new
  type **`strategy.targets.decided`** (new). It carries:
  - the account, the decision time and the session whose closes it used;
  - an outcome, `decided` or `withheld`; `withheld` carries a reason from a
    closed set (for example `missing-bars`, `missing-universe`,
    `stale-account-snapshot`) and no targets;
  - for `decided`, the target weight per instrument. Where both sleeves hold
    one fund, the payload records each sleeve's share and their sum (`plan.md`
    lines 77-78);
  - the figures behind it, so the journal alone can check it: momentum's
    risk-off inputs and each ranked name's score, volatility, sector and cap
    outcome; the glide path's age and *b*; Boost's RSI(2), both means and its
    latched state before and after.
- **Decision times** come from the calendar input and schedule configuration,
  never a clock: momentum per `plan.md` lines 44-45, Boost per lines 148-151.
- **Universe.** New input **`market.universe.snapshot`** (new): the listing
  and classification rows, one session's unadjusted close and volume per
  listed stock (D8, `plan.md` lines 233-256), and each stock's sector label
  with its source. The module ranks and filters (`defensive_v2.py:174-176`)
  and records the membership. Ranking is a rule, so it lives in Go. The Turtle
  `strategy.universe.eligibility` (ADR 0009) is a different rule, not reused.
- **Rules** are the backtested rules as `plan.md` section 2 restates them,
  cited in source comments to that section and the research lines above. Two
  clarifications from the sources:
  - Boost trades only when its state changes (`template.py:97-100`); drift is
    not traded.
  - Weights use split- and dividend-adjusted closes; the planner (point 2)
    uses raw closes, which are what trades (ADR 0004).
- **Floating point:** every product feeding a sum is rounded first
  (`docs/development.md`, "never leave a multiply-add fusible"), including the
  inverse-volatility weights and standard deviation (`defensive_v2.py:76-79,290-291`).

### 2. A strategy-independent order planner turns targets into orders

New domain package (name indicative): `internal/orderplan`. It reads a
`strategy.targets.decided` decision, the account snapshot and configuration,
and emits one new decision per order, **`strategy.order.planned`** (new). It
knows no strategy rule.

**Valuation basis.** Net liquidation *V*, holdings *qᵢ* and raw closes *pᵢ*
are as of the previous close, as in ADR 0020 and the backtest's 08:00
rebalance (`defensive_v2.py:167`). Current weight wᵢ = qᵢ·pᵢ / V.

**Momentum and bond book, in this order** (`defensive_v2.py:383-405`,
`glide_path.py:451-473`):
1. Take the union of held and targeted instruments, in ascending instrument ID.
2. Held, with no target: sell the whole holding.
3. Not held, with a target Tᵢ > 0: buy Tᵢ·V / pᵢ, rounded per D7.
4. Held, with a target: trade only if |wᵢ − Tᵢ| > `REBALANCE_BAND`·Tᵢ
   (strictly greater). Then trade (Tᵢ·V − qᵢ·pᵢ) / pᵢ, rounded toward zero.
5. A sell never exceeds the broker-reported holding minus every working or pending sell (including `cancel-pending` ones, which may still execute)
   (ADR 0019, amended 2026-09-24, "working sell quantity never exceeds the
   holding").
6. Sells come first, in instrument order. Buys follow, in instrument order.

**IRA execution** (`plan.md` lines 369-377). Buys are planned only after
every sell has a terminal fill or lifecycle report, as a second decision
caused by the last of them. **If the plan has no sells** (an account holding
only cash, say), the buy phase is planned at once, in the same decision.
- **Budget.** Buys are funded from cash on hand plus cash realised by the
  sells; which cash counts is D5's answer, in configuration. From that, the
  cash already committed to working buys is subtracted.
- **Worst-case cost.** Each buy is costed at its limit price, the band's edge
  (point 3), not the previous close, plus the configured maximum commission
  and fees. Buys worth $980 at the previous close cost $1,029 at a 5% band
  before fees, and it is the $1,029 that must fit.
- **Scaling.** If the worst-case total exceeds the budget, every buy is
  scaled by min(1, budget / worst-case cost) and rounded again. This scaling
  is **new**, not in the backtest, and goes into the parity rerun
  (`momentum/execution.py`, `EXEC_DELAY`).
- **Carried forward.** The remaining budget is updated as each buy is
  released and as fills arrive (a partial fill releases the unspent part
  only when the rest is cancelled). The limit gate (point 3) checks each buy
  against what remains, not against the start.
- A sell that fails leaves a shortfall that is never borrowed, and the owner
  is alerted.

**Boost** (`template.py:96-100`). On a state change only, one SPY order of
want·V / p − q whole shares, rounded toward zero, where want is 0.98 or
0.98 × `BOOST_SIZE`, for the next open (`plan.md` lines 148-151). The
**first** decision has no state in force (the research starts with
`last_want = None`, `template.py`), so it always produces the order to the
first target, normally 98% from cash, whether or not a boost is on.

**D7, whole shares, open: both options specified.** The choice is
configuration (point 6):
- **D7-F, fractional:** quantity = value / pᵢ, rounded down to IBKR's
  fractional increment, itself configuration. Quantities become exact
  decimals, not `int64`. That needs the schema changes in Consequences.
- **D7-L, minimum lot:** whole shares. Before weighting, a pick whose one
  share at pᵢ exceeds its target value is skipped and the next eligible
  ranked name, within the sector cap, takes its place (`plan.md` lines
  380-383). This changes the picks, so it is a rule inside the momentum module
  and is recorded in its decision. The planner then rounds toward zero to
  whole shares.

**Order identity** (`plan.md` lines 358-368). Each planned order carries:
- a tag: the ID of the `strategy.targets.decided` envelope it serves, or, for
  a D6 sale, the incident decision. This is the same join
  `OrderLifecyclePayload.Tag` makes today;
- a client order ID derived deterministically from the tag, the instrument and
  the phase (sell or buy).

IBKR's order ID arrives only in lifecycle reports (ADR 0022), never derived.
Resends are prevented by journal-before-send and reconnect matching (`plan.md`
lines 358-368), not by the planner.

**Order type and price.** Each order is a limit order at the price band's edge
(point 3): marketable, but a fill can be no worse than the band.

### 3. Hard limits are checked outside the strategy, in Go, and again in the adapter

New domain package (name indicative): `internal/risklimit`. Each
`strategy.order.planned` gets **`risk.order.released`** (new) or
**`risk.order.refused`** (new, closed reason set). Only a released order, once
durably journalled, may be sent (ADR 0019, amended 2026-09-24, "the state
survives a restart"). The adapter checks the same values from the same hashed
configuration before calling IBKR (ADR 0025 Consequences). "Per decision time"
replaces `plan.md`'s "per run", since a live run spans many decisions.

| Limit (source) | Check | On breach |
| --- | --- | --- |
| IRA exposure ≤ 100%, never margin (`plan.md` 333-334) | after the order, Σ qᵢ′·pᵢ / V ≤ 1.00, and buy cost ≤ cash available under D5 | refuse; urgent alert; account **Degraded** |
| Joint account 150%, pre-trade (`plan.md` 336-341) | applies only to an order that **increases** exposure: refused if the exposure it would leave, valued at the previous close, exceeds 150%. A sell is never refused by this check, even when the account stays above 150% after it (160% to 155% is allowed). Held exposure is not capped, and nothing is sold because of drift. | refuse the increasing order; urgent alert; **Degraded** |
| Maximum orders per decision time (`plan.md` 334; ADR 0025) | count of planned orders ≤ N (configuration) | refuse the whole plan's buys; alert; **Degraded** |
| Maximum order size as a share of V (`plan.md` 334-335) | \|qty·pᵢ\| / V ≤ the per-account limit | refuse that order; alert; **Degraded** |
| Price band (`plan.md` 335; ADR 0025) | pᵢ within the band of the latest recorded IBKR mark (an input, never a live read) | refuse; alert; **Degraded** |
| No order without a journalled decision (ADR 0025) | the tag names a durable `strategy.targets.decided` or incident decision in this run | refuse; urgent alert; **Halted** (a defect in the order path, so its scope cannot be bounded) |
| Kill switch (`plan.md` 405) | a recorded `operator.kill-switch` input (new) is engaged | refuse every order except ADR 0019's one sale back to 98% in a margin account (its 2026-10-15 and 2026-10-16 amendments), which still goes; routine risk-reducing sells, including the IRAs', are refused; alert once; state unchanged (owner decision 2026-10-16) |
| Mode (`plan.md` 406-407) | `dry-run`: released orders are journalled with `send: false` and never sent | not a breach |

Degraded and Halted are emitted through the existing `strategy.engine.state`.
A new reason, `risk-limit-breached`, joins its closed set, as
`internal/event/engine_state.go:14-19` invites. ADR 0019's
`EngineStatePayload` version 2 adds the `degraded` state and the scope; it is
not built yet. Leaving either state follows ADR 0019's recovery procedure,
never automatically.

Values are set per account so **every order in the parity backtests
passes** (Boost's step to 147% is about 49% of V; a risk-off month moves about
half an IRA into IEF). A value that refuses a backtested order is a
configuration error.

**Risk-reducing orders** (point 4) are exempt from the order count, the order
size and the price band. They stay subject to the journalled-decision check,
the no-short sizing in point 2.5, and the kill switch, which refuses them
all except ADR 0019's one sale back to 98% in a margin account.

### 4. What a risk-reducing order is

This ADR adopts `plan.md` section 3's definition (lines 193-195), tightened
by one condition: **a sell that moves the account's exposure toward 98% or
lower, sized within the broker-reported holding net of every working or pending sell.**
Examples: Boost's exit, a momentum or bond sale, the urgent Excess Liquidity
sale (`plan.md` lines 350-351). No buy is risk-reducing, even within a
rebalance that also sells.

Per state, under ADR 0019 as amended:
- **Normal:** every released order.
- **Degraded:** no buys. Risk-reducing sells continue for instruments the
  discrepancy does not name. Named instruments are frozen. Sells are sized
  from the most recent reconciliation's broker quantity (ADR 0019, amended
  2026-09-24). The stop-restoration exception has no counterpart. If the named
  instrument is the one Boost holds, the margin account still gets ADR
  0019's one sale back to 98% when Boost's exit fires or the margin alarm
  is urgent, sized and checked exactly as in Halted (ADR 0019, amended
  2026-10-16; owner decision 2026-10-16). Every other order on it stays
  frozen.
- **Halted:** no order, except the one sale ADR 0019's 2026-10-15 amendment
  allows a margin account running a target-weight strategy: when the
  strategy's exit fires or the margin alarm is urgent, one sell-only order per
  instrument, sized and checked exactly as that amendment states, attempted
  once per incident. This ADR follows that amendment and adds nothing to it.
  It does not apply to the IRAs.

**The urgent Excess Liquidity sale in Normal and Degraded** is defined here,
not by ADR 0019, whose 2026-10-15 amendment covers only Halted and changes
nothing else. It uses the same sizing rule, adopted by this ADR for these two
states: one sell-only order bringing exposure toward 98% of net liquidation
and never below, its quantity the smaller of what reaches 98% at the mark of
one IBKR account snapshot (shares kept rounded up) and the broker-reported
holding minus every working or pending sell (including `cancel-pending` ones, which may still execute). When the
instrument to sell is the one a Degraded account's discrepancy names, the
sale is ADR 0019's one sale, sized and checked as that ADR states (amended
2026-10-16).

### 5. State recovered from the journal on restart, never recomputed

A restart reads these from the durable journal, never from a fresh window of
IBKR bars:
- Boost's latched state (`template.py:89-94`), from the last
  `strategy.targets.decided`; its indicators by replaying the journal's own
  bars from the configured warm-up start.
- The book in force for each account, with *b* and each sleeve's share.
- Every planned, released and sent order, and the IRA phase (sells sent, buys
  pending). A restart mid-rebalance sends nothing for that decision time until
  the owner clears it (`plan.md` line 332).
- The engine state and its scope (ADR 0019, amended 2026-09-24).
- Whether the incident sale has been attempted (ADR 0019, 2026-10-15).
- The day-trade count, under the legacy regime (`plan.md` lines 393-404).

**Missing bars fail closed:**
- A missing final bar for SPY, IEF or SHY at a decision time gives `withheld`
  (`missing-bars`). Nothing is traded and the owner is alerted. Boost never
  computes RSI across a gap (`plan.md` lines 152-155).
- A universe stock missing any session in its 253-close window is left out of
  that month's ranking, and the decision records it.
- The backtest silently keeps the last book when its data is short
  (`defensive_v2.py:269-275,319-320`) and treats SPY as risk-on with fewer than
  200 closes (279). Live withholds the decision instead.

### 6. Configuration and its hash (ADR 0016)

A new payload, **`strategy.target-weight.configuration`** (new), opens every
run and is hashed by ADR 0016's derivation. It holds:
- **Strategy:** `mom_trend`, `TOP_N` 25, `RISK_OFF_FRACTION` 0.5,
  `SECTOR_CAP` 3, `INV_VOL` true, `REBALANCE_BAND` 0.25, `TREND_DAYS` 200, the
  21/252/253-close lookbacks, 98% invested, IEF as the risk-off asset, a
  universe of 500, the $5 floor; Boost's `BOOST_SIZE` 1.5, `RSI_ENTRY` 10,
  `TREND_DAYS` 200, `EXIT_SMA` 5, and the indicator warm-up start.
- **Glide path:** the schedule's anchors (`plan.md` lines 94-99), retirement
  at 65, the bond split IEF 0.5 / SHY 0.5 (D2), and the owner's birth year
  and month (a configured value, `YYYY-MM`; the schedule counts age in whole
  months, so the day is never needed and never stored; owner decision
  2026-10-16),
  which lives only in server configuration (`plan.md` lines 109-110).
- **Execution:** the D7 choice and its parameters; the D5 cash rule; the day-trading regime.
- **Limits:** every value in point 3, and the Excess Liquidity thresholds of
  40%, 20% and 30 minutes.
- **Identity:** the account, the mode, the adapter name `ibkr` (ADR 0025,
  point 3), the sector-label source and the universe data source.

Any change is a new hash and a new run, never silent. A rule change also bumps
the module's own rules version and decision corpus (ADR 0016;
`docs/development.md`, "The decision corpus").

### 7. Tests required before code is accepted (AGENTS.md rule 1)

1. **Golden cases from QuantConnect.** The recorded runs (F_COMBO_R4
   24bdb0a9, OOS_F_COMBO_R4 dd45f8ec) report only summary statistics. So a
   logging copy of each research file, with additions only and a test that
   every original line is kept (as `glide_path.py:9-10` does), is run free.
   It records each month's universe, scores, picks, weights, risk-off flag and
   *b*, and each day's Boost state. Goldens are transcribed from it: a
   risk-off month, a binding sector cap, a name under 253 closes, and a Boost
   entry, exit and next-day re-entry, and Boost's first decision from cash
   (the 98% purchase with no boost on).
2. **Parity** (`plan.md` section 6, item 2). Feed the Go modules QuantConnect's
   own universe and adjusted closes. Picks and their order must match exactly.
   Weights must match within a stated tolerance, because numpy's summation
   differs from Go's. Boost's state must match day by day. Repeat with each D7
   option and with the IRA buy delay.
3. **Planner:** the band at exactly 25% and past it; entries and exits; IEF
   summed across sleeves; sells first; a plan with no sells planning its buys
   at once; buys costed at the band edge plus fees; buy scaling and the
   budget carried through releases and partial fills; both D7 roundings; no
   sale beyond the holding net of working and pending sells, a `cancel-pending`
   sell included.
4. **Limits:** each at, below and above its value; the exemptions; a sell
   from 160% to 155% passing the 150% check; the kill switch refusing a routine IRA sell and passing ADR 0019's sale back to 98%; the adapter
   refusing the same orders; every backtested order passing.
5. **States:** no buy while Degraded; named instruments frozen; Halted sends
   only the ADR 0019 (2026-10-15) sale, with that amendment's tests (lines 271-273).
6. **Replay:** byte-identical decisions, refusals and state changes; a restart
   while boosted neither enters nor exits; a missing bar gives `withheld`.

### 8. What remains open, and how each enters

| Open | Enters as |
| --- | --- |
| D5: IRA cash basis, Lite, day-trading regime (`plan.md` 445-457) | execution configuration values; no code path changes |
| D7: fractional shares or minimum lot (`plan.md` 491-492) | configuration choosing D7-F or D7-L; both are built and tested |
| Sector-label source (`plan.md` 280-290; `docs/research/sector-labels-2026-10.md`) | a field of `market.universe.snapshot` rows, with the source named in configuration |
| Live universe data source (`plan.md` 247-253) | the producer of `market.universe.snapshot`; the module sees only the input |
| A Degraded joint account whose named instrument is SPY while boosted | **decided 2026-10-16:** the one sale back to 98% applies, by ADR 0019's 2026-10-16 amendment (point 4) |
| The kill switch and risk-reducing sales | **decided 2026-10-16:** only ADR 0019's one sale back to 98% in a margin account goes through it; every other order stops (point 3) |
| One live engine per account, or one engine for all three behind one gateway (ADR 0025 allows one adapter connection per engine) | **decided 2026-10-16: one engine per account**, each with its own adapter connection (its own client ID) to one IB Gateway, its own configuration, hash, journal and engine state. Market data both IRAs share is fetched once by a separate data step and recorded as the input each engine reads, so IBKR's request limits are not spent twice. A fault, Degraded or Halted state, or an upgrade touches one account only. No limit spans accounts. Depends on one IBKR username reaching all three accounts; if it cannot, each account gets its own gateway |
| The owner's birth date enters the journal through the configuration, though it stays out of the repository | **decided 2026-10-16:** only the birth year and month, as a configured value (point 6) |
| Backtest behaviours live trading will not copy: keeping the last book on short data, SPY treated as risk-on with fewer than 200 closes, a lookback window that shifts across a gap (`defensive_v2.py:269-291`); ties in the ranking (323); a zero volatility weighted as 1 (76); RSI(2)'s dependence on its warm-up seed (`template.py:54-55`) | measured by the parity tests, which report each difference; none is copied into live code without a decision |
| Excess Liquidity thresholds: `plan.md` says "their own hash"; this ADR puts them in the account configuration's one hash | corrected in `plan.md` on acceptance (2026-10-16): part of the account configuration's one hash |

## Alternatives rejected

- **Extend the Turtle reducer.** It would blend source rules with these rules
  (rule 4), and its state is Campaigns and stops.
- **Let the strategy emit orders.** It would put account size, rounding and
  cash timing into every rule, and make limits a matter of trusting each
  module.
- **Limits in the adapter only.** ADR 0025 requires Go to check too.
- **Cap the joint account's held exposure at 150%.** That would sell in the
  dips Boost buys, which is not the backtested rule (`plan.md` lines 338-341).
- **Recompute Boost's state from recent bars on restart.** A restart could
  then enter or leave a boost that the run never decided.

## Consequences

- **New event types**, each needing a schema and tests under ADR 0015:
  `strategy.target-weight.configuration`, `strategy.targets.decided`,
  `strategy.order.planned`, `risk.order.released`, `risk.order.refused`,
  `market.universe.snapshot` and `operator.kill-switch`. They also need an
  account snapshot carrying positions and net liquidation, as a new version of
  `account.snapshot` or as ADR 0019's proposed `account.reconciliation`, and a
  recorded IBKR mark.
- **Existing types to change**, each in its own version bump:
  - `OrderLifecyclePayload`: `StopPrice` is required and positive today
    (`internal/event/order_lifecycle.go:77-80`). It becomes optional for limit
    orders, and `Quantity` (`int64`) becomes a decimal under D7-F;
  - `execution.fill` (`internal/event/fill.go`): `CampaignID` becomes
    optional, a target-weight kind joins `entry`, `stop`, `exit`, `add`, and
    under D7-F `FillPayload.Quantity` (`int64` today) becomes a decimal too,
    with a test of a fractional partial fill;
  - `market.bar.completed` (`internal/event/bar.go:127-130`): a split- and
    dividend-adjusted view beside `split_adjusted` and `raw`;
  - `event.ConfigurationHash`: accepts the new configuration payload, not
    only `event.ConfigurationPayload`.
- **Journal.** Live use needs a durable, streamed journal (#151). ADR 0017's
  one-pass, in-memory write reopens for live runs.
- No code exists yet; Phase 2 implements this, test first.
