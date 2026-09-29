# Edge-search follow-up, September 2026

This is the follow-up to `docs/research/edge-search-2026-09.md` (the "main
report"), written for the project owner. After the main report the owner
asked for four things. First, a closer look at "Boost", the one rule that
beat SPY on the held-out years. Second, whether mixing other strategies in
with the index helps when the market turns bearish. Third, whether any
other futures method shows an edge, and whether shorting helps. Fourth,
what Boost would cost to run in a real account. This report answers each
one. The code behind every number is in `research/edge-search/` (see
"Reproducing these runs", below).

Nothing here is personal investment advice. It is research on historical
data, with the limits listed under "Caveats".

## The short answer

- **Boost holds up.**
  - Its edge over SPY comes from its timing, not from its extra leverage:
    holding the same average amount of extra SPY all the time earns less.
  - It beat buy-and-hold on four other US stock ETFs it was never tuned on,
    before and after 2016.
  - It beat buy-and-hold under every nearby setting tried.
  - It stays ahead of SPY at retail margin rates (T-bill + 9 points) and
    with generous trading costs.
  - It does **not** reduce losses in a bear market. Its drawdowns are
    SPY's.
- **For bear markets, the index plus a futures trend overlay worked.** Both
  before and after 2016 it earned more than the index with smaller losses.
  Run directly, though, it needs millions of dollars. The managed-futures
  ETFs that package the idea for small accounts have short records that
  look good mainly because of 2022. WTMF, the one fund with a longer
  record (2011 onward), added nothing.
- **Futures carry** worked from 1985 to 2015 and then failed after 2016.
- **Shorting did not clearly help.** Shorting the index lost money against
  cash. 130/30 stock momentum beat long-only on the tuning years, but with
  deeper drawdowns and more leverage, and it has not been tested on the
  held-out years.

## Protocol

As in the main report:

- Rules are tuned only on 1998–2015 (1985–2015 for futures). 1998 is the
  indicators' warm-up year, so every stock and ETF figure labelled
  "in-sample" or "1999–2015" below is measured from January 1999.
- Nearby settings are checked on the same years.
- The held-out years (2016 to mid-2026, or to October 2025 for the futures
  files) are run once, with settings fixed beforehand.

**SPY's held-out years have now been looked at many times** (the original
Boost result, its realistic-margin re-test, and the runs here). The
strongest new evidence for Boost is therefore **not** its SPY holdout
number. It is the other ETFs, which were never used for tuning, and the
same-leverage control.

Every run below that uses borrowed money pays a margin rate on the borrowed
part, charged daily: the previous month's 3-month T-bill rate (FRED TB3MS)
plus a spread. The spread is 1.5 points unless stated. Using the previous
month's rate means no future information goes into the cost. All ETF prices
are QuantConnect's dividend-adjusted closes, so buy-and-hold figures are
total returns.

## 1. Boost, looked at closely

**The rule.**

- Hold SPY at 98% of the account.
- When SPY closes above its 200-day average and its 2-day RSI is below 10,
  raise the position to 147% (1.5 × 98%), borrowing the difference.
- Go back to 98% when SPY closes above its 5-day average.

The RSI-2 entry and 5-day exit are Connors & Alvarez (2008); using them to
raise an existing position, rather than to buy from cash, is this project's
own combination. The rule is boosted on about 10% of trading days, usually
for a few days at a time.

### Is it timing, or just leverage?

Boost's average exposure is 1.03 times the account, so some of its gain
could simply be "a little more stock, in a market that went up". The
control holds a constant 1.03 times SPY, pays the same margin rate, and is
rebalanced monthly.

| 1999–2015 / 2016–mid-2026, per year | Boost | Constant 1.03× SPY | Constant 1.08× SPY (1.10 target) | SPY |
| --- | --- | --- | --- | --- |
| In-sample | 6.86% | 4.91% | 4.90% | 4.91% |
| Holdout | 16.43% | 15.52% | 15.98% | 15.19% |

Boost beats the same-leverage control by about 2 points a year in-sample
and 0.9 points on the holdout. Even 1.08× constant leverage, more than
Boost ever averages, earns less.

The day-by-day numbers show where the gain comes from. On days that
followed a boost, SPY's average close-to-close return was **+0.165%**,
against **+0.029%** on all other days (1998–mid-2026). The extra exposure
is concentrated on the days SPY did best.

### Does it work on markets it was never tuned on?

