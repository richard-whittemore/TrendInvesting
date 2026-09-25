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
from datetime import datetime, timezone
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


def utc(day):
    """LEAN's UtcTime at day's close: the instant its daily bar ends."""
    return datetime(2014, 6, day, 20, tzinfo=timezone.utc)


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


def campaign_opened(day=6, campaign_n=1.2, fill_id="lean:1:2"):
    campaign_id = decision_id("campaign", day)
    payload = {"campaign_id": campaign_id, "instrument_id": "AAPL",
               "proposal_id": decision_id("proposal", day - 1),
               "signal_id": decision_id("signal", day - 1), "fill_id": fill_id,
               "rule": "campaign.opened.from-fill", "adr": "0006", "direction": "long",
               "campaign_n": campaign_n, "unit_quantity": 100, "filled_quantity": 100,
               "entry_price": 24.5, "stop_multiple": 2, "protective_stop": 22.1,
               "units": 1, "opened_at": period_end(day)}
    return envelope("strategy.campaign.opened", 1, campaign_id, payload)


def exit_proposed(day, level=23.4, campaign_day=6):
    payload = {"campaign_id": decision_id("campaign", campaign_day), "instrument_id": "AAPL",
               "period_end": period_end(day), "reason": "exit-channel-breached", "level": level,
               "quantity": 100, "rule": "exit.channel.breached", "adr": "0005"}
    return envelope("strategy.exit.proposed", 1, decision_id("exit-proposal", day), payload)


def unit_added(day, unit_index=2, fill_id="lean:2:2", quantity=100):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "unit_index": unit_index, "fill_id": fill_id, "fill_price": 25.2,
               "quantity": quantity, "campaign_n": 1.2, "stop_multiple": 2,
               "protective_stop": 22.8, "units": unit_index, "added_at": period_end(day),
               "rule": "add.ladder.half-n", "adr": "0006"}
    return envelope("strategy.campaign.unit-added", 1,
                    decision_id("unit-added-{}".format(unit_index), day), payload)


def units_stopped(day, unit_indexes=(1,), fill_id="lean:3:2", remaining=0):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "fill_id": fill_id, "unit_indexes": list(unit_indexes), "fill_price": 22.0,
               "quantity_closed": 100, "entry_price": 24.56, "campaign_n": 1.2,
               "dollars_per_point": 1, "realised_result": -256, "stopped_at": period_end(day),
               "remaining_units": remaining, "aggregate_open_risk_after": 0,
               "rule": "campaign.units-stopped.by-stop", "adr": "0005"}
    return envelope("strategy.campaign.units-stopped", 1,
                    decision_id("units-stopped-{}".format(fill_id), day), payload)


def campaign_exited(day, reason="stop"):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "fill_id": "lean:3:2", "exited_at": period_end(day), "reason": reason,
               "entry_price": 24.56, "exit_price": 22.0, "quantity": 100, "campaign_n": 1.2,
               "dollars_per_point": 1, "unit_quantity": 100, "protective_stop_level": 22.1,
               "realised_result": -256, "average_move_in_n": -2.1,
               "realised_result_in_unit_n": -2.1, "units": 1,
               "rule": "campaign.exited.by-stop", "adr": "0005"}
    return envelope("strategy.campaign.exited", 2, decision_id("campaign-exited", day), payload)


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


# The decisions below carry no order of their own (README.md's own decision
# table): OrderDesk.act ignores every one of them (they are not in
# SCHEMA_VERSIONS). Issue #31 still wants a pinned fixture for every decision
# type the adapter may receive, acted on or not, so IgnoredDecisionTests below
# checks the adapter is unmoved by any of them and FixtureContractTests checks
# each against Go's own contract (order_decisions_contract.go). Every derived
# figure below (aggregate_open_risk, the drawdown/cash-adjustment/rebase
# figures, and the raised protective stop) was computed and validated by Go's
# own internal/sizing helpers, not typed by hand, so it cannot silently drift
# from the exact-equality rules each payload's own Validate enforces.


def campaign_evaluated(day, unit_index=1, entry_price=24.5, quantity=100, protective_stop=22.1):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "period_end": period_end(day), "protective_stop": protective_stop,
               "units": [{"unit_index": unit_index, "entry_price": entry_price,
                          "quantity": quantity, "protective_stop": protective_stop}],
               "exit_channel_low": 20.0, "exit_channel_ready": True, "exit_condition_met": False,
               "dollars_per_point": 1, "aggregate_open_risk": 239.99999999999986,
               "notional_account": 1000000, "aggregate_open_risk_fraction": 0.00023999999999999987}
    return envelope("strategy.campaign.evaluated", 2, decision_id("campaign-evaluated", day), payload)


def drawdown_step_applied(day):
    payload = {"as_of": period_end(day), "equity": 890000.0, "threshold": 900000.0,
               "notional_before": 1000000.0, "notional_after": 800000.0, "step_number": 1,
               "rule": "notional-account.drawdown-step", "adr": "0007"}
    return envelope("strategy.drawdown-step.applied", 1, decision_id("drawdown-step", day), payload)


def notional_account_cash_adjusted(day):
    payload = {"as_of": period_end(day), "amount": 100000.0, "equity_before": 1000000.0,
               "equity_after": 1100000.0, "starting_figure_before": 1000000.0,
               "starting_figure_after": 1100000.0, "notional_before": 1000000.0,
               "notional_after": 1100000.0, "rule": "notional-account.cash-adjustment", "adr": "0007"}
    return envelope("strategy.notional-account.cash-adjusted", 1,
                    decision_id("notional-cash-adjusted", day), payload)


def notional_account_rebased(day):
    payload = {"as_of": period_end(day), "previous_starting_figure": 1000000.0,
               "new_starting_figure": 1250000.0, "equity": 1250000.0,
               "rule": "notional-account.rebase", "adr": "0007"}
    return envelope("strategy.notional-account.rebased", 1, decision_id("notional-rebased", day), payload)


def notional_account_recovered(day):
    payload = {"as_of": period_end(day), "equity": 1000000.0, "starting_figure": 1000000.0,
               "notional_before": 800000.0, "steps_cleared": 1,
               "rule": "notional-account.recovery", "adr": "0007"}
    return envelope("strategy.notional-account.recovered", 1, decision_id("notional-recovered", day), payload)


def proposal_declined(day, kind="entry"):
    payload = {"instrument_id": "AAPL", "period_end": period_end(day), "kind": kind,
               "signal_id": decision_id("signal", day) if kind == "entry" else "",
               "campaign_id": decision_id("campaign", 6) if kind == "add" else "",
               "reason": "quantity-below-one-unit", "detail": "quantity 0 is below one unit",
               "required_cash": 0.0, "available_cash": 0.0,
               "cap": "", "cap_limit": 0, "post_trade_exposure": 0}
    return envelope("strategy.proposal.declined", 5, decision_id("proposal-declined", day), payload)


def engine_state(day):
    payload = {"state": "halted", "reason": "campaign-without-protective-stop",
               "detail": "campaign {} has no protective stop".format(decision_id("campaign", 6))}
    return envelope("strategy.engine.state", 1, decision_id("engine-state", day), payload)


def setup_evaluated(day):
    payload = {"instrument_id": "AAPL", "period_end": period_end(day), "n": 1.2, "n_ready": True,
               "entry_channel_high": 24.0, "entry_channel_ready": True, "tier": "B",
               "distance_to_entry_in_n": 0.5}
    return envelope("strategy.setup.evaluated", 2, decision_id("setup-evaluated", day), payload)


