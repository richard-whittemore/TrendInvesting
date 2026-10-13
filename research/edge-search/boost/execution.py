# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Run on QuantConnect Cloud for the execution test
# (M1 against Interactive Brokers) in docs/research/boost-followup-2026-09.md.
# It cannot be imported outside QuantConnect (AlgorithmImports); the rate
# table (__RATES__ below) is filled in by build_variants.py from a local FRED
# TB3MS CSV, and research/edge-search/README.md explains the stand-in used to
# test it locally.
#
# Provenance: boost/template.py's rule (the RSI-2 entry and 5-day SMA exit of
#   Connors & Alvarez (2008) used to raise SPY exposure; PROJECT combination),
#   unchanged, on minute data so that the time of each decision and fill can
#   be set. The indicators are computed here from the daily closes, so a
#   decision made before the close can use the price at that moment as a
#   provisional close. bh: PROJECT control.
from AlgorithmImports import *
from collections import deque
from datetime import date, timedelta

# Monthly 3-month T-bill rate (FRED TB3MS), each month's rate being the
# PREVIOUS month's average (no look-ahead). Margin financing only.
RATES = __RATES__


class Wilder:
    """Relative strength index with Wilder's smoothing, as LEAN's RSI with
    MovingAverageType.Wilders: the first average gain and loss are the simple
    averages of the first ``period`` changes, then each new change ``x``
    gives ``avg = (avg * (period - 1) + x) / period``."""

    def __init__(self, period):
        self.period = period
        self.prev = None
        self.seed = []
        self.gain = self.loss = None

    def _step(self, close):
        change = close - self.prev
        up, down = max(change, 0.0), max(-change, 0.0)
        if self.gain is None:
            seed = self.seed + [(up, down)]
            if len(seed) < self.period:
                return None, None, seed
            return (sum(u for u, _ in seed) / self.period,
                    sum(d for _, d in seed) / self.period, seed)
        n = self.period
        return (self.gain * (n - 1) + up) / n, (self.loss * (n - 1) + down) / n, self.seed

    @staticmethod
    def _value(gain, loss):
        if gain is None:
            return None
        if loss == 0:
            return 100.0
        return 100.0 - 100.0 / (1.0 + gain / loss)

    def update(self, close):
        """Add a final daily close."""
        if self.prev is not None:
            self.gain, self.loss, self.seed = self._step(close)
        self.prev = close

    def peek(self, price):
        """The RSI if ``price`` were the next close; nothing is stored."""
        if self.prev is None:
            return None
        gain, loss, _ = self._step(price)
        return self._value(gain, loss)


class Average:
    """Simple moving average of the last ``period`` daily closes."""

    def __init__(self, period):
        self.period = period
        self.closes = deque(maxlen=period)

    def update(self, close):
        self.closes.append(close)

    def peek(self, price):
        """The average if ``price`` were the next close; None until ready."""
        if len(self.closes) < self.period - 1:
            return None
        last = list(self.closes)[-(self.period - 1):] if self.period > 1 else []
        return (sum(last) + price) / self.period


