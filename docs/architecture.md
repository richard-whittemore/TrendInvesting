# Architecture

## Decision boundary

The system deliberately separates trading decisions from market and brokerage mechanics.

```text
LEAN container
  -> thin Python adapter
  -> versioned, transport-neutral messages
  -> Go application
       - strategy modules
       - portfolio risk controller
       - decision and reconciliation state machine
       - durable event journal and projections
       - structured telemetry
  -> trade proposals returned to LEAN
  -> orders and fill events returned to Go
```

Go never assumes an intended order was filled. Positions, protective stops, and pyramid state change only from recorded brokerage events.

## Event contract

Every input and output uses an immutable envelope containing:

- stable event identifier;
- a required envelope shape version (`envelope_version`), distinct from the payload schema version below — it versions the envelope struct itself, and replay of a version this build does not recognise fails closed unless an explicit upcaster exists (ADR 0015);
- event type and schema version;
- event time and recording time;
- ordered processing sequence;
- correlation and causation identifiers;
- the source that emitted the event, the strategy version, and the configuration hash it was produced under;
- a payload integrity hash — the SHA-256 of the payload bytes exactly as stored, hex-encoded; and
- an immutable JSON payload.

The provenance fields are required. Together they let a reviewer reading a journal say which component, which build, and which configuration produced a decision, and the integrity hash makes a payload altered after recording detectable rather than silent. Results are retained under their configuration hash (ADR 0012), so this is what ties a journal back to a declared Baseline or Variant.

Replay rejects invalid events, sequence gaps, and payloads that do not match their integrity hash. Payload schemas and upcasting rules will be introduced explicitly as the contract evolves.

## Replay engine

`internal/replay.Engine.Run` is the seam between events in and decisions out: it applies a contiguous input stream to a `Handler`, and returns every decision envelope the handler emitted, in emission order. A handler emitting nothing for a given input is valid.

