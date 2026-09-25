# ADR 0022: An order lifecycle report is recorded evidence, not a decision

- Status: Accepted
- Date: 2026-09-24
- Relates to: ADR 0005, ADR 0011, ADR 0017, ADR 0019, ADR 0020

## Context

`docs/architecture.md`'s invariant is that positions, Protective Stops and pyramid state change only from a recorded fill. But a venue reports more about an order than whether it filled: it acknowledges a submission, confirms an amendment, moves a cancellation through its own pending state, confirms the cancellation, or refuses the order outright. None of those five moves a position, and none of them is a fill.

ADR 0019 needs every one of them anyway. Its expected-open-orders comparison is built "from journalled acknowledgements and lifecycle events, not unsubmitted proposals", and an "acknowledged, journalled lifecycle transition can explain" a change reconciliation would otherwise flag as unexplained — a cancellation this system asked for, an amendment it made, an order the venue refused before it ever worked. Without a journalled record of those events, reconciliation could not tell "the venue did what we asked" from "something changed that we cannot explain", and every ordinary cancel-and-replace would look identical to a discrepancy.

`internal/event.OrderLifecyclePayload` and `internal/strategy.applyOrderLifecycle` already exist and ship (`event.OrderLifecycleEventType = "execution.order.lifecycle"`). This ADR records the decision they embody, since none of it was written down: what the closed status set is, why the reducer does nothing but validate and record, why any other status stops the run, why this is an input rather than a decision, and what was rejected along the way.

## Decision

### The closed status set

`OrderLifecyclePayload.Status` is one of exactly five values, matching the five ways an order's lifecycle can change without executing, including a refusal before it ever rests:

| Status | Meaning |
| --- | --- |
| `submitted` | the venue acknowledged the order and it is working |
| `updated` | the venue acknowledged an amendment: the order now works at the payload's `StopPrice` |
| `cancel-pending` | a cancellation was requested; the order may still execute until the venue confirms it |
| `canceled` | the order no longer works, whether this system cancelled it or the venue expired it (a DAY order at session end) |
| `invalid` | the venue refused the order; it never worked |

This is the set of state changes ADR 0019's reconciliation names as explaining an order-book difference, no more and no less: an unknown order, a missing protective order, or an unexplained cancellation or expiry halts reconciliation; an acknowledged, journalled transition in this set is what excuses one. A status the venue reports that is not one of these five is not a smaller or more convenient encoding of them — it is a report this system does not understand, and `OrderLifecyclePayload.Validate` rejects it outright rather than mapping it onto the nearest known value.

### The reducer validates and records; it decides nothing

`applyOrderLifecycle` reads the payload, validates it, and returns `(nil, nil)`: no decision, no state change. This is deliberate, not an omission pending a future ticket. `docs/architecture.md`'s own invariant — "positions, protective stops, and pyramid state change only from recorded brokerage events", and the only such event that may move a position is a fill — already answers what a lifecycle report may do to a Campaign: nothing. `OrderLifecycleEventType` exists as a type distinct from `FillEventType` specifically so that no reducer code path and no reader of a journal can mistake an acknowledgement or a cancellation for an execution; folding lifecycle changes into a status field on the fill event, the alternative considered below, would have reopened exactly that confusion.

What it validates is still real: instrument, order and tag are required (tag is the join back to the decision the order carries — the proposal or the `strategy.exit-order.set` in force), quantity must be non-zero, the stop price must be finite and positive (ADR 0005: every order this system places rests at a stated level), the occurrence time must be present (non-zero) and writable as RFC 3339, and the status must be one of the five above. A payload that fails any of these is not journalled as a lifecycle report, because a report recorded under the wrong meaning would mislead the reconciliation that later reads it — the same fail-closed discipline `docs/development.md` principle 4 states generally, applied here to a record whose only job is to be trusted evidence.

### Any other status stops the run

