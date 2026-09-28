"""Time-series momentum (TSMOM) on Pinnacle CLC futures: a local, monthly
backtester of the core strategy in Moskowitz, Ooi & Pedersen (2012), "Time
Series Momentum", Journal of Financial Economics 104(2), 228-250 ("MOP").

The strategy, from MOP Section 4 (eq. 5) and Section 2.4:

- **Signal.** At each month end, go long a market whose past 12-month excess
  return is positive, short one whose return is negative (MOP Section 4.1,
  the 12-month lookback, 1-month holding period). A futures return is
  already an excess return (MOP Section 2.2: the collateral earns the
  risk-free rate separately).
- **Volatility.** Each position is scaled to 40% annualised ex-ante
  volatility, sigma from an exponentially weighted variance of daily
  returns with a centre of mass of 60 days, annualised by 261 (MOP eq. 1).
- **Portfolio.** Markets are weighted equally, averaging over those with a
  signal (MOP eq. 5), which MOP report gives a portfolio volatility near
  12% (Section 4.2). An optional overlay scales the whole book to a 10%
  target by its own trailing realised volatility.

**Returns from back-adjusted prices.** Pinnacle's ``_REV`` series is
back-adjusted and can be zero or negative (US, ZH, ZB, the grains ...), so no
return is ever taken relative to a ``_REV`` level. The daily return is the
``_REV`` change divided by the previous day's ``_NON`` (real, non-adjusted)
settle; ``_REV`` and ``_NON`` share one calendar in every file used. The
12-month return compounds those daily returns. On a roll day the ``_REV``
change is the new contract's and the denominator is the old contract's
settle; the mismatch is one roll spread over one price, and ignored.

**Execution.** The decision is taken at the close of the last session of
each month (the union calendar of every market), from data up to and
including that day. Each market trades to its target on its own next
session, at that session's ``_REV`` settle. Positions are whole contracts,
rounded half away from zero. Costs: commission per contract per side; one
tick of slippage per contract per side; and one round trip (two of each)
per open contract on every roll, a roll being any day on which ``_REV``
minus ``_NON`` steps (Pinnacle's back-adjustment moves only on a roll;
markets.py read its roll months off the same steps).

**Accounting** mirrors backtest.py: the account settles daily variation
margin at the ``_REV`` settle, less costs, plus optional T-bill interest on
the whole balance (rates.py). Independently, each market keeps a cash-flow
ledger of its fills. Account trading P&L must equal the sum of the markets'
P&L less costs to within $0.01; interest is a separate line on both sides.

ADR 0012's held-out period (2016 on) is refused without ``allow_holdout``.

Standard-library Python only. Research-only; not the production Go engine.
"""

import argparse
import bisect
import math
import os
import sys
from collections import namedtuple
from dataclasses import dataclass, replace
from datetime import date, datetime, timedelta

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import backtest  # noqa: E402  (span_cagr, check_dates, HoldoutError, the defaults)
import loader  # noqa: E402
import rates  # noqa: E402
import rules  # noqa: E402  (research/qc-cloud-futures/rules.py, put on sys.path by backtest)
import tsmom_markets  # noqa: E402

DEFAULT_START = date(1985, 1, 1)
DEFAULT_END = backtest.DEFAULT_END
HoldoutError = backtest.HoldoutError

#: MOP eq. (1): the EWMA weights (1 - d) d^i with d / (1 - d) = 60 days.
CENTER_OF_MASS = 60
#: MOP eq. (1): daily variance x 261 trading days a year.
ANNUALISATION = 261
#: MOP eq. (5): each position is scaled to 40% annualised ex-ante volatility.
TARGET_VOL = 0.40
#: MOP Section 4: the 12-month lookback.
LOOKBACK_MONTHS = 12
#: The optional portfolio overlay's trailing window (one year of sessions)
#: and the fewest sessions it needs.
PORTFOLIO_VOL_WINDOW = 261
PORTFOLIO_VOL_MIN_OBS = 60
#: A market's last bar must be this recent for it to get a new signal.
STALE_DAYS = 7

RECONCILE_TOLERANCE = backtest.RECONCILE_TOLERANCE

