"""Tests for the follow-up research code (docs/research/boost-followup-2026-09.md):
the variant builder, the Boost / mix / trend templates' daily rules, and the
futures carry measure. The templates import QuantConnect's AlgorithmImports,
so, like timing/test_timing.py, these tests install a minimal stand-in with
just the names the templates use and an algorithm that records its orders.
Standard library only; synthetic values, no market data.
"""
import ast
import datetime as _dt
import difflib
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


def _class_constants(template, params):
    """{NAME: value} of the algorithm class's literal constants in the
    rendered template."""
    with open(os.path.join(HERE, template)) as f:
        src = build_variants.render(f.read(), params, {199201: 0.03})
    for node in ast.walk(ast.parse(src)):
        if isinstance(node, ast.ClassDef):
            found = {}
            for stmt in node.body:
                if (isinstance(stmt, ast.Assign) and len(stmt.targets) == 1
                        and isinstance(stmt.targets[0], ast.Name) and stmt.targets[0].id.isupper()):
                    found[stmt.targets[0].id] = ast.literal_eval(stmt.value)
            if found:
                return found
    raise AssertionError("no class constants in " + template)


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

    def test_the_fixed_130_30_runs_keep_the_original_settings_and_differ_only_in_the_template(self):
        # Compares the effective class constants of the rendered scripts, the
        # templates' defaults included, not just the overrides in VARIANTS.
        pairs = {"F_130": "SH_MOM_130", "OOS_F_130": "OOS_MOM_130",
                 "OOS_F_130_S10_B5": "OOS_130_S10_B5", "OOS_F_130_S20_B5": "OOS_130_S20_B5"}
        # The fixed template's own switches, all off for 130/30 (RISK_OFF_FRACTION
        # acts only in mom_trend, VOL_DAYS only with a VOL_TARGET).
        neutral = {"VOL_TARGET": None, "VOL_DAYS": 126, "RISK_OFF_FRACTION": 1.0, "STOCK_TREND_DAYS": 0,
                   "SECTOR_CAP": None, "SCORE": "ret", "SPY_BLEND": 0.0, "INV_VOL": False}
        for fixed, original in pairs.items():
            template, params = build_variants.VARIANTS[fixed]
            self.assertEqual(template, "momentum/defensive_v2.py", fixed)
            new = _class_constants(template, params)
            old = _class_constants(*build_variants.VARIANTS[original])
            self.assertEqual({k: new[k] for k in old}, old, fixed)
            self.assertEqual({k: v for k, v in new.items() if k not in old}, neutral, fixed)

    def test_the_execution_runs_keep_their_baselines_settings_plus_the_delay(self):
        pairs = {"XF_HALF_D3": "F_HALF", "XF_COMBO_D3": "F_COMBO",
                 "OOS_XF_HALF_D1": "OOS_F_HALF", "OOS_XF_HALF_D3": "OOS_F_HALF",
                 "OOS_XF_COMBO_D1": "OOS_F_COMBO", "OOS_XF_COMBO_D3": "OOS_F_COMBO"}
        for late, baseline in pairs.items():
            template, params = build_variants.VARIANTS[late]
            self.assertEqual(template, "momentum/execution.py", late)
            new = _class_constants(template, params)
            delay = new.pop("EXEC_DELAY")
            self.assertEqual(new, _class_constants(*build_variants.VARIANTS[baseline]), late)
            self.assertEqual(delay, int(late[-1]), late)


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
        algo.pending, algo.pending_tickets, algo.cash_sum, algo.cash_days = {}, {}, 0.0, 0
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
        algo.tickets, algo.shares = [], {}         # shares: symbol -> quantity held
        algo.target_shares = 100                     # what each target weight comes to

        def set_holdings(sym, w):
            # Like LEAN: order the difference between the target and what is held;
            # nothing to order returns no tickets.
            orders.append((sym, w))
            target = algo.target_shares if w >= 0 else -algo.target_shares
            missing = target - algo.shares.get(sym, 0)
            if missing == 0:
                return []
            ticket = types.SimpleNamespace(Quantity=missing, QuantityFilled=0)
            algo.tickets.append(ticket)
            return [ticket]

        def fill(sym, qty):
            algo.tickets[-1].QuantityFilled = qty
            algo.shares[sym] = algo.shares.get(sym, 0) + qty
            algo.held.add(sym)

        algo.fill = fill

        algo.SetHoldings = set_holdings
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
        algo.open_orders = set()
        algo.fill("b", 100)
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

    def test_a_partly_filled_retry_is_topped_up_after_the_rest_is_cancelled(self):
        algo, orders = self._algo({"b": True})
        algo.pending = {"b": 0.49}
        self._day(algo)                               # submitted for 100
        algo.fill("b", 40)                            # 40 filled, the rest cancelled
        self._day(algo)                               # held, but short of the target: again
        self.assertEqual(orders, [("b", 0.49), ("b", 0.49)])
        self.assertEqual(algo.tickets[-1].Quantity, 60)   # only the missing shares
        self.assertEqual(algo.pending, {"b": 0.49})
        algo.fill("b", 60)
        self._day(algo)
        self.assertEqual(algo.pending, {})

    def test_a_retry_that_places_no_order_stays_pending_until_held(self):
        algo, orders = self._algo({"b": True})
        algo.pending = {"b": 0.49}
        algo.target_shares = 0                        # the target rounds to no shares yet
        self._day(algo)
        self._day(algo)
        self.assertEqual(algo.pending, {"b": 0.49})   # not dropped: nothing was bought
        self.assertEqual(len(orders), 2)

    def test_an_unpriced_short_waits_until_priced(self):
        algo, orders = self._algo({"a": True, "z": False})
        algo._trade({"a": 1.27 / 25, "z": -0.29 / 25})
        self.assertEqual(orders, [("a", 1.27 / 25)])
        self.assertEqual(algo.pending, {"z": -0.29 / 25})
        algo.Securities["z"] = types.SimpleNamespace(HasData=True, Price=20.0)
        self._day(algo)                               # priced: shorted
        self.assertEqual(orders[-1], ("z", -0.29 / 25))
        self.assertEqual(algo.tickets[-1].Quantity, -100)
        algo.fill("z", -100)                          # the short fills in full
        self._day(algo)
        self._day(algo)
        self.assertEqual(algo.pending, {})
        self.assertEqual(len(orders), 2)              # no second short

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


class _Clock:
    """A Securities stand-in whose SPY price the test sets."""

    def __init__(self):
        self.price = 100.0

    def __getitem__(self, name):
        return types.SimpleNamespace(Price=self.price)


def _boost_execution(module, decisions=None, **settings):
    """boost/execution.py's algorithm built without Initialize(). With
    ``decisions``, each decision returns the next of those weights instead of
    applying the rule; orders are recorded as ("market", weight) or
    ("moc", shares)."""
    algo = module.BoostExecution()
    for name, value in settings.items():
        setattr(algo, name, value)
    algo.sym = "SPY"
    algo.rsi2 = module.Wilder(2)
    algo.trend = module.Average(algo.TREND_DAYS) if algo.TREND_DAYS else None
    algo.exit_sma = module.Average(algo.EXIT_SMA)
    algo._reset_state()
    algo.IsWarmingUp = False
    algo.Time = _dt.datetime(1992, 1, 15)
    algo.Securities = _Clock()
    algo.orders = []
    algo.held, algo.equity = 0.0, 1_000_000.0
    algo.SetHoldings = lambda sym, w: algo.orders.append(("market", w))
    algo.CalculateOrderQuantity = lambda sym, w: round(w * 1000) - round(algo.held / 100.0)
    algo.MarketOnCloseOrder = lambda sym, qty: algo.orders.append(("moc", qty))
    cash = _Cash()

    class Book(dict):
        CashBook = {"USD": cash}

        @property
        def TotalPortfolioValue(self):
            return algo.equity

        def __getitem__(self, name):
            return types.SimpleNamespace(HoldingsValue=algo.held)

    algo.Portfolio = Book()
    if decisions is not None:
        queue = list(decisions)
        algo._decide = lambda price: queue.pop(0)
    return algo


