# Declared Variant: recompute-n-at-add

Declared by ADR 0006, with mechanics specified in its 2026-09-25 amendment,
under ADR 0012's single-dimension protocol. `configuration.json` differs from
the common backtest configuration only in `strategy_id` and
`recompute_n_at_add = true`. All other parameters, including this fixture's
shortened 20/10 channels, are unchanged.

Each Add uses preceding-bar Wilder N for sizing, half-N spacing, its price cap,
slippage, cash hold, new Unit stop and half-N raises of earlier stops. The entry
sizing account stays fixed so N is the only changed sizing input. A standing
proposal retains its own N through fill; opening Campaign N remains separately
journalled and continues to normalise Campaign results.

`bars.json` copies `../../bars_four_units.json`, changing only the January 22
breakout's high from 39.01 to 37.20 and close from 38.81 to 37.10 in both price
views. That prevents all four Units filling on the entry day with the same N.
The next day's changed N then produces a differently sized Add. These are
repository-authored synthetic bars, not third-party data or performance evidence.

`TestDeclaredVariantGolden/recompute-n-at-add` checks the single configuration
dimension, requires a resized Add with a distinct `add_n`, verifies journal and
registry attribution, compares both artifacts byte for byte, and replays the
recorded inputs. `TestRerunGoldens` also regenerates fills through the full
pipeline. The registry here is a test snapshot, not the research registry.

Run: `go test ./cmd/backtest -run '^TestDeclaredVariantGolden$/recompute-n-at-add'`.
Reviewed regeneration uses the same command with `-update`.

ADR 0012 adoption criteria are out of scope: they await the diversified
Baseline backtest and Regime Window evaluation. No win/loss conclusion,
threshold fit or out-of-sample evaluation is claimed.