def signal(day):
    payload = {"instrument_id": "AAPL", "period_end": period_end(day),
               "rule": "entry.channel.breakout", "adr": "0002", "direction": "long",
               "entry_channel_length": 55, "entry_channel_high": 24.0, "breakout_high": 24.5, "n": 1.2}
    return envelope("strategy.signal", 1, decision_id("signal", day), payload)


def protective_stop_set(day, reason="initial", level=22.1, previous_level=0.0):
    rule = "protective-stop.set.from-fill" if reason == "initial" else "stop-ladder.raised-by-half-n"
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL", "unit_index": 1,
               "reason": reason, "as_of": period_end(day), "level": level,
               "previous_level": previous_level, "entry_price": 24.5, "campaign_n": 1.2,
               "stop_multiple": 2, "rule": rule, "adr": "0006"}
    return envelope("strategy.protective-stop.set", 2,
                    decision_id("protective-stop-set-{}".format(reason), day), payload)


class OrderTestCase(unittest.TestCase):
    def start(self):
        algo = scaffold.AlgorithmTests.init(self)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        algo.IsWarmingUp = False
        return algo

    def at(self, algo, day):
        """LEAN's clock reaches day's close: the previous step's deferred
        reports (confirmed cancellations and amendments) arrive first."""
        book = algo.Transactions
        if book.now != utc(day):
            book.settle()
            book.now = utc(day)

    def feed(self, algo, day, decisions=(), snapshot_decisions=(), close_decisions=(), replies=None):
        """One completed bar whose bar, Session-close and snapshot replies carry
        decisions; replies adds answers for other input types."""
        overrides = {
            "market.bar.completed": {"payload": {"decisions": list(decisions)}},
            "market.session.closed": {"payload": {"decisions": list(close_decisions)}},
            "account.snapshot": {"payload": {"decisions": list(snapshot_decisions)}}}
        overrides.update(replies or {})
        algo.client.reply_overrides = overrides
        self.at(algo, day)
        b = bar(day)
        # ratio: the bar's raw price over its split-adjusted one (Frame).
        ratio = getattr(self, "ratio", 1)
        algo.History = lambda *args, **kwargs: Frame(b.EndTime, ratio=ratio)
        algo.OnData(scaffold.slice_of({"AAPL": b}))

    def fill(self, algo, ticket, day, price, fee=1.0, quantity=None, status="filled"):
        """LEAN fills ticket during day's session, before that day's OnData.

        As observed on the pinned image: the holding moves, the slippage model
        is asked for its charge, and OnOrderEvent reports the fill at the
        session's close.
        """
        self.at(algo, day)
        quantity = ticket.Quantity if quantity is None else quantity
        ticket.QuantityFilled += quantity
        ticket.Status = status
        algo.Portfolio.holdings["AAPL"] = algo.Portfolio.holdings.get("AAPL", 0) + quantity
        if ticket.Tag in algo.desk.n_by_tag:
            # Only an order this adapter placed has an N to slip by.
            algo.security.slippage_model.GetSlippageApproximation(
                algo.security, types.SimpleNamespace(Tag=ticket.Tag, Id=ticket.OrderId))
        algo.Transactions.emit(ticket, status, fill_quantity=float(quantity),
                               fill_price=price, fee=fee)

    def tickets(self, algo):
        return algo.Transactions.tickets

    def sells(self, algo):
        return [t for t in algo.Transactions.tickets if t.Quantity < 0]

    def sent(self, algo, event_type):
        return [e for e in algo.client.sent if e["type"] == event_type]

    def types_sent(self, algo, since=0):
        return [e["type"] for e in algo.client.sent[since:]]

    def hold(self, algo, shares):
        """LEAN holds shares from an entry order this adapter placed and LEAN filled.

        The order is placed after the 6th's bar and LEAN fills it in the
        session ending on the 9th, before the 9th's bar reaches the adapter;
        the fill is reported to the engine at the start of the next feed.
        """
        self.feed(algo, 6, [trade_proposal(6, quantity=shares)])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 9, 24.56)

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
    def test_a_valid_proposal_becomes_a_gtc_stop_market_order_at_its_level(self):
        # Good-till-cancelled, never DAY: at daily resolution LEAN expires a
        # DAY order before it evaluates the next session's fill (observed on
        # the pinned image), so a DAY entry could never fill.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed)
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Symbol, ticket.Quantity, ticket.StopPrice, ticket.Tag),
                         ("AAPL", 100, 24.5, proposal["id"]))
        self.assertEqual(ticket.TimeInForce, "gtc")
        self.assertEqual(self.rejections(algo), [])

    def test_a_proposal_in_the_session_close_reply_becomes_an_order(self):
        # The engine decides entries and Adds when the Session closes (ADR
        # 0021), so its proposals arrive in the reply to market.session.closed,
        # not the bar's. They must be acted on exactly like the bar's.
        algo = self.start()
        entry, add = trade_proposal(9), add_proposal(9)
        self.feed(algo, 9, close_decisions=[entry, add])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(sorted(t.Tag for t in self.tickets(algo)), sorted([entry["id"], add["id"]]))
        self.assertTrue(all(t.TimeInForce == "gtc" for t in self.tickets(algo)))

    def test_a_valid_add_proposal_becomes_a_gtc_stop_market_order_at_its_rung(self):
        algo = self.start()
        proposal = add_proposal(9)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Quantity, ticket.StopPrice, ticket.Tag, ticket.TimeInForce),
                         (100, 25.1, proposal["id"], "gtc"))

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
        props.TimeInForce = "gtc"
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
        """An entry LEAN itself refuses at submission (status Invalid, as a
        cash account does for an unaffordable order: ADR 0010) is rejected
        and logged, not stopped: the run continues and the engine's later
        proposal for the same instrument still reaches LEAN's order book."""
        algo = self.start()
        algo.Transactions.submit_status = "invalid"
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [rejection] = self.rejections(algo)
        self.assertIn(proposal["id"], rejection)
        self.assertIn("LEAN refused", rejection)
        self.assertFalse(algo.failed)
        # Not stopped: the engine's re-issued proposal on a later bar still
        # reaches LEAN, once it stops refusing orders.
        algo.Transactions.submit_status = "submitted"
        later = trade_proposal(10)
        self.feed(algo, 10, [later])
        self.assertFalse(algo.failed)
        [ticket] = [t for t in self.tickets(algo) if t.Tag == later["id"]]
        self.assertEqual(ticket.Status, "submitted")

    def test_go_unreachable_means_nothing_is_submitted(self):
        algo = self.start()
        engine = algo.client
        answer = engine.decide

        def decide(input_envelope):
            if input_envelope["type"] == "market.session.closed":
                raise Unavailable("engine closed the connection mid-exchange")
            return answer(input_envelope)
        engine.decide = decide
        # The bar's reply carried a proposal, but the Session close that
        # follows it fails: the stream is out of step, so nothing from it is
        # acted on.
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

    def test_a_proposal_expiry_cancels_its_unfilled_order(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        # LEAN answers CancelPending; Canceled follows after the slice.
        self.assertEqual(algo.Transactions.cancellations, [(ticket.OrderId, None)])
        self.assertEqual(ticket.Status, "cancel-pending")
        self.feed(algo, 11)
        self.assertEqual(ticket.Status, "canceled")
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(len(self.tickets(algo)), 1)

    def test_a_cancellation_never_overwrites_the_orders_tag(self):
        # LEAN's Cancel(tag) replaces the order's tag (observed), and the tag
        # is the decision id every duplicate check reads.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertEqual([tag for _, tag in algo.Transactions.cancellations], [None])
        self.assertEqual(self.tickets(algo)[0].Tag, proposal["id"])

    def test_an_add_proposal_expiry_cancels_its_unfilled_order(self):
        algo = self.start()
        proposal = add_proposal(9)
        self.feed(algo, 9, [proposal])
        self.feed(algo, 10, [proposal_expired(proposal, 10, kind="add")])
        self.assertEqual(len(algo.Transactions.cancellations), 1)

    def test_a_refused_cancellation_stops_the_run(self):
        # An order the engine expired that LEAN will not cancel could still
        # fill into a holding the engine does not expect.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        algo.Transactions.cancel_outcome = "refused"
        self.feed(algo, 10, [proposal_expired(proposal, 10), add_proposal(10)])
        self.assertIn("did not confirm", algo.quit_reason)
        self.assertIn(proposal["id"], algo.quit_reason)
        self.assertFalse(any("adapter: cancelled order" in m for m in algo.logs))
        self.assertEqual(len(self.tickets(algo)), 1)
        self.assert_stopped_after(algo, len(algo.client.sent))

    def test_a_cancellation_unconfirmed_by_the_next_session_stops_the_run(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        algo.Transactions.cancel_outcome = "never"
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertFalse(algo.failed)
        sent = len(algo.client.sent)
        self.feed(algo, 11, [trade_proposal(11)])
        self.assertIn("did not confirm cancelling order(s) 1 (tag={})".format(proposal["id"]),
                      algo.quit_reason)
        # Stopped before the next bar reached the engine.
        self.assertNotIn("market.bar.completed", self.types_sent(algo, sent))
        self.assertEqual(len(self.tickets(algo)), 1)

    def test_a_confirmed_cancellation_is_logged(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.assertTrue(any("adapter: cancel requested for order 1" in m for m in algo.logs))
        self.feed(algo, 11)
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


class IgnoredDecisionTests(OrderTestCase):
    """Issue #31: the decision types the reducer emits that name no order of
    their own (README.md's decision table) are received without error and
    place, amend or cancel nothing, whatever their contents. OrderDesk.act's
    `actionable` filter is the whole mechanism: only a type in SCHEMA_VERSIONS
    is ever looked at, and everything else -- including a future type this
    build has never heard of -- passes through untouched (the same "no new
    orders, no crash" contract a truly unknown type would need)."""

    IGNORED = (campaign_evaluated, drawdown_step_applied, notional_account_cash_adjusted,
              notional_account_rebased, notional_account_recovered, proposal_declined,
              engine_state, setup_evaluated, signal, protective_stop_set)

    def test_every_ignored_decision_type_causes_no_order_and_no_failure(self):
        for builder in self.IGNORED:
            with self.subTest(builder.__name__):
                algo = self.start()
                self.feed(algo, 9, [builder(9)])
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual(self.tickets(algo), [])

    def test_an_ignored_decision_beside_an_actionable_one_still_places_the_order(self):
        # Ignored decisions are dropped by OrderDesk.act's own filter, not by
        # skipping the whole reply: an actionable decision delivered
        # alongside them is still acted on. setup_evaluated and signal are
        # reachable together: the reducer emits "Setup-evaluated, Signal,
        # sizing outcome" for one breakout bar (internal/strategy/reducer.go's
        # own doc comment). engine_state is deliberately NOT used here: it is
        # the reducer's own halt, emitted alone when a capital-safety
        # invariant fails (event.EngineStatePayload's doc comment), so a
        # decisions list ever pairing it with a trade proposal cannot occur
        # and using it here would test a reply the reducer never sends.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [signal(9), proposal, setup_evaluated(9)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [ticket] = self.tickets(algo)
        self.assertEqual(ticket.Tag, proposal["id"])


class ProtectiveOrderSurvivalTests(OrderTestCase):
    """Issue #31: 'Existing protective orders are preserved.' Whatever else a
    fault does, the adapter never cancels a Unit's already-working GTC Exit
    Order merely because something unrelated went wrong: docs/architecture.md
    and the ticket's own governing rule are 'no NEW orders', never 'no
    existing protection'. OrderDesk has no code path that cancels an Exit
    Order at all (_expire only ever cancels an entry or Add's buy order), so
    this fixes that absence in place with a test rather than leaving it
    merely implicit."""

    def test_a_working_exit_order_survives_an_unknown_schema_version(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, unit_index=1, level=22.1, quantity=100)])
        [stop] = self.sells(algo)
        self.assertEqual(stop.Status, "submitted")

        bad = trade_proposal(10)
        bad["schema_version"] = 2
        self.feed(algo, 10, [bad])
        self.assertTrue(algo.failed)

        self.assertEqual(stop.Status, "submitted")
        self.assertEqual(algo.Transactions.cancellations, [])
        self.assertEqual(algo.Transactions.updates, [])

    def test_a_working_exit_order_survives_go_unreachable(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, unit_index=1, level=22.1, quantity=100)])
        [stop] = self.sells(algo)

        engine = algo.client
        answer = engine.decide

        def decide(input_envelope):
            if input_envelope["type"] == "market.bar.completed":
                raise Unavailable("engine closed the connection mid-exchange")
            return answer(input_envelope)
        engine.decide = decide

        self.feed(algo, 10)
        self.assertTrue(algo.failed)

        self.assertEqual(stop.Status, "submitted")
        self.assertEqual(algo.Transactions.cancellations, [])
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


