"""Unit tests for tsmom.py, the time-series momentum backtester
(Moskowitz, Ooi & Pedersen 2012, "Time Series Momentum", JFE).

Every series here is synthetic and built in this file; no real market
data is used (README.md, "No market data is committed").

Run with `make research-test` from the repository root.
"""

import io
import math
import os
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from datetime import date, timedelta

import rates
import tsmom
import tsmom_markets

DAY0 = date(1990, 1, 1)


def _market(symbol="TST", multiplier=1000.0, tick=0.01):
    return tsmom_markets.TsmomMarket(symbol, "test " + symbol, "commodity", multiplier, tick)


def _weekdays(start, count):
    out, day = [], start
    while len(out) < count:
        if day.weekday() < 5:
            out.append(day)
        day += timedelta(days=1)
    return out


def _rows(closes, day0=DAY0, basis=None):
    """``closes`` as both REV and NON (so no roll), one per weekday; with
    ``basis`` (a list), REV = NON + basis[i], so a change in basis is a
    roll."""
    days = _weekdays(day0, len(closes))
    if basis is None:
        basis = [0.0] * len(closes)
    return [tsmom.Row(d, c + b, c) for d, c, b in zip(days, closes, basis)]


def _trend(n, start=100.0, step=0.1, wiggle=0.5):
    """A rising series with alternating noise, so volatility is non-zero."""
    return [start + step * i + (wiggle if i % 2 else -wiggle) for i in range(n)]


def _config(**overrides):
    values = dict(start=date(1991, 3, 1), end=date(1992, 12, 31))
    values.update(overrides)
    return tsmom.Config(**values)


class SignalTests(unittest.TestCase):
    def test_daily_return_is_the_rev_change_over_the_previous_non_settle(self):
        # REV is negative (a back-adjusted level); NON is the real price.
        rows = [tsmom.Row(DAY0, -50.0, 100.0), tsmom.Row(DAY0 + timedelta(1), -48.0, 102.0)]
        self.assertAlmostEqual(tsmom.daily_return(rows[0], rows[1]), 2.0 / 100.0)

    def test_a_non_positive_previous_non_settle_is_refused(self):
        with self.assertRaises(ValueError):
            tsmom.daily_return(tsmom.Row(DAY0, 1.0, 0.0), tsmom.Row(DAY0 + timedelta(1), 2.0, 1.0))

    def test_sign_of_the_twelve_month_return(self):
        history = [(date(1990, 1, 31), 1.0), (date(1990, 6, 29), 1.3), (date(1991, 1, 31), 1.1)]
        self.assertAlmostEqual(tsmom.trailing_return(history, date(1991, 1, 31)), 0.1)
        self.assertEqual(tsmom.signal(tsmom.trailing_return(history, date(1991, 1, 31))), 1)
        falling = [(date(1990, 1, 31), 1.0), (date(1991, 1, 31), 0.9)]
        self.assertEqual(tsmom.signal(tsmom.trailing_return(falling, date(1991, 1, 31))), -1)
        self.assertEqual(tsmom.signal(0.0), 0)

    def test_fewer_than_twelve_months_of_history_gives_no_signal(self):
        history = [(date(1990, 2, 1), 1.0), (date(1991, 1, 31), 1.2)]
        self.assertIsNone(tsmom.trailing_return(history, date(1991, 1, 31)))

    def test_the_anchor_is_the_last_mark_on_or_before_one_year_ago(self):
        history = [(date(1990, 1, 29), 1.0), (date(1990, 2, 1), 5.0), (date(1991, 1, 31), 2.0)]
        # 1990-01-31 is not a mark; the anchor is 1990-01-29's 1.0.
        self.assertAlmostEqual(tsmom.trailing_return(history, date(1991, 1, 31)), 1.0)
        # Marks after the as-of date are never read.
        history.append((date(1991, 2, 28), 100.0))
        self.assertAlmostEqual(tsmom.trailing_return(history, date(1991, 1, 31)), 1.0)


