# Research script, not production code: no TDD, no ADR citations, no
# fidelity check against the Go engine. Preserved as the exact code run on
# QuantConnect Cloud for the "prove an edge" investigation
# (docs/research/edge-search-2026-09.md). Cannot be imported or unit tested
# outside QuantConnect's own environment (AlgorithmImports); see
# research/edge-search/README.md.
#
# In-sample run: 1998-01-01 to 2015-12-31, measured from 1999-01-01
# (START_DATE/END_DATE/MEASURE_FROM below), MODE one of "mom", "mom_trend",
# "lowvol", "quality", "value", "mom_qual", "ew". See
# research/edge-search/README.md for the switches.
#
# Provenance (PUBLISHED: the rule of a named source; ADAPTED: changed as
# stated; PROJECT: this project's own). Full references are in
# docs/research/edge-search-2026-09.md, "Sources for the rules". Page-level
# citations are not given: the sources were not transcribed page by page.
# mom, lowvol, quality, value: PUBLISHED, ADAPTED -- the sort variable of
#   each named paper, applied to a top-500-by-dollar-volume universe, top 50
#   equal weight, monthly.
# mom_trend: PROJECT -- the 200-day SPY filter is this project's crash
#   filter, motivated by Daniel & Moskowitz (2016), not their rule.
# mom_qual, ew: PROJECT -- a combination and a control, no published source.
from AlgorithmImports import *
from datetime import date
import numpy as np


