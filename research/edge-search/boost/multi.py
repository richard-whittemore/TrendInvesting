# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Run on QuantConnect Cloud for "Boost across several
# markets" in docs/research/boost-followup-2026-09.md. It cannot be imported
# outside QuantConnect (AlgorithmImports); the rate table (__RATES__ below) is
# filled in by build_variants.py from a local FRED TB3MS CSV, and
# research/edge-search/README.md explains the stand-in used to test it
# locally.
#
# Provenance: boost/template.py's rule (the RSI-2 entry and 5-day SMA exit of
#   Connors & Alvarez (2008) used to raise exposure; PROJECT combination),
#   unchanged, applied to each fund of a fixed-weight blend on its own
#   prices (PROJECT). bh: PROJECT control, the same blend without the rule.
from AlgorithmImports import *
from datetime import date

# Monthly 3-month T-bill rate (FRED TB3MS), each month's rate being the
# PREVIOUS month's average (no look-ahead). Margin financing only.
RATES = __RATES__


def boost_step(sig, close, trend, rsi, exit_avg, rsi_entry):
    """(signal, entered) after today's close for one fund: enter while RSI(2)
    is below ``rsi_entry`` with the close above its trend average; leave on a
    close above the exit average. An indicator that is not ready is None."""
    if not sig and trend is not None and close > trend and rsi is not None and rsi < rsi_entry:
        return True, True
    if sig and exit_avg is not None and close > exit_avg:
        return False, False
    return sig, False


def sleeve_targets(weights, sigs, boost_size, invested=0.98):
    """Each fund's target weight: its share of ``invested`` (``weights`` are
    relative), times ``boost_size`` while its own signal is on."""
    total = float(sum(weights.values()))
    return {n: invested * w / total * (boost_size if sigs.get(n) else 1.0) for n, w in weights.items()}


def sells_first(targets, current):
    """``targets`` as (name, weight) pairs, the largest reductions first, so
    that sales fund the purchases."""
    return sorted(targets.items(), key=lambda kv: (kv[1] - current.get(kv[0], 0.0), kv[0]))


