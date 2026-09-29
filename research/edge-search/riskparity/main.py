# Research script, not production code: no TDD, no ADR citations, no
# fidelity check against the Go engine. Preserved as the exact code run on
# QuantConnect Cloud for the "prove an edge" investigation
# (docs/research/edge-search-2026-09.md). Cannot be imported or unit tested
# outside QuantConnect's own environment (AlgorithmImports); see
# research/edge-search/README.md.
#
# In-sample run: 2005-01-01 to 2015-12-31, measured from 2006-01-01
# (START_DATE/END_DATE/MEASURE_FROM below); TARGET_VOL None for the
# unlevered run, 0.10 for the 10%-vol-target run. See
# research/edge-search/README.md for the switches.
#
# Provenance (PUBLISHED: the rule of a named source; ADAPTED: changed as
# stated; PROJECT: this project's own). Full references are in
# docs/research/edge-search-2026-09.md, "Sources for the rules". Page-level
# citations are not given: the sources were not transcribed page by page.
# PUBLISHED, ADAPTED -- inverse-volatility weights in the spirit of Qian
#   (2005) and Asness, Frazzini & Pedersen (2012); the ETF list, window and
#   volatility target are this project's choices.
from AlgorithmImports import *
from datetime import date
import numpy as np


class RiskParityResearch(QCAlgorithm):
    """Naive risk parity on ETFs, research only (in the spirit of Qian 2005
    and Asness, Frazzini & Pedersen 2012, "Leverage Aversion and Risk
    Parity"). Monthly, weight each asset by the inverse of its trailing
    VOL_DAYS daily-return volatility. With TARGET_VOL set, scale the whole
    book toward that annualised volatility (capped at MAX_LEVERAGE), paying
    FINANCING_RATE a year on borrowed exposure."""

    START_DATE = (2005, 1, 1)
    END_DATE = (2015, 12, 31)
    MEASURE_FROM = (2006, 1, 1)
    CASH = 1_000_000
    ASSETS = ["SPY", "TLT", "IEF", "GLD", "DBC"]
    VOL_DAYS = 60
    TARGET_VOL = None          # e.g. 0.10 for a 10% portfolio volatility target
    MAX_LEVERAGE = 2.0
    FINANCING_RATE = 0.03

    def Initialize(self):
        self.SetStartDate(*self.START_DATE)
        self.SetEndDate(*self.END_DATE)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        self.sym = {n: self.AddEquity(n, Resolution.Daily, leverage=3).Symbol for n in self.ASSETS}
        self.spy = self.sym["SPY"]
        self.equity_curve, self.spy_curve = [], []
        self.financing_paid = 0.0
        self.last_gross = 0.0
        self.Schedule.On(self.DateRules.MonthStart(self.spy), self.TimeRules.At(8, 0), self.Rebalance)

    def Rebalance(self):
        hist = self.History(list(self.sym.values()), self.VOL_DAYS + 5, Resolution.Daily)
        if hist.empty:
            return
        closes = hist["close"].unstack(level=0)
        inv = {}
        rets = {}
        for n, s in self.sym.items():
            if s not in closes.columns:
                continue
            px = closes[s].dropna()
            if len(px) < self.VOL_DAYS:
                continue
            r = px.pct_change().dropna().iloc[-self.VOL_DAYS:]
            v = float(np.std(r)) * np.sqrt(252)
            if v > 0:
                inv[n] = 1.0 / v
                rets[n] = r.values
        if not inv:
            return
        total = sum(inv.values())
        w = {n: x / total for n, x in inv.items()}
        scale = 0.98
        if self.TARGET_VOL:
            names = list(w)
            m = np.column_stack([rets[n] for n in names])
            port = m @ np.array([w[n] for n in names])
            pv = float(np.std(port)) * np.sqrt(252)
            if pv > 0:
                scale = min(self.MAX_LEVERAGE, self.TARGET_VOL / pv) * 0.98
        targets = [PortfolioTarget(self.sym[n], scale * w.get(n, 0.0)) for n in self.ASSETS]
        self.SetHoldings(sorted(targets, key=lambda t: t.Quantity))
        self.last_gross = scale

    def OnData(self, data):
        bar = data.Bars.get(self.spy)
        if bar is None:
            return
        borrowed = max(0.0, float(self.Portfolio.TotalHoldingsValue) - float(self.Portfolio.TotalPortfolioValue))
        cost = borrowed * self.FINANCING_RATE / 252.0
        if cost > 0:
            self.Portfolio.CashBook["USD"].AddAmount(-cost)
            self.financing_paid += cost
        self.spy_curve.append((self.Time, float(bar.Close)))
        self.equity_curve.append((self.Time, float(self.Portfolio.TotalPortfolioValue)))

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
        return "CAGR={:.4f} MaxDD={:.4f}".format(cagr, mdd)

    def OnEndOfAlgorithm(self):
        a, b = date(*self.MEASURE_FROM), date(*self.END_DATE)
        spans = [("OVERALL", a, b)] + [(str(y), date(y, 1, 1), date(y, 12, 31)) for y in range(a.year, b.year + 1)]
        for label, x, y in spans:
            self.SetRuntimeStatistic(label, self._stats(self.equity_curve, x, y))
            self.SetRuntimeStatistic("SPY " + label, self._stats(self.spy_curve, x, y))
        self.SetRuntimeStatistic("Mode", "target={} last_gross={:.2f} financing={:.0f}".format(
            self.TARGET_VOL, self.last_gross, self.financing_paid))