#: Closes rising 10 a day from 10 to 300: RSI(2) 100, far above every average.
_RISE = tuple(float(c) for c in range(10, 310, 10))


def _close(algo, close):
    algo.OnDailyBar(types.SimpleNamespace(Close=close))


def _wire_initialize(module, algo):
    """Stand-ins for what Initialize() calls; returns the list that records
    each scheduled event as (date rule, time rule, callback name) and the
    fee models set."""
    events, fees = [], []
    module.BrokerageName = types.SimpleNamespace(InteractiveBrokersBrokerage="IB")
    module.AccountType = types.SimpleNamespace(Margin="margin")
    module.Resolution = types.SimpleNamespace(Daily="daily", Minute="minute")
    module.ConstantFeeModel = lambda fee: ("fee", fee)
    for name in ("SetStartDate", "SetEndDate", "SetCash", "SetBrokerageModel", "AddUniverse", "SetWarmUp",
                 "Consolidate"):
        setattr(algo, name, lambda *a, **k: None)
    algo.Settings = types.SimpleNamespace()
    algo.UniverseSettings = types.SimpleNamespace()
    algo.AddEquity = lambda ticker, *a, **k: types.SimpleNamespace(
        Symbol=ticker, SetFeeModel=fees.append)
    algo.DateRules = types.SimpleNamespace(
        MonthStart=lambda sym, offset=0: ("month_start", sym, offset),
        EveryDay=lambda sym: ("every_day", sym))
    algo.TimeRules = types.SimpleNamespace(
        At=lambda h, m: ("at", h, m),
        AfterMarketOpen=lambda sym, minutes: ("after_open", minutes),
        BeforeMarketClose=lambda sym, minutes: ("before_close", minutes))
    algo.Schedule = types.SimpleNamespace(
        On=lambda date_rule, time_rule, callback: events.append((date_rule, time_rule, callback.__name__)))
    return events, fees


class BoostExecutionTest(unittest.TestCase):
    """boost/execution.py: the rule of boost/template.py, decided and filled
    at M1's or Interactive Brokers' times."""

    def setUp(self):
        self.module = _load_template(self, "boost/execution.py", {})


    def _schedule(self, **settings):
        algo = self.module.BoostExecution()
        for name, value in settings.items():
            setattr(algo, name, value)
        events, fees = _wire_initialize(self.module, algo)
        algo.Initialize()
        return events, fees

    def test_each_window_is_scheduled_at_its_time(self):
        day = ("every_day", "SPY")
        self.assertEqual(self._schedule(EXEC="next_open")[0], [(day, ("after_open", 1), "TradeDue")])
        self.assertEqual(self._schedule(EXEC="afternoon")[0],
                         [(day, ("before_close", 70), "DecideIntraday"), (day, ("before_close", 60), "TradeDue")])
        self.assertEqual(self._schedule(EXEC="moc")[0], [(day, ("before_close", 16), "DecideIntraday")])

    def test_m1_runs_charge_no_commission_and_ib_runs_keep_leans(self):
        self.assertEqual(self._schedule(FEES="none")[1], [("fee", 0)])
        self.assertEqual(self._schedule(FEES="ib")[1], [])

    def test_wilder_rsi_seeds_with_simple_averages_then_smooths(self):
        rsi = self.module.Wilder(2)
        for close in (10.0, 11.0, 10.5):
            rsi.update(close)
        # Changes +1, -0.5 seed gain 0.5, loss 0.25; then +1.5 gives gain
        # (0.5 + 1.5) / 2 = 1.0 and loss 0.25 / 2 = 0.125: RS 8.
        self.assertAlmostEqual(rsi.peek(12.0), 100.0 - 100.0 / 9.0)
        self.assertAlmostEqual(rsi.peek(12.0), 100.0 - 100.0 / 9.0)   # peeking stores nothing
        rsi.update(12.0)
        self.assertAlmostEqual(rsi._value(rsi.gain, rsi.loss), 100.0 - 100.0 / 9.0)

    def test_an_rsi_with_no_losses_is_100_and_none_before_it_is_seeded(self):
        rsi = self.module.Wilder(2)
        self.assertIsNone(rsi.peek(1.0))
        rsi.update(10.0)
        self.assertIsNone(rsi.peek(11.0))
        rsi.update(11.0)
        self.assertEqual(rsi.peek(12.0), 100.0)

    def test_the_average_counts_the_provisional_close_with_the_last_closes(self):
        avg = self.module.Average(3)
        avg.update(1.0)
        self.assertIsNone(avg.peek(10.0))
        for close in (2.0, 3.0, 4.0):
            avg.update(close)
        self.assertAlmostEqual(avg.peek(10.0), (3.0 + 4.0 + 10.0) / 3)

    def test_the_rule_enters_on_a_low_rsi_above_the_trend_and_leaves_above_the_exit_average(self):
        algo = _boost_execution(self.module, TREND_DAYS=20)
        for close in _RISE:
            _close(algo, close)
        self.assertAlmostEqual(algo.queue[-1], 0.98)
        _close(algo, 290.0)      # RSI 50: no entry
        _close(algo, 280.0)      # RSI 25: no entry
        self.assertAlmostEqual(algo.queue[-1], 0.98)
        _close(algo, 260.0)      # RSI 8.3, well above the 20-day average: boost
        self.assertAlmostEqual(algo.queue[-1], 0.98 * 1.5)
        self.assertEqual(algo.episodes, 1)
        _close(algo, 255.0)      # below the 5-day average (277): still boosted
        self.assertAlmostEqual(algo.queue[-1], 0.98 * 1.5)
        _close(algo, 300.0)      # above it: back to 98%
        self.assertAlmostEqual(algo.queue[-1], 0.98)

    def test_no_entry_below_the_trend_average(self):
        algo = _boost_execution(self.module, TREND_DAYS=3)
        for close in (120.0, 110.0, 100.0, 90.0):
            _close(algo, close)
        self.assertTrue(all(w == 0.98 for w in algo.queue))
        self.assertEqual(algo.episodes, 0)

    def test_a_decision_on_a_provisional_close_matches_one_on_that_final_close(self):
        on_close = _boost_execution(self.module, TREND_DAYS=20)
        intraday = _boost_execution(self.module, TREND_DAYS=20, EXEC="moc")
        for close in _RISE + (290.0, 280.0):
            _close(on_close, close)
            _close(intraday, close)
        _close(on_close, 260.0)
        intraday.Securities.price = 260.0
        intraday.DecideIntraday()
        self.assertEqual(intraday.orders, [("moc", round(0.98 * 1.5 * 1000))])
        self.assertAlmostEqual(on_close.queue[-1], 0.98 * 1.5)

    def test_next_open_trades_a_close_decision_at_the_next_morning(self):
        algo = _boost_execution(self.module, decisions=[1.47])
        _close(algo, 100.0)
        self.assertEqual(algo.orders, [])
        algo.TradeDue()
        self.assertEqual(algo.orders, [("market", 1.47)])

    def test_a_delay_holds_each_decision_one_more_session(self):
        algo = _boost_execution(self.module, decisions=[1.47, 0.98], EXEC_DELAY=1)
        _close(algo, 100.0)
        algo.TradeDue()
        self.assertEqual(algo.orders, [])
        _close(algo, 100.0)
        algo.TradeDue()
        self.assertEqual(algo.orders, [("market", 1.47)])
        algo.TradeDue()
        self.assertEqual(algo.orders, [("market", 1.47)])   # the second waits for the next session's decision

    def test_the_afternoon_window_trades_the_same_days_decision(self):
        algo = _boost_execution(self.module, decisions=[1.47], EXEC="afternoon")
        algo.DecideIntraday()
        self.assertEqual(algo.orders, [])
        algo.TradeDue()
        self.assertEqual(algo.orders, [("market", 1.47)])

    def test_moc_sends_the_share_difference_to_the_close(self):
        algo = _boost_execution(self.module, decisions=[1.47, 1.47, 0.98], EXEC="moc")
        algo.held = 98_000.0                     # 980 shares at 100
        algo.DecideIntraday()
        self.assertEqual(algo.orders, [("moc", 1470 - 980)])
        algo.held = 147_000.0
        algo.DecideIntraday()                    # unchanged: no order
        algo.DecideIntraday()
        self.assertEqual(algo.orders, [("moc", 490), ("moc", 980 - 1470)])

    def test_moc_with_a_delay_sends_the_previous_sessions_decision(self):
        algo = _boost_execution(self.module, decisions=[1.47, 0.98], EXEC="moc", EXEC_DELAY=1)
        algo.DecideIntraday()
        self.assertEqual(algo.orders, [])
        algo.DecideIntraday()
        self.assertEqual(algo.orders, [("moc", 1470)])

    def test_nothing_is_decided_or_traded_while_warming_up(self):
        algo = _boost_execution(self.module, decisions=[1.47], EXEC="moc")
        algo.IsWarmingUp = True
        algo.DecideIntraday()
        _close(algo, 100.0)
        algo.TradeDue()
        self.assertEqual((algo.orders, algo.decisions, algo.curve), ([], 0, []))
        self.assertEqual(algo.exit_sma.peek(100.0), None)
        self.assertEqual(list(algo.exit_sma.closes), [100.0])   # the indicators still warm up

    def test_buy_and_hold_rebalances_to_98_percent_once_a_month(self):
        algo = _boost_execution(self.module, MODE="bh")
        for day in (_dt.datetime(1992, 1, 15), _dt.datetime(1992, 1, 16), _dt.datetime(1992, 2, 3)):
            algo.Time = day
            _close(algo, 100.0)
            algo.TradeDue()
        self.assertEqual(algo.orders, [("market", 0.98), ("market", 0.98)])

    def test_borrowing_pays_the_months_rate_plus_the_spread(self):
        algo = _boost_execution(self.module, decisions=[1.47], SPREAD=0.022)
        algo.held = 1_470_000.0
        _close(algo, 100.0)
        self.assertAlmostEqual(algo.financing, 470_000.0 * (0.03 + 0.022) / 252.0)
        self.assertAlmostEqual(algo.Portfolio.CashBook["USD"].added, -algo.financing)


