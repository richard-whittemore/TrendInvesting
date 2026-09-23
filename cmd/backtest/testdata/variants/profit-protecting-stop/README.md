# Declared Variant: profit-protecting-stop

Declared before generating this golden: the Stop Multiple is **0.1N**, a
singleton choice borrowed from `TestProfitProtectingCampaignStopExitsCleanly`.
ADR 0012 classifies a change to the source-fixed 2N stop as a Variant. The
mechanistic hypothesis is that half-N Stop Ladder raises can move this narrow
stop above the Campaign's average entry, and that exiting there is valid
(`CONTEXT.md`: "risk-free"). This is a software regression scenario, not a
parameter search or an adoption proposal.

`configuration.json` copies the existing backtest fixture configuration except
for `strategy_id = profit-protecting-stop` and `stop_multiple = 0.1`. Its 20/10
channels are the existing fixture's shortened warmup, not a change to the
source-fixed Baseline 55/20 channels. All other parameters, including half-N
Adds and the four-Unit limit, retain their existing behavior.

The inputs are the repository-authored synthetic `../../bars.json`, spanning
2026-01-02 through 2026-02-02. No third-party market data is included. These
invented bars exercise engine behavior; they are not a source-derived trading
performance scenario. There is no fit, regime evaluation, or opening of ADR
0012's 2016-01-01 out-of-sample window. The research ablation order and adoption
criteria remain unchanged.

`TestDeclaredVariantGolden` runs the full command pipeline with a fixed `+test`
build identifier, the explicitly named Variant, and a registry. It asserts a
Stop Ladder raise and a stop exit at or above average entry, verifies the
journal and its registry attribution/chain anchor, compares both artifacts
byte-for-byte, and replays the recorded inputs. The registry under this
directory is a golden **test fixture snapshot**, generated alongside the golden
journal; it is not an entry in the research run registry. Actual research runs,
including failed runs, remain append-only under ADR 0012/0018.

Run without updating:

```sh
go test ./cmd/backtest -run '^TestDeclaredVariantGolden$' -count=1 -v
```

Regenerate only after reviewing why decisions changed:

```sh
go test ./cmd/backtest -run '^TestDeclaredVariantGolden$' -count=1 -update -v
```

Read both the journal's inputs/decisions and the registry diff before accepting
an update. The existing `../../journal.golden.jsonl` must remain unchanged.
The guard compares behavior, not source text, so a pure refactor needs no
golden update or RulesVersion bump (ADR 0016). A failure reports a possible
rules change and the first decision difference, even if a validator caused
the run to stop early. Investigate the cause; do not re-pin reflexively.