class FactorResearch(QCAlgorithm):
    """Published US stock factor strategies, research only, monthly rebalance.

    Universe: each month, the largest UNIVERSE_SIZE US common stocks by
    dollar volume with fundamental data and price >= $5 (QuantConnect's
    point-in-time, survivorship-free coarse/fine data). Hold TOP_N names,
    equal weight; everything else is sold.

    MODE:
      "mom"       12-1 momentum: return from 12 months ago to 1 month ago
                  (Jegadeesh & Titman 1993), highest first.
      "mom_trend" "mom", but only while SPY closes above its 200-day SMA;
                  otherwise the whole portfolio sits in IEF (a common crash
                  filter for momentum; Daniel & Moskowitz 2016 motivate it).
      "lowvol"    lowest 12-month daily-return volatility first (Baker,
                  Bradley & Wurgler 2011; Frazzini & Pedersen 2014).
      "quality"   gross profits / total assets, highest first (Novy-Marx 2013).
      "value"     earnings yield, highest first (Fama & French lineage).
      "mom_qual"  average of the momentum and quality percentile ranks.
    """

    MODE = "mom"
    START_DATE = (1998, 1, 1)
    END_DATE = (2015, 12, 31)
    MEASURE_FROM = (1999, 1, 1)   # the first year is warm-up for 12-month lookbacks
    CASH = 1_000_000
    UNIVERSE_SIZE = 500
    TOP_N = 50
    TREND_ASSET = "IEF"
    TREND_DAYS = 200

    def Initialize(self):
        self.SetStartDate(*self.START_DATE)
        self.SetEndDate(*self.END_DATE)
        self.SetCash(self.CASH)
        # Weights sum to 98% at most: no leverage. Margin avoids cash-account
        # settlement lag on the monthly swap.
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        self.UniverseSettings.Resolution = Resolution.Daily
        self.spy = self.AddEquity("SPY", Resolution.Daily).Symbol
        self.bond = self.AddEquity(self.TREND_ASSET, Resolution.Daily).Symbol
        self.AddUniverse(self.Coarse, self.Fine)
        self.members = []
        self.fundamentals = {}
        self.last_month = None
        self.equity_curve = []
        self.spy_curve = []
        self.rebalances = 0
        self.fund_reads = 0
        self.fund_hits = 0
        self.risk_off_months = 0
        self.names_held = 0
        self.Schedule.On(self.DateRules.MonthStart(self.spy), self.TimeRules.At(8, 0), self.Rebalance)

    def Coarse(self, coarse):
        month = (self.Time.year, self.Time.month)
        if month == self.last_month:
            return Universe.Unchanged
        self.last_month = month
        rows = [c for c in coarse if c.HasFundamentalData and c.Price >= 5.0]
        rows.sort(key=lambda c: c.DollarVolume, reverse=True)
        return [c.Symbol for c in rows[:self.UNIVERSE_SIZE]]

    @staticmethod
    def _get(obj, *paths):
        """First readable attribute path, trying LEAN's PascalCase and the
        newer snake_case names; None when neither exists or is non-positive."""
        for path in paths:
            cur = obj
            try:
                for name in path.split("."):
                    cur = getattr(cur, name)
                val = float(cur)
            except Exception:
                continue
            if val == val and val != 0.0:
                return val
        return None

    def Fine(self, fine):
        self.fundamentals = {}
        keep = []
        for f in fine:
            gp = self._get(f, "FinancialStatements.IncomeStatement.GrossProfit.TwelveMonths",
                           "financial_statements.income_statement.gross_profit.twelve_months",
                           "FinancialStatements.IncomeStatement.GrossProfit.Value")
            ta = self._get(f, "FinancialStatements.BalanceSheet.TotalAssets.TwelveMonths",
                           "financial_statements.balance_sheet.total_assets.twelve_months",
                           "FinancialStatements.BalanceSheet.TotalAssets.Value")
            ey = self._get(f, "ValuationRatios.EarningsYield", "valuation_ratios.earnings_yield",
                           "ValuationRatios.PERatio", "valuation_ratios.pe_ratio")
            if ey is not None and ey > 1.0:   # a P/E rather than a yield: invert it
                ey = 1.0 / ey
            self.fund_reads += 1
            self.fund_hits += int(gp is not None and ta is not None) + int(ey is not None)
            self.fundamentals[f.Symbol] = {
                "quality": gp / ta if gp is not None and ta and ta > 0 else None,
                "value": ey,
            }
            keep.append(f.Symbol)
        self.members = keep
        return keep

    def OnData(self, data):
        bar = data.Bars.get(self.spy)
        if bar is not None:
            self.spy_curve.append((self.Time, float(bar.Close)))
            self.equity_curve.append((self.Time, float(self.Portfolio.TotalPortfolioValue)))

    @staticmethod
    def _pct_rank(scores):
        """Percentile rank in [0, 1], higher score -> higher rank."""
        ordered = sorted(scores, key=lambda s: scores[s])
        n = len(ordered)
        return {s: (i / (n - 1) if n > 1 else 0.5) for i, s in enumerate(ordered)}

    def Rebalance(self):
        if not self.members:
            return
        hist = self.History(self.members + [self.spy], 260, Resolution.Daily)
        if hist.empty or "close" not in hist.columns:
            return
        closes = hist["close"].unstack(level=0)
        if self.spy not in closes.columns:
            return
        spy = closes[self.spy].dropna()
        risk_off = False
        if self.MODE == "mom_trend" and len(spy) >= self.TREND_DAYS:
            risk_off = spy.iloc[-1] < spy.iloc[-self.TREND_DAYS:].mean()

        mom, vol = {}, {}
        for sym in self.members:
            if sym not in closes.columns:
                continue
            px = closes[sym].dropna()
            if len(px) < 253:
                continue
            mom[sym] = px.iloc[-22] / px.iloc[-253] - 1.0
            rets = px.pct_change().dropna().iloc[-252:]
            vol[sym] = float(np.std(rets))

        scores = {}
        if self.MODE in ("mom", "mom_trend"):
            scores = mom
        elif self.MODE == "lowvol":
            scores = {s: -v for s, v in vol.items() if v > 0}
        elif self.MODE in ("quality", "value"):
            scores = {s: self.fundamentals[s][self.MODE] for s in mom
                      if s in self.fundamentals and self.fundamentals[s][self.MODE] is not None}
        elif self.MODE == "ew":
            # Control: every eligible name, equal weight (the equal-weight
            # effect the factor runs share), no factor tilt at all.
            scores = {s: 0.0 for s in mom}
        elif self.MODE == "mom_qual":
            q = {s: self.fundamentals[s]["quality"] for s in mom
                 if s in self.fundamentals and self.fundamentals[s]["quality"] is not None}
            common = [s for s in q if s in mom]
            if common:
                rm = self._pct_rank({s: mom[s] for s in common})
                rq = self._pct_rank({s: q[s] for s in common})
                scores = {s: rm[s] + rq[s] for s in common}
        if not scores:
            return

        self.rebalances += 1
        if risk_off:
            self.risk_off_months += 1
            targets = {self.bond: 0.98}
        else:
            picks = sorted(scores, key=lambda s: scores[s], reverse=True)[:self.TOP_N]
            w = 0.98 / len(picks)
            targets = {s: w for s in picks}
            self.names_held += len(picks)
        # Trade only names entering or leaving: a continuing holding drifts
        # rather than being trimmed back to equal weight each month, which
        # keeps the run inside the Free plan's 10,000-order cap.
        for kvp in self.Portfolio:
            if kvp.Value.Invested and kvp.Key not in targets:
                self.Liquidate(kvp.Key)
        for sym, w in targets.items():
            if not self.Portfolio[sym].Invested:
                self.SetHoldings(sym, w)

    def _stats(self, curve, start, end):
        pts = [(t, v) for t, v in curve if start <= t.date() <= end and v > 0]
        if len(pts) < 2:
            return "no-data"
        years = (pts[-1][0] - pts[0][0]).days / 365.25
        cagr = (pts[-1][1] / pts[0][1]) ** (1 / years) - 1 if years > 0 else 0.0
        peak, mdd = 0.0, 0.0
        for _, v in pts:
            peak = max(peak, v)
            mdd = max(mdd, 1 - v / peak)
        return "CAGR={:.4f} MaxDD={:.4f} Ratio={}".format(
            cagr, mdd, "{:.4f}".format(cagr / mdd) if mdd else "n/a")

    def OnEndOfAlgorithm(self):
        spans = [("OVERALL", date(*self.MEASURE_FROM), date(*self.END_DATE)),
                 ("1999-02", date(1999, 1, 1), date(2002, 12, 31)),
                 ("2003-07", date(2003, 1, 1), date(2007, 12, 31)),
                 ("2008-09", date(2008, 1, 1), date(2009, 12, 31)),
                 ("2009-15", date(2009, 1, 1), date(2015, 12, 31)),
                 ("2016-", date(2016, 1, 1), date(*self.END_DATE))]
        for label, a, b in spans:
            if a > date(*self.END_DATE) or b < date(*self.MEASURE_FROM):
                continue
            self.SetRuntimeStatistic(label, self._stats(self.equity_curve, a, b))
            self.SetRuntimeStatistic("SPY " + label, self._stats(self.spy_curve, a, b))
        self.SetRuntimeStatistic("Mode", "{} n={} top={} rebalances={} risk_off={} orders={}".format(
            self.MODE, self.UNIVERSE_SIZE, self.TOP_N, self.rebalances, self.risk_off_months,
            self.Transactions.OrdersCount))
        self.SetRuntimeStatistic("Fundamentals", "reads={} hits={}".format(self.fund_reads, self.fund_hits))