class EwmaTests(unittest.TestCase):
    def test_against_a_hand_calculation(self):
        # MOP eq. (1): weights (1 - d) d^i, most recent first, d / (1 - d) = 60.
        returns = [0.01, -0.02, 0.03]
        d = 60.0 / 61.0
        weights = [(1 - d) * d ** i for i in range(3)]      # for 0.03, -0.02, 0.01
        recent_first = [0.03, -0.02, 0.01]
        total = sum(weights)
        mean = sum(w * r for w, r in zip(weights, recent_first)) / total
        variance = sum(w * (r - mean) ** 2 for w, r in zip(weights, recent_first)) / total
        ewma = tsmom.EwmaVariance(center_of_mass=60)
        for r in returns:
            ewma.add(r)
        self.assertAlmostEqual(ewma.variance(), variance, places=15)
        self.assertAlmostEqual(ewma.annualised_vol(), math.sqrt(261 * variance), places=12)
        # Written out: d = 0.98361, so the weights are close to equal and
        # the variance is near the plain (population) variance, 0.000422.
        self.assertAlmostEqual(variance, 0.000422, places=5)

    def test_a_constant_series_has_zero_variance(self):
        ewma = tsmom.EwmaVariance()
        for _ in range(100):
            ewma.add(0.001)
        self.assertAlmostEqual(ewma.variance(), 0.0, places=15)


class SizingTests(unittest.TestCase):
    def test_forty_percent_over_sigma_split_equally(self):
        weights = tsmom.target_weights({"A": (1, 0.20), "B": (-1, 0.40)}, target_vol=0.40)
        self.assertAlmostEqual(weights["A"], 0.40 / 0.20 / 2)
        self.assertAlmostEqual(weights["B"], -0.40 / 0.40 / 2)

    def test_whole_contracts_round_half_away_from_zero(self):
        # $1M x 1.0 / (100 x $1,000) = 10 contracts.
        self.assertEqual(tsmom.contracts(1_000_000, 1.0, 100.0, 1000.0), 10)
        self.assertEqual(tsmom.contracts(1_000_000, 0.024, 100.0, 1000.0), 0)   # 0.24
        self.assertEqual(tsmom.contracts(1_000_000, 0.25, 100.0, 1000.0), 3)    # 2.5
        self.assertEqual(tsmom.contracts(1_000_000, -0.25, 100.0, 1000.0), -3)
        self.assertEqual(tsmom.contracts(1_000_000, 0.249, 100.0, 1000.0), 2)   # 2.49

    def test_portfolio_scale_hits_the_target_on_the_trailing_window(self):
        # Book returns alternate +1% / -1%: sample sd 0.01 * sqrt(n/(n-1)).
        book = [0.01, -0.01] * 50
        scale = tsmom.portfolio_scale(book, target=0.10)
        sd = math.sqrt(sum(r * r for r in book) / (len(book) - 1))
        self.assertAlmostEqual(scale, 0.10 / (sd * math.sqrt(261)))

    def test_portfolio_scale_needs_enough_history(self):
        self.assertIsNone(tsmom.portfolio_scale([0.01] * 5, target=0.10))


class CostTests(unittest.TestCase):
    def test_trade_cost(self):
        commission, slippage = tsmom.trade_cost(-10, commission=2.50, slippage_ticks=1.0, tick=0.01,
                                                multiplier=1000.0)
        self.assertAlmostEqual(commission, 25.0)
        self.assertAlmostEqual(slippage, 100.0)      # 10 x 0.01 x $1,000

    def test_roll_cost_is_a_round_trip(self):
        cost = tsmom.roll_cost(-10, commission=2.50, slippage_ticks=1.0, tick=0.01, multiplier=1000.0)
        self.assertAlmostEqual(cost, 10 * 2 * (2.50 + 10.0))


def _two_markets(n=700):
    up = _rows(_trend(n))
    down = _rows([200.0 - 0.1 * i + (0.5 if i % 2 else -0.5) for i in range(n)])
    universe = {"UP": _market("UP"), "DN": _market("DN", multiplier=500.0, tick=0.05)}
    return universe, {"UP": up, "DN": down}


