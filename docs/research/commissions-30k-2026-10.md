# Commission drag of the combination at $30k (2026-10)

Part of #376 (slice 7). Results are pending: nothing here has been run.

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

## Planned runs

The four variants, run once each on the same data as their bases, after the
review of this change is cleared. Compare each with its base run's return,
drawdown and fee total, and report the result as the combined effect of
account size and fees.

## Results

Pending.