The same rule, unchanged, was run on five other ETFs from their
QuantConnect start dates. Buy-and-hold figures are each ETF's own total
return.

| ETF | 1999–2015: Boost / buy-and-hold | 2016–mid-2026: Boost / buy-and-hold |
| --- | --- | --- |
| QQQ (Nasdaq-100) | 7.53% / 5.37% | 21.88% / 20.81% |
| IWM (Russell 2000) | 8.87% / 7.35% | 11.93% / 11.48% |
| DIA (Dow) | 6.97% / 6.18% | 14.78% / 13.42% |
| MDY (S&P MidCap) | 10.91% / 8.92% | 13.10% / 11.75% |
| EFA (developed ex-US) | 8.71% / 4.92% | 8.72% / 8.98% |

Boost wins 9 of the 10 comparisons. The miss is EFA after 2016, where the
edge on boosted days was also the weakest measured (+0.057% a day against
+0.031%). The rule looks like a property of US stock indexes, not something
tuned to fit SPY.

### Is it one lucky setting?

All runs are on SPY and end in 2015, measured 1999–2015; SPY buy-and-hold over the same years is 4.91%.

| Change | CAGR per year |
| --- | --- |
| Boost size 1.25× / **1.5×** / 2.0× | 5.85% / **6.86%** / 8.84% |
| RSI entry below 5 / **10** / 15 / 20 | 6.01% / **6.86%** / 6.49% / 6.40% |
| Exit on the 3 / **5** / 10-day average | 5.84% / **6.86%** / 7.90% |
| No 200-day filter | 7.80% (with higher volatility) |

Bold is the committed setting. Every variant beats buy-and-hold. Some
variants did better than the committed setting in-sample (a 10-day exit,
no trend filter, a larger boost). They were not adopted: choosing the best
in-sample variant after seeing the results is exactly the overfitting the
protocol guards against. A larger boost also means more leverage risk.

### Trading costs and margin rates

Both tables are on SPY, compared with SPY buy-and-hold (1999–2015: 4.91%;
2016–mid-2026: 15.19%).

| Slippage per fill | 1999–2015 | 2016–mid-2026 |
| --- | --- | --- |
| None (base) | 6.86% | 16.43% |
| 0.05% | 6.47% | 15.96% |
| 0.10% | 6.08% | 15.49% |

SPY's actual bid-ask spread is around 0.005% or less, so 0.05% and 0.10%
per fill are deliberately generous.

| Margin rate | 1999–2015 | 2016–mid-2026 | Financing paid, 1998–2026, $1M start |
| --- | --- | --- | --- |
| Futures-style (T-bill + 0.4) | 6.91% | 16.49% | $0.21M |
| T-bill + 1.5 (base) | 6.86% | 16.43% | $0.28M |
| T-bill + 3 | 6.78% | 16.34% | $0.38M |
| T-bill + 6 | 6.63% | 16.16% | $0.56M |
| T-bill + 9 | 6.48% | 15.98% | $0.73M |

Borrowing happens on only about 10% of days, so even a retail margin rate
costs a few tenths of a point a year. At T-bill + 9, roughly what large
retail brokers charge small accounts, Boost still stays ahead of SPY.

### Bear markets

Boost is a return enhancer, not a protector.

| | Boost | SPY |
| --- | --- | --- |
| Worst drawdown, 1998–mid-2026 | 53.2% | 55.2% |
| 2008 | −35.1% | −36.1% |
| 2022 | −19.7% | −18.8% |

It still buys dips while SPY is above its 200-day average, and the start
of a bear market is usually just such a dip.

## 2. Mixing in something for bear markets

### The index plus a futures trend overlay (local, Pinnacle data)

Futures only need margin, so an account can hold the whole index and also
run a futures strategy on top: an *overlay*. The overlay tested is the
time-series momentum strategy (TSMOM) from the main report, with its 10%
volatility target, on about 40 markets. It is shown at half size ("50%")
and full size. The weights were fixed before any run.

The S&P side is the S&P futures' return plus the T-bill rate, a close
stand-in for SPY's total return. It uses the full-size contract through
2015 and the E-mini for the holdout, because the full-size contract ended
in 2021. Returns are monthly, rebalanced monthly, so drawdowns are measured
at month-ends and understate intra-month lows.

