# Research history

These folders hold the Turtle, Sublime and futures research that came before Boost and Momentum. They are kept as history. They are not built or tested by `make research-test` or `make check`. To run one, check out the tag `turtle-final`, where its tests ran.

The search that produced Boost and Momentum stays live in `research/edge-search`. Its overall write-up is `docs/research/edge-search-2026-09.md`.

All figures below are quoted from the repository's own documents. In-sample means 1998 to 2015 unless stated. SPY buy-and-hold over 1998 to 2015 made +6.07% a year with a 55% worst drawdown.

## qc-cloud: the Turtle Baseline on US stocks

- **What it tested:** whether the Baseline Turtle rules make money on US stocks, after realistic costs, over QuantConnect's free history from 1998. It was a free check, run before buying any market data. It also holds a fidelity check against the production Go engine's fills.
- **Headline result:** it lost. On the top 200 to 500 US stocks by dollar volume, the faithful Turtle rules made -5.74% a year with an 87% worst drawdown. The variants did better but still did not beat SPY:
  - with an SPY 200-day filter: -0.35% a year, 52% drawdown;
  - with a 0.1% Unit: -0.26% a year, 36% drawdown;
  - with both: -1.26% a year, 31% drawdown.
- **Full write-up:** `qc-cloud/README.md`, and the "Stocks: Turtle rules" table in `docs/research/edge-search-2026-09.md`.

## qc-cloud-sublime: the Sublime reconstruction on US stocks

- **What it tested:** whether Sublime-style trend trading on US stocks beats buying and holding SPY. It implements only the Sublime rules recorded in `docs/methodology/Methodology_Analysis.md`.
- **Headline result:** positive but small, and far below SPY. The Sublime control made +1.45% a year with a 13.6% worst drawdown, against SPY's +6.07% and 55%. The edge-search report gives a range of -0.29% to +1.37% across 10 configurations tried, with a 252-day channel the best variant.
- **Full write-up:** `qc-cloud-sublime/README.md` ("Cloud result" and the variant results), and the "Stocks: Sublime reconstruction" table in `docs/research/edge-search-2026-09.md`.

## qc-cloud-futures: Faith's own Turtle rules on futures, in the cloud

- **What it tested:** Faith's unadapted System 2 (55/20, 1% Unit, long and short) on a diversified futures portfolio, using QuantConnect's free futures data.
- **Headline result:** inconclusive, and the folder's own README says the result is not yet trustworthy. The cloud run made -3.5% a year over 1998 to 2015 with an 84% worst drawdown. But the free data covers only ES before April 2007, so most of the span is a single-market test, and roll fills on sparse contracts left a gap of about $1.17M between the Campaigns' own profit and the account. That is why the Pinnacle data was bought and `futures-local` was built.
- **Full write-up:** `qc-cloud-futures/README.md` ("Cloud result" and "Known remaining gap"), and the "Futures" section of `docs/research/edge-search-2026-09.md`.

## futures-local: a local backtester on Pinnacle futures data

- **What it tested:** the same Faith rules on 19 markets of Pinnacle's CLC continuous futures, 1980 to 2015, with a second run on the held-out years 2015 to 2025. It reuses the rule core in `qc-cloud-futures`. It also holds a TSMOM (time-series momentum) backtester. No market data is in the repository.
- **Headline result:** the Turtle rules worked in the 1980s and then failed. With T-bill interest, 1980 to 2015 made +10.36% a year, but the 99.49% worst drawdown makes it unusable, and every later window lost: 2000 to 2008 -17.04%, 2009 to 2015 -25.81%, 2003 to 2015 -22.70%. On the held-out 2015 to 2025 run it made -12.43% a year with a 93.42% drawdown. TSMOM looked better on paper: 2009 to 2015 made 4.6% to 5.7% a year without interest, and the edge-search report records +3.59% a year on the 2016 to 2025 holdout, barely above cash.
- **Full write-up:** `futures-local/README.md` ("In-sample result" and "Held-out result"), and the "Futures" section and held-out scorecard of `docs/research/edge-search-2026-09.md`.
