"""A local, pure-Python daily backtester for Faith's original Turtle rules
(System 2, 55/20, 1% Unit, 2N stop, 1/2N Adds, long AND short) on
back-adjusted continuous futures from Pinnacle Data's CLC database.

research/qc-cloud-futures/rules.py is the single rule core: it is imported
from there, never copied. research/qc-cloud-futures/main.py (the
QuantConnect version) is the spec for everything this file adds around it:
the Session order, entries, Adds, exits, Unit caps, the Notional Account,
Strength ranking and slippage. README.md lists every place this file
differs from main.py, and why.

Why a local engine: QuantConnect's free futures data covers only ES before
2007, and roll fills on sparse contracts distorted its P&L
(research/qc-cloud-futures/README.md, "Cloud result"). Here signals, fills
and P&L all come from one back-adjusted series per market. Back-adjusting
removes the roll price jumps, so a roll moves no price. Its cost is
charged separately (``_charge_rolls``).

Back-adjusted levels can be zero or negative. Nothing here ever divides by
a price level or computes a percentage from one: the Turtle rules use only
price differences and N, and P&L is points x multiplier.

Standard-library Python only. Research-only; not the production Go engine.
"""

import argparse
import math
import os
import sys
from collections import namedtuple
from dataclasses import dataclass
from datetime import date, datetime

_HERE = os.path.dirname(os.path.abspath(__file__))
_RULE_CORE = os.path.normpath(os.path.join(_HERE, os.pardir, "qc-cloud-futures"))
if _RULE_CORE not in sys.path:
    sys.path.insert(0, _RULE_CORE)
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import rules  # noqa: E402  (research/qc-cloud-futures/rules.py)
import loader  # noqa: E402
import markets as market_table  # noqa: E402

DEFAULT_START = date(1980, 1, 1)
DEFAULT_END = date(2015, 12, 31)
#: ADR 0012: 2016 onward is the held-out period. A run reaching into it
#: must say so explicitly (``allow_holdout`` / ``--allow-holdout``).
HOLDOUT_START = date(2016, 1, 1)
#: Dollars per contract per side: a round-number stand-in for a discount
#: futures broker's all-in commission and exchange fees (ADR 0013 names
#: the Interactive Brokers model, which LEAN supplied in the cloud version).
DEFAULT_COMMISSION = 2.50
DEFAULT_DATA_DIR = os.path.expanduser("~/Desktop/Trend_Investing/data/pinnacle")


class HoldoutError(ValueError):
    """A run that reaches the held-out period without ``allow_holdout``."""


@dataclass(frozen=True)
class Config:
    start: date = DEFAULT_START
    end: date = DEFAULT_END
    allow_holdout: bool = False
    starting_equity: float = 1_000_000.0
    #: Faith's own 1% [T p.14]; rules.py explains why this is not ADR 0003's
    #: equity halving.
    unit_volatility_fraction: float = rules.UNIT_VOLATILITY_FRACTION
    commission_per_contract: float = DEFAULT_COMMISSION
    #: ADR 0013: 0.05N per fill, against the trader, never zero.
    slippage_n: float = rules.SLIPPAGE_N
    #: main.py's LONG_ONLY comparison switch.
    long_only: bool = False
    charge_rolls: bool = True


def check_dates(config):
    """Refuse an empty span, and any span reaching 2016 or later unless
    ``allow_holdout`` is set (ADR 0012)."""
    if config.start > config.end:
        raise ValueError("start {} is after end {}".format(config.start, config.end))
    if config.end >= HOLDOUT_START and not config.allow_holdout:
        raise HoldoutError(
            "end {} reaches the held-out period (from {}); refusing without --allow-holdout".format(
                config.end, HOLDOUT_START))


# -----------------------------------------------------------------------------
# Fills.
# -----------------------------------------------------------------------------


