# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Run on QuantConnect Cloud for "Other candidates" in
# docs/research/boost-followup-2026-09.md (tuning years first). It cannot be
# imported outside QuantConnect (AlgorithmImports); the margin-rate table
# below is filled in by build_variants.py from a local FRED TB3MS CSV.
#
# Provenance:
#   sector_rot: PUBLISHED, ADAPTED -- relative-strength sector rotation (Faber
#     2010, "Relative Strength Strategies for Investing", SSRN): each month
#     hold the TOP sector ETFs by the average of their 3-, 6- and 12-month
#     returns; with SMA_MONTHS set, a pick below its own SMA_MONTHS-month
#     average holds the bond fund instead. The nine original Select Sector
#     SPDRs, so the universe is fixed. They began trading in December 1998
#     and a 12-month return needs a year of closes, so the reported
#     sector_rot runs are measured from January 2000 (MEASURE_FROM).
#   lev_trend: PUBLISHED, ADAPTED -- Gayed & Bilello (2016), "Leverage for the
#     Long Run", SSRN: hold LEVERAGE x SPY while SPY closes above its
#     TREND_DAYS average, else the bond fund; checked daily, traded at the next
#     open. Leverage here is margin on SPY at the prior month's T-bill rate +
#     SPREAD (no leveraged fund existed before 2006), reset to the target
#     monthly; the paper's funds reset daily.
#   Both modes hold IEF as "bonds". IEF began trading in July 2002: before
#   then a bond allocation stays in cash, earning nothing.
from AlgorithmImports import *
from datetime import date

# Monthly 3-month T-bill rate (FRED TB3MS), each month's rate being the
# PREVIOUS month's average (no look-ahead). Margin financing only.
RATES = __RATES__

SECTORS = ["XLB", "XLE", "XLF", "XLI", "XLK", "XLP", "XLU", "XLV", "XLY"]


def lev_trend_action(on, last_on, month, last_month, bond_ready, bond_held):
    """What lev_trend does today: "on" (SPY at the leverage target), "off"
    (the bond fund) or None. A signal change always trades. While on, the
    position is reset to the target once a month, so leverage does not
    drift with prices; while off, the bond target is retried once the bond
    fund has a price, since before July 2002 it had none."""
    if on != last_on:
        return "on" if on else "off"
    if on and month != last_month:
        return "on"
    if not on and bond_ready and not bond_held:
        return "off"
    return None


def rotation_targets(scores, top, invested=0.98, above=None, bond="IEF"):
    """Equal weights on the ``top`` highest ``scores``; a pick not ``above``
    its own average holds ``bond`` in its slot."""
    picks = sorted(scores, key=lambda s: scores[s], reverse=True)[:top]
    targets = {}
    for name in picks:
        key = name if above is None or above.get(name, False) else bond
        targets[key] = targets.get(key, 0.0) + invested / len(picks)
    return targets


