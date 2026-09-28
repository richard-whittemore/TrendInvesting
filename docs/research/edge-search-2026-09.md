# Edge-search report, September 2026

This report is written for the project owner, not for a quant. It avoids
jargon where it can and explains the terms it has to use. The code behind
almost every number is in `research/edge-search/` (see that folder's own
`README.md` for how to run each script on QuantConnect Cloud); this report
states the numbers, not the code. The "Boost" re-test with realistic margin rates is rebuilt, rather than
stored, because its rate table is FRED data and the repository never
stores market data: run `research/edge-search/timing/build_realistic_boost.py`
with a local copy of FRED's TB3MS CSV to regenerate the exact script
variant used (see that file for the recipe). That run charged each month its own average T-bill rate from the first day of the month, a tiny use of future information in the financing cost only; the builder's `--prior-month` option avoids it. Re-run that way on the holdout, the result is unchanged: +16.18% a year, with financing of $61,074 against $61,034.

## Purpose

The engine and ticket work on this project was postponed on 2026-09-27
until a strategy is shown to beat simply buying and holding the market
("Prove an edge first"). This is that investigation. It asks one question,
across a wide range of published, source-verified trading rules: **does
anything here reliably do better than buying and holding the S&P 500, on
data the rule was never tuned against?**

"Reliably" matters as much as "better." A rule that only wins on the exact
years and exact settings it was tuned on is not an edge, it is overfitting.
So every strategy below is tested three ways: first tuned freely against
older data, then checked for whether nearby settings still work (not just
one lucky combination), and finally run once, unchanged, on newer data it
never saw during tuning.

## Protocol

Three stages, in order:

1. **In-sample.** Every rule is tried, and its settings adjusted, against
   data from 1998 or 1999 through 2015. This is where tuning is allowed.
2. **Robustness.** A result counts for more if changing a setting
   slightly — a different entry threshold, a different lookback — gives a
   similar answer. A result that only works at one exact setting, with
   worse results just next to it, is a red flag for overfitting, not a
   discovery.
3. **A single holdout run.** Once a rule's settings are fixed from stages
   1 and 2, it is run exactly once, unchanged, on 2016-to-mid-2026 data it
   was never tuned against. This is the only fair test of whether the rule
   actually works going forward, because it is the only data the tuning
   process never got to see.

**Why the holdout can be "spent."** The value of a holdout period comes
entirely from the fact that it was never used to choose anything. The
moment a result from it is used to pick between strategies, or to justify
changing a rule, that holdout stops being a fair test — it has been
"spent," the same way a secret stops being secret once it's shared. Looking
at a holdout result once, to see if the rule survives, is normal. Looking
at it repeatedly while adjusting the rule is the same mistake as tuning on
it directly, just slower. This report flags every place a holdout number
was looked at more than once, because that is exactly the case where the
result deserves more skepticism, not less.

## Data sources and costs

| Source | Used for | Cost |
| --- | --- | --- |
| QuantConnect Free plan | US equities, ETFs, and point-in-time fundamentals, 1998–present | Free |
| Pinnacle CLC continuous futures database | 19-market Turtle futures backtest, 1980–2015 | $99, one time (bought 2026-09-27) |
| FRED (TB3MS, 3-month Treasury bill rate) | Cash/financing rate proxy | Free |
| Azure VM (cloud research runs) | Compute for backtests too large for local machines | Under $1 |

QuantConnect's own free futures data was tried first and found unusable
(see "What held up," below) — the Pinnacle purchase followed only after
that free option failed.

## Results tables

All figures are CAGR (compound annual growth rate) / max drawdown (the
worst peak-to-trough decline the strategy would have suffered), unless
noted. SPY figures are total return (dividends reinvested) unless stated
otherwise.

### Stocks: Turtle rules, in-sample 1998–2015 (top 200–500 US stocks by dollar volume)

| Variant | CAGR / Max drawdown |
| --- | --- |
| Faithful Turtle rules | −5.74% / 87% |
| + SPY-200-day-SMA regime filter | −0.35% / 52% |
| + 0.1% Unit sizing | −0.26% / 36% |
| + both filters | −1.26% / 31% |
| Unlimited pyramiding | Worse (hit the 10,000-order cap) |
| **SPY, 1998–2015** | **+6.07% / 55%** |