Emitted envelopes form their own contiguous output stream, independent of the input stream: the engine assigns each one's `Sequence` from a counter starting at 1, in emission order, overwriting whatever the handler set — input sequences are left exactly as the producer set them. A later journal writer interleaves the two streams by recording order; replay equivalence compares output streams. The engine also sets `CausationID` (the input envelope's `ID`) and `CorrelationID` (the input's `CorrelationID` if set, else its `ID`) on every emission, overwriting whatever the handler set — a handler cannot claim causation or correlation it did not have. Each emitted envelope is validated after stamping, so an invalid emission fails closed, naming the input sequence and the emission index. A handler remains responsible for `Source`, `StrategyVersion`, `ConfigurationHash`, `Payload`, and `PayloadHash` on what it emits.

**A handler that fails closed may emit a final event explaining why**, alongside the error rather than instead of it: `Apply` may return a non-empty emission slice together with a non-nil error on the same call. The engine journals that emission — stamping and validating it exactly like a successful one — before returning the error, so a capital-safety halt or similar terminal decision reaches the journal even though the run does not continue. `Run` returns every emission collected up to and including the failing call's own, alongside the (possibly further-wrapped, if the final emission is itself invalid) error. Without this, a handler's own explanation of why it stopped would never reach a caller of `Run` at all — the run would fail closed *silently*, at exactly the seam a reviewer most needs to see why (PR #72's review round).

## Backtest loop

A backtest has no broker, so something must decide which resting orders a bar executed. `internal/fills` does: it holds the orders in force for an instrument, learned entirely from the reducer's own emissions, and turns one completed bar into `execution.fill` events that are indistinguishable in shape from adapter-produced ones. It implements ADR 0005's fill model and ADR 0013's cost model, and it makes no strategy decision of its own — every level it fills against was computed by the reducer from completed prior bars.

`fills.RunBar` is the loop, and is the single place the protocol is defined; `#19`'s backtest command and `#30`'s LEAN adapter both follow it rather than re-deriving it. For each completed bar **B** of an instrument, in order:

1. **Open-instant pass.** Every order already resting before B whose level lay beyond B's open executed *at the open* — the first instant of the bar — so those fills reach the reducer **before** B does. A Campaign that gapped through its stop was already closed for the whole of that session, and journalling a Campaign-evaluated event for it, or adding a Unit to it later in the bar, would record something that cannot have happened.
2. **The bar.** B is delivered to the reducer, which expires yesterday's proposals (ADR 0011), evaluates an open Campaign — the stop and Exit Channel levels in force, and on a breach the exit proposal — and then the Add; or, with no Campaign open, evaluates the Setup and, on a breakout, raises the trade proposal. ADR 0010's "exits before Adds before entries" is this step's ordering and is unchanged.
3. **Intrabar fixpoint.** Every order now resting is evaluated against B, repeatedly, until nothing more fills: every covered **buy** first, then, when no buy is covered, the single worst-priced covered **sell**. Each fill is delivered as it is decided and whatever it causes — a Unit, its own Protective Stop, the next rung, a cancelled Add, a closed Campaign — is folded back into the book before the next pass. This is what lets all four Units be added inside one bar, each rung measured from the fill before it.
4. Anything still unfilled keeps resting; the reducer expires it at B+1.

The three rules that keep the model from flattering the equity curve are ADR 0005's, and all three are pessimistic by construction:

- **The coverage test is one-sided per side** — a buy executes when the bar's high reaches its level, a sell when the low does — and the executed price is `max(level, reference)` for a buy, `min(level, reference)` for a sell. A two-sided "low ≤ level ≤ high" would make a bar that gapped clean through a level never fill at all, which contradicts the ADR's own gap rule.
- **Slippage of `SlippageN × N` is applied against the trader on every fill**, and a configuration with a non-positive `SlippageN` is refused at construction.
- **Same-bar ambiguity resolves pessimistically**: buys fill before sells, so a bar covering both an entry and the stop that entry sets is entered and *then* stopped, and a bar reaching both a rung and a stop adds the Unit and *then* stops the Campaign. Competing sells fill worst-price-first. Knowledge overrides pessimism: an order that gapped through its level executed at the open, which is a fact the bar states rather than an ambiguity to resolve — which is also why an order created by a fill *inside* the bar takes that fill's price, not the bar's open, as its gap reference.

Fills are stamped at the bar's own period end, inside the window the reducer polices from both sides; the order of fills within a bar is carried by the order they are delivered in, not by their timestamps, because a daily bar cannot say what time of day anything happened.

The loop numbers the composed input stream itself — bars and the fills interleaved around them — because only it knows the running order once fills are interleaved, and `replay.Engine.Run` refuses a stream with a gap. That stream is the journal: replaying it through a fresh reducer reproduces the same decisions, which is what makes it evidence rather than a summary.

A run's last input is `replay.run.completed`, stating the instant the input stream ran to. The reducer handles it by expiring every proposal still outstanding, so a proposal raised by the final bar — which no later bar can supersede under ADR 0011 — still reaches exactly one terminal event. Carrying the fact as an input envelope rather than as a method the engine calls after the last input is what keeps two invariants intact: every emission has a causing input to name in `CausationID`, and a replay of the journal's own input stream reproduces the expiries, because the event that caused them is in that stream.

## Journal

`cmd/backtest` composes the reducer, the fill simulator, a bar source and `internal/journal`, and contains no rules. The journal it writes is a header line — the run's configuration hash and strategy version, both derived from the configuration it ran (ADR 0016), the span of input event times covered, the chain algorithm and the format version — followed by one record per line: each input, then the decisions that input caused, each record stating which of the two it is (`kind`) and carrying a hash chained over the previous record's hash, that kind, and the canonical bytes of its own envelope (ADR 0017). Decisions are stamped by `replay.Stamp`, the same function `Engine.Run` uses, so a replay of the journal's inputs reproduces the recorded decisions byte for byte.

The chain lives in the record and never on the envelope: a chain field on an envelope would make the same decision hash differently depending on its position in a journal, and byte-identical replay would become unsatisfiable. The `kind` is inside the chain because replay equivalence reads it to decide what to feed in and what to compare against, so a record relabelled from decision to input would otherwise change that check's meaning undetectably. `journal.Verify` recomputes it and reports the first broken link by sequence. That is tamper-evidence, not tamper-proofing, and it is a different question from replay equivalence — see ADR 0017 and `docs/running-a-backtest.md`.

## Local transport

Envelopes cross the Python-to-Go boundary over a Unix-domain socket carrying newline-delimited JSON, one request and one reply at a time per connection. Local gRPC was measured against it and rejected: at the universe size this system runs it was no faster, worse at p99, and cost 37 modules of transitive dependency. ADR 0014 records the measurements and the constraints the choice imposes — notably that a socket on a macOS bind mount cannot be reached from inside the container, that a reply must be checked against the bar it answers, and that a connection whose exchange did not complete must be abandoned rather than reused.

The Go side of the boundary is the `transport` package. It lives outside `internal/` because the domain must not import networking, and `depguard`'s `domain-purity` rule enforces that.

## Safety invariants

- If the Go decision engine is unavailable, the adapter submits no new orders.
- Duplicate decision and order identifiers must be idempotent.
- Startup requires reconciliation against brokerage holdings and open orders.
- Material reconciliation differences force safe mode.
- Only one active executor may submit orders.
- Consequential transitions are durably recorded before derived projections are trusted.
- Live use remains prohibited until paper-trading and limited-live gates pass.

## Deferred decisions

The following are intentionally not selected in the initial scaffold:

- PostgreSQL schema and migration tool;
- observability vendor;
- cloud provider and deployment topology;
- brokerage; and
- Kubernetes or service decomposition.

These decisions require evidence from the corresponding GitHub issues (see #4 and `docs/agents/issue-tracker.md`).
