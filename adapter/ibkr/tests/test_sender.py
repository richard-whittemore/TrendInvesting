"""The sending seam: dry-run never reaches IBKR, paper sends only to a paper account, live stays disabled.

AGENTS.md rule 5: no live-order behaviour before the paper-trading and
limited-live gates. Paper sending is bound to the configuration's mode and
account (IBKR paper account IDs begin "DU") and fails closed otherwise.
"""
import sys
import unittest
from datetime import date
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import ACCOUNT
from test_connection import Harness, config
from connection import NotConnected
from orders import collect_broker_orders
from sender import (OrderRequest, DryRunSender, LiveSendingDisabled, NotReconciled, PaperSender, SendRefused,
                    SessionOver, UnsendableOrder, make_sender, release_to_request)


# The New York date of the session the test orders are released in, and the
# adapter's current session unless a test moves it.
SESSION = date(2026, 10, 16)


def request(**overrides):
    values = dict(client_order_id="tag-1", account_id=ACCOUNT, con_id=756733, symbol="SPY",
                  action="BUY", quantity=Decimal("10"), order_type="MKT", limit_price=None,
                  tif="DAY", session=SESSION)
    values.update(overrides)
    return OrderRequest(**values)


def limit_request(**overrides):
    values = dict(order_type="LMT", limit_price=Decimal("401.5"), quantity=Decimal("9"))
    values.update(overrides)
    return request(**values)


def paper_harness(reconciled=True):
    h = Harness(mode="paper")
    h.conn.connect()
    if reconciled:
        h.conn.mark_reconciled()
    return h


def paper_sender(h, journal, today=SESSION):
    return PaperSender(h.conn, journal.append, today=lambda: today)


class SenderTests(unittest.TestCase):
    def test_sender_documents_the_account_quantity_policy(self):
        self.assertIn("configured quantity scale", PaperSender.__doc__)
        self.assertIn("ADR 0026 D7-F", PaperSender.__doc__)
        self.assertNotIn("whole-share DAY", PaperSender.__doc__)

    def test_dry_run_journals_and_never_calls_place_order(self):
        h = Harness()
        h.conn.connect()
        journal = []
        sender = make_sender(config(), journal.append)
        self.assertIsInstance(sender, DryRunSender)
        record = sender.send(request())
        self.assertEqual(journal, [record])
        self.assertFalse(record.send)
        self.assertEqual(record.request.client_order_id, "tag-1")
        self.assertEqual(h.fake.called("place_order"), [])

    def test_live_is_disabled(self):
        live = config(mode="live", account_id="U0000001")
        with self.assertRaises(LiveSendingDisabled):
            make_sender(live, lambda r: None, Harness(mode="live", account_id="U0000001").conn)

    def test_unknown_mode_is_refused(self):
        with self.assertRaises(ValueError):
            make_sender(config()._replace(mode="yolo"), lambda r: None)

    def test_malformed_requests_are_refused_before_the_journal(self):
        journal = []
        sender = DryRunSender(journal.append)
        for bad in (dict(client_order_id=""), dict(action="HOLD"), dict(quantity=Decimal("0")),
                    dict(quantity=10.0), dict(order_type="LMT", limit_price=None),
                    dict(order_type="MKT", limit_price=Decimal("1")), dict(account_id="")):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                sender.send(request(**bad))
        self.assertEqual(journal, [])