def stop_market_fill_price(side, level, open_, high, low, slippage_amount):
    """An Exit Order's fill (ADR 0005, as amended). ``side`` is the ORDER's
    side: -1 sells (closing a long), +1 buys (closing a short). A sell stop
    triggers when the bar's low touches ``level`` and fills at
    min(level, open) - slippage, so a bar that gaps through the stop fills
    at its open, never at the stop. A buy stop is the mirror image. The rule
    core has no stop-market function (in main.py, LEAN filled Exit Orders
    natively). This one triggers on a touch, as ``rules.stop_limit_fill_price``
    does; LEAN needed a strict cross. Returns ``None`` for no fill."""
    if side < 0:
        if low > level:
            return None
        return min(level, open_) - slippage_amount
    if high < level:
        return None
    return max(level, open_) + slippage_amount


Fill = namedtuple("Fill", ["date", "symbol", "kind", "direction", "quantity", "price"])
ClosedCampaign = namedtuple("ClosedCampaign", [
    "symbol", "direction", "entry_date", "exit_date", "unit_quantity", "max_units", "r_multiple",
    "gross_pnl", "commission", "roll_cost", "net_pnl"])
OpenCampaign = namedtuple("OpenCampaign", [
    "symbol", "direction", "entry_date", "unit_quantity", "units", "gross_pnl", "commission", "roll_cost",
    "net_pnl"])
Reconcile = namedtuple("Reconcile", ["account_pnl", "campaign_pnl", "difference", "ok"])

#: Reconciliation tolerance, in dollars: float rounding only. The two sides
#: are computed independently (daily variation margin on the account side,
#: fill-to-exit distance on the Campaign side).
RECONCILE_TOLERANCE = 0.01


class _Order:
    """A resting entry or Add stop-limit order (ADR 0005, as amended)."""

    __slots__ = ("kind", "direction", "level", "cap", "n", "quantity", "multiplier", "placed_on")

    def __init__(self, kind, direction, level, n, quantity, multiplier, placed_on):
        self.kind = kind
        self.direction = direction
        self.level = level
        self.cap = rules.price_cap(level, n, direction)
        self.n = n
        self.quantity = quantity
        self.multiplier = multiplier
        self.placed_on = placed_on


class _Position:
    """Bookkeeping beside one ``rules.Campaign``: costs, dollars and dates.
    The dollars-per-point multiplier is frozen at entry, like N and the
    Unit size (ADR 0006)."""

    def __init__(self, campaign, multiplier, entry_date):
        self.campaign = campaign
        self.multiplier = multiplier
        self.entry_date = entry_date
        self.max_units = 1
        self.gross = 0.0
        self.commission = 0.0
        self.roll_cost = 0.0

    def unrealized(self):
        c = self.campaign
        return sum(c.direction * (unit["mark"] - unit["fill_price"]) for unit in c.units) \
            * c.unit_quantity * self.multiplier


class _MarketState:
    def __init__(self, symbol, market, bars):
        self.symbol = symbol
        self.market = market
        self.bars = bars
        self.n = rules.WilderN()
        self.entry_channel = rules.Channel(rules.ENTRY_CHANNEL_LENGTH)
        self.exit_channel = rules.Channel(rules.EXIT_CHANNEL_LENGTH)
        self.closes = []
        self.previous_close = None
        self.last_bar = None
        self.entry_orders = {1: None, -1: None}
        self.add_order = None
        self.exit_proposal = None
        self.position = None
        self.last_roll = None

    def advance(self, bar):
        """Fold a completed bar into N, both channels and Strength's closes
        (evaluate-then-add: callers read them for this bar first)."""
        self.n.add(rules.true_range(bar.high, bar.low, self.previous_close))
        self.entry_channel.add(bar.high, bar.low)
        self.exit_channel.add(bar.high, bar.low)
        self.closes.append(bar.close)
        if len(self.closes) > rules.STRENGTH_LOOKBACK_BARS + 1:
            del self.closes[0]
        self.previous_close = bar.close
        self.last_bar = bar


