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
- **Shorting the index did not help.** It lost money against simply
  moving to cash.
- **A correction (October 2026): every stock-picking figure below was
  understated.** The backtests skipped buying any stock that joined the
  universe on the rebalance day, so that money sat in cash; in about a
  third of months the book was 20–40% cash. The fixed figures for the
  headline momentum versions are in "The unpriced-buy fix" (section 4).
  The conclusions below hold, with somewhat higher returns and deeper
  drawdowns. Tables elsewhere in section 4 keep the figures as originally
  run.
- **Taxes:** in a taxable account the momentum versions lose about 2–4
  percentage points a year to federal tax (most gains are short-term),
  Boost about 0.7 and SPY buy-and-hold about 0.3. In an IRA there is no
  such cost ("Taxes: taxable account or IRA").
- **Concentrated stock momentum was the strongest result of the whole
  search, but also the riskiest.** Long the 25 strongest stocks, rebalanced
  monthly, returned 27.6% a year on the held-out years after 0.2% slippage
  per trade, against SPY's 15.2%. 130/30 returned 31.1%. On the tuning
  years it beat SPY by only about 4 points, with 64–73% drawdowns, so the
  held-out years were an unusually good period for it (section 4).
  - **25 names is a stable choice:** 15 and 50 give similar results.
  - **A bond filter changes which period it does well in.** Moving to bonds
    whenever SPY is below its 200-day average lifted the tuning years to
    21% a year, but on the held-out years it fell to 17.3% against 29.5%
    without the filter.
  - **A volatility crash guard** cut drawdowns but cost more return than
    it saved. Sector caps did the same.
  - **Moving half the book to bonds in a downtrend** was the best balance.
    It made 15.6% on the tuning years and 23.6% on the held-out years, with
    40–46% drawdowns.
  - **The fixed combination** (sector cap 3, inverse-volatility weights and
    the half filter) had the shallowest losses: 31% and 28% worst
    drawdowns, below SPY's, while making 10.5% and 18.8% a year.

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

The index and stock-momentum rules were first run on the tuning years
only. After the owner asked, the two stock-momentum books that beat SPY
there (long-only and 130/30) were then run once on the held-out years, with
the settings unchanged ("Stock momentum on the held-out years", below).

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
  - On the tuning years alone that was a mixed result, not a case for
    shorting.
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

### Stock momentum on the held-out years

The rules and settings are exactly those of the tuning-year runs above:
- 25 names a side, and the 25% rebalancing band
- for 130/30, a 2% borrow fee

Each run starts in January 2015 so that the indicators warm up, and
returns are measured from January 2016. Each was run once. The cost-stress
runs then re-ran the same fixed rules with a fixed slippage on every fill
and, for 130/30, a 5% borrow fee. That changes only the assumed costs, not
the rules.

| 2016–mid-2026 | CAGR | Worst drawdown |
| --- | --- | --- |
| SPY | 15.19% | 33.7% |
| Long the 25 strongest | 29.50% | 38.7% |
| … with 0.1% / 0.2% slippage per fill | 28.56% / 27.61% | 39.8% / 40.3% |
| 130/30 | 34.99% | 47.7% |
| … with 0.1% / 0.2% slippage and a 5% borrow fee | 32.43% / 31.08% | 47.8% / 47.0% |

| Calendar-year return | 2016 | 2017 | 2018 | 2019 | 2020 | 2021 | 2022 | 2023 | 2024 | 2025 | 2026 H1 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Long the 25 strongest | +8.9% | +8.2% | −10.1% | +32.0% | +78.4% | +18.0% | +2.2% | +29.9% | +40.0% | +53.8% | +74.6% |
| 130/30 | −1.2% | +5.9% | −12.6% | +35.7% | +95.8% | +11.5% | +26.6% | +21.7% | +58.3% | +62.0% | +110.4% |

