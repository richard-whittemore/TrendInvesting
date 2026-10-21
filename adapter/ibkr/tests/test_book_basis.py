"""The book a reading after the close is compared with, found back through the chain of runs (book_basis).

ADR 0019 and ADR 0027, amended 2026-10-18: the latest account snapshot of the
chain or the latest starting book an approval states, whichever is later,
with every fill since; and, for the unexplained cash, the latest
reconciliation whose cash the engine explained or the starting book.
Synthetic figures only.
"""
import sys
import tempfile
import unittest
from decimal import Decimal
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from book_basis import cash_change, holdings_after, previous_close
from engine_double import LoggedEngine, final_hash, run_approval
from fake_tws import ACCOUNT
from test_delivery import commit

FILL = {"account_id": ACCOUNT, "client_order_id": "tw-1", "fill_id": "f1", "instrument_id": "SPY", "side": "buy",
        "quantity": 9, "price": 401.25, "commission": 1.0, "filled_at": "2027-01-14T15:00:00Z"}


def snapshot(engine, ident, positions, cash):
    """A batch's account snapshot and the reconciliation of the same reading, as the adapter derives their IDs."""
    commit(engine, ident + ":account.reconciliation", kind="account.reconciliation", payload={
        "total_cash": cash, "unreadable": []})
    commit(engine, ident + ":account.snapshot", kind="account.snapshot", version=3, payload={
        "as_of": "2027-01-13T21:00:00Z", "positions": [{"instrument_id": s, "quantity": q}
                                                        for s, q in sorted(positions.items())]})


class BookBasisTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.root = Path(self.dir.name)

    def test_a_first_runs_starting_book_is_the_basis_with_the_fills_since(self):
        engine = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({"SPY": 1}, 500.0)))
        commit(engine, "fill", kind="execution.fill", payload=FILL)
        daily, unexplained = previous_close(str(self.root / "a.log"))
        self.assertEqual((daily.positions, daily.cash, daily.fills, daily.own), ({"SPY": 1}, Decimal("500.0"), [FILL],
                                                                                 False))
        self.assertIn("starting book", daily.source)
        self.assertEqual((unexplained.cash, unexplained.fills), (Decimal("500.0"), [FILL]))

    def test_a_fresh_run_reads_back_to_the_earlier_runs_last_snapshot(self):
        first = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({}, 100.0)))
        snapshot(first, "b1", {"SPY": 2}, 90.0)
        commit(first, "fill", kind="execution.fill", payload=FILL)
        fresh = LoggedEngine(str(self.root / "b.log"), approval=run_approval(
            ACCOUNT, previous_log="a.log", previous_log_final_hash=final_hash(self.root / "a.log")))
        commit(fresh, "fill-2", kind="execution.fill", payload=dict(FILL, fill_id="f2"))
        daily, unexplained = previous_close(str(self.root / "b.log"))
        self.assertEqual((daily.positions, daily.cash, len(daily.fills), daily.own), ({"SPY": 2}, Decimal("90.0"), 2,
                                                                                     False))
        self.assertIn("a.log", daily.source)
        self.assertEqual((unexplained.cash, len(unexplained.fills)), (Decimal("100.0"), 2),
                         "a Boost book states no cash, so the owner's starting book is the last cash explained")

    def test_the_runs_own_snapshot_is_its_book(self):
        engine = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({}, 100.0)))
        snapshot(engine, "b1", {"SPY": 2}, 90.0)
        daily, _ = previous_close(str(self.root / "a.log"))
        self.assertTrue(daily.own)

    def test_a_reconciliation_the_engine_explained_resets_the_unexplained_cash(self):
        engine = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({}, 100.0)),
                              decisions={"b1:account.reconciliation": [{"type": "risk.reconciliation.result",
                                                                         "payload": {"classification": "matched",
                                                                                     "cash_compared": True}}]})
        snapshot(engine, "b1", {"SPY": 2}, 90.0)
        _, unexplained = previous_close(str(self.root / "a.log"))
        self.assertEqual((unexplained.cash, unexplained.own), (Decimal("90.0"), True))
        self.assertIn("explained", unexplained.source)

    def test_a_chain_with_no_starting_book_takes_the_daily_basis_for_the_unexplained_cash(self):
        engine = LoggedEngine(str(self.root / "a.log"))
        self.assertEqual(previous_close(str(self.root / "a.log")), (None, None))
        snapshot(engine, "b1", {"SPY": 2}, 90.0)
        daily, unexplained = previous_close(str(self.root / "a.log"))
        self.assertEqual((unexplained.cash, unexplained.fills, unexplained.source), (daily.cash, daily.fills,
                                                                                      daily.source))

    def test_a_snapshot_whose_cash_was_not_read_has_no_cash(self):
        engine = LoggedEngine(str(self.root / "a.log"))
        commit(engine, "b1:account.reconciliation", kind="account.reconciliation", payload={
            "total_cash": 0, "unreadable": ["total-cash"]})
        commit(engine, "b1:account.snapshot", kind="account.snapshot", version=3, payload={
            "as_of": "2027-01-13T21:00:00Z", "positions": []})
        daily, unexplained = previous_close(str(self.root / "a.log"))
        self.assertIsNone(daily.cash)
        self.assertIsNone(unexplained)

    def test_fills_move_holdings_and_cash_as_ibkr_books_them(self):
        sell = dict(FILL, side="sell", quantity=2, price=10.005, commission=0.004)
        self.assertEqual(holdings_after({"SPY": 2}, [sell]), {})
        self.assertEqual(cash_change([FILL, sell]), Decimal("-3612.25") + Decimal("20.01") - Decimal("0.00"))


if __name__ == "__main__":
    unittest.main()
