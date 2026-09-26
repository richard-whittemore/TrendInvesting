"""Unit tests for fidelity/compare_fills.py's grouping and matching, on
synthetic fills. The tool itself reads a LEAN backtest and a Go journal,
which only a local LEAN run produces (README.md, "Checking fidelity against
the Go engine")."""

import os
import sys
import unittest
from datetime import date, datetime, timedelta, timezone

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "fidelity"))

import compare_fills  # noqa: E402

D1, D2 = date(2003, 5, 7), date(2003, 9, 25)


class CompareFillsTests(unittest.TestCase):
    def test_research_kind_reads_the_tag_prefix(self):
        self.assertEqual(compare_fills.research_kind("stop:AAPL"), "stop")
        self.assertEqual(compare_fills.research_kind("entry:AAPL"), "entry")
        self.assertEqual(compare_fills.research_kind("add-orphan-liquidate:AAPL"), "other")
        self.assertEqual(compare_fills.research_kind(None), "other")

    def test_one_exit_per_unit_matches_one_exit_for_every_unit(self):
        research = [(D2, "exit", 100, 0.36), (D2, "exit", 100, 0.36), (D2, "exit", 100, 0.36)]
        go = [(D2, "exit", 300, 0.36)]
        rows = compare_fills.compare(research, go, 0.002, 0.0)
        self.assertEqual([row[3] for row in rows], ["match"])

    def test_quantity_price_and_missing_groups_are_reported(self):
        research = [(D1, "entry", 505985, 0.31), (D2, "stop", 100, 0.40)]
        go = [(D1, "entry", 505960, 0.31), (D2, "exit", 100, 0.40)]
        verdicts = {row[0]: row[3] for row in compare_fills.compare(research, go, 0.002, 0.0)}
        self.assertEqual(verdicts[(D1, "entry")], "mismatch:quantity")
        self.assertEqual(verdicts[(D2, "stop")], "only-research")
        self.assertEqual(verdicts[(D2, "exit")], "only-go")

    def test_prices_within_tolerance_match(self):
        rows = compare_fills.compare([(D1, "add", 10, 1.0001)], [(D1, "add", 10, 1.0)], 0.002, 0.0)
        self.assertEqual(rows[0][3], "match")
        rows = compare_fills.compare([(D1, "add", 10, 1.01)], [(D1, "add", 10, 1.0)], 0.002, 0.0)
        self.assertEqual(rows[0][3], "mismatch:price")

    def test_measure_states_net_profit_cagr_and_drawdown(self):
        t0 = datetime(2003, 1, 1, tzinfo=timezone.utc)
        marks = [(t0, 100.0), (t0 + timedelta(days=100), 120.0), (t0 + timedelta(days=200), 90.0),
                 (t0 + timedelta(days=365.25), 110.0)]
        stats = compare_fills.measure(marks)
        self.assertEqual(stats["net profit"], "10.000%")
        self.assertEqual(stats["CAGR"], "10.000%")
        self.assertEqual(stats["max drawdown"], "25.000%")


if __name__ == "__main__":
    unittest.main()
