# ADR 0026: Target-weight strategies decide targets; orders and limits are decided outside them

- Status: Accepted (2026-10-16, by the owner; amended 2026-10-17: readings and planner choices (#352, #357); amended 2026-10-17: the dry-run synthetic terminal report, point 3 "Mode"; amended 2026-10-18 by the owner: parity thresholds for Boost's indicators, point 7 item 2, and names with no sector label, point 8; amended 2026-10-18 by the owner: a held stock with a gap in its window, point 5; amended 2026-10-18 by the owner: Boost re-plans after a refusal that leaves the engine Normal, point 2 "Boost"; amended 2026-10-18 by the owner: D7-L keeps a held name one lot short, point 2 "D7-L"; amended 2026-10-18 by the owner: the remaining parity scope, points 7 item 2 and 8; amended 2026-10-18 by the owner: a sell that cannot complete, point 2 "IRA execution"; amended 2026-10-18 by the owner: three bounded IRA causes, point 8)
- Date: 2026-10-15
- Relates to: ADR 0004, ADR 0012, ADR 0015, ADR 0016, ADR 0017, ADR 0019 (amendments of 2026-09-24 to 2026-10-17), ADR 0020, ADR 0022, ADR 0025

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
    `stale-account-snapshot`) and no targets. These are the strategy
    decision's reasons. The limit gate's reasons for refusing an order or
    withholding the sale back to 98% are separate closed sets (point 3);
    in particular the gate's `stale-broker-snapshot` is not
    `stale-account-snapshot`;
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
    not traded. Amended 2026-10-18: a refusal that leaves the engine Normal
    is re-planned at the next decision (point 2).
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
- **Scaling.** *(Extended by a fallback in the 2026-10-17 amendment.)* If the worst-case total exceeds the budget, every buy is
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
Amended 2026-10-18 (owner): after a refusal that leaves the engine Normal,
the next decision also plans the difference to the target in force (see the
amendment of that date).

**D7, whole shares, open: both options specified.** The choice is
configuration (point 6):
- **D7-F, fractional:** quantity = value / pᵢ, rounded down to IBKR's
  fractional increment, itself configuration. Quantities become exact
  decimals, not `int64`. That needs the decimal quantity on the parallel
  execution-report versions (`execution.fill` v5 and
  `execution.order.lifecycle` v2, Consequences and the 2026-10-17 amendment).
- **D7-L, minimum lot:** whole shares. Before weighting, a pick whose one
  share at pᵢ exceeds its target value is skipped and the next eligible
  ranked name, within the sector cap, takes its place (`plan.md` lines
  380-383). This changes the picks, so it is a rule inside the momentum module
  and is recorded in its decision. The planner then rounds toward zero to
  whole shares.

**Order identity** (`plan.md` lines 358-368). Each planned order carries:
- a tag: the ID of the `strategy.targets.decided` envelope it serves, or, for
  a D6 sale, the incident decision, a durable `operator.kill-switch`
  engagement or a `risk.margin.alarm` decision (ADR 0019 amendment,
  2026-10-17, reading 2). This is the same join
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
| No order without a journalled decision (ADR 0025) | the tag names a durable `strategy.targets.decided`, incident, `operator.kill-switch` engagement or `risk.margin.alarm` decision in this run | refuse; urgent alert; **Halted** (a defect in the order path, so its scope cannot be bounded) |
| Broker holding (ADR 0019 amendment, 2026-10-17, reading 10) | a sell other than ADR 0019's sale back to 98% is no more than the latest broker snapshot's holding, when that snapshot is readable and fresh; with no readable latest snapshot, a Normal account releases the sell against the journal's account book, only an account Degraded on another instrument reaches this refusal, and every other state has already refused the sell under ADR 0019 reading 7 (checked first) and the exit has fired; an older readable snapshot is never used | refuse (`risk.order.refused`, reason `broker-holding`); alert; **Degraded** on the instrument when the sell is above a fresh snapshot's holding |
| Broker snapshot freshness (ADR 0019 amendment, 2026-10-17, reading 10) | a sell other than ADR 0019's sale back to 98% is checked against the latest broker snapshot, which is stale: older than `MaxSnapshotAge`, or stamped later than the sell's journalled time (point 6) | refuse (`risk.order.refused`, reason `stale-broker-snapshot`); one urgent alert per stale episode, carried by the episode's first `stale-broker-snapshot` refusal or withhold and cleared by whichever comes first: the first journalled `account.broker.snapshot` fresh by its own journalled time, or the first gate check whose latest snapshot is fresh as measured from that check's triggering event (ADR 0019 reading 10); a stale check opens an episode when no episode is open for the account and either no episode has yet closed or the check's triggering event comes after (by journal sequence) the event that closed the most recent episode; a stale check whose triggering event precedes that close belongs to the closed episode and opens none; state unchanged; the sell is re-planned at the next decision |
| Kill switch (`plan.md` 405) | a recorded `operator.kill-switch` input (new) is engaged | refuse every order except ADR 0019's one sale back to 98% in a margin account (its 2026-10-15 and 2026-10-16 amendments), which still goes; routine risk-reducing sells, including the IRAs', are refused; alert once; state unchanged (owner decision 2026-10-16) |
| Mode (`plan.md` 406-407) | `dry-run`: released orders are journalled with `send: false` and never sent. Amended 2026-10-17 (owner): since no venue ever reports on an unsent order, a dry-run momentum run records, at the release, its own **synthetic** terminal report for each order it releases: `execution.order.lifecycle` version 3, `synthetic: true`, status `canceled`, no venue order ID, a message saying it was never sent (target-weight rules version 1.11.0, ADR 0016). It is a decision, never an input (a version-3 report arriving as an input is refused), and it counts no execution: no fill, proceeds, commission or position change follows from it. It ends the order as a venue's cancellation would, so the buy phase (point 2, "IRA execution") is planned at the same close and the next rebalance is not withheld as `rebalance-in-progress`. Paper and live never record one. Boost records none: a dry-run Boost sell stays pending in the gate, so a later exit in the same dry-run is sized net of it, and so are the broker-holding check and ADR 0019's sale back to 98%. Rules version 1.11.0 leaves Boost as it was | not a breach |

Degraded and Halted are emitted through the existing `strategy.engine.state`.
A new reason, `risk-limit-breached`, joins its closed set, as
`internal/event/engine_state.go:14-19` invites.

**The gate's closed reason sets.** Replay compares each reason byte for
byte (point 7, item 6), so the members named below are exact strings, and
a reason both sets hold is spelled the same in each. Each set also holds
its other reasons, not listed here; these are the snapshot reasons.
"The latest broker snapshot" is the latest journalled
`account.broker.snapshot` for the account at or before the triggering
event's journal sequence, as ADR 0019's snapshot rule defines it
(amendment, 2026-10-17, reading 1):
- **`risk.order.refused`** for any sell other than ADR 0019's sale back to
  98% (ADR 0019 amendment, 2026-10-17, reading 10):
  - `broker-holding`: the sell is above the latest broker snapshot's
    holding; or no broker snapshot is readable yet or the latest is
    unreadable and the account is Degraded on another instrument. A
    Normal account then releases the sell against the journal's account
    book; every other state has already refused the sell under ADR 0019
    reading 7, which is checked first, and the exit has fired. An older
    readable snapshot is never used in its place;
  - `stale-broker-snapshot`: the latest broker snapshot is readable but
    stale: older than `MaxSnapshotAge`, or stamped later than the sell's
    journalled time (point 6); one urgent alert is raised per stale
    episode (ADR 0019 reading 10) and the sell is re-planned at the next
    decision.

  These sells never read net liquidation or the mark, so
  `non-positive-broker-values` is not in this set.
- **`risk.sale.withheld`** for ADR 0019's sale back to 98%, beside the
  withholds reading 4 names (values that cannot be read, an unknown working
  sell, nothing to sell):
  - `stale-broker-snapshot`: the latest broker snapshot is readable but
    older than `MaxSnapshotAge`, or stamped later than the triggering
    event's journalled time (the payload records the skew); the attempt is
    not consumed;
  - `non-positive-broker-values`: the latest broker snapshot is readable
    and fresh, but its net liquidation or its mark for the instrument is
    zero or negative; the attempt is not consumed;
  - `lapsed`: the exit or the urgent alarm that needed a sale withheld for
    one of the two reasons above is no longer in force, so the sale is
    dropped unsent; reported in the daily summary, not paged.

`stale-broker-snapshot`, in both sets, is distinct from the strategy
decision's `stale-account-snapshot` (point 1). When, how and with what
effect each is used is ADR 0019's snapshot rule (amendment, 2026-10-17,
reading 1), not restated here. ADR 0019's `EngineStatePayload` version 2
adds the `degraded` state and the scope; it is not built yet. Leaving
either state follows ADR 0019's recovery procedure, never automatically.

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
holding minus every working or pending sell (including `cancel-pending` ones, which may still execute).
*(Refined by the ADR 0019 amendment of 2026-10-17, reading 1: a third bound,
holding − every working or pending sell − keep, is added; the quantity is the
smallest of the three, never below zero and never more than the holding.
Which snapshot sizes the sale, when it is withheld instead, whether the
withhold spends the attempt and when the sale is evaluated again are that
reading's snapshot rule, stated there once.)* When the
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
- Whether the incident sale has been attempted (ADR 0019, 2026-10-15), and
  the one-attempt-per-instrument state of the kill-switch engagement and of the
  margin alarm (ADR 0019, 2026-10-17, reading 2). A released order consumes an
  attempt; a withhold consumes one only in the two cases of reading 4. A
  sale withheld without consuming its attempt is re-evaluated after a
  restart exactly as ADR 0019's snapshot rule states (reading 1), from the
  journalled withhold and the condition still in force; a journalled
  `lapsed` withhold ends it.
- Whether a stale-snapshot episode is open for the account, from its opening
  `stale-broker-snapshot` record and whichever comes first after it: the first
  journalled `account.broker.snapshot` fresh by its own journalled time, or
  the first fresh gate check (ADR 0019 reading 10), ordered by the triggering
  event's journal sequence. A stale check opens an episode when no episode is
  open for the account and either no episode has yet closed or the check's
  triggering event comes after (by journal sequence) the event that closed the
  most recent episode; a stale check whose triggering event precedes that close
  belongs to the closed episode and opens none, so a restart does
  not resend the alert.
- The day-trade count, under the legacy regime (`plan.md` lines 393-404).

**Missing bars fail closed:**
- A missing final bar for SPY, IEF or SHY at a decision time gives `withheld`
  (`missing-bars`). Nothing is traded and the owner is alerted. Boost never
  computes RSI across a gap (`plan.md` lines 152-155).
- A universe stock missing any session in its 253-close window is left out of
  that month's ranking, and the decision records it. *(Amended 2026-10-18:
  a stock the account holds is scored across the gap instead, as the
  backtest scored it; see "Amendment (2026-10-18, owner decision, #434)".)*
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
- **Execution:** the D7 choice and its parameters; the D5 cash rule; the
  day-trading regime; `MaxSnapshotAge`, the age past which the latest broker
  snapshot is stale: the sale back to 98% is withheld and any other sell is
  refused, each with `stale-broker-snapshot` (ADR 0019 amendment,
  2026-10-17, readings 1 and 10; point 3). The age is the journalled time
  of the event that triggers the check minus the snapshot's recorded broker
  timestamp, never the wall clock; a negative age is stale, with no
  tolerance. Configuration validation rejects a
  missing, zero or negative `MaxSnapshotAge`.
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
   option and with the IRA buy delay. *(The thresholds, and the owner's
   amendment of 2026-10-18 for Boost's RSI and moving averages, are in the
   amendment of that date below.)*
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
| A name with no sector label, which the backtest ranked and left uncapped (`defensive_v2.py:69-74`) and live trading had left out (`plan.md` section 4, "Sectors", "Chosen") | **decided 2026-10-18 by the owner ("I want parity"): live trading copies the backtest.** The name is ranked and can be picked, counted toward the book's 25 but toward no sector's cap of 3; it is not excluded and has no status of its own. Target-weight rules version 1.13.0. It is not a listed difference: the parity tests require the live book to equal the backtest's here |
| Three more backtest behaviours of the IRA runs: a buy held back for a newly added stock's first price; a name with no reference price, traded outside the planner; LEAN's own sale of a holding at the stock's delisting | **decided 2026-10-18 by the owner: listed, each only with its recorded evidence and bound.** `price-wait`: a newly added stock bought on the first session it had a price, within five sessions, the wait recorded in the export and verified on the calendar. `unpriced`: only an instrument before its listing date (IEF before July 2002): the export records no price for it that month, and the month comes before the first month any export records a price for it. `liquidated`: LEAN's own "Liquidate from delisting" sale, recorded as such in the orders listing, of a stock Go held (a holding at the last rebalance or a buy it kept). Anything outside those bounds is reported `unexplained` and fails the differences check (`docs/research/parity-2026-10.md`, "Round 4") |
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
  `market.universe.snapshot`, `operator.kill-switch`,
  `account.broker.snapshot`, `risk.margin.alarm`, `risk.sale.withheld` and
  `strategy.order.skipped` (this last lands with the minimum-lot slice). They also need an
  account snapshot carrying positions and net liquidation, as a new version of
  `account.snapshot` or as ADR 0019's proposed `account.reconciliation`, and a
  recorded IBKR mark.