class PaperSenderTests(unittest.TestCase):
    def test_paper_on_a_paper_account_sends_one_order_to_the_gateway_and_journals_it(self):
        h = paper_harness()
        journal = []
        sender = make_sender(h.conn.config, journal.append, h.conn, today=lambda: SESSION)
        self.assertIsInstance(sender, PaperSender)
        record = sender.send(limit_request())
        self.assertEqual(len(h.fake.placed), 1)
        order_id, contract, order = h.fake.placed[0]
        # IBKR's next valid order ID, from the connect, then the next one.
        self.assertEqual(order_id, 7)
        self.assertEqual(h.conn.next_order_id, 8)
        self.assertEqual((contract.conId, contract.symbol, contract.secType, contract.currency, contract.exchange),
                         (756733, "SPY", "STK", "USD", "SMART"))
        self.assertEqual((order.orderRef, order.account, order.action, order.totalQuantity, order.orderType,
                          order.lmtPrice, order.tif, order.transmit),
                         ("tag-1", ACCOUNT, "BUY", Decimal("9"), "LMT", 401.5, "DAY", True))
        self.assertTrue(record.send)
        self.assertEqual((record.client_id, record.broker_order_id), (11, 7))
        self.assertEqual(journal, [record])

    def test_ibkrs_echo_of_an_order_just_placed_is_no_stray_and_the_next_read_does_not_wait_for_quiet(self):
        # IBKR echoes every order placed with an openOrder and orderStatus
        # outside any list: expected traffic, matched to the placement by
        # its order ID, so the next read is asked at once. A row of any
        # other order with no list outstanding is still a stray.
        h = paper_harness()
        collect_broker_orders(h.conn)  # the link is level before the placement
        h.fake.on_place = h.fake.accept
        sender = make_sender(h.conn.config, [].append, h.conn, today=lambda: SESSION)
        sender.send(limit_request())
        while h.conn.pump(0) is not None:
            pass
        self.assertEqual(h.conn.open_order_strays, 0, "the echo of the adapter's own placement counted as a stray")
        quiet_pumps = []
        resync = h.conn.resync_open_orders
        h.conn.resync_open_orders = lambda: (quiet_pumps.append(True), resync())[1]
        collect_broker_orders(h.conn)
        self.assertEqual(quiet_pumps, [], "the read after the adapter's own placement waited for quiet")
        _, contract, order = h.fake.placed[0]
        h.fake.callbacks.open_order(5, contract, h.fake.echoed(5, order), SimpleNamespace(status="Submitted"))
        h.conn.pump(0)
        self.assertEqual(h.conn.open_order_strays, 1)
        collect_broker_orders(h.conn)
        self.assertEqual(quiet_pumps, [True], "a row of an order not placed on this session was not a stray")

    def _placed_harness(self):
        h = paper_harness()
        collect_broker_orders(h.conn)
        h.fake.on_place = h.fake.accept
        sender = make_sender(h.conn.config, [].append, h.conn, today=lambda: SESSION)
        sender.send(limit_request())
        while h.conn.pump(0) is not None:
            pass
        return h

    def test_the_echo_of_a_placed_order_is_taken_once_and_a_later_row_for_it_is_a_stray(self):
        # The exemption is consumed by the first echo: a later row for the
        # same ID outside a list is a stray, so a read that missed the order
        # is not taken as whole (ADR 0019: a list not read in full is unread).
        h = self._placed_harness()
        self.assertEqual(h.conn.open_order_strays, 0)
        _, contract, order = h.fake.placed[0]
        h.fake.callbacks.open_order(7, contract, h.fake.echoed(7, order), SimpleNamespace(status="Submitted"))
        h.conn.pump(0)
        self.assertEqual(h.conn.open_order_strays, 1)
        quiet_pumps = []
        resync = h.conn.resync_open_orders
        h.conn.resync_open_orders = lambda: (quiet_pumps.append(True), resync())[1]
        collect_broker_orders(h.conn)
        self.assertEqual(quiet_pumps, [True])

    def test_a_row_for_a_placed_id_from_another_client_is_a_stray(self):
        h = paper_harness()
        collect_broker_orders(h.conn)
        h.fake.on_place = None
        sender = make_sender(h.conn.config, [].append, h.conn, today=lambda: SESSION)
        sender.send(limit_request())
        _, contract, order = h.fake.placed[0]
        foreign = h.fake.echoed(7, order)
        foreign.clientId = h.conn.config.client_id + 1
        h.fake.callbacks.open_order(7, contract, foreign, SimpleNamespace(status="Submitted"))
        h.conn.pump(0)
        self.assertEqual(h.conn.open_order_strays, 1)

    def test_an_echo_after_a_reconnect_is_a_stray_unless_the_order_is_placed_again(self):
        h = paper_harness()
        collect_broker_orders(h.conn)
        h.fake.on_place = None  # no echo yet: the placement is still unechoed at the drop
        sender = make_sender(h.conn.config, [].append, h.conn, today=lambda: SESSION)
        sender.send(limit_request())
        _, contract, order = h.fake.placed[0]
        h.fake.drop()
        h.conn.pump(0.01)
        h.conn.reconnect()
        before = h.conn.open_order_strays
        h.fake.callbacks.open_order(7, contract, h.fake.echoed(7, order), SimpleNamespace(status="Submitted"))
        h.conn.pump(0)
        self.assertEqual(h.conn.open_order_strays, before + 1)

    def test_paper_is_refused_unless_the_connection_is_the_paper_configuration(self):
        # The sender reads the mode and account from the connection it sends
        # on, so a paper configuration cannot lend its mode to another link.
        dry = Harness()
        dry.conn.connect()
        for conn in (None, dry.conn):
            with self.subTest(conn=conn), self.assertRaises(SendRefused):
                make_sender(config(mode="paper"), lambda r: None, conn)
        for cfg in (config(), config(mode="live", account_id="U0000001")):
            h = Harness(**cfg._asdict())
            with self.subTest(mode=cfg.mode), self.assertRaises(SendRefused):
                PaperSender(h.conn, lambda r: None)
        forged = Harness(mode="paper")
        forged.conn.config = forged.conn.config._replace(account_id="U0000001")
        with self.assertRaises(SendRefused):
            PaperSender(forged.conn, lambda r: None)

    def test_nothing_is_sent_before_reconciliation_or_without_the_link(self):
        h = paper_harness(reconciled=False)
        journal = []
        sender = paper_sender(h, journal)
        with self.assertRaises(NotReconciled):
            sender.check(limit_request())
        h.conn.mark_reconciled()
        h.fake.drop()
        h.conn.pump(0)
        with self.assertRaises(NotConnected):
            sender.check(limit_request())
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(journal, [])

    def test_a_link_down_is_reported_as_link_down_before_the_sending_switch_is_asked(self):
        # A link known to be down holds the input (NotConnected, NotReconciled);
        # the switch's refusal (SendRefused) would stop the adapter instead.
        dropped = paper_harness()
        dropped.fake.can_place = False
        dropped.fake.drop()
        dropped.conn.pump(0)
        # The socket closed underneath, before any callback said so.
        silent = paper_harness()
        silent.fake.can_place = False
        silent.fake.connected = False
        for name, h in (("dropped", dropped), ("socket closed", silent)):
            with self.subTest(name), self.assertRaises(NotConnected):
                paper_sender(h, []).check(limit_request())
        unreconciled = paper_harness(reconciled=False)
        unreconciled.fake.can_place = False
        with self.assertRaises(NotReconciled):
            paper_sender(unreconciled, []).check(limit_request())
        for h in (dropped, silent, unreconciled):
            self.assertEqual(h.fake.placed, [])

    def test_send_after_check_does_not_test_the_link_again(self):
        # The relay's check before its mark is the one link check before the
        # hand-over; the client's own socket test inside place_order is the
        # one after. A socket that closes between the relay's check and send
        # reaches place_order, which reports the hand-over as not confirmed,
        # rather than a NotConnected refusal that leaves the mark behind.
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal)
        sender.check(limit_request())
        h.fake.connected = False
        record = sender.send(limit_request())
        self.assertEqual(h.fake.called("place_order"), [("place_order", 7)])
        self.assertEqual(journal, [record])

    def test_send_still_refuses_a_request_out_of_shape(self):
        h = paper_harness()
        sender = paper_sender(h, [])
        for bad in (limit_request(account_id="DU0000002"), request(), limit_request(tif="GTC")):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                sender.send(bad)
        self.assertEqual(h.fake.placed, [])

    def test_a_request_the_paper_account_cannot_carry_is_refused_before_the_gateway(self):
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal)
        for bad in (limit_request(account_id="DU0000002"), limit_request(account_id="U0000001"), request(),
                    limit_request(quantity=Decimal("9.5")), limit_request(con_id=None), limit_request(tif="GTC"),
                    limit_request(tif="GTD")):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                sender.send(bad)
        h.conn.next_order_id = None
        with self.assertRaises(SendRefused):
            sender.check(limit_request())
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(journal, [])

    def test_a_gateway_that_raises_leaves_nothing_journalled_and_no_order_id_reused(self):
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal)
        h.fake.place_error = ConnectionError("synthetic socket failure")
        with self.assertRaises(ConnectionError):
            sender.send(limit_request())
        self.assertEqual(journal, [])
        # The ID may have reached IBKR, so it is never handed out again.
        self.assertEqual(h.conn.next_order_id, 8)

    def test_an_order_released_for_another_session_is_refused_before_any_order_id_is_used(self):
        # A DAY limit order is for the session it was released in (ADR 0026
        # point 2): released yesterday and asked again today, its limit and
        # the checks behind it are stale, so it is never sent.
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal, today=date(2026, 10, 17))
        for stale in (limit_request(), limit_request(session=None), limit_request(session="2026-10-17")):
            with self.subTest(session=stale.session), self.assertRaises(SendRefused):
                sender.check(stale)
        self.assertEqual(sender.check(limit_request(session=date(2026, 10, 17))).session, date(2026, 10, 17))
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(h.conn.next_order_id, 7)
        self.assertEqual(journal, [])

    def test_an_order_whose_session_is_over_is_refused_as_session_over(self):
        # The owner's decision of 2026-10-22: an order released for an
        # earlier session than today's is retired by the adapter itself, so
        # the sender names that case apart (SessionOver, still a SendRefused)
        # from a session it cannot read, or one not yet begun, which stop
        # for a person as before.
        h = paper_harness()
        sender = paper_sender(h, [], today=date(2026, 10, 17))
        with self.assertRaises(SessionOver) as over:
            sender.check(limit_request())
        self.assertIsInstance(over.exception, SendRefused)
        self.assertIn("2026-10-16", str(over.exception))
        self.assertNotIn("--never-sent", str(over.exception), "a session over is retired, never settled by hand")
        for unread in (limit_request(session=None), limit_request(session="2026-10-16"),
                       limit_request(session=date(2026, 10, 18))):
            with self.subTest(session=unread.session), self.assertRaises(SendRefused) as refused:
                sender.check(unread)
            self.assertNotIsInstance(refused.exception, SessionOver)
        self.assertEqual(h.fake.placed, [])

    def test_an_order_of_a_shape_ibkr_can_never_accept_is_refused_as_unsendable(self):
        # The owner's decision of 2026-10-22: a released order that is not a
        # limit order, is for a fraction of a share, or has no IBKR contract
        # ID can never be sent as it stands, so the adapter stops at it for
        # the owner's retire command; the sender names those shapes apart
        # (UnsendableOrder, still a ValueError) from its other refusals.
        h = paper_harness()
        sender = paper_sender(h, [])
        for bad in (request(), limit_request(quantity=Decimal("9.5")), limit_request(con_id=None),
                    limit_request(con_id=0)):
            with self.subTest(bad=bad):
                for step in (sender.check, sender.send):
                    with self.assertRaises(UnsendableOrder):
                        step(bad)
        for other in (limit_request(account_id="DU0000002"), limit_request(tif="GTC")):
            with self.subTest(other=other), self.assertRaises(ValueError) as refused:
                sender.check(other)
            self.assertNotIsInstance(refused.exception, UnsendableOrder)
        self.assertEqual(h.fake.placed, [])

    def test_a_client_that_cannot_place_orders_is_refused_before_any_order_id_is_used(self):
        # The real client places nothing until the paper-account test enables
        # it; the sender asks before the relay marks the order, so the
        # refusal leaves nothing to settle by hand.
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal)
        h.fake.can_place = False
        with self.assertRaises(SendRefused):
            sender.check(limit_request())
        self.assertEqual(h.fake.called("place_order"), [])
        self.assertEqual(h.conn.next_order_id, 7)
        self.assertEqual(journal, [])

    def test_an_account_the_session_does_not_manage_is_refused(self):
        # IBKR's managedAccounts names the accounts this login trades; an
        # order for an account outside it is never sent.
        h = paper_harness()
        journal = []
        sender = paper_sender(h, journal)
        h.conn.managed_accounts = frozenset({"DU0000002"})
        with self.assertRaises(SendRefused):
            sender.check(limit_request())
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(h.conn.next_order_id, 7)


