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
    stub.ConstantSlippageModel = lambda fraction: ("slippage", fraction)
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

    def test_a_second_placeholder_is_refused(self):
        with self.assertRaises(SystemExit):
            build_variants.render("# RATES = __RATES__\nRATES = __RATES__\n", {}, {199201: 0.04})

    def test_vol_target_is_refused_where_it_would_be_ignored(self):
        with self.assertRaises(SystemExit):
            build_variants.check_variant("momentum/defensive.py", {"MODE": "mom", "VOL_TARGET": 0.2})
        build_variants.check_variant("momentum/momentum.py", {"MODE": "mom", "VOL_TARGET": 0.2})

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


class LongShortCostsTest(unittest.TestCase):
    def test_slippage_is_set_on_every_added_stock_only_when_configured(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        module = _load_template(self, "shorting/factors_ls.py", {"SLIPPAGE": 0.002})
        algo = module.FactorResearch()
        added = [types.SimpleNamespace(model=None) for _ in range(2)]
        for sec in added:
            sec.SetSlippageModel = lambda m, sec=sec: setattr(sec, "model", m)
        algo.OnSecuritiesChanged(types.SimpleNamespace(AddedSecurities=added))
        self.assertEqual([sec.model for sec in added], [("slippage", 0.002)] * 2)
        algo.SLIPPAGE = 0.0
        fresh = types.SimpleNamespace(model=None)
        fresh.SetSlippageModel = lambda m: setattr(fresh, "model", m)
        algo.OnSecuritiesChanged(types.SimpleNamespace(AddedSecurities=[fresh]))
        self.assertIsNone(fresh.model)


class _Iloc:
    """Just enough of a DataFrame's .iloc[-n:].ffill() for _vol_scaled."""

    def __init__(self, data):
        self.data = data

    def __getitem__(self, sl):
        cut = {k: v[sl] for k, v in self.data.items()}
        return types.SimpleNamespace(ffill=lambda: cut)


class LeveragedFundBoostTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "boost/lev_etf_template.py", {"MODE": "boost_etf"})

    def _algo(self, factor=2.0):
        algo = _algorithm(self.module, "BoostResearch", names=("SPY", "SSO"))
        algo.lev, algo.LEV_FACTOR = "SSO", factor
        return algo

    def test_a_signal_swaps_spy_into_the_fund_for_the_same_147_percent_exposure(self):
        algo = self._algo()
        _day(algo)
        algo.rsi2 = _Indicator(5.0)
        _day(algo)
        self.assertEqual(algo.orders, [{"SSO": 0.0, "SPY": 0.98}, {"SSO": 0.49, "SPY": 0.49}])
        self.assertAlmostEqual(algo.prev_w, 0.49 + 2 * 0.49)

    def test_a_3x_fund_needs_a_quarter_of_the_account(self):
        algo = self._algo(3.0)
        algo.rsi2 = _Indicator(5.0)
        _day(algo)
        self.assertEqual(algo.orders, [{"SSO": 0.245, "SPY": 0.735}])

    def test_nothing_is_borrowed_so_no_margin_interest_is_charged(self):
        algo = self._algo()
        algo.rsi2 = _Indicator(5.0)
        _day(algo)
        _day(algo, when=_dt.datetime(1992, 1, 16))
        self.assertEqual(algo.financing, 0.0)


