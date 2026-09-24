"""The adapter turns the engine's decisions into LEAN orders.

Every test here drives the real CompletedBarsAlgorithm through OnData with a
fake engine whose replies carry fixture decisions, and asserts on the orders
that reach LEAN's order book (FakeTransactions) — never on the adapter's own
state. The fixtures use the engine's own field names; FixtureContractTests
checks them against the Go payload types, so a renamed field fails here.
"""
import json
import subprocess
import sys
import types
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

import test_algorithm as scaffold
from test_publisher import Frame, bar

from client import Unavailable

algorithm = scaffold.algorithm


def period_end(day):
    return "2014-06-{:02d}T20:00:00Z".format(day)


def decision_id(kind, day):
    """The reducer's own id shape (internal/strategy decisionID)."""
    return "{}:AAPL:2014-06-{:02d}T20:00:00.000000000Z".format(kind, day)


def envelope(event_type, schema_version, ident, payload):
    return {"id": ident, "type": event_type, "schema_version": schema_version,
            "envelope_version": 1, "payload": payload}


def trade_proposal(day, **changes):
    payload = {"instrument_id": "AAPL", "period_end": period_end(day),
               "signal_id": decision_id("signal", day),
               "rule": "unit.sizing.volatility-normalised", "adr": "0003",
               "direction": "long", "entry_level": 24.5, "quantity": 100,
               "n": 1.2, "sizing_mode": "volatility-normalised",
               "unit_volatility_fraction": 0.01, "stop_multiple": 2,
               "risk_at_stop": 0.02, "realised_risk_at_stop": 0.02,
               "dollars_per_point": 1, "notional_account": 1000000,
               "protective_stop_intent": 22.1}
    payload.update(changes)
    return envelope("strategy.trade.proposed", 1, decision_id("proposal", day), payload)


def add_proposal(day, unit_index=2, **changes):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "period_end": period_end(day), "unit_index": unit_index,
               "level": 25.1, "quantity": 100, "previous_unit_fill": 24.5,
               "campaign_n": 1.2, "rule": "add.ladder.half-n", "adr": "0006"}
    payload.update(changes)
    return envelope("strategy.add.proposed", 1,
                    decision_id("add-proposal-unit-{}".format(unit_index), day), payload)


def proposal_expired(proposal, day, kind="entry"):
    payload = {"instrument_id": "AAPL", "kind": kind, "proposal_id": proposal["id"],
               "signal_id": proposal["payload"].get("signal_id", ""),
               "period_end": proposal["payload"]["period_end"],
               "expired_at": period_end(day), "earliest_fill_at": period_end(day - 1),
               "rule": "signal.expires.with-its-bar", "adr": "0011",
               "reason": "superseded-by-next-bar", "quantity": 100, "level": 24.5}
    return envelope("strategy.proposal.expired", 3, decision_id("proposal-expired", day), payload)


def campaign_opened(day=6, campaign_n=1.2):
    campaign_id = decision_id("campaign", day)
    payload = {"campaign_id": campaign_id, "instrument_id": "AAPL",
               "proposal_id": decision_id("proposal", day - 1),
               "signal_id": decision_id("signal", day - 1), "fill_id": "fill-1",
               "rule": "campaign.opened.from-fill", "adr": "0006", "direction": "long",
               "campaign_n": campaign_n, "unit_quantity": 100, "filled_quantity": 100,
               "entry_price": 24.5, "stop_multiple": 2, "protective_stop": 22.1,
               "units": 1, "opened_at": period_end(day)}
    return envelope("strategy.campaign.opened", 1, campaign_id, payload)


def exit_order_set(day, unit_index=1, level=22.1, quantity=100, cause="bar", **changes):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "unit_index": unit_index, "level": level, "quantity": quantity,
               "source": "protective-stop", "protective_stop": level,
               "exit_channel_level": 0, "as_of": period_end(day),
               "rule": "exit-order.higher-of-stop-and-exit-channel", "adr": "0005"}
    payload.update(changes)
    return envelope("strategy.exit-order.set", 1,
                    decision_id("exit-order-set-unit-{}-{}".format(unit_index, cause), day),
                    payload)


