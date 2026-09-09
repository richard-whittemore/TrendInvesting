# ADR 0015: Version the envelope shape with a required `envelope_version` field

- Status: Accepted
- Date: 2026-09-09

## Context

`Envelope.SchemaVersion` versions the *payload* schema for a given event `Type`. Nothing versions the envelope's own shape — the struct every input and output is wrapped in. #5 changed that shape, adding four new required provenance fields, and it was free only because nothing yet produces or persists envelopes. The next such change will not be free: a journal written under the old shape would fail validation under the new one, and replay would fail closed on every historical event with no way to say why.

#49 decided this had to be settled before the first producer lands (#6, #7), and considered three options: version the envelope shape explicitly; forbid envelope-shape changes once a journal exists; or fold the shape into `StrategyVersion`. Forbidding changes is a promise the project has already broken once (#5). Folding the shape into `StrategyVersion` conflates two independent axes — a strategy-rule change and a transport-shape change can happen independently, and a journal must be able to say *which* one differs when replay disagrees with a prior run. The operator decided option (a) on #49 (2026-09-09): a required `envelope_version` field, starting at 1, with replay of an unrecognised version failing closed unless an explicit upcaster exists.

## Decision

Add `Envelope.EnvelopeVersion uint32`, JSON tag `envelope_version`, required (non-zero) like the other provenance fields. Export `event.CurrentEnvelopeVersion uint32 = 1`, the envelope shape this build produces and expects to replay.

`Envelope.Validate()` rejects, with distinct messages aggregated via `errors.Join` in the existing style:

- a **zero** version — "envelope version is required";
- a version **greater** than `CurrentEnvelopeVersion` — "produced by a newer build": this build cannot know what a newer shape means, so it must not guess;
- a version **less** than `CurrentEnvelopeVersion` — "no upcaster registered": no code exists yet to translate an older shape forward, so it must not be silently accepted as-is.

Both out-of-range cases fail closed. This is deliberately asymmetric with how most of this project treats unknown input — normally the system prefers to reject loudly rather than proceed on a guess, and here that means *neither* direction is treated as compatible by default. A version below current is called out with a distinct message ("no upcaster registered") specifically so a future ticket that builds an upcaster can relax that one case without touching the "newer build" case, which can never be safely relaxed by an upcaster (there is nothing to translate *from* the future).

At `CurrentEnvelopeVersion = 1`, the only integer below the current version is 0, so the below-current check necessarily also fires for a zero `EnvelopeVersion`, alongside the required-field message. This is not treated as redundant: an envelope recorded before this field existed decodes `EnvelopeVersion` as its Go zero value, and that is exactly the real scenario the below-current rule exists to catch — a pre-versioning journal has no upcaster to bring it forward, and both messages describe true, independently useful facts about it.

No upcaster is built in this ticket — only the field, its validation, and the fail-closed rule. `internal/replay.Engine.Run` needed no change: it already validates every input and every emission it stamps (#7), so it inherits this check for free, and a wrong-version input or emission fails closed the same way any other invalid envelope does — for an emission, naming the input sequence and the emission index.

## Consequences

- Every envelope now carries an explicit envelope-shape version, independent of `SchemaVersion` (payload shape) and `StrategyVersion` (strategy rules), so a journal that fails to replay can say which of the three actually changed.
- Until an upcaster ticket lands, **any** envelope-shape change is a breaking change for every existing journal: replay of an old journal fails closed at the first event with the old version, by design (ADR intent, not a bug). This is the cost of choosing option (a) over "forbid changes," accepted deliberately because the shape has already needed to change once.
- The first real producer (#19) must stamp `EnvelopeVersion: event.CurrentEnvelopeVersion` on every envelope it creates; `Envelope.Validate()` will reject anything else.
- A future ticket introducing an upcaster changes the below-current branch from "no upcaster registered" to "look up and apply an upcaster for this version," without needing to touch the above-current branch or the zero-version check.