class MomentumRobustnessTest(unittest.TestCase):
    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/momentum.py", {"TOP_N": 3})

    def test_book_scale_caps_at_one_and_scales_a_volatile_book_down(self):
        calm = {"a": [100.0, 100.1, 100.0, 100.1, 100.0]}
        self.assertEqual(self.module.book_scale(calm, {"a": 1.0}, 0.20), 1.0)
        wild = {"a": [100.0, 110.0, 99.0, 108.9, 98.01]}          # +10%, -10% each day
        scale = self.module.book_scale(wild, {"a": 1.0}, 0.20)
        self.assertAlmostEqual(scale, 0.20 / (0.10 * 252 ** 0.5))

    def test_book_scale_nets_longs_against_shorts_and_skips_missing_prices(self):
        nan = float("nan")
        prices = {"a": [100.0, 110.0, 99.0], "b": [100.0, 110.0, 99.0], "c": [nan, nan, 50.0]}
        # a long and b short of the same moves cancel; c has no returns yet.
        self.assertEqual(self.module.book_scale(prices, {"a": 0.5, "b": -0.5, "c": 0.3}, 0.20), 1.0)

    def _trading_algo(self, held):
        algo = self.module.MomentumResearch()
        algo.MODE, algo.TOP_N, algo.REBALANCE_BAND, algo.resizes = "mom", 3, 0.25, 0
        algo.applied_scale, algo.resize_all, algo.scales = 1.0, False, []
        orders = []

        class Book(dict):
            TotalPortfolioValue = 1_000_000.0

            def __iter__(self):
                return iter([types.SimpleNamespace(Key=k, Value=v) for k, v in dict.items(self)])

        algo.Portfolio = Book({k: types.SimpleNamespace(Invested=True, IsLong=True, IsShort=False,
                                                        HoldingsValue=v * 1_000_000.0) for k, v in held.items()})
        algo.SetHoldings = lambda sym, w: orders.append((sym, round(w, 6)))
        algo.Liquidate = lambda sym: orders.append((sym, 0))
        return algo, orders

    def _scale_to(self, algo, target_vol):
        wild = {"a": [100.0, 110.0, 99.0, 108.9, 98.01]}         # 10% daily moves
        algo.VOL_TARGET, algo.VOL_DAYS = target_vol, 4

        class Closes:
            columns = ["a"]

            def __getitem__(self, names):
                return types.SimpleNamespace(iloc=_Iloc({n: wild[n] for n in names}))

        return algo._vol_scaled({"a": 0.98}, Closes())

    def test_a_scale_cut_of_10_percent_or_more_resizes_every_holding(self):
        algo, orders = self._trading_algo({"a": 0.98})
        targets = self._scale_to(algo, 0.20 * 0.8 * 0.10 * 252 ** 0.5 / 0.20)  # a scale of 0.8
        self.assertTrue(algo.resize_all)
        algo._trade(targets)
        # The book (98% in a) is scaled to 0.8 x its volatility: an 18% cut, inside
        # the 25% band, and still traded because the scale moved by more than 10%.
        self.assertEqual(orders, [("a", 0.8)])
        self.assertFalse(algo.resize_all)
        self.assertAlmostEqual(algo.applied_scale, 0.8 / 0.98)

    def test_a_small_scale_change_leaves_the_band_in_charge(self):
        algo, orders = self._trading_algo({"a": 0.98})
        algo.applied_scale = 0.84
        self._scale_to(algo, 0.20 * 0.8 * 0.10 * 252 ** 0.5 / 0.20)   # 0.8 is under 10% from 0.84
        self.assertFalse(algo.resize_all)
        self.assertAlmostEqual(algo.applied_scale, 0.84)

    def test_without_a_band_a_drifted_holding_is_left_alone(self):
        algo = self.module.MomentumResearch()
        algo.MODE, algo.TOP_N, algo.REBALANCE_BAND, algo.resizes = "mom", 3, None, 0
        algo.resize_all = False
        orders = []

        class Book(dict):
            TotalPortfolioValue = 1_000_000.0

            def __iter__(self):
                return iter([types.SimpleNamespace(Key=k, Value=v) for k, v in dict.items(self)])

        algo.Portfolio = Book(a=types.SimpleNamespace(Invested=True, IsLong=True, IsShort=False,
                                                      HoldingsValue=600_000.0))
        algo.SetHoldings = lambda sym, w: orders.append((sym, w))
        algo.Liquidate = lambda sym: orders.append((sym, 0))
        algo._trade({"a": 0.33})
        self.assertEqual(orders, [])


