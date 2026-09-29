"""Tests for the follow-up research code (docs/research/boost-followup-2026-09.md):
the variant builder, the Boost / mix / trend templates' daily rules, and the
futures carry measure. The templates import QuantConnect's AlgorithmImports,
so, like timing/test_timing.py, these tests install a minimal stand-in with
just the names the templates use and an algorithm that records its orders.
Standard library only; synthetic values, no market data.
"""
import ast
import datetime as _dt
import importlib.util
import os
import sys
import tempfile
import types
import unittest
from collections import namedtuple

import build_variants

HERE = os.path.dirname(os.path.abspath(__file__))
TB3MS_ROWS = ["observation_date,TB3MS", "1991-12-01,4.00", "1992-01-01,3.00", "1992-02-01,.", "1992-03-01,2.00"]


def _csv(test, rows):
    with tempfile.NamedTemporaryFile("w", suffix=".csv", delete=False) as f:
        f.write("\n".join(rows) + "\n")
    test.addCleanup(os.remove, f.name)
    return f.name


class _Target:
    def __init__(self, symbol, quantity):
        self.Symbol, self.Quantity = symbol, quantity


def _load_template(test, template, params):
    stub = types.ModuleType("AlgorithmImports")
    stub.QCAlgorithm = type("QCAlgorithm", (), {})
    stub.PortfolioTarget = _Target
    for name in ("Resolution", "MovingAverageType", "BrokerageName", "AccountType"):
        setattr(stub, name, types.SimpleNamespace())
    sys.modules["AlgorithmImports"] = stub
    with open(os.path.join(HERE, template)) as f:
        src = build_variants.render(f.read(), params, {199201: 0.03, 199203: 0.02})
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as g:
        g.write(src)
    test.addCleanup(os.remove, g.name)
    spec = importlib.util.spec_from_file_location("variant_under_test", g.name)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class _Indicator:
    def __init__(self, value, ready=True):
        self.IsReady = ready
        self.Current = types.SimpleNamespace(Value=value)


class _Cash:
    def __init__(self):
        self.added = 0.0

    def AddAmount(self, amount):
        self.added += amount


def _algorithm(module, cls, names=("SPY",)):
    """An algorithm built without Initialize(): SPY above its 200-day SMA,
    RSI(2) neutral, close below its 5-day SMA, every holding flat."""
    algo = getattr(module, cls)()
    algo.sym = "SPY"
    algo.syms = {n: n for n in names}
    algo.trend, algo.exit_sma, algo.rsi2 = _Indicator(90.0), _Indicator(101.0), _Indicator(50.0)
    algo.IsWarmingUp = False
    algo.Time = _dt.datetime(1992, 1, 15)
    algo.Securities = {n: types.SimpleNamespace(Price=100.0) for n in names}
    algo.holdings = {n: 0.0 for n in names}
    algo.equity = 1_000_000.0
    algo.Portfolio = types.SimpleNamespace(CashBook={"USD": _Cash()})
    algo.curve, algo.px = [], []
    algo.sig, algo.last_want, algo.last_month, algo.prev_close, algo.prev_w = False, None, None, None, 0.0
    algo.financing = algo.w_sum = algo.boost_ret = algo.norm_ret = 0.0
    algo.episodes = algo.days = algo.boost_days = 0
    algo.orders = []

    class _Book(dict):
        def __getitem__(self, name):
            return types.SimpleNamespace(HoldingsValue=algo.holdings[name])

    def refresh():
        book = _Book()
        book.TotalPortfolioValue = algo.equity
        book.TotalHoldingsValue = sum(algo.holdings.values())
        book.CashBook = algo.Portfolio.CashBook
        algo.Portfolio = book

    def set_holdings(target, size=None):
        targets = target if isinstance(target, list) else [_Target(target, size)]
        algo.orders.append({t.Symbol: round(t.Quantity, 6) for t in targets})
        for t in targets:
            algo.holdings[t.Symbol] = t.Quantity * algo.equity
        refresh()

    algo.SetHoldings = set_holdings
    algo.refresh = refresh
    refresh()
    return algo