class Result:
    def __init__(self):
        self.equity_curve = []
        self.fills = []
        self.campaigns = []
        self.open_campaigns = []
        self.total_commission = 0.0
        self.total_roll_cost = 0.0
        self.roll_count = 0
        self.pnl_by_market = {}
        self.declines = {}
        self.reconcile = None
        self.final_equity = None
        self.starting_equity = None
        self.ruined_on = None
        self.markets = []


# -----------------------------------------------------------------------------
# The backtester.
# -----------------------------------------------------------------------------


class Backtester:
    """One run over ``series`` (symbol -> list of ``loader.Bar``) for the
    ``universe`` (symbol -> ``markets.Market``)."""

    def __init__(self, config, universe, series):
        self.config = config
        self.states = {}
        for symbol, bars in series.items():
            kept = [bar for bar in bars if bar.date <= config.end]
            if kept:
                self.states[symbol] = _MarketState(symbol, universe[symbol], kept)
        self.caps = rules.UnitCaps()
        self.notional = rules.NotionalAccount(config.starting_equity)
        self.cash = float(config.starting_equity)
        self.result = Result()
        self.result.starting_equity = float(config.starting_equity)
        self.result.markets = sorted(self.states)

    # --- the Session loop --------------------------------------------------

    def run(self):
        check_dates(self.config)
        by_date = {}
        for symbol, state in self.states.items():
            for bar in state.bars:
                by_date.setdefault(bar.date, []).append((symbol, bar))
        for session_date in sorted(by_date):
            today = sorted(by_date[session_date])
            if session_date < self.config.start:
                for symbol, bar in today:
                    self.states[symbol].advance(bar)  # warm-up: indicators only
                continue
            if not self._session(session_date, today):
                break
        self._finish()
        return self.result

    def _session(self, session_date, today):
        """One Session, in main.py's order: resting orders fill against
        today's bars (exits, then Adds, then entries, per market); then, at
        the close, exits are proposed, Adds decided in ascending symbol
        order, and entries decided ranked by Strength (The Turtle Rules
        p.27-29; ADR 0010, ADR 0021). Returns False once equity is gone."""
        for symbol, bar in today:
            self._fill_phase(symbol, self.states[symbol], bar, session_date)
        self._decide(session_date, today)
        for symbol, bar in today:
            self._mark(self.states[symbol], bar)
            self._charge_rolls(symbol, self.states[symbol], session_date)
        equity = self.cash
        self.result.equity_curve.append((session_date, equity))
        if not (math.isfinite(equity) and equity > 0):
            self.result.ruined_on = session_date
            return False
        self.notional.observe(session_date, equity)
        return True

    # --- fills -------------------------------------------------------------

    def _fill_phase(self, symbol, state, bar, session_date):
        position = state.position
        if position is not None:
            self._fill_exits(symbol, state, bar, session_date)
        if state.add_order is not None:
            self._fill_add(symbol, state, bar, session_date)
        if state.position is None and any(state.entry_orders.values()):
            self._fill_entry(symbol, state, bar, session_date)

    def _fill_exits(self, symbol, state, bar, session_date):
        """Every Unit rests its own Exit Order at the more protective of its
        own Protective Stop and, while one is proposed, the Exit Channel
        level (``rules.exit_order_level``; ADR 0005, as amended). Each
        triggered Unit fills at its own price."""
        campaign = state.position.campaign
        slip = rules.slippage(campaign.campaign_n, self.config.slippage_n)
        exits = []
        for index, unit in enumerate(campaign.units):
            level = rules.exit_order_level(campaign.direction, unit["stop"], state.exit_proposal)
            price = stop_market_fill_price(-campaign.direction, level, bar.open, bar.high, bar.low, slip)
            if price is not None:
                exits.append((index, price))
        if not exits:
            return
        # main.py: a resting Add never survives any exit fill.
        self._cancel(state, "add")
        # In Unit order; each close shifts the later Units down by one.
        for closed, (index, price) in enumerate(exits):
            self._close_unit(symbol, state, index - closed, price, session_date)
        if not campaign.units:
            self._finish_campaign(symbol, state, session_date)

    def _fill_add(self, symbol, state, bar, session_date):
        order = state.add_order
        position = state.position
        if position is None or position.campaign.loaded or position.campaign.partially_stopped:
            self._cancel(state, "add")
            return
        price = rules.stop_limit_fill_price(order.direction, order.level, order.cap, bar.open, bar.high,
                                            bar.low, rules.slippage(order.n, self.config.slippage_n))
        if price is None:
            return
        state.add_order = None  # its Unit-cap reservation is now the Unit's own
        campaign = position.campaign
        campaign.add_unit(price)
        campaign.units[-1]["mark"] = price
        position.max_units = max(position.max_units, campaign.unit_count)
        self._commission(position, order.quantity)
        self._record(session_date, symbol, "add", order.direction, order.quantity, price)
        self._chain_add(symbol, state, session_date)

    def _fill_entry(self, symbol, state, bar, session_date):
        """At most one entry opens a Campaign. If one bar fills both the long
        and the short order, the one whose level is nearer the open is taken
        as having triggered first, and the other is cancelled (main.py
        instead filled both and liquidated the second)."""
        filled = []
        for direction in (1, -1):
            order = state.entry_orders[direction]
            if order is None:
                continue
            price = rules.stop_limit_fill_price(direction, order.level, order.cap, bar.open, bar.high, bar.low,
                                                rules.slippage(order.n, self.config.slippage_n))
            if price is not None:
                filled.append((abs(bar.open - order.level), -direction, order, price))
        if not filled:
            return
        _, _, order, price = min(filled, key=lambda item: item[:2])
        state.entry_orders[order.direction] = None  # reservation is now the Unit's own
        self._cancel(state, -order.direction)
        market = state.market
        campaign = rules.Campaign(symbol, order.direction, price, order.n, order.quantity,
                                  market.closely_group, market.loosely_group)
        campaign.units[0]["mark"] = price
        state.position = _Position(campaign, order.multiplier, session_date)
        self._commission(state.position, order.quantity)
        self._record(session_date, symbol, "entry", order.direction, order.quantity, price)
        self._chain_add(symbol, state, session_date)

    def _chain_add(self, symbol, state, session_date):
        """main.py's ``_chain_add`` (ADR 0011's amendment): after an entry or
        Add fills, the next rung is measured from that fill and checked
        against the last completed bar. If that bar already reached it, the
        Add is proposed now; it can first fill on the market's next bar, and
        survives this Session's expiry."""
        campaign = state.position.campaign
        rung = campaign.next_add_rung()
        previous = state.last_bar
        if rung is None or state.add_order is not None or previous is None:
            return
        reached = previous.high >= rung if campaign.direction > 0 else previous.low <= rung
        if reached:
            self._place_add(symbol, state, rung, session_date, rules.SessionCapLedger(self.caps))

    def _close_unit(self, symbol, state, index, price, session_date):
        position = state.position
        campaign = position.campaign
        unit = campaign.units[index]
        quantity = campaign.unit_quantity
        # Account side: the last day's variation margin, from the Unit's
        # last mark to its exit.
        self.cash += campaign.direction * (price - unit["mark"]) * quantity * position.multiplier
        # Campaign side: the rule core's own realised price distance.
        before = campaign.realized_price_pnl
        campaign.close_units([index], price)
        position.gross += (campaign.realized_price_pnl - before) * quantity * position.multiplier
        self.caps.remove(symbol, campaign.closely_group, campaign.loosely_group, campaign.direction, units=1)
        self._commission(position, quantity)
        self._record(session_date, symbol, "exit", campaign.direction, quantity, price)

    def _finish_campaign(self, symbol, state, session_date):
        position = state.position
        campaign = position.campaign
        net = position.gross - position.commission - position.roll_cost
        self.result.campaigns.append(ClosedCampaign(
            symbol, campaign.direction, position.entry_date, session_date, campaign.unit_quantity,
            position.max_units, campaign.r_multiple(), position.gross, position.commission,
            position.roll_cost, net))
        self.result.pnl_by_market[symbol] = self.result.pnl_by_market.get(symbol, 0.0) + net
        state.position = None
        state.exit_proposal = None

    # --- decisions at the Session's close ------------------------------------

    def _decide(self, session_date, today):
        """main.py's ``_session``, after the fills."""
        # A breakout proposal lasts only until its market's next bar; an
        # ordinary Add proposal too, but a fill-chained Add placed today
        # survives (ADR 0011, as amended).
        for symbol, _ in today:
            state = self.states[symbol]
            self._cancel(state, 1)
            self._cancel(state, -1)
            if state.add_order is not None and state.add_order.placed_on < session_date:
                self._cancel(state, "add")

        add_opportunities = []
        candidates = {1: [], -1: []}
        for symbol, bar in today:
            state = self.states[symbol]
            entry_high, entry_high_ready = state.entry_channel.extreme(1)
            entry_low, entry_low_ready = state.entry_channel.extreme(-1)
            exit_high, exit_high_ready = state.exit_channel.extreme(1)
            exit_low, exit_low_ready = state.exit_channel.extreme(-1)
            n = state.n.value if state.n.ready and state.n.value > 0 else None
            may_enter = (n is not None and state.position is None
                         and market_table.tradable(state.market, session_date))
            long_breakout = may_enter and entry_high_ready and rules.is_breakout(1, bar.high, bar.low, entry_high)
            short_breakout = (may_enter and not self.config.long_only and entry_low_ready
                              and rules.is_breakout(-1, bar.high, bar.low, entry_low))

            if state.position is not None:
                campaign = state.position.campaign
                direction = campaign.direction
                extreme, ready = (exit_low, exit_low_ready) if direction > 0 else (exit_high, exit_high_ready)
                breach = rules.exit_channel_breach(direction, bar.high, bar.low, extreme, ready)
                state.exit_proposal = breach
                rung = campaign.next_add_rung()
                reached = rung is not None and (bar.high >= rung if direction > 0 else bar.low <= rung)
                # No Add in a Session that proposes an exit (main.py).
                if reached and breach is None and state.add_order is None:
                    add_opportunities.append((symbol, state, rung))
            else:
                state.exit_proposal = None

            state.advance(bar)

            if long_breakout or short_breakout:
                # Strength from this Session's completed close and N (ADR
                # 0010, as amended); sizing uses the pre-bar N (ADR 0005).
                value, ready = rules.strength(state.closes, state.n.value)
                if not ready:
                    self._decline("entry: strength not ready")
                    continue
                if long_breakout:
                    candidates[1].append((symbol, state, entry_high, n, value))
                if short_breakout:
                    candidates[-1].append((symbol, state, entry_low, n, value))

        ledger = rules.SessionCapLedger(self.caps)
        for symbol, state, rung in add_opportunities:
            self._place_add(symbol, state, rung, session_date, ledger)
        # Every long candidate first, strongest first; then every short,
        # weakest first (The Turtle Rules p.27-29; main.py's own ordering).
        for direction in (1, -1):
            by_symbol = {entry[0]: entry for entry in candidates[direction]}
            signals = [rules.Signal(symbol, entry[4]) for symbol, entry in by_symbol.items()]
            for signal in rules.rank_signals(signals, direction):
                symbol, state, level, n, _ = by_symbol[signal.symbol]
                self._place_entry(symbol, state, direction, level, n, session_date, ledger)

    def _place_add(self, symbol, state, rung, session_date, ledger):
        campaign = state.position.campaign
        accepted, reason = ledger.try_reserve(symbol, campaign.closely_group, campaign.loosely_group,
                                              campaign.direction)
        if not accepted:
            self._decline("add: " + reason)
            return
        state.add_order = _Order("add", campaign.direction, rung, campaign.campaign_n, campaign.unit_quantity,
                                 state.position.multiplier, session_date)

    def _place_entry(self, symbol, state, direction, level, n, session_date, ledger):
        multiplier = market_table.dollars_per_point(state.market, session_date)
        quantity = rules.unit_quantity(self.notional.current, self.config.unit_volatility_fraction, n,
                                       multiplier)
        if quantity <= 0:
            self._decline("entry: fewer than one contract")
            return
        accepted, reason = ledger.try_reserve(symbol, state.market.closely_group, state.market.loosely_group,
                                              direction)
        if not accepted:
            self._decline("entry: " + reason)
            return
        state.entry_orders[direction] = _Order("entry", direction, level, n, quantity, multiplier, session_date)

    def _cancel(self, state, which):
        """Cancel a resting entry (``which`` = 1 or -1) or Add (``"add"``)
        order, releasing its Unit-cap reservation."""
        if which == "add":
            order, state.add_order = state.add_order, None
        else:
            order, state.entry_orders[which] = state.entry_orders[which], None
        if order is not None:
            market = state.market
            self.caps.remove(state.symbol, market.closely_group, market.loosely_group, order.direction,
                             units=1)

    # --- money ---------------------------------------------------------------

    def _mark(self, state, bar):
        """Daily variation margin: every open Unit settles to today's close."""
        if state.position is None:
            return
        position = state.position
        campaign = position.campaign
        for unit in campaign.units:
            self.cash += campaign.direction * (bar.close - unit["mark"]) * campaign.unit_quantity \
                * position.multiplier
            unit["mark"] = bar.close

    def _charge_rolls(self, symbol, state, session_date):
        """The roll-cost approximation: the series is back-adjusted, so a roll
        moves no price, but a real position pays to roll. On the market's
        first Session on or after ``roll_day`` in each of its
        ``roll_months``, every open position is charged one extra round
        trip: two commissions and two 0.05N slippages per contract."""
        market = state.market
        if not self.config.charge_rolls or session_date.month not in market.roll_months \
                or session_date.day < market.roll_day:
            return
        key = (session_date.year, session_date.month)
        if state.last_roll == key:
            return
        state.last_roll = key
        position = state.position
        if position is None:
            return
        campaign = position.campaign
        contracts = campaign.unit_quantity * campaign.unit_count
        slip_dollars = rules.slippage(campaign.campaign_n, self.config.slippage_n) * position.multiplier
        cost = contracts * 2 * (self.config.commission_per_contract + slip_dollars)
        self.cash -= cost
        position.roll_cost += cost
        self.result.total_roll_cost += cost
        self.result.roll_count += 1

    def _commission(self, position, quantity):
        amount = quantity * self.config.commission_per_contract
        self.cash -= amount
        position.commission += amount
        self.result.total_commission += amount

    def _record(self, session_date, symbol, kind, direction, quantity, price):
        self.result.fills.append(Fill(session_date, symbol, kind, direction, quantity, price))

    def _decline(self, reason):
        self.result.declines[reason] = self.result.declines.get(reason, 0) + 1

    # --- the end of the run ----------------------------------------------------

    def _finish(self):
        result = self.result
        for symbol, state in sorted(self.states.items()):
            position = state.position
            if position is None:
                continue
            campaign = position.campaign
            gross = position.gross + position.unrealized()
            net = gross - position.commission - position.roll_cost
            result.open_campaigns.append(OpenCampaign(
                symbol, campaign.direction, position.entry_date, campaign.unit_quantity, campaign.unit_count,
                gross, position.commission, position.roll_cost, net))
            result.pnl_by_market[symbol] = result.pnl_by_market.get(symbol, 0.0) + net
        result.final_equity = self.cash
        account = self.cash - result.starting_equity
        campaigns = sum(c.net_pnl for c in result.campaigns) + sum(c.net_pnl for c in result.open_campaigns)
        difference = account - campaigns
        result.reconcile = Reconcile(account, campaigns, difference, abs(difference) <= RECONCILE_TOLERANCE)


