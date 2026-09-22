# ADR 0020: Sizing uses the previous close; affordability is checked at the fill

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

### Affordability is checked when cash actually moves

Affordability is a different question from sizing, and it belongs at the fill — that is when cash genuinely moves, and it is what a broker answers when asked.

1. Each bar opens with the cash known at its previous close.
2. Every entry or Add **fill** debits that figure within the bar, in the order ADR 0010 already fixes: Protective Stops and Exit-Channel exits, then Adds, then new entries.
3. Exit proceeds **credit** only at the next previous close. Never same-day.

Debit within the bar, credit at the close. A Unit whose cost exceeds the cash remaining at the moment its fill is applied is skipped and declined with `insufficient-cash` (`event.DeclineReasonInsufficientCash`), exactly as a Unit refused by the existing check is: no partial Units, no borrowing, no deferred queue.

### Why a ledger here is deterministic, when at proposal time it would not be

The standing objection to a running ledger is that it makes the outcome depend on processing order. That objection is sound **at proposal time**, where the order among competing proposals would be arbitrary and a proposal is not a commitment in any case.

It does not apply at the fill. Fills already have a defined order — ADR 0010 fixes exits before Adds before entries — and simultaneous entry signals are already ranked by Faith's mechanical strength measure, `(close − close 63 bars ago) / N`, highest first [T p.29], with ties broken by 20-day median dollar volume and then by symbol (ADR 0010). So when two entries compete for the last of the cash, the one that gets it is the one the declared ranking put first, not the one a map iteration happened to reach first.

A ledger riding that existing order is fully replayable, which is the property #20 exists to protect.

### A proposal remains a non-commitment

The debit happens in the fill path, never in the proposal path. A proposal that is raised and never filled reserves nothing and releases nothing. This keeps the fill-driven invariant ADR 0005 established: state changes only from recorded fills.

### What this amends in ADR 0010

ADR 0010 says "the cash and Unit-cap headroom available to every Add and entry are those known at the previous close". For **Unit-cap headroom** that stands unchanged. For **cash** it now reads: the cash available to a Unit is the previous close's figure less what this bar's earlier fills have already spent.

The two readings must not be left side by side. ADR 0010 carries a pointer to this ADR at that sentence.

## Consequences

- The cumulative overspend closes in both shapes. Four Adds can no longer each pass against an unmoved figure, and two instruments' entries can no longer both be funded from the same dollars.
- The model stays conservative in the direction ADR 0010 cares about. Debits apply immediately because spending cash you have just spent is accurate rather than optimistic; credits wait because same-day proceeds depend on an ordering the bar cannot state.
- **The available-cash figure becomes decision-relevant state carried within a bar**, where it was previously a constant for the whole bar. Replay must reproduce it exactly, and a run containing both a debit and a credit is the case to pin.
- **#106 folds into this.** A withdrawal between snapshots still leaves the reducer optimistic until the next one arrives; the ledger does not fix that, because a movement is not a fill. Whether `account.cash-movement` should also adjust the figure is decided there, and this ADR does not pre-empt it — but the two now share one mechanism to adjust rather than needing separate ones.
- `cmd/backtest` must model cash well enough to state a previous-close figure per day rather than once per run (#128). Until it does, the command's runs remain the weakest evidence this project produces about cash.
- Live trading gains the shape it needs: the broker is authoritative at execution, and the backtest's per-fill ledger stands in for that query. Reconciliation (ADR 0019) continues to explain cash by enumerated causing events rather than adopting a balance, and this ledger is one such enumeration.

## Alternatives rejected

- **Reserve at proposal time.** Contradicts "a proposal is not a commitment", and makes the outcome depend on an arbitrary order among proposals rather than on the declared ranking among fills.
- **Aggregate check with a new priority order.** Ranking competing proposals would invent a rule the Turtle sources do not state — and it is unnecessary, because ADR 0010 already declares a sourced ranking for simultaneous signals.
- **Leave it until the universe widens.** The sequential-Adds shape needs no second instrument and is reachable today. Deferring would leave a capital-safety rule that cannot bind in the artefact this project treats as evidence.
- **Require the producer to re-state cash after every fill.** Pushes the ledger to whoever owns the account, which is where the real balance lives — but it makes every backtest depend on a producer that models cash per fill, and it would let same-day exit proceeds back in through the same door unless the producer itself implemented the credit rule. The credit half belongs in the declared rule, not in a producer contract.

## Open, and deliberately not settled here

Which of two competing entries is funded when only one fits is decided by ADR 0010's strength ranking. That ranking is **declared but not yet implemented** — nothing in `internal/strategy` computes it (#34). Until it is, the order among simultaneous entries is whatever the processing loop produces, so the multi-instrument case is deterministic for a single build but not yet governed by the rule this ADR relies on. #32 and #34 close that; the single-instrument sequential-Adds case this ADR's implementation must fix does not depend on it.
