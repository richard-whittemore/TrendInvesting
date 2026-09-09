# Development guide

## Principles

1. Keep strategy and risk calculations deterministic and side-effect free.
2. Treat source methodology rules separately from experimental variants.
3. Record enough immutable evidence to explain every accepted and rejected decision.
4. Fail closed on unknown schemas, missing sequences, stale data, or uncertain brokerage state.
5. Prefer table-driven tests and replay fixtures over behavior hidden inside LEAN callbacks.

## Local checks

Run the same checks used by CI:

```sh
make check
```

Go code must be formatted with `gofmt`. New behavior should include focused tests, including failure cases and invariant checks.

## Package boundaries

- `cmd/` contains executable composition only.
- `internal/event/` owns the shared event envelope and transport-level validation.
- `internal/replay/` owns deterministic application of recorded events.
- `internal/indicator/` owns pure, side-effect-free strategy arithmetic (True Range, N) with no knowledge of events or replay.
- `internal/strategy/` owns the `replay.Handler` reducers that turn a validated event stream into decision events.
- Future strategy packages must not import LEAN, database, or transport implementations.
- `adapter/lean/` documents and will contain the deliberately thin Python boundary.

## Pull requests

Each change should reference its GitHub issue (`#N`) and state:

- the rule, risk, or operational outcome addressed;
- the evidence and tests added;
- replay or audit impact;
- any new schema or configuration version; and
- unresolved assumptions.

Never hide a failed research result by rewriting or removing its recorded configuration and evidence.