Row = namedtuple("Row", ["date", "rev", "non"])
Fill = namedtuple("Fill", ["date", "symbol", "delta", "price", "kind"])
#: ``weights`` is the raw, pre-overlay MOP weight (sign x target_vol / sigma
#: / n) decided that day for each symbol with a signal -- the "unscaled
#: book" the portfolio-target overlay is measured against (README.md,
#: "Optional 10% portfolio target").
Decision = namedtuple("Decision", ["date", "equity", "n_markets", "scale", "targets", "weights"])
Reconcile = namedtuple("Reconcile", ["account_pnl", "position_pnl", "difference", "ok"])
#: A month-end decision not yet turned into a contract count: ``equity``
#: and ``price`` are frozen at decision time (MOP eq. 5's own timing), but
#: the multiplier is deliberately NOT -- it is looked up again at fill
#: time, from the fill date, not the decision date (a review finding: the
#: SP 1997 change falls between a decision and its fill more than once).
Pending = namedtuple("Pending", ["equity", "weight", "price"])


@dataclass(frozen=True)
class Config:
    start: date = DEFAULT_START
    end: date = DEFAULT_END
    allow_holdout: bool = False
    starting_equity: float = 1_000_000.0
    target_vol: float = TARGET_VOL
    #: ``None``: MOP's plain 40%-per-market book. A number (0.10): scale
    #: the whole book to that annualised volatility.
    portfolio_target: object = None
    commission: float = backtest.DEFAULT_COMMISSION
    slippage_ticks: float = 1.0
    charge_rolls: bool = True
    #: A ``rates.RateCurve``, or ``None`` for no interest.
    interest_rates: object = None


def check_dates(config):
    backtest.check_dates(config)


# -----------------------------------------------------------------------------
# The pieces.
# -----------------------------------------------------------------------------


def join_rows(rev_bars, non_bars, name="market"):
    """One ``Row`` per date from a ``_REV`` and a ``_NON`` series, which
    must share one calendar."""
    if [b.date for b in rev_bars] != [b.date for b in non_bars]:
        raise ValueError("{}: _REV and _NON dates differ".format(name))
    return [Row(r.date, r.close, n.close) for r, n in zip(rev_bars, non_bars)]


def daily_return(previous, current):
    """The ``_REV`` change over the previous ``_NON`` settle (module
    docstring). A non-positive real price is refused."""
    if not previous.non > 0:
        raise ValueError("non-adjusted settle {} on {} is not positive".format(previous.non, previous.date))
    return (current.rev - previous.rev) / previous.non


def _one_year_before(day):
    try:
        return day.replace(year=day.year - 1)
    except ValueError:  # 29 February
        return day.replace(year=day.year - 1, day=28)


def trailing_return(history, as_of, months=LOOKBACK_MONTHS):
    """The compounded return from the last mark on or before one year
    before ``as_of`` to the last mark on or before ``as_of``. ``history`` is
    a date-ordered list of ``(date, cumulative index)``. ``None`` when the
    history does not reach back a full year. Only marks dated on or before
    ``as_of`` are read."""
    assert months == 12, "only MOP's 12-month lookback is implemented"
    dates = [d for d, _ in history]
    now = bisect.bisect_right(dates, as_of) - 1
    then = bisect.bisect_right(dates, _one_year_before(as_of)) - 1
    if now < 0 or then < 0:
        return None
    return history[now][1] / history[then][1] - 1.0


def signal(twelve_month_return):
    """MOP Section 4: +1 long, -1 short, 0 flat on an exactly zero return."""
    if twelve_month_return > 0:
        return 1
    if twelve_month_return < 0:
        return -1
    return 0


class EwmaVariance:
    """MOP eq. (1): sigma^2 = 261 x sum_i (1 - d) d^i (r_{t-i} - rbar)^2,
    with rbar the same-weighted mean and d / (1 - d) the centre of mass.
    The weights are normalised by their sum, so a short history is not
    biased low; the recursive sums below equal the explicit weighted sums
    exactly."""

    def __init__(self, center_of_mass=CENTER_OF_MASS):
        self.delta = center_of_mass / (center_of_mass + 1.0)
        self.weight = 0.0
        self.first = 0.0
        self.second = 0.0
        self.count = 0

    def add(self, r):
        d = self.delta
        self.weight = d * self.weight + (1.0 - d)
        self.first = d * self.first + (1.0 - d) * r
        self.second = d * self.second + (1.0 - d) * r * r
        self.count += 1

    def variance(self):
        if self.count == 0:
            return None
        mean = self.first / self.weight
        return max(self.second / self.weight - mean * mean, 0.0)

    def annualised_vol(self):
        variance = self.variance()
        return None if variance is None else math.sqrt(ANNUALISATION * variance)


def target_weights(signals, target_vol=TARGET_VOL):
    """MOP eq. (5): ``signals`` maps symbol -> (sign, annualised sigma);
    each weight (notional / equity) is sign x target / sigma, averaged over
    the markets given."""
    n = len(signals)
    return {s: sign * target_vol / sigma / n for s, (sign, sigma) in signals.items()} if n else {}


