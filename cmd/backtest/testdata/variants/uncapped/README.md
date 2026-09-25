# Declared Variant: uncapped

Declared by the owner's decision of 2026-09-24 (ADR 0005 and ADR 0020, as
amended), before this golden was generated. The Baseline rests every entry and
Add as a **stop-limit** order at its level, capped at level + 1N. A gap above
the cap does not fill, and the Unit's affordability check and cash hold use
that worst-case price, with slippage and commission. This Variant keeps
Faith's original **stop-market** entry [T p.18]: a gap fills at the open,
however far it gapped, and a Unit is checked and held at its cost at its
level, as the reducer did before RulesVersion 1.10.0. Its only safeguard
against an unfundable fill is the account refusing one. It exists for the
head-to-head comparison ADR 0012 requires, so that the cap's effect is
measured rather than argued. The gap buffer k is a configuration parameter,
and other values (0.5, 2) are testable the same way.

`configuration.json` copies the existing backtest fixture configuration
except for `strategy_id = uncapped`, `buy_order_type = stop-market` and
`gap_buffer_n = 0`. Those three are one dimension: a stop-market order has no
cap, so it states no gap buffer. Its 20/10 channels are the existing
fixture's shortened warm-up, not a change to the source-fixed Baseline 55/20
channels.

The inputs are `../../bars_gap_above_cap.json`: the repository-authored
synthetic `../../bars.json`, with one bar changed. The breakout bar of
2026-01-22 opens at 128.50 with a low of 128.20 (it was 127.01 and 126.90),
and its high and close are unchanged. The Entry Channel it breaks is 127.01
and N is 1.0, so the Baseline's cap is 128.01, and this bar opens above it
and never trades back down to it:

- **Baseline:** the stop-limit does not fill on 2026-01-22. The proposal
  expires with the next bar (ADR 0011), whose own breakout of the new 129.01
  channel enters at that level
  (`TestTheBaselineSkipsTheGapTheUncappedVariantFills`).
- **This Variant:** the stop-market order fills at the open, 128.50 plus
  0.05 N of slippage, 128.55. Unit 2's rung, 129.05, is not reached that day.
  It is reached, and declined for cash at its cost at its level (5,000 ×
  129.05 = 645,250), on each of the ten days after.

No third-party market data is included. These invented bars exercise engine
behaviour; they are not a source-derived performance scenario. No fit, regime
evaluation or opening of ADR 0012's out-of-sample window is involved.

`TestDeclaredVariantGolden/uncapped` runs the full command pipeline with a
fixed `+test` build identifier, the named Variant and a registry. It asserts
that every proposal is a stop-market order with no cap, and that an entry
fills above the level + 1N the Baseline would cap it at. It then verifies the
journal and its registry attribution, compares both artefacts byte for byte,
and replays the recorded inputs. `TestTheUncappedVariantChecksAUnitAtItsLevel`
pins, on the golden bars, that this Variant declines an Add for its cost at
its level while the Baseline declines it for its hold. The registry under this
directory is a golden **test fixture snapshot**, not an entry in the research
run registry.

Run without updating:

```sh
go test ./cmd/backtest -run '^TestDeclaredVariantGolden$/^uncapped$' -count=1 -v
```

Regenerate only after reviewing why decisions changed:

```sh
go test ./cmd/backtest -run '^TestDeclaredVariantGolden$/^uncapped$' -count=1 -update -v
```