The Turtle rules, faithful to the source or with these variants, did not
beat buying and holding on stocks over this span.

### Stocks: Sublime reconstruction, in-sample

| Variant | CAGR / Max drawdown |
| --- | --- |
| Control | +0.38% / 15.4% |
| Best variant (252-day channel) | +1.37% / 18.8% |
| Range across 10 configurations tried | −0.29% to +1.37% |

Positive, but small, and well below SPY's +6.07% over the same kind of
span.

### ETF rules, in-sample

| Rule | CAGR / Max drawdown | Robust range (8 variants) |
| --- | --- | --- |
| GEM (Global Equities Momentum) | +7.6% / 21.5% | 6.9%–8.75% |
| Faber (10-month SMA rotation) | +5.5% / 18.6% | 5.1%–6.1% |

Both beat SPY's drawdown by a wide margin (21.5% and 18.6% vs. SPY's 55%
over the comparable stock span), and both held up across nearby settings —
a sign this isn't a single lucky configuration.

### Stock factors, 1999–2015 (SPY over the same span: +4.9% / 55%)

| Factor | CAGR / Max drawdown | Robust range (5 variants) |
| --- | --- | --- |
| Momentum + trend filter | +18.45% / 49% | 16.7%–20.2% |
| Value (earnings yield) | +13.6% / 61% | — |
| Quality (gross profit / assets) | +11.1% / 55% | — |
| Momentum + quality | +10.9% / 59% | — |
| Low volatility | +7.7% / 38.5% | — |
| Momentum (no trend filter) | +7.0% / 73% | — |

Momentum with a trend filter is the standout, by a wide margin, and its
robustness range (16.7%–20.2% across nearby settings) is a real signal this
is not a fluke of one exact setting.

### The equal-weight effect (isolating how much of the above is just "not being cap-weighted")

| | 2003–2015 | 2009–2015 |
| --- | --- | --- |
| RSP (equal-weight S&P 500, buy and hold) | +10.6% | +16.9% |
| SPY (cap-weighted) | +8.65% | — |
| Value factor | — | +17.6% |
| Quality factor | — | +17.9% |

By 2009–2015, the value and quality factors (+17.6%, +17.9%) are barely
ahead of simply holding the equal-weight index (+16.9%). **Most of the
apparent factor edge in that period is the equal-weight effect, not stock
selection.** This is a caveat on the factor results above, not a separate
finding.

### Timing effects, 1999–2015

| Rule | CAGR / Max drawdown | Note |
| --- | --- | --- |
| RSI-2 (buy oversold dips) | +5.9% / 14.5% | Only 10% of days invested |
| Halloween effect (Nov–Apr) | +6.1% / 32% | |
| Turn of the month | +3.7% / 18% | |
| Overnight (close to next open) | +1.3% / 45% | Real trading costs would erase this |
| Faber rotation, 1.5x leverage | +6.0% / 27.5% | |
| Faber rotation, 2x leverage | +7.0% / 35.8% | |

RSI-2's low drawdown (14.5%) while spending only 10% of the time invested
is notable: most of its return came from a small number of days, with the
rest of the time in cash-like holdings.

**RSI-2 robustness (nearby settings):**

| Setting | CAGR / Max drawdown |
| --- | --- |
| Entry threshold 5 (stricter) | +4.1% |
| Entry threshold 15 (looser) | +5.3% |
| Exit at RSI 70 (instead of price crossing its average) | +7.3% / 15% |
| No trend filter | +8.5% / 22% |
| 2x leverage | +7.0% / 26% |

RSI-2 works across a reasonable range of nearby settings, not just the one
chosen — a good robustness sign.

### Risk parity, 2006–2015

| Variant | CAGR / Max drawdown |
| --- | --- |
| Unlevered | +5.6% / 10.7% |
| Scaled to 10% target volatility | +7.09% / 21.7% |
| **SPY, same span** | **+7.05% / 55%** |

