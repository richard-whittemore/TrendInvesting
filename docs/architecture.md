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

These decisions require evidence from the corresponding Linear issues.
