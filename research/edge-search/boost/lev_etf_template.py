# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Run on QuantConnect Cloud for "Boost without
# borrowing" in docs/research/boost-followup-2026-09.md. It cannot be
# imported outside QuantConnect (AlgorithmImports); the margin-rate table
# below is filled in by build_variants.py from a local FRED TB3MS CSV.
#
# Provenance: boost_etf: PROJECT -- boost/template.py's Boost rule (RSI-2
#   entry and 5-day SMA exit of Connors & Alvarez 2008) with the extra
#   exposure taken by swapping part of the SPY position into a leveraged
#   S&P 500 fund (LEV_ETF, LEV_FACTOR times the index daily) instead of
#   borrowing, so an account that cannot borrow (an IRA) could hold it.
#   boost and bh: as boost/template.py.
from AlgorithmImports import *
from datetime import date

# Monthly 3-month T-bill rate (FRED TB3MS), each month's rate being the
# PREVIOUS month's average (no look-ahead). Margin financing only.
RATES = __RATES__


class BoostResearch(QCAlgorithm):
    """Boost deep-dive, research only (docs/research/edge-search-2026-09.md,
    "Boost"). Hold TICKER at 98%; while RSI(2) < RSI_ENTRY with the close
    above its TREND_DAYS SMA (Connors & Alvarez 2008), hold BOOST_SIZE x
    98% until the close is above its EXIT_SMA-day SMA.
    MODE "boost" as above; "const" holds CONST_LEV x 98% always, rebalanced
    monthly (the leverage-only benchmark); "bh" holds 98%.
    Borrowed exposure pays the prior month's T-bill rate + SPREAD daily."""

    TICKER = "SPY"
    LEV_ETF = "SSO"           # boost_etf: the leveraged S&P 500 fund
    LEV_FACTOR = 2.0          # its daily multiple of the index
    START = (1993, 1, 1)
    END = (2026, 6, 30)
    MODE = "boost"
    BOOST_SIZE = 1.5
    CONST_LEV = 1.0
    RSI_ENTRY = 10
    TREND_DAYS = 200          # 0 = no trend filter
    EXIT_SMA = 5
    SPREAD = 0.015
    SLIPPAGE = 0.0            # fraction of price per fill
    CASH = 1_000_000

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        sec = self.AddEquity(self.TICKER, Resolution.Daily, leverage=4)
        if self.SLIPPAGE:
            sec.SetSlippageModel(ConstantSlippageModel(self.SLIPPAGE))
        self.sym = sec.Symbol
        self.lev = None
        if self.MODE == "boost_etf":
            lev = self.AddEquity(self.LEV_ETF, Resolution.Daily)
            if self.SLIPPAGE:
                lev.SetSlippageModel(ConstantSlippageModel(self.SLIPPAGE))
            self.lev = lev.Symbol
        self.trend = self.SMA(self.sym, self.TREND_DAYS, Resolution.Daily) if self.TREND_DAYS else None
        self.exit_sma = self.SMA(self.sym, self.EXIT_SMA, Resolution.Daily)
        self.rsi2 = self.RSI(self.sym, 2, MovingAverageType.Wilders, Resolution.Daily)
        self.SetWarmUp(max(210, self.TREND_DAYS + 10), Resolution.Daily)
        self.curve, self.px = [], []
        self.sig = False
        self.last_want = None
        self.last_month = None
        self.prev_close = None
        self.prev_w = 0.0
        self.financing = 0.0
        self.episodes = 0
        self.w_sum = 0.0
        self.days = 0
        self.boost_days = 0
        self.boost_ret = 0.0
        self.norm_ret = 0.0

    def OnData(self, data):
        bar = data.Bars.get(self.sym)
        if bar is None or self.IsWarmingUp:
            if bar is not None:
                self.prev_close = float(bar.Close)
            return
        close = float(bar.Close)
        equity = float(self.Portfolio.TotalPortfolioValue)
        if self.prev_close:
            r = close / self.prev_close - 1.0
            if self.prev_w > 1.2:
                self.boost_days += 1
                self.boost_ret += r
            else:
                self.norm_ret += r
        self.prev_close = close
        self.curve.append((self.Time, equity))
        self.px.append((self.Time, close))

        trend_ok = self.trend is None or (self.trend.IsReady and close > self.trend.Current.Value)
        if not self.sig and trend_ok and self.rsi2.IsReady and self.rsi2.Current.Value < self.RSI_ENTRY:
            self.sig = True
            self.episodes += 1
        elif self.sig and close > self.exit_sma.Current.Value:
            self.sig = False

        if self.MODE == "boost_etf":
            # Same total exposure as borrowing: 0.98 x BOOST_SIZE of the index,
            # from SPY plus x in the fund, where x (LEV_FACTOR - 1) is the
            # extra 0.98 (BOOST_SIZE - 1). Nothing is borrowed.
            x = 0.98 * (self.BOOST_SIZE - 1.0) / (self.LEV_FACTOR - 1.0) if self.sig else 0.0
            want = round(x, 6)
            if want != self.last_want:
                self.SetHoldings([PortfolioTarget(self.lev, x), PortfolioTarget(self.sym, 0.98 - x)])
                self.last_want = want
        elif self.MODE == "boost":
            want = 0.98 * (self.BOOST_SIZE if self.sig else 1.0)
            if want != self.last_want:
                self.SetHoldings(self.sym, want)
                self.last_want = want
        else:
            want = 0.98 * (self.CONST_LEV if self.MODE == "const" else 1.0)
            month = (self.Time.year, self.Time.month)
            if month != self.last_month:
                self.SetHoldings(self.sym, want)
                self.last_month = month

        held = float(self.Portfolio[self.sym].HoldingsValue)
        invested = held                  # money actually in positions: what borrowing is measured on
        if self.lev is not None:
            fund = float(self.Portfolio[self.lev].HoldingsValue)
            invested += fund
            # Index exposure: the fund counts LEV_FACTOR times its value.
            held += self.LEV_FACTOR * fund
        self.prev_w = held / equity if equity > 0 else 0.0
        self.w_sum += self.prev_w
        self.days += 1
        borrowed = max(0.0, invested - equity)
        if borrowed > 0:
            key = self.Time.year * 100 + self.Time.month
            rate = RATES.get(key, RATES[max(k for k in RATES if k <= key)]) + self.SPREAD
            cost = borrowed * rate / 252.0
            self.Portfolio.CashBook["USD"].AddAmount(-cost)
            self.financing += cost

    def _stats(self, curve, a, b):
        pts = [(t, v) for t, v in curve if a <= t.date() <= b and v > 0]
        if len(pts) < 50:
            return "no-data"
        years = (pts[-1][0] - pts[0][0]).days / 365.25
        cagr = (pts[-1][1] / pts[0][1]) ** (1 / years) - 1
        peak, mdd = 0.0, 0.0
        rets = []
        for i, (_, v) in enumerate(pts):
            peak = max(peak, v)
            mdd = max(mdd, 1 - v / peak)
            if i:
                rets.append(v / pts[i - 1][1] - 1)
        m = sum(rets) / len(rets)
        sd = (sum((x - m) ** 2 for x in rets) / len(rets)) ** 0.5
        return "{}..{} C={:.4f} DD={:.4f} V={:.3f}".format(
            pts[0][0].date(), pts[-1][0].date(), cagr, mdd, sd * 252 ** 0.5)

    def OnEndOfAlgorithm(self):
        spans = [("RUN", date(*self.START), date(*self.END)),
                 ("ALL", date(1994, 1, 1), date(*self.END)),
                 ("FRESH", date(1994, 1, 1), date(1998, 12, 31)),
                 ("IS", date(1999, 1, 1), date(2015, 12, 31)),
                 ("OOS", date(2016, 1, 1), date(*self.END)),
                 ("Y2008", date(2008, 1, 1), date(2008, 12, 31)),
                 ("Y2020", date(2020, 1, 1), date(2020, 12, 31)),
                 ("Y2022", date(2022, 1, 1), date(2022, 12, 31))]
        for label, a, b in spans:
            self.SetRuntimeStatistic("S " + label, self._stats(self.curve, a, b))
            self.SetRuntimeStatistic("B " + label, self._stats(self.px, a, b))
        n = max(1, self.days - self.boost_days)
        self.SetRuntimeStatistic("Mode", "{} {} avgW={:.3f} boostDays={} eps={} fin={:.0f}".format(
            self.MODE, self.TICKER, self.w_sum / max(1, self.days), self.boost_days, self.episodes, self.financing))
        self.SetRuntimeStatistic("Edge", "boostBp={:.1f} normBp={:.1f}".format(
            1e4 * self.boost_ret / max(1, self.boost_days), 1e4 * self.norm_ret / n))