class EtfRotationResearch(QCAlgorithm):
    START = (1998, 1, 1)
    END = (2015, 12, 31)
    MEASURE_FROM = (1999, 1, 1)
    MODE = "sector_rot"
    TOP = 3
    SMA_MONTHS = 0            # sector_rot: 0 = off, else the per-ETF trend check
    LEVERAGE = 2.0            # lev_trend
    TREND_DAYS = 200          # lev_trend
    BOND = "IEF"
    SPREAD = 0.015
    CASH = 1_000_000

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        self.spy = self.AddEquity("SPY", Resolution.Daily, leverage=4).Symbol
        self.bond = self.AddEquity(self.BOND, Resolution.Daily).Symbol
        self.sym = {n: self.AddEquity(n, Resolution.Daily).Symbol for n in SECTORS} if self.MODE == "sector_rot" else {}
        self.sma = self.SMA(self.spy, self.TREND_DAYS, Resolution.Daily)
        self.SetWarmUp(self.TREND_DAYS + 10, Resolution.Daily)
        self.curve, self.px = [], []
        self.financing = 0.0
        self.switches = 0
        self.last_on = None
        self.last_lev_month = None
        if self.MODE == "sector_rot":
            self.Schedule.On(self.DateRules.MonthStart(self.spy), self.TimeRules.At(8, 0), self.Rotate)

    def Rotate(self):
        if self.IsWarmingUp:
            return
        hist = self.History(list(self.sym.values()), 260, Resolution.Daily)
        if hist.empty:
            return
        closes = hist["close"].unstack(level=0)
        scores, above = {}, {}
        for n, s in self.sym.items():
            if s not in closes.columns:
                continue
            px = closes[s].dropna()
            if len(px) < 253:
                continue
            scores[s] = sum(px.iloc[-1] / px.iloc[-1 - 21 * m] - 1.0 for m in (3, 6, 12)) / 3.0
            if self.SMA_MONTHS:
                above[s] = px.iloc[-1] > px.iloc[-21 * self.SMA_MONTHS:].mean()
        if not scores:
            return
        targets = rotation_targets(scores, self.TOP, above=above if self.SMA_MONTHS else None, bond=self.bond)
        for kvp in self.Portfolio:
            if kvp.Value.Invested and kvp.Key not in targets:
                self.Liquidate(kvp.Key)
        self.SetHoldings([PortfolioTarget(s, w) for s, w in targets.items()])
        self.switches += 1

    def OnData(self, data):
        bar = data.Bars.get(self.spy)
        if bar is None or self.IsWarmingUp:
            return
        equity = float(self.Portfolio.TotalPortfolioValue)
        self.curve.append((self.Time, equity))
        self.px.append((self.Time, float(bar.Close)))
        if self.MODE == "lev_trend" and self.sma.IsReady:
            on = float(bar.Close) > self.sma.Current.Value
            month = (self.Time.year, self.Time.month)
            bond = self.Securities[self.bond]
            action = lev_trend_action(on, self.last_on, month, self.last_lev_month,
                                      bond.HasData and bond.Price > 0, self.Portfolio[self.bond].Invested)
            if action == "on":
                self.SetHoldings([PortfolioTarget(self.bond, 0), PortfolioTarget(self.spy, 0.98 * self.LEVERAGE)])
                self.last_lev_month = month
            elif action == "off":
                self.SetHoldings([PortfolioTarget(self.spy, 0), PortfolioTarget(self.bond, 0.98)])
            if on != self.last_on:
                self.switches += 1
            self.last_on = on
        borrowed = max(0.0, float(self.Portfolio.TotalHoldingsValue) - equity)
        if borrowed > 0:
            key = self.Time.year * 100 + self.Time.month
            rate = RATES.get(key, RATES[max(k for k in RATES if k <= key)]) + self.SPREAD
            cost = borrowed * rate / 252.0
            self.Portfolio.CashBook["USD"].AddAmount(-cost)
            self.financing += cost

    def _stats(self, curve, a, b):
        pts = [(t, v) for t, v in curve if a <= t.date() <= b and v > 0]
        if len(pts) < 20:
            return "no-data"
        years = (pts[-1][0] - pts[0][0]).days / 365.25
        cagr = (pts[-1][1] / pts[0][1]) ** (1 / years) - 1
        peak, mdd = 0.0, 0.0
        for _, v in pts:
            peak = max(peak, v)
            mdd = max(mdd, 1 - v / peak)
        return "C={:.4f} DD={:.4f}".format(cagr, mdd)

    def OnEndOfAlgorithm(self):
        a, b = date(*self.MEASURE_FROM), date(*self.END)
        spans = [("ALL", a, b), ("1999-02", date(1999, 1, 1), date(2002, 12, 31)),
                 ("2008-09", date(2008, 1, 1), date(2009, 12, 31)), ("2009-15", date(2009, 1, 1), date(2015, 12, 31)),
                 ("2016-", date(2016, 1, 1), b)]
        for label, x, y in spans:
            self.SetRuntimeStatistic("S " + label, self._stats(self.curve, x, y))
            self.SetRuntimeStatistic("B " + label, self._stats(self.px, x, y))
        self.SetRuntimeStatistic("Mode", "{} top={} sma={} lev={} switches={} fin={:.0f}".format(
            self.MODE, self.TOP, self.SMA_MONTHS, self.LEVERAGE, self.switches, self.financing))