(Calendar years are from each run's daily equity curve, without slippage.)

**What drove it.** The 130/30 run's gain came from 846 different stocks.
The biggest single contributor (Bloom Energy) was about 9% of the total.
The leaders were the period's actual momentum winners: Micron, Seagate,
SanDisk and Lumentum in the 2025–26 memory and optical boom, and AppLovin,
Palantir, Robinhood, Rocket Lab, Carvana and MicroStrategy. No single
stock or data error explains the result.

**How much weight to put on it:**

- **It beat SPY on both sets of years and survives realistic trading costs,
  in line with decades of published momentum research.**
- **The held-out years were unusually good for momentum.** The speculative
  and AI-led markets of 2019–2026 gave +78% in 2020 and +75% in the first
  half of 2026, for long-only. On the tuning years the same rule beat SPY by
  about 4 points a year (9.06% against 4.91%), with a 64% drawdown and a
  −31% a year 2008–09. A long-run expectation should sit much nearer the
  tuning-year result than the held-out one.
- **The result depends heavily on design.** The main report's momentum rule
  held 50 names, traded only entries and exits, and moved to bonds when
  SPY was below its 200-day average. It earned 13.9% on these same held-out
  years. "Momentum robustness and a crash guard" (below) measures each
  design choice on the tuning years:
  - The number of names and the rebalancing matter little.
  - The bond filter matters a great deal, in opposite directions before
    and after 2016. Its single held-out run (17.34% against 29.50%
    without it) accounts for most of the gap.
- **Practical costs.**
  - Monthly turnover makes nearly all gains short-term in a taxable
    account.
  - 130/30 needs a margin account and shares to borrow. Some of its shorts
    (MSTR, the quantum-computing and space names) have at times cost far
    more than 5% a year to borrow.

### Momentum robustness and a crash guard (tuning years first)

The owner asked whether 25 names was a stable choice or a lucky one, and
whether a crash guard helps. Every run below is on the tuning years only,
1998–2015 measured from 1999 (SPY: 4.91%, worst drawdown 55.2%), except the
single held-out run at the end of this section. The template is
`research/edge-search/momentum/momentum.py`: `shorting/factors_ls.py` plus
two switches, so the earlier runs keep their own unchanged file. Its 25-name
banded run reproduces the earlier 9.06% exactly.

**Names held, rebalancing and the bond filter** (long-only; CAGR / worst
drawdown). "Band" resizes a held name once it is 25% off target; "entries
and exits" never resizes. "Bond filter": when SPY closed the month below its
200-day average, the whole book went to IEF (7–10-year Treasuries) for the
month.

| Names | Band, no filter | Entries and exits, no filter | Band + bond filter | Entries and exits + bond filter |
| --- | --- | --- | --- | --- |
| 15 | 6.37% / 67.5% | 4.73% / 75.2% | 19.54% / 48.5% | 19.01% / 47.8% |
| 25 | 9.06% / 63.7% | 7.32% / 72.8% | **21.28% / 44.6%** | 20.64% / 50.9% |
| 50 | 8.55% / 61.3% | 7.02% / 72.7% | 18.48% / 42.6% | 18.45% / 49.2% |

- **The number of names matters little.** 25 is slightly best in every
  column, but 15 and 50 are close, so 25 is not a lucky outlier.
- **The band adds about 1.5 points** a year and trims the drawdown.
- **The bond filter dominated the tuning years.** It added 10–14 points a
  year and cut the worst drawdown by about a third.
  - It sat in bonds for 59 of the 204 months, mostly 2000–02 and 2008–09.
  - That turned −25% to −41% a year over 2008–09 into +6% to +13%.
- **Cross-check:** the 50-name entries-and-exits run with the filter
  reproduces the main report's "Momentum + trend filter" (18.45% / 49%)
  exactly, and its no-filter twin reproduces that report's 7.0%.

**Crash guard (volatility scaling).** Each month the book was scaled down
so that its trailing six-month volatility was at most 20% or 30% a year,
and never scaled above 1. This follows the spirit of Barroso & Santa-Clara
(2015), "Momentum has its moments", *Journal of Financial Economics*. The
volatility was measured on the book about to be held, not on the factor's
past returns.

| 25 names, banded | No guard | Guard at 30% | Guard at 20% |
| --- | --- | --- | --- |
| Long-only | 9.06% / 63.7% | 6.97% / 52.4% | 5.72% / 44.8% |
| 130/30 | 10.31% / 72.8% | 8.74% / 58.7% | 6.89% / 44.3% |

A scale change of 10% or more from the one last applied resizes every
holding, so the guard caps the book actually held. A first version left
continuing holdings to the 25% band; its figures were within 0.2 points of
these.

The guard did cut the drawdown: 2008–09 went from −31.5% to −16.0% a year
for long-only at 20%. But it cost more return than it saved.
- The cash it frees earns nothing in these runs. At about 30% of the book
  in cash on average, that costs well under a point a year, which does not
  close the gap.
- The bond filter did better on both return and drawdown, so it is the
  better tuning-year choice.

**The tuning-year winner on the held-out years (run once).** Under the
protocol, the best tuning-year setting (25 names, band, bond filter) was
run once on 2016 to mid-2026, unchanged.

| 2016–mid-2026 | CAGR | Worst drawdown | 2018 | 2019 | 2020 | 2022 | 2025 | 2026 H1 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| SPY | 15.19% | 33.7% | | | | −18.8% | | |
| 25 names, band, **bond filter** | 17.34% | 46.8% | −10.3% | +1.6% | +79.2% | −21.7% | +19.4% | +36.0% |
| 25 names, band, no filter (above) | 29.50% | 38.7% | −10.1% | +32.0% | +78.4% | +2.2% | +53.8% | +74.6% |

**The filter hurt on the held-out years** by 12 points a year, and its
drawdown was deeper.
- **Why the filter helped before 2016:** the tuning years' bear markets
  (2000–02 and 2008–09) were long and slow. A monthly 200-day test gets out
  early and stays out.
- **Why it hurt after:** the held-out years' sell-offs were fast and
  V-shaped (late 2018, March 2020, 2022, April 2025). The filter sold after
  the fall, sat in bonds through the rebound, and in 2022 the bonds fell
  too.
- **What this means:** the filter is a bet on the kind of bear market to
  come, not a free improvement.

**Over the whole span,** compounding the two periods' rates (17 years, then
10.5) gives a rough picture:

