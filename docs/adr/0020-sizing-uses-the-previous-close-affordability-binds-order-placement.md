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
2. A running ledger of that figure is carried **within the bar**, reduced by every actual fill cost and by every hold still standing.
3. Placing an order **reserves** its cost against that ledger, and the reservation stands until the order resolves. An order the remaining balance cannot fund is **not placed**. In a backtest `internal/fills` is the broker, so the order is never created and no fill exists to decline. In live the same check runs before submission, and the order is not sent.
4. Exit proceeds **credit** only at the next previous close. Never same-day.

Debit within the bar, credit at the close. A Unit that cannot be funded is skipped and declined with `insufficient-cash` (`event.DeclineReasonInsufficientCash`) exactly as one refused by the present check is: no partial Units, no borrowing, no deferred queue.

### A submitted order reserves its cash; a proposal still reserves nothing

Checking at submission and debiting at the fill would leave a gap between them. Two orders submitted before either fills would each pass against the same balance, and both could then fill — which is the overspend this ADR exists to close, moved rather than removed.

So the reservation happens at **submission**, and this does not contradict "a proposal is not a commitment". A proposal is a statement that a rule fired; a submitted order is an instruction to a broker to trade. The second is a commitment in every sense that matters to cash, and it is the point at which a broker's own buying power falls too.

A reservation is a **hold on cash, sized from an estimate**, because the true cost is not knowable when the order is placed: ADR 0013 applies slippage of `SlippageN × N` against the trader on every fill and IB commissions on top, and a gap fill executes at the open rather than at the order's level. The hold must therefore be the estimated cost of the order's unfilled quantity **including** that slippage and commission, or it would be systematically too small and the check it feeds would be optimistic — the one direction this rule exists to rule out.

What each lifecycle event does:

- **Filled.** The ledger is debited the fill's **actual** cost, slippage and commission included, and the hold is resized to the estimated cost of whatever quantity remains unfilled. A fill that cost more than the hold set aside for it reduces the available cash by that much more, which is correct: the money left is the money left.
- **Cancelled, rejected or expired.** The remaining hold is dropped. Nothing is credited — the cash was never spent, only held — and the hold simply stops reducing what is available.
- **Unknown.** The hold stands. Cash that might already have been spent is not offered to another order on the strength of a guess, and an order stuck in that state is a reconciliation failure under ADR 0019, which halts rather than waits.

A fill debits the ledger exactly once, at its actual cost. Nothing debits it a second time when the order later resolves, and nothing credits back a hold that a fill has already converted into a spend.

### The invariant

Conservation belongs to the **ledger**, not to the order, precisely because an order's estimate and its actual cost differ. Two statements, and only the first involves money — which it states in two steps, because the zero floor from the cash-movement amendment below applies to one of them and not the other:

