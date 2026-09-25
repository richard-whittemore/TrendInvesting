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

# The digest currently pinned in adapter/lean/README.md; any syntactically
# valid digest would do for these tests, but reusing the real one keeps the
# fixture honest.
VALID_LEAN_IMAGE = "quantconnect/lean@sha256:9b8e69ec49e49f0ee207c27c6b0f3e2e6b35cfd7a241f31aa16577c6debb890d"


class FakePortfolio:
    """LEAN's Portfolio: account figures, and each security's holding by symbol."""
    def __init__(self, cash):
        self.TotalPortfolioValue = cash
        self.Cash = cash
        self.holdings = {}

    def __getitem__(self, symbol):
        return types.SimpleNamespace(Quantity=self.holdings.get(symbol, 0))


class FakeResponse:
    def __init__(self, success):
        self.IsSuccess = success


# The LEAN OrderStatus values in which an order is no longer working; a
# ticket in any other state still is.
CLOSED_STATUSES = ("filled", "canceled", "invalid")


class Enumerable:
    """LEAN's order-ticket collections: iterable, but with no len or indexing.

    Observed on the pinned image: Transactions.GetOrderTickets returns a
    MemoizingEnumerable that is 'not subscriptable'. Truth-testing one is
    refused here too, since a .NET object is always truthy and so says
    nothing about whether any ticket matched.
    """
    def __init__(self, items):
        self._items = list(items)

    def __iter__(self):
        return iter(self._items)

    def __bool__(self):
        raise TypeError("a LEAN enumerable has no truth value; convert it with list() first")


def order_event(ticket, status, when, fill_quantity=0, fill_price=0.0, fee=0.0,
                currency="USD", message=""):
    """A LEAN OrderEvent for ticket, as LEAN's OnOrderEvent receives it."""
    ticket.event_ids += 1
    return types.SimpleNamespace(
        OrderId=ticket.OrderId, Id=ticket.event_ids, Symbol=ticket.Symbol, Status=status,
        UtcTime=when, FillQuantity=fill_quantity, FillPrice=fill_price,
        OrderFee=types.SimpleNamespace(Value=types.SimpleNamespace(Amount=fee, Currency=currency)),
        StopPrice=ticket.StopPrice, LimitPrice=ticket.LimitPrice, Quantity=ticket.Quantity,
        Message=message)


class FakeTicket:
    """A LEAN OrderTicket for a stop-market or stop-limit order, held in
    FakeTransactions. LimitPrice is None for a stop-market order."""
    def __init__(self, book, order_id, symbol, quantity, stop_price, tag, properties,
                 limit_price=None):
        self.book = book
        self.OrderId = order_id
        self.Symbol = symbol
        self.Quantity = quantity
        self.QuantityFilled = 0
        self.StopPrice = stop_price
        self.LimitPrice = limit_price
        self.OrderType = "stop-market" if limit_price is None else "stop-limit"
        self.Tag = tag
        self.TimeInForce = properties.TimeInForce
        self.Status = book.submit_status
        self.event_ids = 0

    def Get(self, field):
        """LEAN's OrderTicket.Get(OrderField): the order's current figure."""
        if field == "stop-price":
            return self.StopPrice
        if field == "limit-price" and self.LimitPrice is not None:
            return self.LimitPrice
        raise ValueError("this fake knows only OrderField.StopPrice, and LimitPrice for a "
                         "stop-limit order")

    def Update(self, fields):
        """LEAN's amendment: acknowledged at once, reported after the slice."""
        self.book.updates.append((self.OrderId, fields.StopPrice, fields.Tag))
        if fields.LimitPrice is not None:
            self.book.limit_updates.append((self.OrderId, fields.LimitPrice))
        if not self.book.acknowledge_updates:
            return FakeResponse(False)
        if fields.StopPrice is not None:
            self.StopPrice = fields.StopPrice
        if fields.LimitPrice is not None:
            self.LimitPrice = fields.LimitPrice
        if fields.Tag is not None:
            self.Tag = fields.Tag
        self.book.deferred.append((self, "update-submitted"))
        return FakeResponse(True)

    def Cancel(self, tag=None):
        """LEAN's cancel: the book's cancel_outcome decides what LEAN answers.

        "pending" is what LEAN was observed to do: it answers success, reports
        CancelPending at once, and reports Canceled after the slice (settle).
        "never" answers the same but never confirms. "confirmed" cancels at
        once; "refused" answers with a failed response
        and leaves the order working. A tag, which LEAN would write over the
        order's own, is recorded so a test can refuse it.
        """
        self.book.cancellations.append((self.OrderId, tag))
        if self.book.cancel_outcome == "refused":
            return FakeResponse(False)
        if self.book.cancel_outcome in ("pending", "never"):
            self.Status = "cancel-pending"
            self.book.emit(self, "cancel-pending")
            self.book.deferred.append((self, "canceled"))
            return FakeResponse(True)
        self.Status = "canceled"
        self.book.emit(self, "canceled")
        return FakeResponse(True)