| 1999–mid-2026, roughly | Per year | Worst drawdown in either period |
| --- | --- | --- |
| Momentum with the bond filter | about 19.7% | 46.8% |
| Momentum without the filter | about 16.4% | 63.7% |
| SPY | about 8.7% | 55.2% |

Both momentum versions beat SPY by a wide margin over the full 27 years.
Neither dominates in both periods.

### Momentum with less drawdown

The owner asked whether choosing across sectors, or anything else, could
avoid momentum's deep drawdowns. The template is
`research/edge-search/momentum/defensive.py`. Each control is a switch that
is off by default; with all of them off it reproduces the 9.06% base
exactly. Results are long-only, 25 names, banded, on the tuning years
(1999–2015; SPY 4.91% / 55.2%).

| Control | CAGR | Worst drawdown | 2008–09 a year |
| --- | --- | --- | --- |
| None (base) | 9.06% | 63.7% | −31.5% |
| At most 3 names per Morningstar sector | 5.50% | 57.4% | −25.0% |
| At most 2 names per sector | 5.79% | 56.5% | −21.5% |
| Rank by return over volatility | 9.65% | 62.7% | −30.7% |
| Inverse-volatility weights | 10.19% | 63.0% | −30.7% |
| A name below its own 200-day average holds bonds | 11.47% | 64.2% | −16.1% |
| 50% SPY, 50% momentum | 7.54% | 59.2% | −20.7% |
| **Half filter:** half the book to bonds when SPY is below its 200-day average | **15.60%** | **45.8%** | −10.6% |
| **Combination** (sector cap 3 + inverse volatility + half filter), fixed in advance | **10.46%** | **31.1%** | −6.2% |
| *Full filter (grid above)* | *21.28%* | *44.6%* | *+11.2%* |

**What the tuning years showed:**
- **Sector caps cut the drawdown only a little, at a large cost.** They
  forced the book out of the leading sector in the 1999 technology run
  (−5% a year over 1999–2002).
- **Weighting and ranking changes add up to a point of return** but leave
  the drawdown where it was.
- **Only moving to bonds in a downtrend shrinks the drawdown materially.**
  The combination has the smallest drawdown by far.