class MomentumExecutionTest(unittest.TestCase):
    """momentum/execution.py: the month's book traded EXEC_DELAY sessions late."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/execution.py", {})

    def _algo(self, delay):
        algo = self.module.MomentumResearch()
        algo.EXEC_DELAY, algo.queued, algo.queued_day, algo.wait, algo.late_trades = delay, None, None, 0, 0
        algo.Time = _dt.datetime(2000, 2, 1, 8, 0)
        algo.traded = []
        algo._trade = algo.traded.append
        return algo

    def _session(self, algo, day):
        algo.Time = _dt.datetime(2000, 2, day, 8, 0)
        algo.TradeQueued()

    def test_without_a_delay_the_book_trades_when_computed(self):
        algo = self._algo(0)
        algo._submit({"A": 0.49})
        self.assertEqual(algo.traded, [{"A": 0.49}])

    def test_with_a_delay_the_book_trades_on_the_third_later_session_once(self):
        algo = self._algo(3)
        algo._submit({"A": 0.49})
        algo.TradeQueued()               # the same session, either side of the computation
        self._session(algo, 2)
        self._session(algo, 3)
        self.assertEqual(algo.traded, [])
        self._session(algo, 4)
        self._session(algo, 7)
        self.assertEqual(algo.traded, [{"A": 0.49}])
        self.assertEqual(algo.late_trades, 1)

    def test_the_count_does_not_depend_on_which_8am_event_runs_first(self):
        early = self._algo(1)
        early.TradeQueued()              # the daily event before the month's computation
        early._submit({"A": 0.49})
        late = self._algo(1)
        late._submit({"A": 0.49})
        late.TradeQueued()               # after it
        for algo in (early, late):
            self.assertEqual(algo.traded, [])
            self._session(algo, 2)
            self.assertEqual(algo.traded, [{"A": 0.49}])

    def test_the_delay_adds_one_daily_event_and_keeps_the_month_start_one(self):
        for delay, extra in ((0, []), (1, [(("every_day", "SPY"), ("at", 8, 0), "TradeQueued")]),
                             (3, [(("every_day", "SPY"), ("at", 8, 0), "TradeQueued")])):
            algo = self.module.MomentumResearch()
            algo.EXEC_DELAY = delay
            events, _ = _wire_initialize(self.module, algo)
            algo.Initialize()
            self.assertEqual(events, [(("month_start", "SPY", 0), ("at", 8, 0), "Rebalance")] + extra, delay)

    def test_the_template_only_adds_to_defensive_v2(self):
        # Every code line of defensive_v2.py is kept, in order; only comment
        # lines may change. With EXEC_DELAY 0 the added branch passes the book
        # straight to _trade, as defensive_v2 does.
        with open(os.path.join(HERE, "momentum", "defensive_v2.py")) as f:
            old = f.read().splitlines()
        with open(os.path.join(HERE, "momentum", "execution.py")) as f:
            new = f.read().splitlines()
        matcher = difflib.SequenceMatcher(a=old, b=new, autojunk=False)
        removed = [line for tag, i1, i2, _, _ in matcher.get_opcodes() if tag in ("replace", "delete")
                   for line in old[i1:i2]]
        self.assertEqual([line for line in removed if not line.lstrip().startswith("#")], [])


def _multi(module, sleeves, mode="boost"):
    """A BoostMulti built without Initialize(): every fund priced at 100,
    above its 200-day SMA (90), RSI(2) neutral (50), below its 5-day SMA
    (101), every holding flat."""
    algo = module.BoostMulti()
    algo.SLEEVES, algo.MODE = sleeves, mode
    algo.IsWarmingUp = False
    algo.Time = _dt.datetime(1992, 1, 15)
    algo.syms = {n: n for n in sleeves}
    algo.ind = {n: (_Indicator(90.0), _Indicator(50.0), _Indicator(101.0)) for n in sleeves}
    algo.Securities = {n: types.SimpleNamespace(Price=100.0) for n in sleeves}
    algo.holdings = {n: 0.0 for n in sleeves}
    algo.equity = 1_000_000.0
    cash = _Cash()
    algo.orders = []

    class _Book(dict):
        def __getitem__(self, name):
            return types.SimpleNamespace(HoldingsValue=algo.holdings[name])

    def refresh():
        book = _Book()
        book.TotalPortfolioValue = algo.equity
        book.TotalHoldingsValue = sum(algo.holdings.values())
        book.CashBook = {"USD": cash}
        algo.Portfolio = book

    def set_holdings(targets):
        algo.orders.append([(t.Symbol, round(t.Quantity, 6)) for t in targets])
        for t in targets:
            algo.holdings[t.Symbol] = t.Quantity * algo.equity
        refresh()

    algo.SetHoldings = set_holdings
    algo.refresh = refresh
    refresh()
    algo._reset_state()
    return algo


def _multi_day(algo, when=None, missing=(), **closes):
    if when:
        algo.Time = when
    algo.refresh()
    bars = {n: types.SimpleNamespace(Close=closes.get(n, 100.0)) for n in algo.SLEEVES if n not in missing}
    algo.OnData(types.SimpleNamespace(Bars=bars))


class MultiBoostRuleTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "boost/multi.py", {})

    def test_the_signal_enters_on_a_low_rsi_above_the_trend_and_leaves_above_the_exit_average(self):
        step = self.module.boost_step
        self.assertEqual(step(False, 100.0, 90.0, 5.0, 101.0, 10), (True, True))
        self.assertEqual(step(False, 100.0, 110.0, 5.0, 101.0, 10), (False, False))   # below its trend
        self.assertEqual(step(False, 100.0, 90.0, 15.0, 101.0, 10), (False, False))   # RSI not low
        self.assertEqual(step(False, 100.0, None, 5.0, 101.0, 10), (False, False))    # trend not ready
        self.assertEqual(step(True, 100.0, 90.0, 50.0, 101.0, 10), (True, False))     # held below the exit
        self.assertEqual(step(True, 102.0, 90.0, 50.0, 101.0, 10), (False, False))    # leaves above it
        self.assertEqual(step(True, 102.0, 90.0, 5.0, None, 10), (True, False))       # exit not ready

    def test_weights_are_relative_shares_of_98_percent_and_a_signal_scales_only_its_fund(self):
        targets = self.module.sleeve_targets({"SPY": 1, "MDY": 1, "IWM": 2}, {"MDY": True}, 1.5)
        self.assertAlmostEqual(targets["SPY"], 0.245)
        self.assertAlmostEqual(targets["MDY"], 0.3675)
        self.assertAlmostEqual(targets["IWM"], 0.49)

    def test_reductions_are_ordered_before_increases(self):
        ordered = self.module.sells_first({"A": 0.5, "B": 0.2, "C": 0.4}, {"A": 0.3, "B": 0.4, "C": 0.4})
        self.assertEqual([n for n, _ in ordered], ["B", "C", "A"])


class MultiBoostTradingTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "boost/multi.py", {})

    def test_the_month_starts_at_the_shares_then_only_a_fund_whose_signal_changed_trades(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1})
        _multi_day(algo)
        algo.ind["MDY"][1].Current.Value = 5.0
        _multi_day(algo, when=_dt.datetime(1992, 1, 16))
        algo.ind["MDY"][1].Current.Value = 50.0
        _multi_day(algo, when=_dt.datetime(1992, 1, 17), MDY=102.0)
        self.assertEqual(algo.orders, [[("MDY", 0.49), ("SPY", 0.49)], [("MDY", 0.735)], [("MDY", 0.49)]])
        self.assertEqual(algo.episodes, {"SPY": 0, "MDY": 1})

    def test_each_fund_follows_its_own_indicators(self):
        algo = _multi(self.module, {"SPY": 1, "IWM": 1})
        algo.ind["SPY"][1].Current.Value = 5.0
        algo.ind["IWM"][0].Current.Value = 120.0      # IWM below its trend: no boost
        algo.ind["IWM"][1].Current.Value = 5.0
        _multi_day(algo)
        self.assertEqual(algo.sigs, {"SPY": True, "IWM": False})
        self.assertEqual(algo.orders, [[("IWM", 0.49), ("SPY", 0.735)]])

    def test_a_new_month_resets_every_fund_keeping_a_boost_that_is_still_on(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1})
        algo.ind["SPY"][1].Current.Value = 5.0
        _multi_day(algo)
        algo.holdings["SPY"] = 800_000.0                 # drifted above its 0.735 target
        _multi_day(algo, when=_dt.datetime(1992, 2, 3))
        self.assertEqual(algo.orders[-1], [("SPY", 0.735), ("MDY", 0.49)])   # the sale first

    def test_nothing_is_bought_until_every_fund_has_a_price_and_warm_indicators(self):
        algo = _multi(self.module, {"SPY": 1, "EFA": 1})
        algo.Securities["EFA"].Price = 0.0
        _multi_day(algo)
        algo.Securities["EFA"].Price = 100.0
        algo.ind["EFA"][0].IsReady = False
        _multi_day(algo, when=_dt.datetime(1992, 1, 16))
        self.assertEqual(algo.orders, [])
        self.assertEqual(algo.curve, [])
        algo.ind["EFA"][0].IsReady = True
        _multi_day(algo, when=_dt.datetime(1992, 1, 17))
        self.assertEqual(algo.orders, [[("EFA", 0.49), ("SPY", 0.49)]])

    def test_a_signal_before_the_blend_can_trade_is_not_counted_as_an_episode(self):
        algo = _multi(self.module, {"SPY": 1, "EFA": 1})
        algo.ind["EFA"][0].IsReady = False
        algo.ind["SPY"][1].Current.Value = 5.0
        _multi_day(algo)
        self.assertEqual(algo.sigs["SPY"], True)              # the state still runs
        self.assertEqual(algo.episodes, {"SPY": 0, "EFA": 0})
        algo.ind["EFA"][0].IsReady = True
        _multi_day(algo, when=_dt.datetime(1992, 1, 16))
        self.assertEqual(algo.orders, [[("EFA", 0.49), ("SPY", 0.735)]])
        self.assertEqual(algo.episodes, {"SPY": 0, "EFA": 0})

    def test_the_monthly_reset_waits_for_a_bar_for_every_fund(self):
        for mode in ("boost", "bh"):
            algo = _multi(self.module, {"SPY": 1, "MDY": 1}, mode=mode)
            _multi_day(algo, missing=("MDY",))
            self.assertEqual(algo.orders, [], mode)
            self.assertEqual(len(algo.curve), 1, mode)        # the day is still booked
            _multi_day(algo, when=_dt.datetime(1992, 1, 16))
            self.assertEqual(algo.orders, [[("MDY", 0.49), ("SPY", 0.49)]], mode)

    def test_a_signal_change_trades_even_when_another_fund_has_no_bar(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1})
        _multi_day(algo)
        algo.ind["SPY"][1].Current.Value = 5.0
        _multi_day(algo, when=_dt.datetime(1992, 1, 16), missing=("MDY",))
        self.assertEqual(algo.orders[-1], [("SPY", 0.735)])

    def test_bh_holds_a_fund_whose_indicators_are_still_cold(self):
        # A fund listed after the start date has a price long before 200
        # bars: "bh" holds it at once, "boost" waits for the warm trend.
        for mode, expected in (("bh", [[("IEF", 0.98)]]), ("boost", [])):
            algo = _multi(self.module, {"IEF": 1}, mode=mode)
            algo.ind["IEF"][0].IsReady = False
            _multi_day(algo)
            self.assertEqual(algo.orders, expected, mode)

    def test_bh_never_boosts_and_trades_once_a_month(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1}, mode="bh")
        algo.ind["SPY"][1].Current.Value = 5.0
        _multi_day(algo)
        _multi_day(algo, when=_dt.datetime(1992, 1, 16))
        _multi_day(algo, when=_dt.datetime(1992, 2, 3))
        self.assertEqual(algo.orders, [[("MDY", 0.49), ("SPY", 0.49)]] * 2)
        self.assertEqual(algo.episodes, {"SPY": 0, "MDY": 0})

    def test_borrowing_across_funds_pays_the_months_rate_plus_the_spread(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1})
        algo.ind["SPY"][1].Current.Value = 5.0
        algo.ind["MDY"][1].Current.Value = 5.0
        _multi_day(algo)
        self.assertAlmostEqual(algo.financing, 470_000.0 * (0.03 + 0.015) / 252.0)
        self.assertAlmostEqual(algo.gross_max, 1.47)
        self.assertEqual(algo.boosted, {2: 1})

    def test_the_end_statistics_report_episodes_and_overlap(self):
        algo = _multi(self.module, {"SPY": 1, "MDY": 1})
        stats = {}
        algo.SetRuntimeStatistic = stats.__setitem__
        algo.ind["SPY"][1].Current.Value = 5.0
        _multi_day(algo)
        algo.OnEndOfAlgorithm()
        self.assertEqual(stats["Episodes"], "SPY=1 MDY=0")
        self.assertEqual(stats["Boosted"], "1:1")
        self.assertIn("SPY/MDY", stats["Mode"])

    def test_the_rule_settings_match_the_one_fund_boost(self):
        single = _class_constants("boost/template.py", {})
        multi = _class_constants("boost/multi.py", {})
        for name in ("BOOST_SIZE", "RSI_ENTRY", "TREND_DAYS", "EXIT_SMA", "SPREAD", "CASH"):
            self.assertEqual(multi[name], single[name], name)
        for variant, (template, params) in build_variants.VARIANTS.items():
            if template == "boost/multi.py":
                self.assertFalse(set(params) - {"SLEEVES", "MODE"}, variant)


class CountryRotationTest(unittest.TestCase):
    def setUp(self):
        self.module = _load_template(self, "etf/rotation.py", {})

    def test_the_country_universe_is_seventeen_distinct_funds_and_sectors_are_unchanged(self):
        countries = self.module.UNIVERSES["country_rot"]
        self.assertEqual(len(set(countries)), 17)
        self.assertNotIn("SPY", countries)
        self.assertEqual(self.module.UNIVERSES["sector_rot"], self.module.SECTORS)
        self.assertEqual(len(self.module.SECTORS), 9)

    def test_the_benchmark_variant_holds_every_country_equally(self):
        countries = self.module.COUNTRIES
        _, params = build_variants.VARIANTS["E_CTRY_EW"]
        self.assertEqual(params["TOP"], len(countries))
        targets = self.module.rotation_targets({c: i * 0.01 for i, c in enumerate(countries)}, params["TOP"])
        self.assertEqual(set(targets), set(countries))
        for w in targets.values():
            self.assertAlmostEqual(w, 0.98 / 17)

    def test_the_tuning_runs_end_with_the_tuning_years(self):
        for variant, (template, params) in build_variants.VARIANTS.items():
            if params.get("MODE") == "country_rot" and "_OOS_" not in variant:
                self.assertEqual(_class_constants(template, params)["END"], (2015, 12, 31), variant)

    def test_the_held_out_runs_measure_2016_on_with_a_year_of_closes_before(self):
        for variant in ("E_CTRY_OOS_T5_S10", "E_CTRY_OOS_EW"):
            template, params = build_variants.VARIANTS[variant]
            c = _class_constants(template, params)
            self.assertEqual((c["START"], c["MEASURE_FROM"], c["END"]), ((2015, 1, 1), (2016, 1, 1), (2026, 6, 30)))
        dates = ("START", "END", "MEASURE_FROM")
        for held, tuned in (("E_CTRY_OOS_T5_S10", "E_CTRY_T5_S10"), ("E_CTRY_OOS_EW", "E_CTRY_EW")):
            h = _class_constants(*build_variants.VARIANTS[held])
            t = _class_constants(*build_variants.VARIANTS[tuned])
            for name in dates:
                h.pop(name)
                t.pop(name)
            self.assertEqual(h, t, held)


class UniverseParityTest(unittest.TestCase):
    """momentum/universe_parity.py: the F_COMBO book picked from an ETF's
    holdings as they stood ETF_LAG_DAYS earlier, with each month's picks
    logged for the comparison (docs/live-trading/plan.md, section 4)."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/universe_parity.py", {})
        self.module.Universe = types.SimpleNamespace(Unchanged="UNCHANGED")

    def _algo(self, source="etf", lag=7):
        algo = self.module.MomentumResearch()
        algo.UNIVERSE_SOURCE, algo.ETF_LAG_DAYS, algo.UNIVERSE_SIZE = source, lag, 2
        algo.LOG_PICKS = False
        algo.last_month, algo.etf_snapshots, algo.no_holdings = None, [], 0
        algo.Time = _dt.datetime(2016, 2, 1)
        return algo

    @staticmethod
    def _row(symbol, dollar_volume, price=10.0, fundamentals=True):
        return types.SimpleNamespace(Symbol=symbol, DollarVolume=dollar_volume, Price=price,
                                     HasFundamentalData=fundamentals)

    def test_the_holdings_used_are_the_latest_at_least_the_lag_old(self):
        d = _dt.date
        snaps = [(d(2016, 1, 20), {"a"}), (d(2016, 1, 25), {"b"}), (d(2016, 1, 26), {"c"})]
        lagged = self.module.lagged_holdings
        self.assertEqual(lagged(snaps, d(2016, 2, 1), 7), {"b"})      # 1-25 is 7 days old; 1-26 too new
        self.assertEqual(lagged(snaps, d(2016, 2, 1), 1), {"c"})
        self.assertIsNone(lagged(snaps, d(2016, 1, 26), 7))            # nothing old enough yet
        self.assertIsNone(lagged([], d(2016, 2, 1), 7))

    def test_the_etf_universe_is_its_holdings_ranked_by_dollar_volume(self):
        algo = self._algo()
        algo.etf_snapshots = [(_dt.date(2016, 1, 20), {"a", "b", "c"})]
        rows = [self._row("a", 1.0), self._row("x", 99.0), self._row("b", 3.0), self._row("c", 2.0),
                self._row("p", 50.0, price=4.0)]
        self.assertEqual(algo.Coarse(rows), ["b", "c"])                # x is not held by the fund
        self.assertEqual(algo.Coarse(rows), "UNCHANGED")              # once a month

    def test_a_month_with_no_holdings_old_enough_waits_and_tries_again(self):
        algo = self._algo()
        algo.etf_snapshots = [(_dt.date(2016, 1, 28), {"a"})]
        rows = [self._row("a", 1.0)]
        self.assertEqual(algo.Coarse(rows), "UNCHANGED")
        self.assertEqual((algo.last_month, algo.no_holdings), (None, 1))
        algo.Time = _dt.datetime(2016, 2, 4)
        self.assertEqual(algo.Coarse(rows), ["a"])

    def test_the_coarse_source_is_the_backtests_own_universe(self):
        algo = self._algo(source="coarse")
        rows = [self._row("a", 1.0), self._row("x", 99.0), self._row("b", 3.0)]
        self.assertEqual(algo.Coarse(rows), ["x", "b"])

    def test_each_day_the_funds_holdings_are_recorded_and_nothing_is_subscribed(self):
        algo = self._algo()
        held = [types.SimpleNamespace(Symbol="a"), types.SimpleNamespace(Symbol="b")]
        self.assertEqual(algo.EtfHoldings(held), [])
        self.assertEqual(algo.etf_snapshots, [(_dt.date(2016, 2, 1), {"a", "b"})])
        algo.etf_snapshots.insert(0, (_dt.date(2015, 11, 1), {"old"}))
        algo.Time = _dt.datetime(2016, 2, 2)
        algo.EtfHoldings(held)
        self.assertEqual([day for day, _ in algo.etf_snapshots], [_dt.date(2016, 2, 1), _dt.date(2016, 2, 2)])

    def test_the_template_only_adds_to_defensive_v2(self):
        # Every code line of defensive_v2.py is kept, in order; only comment
        # lines may change, so with UNIVERSE_SOURCE "coarse" it trades as
        # defensive_v2 does.
        with open(os.path.join(HERE, "momentum", "defensive_v2.py")) as f:
            old = f.read().splitlines()
        with open(os.path.join(HERE, "momentum", "universe_parity.py")) as f:
            new = f.read().splitlines()
        matcher = difflib.SequenceMatcher(a=old, b=new, autojunk=False)
        removed = [line for tag, i1, i2, _, _ in matcher.get_opcodes() if tag in ("replace", "delete")
                   for line in old[i1:i2]]
        self.assertEqual([line for line in removed if not line.lstrip().startswith("#")], [])

    def _fine_row(self, ticker, sector):
        sym = namedtuple("Sym", "Value")(ticker)
        row = types.SimpleNamespace(Symbol=sym)
        row.AssetClassification = types.SimpleNamespace(MorningstarSectorCode=sector)
        return row

    def test_a_sector_override_relabels_named_tickers_only(self):
        algo = self._algo(source="coarse")
        algo.SECTOR_OVERRIDE = {"MARA": 311}
        algo.sectors, algo.fund_reads, algo.fund_hits = {}, 0, 0
        rows = [self._fine_row("MARA", 103), self._fine_row("JPM", 103)]
        algo.Fine(rows)
        self.assertEqual({s.Value: c for s, c in algo.sectors.items()}, {"MARA": 311, "JPM": 103})
        self.assertEqual(algo.overridden, 1)
        algo.Fine(rows)                                        # the next month: same ticker again
        self.assertEqual((algo.overridden, algo.relabelled), (2, {"MARA"}))

    def test_without_an_override_the_sectors_are_morningstars(self):
        algo = self._algo(source="coarse")
        algo.SECTOR_OVERRIDE = {}
        algo.sectors, algo.fund_reads, algo.fund_hits = {}, 0, 0
        algo.Fine([self._fine_row("MARA", 103)])
        self.assertEqual([c for c in algo.sectors.values()], [103])

    def test_the_gics_override_run_differs_from_the_coarse_parity_run_only_in_its_labels(self):
        base = _class_constants(*build_variants.VARIANTS["OOS_P_COARSE"])
        c = _class_constants(*build_variants.VARIANTS["OOS_P_GICS"])
        override = c.pop("SECTOR_OVERRIDE")
        self.assertEqual(base.pop("SECTOR_OVERRIDE"), {})
        self.assertEqual(c, base)
        self.assertEqual(len(override), 20)
        self.assertTrue(set(override.values()) <= {101, 102, 103, 104, 205, 206, 207, 308, 309, 310, 311})

    def test_the_parity_runs_keep_the_combinations_settings(self):
        base = _class_constants(*build_variants.VARIANTS["OOS_F_COMBO"])
        base["END_DATE"] = (2026, 9, 30)
        expected = {"OOS_P_COARSE": ("coarse", 7), "OOS_P_IWB7": ("etf", 7), "OOS_P_IWB1": ("etf", 1)}
        for name, (source, lag) in expected.items():
            template, params = build_variants.VARIANTS[name]
            self.assertEqual(template, "momentum/universe_parity.py", name)
            c = _class_constants(template, params)
            own = {k: c.pop(k) for k in ("UNIVERSE_SOURCE", "ETF_TICKER", "ETF_LAG_DAYS", "SNAPSHOT_DAYS",
                                         "SECTOR_OVERRIDE")}
            self.assertEqual(c, base, name)
            self.assertEqual(own, {"UNIVERSE_SOURCE": source, "ETF_TICKER": "IWB", "ETF_LAG_DAYS": lag,
                                   "SNAPSHOT_DAYS": 45, "SECTOR_OVERRIDE": {}}, name)