class BoostExecution(QCAlgorithm):
    """Boost as it could be traded at M1 or Interactive Brokers, research only.

    The rule is boost/template.py's: hold TICKER at 98%; while RSI(2) <
    RSI_ENTRY with the close above its TREND_DAYS SMA, hold BOOST_SIZE x 98%
    until the close is above its EXIT_SMA-day SMA. MODE "bh" holds 98%,
    rebalanced monthly.

    EXEC sets when each decision is made and filled:
      "next_open"  decided on the day's close, a market order at 9:31 the
                   next session (M1's morning window).
      "afternoon"  decided at 14:50 on that minute's price as a provisional
                   close, a market order at 15:00 (M1's afternoon window).
      "moc"        decided at 15:44 on that minute's price as a provisional
                   close, a market-on-close order the same day (Interactive
                   Brokers).
    EXEC_DELAY holds each decision that many more sessions before it is
    traded (a change made late by hand). Borrowed exposure pays the prior
    month's T-bill rate + SPREAD daily. FEES "ib" keeps LEAN's Interactive
    Brokers commissions; "none" charges nothing (M1)."""

    TICKER = "SPY"
    START = (1998, 1, 1)
    END = (2026, 6, 30)
    MODE = "boost"
    EXEC = "next_open"
    EXEC_DELAY = 0
    BOOST_SIZE = 1.5
    RSI_ENTRY = 10
    TREND_DAYS = 200
    EXIT_SMA = 5
    SPREAD = 0.015
    FEES = "ib"
    CASH = 1_000_000

    #: Minutes before the close at which each intraday decision is made, and
    #: (for "afternoon") the order sent. LEAN accepts a market-on-close order
    #: until 15.5 minutes before the close.
    DECIDE_BEFORE_CLOSE = {"afternoon": 70, "moc": 16}
    TRADE_BEFORE_CLOSE = {"afternoon": 60}

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        sec = self.AddEquity(self.TICKER, Resolution.Minute, leverage=4)
        if self.FEES == "none":
            sec.SetFeeModel(ConstantFeeModel(0))
        self.sym = sec.Symbol
        self.rsi2 = Wilder(2)
        self.trend = Average(self.TREND_DAYS)
        self.exit_sma = Average(self.EXIT_SMA)
        self.Consolidate(self.sym, Resolution.Daily, self.OnDailyBar)
        self.SetWarmUp(timedelta(days=int((self.TREND_DAYS + 20) * 1.5)))
        rules = self.DateRules.EveryDay(self.sym)
        if self.EXEC == "next_open":
            self.Schedule.On(rules, self.TimeRules.AfterMarketOpen(self.sym, 1), self.TradeDue)
        else:
            self.Schedule.On(rules, self.TimeRules.BeforeMarketClose(self.sym, self.DECIDE_BEFORE_CLOSE[self.EXEC]),
                             self.DecideIntraday)
            if self.EXEC == "afternoon":
                self.Schedule.On(rules, self.TimeRules.BeforeMarketClose(self.sym, self.TRADE_BEFORE_CLOSE["afternoon"]),
                                 self.TradeDue)
        self._reset_state()

    def _reset_state(self):
        self.queue = deque()
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
        self.decisions = 0
        self.trades = 0

    # -- the rule ---------------------------------------------------------

    def _decide(self, price):
        """The target weight with ``price`` as today's close, updating the
        signal; the same state machine as boost/template.py."""
        if self.MODE == "bh":
            return 0.98
        trend = self.trend.peek(price) if self.TREND_DAYS else None
        trend_ok = not self.TREND_DAYS or (trend is not None and price > trend)
        rsi = self.rsi2.peek(price)
        exit_avg = self.exit_sma.peek(price)
        if not self.sig and trend_ok and rsi is not None and rsi < self.RSI_ENTRY:
            self.sig = True
            self.episodes += 1
        elif self.sig and exit_avg is not None and price > exit_avg:
            self.sig = False
        return 0.98 * (self.BOOST_SIZE if self.sig else 1.0)

    def _queue(self, want):
        self.queue.append(want)
        self.decisions += 1

    def _due(self):
        """The decision now due, or None: each waits EXEC_DELAY sessions."""
        if len(self.queue) > self.EXEC_DELAY:
            return self.queue.popleft()
        return None

    def _trade(self, want, on_close=False):
        if self.MODE == "bh":
            month = (self.Time.year, self.Time.month)
            if month == self.last_month:
                return
            self.last_month = month
        elif want == self.last_want:
            return
        self.last_want = want
        self.trades += 1
        if on_close:
            qty = self.CalculateOrderQuantity(self.sym, want)
            if qty:
                self.MarketOnCloseOrder(self.sym, qty)
        else:
            self.SetHoldings(self.sym, want)

    # -- scheduled events -------------------------------------------------

    def DecideIntraday(self):
        if self.IsWarmingUp:
            return
        price = float(self.Securities[self.sym].Price)
        if price <= 0:
            return
        self._queue(self._decide(price))
        if self.EXEC == "moc":
            want = self._due()
            if want is not None:
                self._trade(want, on_close=True)

    def TradeDue(self):
        if self.IsWarmingUp:
            return
        want = self._due()
        if want is not None:
            self._trade(want)

    def OnDailyBar(self, bar):
        close = float(bar.Close)
        if not self.IsWarmingUp:
            if self.EXEC == "next_open":
                self._queue(self._decide(close))
            self._book(close)
        # The final close goes into the indicators after any decision on it.
        self.rsi2.update(close)
        self.trend.update(close)
        self.exit_sma.update(close)

    def _book(self, close):
        """End-of-day records and the day's margin interest."""
        equity = float(self.Portfolio.TotalPortfolioValue)
        if self.prev_w > 1.2:
            self.boost_days += 1
        self.curve.append((self.Time, equity))
        self.px.append((self.Time, close))
        held = float(self.Portfolio[self.sym].HoldingsValue)
        self.prev_w = held / equity if equity > 0 else 0.0
        self.w_sum += self.prev_w
        self.days += 1
        borrowed = max(0.0, held - equity)
        if borrowed > 0:
            key = self.Time.year * 100 + self.Time.month
            rate = RATES.get(key, RATES[max(k for k in RATES if k <= key)]) + self.SPREAD
            cost = borrowed * rate / 252.0
            self.Portfolio.CashBook["USD"].AddAmount(-cost)
            self.financing += cost

    # -- statistics -------------------------------------------------------

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
        spans = [("ALL", date(1999, 1, 1), date(*self.END)),
                 ("IS", date(1999, 1, 1), date(2015, 12, 31)),
                 ("OOS", date(2016, 1, 1), date(*self.END)),
                 ("Y2008", date(2008, 1, 1), date(2008, 12, 31)),
                 ("Y2020", date(2020, 1, 1), date(2020, 12, 31)),
                 ("Y2022", date(2022, 1, 1), date(2022, 12, 31))]
        for label, a, b in spans:
            self.SetRuntimeStatistic("S " + label, self._stats(self.curve, a, b))
            self.SetRuntimeStatistic("B " + label, self._stats(self.px, a, b))
        self.SetRuntimeStatistic("Mode", "{} {} exec={} delay={} avgW={:.3f} boostDays={} eps={} fin={:.0f}".format(
            self.MODE, self.TICKER, self.EXEC, self.EXEC_DELAY, self.w_sum / max(1, self.days),
            self.boost_days, self.episodes, self.financing))
        self.SetRuntimeStatistic("Orders", "decisions={} trades={} queued={}".format(
            self.decisions, self.trades, len(self.queue)))
