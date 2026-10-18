"""Tests for the remaining parity exports (docs/research/parity-2026-10.md,
"Remaining scope, plan, 2026-10-19"): momentum/parity_export.py, which ports
the IRA planner and the buy scaling from Go and builds each export, and the
three logging templates that use it (glide_parity_log.py,
minimum_lot_parity_log.py, ira_parity_log.py). The planner and scaling
cases transcribe internal/orderplan/ira_test.go. Standard library only;
synthetic values, no market data.
"""
import datetime as _dt
import importlib.util
import json
import os
import sys
import types
import unittest
from collections import namedtuple

import build_variants
from test_followup import (HERE, _Ticket, _Transactions, _additions_only, _class_constants, _limit_orders,
                           _load_template)

sys.path.insert(0, os.path.join(HERE, "momentum"))


def _export_module():
    spec = importlib.util.spec_from_file_location("parity_export", os.path.join(HERE, "momentum", "parity_export.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _decode(tag, prefix):
    assert tag.startswith(prefix), tag[:20]
    return json.loads(tag[len(prefix):])


class IRAPlanPortTest(unittest.TestCase):
    """parity_export.ira_plan is orderplan.PlanIRA, case for case."""

    def setUp(self):
        self.x = _export_module()

    def plan(self, targets, nlv, prices, held=None, working=None):
        return self.x.ira_plan(targets, held or {}, prices, nlv, 0.25, 0.05, working or {})

    def test_the_band_at_exactly_a_quarter_of_the_target_does_not_trade(self):
        # Go: TestIRABandAtExactlyAQuarterOfTheTargetDoesNotTrade.
        t, p = {"AAA": 0.5}, {"AAA": 10}
        self.assertEqual(self.plan(t, 10000, p, {"AAA": 625}), ([], [], []))
        self.assertEqual(self.plan(t, 10000, p, {"AAA": 626})[0], [["AAA", 126, 9.5]])
        self.assertEqual(self.plan(t, 10000, p, {"AAA": 375}), ([], [], []))
        self.assertEqual(self.plan(t, 10000, p, {"AAA": 374})[1], [["AAA", 126, 10.5]])

    def test_entries_exits_and_sells_first_in_instrument_order(self):
        # Go: TestIRAEntriesExitsAndSellsFirstInInstrumentOrder.
        t = {"AAA": 0.25, "BBB": 0.24, "DDD": 0.25, "IEF": 0.24}
        p = {"AAA": 50, "BBB": 30, "CCC": 20, "DDD": 25, "IEF": 100}
        sells, buys, skipped = self.plan(t, 10000, p, {"CCC": 40, "DDD": 100})
        self.assertEqual(sells, [["CCC", 40, 19.0]])
        self.assertEqual(buys, [["AAA", 50, 52.5], ["BBB", 80, 31.5], ["IEF", 24, 105.0]])
        self.assertEqual(skipped, [])

    def test_never_sells_beyond_the_holding_net_of_working_sells(self):
        # Go: TestIRANeverSellsBeyondTheHoldingNetOfWorkingSells.
        sells, _, skipped = self.plan({"AAA": 0.1}, 10000, {"AAA": 10, "BBB": 10}, {"AAA": 500, "BBB": 30},
                                      {"AAA": 120, "BBB": 30})
        self.assertEqual(sells, [["AAA", 380, 9.5]])
        self.assertEqual(skipped, [["BBB", "sell", "held-by-working-sells", 30.0]])

    def test_a_zero_target_not_held_is_no_trade(self):
        # Go: TestIRAZeroTargetNotHeldIsNoTrade.
        self.assertEqual(self.plan({"AAA": 0.0, "BBB": 0.5}, 1000, {"AAA": 7, "BBB": 7}),
                         ([], [["BBB", 71, 7.35]], []))

    def test_rounds_toward_zero_and_records_what_rounds_to_nothing(self):
        # Go: TestIRARoundsTowardZero.
        _, buys, _ = self.plan({"AAA": 0.33, "BBB": 0.5}, 1000, {"AAA": 7, "BBB": 7}, {"BBB": 30})
        self.assertEqual([b[:2] for b in buys], [["AAA", 47], ["BBB", 41]])
        self.assertEqual(self.plan({"AAA": 0.001}, 1000, {"AAA": 7}),
                         ([], [], [["AAA", "buy", "below-one-share", 0.001 * 1000 / 7]]))
        _, buys, skipped = self.plan({"AAA": 0.2}, 1000, {"AAA": 120}, {"AAA": 1})
        self.assertEqual((buys, [s[:3] for s in skipped]), ([], [["AAA", "buy", "below-one-share"]]))

    def test_the_limit_is_the_bands_edge_rounded_to_a_cent_toward_the_reference(self):
        # Go: LimitPrice. 33.33 x 1.05 = 34.9965 rounds down, x 0.95 = 31.6635 up.
        self.assertEqual(self.x.limit_price("buy", 33.33, 0.05), 34.99)
        self.assertEqual(self.x.limit_price("sell", 33.33, 0.05), 31.67)
        self.assertEqual(self.x.limit_price("buy", 0.001, 0.05), 0.001)   # never below the reference


class ScaleBuysPortTest(unittest.TestCase):
    """parity_export.scale_buys is orderplan.ScaleBuys (Go: TestScaleBuys)."""

    BUYS = [["AAA", 10, 10.0], ["BBB", 3, 100.0], ["CCC", 1, 50.0]]

    def setUp(self):
        self.x = _export_module()

    def test_buys_that_fit_are_kept_at_scale_one(self):
        self.assertEqual(self.x.scale_buys(self.BUYS, 450, 0), (self.BUYS, [], 1.0))

    def test_half_the_budget_halves_and_records_the_buy_scaled_to_nothing(self):
        kept, skipped, scale = self.x.scale_buys(self.BUYS, 225, 0)
        self.assertEqual((kept, skipped, scale), ([["AAA", 5, 10.0], ["BBB", 1, 100.0]], [["CCC", 0.5]], 0.5))

    def test_fees_count_in_the_total(self):
        kept, _, scale = self.x.scale_buys(self.BUYS, 240, 10)
        self.assertEqual((kept, scale), ([["AAA", 5, 10.0], ["BBB", 1, 100.0]], 0.5))

    def test_the_fee_aware_scale_when_the_adrs_scale_leaves_the_total_over(self):
        kept, _, scale = self.x.scale_buys([["AAA", 10, 10.0]], 100, 50)
        self.assertEqual((kept, scale), ([["AAA", 5, 10.0]], 0.5))

    def test_no_cash_or_a_debit_buys_nothing(self):
        for budget in (0, -100):
            kept, skipped, scale = self.x.scale_buys(self.BUYS, budget, 1)
            self.assertEqual((kept, scale, len(skipped)), ([], 0.0, 3), budget)

    def test_no_buy_is_scale_one_with_cash_and_zero_with_a_debit(self):
        self.assertEqual(self.x.scale_buys([], 10, 2), ([], [], 1.0))
        self.assertEqual(self.x.scale_buys([], -10, 2), ([], [], 0.0))


class ExportRecordsTest(unittest.TestCase):
    def setUp(self):
        self.x = _export_module()
        self.sym = namedtuple("Sym", "Value ID")

    def test_names_are_tickers_and_a_shared_ticker_is_its_security_id(self):
        a, a2, b = self.sym("A", "A1"), self.sym("A", "A2"), self.sym("B", "B1")
        name = self.x.namer([a, a2, b, b])
        self.assertEqual((name(a), name(a2), name(b), name("SPY")), ("A1", "A2", "B", "SPY"))

    def test_the_glide_record_carries_the_age_share_book_and_targets(self):
        r = _decode(self.x.glide_record("2010-03", "2010-03-01", "path", 53.0, "2002-08", 53.0 + 91 / 12,
                                        0.308 + 0.016 * (91 / 12), {"A": 0.49, "IEF": 0.49},
                                        {"A": 0.343, "IEF": 0.49, "SHY": 0.147}, str), "GLIDE1 ")
        self.assertEqual(r, {"month": "2010-03", "date": "2010-03-01", "glide": "path", "start_age": 53.0,
                             "measure_from": "2002-08", "age": 53.0 + 91 / 12, "b": 0.308 + 0.016 * (91 / 12),
                             "book": {"A": 0.49, "IEF": 0.49}, "targets": {"A": 0.343, "IEF": 0.49, "SHY": 0.147}})

    def test_the_lot_record_carries_the_inputs_and_the_result(self):
        r = _decode(self.x.lot_record("2020-04", "held", 30000.0, True, ["B", "A"], {"A": 0.5, "B": 0.6},
                                      {"A": 0.01, "B": 0.02}, {"A": 311, "B": None}, {"A": 120.5, "B": 10.0},
                                      {"A": 3}, {"B": 0.49, "IEF": 0.49}, ["C"], {"A"}, str), "LOTPARITY1 ")
        self.assertEqual(r, {"month": "2020-04", "mode": "held", "nlv": 30000.0, "risk_off": True,
                             "ranked": [["B", 0.6, 0.02, None], ["A", 0.5, 0.01, 311]],
                             "prices": {"A": 120.5, "B": 10.0}, "held": {"A": 3},
                             "weights": {"B": 0.49, "IEF": 0.49}, "skipped": ["C"], "kept": ["A"]})

    def test_a_withheld_lot_month_has_null_weights(self):
        r = _decode(self.x.lot_record("2020-04", "held", 1.0, False, [], {}, {}, None, {}, {}, None, ["C"], set(),
                                      str), "LOTPARITY1 ")
        self.assertIsNone(r["weights"])

    def test_numbers_round_trip(self):
        r = _decode(self.x.glide_record("m", "d", "fixed", 38.0, "2002-08", 38.0, 0.1 + 0.2, {}, {}, str),
                    "GLIDE1 ")
        self.assertEqual(r["b"], 0.1 + 0.2)

    def test_the_tag_goes_on_the_newest_open_order_unless_a_cancelled_order_is_asked_for(self):
        algo = types.SimpleNamespace(spy="SPY", Securities={"SPY": types.SimpleNamespace(Price=300.0)})
        old, new = _Ticket(7), _Ticket(12)
        algo.Transactions = _Transactions([new, old])
        _limit_orders(algo)
        self.x.attach(algo, "T1")
        self.assertEqual((new.tags, old.tags, algo.limits), (["T1"], [], []))
        self.x.attach(algo, "T2", dummy=True)
        self.assertEqual(algo.limits, [{"sym": "SPY", "qty": 1, "price": 3.0, "tag": "T2", "cancelled": True}])
        self.assertEqual(self.x.stat(algo), "logged=2 bytes=4 dummies=1 lost=0")

    def test_an_open_order_already_carrying_an_export_is_never_overwritten(self):
        # Its export is the only copy: a new one goes on its own order.
        algo = types.SimpleNamespace(spy="SPY", Securities={"SPY": types.SimpleNamespace(Price=300.0)})
        # A working IRA order's tag names its month and phase, which the
        # comparison reads, so it is never overwritten either.
        for prefix in ("PARITY1 ", "GLIDE1 ", "LOTPARITY1 ", "IRAPARITY1 ", "IRAORD1 "):
            carrier = _Ticket(12, tag=prefix + "{}")
            algo.Transactions = _Transactions([carrier])
            _limit_orders(algo)
            self.x.attach(algo, "T")
            self.assertEqual((carrier.tags, carrier.Tag, [r["tag"] for r in algo.limits]), ([], prefix + "{}", ["T"]))

    def test_a_refused_carrier_is_counted_as_lost_not_logged(self):
        algo = types.SimpleNamespace(spy="SPY", Securities={"SPY": types.SimpleNamespace(Price=0.0)})
        algo.Transactions = _Transactions([])
        _limit_orders(algo, refused=True)
        self.assertFalse(self.x.attach(algo, "T", dummy=True))
        self.assertEqual(self.x.stat(algo), "logged=0 bytes=0 dummies=1 lost=1")
        _limit_orders(algo)
        self.assertTrue(self.x.attach(algo, "T", dummy=True))

    def test_with_no_open_order_a_cancelled_order_carries_it(self):
        algo = types.SimpleNamespace(spy="SPY", Securities={"SPY": types.SimpleNamespace(Price=300.0)})
        algo.Transactions = _Transactions([_Ticket(3, ok=False)])
        _limit_orders(algo)
        self.x.attach(algo, "T")
        self.assertEqual(len(algo.limits), 1)
        self.assertEqual(self.x.stat(algo), "logged=1 bytes=1 dummies=1 lost=0")

    def test_the_momentum_record_is_parity_logs(self):
        # The same function, so a P38 glide run's PARITY1 export decodes as
        # FU_PARITY's does.
        module = _load_template(self, "momentum/parity_log.py", {})
        import inspect
        self.assertEqual(inspect.getsource(self.x.parity_record), inspect.getsource(module.parity_record))
        self.assertEqual(inspect.getsource(self.x.parity_marks), inspect.getsource(module.parity_marks))


class _Holding:
    def __init__(self, quantity, price):
        self.Quantity, self.Invested, self.IsLong, self.IsShort = quantity, quantity != 0, quantity > 0, quantity < 0
        self.HoldingsValue = quantity * price


class _Book(dict):
    def __init__(self, held, prices, cash):
        super().__init__({k: _Holding(q, prices[k]) for k, q in held.items()})
        self.Cash = cash
        self.TotalPortfolioValue = cash + sum(h.HoldingsValue for h in self.values())

    def __iter__(self):
        return iter([types.SimpleNamespace(Key=k, Value=v) for k, v in dict.items(self)])

    def __missing__(self, key):
        return _Holding(0, 0.0)


class _Column(list):
    def dropna(self):
        return _Column(x for x in self if x == x)

    @property
    def iloc(self):
        return self


class _Closes:
    def __init__(self, columns):
        self.cols = columns
        self.columns = list(columns)

    def __getitem__(self, k):
        return _Column(self.cols[k])


class _Renamed:
    """A symbol equal to another of the same security ID whatever its
    ticker, as LEAN's are: a holding's symbol can keep the ticker a stock had
    when bought after the universe's symbol has the new one."""

    def __init__(self, value, sid):
        self.Value, self.ID = value, sid

    def __eq__(self, other):
        return isinstance(other, _Renamed) and other.ID == self.ID

    def __hash__(self):
        return hash(self.ID)

    def __repr__(self):
        return self.Value


class _Lean:
    """LEAN's order events as the IRA template meets them: a market order
    works until the open fills it; CancelOpenOrders makes each open order of
    a symbol cancel-pending, still working (it may yet fill) until LEAN
    confirms the cancel."""

    def __init__(self, algo):
        self.algo, self.orders = algo, []
        algo.Transactions, algo.MarketOrder = self, self.market_order

    def market_order(self, sym, qty, tag=""):
        t = types.SimpleNamespace(OrderId=500 + len(self.orders), Symbol=sym, Quantity=qty, QuantityFilled=0, Tag=tag,
                                  status="submitted")
        self.orders.append(t)
        return t

    def GetOpenOrderTickets(self):
        return [t for t in self.orders if t.status in ("submitted", "cancelPending")]

    def CancelOpenOrders(self, sym):
        for t in self.GetOpenOrderTickets():
            if t.Symbol == sym:
                t.status = "cancelPending"

    def confirm_cancels(self):
        for t in self.orders:
            if t.status == "cancelPending":
                t.status = "canceled"

    def fill(self, t):
        self.algo.Portfolio[t.Symbol].Quantity += t.Quantity
        t.QuantityFilled, t.status = t.Quantity, "filled"

    def open_fills(self):
        for t in self.GetOpenOrderTickets():
            if t.status == "submitted":
                self.fill(t)


class IRATemplateTest(unittest.TestCase):
    """momentum/ira_parity_log.py: fresh_universe.py with the month's book
    traded by the ported IRA planner, the buys EXEC_DELAY sessions after the
    sells and scaled to the cash on hand then."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/ira_parity_log.py", {})

    def _algo(self, held, cash, open_tickets=()):
        algo = self.module.MomentumResearch()
        algo.IRA_EXEC, algo.EXEC_DELAY, algo.PRICE_BAND, algo.MAX_FEES = True, 1, 0.05, 2.0
        algo.REBALANCE_BAND, algo.spy, algo.bond = 0.25, "SPY", "IEF"
        prices = {"AAA": 50.0, "BBB": 30.0, "CCC": 20.0, "DDD": 25.0, "IEF": 100.0, "SPY": 300.0}
        algo.Securities = {k: types.SimpleNamespace(Price=v, HasData=True) for k, v in prices.items()}
        algo.Securities["NEW"] = types.SimpleNamespace(Price=0.0, HasData=False)
        algo.Portfolio = _Book(held, prices, cash)
        algo.Transactions = _Transactions(list(open_tickets))
        algo.pending, algo.pending_tickets = {}, {}
        algo.market = []
        def market(sym, qty, tag=""):
            algo.market.append((sym, qty, tag))
            return types.SimpleNamespace(OrderId=100 + len(algo.market))
        algo.MarketOrder = market
        algo.Liquidate = lambda sym: algo.market.append((sym, "liquidate", ""))
        _limit_orders(algo)
        algo.Time = _dt.datetime(2020, 4, 1, 8)
        return algo

    def closes(self):
        return _Closes({"AAA": [49.0, 50.0], "BBB": [29.0, 30.0], "CCC": [20.0, 20.0], "DDD": [24.0, 25.0],
                        "SPY": [299.0, 300.0]})

    def test_sells_go_at_the_rebalance_and_the_buys_wait_a_session(self):
        algo = self._algo({"CCC": 40, "DDD": 100}, cash=6700.0)          # V = 800 + 2,500 + 6,700 = 10,000
        targets = {"AAA": 0.25, "BBB": 0.24, "DDD": 0.25, "IEF": 0.24}
        self.module.ira_rebalance(algo, targets, self.closes())
        self.assertEqual(algo.market, [("CCC", -40, "IRAORD1 2020-04 sell")])
        self.module.ira_buy_phase(algo)                                   # the same session: nothing yet
        self.assertEqual(len(algo.market), 1)
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        algo.Portfolio.Cash = 7500.0                                      # the sale's proceeds have arrived
        self.module.ira_buy_phase(algo)
        # Budget 7,500 against 50 x 52.5 + 80 x 31.5 + 24 x 105 + 3 x $2 = 7,671: scaled.
        scale = 7500.0 / 7671.0
        self.assertEqual(algo.market[1:], [("AAA", int(50 * scale), "IRAORD1 2020-04 buy"),
                                           ("BBB", int(80 * scale), "IRAORD1 2020-04 buy"),
                                           ("IEF", int(24 * scale), "IRAORD1 2020-04 buy")])
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertTrue(algo.limits[0]["cancelled"])
        self.assertEqual((rec["month"], rec["decision"], rec["buy_session"], rec["waited"]),
                         ("2020-04", "2020-04-01", "2020-04-02", 1))
        self.assertEqual((rec["nlv"], rec["band"], rec["price_band"], rec["max_fees"], rec["budget"]),
                         (10000.0, 0.25, 0.05, 2.0, 7500.0))
        self.assertEqual(rec["targets"], targets)
        self.assertEqual(rec["held"], {"CCC": 40, "DDD": 100})
        self.assertEqual(rec["prices"], {"AAA": 50.0, "BBB": 30.0, "CCC": 20.0, "DDD": 25.0, "IEF": 100.0})
        self.assertEqual(rec["sells"], [["CCC", 40, 19.0]])
        self.assertEqual(rec["buys"], [["AAA", 50, 52.5], ["BBB", 80, 31.5], ["IEF", 24, 105.0]])
        self.assertEqual((rec["scale"], rec["kept"]), (scale, [[b[0], b[1]] for b in algo.market[1:]]))
        self.assertEqual((rec["skipped"], rec["scaled_skipped"], rec["unpriced"], rec["working"]), ([], [], [], {}))
        self.assertEqual((rec["for_sells"], rec["for_price"], rec["unplaced"]), (0, 0, []))
        self.assertEqual((rec["v"], rec["refused"], rec["unfinished"], rec["pending"]), (4, [], False, []))
        # The orders by ID, since the orders listing names a stock by its
        # latest ticker, which a renamed stock no longer shares.
        self.assertEqual(rec["placed"], [["CCC", "sell", 101], ["AAA", "buy", 102], ["BBB", "buy", 103],
                                         ["IEF", "buy", 104]])
        algo.Time = _dt.datetime(2020, 4, 3, 8)
        self.module.ira_buy_phase(algo)                                   # done: nothing more
        self.assertEqual((len(algo.market), len(algo.limits)), (4, 1))

    def test_with_no_sell_the_buys_go_at_once(self):
        algo = self._algo({}, cash=1000.0)
        self.module.ira_rebalance(algo, {"AAA": 0.49, "IEF": 0.49}, self.closes())
        self.assertEqual(algo.market, [("AAA", 9, "IRAORD1 2020-04 buy"), ("IEF", 4, "IRAORD1 2020-04 buy")])
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["buy_session"], rec["waited"], rec["scale"]), ("2020-04-01", 0, 1.0))

    def test_the_buys_wait_while_a_sell_is_still_working(self):
        algo = self._algo({"CCC": 40}, cash=0.0)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        working = _Ticket(9, tag="IRAORD1 2020-04 sell")
        working.Symbol, working.Quantity, working.QuantityFilled = "CCC", -40, 0
        algo.Transactions.tickets = [working]
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        self.module.ira_buy_phase(algo)
        self.assertEqual(len(algo.market), 1)
        algo.Transactions.tickets = []
        algo.Time = _dt.datetime(2020, 4, 3, 8)
        algo.Portfolio.Cash = 800.0
        self.module.ira_buy_phase(algo)
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["buy_session"], rec["waited"], rec["for_sells"], rec["for_price"]), ("2020-04-03", 2, 1, 0))

    def test_the_buys_wait_for_every_outstanding_sell_not_only_this_months(self):
        # An earlier month's sell of CCC still working at the rebalance, and
        # an untagged one later: no buy goes while either works.
        algo = self._algo({"CCC": 40}, cash=1000.0)
        earlier = _Ticket(4, tag="IRAORD1 2020-03 sell")
        earlier.Symbol, earlier.Quantity, earlier.QuantityFilled = "CCC", -40, 0
        algo.Transactions.tickets = [earlier]
        self.module.ira_rebalance(algo, {"AAA": 0.49, "CCC": 0.0}, self.closes())
        self.assertEqual(algo.market, [])                                 # nothing to sell: CCC's is working
        self.assertEqual(algo.ira["wait"], 1)
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        self.module.ira_buy_phase(algo)
        untagged = _Ticket(5)
        untagged.Symbol, untagged.Quantity, untagged.QuantityFilled = "DDD", -3, -1
        algo.Transactions.tickets = [untagged]
        algo.Time = _dt.datetime(2020, 4, 3, 8)
        self.module.ira_buy_phase(algo)
        self.assertEqual(algo.market, [])
        algo.Transactions.tickets = []
        algo.Time = _dt.datetime(2020, 4, 6, 8)
        self.module.ira_buy_phase(algo)
        self.assertEqual(algo.market, [("AAA", 17, "IRAORD1 2020-04 buy")])   # 0.49 x 1,800 / 50
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["waited"], rec["for_sells"], rec["for_price"]), (3, 2, 0))

    def test_an_unfinished_phase_is_recorded_before_the_next_rebalance(self):
        # The sale of CCC never fills: April's buys never go. May's
        # rebalance records April as unfinished, then plans May.
        algo = self._algo({"CCC": 40}, cash=0.0)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        stuck = _Ticket(101, tag="IRAORD1 2020-04 sell")
        stuck.Symbol, stuck.Quantity, stuck.QuantityFilled = "CCC", -40, 0
        algo.Transactions.tickets = [stuck]
        for day in (2, 3):
            algo.Time = _dt.datetime(2020, 4, day, 8)
            self.module.ira_buy_phase(algo)
        algo.Time = _dt.datetime(2020, 5, 1, 8)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        april = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((april["month"], april["unfinished"], april["v"], april["buy_session"]),
                         ("2020-04", True, 4, "2020-05-01"))
        self.assertEqual((april["waited"], april["for_sells"], april["kept"], april["scale"], april["budget"]),
                         (2, 2, [], 0.0, 0.0))
        self.assertEqual(april["placed"], [["CCC", "sell", 101]])
        self.assertEqual((algo.ira["month"], algo.ira["working"], algo.ira["sells"]), ("2020-05", {"CCC": 40}, []))

    def test_an_unfinished_phase_at_the_end_is_recorded(self):
        algo = self._algo({"CCC": 40}, cash=0.0)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        self.module.ira_unfinished(algo)
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["month"], rec["unfinished"]), ("2020-04", True))
        self.module.ira_unfinished(algo)                                   # once only
        self.assertEqual(len(algo.limits), 1)

    def test_an_unfinished_months_export_survives_a_refused_carrier(self):
        # April's record goes on a carrier QuantConnect refuses; May's
        # rebalance replaces April's phase, yet April's record is kept and
        # sent first on the next carrier accepted.
        algo = self._algo({"CCC": 40}, cash=0.0)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        stuck = _Ticket(101, tag="IRAORD1 2020-04 sell")
        stuck.Symbol, stuck.Quantity, stuck.QuantityFilled = "CCC", -40, 0
        algo.Transactions.tickets = [stuck]
        _limit_orders(algo, refused=True)
        algo.Time = _dt.datetime(2020, 5, 1, 8)
        self.module.ira_rebalance(algo, {"AAA": 0.5}, self.closes())
        self.assertEqual(algo.ira["month"], "2020-05")
        self.assertTrue(self.module.stat(algo).endswith("lost=1"), self.module.stat(algo))
        _limit_orders(algo)
        algo.Time = _dt.datetime(2020, 5, 4, 8)
        self.module.ira_buy_phase(algo)
        april = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((april["month"], april["unfinished"]), ("2020-04", True))
        self.assertTrue(self.module.stat(algo).endswith("lost=0"), self.module.stat(algo))

    def test_the_end_sends_a_record_a_refused_carrier_held_back(self):
        algo = self._algo({}, cash=1000.0)
        _limit_orders(algo, refused=True)
        self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())    # the buys go at once
        self.assertTrue(algo.ira["done"])
        _limit_orders(algo)
        self.module.ira_unfinished(algo)                                 # the run's end
        self.assertEqual(_decode(algo.limits[0]["tag"], "IRAPARITY1 ")["month"], "2020-04")

    def test_an_unpriced_holding_is_sold_by_a_tagged_order_and_the_buys_wait_for_it(self):
        algo = self._algo({}, cash=1000.0)
        algo.Portfolio = _Book({"NEW": 7}, {"NEW": 0.0}, 1000.0)
        algo.Transactions.CancelOpenOrders = lambda sym: algo.market.append((sym, "cancel", ""))
        self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())
        self.assertEqual(algo.market, [("NEW", "cancel", ""), ("NEW", -7, "IRAORD1 2020-04 unpriced")])
        self.assertEqual((algo.ira["placed"], algo.ira["wait"]), ([["NEW", "unpriced", 102]], 1))   # the cancel is the mock's 101st
        self.assertEqual((algo.ira["unpriced"], algo.ira["pending"]), (["NEW"], []))

    def test_an_unpriced_holding_is_never_sold_twice_over_a_sell_still_working(self):
        # A sell of 3 of the 7 shares stays working after the cancel (its
        # cancel not yet confirmed): only the 4 it does not hold are sold.
        for working, sale in ((3, [("NEW", -4, "IRAORD1 2020-04 unpriced")]), (7, [])):
            algo = self._algo({}, cash=1000.0)
            algo.Portfolio = _Book({"NEW": 7}, {"NEW": 0.0}, 1000.0)
            old = _Ticket(9, tag="IRAORD1 2020-03 sell")
            old.Symbol, old.Quantity, old.QuantityFilled = "NEW", -working, 0
            algo.Transactions.tickets = [old]
            algo.Transactions.CancelOpenOrders = lambda sym: algo.market.append((sym, "cancel", ""))
            self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())
            self.assertEqual(algo.market[1:], sale, working)


    def _reserved(self, working):
        """An unpriced holding of 7 NEW, ``working`` of them held by an
        earlier month's sell still working at the rebalance."""
        algo = self._algo({}, cash=1000.0)
        algo.Portfolio = _Book({"NEW": 7}, {"NEW": 0.0}, 1000.0)
        lean = _Lean(algo)
        old = lean.market_order("NEW", -working, tag="IRAORD1 2020-03 sell")
        self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())
        return algo, lean, old

    def _sales(self, lean):
        return [(t.Quantity, t.status) for t in lean.orders if t.Tag == "IRAORD1 2020-04 unpriced"]

    def test_shares_held_by_a_cancel_not_yet_confirmed_are_sold_once_it_is(self):
        algo, lean, _ = self._reserved(7)
        self.assertEqual(self._sales(lean), [])                            # all 7 held by the working sell
        lean.confirm_cancels()
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        self.module.ira_buy_phase(algo)
        # The 7 the cancelled sell held are sold now; the buys wait for it.
        self.assertEqual(self._sales(lean), [(-7, "submitted")])
        self.assertEqual([t for t in lean.orders if t.Symbol == "AAA"], [])
        lean.open_fills()
        algo.Time = _dt.datetime(2020, 4, 3, 8)
        self.module.ira_buy_phase(algo)
        self.assertEqual([(t.Symbol, t.Quantity) for t in lean.orders if t.Tag == "IRAORD1 2020-04 buy"], [("AAA", 9)])
        self.assertEqual(algo.Portfolio["NEW"].Quantity, 0)
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        sale = [t.OrderId for t in lean.orders if t.Tag == "IRAORD1 2020-04 unpriced"][0]
        self.assertIn(["NEW", "unpriced", sale], rec["placed"])
        self.assertEqual((rec["waited"], rec["for_sells"], rec["for_price"]), (2, 1, 0))

    def test_shares_a_sell_still_works_on_are_sold_once_it_is_cancelled_and_the_rest_at_once(self):
        algo, lean, _ = self._reserved(3)
        self.assertEqual(self._sales(lean), [(-4, "submitted")])           # the 4 no sell holds
        lean.confirm_cancels()
        lean.open_fills()
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        self.module.ira_buy_phase(algo)
        self.assertEqual(self._sales(lean), [(-4, "filled"), (-3, "submitted")])
        lean.open_fills()
        for day in (3, 6):
            algo.Time = _dt.datetime(2020, 4, day, 8)
            self.module.ira_buy_phase(algo)
        self.assertEqual((algo.Portfolio["NEW"].Quantity, len(self._sales(lean))), (0, 2))

    def test_shares_a_sell_filled_before_its_cancel_are_never_sold_again(self):
        algo, lean, old = self._reserved(7)
        lean.fill(old)                                                     # the cancel came too late
        lean.confirm_cancels()
        for day in (2, 3):
            algo.Time = _dt.datetime(2020, 4, day, 8)
            self.module.ira_buy_phase(algo)
        self.assertEqual((self._sales(lean), algo.Portfolio["NEW"].Quantity), ([], 0))

    def test_a_refused_order_is_recorded_apart_with_its_reason(self):
        algo = self._algo({}, cash=1000.0)
        def market(sym, qty, tag=""):
            algo.market.append((sym, qty, tag))
            response = types.SimpleNamespace(ErrorMessage="Security price is zero" if sym == "IEF" else "")
            return types.SimpleNamespace(OrderId=-10 if sym != "AAA" else 7,
                                         SubmitRequest=types.SimpleNamespace(Response=response))
        algo.MarketOrder = market
        self.module.ira_rebalance(algo, {"AAA": 0.3, "BBB": 0.3, "IEF": 0.3}, self.closes())
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual(rec["placed"], [["AAA", "buy", 7]])
        self.assertEqual(rec["refused"], [["BBB", "buy", ""], ["IEF", "buy", "Security price is zero"]])
        self.assertEqual((rec["v"], rec["unfinished"]), (4, False))

    def test_buys_wait_for_a_security_with_no_price_yet(self):
        # NEW joined the universe at the rebalance: the plan sizes it from
        # the history's close, but it cannot be ordered until its first bar.
        algo = self._algo({}, cash=1000.0)
        closes = _Closes({"AAA": [49.0, 50.0], "NEW": [9.0, 10.0], "SPY": [1.0, 2.0]})
        self.module.ira_rebalance(algo, {"AAA": 0.49, "NEW": 0.49}, closes)
        self.assertEqual((algo.market, algo.limits), ([], []))
        algo.Time = _dt.datetime(2020, 4, 2, 8)
        algo.Securities["NEW"] = types.SimpleNamespace(Price=10.5, HasData=True)
        self.module.ira_buy_phase(algo)
        self.assertEqual(algo.market, [("AAA", 9, "IRAORD1 2020-04 buy"), ("NEW", 49, "IRAORD1 2020-04 buy")])
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["waited"], rec["for_sells"], rec["for_price"], rec["buy_session"]), (1, 0, 1, "2020-04-02"))

    def test_a_security_that_never_gets_a_price_is_left_unplaced(self):
        algo = self._algo({}, cash=1000.0)
        closes = _Closes({"AAA": [49.0, 50.0], "NEW": [9.0, 10.0], "SPY": [1.0, 2.0]})
        self.module.ira_rebalance(algo, {"AAA": 0.49, "NEW": 0.49}, closes)
        for day in range(2, 9):
            algo.Time = _dt.datetime(2020, 4, day, 8)
            self.module.ira_buy_phase(algo)
        self.assertEqual(algo.market, [("AAA", 9, "IRAORD1 2020-04 buy")])
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["waited"], rec["for_price"], rec["unplaced"]), (5, 5, ["NEW"]))

    def test_a_target_with_no_price_waits_in_pending_outside_the_plan(self):
        algo = self._algo({}, cash=1000.0)
        self.module.ira_rebalance(algo, {"AAA": 0.49, "NEW": 0.49}, self.closes())
        self.assertEqual(algo.pending, {"NEW": 0.49})
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["unpriced"], sorted(rec["targets"]), rec["pending"]), (["NEW"], ["AAA"], ["NEW"]))
        # The pending target's security ID, by which its buy outside the
        # planner is matched whatever ticker the stock later takes.
        self.assertEqual(rec["pending_ids"], {"NEW": "NEW"})
        algo = self._algo({}, cash=1000.0)
        new = _Renamed("NEW", "NEW X1")
        algo.Securities[new] = types.SimpleNamespace(Price=0.0, HasData=False)
        self.module.ira_rebalance(algo, {"AAA": 0.49, new: 0.49}, self.closes())
        self.assertEqual(_decode(algo.limits[0]["tag"], "IRAPARITY1 ")["pending_ids"], {"NEW": "NEW X1"})

    def test_the_reference_price_is_the_last_close_of_the_rebalances_history(self):
        algo = self._algo({}, cash=1000.0)
        closes = _Closes({"AAA": [49.0, float("nan")], "SPY": [1.0, 2.0]})
        algo.Securities["AAA"].Price = 51.0
        self.module.ira_rebalance(algo, {"AAA": 0.49, "BBB": 0.49}, closes)
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        # AAA's last row has no close: the last it has. BBB is not in the
        # history: its security's price.
        self.assertEqual(rec["prices"], {"AAA": 49.0, "BBB": 30.0})

    def test_a_holding_whose_ticker_changed_is_named_as_the_book_names_it(self):
        old, new = _Renamed("OLD", "X1"), _Renamed("NEW2", "X1")
        algo = self._algo({}, cash=500.0)
        algo.Portfolio = _Book({old: 10}, {old: 50.0}, 500.0)
        algo.Securities[new] = types.SimpleNamespace(Price=50.0, HasData=True)
        closes = _Closes({"AAA": [50.0, 50.0], new: [50.0, 50.0], "SPY": [1.0, 2.0]})
        self.module.ira_rebalance(algo, {new: 0.49, "AAA": 0.49}, closes)
        rec = _decode(algo.limits[0]["tag"], "IRAPARITY1 ")
        self.assertEqual((rec["held"], sorted(rec["prices"])), ({"NEW2": 10}, ["AAA", "NEW2"]))

    def test_working_sells_at_the_rebalance_are_recorded_and_netted(self):
        t = _Ticket(4)
        t.Symbol, t.Quantity, t.QuantityFilled, t.Tag = "CCC", -10, -4, ""
        algo = self._algo({"CCC": 40}, cash=1000.0, open_tickets=[t])
        self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())
        self.assertEqual(algo.market[0], ("CCC", -34, "IRAORD1 2020-04 sell"))

    def test_rebalance_hands_the_book_to_the_ira_path_only_when_switched_on(self):
        import inspect
        src = inspect.getsource(self.module.MomentumResearch.Rebalance)
        self.assertLess(src.index("ira_rebalance(self, targets, closes)"), src.index("self._trade(targets)"))
        algo = self.module.MomentumResearch()
        self.assertFalse(algo.IRA_EXEC)

    def test_the_end_records_an_unfinished_phase_then_counts_the_exports(self):
        algo = self._algo({"CCC": 40}, cash=0.0)
        self.module.ira_rebalance(algo, {"AAA": 0.49}, self.closes())    # the sale of CCC never fills
        self.assertEqual(algo.limits, [])
        stats = {}
        algo.SetRuntimeStatistic = lambda k, v: stats.__setitem__(k, v)
        algo.Portfolio, algo.equity_curve, algo.spy_curve, algo.scales = [], [], [], []
        algo.Transactions.OrdersCount = 1
        algo.MEASURE_FROM, algo.END_DATE = (1999, 1, 1), (2015, 12, 31)
        for attr in ("rebalances", "risk_off_months", "borrow_paid", "resizes", "skipped", "retried", "cash_sum",
                     "cash_days", "fund_reads", "fund_hits", "early_selections", "names_held"):
            setattr(algo, attr, 0)
        algo.OnEndOfAlgorithm()
        self.assertTrue(stats["IraParity"].startswith("delay=1 logged=1 bytes="), stats["IraParity"])
        self.assertTrue(_decode(algo.limits[0]["tag"], "IRAPARITY1 ")["unfinished"])

    def test_the_template_only_adds_to_fresh_universe(self):
        _additions_only(self, "momentum/fresh_universe.py", "momentum/ira_parity_log.py")

    def test_the_template_fits_the_free_plans_file_limit(self):
        for name in ("ira_parity_log.py", "parity_export.py"):
            with open(os.path.join(HERE, "momentum", name)) as f:
                self.assertLess(len(f.read()), 32_000, name)

    def test_the_runs_are_the_30k_combination_plus_the_ira_settings(self):
        for name, base in (("IRA_PARITY", "FU_COMBO_30K"), ("OOS_IRA_PARITY", "OOS_FU_COMBO_30K")):
            template, params = build_variants.VARIANTS[name]
            self.assertEqual(template, "momentum/ira_parity_log.py", name)
            c = _class_constants(template, params)
            self.assertEqual((c.pop("IRA_EXEC"), c.pop("EXEC_DELAY"), c.pop("PRICE_BAND"), c.pop("MAX_FEES")),
                             (True, 1, 0.05, 2.0), name)
            self.assertEqual(c, _class_constants(*build_variants.VARIANTS[base]), name)