class OrderTestCase(unittest.TestCase):
    def start(self):
        algo = scaffold.AlgorithmTests.init(self)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        algo.IsWarmingUp = False
        return algo

    def feed(self, algo, day, decisions=(), snapshot_decisions=()):
        """One completed bar whose bar and snapshot replies carry decisions."""
        algo.client.reply_overrides = {
            "market.bar.completed": {"payload": {"decisions": list(decisions)}},
            "account.snapshot": {"payload": {"decisions": list(snapshot_decisions)}}}
        b = bar(day)
        algo.History = lambda *args, **kwargs: Frame(b.EndTime)
        algo.OnData(scaffold.slice_of({"AAPL": b}))

    def tickets(self, algo):
        return algo.Transactions.tickets

    def sells(self, algo):
        return [t for t in algo.Transactions.tickets if t.Quantity < 0]

    def hold(self, algo, shares):
        """LEAN holds shares from an entry order this adapter placed and LEAN filled.

        The fill is applied to the fake book directly, as a returned fill
        would leave it; OnOrderEvent is not raised, since today a fill stops
        the run (FillStopsTheRunTests).
        """
        self.feed(algo, 6, [trade_proposal(6, quantity=shares)])
        [entry] = self.tickets(algo)
        entry.Status = "filled"
        entry.QuantityFilled = shares
        algo.Portfolio.holdings["AAPL"] = shares

    def assert_stopped_after(self, algo, sent_before_stop):
        """The run stopped, and nothing further reaches LEAN or the engine."""
        self.assertTrue(algo.failed)
        orders = len(getattr(algo, "orders", []))
        self.feed(algo, 10, [trade_proposal(10), exit_order_set(10, unit_index=3)])
        self.assertEqual(len(getattr(algo, "orders", [])), orders)
        self.assertEqual(len(algo.client.sent), sent_before_stop)

    def rejections(self, algo):
        return [m for m in getattr(algo, "logs", []) if "REJECTED" in m]