class FakeTransactions:
    """LEAN's order book: every ticket ever submitted, in any state."""
    def __init__(self, algorithm=None):
        self.algorithm = algorithm
        self.tickets = []
        self.updates = []
        self.limit_updates = []
        self.cancellations = []
        self.deferred = []
        self.acknowledge_updates = True
        self.cancel_outcome = "pending"
        self.submit_status = "submitted"
        self.now = None

    def emit(self, ticket, status, **fill):
        """Raise OnOrderEvent on the algorithm, as LEAN does, at the book's clock."""
        if self.algorithm is not None and hasattr(self.algorithm, "OnOrderEvent"):
            self.algorithm.OnOrderEvent(order_event(ticket, status, self.now, **fill))

    def settle(self):
        """The end of LEAN's time step: confirm cancellations and amendments."""
        deferred, self.deferred = self.deferred, []
        for ticket, status in deferred:
            if status == "canceled":
                if ticket.Status != "cancel-pending" or self.cancel_outcome == "never":
                    continue
                ticket.Status = "canceled"
            self.emit(ticket, status)

    def split_holding(self, symbol, factor):
        """LEAN's split of the holding under Raw normalisation, as observed on the
        pinned image (AAPL's 2-for-1 of 2005-02-28, factor 0.4999986): done
        before the split's slice reaches OnSplits and OnData, dividing the
        holding by the factor, truncated to whole shares with the remainder
        paid as cash."""
        portfolio = self.algorithm.Portfolio
        held = portfolio.holdings.get(symbol, 0)
        if held:
            portfolio.holdings[symbol] = int(held / factor)

    def split_orders(self, symbol, factor, tick=0.01, limit_rounding=round):
        """LEAN's split of the open orders, as observed on the pinned image: done
        after the split's slice's OnData returns, in the same time step, each
        order's quantity divided by the factor and its stop multiplied by it
        and rounded to the tick, each reported through OnOrderEvent as
        UpdateSubmitted with its ticket already changed."""
        for ticket in self.GetOpenOrderTickets(symbol):
            ticket.Quantity = round(ticket.Quantity / factor)
            ticket.StopPrice = round(round(ticket.StopPrice * factor / tick) * tick, 10)
            if ticket.LimitPrice is not None:
                # Confirmed by a probe on the pinned image (adapter README,
                # "Observed LEAN behaviour"): a stop-limit's limit is split
                # the same way as its stop, multiplied by the factor and
                # rounded to the cent in the same report. limit_rounding
                # picks the direction (round, math.floor or math.ceil): the
                # probe's own factor happened to round up both times, but the
                # exact tie-break for a value landing precisely on a
                # half-cent was not observed, so the code defends both ways
                # the rounding can land relative to the engine's cap.
                ticket.LimitPrice = round(limit_rounding(round(ticket.LimitPrice * factor / tick, 9)) * tick, 10)
            self.emit(ticket, "update-submitted")

    def GetOrderTickets(self, predicate=None):
        return Enumerable(t for t in self.tickets if predicate is None or predicate(t))

    def GetOpenOrderTickets(self, symbol=None):
        return Enumerable(t for t in self.tickets if t.Status not in CLOSED_STATUSES
                          and (symbol is None or t.Symbol == symbol))


class FakeSecurity:
    Symbol = "AAPL"
    def __init__(self, algorithm=None):
        self.algorithm = algorithm
    def SetSlippageModel(self, model):
        self.slippage_model = model
        if self.algorithm is not None:
            self.algorithm.model_calls.append(("SetSlippageModel", model))
    def SetFeeModel(self, model):
        self.fee_model = model
        if self.algorithm is not None:
            self.algorithm.model_calls.append(("SetFeeModel", model))
    def SetFillModel(self, model):
        self.fill_model = model
        if self.algorithm is not None:
            self.algorithm.model_calls.append(("SetFillModel", model))