class RenderedRunsTest(unittest.TestCase):
    def test_every_remaining_parity_run_renders_within_the_free_plans_file_limit(self):
        names = [n for n in build_variants.VARIANTS if n.endswith("_PARITY") and
                 build_variants.VARIANTS[n][0] in ("momentum/glide_parity_log.py",
                                                   "momentum/minimum_lot_parity_log.py",
                                                   "momentum/ira_parity_log.py")]
        self.assertEqual(len(names), 10)
        for name in names:
            template, params = build_variants.VARIANTS[name]
            with open(os.path.join(HERE, template)) as f:
                self.assertLess(len(build_variants.render(f.read(), params, {})), 32_000, name)


class LotTemplateTest(unittest.TestCase):
    """momentum/minimum_lot_parity_log.py: minimum_lot.py exporting D7-L's
    inputs and result in the planned months and in every month D7-L
    skipped, kept or withheld."""

    RANKED = ["A", "B", "C", "E", "F"]
    SECTOR = {"A": 311, "B": 311, "C": 205, "E": 206, "F": 101}
    VOL = {"A": 0.02, "B": 0.05, "C": 0.04, "E": 0.01, "F": 0.03}
    SCORES = {"A": 0.5, "B": 0.4, "C": 0.3, "E": 0.2, "F": 0.1}
    PRICES = {"A": 100.0, "B": 100.0, "C": 100.0, "E": 100.0, "F": 50.0}

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/minimum_lot_parity_log.py", {})

    def _algo(self, held, months=(), raw=None):
        algo = self.module.MomentumResearch()
        algo.MIN_LOT, algo.TOP_N, algo.SECTOR_CAP, algo.INV_VOL = "held", 3, 1, True
        algo.RISK_OFF_FRACTION, algo.SPY_BLEND, algo.STOCK_TREND_DAYS = 0.5, 0.0, 0
        algo.bond, algo.spy, algo.sectors = "IEF", "SPY", self.SECTOR
        algo.raw_close = dict(self.PRICES, **(raw or {}))
        algo.LOT_PARITY_MONTHS = months
        for attr in ("lot_skipped", "lot_held_sold", "lot_kept_n", "lot_withheld", "lot_noprice", "resizes"):
            setattr(algo, attr, 0)
        algo.lot_kept = set()
        algo.lot_scores = self.SCORES
        algo.Portfolio = _Book(held, dict(self.PRICES, IEF=100.0), 1000.0 - sum(held.values()) * 100.0)
        algo.Transactions = _Transactions([_Ticket(5)])
        algo.Securities = {"SPY": types.SimpleNamespace(Price=300.0)}
        algo.Time = _dt.datetime(2020, 4, 1, 8)
        _limit_orders(algo)
        return algo

    def test_a_planned_month_is_exported_with_inputs_and_result(self):
        algo = self._algo({}, months=((2020, 4),))
        targets = algo._minimum_lot(self.RANKED, self.VOL, None, False)
        self.module.lot_export(algo)
        r = _decode(algo.Transactions.tickets[0].tags[0], "LOTPARITY1 ")
        self.assertEqual((r["month"], r["mode"], r["nlv"], r["risk_off"]), ("2020-04", "held", 1000.0, False))
        self.assertEqual(r["ranked"], [[n, self.SCORES[n], self.VOL[n], self.SECTOR[n]] for n in self.RANKED])
        self.assertEqual(r["prices"], self.PRICES)
        self.assertEqual((r["weights"], r["skipped"], r["kept"], r["held"]), (targets, [], [], {}))
        self.assertIsNone(algo.lot_last)

    def test_an_unplanned_month_is_exported_only_when_d7l_skipped_or_kept(self):
        algo = self._algo({})
        algo._minimum_lot(self.RANKED, self.VOL, None, False)
        self.module.lot_export(algo)
        self.assertEqual(algo.Transactions.tickets[0].tags, [])
        algo.raw_close["C"] = 150.0                                       # C's $140 target is now short of a share
        algo._minimum_lot(self.RANKED, self.VOL, None, False)
        self.module.lot_export(algo)
        self.assertEqual(len(algo.Transactions.tickets[0].tags), 1)
        r = _decode(algo.Transactions.tickets[0].tags[0], "LOTPARITY1 ")
        self.assertEqual(r["skipped"], ["C"])

    def test_a_kept_name_is_exported_with_its_holding(self):
        algo = self._algo({"C": 4}, raw={"C": 150.0})
        algo._minimum_lot(self.RANKED, self.VOL, None, False)
        self.module.lot_export(algo)
        r = _decode(algo.Transactions.tickets[0].tags[0], "LOTPARITY1 ")
        self.assertEqual((r["kept"], r["held"], r["skipped"]), (["C"], {"C": 4}, []))

    def test_a_holding_whose_ticker_changed_is_named_as_the_ranking_names_it(self):
        old, new = _Renamed("OLD", "X1"), _Renamed("C", "X1")
        algo = self._algo({}, raw={"C": 150.0})
        ranked = ["A", "B", new, "E", "F"]
        algo.raw_close[new] = algo.raw_close.pop("C")
        algo.lot_scores = {**self.SCORES, new: self.SCORES["C"]}
        algo.sectors = {**self.SECTOR, new: self.SECTOR["C"]}
        vol = {**self.VOL, new: self.VOL["C"]}
        algo.Portfolio = _Book({old: 4}, {old: 100.0}, 600.0)
        algo._minimum_lot(ranked, vol, None, False)
        self.module.lot_export(algo)
        r = _decode(algo.Transactions.tickets[0].tags[0], "LOTPARITY1 ")
        self.assertEqual((r["kept"], r["held"]), (["C"], {"C": 4}))

    def test_a_withheld_month_is_exported_with_no_weights(self):
        algo = self._algo({}, raw={n: 5000.0 for n in self.RANKED})
        algo.Transactions = _Transactions([])
        self.assertIsNone(algo._minimum_lot(self.RANKED, self.VOL, None, False))
        self.module.lot_export(algo)
        r = _decode(algo.limits[0]["tag"], "LOTPARITY1 ")
        self.assertIsNone(r["weights"])
        self.assertEqual(sorted(r["skipped"]), self.RANKED)

    def test_rebalance_exports_after_trading_and_before_a_withheld_return(self):
        import inspect
        src = inspect.getsource(self.module.MomentumResearch.Rebalance)
        self.assertLess(src.index("self.lot_scores = mom"), src.index("targets = self._minimum_lot("))
        withheld = src.index("if targets is None:")
        self.assertLess(withheld, src.index("lot_export(self)"))
        self.assertLess(src.index("self._trade(targets)"), src.rindex("lot_export(self)"))

    def test_the_template_only_adds_to_minimum_lot(self):
        _additions_only(self, "momentum/minimum_lot.py", "momentum/minimum_lot_parity_log.py")

    def test_the_template_fits_the_free_plans_file_limit(self):
        with open(os.path.join(HERE, "momentum", "minimum_lot_parity_log.py")) as f:
            self.assertLess(len(f.read()), 32_000)

    def test_the_runs_are_the_held_d7l_runs_plus_their_months(self):
        for name, base, months in (("D7L_PARITY", "D7L_HELD_30K", build_variants.PARITY_MONTHS_IS),
                                   ("OOS_D7L_PARITY", "OOS_D7L_HELD_30K", build_variants.PARITY_MONTHS_OOS)):
            template, params = build_variants.VARIANTS[name]
            self.assertEqual(template, "momentum/minimum_lot_parity_log.py", name)
            c = _class_constants(template, params)
            self.assertEqual(tuple(map(tuple, c.pop("LOT_PARITY_MONTHS"))), months, name)
            self.assertEqual(c, _class_constants(*build_variants.VARIANTS[base]), name)


