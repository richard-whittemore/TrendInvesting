# ADR 0026: Target-weight strategies decide targets; orders and limits are decided outside them

- Status: Accepted (2026-10-16, by the owner; amended 2026-10-17: readings and planner choices (#352, #357); amended 2026-10-17: the dry-run synthetic terminal report, point 3 "Mode"; amended 2026-10-18 by the owner: parity thresholds for Boost's indicators, point 7 item 2, and names with no sector label, point 8; amended 2026-10-18 by the owner: a held stock with a gap in its window, point 5; amended 2026-10-18 by the owner: Boost re-plans after a refusal that leaves the engine Normal, point 2 "Boost"; amended 2026-10-18 by the owner: D7-L keeps a held name one lot short, point 2 "D7-L"; amended 2026-10-18 by the owner: the remaining parity scope, points 7 item 2 and 8; amended 2026-10-18 by the owner: a sell that cannot complete, point 2 "IRA execution"; amended 2026-10-18 by the owner: three bounded IRA causes, point 8; amended 2026-10-18 by the owner: Boost skips a session with no SPY bar, point 5, and re-plans an order that ends unfilled, point 2 "Boost"; amended 2026-10-22, owner-approved Boost-to-paper plan: a dry-run Boost run records a synthetic cancel for each unsent release, point 3 "Mode" (rules version 1.22.0); amended 2026-10-22 by the owner: an order the adapter can never send is retired, point 2 "Boost" and point 5 (rules version 1.23.0; a live sale a venue has named survives a restart, rules version 1.24.0; a sale that had ended survives a restart, rules version 1.25.0); amended 2026-10-29: a missing IRA mark withholds one pick and pages (rules version 1.29.0; strategy.targets.decided payload v10; market.mark.unavailable v1))
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
amendment of that date); and after its last order ends with shares
unfilled, or is rejected by the venue, once, and two of its orders
rejected in a row stop its buys, never a sell, until the owner starts a
fresh run (see
"Amendment (2026-10-18, owner decision, #487 and #488)").

**D7, share quantities.** The choice is configuration (point 6). The owner
selected D7-F for the IRAs on 2026-10-29; its engine implementation is stated
in the 2026-10-30 amendment below:
- **D7-F, fractional:** quantity = value / pᵢ, rounded down to IBKR's
  fractional increment, itself configuration. Quantities become exact
  decimals, rather than whole-share counts. The engine uses fixed-point
  integer ten-thousandths with an explicit scale, in `execution.fill` v6 and
  venue `execution.order.lifecycle` v4 (synthetic v5). Earlier v5 fills and
  v2 lifecycle reports retain their whole-share meaning (ADR 0015).
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
| Mode (`plan.md` 406-407) | `dry-run`: released orders are journalled with `send: false` and never sent. Amended 2026-10-17 (owner): since no venue ever reports on an unsent order, a dry-run momentum run records, at the release, its own **synthetic** terminal report for each order it releases: `execution.order.lifecycle` version 3, `synthetic: true`, status `canceled`, no venue order ID, a message saying it was never sent (target-weight rules version 1.11.0, ADR 0016). It is a decision, never an input (a version-3 report arriving as an input is refused), and it counts no execution: no fill, proceeds, commission or position change follows from it. It ends the order as a venue's cancellation would, so the buy phase (point 2, "IRA execution") is planned at the same close and the next rebalance is not withheld as `rebalance-in-progress`. Paper and live never record one. Boost records none: a dry-run Boost sell stays pending in the gate, so a later exit in the same dry-run is sized net of it, and so are the broker-holding check and ADR 0019's sale back to 98%. Rules version 1.11.0 leaves Boost as it was. Rules version 1.18.0: no dry-run Boost order is listed as awaiting the venue's word (`boost.undecided_orders`), since none will come; each dry-run Boost decision records `boost.no_venue: "dry-run"` instead (point 2, "Boost", amended 2026-10-18). Amended 2026-10-22 (owner-approved Boost-to-paper plan): under every rules version from 1.11.0 through 1.21.0 Boost recorded none, as this row says; from rules version 1.22.0 a dry-run Boost run records a synthetic cancel for each unsent release and refuses a venue report on one: see "Amendment (2026-10-22, owner-approved Boost-to-paper plan)" | not a breach |

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
  computes RSI across a gap (`plan.md` lines 152-155). *(Amended
  2026-10-18: Boost skips the session and its readings continue at the next
  bar; see "Amendment (2026-10-18, owner decision, #487 and #488)".)*
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
| D7: fractional shares or minimum lot (`plan.md` 491-492) | D7-L built; D7-F engine built by the 2026-10-30 amendment below; adapter and surfaces follow separately |
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

## Amendment (2026-10-18, owner decision, #487 and #488): a session with no SPY bar, and an order that ends unfilled

Accepted on 2026-10-18: the owner's decisions on #487 and #488 ("use the
recommendation"), with three readings settled by the orchestrator and
flagged to the owner, who can veto them before merge. Each is marked below. It amends
point 2, "Boost", and point 5, "Missing bars fail closed", for Boost.

### A session with no SPY bar is skipped

**What happened before.** A session that closed without SPY's bar was
withheld (`missing-bars`), and so was every later session of the run, since
the indicators were never computed across a gap. The exit included: one
missing bar stopped Boost until a fresh run the owner approved.

**The rule now.**

- The session with no bar is withheld (`missing-bars`) and nothing is
  traded. The latch and the indicators do not move: no close is added for
  it.
- At the next session with a bar, RSI(2), the 200-session mean and the
  5-session mean continue over the closes received, as if the missing
  session were not in the series, and that session decides as any other.
  The latch moves on its readings, and the exit can fire.
- The decision that reads the first bar after the gap records the missing
  sessions (`boost.gap_sessions`, YYYY-MM-DD, ascending, each before the
  decision's session). A gap is recorded by the first decision that reads
  a bar after it, unless a quiet session with a bar comes between them
  (a restarted run's warm-up sessions, which only feed the indicators): a
  gap during the warm-up, or just before a restart's recovered decision,
  is skipped the same way and recorded by that first decision.
- The reporter pages once per gap, however many sessions in a row are
  missing, naming the first and saying that decisions continue.

**Who decided what.** The owner accepted the recommendation that a missing
bar must not freeze the run (2026-10-18). Skipping the session, rather than
filling its close forward as LEAN does, is the orchestrator's reading, put
to the owner and awaiting his confirmation. **It is not a copy of the
backtest.** The owner's recommendation said "as the backtest does"; it does
not. The Boost template subscribes to SPY with LEAN's default fill-forward
(`template.py:48`). LEAN puts a filled-forward bar into `data.Bars` and into
every consolidator, so into `SMA` and `RSI` alike (LEAN's
`TimeSliceFactory` and `AlgorithmManager`, checked on 2026-10-18): on a
session its data lacked, the backtest would read the previous close again,
update all three indicators with it (no change for RSI, a repeated value in
both means) and decide on it, possibly turning the boost on or off and
trading. That decides on a price nobody traded. A live gap is a failure of
the data job or the vendor, not a session SPY did not trade, so live trading
skips the session instead (the orchestrator's reading of "skipped, not
filled", flagged to the owner). QuantConnect's SPY daily data has no missing
session in the recorded runs, so the two rules never differed there, and
no golden case can come from those logs: the golden case for this rule is
synthetic and hand-computed (`TestSyntheticGoldenBoostGap`).

### An order that ends with shares unfilled is re-planned

**What happened before.** A Boost DAY limit order that expired or was
cancelled with part unfilled left the account short of its target until the
target next changed; point 2 said nothing of a partial fill.

**The rule now.**

- When the last order a decided session released is canceled (which
  includes a DAY order's expiry) with fewer shares filled than released,
  the next decided session plans the remaining difference to the target in
  force through the same re-plan as a refusal (the amendment of 2026-10-18
  above): want·V / p − q whole shares, rounded toward zero, from the
  account snapshot as of that close.
- `part-filled`: more than none filled. This is the owner's decision on
  #488.
- `unfilled`: none filled; the whole difference is planned. **This extends
  the owner's part-fill decision; confirmed by the owner on 2026-10-18**:
  without it, one share filled of 39 would be re-planned and none of 39
  would not.
- `rejected`: the venue refused the order before it rested (`invalid`).
  Its reason may last (a permission, a margin requirement, a contract
  IBKR will not trade), so the re-plan is bounded: the next decided session
  re-plans it **once**, through the same re-plan as a refusal, recording the
  cause `rejected`. A re-planned order that is instead canceled
  part-filled is re-planned as any other. The bound counts orders the venue
  rejected, not decisions: when the gate refuses the one re-plan (the kill
  switch, a stale broker snapshot) and a later decided session releases it,
  that order is still the rejection's one re-plan, and a restart from the
  decision that recorded the refusal recovers it as such. The one re-plan
  is marked only while the target in force is unchanged: an order planned
  to a new target is a new order, so a rejection's re-plan pending when the
  target changes does not mark the order to the new target, and a restart
  from a decision recording it reads it the same way, by the target in
  force before that decision and the target it decided.
- **Two rejections in a row stop Boost's buys.** A target that changes
  at each session (the latch flipping) would give every order its own one
  re-plan, so a venue that rejects for a lasting reason would be sent an
  order at every session. The bound is therefore also independent of the
  target: when the venue has rejected **2** of the account's orders in a
  row (the orders decided sessions released, each counted when the
  venue's first word on it is read, however late; the sale
  back to 98% is ADR 0019's one attempt and not counted), whatever their
  targets, the decision of the session at whose close the rejection that
  makes it two is read records it (`boost.replan_withheld`, the order and
  its shares), whichever order it was: a late rejection of an older order
  can make it two while a later order is watched, working or
  accepted, and is recorded all the same. The reporter pages it once
  (urgent), and **no session buys again**, a
  change of target included, until the owner starts a fresh run he
  approves (`operator approve`, with `-clear-incident` when the run also
  ended in an incident; ADR 0027). The stop never blocks an order that
  reduces exposure (point 4's exemption): Boost's exit, and any sell a
  decided session plans, is still planned, checked by the gate and sent
  while the stop is in force, and an exit the gate refuses still falls
  back to ADR 0019's one sale back to 98%. Such a sell may itself be
  rejected: each rejection read after the stop, of any of the account's
  orders, is recorded once (`boost.rejected_while_stopped`, the order and
  its shares, with the count of such rejections since the stop in
  `boost.rejections_while_stopped`) and paged once as another rejection
  while buys are stopped, naming which one since the stop it is and the
  two rejections that stopped buys, and the sell is planned again at every decided session while it is due. Once
  reached, the count stays at 2, never above it, until the fresh run: a
  sell the venue accepts does not lift the stop. Before the stop, an
  order the venue accepted sets the count back to none: any execution on
  it, or any lifecycle report but `submitted` (IBKR's `PreSubmitted` maps
  to `submitted` and can precede a rejection), whether or not a later
  decided session's order has replaced it as the watched order. An order
  the venue has shown neither accepted nor rejected counts nothing until
  it does: a late rejection is one more in a row, or, once buys are
  stopped, one more while stopped; a late acceptance sets the count back
  to none before the stop, but only when the order accepted was released
  after the latest released of the rejections counted, and changes nothing
  after the stop. An acceptance of an order released before those
  rejections, read late, is older than the run in release order and does
  not end it: otherwise an older order's expiry read between a rejection
  and its re-plan's rejection would give the rejected order a third send.
  So a re-planned order rejected again is always the second in a row, and
  this bound holds it too. Every decision records the state the rule needs, as it stands
  after the decision, and nothing beyond it, so nothing is trimmed: the
  rejections in a row (`boost.venue_rejections`, omitted when none); the
  orders they name (`boost.rejected_in_a_row`, each client order ID and,
  when a lifecycle report or a broker snapshot's open-order listing named
  it, the venue order ID, exactly as many
  as the count, so at the stop the two rejections that stopped buys,
  which stay named until the fresh run and are named in the page); and
  every order decided sessions released that the venue has not yet ruled
  on (`boost.undecided_orders`, in release order, each but the watched one
  with its side, shares and limit as released and its venue order ID once
  a lifecycle report or a broker snapshot's open-order listing under its
  client order ID named it), and how many of those were released before
  the latest rejection counted (`boost.undecided_before_rejection`, omitted
  when none), which an acceptance is compared with. A run restarted from the decision restores
  these exactly and counts as the full run does: a late verdict on any
  order released before the restart is read against the orders not yet
  ruled on, an order the venue has ruled on is never counted again, the
  venue's reports on an order still undecided are read, and a working sell
  among them netted, as on an order the restarted run released. Which
  orders stay listed, and when they leave: an order the venue received
  leaves at its first word beyond `submitted` (an execution, its expiry or
  cancellation, or a rejection; an order the venue has only named as
  submitted stays listed), and a DAY order always gets one, so in paper and
  live the list holds only orders still working or whose word is late. An
  order the adapter never sent gets no word: it stays listed, in every
  decision, until the owner settles it. The owner settles it with `relay.py
  resolve --never-sent` and then records `operator rebalance-cleared` naming
  the decision that released it and the order; Boost reads that clearance
  for this alone, refuses one that names anything but an undecided order of
  that decision, or an order the venue has already named (a venue order ID
  from a `submitted` report, or an open-order listing under its client
  order ID in a broker snapshot: it was sent), and takes the order off the
  list, counting it neither accepted nor rejected. The clearance changes
  nothing else: the order stays released in the gate, so a never-sent sell
  still counts as being sold and a never-sent buy is not planned again,
  until the fresh run the owner starts after reconciling. A venue report
  on a cleared order (a fill, or any lifecycle report) proves the
  clearance wrong: the report is refused, which stops the run for a person
  to reconcile, as a later report on a cleared IRA order does (fail
  closed). Each decision
  records the cleared orders with their release
  (`boost.cleared_orders`, omitted when none), so a run restarted from it
  restores them and counts as the full run does. A dry-run's orders reach no venue, so none is
  ever listed: each dry-run Boost decision records `boost.no_venue:
  "dry-run"` in place of the list and counts no verdict, and the clearance
  is refused there, there being nothing to clear. An order the venue
  accepted (an execution, or any word beyond `submitted`) that is neither
  filled in full nor ended when a later order replaces it as the watched
  one stays recorded, with its release, executions and venue order ID, in
  `boost.working_orders` (omitted when none), so a run restarted from the
  decision reads the venue's later fills and cancel of it and nets a
  working sell as on an order it released; a dry-run decision lists none.
  Its venue order ID is recorded once a lifecycle report or a broker
  snapshot's open-order listing names it, even when the venue's first word
  on it was an execution, so the restarted run matches IBKR's listing of a
  working sell with no client order ID echoed. A run restarted from a
  decision whose watched order the venue accepted and has not ended (it
  is neither undecided nor cleared) restores it as working too, so once a
  later order replaces it the decision records it as the full run's does;
  an order replaced as the watched one is listed after the other working
  orders.
  A fresh run starts from none. A refusal's re-plan pending at the second rejection is
  dropped, and the rejection is recorded all the same. One read during a
  restarted run's warm-up, whose sessions only feed the indicators, is
  recorded by the first decision after it. The limit, 2, is a constant of
  the target-weight module (`maxVenueRejections`), not configuration. While
  buys are stopped the page tells the owner that Boost's exit and the
  margin alarm's sale back to 98% (ADR 0019) still go, and that before any
  sale by hand he follows the hand-sale procedure of the operator's manual
  (`docs/manual/states-and-kill-switch.md`): stop new orders first (the
  kill switch where it exists, otherwise the engine), cancel the working
  program sale in IBKR's app, confirm that none remains, and only then
  sell, since the exit is planned again at each decided close while it is
  due. (The orchestrator's reading of review findings, confirmed by the
  owner on 2026-10-18 with the `unfilled` extension and the one re-plan of
  a rejection; the skipped session remains put to the owner.)
- An order filled in full plans nothing more, and a re-plan it set is
  cleared if the rest of its fills arrive before the session that would
  plan it: re-planning then would trade drift toward an unchanged target.
- **What the watch is.** The last order a decided session released is
  watched until it has filled in full, or the venue has ended it and a
  decided session has planned after that end. A decided session that plans
  no order, or whose order is refused, does not end the watch: the venue's
  cancel of a DAY order can be recorded after the close of its own expiry
  session (two producers race), and a decision that ordered nothing sized
  nothing. Only a later decided session's **released** order replaces the
  watch: that order was sized from the account as it stood, and re-planning
  the earlier one after it would trade drift, or buy twice while the later
  buy still works. The sale back to 98% is never watched: its one attempt
  is ADR 0019's.
- The engine reads the watched order at each session's close, not at the
  terminal report, so a fill the venue reports after the cancellation still
  counts, at every close until the session that plans the re-plan, whose
  decision records the shares filled as they then stand.
- `boost.replan_pending` is set from that close until the session that
  plans it; a withheld session carries it. Every decision that enters with
  a re-plan pending records its cause (`boost.replan`): `refused`, or
  `part-filled`, `unfilled` or `rejected` with the order's client order ID
  and its filled and released shares.
- **Restarts.** Every decision records the watched order as it stands after
  it (`boost.watched`: client order ID, side, released shares, limit, shares
  filled so far and each execution (`fills`: fill ID, shares, price,
  commission, time), its terminal status once reported and when it
  occurred (`ended_at`), whether it is a rejection's one re-plan, and its
  venue order ID (`venue_order_id`, from the first lifecycle report or
  broker snapshot's open-order listing that names it, omitted before
  one)). A
  run restarted from that decision watches the order still and reads the
  venue's fill and lifecycle reports on it as on an order it released,
  adding it to any order a broker snapshot read before the recovery
  released. An execution the decision holds, sent again by the venue, is a
  redelivery that changes nothing, and the same fill ID for a different
  execution is refused, as on an order this run released (ADR 0019,
  "Explained means"); an execution the earlier run recorded after its last
  decision is not in it, so the venue's report of it counts once. A watched
  **sell** is restored as a working sell (ADR 0019, amended 2026-10-15:
  every exit and sale back to 98% is sized net of every working sell),
  released before every snapshot of the restarted run, filled by its
  recorded executions as stamped and ended at its recorded end, so a later
  exit cannot sell its shares again. A broker snapshot read before the
  recovery that listed it, as a sell the gate did not then know, is read as
  its listing, so it is counted once. IBKR does not always echo the client
  order ID on its listing, so the listing is matched in this order:
  1. by its client order ID and instrument;
  2. failing that, with no client order ID echoed, by the venue order ID
     the decision recorded, for the listings before the recovery and every
     one after it. The same holds for a sell this run released: the venue's
     lifecycle report names its venue order ID, and a listing under it with
     no client order ID is its own. This needs the producer to write the
     same IBKR identifier (the `permId`) as a lifecycle report's `order_id`
     and a listing's `broker_order_id`; where they differ, nothing matches
     and the sell is counted twice, which sells less, never more.

  Any other listing with no client order ID is counted beside the
  restored sell until a client order ID or venue order ID shows they are
  one order: a brief over-count, which sells less, the pending rule's
  fail-closed direction, ending at the first listing that echoes the
  restored sell's client order ID under the listing's venue order ID, or
  the first lifecycle report naming that venue order ID: either records it
  as the sell's, so every later listing under it with no client order ID
  echoed is the sell's own. A listing or report under another venue order
  ID keeps both counted. A run restarted from a withheld decision recovers the pending re-plan's cause; one restarted from a
  decided decision whose own order was refused recovers `refused`, or
  `rejected` when that order was a rejection's one re-plan: the decision
  entered with `rejected` and decided the target in force before it
  (`target_in_force`), so an order refused at a change of target recovers
  `refused`, as the full run reads it.
- **Which start reads a recorded decision.** Neither of the engine's two
  starts feeds the handler a recorded decision today. A **resume** after a
  crash (ADR 0027) re-applies every input of the crashed log, so the order,
  its executions and its reports are read again in order and the watch is
  rebuilt exactly; an execution the venue sends again after the resume is a
  redelivery. A **fresh run** (CONTEXT.md, "Fresh run": any start that is
  not a resume, started from the owner's approval) watches no order of the
  earlier run, refuses the venue's reports on one as on any order it did
  not release (reconciliation's to explain, ADR 0019),
  and its first decided session plans to the target from the account as it
  stands. Nothing in the engine makes a fresh run wait for the earlier
  run's DAY order to expire: the runbook asks the owner to start one after a
  session's end and a reconciliation (docs/runbooks/durable-log.md, "Start
  a fresh run after an earlier one"). The recovery from a decided record
  is the handler's input contract for a restart from a decision (point 5),
  tested as such.

### Rules and schema

A journal holding a gap or such an order decides differently under an
earlier build: target-weight rules version **1.18.0** (ADR 0016).
`strategy.targets.decided` moves to schema version **6** for the new
Boost fields, `replan`, `replan_withheld`, `watched` (with its
`venue_order_id`), `venue_rejections` (with `rejected_in_a_row`,
`undecided_orders`, `undecided_before_rejection`, `cleared_orders`,
`no_venue`, and `rejected_while_stopped` and `rejections_while_stopped`
after the stop), `working_orders` (the accepted orders still working
beside the watched one, each with its executions) and `gap_sessions`, each omitted when unset; the refused re-plan now records its cause too. The synthetic cases are the
golden cases for both rules (`TestSyntheticGoldenBoostGap`;
`TestAPartFilledOrderIsReplannedAtTheNextDecision` and
`TestAnExpiredUnfilledOrderIsReplannedInFull`,
`TestACancelRecordedAfterItsExpirySessionIsStillReplanned`,
`TestAPendingPartFillReplanReadsTheLatestFills`,
`TestAVenueRejectionIsReplannedOnceThenWithheld` and
`TestTwoRejectionsInARowAwaitTheOwnerAcrossATargetChange`, whose quantities are
computed by hand in their comments; the gap case is built so that skipping
and LEAN's fill-forward give a different RSI(2) and different means). The parity tool's Boost comparison is
unchanged: QuantConnect's exports have neither a gap nor a partial fill.

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

To point 1 ("Inputs only": the account snapshot), point 2 (orders valued at
the previous close) and point 3 (the price band). The owner decided on
2026-10-18 that the account snapshot a session is decided from may be the
IBKR adapter's one reading taken after that close, when ADR 0019's
conditions (amended 2026-10-18) hold: read after the close on its own New
York date, no order working or pending at IBKR for the account, no
execution at or since the close, and the holdings and cash those of the
previous close's book adjusted by the fills since (the cash within 1% of
net liquidation, recorded as `cash_drift`; the previous close's book is
found back through the chain of runs, or is the owner's starting book),
which catches a corporate action, a cash posting or another client's trade
that no list shows. The adapter
then stamps its `account.snapshot` with the close (the broker snapshot
carries the true read time beside it) and values it at the close: its net
liquidation is the total cash plus each holding times the delivered bar's
official close, the same basis as the previous close the planner prices
from, so Boost's `want × V / p − q`, the 150% exposure limit and the order
size limit take one price basis; IBKR's own net liquidation at the read is
recorded beside it on the broker snapshot (`read_net_liquidation`). When a
condition fails the adapter stamps the true read time, IBKR's own reading,
and the strategy withholds the session as `stale-account-snapshot`, as this
point already requires of a snapshot taken after the close.

The `market.mark.recorded` is the delivered bar's official close, the price
the backtest marks at, at schema version 2 with IBKR's last price beside it
(`broker_last`). **The price band (point 3) compares a checked order's
reference price with IBKR's last price when IBKR gave one, and with the
official close otherwise**, one rule for every checked order, Boost's and
momentum's alike, and the order's figures record which
(`mark_source`: `broker-last` or `official-close`). The reference price is
the same official close, so without IBKR's price the band compares the close
with itself and can refuse nothing: the band has teeth only when IBKR
provides a price, and then the limit price is not the only bound on the
fill. This is the orchestrator's reading of the owner's decision, put to the
owner. Nothing is sized from IBKR's price, so no decision needs IBKR
market-data permissions. Target-weight rules version 1.19.0 reads the new
versions of the broker snapshot and the mark, so a journal holding either
does not replay under 1.18.0; with a version 2 mark the band compares as
above, and decisions are otherwise unchanged.

## Amendment (2026-10-18, the orchestrator's reading, put to the owner): the operator inbox has a writer of its own

This paragraph records the reading the orchestrator took of the kill-switch
amendment's identity model, for the owner to confirm or correct; it is not yet
an owner decision. An instruction in the operator inbox moves the account's
kill switch, so whoever can write the inbox can release it. The writer is
therefore an identity of its own, `ti-operator` (a system user with no login
shell, whose group is the engine's, `ti-trader`), and each account's inbox is
`ti-operator:ti-trader`, mode `0750`: the engine reads it through its group
and cannot write it. The operator tool writes each record `0640` in the
inbox's group and refuses to run as any other user.

The engine refuses to start, and says which, when the inbox is missing; when
its group or others can write it; when others can read it (the records are
readable only through the group, so a wider mode hides a group mismatch);
when root or the engine's own user owns it, or the engine runs as root; when
the engine's user is not in the inbox's group; or when it cannot list the
inbox or open a record in it. A kill switch the engine cannot read while it
decides is the failure to refuse (fail closed, ADR 0019). What the engine
notes about a record (delivered, refused) it writes in a folder of its own
beside the inbox, `<inbox>-notes`, mode `0700`, which it also refuses when
others can write it, when another user owns it, or when it cannot list it.
Only that folder settles a record. A `.delivered` or `.refused` file in the
inbox itself settles nothing, since whoever writes the inbox could otherwise
withhold an instruction as well as write one; the engine reads only the
records there. A record the earlier layout delivered (it wrote its markers
beside each record in the inbox) is kept from being recorded twice by the
durable log's committed input (ADR 0027): its ID is in a log of the `-log`
directory. While it runs, an inbox the engine cannot list, or a record in it
the engine cannot open, stops the run for a person, as a notes folder it
cannot list does: the start-up check cannot see a record placed later.

No unit that reads data from outside runs as `ti-operator`. The adapter, the
data job, the reporter and the dashboard run as `ti-trader` but cannot reach
the inbox (each unit's `InaccessiblePaths=`) and can write nothing under the
engines' state directory, so none can write an instruction.

## Amendment (2026-10-22, owner-approved Boost-to-paper plan): a dry-run Boost run records a synthetic cancel for each unsent release

Approved by the owner as part of the Boost-to-paper plan, which keeps one
dry-run rule for both strategies. It amends point 3, "Mode" (the limits
table), and is target-weight rules version 1.22.0 (ADR 0016).

- **Before.** Under every rules version from 1.11.0 through 1.21.0, a
  dry-run momentum run recorded a synthetic terminal report for each order
  it released and Boost recorded none: a dry-run Boost sell stayed pending
  in the gate, so a later exit in the same dry-run, the broker-holding check
  and ADR 0019's sale back to 98% were all sized net of a sell no venue
  ever received.
- **The rule.** A dry-run Boost run records, at the release, the same
  synthetic report a dry-run momentum run records for each order it
  releases, the sale back to 98% included: `execution.order.lifecycle`
  version 3, `synthetic: true`, status `canceled`, no venue order ID, a
  message saying it was never sent. It is a decision, never an input, and
  counts no execution. The gate ends the order at its release, so a later
  exit and the broker-holding check are sized from the account snapshot,
  and the sale back to 98% from the broker snapshot by its own rule (point
  4; ADR 0019, amended 2026-10-15), each with no unsent sell netted, as
  paper sizes them. A released sale still consumes its trigger's one
  attempt (point 5), so a cancelled, unsent sale is not sent again at the
  next broker snapshot.
- **No dry-run order is watched.** `boost.watched` stays empty in a dry-run,
  since no order of it can execute, so its synthetic end re-plans nothing
  (point 2, "Boost", amended 2026-10-18), and none is listed as awaiting the
  venue's word (`boost.no_venue: "dry-run"`, rules version 1.18.0).
- **A venue report on a dry-run order is refused.** No venue received the
  order, so a fill or a venue lifecycle report naming one can only be a
  defect upstream: the run refuses it, the refusal journalled with the input
  and naming the order, and nothing is read from it, so no decision changes
  (fail closed, AGENTS.md).
- **What does not change.** Paper and live never record a synthetic report
  and read the venue's reports as before. A dry-run's account is not frozen:
  each decision still reads the latest validated account and broker
  snapshots, so a recorded corporate action or deposit still changes what
  it sizes from; only the run's own unsent orders change nothing.
- **Replay.** A dry-run Boost journal decides differently from one written
  under 1.21.0, so a journal written under 1.21.0 does not replay under
  1.22.0; paper and live journals are unaffected.

## Amendment (2026-10-22, owner): an order the adapter can never send is retired

The owner's decision of 2026-10-22, part of the Boost-to-paper plan. It
amends point 2, "Boost" (the re-plan of the watched order), and point 5
(a rebalance in progress), and is target-weight rules version 1.23.0 (ADR
0016); a decision's record of the run's own live sale back to 98% a venue
has named or part filled, below, is 1.24.0, and its record of that sale
once it has ended or filled in full is 1.25.0.

- **Before.** A released order the IBKR adapter's paper sender refused
  before any write-ahead mark, for good, stopped the adapter at that order
  until the owner settled it as never sent. The engine was never told: the
  order stayed released and awaiting the venue's word, so a never-sent Boost
  sell kept counting as being sold, a never-sent buy was never planned
  again, and a momentum rebalance stayed in progress until the owner's
  clearance.
- **The input.** `execution.order.retired` version 1
  (`OrderRetiredPayload`): the order as released, a reason from a closed
  set, what refused it or why, who retired it and when. It is a refusal of
  an order that never left the adapter, never a cancel of an order at the
  venue, so it is a type of its own, not a lifecycle status.
- **Its session is over.** An order released for a session that has ended
  before the adapter could send it is never sent (point 2, "Order type and
  price": a DAY limit order for its own session). The adapter retires it
  itself, reason `session over`, retired by `adapter`, after the same check
  at IBKR as any order (an order IBKR shows under its tag was sent, and is
  never retired), and goes on. The owner is told by a routine notice, never
  a page.
- **IBKR can never accept it as it stands.** Not a limit order, a fraction
  of a share, or no IBKR contract ID: the adapter stops at that order, the
  owner is paged urgently (the order goes unacknowledged), and the owner,
  once the cause is found, retires it with the relay's retire command,
  reason `owner retired`, retired by the owner named, owner only. Any other
  refusal is not retired.
- **The rule.** The engine reads a retirement only for an order of its
  account it released, as released, that no venue has reported on and the
  owner has not cleared (in Boost: that no venue has named, by a report, an
  execution or a broker snapshot's open-order listing). Every session-over
  retirement, and every owner's retirement, ends the order with nothing
  filled, in every strategy, the sale back to 98% included. In Boost an
  order among those awaiting the venue's word leaves them, counted neither
  accepted nor rejected; any other order Boost released, the sale back to
  98% and any order released outside a decided session among them, is
  retired by the same rule without that count. The gate no longer counts a
  retired sell as being sold, and the watched order so retired is
  re-planned at the next decided session as `unfilled`. A retired sale back
  to 98% sent nothing, so ADR 0019's readings of 2026-10-17 count it as no
  attempt (reading 4): the sale is attempted again under its own rule at
  the next opportunity, its tag and trigger unchanged, as reading 1 retries
  a sale withheld from a snapshot that could not size it (the next readable,
  fresh, positive broker snapshot while what fired it is in force, or its
  trigger's next firing), and a fresh run's approval does not state it
  attempted. Each attempt after a retired one is its own order, with a
  client order ID of its own (point 2, "Order identity": the sale's phase
  numbered by attempt), so a venue report naming the retired one is never
  read as the new one's. In a momentum run the order is terminal by the
  shared report rule; a retired sell ends the rebalance's buy phase, which
  is never started, since it would buy from the decision's targets, prices
  and cash after a session that sold nothing it planned, and once the
  rebalance's orders are all terminal the next decision decides afresh. A
  venue report on a retired order proves the retirement wrong and is
  refused. Each Boost decision records the orders retired
  (`retired_orders`, `strategy.targets.decided` version 7), a retired sale
  back to 98% with its tag and the number of the attempt retired
  (`sale_tag`, `sale_attempt`), and the watched, undecided and working
  orders a venue has named (`named_orders`), so a run restarted from it
  refuses such a report, and the retirement of an order a venue named, as
  the full run does, and gives the next attempt of a retired sale a client
  order ID of its own (point 5: recovered, never recomputed). A fresh run's
  approval carries how many of the incident's sales were retired
  (`operator.run.approved` version 4, ADR 0027), counted only from
  retirements the engine applied, by the engine's one rule for applying a
  retirement, which the reporter and the restart reading call too, so its
  next attempt follows them too; each decision records the sale attempt
  as the run holds it at the decision (`restored_sale`): the one the fresh
  run's approval restored or, with none restored, that of the incident in
  force on Boost's instrument, approved or not: attempted once the approval
  or the run itself attempted it and no retirement has since freed it, with
  the approval's retired count, the run's own retirements being recorded
  among the retired orders, so a run restarted from one restores it as the
  run held it and never attempts the sale again under a live attempt's
  client order ID. It is read when the decision is recorded, after the
  session's own orders and the sale their refusal fires, so a decision
  states an attempt its own session made. A sale the run attempted itself
  and released that is still live (not ended and not filled in full) is
  recorded with its order (`restored_sale.order`, `strategy.targets.decided`
  version 8) whether or not a venue has named it and whether or not part
  of it has filled: its client order ID, side, shares, limit, trigger and
  attempt number, whether a venue has named it (a lifecycle report, an
  execution or a broker snapshot's open-order listing), its venue order ID
  once one is known, and its executions so far. A run restarted from the
  decision holds it as that run's own attempt, released, filled as far as
  it was, so only its remaining shares are counted as being sold and an
  execution sent again is a redelivery, and named as it was, so the
  venue's next acknowledgement, listing, execution, cancel or expiry of it
  is read as in the full run, its retirement is refused once a venue has
  named it and frees the attempt while none has, and no second attempt is
  made while it works (rules version 1.24.0). A decision recorded at
  version 7 recorded the sale only while no venue had named it and nothing
  had filled, and is read as written; one carrying a field of version 8 is
  refused. An attempt withheld for a person stays spent. A sale the run
  attempted itself and released that has ended (canceled, expired or
  invalid, part filled or not) or filled in full is recorded with its
  order too (`strategy.targets.decided` version 9, rules version 1.25.0)
  once a venue order ID is known for it: its client order ID, side,
  shares, limit, trigger, attempt, naming, venue order ID, its executions
  (fill IDs, shares, prices, commissions and times), and, when a venue
  ended it, how (`status`, canceled or invalid) and the second of the
  earliest such report (`ended_at`), as the gate holds it. A run restarted
  from the decision restores it as that run's own spent attempt, released
  and ended as it was, never attempted again, so every later report on it
  is read as in the full run: an execution reported after its cancel (ADR
  0022) counts once, an execution sent again, of a part-filled sale or one
  filled in full, is a redelivery by its fill ID (ADR 0019, "Explained
  means"), a later lifecycle report is read, and an execution taking the
  sale past its released shares, or reusing a fill ID for another
  execution, is refused as in the full run, with the full run's words. The
  full run reads such reports for as long as the gate holds the order,
  which is the rest of the run, and the decision records the sale for as
  long as it records the incident's sale attempt, which is the rest of the
  run too (an incident is never left within a run: ADR 0019, "Leaving
  Degraded or Halted is never automatic"): ADR 0022 sets no shorter window for a fill after a cancel,
  so none is invented here. The record is bounded all the same: an attempt
  that ended is spent, so a decision carries at most one such sale, the
  incident's. A report giving another order that sale's venue order ID is
  refused on arrival, as for a live sale. A decision recorded at version 8
  recorded the sale only while it was live and is read as written; one
  carrying an ended sale is refused. Stated limit: a sale that ended with
  no venue order ID known (in practice, one filled in full by executions
  alone, never acknowledged or listed) is still recorded as spent, without
  its order, so after a restart from such a decision a later report on it
  is refused, where the full run reads it, and the run stops for a person
  to reconcile.
  A retirement frees only an attempt this run, or the run whose decision
  it was restarted from, made: never one the approval restored as an
  earlier run's. Stated limit: a recovered decision states only the
  attempts made by the time it was recorded. One made before the incident
  began restores no attempt of it, so the restarted run refuses a snapshot
  that would fire the recorded incident's sale until its first bar or
  session, as below, and holds the sale as not attempted after it. One
  made during the incident records its sale, attempted or not
  (`restored_sale`), which states the incident's attempts, so such a
  snapshot is accepted. A restart from a decision recorded before the
  run's later attempt does not know that attempt; such a restart is not
  used by the production start, which always begins a fresh run from an
  owner approval that carries the attempt count (ADR 0027; "Which start
  reads a recorded decision", above). A recovered
  decision comes before any broker snapshot that would fire the sale of an
  incident an earlier run's recorded engine state restored: such a
  snapshot read before the approval, before a recovered decision that
  restored that incident's attempt, and before the run's first bar or
  session, is refused, since only the approval or such a decision states
  that incident's attempts. It is refused on what it would fire, before it
  is observed, so the refusal changes no alarm and no retry, and a later
  snapshot fires what it would have (ADR 0027, section 6). A
  retired urgent sale is attempted again while the alarm stays urgent
  whatever its tag: one fired while Halted, Degraded on the instrument or
  under the kill switch carries the incident's or the engagement's tag. A
  dry-run sends nothing, so it refuses every retirement.
- **The adapter.** The owner's `retire` command is the only settlement of
  an order IBKR can never accept: `resolve --never-sent`, which tells the
  engine nothing, is refused for it. A retirement the adapter or the command
  began and a stop left unfinished is finished when the adapter starts
  again, before the order is judged sendable, so an order the engine is
  told was never sent is never sent. A retirement recorded for the input
  awaiting its reply counts as begun: one recorded for another input under
  the same client order ID is not this order's. A retirement recorded with
  no input is read as this order's unless a later one for another input
  follows it, so an order is never sent on a record that cannot say whose
  it is; when that record's entry has already reached the engine, the
  engine is told again of this input's release, under an entry named after
  the order and the input, and the retirement is recorded for this input,
  so the engine ends the order and the record says whose it was. Found
  again after a stop under either name, that entry is never queued twice.
  It is queued only while the adapter holds its lock, only once IBKR shows
  nothing under the order's tag, and only from a record that reads as a
  retirement the adapter writes: one of the two reasons, a detail, and who
  retired it fitting the reason; anything else stops the adapter. IBKR
  showing the order's tag wins on every path: the adapter records it at
  the broker (`at-broker`, the decision and then the mark) first, and only
  then strikes off every retirement of the order still queued in the
  outbox, under either name, and stops for `resolve --sent`; a stop part
  way leaves the order at the broker, and the next start, or the owner's
  `--sent`, strikes off whatever is still queued first, so no retirement
  of an order IBKR shows ever reaches the engine. Stated
  limit: an entry named after the order alone that already reached the
  engine is struck from the outbox, so the adapter cannot tell whether the
  engine applied it to this order; it tells the engine of this input's
  release again, and an engine that already retired the order refuses the
  second retirement, as it refuses every retirement of an order retired
  already, rather than end an order twice. The owner checks that the
  engine's log holds the order's first retirement and that IBKR shows
  nothing under its tag, as the manual's redelivery chapter says.
- **Replay.** Rules version 1.22.0 refused the input, so a journal holding
  a retirement decides differently and does not replay across the two.


## Amendment (2026-10-29): a missing IRA mark withholds one pick and pages

The owner selected option C on 2026-10-29: an IRA pick whose buy has no
recorded mark is withheld for that month, its unbought weight stays in cash,
and an urgent page names it. The account is not degraded by this missing
mark. Every other limit breach retains its existing state transition.

The following is the orchestrator's reading of that decision, put to the
owner for review with this implementation:

- **Point 2, IRA execution:** keep the declared target weights and include
  the withheld buy in the same budget scaling calculation, so its allocation
  is never redistributed to the other picks. Record the withheld picks when
  deciding the month, before either execution phase. A later successful mark
  does not restore that pick during the month's buy phase or create a
  mid-month catch-up. For a held name being increased, only the unbought
  increase stays in cash; no existing holding is sold merely for this reason.
- **Point 3, limits:** an IRA buy without a mark is refused as `missing-mark`
  without a Degraded transition. Other buys in the decision proceed through
  every other gate check. Another breach in the same decision still degrades;
  the exception never clears an existing Degraded/Halted state. Boost's gate
  behavior is unchanged.
- **Points 1 and 5, evidence and recovery:** `strategy.targets.decided` payload
  version 10 adds `withheld_picks`, naming each instrument, the closed reason
  `missing-mark`, failure classification and exact adapter detail. Targets
  remain the intended weights. Readers retain explicit support for payload
  versions 7–9 without inventing withheld entries (ADR 0015). Rules version
  1.29.0 declares the changed behavior (ADR 0016). The new input
  `market.mark.unavailable` version 1 journals the adapter's instrument, as-of
  time, source, detail, and `transient` or `final` failure. Without recorded
  evidence a decision says `unknown`; it never infers an IBKR refusal.
- **Re-ask before withholding:** a lookup that does not end is asked twice
  within its bounded reading attempt. On each later attempt to read the
  account that morning, every unresolved name is resolved again, including
  one previously refused by IBKR. Successful resolutions are cached only
  within that reading on the unchanged connection. The delivered reading
  records the remaining failures: timeout is `transient`, an IBKR refusal or
  an unresolvable/ambiguous name is `final`. No sending behavior changes.
- **Alerting:** one urgent page per account, decision month and instrument,
  `<account>: <instrument> withheld this month (missing mark:
  <transient|final> …)`, with the exact recorded detail, repeating until
  acknowledged like the withheld-rebalance page. No recorded reason means
  `unknown`. Acknowledgement stops reminders, not the withholding. A decided
  month with no withheld picks has no per-pick page.


## Amendment (2026-10-30): D7-F engine implementation reading for the owner

The owner chose fractional IRA shares on 2026-10-29. This records the
orchestrator's implementation reading put to the owner: D7-L was built;
D7-F was specified, despite the earlier D7 table saying both were built.
Target-weight rules version 1.30.0 builds the D7-F engine capital path.
The broker adapter and the general reporting surfaces remain a separate
slice; this amendment does not enable sending orders or change the paper gate.

`execution.share_quantity: "fractional"` selects D7-F.
`fractional_increment` is a decimal string, defaults to `"0.0001"`, has at
most four decimal places, is positive and no greater than one share, and
must divide one share exactly. The choice and effective increment enter
the configuration hash (ADR 0016). D7-L and Boost retain their previous
configuration encodings and hashes.

Quantities use integer ten-thousandths of a share with an explicit
`quantity_scale: 10000` in new event schema versions (ADR 0015). Thus
`quantity: 72500` is exactly 7.25 shares. Consumers decode ticks as exact
integers, never floating-point numbers. Legacy records omit the scale and
continue to mean whole shares; they are explicitly upcast before a
fractional account consumes them, with overflow checked. A whole-share
account refuses fractional schemas. A fractional count cannot contain a
fifth decimal place: the integer encoding has no smaller unit, and decimal
input parsing rejects excess precision instead of rounding it. New
fractional observations refuse negative positions. Legacy short-position
observations retain their historical interpretation.

The planner divides the monetary delta by the raw reference price using
exact decimal rational arithmetic, then rounds down to the configured
increment. It applies the same increment after budget scaling. D7-L's
minimum-lot pick substitution does not apply. The two execution phases,
sector cap, rebalance band, fees, and withheld-pick allocation are unchanged.
Book, lifecycle, outstanding sells and reconciliation compare integer ticks;
valuation multiplies exact quantity by decimal price before converting the
monetary result to the existing monetary representation. A split producing
less than one tick is unverifiable rather than truncated.

Schema versions introduced here are configuration 8, positions snapshot 5,
cash snapshot 6, fill 6, venue lifecycle 4, synthetic lifecycle 5, planned
order 3, released order 4, refused order 5, broker snapshot 3, reconciliation
input/result 2, skipped order 2, retirement 2 and target decision 11.
Earlier supported schemas remain readable under their original meanings.
The incomplete-sell page renders decimal outstanding quantities. Summary
order lines and refused-report text carry the quantity scale and render
decimal shares. The summary still refuses fractional configurations
and other fractional record types, and journal-query surfaces refuse
fractional records until their separate migration, so fixed-point counts
cannot be presented as shares.


The existing operator approval's `StartingBook` remains a whole-share
surface. Its strict decoder refuses fractional starting holdings; its
schema and operator/adapter comparison need migration before a fractional
paper account can be approved. This stays fail closed with the adapter and
surfaces slice. Venue fills, lifecycle reports and retirements must use the
same quantity scale as their released order; historical whole-share reports
are not silently reinterpreted against fractional releases.

### Per-execution rounding reserve

Owner decision, 2026-10-30: the fractional buy planner reserves explicit
rounding slack per planned buy. `execution.rounding_slack_per_order` defaults
to $0.10 and accepts finite values from $0.01 through $1.00, inclusive.
Omission and an explicit default hash identically; every changed effective
allowance changes the configuration hash (ADR 0016). D7-L and Boost retain
their earlier execution encodings and cash checks.

The basket is scaled against cash minus the sum of these allowances before
quantity rounding. Each kept buy reserves its entire quantity at the limit,
rounded up to cents, plus the order's maximum fees and its rounding slack.
The planner and independent insufficient-cash gate use the same allowance;
both journal it as `rounding_slack`. The allowance stays committed while the
order is working and only its unused portion is released at termination.
Each fractional buy fill debits its gross value rounded UP to the cent,
plus its commission added without cent rounding, without charging the
allowance again. Terminal budget spending retains this conservative estimate;
closing the order releases only the reservation above that spending. This is
distinct from reconciliation booking (ADR 0019), which rounds gross value
and commission separately to the nearest cent: a 0.1-share fill at $10.01
with no commission debits $1.01 from the budget but books $1.00.

Each execution can add less than one cent of upward rounding. Reserving one
cent per possible execution covers every quantity split through
floor(100 × rounding_slack_per_order) executions: ten at the default. For
example, four executions of 1.8125 shares at $10.10 debit $73.24 from the
budget, which fits the $73.23 whole-order reservation plus $0.10 slack. This bound assumes fills
at or below the buy limit and aggregate fees within the order's fee allowance.
It is not a structural reserve for execution counts beyond that cap or other
cash changes. The structural cash buffer remains an owner decision pending
under #622; this execution allowance does not settle that decision.