class ParityCompareTest(unittest.TestCase):
    """momentum/parity_compare.py: month-by-month overlap of two runs'
    holdings, rebuilt from their fills."""

    def setUp(self):
        spec = importlib.util.spec_from_file_location(
            "parity_compare", os.path.join(HERE, "momentum", "parity_compare.py"))
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)

    @staticmethod
    def _t(y, m, d):
        return _dt.datetime(y, m, d, 13, tzinfo=_dt.timezone.utc).timestamp()

    def test_the_months_run_from_the_first_to_the_last_inclusive(self):
        self.assertEqual(self.module.months("2015-11", "2016-02"), ["2015-11", "2015-12", "2016-01", "2016-02"])

    def test_holdings_after_each_months_fills_carry_into_quiet_months(self):
        t = self._t
        fills = [[t(2016, 1, 4), "A", 10, 1.0, 0], [t(2016, 1, 4), "B", 5, 1.0, 0],
                 [t(2016, 1, 6), "C", 3, 1.0, 0],                         # a late buy in the same month
                 [t(2016, 3, 1), "B", -5, 1.0, 0], [t(2016, 3, 1), "IEF", 7, 1.0, 0]]
        held = self.module.holdings_by_month(fills, ["2016-01", "2016-02", "2016-03"])
        self.assertEqual(held["2016-01"], ({"A", "B", "C"}, False))
        self.assertEqual(held["2016-02"], ({"A", "B", "C"}, False))     # no trades: unchanged
        self.assertEqual(held["2016-03"], ({"A", "C"}, True))           # the bond fund is held

    def test_fills_before_the_study_count_and_after_it_do_not(self):
        t = self._t
        fills = [[t(2015, 6, 1), "A", 10, 1.0, 0], [t(2016, 2, 1), "A", -10, 1.0, 0]]
        self.assertEqual(self.module.holdings_by_month(fills, ["2016-01"]), {"2016-01": ({"A"}, False)})

    def test_the_bond_fund_is_recognised_by_ticker_or_by_security_id(self):
        # The tax study's export names each security by QuantConnect's
        # security ID, "<ticker at first listing> <code>".
        t = self._t
        fills = [[t(2016, 1, 4), "IEF SGNUDG1YI9ID", 7, 1.0, 0], [t(2016, 1, 4), "AAPL R735QTJ8XC9X", 3, 1.0, 0],
                 [t(2016, 1, 4), "IEFA VFL8MXEK1IP1", 2, 1.0, 0]]
        held = self.module.holdings_by_month(fills, ["2016-01"])
        self.assertEqual(held["2016-01"], ({"AAPL R735QTJ8XC9X", "IEFA VFL8MXEK1IP1"}, True))

    def test_a_month_with_nothing_held_is_refused(self):
        # An export that lost its early orders would otherwise look like an
        # empty book and lower the overlap without saying so.
        t = self._t
        fills = [[t(2016, 2, 1), "A", 10, 1.0, 0]]
        with self.assertRaises(ValueError):
            self.module.holdings_by_month(fills, ["2016-01", "2016-02"])

    def test_overlap_counts_shared_stocks_and_risk_off_agreement(self):
        a = {"2016-01": ({"A", "B"}, False), "2016-02": ({"A"}, True)}
        b = {"2016-01": ({"A", "C"}, False), "2016-02": ({"A"}, False)}
        rows = self.module.compare(a, b)
        self.assertEqual([(r["month"], r["common"], r["stocks_a"], r["stocks_b"], r["same_risk_off"])
                          for r in rows], [("2016-01", 1, 2, 2, True), ("2016-02", 1, 1, 1, False)])

    def test_the_summary_reports_the_mean_and_worst_month(self):
        rows = [{"month": "2016-02", "common": 20, "stocks_a": 25, "stocks_b": 25, "same_risk_off": True},
                {"month": "2016-03", "common": 10, "stocks_a": 25, "stocks_b": 25, "same_risk_off": False}]
        s = self.module.summary(rows)
        self.assertEqual((s["months"], s["worst_month"], s["risk_off_differs"]), (2, "2016-03", 1))
        self.assertAlmostEqual(s["mean_common_share"], 0.6)
        self.assertAlmostEqual(s["worst_common_share"], 0.4)


