# ADR 0012: Experiment protocol — span, regime windows, out-of-sample discipline, and what "winning" means

- Status: Accepted
- Date: 2026-09-08

## Context

Tunable parameters are how hindsight leaks into a backtest, and the best-looking result is usually the most over-fitted one. This record fixes, in advance, how results are produced and judged, so that no choice is made after seeing the equity curve.

The protection the Baseline needs is often misstated as "every Baseline number comes from Faith." That is not true, and stating it that way invites an implementer to "correct" a deliberate adaptation back to the source value. The Baseline contains numbers Faith never wrote: the Unit Volatility Fraction is 0.5 %, half his (ADR 0003), and the Universe thresholds are ours entirely (ADR 0009).

The accurate statement is narrower and stronger: **no Baseline parameter was chosen by looking at results.** Every Baseline number is either transcribed from the source or justified by an argument made before any backtest was run. That is why the Baseline needs no in-sample/out-of-sample protection, and why Variants do.

## Decision

### Parameter provenance

Every parameter falls into exactly one of three categories, and its category determines who may change it and how.

**Source-fixed** — transcribed from `The Turtle Rules` and not changed in the Baseline: the 55-bar Entry Channel and 20-bar Exit Channel, the Stop Multiple of 2, the half-N Add spacing, the four-Unit maximum, the 4/6/10/12 Unit caps, and the 20 %-per-10 % Drawdown Step. Changing one of these produces a Variant, never a corrected Baseline.

**Baseline-declared adaptation** — chosen by this project because Faith's futures rules do not map onto long-only equities unchanged, each justified by a recorded argument rather than by a result: the 0.5 % Unit Volatility Fraction (concentration argument, ADR 0003), the Universe eligibility thresholds (deliberately permissive so Sublime's stricter filters stay measurable, ADR 0009), the fill model (ADR 0005), 0.05 N slippage (ADR 0013), and the yearly re-basing date (ADR 0007). These are fixed for the Baseline; changing one is a declared Variant.

**Variant-tunable** — thresholds a Variant introduces, and the only parameters ever chosen by fitting: the 4PS thresholds, Grade cut-offs, alternative cap sizes, and the recompute-N rule. These, and only these, are what the in-sample/out-of-sample discipline below exists to protect.

An implementer who finds a Baseline number that differs from the source should expect to find its justification in the ADR that set it. If there is no such justification, that is a defect in the record, not a licence to change the number.

### Protocol

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