class TimingTests(unittest.TestCase):
    def test_rebalance_decides_at_month_end_and_fills_on_the_next_session(self):
        universe, series = _two_markets()
        result = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(result.decisions)
        calendar = sorted({row.date for rows in series.values() for row in rows})
        for decision in result.decisions:
            index = calendar.index(decision.date)
            self.assertNotEqual(calendar[index + 1].month, decision.date.month)
        first = result.decisions[0]
        fills = [f for f in result.fills if f.date > first.date]
        next_session = calendar[calendar.index(first.date) + 1]
        self.assertTrue(fills)
        self.assertEqual(min(f.date for f in fills), next_session)
        self.assertGreaterEqual(next_session, _config().start)
        # UP rose, DN fell over the year: long UP, short DN.
        self.assertGreater(first.targets["UP"], 0)
        self.assertLess(first.targets["DN"], 0)
        # The fill is at the next session's REV settle.
        up_fill = [f for f in fills if f.symbol == "UP"][0]
        settle = [r.rev for r in series["UP"] if r.date == next_session][0]
        self.assertEqual(up_fill.price, settle)

    def test_a_decision_uses_no_data_after_its_day(self):
        universe, series = _two_markets()
        base = tsmom.run_backtest(_config(), universe, series)
        decision = base.decisions[3]
        # Change every price after the decision day drastically.
        altered = {}
        for symbol, rows in series.items():
            altered[symbol] = [r if r.date <= decision.date else tsmom.Row(r.date, r.rev * 3.0, r.non * 3.0)
                               for r in rows]
        other = tsmom.run_backtest(_config(), universe, altered)
        match = [d for d in other.decisions if d.date == decision.date][0]
        self.assertEqual(match.targets, decision.targets)
        self.assertEqual(match.equity, decision.equity)

    def test_a_market_that_has_not_started_trading_yet_does_not_crash_the_decision(self):
        # A market that joins the universe partway through the run (its
        # first bar is after several decisions have already happened) has
        # no signal and no state.prev yet on those earlier decision days;
        # it must be skipped, not crash on state.prev.non.
        universe, series = _two_markets(900)
        late_start = DAY0 + timedelta(days=400)
        universe["LATE"] = _market("LATE")
        series["LATE"] = _rows(_trend(200), day0=late_start)
        result = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(result.reconcile.ok, result.reconcile)

    def test_nothing_fills_before_the_start(self):
        universe, series = _two_markets()
        result = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(all(f.date >= _config().start for f in result.fills))


class MultiplierTimingTests(unittest.TestCase):
    def test_sizing_uses_the_fill_dates_multiplier_not_the_decision_dates(self):
        # The SP 1997 change can fall between a decision (month end) and its
        # fill (the next session); the CONTRACT COUNT traded must be sized
        # with the fill date's multiplier, not the decision date's (a
        # review finding).
        n = 700
        universe = {"UP": _market("UP")}
        series = {"UP": _rows(_trend(n))}
        probe = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(probe.decisions)
        decision = probe.decisions[0]
        calendar = sorted({r.date for r in series["UP"]})
        fill_date = calendar[calendar.index(decision.date) + 1]

        old_multiplier, new_multiplier = 1000.0, 400.0
        universe["UP"] = _market("UP", multiplier=((date(1900, 1, 1), old_multiplier),
                                                     (fill_date, new_multiplier)))
        result = tsmom.run_backtest(_config(), universe, series)
        d = result.decisions[0]
        self.assertEqual(d.date, decision.date)
        row = next(r for r in series["UP"] if r.date == d.date)
        expected = tsmom.contracts(d.equity, d.weights["UP"], row.non, new_multiplier)
        wrong = tsmom.contracts(d.equity, d.weights["UP"], row.non, old_multiplier)
        self.assertNotEqual(expected, wrong)  # the schedule must actually bite
        self.assertEqual(result.positions_on(fill_date)["UP"], expected)