def run_backtest(config, universe, series):
    """Check the dates (``HoldoutError`` past 2015 without
    ``allow_holdout``), then run."""
    check_dates(config)
    return Backtester(config, universe, series).run()


# -----------------------------------------------------------------------------
# Metrics and the report.
# -----------------------------------------------------------------------------


def span_cagr(curve, starting_equity, first, last):
    """CAGR over the curve's marks dated ``first``..``last`` inclusive, from
    the last mark before ``first`` (or the starting equity) so no Session's
    return is lost between spans."""
    inside = [(d, e) for d, e in curve if first <= d <= last]
    if not inside:
        return None
    before = [(d, e) for d, e in curve if d < first]
    start_date, start_equity = before[-1] if before else (inside[0][0], starting_equity)
    end_date, end_equity = inside[-1]
    return rules.annualised_return(start_equity, end_equity, (end_date - start_date).days)


def metrics(result):
    curve = result.equity_curve
    out = {"cagr": None, "max_drawdown": None, "ratio": None, "decades": [], "campaigns": len(result.campaigns),
           "win_rate": None, "avg_win_r": None, "avg_loss_r": None}
    if curve:
        out["cagr"] = span_cagr(curve, result.starting_equity, curve[0][0], curve[-1][0])
        out["max_drawdown"] = rules.max_drawdown([result.starting_equity] + [e for _, e in curve])
        out["ratio"] = rules.cagr_over_max_drawdown(out["cagr"], out["max_drawdown"])
        for decade in range(curve[0][0].year // 10 * 10, curve[-1][0].year + 1, 10):
            first, last = date(decade, 1, 1), date(decade + 9, 12, 31)
            out["decades"].append(("{}s".format(decade), span_cagr(curve, result.starting_equity, first, last)))
    wins = [c.r_multiple for c in result.campaigns if c.r_multiple > 0]
    losses = [c.r_multiple for c in result.campaigns if c.r_multiple <= 0]
    if result.campaigns:
        out["win_rate"] = len(wins) / len(result.campaigns)
    out["avg_win_r"] = sum(wins) / len(wins) if wins else None
    out["avg_loss_r"] = sum(losses) / len(losses) if losses else None
    return out


def _pct(value):
    return "n/a" if value is None else "{:+.2f}%".format(100 * value)


def _share(value):
    return "n/a" if value is None else "{:.2f}%".format(100 * value)


def _num(value):
    return "n/a" if value is None else "{:.3f}".format(value)


def format_report(config, result):
    m = metrics(result)
    curve = result.equity_curve
    lines = ["Turtle (Faith System 2) on back-adjusted futures -- local Pinnacle CLC backtest"]
    span = "{}..{}".format(curve[0][0], curve[-1][0]) if curve else "no sessions"
    lines.append("Span {}  markets {}  start equity ${:,.0f}  final ${:,.0f}".format(
        span, ",".join(result.markets), result.starting_equity, result.final_equity))
    if result.ruined_on:
        lines.append("RUINED: equity reached zero on {}".format(result.ruined_on))
    lines.append("CAGR {}  MaxDD {}  CAGR/MaxDD {}".format(
        _pct(m["cagr"]), _share(m["max_drawdown"]), _num(m["ratio"])))
    for label, value in m["decades"]:
        lines.append("  {} CAGR {}".format(label, _pct(value)))
    lines.append("Campaigns {} (open at end {})  win rate {}  avg win {}R  avg loss {}R".format(
        m["campaigns"], len(result.open_campaigns), _share(m["win_rate"]), _num(m["avg_win_r"]),
        _num(m["avg_loss_r"])))
    lines.append("Commission ${:,.2f}  Roll cost ${:,.2f} ({} rolls)".format(
        result.total_commission, result.total_roll_cost, result.roll_count))
    lines.append("P&L by market (net of costs, open Campaigns marked to the last settle):")
    for symbol, pnl in sorted(result.pnl_by_market.items(), key=lambda item: -item[1]):
        lines.append("  {:<4} ${:>16,.2f}".format(symbol, pnl))
    r = result.reconcile
    lines.append("Reconcile {}: account ${:,.2f}  campaigns ${:,.2f}  difference ${:,.4f}".format(
        "OK" if r.ok else "FAILED", r.account_pnl, r.campaign_pnl, r.difference))
    if result.declines:
        lines.append("Declines: " + ", ".join("{} {}".format(k, v) for k, v in sorted(result.declines.items())))
    return "\n".join(lines)


# -----------------------------------------------------------------------------
# The command line.
# -----------------------------------------------------------------------------


def _date_arg(text):
    return datetime.strptime(text, "%Y-%m-%d").date()


def _parser():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--data-dir", default=DEFAULT_DATA_DIR)
    parser.add_argument("--start", type=_date_arg, default=DEFAULT_START, help="YYYY-MM-DD")
    parser.add_argument("--end", type=_date_arg, default=DEFAULT_END, help="YYYY-MM-DD")
    parser.add_argument("--allow-holdout", action="store_true",
                        help="permit an end date in the held-out period (2016 on)")
    parser.add_argument("--markets", default=None, help="comma-separated symbols from markets.py")
    parser.add_argument("--file-template", default="{stem}",
                        help="file name without extension; {stem} and {symbol} are substituted")
    parser.add_argument("--commission", type=float, default=DEFAULT_COMMISSION, help="$ per contract per side")
    parser.add_argument("--slippage-n", type=float, default=rules.SLIPPAGE_N)
    parser.add_argument("--unit-fraction", type=float, default=rules.UNIT_VOLATILITY_FRACTION)
    parser.add_argument("--equity", type=float, default=1_000_000.0)
    parser.add_argument("--long-only", action="store_true")
    parser.add_argument("--no-roll-costs", action="store_true")
    return parser


def main(argv=None):
    args = _parser().parse_args(argv)
    config = Config(start=args.start, end=args.end, allow_holdout=args.allow_holdout,
                    starting_equity=args.equity, unit_volatility_fraction=args.unit_fraction,
                    commission_per_contract=args.commission, slippage_n=args.slippage_n,
                    long_only=args.long_only, charge_rolls=not args.no_roll_costs)
    try:
        check_dates(config)
    except ValueError as err:
        print("backtest: {}".format(err), file=sys.stderr)
        return 2
    if not os.path.isdir(args.data_dir):
        print("backtest: no data directory {}".format(args.data_dir), file=sys.stderr)
        return 1
    symbols = args.markets.split(",") if args.markets else list(market_table.MARKETS)
    universe, series = {}, {}
    for symbol in symbols:
        market = market_table.MARKETS.get(symbol.strip())
        if market is None:
            print("backtest: unknown market {}".format(symbol), file=sys.stderr)
            return 1
        stem = args.file_template.format(stem=market.file_stem, symbol=market.symbol)
        path = loader.find_market_file(args.data_dir, stem)
        if path is None:
            print("backtest: {}: no file {}.* in {}; skipped".format(market.symbol, stem, args.data_dir),
                  file=sys.stderr)
            continue
        universe[market.symbol] = market
        series[market.symbol] = loader.load_series(path)
        scale = loader.check_series_scale(market.symbol, series[market.symbol])
        if scale == "alternate":
            print("backtest: {}: last settle {} looks like the alternate price scale markets.py's "
                  "comment names, not the one its multiplier assumes -- check price_units before "
                  "trusting this run".format(market.symbol, series[market.symbol][-1].close),
                  file=sys.stderr)
        elif scale == "unknown":
            print("backtest: {}: last settle {} matches neither price scale markets.py "
                  "considers -- check price_units before trusting this run".format(
                      market.symbol, series[market.symbol][-1].close), file=sys.stderr)
    if not series:
        print("backtest: no market files found", file=sys.stderr)
        return 1
    result = run_backtest(config, universe, series)
    print(format_report(config, result))
    return 0 if result.reconcile.ok else 1


if __name__ == "__main__":
    sys.exit(main())
