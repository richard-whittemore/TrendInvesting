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

from test_publisher import Client as FakeEngineClient, Frame, bar

from client import Unavailable
from publisher import Publisher


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
    # Every LEAN order entry point records its call, so a test can assert that
    # none was made rather than relying on the method being absent.
    def _order(self, *args, **kwargs):
        self.__dict__.setdefault("orders", []).append((args, kwargs))
    MarketOrder = LimitOrder = StopMarketOrder = StopLimitOrder = MarketOnOpenOrder = _order


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
        # Every completed bar is published, warm-up included: the reducer
        # builds N and the Entry/Exit Channels from every bar it receives, so
        # withholding LEAN's warm-up bars would starve those figures rather
        # than suppress any decision.
        self.assertEqual(len(seen), 4)
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        self.assertEqual(seen[-1][-1], "2014-06-09T20:00:00Z")
        self.assertEqual(getattr(algo, "orders", []), [])

    def test_warmup_bars_are_published_and_numbered_contiguously(self):
        """publishes every completed bar, warm-up included, in one contiguous sequence."""
        algo = self.init()
        client = FakeEngineClient()
        algo.publisher = Publisher(client, "hash", "version", "test")
        for day, warming in ((4, True), (5, True), (6, True), (9, False)):
            b = bar(day)
            algo.IsWarmingUp = warming
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(types.SimpleNamespace(Bars={"AAPL": b}))
        self.assertFalse(algo.failed)
        # The first bar carries Sequence 2 (continuing the engine's own
        # configuration input at Sequence 1), and warm-up bars share that
        # same numbering with the bars that follow warm-up, contiguously.
        self.assertEqual([e["sequence"] for e in client.sent], [2, 3, 4, 5])
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        # FakeEngineClient answers every bar with zero decisions; what this
        # test pins is that all four bars — warm-up included — reach the
        # engine at all, contiguously numbered.
        self.assertEqual(algo.decision_count, 0)
        self.assertEqual(client.sent[-1]["payload"]["period_end"], "2014-06-09T20:00:00Z")

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
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3}
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client", side_effect=Unavailable("no engine on the socket")):
            algo = algorithm.CompletedBarsAlgorithm()
            algo.Initialize()
        self.assertIsNone(algo.client)
        self.assertTrue(algo.failed)
        self.assertIn("no engine on the socket", algo.quit_reason)

    def test_warmup_is_requested_in_bars_not_as_a_calendar_span(self):
        """LEAN counts a SetWarmUp(int, Resolution) in bars; a timedelta would
        be read as calendar time. Driven over Friday, Monday and Tuesday, the
        three warm-up bars span five calendar days and are all counted."""
        algo = self.init()
        count, resolution = algo.warmup
        self.assertIs(type(count), int)
        self.assertEqual((count, resolution), (3, "daily"))
        algo.publisher = types.SimpleNamespace(sequence=2, publish=lambda *args: [])
        for day in (6, 9, 10):
            b = bar(day)
            algo.IsWarmingUp = True
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(types.SimpleNamespace(Bars={"AAPL": b}))
        self.assertEqual(algo.warmup_seen, 3)
        self.assertFalse(algo.failed)

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

