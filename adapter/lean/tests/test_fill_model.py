"""The adapter's ADR 0005 fill model prices a capped buy exactly as internal/fills does.

ADR 0005, as amended 2026-09-24: a buy stop-limit at (level, cap) resting
from the bar's open triggers when the bar reaches the level, an exact touch
included; it executes at max(level, open) within the cap; an open above the
cap fills at the cap only if the bar trades back down to it, and otherwise
not at all. ADR 0013: slippage_n x N is added to every fill. The model runs
inside LEAN as a subclass of its EquityFillModel; here it runs against the
scaffold's fakes of that class, the security, the order and the bar.
"""
import json
import sys
import types
import unittest
from datetime import datetime, timezone
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

import test_algorithm as scaffold  # noqa: E402  (installs the AlgorithmImports fake)

import orders  # noqa: E402

SHARED_CASES = Path(__file__).resolve().parent / "testdata" / "stop_limit_fill_cases.json"

PLACED = datetime(2000, 1, 3, 21, tzinfo=timezone.utc)
SESSION_END = datetime(2000, 1, 4, 21, tzinfo=timezone.utc)

# Synthetic bars, invented round numbers (never market data), the shapes
# of the shared table's bars A and B. BAR_A trades around its open.
BAR_A = dict(open_=100.0, high=104.0, low=98.0)
# BAR_B opens high, at 110, and trades back down to 105.
BAR_B = dict(open_=110.0, high=112.0, low=105.0)


def lean():
    imports = sys.modules["AlgorithmImports"]
    return types.SimpleNamespace(OrderEvent=imports.OrderEvent, OrderFee=imports.OrderFee,
                                 OrderStatus=imports.OrderStatus,
                                 OrderDirection=imports.OrderDirection,
                                 to_utc=imports.Extensions.ConvertToUtc)


def asset(open_, high, low, end=SESSION_END, exchange_open=True):
    return types.SimpleNamespace(
        LocalTime=end, Exchange=types.SimpleNamespace(TimeZone="UTC"),
        exchange_open=exchange_open,
        prices=types.SimpleNamespace(Open=open_, High=high, Low=low, EndTime=end))


def order(level, cap, tag="proposal", direction="buy", status="submitted", quantity=10,
          placed=PLACED, order_id=7):
    return types.SimpleNamespace(Id=order_id, Tag=tag, Direction=direction, Status=status,
                                 Time=placed, StopPrice=level, LimitPrice=cap, Quantity=quantity)


class FillModelCase(unittest.TestCase):
    def model(self, n=10.0, slippage_n=0.05):
        """The adapter's model over EquityFillModel's fake, slipping every
        order tagged "proposal" by slippage_n x n and recording the charge."""
        self.charged = []
        n_by_tag = {"proposal": n}

        def n_for_tag(tag):
            if tag not in n_by_tag:
                raise ValueError("no N was supplied for order tag {!r}".format(tag))
            return n_by_tag[tag]

        slippage = orders.NSlippageModel(slippage_n, n_for_tag,
                                         lambda order_id, s: self.charged.append((order_id, s)))
        model_class = orders.adr_0005_fill_model(scaffold.EquityFillModel, lean())
        return model_class(slippage)

    def fill(self, model, bar, placed_order):
        return model.StopLimitFill(bar, placed_order)

    def assert_filled(self, event, price, quantity=10):
        self.assertEqual(event.Status, "filled")
        self.assertEqual(event.FillQuantity, quantity)
        self.assertAlmostEqual(event.FillPrice, price, delta=1e-9)

    def assert_unfilled(self, event):
        self.assertIsNone(event.Status)
        self.assertEqual(event.FillQuantity, 0)


