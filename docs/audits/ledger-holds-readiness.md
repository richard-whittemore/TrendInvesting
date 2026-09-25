# Ledger holds: specification findings and local work log

Scope: issue #220, branch `ticket/220-ledger-holds`, starting from `946d3c2`.
No GitHub access, push, or adapter changes are authorized for this work.

## Completed: schema-aware decline validation

`ProposalDeclinedPayload.ValidateSchema` validates against an explicit schema.
Schemas 2 and 3 reject negative available cash; schema 4 accepts finite negative
cash while retaining all existing payload checks. Schema 1 remains unsupported:
it predates the required Kind. Zero and future schema versions fail closed.
`Validate` delegates to the current schema, preserving current producers.

The event package at this revision has no existing version-parameter payload
validator to reuse. Existing consumers dispatch by checking the envelope schema
before calling a payload's `Validate`; for example, `renderDecision` in
`cmd/backtest/decision_text.go` rejects every non-current decline schema. This
change adds an explicit payload validation entry point without changing which
historical records those consumers accept or silently upgrading old meanings.

TDD evidence:

- Added `TestProposalDeclinedValidationBySchema` before the method existed;
  the Go compiler rejected the missing method.
- After implementation, `go test ./internal/event` passed.
- Falsification: temporarily changed the historical guard from `< 0` to `< -2`.
  The schema-2 and schema-3 cases with cash `-1` both failed because validation
  incorrectly succeeded. Restored the guard before the complete check.
- `make check COVERAGE_MIN=95.3` passed: dependencies, formatting, vet,
  staticcheck, golangci-lint, race-enabled tests, the uncovered-branch audit,
  vulnerability scan, and build. Total coverage remains 95.3%.
- Existing byte-identical golden replay, Baseline and Variant journal goldens,
  and the unfiltered decision corpus passed. No decision changes; RulesVersion
  stays 1.8.0. No golden, corpus, fingerprint, or exclusions changes are needed.

## Pending: the requested proposal holds contradict ADR 0020

ADR 0020, Decision, explicitly states:

> A submitted order reserves its cash; a proposal still reserves nothing

Its Alternatives rejected says:

> **Reserve at proposal time.** Contradicts "a proposal is not a commitment",
> and makes the outcome depend on an arbitrary order among proposals rather
> than on the recorded order of fills.

ADR 0021, section 3, says it changes ordering but adds no cash rule. Its Open
section explicitly confirms that the session-close pass reserves nothing.
Section 7 keeps fill-chained proposals subject to the cash check in force.

Consequently the requested accepted-proposal holds cannot simultaneously be
implemented strictly under the current ADR. Clarification requested: either
amend the ADR to make acceptance reserve cash in the declared session-close
order, including fill-chained Adds, or retain submission-only holds and place
them in the order-placement protocol. A venue's `submitted` lifecycle report
is an acknowledgement after submission, not the pre-submission check required
by the ADR. The current reducer only validates lifecycle inputs.

The amount is explicit: the hold covers unfilled quantity at the estimated
execution price, including configured slippage and commission, not merely
quantity times resting level times dollars per point. Filled quantity replaces
its estimate with actual cost exactly once; cancellation, rejection, and expiry
release the remainder; unknown order state retains it.

## Pending: unfundable-fill response and evidence

ADR 0020, The invariant, states:

> **available = basis − every actual fill cost this bar − every hold still standing.**

Its 1.7.0 implementation note identifies the missing behavior as:

> the halt when a fill drives `available` below zero

The relevant figure is the post-fill remainder, after replacing the filled
portion's hold with actual cost, without flooring it. Zero is fundable;
strictly negative is not. The fill must still be applied. Returning an ordinary
builder error would discard it under `transact` and violate this requirement.

ADR 0020's fill section says:

> the fill is applied, because the broker's reality is authoritative, and the
> unexplained difference halts the engine rather than being absorbed.

ADR 0019's approved graduated-response amendment instead says:

> Only `unverifiable` evidence, or a discrepancy the evidence cannot bound,
> still halts.

And:

> **A cash-only discrepancy names no instrument.** It blocks entries and Adds,
> because sizing depends on cash, and leaves every position under normal management.

Two readings require a decision: treat the negative ledger remainder as a
bounded cash-only discrepancy and enter scoped Degraded, applying the fill and
continuing risk-reducing management; or treat the absent reconciliation evidence
as unverifiable and enter Halted after applying the fill. The ledger alone does
not provide ADR 0019's verified anchor, complete activity evidence, and exact
monetary reconciliation. The current engine-state payload supports only schema
1/halted; Degraded requires the amendment's schema 2 and persistent scope/state.
Neither response has been implemented in this partial change.

## Pending: a snapshot cannot attest an unfilled hold by time alone

The 1.7.0 and 1.8.0 notes explicitly drop only fill debits at or before the
snapshot's as-of; later fills remain debited, even when delivered earlier than
the snapshot. This is confirmed for the simulator's shared clock.

Neither note says an unfilled hold is reflected by such a snapshot. The producer
amendment names LEAN `Portfolio.Cash`, and the simulator states account cash,
not cash less outstanding order reservations. Dropping a still-standing hold
solely because its placement predates the snapshot would make reserved cash
spendable again. The ADR requires that reservation to stand until resolution.

The remaining specification must state whether holds survive balance snapshots
until lifecycle resolution (consistent with the present cash definition), or
introduce a snapshot contract that explicitly attests which holds its cash
already subtracts. Matching timestamps alone cannot supply that evidence; live
fill/snapshot clock and cutoff semantics also remain unconfirmed.

These concerns are recorded locally because the task expressly forbids GitHub
access. The holds, transaction-isolation tests, fill response, and associated
RulesVersion 1.9.0 evidence remain outstanding pending the specification choice.

## Resolved by the owner's decisions of 2026-09-24

The three pending questions above were answered on #220 ("Decisions (Richard,
2026-09-24)") and are recorded as amendments to ADR 0005, ADR 0008's note, ADR
0010, ADR 0013, ADR 0020 and ADR 0021, all dated 2026-09-24:

- **Holds.** A proposal reserves its worst-case cost and its cap headroom at
  once, in ADR 0021's order, fill-chained proposals included. ADR 0020's
  rejected alternative "Reserve at proposal time" is withdrawn. The hold is
  the Unit's quantity at its price cap (level + 1N in the Baseline), plus
  slippage and commission at that price. A fill releases it and is debited
  once at its actual cost; an expiry or a cancellation releases it.
- **Snapshots.** A snapshot never releases a hold. It states the account's
  cash, which an unfilled order has not reduced.
- **An unfundable fill.** Under the Baseline's stop-limit a fill cannot cost
  more than its hold. The safety net is ADR 0019's Degraded state (#227);
  until that exists, the existing behaviour (every later Unit declined while
  available cash is negative) is the interim form.

Implemented at RulesVersion 1.10.0. The version-aware decline validation
above carries forward: schema 6 is validated as schema 5 is, with the new
meanings stated on `event.ProposalDeclinedSchemaVersion`.
