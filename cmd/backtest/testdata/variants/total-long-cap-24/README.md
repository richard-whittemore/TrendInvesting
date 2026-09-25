# Declared Variant: total-long-cap-24

ADR 0008 predeclares total-long caps of 24 and 36 Units; ADR 0012 permits
one changed dimension. This Variant changes only `max_units_total_long` to
24, plus its identifying `strategy_id`. The Baseline remains 12.
`registry.WiderTotalLongCap` applies that declaration to any common control
without changing another setting.

This command fixture follows the existing Variant pattern: it shares
`../../bars_four_units.json` and the common synthetic configuration (20/10
channels, generous industry/sector caps, universe gate off). Those fixture
settings are not a diversified Baseline. It opens one four-Unit Campaign;
cap-boundary coverage is in `TestTotalLongCapsProperty` for 12, 24 and 36.
The golden pins journal decisions and registry attribution, verifies the
hash chain, and replays the journal. No third-party data is used.

Run `go test ./cmd/backtest -run 'TestDeclaredVariantGolden/total-long-cap-24'`.
Reviewed regeneration adds `-update`. The registry here is test evidence,
not a research evaluation or an out-of-sample opening against real data.

The current classification seam assigns all instruments to the shared
Unclassified Group. Its unchanged Baseline cap of 10 binds before any of
these total-long caps; every nonempty book currently reports peak sector
concentration of 1. Diversified comparison and all five ADR 0012 adoption
criteria await #42. No Variant adoption or rejection is claimed.
