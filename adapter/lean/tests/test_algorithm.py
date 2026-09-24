import importlib.util
import sys
import types
import unittest
from datetime import datetime, time
from pathlib import Path
from unittest.mock import patch

from test_publisher import Frame, bar


class FakeAlgorithm:
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
        algo.publisher = types.SimpleNamespace(publish=lambda *args: seen.append(args) or [])
        for day, warming in ((4, True), (5, True), (6, True), (9, False)):
            b = bar(day)
            algo.IsWarmingUp = warming
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(types.SimpleNamespace(Bars={"AAPL": b}))
        self.assertEqual(len(seen), 4)
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        self.assertEqual(seen[-1][-1], "2014-06-09T20:00:00Z")
        self.assertFalse(hasattr(algo, "MarketOrder"))

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