def contracts(equity, weight, price, multiplier):
    """Whole contracts for ``weight`` x ``equity`` of notional at ``price``
    x ``multiplier`` a contract, rounded half away from zero."""
    raw = weight * equity / (price * multiplier)
    return int(math.copysign(math.floor(abs(raw) + 0.5), raw))


def portfolio_scale(book_returns, target, min_obs=PORTFOLIO_VOL_MIN_OBS):
    """``target`` over the annualised sample volatility of the unscaled
    book's daily returns over a trailing window, or ``None`` with too few
    observations or zero volatility."""
    n = len(book_returns)
    if n < min_obs:
        return None
    mean = sum(book_returns) / n
    sd = math.sqrt(sum((r - mean) ** 2 for r in book_returns) / (n - 1))
    if sd <= 0:
        return None
    return target / (sd * math.sqrt(ANNUALISATION))


def trade_cost(delta, commission, slippage_ticks, tick, multiplier):
    """(commission, slippage) in dollars for ``delta`` contracts, one side."""
    quantity = abs(delta)
    return quantity * commission, quantity * slippage_ticks * tick * multiplier


def roll_cost(quantity, commission, slippage_ticks, tick, multiplier):
    """One round trip for every open contract: two commissions and two
    slippages."""
    c, s = trade_cost(quantity, commission, slippage_ticks, tick, multiplier)
    return 2 * (c + s)


# -----------------------------------------------------------------------------
# The backtester.
# -----------------------------------------------------------------------------


class _Market:
    def __init__(self, symbol, market, rows):
        self.symbol = symbol
        self.market = market
        self.rows = rows
        self.by_date = {r.date: r for r in rows}
        self.last_date = rows[-1].date
        self.prev = None
        self.index = 1.0
        self.history = []           # (date, cumulative index)
        self.returns = {}           # date -> daily return
        self.ewma = EwmaVariance()
        self.pending = None         # a Pending, resolved to contracts at fill time
        self.alive = True
        # Position and its ledger.
        self.quantity = 0
        self.multiplier = None      # frozen for the position; see _fill
        self.flows = 0.0            # sum of -delta x price x multiplier
        self.last_price = None
        self.commission = 0.0
        self.slippage = 0.0
        self.roll_cost = 0.0

    def pnl(self):
        """Cash-flow P&L before costs: fills plus the open position at the
        last settle."""
        open_value = self.quantity * self.last_price * self.multiplier if self.quantity else 0.0
        return self.flows + open_value

    def net(self):
        return self.pnl() - self.commission - self.slippage - self.roll_cost


class Result:
    def __init__(self):
        self.equity_curve = []
        self.fills = []
        self.decisions = []
        self.total_commission = 0.0
        self.total_slippage = 0.0
        self.total_roll_cost = 0.0
        self.roll_count = 0
        self.contracts_traded = 0
        self.total_interest = 0.0
        self.pnl_by_market = {}
        self.gross_by_market = {}
        self.reconcile = None
        self.starting_equity = None
        self.final_equity = None
        self.markets = []

    def positions_on(self, day):
        """Contracts held at the close of ``day``, from the fills."""
        held = {s: 0 for s in self.markets}
        for fill in self.fills:
            if fill.date <= day:
                held[fill.symbol] += fill.delta
        return held