class FillReturnTests(OrderTestCase):
    """LEAN's fills reach the engine as execution.fill, shaped for each order's kind.

    Every payload mirrors internal/fills' own FillPayload for the same kind,
    and the adapter acts on the engine's reply with the same OrderDesk.
    """

    def entered(self, algo, shares=100, reply=()):
        """An entry placed after the 9th's bar and filled by LEAN on the 10th."""
        proposal = trade_proposal(9, quantity=shares)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 10, 24.56, fee=1.0)
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": list(reply)}}})
        return proposal, entry

    def test_an_entry_fill_is_the_trade_proposals_fill(self):
        algo = self.start()
        proposal, entry = self.entered(algo)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [fill] = self.sent(algo, "execution.fill")
        self.assertEqual(fill["schema_version"], 4)
        self.assertEqual(fill["event_time"], period_end(10))
        self.assertEqual(fill["payload"], {
            "instrument_id": "AAPL", "kind": "entry", "proposal_id": proposal["id"],
            "campaign_id": "", "fill_id": "lean:1:2", "unit_ids": [], "direction": "long",
            "quantity": 100, "price": 24.56, "filled_at": period_end(10), "level": 24.5,
            # What LEAN's slippage model charged: 0.05 x the proposal's n.
            "slippage_applied": 0.05 * 1.2, "commission": 1.0})

    def test_the_exit_order_the_engine_sets_on_an_entry_fill_is_placed_before_the_bar(self):
        algo = self.start()
        opened, placed = campaign_opened(), exit_order_set(10, level=22.1)
        self.entered(algo, reply=[opened, placed])
        [sell] = self.sells(algo)
        self.assertEqual((sell.Quantity, sell.StopPrice, sell.Tag, sell.TimeInForce),
                         (-100, 22.1, placed["id"], "gtc"))
        # The Exit Order is placed from the fill's reply, and LEAN's report of
        # its submission reaches the engine before the session's bar does.
        types_ = self.types_sent(algo)
        fill_at = types_.index("execution.fill")
        self.assertEqual(types_[fill_at:fill_at + 4],
                         ["execution.fill", "execution.order.lifecycle", "account.snapshot",
                          "market.bar.completed"])
        submitted = self.sent(algo, "execution.order.lifecycle")[-1]["payload"]
        self.assertEqual((submitted["status"], submitted["tag"], submitted["quantity"]),
                         ("submitted", placed["id"], -100))

    def test_an_add_fill_is_the_add_proposals_fill(self):
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10)])
        proposal = add_proposal(11)
        self.feed(algo, 11, close_decisions=[proposal])
        [_, add] = [t for t in self.tickets(algo) if t.Quantity > 0]
        self.fill(algo, add, 12, 25.2, fee=1.0)
        self.feed(algo, 12)
        fill = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((fill["kind"], fill["proposal_id"], fill["campaign_id"], fill["unit_ids"],
                          fill["quantity"], fill["level"], fill["fill_id"]),
                         ("add", proposal["id"], decision_id("campaign", 6), [], 100, 25.1,
                          "lean:{}:2".format(add.OrderId)))
        self.assertAlmostEqual(fill["slippage_applied"], 0.05 * 1.2)

    def test_a_stop_fill_names_the_unit_its_exit_order_protects(self):
        algo = self.start()
        opened = campaign_opened()
        self.entered(algo, reply=[opened, exit_order_set(10, level=22.1)])
        [sell] = self.sells(algo)
        self.fill(algo, sell, 11, 22.0, fee=1.0)
        self.feed(algo, 11, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(11, fill_id="lean:2:2"), campaign_exited(11)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        fill = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((fill["kind"], fill["proposal_id"], fill["campaign_id"], fill["unit_ids"],
                          fill["quantity"], fill["price"], fill["level"]),
                         ("stop", "", opened["payload"]["campaign_id"],
                          [opened["payload"]["fill_id"]], 100, 22.0, 22.1))
        self.assertAlmostEqual(fill["slippage_applied"], 0.05 * 1.2)

    def two_units_at_the_exit_channel(self, algo, unit2_source="exit-channel", unit2_level=23.4):
        """A Campaign of two Units, then an exit proposed at 23.4 on the 12th's bar."""
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
        self.feed(algo, 11, close_decisions=[add_proposal(11)])
        add = self.tickets(algo)[-1]
        self.fill(algo, add, 12, 25.2)
        self.feed(algo, 12, replies={"execution.fill": {"payload": {"decisions": [
            unit_added(12, fill_id="lean:{}:2".format(add.OrderId)),
            exit_order_set(12, unit_index=2, level=22.8, cause="add"),
            exit_order_set(12, unit_index=1, level=22.7, cause="add")]}}})
        proposal = exit_proposed(13)
        self.feed(algo, 13, [proposal,
                             exit_order_set(13, unit_index=1, level=23.4, source="exit-channel",
                                            exit_channel_level=23.4),
                             exit_order_set(13, unit_index=2, level=unit2_level, source=unit2_source,
                                            exit_channel_level=23.4)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        return proposal, self.sells(algo)

    def test_the_units_at_the_exit_channel_fill_as_one_exit(self):
        algo = self.start()
        proposal, [first, second] = self.two_units_at_the_exit_channel(algo)
        self.fill(algo, first, 14, 23.3, fee=1.0)
        self.fill(algo, second, 14, 23.3, fee=1.25)
        self.feed(algo, 14)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        fill = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((fill["kind"], fill["proposal_id"], fill["campaign_id"], fill["unit_ids"],
                          fill["quantity"], fill["price"], fill["level"], fill["commission"],
                          fill["fill_id"]),
                         ("exit", proposal["id"], decision_id("campaign", 6), [], 200, 23.3, 23.4,
                          2.25, "lean:{}:{}+{}:{}".format(first.OrderId, first.event_ids,
                                                          second.OrderId, second.event_ids)))
        # One exit fill, never one per Unit: the reducer accepts an exit fill
        # only for the whole remaining holding.
        self.assertEqual(len(self.sent(algo, "execution.fill")), 3)

    def test_a_unit_at_its_own_stop_fills_before_the_exit(self):
        algo = self.start()
        proposal, [first, second] = self.two_units_at_the_exit_channel(
            algo, unit2_source="protective-stop", unit2_level=23.6)
        # LEAN reports the exit-channel Unit first; the stop is still sent first.
        self.fill(algo, first, 14, 23.3)
        self.fill(algo, second, 14, 23.5)
        self.feed(algo, 14)
        stop, exit_ = [e["payload"] for e in self.sent(algo, "execution.fill")[-2:]]
        self.assertEqual((stop["kind"], stop["unit_ids"], stop["quantity"]),
                         ("stop", ["lean:{}:2".format(self.tickets(algo)[2].OrderId)], 100))
        self.assertEqual((exit_["kind"], exit_["proposal_id"], exit_["quantity"]),
                         ("exit", proposal["id"], 100))

    def test_stop_fills_in_one_slice_are_sent_worst_price_first(self):
        # ADR 0005 rule 3, as internal/fills orders them: whatever order LEAN
        # reported them in.
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
        self.feed(algo, 11, close_decisions=[add_proposal(11)])
        add = self.tickets(algo)[-1]
        self.fill(algo, add, 12, 25.2)
        self.feed(algo, 12, replies={"execution.fill": {"payload": {"decisions": [
            unit_added(12, fill_id="lean:{}:2".format(add.OrderId)),
            exit_order_set(12, unit_index=2, level=22.8, cause="add"),
            exit_order_set(12, unit_index=1, level=22.7, cause="add")]}}})
        unit1, unit2 = self.sells(algo)
        self.fill(algo, unit2, 13, 22.75)
        self.fill(algo, unit1, 13, 22.6)
        self.feed(algo, 13)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stops = [e["payload"] for e in self.sent(algo, "execution.fill")[-2:]]
        self.assertEqual([(s["kind"], s["price"], s["unit_ids"]) for s in stops],
                         [("stop", 22.6, ["lean:1:2"]),
                          ("stop", 22.75, ["lean:{}:2".format(add.OrderId)])])

    def test_only_some_units_at_the_exit_channel_filling_stops_the_run(self):
        algo = self.start()
        _, [first, _] = self.two_units_at_the_exit_channel(algo)
        sent = len(algo.client.sent)
        self.fill(algo, first, 14, 23.3)
        self.feed(algo, 14)
        self.assertIn("one exit fill cannot state that", algo.quit_reason)
        self.assertNotIn("execution.fill", self.types_sent(algo, sent))

    def test_an_add_and_a_stop_in_one_slice_enter_then_stop(self):
        # ADR 0005 rule 3: a bar that fills an Add and a stop entered first.
        # The Add's reply raises Unit 1's stop, but LEAN has already filled
        # Unit 1's order, so the amendment is skipped and its fill is sent.
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
        self.feed(algo, 11, close_decisions=[add_proposal(11)])
        entry, unit1, add = self.tickets(algo)
        self.fill(algo, unit1, 12, 22.0)
        self.fill(algo, add, 12, 25.2)
        self.feed(algo, 12, replies={"execution.fill": lambda e: {"payload": {"decisions": [
            unit_added(12, fill_id="lean:{}:2".format(add.OrderId)),
            exit_order_set(12, unit_index=2, level=22.8, cause="add"),
            exit_order_set(12, unit_index=1, level=22.7, cause="add")]
            if e["payload"]["kind"] == "add" else []}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        kinds = [e["payload"]["kind"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual(kinds, ["entry", "add", "stop"])
        self.assertEqual(algo.Transactions.updates, [])
        self.assertTrue(any("has already filled and that fill is reported next" in m
                            for m in algo.logs))

    def test_a_chained_add_proposal_in_a_fill_reply_is_stale(self):
        # The engine measures the next rung from the fill against the bar
        # that signalled the entry; that session has already traded in LEAN.
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10), add_proposal(9)])
        self.assertEqual([t.Quantity for t in self.tickets(algo)], [100, -100])
        [rejection] = self.rejections(algo)
        self.assertIn("stale", rejection)

    def test_a_partial_fill_stops_the_run(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        sent = len(algo.client.sent)
        self.fill(algo, entry, 10, 24.56, quantity=40, status="partially-filled")
        self.feed(algo, 10)
        for fact in ("partially filled", "40", "#67", proposal["id"]):
            self.assertIn(fact, algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [])
        self.assert_stopped_after(algo, sent)

    def test_a_fill_of_an_order_the_adapter_did_not_place_stops_the_run(self):
        algo = self.start()
        props = scaffold.OrderProperties()
        props.TimeInForce = "gtc"
        foreign = scaffold.FakeTicket(algo.Transactions, 99, "AAPL", 10, 20.0, "manual", props)
        algo.Transactions.tickets.append(foreign)
        self.fill(algo, foreign, 10, 20.1)
        self.feed(algo, 10)
        self.assertIn("did not place", algo.quit_reason)
        self.assertEqual(self.sent(algo, "execution.fill"), [])

    def test_a_fill_after_the_stream_completed_is_not_sent(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        algo.OnEndOfAlgorithm()
        sent = len(algo.client.sent)
        self.fill(algo, self.tickets(algo)[0], 10, 24.56)
        self.assertEqual(algo.order_events, [])
        self.assertEqual(len(algo.client.sent), sent)


class LifecycleTests(OrderTestCase):
    """Every LEAN order change that is not an execution reaches the engine as
    execution.order.lifecycle, in the order LEAN reported it."""

    def lifecycles(self, algo):
        return [e["payload"] for e in self.sent(algo, "execution.order.lifecycle")]

    def test_each_transition_maps_to_its_status(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        self.feed(algo, 11)
        self.assertEqual([(p["status"], p["tag"]) for p in self.lifecycles(algo)],
                         [("submitted", proposal["id"]), ("cancel-pending", proposal["id"]),
                          ("canceled", proposal["id"])])
        self.assertEqual(self.lifecycles(algo)[0], {
            "instrument_id": "AAPL", "order_id": "1", "tag": proposal["id"], "status": "submitted",
            "quantity": 100, "stop_price": 24.5, "occurred_at": period_end(9), "message": ""})
        self.assertEqual(self.lifecycles(algo)[2]["occurred_at"], period_end(10))

    def test_an_amendment_is_reported_as_updated(self):
        algo = self.start()
        self.hold(algo, 100)
        self.feed(algo, 9, [campaign_opened(), exit_order_set(9, level=22.1)])
        raised = exit_order_set(10, level=23.4)
        self.feed(algo, 10, [raised])
        self.feed(algo, 11)
        updated = self.lifecycles(algo)[-1]
        self.assertEqual((updated["status"], updated["tag"], updated["stop_price"],
                          updated["quantity"]), ("updated", raised["id"], 23.4, -100))

    def test_an_order_lean_refuses_is_reported_as_invalid(self):
        algo = self.start()
        algo.Transactions.submit_status = "invalid"
        self.feed(algo, 9, [trade_proposal(9)])
        self.assertEqual([p["status"] for p in self.lifecycles(algo)], ["invalid"])

    def test_a_status_with_no_lifecycle_meaning_stops_the_run(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        [entry] = self.tickets(algo)
        self.at(algo, 10)
        algo.Transactions.emit(entry, "new")
        self.feed(algo, 10)
        self.assertIn("cannot state as an order lifecycle change", algo.quit_reason)


class OrderingTests(OrderTestCase):
    """Where each report falls against a slice's bar, Session close and snapshot."""

    def test_a_slice_is_reports_then_fills_then_bar_close_snapshot_then_new_orders(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.assertEqual(self.types_sent(algo), [
            "market.bar.completed", "market.session.closed",
            # The order placed from those decisions. The Session's snapshot
            # waits for the next slice (algorithm.py, flush_snapshot).
            "execution.order.lifecycle"])
        [entry] = self.tickets(algo)
        sent = len(algo.client.sent)
        self.fill(algo, entry, 10, 24.56)
        self.feed(algo, 10, [add_proposal(10)])
        # The session's fill precedes its bar: the proposal it executes is
        # still outstanding until that bar expires it (ADR 0011). The previous
        # Session's snapshot follows the fill, so an Add the engine chains
        # from the fill is still sized against the cash its own bar was
        # entitled to (flush_snapshot), and precedes the bar it is the basis
        # for.
        self.assertEqual(self.types_sent(algo, sent), [
            "execution.fill", "account.snapshot", "market.bar.completed",
            "market.session.closed", "execution.order.lifecycle"])
        fill, snapshot, bar_ = algo.client.sent[sent:sent + 3]
        self.assertEqual(fill["event_time"], bar_["event_time"])
        self.assertEqual(snapshot["payload"]["as_of"], period_end(9))
        self.assertEqual([e["sequence"] for e in algo.client.sent], list(range(2, len(algo.client.sent) + 2)))

    def test_a_cancellation_confirmed_after_the_slice_is_sent_before_the_next_fills(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal, add_proposal(9)])
        entry, add = self.tickets(algo)
        self.feed(algo, 10, [proposal_expired(proposal, 10)])
        sent = len(algo.client.sent)
        self.fill(algo, add, 11, 25.2)
        self.feed(algo, 11)
        self.assertEqual(self.types_sent(algo, sent)[:2],
                         ["execution.order.lifecycle", "execution.fill"])
        self.assertEqual(algo.client.sent[sent]["payload"]["status"], "canceled")

    def test_fills_are_sent_before_the_end_of_the_stream(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        self.fill(algo, self.tickets(algo)[0], 10, 24.56)
        algo.OnEndOfAlgorithm()
        self.assertEqual(self.types_sent(algo)[-3:],
                         ["execution.fill", "account.snapshot", "replay.run.completed"])


class InputContractTests(OrderTestCase):
    """The fill and lifecycle inputs the adapter sends pass Go's own validators."""

    def test_every_input_the_adapter_builds_is_valid_in_go(self):
        algo = self.start()
        opened = campaign_opened()
        proposal, entry = FillReturnTests.entered(self, algo, reply=[opened, exit_order_set(10)])
        [sell] = self.sells(algo)
        self.fill(algo, sell, 11, 22.0)
        self.feed(algo, 11)
        inputs = [e for e in algo.client.sent
                  if e["type"] in ("execution.fill", "execution.order.lifecycle")]
        self.assertEqual({e["type"] for e in inputs},
                         {"execution.fill", "execution.order.lifecycle"})
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/execution_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(inputs, separators=(",", ":")), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


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
                      "InteractiveBrokersFeeModel", "$0.005", "0.5%", "amended", "partial",
                      "one bar later", "CancelPending", "price views", "raw shares",
                      "split-adjusted view",
                      "rounded down", "to the cent", "split"):
            self.assertIn(topic, text)
        # Every statement about LEAN's own behaviour was settled by a run on
        # the pinned image; none is left as belief.
        self.assertNotIn("unconfirmed", text)
        self.assertNotIn("believed", text)
        # Logged at startup, before any bar reaches the engine.
        self.assertEqual(algo.client.sent, [])


def ib_fee(quantity):
    """LEAN's InteractiveBrokersFeeModel as observed on the pinned image, less
    its cap: $0.005 per share, $1.00 minimum per order."""
    return max(1.0, 0.005 * abs(quantity))


class RawAccountingTests(OrderTestCase):
    """ADR 0004: LEAN trades, holds and charges in raw shares and prices, while
    the engine's levels, quantities and fills stay in the split-adjusted view.

    AAPL in June 2014 before its 7-for-1: every raw price is 28 times its
    split-adjusted one (ratio), and a split-adjusted share is 1/28 of a raw one.
    """
    ratio = 28

    def entry(self, day=9, **changes):
        # 0.875 split-adjusted is 24.5 raw; 2,800 split-adjusted shares are
        # 100 raw ones; N of 0.05 split-adjusted is 1.4 raw.
        fields = dict(entry_level=0.875, quantity=2800, n=0.05)
        fields.update(changes)
        return trade_proposal(day, **fields)

    def entered(self, reply=(), fee=None, **changes):
        """An entry placed after the 9th's bar and filled by LEAN on the 10th."""
        algo = self.start()
        proposal = self.entry(**changes)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [ticket] = self.tickets(algo)
        self.fill(algo, ticket, 10, 24.57, fee=ib_fee(ticket.Quantity) if fee is None else fee)
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": list(reply)}}})
        return algo, proposal, ticket

    def test_an_entry_is_ordered_in_raw_shares_at_the_raw_level(self):
        algo = self.start()
        proposal = self.entry()
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Quantity, ticket.StopPrice, ticket.Tag), (100, 24.5, proposal["id"]))

    def test_an_add_is_ordered_in_raw_shares_at_the_raw_rung(self):
        algo = self.start()
        proposal = add_proposal(9, level=0.9, quantity=2800, campaign_n=0.05)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.assertEqual(ticket.Quantity, 100)
        self.assertAlmostEqual(ticket.StopPrice, 25.2)

    def test_a_unit_is_rounded_down_to_whole_raw_shares(self):
        # A raw share is 28 split-adjusted ones, so 2,830 is 101 whole raw
        # shares and a remainder. Rounding down never risks more than the
        # Unit the engine sized (ADR 0003); the fill reports what executed.
        algo = self.start()
        self.feed(algo, 9, [self.entry(quantity=2830)])
        [ticket] = self.tickets(algo)
        self.assertEqual(ticket.Quantity, 101)

    def test_a_unit_smaller_than_one_raw_share_is_rejected(self):
        algo = self.start()
        proposal = self.entry(quantity=27)
        self.feed(algo, 9, [proposal])
        self.assertEqual(self.tickets(algo), [])
        [rejection] = self.rejections(algo)
        self.assertIn(proposal["id"], rejection)
        self.assertIn("less than one raw share", rejection)
        self.assertFalse(algo.failed)

    def test_slippage_is_charged_in_raw_prices(self):
        # ADR 0013: slippage_n x N, where N is the engine's split-adjusted
        # figure; LEAN prices the fill in raw, so the charge is 28 times it.
        algo = self.start()
        proposal = self.entry()
        self.feed(algo, 9, [proposal])
        slip = algo.security.slippage_model.GetSlippageApproximation(
            algo.security, types.SimpleNamespace(Tag=proposal["id"]))
        self.assertAlmostEqual(slip, 0.05 * 0.05 * 28)

    def test_a_fill_is_returned_in_the_split_adjusted_view(self):
        # ADR 0004's amendment: a Campaign's money is computed in the one
        # view its fills are priced in, split-adjusted until a split can
        # adjust a held position, so the engine hears the raw execution
        # restated in that view: quantity x price is unchanged by it.
        algo, proposal, ticket = self.entered()
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [fill] = [e["payload"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual(fill["quantity"], 2800)
        self.assertAlmostEqual(fill["price"], 24.57 / 28)
        self.assertAlmostEqual(fill["level"], 0.875)
        self.assertAlmostEqual(fill["slippage_applied"], 0.05 * 0.05)
        self.assertAlmostEqual(fill["quantity"] * fill["price"], 100 * 24.57)
        self.assertTrue(any("100 raw shares @ 24.57" in m for m in algo.logs), algo.logs)

    def test_a_partial_unit_fill_reports_the_whole_raw_shares_that_executed(self):
        algo, _, ticket = self.entered(quantity=2830)
        [fill] = [e["payload"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual((ticket.Quantity, fill["quantity"]), (101, 101 * 28))

    def test_commission_is_leans_charge_on_raw_shares(self):
        # 28,000 split-adjusted shares are 1,000 raw ones: $5.00 at $0.005 a
        # share, where the same Unit in split-adjusted shares would be
        # charged $140.00. The commission is cash, the same in either view,
        # so it is returned exactly as LEAN charged it.
        algo, _, ticket = self.entered(quantity=28000)
        self.assertEqual(ticket.Quantity, 1000)
        [fill] = [e["payload"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual(fill["commission"], 5.0)

    def test_an_exit_order_rests_in_raw_shares_at_the_raw_level(self):
        opened = campaign_opened(campaign_n=0.05)
        placed = exit_order_set(10, level=0.7875, quantity=2800)
        algo, _, _ = self.entered(reply=[opened, placed])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [sell] = self.sells(algo)
        self.assertEqual((sell.Quantity, sell.Tag), (-100, placed["id"]))
        self.assertAlmostEqual(sell.StopPrice, 22.05)
        raised = exit_order_set(11, level=0.8, quantity=2800)
        self.feed(algo, 11, [raised])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(sell.Quantity, -100)
        self.assertAlmostEqual(sell.StopPrice, 22.4)

    def test_an_exit_order_that_is_not_whole_raw_shares_stops_the_run(self):
        # A Unit's quantity is what its fill reported, always whole raw
        # shares; one that is not cannot be protected by a whole-share order.
        opened = campaign_opened(campaign_n=0.05)
        bad = exit_order_set(10, level=0.7875, quantity=2810)
        algo, _, _ = self.entered(reply=[opened, bad])
        self.assertEqual(self.sells(algo), [])
        for fact in (bad["id"], "2810", "cannot be protected"):
            self.assertIn(fact, algo.quit_reason)

    def test_a_stop_fill_is_returned_in_the_split_adjusted_view(self):
        opened = campaign_opened(campaign_n=0.05)
        algo, _, _ = self.entered(reply=[opened, exit_order_set(10, level=0.7875, quantity=2800)])
        [sell] = self.sells(algo)
        self.fill(algo, sell, 11, 21.98, fee=ib_fee(sell.Quantity))
        self.feed(algo, 11, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(11, fill_id="lean:2:2"), campaign_exited(11)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stop = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((stop["kind"], stop["quantity"], stop["commission"]), ("stop", 2800, 1.0))
        self.assertAlmostEqual(stop["price"], 21.98 / 28)
        self.assertAlmostEqual(stop["level"], 0.7875)
        self.assertAlmostEqual(stop["slippage_applied"], 0.05 * 0.05)

    def test_order_changes_are_reported_as_lean_states_them_in_raw(self):
        # event.OrderLifecyclePayload carries the order's figures as the venue
        # stated them, which reconciliation compares with the broker's.
        algo = self.start()
        self.feed(algo, 9, [self.entry()])
        [submitted] = [e["payload"] for e in self.sent(algo, "execution.order.lifecycle")]
        self.assertEqual((submitted["quantity"], submitted["stop_price"]), (100, 24.5))

    def test_every_input_is_valid_in_go(self):
        opened = campaign_opened(campaign_n=0.05)
        algo, _, _ = self.entered(reply=[opened, exit_order_set(10, level=0.7875, quantity=2800)])
        [sell] = self.sells(algo)
        self.fill(algo, sell, 11, 21.98)
        self.feed(algo, 11)
        inputs = [e for e in algo.client.sent
                  if e["type"] in ("execution.fill", "execution.order.lifecycle")]
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/execution_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(inputs, separators=(",", ":")), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_a_bar_whose_views_are_not_a_whole_split_ratio_apart_stops_the_run(self):
        # A raw price is the split-adjusted one times the product of every
        # later split. A whole share of one view must be a whole number of
        # shares of the other, or no fill could be stated in both.
        for ratio in (1.5, 0.5, 27.9):
            with self.subTest(ratio=ratio):
                self.ratio = ratio
                algo = self.start()
                self.feed(algo, 9, [self.entry()])
                self.assertTrue(algo.failed)
                self.assertIn("split ratio", algo.quit_reason)
                self.assertEqual(self.sent(algo, "market.bar.completed"), [])
                self.assertEqual(self.tickets(algo), [])
        del self.ratio

    def test_leans_rounded_factor_file_ratio_is_a_whole_split_ratio(self):
        # LEAN's factor file states AAPL's 1/56 as 0.0178571, so its raw and
        # split-adjusted closes are 56.000134 apart (observed on the pinned
        # image, 2005-02-22: 85.38 and 1.524639198).
        self.ratio = 85.38 / 1.524639198
        algo = self.start()
        self.feed(algo, 9, [self.entry(quantity=5600)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Quantity, ticket.StopPrice), (100, 0.875 * 56))
        del self.ratio


# LEAN's factor for AAPL's 2-for-1 of 2005-02-28, from its rounded factor
# file (observed on the pinned image).
HALF = 0.49999860000056007


class SplitTests(OrderTestCase):
    """A split while LEAN holds a position or works an order (ADR 0004).

    The engine's split-adjusted view is adjusted for every split, later ones
    included, so a split changes none of its figures: only how many raw
    shares a split-adjusted one is. LEAN, under Raw normalisation, divides
    the holding and every open order's quantity by the split factor and
    multiplies each stop price by it (observed on the pinned image), so the
    adapter takes the new ratio from the split and then requires LEAN's
    position and orders to be exactly the engine's at it.

    AAPL before its 2-for-1 of 2005-02-28: 56 split-adjusted shares a raw
    share, then 28.
    """
    ratio = 56

    def split(self, algo, day, factor=HALF, orders_split=True, before_data=None, meddle=None,
              checked=True):
        """The split's time step before day's session, as observed on the pinned
        image. LEAN splits the holding; raises OnData with the split and no bar
        (the tickets not yet adjusted); splits each open order and reports it
        through OnOrderEvent; then the adapter's 00:01 scheduled check runs,
        before the session's fills. before_data and meddle change LEAN's state
        before OnData and after the orders' adjustment; checked=False leaves
        the scheduled check out, as though it had not run."""
        self.at(algo, day)
        book = algo.Transactions
        book.split_holding("AAPL", factor)
        if before_data is not None:
            before_data()
        algo.OnData(scaffold.slice_of(splits={"AAPL": types.SimpleNamespace(
            Type="split-occurred", SplitFactor=factor, Time=datetime(2014, 6, day))}))
        if orders_split:
            book.split_orders("AAPL", factor)
        if meddle is not None:
            meddle()
        if checked:
            self.scheduled_check(algo)

    def scheduled_check(self, algo):
        """LEAN fires the adapter's 00:01 event: after a split's time step, and
        before the session's fills (observed on the pinned image)."""
        [callback] = [c for _, rule, c in algo.scheduled if rule == ("at", 0, 1)]
        callback()

    def held(self, ratio=56, level=0.8, quantity=5600, entry_level=0.875):
        """One Unit of 5,600 split-adjusted shares (100 raw at 56), entered at
        0.875 (49.00 raw), with its Exit Order at 0.8 (44.80 raw)."""
        self.ratio = ratio
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=entry_level, quantity=quantity, n=0.05)])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 10, entry.StopPrice + 0.1)
        placed = exit_order_set(10, level=level, quantity=quantity)
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": [
            campaign_opened(campaign_n=0.05), placed]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [sell] = self.sells(algo)
        self.assertEqual(sell.Quantity, -(quantity // ratio))
        self.assertAlmostEqual(sell.StopPrice, level * ratio)
        return algo, sell

    def assert_stopped_before_the_session(self, algo, *facts):
        """The run stopped inside the split's time step, before LEAN could fill
        anything in the next session and before its bar reached the engine."""
        self.assertTrue(algo.failed)
        for fact in facts:
            self.assertIn(fact, algo.quit_reason)
        self.assertNotIn(period_end(11), [e["event_time"] for e in algo.client.sent])

    def test_the_split_check_is_scheduled_before_every_session(self):
        algo = self.start()
        self.assertIn((("every-day", "AAPL"), ("at", 0, 1), algo.verify_split), algo.scheduled)

    def test_a_held_unit_and_its_exit_order_carry_across_a_split(self):
        algo, sell = self.held()
        self.split(algo, 11)
        self.ratio = 28
        self.feed(algo, 11)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(algo.Portfolio.holdings["AAPL"], 200)
        self.assertEqual((sell.Quantity, sell.StopPrice), (-200, 22.4))
        # LEAN's own change to the order is reported as it states it.
        updated = self.sent(algo, "execution.order.lifecycle")[-1]["payload"]
        self.assertEqual((updated["status"], updated["quantity"], updated["stop_price"]),
                         ("updated", -200, 22.4))
        # The engine's next level for the Unit is placed at the new ratio.
        self.feed(algo, 12, [exit_order_set(12, level=0.81, quantity=5600)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(sell.Quantity, -200)
        self.assertAlmostEqual(sell.StopPrice, 0.81 * 28)
        # And its fill is returned in the engine's view: the same Unit.
        self.fill(algo, sell, 13, 22.5)
        self.feed(algo, 13, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(13, fill_id="lean:1:2"), campaign_exited(13)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stop = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((stop["kind"], stop["quantity"]), ("stop", 5600))
        self.assertAlmostEqual(stop["price"], 22.5 / 28)
        self.assertAlmostEqual(stop["level"], 0.81)
        self.assertAlmostEqual(stop["slippage_applied"], 0.05 * 0.05)
        self.assertAlmostEqual(algo.security.slippage_model.GetSlippageApproximation(
            algo.security, types.SimpleNamespace(Tag=sell.Tag)), 0.05 * 0.05 * 28)

    def test_an_entry_order_working_across_a_split_fills_as_the_same_unit(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.875, quantity=5600, n=0.05)])
        [entry] = self.tickets(algo)
        self.split(algo, 10)
        self.assertEqual((entry.Quantity, entry.StopPrice), (200, 24.5))
        self.fill(algo, entry, 10, 24.6)
        self.ratio = 28
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": [
            campaign_opened(campaign_n=0.05), exit_order_set(10, level=0.8, quantity=5600)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [fill] = [e["payload"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual(fill["quantity"], 5600)
        self.assertAlmostEqual(fill["price"], 24.6 / 28)
        self.assertAlmostEqual(fill["level"], 0.875)
        [sell] = self.sells(algo)
        self.assertEqual(sell.Quantity, -200)
        self.assertAlmostEqual(sell.StopPrice, 0.8 * 28)

    def test_a_split_lean_did_not_apply_to_a_working_order_stops_before_the_session(self):
        algo, sell = self.held()
        self.split(algo, 11, orders_split=False)
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(sell.OrderId), "-100", "-200")

    def test_a_split_lean_applied_to_the_holding_differently_stops_before_the_session(self):
        algo, _ = self.held()
        self.split(algo, 11, meddle=lambda: algo.Portfolio.holdings.update(AAPL=201))
        self.assert_stopped_before_the_session(algo, "split", "holds 201", "200")

    def test_a_split_holding_already_wrong_in_the_splits_slice_stops_there(self):
        # LEAN splits the holding before the slice reaches OnData, so a wrong
        # one is caught in the split's own slice.
        algo, sell = self.held()
        self.split(algo, 11, before_data=lambda: algo.Portfolio.holdings.update(AAPL=201),
                   checked=False)
        self.assert_stopped_before_the_session(algo, "split", "holds 201", "200")

    def test_a_split_that_moved_a_stop_off_the_engines_level_stops_before_the_session(self):
        # LEAN rounds the split stop to the tick; anything further from the
        # engine's level at the new ratio is not the Unit's Exit Order.
        algo, sell = self.held()
        self.split(algo, 11, meddle=lambda: setattr(sell, "StopPrice", 22.0))
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(sell.OrderId), "22.0", "22.4")

    def test_an_exit_order_lean_dropped_in_the_split_stops_before_the_session(self):
        # The holding still matches the engine's Units, so only checking each
        # stored Exit Order finds the Unit left without a stop.
        for status in ("canceled", "invalid"):
            with self.subTest(status=status):
                algo, sell = self.held()
                self.split(algo, 11, meddle=lambda: setattr(sell, "Status", status))
                self.assert_stopped_before_the_session(
                    algo, "split", "order {}".format(sell.OrderId), "not working", status)

    def test_an_exit_order_gone_before_the_splits_slice_stops_there(self):
        algo, sell = self.held()
        self.split(algo, 11, before_data=lambda: setattr(sell, "Status", "canceled"),
                   checked=False)
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(sell.OrderId), "not working")

    def test_the_next_slice_checks_the_split_again(self):
        # The second line: had the scheduled check not run, the next slice
        # still refuses LEAN's unadjusted order before its bar is sent.
        algo, sell = self.held()
        self.split(algo, 11, orders_split=False, checked=False)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        sent = len(algo.client.sent)
        self.ratio = 28
        self.feed(algo, 11)
        for fact in ("split", "order {}".format(sell.OrderId), "-100", "-200"):
            self.assertIn(fact, algo.quit_reason)
        self.assertNotIn("market.bar.completed", self.types_sent(algo, sent))

    def test_the_next_slice_checks_the_split_even_after_the_scheduled_check_passed(self):
        algo, sell = self.held()
        self.split(algo, 11)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        sell.StopPrice = 22.0
        self.ratio = 28
        self.feed(algo, 11)
        for fact in ("split", "order {}".format(sell.OrderId), "22.0", "22.4"):
            self.assertIn(fact, algo.quit_reason)

    def test_a_split_to_a_ratio_that_is_not_whole_stops_the_run(self):
        # A 3-for-2 of a position held at 56 split-adjusted shares a raw share
        # would leave 37.33: no whole raw share is a whole number of them.
        algo, _ = self.held()
        self.split(algo, 11, factor=2 / 3, checked=False)
        self.assert_stopped_before_the_session(algo, "3-for-2")

    def test_a_3_for_2_split_with_a_held_unit_stops_the_run_though_it_divides(self):
        # From 3 split-adjusted shares a raw share to 2: whole ratios either
        # side, and 100 raw shares become exactly 150, but a split that is not
        # n-for-1 is not supported (README.md: Price views and raw
        # accounting), so the run stops rather than carry the Unit across.
        algo, _ = self.held(ratio=3, level=7.0, quantity=300, entry_level=8.0)
        self.split(algo, 11, factor=2 / 3, checked=False)
        self.assert_stopped_before_the_session(algo, "3-for-2", "n-for-1")

    def test_a_2_for_1_split_that_does_not_divide_the_ratio_stops_the_run(self):
        # At 3 split-adjusted shares a raw share, a 2-for-1 would leave 1.5.
        algo, _ = self.held(ratio=3, level=7.0, quantity=300, entry_level=8.0)
        self.split(algo, 11, checked=False)
        self.assert_stopped_before_the_session(algo, "2-for-1", "n-for-1", "divides")

    def test_a_reverse_split_stops_the_run(self):
        algo, _ = self.held()
        self.split(algo, 11, factor=2.0, checked=False)
        self.assert_stopped_before_the_session(algo, "n-for-1")

    def test_a_split_while_flat_only_changes_the_ratio(self):
        algo = self.start()
        self.feed(algo, 9)
        self.split(algo, 10)
        self.ratio = 28
        self.feed(algo, 10, [trade_proposal(10, entry_level=0.875, quantity=5600, n=0.05)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [entry] = self.tickets(algo)
        self.assertEqual((entry.Quantity, entry.StopPrice), (200, 24.5))

    def test_a_changed_ratio_with_no_split_while_holding_stops_the_run(self):
        algo, _ = self.held()
        sent = len(algo.client.sent)
        self.ratio = 28
        self.feed(algo, 11)
        for fact in ("28", "56", "no split", "holds 100"):
            self.assertIn(fact, algo.quit_reason)
        self.assertNotIn("market.bar.completed", self.types_sent(algo, sent))

    def test_a_changed_ratio_with_no_split_while_flat_is_taken(self):
        algo = self.start()
        self.feed(algo, 9)
        self.ratio = 28
        self.feed(algo, 10, [trade_proposal(10, entry_level=0.875, quantity=5600, n=0.05)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(self.tickets(algo)[0].Quantity, 200)
        self.assertTrue(any("split ratio 56 -> 28" in m for m in algo.logs))


class FixtureContractTests(unittest.TestCase):
    """The fixtures above name only fields the Go payloads define."""

    def test_fixture_fields_and_schema_versions_match_the_go_payloads(self):
        proposal = trade_proposal(9)
        fixtures = [proposal, add_proposal(9), proposal_expired(proposal, 10),
                    campaign_opened(), exit_order_set(9), exit_proposed(9), unit_added(9),
                    units_stopped(9), campaign_exited(9),
                    # The decisions the adapter receives but never acts on
                    # (issue #31: a fixture for every decision type, not only
                    # the ones orders.py's SCHEMA_VERSIONS names).
                    campaign_evaluated(9), drawdown_step_applied(9), notional_account_cash_adjusted(9),
                    notional_account_rebased(9), notional_account_recovered(9), proposal_declined(9),
                    engine_state(9), setup_evaluated(9), signal(9), protective_stop_set(9),
                    protective_stop_set(9, reason="add-ladder", level=22.700000000000003,
                                        previous_level=22.1)]
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/order_decisions_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(fixtures), text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