class EntryAndAddOrderTests(OrderTestCase):
    def test_a_valid_proposal_becomes_a_day_stop_market_order_at_its_level(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed)
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Symbol, ticket.Quantity, ticket.StopPrice, ticket.Tag),
                         ("AAPL", 100, 24.5, proposal["id"]))
        self.assertEqual(ticket.TimeInForce, "day")
        self.assertEqual(self.rejections(algo), [])

    def test_a_valid_add_proposal_becomes_a_day_stop_market_order_at_its_rung(self):
        algo = self.start()
        proposal = add_proposal(9)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Quantity, ticket.StopPrice, ticket.Tag, ticket.TimeInForce),
                         (100, 25.1, proposal["id"], "day"))

    def test_a_redelivered_proposal_creates_no_second_order(self):
        algo = self.start()
        proposal = trade_proposal(9)
        # Delivered twice in one bar's replies: once with the bar, again with
        # the snapshot.
        self.feed(algo, 9, [proposal, proposal], [proposal])
        self.assertEqual(len(self.tickets(algo)), 1)
        self.assertFalse(algo.failed)

    def test_duplicate_detection_reads_leans_order_book_not_adapter_memory(self):
        algo = self.start()
        proposal = trade_proposal(9)
        # An order carrying this decision id is already in LEAN's book —
        # filled, so not even working — although this adapter instance never
        # submitted it.
        props = scaffold.OrderProperties()
        props.TimeInForce = "day"
        existing = scaffold.FakeTicket(algo.Transactions, 1, "AAPL", 100, 24.5,
                                       proposal["id"], props)
        existing.Status = "filled"
        algo.Transactions.tickets.append(existing)
        self.feed(algo, 9, [proposal])
        self.assertEqual(self.tickets(algo), [existing])
        self.assertTrue(any("already submitted" in m for m in self.rejections(algo)))

    def test_a_stale_proposal_is_rejected_with_its_reason(self):
        algo = self.start()
        self.feed(algo, 6)
        # A proposal produced by the 6th's bar, arriving in answer to the
        # 9th's: its one-bar window has passed.
        stale = trade_proposal(6)
        self.feed(algo, 9, [stale])
        self.assertEqual(self.tickets(algo), [])
        [rejection] = self.rejections(algo)
        self.assertIn(stale["id"], rejection)
        self.assertIn("stale", rejection)

    def test_a_proposal_for_another_instrument_is_rejected(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, instrument_id="MSFT")])
        self.assertEqual(self.tickets(algo), [])
        [rejection] = self.rejections(algo)
        self.assertIn("MSFT", rejection)
        self.assertIn("not this run's tradable instrument", rejection)

    def test_a_proposal_for_an_untradable_security_is_rejected(self):
        algo = self.start()
        algo.Securities["AAPL"].IsTradable = False
        self.feed(algo, 9, [trade_proposal(9)])
        self.assertEqual(self.tickets(algo), [])
        [rejection] = self.rejections(algo)
        self.assertIn("not tradable", rejection)

    def test_a_quantity_that_is_not_a_positive_integer_is_rejected(self):
        for quantity in (0, -100, 100.5, "100", True, None):
            with self.subTest(quantity=quantity):
                algo = self.start()
                self.feed(algo, 9, [trade_proposal(9, quantity=quantity)])
                self.assertEqual(self.tickets(algo), [])
                [rejection] = self.rejections(algo)
                self.assertIn("quantity", rejection)

    def test_a_level_that_is_not_a_positive_price_is_rejected(self):
        for level in (0, -1, None, "24.5"):
            with self.subTest(level=level):
                algo = self.start()
                self.feed(algo, 9, [trade_proposal(9, entry_level=level)])
                self.assertEqual(self.tickets(algo), [])
                self.assertEqual(len(self.rejections(algo)), 1)

    def test_a_short_proposal_is_rejected(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, direction="short")])
        self.assertEqual(self.tickets(algo), [])
        self.assertIn("direction", self.rejections(algo)[0])

    def test_an_order_lean_refuses_is_recorded(self):
        algo = self.start()
        algo.Transactions.submit_status = "invalid"
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [rejection] = self.rejections(algo)
        self.assertIn(proposal["id"], rejection)
        self.assertIn("LEAN refused", rejection)

    def test_go_unreachable_means_nothing_is_submitted(self):
        algo = self.start()
        engine = algo.client
        answer = engine.decide

        def decide(input_envelope):
            if input_envelope["type"] == "account.snapshot":
                raise Unavailable("engine closed the connection mid-exchange")
            return answer(input_envelope)
        engine.decide = decide
        # The bar's reply carried a proposal, but the exchange that follows it
        # fails: the stream is out of step, so nothing from it is acted on.
        self.feed(algo, 9, [trade_proposal(9)])
        self.assertTrue(algo.failed)
        self.assertEqual(self.tickets(algo), [])
        self.assertEqual(getattr(algo, "orders", []), [])

    def test_nothing_is_submitted_once_the_run_has_stopped(self):
        algo = self.start()
        algo.stop("engine unavailable")
        self.feed(algo, 9, [trade_proposal(9)])
        self.assertEqual(self.tickets(algo), [])

    def test_a_decision_answering_a_warmup_bar_is_not_acted_on(self):
        algo = self.start()
        algo.IsWarmingUp = True
        self.feed(algo, 9, [trade_proposal(9), add_proposal(9)],
                  [campaign_opened(), exit_order_set(9)])
        self.assertFalse(algo.failed)
        self.assertEqual(self.tickets(algo), [])
        self.assertEqual(getattr(algo, "orders", []), [])
        self.assertTrue(any("warm-up" in m and "not acted on" in m for m in algo.logs))

    def test_a_proposal_expiry_cancels_its_unfilled_day_order(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertEqual([order_id for order_id, _ in algo.Transactions.cancellations],
                         [ticket.OrderId])
        self.assertEqual(ticket.Status, "canceled")
        self.assertEqual(len(self.tickets(algo)), 1)

    def test_an_add_proposal_expiry_cancels_its_unfilled_day_order(self):
        algo = self.start()
        proposal = add_proposal(9)
        self.feed(algo, 9, [proposal])
        self.feed(algo, 10, [proposal_expired(proposal, 10, kind="add")])
        self.assertEqual(len(algo.Transactions.cancellations), 1)

    def test_an_unconfirmed_cancellation_stops_the_run(self):
        # An order the engine expired that LEAN has not confirmed cancelled
        # could still fill into a holding the engine does not expect.
        for outcome in ("refused", "pending"):
            with self.subTest(outcome):
                algo = self.start()
                proposal = trade_proposal(9)
                self.feed(algo, 9, [proposal])
                algo.Transactions.cancel_outcome = outcome
                self.feed(algo, 10, [proposal_expired(proposal, 10), add_proposal(10)])
                self.assertIn("did not confirm", algo.quit_reason)
                self.assertIn(proposal["id"], algo.quit_reason)
                self.assertFalse(any("adapter: cancelled order" in m for m in algo.logs))
                self.assertEqual(len(self.tickets(algo)), 1)
                self.assert_stopped_after(algo, len(algo.client.sent))

    def test_a_confirmed_cancellation_is_logged(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertFalse(algo.failed)
        self.assertTrue(any("adapter: cancelled order 1" in m for m in algo.logs))

    def test_a_trade_proposal_while_lean_holds_the_instrument_stops_the_run(self):
        # The engine proposes an entry only when it believes it is flat, so a
        # holding in LEAN means the two disagree.
        algo = self.start()
        self.hold(algo, 100)
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.assertEqual(len(self.tickets(algo)), 1)
        for fact in (proposal["id"], "'AAPL'", "holds 100"):
            self.assertIn(fact, algo.quit_reason)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_an_add_proposal_while_lean_holds_the_instrument_is_placed(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [add_proposal(9)])
        self.assertFalse(algo.failed)
        self.assertEqual([t.Quantity for t in self.tickets(algo)], [100, 100])

    def test_an_expiry_leaves_a_filled_order_alone(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.tickets(algo)[0].Status = "filled"
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertEqual(algo.Transactions.cancellations, [])

    def test_an_unknown_schema_version_stops_the_run(self):
        algo = self.start()
        proposal = trade_proposal(9)
        proposal["schema_version"] = 2
        self.feed(algo, 9, [proposal])
        self.assertTrue(algo.failed)
        self.assertEqual(self.tickets(algo), [])
        self.assertIn("schema", algo.quit_reason)


class ExitOrderTests(OrderTestCase):
    """Each held Unit rests one GTC sell stop at the level the engine sets (ADR 0005's amendment)."""

    def test_exit_order_set_places_one_gtc_stop_per_unit_for_its_quantity(self):
        algo = self.start()
        self.hold(algo, 250)
        first = exit_order_set(9, unit_index=1, level=22.1, quantity=100)
        second = exit_order_set(9, unit_index=2, level=22.7, quantity=150)
        self.feed(algo, 9, [campaign_opened(), first, second])
        self.assertFalse(algo.failed)
        self.assertEqual(
            [(t.Quantity, t.StopPrice, t.Tag, t.TimeInForce) for t in self.sells(algo)],
            [(-100, 22.1, first["id"], "gtc"), (-150, 22.7, second["id"], "gtc")])

    def test_a_second_exit_order_set_amends_the_units_order(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, level=22.1)])
        raised = exit_order_set(10, level=23.4, source="exit-channel", exit_channel_level=23.4)
        self.feed(algo, 10, [raised])
        [ticket] = self.sells(algo)
        self.assertEqual(algo.Transactions.updates, [(ticket.OrderId, 23.4, raised["id"])])
        self.assertEqual((ticket.Quantity, ticket.StopPrice, ticket.Tag), (-100, 23.4, raised["id"]))

    def test_a_redelivered_exit_order_set_changes_nothing(self):
        algo = self.start()
        self.hold(algo, 100)
        placed = exit_order_set(9)
        self.feed(algo, 9, [campaign_opened(), placed, placed])
        self.assertEqual(len(self.sells(algo)), 1)
        self.assertEqual(algo.Transactions.updates, [])

    def test_an_older_exit_order_set_does_not_lower_the_level_in_force(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, level=22.1)])
        self.feed(algo, 10, [exit_order_set(10, level=23.4)])
        self.feed(algo, 11, [exit_order_set(9, level=22.5, cause="late")])
        [ticket] = self.sells(algo)
        self.assertEqual(ticket.StopPrice, 23.4)
        self.assertEqual(len(algo.Transactions.updates), 1)
        self.assertTrue(any("older" in m for m in self.rejections(algo)))

    def test_an_unacknowledged_amendment_leaves_the_previous_level_in_force(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, level=22.1)])
        algo.Transactions.acknowledge_updates = False
        self.feed(algo, 10, [exit_order_set(10, level=23.4)])
        [ticket] = self.sells(algo)
        self.assertEqual(ticket.StopPrice, 22.1)
        self.assertTrue(any("not acknowledged" in m for m in algo.logs))

    def test_the_working_sell_quantity_never_exceeds_the_holding(self):
        algo = self.start()
        self.hold(algo, 100)
        refused = exit_order_set(9, unit_index=2, quantity=100)
        # Unit 2's order would be placed before any later decision in the same
        # reply; the entry after it must not be submitted either.
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, unit_index=1), refused,
                            trade_proposal(9)])
        self.assertEqual([t.Quantity for t in self.sells(algo)], [-100])
        for fact in ("'AAPL'", "unit 2", refused["id"], "working sell quantity 100",
                     "holding of 100"):
            self.assertIn(fact, algo.quit_reason)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_an_exit_order_with_no_holding_stops_the_run(self):
        algo = self.start()
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9)])
        self.assertEqual(self.sells(algo), [])
        for fact in ("'AAPL'", "unit 1", "working sell quantity 0", "holding of 0"):
            self.assertIn(fact, algo.quit_reason)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_an_exit_order_lean_refuses_stops_the_run(self):
        algo = self.start()
        self.hold(algo, 100)
        algo.Transactions.submit_status = "invalid"
        placed = exit_order_set(9)
        self.feed(algo, 9, [campaign_opened(), placed, add_proposal(9)])
        self.assertIn("LEAN refused", algo.quit_reason)
        self.assertIn(placed["id"], algo.quit_reason)
        self.assertEqual([t.Status for t in self.sells(algo)], ["invalid"])
        self.assertEqual(len(self.tickets(algo)), 2)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_a_redelivered_exit_order_whose_order_is_closed_stops_the_run(self):
        # The engine still sets this level, but no working order protects the
        # Unit: redelivery must not be read as "already in force".
        for status in ("filled", "canceled", "invalid"):
            with self.subTest(status):
                algo = self.start()
                self.hold(algo, 100)
                placed = exit_order_set(9)
                self.feed(algo, 9, [campaign_opened(), placed])
                [ticket] = self.sells(algo)
                ticket.Status = status
                self.feed(algo, 10, [placed])
                self.assertIn("no longer working", algo.quit_reason)
                self.assertIn(placed["id"], algo.quit_reason)
                self.assertEqual(self.sells(algo), [ticket])
                self.assertEqual(algo.Transactions.updates, [])
                self.assert_stopped_after(algo, len(algo.client.sent))

    def test_a_malformed_exit_order_stops_the_run(self):
        # An Exit Order the adapter cannot place leaves its Unit without a
        # stop, so, unlike an entry or Add the engine re-issues next bar, it
        # stops the run rather than being rejected and forgotten.
        for name, changes in (("another instrument", {"instrument_id": "MSFT"}),
                              ("zero quantity", {"quantity": 0}),
                              ("fractional quantity", {"quantity": 100.5}),
                              ("non-positive level", {"level": 0}),
                              ("unreadable as_of", {"as_of": "not a time"})):
            with self.subTest(name):
                algo = self.start()
                self.hold(algo, 100)
                bad = exit_order_set(9)
                bad["payload"].update(changes)
                self.feed(algo, 9, [campaign_opened(), bad, trade_proposal(9)])
                self.assertEqual(self.sells(algo), [])
                self.assertEqual(self.rejections(algo), [])
                self.assertIn("cannot be protected", algo.quit_reason)
                self.assertIn(bad["id"], algo.quit_reason)
                self.assert_stopped_after(algo, len(algo.client.sent))

    def test_an_exit_order_without_its_campaigns_n_stops_the_run(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [exit_order_set(9)])
        self.assertTrue(algo.failed)
        self.assertEqual(self.sells(algo), [])

    def test_a_unit_whose_order_is_no_longer_working_stops_the_run(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9)])
        self.sells(algo)[0].Status = "filled"
        self.feed(algo, 10, [exit_order_set(10, level=23.4)])
        self.assertTrue(algo.failed)
        self.assertEqual(len(self.sells(algo)), 1)
        self.assertEqual(algo.Transactions.updates, [])


