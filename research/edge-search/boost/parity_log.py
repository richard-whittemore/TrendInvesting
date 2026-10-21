# Research script, not production code. Run on QuantConnect Cloud to export
# the parity check's Boost inputs (docs/adr/0026, point 7, item 2;
# docs/research/parity-2026-10.md): with PARITY on, every bar the indicators
# see, warm-up included, is exported as [date, close, RSI(2), 200-day mean,
# 5-day mean, state, target], an indicator not ready and the state and
# target during warm-up being null. Numbers are written with repr (json), so
# they round-trip exactly.
#
# The Free plan's log holds 10 KB and its object store cannot be written,
# so the export rides in order tags: each order Boost places gets the rows
# since the last with UpdateTag, which changes no fill. Rows not yet sent
# after PARITY_CHUNK sessions, on each of the last 10 days before END, and at
# the end, go on a limit order to buy 1 share at 1% of the price, cancelled
# at once; "Parity" counts the rows, the tags and those orders.
#
# This is boost/template.py with additions only (a test checks that every
# code line of it is kept), so it trades exactly as template.py does.
#
# template.py's own header follows.
#
# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Preserved as the code run on QuantConnect Cloud for
# the follow-up investigation (docs/research/boost-followup-2026-09.md). It
# cannot be imported outside QuantConnect (AlgorithmImports); the rate table
# (__RATES__ below, where present) is filled in by build_variants.py from a
# local FRED TB3MS CSV, and research/edge-search/README.md explains the
# stand-in used to test it locally.
#
# Provenance: boost: PUBLISHED, ADAPTED -- the RSI-2 entry and 5-day SMA exit
#   of Connors & Alvarez (2008) used to raise SPY exposure rather than to
#   enter from cash (PROJECT combination). const and bh: PROJECT controls.
from AlgorithmImports import *
from datetime import date
import json

#: Order-tag prefix of an exported chunk of sessions (cmd/parity reads it).
BOOST_PARITY_PREFIX = "BPARITY1 "


def parity_row(when, close, rsi2, trend, exit_sma):
    """One bar's row before the rule runs: the state and target are null
    until set_parity_state fills them."""
    ready = lambda ind: float(ind.Current.Value) if ind is not None and ind.IsReady else None
    return [when.strftime("%Y-%m-%d"), float(close), ready(rsi2), ready(trend), ready(exit_sma), None, None]


def set_parity_state(row, sig, want):
    row[5], row[6] = int(bool(sig)), float(want)


def parity_chunk(rows):
    return BOOST_PARITY_PREFIX + json.dumps({"rows": rows}, separators=(",", ":"), allow_nan=False)


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
    PARITY = False            # export every bar's close, indicators and state in order tags
    PARITY_CHUNK = 1500       # sessions per tag at most
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
        if self.PARITY and bar is not None:
            self.parity_rows = getattr(self, "parity_rows", [])
            self.parity_rows.append(parity_row(self.Time, bar.Close, self.rsi2, self.trend, self.exit_sma))
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
        if self.PARITY and self.MODE == "boost":
            set_parity_state(self.parity_rows[-1], self.sig, 0.98 * (self.BOOST_SIZE if self.sig else 1.0))

        if self.MODE == "boost":
            want = 0.98 * (self.BOOST_SIZE if self.sig else 1.0)
            if want != self.last_want:
                self.SetHoldings(self.sym, want)
                self.last_want = want
                if self.PARITY:
                    self._parity_flush(order_placed=True)
        else:
            want = 0.98 * (self.CONST_LEV if self.MODE == "const" else 1.0)
            month = (self.Time.year, self.Time.month)
            if month != self.last_month:
                self.SetHoldings(self.sym, want)
                self.last_month = month

        if self.PARITY and (len(self.parity_rows) >= self.PARITY_CHUNK
                            or date(*self.END).toordinal() - self.Time.date().toordinal() <= 10):
            self._parity_flush(order_placed=False)
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

    def _parity_flush(self, order_placed):
        """Send the rows not yet sent: on the order just placed, or else on
        a 1-share limit order at 1% of the price, cancelled at once."""
        if not getattr(self, "parity_rows", None):
            return
        tag = parity_chunk(self.parity_rows)
        tickets = sorted(self.Transactions.GetOpenOrderTickets(), key=lambda t: t.OrderId) if order_placed else []
        if not (tickets and bool(tickets[-1].UpdateTag(tag).IsSuccess)):
            price = float(self.Securities[self.sym].Price)
            ticket = self.LimitOrder(self.sym, 1, round(max(price, 1.0) * 0.01, 2), tag=tag)
            ticket.Cancel()
            self.parity_dummies = getattr(self, "parity_dummies", 0) + 1
        self.parity_sent = getattr(self, "parity_sent", 0) + len(self.parity_rows)
        self.parity_tags = getattr(self, "parity_tags", 0) + 1
        self.parity_rows = []

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
        if self.PARITY:
            self._parity_flush(order_placed=False)
            self.SetRuntimeStatistic("Parity", "rows={} tags={} dummies={}".format(
                getattr(self, "parity_sent", 0), getattr(self, "parity_tags", 0), getattr(self, "parity_dummies", 0)))
        spans = [("ALL", date(1994, 1, 1), date(*self.END)),
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
