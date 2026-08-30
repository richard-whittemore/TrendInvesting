# ADR 0001: Begin with a modular monolith

- Status: Accepted
- Date: 2026-08-29

## Context

The system needs independent testing, durable audit state, and operational control beyond QuantConnect Cloud. It does not yet have evidence that multiple independently deployed services or Kubernetes are necessary.

## Decision

Begin with one Go application and a thin Python LEAN adapter on the same host. Keep domain packages independent of transport and persistence implementations. Use container or service-manager restart policies only after idempotency, reconciliation, and safe recovery are implemented.

## Consequences

- Local development and replay stay simple.
- Strategy behavior can be tested without LEAN.
- Operational complexity is constrained during research and paper trading.
- Components may later be separated without rewriting domain rules if measured scaling or reliability needs justify it.