class SlippageTests(OrderTestCase):
    """ADR 0013: every fill slips slippage_n x the N the engine supplied."""

    def slip(self, algo, tag):
        return algo.security.slippage_model.GetSlippageApproximation(
            algo.security, types.SimpleNamespace(Tag=tag))

    def test_an_entry_slips_by_the_proposals_n(self):
        algo = self.start()
        proposal = trade_proposal(9, n=1.2)
        self.feed(algo, 9, [proposal])
        self.assertAlmostEqual(self.slip(algo, proposal["id"]), 0.05 * 1.2)

    def test_an_add_slips_by_its_campaign_n(self):
        algo = self.start()
        proposal = add_proposal(9, campaign_n=1.7)
        self.feed(algo, 9, [proposal])
        self.assertAlmostEqual(self.slip(algo, proposal["id"]), 0.05 * 1.7)

    def test_an_exit_order_slips_by_the_campaigns_frozen_n_after_amendment_too(self):
        algo = self.start()
        self.hold(algo, 100)
        placed = exit_order_set(9)
        self.feed(algo, 9, [campaign_opened(campaign_n=2.5), placed])
        raised = exit_order_set(10, level=23.4)
        self.feed(algo, 10, [raised])
        self.assertAlmostEqual(self.slip(algo, placed["id"]), 0.05 * 2.5)
        self.assertAlmostEqual(self.slip(algo, raised["id"]), 0.05 * 2.5)

    def test_an_order_without_a_supplied_n_is_never_slipped_by_zero(self):
        algo = self.start()
        with self.assertRaises(ValueError):
            self.slip(algo, "an order this adapter never placed")

    def test_commission_uses_leans_interactive_brokers_fee_model(self):
        algo = self.start()
        self.assertIsInstance(algo.security.fee_model, scaffold.InteractiveBrokersFeeModel)

    def test_the_slippage_fraction_is_the_runs_own_setting(self):
        for bad in (None, 0, -0.05, "0.05", float("nan"), float("inf"), True):
            with self.subTest(slippage_n=bad):
                settings = {"socket": "unused", "configuration_hash": "hash",
                            "strategy_version": "version", "run_id": "test",
                            "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                            "warmup_bars": 3, "cash": 1000000,
                            "lean_image": scaffold.VALID_LEAN_IMAGE}
                if bad is not None:
                    settings["slippage_n"] = bad
                algo = algorithm.CompletedBarsAlgorithm()
                with scaffold.patch.object(algorithm, "load_settings", return_value=settings), \
                        scaffold.patch.object(algorithm, "Client"):
                    algo.Initialize()
                self.assertTrue(algo.failed)
                self.assertIn("slippage_n", algo.quit_reason)


