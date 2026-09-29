"""Behavioural tests for the timing script's Boost and RSI-2 rules and for the
realistic-margin builder (docs/research/edge-search-2026-09.md, "Boost" and
"Boost re-test with realistic margin rates").

main.py imports QuantConnect's AlgorithmImports, which only exists in their
cloud, so these tests install a minimal stand-in module: just the names
main.py uses, with an algorithm that records its orders instead of trading.
Standard library only; synthetic values, no market data.
"""
import datetime as _dt
import importlib.util
import os
import sys
import tempfile
import types
import unittest

import build_realistic_boost

HERE = os.path.dirname(os.path.abspath(__file__))


class _Target:
    def __init__(self, symbol, quantity):
        self.Symbol, self.Quantity = symbol, quantity


def _install_stub():
    stub = types.ModuleType("AlgorithmImports")
    stub.QCAlgorithm = type("QCAlgorithm", (), {})
    stub.PortfolioTarget = _Target
    stub.datetime = _dt.datetime
    for name in ("Resolution", "MovingAverageType", "BrokerageName", "AccountType"):
        setattr(stub, name, types.SimpleNamespace())
    sys.modules["AlgorithmImports"] = stub


def _load(path, name):
    _install_stub()
    spec = importlib.util.spec_from_file_location(name, path)
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


class _Portfolio(dict):
    def __init__(self, equity):
        super().__init__(SPY=types.SimpleNamespace(Invested=False), SHY=types.SimpleNamespace(Invested=False))
        self.TotalPortfolioValue = equity
        self.TotalHoldingsValue = 0.0
        self.CashBook = {"USD": _Cash()}


def _algorithm(module, mode, equity=1_000_000.0):
    """A TimingResearch built without Initialize(): SPY above its 200-day
    SMA, RSI(2) neutral, close below its 5-day SMA, SHY priced."""
    algo = module.TimingResearch()
    algo.MODE = mode
    algo.spy, algo.shy = "SPY", "SHY"
    algo.sma200, algo.sma5, algo.rsi2 = _Indicator(90.0), _Indicator(101.0), _Indicator(50.0)
    algo.IsWarmingUp = False
    algo.Time = _dt.datetime(2010, 6, 1)
    algo.Portfolio = _Portfolio(equity)
    algo.Securities = {"SHY": types.SimpleNamespace(HasData=True, Price=80.0)}
    algo.equity_curve, algo.spy_curve = [], []
    algo.days_in = algo.days_total = algo.trades = 0
    algo.financing_paid = 0.0
    algo.orders = []

    def set_holdings(target, size=None):
        targets = target if isinstance(target, list) else [_Target(target, size)]
        algo.orders.append({t.Symbol: round(t.Quantity, 6) for t in targets})
        for t in targets:
            algo.Portfolio[t.Symbol].Invested = t.Quantity != 0

    def liquidate(symbol):
        algo.orders.append({symbol: 0.0})
        algo.Portfolio[symbol].Invested = False

    algo.SetHoldings, algo.Liquidate = set_holdings, liquidate
    return algo


def _day(algo, close):
    algo.OnData(types.SimpleNamespace(Bars={"SPY": types.SimpleNamespace(Close=close)}))


class BoostTest(unittest.TestCase):
    def setUp(self):
        self.main = _load(os.path.join(HERE, "main.py"), "timing_main")
        self.algo = _algorithm(self.main, "boost")

    def test_without_a_signal_it_holds_spy_at_98_percent(self):
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.0, "SPY": 0.98}])
        self.assertEqual((self.algo.trades, self.algo.days_in), (0, 0))

    def test_an_oversold_day_in_an_uptrend_raises_spy_to_the_boost_size(self):
        self.algo.rsi2 = _Indicator(5.0)
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.0, "SPY": 1.47}])
        self.assertEqual((self.algo.trades, self.algo.days_in), (1, 1))

    def test_no_boost_below_the_200_day_sma(self):
        self.algo.rsi2 = _Indicator(5.0)
        self.algo.sma200 = _Indicator(110.0)
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.0, "SPY": 0.98}])

    def test_no_boost_until_the_200_day_sma_is_ready(self):
        self.algo.rsi2 = _Indicator(5.0)
        self.algo.sma200 = _Indicator(90.0, ready=False)
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.0, "SPY": 0.98}])

    def test_the_boost_holds_until_a_close_above_the_5_day_sma(self):
        self.algo.rsi2 = _Indicator(5.0)
        _day(self.algo, 100.0)
        self.algo.rsi2 = _Indicator(50.0)
        _day(self.algo, 100.5)      # still below the 5-day SMA of 101: stays boosted, no new order
        _day(self.algo, 102.0)      # above it: back to 98%
        self.assertEqual(self.algo.orders, [{"SHY": 0.0, "SPY": 1.47}, {"SHY": 0.0, "SPY": 0.98}])
        self.assertEqual(self.algo.days_in, 2)

    def test_borrowed_exposure_is_charged_the_financing_rate_daily(self):
        self.algo.Portfolio.TotalHoldingsValue = 1_470_000.0
        _day(self.algo, 100.0)
        cost = 470_000.0 * self.main.TimingResearch.FINANCING_RATE / 252.0
        self.assertAlmostEqual(self.algo.financing_paid, cost)
        self.assertAlmostEqual(self.algo.Portfolio.CashBook["USD"].added, -cost)

    def test_nothing_is_charged_without_borrowing(self):
        self.algo.Portfolio.TotalHoldingsValue = 980_000.0
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.financing_paid, 0.0)


