"""Tests for blend.py: synthetic curves only, no market data."""
import json
import os
import tempfile
import unittest
from datetime import datetime, timezone

import blend


def _t(y, m, d):
    return datetime(y, m, d, 16, tzinfo=timezone.utc).timestamp()


class MonthEndTest(unittest.TestCase):
    def test_the_last_value_of_each_month_is_kept_whatever_the_input_order(self):
        pts = [[_t(2020, 1, 31), 110.0], [_t(2020, 1, 2), 100.0], [_t(2020, 2, 3), 120.0], [_t(2020, 2, 28), 0.0]]
        self.assertEqual(blend.month_ends(pts), {(2020, 1): 110.0, (2020, 2): 120.0})

    def test_a_missing_month_is_an_error(self):
        with self.assertRaises(ValueError):
            blend.growth({(2020, 1): 1.0, (2020, 3): 2.0}, blend.span((2020, 1), (2020, 3)))

    def test_the_span_crosses_a_year_end(self):
        self.assertEqual(blend.span((2019, 11), (2020, 2)), [(2019, 11), (2019, 12), (2020, 1), (2020, 2)])


class CombineTest(unittest.TestCase):
    def test_parts_are_never_rebalanced(self):
        # A doubles then stays; B halves then doubles back: rebalancing would differ.
        a, b = [1.0, 2.0, 2.0], [1.0, 0.5, 1.0]
        self.assertEqual(blend.combine([a, b], [1, 1]), [1.0, 1.25, 1.5])

    def test_weights_are_relative(self):
        self.assertEqual(blend.combine([[1.0, 2.0], [1.0, 1.0]], [2, 2]), blend.combine([[1.0, 2.0], [1.0, 1.0]], [1, 1]))


class MeasureTest(unittest.TestCase):
    def test_cagr_over_months(self):
        self.assertAlmostEqual(blend.cagr([1.0] + [None] * 23 + [1.21], 24), 0.10)

    def test_drawdown_is_from_the_running_peak(self):
        self.assertAlmostEqual(blend.max_drawdown([1.0, 2.0, 1.0, 1.5, 0.8]), 0.6)

    def test_correlation_of_opposite_returns_is_minus_one(self):
        self.assertAlmostEqual(blend.correlation([0.1, -0.1, 0.2], [-0.1, 0.1, -0.2]), -1.0)

    def test_months_behind_counts_the_longest_stretch_below_the_best_relative_level(self):
        bench = [1.0, 1.0, 1.0, 1.0, 1.0, 1.0]
        path = [1.0, 1.2, 1.1, 1.0, 1.3, 1.25]
        self.assertEqual(blend.longest_behind(path, bench), 2)

    def test_a_report_on_synthetic_curves(self):
        months = blend.span((2020, 1), (2021, 1))
        spy = {m: 1.0 + 0.01 * i for i, m in enumerate(months)}
        flat = {m: 1.0 for m in months}
        rows, corr = blend.report({"SPY": spy, "FLAT": flat}, [("half", {"SPY": 1, "FLAT": 1})], (2020, 1), (2021, 1))
        (label, c, dd, behind), = rows
        self.assertEqual(label, "half")
        self.assertAlmostEqual(c, 0.06)
        self.assertEqual((dd, behind), (0.0, 12))
        self.assertIn(("SPY", "FLAT"), corr)


class MainTest(unittest.TestCase):
    def test_main_reads_a_spec_and_prints_each_period(self):
        with tempfile.TemporaryDirectory() as d:
            pts = [[_t(2020, m, 28), 100.0 + m] for m in range(1, 13)]
            with open(os.path.join(d, "a.json"), "w") as f:
                json.dump(pts, f)
            spec = {"curves": {"A": os.path.join(d, "a.json")},
                    "periods": [["P", [2020, 1], [2020, 12], {"SPY": "A"}]],
                    "books": [["SPY held", {"SPY": 1}]]}
            with open(os.path.join(d, "spec.json"), "w") as f:
                json.dump(spec, f)
            blend.main(os.path.join(d, "spec.json"))


if __name__ == "__main__":
    unittest.main()
