# Review guidance for TrendInvesting

This repository is a research and trading platform that will eventually place orders with **real money**. Correctness, determinism, and auditability outrank brevity, cleverness, and performance in every trade-off. Review accordingly: a comment that prevents a silent capital-safety defect is always worth making, even if the code "works".

Read `AGENTS.md`, `CONTEXT.md`, and `docs/methodology/Methodology_Analysis.md` before judging strategy code.

## The failure modes that matter most here

These are not hypothetical — every one of them occurred in the predecessor prototype and is documented in `docs/methodology/Plan_and_Ticket_Review.md`.

**Look-ahead bias.** Any indicator, channel, or signal must be computed from *completed prior bars only*. A Donchian channel read after the current bar has updated it silently becomes "close ≥ today's own high", which almost never triggers. If a calculation could see the bar it is deciding on, flag it.

**Wrong volatility smoothing.** `N` is the 20-day *Wilder* average of True Range — `N = (19·PDN + TR) / 20`, seeded with a 20-day simple average of TR. It is **not** a simple moving average of True Range, and it is **not** an average of price. Flag either substitution.

**Risk multiplication when pyramiding.** Each added Unit must not be granted a fresh full risk budget. Aggregate open risk after a proposed addition must be computed and checked against the per-stock and portfolio limits before the order is proposed. Flag any sizing path that risks *n* × the per-trade budget once *n* Units are open.

**Request-driven state.** Position, stop, and pyramid state may change **only** in response to a recorded fill or brokerage event — never on the assumption that a submitted order was filled, and never from a quoted price. Flag any state mutation that happens at order-submission time.

**Close-only stop checks.** A stop evaluated against the daily close lets price trade through the level intraday and recover, and understates gap risk. Protective intent must be explicit, and backtests must model gap-through-stop behaviour.

**Silent strategy drift.** Turtle baseline and Sublime/hybrid variants are distinct configurations. Turtle adds every ½N with a 2N stop; Sublime adds every 1 ATR with a ≈3×ATR stop and only once the prior position is risk-free. Flag any code that blends these, or that hard-codes one where a named, versioned configuration parameter belongs.

## Determinism

The Go domain (`internal/`) is a deterministic reducer over an ordered event stream. Given the same inputs, configuration, and code version, it must produce identical decisions. Flag anything that breaks that:

- `time.Now()`, wall-clock reads, or timers influencing a decision — time must arrive in the event.
- `math/rand` global state, or any unseeded randomness.
- Iteration over a `map` where the order affects output — sort the keys.
- Goroutine scheduling or concurrency affecting decision order.
- Network, filesystem, or database access inside a strategy calculation.
- Unstable identifiers: a decision ID must be reproducible on replay.

## Layering

- `internal/` domain packages must not import LEAN, database, transport, or HTTP packages.
- `cmd/` is composition only — no rules.
- `adapter/lean/` translates events and submits validated orders. It must contain **no** methodology: no sizing, no pyramid state, no drawdown logic, no portfolio-risk decisions.

## Numbers and money

- Be explicit about decimal versus float. Never compare prices for equality with `==`; never accumulate money in a way that lets rounding drift.
- Share quantities are integers; state the rounding direction (the Turtle rule truncates).
- A zero, negative, or not-yet-warm volatility value must fail closed — never size a position from it.

## Tests

TDD is mandatory here. Flag a PR that adds strategy behaviour without a test that would have failed before the change.

- Golden scenarios must be transcribed from the primary sources, not invented. Cite them (e.g. "The Turtle Rules p.20, Gold ladder").
- Cover the edge cases that cost money: gaps through entry and stop, partial fills, insufficient history, splits and dividends, delisting, duplicate and out-of-order events, sequence gaps.
- Prefer table-driven tests and property assertions over example-only tests for sizing and risk invariants.

## Documentation and provenance

Any constant that encodes a strategy rule (55, 20, 2N, ½N, 4 Units, 10 %/20 % drawdown) needs a comment citing its source. If a change would depart from a rule recorded as DISCLOSED in `docs/methodology/Methodology_Analysis.md`, that is a specification change, not an implementation detail — say so rather than approving it.

## What not to comment on

- Formatting, import order, and naming already enforced by `gofmt`, `go vet`, and `staticcheck` in `make check`.
- Preferences with no correctness or auditability consequence.
- The `docs/methodology/*.transcript.*` files — machine transcripts, reviewed separately.
