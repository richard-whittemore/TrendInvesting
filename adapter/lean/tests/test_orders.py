"""The adapter turns the engine's decisions into LEAN orders.

Every test here drives the real CompletedBarsAlgorithm through OnData with a
fake engine whose replies carry fixture decisions, and asserts on the orders
that reach LEAN's order book (FakeTransactions) — never on the adapter's own
state. The fixtures use the engine's own field names; FixtureContractTests
checks them against the Go payload types, so a renamed field fails here.
"""
import json
import math
import subprocess
import sys
import types
import unittest
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

import test_algorithm as scaffold
from test_publisher import Frame, bar

from client import Unavailable
import orders

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
               "protective_stop_intent": 22.1,
               # The Baseline's stop-limit, capped at level + 1N (ADR 0005, as
               # amended 2026-09-24): 24.5 + 1.2.
               "order_type": "stop-limit", "gap_buffer_n": 1,
               # Strength ranked this Signal (ADR 0010, as amended 2026-09-25);
               # the adapter carries it through without acting on it.
               "strength": 3.5}
    payload.update(changes)
    if "price_cap" not in changes:
        level, n = payload["entry_level"], payload["n"]
        # A malformed level or N gets the fixture's own cap, so each rejection
        # test still names the one field it is about.
        numeric = all(type(v) in (int, float) for v in (level, n))
        payload["price_cap"] = level + n if numeric else 25.7
    return envelope("strategy.trade.proposed", 3, decision_id("proposal", day), payload)