class Rsi2Test(unittest.TestCase):
    def setUp(self):
        self.algo = _algorithm(_load(os.path.join(HERE, "main.py"), "timing_main"), "rsi2")

    def test_idle_money_is_parked_in_shy(self):
        _day(self.algo, 100.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.98}])

    def test_it_buys_when_oversold_in_an_uptrend_and_sells_above_the_5_day_sma(self):
        self.algo.Portfolio["SHY"].Invested = True
        self.algo.rsi2 = _Indicator(5.0)
        _day(self.algo, 100.0)
        self.algo.rsi2 = _Indicator(50.0)
        _day(self.algo, 100.5)
        _day(self.algo, 102.0)
        self.assertEqual(self.algo.orders, [{"SHY": 0.0}, {"SPY": 0.98}, {"SPY": 0.0}, {"SHY": 0.98}])


class StatsTest(unittest.TestCase):
    def test_cagr_and_max_drawdown(self):
        algo = _load(os.path.join(HERE, "main.py"), "timing_main").TimingResearch()
        t0 = _dt.datetime(2000, 1, 1)
        curve = [(t0, 100.0), (t0 + _dt.timedelta(days=100), 150.0),
                 (t0 + _dt.timedelta(days=200), 120.0), (t0 + _dt.timedelta(days=1461), 146.41)]
        self.assertEqual(algo._stats(curve, _dt.date(2000, 1, 1), _dt.date(2004, 12, 31)),
                         "CAGR=0.1000 MaxDD=0.2000")


class BuilderTest(unittest.TestCase):
    ROWS = ["observation_date,TB3MS", "1997-12-01,5.00", "1998-01-01,4.00", "1998-02-01,.", "1998-03-01,3.00"]

    def _built(self, prior_month=False, rows=None):
        with tempfile.NamedTemporaryFile("w", suffix=".csv", delete=False) as f:
            f.write("\n".join(rows or self.ROWS) + "\n")
        self.addCleanup(os.remove, f.name)
        src = build_realistic_boost.build(f.name, prior_month=prior_month)
        with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as g:
            g.write(src)
        self.addCleanup(os.remove, g.name)
        return _load(g.name, "boost_realistic")

    def _charge(self, module, when):
        algo = _algorithm(module, module.TimingResearch.MODE)
        algo.Time = when
        algo.Portfolio.TotalHoldingsValue = 1_252_000.0
        _day(algo, 100.0)
        return algo.financing_paid

    def test_same_month_rates_from_1998_with_missing_months_skipped(self):
        module = self._built()
        self.assertEqual(module.RATES, {199801: 0.04, 199803: 0.03})
        self.assertEqual(module.TimingResearch.MODE, "boost")

    def test_prior_month_charges_each_month_the_previous_months_rate(self):
        self.assertEqual(self._built(prior_month=True).RATES, {199801: 0.05, 199802: 0.04, 199804: 0.03})

    def test_the_charge_is_the_t_bill_rate_plus_the_spread(self):
        module = self._built()
        self.assertAlmostEqual(self._charge(module, _dt.datetime(1998, 1, 15)), 252_000.0 * 0.055 / 252.0)

    def test_a_missing_month_uses_the_latest_earlier_rate(self):
        module = self._built()
        self.assertAlmostEqual(self._charge(module, _dt.datetime(1998, 2, 15)), 252_000.0 * 0.055 / 252.0)

    def test_prior_month_without_december_1997_is_refused(self):
        with self.assertRaises(SystemExit):
            self._built(prior_month=True, rows=self.ROWS[:1] + self.ROWS[2:])


if __name__ == "__main__":
    unittest.main()
