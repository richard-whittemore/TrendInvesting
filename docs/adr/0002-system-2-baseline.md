# ADR 0002: System 2 is the Turtle baseline

- Status: Accepted
- Date: 2026-09-06

## Context

Faith's *Original Turtle Trading Rules* give two related systems [T p.18–19]. System 1 enters on a 20-day breakout and exits on a 10-day reversal, but skips a signal when the *previous* breakout in that instrument would have been profitable, falling back to a 55-day failsafe. System 2 enters on a 55-day breakout, exits on a 20-day reversal, and takes every signal. Turtles allocated between them at their own discretion.

The project needs a Baseline whose every decision can be reconstructed from retained inputs.

## Decision

The Baseline is **System 2**: a 55-bar Entry Channel, a 20-bar Exit Channel, and every Breakout taken.

## Consequences

- No phantom state. System 1's filter requires tracking hypothetical breakouts that were *not* taken and classifying each by whether price moved 2N against it before a profitable 10-day exit — a second, path-dependent simulation running alongside the real one. System 2 has none, which keeps the reducer small and its golden tests tractable.
- The 55/20 horizon produces fewer, longer trades, closer to Sublime's 12–18-month holds, so the later hybrid comparison is between comparable time frames.
- System 1 (and a System 1 / System 2 allocation) becomes a declared Variant once the engine is trusted; it is not lost, only deferred.
