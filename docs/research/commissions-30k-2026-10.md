# Commission drag of the combination at $30k (2026-10)

Part of #376 (slice 7). Results recorded 2026-10-17 (below).

## Question

How much of the combination's return does Interactive Brokers' commission
take at the IRAs' size, $30,000 of starting cash, against the $1,000,000 the
recorded runs used?

## Variants

Each is the run it is named for with only `CASH` changed to 30,000.

| Variant | Same as |
| --- | --- |
| `F_COMBO_30K` | `F_COMBO` |
| `OOS_F_COMBO_30K` | `OOS_F_COMBO` |
| `FU_COMBO_30K` | `FU_COMBO` |
| `OOS_FU_COMBO_30K` | `OOS_FU_COMBO` |

## Fee model

No switch was added. `defensive_v2.py` and `fresh_universe.py` call
`SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)`,
and LEAN's `InteractiveBrokersBrokerageModel.GetFeeModel` returns
`new InteractiveBrokersFeeModel()` for every security
(`Common/Brokerages/InteractiveBrokersBrokerageModel.cs` in
QuantConnect/Lean). So the recorded $1,000,000 runs already paid IB
commissions. A base-versus-variant difference is therefore the combined
effect of the smaller account (different order sizes and holdings, and IB's
per-order minimum weighing more on small orders) and fees; it is not a
commission-only effect. The fee total of each run shows how much of the
difference commissions could account for, and a matched $30,000 run with fees
disabled would be needed to separate the two.

## Runs

The four variants were run once each on 2026-10-17 (QuantConnect project
37096083, LEAN IB brokerage model, $30,000 starting cash). Compare each with its base run's return,
drawdown and fee total, and report the result as the combined effect of
account size and fees.

Settings per variant: each variant is its base script with `CASH=30000` and
the `InteractiveBrokersBrokerage` model set by the script (see Fee model);
the `$1M` baselines keep their recorded `CASH`. This note does not archive
the rendered settings of each run; the backtest IDs in the table below
identify the four $30k runs in project 37096083.

## Results

The $1,000,000 baselines are the 2026-10-16 runs recorded in
`docs/research/universe-massive-2026-10.md`. Research only; not investment
advice.

| Run | Backtest ID | Window | CAGR | MaxDD | Ratio | Orders | Fees | Fundamentals | Borrow line |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| FU_COMBO_30K | 28188ee1f1c4776a6f68080005c9d974 | 1999-01 to 2015-12 | 14.50% | 43.06% | 0.3367 | 5528 | $8,257.05 | reads=107500 hits=194735 | resizes=572 skipped=0 retried=1615 avgCash=0.150 |
| FU_COMBO ($1M baseline) | 821115ccb7444cf44de683721cf9b756 | same | 14.78% | 43.48% | 0.3399 | 5571 | $209,986.71 | hits=194735 | resizes=584 retried=1488 avgCash=0.141 |
| OOS_FU_COMBO_30K | 94dc89773a01555742724dab76a1d04b | 2016-01 to 2026-06-30 | 18.86% | 37.22% | 0.5067 | 3778 | $3,895.99 | reads=69000 hits=126722 | resizes=632 retried=1269 avgCash=0.055 |
| OOS_FU_COMBO ($1M) | c2b3fe0bde31901df93b645eb0f19e2b | same | 18.65% | 39.47% | 0.4725 | 3816 | $34,140.36 | hits=126722 | resizes=626 retried=769 avgCash=0.034 |
| F_COMBO_30K | c52fbafbe949ecbb9b734214ec755fa8 | 1999-01 to 2015-12 | 12.78% | 45.22% | 0.2826 | 5146 | $6,974.84 | reads=107500 hits=194862 | resizes=620 retried=973 avgCash=0.149 |
| F_COMBO ($1M) | eee58c08c960a253047aab3b075c75f0 | same | 13.09% | 46.05% | 0.2843 | 5187 | $161,655.35 | hits=194862 | retried=894 avgCash=0.139 |
| OOS_F_COMBO_30K | fbf473515fdfa63f359329ea0c0a9c2e | 2016-01 to 2026-06-30 | 20.15% | 38.70% | 0.5206 | 3546 | $3,658.11 | reads=69000 hits=126718 | resizes=686 retried=831 avgCash=0.057 |
| OOS_F_COMBO ($1M) | a752a0ae4155142464abb218b826abc9 | same | 20.06% | 40.47% | 0.4957 | 3589 | $34,224.92 | hits=126718 | resizes=691 retried=506 avgCash=0.036 |

SPY: 1999-2015 4.91% / 55.17%; 2016-2026-06 15.19% / 33.72%.

Data state: the Fundamentals counters equal the $1M baselines pairwise (FU
194,735 / 126,722 and F 194,862 / 126,718). These matches support, but do not
prove, that the $30k runs used the baselines' 2026-10-16 data state. The
counters count readable nonzero fields; they do not compare the underlying
values, names, sectors or prices, and the referenced
`docs/research/r4-rerun-discrepancy-2026-10.md` ties the baselines, not these
four 2026-10-17 runs, to that state. It does show that the revised state was
unchanged on 2026-10-17 (its rerun pair matches statistic for statistic).

### Deltas ($30k minus $1M)

| Pair | CAGR (pp) | MaxDD (pp) | Ratio | Orders | Fees |
| --- | --- | --- | --- | --- | --- |
| FU, 1999-2015 | -0.28 | -0.42 | -0.0032 | -43 | -$201,729.66 |
| FU, 2016-2026-06 | +0.21 | -2.25 | +0.0342 | -38 | -$30,244.37 |
| F, 1999-2015 | -0.31 | -0.83 | -0.0017 | -41 | -$154,680.51 |
| F, 2016-2026-06 | +0.09 | -1.77 | +0.0249 | -43 | -$30,566.81 |

A negative MaxDD delta is a shallower drawdown. The fee deltas mostly reflect
the account size, not the commission rate.

### Fee per order

| Run | Fees / orders | Per order |
| --- | --- | --- |
| FU_COMBO_30K | $8,257.05 / 5,528 | $1.49 |
| OOS_FU_COMBO_30K | $3,895.99 / 3,778 | $1.03 |
| F_COMBO_30K | $6,974.84 / 5,146 | $1.36 |
| OOS_F_COMBO_30K | $3,658.11 / 3,546 | $1.03 |

### Reading

A $30k account differs from $1M by -0.28 and -0.31 pp/yr in the tuning years
(FU and F, 1999-2015) and +0.21 and +0.09 pp/yr held-out (2016-2026-06), with
shallower drawdowns (-0.42, -2.25, -0.83 and -1.77 pp). The difference bundles the $1 commission floor
with whole-share sizing, visible as higher avgCash (15% vs 14% in-sample, 5.5%
vs 3.4% held-out) and more retried buys. These comparisons cannot confirm
or reject the independent review's 0.6-1.2 pp/yr estimate for commission
drag: both account sizes paid commissions, and holdings differ. A matched
no-fee run would separate the two effects and was not run.

### Recommendation

Use $2.00 per order as the planner's fee allowance: above every observed
average ($1.03 to $1.49), below the $2.50 that 500 shares would cost
($0.005 x 500). To be confirmed in paper trading.
