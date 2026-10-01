# Research script, not production code: no ADR citations, no fidelity check
# against the Go engine. Run on QuantConnect Cloud for "Other candidates" in
# docs/research/boost-followup-2026-09.md (tuning years first). It cannot be
# imported outside QuantConnect (AlgorithmImports); research/edge-search/
# README.md explains the stand-in used to test it.
#
# Provenance: momentum/defensive.py (12-1 momentum, Jegadeesh & Titman 1993,
#   long-only book) with other published ways to rank the winners (SCORE):
#   - "high52": price over its 52-week high (George & Hwang 2004, "The 52-Week
#     High and Momentum Investing", Journal of Finance).
#   - "inter": the return from 12 to 7 months ago (Novy-Marx 2012, "Is
#     momentum really momentum?", Journal of Financial Economics).
#   - "fip": the top FIP_POOL names by 12-1 return, ranked by how gradually
#     they rose ("frog in the pan": Da, Gurun & Warachka 2014, Review of
#     Financial Studies), using their information-discreteness measure.
#   - "resid": 12-1 return after removing SPY's part (beta x SPY's return),
#     over its volatility, in the spirit of Blitz, Huij & Martens 2011,
#     "Residual momentum", Journal of Empirical Finance. ADAPTED: one market
#     factor and the same 12 months to estimate beta, not their three
#     factors and 36 months.
from AlgorithmImports import *
from datetime import date
import numpy as np


#: A volatility scale this far (relative) from the last one applied to the
#: whole book resizes every holding.
SCALE_MOVE = 0.10


def book_scale(prices, weights, target, periods=252):
    """min(1, target / annualised volatility) of the daily returns of a book
    holding ``weights`` (symbol -> fraction of equity) in the ``prices``
    series (symbol -> closes, oldest first, NaN before a stock's first
    close). A day where a stock has no return counts as 0 for it; 1 when
    the volatility cannot be measured."""
    length = max((len(p) for p in prices.values()), default=0)
    book = []
    for i in range(1, length):
        r = 0.0
        for s, p in prices.items():
            if i < len(p) and p[i] == p[i] and p[i - 1] == p[i - 1] and p[i - 1] > 0:
                r += weights[s] * (p[i] / p[i - 1] - 1.0)
        book.append(r)
    if len(book) < 2:
        return 1.0
    mean = sum(book) / len(book)
    vol = (sum((x - mean) ** 2 for x in book) / len(book)) ** 0.5 * periods ** 0.5
    return min(1.0, target / vol) if vol > 0 else 1.0


def info_discreteness(prices):
    """Da, Gurun & Warachka's ID = sign(return) x (% days down - % days up)
    over ``prices``; lower means the move came more gradually."""
    rets = [b / a - 1.0 for a, b in zip(prices, prices[1:]) if a > 0]
    if not rets or prices[0] <= 0:
        return None
    total = prices[-1] / prices[0] - 1.0
    up = sum(1 for r in rets if r > 0) / len(rets)
    down = sum(1 for r in rets if r < 0) / len(rets)
    return (1.0 if total > 0 else -1.0 if total < 0 else 0.0) * (down - up)


def residual_score(stock, market):
    """Market-adjusted 12-1 momentum: the daily residuals e = r - beta x r_m
    (beta from the same window), summed and divided by their standard
    deviation times sqrt(n). Two equal-length price lists, oldest first. With
    only one window, the intercept is left in the residual: fitted inside the
    window it would make the residuals sum to zero."""
    rs = [b / a - 1.0 for a, b in zip(stock, stock[1:])]
    rm = [b / a - 1.0 for a, b in zip(market, market[1:])]
    n = len(rs)
    if n < 20 or len(rm) != n:
        return None
    ms, mm = sum(rs) / n, sum(rm) / n
    var = sum((x - mm) ** 2 for x in rm)
    beta = sum((x - mm) * (y - ms) for x, y in zip(rm, rs)) / var if var > 0 else 0.0
    res = [y - beta * x for x, y in zip(rm, rs)]
    mean = sum(res) / n
    sd = (sum((e - mean) ** 2 for e in res) / n) ** 0.5
    return sum(res) / (sd * n ** 0.5) if sd > 1e-12 else None


def defensive_book(ranked, top_n, invested=0.98, sector=None, sector_cap=None, vol=None,
                   inv_vol=False, above=None, risk_off=False, risk_off_fraction=1.0,
                   spy_blend=0.0, bond="IEF", spy="SPY"):
    """Target weights for the long-only book. ``ranked`` is best first.

    The SPY_BLEND share goes to ``spy``; when ``risk_off`` the
    ``risk_off_fraction`` share of the rest goes to ``bond``. The momentum part
    is split over the first ``top_n`` ranked names allowed by ``sector_cap``
    (names with no sector are uncapped), equally or by inverse ``vol``. A
    pick not ``above`` its own average keeps its weight in ``bond`` instead."""
    targets = {}
    equity = invested * (1.0 - spy_blend)
    if spy_blend:
        targets[spy] = invested * spy_blend
    part = equity * (1.0 - risk_off_fraction) if risk_off else equity
    to_bond = equity - part
    picks, counts = [], {}
    for name in ranked:
        if len(picks) == top_n:
            break
        sec = sector.get(name) if sector else None
        if sector_cap and sec is not None and counts.get(sec, 0) >= sector_cap:
            continue
        picks.append(name)
        if sec is not None:
            counts[sec] = counts.get(sec, 0) + 1
    if picks and part > 0:
        raw = {n: (1.0 / vol[n] if inv_vol and vol and vol.get(n) else 1.0) for n in picks}
        total = sum(raw.values())
        for n in picks:
            w = part * raw[n] / total
            if above is not None and not above.get(n, False):
                to_bond += w
            else:
                targets[n] = targets.get(n, 0.0) + w
    elif part > 0:
        to_bond += part
    if to_bond > 1e-12:
        targets[bond] = targets.get(bond, 0.0) + to_bond
    return targets