class Backtester:
    def __init__(self, config, universe, series):
        self.config = config
        self.states = {}
        for symbol, rows in series.items():
            kept = [r for r in rows if r.date <= config.end]
            if kept:
                self.states[symbol] = _Market(symbol, universe[symbol], kept)
        self.cash = float(config.starting_equity)
        self.result = Result()
        self.result.starting_equity = float(config.starting_equity)
        self.result.markets = sorted(self.states)
        self._last_interest_date = config.start
        #: The most recent decision's raw, pre-overlay weights, applied to
        #: each day's own realised return to build the REALISED unscaled
        #: book (README.md, "Optional 10% portfolio target") -- never
        #: today's brand-new weights against a year of past returns, which
        #: is not the book that was actually held.
        self._active_weights = {}
        self._book_history = []     # [(date, unscaled book return)]

    def run(self):
        check_dates(self.config)
        by_date = {}
        for symbol, state in self.states.items():
            for row in state.rows:
                by_date.setdefault(row.date, []).append(symbol)
        calendar = sorted(by_date)
        self.calendar = calendar
        for i, day in enumerate(calendar):
            trading = day >= self.config.start
            if trading:
                self._credit_interest(day)
            for symbol in sorted(by_date[day]):
                self._bar(self.states[symbol], day, trading)
            # Uses whatever weights were active BEFORE today's decision (if
            # any) -- appended before _decide below may replace them. Days
            # with no active book are not recorded: counting them as 0.0
            # would understate volatility and oversize the first scaled
            # positions (README.md, "Optional 10% portfolio target").
            if self._active_weights:
                self._book_history.append((day, self._unscaled_book_return(day)))
            if trading:
                self.result.equity_curve.append((day, self.cash))
            following = calendar[i + 1] if i + 1 < len(calendar) else None
            if following is not None and following.month != day.month and following >= self.config.start:
                self._decide(day)
        self._finish()
        return self.result

    def _unscaled_book_return(self, day):
        """Today's return of the currently active, unscaled MOP book: each
        symbol's last-decided raw weight times its OWN realised return
        today. Only ever uses weights already decided (no look-ahead)."""
        if not self._active_weights:
            return 0.0
        return sum(w * self.states[s].returns.get(day, 0.0)
                  for s, w in self._active_weights.items() if s in self.states)

    # --- one market's bar ---------------------------------------------------

    def _bar(self, state, day, trading):
        row = state.by_date[day]
        prev = state.prev
        if prev is not None:
            r = daily_return(prev, row)
            state.returns[day] = r
            state.ewma.add(r)
            state.index *= 1.0 + r
        state.history.append((day, state.index))
        if trading and state.alive:
            if state.quantity and prev is not None:
                # Daily variation margin on the account side, still at
                # whatever quantity and multiplier were held coming into
                # today's close.
                self.cash += state.quantity * (row.rev - prev.rev) * state.multiplier
                if self.config.charge_rolls and self._is_roll(state, prev, row):
                    self._charge_roll(state)
            if state.quantity:
                # A contract-size change (module docstring, "The SP
                # contract change"), applied at today's close -- resizes
                # through the same costed close-then-reopen trade as any
                # other multiplier change, so today's mark above still used
                # the OLD basis and the reconcile invariant holds exactly.
                self._resize_for_multiplier_change(state, row)
            state.last_price = row.rev
            if state.pending is not None:
                # Size at the FILL date's multiplier, not the decision
                # date's (a review finding: they can differ, e.g. across
                # the SP 1997 change).
                multiplier = tsmom_markets.dollars_per_point(state.market, row.date)
                target = contracts(state.pending.equity, state.pending.weight, state.pending.price, multiplier)
                self._fill(state, target, row, "rebalance")
                state.pending = None
            if day == state.last_date and day < self.calendar[-1]:
                # The file genuinely ends before the run's own last traded
                # day (the contract moved or was delisted): flatten. Compare
                # against the run's own last date, not ``config.end``
                # directly -- a weekend or holiday ``--end`` (never itself a
                # trading day) would otherwise make every still-live market
                # look delisted on its last real bar before that date.
                self._fill(state, 0, row, "delist")
                state.alive = False
        state.prev = row

    @staticmethod
    def _is_roll(state, prev, row):
        step = (row.rev - row.non) - (prev.rev - prev.non)
        return abs(step) > state.market.tick_size / 10.0

    def _charge_roll(self, state):
        cost = roll_cost(state.quantity, self.config.commission, self.config.slippage_ticks,
                         state.market.tick_size, state.multiplier)
        self.cash -= cost
        state.roll_cost += cost
        self.result.total_roll_cost += cost
        self.result.roll_count += 1

    def _resize_for_multiplier_change(self, state, row):
        """The exchange's own contract redefinition (the module docstring,
        "The SP contract change", 1997) changes the dollars-per-point
        multiplier on a fixed date, independent of any rebalance. Left
        alone until the next rebalance -- up to a month away -- mark to
        market would keep using the stale multiplier, and every
        per-contract cost (commission, slippage, a roll) would be charged
        on the stale, un-resized quantity (a review finding).

        Handled the same way ``_fill`` already handled it at a rebalance
        (below): close the old-size position and reopen the equivalent
        notional at the new size, both at today's close, through the same
        ``_trade`` the account side's daily mark already assumes for any
        quantity change. A "free" resize that only relabelled ``quantity``
        and ``multiplier`` without going through a trade would look
        reconciled in the common case, but whole-contract rounding of the
        new size cannot in general preserve ``quantity x multiplier``
        exactly, and that residual notional would otherwise silently leak
        out of the reconcile invariant every day afterwards. Routing it
        through ``_trade`` (paying commission and slippage once, like any
        other rebalance) keeps the reconcile exact by construction."""
        multiplier = tsmom_markets.dollars_per_point(state.market, row.date)
        if multiplier == state.multiplier:
            return
        old_quantity, old_multiplier = state.quantity, state.multiplier
        new_quantity = int(round(old_quantity * old_multiplier / multiplier))
        self._trade(state, -old_quantity, row, "resize")
        state.multiplier = multiplier
        self._trade(state, new_quantity, row, "resize")

    def _fill(self, state, target, row, kind):
        multiplier = tsmom_markets.dollars_per_point(state.market, row.date)
        if state.quantity and multiplier != state.multiplier:
            # A contract-size change (SP, 1997): close at the old size, then
            # reopen at the new one.
            self._trade(state, -state.quantity, row, "resize")
        if state.quantity == 0:
            state.multiplier = multiplier
        if target != state.quantity:
            self._trade(state, target - state.quantity, row, kind)

    def _trade(self, state, delta, row, kind):
        state.flows -= delta * row.rev * state.multiplier
        commission, slippage = trade_cost(delta, self.config.commission, self.config.slippage_ticks,
                                          state.market.tick_size, state.multiplier)
        self.cash -= commission + slippage
        state.commission += commission
        state.slippage += slippage
        self.result.total_commission += commission
        self.result.total_slippage += slippage
        self.result.contracts_traded += abs(delta)
        state.quantity += delta
        self.result.fills.append(Fill(row.date, state.symbol, delta, row.rev, kind))

    # --- the month-end decision ----------------------------------------------

    def _decide(self, day):
        signals = {}
        for symbol, state in self.states.items():
            if not state.alive or state.prev is None or state.last_date <= day:
                continue
            if (day - state.prev.date).days > STALE_DAYS:
                continue
            ret = trailing_return(state.history, day)
            sigma = state.ewma.annualised_vol()
            if ret is None or not sigma:
                continue
            sign = signal(ret)
            if sign:
                signals[symbol] = (sign, sigma)
        weights = target_weights(signals, self.config.target_vol)
        scale = 1.0
        if self.config.portfolio_target is not None:
            # The REALISED unscaled book's own trailing return history
            # (README.md, "Optional 10% portfolio target"): each past
            # day's return under whatever weights were actually active
            # that day, never today's brand-new ``weights`` applied
            # retroactively to a year of history that was never actually
            # held at these weights (a review finding).
            book = [r for _, r in self._book_history[-PORTFOLIO_VOL_WINDOW:]]
            scale = portfolio_scale(book, self.config.portfolio_target) if weights else None
        equity = self.cash
        targets = {}
        for symbol, state in self.states.items():
            if not state.alive or state.last_date <= day:
                continue
            weight = weights.get(symbol, 0.0)
            if weight == 0.0 or scale is None:
                # No signal (or no scale): flat, and -- like the signals
                # loop above -- never touch state.prev.non, since a market
                # that has not started trading yet (state.prev is None)
                # can only land here (it never gets a signal).
                targets[symbol] = 0
                state.pending = Pending(equity, 0.0, 1.0)
                continue
            effective = scale * weight
            # This multiplier is only for the informational Decision.targets
            # figure below; the actual fill is sized at the fill date's own
            # multiplier (state.pending, resolved in _bar).
            multiplier = tsmom_markets.dollars_per_point(state.market, day)
            targets[symbol] = contracts(equity, effective, state.prev.non, multiplier)
            state.pending = Pending(equity, effective, state.prev.non)
        self.result.decisions.append(Decision(day, equity, len(signals), scale, targets, weights))
        self._active_weights = weights

    # --- money -----------------------------------------------------------------

    def _credit_interest(self, day):
        curve = self.config.interest_rates
        if curve is None:
            return
        fraction = curve.accrued_fraction(self._last_interest_date, day)
        self._last_interest_date = day
        if fraction:
            interest = self.cash * fraction
            self.cash += interest
            self.result.total_interest += interest

    def _finish(self):
        result = self.result
        for symbol, state in self.states.items():
            if state.last_price is None:
                continue
            result.gross_by_market[symbol] = state.pnl()
            result.pnl_by_market[symbol] = state.net()
        result.final_equity = self.cash
        account = self.cash - result.starting_equity - result.total_interest
        positions = math.fsum(result.pnl_by_market.values())
        difference = account - positions
        result.reconcile = Reconcile(account, positions, difference, abs(difference) <= RECONCILE_TOLERANCE)


