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


class FakeAlgorithm:
    LiveMode = False
    def SetStartDate(self, *args): pass
    def SetEndDate(self, *args): pass
    def SetCash(self, cash):
        self.Portfolio = types.SimpleNamespace(TotalPortfolioValue=cash, Cash=cash)
    def SetTimeZone(self, *args): pass
    def AddEquity(self, ticker, resolution, **kwargs):
        self.subscription = kwargs
        return types.SimpleNamespace(Symbol="AAPL")
    def SetWarmUp(self, count, resolution):
        self.warmup = (count, resolution)
    def Log(self, message): self.__dict__.setdefault("logs", []).append(message)
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
imports.DelistingType = types.SimpleNamespace(Warning="warning", Delisted="delisted")
imports.time = time
sys.modules["AlgorithmImports"] = imports
spec = importlib.util.spec_from_file_location("lean_algorithm", Path(__file__).parents[1] / "algorithm.py")
algorithm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(algorithm)


def slice_of(bars=None, delistings=None, changes=None):
    """A LEAN data slice: LEAN always provides all three collections."""
    return types.SimpleNamespace(Bars=bars or {}, Delistings=delistings or {},
                                 SymbolChangedEvents=changes or {})


class AlgorithmTests(unittest.TestCase):
    def init(self):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3, "cash": 1000000}
        algo = algorithm.CompletedBarsAlgorithm()
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client", return_value=FakeEngineClient()):
            algo.Initialize()
        return algo

    def test_warmup_is_three_bars_across_weekend_and_not_three_days(self):
        algo = self.init()
        self.assertEqual(algo.warmup, (3, "daily"))
        self.assertEqual(algo.subscription["dataNormalizationMode"], "split")
        self.assertFalse(algo.subscription["fillForward"])
        seen = []
        algo.publisher = types.SimpleNamespace(sequence=2, publish=lambda *args: seen.append(args) or [],
                                               publish_snapshot=lambda *args: [])
        for day, warming in ((4, True), (5, True), (6, True), (9, False)):
            b = bar(day)
            algo.IsWarmingUp = warming
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(slice_of({"AAPL": b}))
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
        client = algo.client
        self.assertEqual(client.sent, [])
        for day, warming in ((4, True), (5, True), (6, True), (9, False)):
            b = bar(day)
            algo.IsWarmingUp = warming
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(slice_of({"AAPL": b}))
        self.assertFalse(algo.failed)
        # The first bar carries Sequence 2 (continuing the engine's own
        # configuration input at Sequence 1), and warm-up bars share that
        # same numbering with the bars that follow warm-up, contiguously.
        self.assertEqual([e["sequence"] for e in client.sent], list(range(2, 10)))
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        # FakeEngineClient answers every input with zero decisions; all four
        # bars and their snapshots reach the engine in one sequence.
        self.assertEqual(algo.decision_count, 0)
        self.assertEqual([e["type"] for e in client.sent],
                         ["market.bar.completed", "account.snapshot"] * 4)
        ends = []
        for completed, snapshot in zip(client.sent[::2], client.sent[1::2]):
            end = completed["payload"]["period_end"]
            self.assertEqual(snapshot["payload"]["as_of"], end)
            self.assertEqual(snapshot["recorded_at"], end)
            ends.append(end)
        self.assertTrue(all(a < b for a, b in zip(ends, ends[1:])))
        self.assertEqual(ends[-1], "2014-06-09T20:00:00Z")

    def test_portfolio_is_read_after_bar_reply(self):
        algo = self.init()
        def update_portfolio(envelope):
            if envelope["type"] == "market.bar.completed":
                algo.Portfolio.TotalPortfolioValue = 123456.75
                algo.Portfolio.Cash = 54321.25
        algo.client.after_reply = update_portfolio
        algo.IsWarmingUp = True
        algo.History = lambda *args, **kwargs: Frame(bar(6).EndTime)
        algo.OnData(slice_of({"AAPL": bar(6)}))
        self.assertFalse(algo.failed)
        self.assertEqual(algo.client.sent[-1]["payload"], {
            "as_of": "2014-06-06T20:00:00Z", "equity": 123456.75,
            "available_cash": 54321.25, "currency": "USD"})

    def test_bad_snapshot_reply_stops_run(self):
        for field, value in (("configuration_hash", "other"), ("strategy_version", "other"),
                             ("sequence", 99), ("correlation_id", "other"),
                             ("causation_id", "other"), ("payload_hash", "bad"),
                             ("type", "other"), ("schema_version", 99),
                             ("envelope_version", 99), ("payload", {"decisions": None})):
            with self.subTest(field=field):
                algo = self.init()
                algo.client.reply_overrides = {"account.snapshot": {field: value}}
                algo.IsWarmingUp = True
                algo.History = lambda *args, **kwargs: Frame(bar(6).EndTime)
                data = slice_of({"AAPL": bar(6)})
                algo.OnData(data)
                self.assertTrue(algo.failed)
                self.assertTrue(algo.client.closed)
                self.assertEqual(len(algo.client.sent), 2)
                self.assertEqual(algo.publisher.sequence, 2)
                algo.OnData(data)
                self.assertEqual(len(algo.client.sent), 2)

    def test_bad_bar_reply_sends_no_snapshot(self):
        algo = self.init()
        algo.client.reply_overrides = {"market.bar.completed": {"sequence": 99}}
        algo.IsWarmingUp = True
        algo.History = lambda *args, **kwargs: Frame(bar(6).EndTime)
        algo.OnData(slice_of({"AAPL": bar(6)}))
        self.assertTrue(algo.failed)
        self.assertEqual(len(algo.client.sent), 1)

    def test_missing_raw_stops_stream_without_reusing_connection(self):
        algo = self.init()
        algo.IsWarmingUp = False
        algo.History = lambda *args, **kwargs: None
        data = slice_of({"AAPL": bar(9)})
        algo.OnData(data)
        self.assertTrue(algo.failed)
        self.assertEqual(algo.bar_count, 0)
        algo.OnData(data)
        self.assertEqual(algo.bar_count, 0)

    def test_absent_engine_quits_at_startup(self):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3, "cash": 1000000}
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
        algo.publisher = types.SimpleNamespace(sequence=2, publish=lambda *args: [],
                                               publish_snapshot=lambda *args: [])
        for day in (6, 9, 10):
            b = bar(day)
            algo.IsWarmingUp = True
            algo.History = lambda *args, **kwargs: Frame(b.EndTime)
            algo.OnData(slice_of({"AAPL": b}))
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



