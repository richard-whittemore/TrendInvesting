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

- exact local transport (Unix-domain gRPC is preferred but not frozen);
- PostgreSQL schema and migration tool;
- observability vendor;
- cloud provider and deployment topology;
- brokerage; and
- Kubernetes or service decomposition.

These decisions require evidence from the corresponding Linear issues.
