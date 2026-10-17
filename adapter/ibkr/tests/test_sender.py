"""Dry-run journals an order and never reaches IBKR; live sending is disabled (AGENTS.md rule 5)."""
import sys
import unittest
from decimal import Decimal
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import ACCOUNT
from test_connection import Harness
from sender import OrderRequest, DryRunSender, LiveSendingDisabled, make_sender


def request(**overrides):
    values = dict(client_order_id="tag-1", account_id=ACCOUNT, con_id=756733, symbol="SPY",
                  action="BUY", quantity=Decimal("10"), order_type="MKT", limit_price=None,
                  tif="DAY")
    values.update(overrides)
    return OrderRequest(**values)


class SenderTests(unittest.TestCase):
    def test_dry_run_journals_and_never_calls_place_order(self):
        h = Harness()
        h.conn.connect()
        journal = []
        sender = make_sender("dry-run", journal.append)
        self.assertIsInstance(sender, DryRunSender)
        record = sender.send(request())
        self.assertEqual(journal, [record])
        self.assertFalse(record.send)
        self.assertEqual(record.request.client_order_id, "tag-1")
        self.assertEqual(h.fake.called("place_order"), [])

    def test_paper_and_live_are_disabled(self):
        for mode in ("paper", "live"):
            with self.subTest(mode=mode), self.assertRaises(LiveSendingDisabled):
                make_sender(mode, lambda r: None)

    def test_unknown_mode_is_refused(self):
        with self.assertRaises(ValueError):
            make_sender("yolo", lambda r: None)

    def test_malformed_requests_are_refused_before_the_journal(self):
        journal = []
        sender = DryRunSender(journal.append)
        for bad in (dict(client_order_id=""), dict(action="HOLD"), dict(quantity=Decimal("0")),
                    dict(quantity=10.0), dict(order_type="LMT", limit_price=None),
                    dict(order_type="MKT", limit_price=Decimal("1")), dict(account_id="")):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                sender.send(request(**bad))
        self.assertEqual(journal, [])


if __name__ == "__main__":
    unittest.main()