| 1985–2015 | CAGR | Volatility | Worst drawdown | 2000–02 | 2008 |
| --- | --- | --- | --- | --- | --- |
| S&P 100% | 10.29% | 15.2% | 52.2% | −39.1% | −38.3% |
| S&P 80% / TSMOM 20% | 11.28% | 12.3% | 41.9% | −23.4% | −29.0% |
| S&P 60% / TSMOM 40% | 12.12% | 9.9% | 29.9% | −4.7% | −18.8% |
| **S&P 100% + TSMOM overlay 50%** | **15.76%** | 15.9% | 47.7% | −22.0% | −32.5% |
| S&P 100% + TSMOM overlay 100% | 21.16% | 18.1% | 43.1% | −1.2% | −26.7% |
| TSMOM alone | 13.73% | 10.3% | 16.3% | +73.4% | +17.6% |

| 2016–Oct 2025 (holdout, run once) | CAGR | Volatility | Worst drawdown | 2018 | 2020 | 2022 |
| --- | --- | --- | --- | --- | --- | --- |
| S&P 100% | 14.50% | 15.3% | 23.9% | −5.2% | +18.2% | −18.3% |
| S&P 80% / TSMOM 20% | 12.59% | 12.0% | 16.6% | −6.2% | +18.9% | −12.1% |
| S&P 60% / TSMOM 40% | 10.53% | 9.3% | 12.1% | −7.3% | +19.0% | −5.7% |
| **S&P 100% + TSMOM overlay 50%** | **15.76%** | 15.1% | 18.6% | −11.9% | +27.5% | −12.8% |
| S&P 100% + TSMOM overlay 100% | 16.76% | 16.4% | 26.4% | −18.4% | +36.8% | −7.1% |
| TSMOM alone | 3.59% | 9.9% | 16.5% | −10.7% | +16.0% | +14.2% |

- **The overlay did what an overlay should, in both periods.** The monthly
  correlation between the S&P and TSMOM was −0.03 before 2016 and −0.20
  after, so the overlay's losses and the index's rarely lined up.
- **At half size,** the overlay added 1.3 points a year on the holdout and
  cut the worst month-end drawdown from 24% to 19% (2022: −12.8% against
  −18.3%).
- **Blending instead of overlaying** (80/20, 60/40) cut losses more but also
  cut return after 2016, when TSMOM on its own earned about cash.
- **2018 is the counter-example.** The overlay lost on both sides at once
  that year.

### What the overlay needs in capital

The sizing works like this:

- Each market's position is 40% / its volatility / the number of markets.
- That is scaled by about 0.86 for the 10% target, then halved for a 50%
  overlay.
- For one full-size contract, the account must be large enough that this
  small slice is still worth a whole contract.

At 2026 prices, that takes $0.4M–$1M for the smallest financial markets
(the 2-year and 5-year Treasury notes, the Mexican peso). Most commodities
need $1M–$26M, and gold alone about $26M. Micro contracts (a tenth of the
size) exist for some markets and cut those figures by ten. A faithful
40-market overlay still needs several million dollars. With fewer markets
it is a different, less diversified strategy.

### Managed-futures ETFs (QuantConnect)

Several ETFs run trend-following futures programs inside a fund, so a
small account can hold one next to SPY. Each was tested:

- alone
- blended 80/20 with SPY
- as a financed overlay: 100% SPY plus 50% of the fund (147% of the
  account, paying T-bill + 1.5 on the borrowed part)
- with Boost on the SPY part

Each run starts once the fund has existed for about two months, so the
spans differ. Every row compares against SPY over the same span.

| Fund (span) | Fund alone | SPY 80 / fund 20 | SPY 100 + fund 50 | Boost + fund 50 | SPY |
| --- | --- | --- | --- | --- | --- |
| DBMF (Jul 2019–) | 7.29% | 14.20% | 17.48% | 19.72% | 15.84% |
| KMLM (Feb 2021–) | | | 15.17% | | 15.08% |
| CTA (May 2022–) | | | 17.26% | | 16.81% |
| WTMF (Mar 2011–) | 0.65% | 11.22% | 12.50% | 14.04% | 14.05% |

RSST, a single fund that holds US stocks and managed futures together:
23.21% against SPY's 25.43%, with a worst drawdown of 30.2% against 18.8%
(Nov 2023 to mid-2026).

Boost alone, for reference: 17.77% from July 2019, and 15.31% from March
2011.

| Worst drawdown and 2022 | SPY 100 + fund 50 | SPY |
| --- | --- | --- |
| DBMF (Jul 2019–) | 35.5%; 2022 −12.4% | 33.7%; 2022 −18.8% |
| KMLM (Feb 2021–) | 19.9%; 2022 −9.3% | 24.5%; 2022 −18.8% |
| WTMF (Mar 2011–) | 31.9%; 2022 −22.1% | 33.7%; 2022 −18.8% |

