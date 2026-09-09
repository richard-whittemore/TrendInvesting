# ADR 0012: Experiment protocol — span, regime windows, out-of-sample discipline, and what "winning" means

- Status: Accepted
- Date: 2026-09-08

## Context

The Baseline has no tunable parameters; every number is Faith's. Variants do — the 4PS thresholds, Grade cut-offs, cap sizes, the recompute-N rule — and tunable parameters are how hindsight leaks into a backtest. The best-looking result is usually the most over-fitted one. This record fixes, in advance, how results are produced and judged so that no choice is made after seeing the equity curve.

## Decision

**Span and regimes.** Backtests run over the full available history (QC equity data begins 1998-01) and are additionally evaluated over seven named Regime Windows: 1998–2000 late bull, 2000–02 bear, 2003–07 bull, 2008–09 crash, 2009–19 bull, 2020 COVID, 2022 correction. The windows are fixed here and not revised after results are seen.

**In-sample / out-of-sample.** A hard split at **2016-01-01**. Variant parameters are chosen only on 1998–2015, from a grid declared before the run. The out-of-sample window is opened **exactly once per Variant**; a second look is a new hypothesis and is recorded as one. Walk-forward evaluation (rolling five-year fit, two-year test) is a robustness check in addition, never a substitute.

**A Variant is adopted only if it clears all of:**

1. Better out-of-sample annualised return divided by maximum drawdown than the Baseline on the same span.
2. The improvement points the same way in at least five of the seven Regime Windows.
3. It survives ±30 % perturbation of every threshold it introduced.
4. It does not reduce the number of independent Campaigns below 60 % of the Baseline's.
5. It does not increase peak sector concentration.

**"No Variant wins" is a legitimate outcome.** Every result — adopted, rejected, and failed — is retained under its configuration hash, as `docs/development.md` already requires.

**Ablation order.** The first two Variants run are "recompute N per Add" (ADR 0006) and the wider total-long cap (ADR 0008), because each addresses a known, mechanistically-predicted weakness of the Baseline.

## Consequences

- Return per unit of drawdown, not CAGR, is the primary metric: it measures return per unit of pain, and a strategy that would have been abandoned mid-drawdown is not a good strategy.
- The regime and perturbation rules filter out regime luck and coincidental thresholds; the trade-count floor and concentration rule filter out hindsight selection and accidental sector bets.
- Every run's configuration, span, split, and outcome are journaled, so an adopted Variant can be shown to have cleared every rule.