class MultiplierMidPeriodTests(unittest.TestCase):
    def test_a_multiplier_change_mid_period_resizes_and_reprices_costs(self):
        # Between two rebalances, a contract-size change (SP, 1997) must
        # resize the held position and be used for every subsequent
        # per-contract cost (here, a roll) straight away -- not only at the
        # next rebalance, up to a month later (a review finding).
        n = 900
        universe = {"UP": _market("UP")}
        series = {"UP": _rows(_trend(n))}
        probe = tsmom.run_backtest(_config(), universe, series)
        calendar = sorted({r.date for r in series["UP"]})
        d0, d1 = probe.decisions[0], probe.decisions[1]
        fill0 = calendar[calendar.index(d0.date) + 1]
        fill1 = calendar[calendar.index(d1.date) + 1]
        initial_quantity = probe.positions_on(fill0)["UP"]
        self.assertNotEqual(initial_quantity, 0)

        between = [d for d in calendar if fill0 < d < fill1]
        self.assertGreater(len(between), 10)
        change_date = between[len(between) // 3]
        roll_date = between[len(between) // 3 + 5]
        self.assertLess(roll_date, fill1)

        basis = [3.0 if r.date >= roll_date else 0.0 for r in series["UP"]]
        rolled_series = {"UP": _rows(_trend(n), basis=basis)}
        old_multiplier, new_multiplier = 1000.0, 400.0
        schedule_market = _market("UP", multiplier=((date(1900, 1, 1), old_multiplier),
                                                     (change_date, new_multiplier)))
        result = tsmom.run_backtest(_config(), {"UP": schedule_market}, rolled_series)

        resized_quantity = int(round(initial_quantity * old_multiplier / new_multiplier))
        self.assertEqual(result.positions_on(change_date)["UP"], resized_quantity)
        self.assertEqual(result.roll_count, 1)
        expected_roll_cost = tsmom.roll_cost(resized_quantity, tsmom.backtest.DEFAULT_COMMISSION, 1.0,
                                             schedule_market.tick_size, new_multiplier)
        wrong_roll_cost = tsmom.roll_cost(initial_quantity, tsmom.backtest.DEFAULT_COMMISSION, 1.0,
                                          schedule_market.tick_size, old_multiplier)
        self.assertNotEqual(round(expected_roll_cost, 6), round(wrong_roll_cost, 6))
        self.assertAlmostEqual(result.total_roll_cost, expected_roll_cost, places=6)
        self.assertTrue(result.reconcile.ok, result.reconcile)


def _realised_book(series, decisions, end=None):
    """Rebuild the overlay's realised book the way the execution contract
    defines it: a decision on day d fills at the next session's settle, so
    day d+1's return still belongs to the weights held BEFORE the decision,
    and days with no active weights are not part of the book at all."""
    returns = {}
    for symbol, rows in series.items():
        by_date, prev = {}, None
        for row in rows:
            if prev is not None:
                by_date[row.date] = tsmom.daily_return(prev, row)
            prev = row
        returns[symbol] = by_date
    calendar = sorted({r.date for rows in series.values() for r in rows if end is None or r.date <= end})
    decisions_by_date = {d.date: d for d in decisions}
    traded = {s: {r.date for r in rows} for s, rows in series.items()}
    last = {s: max(d for d in traded[s] if d in set(calendar)) for s in series}
    active, pending, book = {}, {}, []
    for day in calendar:
        if active:
            book.append((day, sum(w * returns[s].get(day, 0.0) for s, w in active.items())))
        # Each market's new weight takes over only once THAT market has
        # traded (filled) after the decision. A market whose data has ended
        # (flattened on its last bar) leaves the book entirely.
        for s in [s for s in pending if day in traded[s]]:
            w = pending.pop(s)
            if w:
                active[s] = w
            else:
                active.pop(s, None)
        for s in [s for s in list(active) + list(pending) if last[s] <= day < calendar[-1]]:
            active.pop(s, None)
            pending.pop(s, None)
        if day in decisions_by_date:
            new = decisions_by_date[day].weights
            pending = {s: new.get(s, 0.0) for s in set(active) | set(new)}
    return calendar, returns, book


class PortfolioOverlayTests(unittest.TestCase):
    def test_a_decisions_weights_start_counting_the_day_after_its_fill_day(self):
        # A decision at day d fills at day d+1's settle; the d+1 return was
        # earned by the OLD exposure (a review finding).
        universe, series = _two_markets(900)
        result = tsmom.run_backtest(_config(portfolio_target=0.10), universe, series)
        _, _, expected = _realised_book(series, result.decisions, _config().end)
        self.assertEqual(len(result.book_history), len(expected))
        for (d1, r1), (d2, r2) in zip(result.book_history, expected):
            self.assertEqual(d1, d2)
            self.assertAlmostEqual(r1, r2, places=12)

    def test_a_market_that_misses_the_fill_session_keeps_its_old_weight_until_it_trades(self):
        # DN has no bar on the first session of each month, so it fills a
        # session later than UP; its pre-fill return must still carry its
        # OLD weight (a review finding).
        universe, series = _two_markets(900)
        firsts = set()
        seen = set()
        for row in series["UP"]:
            key = (row.date.year, row.date.month)
            if key not in seen:
                seen.add(key)
                firsts.add(row.date)
        series["DN"] = [r for r in series["DN"] if r.date not in firsts]
        config = _config(portfolio_target=0.10)
        result = tsmom.run_backtest(config, universe, series)
        _, _, expected = _realised_book(series, result.decisions, config.end)
        self.assertEqual([d for d, _ in result.book_history], [d for d, _ in expected])
        for (_, r1), (_, r2) in zip(result.book_history, expected):
            self.assertAlmostEqual(r1, r2, places=12)

    def test_a_market_whose_data_ends_leaves_the_overlay_book(self):
        # A market whose file ends mid-run is flattened on its final bar,
        # and its weight must not keep adding zero-return days to the book
        # (a review finding).
        # UP is the only market with a signal and its file ends at bar 600;
        # FLAT (constant price, so no volatility and never a signal) keeps
        # the calendar running. After UP's last bar the book must be
        # inactive, not a run of zero-return "active" days.
        universe = {"UP": _market("UP"), "FLAT": _market("FLAT")}
        series = {"UP": _rows(_trend(900))[:600], "FLAT": _rows([100.0] * 900)}
        config = _config(portfolio_target=0.10)
        result = tsmom.run_backtest(config, universe, series)
        _, _, expected = _realised_book(series, result.decisions, config.end)
        self.assertEqual([d for d, _ in result.book_history], [d for d, _ in expected])
        for (_, r1), (_, r2) in zip(result.book_history, expected):
            self.assertAlmostEqual(r1, r2, places=12)
        up_last = series["UP"][-1].date
        self.assertFalse(any(d > up_last for d, _ in result.book_history))

    def test_the_overlay_window_is_the_last_year_of_sessions_not_of_active_days(self):
        # After a long inactive gap, returns from before the gap must not
        # stay in the trailing window (a review finding).
        day0 = date(2020, 1, 1)
        calendar = [day0 + timedelta(days=i) for i in range(400)]
        history = [(calendar[i], 0.01) for i in range(0, 100)] + [(calendar[i], 0.001) for i in range(300, 400)]
        window = tsmom.trailing_book_returns(history, calendar, 399, tsmom.PORTFOLIO_VOL_WINDOW)
        self.assertEqual(window, [0.001] * 100)

    def test_the_overlay_ignores_days_before_the_book_was_active(self):
        # Days before any weights were decided carry no book return at all;
        # counting them as 0.0 would shrink the measured volatility and
        # oversize the first scaled positions (a review finding). Each
        # scale must come from active days only, and needs at least
        # PORTFOLIO_VOL_MIN_OBS of them.
        universe, series = _two_markets(900)
        config = _config(portfolio_target=0.10)
        result = tsmom.run_backtest(config, universe, series)
        calendar, _, book = _realised_book(series, result.decisions, config.end)
        self.assertFalse(any(d < result.decisions[0].date for d, _ in book))
        checked = 0
        for decision in result.decisions:
            if not decision.weights:
                continue
            window = tsmom.trailing_book_returns(book, calendar, calendar.index(decision.date))
            expected = tsmom.portfolio_scale(window, config.portfolio_target)
            if expected is None:
                self.assertIsNone(decision.scale)
            else:
                self.assertAlmostEqual(decision.scale, expected, places=9)
            checked += 1
        self.assertGreaterEqual(checked, 2)

    def test_the_overlay_uses_the_realised_book_not_this_months_new_weights(self):
        # The 10% overlay's trailing vol must come from each PAST day's
        # ACTIVE (already-filled) weights times that day's own realised
        # return -- never today's brand-new weights re-applied to a whole
        # year of history that was never actually held at those weights
        # (a review finding).
        universe, series = _two_markets(900)
        config = _config(portfolio_target=0.10)
        result = tsmom.run_backtest(config, universe, series)
        calendar, returns, book = _realised_book(series, result.decisions, config.end)
        scaled = [d for d in result.decisions if d.weights and d.scale is not None]
        self.assertGreaterEqual(len(scaled), 1)
        decision = scaled[0]
        idx = calendar.index(decision.date)
        window = tsmom.trailing_book_returns(book, calendar, idx)
        self.assertAlmostEqual(decision.scale, tsmom.portfolio_scale(window, config.portfolio_target), places=9)

        # The naive (ex-ante) method -- this decision's own new weights
        # applied across the whole window -- must give a DIFFERENT number,
        # or this scenario doesn't exercise the fix.
        first = max(0, idx + 1 - tsmom.PORTFOLIO_VOL_WINDOW)
        naive_window = [sum(w * returns[s].get(d, 0.0) for s, w in decision.weights.items())
                        for d in calendar[first:idx + 1]]
        naive_scale = tsmom.portfolio_scale(naive_window, config.portfolio_target)
        self.assertNotAlmostEqual(decision.scale, naive_scale, places=6)

class RollAndReconcileTests(unittest.TestCase):
    def test_a_roll_charges_every_open_position_one_round_trip(self):
        n = 700
        basis = [0.0 if i < 600 else 3.0 for i in range(n)]    # one roll, on bar 600
        universe = {"UP": _market("UP")}
        series = {"UP": _rows(_trend(n), basis=basis)}
        config = _config(slippage_ticks=1.0, commission=2.5)
        result = tsmom.run_backtest(config, universe, series)
        roll_day = series["UP"][600].date
        self.assertEqual(result.roll_count, 1)
        held = result.positions_on(roll_day - timedelta(days=1))["UP"]
        self.assertNotEqual(held, 0)
        self.assertAlmostEqual(result.total_roll_cost, abs(held) * 2 * (2.5 + 0.01 * 1000.0))

    def test_position_pnl_plus_costs_equals_the_account_pnl(self):
        universe, series = _two_markets(900)
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.05)])
        for config in (_config(), _config(interest_rates=curve), _config(portfolio_target=0.10),
                       _config(end=date(1993, 6, 30))):
            result = tsmom.run_backtest(config, universe, series)
            self.assertTrue(result.reconcile.ok, result.reconcile)
            self.assertGreater(result.total_commission, 0)
            self.assertGreater(result.total_slippage, 0)

    def test_reconcile_holds_across_a_multiplier_change(self):
        universe, series = _two_markets(900)
        universe["UP"] = _market("UP", multiplier=((date(1900, 1, 1), 1000.0), (date(1992, 1, 1), 500.0)))
        result = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(result.reconcile.ok, result.reconcile)

    def test_a_market_whose_file_ends_is_flattened_on_its_last_bar(self):
        universe, series = _two_markets(900)
        series["DN"] = series["DN"][:600]
        result = tsmom.run_backtest(_config(), universe, series)
        last = series["DN"][-1].date
        self.assertEqual(result.positions_on(last)["DN"], 0)
        self.assertTrue(result.reconcile.ok)

    def test_a_weekend_end_date_is_not_treated_as_a_delisting(self):
        # Both markets' real data run well past the chosen --end; a Friday
        # right before a weekend --end must not be read as "the file ends
        # here" just because clipping made it the last bar.
        universe, series = _two_markets(920)
        weekdays = sorted({r.date for rows in series.values() for r in rows})
        friday = next(d for d in weekdays[400:-10] if d.weekday() == 4)
        saturday = friday + timedelta(days=1)  # a weekend date, no bar of its own
        self.assertEqual(saturday.weekday(), 5)
        result = tsmom.run_backtest(_config(end=saturday), universe, series)
        self.assertFalse(any(f.kind == "delist" for f in result.fills))
        self.assertTrue(result.reconcile.ok, result.reconcile)

    def test_interest_is_credited_but_kept_out_of_the_reconcile(self):
        universe, series = _two_markets()
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.036)])
        without = tsmom.run_backtest(_config(), universe, series)
        with_interest = tsmom.run_backtest(_config(interest_rates=curve), universe, series)
        self.assertGreater(with_interest.total_interest, 0)
        self.assertEqual(without.total_interest, 0)
        self.assertTrue(with_interest.reconcile.ok)


