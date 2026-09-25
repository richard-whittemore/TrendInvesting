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


## Declared Variant mechanics: recompute-n-at-add (2026-09-25)

The Baseline decision above is unchanged. The separate configuration dimension
`recompute_n_at_add = true`, declared as `recompute-n-at-add`, selects these
mechanics, without changing half-N spacing, the 2N stop or any Unit cap:

- At each Add opportunity use Wilder N from the completed bars **preceding**
  its decision bar. The current bar's range never sizes its own resting order.
- Recalculate whole-share Unit size with the entry proposal's sizing inputs,
  substituting only that N. In particular, preserve the entry proposal's
  Notional Account, before share truncation; multiplying a rounded opening
  Unit by a ratio of Ns would lose information. Changing the account basis at
  each Add would introduce a second dimension. Cash affordability still uses
  the current eligible snapshot, fill debits and holds (ADR 0020).
- Compute the rung from the preceding actual fill plus half this N. Use the
  same N for the price cap, Add slippage and cash hold (ADR 0005/0013/0020).
- Retain that operand and quantity with the proposal until it fills or expires.
  A proposal surviving the next Session under ADR 0011 keeps its authorised N;
  a new fill-chained proposal reads the N for its own decision bar.
- Set the new Unit's stop from its actual fill minus Stop Multiple times this
  N. Raise each earlier Unit's existing stop by half this same N, including
  gapped fills. Do not reset earlier stops to the newest Unit's stop.
- Decline a recomputed Unit smaller than one share or with a non-positive stop
  intent. Existing cap, cash, partial-stop and fill reconciliation guards stand.

Journal `add_n` separately on Add proposals, Unit additions and stop changes;
`campaign_n` and the opening Unit quantity remain immutable reference values.
Absent/zero `add_n` selects frozen Baseline arithmetic. Positive `add_n` selects
this Variant operand. Configuration schema 8 makes the switch explicit; Add
proposal schema 4, Unit-added schema 2 and stop-set schema 3 carry the operand.
Whole-Campaign results continue to normalise by opening N and opening Unit;
exit slippage retains its existing opening-N basis (ADR 0013). No claim of
per-Unit return normalisation is introduced.

These are synthetic mechanics checks only. Adoption, diversified Baseline
comparison and Regime Window evaluation remain pending; no ADR 0012 criterion
has been evaluated and no out-of-sample research window has been opened.