class FakeAlgorithm:
    LiveMode = False
    def SetStartDate(self, *args): pass
    def SetEndDate(self, *args): pass
    def SetCash(self, cash):
        self.Portfolio = FakePortfolio(cash)
        # A test may give LEAN a holding before Initialize runs.
        self.Portfolio.holdings.update(getattr(self, "initial_holdings", {}))
    def SetTimeZone(self, *args): pass
    @property
    def model_calls(self):
        # Every SetBrokerageModel/SetSlippageModel/SetFeeModel call, in the
        # order Initialize made them: a test falsifies the order by asserting
        # on this list, since LEAN itself was observed to reset a security's
        # slippage model when SetBrokerageModel is called after it
        # (adapter/lean/README.md, "Observed LEAN behaviour").
        return self.__dict__.setdefault("_model_calls", [])
    def SetBrokerageModel(self, brokerage, account_type):
        self.brokerage_model = (brokerage, account_type)
        self.model_calls.append(("SetBrokerageModel", brokerage, account_type))
    def AddEquity(self, ticker, resolution, **kwargs):
        self.subscription = kwargs
        self.security = FakeSecurity(self)
        return self.security
    def SetWarmUp(self, count, resolution):
        self.warmup = (count, resolution)
    # LEAN's scheduling API: each rule is recorded as what it was built from,
    # and every scheduled callback is kept, so a test can assert on the
    # schedule and play the callback at the time LEAN would.
    DateRules = types.SimpleNamespace(EveryDay=lambda symbol: ("every-day", symbol))
    TimeRules = types.SimpleNamespace(At=lambda hour, minute: ("at", hour, minute))
    @property
    def Schedule(self):
        scheduled = self.__dict__.setdefault("scheduled", [])
        return types.SimpleNamespace(
            On=lambda date_rule, time_rule, callback: scheduled.append(
                (date_rule, time_rule, callback)))
    def Log(self, message): self.__dict__.setdefault("logs", []).append(message)
    def Quit(self, message): self.quit_reason = message
    @property
    def Transactions(self):
        return self.__dict__.setdefault("_transactions", FakeTransactions(self))
    @property
    def Securities(self):
        return self.__dict__.setdefault(
            "_securities", {"AAPL": types.SimpleNamespace(
                IsTradable=True,
                SymbolProperties=types.SimpleNamespace(MinimumPriceVariation=0.01))})
    # Every LEAN order entry point records its call, so a test can assert that
    # none was made rather than relying on the method being absent.
    def _order(self, *args, **kwargs):
        self.__dict__.setdefault("orders", []).append((args, kwargs))
    MarketOrder = LimitOrder = MarketOnOpenOrder = _order
    def StopLimitOrder(self, symbol, quantity, stop_price, limit_price, asynchronous=False,
                       tag="", order_properties=None):
        """LEAN's stop-limit entry point: (symbol, quantity, stop_price,
        limit_price, asynchronous, tag, order_properties), confirmed by a
        probe on the pinned image (adapter README, "Observed LEAN
        behaviour")."""
        if type(asynchronous) is not bool:
            raise TypeError("stop_limit_order: argument 5 ('asynchronous') expected bool, "
                            "got {}".format(type(asynchronous).__name__))
        self._order(symbol, quantity, stop_price, limit_price, tag, order_properties)
        book = self.Transactions
        ticket = FakeTicket(book, len(book.tickets) + 1, symbol, quantity,
                            stop_price, tag, order_properties, limit_price=limit_price)
        book.tickets.append(ticket)
        book.emit(ticket, "invalid" if ticket.Status == "invalid" else "submitted")
        return ticket
    def StopMarketOrder(self, symbol, quantity, stop_price, asynchronous=False, tag="",
                        order_properties=None):
        """LEAN's signature, observed on the pinned image: the fourth argument is
        the bool 'asynchronous', and LEAN refuses anything else there."""
        if type(asynchronous) is not bool:
            raise TypeError("stop_market_order: argument 4 ('asynchronous') expected bool, "
                            "got {}".format(type(asynchronous).__name__))
        self._order(symbol, quantity, stop_price, tag, order_properties)
        book = self.Transactions
        ticket = FakeTicket(book, len(book.tickets) + 1, symbol, quantity,
                            stop_price, tag, order_properties)
        book.tickets.append(ticket)
        # LEAN reports the submission before StopMarketOrder returns.
        book.emit(ticket, "invalid" if ticket.Status == "invalid" else "submitted")
        return ticket