class GlideTemplateTest(unittest.TestCase):
    """momentum/glide_parity_log.py: glide_path.py exporting each
    rebalance's bond share, age, momentum book and combined targets, and,
    in the planned months, parity_log.py's momentum export."""

    def setUp(self):
        sys.modules.setdefault("numpy", types.SimpleNamespace(std=lambda x: 0.0))
        self.module = _load_template(self, "momentum/glide_parity_log.py", {})

    def _algo(self):
        algo = self.module.MomentumResearch()
        algo.GLIDE, algo.GLIDE_PARITY, algo.PATH_START_AGE = "path", True, 53.0
        algo.MEASURE_FROM, algo.PARITY_MONTHS = (2002, 8, 1), ()
        algo.bond_syms, algo.b_used = {"IEF": "IEF", "SHY": "SHY"}, []
        algo.spy = "SPY"
        algo.Securities = {"SPY": types.SimpleNamespace(Price=300.0)}
        algo.Transactions = _Transactions([_Ticket(5)])
        algo.Time = _dt.datetime(2010, 3, 1, 8)
        _limit_orders(algo)
        return algo

    def test_each_rebalance_exports_the_share_age_book_and_combined_targets(self):
        algo = self._algo()
        book = {"A": 0.49, "IEF": 0.49}
        combined = algo._glide_targets(dict(book))
        self.module.glide_export(algo, combined, None, None, False, {}, {}, [])
        r = _decode(algo.limits[0]["tag"], "GLIDE1 ")
        age = 53.0 + 91 / 12
        self.assertEqual((r["month"], r["date"], r["glide"], r["start_age"], r["measure_from"]),
                         ("2010-03", "2010-03-01", "path", 53.0, "2002-08"))
        self.assertEqual((r["age"], r["b"]), (age, self.module.glide_share(age)))
        self.assertEqual((r["book"], r["targets"]), (book, combined))
        self.assertEqual(algo.Transactions.tickets[0].tags, [])          # the strategy's orders keep their tags

    def test_without_the_switch_nothing_is_exported(self):
        algo = self._algo()
        algo.GLIDE_PARITY = False
        self.module.glide_export(algo, algo._glide_targets({"A": 0.98}), None, None, False, {}, {}, [])
        self.assertEqual(algo.limits, [])

    def test_a_planned_month_also_carries_parity_logs_momentum_export_on_the_open_order(self):
        algo = self._algo()
        algo.PARITY_MONTHS, algo.TOP_N, algo.SECTOR_CAP, algo.PARITY_SAMPLE, algo.TREND_DAYS = ((2010, 3),), 1, 3, 0, 2
        algo.members, algo.sectors = ["A"], {"A": 311}
        closes = _Closes({"A": [1.0, 1.1], "SPY": [250.0, 251.0]})
        closes.index = [_dt.datetime(2010, 2, 25), _dt.datetime(2010, 2, 26)]
        combined = algo._glide_targets({"A": 0.98})
        spy = _SpySeries([250.0, 251.0])
        self.module.glide_export(algo, combined, closes, spy, False, {"A": 0.5}, {"A": 0.01}, ["A"])
        r = _decode(algo.Transactions.tickets[0].tags[0], "PARITY1 ")
        self.assertEqual((r["month"], r["ranked"], r["weights"]), ("2010-03", [["A", 0.5, 0.01, 311, "P"]],
                                                                   {"A": 0.98}))
        self.assertEqual((r["spy_close"], r["spy_mean"], r["spy_n"]), (251.0, 250.5, 2))
        self.assertEqual(len(algo.limits), 1)                             # and the GLIDE1 record

    def test_rebalance_exports_after_trading(self):
        import inspect
        src = inspect.getsource(self.module.MomentumResearch.Rebalance)
        self.assertLess(src.index("self._trade(targets)"), src.index("glide_export(self, targets"))

    def test_the_template_only_adds_to_glide_path(self):
        _additions_only(self, "momentum/glide_path.py", "momentum/glide_parity_log.py")

    def test_the_template_fits_the_free_plans_file_limit(self):
        with open(os.path.join(HERE, "momentum", "glide_parity_log.py")) as f:
            self.assertLess(len(f.read()), 32_000)

    def test_the_runs_are_the_path_runs_plus_the_switch_and_p38s_months(self):
        for window, months in (("IS", build_variants.GLIDE_PARITY_MONTHS_IS),
                               ("OOS", build_variants.PARITY_MONTHS_OOS)):
            for age in ("P38", "P53", "P65"):
                name, base = "G_{}_{}_PARITY".format(window, age), "G_{}_{}".format(window, age)
                template, params = build_variants.VARIANTS[name]
                self.assertEqual(template, "momentum/glide_parity_log.py", name)
                c = _class_constants(template, params)
                self.assertIs(c.pop("GLIDE_PARITY"), True, name)
                want = months if age == "P38" else ()
                self.assertEqual(tuple(map(tuple, c.pop("PARITY_MONTHS"))), want, name)
                self.assertEqual(c.pop("PARITY_SAMPLE"), 5, name)
                self.assertEqual(c, _class_constants(*build_variants.VARIANTS[base]), name)

    def test_the_tuning_months_are_every_sixth_from_2002_08_plus_the_golden_months_in_the_window(self):
        months = build_variants.GLIDE_PARITY_MONTHS_IS
        sixth = set(build_variants._every((2002, 8), (2015, 12), 6))
        self.assertEqual(set(months), sixth | {(2008, 11), (2009, 6), (2013, 5)})
        self.assertEqual((len(sixth), len(months)), (27, 30))
        self.assertEqual(list(months), sorted(months))


class _SpySeries(list):
    @property
    def iloc(self):
        return self

    def mean(self):
        return sum(self) / len(self)

    def __getitem__(self, i):
        out = list.__getitem__(self, i)
        return _SpySeries(out) if isinstance(i, slice) else out


if __name__ == "__main__":
    unittest.main()
