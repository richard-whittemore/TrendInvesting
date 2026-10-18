"""The sending seam: dry-run never reaches IBKR, paper sends only to a paper account, live stays disabled.

AGENTS.md rule 5: no live-order behaviour before the paper-trading and
limited-live gates. Paper sending is bound to the configuration's mode and
account (IBKR paper account IDs begin "DU") and fails closed otherwise.
"""
import sys
import unittest
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import ACCOUNT
from test_connection import Harness, config
from connection import NotConnected
from sender import (OrderRequest, DryRunSender, LiveSendingDisabled, NotReconciled, PaperSender, SendRefused,
                    make_sender, release_to_request)


def request(**overrides):
    values = dict(client_order_id="tag-1", account_id=ACCOUNT, con_id=756733, symbol="SPY",
                  action="BUY", quantity=Decimal("10"), order_type="MKT", limit_price=None,
                  tif="DAY")
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


class SenderTests(unittest.TestCase):
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
        sender = make_sender(h.conn.config, journal.append, h.conn)
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
        sender = PaperSender(h.conn, journal.append)
        with self.assertRaises(NotReconciled):
            sender.send(limit_request())
        h.conn.mark_reconciled()
        h.fake.drop()
        h.conn.pump(0)
        with self.assertRaises(NotConnected):
            sender.send(limit_request())
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(journal, [])

    def test_a_request_the_paper_account_cannot_carry_is_refused_before_the_gateway(self):
        h = paper_harness()
        journal = []
        sender = PaperSender(h.conn, journal.append)
        for bad in (limit_request(account_id="DU0000002"), limit_request(account_id="U0000001"), request(),
                    limit_request(quantity=Decimal("9.5")), limit_request(con_id=None)):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                sender.send(bad)
        h.conn.next_order_id = None
        with self.assertRaises(SendRefused):
            sender.send(limit_request())
        self.assertEqual(h.fake.placed, [])
        self.assertEqual(journal, [])

    def test_a_gateway_that_raises_leaves_nothing_journalled_and_no_order_id_reused(self):
        h = paper_harness()
        journal = []
        sender = PaperSender(h.conn, journal.append)
        h.fake.place_error = ConnectionError("synthetic socket failure")
        with self.assertRaises(ConnectionError):
            sender.send(limit_request())
        self.assertEqual(journal, [])
        # The ID may have reached IBKR, so it is never handed out again.
        self.assertEqual(h.conn.next_order_id, 8)


class ReleaseToRequestTests(unittest.TestCase):
    def released(self, **overrides):
        values = {"account_id": ACCOUNT, "client_order_id": "tw-1", "instrument_id": "SPY", "side": "buy",
                  "quantity": 9, "limit_price": 401.5, "mode": "paper", "send": True}
        values.update(overrides)
        return values

    def test_a_paper_release_becomes_a_day_limit_order_for_its_contract(self):
        r = release_to_request(self.released(), "paper", {"SPY": 756733})
        self.assertEqual(r, OrderRequest(client_order_id="tw-1", account_id=ACCOUNT, con_id=756733, symbol="SPY",
                                         action="BUY", quantity=Decimal("9"), order_type="LMT",
                                         limit_price=Decimal("401.5"), tif="DAY"))
        self.assertEqual(release_to_request(self.released(side="sell"), "paper", {"SPY": 756733}).action, "SELL")

    def test_a_release_this_adapter_must_not_send_is_refused(self):
        for bad in (dict(mode="live"), dict(mode="dry-run", send=False), dict(send=False), dict(side="hold"),
                    dict(instrument_id="QQQ"), dict(quantity=0), dict(quantity=9.5), dict(quantity=True),
                    dict(limit_price=0), dict(limit_price=float("nan"))):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                release_to_request(self.released(**bad), "paper", {"SPY": 756733})


if __name__ == "__main__":
    unittest.main()