class OrderProperties:
    TimeInForce = None


class UpdateOrderFields:
    StopPrice = None
    LimitPrice = None
    Tag = None


class InteractiveBrokersFeeModel:
    pass


class EquityFillModel:
    """LEAN's EquityFillModel, as far as the adapter's fill model uses it: the
    protected helpers it calls (whether the exchange is open, and the bar's
    prices) answer from the security, and its own StopLimitFill (LEAN's
    native fill) is recorded, never priced."""
    def __init__(self):
        self.native_calls = []
    def StopLimitFill(self, asset, order):
        self.native_calls.append(order)
        return ("lean's own stop-limit fill", order)
    def IsExchangeOpen(self, asset, is_extended_market_hours):
        return asset.exchange_open
    def GetPricesCheckingPythonWrapper(self, asset, direction):
        return asset.prices


class OrderEvent:
    """LEAN's OrderEvent(order, utc_time, order_fee): unfilled until set."""
    def __init__(self, order, utc_time, order_fee):
        self.OrderId = order.Id
        self.UtcTime = utc_time
        self.OrderFee = order_fee
        self.Status = None
        self.FillQuantity = 0
        self.FillPrice = 0


imports = types.ModuleType("AlgorithmImports")
imports.QCAlgorithm = FakeAlgorithm
imports.Resolution = types.SimpleNamespace(Daily="daily")
imports.DataNormalizationMode = types.SimpleNamespace(SplitAdjusted="split", Raw="raw")
imports.BrokerageName = types.SimpleNamespace(InteractiveBrokersBrokerage="interactive-brokers")
imports.AccountType = types.SimpleNamespace(Cash="cash", Margin="margin")
imports.TimeZones = types.SimpleNamespace(NewYork="NY")
imports.DelistingType = types.SimpleNamespace(Warning="warning", Delisted="delisted")
imports.SplitType = types.SimpleNamespace(Warning="split-warning", SplitOccurred="split-occurred")
imports.OrderField = types.SimpleNamespace(StopPrice="stop-price", LimitPrice="limit-price")
imports.time = time
imports.OrderProperties = OrderProperties
imports.UpdateOrderFields = UpdateOrderFields
imports.InteractiveBrokersFeeModel = InteractiveBrokersFeeModel
imports.EquityFillModel = EquityFillModel
imports.OrderEvent = OrderEvent
imports.OrderFee = types.SimpleNamespace(Zero="no fee")
imports.OrderDirection = types.SimpleNamespace(Buy="buy", Sell="sell")
# Extensions.ConvertToUtc: the fakes' exchange times are already UTC.
imports.Extensions = types.SimpleNamespace(ConvertToUtc=lambda moment, time_zone: moment)
imports.TimeInForce = types.SimpleNamespace(Day="day", GoodTilCanceled="gtc")
imports.OrderStatus = types.SimpleNamespace(
    New="new", Submitted="submitted", PartiallyFilled="partially-filled", Filled="filled",
    Canceled="canceled", CancelPending="cancel-pending", UpdateSubmitted="update-submitted",
    Invalid="invalid")
sys.modules["AlgorithmImports"] = imports
spec = importlib.util.spec_from_file_location("lean_algorithm", Path(__file__).parents[1] / "algorithm.py")
algorithm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(algorithm)


def slice_of(bars=None, delistings=None, changes=None, splits=None):
    """A LEAN data slice: LEAN always provides all four collections."""
    return types.SimpleNamespace(Bars=bars or {}, Delistings=delistings or {},
                                 SymbolChangedEvents=changes or {}, Splits=splits or {})


