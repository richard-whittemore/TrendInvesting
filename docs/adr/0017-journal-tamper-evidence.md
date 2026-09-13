# ADR 0017: Journal tamper-evidence is a hash chain in the journal record

- Status: Accepted
- Date: 2026-09-12

## Context

The envelope carries a payload integrity hash: a plain SHA-256 of the payload bytes stored beside the payload (`event.HashPayload`). That detects corruption and careless edits to a payload. It is **not** tamper-evidence against a party who can edit the journal, because whoever can change the payload can recompute the hash beside it.

For a system whose journal is the audit trail behind real-money decisions, "the journal cannot be silently rewritten after the fact" is worth having. It is a **journal-level** property, not an envelope-level one: it concerns the relationship between records, which no single envelope can attest to.

The question was whether to use a keyed MAC or a hash chain, and where the resulting value lives.

## Decision

**A hash chain, carried in the journal record and never in the envelope. No keyed MAC.**

### The record shape

A journal is a header line followed by one record per line:

```
{"sequence": N, "envelope": {...}, "record_hash": "sha256:..."}
```

where `record_hash = SHA-256(previous_record_hash || canonical_envelope_bytes)`, the previous hash is the 32 raw bytes of the previous record's digest, and the first record's predecessor is 32 zero bytes (`journal.ZeroRecordHash`). Canonical envelope bytes come from `event.CanonicalEnvelopeBytes`, which reuses ADR 0016's canonical encoder so this project has exactly one definition of "the canonical bytes of a value".

The header records the run's configuration hash (ADR 0016), strategy version, the span of input event times covered, the chain algorithm, and the journal format version. An unrecognised format version fails closed in both directions, exactly as ADR 0015 requires of the envelope.

### Why not a keyed MAC

A keyed MAC defends against a party who can rewrite the journal but does not hold the key. In a single-operator research platform that party is the operator, so the MAC buys nothing today while adding real cost: where the key lives, how a verifier without it checks anything, rotation, and what a journal signed with a lost key is worth. Revisit when a journal must convince someone else — a broker dispute, an auditor, an investor — and at that point the cheaper and stronger answer is anchoring, below, not a secret.

### The chain sits outside the envelope

If a chain field lived on the envelope, the same decision event would hash differently depending on its position in a journal, and "replay produces byte-identical decisions" would be impossible to satisfy. Therefore the envelope shape is unchanged, ADR 0015's shape stays put, and `payload_hash` keeps doing its own job: payload corruption, per event.

### Two properties, two checks

- **Chain verification** (`journal.Verify`) recomputes the chain over a file and reports the first broken link by sequence. It answers "was this file edited after it was written".
- **Replay equivalence** compares envelope streams — it reads a journal's *inputs*, runs them through a fresh reducer, and compares the result against the decisions the journal recorded. It answers "does this engine still produce these decisions", and never looks at the chain.

A journal that replays identically but whose chain is broken means the file was edited after the fact. A journal whose chain is intact but replays differently means the engine changed, or the record of what it decided was forged by someone who repaired the chain. Keeping the two checks separate is what makes those failures distinguishable, and both are tested directly against a real journal in `cmd/backtest`.

### This is tamper-evidence, not tamper-proofing

It must not be mistaken for proof. Altering event *k* breaks every link after *k*, so a silent edit to recorded history is detectable without a secret — but **whoever can rewrite one record can rewrite the whole file**, recompute every hash after it, and produce a journal that verifies perfectly. What the chain buys is that an edit cannot be *quiet*: it forces the editor to rewrite everything downstream, and it makes a partial or careless edit obvious and attributable to a record.

Two further limits, stated rather than assumed away:

- **Dropping records from the end is undetectable within the file.** A prefix of a valid chain is itself a valid chain. `internal/journal` has a test asserting exactly this, so the gap is recorded rather than discovered later.
- **The chain attests the envelopes, not the run.** It cannot say that the recorded events are the ones the engine actually produced; that is replay equivalence's question.

### Anchoring is the intended next step

Before paper trading, each completed run's final `record_hash` — which `journal.Verify` returns for this purpose — together with its record count and span, should be recorded in the run registry and that registry committed to git. The git history then anchors the chain heads outside the system, turning "detectable if you kept the original" into "detectable, full stop", with no key management, and it closes the end-truncation gap above.

## Consequences

- A journal is self-verifying without a secret, and `backtest -verify` is the documented way to check one.
- The envelope contract is untouched, so byte-identical replay stays satisfiable.
- A journal is composed in memory and written in one pass, because the header states the span the run covered and that is not known until the last input has arrived. This is fine at fixture and daily-bar scale and will need revisiting for a run large enough that its journal does not fit in memory.
- Nobody should describe this as making the journal immutable. It makes the journal *checkable*.
