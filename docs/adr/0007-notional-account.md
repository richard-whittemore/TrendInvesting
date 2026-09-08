# ADR 0007: The Notional Account is re-based yearly and recovers only at the yearly start

- Status: Accepted
- Date: 2026-09-08

## Context

Faith's Turtles sized positions from a notional account that Dennis re-based each January, and reduced it by 20 % each time equity fell 10 % of the original, trading at the reduced size until equity regained the yearly starting figure [T p.17]. A self-managed account has no Dennis, so *original*, *yearly*, and the effect of deposits must be defined. The earlier prototype instead reset its drawdown ladder at every new equity high — a materially different, more aggressive strategy.

## Decision

The Notional Account is a configured starting equity, **re-based to actual equity every 1 January**. Within a year the drawdown ladder is measured against that year's starting figure: a 10 % fall multiplies the Notional Account by 0.8; a further 10 % fall of the *reduced* figure multiplies by 0.8 again, exactly Faith's $1 M → $800 k → $640 k. The full Notional Account **returns only when actual equity regains the yearly starting figure**, never on a new high-water mark. Deposits and withdrawals scale the yearly starting figure proportionally on the day they occur, so they can neither trigger nor mask a Drawdown Step. Every step and re-basing is journaled.

## Consequences

- Sizing shrinks quickly in a losing year and does not re-expand on a partial recovery, which is the Faith behaviour and is deliberately conservative.
- The high-water-mark reset is a declared Variant, not the Baseline.
- The yearly re-basing date is a parameter so the sensitivity to it can be measured.