class DefensiveBookTest(unittest.TestCase):
    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.book = _load_template(self, "momentum/defensive.py", {}).defensive_book

    def test_all_switches_off_is_equal_weight_top_n(self):
        self.assertEqual(self.book(list("abcde"), 3), dict.fromkeys("abc", 0.98 / 3))

    def test_a_sector_cap_skips_to_the_next_name_from_another_sector(self):
        sector = {"a": 1, "b": 1, "c": 1, "d": 2, "e": None}
        targets = self.book(list("abcde"), 3, sector=sector, sector_cap=2)
        self.assertEqual(sorted(targets), ["a", "b", "d"])

    def test_inverse_volatility_weights(self):
        targets = self.book(["a", "b"], 2, vol={"a": 0.01, "b": 0.02}, inv_vol=True)
        self.assertAlmostEqual(targets["a"], 0.98 * 2 / 3)
        self.assertAlmostEqual(targets["b"], 0.98 / 3)

    def test_a_pick_below_its_own_average_holds_bonds_in_its_slot(self):
        targets = self.book(list("abc"), 2, above={"a": True, "b": False, "c": True})
        self.assertEqual(sorted(targets), ["IEF", "a"])
        self.assertAlmostEqual(targets["IEF"], 0.49)

    def test_half_risk_off_and_a_spy_blend(self):
        targets = self.book(list("ab"), 2, risk_off=True, risk_off_fraction=0.5, spy_blend=0.5)
        self.assertAlmostEqual(targets["SPY"], 0.49)
        self.assertAlmostEqual(targets["IEF"], 0.245)
        self.assertAlmostEqual(targets["a"] + targets["b"], 0.245)
        self.assertAlmostEqual(sum(targets.values()), 0.98)

    def test_full_risk_off_is_all_bonds(self):
        self.assertEqual(self.book(list("ab"), 2, risk_off=True), {"IEF": 0.98})


class AlternativeScoresTest(unittest.TestCase):
    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/scores.py", {})

    def test_a_steady_rise_is_less_discrete_than_a_jump(self):
        steady = [100.0 + i for i in range(11)]                 # up every day
        jumpy = [100.0] + [99.9 - 0.1 * i for i in range(9)] + [110.0]   # down 9 days, one big jump
        d = self.module.info_discreteness
        self.assertAlmostEqual(d(steady), -1.0)
        self.assertGreater(d(jumpy), d(steady))

    def test_residual_score_removes_the_market_part(self):
        moves = [0.01 if i % 3 else -0.012 for i in range(29)]
        market, follower = [100.0], [50.0]
        for m in moves:
            market.append(market[-1] * (1 + m))
            follower.append(follower[-1] * (1 + 2 * m))           # exactly 2x the market's daily return
        self.assertIsNone(self.module.residual_score(follower, market))   # no residual left
        drift = [m * (1.0 + 0.002 * i + (0.001 if i % 2 else -0.001)) for i, m in enumerate(market)]
        self.assertGreater(self.module.residual_score(drift, market), 0)

    def test_returns_pair_the_same_sessions_when_a_stock_has_a_gap(self):
        nan = float("nan")
        rows = [(10.0, 100.0), (nan, 101.0), (11.0, 102.0), (12.0, 103.0), (13.0, 104.0)]
        stock, market = self.module.paired_window(rows, skip=1, min_rows=3)
        self.assertEqual((stock, market), ([10.0, 11.0, 12.0], [100.0, 102.0, 103.0]))
        self.assertEqual(self.module.paired_window(rows, skip=1, min_rows=5), ([], []))

    def test_too_short_a_window_has_no_score(self):
        self.assertIsNone(self.module.residual_score([1.0, 1.1], [1.0, 1.1]))


class EtfRotationTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "etf/rotation.py", {})

    def test_top_picks_equal_weight_and_a_failed_trend_check_holds_bonds(self):
        rot = self.module.rotation_targets
        scores = {"XLK": 0.3, "XLE": 0.2, "XLV": 0.1, "XLU": -0.1}
        self.assertEqual(rot(scores, 2), {"XLK": 0.49, "XLE": 0.49})
        targets = rot(scores, 3, above={"XLK": True, "XLE": False, "XLV": False})
        self.assertAlmostEqual(targets["XLK"], 0.98 / 3)
        self.assertAlmostEqual(targets["IEF"], 2 * 0.98 / 3)


class LeveragedTrendTest(unittest.TestCase):
    def setUp(self):
        self.act = _load_template(self, "etf/rotation.py", {}).lev_trend_action

    def test_a_signal_change_trades(self):
        self.assertEqual(self.act(True, False, (2000, 1), None, True, True), "on")
        self.assertEqual(self.act(False, True, (2000, 1), (2000, 1), True, False), "off")

    def test_a_lasting_uptrend_resets_leverage_once_a_month(self):
        self.assertIsNone(self.act(True, True, (2000, 1), (2000, 1), True, False))
        self.assertEqual(self.act(True, True, (2000, 2), (2000, 1), True, False), "on")

    def test_risk_off_retries_the_bond_fund_once_it_trades(self):
        self.assertIsNone(self.act(False, False, (2000, 1), None, False, False))     # no IEF yet
        self.assertEqual(self.act(False, False, (2002, 8), None, True, False), "off")
        self.assertIsNone(self.act(False, False, (2002, 9), None, True, True))


