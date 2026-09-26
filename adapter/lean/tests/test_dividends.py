"""ADR 0024 dividends use raw holdings and LEAN's own cash (ADRs 0004/0019)."""
import copy
import json
import subprocess
import types
from datetime import datetime
from pathlib import Path

import test_orders as order_fixtures
from test_orders import (OrderTestCase, campaign_exited, envelope,
                         period_end, units_stopped)
import test_algorithm as scaffold
from test_publisher import Frame, bar
from client import Unavailable


def dividend_reply(sent):
    action = sent["payload"]
    payload = {key: action[key] for key in
               ("instrument_id", "effective_at", "cash_amount", "currency")}
    payload.update(campaign_id="campaign:AAPL:2014-06-06T20:00:00.000000000Z",
                   corporate_action_id=sent["id"],
                   rule="campaign.dividend.credited-as-cash", adr="0024")
    return {"payload": {"decisions": [envelope(
        "strategy.campaign.dividend", 1, "dividend:" + sent["id"], payload)]}}


class DividendTests(OrderTestCase):
    held = order_fixtures.SplitTests.held

    def pay(self, algo, credit=25, distribution=0.25, day=11, with_bar=False,
            reply=dividend_reply, symbol="AAPL"):
        self.at(algo, day)
        algo.Portfolio.Cash += credit
        algo.Portfolio.TotalPortfolioValue += credit
        algo.client.reply_overrides = {"market.corporate-action": reply}
        b = bar(day)
        algo.History = lambda *args, **kwargs: Frame(b.EndTime, ratio=self.ratio)
        algo.OnData(scaffold.slice_of(
            {"AAPL": b} if with_bar else {}, dividends={symbol: types.SimpleNamespace(
                Distribution=distribution, Time=datetime(2014, 6, day))}))

    def test_holding_publishes_raw_cash_before_bar_and_credits_next_snapshot(self):
        algo, sell = self.held()  # 5,600 engine shares, 100 raw shares.
        before_cash = algo.Portfolio.Cash
        before_order = (sell.Quantity, sell.StopPrice, sell.Tag)
        start = len(algo.client.sent)
        self.pay(algo, with_bar=True)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(self.types_sent(algo, start), [
            "account.snapshot", "market.corporate-action", "market.bar.completed",
            "market.session.closed"])
        action = self.sent(algo, "market.corporate-action")[0]
        self.assertEqual(action["schema_version"], 3)
        self.assertEqual(action["event_time"], "2014-06-11T04:00:00Z")
        self.assertEqual(action["payload"], {
            "instrument_id": "AAPL", "kind": "dividend", "cash_amount": 25,
            "currency": "USD", "effective_at": "2014-06-11T04:00:00Z"})
        self.assertEqual(self.sent(algo, "account.snapshot")[-1]["payload"]["available_cash"],
                         before_cash)
        algo.OnEndOfAlgorithm()
        snapshot = self.sent(algo, "account.snapshot")[-1]["payload"]
        self.assertEqual((snapshot["available_cash"], snapshot["as_of"]),
                         (before_cash + 25, period_end(11)))
        self.assertEqual((sell.Quantity, sell.StopPrice, sell.Tag), before_order)
        self.assertEqual([e["sequence"] for e in algo.client.sent],
                         list(range(2, len(algo.client.sent) + 2)))

    def test_published_payload_matches_go_contract(self):
        algo, _ = self.held()
        self.pay(algo, with_bar=True)
        [action] = self.sent(algo, "market.corporate-action")
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/corporate_action_contract.go"],
            input=json.dumps(action, separators=(",", ":")), text=True, capture_output=True,
            cwd=Path(__file__).resolve().parents[3])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_flat_dividend_is_logged_without_publishing(self):
        self.ratio = 1
        algo = self.start()
        self.pay(algo, credit=0, with_bar=True)
        self.assertFalse(algo.failed)
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])
        self.assertTrue(any("dividend" in m and "flat" in m for m in algo.logs))

    def test_dividend_reply_fixture_matches_go_contract(self):
        algo, _ = self.held()
        self.pay(algo)
        [action] = self.sent(algo, "market.corporate-action")
        decisions = dividend_reply(action)["payload"]["decisions"]
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/order_decisions_contract.go"],
            input=json.dumps(decisions), text=True, capture_output=True,
            cwd=Path(__file__).resolve().parents[3])
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_other_symbols_dividend_is_ignored(self):
        algo, _ = self.held()
        self.pay(algo, credit=0, symbol="MSFT", with_bar=True)
        self.assertFalse(algo.failed)
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_engine_refusal_stops_before_bar_and_never_reuses_stream(self):
        algo, _ = self.held()
        def refuse(sent):
            raise Unavailable("engine refused dividend")
        self.pay(algo, reply=refuse, with_bar=True)
        self.assertTrue(algo.failed)
        self.assertIn("engine refused dividend", algo.quit_reason)
        self.assertNotIn(period_end(11), [e["event_time"] for e in algo.client.sent])
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_unexpected_engine_replies_stop_before_bar(self):
        mutations = [
            lambda r: r.update(sequence=999),
            lambda r: r["payload"].update(decisions=[]),
            lambda r: r["payload"]["decisions"].append(copy.deepcopy(r["payload"]["decisions"][0])),
            lambda r: r["payload"]["decisions"][0].update(type="strategy.engine.state"),
            lambda r: r["payload"]["decisions"][0].update(schema_version=2),
        ]
        for key, value in (("cash_amount", 26), ("currency", "EUR"),
                           ("instrument_id", "MSFT"), ("campaign_id", "other"),
                           ("corporate_action_id", "other"), ("effective_at", period_end(10)),
                           ("rule", "other"), ("adr", "0023")):
            mutations.append(lambda r, k=key, v=value:
                             r["payload"]["decisions"][0]["payload"].update({k: v}))
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                algo, _ = self.held()
                def reply(sent):
                    result = dividend_reply(sent)
                    mutate(result)
                    return result
                self.pay(algo, reply=reply, with_bar=True)
                self.assertTrue(algo.failed)
                self.assertNotIn(period_end(11), [e["event_time"] for e in algo.client.sent])

    def test_cash_mismatch_stops_before_next_snapshot(self):
        for credit in (0, 24.98, 25.02, float("nan"), float("inf")):
            with self.subTest(credit=credit):
                algo, _ = self.held()
                self.pay(algo, credit=credit)
                self.feed(algo, 11)
                self.assertTrue(algo.failed)
                self.assertIn("dividend cash mismatch", algo.quit_reason)
                self.assertIn("AAPL", algo.quit_reason)
                self.assertNotIn(period_end(11),
                                 [e["payload"]["as_of"] for e in self.sent(algo, "account.snapshot")])

    def test_one_cent_tolerance_and_barless_payment_reach_next_snapshot(self):
        for credit in (24.99, 25, 25.01):
            with self.subTest(credit=credit):
                algo, _ = self.held()
                before = algo.Portfolio.Cash
                self.pay(algo, credit=credit)
                self.feed(algo, 11)
                algo.OnEndOfAlgorithm()
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual(self.sent(algo, "account.snapshot")[-1]["payload"]["available_cash"],
                                 before + credit)

    def test_fill_and_fee_between_dividend_and_snapshot_are_explained(self):
        algo, sell = self.held()
        self.pay(algo)
        self.fill(algo, sell, 11, 44.8, fee=1)
        # The existing order fake changes shares but leaves cash to its caller.
        algo.Portfolio.Cash += 100 * 44.8 - 1
        self.feed(algo, 11, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(11), campaign_exited(11)]}}})
        algo.OnEndOfAlgorithm()
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))

    def test_holding_disagreement_is_not_adopted(self):
        for holding in (0, 99, 101, 100.5):
            with self.subTest(holding=holding):
                algo, _ = self.held()
                algo.Portfolio.holdings["AAPL"] = holding
                self.pay(algo)
                self.assertTrue(algo.failed)
                self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_invalid_distribution_is_refused(self):
        for distribution in (0, -1, float("nan"), float("inf")):
            with self.subTest(distribution=distribution):
                algo, _ = self.held()
                self.pay(algo, distribution=distribution)
                self.assertTrue(algo.failed)
                self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_terminal_dividend_without_a_later_close_stops(self):
        algo, _ = self.held()
        self.pay(algo)
        algo.OnEndOfAlgorithm()
        self.assertTrue(algo.failed)
        self.assertIn("dividend", algo.quit_reason)
        self.assertIn("snapshot", algo.quit_reason)
        self.assertEqual(self.sent(algo, "replay.run.completed"), [])

    def test_any_completion_requires_the_dividend_to_reach_a_snapshot(self):
        algo, _ = self.held()
        self.pay(algo)
        algo.complete_run()
        self.assertTrue(algo.failed)
        self.assertEqual(self.sent(algo, "replay.run.completed"), [])

    def test_split_cash_and_dividend_are_both_explained(self):
        algo, _ = self.held()
        order_fixtures.SplitTests.split(self, algo, 11, checked=False)
        algo.verify_split()
        algo.Transactions.settle()
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.ratio = 28
        before = algo.Portfolio.Cash
        self.pay(algo, credit=50, with_bar=True)
        algo.OnEndOfAlgorithm()
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual([e["payload"]["kind"] for e in self.sent(algo, "market.corporate-action")],
                         ["split", "dividend"])
        self.assertEqual(self.sent(algo, "account.snapshot")[-1]["payload"]["available_cash"],
                         before + 50)