def run_backtest(config, universe, series):
    """Check the dates (``HoldoutError`` past 2015 without
    ``allow_holdout``), then run."""
    check_dates(config)
    return Backtester(config, universe, series).run()


# -----------------------------------------------------------------------------
# Metrics.
# -----------------------------------------------------------------------------

#: The report's fixed windows.
REPORT_PERIODS = (
    ("1985-1989", date(1985, 1, 1), date(1989, 12, 31)),
    ("1990-1999", date(1990, 1, 1), date(1999, 12, 31)),
    ("2000-2008", date(2000, 1, 1), date(2008, 12, 31)),
    ("2009-2015", date(2009, 1, 1), date(2015, 12, 31)),
    ("2003-2015", date(2003, 1, 1), date(2015, 12, 31)),
)


def month_end_marks(curve):
    """[(date, value)] at the last mark of each calendar month."""
    out = []
    for day, value in curve:
        if out and (out[-1][0].year, out[-1][0].month) == (day.year, day.month):
            out[-1] = (day, value)
        else:
            out.append((day, value))
    return out


def monthly_returns(curve, starting_value, start):
    """{(year, month): return}, each month's from the previous month-end
    mark (the first from ``starting_value``), with each month's end date."""
    out = {}
    previous_date, previous = start - timedelta(days=1), starting_value
    for day, value in month_end_marks(curve):
        out[(day.year, day.month)] = (previous_date, day, value / previous - 1.0)
        previous_date, previous = day, value
    return out


