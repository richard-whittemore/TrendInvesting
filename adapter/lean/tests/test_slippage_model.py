"""ADR 0013: uncertain slippage stops both adapter boundaries, even when
LEAN swallows the model's exception (ADR 0005's fill-model discipline).
"""
import types
import unittest

from test_orders import (OrderTestCase, add_proposal, campaign_opened, exit_order_set,
                         period_end, scaffold, trade_proposal)
from test_publisher import Frame, bar

import orders
from publisher import Refused, split_adjusted_view


class SlippageFailureTests(OrderTestCase):
    def resting(self, kind):
        algo = self.start()
        if kind == "exit":
            self.hold(algo, 100)
            self.feed(algo, 9, [campaign_opened(), exit_order_set(9)])
            [ticket] = self.sells(algo)
        else:
            proposal = trade_proposal if kind == "entry" else add_proposal
            self.feed(algo, 9, [proposal(9, order_type="stop-market", gap_buffer_n=0,
                                         price_cap=0)])
            [ticket] = self.tickets(algo)
        self.assertEqual(ticket.OrderType, "stop-market")
        return algo, ticket

    def stop_market_fill(self, algo, ticket):
        """ADR 0005/0013: price a triggered stop at its level plus/minus
        slippage; emulate LEAN swallowing model errors and leaving it unfilled.
        """
        self.at(algo, 10)
        try:
            slip = algo.security.slippage_model.GetSlippageApproximation(
                algo.security, types.SimpleNamespace(Id=ticket.OrderId, Tag=ticket.Tag))
        except Exception as err:
            algo.Log("Transaction model failed to fill: {}".format(err))
            return
        price = ticket.StopPrice + (slip if ticket.Quantity > 0 else -slip)
        ticket.Status = "filled"
        ticket.QuantityFilled = ticket.Quantity
        algo.Portfolio.holdings["AAPL"] = algo.Portfolio.holdings.get("AAPL", 0) + ticket.Quantity
        algo.Transactions.emit(ticket, "filled", fill_quantity=ticket.Quantity,
                               fill_price=price, fee=1.0)

    def fail_slippage(self, algo):
        return algo.security.slippage_model.GetSlippageApproximation(
            algo.security, types.SimpleNamespace(Id=99, Tag="missing-n"))

    def test_unknown_tag_stop_market_fill_stops_before_any_input_reaches_the_engine(self):
        for kind in ("entry", "add", "exit"):
            for boundary in ("slice", "drain", "end"):
                with self.subTest(kind=kind, boundary=boundary):
                    algo, ticket = self.resting(kind)
                    # Keep the placed order known to the desk so its unknown-order
                    # guard cannot mask missing slippage protection (ADR 0013).
                    del algo.desk.n_by_tag[ticket.Tag]
                    sent = len(algo.client.sent)
                    self.stop_market_fill(algo, ticket)
                    if boundary == "slice":
                        self.feed(algo, 10)
                    elif boundary == "drain":
                        algo.drain_order_events()
                    else:
                        algo.OnEndOfAlgorithm()
                    self.assertTrue(algo.failed)
                    self.assertTrue(algo.client.closed)
                    for fact in (str(ticket.OrderId), ticket.Tag, "no N was supplied", "ADR 0013"):
                        self.assertIn(fact, algo.quit_reason)
                        self.assertIn(fact, algo.security.slippage_model.failure)
                    self.assertIsNone(algo.fill_model.failure)
                    self.assertEqual(self.types_sent(algo, sent), [])
                    self.assert_stopped_after(algo, sent)
                    algo.OnEndOfAlgorithm()
                    self.assertEqual(self.types_sent(algo, sent), [])

    def test_soundness_check_stops_even_without_a_queued_report(self):
        algo = self.start()
        self.fail_slippage(algo)
        self.assertEqual(algo.order_events, [])
        self.assertFalse(algo.fill_model_sound())
        self.assertTrue(algo.failed)
        self.assertTrue(algo.client.closed)
        self.assertIn("missing-n", algo.quit_reason)
        self.assertEqual(algo.client.sent, [])

    def test_publisher_refuses_every_input_after_slippage_failure(self):
        end = period_end(10)
        b = bar(10)
        attempts = {
            "market.bar.completed": lambda p: p.publish(
                "AAPL", b, split_adjusted_view(Frame(b.EndTime), b.EndTime), end),
            "market.session.closed": lambda p: p.publish_session_closed(period_end(9)),
            "account.snapshot": lambda p: p.publish_snapshot(
                types.SimpleNamespace(TotalPortfolioValue=100.0, Cash=50.0), end),
            "execution.fill": lambda p: p.publish_fill({"filled_at": end}),
            "execution.order.lifecycle": lambda p: p.publish_order_lifecycle({"occurred_at": end}),
            "adapter.run.stopped": lambda p: p.publish_run_stopped("delisted", "detail", "AAPL"),
            "replay.run.completed": lambda p: p.publish_run_completed(),
        }
        for event_type, attempt in attempts.items():
            for failed in (False, True):
                with self.subTest(event_type=event_type, failed=failed):
                    algo = self.start()
                    p = algo.publisher
                    b9 = bar(9)
                    p.publish("AAPL", b9, split_adjusted_view(Frame(b9.EndTime), b9.EndTime),
                              period_end(9))
                    if event_type != "market.session.closed":
                        p.publish_session_closed(period_end(9))
                    sent, sequence = len(algo.client.sent), p.sequence
                    if not failed:
                        attempt(p)
                        self.assertEqual(self.types_sent(algo, sent), [event_type])
                        continue
                    self.fail_slippage(algo)
                    with self.assertRaises(Refused) as caught:
                        attempt(p)
                    self.assertIn("missing-n", str(caught.exception))
                    self.assertEqual(self.types_sent(algo, sent), [])
                    self.assertEqual(p.sequence, sequence)
                    self.assertFalse(p.completed)

    def test_desk_refuses_every_change_after_slippage_failure(self):
        for action in ("stop-market", "stop-limit", "amend", "cancel", "decision"):
            for failed in (False, True):
                with self.subTest(action=action, failed=failed):
                    algo, ticket = self.resting("entry")
                    book = algo.Transactions
                    fields = scaffold.UpdateOrderFields()
                    fields.StopPrice = 25.0
                    props = scaffold.OrderProperties()
                    attempts = {
                        "stop-market": lambda: algo.desk._submit(10, 24.0, "another", props),
                        "stop-limit": lambda: algo.desk._submit(10, 24.0, "another", props, 25.0),
                        "amend": lambda: algo.desk._amend(ticket, fields),
                        "cancel": lambda: algo.desk._cancel(ticket),
                        "decision": lambda: algo.desk.act([add_proposal(9)], period_end(9), False),
                    }
                    def state():
                        return (len(book.tickets), list(book.updates), list(book.cancellations),
                                ticket.StopPrice, ticket.Status)
                    before = state()
                    if not failed:
                        attempts[action]()
                        self.assertNotEqual(state(), before)
                        continue
                    self.fail_slippage(algo)
                    with self.assertRaises(orders.Uncertain) as caught:
                        attempts[action]()
                    self.assertIn("missing-n", str(caught.exception))
                    self.assertEqual(state(), before)

    def test_normal_stop_market_fills_keep_their_price_and_recorded_slippage(self):
        for kind in ("entry", "add", "exit"):
            with self.subTest(kind=kind):
                algo, ticket = self.resting(kind)
                sent = len(algo.client.sent)
                self.stop_market_fill(algo, ticket)
                self.feed(algo, 10)
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertIsNone(algo.security.slippage_model.failure)
                [fill] = [e["payload"] for e in algo.client.sent[sent:]
                          if e["type"] == "execution.fill"]
                self.assertEqual(fill["kind"], "stop" if kind == "exit" else kind)
                self.assertEqual(fill["quantity"], 100)
                self.assertEqual(fill["slippage_applied"], 0.06)
                self.assertEqual(fill["price"], ticket.StopPrice + (-0.06 if kind == "exit" else 0.06))