class BoostMulti(QCAlgorithm):
    """Boost on several funds at once, research only. SLEEVES gives each
    fund's relative share of 98%; each fund is boosted to BOOST_SIZE x its share while
    its own RSI(2) < RSI_ENTRY with its close above its TREND_DAYS SMA, until
    its close is above its EXIT_SMA-day SMA. Every fund is reset to its
    target on the first trading day of each month; between resets only a
    fund whose signal changed is traded. MODE "bh" holds the shares without
    the rule, reset monthly. Nothing is bought until every fund has a price
    and its indicators are warm, and a monthly reset waits for a session in
    which every fund has a bar. Borrowed exposure pays the prior month's
    T-bill rate + SPREAD daily."""

    SLEEVES = {"SPY": 1, "MDY": 1, "IWM": 1}
    START = (2001, 1, 1)
    END = (2026, 6, 30)
    MEASURE_FROM = (2002, 9, 1)    # every fund, EFA included, warm by then
    MODE = "boost"
    BOOST_SIZE = 1.5
    RSI_ENTRY = 10
    TREND_DAYS = 200
    EXIT_SMA = 5
    SPREAD = 0.015
    CASH = 1_000_000

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(self.CASH)
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)
        self.Settings.MinimumOrderMarginPortfolioPercentage = 0
        self.syms, self.ind = {}, {}
        for name in self.SLEEVES:
            sym = self.AddEquity(name, Resolution.Daily, leverage=4).Symbol
            self.syms[name] = sym
            self.ind[name] = (self.SMA(sym, self.TREND_DAYS, Resolution.Daily),
                              self.RSI(sym, 2, MovingAverageType.Wilders, Resolution.Daily),
                              self.SMA(sym, self.EXIT_SMA, Resolution.Daily))
        self.SetWarmUp(self.TREND_DAYS + 10, Resolution.Daily)
        self._reset_state()

    def _reset_state(self):
        self.curve = []
        self.sigs = {n: False for n in self.SLEEVES}
        self.episodes = {n: 0 for n in self.SLEEVES}
        self.last_month = None
        self.financing = 0.0
        self.gross_sum = 0.0
        self.gross_max = 0.0
        self.days = 0
        self.boosted = {}             # number of funds boosted -> days
        self.trades = 0

    @staticmethod
    def _value(indicator):
        return float(indicator.Current.Value) if indicator.IsReady else None

    def _ready(self):
        return all(self.Securities[s].Price > 0 for s in self.syms.values()) and all(
            all(i.IsReady for i in self.ind[n]) for n in self.SLEEVES)

    def _weights(self):
        equity = float(self.Portfolio.TotalPortfolioValue)
        return {n: float(self.Portfolio[s].HoldingsValue) / equity for n, s in self.syms.items()} if equity > 0 else {}

    def _trade(self, targets):
        ordered = sells_first(targets, self._weights())
        self.SetHoldings([PortfolioTarget(self.syms[n], w) for n, w in ordered])
        self.trades += len(ordered)

    def OnData(self, data):
        if self.IsWarmingUp:
            return
        # Signals run from the first warm bar, so each fund's state is right
        # when the blend starts; an entry counts only once the blend trades.
        ready = self._ready()
        changed = {}
        for name, sym in self.syms.items():
            bar = data.Bars.get(sym)
            if bar is None:
                continue
            trend, rsi, exit_sma = self.ind[name]
            sig, entered = boost_step(self.sigs[name], float(bar.Close), self._value(trend), self._value(rsi),
                                      self._value(exit_sma), self.RSI_ENTRY)
            if self.MODE == "boost" and sig != self.sigs[name]:
                changed[name] = sig
            if entered and ready and self.MODE == "boost":
                self.episodes[name] += 1
            self.sigs[name] = sig if self.MODE == "boost" else False
        if not ready:
            return
        month = (self.Time.year, self.Time.month)
        targets = sleeve_targets(self.SLEEVES, self.sigs, self.BOOST_SIZE)
        if month != self.last_month and all(data.Bars.get(s) is not None for s in self.syms.values()):
            # The monthly reset waits for a session with a bar for every fund,
            # so no fund is sized on a stale price and none misses the reset.
            self._trade(targets)
            self.last_month = month
        elif changed:
            self._trade({n: targets[n] for n in changed})
        self._book()

    def _book(self):
        """End-of-day records and the day's margin interest."""
        equity = float(self.Portfolio.TotalPortfolioValue)
        held = float(self.Portfolio.TotalHoldingsValue)
        self.curve.append((self.Time, equity))
        gross = held / equity if equity > 0 else 0.0
        self.gross_sum += gross
        self.gross_max = max(self.gross_max, gross)
        self.days += 1
        k = sum(1 for s in self.sigs.values() if s)
        self.boosted[k] = self.boosted.get(k, 0) + 1
        borrowed = max(0.0, held - equity)
        if borrowed > 0:
            key = self.Time.year * 100 + self.Time.month
            rate = RATES.get(key, RATES[max(k for k in RATES if k <= key)]) + self.SPREAD
            cost = borrowed * rate / 252.0
            self.Portfolio.CashBook["USD"].AddAmount(-cost)
            self.financing += cost

    def _stats(self, a, b):
        pts = [(t, v) for t, v in self.curve if a <= t.date() <= b and v > 0]
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
        spans = [("ALL", date(*self.MEASURE_FROM), date(*self.END)),
                 ("IS", date(*self.MEASURE_FROM), date(2015, 12, 31)),
                 ("OOS", date(2016, 1, 1), date(*self.END)),
                 ("Y2008", date(2008, 1, 1), date(2008, 12, 31)),
                 ("Y2020", date(2020, 1, 1), date(2020, 12, 31)),
                 ("Y2022", date(2022, 1, 1), date(2022, 12, 31))]
        for label, a, b in spans:
            self.SetRuntimeStatistic("S " + label, self._stats(a, b))
        self.SetRuntimeStatistic("Mode", "{} {} avgGross={:.3f} maxGross={:.3f} fin={:.0f} trades={}".format(
            self.MODE, "/".join(self.SLEEVES), self.gross_sum / max(1, self.days), self.gross_max,
            self.financing, self.trades))
        self.SetRuntimeStatistic("Episodes", " ".join("{}={}".format(n, e) for n, e in self.episodes.items()))
        self.SetRuntimeStatistic("Boosted", " ".join("{}:{}".format(k, d) for k, d in sorted(self.boosted.items())))
