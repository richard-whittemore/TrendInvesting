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
{"sequence": N, "kind": "input"|"decision", "envelope": {...}, "record_hash": "sha256:..."}
```

where `record_hash = SHA-256(previous_record_hash || kind || canonical_envelope_bytes)`, the previous hash is the 32 raw bytes of the previous record's digest, and **the first record's predecessor is the hash of the header**: `SHA-256(canonical_header_bytes)`. Canonical bytes, for the header and the envelope alike, come from `event.CanonicalBytes` — ADR 0016's canonical encoder — so this project has exactly one definition of "the canonical bytes of a value".

### Seeding the chain with the header, and why zeros were wrong

The chain was first specified to start from a zero hash, which left the header outside it. That was a hole, found in review of the implementation and fixed here: the header states the configuration hash, the strategy version, the span and the chain algorithm — *which run this journal is* — and none of it was attested. Someone could rewrite a journal's configuration hash to claim it came from a different Variant, or its strategy version to claim a different build produced it, and every record hash would still verify perfectly. A chain that exists so a journal cannot be silently rewritten, but permits it to be silently **re-attributed**, protects the wrong half of the problem.

Seeding the chain with the header's hash makes any edit to any header field break record 1, and therefore every record after it. `Verify` recomputes the seed from the header it actually read, never from what the header ought to say, so a tampered header fails at record 1 rather than being taken on trust. `ChainAlgorithm` states the seed as part of the formula, and is itself inside the seed, so a journal cannot claim to have been chained some other way either.

There is consequently no zero-hash constant any more. A journal always has a header — `Write` requires a valid one and `Read` refuses a file that does not begin with one — so a header-less journal was never a thing this format supported.

### The record states whether its envelope was an input or a decision, and the chain covers that claim

`kind` is a closed set: `input` for an event the run was given, `decision` for one the reducer produced. The alternative was to infer it from `Envelope.Source` — decisions carry the reducer's source, everything else is an input — and that was rejected for two reasons.

First, it is implicit coupling: nothing stops a producer stamping that source on something that is not a reducer decision, and the journal's meaning should not depend on a string constant owned by another package.

Second, and load-bearing: **if `kind` were outside the hash, flipping one record from `decision` to `input` would be undetectable.** That single flip changes what replay equivalence feeds in versus what it compares against — which is precisely the property replay equivalence exists to guarantee. A chain that protects the envelope but not the record's own claim about that envelope protects the wrong thing. So the kind is hashed with the envelope, and `Verify` also rejects any value outside the closed set, since a forger who recomputed the chain could otherwise write anything there.

This refines the formula rather than contradicting the decision it came from: the intent was always that the chain protects the record, and the kind is part of the record. `journal.Recorder` is where the two are told apart, structurally and once — what arrives as `Apply`'s argument is an input, what the handler returns is a decision — so no reader re-derives it.

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

### The buffer is bounded, and the span is checked rather than trusted

*Amended 2026-09-17.* The consequence below — that a journal is composed in memory and will need revisiting for a run large enough not to fit — was revisited. The format is unchanged. Two things around it are not.

**The span is now compared with the records, not taken on trust.** `journal.Write` refuses a header whose span its own input records deny, and `journal.CheckSpan` is the reader's half of the same rule: the span is exactly the first and last event time among the `input` records, and a file with no input among its records has no span it could state truthfully. `Recorder` derives its header through the function `Write` checks against, so the deriver and the checker cannot drift.

This closes a hole the chain does not. The chain covers the span, so an edit to it is detectable — but only against a chain nobody repaired, and a run composing its own header was *trusted* to state the span honestly rather than checked. `Write` took a header and a slice of entries from any caller and compared them to nothing, so a journal could chain perfectly, pass `CheckIdentity` and still claim a period the run never reached. The claim is now falsifiable from the file alone, which is what any future streaming design would have to be checked by.

The span speaks for the input stream and nothing else. A decision is attributed to the input that caused it, and `replay.Stamp` does not set a decision's event time, so a decision dated outside the span is not the span's business.

**The buffer is bounded at `journal.DefaultMaxRecords` (2,000,000), raised for `cmd/backtest` by `-max-records`.** Reaching it stops the run with a `*journal.RecordLimitError`; what was recorded is journalled under the span it covers and registered as the failed run it is. The bound is checked at an **input boundary**, before the input is recorded — refusing part way through an input's emissions would leave a journal claiming the reducer decided nothing for its last input, and replay equivalence would report that as a divergence in a faithful record. A run therefore overshoots its bound by at most the decisions of the input that reached it.

Two million records is roughly twice the largest daily-bar run this platform is built for — a decade over a hundred instruments is about a million, at the golden fixture's ratio of records to bars — and about a gigabyte of resident entries at that fixture's ~1 kB per record. It is a tripwire, not a capacity model: its job is to fire *before* the allocator does, so the failure is a diagnosable refusal instead of a process killed with nothing on disk.

### Why the journal is still written in one pass

Three alternatives were considered and rejected, and the reason is the same in each case: the chain is seeded from the canonical header bytes, so record 1's hash cannot be computed until the header exists.

- **Writing the header last**, as a footer or a sidecar, means either the records are streamed unchained and the chain is computed in a second pass — in which case they were held somewhere after all — or the seed stops being the header, which reopens the re-attribution hole seeding closed. A sidecar is worse than a footer: the header *is* the journal's identity, and a second file makes that identity separable, and makes ADR 0018's exclusive install a two-file transaction with no atomic form.
- **Streaming with a fixed-width placeholder header, rewritten in place at the end**, chains every record from the *placeholder's* hash. `Verify` reseeds from the header as read, deliberately, so rewriting the header makes every record in the file wrong at once and the chain breaks at record 1. The only repairs are to rewrite every record hash too — the whole file, so nothing was streamed — or to drop the span from the seed, which makes the span a header field editable without breaking anything: a header that can lie about its span, undetectably. A crash mid-rewrite leaves a header half placeholder and half final, which is either unparseable or parseable and wrong.
- **Two passes over the bar source**, deriving the span before the run, makes the span a claim about the *planned* inputs. A run that fails closed part way through, or that the operator interrupts, would state a span running to the last planned input and assert coverage of a period it never reached — the header lying by construction rather than by malice.

There is also a point about what streaming would buy. The run already holds every bar resident and the reducer holds its own state, so removing the journal's buffer turns O(n) into O(n), not into O(1): a run whose journal does not fit in memory would still not run. The format question therefore reopens when the bar source is streamed, and is decided then as one change rather than three.

Nor does a partial journal argue for streaming. Nothing is ever written at a journal's final path: it is composed in a temporary file and hard-linked in (ADR 0018), precisely so an interrupted run leaves no partial journal that reads like a complete one. A streamed partial file would still not be linked, and one carrying a placeholder span would present as a *tampered* journal. Installing partial evidence is a change to ADR 0018, and a separate decision.

### Anchoring is the intended next step

Before paper trading, each completed run's final `record_hash` — which `journal.Verify` returns for this purpose — together with its record count and span, should be recorded in the run registry and that registry committed to git. The git history then anchors the chain heads outside the system, turning "detectable if you kept the original" into "detectable, full stop", with no key management, and it closes the end-truncation gap above.

## Consequences

- A journal is self-verifying without a secret, and `backtest -verify` is the documented way to check one.
- The envelope contract is untouched, so byte-identical replay stays satisfiable.
- A journal is composed in memory and written in one pass, because the header states the span the run covered and that is not known until the last input has arrived. The buffer is bounded rather than unbounded, and a run that reaches the bound stops and journals what it took; the format reopens when the bar source is streamed, not before.
- A header cannot state a span its own input records deny, whoever composed it. That is checked on the way in by `Write` and on the way out by `CheckSpan`, which is a third question about a journal, separate from chain verification and from replay equivalence.
- Replay equivalence reads `kind` to split a journal, rather than inferring the split from a producer's name.
- A journal records the build that produced it, in every envelope's strategy version (ADR 0016). The build identifier is therefore injected at the composition root rather than read from `internal/buildinfo` inside the run, so a test asserting a journal byte for byte fixes it: a golden keyed to the build identifier would assert which machine produced the journal rather than what the platform decided.
- A journal is written once. `cmd/backtest` refuses a path that already exists rather than truncating it (AGENTS.md rule 6: never rewrite or delete recorded evidence) and installs the journal by hard-linking a completed temporary file into place, so an interrupted run cannot leave a partial journal that reads like a complete one and a concurrent run cannot replace one that already arrived — `rename(2)` replaces a destination silently, `link(2)` fails atomically. There is deliberately no overwrite flag.
- Nobody should describe this as making the journal immutable. It makes the journal *checkable*.
