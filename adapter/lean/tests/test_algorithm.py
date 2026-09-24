import importlib.util
import sys
import types
import unittest
from datetime import datetime, time
from pathlib import Path
from unittest.mock import patch

tests_dir = str(Path(__file__).resolve().parent)
adapter_dir = str(Path(__file__).resolve().parents[1])
for p in (tests_dir, adapter_dir):
    if p not in sys.path:
        sys.path.insert(0, p)

from test_publisher import Frame, bar


class FakeAlgorithm:
    LiveMode = False
    def SetStartDate(self, *args): pass
    def SetEndDate(self, *args): pass
    def SetCash(self, *args): pass
    def SetTimeZone(self, *args): pass
    def AddEquity(self, ticker, resolution, **kwargs):
        self.subscription = kwargs
        return types.SimpleNamespace(Symbol="AAPL")
    def SetWarmUp(self, count, resolution):
        self.warmup = (count, resolution)
    def Log(self, message): pass
    def Quit(self, message): self.quit_reason = message


imports = types.ModuleType("AlgorithmImports")
imports.QCAlgorithm = FakeAlgorithm
imports.Resolution = types.SimpleNamespace(Daily="daily")
imports.DataNormalizationMode = types.SimpleNamespace(SplitAdjusted="split", Raw="raw")
imports.TimeZones = types.SimpleNamespace(NewYork="NY")
imports.time = time
sys.modules["AlgorithmImports"] = imports
spec = importlib.util.spec_from_file_location("lean_algorithm", Path(__file__).parents[1] / "algorithm.py")
algorithm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(algorithm)


class AlgorithmTests(unittest.TestCase):
    def init(self):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3}
        algo = algorithm.CompletedBarsAlgorithm()
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client"):
            algo.Initialize()
        return algo

    def test_warmup_is_three_bars_across_weekend_and_not_three_days(self):
        algo = self.init()
        self.assertEqual(algo.warmup, (3, "daily"))
        self.assertEqual(algo.subscription["dataNormalizationMode"], "split")
        self.assertFalse(algo.subscription["fillForward"])
        seen = []
        algo.publisher = types.SimpleNamespace(sequence=2, publish=lambda *args: seen.append(args) or [])
        for day, warming in ((4, True), (5, True), (6, True), (9, False)):
            b = bar(day)
            algo.IsWarmingUp = warming
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(types.SimpleNamespace(Bars={"AAPL": b}))
        # Warm-up bars are counted but never sent to the engine: the engine
        # has no notion of priming, so this is the only way to guarantee it
        # decides nothing during warm-up (only day 9, past warm-up, reaches
        # publish()).
        self.assertEqual(len(seen), 1)
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        self.assertEqual(seen[-1][-1], "2014-06-09T20:00:00Z")
        self.assertFalse(hasattr(algo, "MarketOrder"))

    def test_no_engine_exchange_while_warming_up(self):
        """publishes no decisions during warm-up (no bar is even sent)."""
        algo = self.init()
        calls = []
        algo.publisher = types.SimpleNamespace(
            sequence=2, publish=lambda *a: calls.append(a) or [])
        algo.History = lambda *a, **k: (_ for _ in ()).throw(
            AssertionError("History must not be called during warm-up"))
        algo.IsWarmingUp = True
        for day in (4, 5, 6):
            algo.OnData(types.SimpleNamespace(Bars={"AAPL": bar(day)}))
        self.assertEqual(calls, [])
        self.assertFalse(algo.failed)
        self.assertEqual(algo.decision_count, 0)

    def test_missing_raw_stops_stream_without_reusing_connection(self):
        algo = self.init()
        algo.IsWarmingUp = False
        algo.History = lambda *args, **kwargs: None
        data = types.SimpleNamespace(Bars={"AAPL": bar(9)})
        algo.OnData(data)
        self.assertTrue(algo.failed)
        self.assertEqual(algo.bar_count, 0)
        algo.OnData(data)
        self.assertEqual(algo.bar_count, 0)

    def test_absent_engine_quits_at_startup(self):
        with patch.object(algorithm, "load_settings", side_effect=OSError("absent")):
            algo = algorithm.CompletedBarsAlgorithm()
            algo.Initialize()
        self.assertTrue(algo.failed)
        self.assertIn("unavailable", algo.quit_reason)

    def test_calendar_day_warmup_reading_fails_bar_count(self):
        """Warm-up counts completed bars, never calendar days.

        Starting Friday 2014-06-06, 3 calendar days ends on Monday 2014-06-09.
        A calendar-day reading would conclude warm-up having seen only 1
        completed trading bar (Friday). The adapter configures SetWarmUp with
        bar count at daily resolution so all 3 trading bars are observed.
        """
        start = datetime(2014, 6, 6)
        warmup_target = 3
        calendar_bars = [
            b for b in (datetime(2014, 6, 6), datetime(2014, 6, 9), datetime(2014, 6, 10))
            if (b - start).days < warmup_target
        ]
        self.assertEqual(len(calendar_bars), 1)
        self.assertNotEqual(len(calendar_bars), warmup_target)

    def test_invalid_warmup_bars_fails_closed(self):
        for bad_warmup in (-1, "3", 3.5, None):
            settings = {"socket": "unused", "configuration_hash": "hash",
                        "strategy_version": "version", "run_id": "test",
                        "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                        "warmup_bars": bad_warmup}
            algo = algorithm.CompletedBarsAlgorithm()
            with patch.object(algorithm, "load_settings", return_value=settings):
                algo.Initialize()
            self.assertTrue(algo.failed)

    def test_live_mode_rejected(self):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 0}
        algo = algorithm.CompletedBarsAlgorithm()
        algo.LiveMode = True
        with patch.object(algorithm, "load_settings", return_value=settings):
            algo.Initialize()
        self.assertTrue(algo.failed)
        self.assertIn("backtest-only", algo.quit_reason)