class FillStopsTheRunTests(OrderTestCase):
    """A LEAN fill cannot yet be returned to the engine, so the first one stops the run.

    Otherwise the engine would still believe it was flat: it would place no
    Exit Order for the new holding and could propose further entries.
    """

    def event(self, status, order_id=1):
        return types.SimpleNamespace(OrderId=order_id, Status=status, Symbol="AAPL",
                                     FillQuantity=100, FillPrice=24.6)

    def test_the_first_fill_stops_the_run(self):
        for status in ("filled", "partially-filled"):
            with self.subTest(status):
                algo = self.start()
                proposal = trade_proposal(9)
                self.feed(algo, 9, [proposal])
                sent = len(algo.client.sent)
                algo.OnOrderEvent(self.event(status))
                self.assertIn("cannot yet be returned to the engine", algo.quit_reason)
                self.assertIn("order 1", algo.quit_reason)
                self.assertIn(proposal["id"], algo.quit_reason)
                self.assert_stopped_after(algo, sent)

    def test_order_events_other_than_fills_do_not_stop_the_run(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        for status in ("submitted", "canceled", "cancel-pending", "invalid"):
            algo.OnOrderEvent(self.event(status))
        self.assertFalse(algo.failed)


class StartupReconciliationTests(OrderTestCase):
    """LEAN must hold nothing and work no order for the instrument before the adapter trades.

    docs/architecture.md: reconcile before any executor submits. A backtest
    starts flat, so this always passes there; it is checked anyway.
    """

    def start_with(self, prepare):
        settings = {"socket": "unused", "configuration_hash": "hash",
                    "strategy_version": "version", "run_id": "test",
                    "symbol": "AAPL", "start": "2014-06-09", "end": "2014-06-10",
                    "warmup_bars": 3, "cash": 1000000,
                    "lean_image": scaffold.VALID_LEAN_IMAGE, "slippage_n": 0.05}
        algo = algorithm.CompletedBarsAlgorithm()
        prepare(algo)
        with scaffold.patch.object(algorithm, "load_settings", return_value=settings), \
                scaffold.patch.object(algorithm, "Client",
                                      return_value=scaffold.FakeEngineClient()):
            algo.Initialize()
        return algo

    def foreign_order(self, algo):
        props = scaffold.OrderProperties()
        props.TimeInForce = "gtc"
        ticket = scaffold.FakeTicket(algo.Transactions, 99, "AAPL", -50, 20.0,
                                     "not placed by this adapter", props)
        algo.Transactions.tickets.append(ticket)
        return ticket

    def test_a_holding_at_startup_stops_the_run(self):
        algo = self.start_with(lambda a: setattr(a, "initial_holdings", {"AAPL": 100}))
        self.assertTrue(algo.failed)
        self.assertIn("holds 100", algo.quit_reason)
        self.assertIn("at startup", algo.quit_reason)

    def test_an_open_order_at_startup_stops_the_run(self):
        algo = self.start_with(self.foreign_order)
        self.assertTrue(algo.failed)
        self.assertIn("1 open order", algo.quit_reason)
        self.assertIn("at startup", algo.quit_reason)

    def test_a_flat_start_passes(self):
        algo = self.start_with(lambda a: None)
        self.assertFalse(algo.failed)

    def test_an_open_order_before_the_first_order_stops_the_run(self):
        algo = self.start()
        self.foreign_order(algo)
        self.feed(algo, 9, [trade_proposal(9)])
        self.assertIn("before the first order", algo.quit_reason)
        self.assertIn("1 open order", algo.quit_reason)
        self.assertEqual(len(self.tickets(algo)), 1)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_a_holding_before_the_first_order_stops_the_run(self):
        algo = self.start()
        algo.Portfolio.holdings["AAPL"] = 100
        self.feed(algo, 9, [add_proposal(9)])
        self.assertIn("before the first order", algo.quit_reason)
        self.assertIn("holds 100", algo.quit_reason)
        self.assertEqual(self.tickets(algo), [])


class StartupReportTests(OrderTestCase):
    def test_the_startup_report_lists_every_adr_0005_departure(self):
        algo = self.start()
        report = [m for m in algo.logs if m.startswith("adapter: fill model:")]
        text = "\n".join(report)
        for topic in ("gap", "same-bar", "intrabar", "touch", "DAY", "slippage",
                      "0.05 x N", "LEAN's default equity slippage", "commission",
                      "InteractiveBrokersFeeModel", "$0.005"):
            self.assertIn(topic, text)
        # Logged at startup, before any bar reaches the engine.
        self.assertEqual(algo.client.sent, [])


class FixtureContractTests(unittest.TestCase):
    """The fixtures above name only fields the Go payloads define."""

    def test_fixture_fields_and_schema_versions_match_the_go_payloads(self):
        proposal = trade_proposal(9)
        fixtures = [proposal, add_proposal(9), proposal_expired(proposal, 10),
                    campaign_opened(), exit_order_set(9)]
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/order_decisions_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(fixtures), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
