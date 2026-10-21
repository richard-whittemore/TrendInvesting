"""The book a reading after the close is compared with, found back through the chain of runs (book_basis).

ADR 0019 and ADR 0027, amended 2026-10-18: the latest account snapshot of the
chain or the latest starting book an approval states, whichever is later,
with every fill since; and, for the unexplained cash, the latest
reconciliation whose cash the engine explained or the starting book.
Synthetic figures only.
"""
import json
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from book_basis import cash_change, fills_since, holdings_after, previous_close
from relay import ResolveRefused
from engine_double import LoggedEngine, final_hash, run_approval
from fake_tws import ACCOUNT
from test_delivery import commit

FILL = {"account_id": ACCOUNT, "client_order_id": "tw-1", "fill_id": "f1", "instrument_id": "SPY", "side": "buy",
        "quantity": 9, "price": 401.25, "commission": 1.0, "filled_at": "2027-01-14T15:00:00Z"}


def snapshot(engine, ident, positions, cash):
    """A batch's account snapshot and the reconciliation of the same reading, as the adapter derives their IDs."""
    commit(engine, ident + ":account.reconciliation", kind="account.reconciliation", payload={
        "as_of": "2027-01-13T21:00:00Z", "total_cash": cash, "unreadable": []})
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

    # -- the runs before this one, read once per run --------------------------
    def chain(self):
        """a.log, the account's first run, with a snapshot and a fill; b.log, a fresh run approving it, with a fill."""
        first = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({}, 100.0)))
        snapshot(first, "b1", {"SPY": 2}, 90.0)
        commit(first, "fill", kind="execution.fill", payload=FILL)
        fresh = LoggedEngine(str(self.root / "b.log"), approval=run_approval(
            ACCOUNT, previous_log="a.log", previous_log_final_hash=final_hash(self.root / "a.log")))
        commit(fresh, "fill-2", kind="execution.fill", payload=dict(FILL, fill_id="f2"))
        return fresh

    def test_the_runs_before_this_one_are_read_once_and_the_basis_kept(self):
        fresh = self.chain()
        cache = str(self.root / "pending.json.basis")
        walked = previous_close(str(self.root / "b.log"), cache)
        # The earlier run's log is never read again for this run: the basis
        # its walk found is kept beside the adapter's files.
        (self.root / "a.log").unlink()
        self.assertEqual(previous_close(str(self.root / "b.log"), cache), walked)
        commit(fresh, "fill-3", kind="execution.fill", payload=dict(FILL, fill_id="f3"))
        daily, unexplained = previous_close(str(self.root / "b.log"), cache)
        self.assertEqual((daily.positions, daily.cash, len(daily.fills)), ({"SPY": 2}, Decimal("90.0"), 3),
                         "this run's own log is read every time; only the runs before it are kept")
        self.assertEqual((unexplained.cash, len(unexplained.fills)), (Decimal("100.0"), 3))

    def test_a_kept_basis_of_another_approval_is_walked_again(self):
        self.chain()
        cache = str(self.root / "pending.json.basis")
        previous_close(str(self.root / "b.log"), cache)
        LoggedEngine(str(self.root / "c.log"), approval=run_approval(
            ACCOUNT, previous_log="b.log", previous_log_final_hash=final_hash(self.root / "b.log")))
        daily, _ = previous_close(str(self.root / "c.log"), cache)
        self.assertEqual(len(daily.fills), 2, "a fresh run's chain is walked once for it, b.log's fill included")
        self.assertIn("a.log", daily.source)

    def test_a_kept_basis_that_does_not_read_is_walked_again(self):
        self.chain()
        cache = self.root / "pending.json.basis"
        cache.write_text("{not json")
        daily, _ = previous_close(str(self.root / "b.log"), str(cache))
        self.assertEqual(daily.positions, {"SPY": 2})

    def test_a_kept_basis_whose_fields_are_malformed_is_logged_walked_again_and_replaced(self):
        self.chain()
        cache = self.root / "pending.json.basis"
        walked = previous_close(str(self.root / "b.log"), str(cache))
        good = json.loads(cache.read_text())
        broken = {
            "a fill missing its fields": lambda kept: kept["daily"]["fills"].append({}),
            "a fill on no side": lambda kept: kept["daily"]["fills"].append(dict(FILL, side="hold")),
            "a fill of no shares": lambda kept: kept["daily"]["fills"].append(dict(FILL, quantity=0)),
            "a fill at a time that is no time": lambda kept: kept["daily"]["fills"].append(
                dict(FILL, filled_at="yesterday")),
            "a fill priced at nothing": lambda kept: kept["daily"]["fills"].append(dict(FILL, price=0)),
            "a fill with a negative commission": lambda kept: kept["daily"]["fills"].append(
                dict(FILL, commission=-1)),
            "a cash that is not a number": lambda kept: kept["daily"].update(cash="NaN"),
            "a cash that is infinite": lambda kept: kept["unexplained"].update(cash="Infinity"),
            "a cash that is not text": lambda kept: kept["daily"].update(cash=90),
            "a position held as true": lambda kept: kept["daily"].update(positions={"SPY": True}),
            "a time that is not text": lambda kept: kept["daily"].update(as_of=5),
            "a basis that is not an object": lambda kept: kept.update(daily=[]),
        }
        for name, spoil in broken.items():
            with self.subTest(name):
                kept = json.loads(json.dumps(good))
                spoil(kept)
                cache.write_text(json.dumps(kept))
                with self.assertLogs("ibkr", level="WARNING") as logged:
                    self.assertEqual(previous_close(str(self.root / "b.log"), str(cache)), walked,
                                     "a malformed kept file is walked again, never crashes a reading")
                self.assertIn(str(cache), "\n".join(logged.output), "the file discarded is named")
                self.assertEqual(json.loads(cache.read_text()), good, "and it is replaced")

    def test_a_missing_earlier_log_stops_naming_it(self):
        self.chain()
        (self.root / "a.log").unlink()
        with self.assertRaises(ResolveRefused) as caught:
            previous_close(str(self.root / "b.log"), str(self.root / "pending.json.basis"))
        self.assertIn("a.log", str(caught.exception))
        self.assertIn("the run before it", str(caught.exception))

    def test_a_starting_book_in_another_currency_is_never_compared(self):
        approval = run_approval(ACCOUNT, starting_book=({"SPY": 1}, 500.0))
        approval["starting_book"]["currency"] = "CAD"
        LoggedEngine(str(self.root / "a.log"), approval=approval)
        daily, unexplained = previous_close(str(self.root / "a.log"))
        self.assertIn("CAD", daily.refused)
        self.assertIn("USD", daily.refused)
        self.assertEqual(unexplained.refused, daily.refused)

    # -- a book's fills are those after its as-of time (internal/reconcile) --
    def test_a_fill_the_starting_book_already_shows_is_not_counted_again(self):
        # The owner checked IBKR, which already showed an execution the
        # adapter then replays after the approval (run_approval's approved_at
        # is 2026-10-17T14:00:00Z): it is in the book, never added to it.
        engine = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({"SPY": 9}, 500.0)))
        commit(engine, "fill", kind="execution.fill", payload=dict(FILL, filled_at="2026-10-17T13:59:59Z"))
        commit(engine, "fill-2", kind="execution.fill", payload=dict(FILL, fill_id="f2", filled_at="2026-10-17T14:00:00Z"))
        daily, unexplained = previous_close(str(self.root / "a.log"))
        self.assertEqual([f["fill_id"] for f in daily.fills], ["f2"])
        self.assertEqual([f["fill_id"] for f in unexplained.fills], ["f2"])

    def test_a_fill_before_the_snapshots_as_of_time_is_in_the_snapshot(self):
        engine = LoggedEngine(str(self.root / "a.log"), approval=run_approval(ACCOUNT, starting_book=({}, 100.0)))
        snapshot(engine, "b1", {"SPY": 2}, 90.0)  # as of 2027-01-13T21:00:00Z
        commit(engine, "fill", kind="execution.fill", payload=dict(FILL, filled_at="2027-01-13T20:59:59Z"))
        commit(engine, "fill-2", kind="execution.fill", payload=dict(FILL, fill_id="f2"))
        daily, _ = previous_close(str(self.root / "a.log"))
        self.assertEqual([f["fill_id"] for f in daily.fills], ["f2"])

    def test_fills_since_selects_by_the_books_and_the_readings_times(self):
        early, inside, late = (dict(FILL, fill_id=i, filled_at=t) for i, t in (
            ("early", "2027-01-13T20:59:59Z"), ("inside", "2027-01-13T21:00:00.400Z"), ("late", "2027-01-14T22:05:00Z")))
        at = datetime(2027, 1, 13, 21, tzinfo=timezone.utc)
        read = datetime(2027, 1, 14, 22, 5, tzinfo=timezone.utc)
        self.assertEqual([f["fill_id"] for f in fills_since([late, inside, early], at, read)], ["inside"])

    # -- an earlier run's log is read only when its record hash chain holds --
    def test_a_log_whose_record_hash_chain_is_broken_stops_naming_it(self):
        self.chain()
        a = self.root / "a.log"
        text = a.read_text(encoding="utf-8")
        # A record's schema version changed: its payload hash and the log's
        # final record hash still read, its record hash does not.
        tampered = text.replace('"type":"account.snapshot","envelope_version":1,"schema_version":3',
                                '"type":"account.snapshot","envelope_version":1,"schema_version":4', 1)
        self.assertNotEqual(tampered, text, "setup: the snapshot's record")
        a.write_text(tampered, encoding="utf-8")
        with self.assertRaises(ResolveRefused) as caught:
            previous_close(str(self.root / "b.log"))
        self.assertIn("a.log", str(caught.exception))
        self.assertIn("record hash chain", str(caught.exception))

    def test_fills_move_holdings_and_cash_as_ibkr_books_them(self):
        sell = dict(FILL, side="sell", quantity=2, price=10.005, commission=0.004)
        self.assertEqual(holdings_after({"SPY": 2}, [sell]), {})
        self.assertEqual(cash_change([FILL, sell]), Decimal("-3612.25") + Decimal("20.01") - Decimal("0.00"))


if __name__ == "__main__":
    unittest.main()