def _sd(values):
    n = len(values)
    if n < 2:
        return None
    mean = sum(values) / n
    return math.sqrt(sum((v - mean) ** 2 for v in values) / (n - 1))


def correlation(a, b):
    n = len(a)
    if n < 3:
        return None
    ma, mb = sum(a) / n, sum(b) / n
    cov = sum((x - ma) * (y - mb) for x, y in zip(a, b))
    va = sum((x - ma) ** 2 for x in a)
    vb = sum((y - mb) ** 2 for y in b)
    return cov / math.sqrt(va * vb) if va > 0 and vb > 0 else None


def metrics(config, result, benchmark=None):
    """The report's numbers. ``benchmark`` is {(year, month): return}.
    ``result.equity_curve`` is empty when no market has a bar inside the
    run's own span (README.md): every number is then ``None`` rather than
    an ``IndexError`` on the first mark."""
    curve = result.equity_curve
    out = {}
    if not curve:
        out.update(cagr=None, max_drawdown=None, ratio=None, vol=None, sharpe=None,
                    periods=[(label, None) for label, _, _ in REPORT_PERIODS],
                    year_2008=None, months={}, avg_markets=None, scale_range=None)
        return out
    first, last = curve[0][0], curve[-1][0]
    out["cagr"] = backtest.span_cagr(curve, result.starting_equity, first, last)
    out["max_drawdown"] = rules.max_drawdown([result.starting_equity] + [e for _, e in curve])
    out["ratio"] = rules.cagr_over_max_drawdown(out["cagr"], out["max_drawdown"])
    months = monthly_returns(curve, result.starting_equity, config.start)
    returns = [r for _, _, r in months.values()]
    sd = _sd(returns)
    out["vol"] = sd * math.sqrt(12) if sd else None
    curve_rates = config.interest_rates
    excess = [r - (curve_rates.accrued_fraction(a, b) if curve_rates else 0.0) for a, b, r in months.values()]
    sd_excess = _sd(excess)
    out["sharpe"] = (sum(excess) / len(excess)) / sd_excess * math.sqrt(12) if sd_excess else None
    out["periods"] = [(label, backtest.span_cagr(curve, result.starting_equity, a, b))
                      for label, a, b in REPORT_PERIODS]
    marks = dict(((d.year), e) for d, e in curve)
    out["year_2008"] = marks[2008] / marks[2007] - 1.0 if 2008 in marks and 2007 in marks else None
    out["months"] = months
    if benchmark:
        keys = [k for k in months if k in benchmark]
        out["sp_correlation"] = correlation([months[k][2] for k in keys], [benchmark[k] for k in keys])
        worst = sorted(keys, key=lambda k: benchmark[k])[:10]
        out["sp_worst"] = [(k, benchmark[k], months[k][2]) for k in worst]
    out["avg_markets"] = (sum(d.n_markets for d in result.decisions) / len(result.decisions)
                          if result.decisions else None)
    scales = [d.scale for d in result.decisions if d.scale is not None]
    out["scale_range"] = (min(scales), sum(scales) / len(scales), max(scales)) if scales else None
    return out


def benchmark_monthly(bars, start, end):
    """{(year, month): price-only return} from a non-adjusted series."""
    inside = [(b.date, b.close) for b in bars if b.date <= end]
    marks = month_end_marks(inside)
    out = {}
    for (d0, v0), (d1, v1) in zip(marks, marks[1:]):
        if d1 >= start:
            out[(d1.year, d1.month)] = v1 / v0 - 1.0
    return out


