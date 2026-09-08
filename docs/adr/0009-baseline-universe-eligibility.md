# ADR 0009: The Baseline universe is deliberately permissive, and losing eligibility never closes a Campaign

- Status: Accepted
- Date: 2026-09-08

## Context

Faith's only universe rule is liquidity [T p.10]. Sublime adds a $20 price floor, a 1 M-share volume floor, and a 5–10-year history requirement — rules that belong to the Sublime Variant, not the Turtle Baseline. If those were baked into the Baseline, their effect could never be measured.

## Decision

An instrument is **eligible** for the Baseline universe when it is common stock on a US primary exchange (no ETFs, ADRs, or SPACs), its price is at least $5, its 20-day median dollar volume is at least $5 M, and it has at least 250 completed bars of history. Eligibility is re-evaluated **point-in-time on the first trading day of each month**.

**Losing eligibility never closes an open Campaign.** A Campaign ends only through the Exit Channel, the Protective Stop, or a delisting; a delisting is a forced exit at the last available price, journaled as a distinct exit reason.

## Consequences

- Sublime's stricter filters become measurable ablations.
- Universe membership changes only through declared criteria on declared dates, so it can be reproduced exactly on replay.
- The thresholds are parameters and are sensitivity-tested.