- **DBMF, KMLM and CTA** all helped, most of all in 2022, the strongest year
  for trend following in a decade. Their records are only 4–7 years long,
  and 2022 accounts for much of their result. That is too little to count
  as evidence of a lasting edge.
- **WTMF, the one fund with 15 years of history,** returned 0.65% a year on
  its own. It made every combination worse than plain SPY, and it lost
  money in 2022. Managed-futures funds differ a great deal from each other
  and from the TSMOM overlay above, and the category's short-record
  winners may not last.
- **RSST** trailed SPY with a much larger drawdown over its short life.
- **Boost + DBMF** was the best combination (19.72% against 15.84%). It
  mixes a small, well-tested edge (Boost) with a short-record one (DBMF),
  so treat it as a hypothesis, not a result.

## 3. Other futures methods: carry (local, Pinnacle data)

**Carry** (Koijen, Moskowitz, Pedersen & Vrugt 2018, "Carry", *Journal of
Financial Economics*) is the other widely documented futures return
besides momentum.

- **The idea:** a futures contract that is cheaper than the one after it
  earns money as it converges on the spot price, and a dearer one loses
  money.
- **The strategy:** go long markets with positive carry and short those
  with negative carry, using the same sizing, 10% target, costs and T-bill
  interest as TSMOM.
- **How carry was measured:** from the Pinnacle files. On a roll day the
  jump in (back-adjusted − unadjusted price) equals the old contract's
  price minus the new one's, so carry is measured once per roll and held
  until the next roll.

Two versions were declared before any run, and both were run on the
holdout once.

| | 1985–2015 CAGR / Sharpe / worst drawdown | 2016–Oct 2025 CAGR / Sharpe / worst drawdown |
| --- | --- | --- |
| Carry | 10.35% / 0.64 / 30.7% | 0.52% / −0.13 / 28.9% |
| Carry + momentum, averaged | 14.46% / 1.02 / 18.5% | 3.28% / 0.16 / 19.8% |
| Momentum alone (main report) | 13.73% / 0.96 / 17.2% | 3.59% / 0.19 / 19.0% |

All figures include T-bill interest. Carry was a real return before 2016,
and combining it with momentum improved every in-sample number. After 2016
carry lost money relative to cash, and the combination did slightly worse
than momentum alone. Like the other futures factors in this study, carry
has faded since about 2009.

## 4. Shorting (QuantConnect, tuning years only, measured 1999–2015)

Shorting did not clearly help on the tuning years, so no holdout was spent on it.

| Rule | CAGR | Worst drawdown | 2008 |
| --- | --- | --- | --- |
| SPY buy-and-hold | 4.91% | 55.2% | −36.1% |
| SPY: long above the 200-day average, cash below | 4.12% | 25.7% | −0.7% |
| SPY: long above, **short** below | 2.11% | 39.9% | +34.0% |
| QQQ: long above, cash below | 3.37% | 51.4% | |
| QQQ: long above, **short** below | −6.27% | 84.3% | |
| Stocks: long the 25 strongest (12-1 momentum) | 9.06% | 63.7% | |
| Stocks: long 25 strongest, short 25 weakest, market-neutral | 4.41% | 55.7% | |
| Stocks: 130% long strongest, 30% short weakest | 10.31% | 72.8% | |

- **Shorting the index in downtrends** paid off hugely in 2008 but lost more
  on false signals the rest of the time.
- **Shorting the weakest stocks** hurt badly in 2009. As markets turn up,
  the most beaten-down stocks rebound first: the "momentum crash" of
  Daniel & Moskowitz (2016).
  - The market-neutral version earned 4.41% a year with a 55.7% drawdown.
    It lost 23% a year over 2008–09 and 1.0% a year over 2009–2015, after a
    2% annual borrow fee on the shorts.
  - 130/30 beat long-only by 1.25 points a year, but with a deeper drawdown
    (72.8% against 63.7%) and 127% long exposure, so part of that gain is
    simply leverage.
  - It is a mixed result, not a case for shorting, and no holdout was spent
    on it.
- **How the stock books were traded:** every month, names entering or
  leaving the lists trade, and a continuing holding is resized once it
  drifts 25% from its target weight. That keeps each side near its stated
  allocation within the Free plan's order cap.
  - A first version traded only entries and exits, so continuing holdings
    drifted. It reported 7.32%, 4.24% and 7.69%.
  - The long and short lists never overlapped: at least 50 names had a
    score every month.

