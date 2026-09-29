# Research script, not production code: no TDD, no ADR citations, no
# fidelity check against the Go engine. Preserved as the exact code run on
# QuantConnect Cloud for the "prove an edge" investigation
# (docs/research/edge-search-2026-09.md). Cannot be imported or unit tested
# outside QuantConnect's own environment (AlgorithmImports); see
# research/edge-search/README.md.
#
# In-sample run: 1998-01-01 to 2015-12-31, measured from 1999-01-01
# (START_DATE/END_DATE/MEASURE_FROM below), MODE one of "rsi2", "tom",
# "overnight", "halloween", "faber_lev", "core_rsi2", "boost". See
# research/edge-search/README.md for the switches, including the "Core" and
# "Boost" combinations and their held-out runs.
#
# Provenance (PUBLISHED: the rule of a named source; ADAPTED: changed as
# stated; PROJECT: this project's own). Full references are in
# docs/research/edge-search-2026-09.md, "Sources for the rules". Page-level
# citations are not given: the sources were not transcribed page by page.
# rsi2, tom, overnight, halloween: PUBLISHED, ADAPTED -- rules of the named
#   sources, traded in SPY with idle money in SHY.
# faber_lev: PUBLISHED, ADAPTED -- Faber (2007) with leverage and a flat
#   financing charge added by this project.
# core_rsi2, boost: PROJECT -- this project's combinations of RSI-2 with
#   buy-and-hold SPY.
from AlgorithmImports import *
from datetime import date, timedelta