class GlidePathTest(unittest.TestCase):
    """momentum/glide_path.py: the combination plus a half-IEF/SHY bond
    sleeve whose share follows the owner's age schedule or stays fixed
    (docs/live-trading/plan.md, section 2)."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/glide_path.py", {})

    def test_the_schedule_meets_its_anchor_points(self):
        g = self.module.glide_share
        for age, share in ((37, 0.0), (38, 0.0), (45.5, 0.154), (53, 0.308), (59, 0.404), (65, 0.50),
                           (68.5, 0.60), (72, 0.70), (80, 0.70)):
            self.assertAlmostEqual(g(age), share, places=6, msg=age)

    def test_the_schedule_never_holds_more_bonds_than_the_straight_line_through_vanguards_points(self):
        def vanguard(age):
            if age <= 40:
                return 0.10
            if age <= 65:
                return 0.10 + 0.016 * (age - 40)
            return min(0.70, 0.50 + 0.20 * (age - 65) / 7)
        for months in range(38 * 12, 80 * 12):
            age = months / 12
            self.assertLessEqual(self.module.glide_share(age), vanguard(age) + 1e-9, age)

    def test_age_counts_whole_months_from_the_measurement_start(self):
        age = self.module.age_on
        start = _dt.date(2002, 8, 1)
        self.assertEqual(age(_dt.date(2002, 8, 1), 38.0, start), 38.0)
        self.assertEqual(age(_dt.date(2002, 8, 31), 38.0, start), 38.0)     # not yet a month
        self.assertEqual(age(_dt.date(2002, 9, 3), 38.0, start), 38.0 + 1 / 12)
        self.assertEqual(age(_dt.date(2003, 8, 1), 53.0, start), 54.0)
        self.assertEqual(age(_dt.date(2002, 7, 1), 38.0, start), 38.0 - 1 / 12)  # warm-up

    def test_the_bond_sleeve_takes_its_share_and_merges_with_risk_off_ief(self):
        book = {"A": 0.49, "IEF": 0.49}                 # a risk-off month's momentum book
        out = self.module.with_bond_sleeve(book, 0.30, {"IEF": 0.5, "SHY": 0.5})
        self.assertAlmostEqual(out["A"], 0.49 * 0.7)
        self.assertAlmostEqual(out["IEF"], 0.49 * 0.7 + 0.98 * 0.3 * 0.5)  # one position, both sleeves
        self.assertAlmostEqual(out["SHY"], 0.98 * 0.3 * 0.5)
        self.assertAlmostEqual(sum(out.values()), 0.98)

    def test_no_bond_share_leaves_the_book_unchanged(self):
        book = {"A": 0.5, "B": 0.48}
        self.assertEqual(self.module.with_bond_sleeve(book, 0.0, {"IEF": 0.5, "SHY": 0.5}), book)

    def test_the_share_used_each_month(self):
        algo = self.module.MomentumResearch()
        algo.MEASURE_FROM, algo.PATH_START_AGE = (2002, 8, 1), 53.0
        algo.Time = _dt.datetime(2008, 8, 1, 8)
        algo.GLIDE, algo.BOND_SHARE = "path", 0.0
        self.assertAlmostEqual(algo._bond_share(), 0.308 + 0.016 * 6)
        algo.GLIDE, algo.BOND_SHARE = "fixed", 0.6
        self.assertAlmostEqual(algo._bond_share(), 0.6)
        algo.GLIDE = "none"
        self.assertEqual(algo._bond_share(), 0.0)

    def _trading_algo(self, held):
        """An algorithm whose _trade can run: ``held`` maps symbol -> held
        fraction of a $1m account; every symbol is priced."""
        algo = self.module.MomentumResearch()
        algo.REBALANCE_BAND, algo.resizes, algo.resize_all = 0.25, 0, False
        algo.pending, algo.pending_tickets = {}, {}
        equity = 1_000_000.0

        class Securities(dict):
            def ContainsKey(self, k):
                return True

            def __getitem__(self, k):
                return types.SimpleNamespace(HasData=True, Price=100.0)

        def holding(k):
            w = held.get(k, 0.0)
            return types.SimpleNamespace(Invested=bool(w), IsLong=w > 0, IsShort=w < 0, HoldingsValue=w * equity)

        class Book(dict):
            TotalPortfolioValue = equity

            def __iter__(self):
                # Like LEAN's portfolio: key/value pairs of every holding.
                return iter([types.SimpleNamespace(Key=k, Value=holding(k)) for k in held])

            def __getitem__(self, k):
                return holding(k)

        algo.Securities, algo.Portfolio = Securities(), Book()
        algo.orders = []
        algo.SetHoldings = lambda sym, w: algo.orders.append((sym, round(w, 6)))
        algo.Liquidate = lambda sym: algo.orders.append((sym, 0))
        return algo

    def test_the_sleeve_reaches_trading_as_one_ief_target_and_the_band_uses_it(self):
        algo = self._trading_algo({"A": 0.343, "IEF": 0.49})
        algo.GLIDE, algo.BOND_SHARE, algo.MEASURE_FROM = "fixed", 0.30, (2002, 8, 1)
        algo.Time = _dt.datetime(2010, 3, 1, 8)
        algo.bond_syms, algo.b_used = {"IEF": "IEF", "SHY": "SHY"}, []
        book = algo._glide_targets({"A": 0.49, "IEF": 0.49})       # a risk-off month
        self.assertAlmostEqual(book["IEF"], 0.49)                  # 0.343 + 0.147, one target
        algo._trade(book)
        # IEF held at 0.49 is on its summed target, A on its scaled one: only
        # SHY, not yet held, is bought. Against momentum's IEF share alone
        # (0.343) the held 0.49 would be 43% off and resized.
        self.assertEqual(algo.orders, [("SHY", 0.147)])
        self.assertEqual(algo.b_used, [0.30])

    def test_a_held_stock_the_months_book_drops_is_sold_and_the_sleeve_still_bought(self):
        algo = self._trading_algo({"A": 0.686, "OLD": 0.30})
        algo.GLIDE, algo.BOND_SHARE, algo.MEASURE_FROM = "fixed", 0.30, (2002, 8, 1)
        algo.Time = _dt.datetime(2010, 3, 1, 8)
        algo.bond_syms, algo.b_used = {"IEF": "IEF", "SHY": "SHY"}, []
        algo._trade(algo._glide_targets({"A": 0.98}))             # OLD left the momentum book
        self.assertEqual(algo.orders, [("OLD", 0), ("IEF", 0.147), ("SHY", 0.147)])

    def test_rebalance_passes_its_book_through_the_sleeve_before_trading(self):
        import inspect
        src = inspect.getsource(self.module.MomentumResearch.Rebalance)
        self.assertLess(src.index("self._glide_targets(targets)"), src.index("self._trade(targets)"))

    def test_without_a_glide_the_book_is_untouched(self):
        algo = self.module.MomentumResearch()
        algo.GLIDE = "none"
        book = {"A": 0.98}
        self.assertIs(algo._glide_targets(book), book)

    def test_the_template_only_adds_to_defensive_v2(self):
        with open(os.path.join(HERE, "momentum", "defensive_v2.py")) as f:
            old = f.read().splitlines()
        with open(os.path.join(HERE, "momentum", "glide_path.py")) as f:
            new = f.read().splitlines()
        matcher = difflib.SequenceMatcher(a=old, b=new, autojunk=False)
        removed = [line for tag, i1, i2, _, _ in matcher.get_opcodes() if tag in ("replace", "delete")
                   for line in old[i1:i2]]
        self.assertEqual([line for line in removed if not line.lstrip().startswith("#")], [])

    def test_the_glide_runs_keep_the_combinations_settings_and_name_their_windows(self):
        combo = _class_constants(*build_variants.VARIANTS["F_COMBO"])
        windows = {"G_IS": ((2001, 8, 1), (2002, 8, 1), (2015, 12, 31)),
                   "G_OOS": ((2015, 1, 1), (2016, 1, 1), (2026, 9, 30))}
        expected = {"P38": ("path", 0.0, 38.0), "P53": ("path", 0.0, 53.0), "P65": ("path", 0.0, 65.0),
                    "F00": ("fixed", 0.0, 38.0), "F30": ("fixed", 0.3, 38.0), "F50": ("fixed", 0.5, 38.0),
                    "F60": ("fixed", 0.6, 38.0), "F70": ("fixed", 0.7, 38.0)}
        names = [n for n in build_variants.VARIANTS if n.startswith("G_")]
        self.assertEqual(len(names), 15)
        for name in names:
            window, kind = name.rsplit("_", 1)
            template, params = build_variants.VARIANTS[name]
            self.assertEqual(template, "momentum/glide_path.py", name)
            c = _class_constants(template, params)
            self.assertEqual((c.pop("START_DATE"), c.pop("MEASURE_FROM"), c.pop("END_DATE")), windows[window], name)
            self.assertEqual((c.pop("GLIDE"), c.pop("BOND_SHARE"), c.pop("PATH_START_AGE")), expected[kind], name)
            self.assertEqual(c.pop("BOND_FUNDS"), {"IEF": 0.5, "SHY": 0.5}, name)
            self.assertEqual(c, {k: v for k, v in combo.items() if k not in ("START_DATE", "MEASURE_FROM", "END_DATE")},
                             name)
        self.assertNotIn("G_OOS_F60", build_variants.VARIANTS)       # no 2016 run reaches 60% bonds


class BoostGoldenLogTest(unittest.TestCase):
    """boost/golden_log.py: template.py logging each day of chosen months,
    and its first decision, for the Go module's golden cases (ADR 0026)."""

    def setUp(self):
        self.module = _load_template(self, "boost/golden_log.py", {})

    def test_the_day_line_carries_the_inputs_and_the_state(self):
        line = self.module.boost_line(_dt.datetime(2019, 5, 13, 16), 280.5, 6.25, 275.0, 285.0, True, 1.47)
        self.assertEqual(line, "BGOLD 2019-05-13 close=280.500000 rsi2=6.250000 sma200=275.000000 "
                               "sma5=285.000000 sig=1 want=1.470000|")
        self.assertLessEqual(len(line), 190)

    def test_an_indicator_not_ready_is_written_as_a_dash(self):
        line = self.module.boost_line(_dt.datetime(1993, 9, 1, 16), 45.0, None, None, 44.0, False, 0.98)
        self.assertIn("rsi2=- sma200=-", line)

    def test_ondata_logs_the_first_decision_and_the_chosen_months_days_only(self):
        algo = _algorithm(self.module, "BoostResearch")
        algo.GOLDEN_MONTHS = ((1992, 2),)
        algo.logs, algo.stats = [], {}
        algo.Log = algo.logs.append
        algo.SetRuntimeStatistic = lambda k, v: algo.stats.__setitem__(k, v)
        algo.IsWarmingUp = True
        _day(algo, when=_dt.datetime(1992, 1, 2))          # warm-up: nothing
        algo.IsWarmingUp = False
        _day(algo, when=_dt.datetime(1992, 1, 15))         # the first decision, outside the months
        _day(algo, when=_dt.datetime(1992, 1, 16))         # skipped
        algo.rsi2 = _Indicator(5.0)
        _day(algo, when=_dt.datetime(1992, 2, 3))          # chosen month: a boost begins
        self.assertEqual([l.split()[1] for l in algo.logs], ["1992-01-15", "1992-02-03"])
        self.assertIn("sig=0 want=0.980000|", algo.logs[0])
        self.assertIn("rsi2=5.000000", algo.logs[1])
        self.assertIn("sig=1 want=1.470000|", algo.logs[1])
        self.assertEqual(algo.orders, [{"SPY": 0.98}, {"SPY": 1.47}])   # trading unchanged
        algo.OnEndOfAlgorithm()
        self.assertEqual(algo.stats["Golden"], "months=1 lines=2")

    def test_only_chosen_months_and_the_first_decision_are_logged(self):
        algo = self.module.BoostResearch()
        algo.GOLDEN_MONTHS = ((2019, 5),)
        algo.Time = _dt.datetime(2019, 5, 2, 16)
        self.assertTrue(algo._golden_day(first=False))
        algo.Time = _dt.datetime(2019, 6, 3, 16)
        self.assertFalse(algo._golden_day(first=False))
        self.assertTrue(algo._golden_day(first=True))

    def test_the_template_only_adds_to_template_py(self):
        with open(os.path.join(HERE, "boost", "template.py")) as f:
            old = f.read().splitlines()
        with open(os.path.join(HERE, "boost", "golden_log.py")) as f:
            new = f.read().splitlines()
        matcher = difflib.SequenceMatcher(a=old, b=new, autojunk=False)
        removed = [line for tag, i1, i2, _, _ in matcher.get_opcodes() if tag in ("replace", "delete")
                   for line in old[i1:i2]]
        self.assertEqual([line for line in removed if not line.lstrip().startswith("#")], [])

    def test_the_golden_run_keeps_bx_spys_settings(self):
        c = _class_constants(*build_variants.VARIANTS["BX_GOLD"])
        months = c.pop("GOLDEN_MONTHS")
        self.assertEqual(c, _class_constants(*build_variants.VARIANTS["BX_SPY"]))
        self.assertTrue(1 <= len(months) <= 3)


if __name__ == "__main__":
    unittest.main()