def _pct(value):
    return "n/a" if value is None else "{:+.2f}%".format(100 * value)


def _num(value, places=2):
    return "n/a" if value is None else "{:.{}f}".format(value, places)


def label(config):
    sizing = "40%/market" if config.portfolio_target is None else "{:.0f}% portfolio target".format(
        100 * config.portfolio_target)
    interest = "with T-bill interest" if config.interest_rates is not None else "no interest"
    return "${:,.0f}, {}, {}".format(config.starting_equity, sizing, interest)


def format_report(config, result, benchmark=None, universe=None):
    m = metrics(config, result, benchmark)
    curve = result.equity_curve
    lines = ["TSMOM (MOP 2012) -- {}".format(label(config))]
    if not curve:
        # No market had a bar inside start..end (README.md): nothing to
        # mark to market, but the reconcile (trivially 0 == 0) still holds.
        lines.append("No bars in {}..{}: nothing to report.".format(config.start, config.end))
        r = result.reconcile
        lines.append("Reconcile {}: account ${:,.2f}  positions ${:,.2f}  difference ${:,.4f}".format(
            "OK" if r.ok else "FAILED", r.account_pnl, r.position_pnl, r.difference))
        return "\n".join(lines), m
    lines.append("Span {}..{}  markets {}  final ${:,.0f}".format(curve[0][0], curve[-1][0], len(result.markets),
                                                                  result.final_equity))
    lines.append("CAGR {}  vol {}  Sharpe {}  MaxDD {}  CAGR/MaxDD {}  2008 {}".format(
        _pct(m["cagr"]), _pct(m["vol"]), _num(m["sharpe"]), _pct(m["max_drawdown"]), _num(m["ratio"], 3),
        _pct(m["year_2008"])))
    lines.append("Per-period CAGR: " + "  ".join("{} {}".format(k, _pct(v)) for k, v in m["periods"]))
    lines.append("Markets with a signal per month (avg) {}  portfolio scale min/avg/max {}".format(
        _num(m["avg_markets"], 1),
        "n/a" if m["scale_range"] is None else "/".join(_num(v) for v in m["scale_range"])))
    if "sp_correlation" in m:
        lines.append("Correlation of monthly returns with the S&P (price only) {}".format(_num(m["sp_correlation"])))
        worst = m["sp_worst"]
        lines.append("S&P's worst 10 months (S&P / TSMOM): " + ", ".join(
            "{}-{:02d} {} / {}".format(k[0], k[1], _pct(s), _pct(t)) for k, s, t in worst))
        lines.append("  average: S&P {}  TSMOM {}".format(_pct(sum(s for _, s, _ in worst) / len(worst)),
                                                          _pct(sum(t for _, _, t in worst) / len(worst))))
    ranked = sorted(result.pnl_by_market.items(), key=lambda item: -item[1])
    total = sum(result.pnl_by_market.values())
    lines.append("Top 5 markets: " + ", ".join("{} ${:,.0f}".format(s, p) for s, p in ranked[:5]))
    lines.append("Bottom 5 markets: " + ", ".join("{} ${:,.0f}".format(s, p) for s, p in ranked[-5:][::-1]))
    positive = sum(p for _, p in ranked if p > 0)
    if positive > 0:
        lines.append("Largest market's share of all positive market P&L: {} {:.1%}  (net total ${:,.0f})".format(
            ranked[0][0], ranked[0][1] / positive, total))
    if universe:
        sectors = {}
        for symbol, pnl in result.pnl_by_market.items():
            sector = universe[symbol].sector
            sectors[sector] = sectors.get(sector, 0.0) + pnl
        lines.append("P&L by sector: " + ", ".join("{} ${:,.0f}".format(k, v) for k, v in sorted(sectors.items())))
    lines.append("Costs: commission ${:,.0f}  slippage ${:,.0f}  roll ${:,.0f} ({} rolls)  contracts traded {:,}".format(
        result.total_commission, result.total_slippage, result.total_roll_cost, result.roll_count,
        result.contracts_traded))
    lines.append("Interest ${:,.0f}".format(result.total_interest))
    r = result.reconcile
    lines.append("Reconcile {}: account ${:,.2f}  positions ${:,.2f}  difference ${:,.4f}".format(
        "OK" if r.ok else "FAILED", r.account_pnl, r.position_pnl, r.difference))
    return "\n".join(lines), m


# -----------------------------------------------------------------------------
# The command line.
# -----------------------------------------------------------------------------