class EmptySpanTests(unittest.TestCase):
    def test_metrics_and_the_report_handle_a_span_with_no_bars(self):
        # A run whose start..end has no bar for any market (e.g. every
        # market file starts after the requested span) must not crash the
        # report on an empty equity_curve's curve[0].
        universe, series = _two_markets(50)
        config = _config(start=date(1960, 1, 1), end=date(1960, 12, 31))
        result = tsmom.run_backtest(config, universe, series)
        self.assertEqual(result.equity_curve, [])
        m = tsmom.metrics(config, result)
        self.assertIsNone(m["cagr"])
        self.assertIsNone(m["max_drawdown"])
        self.assertIsNone(m["sharpe"])
        text, m2 = tsmom.format_report(config, result)
        self.assertIn("No bars", text)
        self.assertIn("Reconcile", text)


class HoldoutTests(unittest.TestCase):
    def test_an_end_in_2016_is_refused_without_the_flag(self):
        universe, series = _two_markets()
        with self.assertRaises(tsmom.HoldoutError):
            tsmom.run_backtest(_config(end=date(2016, 1, 4)), universe, series)
        tsmom.run_backtest(_config(end=date(2016, 1, 4), allow_holdout=True), universe, series)

    def test_the_command_line_refuses_without_the_flag(self):
        err = io.StringIO()
        with redirect_stderr(err):
            code = tsmom.main(["--end", "2016-06-30", "--data-dir", "/nonexistent"])
        self.assertEqual(code, 2)
        self.assertIn("held-out", err.getvalue())