The cash in the trend rules earns no interest here, which understates both
of them a little.

## 5. What running Boost would take

This section gathers facts for the owner's own decision. It is not a
recommendation.

**Two ways to hold the extra 50%:**

1. **A margin account with SPY.** Buy the extra shares on a signal and sell
   those same lots on the exit.
   - The rate is what matters. Interactive Brokers charges about its
     benchmark + 1.5 points on small balances. Schwab and Fidelity list
     roughly 10.4%–13.3% for balances under $100,000.
   - Even at T-bill + 9, the backtest stays ahead of SPY (table above).
   - Margin is not allowed in an IRA, so this route needs a taxable
     account.
2. **Micro E-mini S&P 500 futures (MES)** for the extra part, with the core
   in SPY.
   - Each contract is $5 × the index, about $39,000 at today's level
     (about 7,800). The overnight exchange margin is roughly $2,500.
   - The financing cost is built into the futures price, close to the
     T-bill rate: the "futures-style" row above.
   - Contracts are whole, so the extra 50% is exact only at multiples of
     about $78,000 of account value. At $50,000, one contract would be 78%
     extra rather than 50%.
   - Some brokers allow futures in an IRA, with higher margin
     requirements.

**Taxes (general rules, not tax advice).**

- **Margin route:** the boosted shares are held for days, so their gains
  and losses are short-term and taxed as ordinary income. The core SPY
  position is never sold, so it keeps its long-term treatment.
- **Futures route:** US index futures are Section 1256 contracts, taxed 60%
  long-term / 40% short-term whatever the holding period, and marked to
  market at year end.
- **Rough scale of the tax cost:** Boost's extra return is about 1.2–1.6
  points a year, all of it realized in the year earned. At a 24% ordinary
  rate that costs a few tenths of a point a year in a taxable account. A
  tax professional should confirm this for the owner's own situation.

**Behaviour.** Boost buys more stock right after sharp drops. Doing that
reliably, including early in a real bear market (2008, 2022), is the part
backtests cannot test.

## Caveats

- **SPY's held-out years have been examined repeatedly,** so the SPY
  holdout figure alone is weak evidence. The untuned ETFs and the
  same-leverage control carry more weight.
- **QuantConnect's US stock data starts in 1998,** so there was no earlier
  unseen period to test Boost on.
- **Every rule decides at the daily close and trades at the next open.**
  Gaps between them are captured, but there is no intraday modelling.
- **The futures overlay and blends** use month-end marks (drawdowns
  understated), an S&P stand-in built from futures, and whole-contract
  sizing at a $1M account.
- **The managed-futures ETF records** are short and dominated by one year
  (2022).
- **The margin runs assume a margin call never forces a sale.**
  - At 147% exposure the equity is gone only after a fall of about 68%, but
    a typical 25–30% maintenance requirement would force a sale after a
    fall of roughly 55% from the boosted level.
  - The worst one-day SPY fall since 1998 was about 11%.
  - Brokers can raise their requirements in a crisis.
- **This is a backtest,** with the general limits described in the main
  report's "Caveats".

## Reproducing these runs

- **QuantConnect scripts.** Templates are in `research/edge-search/boost/`
  (`template.py`, `mix_template.py`) and `research/edge-search/shorting/`
  (`trend_template.py`, `factors_ls.py`).
  - `research/edge-search/build_variants.py TB3MS.csv OUT_DIR` writes all 43
    variants exactly as run, one `OUT_DIR/<name>/main.py` each, filling
    in each run's settings and the margin-rate table from a local FRED
    TB3MS file.
  - Paste each into a QuantConnect project and run it. The results are in
    each backtest's runtime statistics: `S …` for the strategy, `B …` for
    buy-and-hold of the traded ETF, with the span in each value.
- **Local futures scripts.** `research/edge-search/futures/carry.py` and
  `blend.py` run the carry and blend studies on the
  `research/futures-local` engine.
  - They need the Pinnacle CLC files (`PINNACLE_DIR`) and a TB3MS CSV
    (`TB3MS_CSV`). Neither is in the repository.
  - `carry.py --holdout` runs the single held-out carry run.
- **Tests.** `make research-test` runs `research/edge-search/test_followup.py`,
  which covers:
  - the builder
  - the templates' daily rules, against a stand-in for QuantConnect's
    module
  - the carry measure
