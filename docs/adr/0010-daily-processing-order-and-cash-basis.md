# ADR 0010: Exits are processed before entries, but entries only ever see the previous close's cash

- Status: Accepted
- Date: 2026-09-08

## Context

Under the intraday fill model (ADR 0005) a daily bar cannot say whether a stop in one instrument occurred before an entry in another. If entries could spend cash freed by same-day exits, or use cap headroom freed by same-day exits, the model would be quietly optimistic and the daily decision would depend on an unknowable ordering.

## Decision

Within a trading day, events are processed in the order **Protective Stops and Exit-Channel exits → Adds → new entries**. However, **the cash and Unit-cap headroom available to every Add and entry are those known at the previous close**. Exits in bar *t* free capital and headroom for bar *t+1*, never for bar *t*.

> **Amended by [ADR 0020](0020-sizing-uses-the-previous-close-affordability-binds-order-placement.md) (2026-09-22), for cash only.** The previous close remains the basis for **sizing** and for **Unit-cap headroom**, unchanged. For **affordability**, the cash available to a Unit is the previous close's figure *less what this bar's earlier fills have already spent*, and the check binds whether an order is placed rather than whether a recorded fill is applied — because several Units checked independently against one unmoved figure can each pass while together overspending the account. Credits still wait for the next previous close, so the sentence above continues to hold in the direction it was written to protect. ADR 0020’s 2026-09-23 cash-movement amendment also subtracts accepted withdrawals immediately, defers deposits until an eligible replacement snapshot, and preserves the original snapshot timestamp. ADR 0020’s 2026-09-24 producer amendment specifies LEAN portfolio snapshots after each bar’s decisions, supplying the next bar’s previous-close basis. Its RulesVersion 1.8.0 implementation note has `cmd/backtest` state each Session’s close from the simulated account in the same way, so in a backtest too an exit in bar *t* frees cash for bar *t+1*.

> **Amended by [ADR 0020](0020-sizing-uses-the-previous-close-affordability-binds-order-placement.md) (2026-09-24), for Unit-cap headroom and for proposals.** Under the owner's decision of 2026-09-24, every proposed entry and Add reserves its worst-case cost and one Unit of headroom under each cap until it fills, expires or is cancelled. The cash available to a Unit is therefore the previous close's figure less this bar's fill debits **and** every standing hold. The Unit-cap headroom available to it is each cap less the Units committed by fills and the Units reserved by standing holds. Exits still free nothing for the bar they happen in.

When the next Unit costs more than the available cash, the Unit is **skipped** and the rejection is journaled with reason `insufficient-cash`; there are no partial Units, no borrowing, and no deferred queue.

Simultaneous signals are ranked by Faith's mechanical strength measure — **(close − close 63 bars ago) / N**, highest first [T p.29] — with ties broken by 20-day median dollar volume and then by symbol.

> **Amended by the owner's decision of 2026-09-25 (issue #34): Strength and the dollar-volume tie-break, defined.** ADR 0021's session-close pass ranks a Session's Signals through `rankSignals`; until now neither key it sorts by was computed. This amendment settles both, so `rankSignals` can compute them rather than falling back to instrument ID alone.
>
> - **Strength.** For a Signal decided at the close of Session *d* (ADR 0021), Strength is `(close(d) − close(d−63)) / N(d)`: the split-adjusted close (ADR 0004) 63 completed Sessions apart, divided by N as Session *d*'s own close leaves it — the last completed Session, and its own N, not the pre-advance figure the same Signal's entry sizing uses (ADR 0005 needs that figure computable before Session *d* opens, since the entry can still fill inside it; ranking runs only once Session *d* has fully closed, so its own close and N are already completed facts by then).
> - **View.** Strength reads the split-adjusted view (ADR 0004), consistent with every other signal computation. The dollar-volume tie-break reads **raw** close × raw volume — dollar volume is an accounting-flavoured quantity, real money traded — the same raw-view rule ADR 0009's $5M eligibility test already needs, and the two share one definition.
> - **The dollar-volume window.** The 20 completed Sessions ending with *d*, inclusive.
> - **The median of that window.** The window is always 20 wide, so always even: the mean of the two middle values.
> - **Insufficient history.** An instrument with fewer than 64 split-adjusted closes or fewer than 20 raw closes/volumes cannot be ranked at all. Its Signal is **declined**, with a recorded reason (`event.DeclineReasonInsufficientHistory`) — never ranked last, since an incomparable instrument has no place in a total order. This is consistent with ADR 0009, whose eligibility rule already requires far more history than either window.
> - **One shared function.** `internal/indicator.MedianDollarVolume` is the one definition this tie-break and ADR 0009's eligibility test both read; `internal/indicator.Strength` is Strength's own. Implementing ADR 0009's eligibility gate itself remains open.
>
> The tie-break order is unchanged: Strength descending, then 20-day median dollar volume descending, then symbol ascending — a total order, since instrument IDs are unique.

## Consequences

- The day's decisions are reproducible from the previous-close snapshot and the recorded debit/hold events specified by ADR 0020, including cash withdrawals.
- The model is never optimistic about cash or caps.
- A partial Unit would break the Unit-as-risk-measure invariant the caps depend on; skipping preserves it.
- Sublime's sector-strength ranking is a declared Variant of the ranking rule.
- Each Signal's Strength appears in its own decision event (`strategy.trade.proposed` or `strategy.proposal.declined`), so a reviewer can check the ranking a Session's proposals and declines were actually decided by.