class AlgorithmTests(unittest.TestCase):
    def init(self):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3, "cash": 1000000, "lean_image": VALID_LEAN_IMAGE,
                    "slippage_n": 0.05}
        algo = algorithm.CompletedBarsAlgorithm()
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client", return_value=FakeEngineClient()):
            algo.Initialize()
        return algo

    def test_warmup_is_three_bars_across_weekend_and_not_three_days(self):
        algo = self.init()
        self.assertEqual(algo.warmup, (3, "daily"))
        # ADR 0004: LEAN trades and accounts in raw prices and shares.
        self.assertEqual(algo.subscription["dataNormalizationMode"], "raw")
        self.assertFalse(algo.subscription["fillForward"])
        seen = []
        algo.publisher = types.SimpleNamespace(sequence=2, publish=lambda *args: seen.append(args) or [],
                                               publish_session_closed=lambda *args: [],
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
        algo.OnEndOfAlgorithm()
        self.assertEqual([e["sequence"] for e in client.sent], list(range(2, 15)))
        self.assertEqual(algo.warmup_seen, 3)
        self.assertEqual(algo.bar_count, 4)
        # FakeEngineClient answers every input with zero decisions; all four
        # bars, the closes of their Sessions and their snapshots reach the
        # engine in one sequence (ADR 0021: the close before the snapshot).
        # Each snapshot is sent before the next bar, after anything LEAN
        # reported in between (flush_snapshot); the last one before the end
        # of the stream.
        self.assertEqual(algo.decision_count, 0)
        self.assertEqual([e["type"] for e in client.sent],
                         ["market.bar.completed", "market.session.closed", "account.snapshot"] * 4
                         + ["replay.run.completed"])
        ends = []
        for completed, closed, snapshot in zip(client.sent[:-1:3], client.sent[1::3], client.sent[2::3]):
            end = completed["payload"]["period_end"]
            self.assertEqual(closed["payload"], {"period_end": end, "instrument_ids": ["AAPL"]})
            self.assertEqual(snapshot["payload"]["as_of"], end)
            self.assertEqual(snapshot["recorded_at"], end)
            ends.append(end)
        self.assertTrue(all(a < b for a, b in zip(ends, ends[1:])))
        self.assertEqual(ends[-1], "2014-06-09T20:00:00Z")

    def test_a_bar_carries_leans_raw_bar_and_its_split_adjusted_history(self):
        """ADR 0004: LEAN trades raw, so its subscription bar is the raw view,
        and the split-adjusted view every signal reads is LEAN's own
        split-adjusted History for the same bar, never arithmetic here."""
        algo = self.init()
        requests = []
        b = bar(6)

        def history(symbols, count, resolution, **kwargs):
            requests.append((symbols, count, resolution, kwargs))
            return Frame(b.EndTime, ratio=28)
        algo.History = history
        algo.IsWarmingUp = True
        algo.OnData(slice_of({"AAPL": b}))
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(requests, [(["AAPL"], 1, "daily", {"dataNormalizationMode": "split"})])
        [completed] = [e for e in algo.client.sent if e["type"] == "market.bar.completed"]
        self.assertEqual(completed["payload"]["raw"], {
            "view": "raw", "open": 23.0, "high": 24.0, "low": 22.0, "close": 23.056,
            "volume": 2800.0})
        self.assertEqual(completed["payload"]["split_adjusted"], {
            "view": "split-adjusted", "open": 23 / 28, "high": 24 / 28, "low": 22 / 28,
            "close": 23.056 / 28, "volume": 2800.0 * 28})

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
        algo.OnEndOfAlgorithm()
        self.assertFalse(algo.failed)
        [snapshot] = [e for e in algo.client.sent if e["type"] == "account.snapshot"]
        self.assertEqual(snapshot["payload"], {
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
                algo.OnData(slice_of({"AAPL": bar(6)}))
                self.assertFalse(algo.failed)
                # The 6th's close is sent before the next bar, and its bad
                # reply stops the run before that bar is sent.
                algo.History = lambda *args, **kwargs: Frame(bar(9).EndTime)
                data = slice_of({"AAPL": bar(9)})
                algo.OnData(data)
                self.assertTrue(algo.failed)
                self.assertTrue(algo.client.closed)
                self.assertEqual(len(algo.client.sent), 3)
                self.assertEqual(algo.client.sent[-1]["type"], "account.snapshot")
                self.assertEqual(algo.publisher.sequence, 3)
                algo.OnData(data)
                self.assertEqual(len(algo.client.sent), 3)

    def test_bad_session_close_reply_sends_no_snapshot(self):
        algo = self.init()
        algo.client.reply_overrides = {"market.session.closed": {"sequence": 99}}
        algo.IsWarmingUp = True
        algo.History = lambda *args, **kwargs: Frame(bar(6).EndTime)
        algo.OnData(slice_of({"AAPL": bar(6)}))
        self.assertTrue(algo.failed)
        self.assertEqual([e["type"] for e in algo.client.sent],
                         ["market.bar.completed", "market.session.closed"])

    def test_bad_bar_reply_sends_no_snapshot(self):
        algo = self.init()
        algo.client.reply_overrides = {"market.bar.completed": {"sequence": 99}}
        algo.IsWarmingUp = True
        algo.History = lambda *args, **kwargs: Frame(bar(6).EndTime)
        algo.OnData(slice_of({"AAPL": bar(6)}))
        self.assertTrue(algo.failed)
        self.assertEqual(len(algo.client.sent), 1)

    def test_missing_split_adjusted_history_stops_stream_without_reusing_connection(self):
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
                    "warmup_bars": 3, "cash": 1000000, "lean_image": VALID_LEAN_IMAGE,
                    "slippage_n": 0.05}
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
                                               publish_session_closed=lambda *args: [],
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


class BrokerageModelTests(unittest.TestCase):
    """ADR 0010: no partial Units, no borrowing. LEAN runs on a cash account,
    so LEAN itself refuses an order it cannot fund, and the adapter's own
    fee and slippage models still apply once that account type is set."""

    def test_the_brokerage_model_is_set_with_a_cash_account(self):
        algo = AlgorithmTests.init(self)
        self.assertEqual(algo.brokerage_model,
                         (algorithm.BrokerageName.InteractiveBrokersBrokerage,
                          algorithm.AccountType.Cash))

    def test_the_fee_and_slippage_models_are_still_the_adapters_own(self):
        algo = AlgorithmTests.init(self)
        self.assertIsInstance(algo.security.slippage_model, algorithm.NSlippageModel)
        self.assertIsInstance(algo.security.fee_model, InteractiveBrokersFeeModel)

    def test_capped_buys_fill_by_the_adapters_adr_0005_model_slipped_by_its_slippage_model(self):
        # ADR 0005, as amended 2026-09-24, in place of LEAN's native
        # stop-limit fill; a subclass of LEAN's EquityFillModel, so every
        # other order keeps LEAN's own equity fill.
        algo = AlgorithmTests.init(self)
        model = algo.security.fill_model
        self.assertIsInstance(model, EquityFillModel)
        self.assertIn("StopLimitFill", vars(type(model)))
        self.assertIs(model, algo.fill_model)
        self.assertIs(model.slippage_model, algo.security.slippage_model)

    def test_the_brokerage_model_is_set_before_the_fee_and_slippage_models(self):
        """LEAN was observed to reset a security's slippage model back to its
        own default when SetBrokerageModel is called after the security's own
        model is set (README.md, "Observed LEAN behaviour"), so Initialize
        must set the brokerage model first."""
        algo = AlgorithmTests.init(self)
        kinds = [call[0] for call in algo.model_calls]
        self.assertIn("SetBrokerageModel", kinds)
        brokerage_index = kinds.index("SetBrokerageModel")
        for kind in ("SetSlippageModel", "SetFillModel", "SetFeeModel"):
            self.assertGreater(kinds.index(kind), brokerage_index,
                               "{} must be set after SetBrokerageModel".format(kind))


class CashSettingTests(unittest.TestCase):
    """Starting cash comes from the run's settings, never a built-in default."""
    base = {"socket": "unused", "configuration_hash": "hash",
            "strategy_version": "version", "run_id": "test", "symbol": "AAPL",
            "start": "2014-06-09", "end": "2014-06-10", "warmup_bars": 3,
            "lean_image": VALID_LEAN_IMAGE, "slippage_n": 0.05}

    def start(self, settings):
        algo = algorithm.CompletedBarsAlgorithm()
        def set_cash(value):
            algo.cash = value
            FakeAlgorithm.SetCash(algo, value)
        algo.SetCash = set_cash
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


class LeanImageValidationTests(unittest.TestCase):
    """validate_lean_image is a pure function: no LEAN state is needed to test it.

    A LEAN run is evidence (ADR 0012, ADR 0017); only the digest form ties a
    run to the exact engine that produced it, so a moving tag is rejected
    alongside anything malformed.
    """

    def test_a_valid_digest_is_accepted(self):
        self.assertEqual(algorithm.validate_lean_image(VALID_LEAN_IMAGE), VALID_LEAN_IMAGE)

    def test_a_missing_image_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image(None)

    def test_a_moving_tag_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image("quantconnect/lean:latest")

    def test_a_numeric_moving_tag_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image("quantconnect/lean:17490")

    def test_uppercase_hex_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image("quantconnect/lean@sha256:" + "A" * 64)

    def test_63_hex_characters_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image("quantconnect/lean@sha256:" + "a" * 63)

    def test_a_wrong_repository_is_rejected(self):
        with self.assertRaises(ValueError):
            algorithm.validate_lean_image("quantconnect/lean-cli@sha256:" + "a" * 64)


class LeanImageSettingTests(unittest.TestCase):
    """The engine image comes from the run's own settings, never a default.

    A LEAN run is evidence (ADR 0012, ADR 0017): pinning by digest, and
    logging that digest at startup, is what ties a run to the exact engine
    that produced it.
    """
    base = {"socket": "unused", "configuration_hash": "hash",
            "strategy_version": "version", "run_id": "test", "symbol": "AAPL",
            "start": "2014-06-09", "end": "2014-06-10", "warmup_bars": 3,
            "cash": 1000000, "slippage_n": 0.05}

    def start(self, settings):
        algo = algorithm.CompletedBarsAlgorithm()
        with patch.object(algorithm, "load_settings", return_value=settings), \
                patch.object(algorithm, "Client"):
            algo.Initialize()
        return algo

    def test_a_valid_image_is_accepted_and_logged_at_startup(self):
        algo = self.start(dict(self.base, lean_image=VALID_LEAN_IMAGE))
        self.assertFalse(algo.failed)
        self.assertIn("adapter: lean_image=" + VALID_LEAN_IMAGE, algo.logs)

    def test_missing_lean_image_fails_closed(self):
        algo = self.start(dict(self.base))
        self.assertTrue(algo.failed)
        self.assertIn("lean_image", algo.quit_reason)

    def test_a_moving_tag_fails_closed(self):
        algo = self.start(dict(self.base, lean_image="quantconnect/lean:latest"))
        self.assertTrue(algo.failed)
        self.assertIn("lean_image", algo.quit_reason)


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
        # delisting itself is never published. adapter.run.stopped records
        # the deliberate stop, immediately before the stream's own
        # completion, which expires anything outstanding; the run then stops.
        self.assertEqual([e["type"] for e in algo.client.sent],
                         ["market.bar.completed", "market.session.closed", "account.snapshot"] * 2
                         + ["adapter.run.stopped", "replay.run.completed"])
        self.assertTrue(algo.failed)
        self.assertIn("AAPL DELISTED", algo.quit_reason)

    def test_delisted_on_a_barless_slice_stops_and_publishes_nothing(self):
        algo = self.start()
        self.feed(algo, 6)
        sent = len(algo.client.sent)
        algo.OnData(slice_of({}, delistings={"AAPL": self.notice("delisted")}))
        # The 6th's close, then the stop and the stream's completion, all
        # stamped at the last bar this run actually received.
        self.assertEqual([e["type"] for e in algo.client.sent[sent:]],
                         ["account.snapshot", "adapter.run.stopped", "replay.run.completed"])
        for envelope in algo.client.sent[sent:]:
            self.assertEqual(envelope["event_time"], "2014-06-06T20:00:00Z")
        self.assertTrue(algo.failed)

    def test_delisted_sends_stop_then_completed_in_order_with_sequence_numbers(self):
        """The stop is sent, then the completion, in that
        order, continuing the one input sequence — never the other way
        round and never with a gap or a repeat."""
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9, delistings={"AAPL": self.notice("delisted")})
        stop, completed = algo.client.sent[-2], algo.client.sent[-1]

        self.assertEqual(stop["type"], "adapter.run.stopped")
        self.assertEqual(stop["schema_version"], 1)
        self.assertEqual(stop["payload"], {
            "reason": "delisted", "instrument_id": "AAPL",
            "detail": stop["payload"]["detail"]})
        self.assertIn("AAPL DELISTED", stop["payload"]["detail"])

        self.assertEqual(completed["type"], "replay.run.completed")

        # Sequences continue the one contiguous stream with no gap: two
        # bar/close/snapshot triples, then the stop, then the completion.
        self.assertEqual([e["sequence"] for e in algo.client.sent], list(range(2, 10)))
        self.assertEqual(stop["sequence"] + 1, completed["sequence"])
        self.assertEqual(stop["event_time"], completed["event_time"])

    def test_a_clean_end_sends_no_stop(self):
        """A run that simply reaches its last bar sends no adapter.run.stopped
        at all: the event exists only for a DELIBERATE stop."""
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9)
        algo.OnEndOfAlgorithm()
        self.assertNotIn("adapter.run.stopped", [e["type"] for e in algo.client.sent])

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
        # bar, close, the 6th's snapshot, bar, close: the 9th's snapshot
        # waits for the next slice (flush_snapshot).
        self.assertEqual(len(algo.client.sent), 5)
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
                         ["market.bar.completed", "market.session.closed"])
        self.assertTrue(any("symbol changed GOOAV -> GOOG" in m for m in algo.logs))