def add_proposal(day, unit_index=2, **changes):
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "period_end": period_end(day), "unit_index": unit_index, "valid_for_sessions": 1,
               "level": 25.1, "quantity": 100, "previous_unit_fill": 24.5,
               "campaign_n": 1.2, "rule": "add.ladder.half-n", "adr": "0006",
               "order_type": "stop-limit", "gap_buffer_n": 1}
    payload.update(changes)
    if "price_cap" not in changes:
        # The Add's cap is measured in the Campaign's frozen N.
        level, n = payload["level"], payload.get("add_n", 0) or payload["campaign_n"]
        numeric = all(type(v) in (int, float) for v in (level, n))
        payload["price_cap"] = level + n if numeric else 26.3
    return envelope("strategy.add.proposed", 4,
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
    return envelope("strategy.campaign.unit-added", 2,
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


def cash_in_lieu(effective_at, reductions, per_raw=4, cash=91.0, new_shares=7):
    """strategy.campaign.cash-in-lieu (ADR 0023): reductions are (unit index,
    quantity before, quantity after), most recent first."""
    before = sum(b for _, b, _ in reductions) or 5600
    lost = len(reductions)
    payload = {"campaign_id": decision_id("campaign", 6), "instrument_id": "AAPL",
               "corporate_action_id": "test:corporate-action:9", "effective_at": effective_at,
               "new_shares": new_shares, "old_shares": 1, "engine_shares_per_raw_share": per_raw,
               "raw_shares_lost": lost, "engine_shares_lost": lost * per_raw,
               "cash_in_lieu": cash, "currency": "USD",
               "reductions": [{"unit_index": u, "quantity_before": b, "quantity_after": a}
                              for u, b, a in reductions],
               "quantity_before": before, "quantity_after": before - lost * per_raw,
               "rule": "campaign.cash-in-lieu.most-recent-units-first", "adr": "0023"}
    return envelope("strategy.campaign.cash-in-lieu", 1,
                    "cash-in-lieu:AAPL:{}".format(effective_at), payload)


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
               "cap": "", "cap_limit": 0, "post_trade_exposure": 0, "strength": 0.0}
    return envelope("strategy.proposal.declined", 8, decision_id("proposal-declined", day), payload)


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
    return envelope("strategy.protective-stop.set", 3,
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
        if ticket.OrderId in algo.desk.orders:
            # LEAN asks the slippage model for every order the adapter
            # placed, under the tag the order carries now: one whose tag the
            # desk never gave an N reaches the model and fails it, exactly as
            # in LEAN, rather than being skipped here.
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
    def test_a_valid_proposal_becomes_a_gtc_stop_limit_order_at_its_level_and_cap(self):
        # Good-till-cancelled, never DAY: at daily resolution LEAN expires a
        # DAY order before it evaluates the next session's fill (observed on
        # the pinned image), so a DAY entry could never fill. A stop-limit,
        # limited at the proposal's price cap (ADR 0005, as amended
        # 2026-09-24).
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed)
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Symbol, ticket.Quantity, ticket.StopPrice, ticket.Tag),
                         ("AAPL", 100, 24.5, proposal["id"]))
        self.assertEqual((ticket.OrderType, ticket.LimitPrice), ("stop-limit", 25.7))
        self.assertEqual(ticket.TimeInForce, "gtc")
        self.assertEqual(self.rejections(algo), [])

    def test_an_uncapped_proposal_becomes_a_stop_market_order(self):
        # The declared Variant "uncapped" keeps Faith's stop-market entry.
        algo = self.start()
        proposal = trade_proposal(9, order_type="stop-market", gap_buffer_n=0, price_cap=0)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.OrderType, ticket.StopPrice, ticket.LimitPrice),
                         ("stop-market", 24.5, None))

    def test_a_proposal_whose_order_cannot_be_placed_is_rejected(self):
        # event.TradeProposalPayload's own rule: a stop-limit's cap is a price
        # at or above its level; a stop-market order has none; no other order
        # type exists.
        for name, changes in {
                "a cap below the level": dict(price_cap=24.4),
                "a non-positive cap": dict(price_cap=0),
                "a stop-market order stating a cap": dict(order_type="stop-market", price_cap=25.7),
                "an unknown order type": dict(order_type="limit"),
                "no order type": dict(order_type=None)}.items():
            with self.subTest(name):
                algo = self.start()
                self.feed(algo, 9, [trade_proposal(9, **changes)])
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual(self.tickets(algo), [])
                [rejection] = self.rejections(algo)
                self.assertIn("strategy.trade.proposed", rejection)

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

    def test_a_valid_add_proposal_becomes_a_gtc_stop_limit_order_at_its_rung_and_cap(self):
        algo = self.start()
        proposal = add_proposal(9)
        self.feed(algo, 9, [proposal])
        [ticket] = self.tickets(algo)
        self.assertEqual((ticket.Quantity, ticket.StopPrice, ticket.Tag, ticket.TimeInForce),
                         (100, 25.1, proposal["id"], "gtc"))
        self.assertEqual((ticket.OrderType, ticket.LimitPrice), ("stop-limit", 25.1 + 1.2))

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
        # Schema 1 predates the price cap (ADR 0005, as amended 2026-09-24):
        # read as schema 2 it would be placed with no cap at all.
        algo = self.start()
        proposal = trade_proposal(9)
        proposal["schema_version"] = 1
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

    def test_recomputed_add_n_drives_slippage(self):
        algo = self.start()
        proposal = add_proposal(9, add_n=2.2)
        self.feed(algo, 9, [proposal])
        self.assertAlmostEqual(self.slip(algo, proposal["id"]), 0.11)

    def test_an_exit_order_slips_by_the_campaigns_frozen_n_after_amendment_too(self):
        algo = self.start()
        self.hold(algo, 100)
        placed = exit_order_set(9)
        self.feed(algo, 9, [campaign_opened(campaign_n=2.5), placed])
        raised = exit_order_set(10, level=23.4)
        self.feed(algo, 10, [raised])
        self.assertAlmostEqual(self.slip(algo, placed["id"]), 0.05 * 2.5)
        self.assertAlmostEqual(self.slip(algo, raised["id"]), 0.05 * 2.5)

    def test_an_order_without_a_supplied_n_records_a_failure_and_stops_the_run(self):
        algo = self.start()
        self.slip(algo, "an order this adapter never placed")
        self.assertIn("no N was supplied", algo.security.slippage_model.failure)
        self.assertFalse(algo.fill_model_sound())
        self.assertTrue(algo.failed)

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

    def test_a_full_stop_out_cancels_the_working_add_order(self):
        # ADR 0011, as amended 2026-09-24: a stop fill that closes the whole
        # Campaign expires its pending Add in the same reply, ordinary or
        # fill-chained, so the order is cancelled before the next Session and
        # can never fill into the closed Campaign. The cancellation is
        # requested inside the slice, so LEAN confirms it after the slice, and
        # one it never confirms stops the run before the next bar.
        for chained, confirmed in ((False, True), (True, True), (True, False)):
            with self.subTest(chained=chained, confirmed=confirmed):
                algo = self.start()
                if chained:
                    add = add_proposal(9, valid_for_sessions=2)
                    self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1), add])
                    stopped_on = 11
                else:
                    add = add_proposal(11)
                    self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
                    self.feed(algo, 11, close_decisions=[add])
                    stopped_on = 12
                [ticket] = [t for t in self.tickets(algo) if t.Tag == add["id"]]
                self.assertEqual(ticket.Status, "submitted")
                [sell] = self.sells(algo)
                self.fill(algo, sell, stopped_on, 22.0, fee=1.0)
                if not confirmed:
                    algo.Transactions.cancel_outcome = "never"
                expired = proposal_expired(add, stopped_on, kind="add")
                expired["payload"].update(rule="add-proposal.superseded-by-stop",
                                          reason="superseded-by-stop")
                self.feed(algo, stopped_on, replies={"execution.fill": {"payload": {"decisions": [
                    units_stopped(stopped_on, fill_id="lean:2:2"), expired,
                    campaign_exited(stopped_on)]}}})
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual([order for order, _ in algo.Transactions.cancellations],
                                 [ticket.OrderId])
                sent = len(algo.client.sent)
                self.feed(algo, stopped_on + 1)
                if not confirmed:
                    self.assertIn("did not confirm cancelling order(s) {} (tag={})".format(
                        ticket.OrderId, add["id"]), algo.quit_reason)
                    self.assertNotIn("market.bar.completed", self.types_sent(algo, sent))
                    continue
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual(ticket.Status, "canceled")
                self.assertEqual([e["payload"]["kind"] for e in self.sent(algo, "execution.fill")],
                                 ["entry", "stop"])

    def stopped_out_with_a_working_add(self, algo, confirmed):
        """A fill-chained Add working when a stop fill closes its Campaign on the
        11th; the stop's reply expires the Add and the adapter requests its
        cancellation, which LEAN confirms after the slice unless confirmed is
        False (ADR 0011, as amended 2026-09-24)."""
        add = add_proposal(9, valid_for_sessions=2)
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1), add])
        [ticket] = [t for t in self.tickets(algo) if t.Tag == add["id"]]
        [sell] = self.sells(algo)
        self.fill(algo, sell, 11, 22.0, fee=1.0)
        if not confirmed:
            algo.Transactions.cancel_outcome = "never"
        expired = proposal_expired(add, 11, kind="add")
        expired["payload"].update(rule="add-proposal.superseded-by-stop", reason="superseded-by-stop")
        self.feed(algo, 11, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(11, fill_id="lean:2:2"), expired, campaign_exited(11)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(ticket.Status, "cancel-pending")
        return ticket

    def test_an_add_filling_before_its_cancellation_is_confirmed_stops_the_run(self):
        # ADR 0019: the engine expired the Add when the stop closed its
        # Campaign, so LEAN filling it anyway contradicts the engine's state.
        # The unconfirmed cancellation is found at the start of the next
        # slice, before any of that slice's fills reaches the engine.
        algo = self.start()
        ticket = self.stopped_out_with_a_working_add(algo, confirmed=False)
        sent = len(algo.client.sent)
        self.fill(algo, ticket, 12, 25.2)
        self.feed(algo, 12)
        self.assertTrue(algo.failed)
        self.assertIn("did not confirm cancelling order(s) {} (tag={})".format(
            ticket.OrderId, ticket.Tag), algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [],
                         "nothing may reach the engine after an unconfirmed cancellation")

    def test_a_fill_of_an_order_whose_cancellation_was_requested_is_never_sent(self):
        # Whenever it is drained, a fill of an order the adapter has asked LEAN
        # to cancel, confirmed or not, stops the run instead of reaching the
        # engine, which has already expired its proposal (ADR 0011, ADR 0019).
        for confirmed in (False, True):
            with self.subTest(confirmed=confirmed):
                algo = self.start()
                ticket = self.stopped_out_with_a_working_add(algo, confirmed=confirmed)
                if confirmed:
                    self.feed(algo, 12)
                    self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                    self.assertEqual(ticket.Status, "canceled")
                sent = len(algo.client.sent)
                # LEAN reports the fill at a point where the queue is drained
                # without the slice-start check (after a slice's decisions).
                self.fill(algo, ticket, 13, 25.2)
                algo.drain_order_events()
                self.assertTrue(algo.failed)
                self.assertIn("order {} (tag={})".format(ticket.OrderId, ticket.Tag),
                              algo.quit_reason)
                self.assertIn("cancellation", algo.quit_reason)
                self.assertNotIn("execution.fill", self.types_sent(algo, sent))

    def test_an_add_whose_cancellation_is_confirmed_never_fills(self):
        algo = self.start()
        ticket = self.stopped_out_with_a_working_add(algo, confirmed=True)
        self.feed(algo, 12)
        self.feed(algo, 13)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(ticket.Status, "canceled")
        self.assertEqual([e["payload"]["kind"] for e in self.sent(algo, "execution.fill")],
                         ["entry", "stop"])

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

    def test_a_fill_model_failure_while_acting_on_one_fill_stops_the_rest_of_its_group(self):
        # Acting on a fill's decisions can place or amend an order, and
        # LEAN's rescan can then record a fill-model failure while the next
        # fill of the same instant waits to be sent. The run stops before
        # it: the second stop fill never reaches the engine.
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
        sent = len(algo.client.sent)
        original_act = algo.desk.act

        def act(decisions, *args, **kwargs):
            result = original_act(decisions, *args, **kwargs)
            if "execution.fill" in self.types_sent(algo, sent) and algo.fill_model.failure is None:
                algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
            return result

        algo.desk.act = act
        self.feed(algo, 13)
        self.assertTrue(algo.failed)
        self.assertIn("could not be priced", algo.quit_reason)
        fills = [e["payload"] for e in self.sent(algo, "execution.fill")]
        self.assertEqual([f["price"] for f in fills[-1:]], [22.6])
        self.assertEqual(self.types_sent(algo, sent).count("execution.fill"), 1)

    def test_a_fill_model_failure_while_acting_on_a_groups_last_fill_stops_the_run_there(self):
        # The failure is recorded while acting on the only fill of the
        # instant and nothing else is queued: the run stops there, before
        # the session's snapshot or bar is sent.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 10, 24.56)
        sent = len(algo.client.sent)
        original_act = algo.desk.act

        def act(decisions, *args, **kwargs):
            result = original_act(decisions, *args, **kwargs)
            if "execution.fill" in self.types_sent(algo, sent) and algo.fill_model.failure is None:
                algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
            return result

        algo.desk.act = act
        self.feed(algo, 10)
        self.assertTrue(algo.failed)
        self.assertIn("could not be priced", algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), ["execution.fill"])

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

    def test_a_chained_add_proposal_in_a_fill_reply_is_placed(self):
        # ADR 0011's amendment: a fill-chained Add has one extra session.
        algo = self.start()
        chained = add_proposal(9, valid_for_sessions=2)
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10), chained])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual([t.Quantity for t in self.tickets(algo)], [100, -100, 100])
        self.assertEqual(self.tickets(algo)[-1].Tag, chained["id"])
        self.assertEqual(self.rejections(algo), [])

    def test_an_ordinary_add_in_a_fill_reply_is_still_stale(self):
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10), add_proposal(9)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual([t.Quantity for t in self.tickets(algo)], [100, -100])
        [rejection] = self.rejections(algo)
        self.assertIn("stale", rejection)

    def test_a_chained_add_window_counts_bars_across_a_weekend(self):
        algo = self.start()
        self.hold(algo, 100)
        chained = add_proposal(6, valid_for_sessions=2)
        self.feed(algo, 9, replies={"execution.fill": {"payload": {"decisions": [chained]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(self.tickets(algo)[-1].Tag, chained["id"])
        self.assertEqual(self.rejections(algo), [])

    def test_a_chained_add_is_accepted_at_the_first_next_bar(self):
        algo = self.start()
        self.feed(algo, 6)
        chained = add_proposal(6, valid_for_sessions=2)
        self.feed(algo, 9, [chained])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual([t.Tag for t in self.tickets(algo)], [chained["id"]])

    def test_a_chained_add_older_than_its_window_is_rejected(self):
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9)
        self.feed(algo, 10, [add_proposal(6, valid_for_sessions=2)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(self.tickets(algo), [])
        [rejection] = self.rejections(algo)
        self.assertIn("stale", rejection)

    def test_a_later_fill_reply_cannot_renew_a_chained_add_window(self):
        algo = self.start()
        self.feed(algo, 6)
        self.feed(algo, 9, [trade_proposal(9)])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 10, 24.56)
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": [
            add_proposal(6, valid_for_sessions=2)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(len(self.tickets(algo)), 1)
        [rejection] = self.rejections(algo)
        self.assertIn("stale", rejection)

    def test_an_add_with_an_invalid_window_is_rejected(self):
        for window in (None, 0, 3, -1, True, 2.0, "2"):
            with self.subTest(window=window):
                algo = self.start()
                proposal = add_proposal(9, valid_for_sessions=window)
                if window is None:
                    del proposal["payload"]["valid_for_sessions"]
                self.feed(algo, 9, [proposal])
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                self.assertEqual(self.tickets(algo), [])
                [rejection] = self.rejections(algo)
                self.assertIn("valid_for_sessions", rejection)

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

    def test_a_fill_above_a_stop_limits_cap_stops_the_run(self):
        # A stop-limit's price cap is the most the hold reserved (ADR 0005 and
        # ADR 0020, as amended 2026-09-24). A fill LEAN itself reports above
        # its own LimitPrice, raw, means this adapter's understanding of
        # LEAN's fill model is wrong, so the run stops rather than accept a
        # fill the hold did not cover; the fill is never sent to the engine.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        self.assertEqual(entry.LimitPrice, 25.7)
        sent = len(algo.client.sent)
        self.fill(algo, entry, 10, 26.0)
        self.feed(algo, 10)
        for fact in ("above its own limit", "26.0", "25.7", str(entry.OrderId)):
            self.assertIn(fact, algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [])
        self.assert_stopped_after(algo, sent)

    def test_a_fill_at_a_stop_limits_cap_plus_its_slippage_is_sent(self):
        # ADR 0005 bounds the execution by the cap before slippage, and ADR
        # 0013 adds slippage to every fill: cap plus slippage is exactly what
        # the hold reserved, and a legitimate fill.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        slippage = 0.05 * proposal["payload"]["n"]
        self.fill(algo, entry, 10, 25.7 + slippage)
        self.feed(algo, 10)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [fill] = self.sent(algo, "execution.fill")
        self.assertAlmostEqual(fill["payload"]["price"], 25.7 + slippage)
        self.assertAlmostEqual(fill["payload"]["slippage_applied"], slippage)

    def test_a_fill_just_above_a_stop_limits_cap_plus_its_slippage_stops_the_run(self):
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        sent = len(algo.client.sent)
        self.fill(algo, entry, 10, 25.7 + 0.05 * proposal["payload"]["n"] + 1e-6)
        self.feed(algo, 10)
        for fact in ("above its own limit", "plus the", "slippage", str(entry.OrderId)):
            self.assertIn(fact, algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [])

    def test_an_order_the_fill_model_could_not_price_stops_the_run_before_the_slices_fills(self):
        # LEAN swallows a fill model's exception and leaves the order
        # unfilled, so the adapter's model records the failure and the run
        # stops at the slice's first point, before any fill is sent.
        algo = self.start()
        proposal = trade_proposal(9)
        self.feed(algo, 9, [proposal])
        [entry] = self.tickets(algo)
        sent = len(algo.client.sent)
        algo.fill_model.failure = "LEAN order 1 (tag=x) could not be priced"
        self.fill(algo, entry, 10, 24.56)
        self.feed(algo, 10)
        self.assertIn("could not be priced", algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [])
        self.assert_stopped_after(algo, sent)

    def test_a_fill_model_failure_recorded_mid_slice_stops_the_run_before_any_queued_report(self):
        # LEAN rescans every working order after an order is placed or
        # amended, so the fill model can record a failure after the check at
        # the start of OnData and while other reports of that same scan are
        # queued. Every drain checks first: neither the queued fill nor the
        # placement's own report reaches the engine.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        [entry] = self.tickets(algo)
        original_act = algo.desk.act
        state = {"sent": None}

        def act(decisions, *args, **kwargs):
            result = original_act(decisions, *args, **kwargs)
            if decisions and state["sent"] is None:
                # The slice's decisions placed an order; LEAN's rescan then
                # filled another and the fill model failed on a third.
                state["sent"] = len(algo.client.sent)
                algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
                self.fill(algo, entry, 10, 24.56)
            return result

        algo.desk.act = act
        self.feed(algo, 10, [trade_proposal(10)])
        self.assertIsNotNone(state["sent"])
        self.assertTrue(algo.failed)
        self.assertIn("could not be priced", algo.quit_reason)
        self.assertEqual([t for t in self.types_sent(algo, state["sent"])
                          if t.startswith("execution.")], [])

    def test_a_fill_model_failure_while_acting_on_a_snapshots_decisions_sends_no_bar(self):
        # A snapshot's decisions can place or amend an order, and LEAN's
        # rescan can then record a fill-model failure. The next input, the
        # session's bar, is refused at the publisher, the one chokepoint
        # every input passes through: neither the bar nor its session close
        # reaches the engine.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        state = {"sent": None}
        original_act = algo.desk.act

        def act(decisions, *args, **kwargs):
            result = original_act(decisions, *args, **kwargs)
            if state["sent"] is None and algo.client.sent \
                    and algo.client.sent[-1]["type"] == "account.snapshot":
                state["sent"] = len(algo.client.sent)
                algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
            return result

        algo.desk.act = act
        self.feed(algo, 10, snapshot_decisions=[proposal_expired(trade_proposal(9), 10)])
        self.assertIsNotNone(state["sent"])
        self.assertTrue(algo.failed)
        self.assertIn("could not be priced", algo.quit_reason)
        self.assertEqual(self.types_sent(algo, state["sent"]), [])

    def test_a_fill_model_failure_during_one_decision_stops_the_rest_of_the_batch(self):
        # OrderDesk.act takes the engine's decisions in turn. If LEAN records
        # a fill-model failure while the first one amends an order, the next
        # one places or amends nothing: the run is stopping, and containment
        # is a person's (ADR 0019).
        for refused in (False, True):
            with self.subTest(refused=refused):
                algo = self.start()
                self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
                [sell] = self.sells(algo)
                placed = len(self.tickets(algo))
                amend = sell.Update

                def update(fields, amend=amend, algo=algo):
                    response = amend(fields)
                    if refused:
                        algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
                    return response

                sell.Update = update
                self.feed(algo, 11, close_decisions=[exit_order_set(11, level=22.5),
                                                     add_proposal(11)])
                self.assertEqual(sell.StopPrice, 22.5)
                if not refused:
                    self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                    self.assertEqual(len(self.tickets(algo)), placed + 1)
                    continue
                self.assertTrue(algo.failed)
                self.assertIn("could not be priced", algo.quit_reason)
                self.assertEqual(len(self.tickets(algo)), placed)

    def test_no_order_is_placed_amended_or_cancelled_while_a_fill_model_failure_is_recorded(self):
        # The desk's one chokepoint for every change it makes to LEAN's order
        # book: once a failure is recorded, submit, amend and cancel all
        # refuse, and LEAN's book is left exactly as it was.
        algo = self.start()
        self.entered(algo, reply=[campaign_opened(), exit_order_set(10, level=22.1)])
        self.feed(algo, 11, close_decisions=[add_proposal(11)])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [sell] = self.sells(algo)
        add = self.tickets(algo)[-1]
        algo.fill_model.failure = "LEAN order 9 (tag=x) could not be priced"
        book = algo.Transactions
        before = (len(book.tickets), list(book.updates), sell.StopPrice, add.Status)
        fields = scaffold.UpdateOrderFields()
        fields.StopPrice = 23.0
        props = scaffold.OrderProperties()
        attempts = {
            "submit": lambda: algo.desk._submit(10, 24.0, "another", props, 25.0),
            "amend": lambda: algo.desk._amend(sell, fields),
            "cancel": lambda: algo.desk._cancel(add),
        }
        for action, attempt in attempts.items():
            with self.subTest(action):
                with self.assertRaises(orders.Uncertain) as caught:
                    attempt()
                self.assertIn("could not be priced", str(caught.exception))
                self.assertEqual((len(book.tickets), list(book.updates), sell.StopPrice,
                                  add.Status), before)

    def test_a_fill_model_failure_in_the_last_slice_stops_the_run_at_its_end(self):
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9)])
        sent = len(algo.client.sent)
        algo.fill_model.failure = "LEAN order 1 (tag=x) could not be priced"
        algo.OnEndOfAlgorithm()
        self.assertTrue(algo.failed)
        self.assertIn("could not be priced", algo.quit_reason)
        self.assertEqual(self.types_sent(algo, sent), [])

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
                      "rounded down", "to the cent", "split", "price cap", "StopLimitOrder"):
            self.assertIn(topic, text)
        # Every statement about LEAN's own behaviour was settled by a run on
        # the pinned image, the price cap's included: none is left as belief.
        # The no-fill-exceeds-its-hold guarantee is exact only in cmd/backtest:
        # LEAN's fee model can charge more than ADR 0013's schedule the hold
        # reserves (a $1.00 minimum against a few cents), so the account
        # statement qualifies it by that difference (#81).
        [account] = [m for m in report if "account (ADR 0010)" in m]
        for fact in ("exact in cmd/backtest", "fee model", "$1.00 minimum", "#81"):
            self.assertIn(fact, account)
        # No statement about LEAN's own behaviour is left unsettled: the price
        # cap, once the only belief rather than an observation, is confirmed
        # by a probe on the pinned image (README.md, "Observed LEAN
        # behaviour"), and the report says so rather than leaving "believed"
        # or "unconfirmed" language behind.
        self.assertEqual([m for m in report if "unconfirmed" in m.lower() or "believed" in m], [])
        [price_cap] = [m for m in report if "price cap (ADR 0005" in m]
        for fact in (
                # The adapter's own fill model, ADR 0005's rule, in place of
                # LEAN's native stop-limit fill, confirmed on the pinned image
                # against the table shared with internal/fills.
                "adapter's ADR 0005 fill model", "not by LEAN's native stop-limit fill",
                "EquityFillModel", "an exact touch included", "max(stop, open)",
                "trades back down to it", "slippage_n x N", "stop_limit_fill_cases.json",
                "to within 1e-9", "expiry stays the adapter's",
                # The guard and the hold, qualified by the same fee-model gap
                # the account paragraph names.
                "above its own LimitPrice plus the slippage", "fee-model gap #81 tracks",
                # A fill model's exception is swallowed by LEAN, so the run
                # stops on the recorded failure instead.
                "swallows an exception",
                # LEAN's native model is kept as history.
                "min(high, limit)", "favorable-gap open", "applied no slippage",
                "the limit exactly as it adjusts its stop"):
            self.assertIn(fact, price_cap)
        [slippage] = [m for m in report if m.startswith("adapter: fill model: slippage is")]
        self.assertIn("adds it explicitly", slippage)
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

    def test_a_cap_is_limited_in_raw_prices_rounded_down_to_the_tick(self):
        # 0.925 split-adjusted is 25.9 raw at a ratio of 28; a cap of 0.92525
        # is 25.907 raw, limited at 25.90: rounded down, so LEAN's limit never
        # exceeds the engine's cap (ADR 0005 and ADR 0020, as amended).
        for cap, limit in ((0.925, 25.9), (0.92525, 25.9)):
            with self.subTest(cap=cap):
                algo = self.start()
                self.feed(algo, 9, [self.entry(price_cap=cap)])
                self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
                [ticket] = self.tickets(algo)
                self.assertEqual((ticket.StopPrice, ticket.LimitPrice), (24.5, limit))

    def test_a_cap_whose_tick_floor_is_below_the_stop_is_rejected_not_placed(self):
        # A stop of 0.925182 split-adjusted is 25.9051 raw at a ratio of 28,
        # which LEAN places at 25.91; a cap of 0.9252 is 25.9056 raw, floored
        # to 25.90, below the stop as placed, so touching it could never
        # fill. The limit is never raised above the engine's cap (ADR 0005
        # and ADR 0020, as amended): the proposal is rejected, not placed.
        algo = self.start()
        proposal = self.entry(entry_level=0.925182, price_cap=0.9252)
        self.feed(algo, 9, [proposal])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(self.tickets(algo), [])
        self.assertTrue(any("below the stop" in r for r in self.rejections(algo)), self.rejections(algo))

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
              checked=True, limit_rounding=round, reference=49.0, engine=None,
              quantity_rounding=round, processed=True):
        """The split's time step before day's session, as observed on the pinned
        image. LEAN splits the holding, paying the fraction as cash at the
        reference price times the factor; raises OnData with the split and no
        bar (the tickets not yet adjusted); splits each open order and reports
        it through OnOrderEvent; then the adapter's 00:01 scheduled check runs,
        before the session's fills. before_data and meddle change LEAN's state
        before OnData and after the orders' adjustment; checked=False leaves
        the scheduled check out, as though it had not run; processed=False
        stops before LEAN processes the requests it made. engine is the
        decisions the engine answers the split's corporate action with; by
        default, the reducer's own answer to a split that lost no share: the
        cash it paid, if any, reducing nothing (ADR 0023)."""
        self.at(algo, day)
        book = algo.Transactions
        book.split_holding("AAPL", factor, reference)
        if before_data is not None:
            before_data()

        def no_share_lost(sent):
            action = sent["payload"]
            if not action["cash_in_lieu"]:
                return {"payload": {"decisions": []}}
            return {"payload": {"decisions": [cash_in_lieu(
                action["effective_at"], [], per_raw=action["engine_shares_per_raw_share"],
                cash=action["cash_in_lieu"], new_shares=action["new_shares"])]}}
        algo.client.reply_overrides = {"market.corporate-action": no_share_lost if engine is None
                                       else {"payload": {"decisions": list(engine)}}}
        algo.OnData(scaffold.slice_of(splits={"AAPL": types.SimpleNamespace(
            Type="split-occurred", SplitFactor=factor, ReferencePrice=reference,
            Time=datetime(2014, 6, day))}))
        if orders_split:
            book.split_orders("AAPL", factor, limit_rounding=limit_rounding,
                              quantity_rounding=quantity_rounding)
        if meddle is not None:
            meddle()
        if checked:
            self.scheduled_check(algo)
        if processed:
            # LEAN's next time step processes the requests made at 00:01,
            # before the session's fills (observed on the pinned image).
            book.settle()

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

    def test_a_stop_fill_in_the_splits_final_verification_slice_is_not_a_missing_stop(self):
        # Greptile 4112071877: require_split_applied's own "final" check now
        # runs before this slice's fills are drained, exactly like a
        # dividend or a new split, so LEAN's Portfolio and order book can
        # already reflect this session's own stop fill, closing the Unit,
        # before the engine has even been told of it. Reading the still-open
        # (pre-fill) Exit Order as a missing stop, or the still-unreduced
        # (pre-fill) engine Units as disagreeing with LEAN's now-flat
        # holding, would both be false: the fill fully explains both, once
        # drained.
        algo, sell = self.held()
        self.split(algo, 11)
        self.ratio = 28
        self.fill(algo, sell, 11, 22.5, fee=1)
        # The existing order fake changes shares but leaves cash to its
        # caller: 200 raw shares sold at 22.5.
        algo.Portfolio.Cash += 200 * 22.5 - 1
        self.feed(algo, 11, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(11, fill_id="lean:1:2"), campaign_exited(11)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stop = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((stop["kind"], stop["quantity"]), ("stop", 5600))

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

    def test_a_split_that_moved_a_limit_far_below_the_cap_stops_before_the_session(self):
        # A working stop-limit entry's limit is split like its stop, confirmed
        # by a probe on the pinned image (README.md, "Observed LEAN
        # behaviour"); one LEAN left more than a tick below the cap is not the
        # order the engine's hold was computed for.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.875, quantity=5600, n=0.05)])
        [entry] = self.tickets(algo)
        self.assertAlmostEqual(entry.LimitPrice, 0.925 * 56)
        self.split(algo, 10, meddle=lambda: setattr(entry, "LimitPrice", 20.0))
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(entry.OrderId), "limited at 20.0000", "25.9000")

    def capped_entry_across_a_split(self, limit_rounding, acknowledge=True):
        """A stop-limit entry capped at 0.92525 split-adjusted, limited at 51.81
        raw at 56 (51.814 floored), working across the 2-for-1: at 28 its cap
        is 25.907 raw, and LEAN's split puts the limit at 25.90 rounded down
        or 25.91 rounded up."""
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.875, quantity=5600, n=0.05,
                                           price_cap=0.92525)])
        [entry] = self.tickets(algo)
        self.assertEqual(entry.LimitPrice, 51.81)
        algo.Transactions.acknowledge_updates = acknowledge
        self.split(algo, 10, limit_rounding=limit_rounding)
        return algo, entry

    def test_a_split_limit_rounded_up_above_the_cap_is_amended_down(self):
        # A limit above the cap could fill above what the engine's hold
        # reserved (ADR 0020, as amended 2026-09-24), so it is amended to the
        # cap floored to the tick before the session can fill.
        algo, entry = self.capped_entry_across_a_split(math.ceil)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(algo.Transactions.limit_updates, [(entry.OrderId, 25.9)])
        self.assertEqual(entry.LimitPrice, 25.9)

    def test_a_split_limit_rounded_down_below_the_cap_is_kept(self):
        algo, entry = self.capped_entry_across_a_split(math.floor)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertLessEqual(entry.StopPrice, entry.LimitPrice)
        self.assertEqual(algo.Transactions.limit_updates, [])
        self.assertEqual(entry.LimitPrice, 25.9)

    def test_an_unacknowledged_amendment_of_a_split_limit_stops_before_the_session(self):
        algo, entry = self.capped_entry_across_a_split(math.ceil, acknowledge=False)
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(entry.OrderId), "25.9100", "25.9070")

    def test_a_split_cap_that_floors_below_the_split_stop_stops_before_the_session(self):
        # k = 0: the cap is the level, 0.87525 split-adjusted, 49.014 raw at 56
        # (limit 49.01 floored). At 28 LEAN rounds the split stop up to 24.51
        # and the limit up to 24.51, above the 24.507 cap; flooring the cap
        # gives 24.50, below the stop, so a touch of the stop could no longer
        # fill. The run stops rather than silently skip the Unit.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.87525, quantity=5600, n=0.05,
                                           gap_buffer_n=0, price_cap=0.87525)])
        [entry] = self.tickets(algo)
        self.split(algo, 10, limit_rounding=math.ceil)
        self.assertEqual(algo.Transactions.limit_updates, [])
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(entry.OrderId), "below its stop", "24.5100")

    def test_a_split_limit_rounded_down_below_the_split_stop_stops_before_the_session(self):
        # The same k = 0 order with the limit rounded down, to 24.50: within a
        # tick of the cap, but below the 24.51 stop.
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.87525, quantity=5600, n=0.05,
                                           gap_buffer_n=0, price_cap=0.87525)])
        [entry] = self.tickets(algo)
        self.split(algo, 10, limit_rounding=math.floor)
        self.assert_stopped_before_the_session(
            algo, "split", "order {}".format(entry.OrderId), "below its stop", "24.5000")

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
        # Nothing was held, so there is no cash in lieu to publish (ADR 0023).
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])

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


