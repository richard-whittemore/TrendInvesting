# Trend Investing

Trend Investing is a stock-first research and trading platform for evaluating:

1. a source-verified Turtle Trading control adapted to stocks;
2. a rules-based Sublime Trading control; and
3. predeclared hybrid experiments.

The project prioritizes research correctness, deterministic replay, and safe paper trading. It is not a claim that any strategy will be profitable.

## Architecture direction

- **Go** owns strategy rules, portfolio risk, state transitions, replay, persistence interfaces, and telemetry.
- **LEAN** owns market-data delivery, brokerage integration, orders, fills, calendars, and corporate actions.
- A deliberately thin **Python adapter** will translate between LEAN and versioned messages understood by Go.
- Every consequential input and output is recorded as an ordered, versioned event.
- PostgreSQL will store durable events and rebuildable operational projections.

The first deployment is a modular monolith on one host. Microservices and Kubernetes are deferred until measured operational needs justify them.

## Repository status

This initial foundation contains the event contract, deterministic replay sequencing, tests, CI, and architectural documentation. Strategy semantics are intentionally not implemented until the source-grounded specifications in the Linear roadmap are frozen.

## Quick start

Requirements: Go 1.24 or newer.

```sh
make check
```

This runs the same formatting, analysis, race-enabled tests, coverage, vulnerability, dependency, and build gates used by CI. See [docs/architecture.md](docs/architecture.md), [docs/development.md](docs/development.md), and [docs/dependency-policy.md](docs/dependency-policy.md) before adding production behavior.

## Safety

This software is under active development. It must not be used to place live orders until the paper-trading and limited-live readiness gates are explicitly satisfied.