class ReleaseToRequestTests(unittest.TestCase):
    def released(self, **overrides):
        values = {"account_id": ACCOUNT, "client_order_id": "tw-1", "instrument_id": "SPY", "side": "buy",
                  "quantity": 9, "limit_price": 401.5, "mode": "paper", "send": True}
        values.update(overrides)
        return values

    def test_a_paper_release_becomes_a_day_limit_order_for_its_contract(self):
        r = release_to_request(self.released(), "paper", {"SPY": 756733}, SESSION)
        self.assertEqual(r, OrderRequest(client_order_id="tw-1", account_id=ACCOUNT, con_id=756733, symbol="SPY",
                                         action="BUY", quantity=Decimal("9"), order_type="LMT",
                                         limit_price=Decimal("401.5"), tif="DAY", session=SESSION))
        self.assertEqual(release_to_request(self.released(side="sell"), "paper", {"SPY": 756733}, SESSION).action,
                         "SELL")

    def test_a_release_this_adapter_must_not_send_is_refused(self):
        for bad in (dict(mode="live"), dict(mode="dry-run", send=False), dict(send=False), dict(side="hold"),
                    dict(instrument_id="QQQ"), dict(quantity=0), dict(quantity=9.5), dict(quantity=True),
                    dict(limit_price=0), dict(limit_price=float("nan"))):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                release_to_request(self.released(**bad), "paper", {"SPY": 756733}, SESSION)

    def test_a_release_with_no_contract_or_a_fractional_quantity_is_unsendable(self):
        # The owner's decision of 2026-10-22: no IBKR contract ID, or a
        # fraction of a share, can never be sent as it stands.
        for bad in (dict(instrument_id="QQQ"), dict(quantity=9.5), dict(quantity=Decimal("9.5"))):
            with self.subTest(bad=bad), self.assertRaises(UnsendableOrder):
                release_to_request(self.released(**bad), "paper", {"SPY": 756733}, SESSION)
        for other in (dict(quantity=0), dict(quantity=True), dict(side="hold"), dict(send=False)):
            with self.subTest(other=other), self.assertRaises(ValueError) as refused:
                release_to_request(self.released(**other), "paper", {"SPY": 756733}, SESSION)
            self.assertNotIsInstance(refused.exception, UnsendableOrder)

    def test_a_release_with_no_session_is_refused(self):
        for session in (None, "2026-10-16"):
            with self.subTest(session=session), self.assertRaises(ValueError):
                release_to_request(self.released(), "paper", {"SPY": 756733}, session)


if __name__ == "__main__":
    unittest.main()