class Adr0005CaseTests(FillModelCase):
    """Each of ADR 0005's cases, at 0.05 x 10 = 0.5 of slippage."""

    def test_a_trigger_inside_the_bar_fills_at_the_level_plus_slippage(self):
        # LEAN's native stop-limit fill was observed at the bar's high
        # instead; ADR 0005 fills at the level.
        self.assert_filled(self.fill(self.model(), asset(**BAR_A), order(102.0, 106.0)),
                           102.5)

    def test_an_exact_touch_of_the_level_fills(self):
        # LEAN's native stop-limit fill was observed not to trigger at all.
        self.assert_filled(self.fill(self.model(), asset(**BAR_A), order(104.0, 106.0)),
                           104.5)

    def test_a_bar_whose_high_is_below_the_level_does_not_fill(self):
        self.assert_unfilled(self.fill(self.model(), asset(**BAR_A), order(105.0, 110.0)))

    def test_a_gap_within_the_cap_fills_at_the_open(self):
        # LEAN's native stop-limit fill was observed at min(high, limit).
        self.assert_filled(self.fill(self.model(), asset(**BAR_A), order(95.0, 106.0)),
                           100.5)

    def test_a_gap_above_the_cap_that_trades_back_fills_at_the_cap(self):
        self.assert_filled(self.fill(self.model(), asset(**BAR_B), order(104.0, 107.0)),
                           107.5)

    def test_a_gap_above_the_cap_with_no_trade_back_does_not_fill(self):
        self.assert_unfilled(self.fill(self.model(), asset(**BAR_A), order(90.0, 95.0)))

    def test_k_0_fills_at_the_shared_level_inside_the_bar_on_a_touch_and_on_a_trade_back(self):
        for bar, level, price in ((BAR_A, 102.0, 102.5), (BAR_A, 104.0, 104.5),
                                  (BAR_B, 106.0, 106.5)):
            with self.subTest(level=level):
                self.assert_filled(self.fill(self.model(), asset(**bar), order(level, level)),
                                   price)

    def test_k_0_with_a_gap_and_no_trade_back_does_not_fill(self):
        self.assert_unfilled(self.fill(self.model(), asset(**BAR_A), order(96.0, 96.0)))

    def test_slippage_is_slippage_n_x_the_orders_n_and_is_recorded(self):
        model = self.model(n=4.0, slippage_n=0.1)
        self.assert_filled(self.fill(model, asset(108.0, 109.0, 103.0),
                                     order(108.5, 110.0, order_id=23)), 108.9)
        self.assertEqual(len(self.charged), 1)
        self.assertEqual(self.charged[0][0], 23)
        self.assertAlmostEqual(self.charged[0][1], 0.4, delta=1e-12)

    def test_the_fill_is_the_whole_order(self):
        self.assert_filled(self.fill(self.model(), asset(**BAR_A), order(102.0, 106.0,
                                                                         quantity=615)),
                           102.5, quantity=615)


class SharedCaseTests(FillModelCase):
    """tests/testdata/stop_limit_fill_cases.json: the cases internal/fills'
    TestExecuteStopLimitAnswersTheSharedCases answers too."""

    def cases(self):
        table = json.loads(SHARED_CASES.read_text())
        self.assertGreater(table["tolerance"], 0)
        self.assertLessEqual(table["tolerance"], 1e-9)
        self.assertTrue(table["cases"])
        return table["tolerance"], table["cases"]

    def test_the_pricing_rule_answers_every_shared_case(self):
        tolerance, cases = self.cases()
        for case in cases:
            with self.subTest(case["name"]):
                self.assertEqual(case["filled"], "price" in case)
                price = orders.stop_limit_buy_fill_price(
                    case["level"], case["price_cap"], case["open"], case["high"], case["low"],
                    case["slippage_n"] * case["n"])
                self.assertEqual(price is not None, case["filled"])
                if case["filled"]:
                    self.assertAlmostEqual(price, case["price"], delta=tolerance)

    def test_the_fill_model_answers_every_shared_case(self):
        tolerance, cases = self.cases()
        for case in cases:
            with self.subTest(case["name"]):
                model = self.model(n=case["n"], slippage_n=case["slippage_n"])
                event = self.fill(model, asset(case["open"], case["high"], case["low"]),
                                  order(case["level"], case["price_cap"]))
                self.assertEqual(event.Status == "filled", case["filled"])
                if case["filled"]:
                    self.assertAlmostEqual(event.FillPrice, case["price"], delta=tolerance)
                self.assertIsNone(model.failure)


