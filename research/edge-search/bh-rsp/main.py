# Research script, not production code: no TDD, no ADR citations, no
# fidelity check against the Go engine. Preserved as the exact code run on
# QuantConnect Cloud for the "prove an edge" investigation
# (docs/research/edge-search-2026-09.md). Cannot be imported or unit tested
# outside QuantConnect's own environment (AlgorithmImports); see
# research/edge-search/README.md.
#
# Fixed run: 2003-05-01 to 2015-12-31, no switches. Buy-and-hold RSP
# (equal-weight S&P 500) as the control that isolates the equal-weight
# effect from the factor tilts in research/edge-search/factors/main.py.
#
# Provenance (PUBLISHED: the rule of a named source; ADAPTED: changed as
# stated; PROJECT: this project's own). Full references are in
# docs/research/edge-search-2026-09.md, "Sources for the rules". Page-level
# citations are not given: the sources were not transcribed page by page.
# PROJECT -- a buy-and-hold control, not a strategy rule.
from AlgorithmImports import *
from datetime import date


class BuyHoldControl(QCAlgorithm):
    """Buy-and-hold RSP (equal-weight S&P 500) as the control for the factor
    runs; SPY tracked alongside. Research only."""

    def Initialize(self):
        self.SetStartDate(2003, 5, 1)
        self.SetEndDate(2015, 12, 31)
        self.SetCash(1_000_000)
        self.rsp = self.AddEquity("RSP", Resolution.Daily).Symbol
        self.spy = self.AddEquity("SPY", Resolution.Daily).Symbol
        self.curves = {"RSP": [], "SPY": []}

    def OnData(self, data):
        if not self.Portfolio.Invested and data.Bars.ContainsKey(self.rsp):
            self.SetHoldings(self.rsp, 0.99)
        for name, sym in (("RSP", self.rsp), ("SPY", self.spy)):
            bar = data.Bars.get(sym)
            if bar is not None:
                self.curves[name].append((self.Time, float(bar.Close)))

    def _stats(self, curve, start, end):
        pts = [(t, v) for t, v in curve if start <= t.date() <= end]
        if len(pts) < 2:
            return "no-data"
        years = (pts[-1][0] - pts[0][0]).days / 365.25
        cagr = (pts[-1][1] / pts[0][1]) ** (1 / years) - 1
        peak, mdd = 0.0, 0.0
        for _, v in pts:
            peak = max(peak, v)
            mdd = max(mdd, 1 - v / peak)
        return "CAGR={:.4f} MaxDD={:.4f}".format(cagr, mdd)

    def OnEndOfAlgorithm(self):
        spans = [("2003-07", date(2003, 5, 1), date(2007, 12, 31)),
                 ("2008-09", date(2008, 1, 1), date(2009, 12, 31)),
                 ("2009-15", date(2009, 1, 1), date(2015, 12, 31)),
                 ("2003-15", date(2003, 5, 1), date(2015, 12, 31))]
        for label, a, b in spans:
            for name in ("RSP", "SPY"):
                self.SetRuntimeStatistic(name + " " + label, self._stats(self.curves[name], a, b))
