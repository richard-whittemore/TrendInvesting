# ADR 0020: Sizing uses the previous close; affordability binds order placement

- Status: Accepted
- Date: 2026-09-22
- Amends: ADR 0010

## Context

ADR 0010 fixes the cash **basis**: every Add and entry decided on bar *t* measures against the cash known at *t*'s previous close, and exits in *t* free capital for *t+1* and never for *t*. That rule exists to stop the model being optimistic about an intraday sequence a daily bar cannot state.

It does not say what happens when several Units compete for that one figure, and the implementation checks each independently against it. `Reducer.availableCash` is assigned in exactly one place, from an `account.snapshot` (`internal/strategy/notional.go`), and nothing decrements it. So two shapes of the same gap exist:

- **Across instruments.** Two entries that each fit alone can be decided on the same bar and together cost more cash than the account holds.
- **Across sequential Units of one instrument.** The Add Ladder can add four Units in one day (The Turtle Rules p.19). Each rung is compared against the same unmoved figure, so four Adds can each pass while their total exceeds the cash. This shape needs no second instrument and can be constructed today.

The second is worse than it looks in a backtest. `cmd/backtest` emits a single `account.snapshot` at the start of a run, so the figure never moves at all: a whole run's spending is checked against the opening balance. `applyCashMovement` handles deposits and withdrawals for the Notional Account's arithmetic (ADR 0007) and deliberately does not touch `availableCash`, so a withdrawal leaves the reducer sizing against cash the account no longer holds until a new snapshot arrives (#106).

The per-Unit check therefore passes every time while the portfolio goes overdrawn, which is precisely the outcome `insufficient-cash` exists to prevent.

Faith's rules size positions through Units and the caps of ADR 0008. They do not address cash exhaustion across simultaneous or sequential entries, so this is a decision this project must make and record rather than a rule to transcribe.

## Decision

**Two questions were being conflated, and separating them dissolves most of the problem.**

### Sizing uses the previous close, unchanged

How many shares a Unit is depends on the Notional Account and on N, and letting that depend on cash that moved during the decision bar would be look-ahead: with daily OHLC we do not know the intraday sequence, so "cash after today's exit" is not knowable when today's entry is decided. **ADR 0010's basis is right and is not amended here.** `Reducer.cashAtPreviousClose` continues to refuse a figure stamped later than the decision bar's previous close, and continues to fail closed when no snapshot has ever arrived.

### Affordability binds what may be submitted, never what has already executed

Affordability is a different question from sizing, and it belongs where cash actually moves. But it is a constraint on **placing an order**, not a veto on a fill.

A recorded `execution.fill` is an external fact. In live trading it means the broker has already traded: holdings and cash have moved whether or not this system likes the arithmetic. Refusing to apply such a fill would leave the reducer's state deliberately inconsistent with the account it is supposed to mirror, which is the opposite of what a capital-safety rule should do.

So the rule is:

1. Each bar opens with the cash known at its previous close.
2. A running ledger of that figure is carried **within the bar**, reduced by every entry or Add fill as it is applied.
3. An order the ledger cannot fund is **not placed**. In a backtest `internal/fills` is the broker, so the order is never created and no fill exists to decline. In live the same check runs before submission, and the order is not sent.
4. Exit proceeds **credit** only at the next previous close. Never same-day.

Debit within the bar, credit at the close. A Unit that cannot be funded is skipped and declined with `insufficient-cash` (`event.DeclineReasonInsufficientCash`) exactly as one refused by the present check is: no partial Units, no borrowing, no deferred queue.

### A fill that arrives anyway is applied, and halts the run

A live fill can still arrive that the ledger cannot fund — a partial fill, a race between submission and a cash movement, a broker error, an order this system did not place. That is not an affordability question any more. It is a **reconciliation failure**, and ADR 0019 already says what happens to one: the fill is applied, because the broker's reality is authoritative, and the unexplained difference halts the engine rather than being absorbed.

This ADR adds no path by which a recorded fill is dropped, and none by which a discrepancy is quietly reconciled. Those are the two failures that would make the ledger worse than no ledger.

### Why the ledger is deterministic

The standing objection to a running ledger is that it makes the outcome depend on processing order. That objection is sound **at proposal time**, where the order among competing proposals would be arbitrary and a proposal commits nothing in any case. The debit therefore never happens in the proposal path.

At the fill it does not apply, but not for the reason a first reading suggests. ADR 0010 orders **decisions** — exits, then Adds, then entries. It does not order **executions**: `internal/fills` applies its own pessimistic rules (every covered buy before any sell, competing sells worst-price-first, folded back into the book until nothing more fills), and a live venue reports in arrival order. The ledger does not ride ADR 0010's order and must not claim to.

What makes it deterministic is narrower and stronger: **the ledger follows the recorded order of fills**, and the journal records that order. In a backtest that order is `fills.RunBar`'s, which is itself deterministic, so two runs of one fixture agree. In live it is the order the venue reported, which no rule governs — but replay reads it from the journal rather than recomputing it, so a recorded run reproduces byte for byte. That is the property #20 protects, and it holds for the same reason fill-driven position state does (ADR 0005): the system does not predict the order, it records it.

### What this amends in ADR 0010

ADR 0010 says "the cash and Unit-cap headroom available to every Add and entry are those known at the previous close". For **Unit-cap headroom** that stands unchanged. For **cash** it now reads: the cash available to a Unit is the previous close's figure less what this bar's earlier fills have already spent.

The two readings must not be left side by side. ADR 0010 carries a pointer to this ADR at that sentence.

## Consequences

- The cumulative overspend closes in both shapes. Four Adds can no longer each pass against an unmoved figure, and two instruments' entries can no longer both be funded from the same dollars.
- The model stays conservative in the direction ADR 0010 cares about. Debits apply immediately because spending cash you have just spent is accurate rather than optimistic; credits wait because same-day proceeds depend on an ordering the bar cannot state.
- **The available-cash figure becomes decision-relevant state carried within a bar**, where it was previously a constant for the whole bar. Replay must reproduce it exactly, and a run containing both a debit and a credit is the case to pin.
- **`ProposalDeclinedPayload`'s cash fields change meaning.** `DeclineReasonInsufficientCash` documents `AvailableCash` as the cash available at the previous close, and the existing Add-decline tests assert that unchanged figure. Under this ADR the comparison is made against the balance remaining at the moment of the attempt, so `AvailableCash` must carry that and `RequiredCash` the cost compared against it. The two figures must still be the ones the comparison actually used — that is what makes a decline auditable — so this is a schema change with its own version bump, not a re-labelling.
- **The check moves from the proposal path to order placement.** Today the decline is raised where a proposal is built (`internal/strategy/reducer.go` for entries, `campaign.go` for Adds). That is the right place for a decision and the wrong place for an affordability test, since nothing has been spent yet.
- **#106 folds into this.** A withdrawal between snapshots still leaves the reducer optimistic until the next one arrives; the ledger does not fix that, because a movement is not a fill. Whether `account.cash-movement` should also adjust the figure is decided there, and this ADR does not pre-empt it — but the two now share one mechanism to adjust rather than needing separate ones.
- `cmd/backtest` must model cash well enough to state a previous-close figure per day rather than once per run (#128). Until it does, the command's runs remain the weakest evidence this project produces about cash.
- Live trading gains the shape it needs: the broker is authoritative at execution, and the backtest's per-fill ledger stands in for that query. Reconciliation (ADR 0019) continues to explain cash by enumerated causing events rather than adopting a balance, and this ledger is one such enumeration.

## Alternatives rejected

- **Reserve at proposal time.** Contradicts "a proposal is not a commitment", and makes the outcome depend on an arbitrary order among proposals rather than on the recorded order of fills.
- **Aggregate check with a new priority order.** Ranking competing proposals would invent a rule the Turtle sources do not state — and it is unnecessary, because ADR 0010 already declares a sourced ranking for simultaneous signals.
- **Leave it until the universe widens.** The sequential-Adds shape needs no second instrument and is reachable today. Deferring would leave a capital-safety rule that cannot bind in the artefact this project treats as evidence.
- **Require the producer to re-state cash after every fill.** Pushes the ledger to whoever owns the account, which is where the real balance lives — but it makes every backtest depend on a producer that models cash per fill, and it would let same-day exit proceeds back in through the same door unless the producer itself implemented the credit rule. The credit half belongs in the declared rule, not in a producer contract.

## Open, and deliberately not settled here

Which of two competing entries is funded when only one fits is settled by whichever order is placed first, and this ADR does not decide that order. ADR 0010 ranks simultaneous **signals** by Faith's strength measure, `(close − close 63 bars ago) / N`, highest first [T p.29], ties by 20-day median dollar volume then symbol — but that ranking is **declared and not implemented**: nothing in `internal/strategy` computes it (#34). Nor does it govern execution, which is `internal/fills`' own concern.

So the multi-instrument case is deterministic for a single build and not yet governed by a stated rule. #32 and #34 close that, and it is the right place for it: an execution-level ordering is a decision about the whole daily loop, not about cash.

The single-instrument sequential-Adds case — four rungs on one instrument in one bar — does not depend on any of it, is reachable today, and is what the implementation must fix first.