class TimingResearch(QCAlgorithm):
    """Published SPY timing effects, research only. Idle money sits in SHY
    (short Treasuries) once SHY exists, else cash.

    MODE:
      "rsi2"      Connors & Alvarez (2008): with SPY above its 200-day SMA, buy
                  at the close when RSI(2) < 10; sell at the close when SPY
                  closes above its 5-day SMA.
      "tom"       Turn of the month (Ariel 1987; McConnell & Xu 2008): hold SPY
                  from the close of the second-to-last trading day of the
                  month (so the position is on for the last day) through the
                  close of the 3rd trading day of the next month.
      "overnight" Cliff, Cooper & Gulen (2008): buy at the close, sell at the
                  next open, every day.
      "halloween" Bouman & Jacobsen (2002): SPY November through April, SHY
                  May through October.
      "faber_lev" Faber's 10-month SMA rule on SPY/EFA/IEF/VNQ/DBC with gross
                  exposure LEVERAGE; a flat FINANCING_RATE a year is deducted
                  daily on borrowed exposure (LEAN's margin model charges no
                  interest by default).
      "core_rsi2" CORE of equity always in SPY; the rest is an RSI-2 sleeve,
                  otherwise SHY.
      "boost"     100% SPY, BOOST_SIZE while RSI-2 signals; FINANCING_RATE on
                  borrowed exposure.
    """

    MODE = "rsi2"
    START_DATE = (1998, 1, 1)
    END_DATE = (2015, 12, 31)
    MEASURE_FROM = (1999, 1, 1)
    CASH = 1_000_000
    LEVERAGE = 1.5
    FINANCING_RATE = 0.03          # flat annual cost on borrowed exposure (conservative proxy)
    RSI_ENTRY = 10                 # rsi2: buy below this RSI(2)
    RSI_EXIT = None                # rsi2: if set, sell when RSI(2) rises above it instead of close > SMA(5)
    RSI_TREND_FILTER = True        # rsi2: require close above the 200-day SMA
    RSI_SIZE = 0.98                # rsi2: fraction of equity while in (above 1 means margin)
    CORE = 0.80                    # core_rsi2: share always held in SPY; the rest is an RSI-2 sleeve
    BOOST_SIZE = 1.5               # boost: SPY exposure while RSI-2 is signalling (100% otherwise)
    OVERNIGHT_COST_BPS = 2.0       # commission + spread per round trip, basis points

    def Initialize(self):
        self.SetStartDate(*self.START_DATE)
        self.SetEndDate(*self.END_DATE)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        self.spy = self.AddEquity("SPY", Resolution.Daily, leverage=3).Symbol
        self.shy = self.AddEquity("SHY", Resolution.Daily).Symbol
        self.sma200 = self.SMA(self.spy, 200, Resolution.Daily)
        self.sma5 = self.SMA(self.spy, 5, Resolution.Daily)
        self.rsi2 = self.RSI(self.spy, 2, MovingAverageType.Wilders, Resolution.Daily)
        self.SetWarmUp(210, Resolution.Daily)
        self.equity_curve, self.spy_curve = [], []
        self.days_in = 0
        self.days_total = 0
        self.trades = 0
        self.financing_paid = 0.0
        # Overnight is computed from daily bars (close -> next open) as a
        # synthetic account, since daily-resolution orders cannot be timed at
        # the close; each round trip pays OVERNIGHT_COST_BPS.
        self.prev_close = None
        self.synthetic = float(self.CASH)
        if self.MODE == "faber_lev":
            self.assets = ["SPY", "EFA", "IEF", "VNQ", "DBC"]
            self.sym = {n: self.AddEquity(n, Resolution.Daily, leverage=3).Symbol for n in self.assets}
            self.sym["SPY"] = self.spy
            self.monthly = {n: [] for n in self.assets}
            self.Schedule.On(self.DateRules.MonthStart(self.spy), self.TimeRules.At(8, 0), self.FaberRebalance)

    # ------------------------------------------------------------------ helpers
    def _park(self):
        """Idle money goes to SHY when it has data, else stays in cash."""
        if self.Securities[self.shy].HasData and self.Securities[self.shy].Price > 0:
            self.SetHoldings(self.shy, 0.98)

    def _go_spy(self, size=0.98):
        if self.Portfolio[self.shy].Invested:
            self.Liquidate(self.shy)
        self.SetHoldings(self.spy, size)
        self.trades += 1

    def _go_idle(self):
        if self.Portfolio[self.spy].Invested:
            self.Liquidate(self.spy)
        self._park()

    def _trading_day_index(self):
        """(index from start of month, index from end of month) for today,
        using the exchange calendar."""
        hours = self.Securities[self.spy].Exchange.Hours
        d = self.Time.date()
        first = date(d.year, d.month, 1)
        nxt = date(d.year + (d.month == 12), d.month % 12 + 1, 1)
        days = []
        cur = first
        while cur < nxt:
            if hours.IsDateOpen(datetime(cur.year, cur.month, cur.day)):
                days.append(cur)
            cur += timedelta(days=1)
        if d not in days:
            return None, None
        i = days.index(d)
        return i + 1, len(days) - i   # 1-based from start; 1 == last day from end

    # ---------------------------------------------------------------- per-day
    def OnData(self, data):
        bar = data.Bars.get(self.spy)
        if bar is None or self.IsWarmingUp:
            return
        self.spy_curve.append((self.Time, float(bar.Close)))
        self.equity_curve.append((self.Time, float(self.Portfolio.TotalPortfolioValue)))
        self.days_total += 1
        if self.MODE not in ("core_rsi2", "boost") and self.Portfolio[self.spy].Invested:
            self.days_in += 1

        if self.MODE == "overnight":
            if self.prev_close:
                ret = float(bar.Open) / self.prev_close - 1.0
                self.synthetic *= (1.0 + ret) * (1.0 - self.OVERNIGHT_COST_BPS / 10000.0)
                self.trades += 1
                self.days_in += 1
            self.prev_close = float(bar.Close)
            self.equity_curve[-1] = (self.Time, self.synthetic)
            return
        if self.MODE in ("core_rsi2", "boost"):
            trend_ok = self.sma200.IsReady and bar.Close > self.sma200.Current.Value
            if not hasattr(self, "_sig"):
                self._sig = False
            if not self._sig and trend_ok and self.rsi2.IsReady and self.rsi2.Current.Value < self.RSI_ENTRY:
                self._sig = True
                self.trades += 1
            elif self._sig and bar.Close > self.sma5.Current.Value:
                self._sig = False
            if self.MODE == "core_rsi2":
                spy_w = 0.98 * (self.CORE + (1 - self.CORE) * (1 if self._sig else 0))
                shy_w = 0.98 - spy_w if self.Securities[self.shy].HasData and self.Securities[self.shy].Price > 0 else 0.0
            else:
                spy_w = 0.98 * (self.BOOST_SIZE if self._sig else 1.0)
                shy_w = 0.0
            want = (round(spy_w, 3), round(shy_w, 3))
            if getattr(self, "_last_want", None) != want:
                self.SetHoldings([PortfolioTarget(self.shy, shy_w), PortfolioTarget(self.spy, spy_w)])
                self._last_want = want
            # In these modes SPY is held every day, so days_in counts
            # signal days only.
            if self._sig:
                self.days_in += 1
            borrowed = max(0.0, float(self.Portfolio.TotalHoldingsValue) - float(self.Portfolio.TotalPortfolioValue))
            cost = borrowed * self.FINANCING_RATE / 252.0
            if cost > 0:
                self.Portfolio.CashBook["USD"].AddAmount(-cost)
                self.financing_paid += cost
            return
        if self.MODE == "rsi2":
            invested = self.Portfolio[self.spy].Invested
            trend_ok = (not self.RSI_TREND_FILTER) or (self.sma200.IsReady and bar.Close > self.sma200.Current.Value)
            exit_now = (self.rsi2.Current.Value > self.RSI_EXIT) if self.RSI_EXIT else (bar.Close > self.sma5.Current.Value)
            if not invested and trend_ok and self.rsi2.IsReady and self.rsi2.Current.Value < self.RSI_ENTRY:
                self._go_spy(self.RSI_SIZE)
            elif invested and exit_now:
                self._go_idle()
            elif not invested and not self.Portfolio[self.shy].Invested:
                self._park()
        elif self.MODE == "tom":
            from_start, from_end = self._trading_day_index()
            if from_start is None:
                return
            invested = self.Portfolio[self.spy].Invested
            # Orders placed on today's daily bar fill at the next open, so to
            # be long for the last trading day, decide on the day before it.
            if not invested and from_end == 2:
                self._go_spy()
            elif invested and from_start == 3:
                self._go_idle()
            elif not invested and not self.Portfolio[self.shy].Invested:
                self._park()
        elif self.MODE == "halloween":
            m = self.Time.month
            want = m >= 11 or m <= 4
            if want and not self.Portfolio[self.spy].Invested:
                self._go_spy()
            elif not want and self.Portfolio[self.spy].Invested:
                self._go_idle()
            elif not want and not self.Portfolio[self.shy].Invested:
                self._park()
        elif self.MODE == "faber_lev":
            borrowed = max(0.0, float(self.Portfolio.TotalHoldingsValue) - float(self.Portfolio.TotalPortfolioValue))
            cost = borrowed * self.FINANCING_RATE / 252.0
            if cost > 0:
                self.Portfolio.CashBook["USD"].AddAmount(-cost)
                self.financing_paid += cost

    def FaberRebalance(self):
        if self.IsWarmingUp:
            return
        for n in self.assets:
            s = self.Securities[self.sym[n]]
            if s.HasData and s.Price > 0:
                self.monthly[n].append(float(s.Price))
        live = [n for n in self.assets if len(self.monthly[n]) >= 10]
        if not live:
            return
        w = self.LEVERAGE * 0.98 / len(live)
        targets = []
        for n in live:
            h = self.monthly[n][-10:]
            targets.append(PortfolioTarget(self.sym[n], w if h[-1] > sum(h) / len(h) else 0.0))
        self.SetHoldings(sorted(targets, key=lambda t: t.Quantity))
        self.trades += 1

    # ----------------------------------------------------------------- report
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
        spans = [("OVERALL", a, b)]
        if self.END_DATE[0] <= 2015:
            spans += [("1999-02", date(1999, 1, 1), date(2002, 12, 31)), ("2003-07", date(2003, 1, 1), date(2007, 12, 31)),
                      ("2008-09", date(2008, 1, 1), date(2009, 12, 31)), ("2009-15", date(2009, 1, 1), date(2015, 12, 31))]
        else:
            spans += [(str(y), date(y, 1, 1), date(y, 12, 31)) for y in range(a.year, b.year + 1)]
        for label, x, y in spans:
            self.SetRuntimeStatistic(label, self._stats(self.equity_curve, x, y))
            self.SetRuntimeStatistic("SPY " + label, self._stats(self.spy_curve, x, y))
        self.SetRuntimeStatistic("Mode", "{} lev={} trades={} time_in_spy={:.2f} financing={:.0f}".format(
            self.MODE, self.LEVERAGE, self.trades, self.days_in / max(1, self.days_total), self.financing_paid))