class WhenTheModelFillsNothingTests(FillModelCase):
    """As LEAN's own fills do, nothing fills for a cancelled order, while the
    exchange is closed, or against the bar the order was placed after."""

    def test_a_new_order_never_fills_against_the_bar_it_was_placed_after(self):
        bar = asset(**BAR_A)
        self.assert_unfilled(self.fill(self.model(), bar, order(102.0, 106.0,
                                                                placed=SESSION_END)))

    def test_an_order_placed_before_the_bar_ended_fills_against_it(self):
        bar = asset(**BAR_A)
        self.assert_filled(self.fill(self.model(), bar, order(
            102.0, 106.0, placed=SESSION_END.replace(minute=0, hour=20))), 102.5)

    def test_naive_lean_times_are_read_as_utc(self):
        bar = asset(**BAR_A, end=SESSION_END.replace(tzinfo=None))
        self.assert_unfilled(self.fill(self.model(), bar, order(
            102.0, 106.0, placed=SESSION_END.replace(tzinfo=None))))
        self.assert_filled(self.fill(self.model(), bar, order(102.0, 106.0)), 102.5)

    def test_a_cancelled_order_does_not_fill(self):
        self.assert_unfilled(self.fill(self.model(), asset(**BAR_A),
                                       order(102.0, 106.0, status="canceled")))

    def test_nothing_fills_while_the_exchange_is_closed(self):
        self.assert_unfilled(self.fill(self.model(), asset(**BAR_A, exchange_open=False),
                                       order(102.0, 106.0)))

    def test_the_fill_is_stamped_at_the_securitys_time_in_utc(self):
        event = self.fill(self.model(), asset(**BAR_A), order(102.0, 106.0))
        self.assertEqual(event.UtcTime, SESSION_END)
        self.assertEqual(event.OrderFee, "no fee")


class OtherOrdersKeepLeansFillTests(FillModelCase):
    def test_a_sell_stop_limit_keeps_leans_own_fill(self):
        model = self.model()
        sell = order(104.0, 103.0, direction="sell")
        self.assertEqual(self.fill(model, asset(**BAR_B), sell), ("lean's own stop-limit fill", sell))
        self.assertEqual(model.native_calls, [sell])

    def test_a_buy_never_reaches_leans_own_stop_limit_fill(self):
        model = self.model()
        self.fill(model, asset(**BAR_A), order(102.0, 106.0))
        self.assertEqual(model.native_calls, [])

    def test_only_the_stop_limit_fill_is_replaced(self):
        # Stop-market orders (the Variant "uncapped", every Exit Order) keep
        # EquityFillModel's own fill, observed to match ADR 0005 on the
        # pinned image.
        model_class = orders.adr_0005_fill_model(scaffold.EquityFillModel, lean())
        self.assertTrue(issubclass(model_class, scaffold.EquityFillModel))
        own = {name for name in vars(model_class) if name[0].isupper()}
        self.assertEqual(own, {"StopLimitFill"})


class FailClosedTests(FillModelCase):
    """LEAN swallows a fill model's exception (observed: logged as an order
    error, the order left unfilled), so the model records it for the
    algorithm to stop the run on."""

    def test_an_order_it_cannot_price_is_left_unfilled_and_recorded(self):
        model = self.model()
        event = self.fill(model, asset(**BAR_A), order(102.0, 100.0, order_id=41))
        self.assert_unfilled(event)
        for fact in ("41", "proposal", "below its own level"):
            self.assertIn(fact, model.failure)

    def test_an_order_with_no_n_is_never_slipped_by_zero(self):
        model = self.model()
        self.assert_unfilled(self.fill(model, asset(**BAR_A), order(102.0, 106.0, tag="stray")))
        self.assertIn("no N was supplied", model.failure)

    def test_the_first_failure_is_kept(self):
        model = self.model()
        self.fill(model, asset(**BAR_A), order(102.0, 100.0, order_id=41))
        self.fill(model, asset(**BAR_A), order(102.0, 106.0, tag="stray", order_id=42))
        self.assertIn("41", model.failure)
        self.assertNotIn("stray", model.failure)

    def test_every_bad_input_is_refused(self):
        good = dict(level=102.0, price_cap=106.0, open_=100.0, high=104.0, low=98.0,
                    slippage=0.5)
        for name, bad in (("level", 0), ("price_cap", -1.0), ("open_", float("nan")),
                          ("high", float("inf")), ("low", 0.0), ("slippage", 0.0),
                          ("slippage", -0.1), ("high", 97.0)):
            with self.subTest(name=name, value=bad):
                with self.assertRaises(orders.Uncertain):
                    orders.stop_limit_buy_fill_price(**dict(good, **{name: bad}))


if __name__ == "__main__":
    unittest.main()