def _date_arg(text):
    return datetime.strptime(text, "%Y-%m-%d").date()


def _parser():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--data-dir", default=backtest.DEFAULT_DATA_DIR)
    parser.add_argument("--start", type=_date_arg, default=DEFAULT_START, help="YYYY-MM-DD")
    parser.add_argument("--end", type=_date_arg, default=DEFAULT_END, help="YYYY-MM-DD")
    parser.add_argument("--allow-holdout", action="store_true",
                        help="permit an end date in the held-out period (2016 on)")
    parser.add_argument("--markets", default=None, help="comma-separated Pinnacle symbols (default: all)")
    parser.add_argument("--equity", default="1000000,10000000", help="comma-separated starting equities")
    parser.add_argument("--portfolio-target", default="none,0.10",
                        help="comma-separated: 'none' (40%% per market) and/or an annual vol target")
    parser.add_argument("--commission", type=float, default=backtest.DEFAULT_COMMISSION)
    parser.add_argument("--slippage-ticks", type=float, default=1.0)
    parser.add_argument("--no-roll-costs", action="store_true")
    parser.add_argument("--interest-rates", default=None, help="a FRED TB3MS/DTB3 CSV; runs with and without")
    parser.add_argument("--benchmark", default="SP", help="a symbol whose _NON close is the equity benchmark")
    return parser


def main(argv=None, universe=None):
    args = _parser().parse_args(argv)
    universe = universe or tsmom_markets.TSMOM_MARKETS
    base = Config(start=args.start, end=args.end, allow_holdout=args.allow_holdout,
                  commission=args.commission, slippage_ticks=args.slippage_ticks,
                  charge_rolls=not args.no_roll_costs)
    try:
        check_dates(base)
    except ValueError as err:
        print("tsmom: {}".format(err), file=sys.stderr)
        return 2
    curve = None
    if args.interest_rates:
        try:
            curve = rates.load_rate_curve(args.interest_rates)
        except (OSError, ValueError) as err:
            print("tsmom: --interest-rates {}: {}".format(args.interest_rates, err), file=sys.stderr)
            return 1
    if not os.path.isdir(args.data_dir):
        print("tsmom: no data directory {}".format(args.data_dir), file=sys.stderr)
        return 1
    symbols = [s.strip() for s in args.markets.split(",")] if args.markets else sorted(universe)
    series = {}
    for symbol in symbols:
        if symbol not in universe:
            print("tsmom: unknown or excluded market {} ({})".format(
                symbol, tsmom_markets.EXCLUDED.get(symbol, "not in tsmom_markets.py")), file=sys.stderr)
            return 1
        paths = [loader.find_market_file(args.data_dir, "{}_{}".format(symbol, kind)) for kind in ("REV", "NON")]
        if None in paths:
            print("tsmom: {}: missing _REV or _NON file; skipped".format(symbol), file=sys.stderr)
            continue
        # Clipped at the end: the held-out period is never read.
        rev, non = (loader.load_series(p, end=args.end) for p in paths)
        series[symbol] = join_rows(rev, non, symbol)
    if not series:
        print("tsmom: no market files found", file=sys.stderr)
        return 1
    benchmark = None
    if args.benchmark:
        path = loader.find_market_file(args.data_dir, "{}_NON".format(args.benchmark))
        if path is not None:
            benchmark = benchmark_monthly(loader.load_series(path, end=args.end), args.start, args.end)
    equities = [float(e) for e in args.equity.split(",")]
    targets = [None if t.strip().lower() == "none" else float(t) for t in args.portfolio_target.split(",")]
    ok = True
    summary = []
    for equity in equities:
        for target in targets:
            for rate_curve in ([None, curve] if curve is not None else [None]):
                config = replace(base, starting_equity=equity, portfolio_target=target, interest_rates=rate_curve)
                result = run_backtest(config, universe, series)
                ok = ok and result.reconcile.ok
                text, m = format_report(config, result, benchmark, universe)
                print(text)
                print()
                summary.append((label(config), m))
    print("Summary")
    print("{:<52} {:>8} {:>8} {:>7} {:>8} {:>7} {:>6}".format("run", "CAGR", "vol", "Sharpe", "MaxDD",
                                                            "CAGR/DD", "SPcorr"))
    for name, m in summary:
        print("{:<52} {:>8} {:>8} {:>7} {:>8} {:>7} {:>6}".format(
            name, _pct(m["cagr"]), _pct(m["vol"]), _num(m["sharpe"]), _pct(m["max_drawdown"]),
            _num(m["ratio"], 3), _num(m.get("sp_correlation"))))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