# LEAN's factor file's 7-for-1 of AAPL on 2014-06-09: 1/7 rounded to seven
# digits, slightly more than 1/7, so LEAN's division of a holding by it falls
# just short of 7 times the holding (ADR 0023's Context).
SEVENTH = 0.1428572
SPLIT_AT = "2014-06-11T04:00:00Z"


def lean_split_to(ticket, quantity):
    """LEAN's split of ticket landed on quantity (its own rounding)."""
    ticket.Quantity = quantity
    ticket.UpdateRequests[-1].Quantity = quantity


def lean_cash_in_lieu(before, factor, reference):
    """LEAN's cash for the fraction of a share its truncated split left: the
    fraction, times the reference price, times the factor (observed on the
    pinned image: $0.25 on 1,000 shares, $2.75 on 11,056)."""
    split = Decimal(before) / Decimal(repr(factor))
    return float((split - int(split)) * Decimal(repr(reference)) * Decimal(repr(factor)))


class CashInLieuTests(OrderTestCase):
    """A split whose rounded factor left LEAN short of the engine's Units at the
    exact ratio (CONTEXT.md: "Cash in lieu"; ADR 0023).

    The adapter derives the shortfall from LEAN's holding against the Units,
    accepts it only when it is at most one raw share per Unit and is LEAN's
    own truncation of the pre-split holding, publishes it as a
    market.corporate-action of kind split, and carries the engine's reduced
    Exit Orders into LEAN before the session. Anything else stops the run as
    before.
    """
    ratio = 56
    split = SplitTests.split
    scheduled_check = SplitTests.scheduled_check
    held = SplitTests.held
    assert_stopped_before_the_session = SplitTests.assert_stopped_before_the_session

    def held_aapl(self):
        """AAPL before its 7-for-1: one Unit of 3,516 raw shares at 28
        split-adjusted shares a raw share, its Exit Order at 0.8 (22.40 raw)."""
        return self.held(ratio=28, quantity=3516 * 28)

    def aapl_cash_in_lieu(self, reductions=((1, 98448, 98444),)):
        """The reducer's answer to the rounded 7-for-1: the cash LEAN paid,
        and the most recent Unit one raw share smaller."""
        return cash_in_lieu(SPLIT_AT, list(reductions), cash=lean_cash_in_lieu(3516, SEVENTH, 24.0))

    def reduced(self, quantity=3516 * 28 - 4, level=0.8, **changes):
        return exit_order_set(11, level=level, quantity=quantity, cause="split",
                              as_of=SPLIT_AT, **changes)

    def test_the_rounded_7_for_1_is_published_as_cash_in_lieu_and_carried_across(self):
        algo, sell = self.held_aapl()
        cash = lean_cash_in_lieu(3516, SEVENTH, 24.0)
        reduced = self.reduced()
        self.split(algo, 11, factor=SEVENTH, reference=24.0,
                   engine=[cash_in_lieu(SPLIT_AT, [(1, 98448, 98444)], cash=cash), reduced])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(algo.Portfolio.holdings["AAPL"], 24611)
        [action] = self.sent(algo, "market.corporate-action")
        self.assertEqual(action["schema_version"], 3)
        self.assertEqual(action["event_time"], SPLIT_AT)
        payload = dict(action["payload"])
        self.assertAlmostEqual(payload.pop("cash_in_lieu"), cash, places=9)
        self.assertEqual(payload, {
            "instrument_id": "AAPL", "kind": "split", "effective_at": SPLIT_AT,
            "new_shares": 7, "old_shares": 1, "engine_shares_per_raw_share": 4,
            "raw_shares_lost": 1, "currency": "USD"})
        # LEAN split the order to 24,612 raw shares (the exact ratio); the
        # engine's reduced Exit Order is 24,611, which is what LEAN holds,
        # and the order now carries that decision's tag. LEAN applies the
        # quantity when it processes the request, before the session.
        self.assertEqual(sell.Tag, reduced["id"])
        self.assertIn((sell.OrderId, -24611), algo.Transactions.quantity_updates)
        self.assertEqual(sell.UpdateRequests[-1].Quantity, -24611)
        self.assertAlmostEqual(sell.StopPrice, 3.2)
        # The next session trades on: the stop fills for the reduced Unit.
        self.ratio = 4
        self.feed(algo, 12)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(sell.Quantity, -24611)
        self.fill(algo, sell, 13, 3.1)
        self.feed(algo, 13, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(13, fill_id="lean:1:2"), campaign_exited(13)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stop = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((stop["kind"], stop["quantity"]), ("stop", 24611 * 4))

    def test_the_published_split_is_valid_in_go(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, engine=[
            self.aapl_cash_in_lieu(), self.reduced()])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        result = subprocess.run(
            ["go", "run", "./adapter/lean/tests/testdata/corporate_action_contract.go"],
            cwd=Path(__file__).resolve().parents[3],
            input=json.dumps(self.sent(algo, "market.corporate-action")[0], separators=(",", ":")),
            text=True,
            capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_a_split_with_no_share_lost_still_publishes_its_cash(self):
        # AAPL's 2-for-1 of 2005: nothing lost, a fraction paid as cash. The
        # engine records the cash and changes no order.
        algo, sell = self.held()
        self.split(algo, 11)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        [action] = self.sent(algo, "market.corporate-action")
        self.assertEqual(action["payload"]["raw_shares_lost"], 0)
        self.assertAlmostEqual(action["payload"]["cash_in_lieu"], lean_cash_in_lieu(100, HALF, 49.0))
        self.assertEqual(sell.Quantity, -200)
        self.assertEqual(algo.Transactions.quantity_updates, [])

    def test_a_shortfall_beyond_one_raw_share_per_unit_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False,
                   before_data=lambda: algo.Portfolio.holdings.update(AAPL=24610))
        self.assert_stopped_before_the_session(algo, "holds 24610", "24612")
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_a_shortfall_that_is_not_the_splits_own_truncation_stops_the_run(self):
        # One raw share short of one Unit is within the bound, but LEAN's own
        # division of 100 raw shares by the factor is 200, not 199: the share
        # is missing for some other reason, which is not cash in lieu.
        algo, _ = self.held()
        self.split(algo, 11, checked=False,
                   before_data=lambda: algo.Portfolio.holdings.update(AAPL=199))
        self.assert_stopped_before_the_session(algo, "holds 199", "truncat")
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_a_holding_above_leans_own_truncation_stops_the_run(self):
        # LEAN holds exactly the engine's 24,612, but its own division of
        # 3,516 raw shares by the rounded factor is 24,611: the holding is not
        # what this split produced, so it is unexplained even though it
        # matches the Units.
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False,
                   before_data=lambda: algo.Portfolio.holdings.update(AAPL=24612))
        self.assert_stopped_before_the_session(algo, "holds 24612", "truncat")
        self.assertEqual(self.sent(algo, "market.corporate-action"), [])

    def test_an_engine_that_does_not_reduce_the_units_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False, engine=[])
        self.assert_stopped_before_the_session(algo, "cash in lieu")

    def test_an_engine_reduction_that_is_not_one_raw_share_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False, engine=[
            self.aapl_cash_in_lieu(), self.reduced(quantity=98440)])
        self.assert_stopped_before_the_session(algo, "98440")

    def test_an_engine_that_moves_a_level_at_a_split_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False, engine=[
            self.aapl_cash_in_lieu(), self.reduced(level=0.81)])
        self.assert_stopped_before_the_session(algo, "level")

    def test_a_cash_in_lieu_for_a_unit_lean_does_not_carry_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.split(algo, 11, factor=SEVENTH, reference=24.0, checked=False, engine=[
            self.aapl_cash_in_lieu([(2, 98448, 98444)]), self.reduced()])
        self.assert_stopped_before_the_session(algo, "unit 2")

    def test_a_cash_in_lieu_outside_a_split_stops_the_run(self):
        algo, _ = self.held_aapl()
        self.feed(algo, 11, [cash_in_lieu(SPLIT_AT, [(1, 98448, 98444)])])
        self.assertTrue(algo.failed)
        self.assertIn("split", algo.quit_reason)

    def test_an_order_lean_split_one_share_off_is_amended_to_the_engines(self):
        # LEAN rounds each open order's split with the same rounded factor,
        # so an order can land one raw share from the engine's figure at the
        # new ratio: it is amended back before the session.
        algo, sell = self.held()
        self.split(algo, 11, meddle=lambda: lean_split_to(sell, -199), processed=False)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertIn((sell.OrderId, -200), algo.Transactions.quantity_updates)
        self.assertEqual(sell.Quantity, -199)
        algo.Transactions.settle()
        self.ratio = 28
        self.feed(algo, 11)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(sell.Quantity, -200)

    def test_an_unacknowledged_quantity_amendment_stops_the_run(self):
        algo, sell = self.held()

        def refuse():
            lean_split_to(sell, -199)
            algo.Transactions.acknowledge_updates = False
        self.split(algo, 11, meddle=refuse)
        # The failure carries LEAN's own reason.
        self.assert_stopped_before_the_session(
            algo, "order {}".format(sell.OrderId), "-199", "invalid-request",
            "the fake broker refused the update")

    def test_lean_truncating_every_resting_order_at_the_split_is_carried_across(self):
        # The 2014-06-09 acceptance run: LEAN split each Unit's Exit Order on
        # its own, truncating each (-1,310 raw became -9,169, not -9,170),
        # while the holding as a whole lost one share. The engine reduces the
        # most recent Unit only, so the other Unit's order is amended back up
        # to its exact figure: LEAN answers with success and applies it when
        # it processes the request, before the session's fills.
        algo, (first, second) = self.two_units_aapl()
        reduced = exit_order_set(13, unit_index=2, level=0.82, quantity=36676, cause="split",
                                 as_of="2014-06-13T04:00:00Z")
        self.split(algo, 13, factor=SEVENTH, reference=24.0, quantity_rounding=math.trunc,
                   processed=False, engine=[
                       cash_in_lieu("2014-06-13T04:00:00Z", [(2, 36680, 36676)],
                                    cash=lean_cash_in_lieu(2620, SEVENTH, 24.0)),
                       reduced])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual(algo.Portfolio.holdings["AAPL"], 18339)
        self.assertEqual((first.Quantity, second.Quantity), (-9169, -9169))
        self.assertEqual(first.UpdateRequests[-1].Quantity, -9170)
        self.assertEqual(second.Tag, reduced["id"])
        self.assertNotIn((second.OrderId, -9170), algo.Transactions.quantity_updates)
        # LEAN processes the amendment; the next slice's final check finds
        # every Exit Order at its Unit's quantity, together the holding.
        algo.Transactions.settle()
        self.ratio = 4
        self.feed(algo, 13)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.assertEqual((first.Quantity, second.Quantity), (-9170, -9169))
        self.assertEqual(-(first.Quantity + second.Quantity), algo.Portfolio.holdings["AAPL"])

    def test_an_amendment_lean_refuses_when_it_processes_it_stops_the_run(self):
        # LEAN can accept a request and refuse it when it processes it: the
        # next slice's final check then finds the old quantity, and stops the
        # run naming LEAN's reason.
        algo, (first, _) = self.two_units_aapl()
        self.split(algo, 13, factor=SEVENTH, reference=24.0, quantity_rounding=math.trunc,
                   processed=False, engine=[
                       cash_in_lieu("2014-06-13T04:00:00Z", [(2, 36680, 36676)],
                                    cash=lean_cash_in_lieu(2620, SEVENTH, 24.0)),
                       exit_order_set(13, unit_index=2, level=0.82, quantity=36676,
                                      cause="split", as_of="2014-06-13T04:00:00Z")])
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        request = first.UpdateRequests[-1]
        request.Status = "error"
        request.Response = types.SimpleNamespace(ErrorCode="invalid-new-order-status",
                                                 ErrorMessage="processed and refused")
        algo.Transactions.deferred = [d for d in algo.Transactions.deferred
                                      if not isinstance(d[1], tuple) or d[1][1] is not request]
        algo.Transactions.settle()
        self.ratio = 4
        self.feed(algo, 13)
        self.assertTrue(algo.failed)
        for fact in ("order {}".format(first.OrderId), "-9169", "invalid-new-order-status",
                     "processed and refused"):
            self.assertIn(fact, algo.quit_reason)

    def truncated_split(self, algo, processed=True):
        """The two-Unit Campaign across the rounded 7-for-1, LEAN truncating
        each order, the engine reducing Unit 2; returns Unit 2's re-stated
        Exit Order."""
        reduced = exit_order_set(13, unit_index=2, level=0.82, quantity=36676, cause="split",
                                 as_of="2014-06-13T04:00:00Z")
        self.split(algo, 13, factor=SEVENTH, reference=24.0, quantity_rounding=math.trunc,
                   processed=processed, engine=[
                       cash_in_lieu("2014-06-13T04:00:00Z", [(2, 36680, 36676)],
                                    cash=lean_cash_in_lieu(2620, SEVENTH, 24.0)),
                       reduced])
        return reduced

    def test_a_reduced_units_stop_fills_after_the_split_and_is_slipped(self):
        # The re-stated Exit Order carries the engine's new decision id as its
        # tag; LEAN slips its fill by the Campaign's frozen N at the new
        # ratio, looked up by that tag (ADR 0013, ADR 0023).
        algo, (first, second) = self.two_units_aapl()
        reduced = self.truncated_split(algo)
        self.assertEqual(second.Tag, reduced["id"])
        self.ratio = 4
        self.feed(algo, 13)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        self.fill(algo, second, 16, 3.2)
        self.feed(algo, 16, replies={"execution.fill": {"payload": {"decisions": [
            units_stopped(16, unit_indexes=(2,), fill_id="lean:{}:2".format(second.OrderId),
                          remaining=1)]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        stop = self.sent(algo, "execution.fill")[-1]["payload"]
        self.assertEqual((stop["kind"], stop["quantity"]), ("stop", 36676))
        self.assertAlmostEqual(stop["slippage_applied"], 0.05 * 0.05)
        self.assertAlmostEqual(algo.security.slippage_model.GetSlippageApproximation(
            algo.security, types.SimpleNamespace(Tag=second.Tag, Id=second.OrderId)),
            0.05 * 0.05 * 4)

    def test_a_tag_lean_refuses_to_apply_at_the_split_stops_the_run(self):
        # A tag-only amendment (the reduced Unit's order already at its
        # quantity) is answered with success on submission; if LEAN refuses it
        # when it processes it, the order does not carry the decision in
        # force, and the next slice's check stops the run.
        algo, (_, second) = self.two_units_aapl()
        old_tag = second.Tag
        self.truncated_split(algo, processed=False)
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        request = second.UpdateRequests[-1]
        request.Status = "error"
        request.Response = types.SimpleNamespace(ErrorCode="invalid-request",
                                                 ErrorMessage="tag refused")
        second.Tag = old_tag
        algo.Transactions.deferred = [d for d in algo.Transactions.deferred
                                      if not isinstance(d[1], tuple) or d[1][1] is not request]
        algo.Transactions.settle()
        self.ratio = 4
        self.feed(algo, 13)
        self.assertTrue(algo.failed)
        for fact in ("order {}".format(second.OrderId), old_tag, "exit-order-set-unit-2-split"):
            self.assertIn(fact, algo.quit_reason)

    def two_units_aapl(self):
        """Two Units of 1,310 raw shares each at 28 split-adjusted shares a raw
        share (the acceptance run's Unit size), each resting its Exit Order."""
        self.ratio = 28
        algo = self.start()
        self.feed(algo, 9, [trade_proposal(9, entry_level=0.875, quantity=36680, n=0.05)])
        [entry] = self.tickets(algo)
        self.fill(algo, entry, 10, entry.StopPrice + 0.1)
        self.feed(algo, 10, replies={"execution.fill": {"payload": {"decisions": [
            campaign_opened(campaign_n=0.05), exit_order_set(10, level=0.8, quantity=36680)]}}})
        self.feed(algo, 11, close_decisions=[add_proposal(
            11, level=0.9, quantity=36680, campaign_n=0.05, previous_unit_fill=0.88)])
        add = self.tickets(algo)[-1]
        self.fill(algo, add, 12, add.StopPrice + 0.1)
        self.feed(algo, 12, replies={"execution.fill": {"payload": {"decisions": [
            unit_added(12, fill_id="lean:{}:2".format(add.OrderId), quantity=36680),
            exit_order_set(12, unit_index=2, level=0.82, quantity=36680, cause="add")]}}})
        self.assertFalse(algo.failed, getattr(algo, "quit_reason", ""))
        sells = self.sells(algo)
        self.assertEqual([t.Quantity for t in sells], [-1310, -1310])
        return algo, sells


class FixtureContractTests(unittest.TestCase):
    """The fixtures above name only fields the Go payloads define."""

    def test_fixture_fields_and_schema_versions_match_the_go_payloads(self):
        proposal = trade_proposal(9)
        fixtures = [proposal, add_proposal(9, add_n=2.2, level=25.6), add_proposal(9), proposal_expired(proposal, 10),
                    campaign_opened(), exit_order_set(9), exit_proposed(9), unit_added(9),
                    units_stopped(9), campaign_exited(9),
                    cash_in_lieu("2014-06-11T04:00:00Z", [(1, 98448, 98444)]),
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