- **Existing types to change**, each in its own version bump *(the first two
  sub-bullets, `OrderLifecyclePayload` and `execution.fill`, are superseded by
  the 2026-10-17 amendment: parallel versions, not bumps)*:
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
- *(Historical.)* No code existed when this was accepted; code exists since
  slice 2 (PR #358) and slice 5 (PR #353), and the 2026-10-17 amendment
  records what was built. Phase 2 implements the rest, test first.

## Amendment (2026-10-17, owner decision, #352 and #357): readings and planner choices

The owner accepted the following as built (slice 5, PR #353; slice 2,
PR #358). Readings 1 to 4 and 7 to 10 of the ADR 0019 amendment of the same
date (the sale back to 98%) are stated there; this amendment states the
ones that belong here.

### From #352

- **The urgent Excess Liquidity sale (point 4) goes once per urgent alarm.**
  Its client identifier derives from the alarm decision, and a new alarm
  is minted on every change of the alarm's level into urgent. An alarm
  episode runs from the level becoming urgent until it next changes level.
  Urgent, then warning, then urgent again is two alarms and may sell twice,
  even if the cushion never got back above 40%; a change of cause while
  staying urgent (ratio to no reading) is the same alarm. This applies in
  Normal, and in Degraded on another instrument, with the kill switch
  disengaged, where the sale carries the alarm's tag (ADR 0019 amendment,
  reading 2). In a Halted account, in Degraded on the instrument, or with
  the kill switch engaged, the sale carries the incident's or the
  engagement's tag, so that tag's one attempt per instrument applies and
  ADR 0019's unknown-working-sell check applies (readings 4 and 8). Under
  the alarm's tag there is no unknown-working-sell check; the sizing nets
  every working sell, so it cannot open a short.
- **Risk-reducing orders (point 3) are not counted toward N.** They are
  exempt from the order count, order size and price band, and do not add to
  the count that later orders are checked against.
- **New event types.** `account.broker.snapshot` (the single IBKR snapshot
  ADR 0019's sale is sized from), `risk.margin.alarm` and
  `risk.sale.withheld` join the safety types in Consequences beside
  `operator.kill-switch`, each under ADR 0015.

### From #357

- **Buy scaling always fits the budget (extends the Scaling rule in point
  2 with a fallback).** With `max_fees_per_order` above zero, point 2's
  scaling alone can leave a total over the budget, because the fee allowance
  does not shrink with quantity. As built (`orderplan.ScaleBuys`) the rule
  has two steps. (a) Point 2's rule: scale every buy by
  min(1, budget ÷ worst-case total), round toward zero and drop any buy that
  rounds to zero. (b) Only if the rescaled total still exceeds the budget,
  scale every buy instead by (budget − n × fee allowance) ÷
  Σ(quantity × limit price), n being the number of buys before step (a),
  floored at zero and rounded toward zero. The result fits the budget with
  worst-case fees included. A buy that scales to zero is dropped: no buy is
  planned for it, and each dropped buy is recorded as
  `strategy.order.skipped` (the type lands with the minimum-lot slice; until
  then the drop is unrecorded). It is not a withheld decision with
  a reason from point 1's closed set. The budget is not a cutoff: a budget
  at or below n × fee allowance can still release a buy (for example 100,
  1 and 1 shares at $10, a $10 allowance and a $20 budget keep one share of
  the first buy, which costs $20). With a $0 allowance step (b) is never
  needed.
- **A new rebalance is withheld while an earlier one's orders are working.**
  The decision is withheld with reason `rebalance-in-progress` while any
  order of an earlier rebalance is not terminal. It is the live form of
  point 5's "a restart mid-rebalance sends nothing until the owner clears
  it", and it waits only for orders of an earlier
  rebalance, so the planner's own earlier rebalance sells are never working at
  plan time. The planner still nets every other working or pending sell
  (point 2, step 5).
- **Execution reports for target-weight accounts are parallel versions, not
  bumps.** `execution.fill` v5 (`TargetWeightFillPayload`) and
  `execution.order.lifecycle` v2 (`LimitOrderLifecyclePayload`) sit beside
  the Turtle's fill v4 and lifecycle v1, joined by client order ID. The
  Turtle reducer still reads v1 and v4; the target-weight engine reads v2
  and v5. Both carry the D7-F decimal quantity, and
  `StopPrice` is optional for limit orders on v2. The required test of a
  fractional partial fill stays, applied to v5 and v2 and built with D7-F as
  configuration. This supersedes the `OrderLifecyclePayload` and
  `execution.fill` sub-bullets of the Consequences bullet "Existing types to
  change" as versioning (parallel versions, not bumps); their facts carry over
  to v2 and v5 as just stated. The
  `market.bar.completed` and `event.ConfigurationHash` sub-bullets stand.
- **The per-order fee allowance is $2.00**, pending confirmation in paper
  trading (`docs/research/commissions-30k-2026-10.md`): above every observed
  $30k average ($1.03 to $1.49), below the $2.50 that 500 shares would
  cost before the 1% cap lowers it.

## Amendment (2026-10-17): D7-L as built, accepted by the owner 2026-10-18

This extends the D7-L decision recorded in point 2 so the text matches the
code as built. The owner accepted it on 2026-10-18. D7-L compares only a pick
whose target weight is positive; the three cases below follow from that.

- **(a) Fully risk-off by design.** When every pick's target weight is zero
  (a risk-off month at `RiskOffFraction` 1), D7-L compares nothing and the
  decision is decided: the whole invested fraction goes to the risk-off
  instrument and the planner sells every stock held. The decision carries a
  detail saying so.
- **(b) Every positive-target pick unaffordable.** When no stock is picked and
  D7-L skipped at least one candidate as unaffordable, the decision is
  withheld (`all-unaffordable`) and the book is kept, in a risk-on month and
  a partial risk-off month alike. A systematic valuation error (a bad net
  liquidation or bad raw closes) looks the same as an account too small for
  every pick, and selling every stock on it is the wrong failure (point 5,
  fail closed). Every skipped candidate is recorded `unaffordable` with its
  one-share price and target value, the detail names the one-share lot, the
  momentum scale and the net liquidation, and the reporter sends the withhold
  as an urgent alert. A book with no pick and none skipped is withheld as
  `no-eligible-names`.
- **(c) Some picks affordable.** The book is the affordable picks after
  substitution, decided as usual.
- **What is recorded.** The `MinimumLot` figures (net liquidation V and the
  momentum sleeve's scale, 1 - b) are recorded exactly when D7-L valued the
  picks: a book decided under it, or one withheld as `all-unaffordable`. They
  stay if the run then withholds a decided book; a decision the rule
  withheld for any other reason compared no pick and records none.

## Amendment (2026-10-18, owner decision, #461, #462 and #463): parity thresholds and names with no sector

The parity run of 2026-10-18 (`docs/research/parity-2026-10.md`) failed on
three checks; the owner decided each on 2026-10-18. The recorded failures
stand as recorded (AGENTS.md rule 6).

- **Point 8: names with no sector label (#461).** QuantConnect's universe
  held names with no Morningstar sector (5.3% of the ranked names in the
  tuning months, 3.5% held out: mostly foreign ADRs and recent listings).
  `defensive_book` ranked them, uncapped (`defensive_v2.py:69-74`); live
  trading left them out (`plan.md` section 4, "Sectors", "Chosen"), so the
  live book departed from the backtest's in 29 of 30 tuning months and 26
  of 31 held out. The owner chose parity: the live rule ranks a name with
  no sector label and can pick it, counted toward the book's `TOP_N` but
  toward no sector's `SECTOR_CAP`, exactly as the backtest did. It is no
  longer left out, and a decision no longer records a `no-sector` status
  (the event schema keeps the value so earlier decisions still read). A
  rules change: target-weight rules version 1.13.0. The live labels come
  from other sources than Morningstar's (point 8, "Sector-label source"),
  so which names carry no label still differs from the backtest; that is
  measured through dry-run and paper trading (`plan.md` section 4).
  **Guard against a label outage.** Ranking an unlabelled name uncapped
  means a snapshot in which every name has lost its label (a renamed
  column, a renamed sector vocabulary, a broken source) would otherwise be
  decided as an uncapped top-25 book, with the sector cap silently absent;
  point 5 requires that to fail closed. So the momentum rule withholds a
  month, with the new reason `labels-missing` and the last book kept, when
  more than a configured share of the ranked names has no label: the
  momentum configuration's `max_unlabelled_share` (configuration schema
  version 7, ADR 0015 and 0016), set to **0.20**. The backtest's months had
  3.5% to 5.3% unlabelled, so 0.20 sits well above normal and well below an
  outage; **the figure was accepted by the owner on 2026-10-18.** The reporter sends
  `labels-missing` as an urgent alert. The market-data step also refuses a
  session whose sector file has lost its sector column, or in which no
  label from either source maps onto a Morningstar sector, rather than
  writing a snapshot with no labels, and likewise a session in which either
  source that parsed maps none of its labels. The withhold is part of rules
  version 1.13.0; the market-data refusal is a change to the encoder, which
  has no rules version and alters recordings, not decisions. Neither changes
  a month the parity run compared, since the parity tool compares the book
  selection, not the withhold.
- **Point 7, item 2: Boost's indicators (#462, #463).** The thresholds
  were fixed before the first parity run (`docs/research/parity-2026-10.md`,
  "Thresholds"), with Boost's RSI(2) and its 200-day and 5-day means
  required bit for bit. They are amended: **RSI(2) and both means must
  match within 1e-9 relative** (|Go - QuantConnect| / max(|Go|,
  |QuantConnect|)), the tolerance already set for scores and volatility;
  readiness must still match exactly, and Boost's latched state and SPY
  target exactly, every session. Rationale, as the owner recorded it:
  QuantConnect's LEAN computes the indicators in C# `decimal`, the Go
  module in float64, so bit equality is out of reach of any float64
  implementation; the largest differences found were 4.26e-12 on the
  0-100 RSI (6.1e-13 relative) and 5.68e-13 on the means (1.3e-15
  relative); the closest RSI(2) came to its entry threshold of 10 was
  0.0027, and every one of 7,166 sessions reached the same state and
  target. Matching LEAN bit for bit would mean reimplementing its
  indicator arithmetic in a decimal library, operation for operation, and
  tracking LEAN's changes; judged not worth it. No decision changed.

The thresholds of the parity gate are now:

| Check | Threshold |
| --- | --- |
| months | every planned month exported, no other |
| ranked order, cap marks | exact |
| weights | absolute difference at most 1e-12 |
| scores, volatility | relative difference at most 1e-9 |
| risk-off | flag, close and close count exact; 200-day mean within 1e-9 relative |
| history | each sampled name's status and close count exact |
| Boost RSI, means | within 1e-9 relative, readiness exact, every session (amended 2026-10-18) |
| Boost state | latched state and SPY target exact, every session |
| differences | any live departure whose cause point 8 does not list fails |

The `labels-missing` withhold is no parity check: the parity tool compares
the book the rule selects, which the withhold does not change, and the
exports' months had at most 5.3% unlabelled, under the 0.20 ceiling.

The comparison was re-run on the kept exports with no new QuantConnect run
(`docs/research/parity-2026-10.md`, "Owner decisions applied,
2026-10-18").

## Amendment (2026-10-18, owner decision): Boost re-plans after a refusal that leaves the engine Normal

Accepted by the owner on 2026-10-18. It amends point 2, "Boost", and the
clarification in point 1 that Boost trades only when its state changes.

- **The rule.** After Boost's planned order is refused and the refusal
  leaves the engine Normal, the next decided session plans the difference
  between the account's position and the target in force, want·V / p − q
  whole shares rounded toward zero as before, even though the target has not
  changed. This is consistent with ADR 0019 reading 10: a refused sell "is
  re-planned at the next decision". The refusals that leave the engine
  Normal are the kill switch and `stale-broker-snapshot` (point 3).
- **Why.** Ordering on a state change only left the account away from its
  target after such a refusal until Boost's target next changed: the kill
  switch engaged at the first decision refused the 98% entry, and after its
  release the account stayed in cash.
- **What does not change.** A refusal under Degraded or Halted, or one that
  degrades or halts the account (a limit breach, an undecided order),
  follows those states' own rules (ADR 0019, amended 2026-09-24), including
  the sale back to 98% that a refused exit fires. Drift is still not
  traded: with no refusal before it, an unchanged target plans nothing. A
  withheld session plans nothing, so the re-plan waits for the next decided
  one. If the re-planned order is refused again while the engine is still
  Normal (the switch still on), the decision after it re-plans again, so a
  switch left on records one refusal per decision. The switch still alerts
  once per engagement (point 3, limits table, kill switch: "alert once"):
  the gate flags the engagement's first refusal (`risk.order.refused`
  `alert`), as it flags a stale episode's first (ADR 0019 reading 10), and
  the reporter sends nothing for the later ones. A refused re-planned sell
  is the strategy's exit firing again (ADR 0019 reading 7), so a switch
  released and engaged again, a new engagement, may send one more sale back
  to 98% for the difference left after the first: a share or two, since the
  sale keeps ceil(0.98·V/mark) shares at the broker's mark and the planner
  sizes from the close. A sell is still net of the sells already working,
  so it never sells below the target alongside the sale back to 98%.
- **Replay and restart.** The re-plan is derived from the journal's own
  refusal, so a resumed run rebuilds it by replaying its durable log (ADR
  0027). Each Boost decision also records whether a re-plan is pending
  after it (`strategy.targets.decided` version 5, `boost.replan_pending`,
  omitted when unset; ADR 0015), so a run restarted from an earlier run's
  last decision (point 5) re-plans as that run would have; a withheld
  decision carries the pending re-plan forward. A journal holding such a
  refusal decides differently from an earlier build, so this is a rules
  change: target-weight RulesVersion 1.16.0 (ADR 0016; 1.14.0 was reserved for it and
  never released).

## Amendment (2026-10-18, owner decision, #434): a held stock with a gap in its window

Accepted by the owner on 2026-10-18 (option (b) of #434).

**What happened before.** Point 5 leaves a universe stock missing any
session of its 253-close window out of that month's ranking. A stock the
account already held was treated the same way: it got no target, so the
planner sold the whole holding (point 2, "Held, with no target"), and it
could not be bought again until its window was complete, about a year
later. One missing session, a day the stock did not trade or one bar the
vendor got wrong, forced a sale the strategy had not decided on its
merits. The decision recorded it as `short-history`, not `gap`: the run
kept exactly 253 sessions of closes, so one missing session left 252,
fewer than the score needs, and the `gap` status was in practice never
recorded.

**The rule now.**

- **A held stock is scored across its gap, as the backtest scored every
  stock.** `defensive_v2.py` read 260 sessions of history at each
  rebalance (271), dropped the missing closes, and scored each stock from
  the last 253 closes it had (286-291): the 12-1 score from two of them,
  the 22nd and the 253rd closes from the end, and the volatility from the
  252 daily returns of all 253 closes, one of which spans the gap. A stock the account holds at
  the decision, whose live window has a gap, is now scored and weighted
  exactly so (the momentum module's `ScoreHeld`, which reuses
  `BacktestScore`). It is then kept, resized or sold on its merits, like
  any other name. A held stock with fewer than 253 closes in those 260
  sessions is still `short-history` and is sold, as the backtest left it
  out too.
- **The ranking of stocks not held is unchanged.** A candidate the account
  does not hold still needs a complete 253-close window to be ranked;
  otherwise it is left out with the status `gap`.
- **The gap is recorded.** Every name with a gap names the missing sessions
  in the decision (`gap_sessions`): for a `gap` name the sessions missing
  from its 253-session window, and for a held stock scored across its gap
  the sessions missing between the first close read and the last. A name
  left out for a gap now reads `gap`, not `short-history`.
- **260 sessions are kept.** The engine keeps 260 sessions of closes (the
  backtest's history), and the compact universe input's close histories
  cover 260 sessions. The recorded close counts (`closes`,
  `trend_closes`) count closes in those 260 sessions, as the backtest's
  export did. The market-data backfill records 260 sessions by default.
- **Only a holding positively known to have sent its own bars: a compact
  run.** A score across a gap must never span a split or dividend the
  market-data step could not adjust, which would put an unadjusted price
  jump inside the window. A stock named to that step (`--optional`, every
  holding) refuses the whole stream on such an action, so a gap in its own
  bars is a session it did not trade. In the compact stream only the named
  holdings (and the fixed instruments) have bars, so a held stock with its
  own bar in the decision session is known to be named, and only it is
  scored across its gap. A held stock the engine sees only through its
  close history, because it was not named, is not known to be safe, so its
  gap leaves it out as for a stock not held. In a bars stream written with
  `--universe-members` every member has bars, named or not, and the
  market-data step leaves an unnamed member out of the ex-date session of
  an action it could not adjust, a gap the engine cannot tell from a
  session the stock did not trade; the stream marks no gap as a plain
  missing session. A bars run therefore keeps the complete-window rule for
  every name, held or not (fail closed): a held stock with a gap reads
  `gap` and is sold, as it was sold before this amendment (then recorded as
  `short-history`). The compact stream is the
  shape the IRAs run on (the input-shape decision of 2026-10-16, recorded in
  docs/manual/momentum-universe.md, "The configuration says which shape the
  run reads"), and naming every holding with `--optional` is mandatory there.

A rules change: target-weight rules version 1.15.0 (1.14.0 was reserved for the
Boost re-plan, which is 1.16.0), so a journal written under 1.13.0 does not
replay. `strategy.targets.decided` kept schema version 4 at rules 1.15.0 (the Boost
re-plan amendment above raised it to 5): `gap_sessions` is omitted when empty,
so a Boost decision's bytes were unchanged. A reader
accepts a `gap` name that names no sessions, as a record written before
`gap_sessions` existed does; the writer refuses to stamp one.

**Parity.** The live rule now departs from the backtest for a gapped name
only when the account does not hold it, which is point 8's gap departure
narrowed. The parity tool compares the live `Score` with `BacktestScore`
for each name and knows no holdings, so its results are unchanged; it was
re-run on the kept exports (`docs/research/parity-2026-10.md`, "Held stock
with a gap, 2026-10-18").

## Amendment (2026-10-18, owner decision, #437): D7-L keeps a held name one lot short

Accepted by the owner on 2026-10-18 (option 2 of #437), on condition that it
was measured once first (AGENTS.md rule 6); measured on 2026-10-18, all
five QuantConnect runs that day (`docs/research/d7l-held-name-2026-10.md`),
verdict adopt. It
amends point 2, "D7-L", and its amendment of 2026-10-17.

**What happened before.** D7-L did not look at what the account holds. A
stock that was held and still ranked into the book, but whose target value
had fallen below one share's price, was skipped like any other pick: it got
no target and the planner sold the whole holding. Had it kept its target,
the rebalance band, or the whole-share rounding of the trade it asked for,
would have left a one-share holding alone, and trimmed a holding of more
shares to one share; the kept holding is now neither sold
nor trimmed.

**The rule now.**

- **Reading of "within one lot of affordability."** A lot is one share. A
  pick is unaffordable when one share at its previous raw close, p, exceeds
  its target value t (weight x momentum scale x net liquidation). D7-L
  compares only a pick with t > 0, so the shortfall p - t is always less
  than one lot, and every held pick D7-L would skip qualifies. This is the
  reading the study measured.
- **A held pick is kept.** When D7-L would skip a pick the account holds
  (one share or more in the account snapshot as of the previous close), it
  is not skipped. It keeps its place in the book, its sector slot and its
  weight, so the other picks' weights are those of a book that holds it,
  and the substitution's fixed point is otherwise unchanged: each pass
  skips every unaffordable pick not held and keeps every unaffordable pick
  held, and the passes end when one skips nothing new.
- **No order.** The planner places no order in a kept name at that
  rebalance: no buy, no sell, no resize by the band, and no
  `strategy.order.skipped` record, since the decision itself records why.
  The next rebalance compares it afresh.
- **Recorded with its own status.** The decision records the name as
  `held-one-lot-short`, with its one-share price and target value as for an
  `unaffordable` name, and the holding it keeps: the shares
  (`held_quantity`), their value at the previous raw close (`held_value`)
  and that value as a fraction of net liquidation (`held_share`). The
  decision's detail names each kept name with the same figures, so paper
  trading measures the concentration the study could not.
- **A pick not held is skipped as before**, and a held name with no target
  (dropped from the book) is still sold in full.
- **The all-unaffordable withhold is unchanged in effect.** A kept name is
  not an affordable pick. When no pick is affordable and at least one was
  skipped or kept, the decision is withheld as `all-unaffordable` and the
  whole book is kept (amendment of 2026-10-17, case (b)). Deciding a book of
  kept names alone would sell every other stock held on a valuation that
  may be wrong. The study's instrument decided such a book instead; it never
  arose there (no month was withheld and none was kept wholly).

**Accepted concentration.** A kept holding is not resized toward its target,
so the kept holding may exceed its target by a multiple bounded by one
share's price: q shares at price p are worth q x p against a target value t
below p, so the holding exceeds its target by q x p - t, less than one
share's price for each share held (less than p itself for a one-share
holding). The study's runs at $30,000 kept 3
name-months in 10.5 held-out years and none in 17 tuning years. **This is
accepted by the owner for now**; the recorded `held_share` is how paper
trading measures it.

**Measured.** Paired same-day QuantConnect runs at $30,000 under Interactive
Brokers' fees (`docs/research/d7l-held-name-2026-10.md`): in-sample
(1999-2015) the exception never fired and the runs are identical; held out
(2016-2026-06) it kept 3 name-months, CAGR +0.02 pp, maximum drawdown 0.11
pp deeper, 12 fewer orders and $11.50 lower fees, all inside the margins
fixed before the runs.

A rules change: target-weight rules version **1.17.0**, so a journal holding
such a name does not replay under 1.16.0. `strategy.targets.decided` keeps
schema version 5: the status and the holding figures are of that version,
and the figures are omitted when unset, so a decision with no kept name
has the bytes it had before. Boost is unaffected.

**Golden cases and parity.** The golden months replay through `SelectBook`,
which applies no minimum lot, and record no holdings, so no golden month
changed. The parity tool knows no holdings either; it was re-run on the
kept exports and its three reports are byte-identical to the
`_held_gap` reports (`docs/research/parity-2026-10.md`).

## Amendment (2026-10-18, owner decision): the remaining parity scope

The parity runs of the glide path, D7-L and the IRA execution
(`docs/research/parity-2026-10.md`, "Remaining scope", and "Review fixes
and re-runs, 2026-10-18") failed only on IRA orders the backtest could not
place. The owner decided four things on 2026-10-18, and a fifth, on a sell that cannot complete, later that day. The recorded failures
stand as recorded (AGENTS.md rule 6).

- **Point 8: a stock delisted or acquired at the rebalance.** The list of
  backtest behaviours live trading does not copy gains one cause: *the
  stock was delisted or acquired at the rebalance, so the backtest could
  not trade it.* The backtest's universe still ranks such a stock from the
  session before, and the IRA planner plans an order for it that
  QuantConnect cannot fill. The parity tool lists each such order under
  that cause (`delisted`) **with the detection the export records**, never
  an inferred cause: QuantConnect refused the order (its placeholder order
  ID in the first exports, or a refusal that it gave as "non-tradable"), or
  the stock had no price within five sessions (`unplaced`). An order
  refused for any other reason still fails. The `ira-orders` check passes
  when every departure is so listed. *(As the tool applies it, after a
  second review on 2026-10-18: a placeholder order ID records only that
  QuantConnect refused, not why, so a refusal recorded by it alone fails;
  an `unplaced` buy is listed only when the export records the five
  sessions waited for a price, which the sequence check counts on the
  calendar; and each detection must name an order Go planned that the
  backtest did not fill. Every other refusal or unplaced buy fails.)*
- **Point 7, item 2: the thresholds proposed for the remaining runs are
  confirmed** as the rule: the glide path's age and bond share within
  1e-12 absolute; the IRA plan, orders and sequence exact; the IRA buy
  scaling's scale within 1e-12 absolute, its shares and skips exact.
- **The IRA buy delay** is read as point 2, "IRA execution", states it:
  the buys go only after the rebalance's sells have filled (one session
  later in the runs), at once when there is no sell. Confirmed. *(As the
  parity tool and the IRA template apply it: a buy waits until every sell
  of the run is terminal, of any month or tag, not only the rebalance's
  own; the delay applies when a sell is placed or still working at the
  rebalance.)*
- **A sell that cannot complete (decided by the owner, 2026-10-18).** The
  held-out run had a sell that never filled (CBAY, 2024-04), and no later
  month's buys were placed. A sell that cannot complete is either refused
  as non-tradable or left without a price for five sessions. The owner
  decided that such a sell is set aside as `unfinished` and does not
  block that rebalance's buys. The buys are scaled to the cash actually
  available, never counting the stuck sell's proceeds. The stuck holding is
  reported, and live trading pages the owner (built separately, with the
  live alerts). *(As the IRA template and the parity tool apply it: a
  refused sell never works, so it holds nothing; a market sell still
  working at 8:00 on the fifth session at whose 8:00 it was working (its
  own day the first when it was placed before 8:00) had no price at any
  of those opens, and from then on it holds no buy. It stays
  netted from the holding, since it may still execute (point 5). The tool
  finds it from the orders listing and the calendar, never from the
  export, and reports it as `set-aside` when it never filled and either is Go's
  planned sell of exactly its shares, or is an unpriced sale of a holding
  with no reference price that was not a pending target; any other
  set-aside sell fails. One that
  later fills in full is an ordinary sell, filled after buys it no longer
  held; one left partly filled fails `ira-orders`.)*
- **D7-F is dropped from the parity scope.** Point 7, item 2's "repeat
  with each D7 option" reads D7-L only: D7-F, fractional shares, is not
  built (point 2, "D7-L", the owner's choice of 2026-10-16). Point 8's D7
  row is unchanged: if D7-F is ever built, it enters with its own tests.

The thresholds of the remaining parity checks are now:

| Check | Threshold |
| --- | --- |
| glide age, glide share | absolute difference at most 1e-12 |
| glide weights, glide sleeves, D7-L weights | the same instruments; absolute difference at most 1e-12 |
| D7-L picks, skipped, kept | exact, a withheld month included |
| IRA plan | exact |
| IRA scale | scale within 1e-12 absolute; shares and skips exact |
| IRA orders | exact; a departure listed under point 8's causes passes |
| IRA sequence | exact |
| differences | any live departure whose cause point 8 does not list fails |

The comparison was re-run on the kept exports and on the two IRA runs
made with the fixed template (IRA_PARITY_R2, OOS_IRA_PARITY_R2); no other
QuantConnect run was made for that review. After a second review the same
day, the comparison was re-run on the kept exports again and the two IRA
runs were made once more with the template's further fixes (IRA_PARITY_R3,
OOS_IRA_PARITY_R3; `docs/research/parity-2026-10.md`, "Second review
fixes, 2026-10-18").

## Amendment (2026-10-18, owner decision): the account snapshot as of the close, and the mark

To point 1 ("Inputs only": the account snapshot) and point 2 (orders valued
at the previous close). The owner decided on 2026-10-18 that the account
snapshot a session is decided from may be the IBKR adapter's one reading
taken after that close: the account changes between the close and the next
open only through an order still working or an execution, so a reading taken
after the close, on its own New York date, with no order working or pending
at IBKR for the account and no execution since the close, is the account as
of that close, and the adapter stamps its `account.snapshot` with the close
(the broker snapshot carries the true read time beside it, ADR 0019,
amended 2026-10-18). When that condition fails the adapter stamps the true
read time, and the strategy withholds the session as
`stale-account-snapshot`, as this point already requires of a snapshot taken
after the close: no rule changes. The `market.mark.recorded` the gate checks
a buy's price band against (point 3) is the delivered bar's official close,
the price the backtest marks at, at schema version 2 with IBKR's last price
beside it as evidence only; nothing is decided from IBKR's price, so no
decision needs IBKR market-data permissions. Target-weight rules version
1.18.0 reads the new versions of the broker snapshot and the mark, so a
journal holding either does not replay under 1.17.0; decisions are
otherwise unchanged.