class CashSettingTests(unittest.TestCase):
    """Starting cash comes from the run's settings, never a built-in default."""
    base = {"socket": "unused", "configuration_hash": "hash",
            "strategy_version": "version", "run_id": "test", "symbol": "AAPL",
            "start": "2014-06-09", "end": "2014-06-10", "warmup_bars": 3}

    def start(self, settings):
        algo = algorithm.CompletedBarsAlgorithm()
        algo.SetCash = lambda value: setattr(algo, "cash", value)
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client"):
            algo.Initialize()
        return algo

    def test_cash_is_the_runs_own_figure(self):
        algo = self.start(dict(self.base, cash=1000000))
        self.assertFalse(algo.failed)
        self.assertEqual(algo.cash, 1000000)

    def test_missing_or_invalid_cash_fails_closed(self):
        for bad in (None, 0, -1, "1000000", float("inf"), float("nan"), True):
            with self.subTest(cash=bad):
                settings = dict(self.base)
                if bad is not None:
                    settings["cash"] = bad
                algo = self.start(settings)
                self.assertTrue(algo.failed)
                self.assertFalse(hasattr(algo, "cash"))


class DelistingTests(unittest.TestCase):
    """LEAN's DELISTED stops the run; nothing about it is ever published.

    LEAN reported GOOAV — a when-issued share that converted into GOOG — as
    DELISTED on 2014-04-03, and its delisting carries no reason, so the
    adapter cannot tell a real delisting from a conversion.
    """

    def start(self):
        algo = AlgorithmTests.init(self)
        algo.IsWarmingUp = False
        return algo

    def feed(self, algo, day, **kwargs):
        b = bar(day)
        algo.History = lambda *args, **kw: Frame(b.EndTime)
        algo.OnData(slice_of({"AAPL": b}, **kwargs))

    def notice(self, kind, day=9):
        return types.SimpleNamespace(Type=kind, Time=datetime(2014, 6, day))

    def test_delisted_with_a_bar_publishes_the_bar_then_stops(self):
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9, delistings={"AAPL": self.notice("delisted")})
        # The day's real bar and its snapshot still reach the engine; the
        # delisting itself is never published, and the run stops naming it.
        self.assertEqual([e["type"] for e in algo.client.sent],
                         ["market.bar.completed", "account.snapshot"] * 2)
        self.assertTrue(algo.failed)
        self.assertIn("AAPL DELISTED", algo.quit_reason)

    def test_delisted_on_a_barless_slice_stops_and_publishes_nothing(self):
        algo = self.start()
        self.feed(algo, 6)
        sent = len(algo.client.sent)
        algo.OnData(slice_of({}, delistings={"AAPL": self.notice("delisted")}))
        self.assertEqual(len(algo.client.sent), sent)
        self.assertTrue(algo.failed)

    def test_nothing_is_published_after_the_stop(self):
        algo = self.start()
        algo.OnData(slice_of({}, delistings={"AAPL": self.notice("delisted")}))
        self.feed(algo, 10)
        self.assertEqual(algo.client.sent, [])

    def test_a_delisting_warning_is_logged_and_the_run_continues(self):
        algo = self.start()
        self.feed(algo, 6, delistings={"AAPL": self.notice("warning", 6)})
        self.feed(algo, 9)
        self.assertFalse(algo.failed)
        self.assertEqual(len(algo.client.sent), 4)
        self.assertTrue(any("delisting warning for AAPL" in m for m in algo.logs))

    def test_another_instruments_delisting_is_ignored(self):
        algo = self.start()
        self.feed(algo, 6, delistings={"MSFT": self.notice("delisted", 6)})
        self.assertFalse(algo.failed)

    def test_a_symbol_change_is_logged_and_never_published(self):
        algo = self.start()
        change = types.SimpleNamespace(OldSymbol="GOOAV", NewSymbol="GOOG")
        self.feed(algo, 6, changes={"AAPL": change})
        self.assertFalse(algo.failed)
        self.assertEqual([e["type"] for e in algo.client.sent],
                         ["market.bar.completed", "account.snapshot"])
        self.assertTrue(any("symbol changed GOOAV -> GOOG" in m for m in algo.logs))