class CommandLineTests(unittest.TestCase):
    def test_runs_on_synthetic_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            universe, series = _two_markets(900)
            for symbol, rows in series.items():
                for kind, field in (("REV", "rev"), ("NON", "non")):
                    with open(os.path.join(tmp, "{}_{}.CSV".format(symbol, kind)), "w") as handle:
                        for r in rows:
                            price = getattr(r, field)
                            handle.write("{},{},{},{},{},0,0\n".format(r.date.strftime("%m/%d/%Y"),
                                                                       price, price, price, price))
            out = io.StringIO()
            with redirect_stdout(out), redirect_stderr(io.StringIO()):
                code = tsmom.main(["--data-dir", tmp, "--markets", "UP,DN", "--start", "1991-03-01",
                                   "--end", "1993-06-30", "--benchmark", "UP", "--equity", "1000000"],
                                  universe=universe)
        self.assertEqual(code, 0)
        text = out.getvalue()
        self.assertIn("Reconcile OK", text)
        self.assertIn("Sharpe", text)

    def test_a_bad_interest_rates_file_is_caught_not_a_crash(self):
        # Mirrors backtest.main's own --interest-rates error handling: print
        # a message and exit 1, rather than letting the exception propagate.
        with tempfile.TemporaryDirectory() as tmp:
            bad_path = os.path.join(tmp, "rates.csv")
            with open(bad_path, "w") as handle:
                handle.write("DATE,RATE\nnot-a-date,5.0\n")
            err = io.StringIO()
            with redirect_stderr(err), redirect_stdout(io.StringIO()):
                code = tsmom.main(["--data-dir", tmp, "--interest-rates", bad_path])
        self.assertEqual(code, 1)
        self.assertIn(bad_path, err.getvalue())

    def test_a_missing_interest_rates_file_is_caught_not_a_crash(self):
        with tempfile.TemporaryDirectory() as tmp:
            missing_path = os.path.join(tmp, "nonexistent.csv")
            err = io.StringIO()
            with redirect_stderr(err), redirect_stdout(io.StringIO()):
                code = tsmom.main(["--data-dir", tmp, "--interest-rates", missing_path])
        self.assertEqual(code, 1)
        self.assertIn(missing_path, err.getvalue())


class MarketTableTests(unittest.TestCase):
    def test_every_market_has_a_positive_multiplier_and_tick(self):
        for market in tsmom_markets.TSMOM_MARKETS.values():
            self.assertGreater(tsmom_markets.dollars_per_point(market, date(2015, 12, 31)), 0)
            self.assertGreater(market.tick_size, 0)
            self.assertIn(market.sector, ("equity", "bond", "currency", "commodity"))

    def test_no_market_is_both_traded_and_excluded(self):
        self.assertFalse(set(tsmom_markets.TSMOM_MARKETS) & set(tsmom_markets.EXCLUDED))

    def test_silver_uses_min_move_over_tick_not_bigpoint(self):
        self.assertEqual(tsmom_markets.TSMOM_MARKETS["ZI"].multiplier, 25.0 / 0.5)

    def test_sp_keeps_the_1997_halving(self):
        sp = tsmom_markets.TSMOM_MARKETS["SP"]
        self.assertEqual(tsmom_markets.dollars_per_point(sp, date(1990, 1, 2)), 500.0)
        self.assertEqual(tsmom_markets.dollars_per_point(sp, date(2000, 1, 3)), 250.0)


if __name__ == "__main__":
    unittest.main()