**The two leaders on the held-out years (2016 to mid-2026, each run once).**
- The combination was declared before any result was seen, so its run is a
  clean test.
- The half filter was chosen after the full filter's held-out run had shown
  the whipsaw problem, so its result is weaker evidence.

| 2016–mid-2026 | CAGR | Worst drawdown | 2018 | 2020 | 2022 | 2025 | 2026 H1 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| SPY | 15.19% | 33.7% | | | −18.8% | | |
| No filter (above) | 29.50% | 38.7% | −10.1% | +78.4% | +2.2% | +53.8% | +74.6% |
| Full filter (above; a repeat run matched at 17.33%) | 17.34% | 46.8% | −10.3% | +79.2% | −21.7% | +19.4% | +36.0% |
| **Half filter** | **23.61%** | **40.4%** | −10.1% | +79.1% | −10.6% | +37.2% | +54.9% |
| **Combination** | **18.80%** | **27.7%** | −2.6% | +49.0% | −14.4% | +21.9% | +11.2% |

- **The combination** beat SPY by about 3.6 points a year with a smaller
  worst drawdown than SPY itself.
- **The half filter** kept most of the no-filter return, and its drawdown
  was close to the no-filter version's.

| 1999–mid-2026, roughly (compounding the two periods' rates) | Per year | Worst drawdown in either period |
| --- | --- | --- |
| Combination | about 13.6% | 31.1% |
| Half filter | about 18.6% | 45.8% |
| Full filter | about 19.7% | 46.8% |
| No filter | about 16.4% | 63.7% |
| SPY | about 8.7% | 55.2% |

On these two periods the half filter is the steadier choice than either
extreme, and the combination trades return for the shallowest losses.

**Bonds before July 2002.** Every "to bonds" rule here holds IEF (7–10-year
Treasuries), which began trading on July 22, 2002. QuantConnect's ETF data
has no Treasury fund before then.
- **What that means:** in risk-off months before mid-2002, the bond share
  sat in cash earning nothing. That was much of 2000–02, when SPY was
  mostly below its 200-day average.
- **Which way it biases the results:** 7–10-year Treasuries gained in each
  of those years, so the tuning-year figures for the full and half filters
  and the combination are, if anything, understated.
- **Unaffected:** the held-out runs (2016 on).

**A data glitch, caught.** The first re-run of the full-filter held-out run
returned 14.58%, having made only 105 of its 138 monthly rebalances. Its
history requests evidently came back empty for 33 months. A repeat made all
138 and returned 17.33%, so the 14.58% run is disregarded. Every result here
was checked for its full count of rebalances: 203 on the tuning years, 138
on the held-out years.

### Other candidates (tuning years)

Each was tested on the tuning years first (1999–2015; SPY 4.91% / 55.2%).

| Candidate | Source | CAGR | Worst drawdown |
| --- | --- | --- | --- |
| Momentum ranked by closeness to the 52-week high | George & Hwang (2004) | 3.28% | 46.9% |
| Intermediate momentum (12 to 7 months ago) | Novy-Marx (2012) | 11.21% | 65.6% |
| "Frog in the pan": steadier winners preferred | Da, Gurun & Warachka (2014) | 6.36% | 70.4% |
| Residual (market-adjusted) momentum, returns paired by session | after Blitz, Huij & Martens (2011) | 7.95% | 62.6% |
| Sector ETF rotation: top 3 of 9 by 3/6/12-month return, **2000–2015** (SPY 4.04% / 55.2%) | Faber (2010) | 5.47% | 44.2% |
| … with each ETF's own 10-month trend check, **2000–2015** | Faber (2010) | 6.39% | 31.0% |
| 2× SPY on margin above its 200-day average, else bonds; reset to 2× monthly | Gayed & Bilello (2016) | 5.69% | 47.9% |
| 1× of the same | | 4.95% | 25.4% |

- **None beat the momentum variants above.** The momentum rankings are in
  `momentum/scores.py`; their all-off control reproduces 9.06%.
- **Intermediate momentum** added return but no drawdown relief.
- **The 52-week-high ranking** cut the drawdown but fell below SPY.
- **Sector rotation** is measured from 2000. The sector ETFs began trading
  in December 1998, and the rule needs a year of closes, so 1999 would be
  mostly cash. With the trend check it had a shallow drawdown but beat SPY
  by only about 2 points.
- **The leveraged trend rule** was close to SPY once margin interest was
  paid.
- None of these went on to a held-out run.

### The unpriced-buy fix

**What went wrong.** Each stock-picking script rebalances at 8:00 on the
first trading day of the month, from a universe chosen that same day. A
stock that joined the universe that morning has no price yet, and LEAN
skips the order ("The security does not have an accurate price as it has
not yet received a bar of data"). Its weight then sat in cash, earning
nothing, until the next month.

Rebuilding the holdings from the fills shows how often this happened:
- the 1999–2015 no-filter run held 15–20 names with 20–40% cash in 62 of
  its 203 months;
- the 2016+ half-filter run did so in 39 of 138.

No order was rejected; the orders were simply never placed.

**Affected:** every stock-universe result in this report and the main
report. That covers momentum, the factors, the long/short books, the
drawdown controls and the alternative rankings. Boost, the ETF rules and
the futures studies trade instruments that always have a price, so they
are unaffected. The original scripts and figures stay as they ran.

**The fix:** `momentum/defensive_v2.py`. A target with no price waits and
is bought on the first day the stock has one, normally the next day. The
bond fund waits the same way until IEF began trading in July 2002. Each
run now reports its average cash share: about 3% on the held-out years.
The tuning years show 9–18%, because their bond share before July 2002
had no fund to go to. A retried target now stays on the list until its
order has filled in full, so a fill that is still pending is never ordered
twice, and a partly filled order is topped up. All eight runs were re-run
with each of these changes and gave identical results, to the cent of
their final holdings.

| Headline version, fixed | 1999–2015 CAGR / worst drawdown | 2016–mid-2026 CAGR / worst drawdown | As originally run (1999–2015; 2016+) |
| --- | --- | --- | --- |
| No filter | 9.95% / 67.9% | 30.05% / 44.2% | 9.06% / 63.7%; 29.50% / 38.7% |
| Full filter | 22.80% / 49.5% | 17.00% / 53.2% | 21.28% / 44.6%; 17.34% / 46.8% |
| **Half filter** | **16.96% / 49.4%** | **23.87% / 45.3%** | 15.60% / 45.8%; 23.61% / 40.4% |
| **Combination** | **11.25% / 34.0%** | **20.85% / 31.0%** | 10.46% / 31.1%; 18.80% / 27.7% |

SPY over the same spans: 4.91% / 55.2% and 15.19% / 33.7%.

Compounding the two periods gives a rough 1999–mid-2026 figure for each:

| Version | Per year | Worst drawdown in either period |
| --- | --- | --- |
| Full filter | about 20.6% | 53.2% |
| Half filter | about 19.6% | 49.4% |
| No filter | about 17.2% | 67.9% |
| Combination | about 14.8% | 34.0% |
| SPY | about 8.7% | 55.2% |

**What changed:** fully invested, every version earns more and has deeper
drawdowns. The ranking among them is unchanged, and the combination's
worst drawdown on the held-out years (31.0%) is still below SPY's.

## Taxes: taxable account or IRA

The owner asked which account each strategy belongs in. This section
estimates the US federal income tax each strategy would have cost in a
taxable account, computed from its own fills by
`research/edge-search/tax/tax_drag.py`. It is not tax advice.

**The method:**
- **Matching sales to purchases:** first in, first out for momentum. For
  Boost, the newest lots are sold first, which is how its extra shares
  would be sold while the core SPY is kept.
- **Holding period:** held more than a year is long-term.
- **Netting:** each year's results are netted by the IRS rules, with the
  $3,000 loss deduction and carryforward.
- **Paying the tax:** it comes out of the account each year end.
- **SPY's dividends** (about 1.7% a year) are taxed yearly for Boost and
  buy-and-hold.
- **Not modelled:** state tax, the 3.8% net investment income tax, and
  wash sales.

Each strategy's own CAGR is shown below, less its tax cost.

| Federal rates: short-term 22%, long-term 15% | Before tax (= Roth) | Taxable, still holding | Tax cost (percentage points a year) |
| --- | --- | --- | --- |
| Momentum, half filter, 2016+ | 23.87% | 20.35% | 3.52 |
| Momentum, combination, 2016+ | 20.85% | 17.08% | 3.77 |
| Momentum, no filter, 2016+ | 30.05% | 25.72% | 4.33 |
| Momentum, half filter, 1999–2015 | 16.96% | 14.06% | 2.90 |
| Momentum, combination, 1999–2015 | 11.25% | 9.12% | 2.13 |
| Boost on margin, 1999–mid-2026 | 10.32% | 9.59% | 0.72 |
| SPY buy-and-hold, 1999–mid-2026 | 8.46% | 8.18% | 0.28 |

- **Selling everything at the end**, each open lot taxed on its own gain
  and holding period, costs 0.2–0.9 points more for momentum on the
  held-out years (nothing on the tuning years, which end fully realized),
  and about 0.4–0.5 for Boost and SPY, whose gains have built up untaxed.
- **At 24% / 15%** the costs rise by about 0.3 points.
- **At 12% / 0%** (a lower bracket) they roughly halve: momentum 0.8–2.0,
  Boost 0.2, SPY 0.
- **The momentum cost is mostly short-term tax.** Most positions are held
  under a year: the monthly ranking replaces names, and the band resizes
  them.
- **Boost keeps its edge after tax** (9.59% against SPY's 8.18%), because
  only its short-lived extra shares are sold.

**For the account choice (general rules, not advice):**
- In a Roth IRA, momentum's whole tax cost disappears. It needs no margin
  or shorting, so it fits there.
- Margin Boost needs a taxable account, and its tax cost is small.
- The leveraged-fund version of Boost fits an IRA.

## 5. What running Boost would take

This section gathers facts for the owner's own decision. It is not a
recommendation.

**Three ways to hold the extra 50%:**

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
3. **Boost without borrowing: a leveraged S&P 500 fund** (QuantConnect,
   `boost/lev_etf_template.py`).
   - On a signal, swap part of the SPY position into a fund that returns 2×
     (SSO) or 3× (UPRO) the index each day: 49% of the account into SSO, or
     24.5% into UPRO. Either gives the same 147% index exposure as
     borrowing. Swap back on the exit.
   - Nothing is borrowed, so an IRA could hold it, if its broker allows
     these funds.
   - The rule is otherwise identical. Each fund is tested from shortly
     after it launched, against margin Boost and SPY over the same span:

   | Span | Boost with the fund | Boost on margin (T-bill + 1.5) | SPY |
   | --- | --- | --- | --- |
   | SSO, Jul 2006–mid-2026 | 12.07% | 12.84% | 11.25% |
   | … of which 2016 on | 16.12% | 16.43% | 15.19% |
   | UPRO, Jul 2009–mid-2026 | 16.29% | 16.75% | 15.12% |

   - The fund route keeps roughly half (SSO since 2006) to three-quarters
     (UPRO since 2009) of margin Boost's edge over SPY.
   - The rest goes to the funds' fees and to their daily reset, which costs
     a little on volatile days.
   - Drawdowns were the same as margin Boost.

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
  - `research/edge-search/build_variants.py TB3MS.csv OUT_DIR` writes all 98
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
- **Taxes.** `research/edge-search/tax/tax_drag.py EXPORT.json ...` prints
  the tax tables from a JSON export of each run's fills and equity. The
  export is not committed because it is derived from market data.
  - Export the fills from each run's orders (every order: QuantConnect may
    return an empty first page until it has prepared them).
  - Export the equity from its "Strategy Equity" chart.
  - Export the end prices from the run's `End1`, `End2`, … statistics
    (`defensive_v2.py` writes each open position as `security id=price`),
    as `end_prices` keyed the same way as the fills. Open lots at those
    prices should add up to the run's reported holdings; they do to the
    cent in all eight momentum runs.
  - The script refuses a run with no fills, so an empty export cannot read
    as no tax.
- **Tests.** `make research-test` runs `research/edge-search/test_followup.py`,
  which covers:
  - the builder
  - the templates' daily rules, against a stand-in for QuantConnect's
    module
  - the carry measure