Risk parity's headline return roughly matched SPY, but with far less
drawdown (21.7% or less, vs. SPY's 55%) — a smoother ride, not a bigger
number.

### Futures

QuantConnect's own free futures data was tried first and found unusable:
its ES (S&P 500 futures) history only goes back to 2007, and stitching
together a continuous price series across contract rolls left a gap that
would not reconcile. This is why the Pinnacle CLC database was purchased.

**Turtle rules on 19 futures markets, 1980–2015, Pinnacle data, 1% Unit sizing, with T-bill interest on cash:**

| Period | CAGR |
| --- | --- |
| Overall, 1980–2015 | +10.4% (max drawdown 99.5%) |
| 1980s | +86%/yr |
| 1990s | +11.6%/yr |
| 2000–2008 | −17.0%/yr |
| 2009–2015 | −25.8%/yr |
| 2003–2015 | −22.7%/yr |
| S&P 500 price-only, 2003–2015 (comparison) | +6.7% |

The overall 1980–2015 number is dominated by the 1980s; every later period
shown lost money, badly, and the 99.5% max drawdown means the strategy
would have lost essentially the entire account at its worst point over the
full run. With trading costs stripped out entirely, 2003–2015 still loses
about 19%/yr — this is not a costs problem, the rule itself stopped
working. At a much smaller position size (0.25% Unit instead of 1%),
2003–2015 was −3.5%/yr — smaller losses, but still losses.

**Time-series momentum (TSMOM, the trend-following approach from
Moskowitz, Ooi & Pedersen's 2012 paper), 41 markets, 1985–2015, scaled to a
10% volatility target, with interest:**

| Metric | Value |
| --- | --- |
| CAGR / Max drawdown | +13.73% / 17.2% |
| Sharpe ratio (return per unit of risk) | 0.96 |
| Correlation with the S&P 500 | About 0 |
| 2009–2015 only | +5.6% |

*Futures figures reflect the corrected local backtester (PR #269): monthly
T-bill rates are applied from the following month (no look-ahead), the TSMOM
10% overlay is measured on the realised book (active days only, weights effective after their fill), and contract sizing uses the
fill date's multiplier. These corrections moved the numbers slightly; none
changed a conclusion.*

TSMOM's full-period number is strong and, importantly, close to unrelated
to the stock market's own ups and downs. But its more recent sub-period
(2009–2015, +5.6%) is much weaker than the full-period average, an early
warning echoed by the holdout result below.

## Held-out scorecard, 2016 to mid-2026

**SPY over this span: +14.95% / 33.7% max drawdown (return ÷ drawdown: 0.44).**
This is the bar every strategy has to clear, on data none of them were
tuned against.

| Strategy | CAGR / Max drawdown | Return ÷ drawdown | Note |
| --- | --- | --- | --- |
| GEM | +8.0% / 33% | — | Below SPY |
| Faber | +5.8% / 9.8% | 0.59 | Below SPY on return, much smaller drawdown |
| Momentum + trend filter | +13.9% / 46.9% | — | Close to SPY's return, worse drawdown |
| RSI-2 | +4.57% / 9.5% | 0.48 | 11% of days invested |
| Risk parity, 10% vol target | +7.67% / 20.9% | 0.37 | −11.2% in 2022 |
| TSMOM (2016 to 2025-10) | +3.59% / 19.0% | Sharpe 0.19 | Barely above holding cash; +14.0% in March 2020 |
| Turtle futures, 1% Unit (2015-01 to 2025-10) | −12.4% / 93% | — | Confirms the in-sample futures breakdown |
| Turtle futures, 0.25% Unit | +0.4% / 46% | — | Barely positive, at much smaller size |

**None of the single strategies above beat SPY's return on the holdout.**
Several (Faber, RSI-2) delivered a much smoother ride for a lower return —
useful in its own way, but not what "beats buy-and-hold" means here.

### Combinations

Two combination strategies were also tested — built in-sample, then run
once on the holdout like everything else.

**"Core":** 80% SPY, plus a 20% RSI-2 sleeve (the remaining 20% sits in
SHY, a short-Treasury ETF, except when RSI-2 is signalling).

| Period | CAGR / Max drawdown |
| --- | --- |
| In-sample, 1999–2015 | +5.41% / 41.4% (SPY: +4.91% / 55%) |
| Holdout, 2016–mid-2026 | +12.6% / 27.2% |

**"Boost":** 100% SPY, increased to 150% SPY while RSI-2 is signalling,
with a flat 3% annual financing charge on the borrowed portion.

| Period | CAGR / Max drawdown |
| --- | --- |
| In-sample, 1999–2015 | +6.88% / 53.2% |
| Holdout, 2016–mid-2026 | **+16.24% / 34.5%** |

**"Boost" is the only strategy in this study that beat SPY on the
holdout** — by about 1.3 percentage points a year, with almost the same
drawdown (34.5% vs. SPY's 33.7%).

## What held up

- **ETF trend rules (GEM, Faber)** were robust in-sample across several
  variants, and their holdout drawdowns stayed much smaller than SPY's,
  even though their holdout returns fell short of SPY.
- **Momentum with a trend filter** was the single strongest in-sample
  factor result, robust across nearby settings, and came reasonably close
  to SPY's return on the holdout (+13.9% vs. +14.95%), but with a much
  deeper drawdown (46.9% vs. 33.7%), so it still did worse than SPY on risk.
- **RSI-2** was robust across a range of entry/exit settings in-sample and
  kept a low drawdown on the holdout, at the cost of a lower return.
- **"Boost" (SPY + RSI-2 leverage overlay)** is the one strategy that beat
  SPY on the holdout, in both the original 3%-flat-financing version and
  the realistic-rate re-test below.
- **Futures trend-following (Turtle rules) did not hold up**: strong only
  in the 1980s, losing steadily from 2000 on, in-sample and on the
  holdout, regardless of position size.
- **TSMOM (a different futures trend approach) was strong on paper
  (1985–2015) but weakening**, both in its own later sub-period (2009–15)
  and on the holdout (2016–2025, essentially cash-like returns).

## Boost re-test with realistic margin rates

The original "Boost" result used a flat, hypothetical 3% annual financing
charge on the borrowed portion of the position, while real margin rates
were meaningfully higher for part of the holdout period (roughly 6–7% in
2023–2025). This re-test replaces that flat rate with a realistic one.

**Method.** The financing rate used each day was the monthly FRED TB3MS
rate (the 3-month Treasury bill rate) for that month, plus 1.5 percentage
points, charged daily on the borrowed (margin) portion of the position —
about 6.7% in mid-2023, moving with the T-bill rate over the rest of the
span. The FRED rate table itself is not committed to this repository (see
"Caveats," below); the script variant with the table embedded is not
committed either — only the original flat-rate `boost` mode in
`research/edge-search/timing/main.py` is preserved here.

**Results:**

| Period | CAGR / Max drawdown | Financing paid |
| --- | --- | --- |
| In-sample, 1999–2015 (SPY: +4.91% / 55%) | +6.85% / 53.2% | $52,869 |
| Holdout, 2016–mid-2026 (SPY: +14.95% / 33.7%) | +16.18% / 34.5% | $61,034 |

**Conclusion.** The edge survives realistic margin rates essentially
unchanged: about +1.2 percentage points a year over SPY on the holdout,
against +1.3 points under the original flat 3% assumption. The strategy
only borrows on about 11% of trading days (the fraction of time RSI-2 is
signalling), so the financing cost is small relative to the account either
way.

**Remaining caveats on this re-test**, unchanged from the original result:

- Taxes on short-term trades are not modelled — this strategy would trade
  in and out of a leveraged position roughly 100 times over the study
  period, and in a taxable account, short-term gains are taxed as ordinary
  income.
- Leverage risk: a crash that starts while the position is already
  boosted to 150% would hurt more than these backtested numbers show if it
  moved faster or gapped further than anything in this data.
- The holdout period has now been looked at multiple times (the original
  Boost result, and this re-test) — see "Caveats," below, on what that
  means for how much to trust the result.
- This is still a backtest, with a backtest's general limitations (also
  below).

## Caveats

- **The stock-factor scripts let weights drift and do not cap gross
  exposure.** To stay inside QuantConnect's 10,000-order limit, the factor
  runs (momentum, momentum with trend filter, value, quality, low
  volatility, and the equal-weight control) trade only names entering or
  leaving the portfolio. Continuing holdings are not trimmed back to equal
  weight, and a new entry is sized at its target weight of the whole
  account, so after strong gains total exposure can creep above 100% on
  the margin account. The factor results therefore describe a drifting,
  sometimes slightly levered portfolio, not a strict monthly equal-weight
  one. Exposure above 100% amplifies both gains and losses, so the drift
  could have pushed these results in either direction; without re-running
  the factors with strict rebalancing, its net effect is unknown. The
  factor conclusions rest mainly on the held-out and equal-weight-control
  comparisons, not on the exact in-sample figures.
- **The risk-parity volatility window can be one day short** when an asset
  has exactly 60 closes (its first months of data); the completed runs were
  unaffected in practice, but the script is not hardened against it.

- **Taxes are not modelled anywhere in this report.** Every return figure
  is pre-tax. A strategy that trades often (RSI-2, "Core," "Boost") would
  owe short-term capital gains tax on most of its trades in a taxable
  account, which would reduce its real-world return more than a
  buy-and-hold strategy's.
- **Leverage carries risk beyond what a backtest can show.** "Boost" and
  the levered Faber variants borrow money to hold more than 100% of the
  account in stocks. A backtest only shows what happened in the past; a
  fast, larger, or differently-timed crash than anything in 1998–2026
  could lose more than these numbers suggest, including a forced sale at
  the worst possible moment (a margin call).
- **The financing rate used matters, and was a simplification in the
  original Boost result** (flat 3%, when real rates went higher) — see the
  re-test above, which found the conclusion holds up under realistic
  rates.
- **The 2016+ holdout has been used more than once.** It was looked at
  for the original scorecard above, and again for the Boost financing
  re-test. Each additional look, even without changing the rule itself,
  makes it a slightly less pure out-of-sample test. This is disclosed
  rather than hidden, but it means "Boost beat SPY on the holdout" should
  be read as encouraging, not as proof.
- **These are backtests, with backtests' usual limitations**: they assume
  orders fill at modeled prices with modeled slippage and commission, not
  necessarily what a real broker would have given; the underlying
  ETFs/futures existed as this exact instrument the whole time (a rule
  applied to a fund that didn't exist yet uses a proxy — see each script's
  own header); and a rule that worked over 1998–2026 has no guarantee of
  continuing to work.

## Conclusions

- Most published rules tested — Turtle on stocks, Turtle on futures,
  Sublime, most single stock factors, most timing effects on their own —
  did not beat buying and holding the S&P 500 on data they weren't tuned
  against.
- A meaningful share of the stock-factor results (value, quality) is
  explained by the equal-weight effect rather than genuine stock
  selection, once compared against an equal-weight buy-and-hold control.
- Futures trend-following, in both the Turtle and TSMOM forms, looked
  strong decades ago and has been getting weaker, not stronger, in every
  more recent period tested, including the holdout.
- One combination, "Boost" (SPY with a leveraged overlay while RSI-2
  signals an oversold dip), beat SPY on the holdout by about 1.2–1.3
  points a year, with a comparable drawdown, and that result survived a
  re-test using realistic margin rates. It is the one candidate this study
  found worth taking seriously — with the caveats above, especially taxes,
  leverage risk, and a holdout that has now been consulted twice.

## What this means for the Go engine project

The Go engine and ticket work were postponed on 2026-09-27 specifically
until a strategy was shown to beat buy-and-hold. "Boost" is the first
result in this investigation that clears that bar on held-out data. That
is not, by itself, a decision to resume engine work — the caveats above
(taxes, leverage risk, a holdout used twice, and the general limits of any
backtest) are real open questions, not fine print to wave past — but it is
the first result worth building a decision around, rather than a reason to
keep searching from scratch. The next step is the owner's call: whether
"Boost," with its caveats resolved or accepted, is enough to justify
resuming engine work, or whether more validation (a longer holdout, live
paper trading, a tax-aware re-run) should come first.
