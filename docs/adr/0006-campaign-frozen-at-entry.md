# ADR 0006: A Campaign's N and Unit size are frozen at first entry

- Status: Accepted
- Date: 2026-09-07

## Context

Faith does not say whether later Units of a Campaign use the N that existed at the first entry or the N at the time of each Add. Turtles received a fresh Unit sheet weekly [T p.15], so in practice N drifted within a multi-week Campaign, yet the Add Ladder is explicitly measured from actual fills [T p.19] and the printed Gold and Crude ladders [T p.20] are computed from a single N.

Freezing has a known directional bias. Breakouts follow consolidations, which are low-volatility by nature, so N at entry is systematically lower than N during the trend that follows. A frozen Campaign therefore locks in a comparatively large Unit and a comparatively tight Protective Stop, and carries both into the phase where volatility expands.

## Decision

At first entry a Campaign records its **campaign N** and its **Unit share count**, and every subsequent Add, the entire Add Ladder, and the entire Stop Ladder are computed from those frozen values. The Campaign's N is not recomputed while it is open.

Because of the bias described above, **"recompute N at each Add" is the first Variant to be run**, not a late one. If freezing hurts, the evidence appears immediately.

## Consequences

- The full ladder is computable from one number at entry, so it can be tested directly against Faith's printed examples and explained in a single journal entry.
- No feedback loop exists in which a mid-Campaign volatility spike shrinks later Units and distorts the ladder.
- The frozen values are part of Campaign state and must be journaled; replay depends on them, not on recomputation.
- The known bias is accepted knowingly, and measured by the first ablation rather than assumed away.