`Reducer.Apply`'s dispatch fails closed on any event type it does not recognise (`docs/development.md` principle 4, restated in `Apply`'s own doc comment: "Any other event type fails closed rather than being silently ignored"). `applyOrderLifecycle` applies the identical discipline one level down, inside a type it does recognise: a `Status` outside the closed set fails `Validate`, `applyOrderLifecycle` returns that error wrapped with a `strategy:` prefix (the wrapper changes only the message, not the fail-closed outcome), and — exactly as for a malformed bar, a schema-version mismatch, or any other input the reducer refuses — the run stops rather than absorbing an order-lifecycle report it cannot classify. Silently defaulting an unrecognised status to the nearest known one, or dropping it while continuing the run, would let reconciliation reason about an order book from a record that quietly omitted or misstated one of its changes — the one kind of gap ADR 0019 exists to make impossible.

### Why an input, not a decision

ADR 0017 draws the journal's own closed line between the two: `kind` is `input` for an event the run was given, `decision` for one the reducer produced, and the distinction is covered by the hash chain because "flipping one record from `decision` to `input` would be undetectable" otherwise, and that flip is exactly the one that would change what replay equivalence feeds in versus what it compares against. An order lifecycle report is a fact a venue asserted happened, external to this system, arriving over the wire — the shape ADR 0017 calls an input, not a value the reducer computed. `applyOrderLifecycle` returning `(nil, nil)` follows from being on that side of the line: `internal/strategy` is a pure domain package that reads a fact and records it, and reconciliation (ADR 0019, still design-only) is what turns a *sequence* of such facts into a comparison against a broker's own order book. Nothing about that comparison is a decision internal/strategy would need to make; it needs only that every fact reach the journal, unaltered, in the order the venue reported it.

### Alternatives rejected

- **A status field on `execution.fill`.** Rejected in the original design, and restated here: it would let a lifecycle change move a position by construction, since `FillEventType` is the one event type `applyFill` is allowed to change Campaign state from. Keeping a fill's shape unable to express "acknowledged" or "cancelled" at all is stronger than trusting every caller never to set the field that way.
- **Inferring `kind` from the payload's own shape**, rather than a stated, closed status set. Considered and rejected by ADR 0017 for the same construction generally: it is implicit coupling between the reducer's contract and a string another package owns, and it would let a status this build does not recognise be silently sorted into whichever bucket happened to parse.
- **Having the reducer derive something from a lifecycle report** — for instance, treating `invalid` as evidence a proposal should be re-evaluated, or `canceled` as freeing capacity for a new order. Rejected: ADR 0020's cash ledger already states what frees a reservation (an order resolving, at submission-time granularity, not a lifecycle report at the wire), and a rule that reacted to a lifecycle status would be a second, competing decision path for the same fact reconciliation is built to explain, without a source citation for what that decision should be.
- **Silently defaulting an unrecognised status** (mapping it to `canceled`, say, as the conservative-sounding choice) instead of failing closed. Rejected for the reason stated above: reconciliation's explanations are only as good as the record they are checked against, and a defaulted status is a fabricated fact wearing the shape of a recorded one.

## Consequences

- `internal/strategy.applyOrderLifecycle` needs no further work to satisfy this ADR; it already validates and records without deciding, and already fails closed on an unrecognised status. This ADR documents that behaviour rather than changing it.
- Reconciliation (ADR 0019, still design-only) can rely on `execution.order.lifecycle` being present in the journal for every acknowledgement, amendment, cancellation and refusal a venue reports, in the order it reported them, as one of the "journalled acknowledgements and lifecycle events" its expected-open-orders comparison already assumes.
- A future venue-specific status this build does not recognise is a schema or contract question (ADR 0015), not something a producer or the reducer may paper over locally.
- **Glossary.** CONTEXT.md gains **Order lifecycle report**: a venue's account of a change in an order's lifecycle that is not an execution, including a refusal before the order ever rests.