class MomentumScoresResearch(QCAlgorithm):
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
    SLIPPAGE = 0.0                # fraction of price per fill; 0 = none (every reported 1999-2015 run)
    SECTOR_CAP = None             # at most this many names per Morningstar sector
    SCORE = "ret"                 # "ret", "ret_vol", "high52", "inter", "fip" or "resid"
    FIP_POOL = 100                # fip: rank only the top this many by 12-1 return
    INV_VOL = False               # weight names by inverse volatility
    STOCK_TREND_DAYS = 0          # 0 = off; else a name below its own N-day average goes to bonds
    RISK_OFF_FRACTION = 1.0       # mom_trend: share of the book moved to bonds when SPY is below its 200-day average
    SPY_BLEND = 0.0               # share of the account held in SPY instead of momentum
    VOL_TARGET = None             # e.g. 0.20: scale the book down to this annualised volatility
    VOL_DAYS = 126                # trailing sessions for that volatility
    REBALANCE_BAND = 0.25         # resize a held name once it is 25% off its target weight
    BORROW_FEE = 0.02             # annual fee on short market value (losers are often costly to borrow)

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
        self.sectors = {}
        self.last_month = None
        self.equity_curve = []
        self.spy_curve = []
        self.rebalances = 0
        self.fund_reads = 0
        self.fund_hits = 0
        self.risk_off_months = 0
        self.names_held = 0
        self.borrow_paid = 0.0
        self.resizes = 0
        self.scales = []
        self.applied_scale = 1.0
        self.resize_all = False
        self.skipped = 0
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
            sec = self._get(f, "AssetClassification.MorningstarSectorCode",
                            "asset_classification.morningstar_sector_code")
            self.sectors[f.Symbol] = int(sec) if sec is not None else None
            self.fundamentals[f.Symbol] = {
                "quality": gp / ta if gp is not None and ta and ta > 0 else None,
                "value": ey,
            }
            keep.append(f.Symbol)
        self.members = keep
        return keep

    def OnSecuritiesChanged(self, changes):
        # A fixed slippage (fraction of price per fill) on every stock that
        # enters the universe; 0 keeps LEAN's default of none.
        if self.SLIPPAGE:
            for sec in changes.AddedSecurities:
                sec.SetSlippageModel(ConstantSlippageModel(self.SLIPPAGE))

    def OnData(self, data):
        bar = data.Bars.get(self.spy)
        if bar is not None and self.BORROW_FEE:
            short_value = sum(-float(h.HoldingsValue) for h in self.Portfolio.Values if h.IsShort)
            fee = short_value * self.BORROW_FEE / 252.0
            if fee > 0:
                self.Portfolio.CashBook["USD"].AddAmount(-fee)
                self.borrow_paid += fee
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

        mom, vol, above = {}, {}, {}
        for sym in self.members:
            if sym not in closes.columns:
                continue
            px = closes[sym].dropna()
            if len(px) < 253:
                continue
            mom[sym] = px.iloc[-22] / px.iloc[-253] - 1.0
            rets = px.pct_change().dropna().iloc[-252:]
            vol[sym] = float(np.std(rets))
            if self.STOCK_TREND_DAYS:
                above[sym] = px.iloc[-1] > px.iloc[-self.STOCK_TREND_DAYS:].mean()

        scores = {}
        if self.MODE in ("mom", "mom_trend"):
            scores = mom
            if self.SCORE == "ret_vol":
                scores = {s: m / vol[s] for s, m in mom.items() if vol.get(s, 0) > 0}
            elif self.SCORE in ("high52", "inter", "fip", "resid"):
                scores = self._alt_scores(mom, closes)
        elif self.MODE == "lowvol":
            scores = {s: -v for s, v in vol.items() if v > 0}
        elif self.MODE in ("quality", "value"):
            scores = {s: self.fundamentals[s][self.MODE] for s in mom
                      if s in self.fundamentals and self.fundamentals[s][self.MODE] is not None}
        elif self.MODE in ("mom_ls", "mom_130"):
            scores = mom
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

        if self.MODE in ("mom", "mom_trend"):
            ranked = sorted(scores, key=lambda s: scores[s], reverse=True)
            targets = defensive_book(
                ranked, self.TOP_N, sector=self.sectors if self.SECTOR_CAP else None,
                sector_cap=self.SECTOR_CAP, vol=vol, inv_vol=self.INV_VOL,
                above=above if self.STOCK_TREND_DAYS else None, risk_off=risk_off,
                risk_off_fraction=self.RISK_OFF_FRACTION, spy_blend=self.SPY_BLEND,
                bond=self.bond, spy=self.spy)
            self.names_held += self.TOP_N
        elif risk_off:
            targets = {self.bond: 0.98}
        else:
            targets = self._targets(sorted(scores, key=lambda s: scores[s], reverse=True))
            if targets is None:
                self.skipped += 1
                return
            self.names_held += self.TOP_N
            if self.VOL_TARGET:
                targets = self._vol_scaled(targets, closes)
        self.rebalances += 1
        if risk_off:
            self.risk_off_months += 1
        self._trade(targets)

    def _targets(self, ranked):
        """Target weights from names ranked best first: the top TOP_N long,
        and in the long/short modes the bottom TOP_N short. None when the
        long/short modes have fewer than 2 x TOP_N names, since the two
        lists would overlap (last month's book is then kept)."""
        picks = ranked[:self.TOP_N]
        if self.MODE in ("mom_ls", "mom_130"):
            if len(ranked) < 2 * self.TOP_N:
                return None
            # mom_ls: market neutral, 49% long the winners and 49% short the
            # losers. mom_130: 127% long the winners, 29% short the losers.
            long_w, short_w = (0.49, 0.49) if self.MODE == "mom_ls" else (1.27, 0.29)
            targets = dict.fromkeys(picks, long_w / self.TOP_N)
            targets.update(dict.fromkeys(ranked[-self.TOP_N:], -short_w / self.TOP_N))
            return targets
        return dict.fromkeys(picks, 0.98 / len(picks))

    def _vol_scaled(self, targets, closes):
        """``targets`` scaled by min(1, VOL_TARGET / the book's trailing
        VOL_DAYS annualised volatility), measured on the current closes."""
        names = [s for s in targets if s in closes.columns]
        frame = closes[names].iloc[-(self.VOL_DAYS + 1):].ffill()
        prices = {s: [float(x) for x in frame[s]] for s in names}
        scale = book_scale(prices, targets, self.VOL_TARGET)
        self.scales.append(scale)
        # A scale that moved 10% or more from the one last applied to the whole
        # book resizes every continuing holding (_trade), so the guard caps the
        # book actually held rather than only the names the band would trade.
        self.resize_all = abs(scale - self.applied_scale) >= SCALE_MOVE * self.applied_scale
        if self.resize_all:
            self.applied_scale = scale
        return {s: w * scale for s, w in targets.items()}

    def _alt_scores(self, mom, closes):
        """The SCORE ranking for names that have a 12-1 return."""
        out = {}
        spy = [float(x) for x in closes[self.spy].ffill().iloc[-253:-21]]
        pool = set(sorted(mom, key=lambda s: mom[s], reverse=True)[:self.FIP_POOL])
        for sym in mom:
            px = [float(x) for x in closes[sym].dropna().iloc[-253:]]
            if len(px) < 253:
                continue
            if self.SCORE == "high52":
                out[sym] = px[-1] / max(px)
            elif self.SCORE == "inter":
                out[sym] = px[-148] / px[0] - 1.0
            elif self.SCORE == "fip":
                if sym in pool:
                    d = info_discreteness(px[:-21])
                    if d is not None:
                        out[sym] = -d
            else:
                r = residual_score(px[:-21], spy) if len(spy) == len(px[:-21]) else None
                if r is not None:
                    out[sym] = r
        return out

    def _trade(self, targets):
        """Names entering or leaving trade every month. A continuing holding
        is resized only once it has drifted more than REBALANCE_BAND (as a
        fraction of its target) from its target, so each side keeps its
        stated allocation within the band while the run stays inside the
        Free plan's 10,000-order cap."""
        for kvp in self.Portfolio:
            if kvp.Value.Invested and kvp.Key not in targets:
                self.Liquidate(kvp.Key)
        equity = float(self.Portfolio.TotalPortfolioValue)
        for sym, w in targets.items():
            h = self.Portfolio[sym]
            if not h.Invested or (h.IsLong and w < 0) or (h.IsShort and w > 0):
                self.SetHoldings(sym, w)
            elif self.resize_all or (self.REBALANCE_BAND is not None and equity > 0
                                     and abs(float(h.HoldingsValue) / equity - w) > self.REBALANCE_BAND * abs(w)):
                self.SetHoldings(sym, w)
                self.resizes += 1
        self.resize_all = False

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
        self.SetRuntimeStatistic("Borrow", "{:.0f} resizes={} skipped={}".format(
            self.borrow_paid, self.resizes, self.skipped))
        if self.scales:
            self.SetRuntimeStatistic("Scale", "min={:.2f} avg={:.2f} months<1={}".format(
                min(self.scales), sum(self.scales) / len(self.scales), sum(1 for x in self.scales if x < 1)))
        self.SetRuntimeStatistic("Fundamentals", "reads={} hits={}".format(self.fund_reads, self.fund_hits))