class UnpricedBuyTest(unittest.TestCase):
    """momentum/defensive_v2.py: a target with no price yet waits, then is bought."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/defensive_v2.py", {})

    def _algo(self, priced):
        algo = self.module.MomentumResearch()
        algo.MODE, algo.TOP_N, algo.REBALANCE_BAND = "mom", 2, 0.25
        algo.resizes, algo.resize_all, algo.retried = 0, False, 0
        algo.pending, algo.cash_sum, algo.cash_days = {}, 0.0, 0
        algo.BORROW_FEE = 0.0
        algo.spy = "SPY"
        algo.spy_curve, algo.equity_curve = [], []
        algo.Time = _dt.datetime(2000, 1, 3)
        orders = []

        class Securities(dict):
            def ContainsKey(self, k):
                return k in self

        algo.Securities = Securities({k: types.SimpleNamespace(HasData=v, Price=100.0 if v else 0.0)
                                      for k, v in priced.items()})

        class Book(dict):
            TotalPortfolioValue = 1_000_000.0
            Cash = 500_000.0

            def __iter__(self):
                return iter([])

            def __getitem__(self, k):
                return types.SimpleNamespace(Invested=False, IsLong=False, IsShort=False, HoldingsValue=0.0)

        algo.held, algo.open_orders = set(), set()

        class Book(dict):
            TotalPortfolioValue = 1_000_000.0
            Cash = 500_000.0

            def __iter__(self):
                return iter([])

            def __getitem__(self, k):
                return types.SimpleNamespace(Invested=k in algo.held, IsLong=k in algo.held, IsShort=False,
                                             HoldingsValue=0.0)

        algo.Portfolio = Book()
        algo.Transactions = types.SimpleNamespace(GetOpenOrders=lambda sym: [1] if sym in algo.open_orders else [])
        algo.SetHoldings = lambda sym, w: orders.append((sym, w))
        algo.Liquidate = lambda sym: orders.append((sym, 0))
        return algo, orders

    def _day(self, algo):
        algo.OnData(types.SimpleNamespace(Bars={"SPY": types.SimpleNamespace(Close=100.0)}))

    def test_an_unpriced_target_waits_until_priced_and_then_until_held(self):
        algo, orders = self._algo({"a": True, "b": False})
        algo._trade({"a": 0.49, "b": 0.49})
        self.assertEqual(orders, [("a", 0.49)])
        self.assertEqual(algo.pending, {"b": 0.49})
        algo.Securities["b"] = types.SimpleNamespace(HasData=True, Price=50.0)
        self._day(algo)                               # priced: submitted
        self.assertEqual(orders, [("a", 0.49), ("b", 0.49)])
        self.assertEqual(algo.pending, {"b": 0.49})
        algo.open_orders = {"b"}                      # working, not yet filled
        self._day(algo)                               # no duplicate
        self.assertEqual(len(orders), 2)
        algo.held = {"b"}
        self._day(algo)
        self.assertEqual((algo.pending, algo.retried), ({}, 1))
        self.assertAlmostEqual(algo.cash_sum, 1.5)

    def test_a_cancelled_retry_is_submitted_again(self):
        algo, orders = self._algo({"b": True})
        algo.pending = {"b": 0.49}
        algo.open_orders = set()                      # the earlier order died, nothing held
        self._day(algo)
        self._day(algo)
        self.assertEqual(orders, [("b", 0.49), ("b", 0.49)])
        self.assertEqual(algo.retried, 2)

    def test_a_new_rebalance_replaces_what_was_waiting(self):
        algo, orders = self._algo({"a": False, "c": True})
        algo._trade({"a": 0.98})
        algo._trade({"c": 0.98})
        self.assertEqual(algo.pending, {})
        self.assertEqual(orders, [("c", 0.98)])


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