def _day(algo, close=100.0, when=None):
    if when:
        algo.Time = when
    algo.refresh()
    algo.OnData(types.SimpleNamespace(Bars={"SPY": types.SimpleNamespace(Close=close)}))


class BuilderTest(unittest.TestCase):
    def test_rates_are_the_previous_months_average_from_the_first_month(self):
        rates = build_variants.prior_month_rates(_csv(self, TB3MS_ROWS))
        self.assertEqual(rates, {199201: 0.04, 199202: 0.03, 199204: 0.02})

    def test_a_file_without_the_first_months_rate_is_refused(self):
        with self.assertRaises(SystemExit):
            build_variants.prior_month_rates(_csv(self, TB3MS_ROWS[:1] + TB3MS_ROWS[2:]))

    def test_render_fills_the_rate_table_and_the_named_constants(self):
        src = "RATES = __RATES__\nclass A:\n    MODE = \"boost\"\n    SPREAD = 0.015\n"
        out = build_variants.render(src, {"MODE": "bh"}, {199201: 0.04})
        self.assertIn("RATES = {199201: 0.0400}", out)
        self.assertIn("    MODE = 'bh'", out)
        self.assertIn("    SPREAD = 0.015", out)

    def test_an_unknown_constant_is_refused(self):
        with self.assertRaises(SystemExit):
            build_variants.render("class A:\n    MODE = 1\n", {"NOPE": 2}, {})

    def test_every_variant_builds_to_valid_python_with_its_settings(self):
        with tempfile.TemporaryDirectory() as out:
            build_variants.build_all(_csv(self, TB3MS_ROWS), out)
            self.assertEqual(sorted(os.listdir(out)), sorted(build_variants.VARIANTS))
            for name, (_, params) in build_variants.VARIANTS.items():
                with open(os.path.join(out, name, "main.py")) as f:
                    src = f.read()
                ast.parse(src)
                self.assertNotIn("RATES = __RATES__", src)
                for key, value in params.items():
                    self.assertIn("    {} = {!r}".format(key, value), src, name)


class BoostTemplateTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "boost/template.py", {})

    def test_boost_holds_98_percent_then_raises_to_the_boost_size_on_a_signal(self):
        algo = _algorithm(self.module, "BoostResearch")
        _day(algo)
        algo.rsi2 = _Indicator(5.0)
        _day(algo)
        self.assertEqual(algo.orders, [{"SPY": 0.98}, {"SPY": 1.47}])
        self.assertEqual(algo.episodes, 1)

    def test_the_boost_ends_on_a_close_above_the_exit_sma(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.rsi2 = _Indicator(5.0)
        _day(algo)
        algo.rsi2 = _Indicator(50.0)
        _day(algo, 102.0)
        self.assertEqual(algo.orders, [{"SPY": 1.47}, {"SPY": 0.98}])

    def test_const_rebalances_to_its_leverage_once_a_month(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.MODE, algo.CONST_LEV = "const", 1.1
        _day(algo, when=_dt.datetime(1992, 1, 15))
        _day(algo, when=_dt.datetime(1992, 1, 16))
        _day(algo, when=_dt.datetime(1992, 2, 3))
        self.assertEqual(algo.orders, [{"SPY": 1.078}, {"SPY": 1.078}])

    def test_borrowing_pays_the_months_rate_plus_the_spread(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.holdings["SPY"] = 1_470_000.0
        algo.last_want = 1.47
        algo.sig = True
        _day(algo, when=_dt.datetime(1992, 1, 15))
        self.assertAlmostEqual(algo.financing, 470_000.0 * (0.03 + 0.015) / 252.0)

    def test_a_month_missing_from_the_table_uses_the_latest_earlier_rate(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.holdings["SPY"] = 1_470_000.0
        algo.last_want = 1.47
        algo.sig = True
        _day(algo, when=_dt.datetime(1992, 2, 14))
        self.assertAlmostEqual(algo.financing, 470_000.0 * (0.03 + 0.015) / 252.0)


class MixTemplateTest(unittest.TestCase):
    def test_monthly_weights_then_the_boost_moves_only_the_spy_sleeve(self):
        module = _load_template(self, "boost/mix_template.py", {"MODE": "mix", "BOOST": True,
                                                                 "WEIGHTS": {"SPY": 1.0, "DBMF": 0.5}})
        algo = _algorithm(module, "BoostResearch", names=("SPY", "DBMF"))
        _day(algo)
        algo.rsi2 = _Indicator(5.0)
        _day(algo, when=_dt.datetime(1992, 1, 16))
        self.assertEqual(algo.orders, [{"SPY": 0.98, "DBMF": 0.49}, {"SPY": 1.47}])

    def test_nothing_is_bought_until_every_fund_has_a_price(self):
        module = _load_template(self, "boost/mix_template.py", {"MODE": "mix", "WEIGHTS": {"SPY": 0.8, "DBMF": 0.2}})
        algo = _algorithm(module, "BoostResearch", names=("SPY", "DBMF"))
        algo.Securities["DBMF"].Price = 0.0
        _day(algo)
        self.assertEqual(algo.orders, [])


class MixStatisticsTest(unittest.TestCase):
    def test_a_blend_reports_no_boosted_day_attribution(self):
        module = _load_template(self, "boost/mix_template.py", {"MODE": "mix", "BOOST": True,
                                                                 "WEIGHTS": {"SPY": 1.0, "DBMF": 0.5}})
        algo = _algorithm(module, "BoostResearch", names=("SPY", "DBMF"))
        stats = {}
        algo.SetRuntimeStatistic = stats.__setitem__
        algo.START, algo.END = (1992, 1, 1), (1992, 12, 31)
        _day(algo)
        algo.rsi2 = _Indicator(5.0)
        _day(algo, 100.5, when=_dt.datetime(1992, 1, 16))
        algo.OnEndOfAlgorithm()
        self.assertNotIn("Edge", stats)
        self.assertNotIn("boostDays", stats["Mode"])
        self.assertEqual(algo.orders[-1], {"SPY": 1.47})


class LongShortTest(unittest.TestCase):
    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "shorting/factors_ls.py", {"TOP_N": 3})

    def _algo(self, mode):
        algo = self.module.FactorResearch()
        algo.MODE, algo.TOP_N = mode, 3
        algo.resizes = 0
        algo.orders = []
        return algo

    def test_market_neutral_is_49_percent_long_the_best_and_49_short_the_worst(self):
        targets = self._algo("mom_ls")._targets(list("abcdefg"))
        self.assertEqual(sorted(targets), list("abcefg"))
        self.assertAlmostEqual(sum(w for w in targets.values() if w > 0), 0.49)
        self.assertAlmostEqual(sum(w for w in targets.values() if w < 0), -0.49)
        self.assertTrue(all(targets[s] < 0 for s in "efg"))

    def test_130_30_holds_its_stated_sides(self):
        targets = self._algo("mom_130")._targets(list("abcdef"))
        self.assertAlmostEqual(sum(w for w in targets.values() if w > 0), 1.27)
        self.assertAlmostEqual(sum(w for w in targets.values() if w < 0), -0.29)

    def test_too_few_names_for_two_separate_lists_keeps_last_months_book(self):
        self.assertIsNone(self._algo("mom_ls")._targets(list("abcde")))

    def test_long_only_momentum_is_equal_weight_top_n(self):
        self.assertEqual(self._algo("mom")._targets(list("abcde")), dict.fromkeys("abc", 0.98 / 3))

    def test_trading_resizes_only_beyond_the_band_and_flips_or_drops_the_rest(self):
        algo = self._algo("mom_ls")
        held = {"a": 0.10, "b": 0.12, "c": -0.10, "x": 0.05}      # fractions of equity

        class Book(dict):
            TotalPortfolioValue = 1_000_000.0

            def __iter__(self):
                return iter([types.SimpleNamespace(Key=k, Value=self[k]) for k in held])

        book = Book({k: types.SimpleNamespace(Invested=True, IsLong=v > 0, IsShort=v < 0,
                                              HoldingsValue=v * 1_000_000.0) for k, v in held.items()})
        book["d"] = types.SimpleNamespace(Invested=False, IsLong=False, IsShort=False, HoldingsValue=0.0)
        algo.Portfolio = book
        algo.SetHoldings = lambda s, w: algo.orders.append((s, w))
        algo.Liquidate = lambda s: algo.orders.append((s, 0))
        # a: within 25% of 0.11, kept; b: 0.12 vs 0.09 target, resized;
        # c: short now wanted long, flipped; d: new; x: not a target, sold.
        algo._trade({"a": 0.11, "b": 0.09, "c": 0.05, "d": -0.05})
        self.assertEqual(sorted(algo.orders), [("b", 0.09), ("c", 0.05), ("d", -0.05), ("x", 0)])
        self.assertEqual(algo.resizes, 1)


class TrendTemplateTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "shorting/trend_template.py", {"MODE": "trend_short"})

    def test_short_below_the_trend_long_above_and_the_borrow_fee_is_charged(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.trend = _Indicator(110.0)
        _day(algo)
        self.assertEqual(algo.orders, [{"SPY": -0.98}])
        self.assertAlmostEqual(algo.financing, 980_000.0 * algo.SHORT_FEE / 252.0)
        algo.trend = _Indicator(90.0)
        _day(algo)
        self.assertEqual(algo.orders[-1], {"SPY": 0.98})

    def test_trend_cash_goes_flat_below_the_trend(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.MODE = "trend_cash"
        algo.trend = _Indicator(110.0)
        _day(algo)
        self.assertEqual(algo.orders, [{"SPY": 0.0}])


class CarryTest(unittest.TestCase):
    Row = namedtuple("Row", ["date", "rev", "non"])

    def _carry(self):
        sys.path.insert(0, os.path.join(HERE, "futures"))
        self.addCleanup(sys.path.remove, os.path.join(HERE, "futures"))
        spec = importlib.util.spec_from_file_location("carry_under_test", os.path.join(HERE, "futures", "carry.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_a_roll_into_a_cheaper_contract_is_positive_annualised_carry(self):
        d = _dt.date(2000, 1, 3)
        rows = [self.Row(d, 100.0, 100.0),
                self.Row(d + _dt.timedelta(1), 101.0, 100.5),     # first roll: starts the clock only
                self.Row(d + _dt.timedelta(2), 101.0, 100.5),
                # Old contract last settled 100.5, the new one 98.5 and unchanged today: _REV
                # moves by the new contract's change (none), _NON jumps to the new contract.
                self.Row(d + _dt.timedelta(92), 101.0, 98.5)]
        (when, value), = self._carry().carry_series(rows)
        self.assertEqual(when, d + _dt.timedelta(92))
        self.assertAlmostEqual(value, (100.5 - 98.5) / 98.5 * 365.0 / 91)

    def test_rolls_closer_than_20_days_give_no_reading(self):
        d = _dt.date(2000, 1, 3)
        rows = [self.Row(d, 100.0, 100.0), self.Row(d + _dt.timedelta(1), 100.0, 101.0),
                self.Row(d + _dt.timedelta(10), 100.0, 99.0)]
        self.assertEqual(self._carry().carry_series(rows), [])


if __name__ == "__main__":
    unittest.main()