class SlippageRecordingTests(unittest.TestCase):
    def test_every_callback_failure_is_recorded_and_returns_a_finite_fallback(self):
        def broken(*args):
            raise RuntimeError("callback failed")
        for stage in ("lookup", "arithmetic", "record", "tag"):
            with self.subTest(stage=stage):
                model = orders.NSlippageModel(0.05, broken if stage == "lookup" else lambda tag: 2.0,
                                              broken if stage == "record" else None)
                if stage == "arithmetic":
                    model.n_for_tag = lambda tag: None
                order = types.SimpleNamespace(Id=7, Tag="proposal")
                if stage == "tag":
                    del order.Tag
                self.assertEqual(model.GetSlippageApproximation(None, order), 0.0)
                self.assertIn("7", model.failure)
                self.assertIn("ADR 0013", model.failure)
                if stage != "tag":
                    self.assertIn("proposal", model.failure)

    def test_first_failure_survives_later_failure_and_success(self):
        def n_for_tag(tag):
            if tag != "known":
                raise ValueError("missing N: " + tag)
            return 2.0
        model = orders.NSlippageModel(0.05, n_for_tag)
        self.assertEqual(model.GetSlippageApproximation(
            None, types.SimpleNamespace(Id=7, Tag="first")), 0.0)
        first = model.failure
        self.assertIn("first", first)
        self.assertEqual(model.get_slippage_approximation(
            None, types.SimpleNamespace(Id=8, Tag="second")), 0.0)
        self.assertEqual(model.failure, first)
        self.assertEqual(model.GetSlippageApproximation(
            None, types.SimpleNamespace(Id=9, Tag="known")), 0.1)
        self.assertEqual(model.failure, first)


if __name__ == "__main__":
    unittest.main()
