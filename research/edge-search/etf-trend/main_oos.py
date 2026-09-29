# Research script, not production code: no TDD, no ADR citations, no
# fidelity check against the Go engine. Preserved as the exact code run on
# QuantConnect Cloud for the "prove an edge" investigation
# (docs/research/edge-search-2026-09.md). Cannot be imported or unit tested
# outside QuantConnect's own environment (AlgorithmImports); see
# research/edge-search/README.md.
#
# The 2016+ single held-out run (START_DATE/MEASURE_FROM/END_DATE below):
# run once, with the in-sample-fixed settings, and never re-tuned against
# this span. See research/edge-search/README.md and
# docs/research/edge-search-2026-09.md ("the holdout is spent").
#
# Provenance (PUBLISHED: the rule of a named source; ADAPTED: changed as
# stated; PROJECT: this project's own). Full references are in
# docs/research/edge-search-2026-09.md, "Sources for the rules". Page-level
# citations are not given: the sources were not transcribed page by page.
# Same rules and tags as etf-trend/main.py; only the dates differ.
from AlgorithmImports import *


class EtfTrendResearch(QCAlgorithm):
    """Published asset-class trend rules on ETFs, research only.

    MODE "faber": Faber (2007), "A Quantitative Approach to Tactical Asset
    Allocation": equal weight per asset class; hold a class only while its
    month-end close is above its 10-month SMA, otherwise that slice sits in
    cash (SHY once it exists).
    MODE "gem": Antonacci's Global Equities Momentum: at each month end, if
    SPY's 12-month return beats SHY's, hold whichever of SPY/EFA has the
    higher 12-month return; otherwise hold AGG (IEF before AGG exists).
    """

    MODE = "faber"
    START_DATE = (2014, 11, 1)     # warm-up months so the first 2016 decision has full lookbacks
    MEASURE_FROM = (2016, 1, 1)    # held-out period: statistics are measured from here
    END_DATE = (2026, 6, 26)
    CASH = 1_000_000
    ASSETS = ["SPY", "EFA", "IEF", "VNQ", "DBC"]
    CASH_ETF = "SHY"
    SMA_MONTHS = 10
    MOMENTUM_MONTHS = 12
    REBALANCE_OFFSET = 0          # trading days after the month's first session
    GEM_BOND = "AGG"              # the risk-off holding once it exists

    def Initialize(self):
        self.SetStartDate(*self.START_DATE)
        self.SetEndDate(*self.END_DATE)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)  # weights stay at or below 98%, so no leverage; avoids cash-account settlement lag
        names = set(self.ASSETS + [self.CASH_ETF, "SPY", "EFA", "AGG", "IEF", self.GEM_BOND])
        self.sym = {n: self.AddEquity(n, Resolution.Daily).Symbol for n in names}
        self.monthly = {n: [] for n in names}
        self.equity_curve = []
        self.spy_curve = []
        self.months_invested = {n: 0 for n in names}
        self.rebalances = 0
        # Decide before the first open of each month on the prior month-end
        # close; the orders go in as market-on-open, the only order type
        # LEAN accepts from a daily-resolution schedule outside the session.
        self.Schedule.On(self.DateRules.MonthStart("SPY", self.REBALANCE_OFFSET), self.TimeRules.At(8, 0), self.Rebalance)

    def OnData(self, data):
        spy = data.Bars.get(self.sym["SPY"])
        if spy is not None:
            self.spy_curve.append((self.Time, float(spy.Close)))
            self.equity_curve.append((self.Time, float(self.Portfolio.TotalPortfolioValue)))

    def _record_month_end(self):
        for n, s in self.sym.items():
            sec = self.Securities[s]
            if sec.HasData and sec.Price > 0:
                self.monthly[n].append(float(sec.Price))

    def _ret(self, n, months):
        h = self.monthly[n]
        return h[-1] / h[-1 - months] - 1.0 if len(h) > months else None

    def Rebalance(self):
        self._record_month_end()
        weights = {}
        if self.MODE == "faber":
            live = [n for n in self.ASSETS if len(self.monthly[n]) >= self.SMA_MONTHS]
            if not live:
                return
            slice_w = 1.0 / len(live)
            cash_ok = len(self.monthly[self.CASH_ETF]) > 0
            for n in live:
                h = self.monthly[n][-self.SMA_MONTHS:]
                if h[-1] > sum(h) / len(h):
                    weights[n] = weights.get(n, 0.0) + slice_w
                elif cash_ok:
                    weights[self.CASH_ETF] = weights.get(self.CASH_ETF, 0.0) + slice_w
        else:
            spy, efa, cash = (self._ret("SPY", self.MOMENTUM_MONTHS), self._ret("EFA", self.MOMENTUM_MONTHS),
                              self._ret(self.CASH_ETF, self.MOMENTUM_MONTHS) or 0.0)
            if spy is None:
                return
            if spy > cash:
                pick = "EFA" if efa is not None and efa > spy else "SPY"
            else:
                pick = self.GEM_BOND if len(self.monthly[self.GEM_BOND]) > 0 else "IEF"
            weights[pick] = 1.0
        self.rebalances += 1
        for n in weights:
            self.months_invested[n] += 1
        targets = [PortfolioTarget(self.sym[n], 0.98 * w) for n, w in weights.items()]
        for n, s in self.sym.items():
            if n not in weights and self.Portfolio[s].Invested:
                targets.append(PortfolioTarget(s, 0))
        self.SetHoldings(sorted(targets, key=lambda t: t.Quantity))

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
        return "{}..{} CAGR={:.4f} MaxDD={:.4f} Ratio={}".format(
            pts[0][0].date(), pts[-1][0].date(), cagr, mdd, "{:.4f}".format(cagr / mdd) if mdd else "n/a")

    def OnEndOfAlgorithm(self):
        from datetime import date
        spans = [("OVERALL", date(*self.MEASURE_FROM), date(*self.END_DATE))] + [
            (str(y), date(y, 1, 1), date(y, 12, 31)) for y in range(self.MEASURE_FROM[0], self.END_DATE[0] + 1)]
        for label, a, b in spans:
            self.SetRuntimeStatistic(label, self._stats(self.equity_curve, a, b))
            self.SetRuntimeStatistic("SPY " + label, self._stats(self.spy_curve, a, b))
        first = {n: (self.monthly[n] and len(self.monthly[n])) for n in self.ASSETS}
        self.SetRuntimeStatistic("Months of data", " ".join("{}={}".format(n, first[n] or 0) for n in self.ASSETS))
        self.SetRuntimeStatistic("Months held", " ".join(
            "{}={}".format(n, c) for n, c in sorted(self.months_invested.items()) if c))
        self.SetRuntimeStatistic("Mode", "{} rebalances={}".format(self.MODE, self.rebalances))