class RunCompletionTests(unittest.TestCase):
    """replay.run.completed ends every cleanly finished stream exactly once.

    Without it the reducer never expires the last bar's proposal, so the
    journal ends with a proposal that never reaches a terminal event
    (event.RunCompletedEventType).
    """

    def start(self):
        algo = AlgorithmTests.init(self)
        algo.IsWarmingUp = False
        return algo

    def feed(self, algo, day):
        b = bar(day)
        algo.History = lambda *args, **kw: Frame(b.EndTime)
        algo.OnData(slice_of({"AAPL": b}))

    def test_the_normal_end_completes_the_stream_at_the_last_bar(self):
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9)
        algo.OnEndOfAlgorithm()
        last = algo.client.sent[-1]
        self.assertEqual(last["type"], "replay.run.completed")
        self.assertEqual(last["event_time"], "2014-06-09T20:00:00Z")
        self.assertEqual(last["payload"], {})
        self.assertEqual([e["sequence"] for e in algo.client.sent], list(range(2, 9)))

    def test_completion_is_sent_once_even_after_a_deliberate_stop(self):
        algo = self.start()
        self.feed(algo, 6)
        algo.OnData(slice_of({}, delistings={"AAPL": types.SimpleNamespace(
            Type="delisted", Time=datetime(2014, 6, 9))}))
        algo.OnEndOfAlgorithm()
        self.assertEqual([e["type"] for e in algo.client.sent].count("replay.run.completed"), 1)

    def test_an_empty_run_sends_no_completion(self):
        algo = self.start()
        algo.OnEndOfAlgorithm()
        self.assertEqual(algo.client.sent, [])

    def test_a_broken_stream_sends_no_completion(self):
        """After a failure the stream may be out of step with the engine."""
        algo = self.start()
        self.feed(algo, 6)
        algo.History = lambda *args, **kw: None  # the split-adjusted view is missing: the bar fails
        algo.OnData(slice_of({"AAPL": bar(9)}))
        self.assertTrue(algo.failed)
        algo.OnEndOfAlgorithm()
        self.assertNotIn("replay.run.completed", [e["type"] for e in algo.client.sent])


class CompletionFailureTests(unittest.TestCase):
    def test_a_failed_completion_keeps_its_own_stop_reason(self):
        """If completing the stream fails during a delisting stop, the quit
        reason is the completion failure, not the delisting."""
        algo = AlgorithmTests.init(self)
        algo.IsWarmingUp = False
        b = bar(6)
        algo.History = lambda *args, **kw: Frame(b.EndTime)
        algo.OnData(slice_of({"AAPL": b}))

        def refuse():
            raise ValueError("engine refused the completion")
        algo.publisher.publish_run_completed = refuse
        algo.OnData(slice_of({}, delistings={"AAPL": types.SimpleNamespace(
            Type="delisted", Time=datetime(2014, 6, 9))}))
        self.assertTrue(algo.failed)
        self.assertIn("run completion failed", algo.quit_reason)
        self.assertNotIn("DELISTED", algo.quit_reason)