> **basis = max(0, the previous close's figure − withdrawals recorded since that figure)**
>
> **available = basis − every actual fill cost this bar − every hold still standing.**
>
> **an order's unfilled quantity + its filled quantity + its cancelled quantity = the quantity it was placed for.**

The floor bounds the **basis** only, before any fill or hold is deducted. It is a statement about purchasing capacity: once withdrawals have consumed the known cash, no order may be placed against it. It is not a floor on `available`. An order is only placed when `available` covers its hold, so a placed order never drives it negative — but a fill the ledger cannot fund can still arrive (next section), is still recorded at its actual cost, and can leave `available` below zero. That negative remainder is the evidence the reconciliation failure is built on, and flooring it would hide exactly the discrepancy ADR 0019 requires to halt the run.

The quantity statement is exact because quantities are whole and no estimate enters it. The money statement needs no estimate to be correct either: holds are estimates while they stand, and each is replaced by a real number the moment a fill makes one available.

This replaces an earlier two-term form that tried to conserve *money* across an order's lifecycle. It could not: a $100 order filling $40 and then cancelling left $40 against a $100 placement, and adding a third term for released cash would still have broken the moment a fill cost more than its share of the estimate.

Releasing on cancellation is what keeps the rule from being merely restrictive: a bar that raises four Adds and fills two has the other two's cash back before the next bar's decisions, without waiting for a snapshot.

### A fill that arrives anyway is applied, and halts the run

A live fill can still arrive that the ledger cannot fund — a partial fill, a race between submission and a cash movement, a broker error, an order this system did not place. That is not an affordability question any more. It is a **reconciliation failure**, and ADR 0019 already says what happens to one: the fill is applied, because the broker's reality is authoritative, and the unexplained difference halts the engine rather than being absorbed.

This ADR adds no path by which a recorded fill is dropped, and none by which a discrepancy is quietly reconciled. Those are the two failures that would make the ledger worse than no ledger.

### Why the ledger is deterministic

The standing objection to a running ledger is that it makes the outcome depend on processing order. That objection is sound **at proposal time**, where the order among competing proposals would be arbitrary and a proposal commits nothing in any case. The debit therefore never happens in the proposal path.

At the fill it does not apply, but not for the reason a first reading suggests. ADR 0010 orders **decisions** — exits, then Adds, then entries. It does not order **executions**: `internal/fills` applies its own pessimistic rules (every covered buy before any sell, competing sells worst-price-first, folded back into the book until nothing more fills), and a live venue reports in arrival order. The ledger does not ride ADR 0010's order and must not claim to.

What makes it deterministic is narrower and stronger: **the ledger follows the recorded order of order and fill events**, and the journal records that order. Replay does not recompute the balance from the previous-close snapshot alone — that figure is only the ledger's opening value, and the running state is derived by applying the recorded reservations, spends and releases in the sequence the journal holds. In a backtest that order is `fills.RunBar`'s, which is itself deterministic, so two runs of one fixture agree. In live it is the order the venue reported, which no rule governs — but replay reads it from the journal rather than recomputing it, so a recorded run reproduces byte for byte. That is the property #20 protects, and it holds for the same reason fill-driven position state does (ADR 0005): the system does not predict the order, it records it.

### What this amends in ADR 0010

ADR 0010 says "the cash and Unit-cap headroom available to every Add and entry are those known at the previous close". For **Unit-cap headroom** that stands unchanged. For **cash** it now reads: the cash available to a Unit is the previous close's figure less what this bar's earlier fills have already spent.

The two readings must not be left side by side. ADR 0010 carries a pointer to this ADR at that sentence.

## Consequences

- The cumulative overspend closes in both shapes, and in the concurrent shape too. Four Adds can no longer each pass against an unmoved figure; two instruments' entries can no longer both be funded from the same dollars; and two orders outstanding at once can no longer each pass against a balance neither has yet reduced.
- **Order lifecycle events become cash-relevant.** Acknowledgement, cancellation, rejection and expiry each move the ledger, so they must be journalled inputs like fills rather than adapter-local state. `internal/fills` already learns the resting book from the reducer's own emissions, so the backtest side has the events; the live side needs them from the adapter (#29, #30).
- The model stays conservative in the direction ADR 0010 cares about. Debits apply immediately because spending cash you have just spent is accurate rather than optimistic; credits wait because same-day proceeds depend on an ordering the bar cannot state.
- **The available-cash figure becomes decision-relevant state carried within a bar**, where it was previously a constant for the whole bar. Replay must reproduce it exactly, and a run containing both a debit and a credit is the case to pin.
- **`ProposalDeclinedPayload`'s cash fields change meaning.** `DeclineReasonInsufficientCash` documents `AvailableCash` as the cash available at the previous close, and the existing Add-decline tests assert that unchanged figure. Under this ADR the comparison is made against the balance remaining at the moment of the attempt, so `AvailableCash` must carry that and `RequiredCash` the cost compared against it. The two figures must still be the ones the comparison actually used — that is what makes a decline auditable — so this is a schema change with its own version bump, not a re-labelling.
- **The check moves from the proposal path to order placement.** Today the decline is raised where a proposal is built (`internal/strategy/reducer.go` for entries, `campaign.go` for Adds). That is the right place for a decision and the wrong place for an affordability test, since nothing has been spent yet.
- **#106 folds into this.** A withdrawal between snapshots still leaves the reducer optimistic until the next one arrives; the ledger does not fix that, because a movement is not a fill. The cash-movement amendment below now settles that question; the eventual fill ledger must carry the same withdrawal debits rather than debit them twice.
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

## Amendment: cash movements constrain spendable cash (2026-09-23)

A cash movement is not a fill and is not a replacement balance statement.
The three alternatives in #106 were to apply both signs and advance the basis,
invalidate the basis until a snapshot arrives, or require the producer to send
one. None follows the asymmetry above as closely as applying only the debit:

- **An accepted withdrawal immediately reduces snapshot-backed spendable cash.**
  Subtract its magnitude once, in recorded account-event order, even if its
  timestamp is inside the decision bar. Repeated withdrawals accumulate.
- **A deposit adds nothing to spendable cash.** A later accepted snapshot may
  include it, but that snapshot must still be no later than the decision bar's
  previous close before its cash may be spent. Passage of a bar alone does not
  promote deposits: a movement states a delta, not the account's balance.
- Keep the snapshot's as-of timestamp and presence flag. Neither sign establishes
  an unknown balance or makes a snapshot from inside the decision bar eligible.
  With no snapshot, affordability continues to fail closed under ADR 0010.
- A subsequent snapshot replaces the constrained figure outright; earlier
  withdrawals are already reflected in that balance and must not be deducted
  again. Shared, strictly increasing account-event chronology rejects duplicate
  or out-of-order movements and snapshots before they can change cash.
- Spendable cash has a floor of zero. If withdrawals consume or exceed the known
  cash, no Unit can be funded until a later eligible snapshot supplies cash.
  This floor represents purchasing capacity, not a claim that actual cash cannot
  be negative. The full movement remains in the journal; deposits never offset
  the floor, and ADR 0019's reconciliation obligations remain unchanged.
- Apply the spendable adjustment only after the movement passes validation,
  chronology, currency and ADR 0007's Notional Account scaling. Rejected
  movements must not change spendable cash. ADR 0007's proportional scaling of
  the Notional Account, yearly starting figure and measurement base is unchanged.

This follows because a known withdrawal can only remove opportunities: it does
not let a decision exploit an unknown intraday sequence to spend more. The
converse credit could do exactly that. Keeping the snapshot timestamp preserves
its eligibility test while the debit imposes an additional constraint; advancing
it would instead halt all decisions after an ordinary movement, even those still
fundable. Invalidation is safe but unnecessarily restrictive, and relying solely
on an unenforced producer contract leaves the unsafe interval intact. Applying
both signs would admit same-day deposits and violate the credit rule.

This is a project cash-safety rule, not a Turtle rule from the primary sources.
It changes affordability only; it does not amend any DISCLOSED strategy rule or
blend Baseline and Variant settings. At this amendment's implementation point,
the reducer still checks entry and Add proposals, and the broader order/fill
ledger above remains work for #105. Both existing checks must use the reduced
figure now; that does not claim cumulative fill affordability is implemented.

`strategy.proposal.declined` advances to payload schema **3**: `AvailableCash`
means spendable cash at the attempt after accepted withdrawal debits, and
`RequiredCash` remains the Unit cost actually compared. Old schema-2 decisions
must not silently acquire that meaning. There are no new fields and neither
cash-movement nor account-snapshot input schemas change. The future fill ledger
must account for its own cost/check changes when versioning this contract.
Replay uses the journal's movement order, including the original full amounts,
so neither the conservative floor nor deferred deposits discard audit evidence.


## Amendment: the adapter produces LEAN portfolio snapshots (2026-09-24)

The LEAN adapter is the producer of `account.snapshot` for a running
`cmd/engine`. It reports LEAN's own account figures, without strategy arithmetic:

- `equity` is `Portfolio.TotalPortfolioValue`, `available_cash` is
  `Portfolio.Cash`, and `currency` is `USD`. The payload follows
  `event.AccountSnapshotPayload`, including its required fields and validation,
  and uses `event.AccountSnapshotSchemaVersion` (currently 2).
- Send one snapshot **after each completed bar's decisions have been received**,
  before the next bar. Its `as_of` is that bar's own `period_end`, strictly
  increasing across snapshots. Warm-up bars receive snapshots in exactly the
  same way; they are neither withheld nor marked to change reducer readiness.
- A snapshot is eligible for sizing only when its `as_of` is no later than the
  decision bar's previous close (ADR 0010). Thus the snapshot as of bar *t*'s
  close is precisely the basis used for bar *t+1*, never for bar *t* itself.
- There is **no opening snapshot**. LEAN delivers warm-up bars before StartDate,
  so an opening snapshot stamped at StartDate would not precede those bars.
  None is needed: the reducer cannot size a Unit on the first bar it receives,
  because N and the channels are computed from bars preceding the decision bar.
  The snapshot sent after bar 1 therefore arrives before any sizing is possible.
- `event_time` and `recorded_at` equal `as_of`, just as the bar publisher records
  the bar's own period end. This preserves backtest determinism; the adapter
  continues to refuse LiveMode.
- Bars and snapshots share the one contiguous input sequence after the engine's
  configuration at 1: first bar 2, its snapshot 3, next bar 4, its snapshot 5.
  Each snapshot reply receives the same identity, sequence, causation,
  correlation and payload-hash checks as a bar reply. Any failure stops the run.

In live trading LEAN's portfolio is brokerage-backed and supplies the account
observation for the reconciliation input ADR 0019 anticipates. Publishing these
figures does not implement that reconciliation: its independent projection,
causing-event evidence and halt requirements remain, and an observed balance
cannot explain its own discrepancies. This amendment does not clear a live gate.

`account.cash-movement` production remains out of scope. This amendment settles
the snapshot producer and delivery timing only; no DISCLOSED strategy rule,
Baseline or Variant setting, payload schema or RulesVersion changes.
