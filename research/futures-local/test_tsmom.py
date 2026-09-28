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

    def test_nothing_fills_before_the_start(self):
        universe, series = _two_markets()
        result = tsmom.run_backtest(_config(), universe, series)
        self.assertTrue(all(f.date >= _config().start for f in result.fills))


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

    def test_interest_is_credited_but_kept_out_of_the_reconcile(self):
        universe, series = _two_markets()
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.036)])
        without = tsmom.run_backtest(_config(), universe, series)
        with_interest = tsmom.run_backtest(_config(interest_rates=curve), universe, series)
        self.assertGreater(with_interest.total_interest, 0)
        self.assertEqual(without.total_interest, 0)
        self.assertTrue(with_interest.reconcile.ok)


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
