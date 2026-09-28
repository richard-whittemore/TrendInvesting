"""Unit tests for rates.py, the 3-month T-bill rate loader and lookup.

Every CSV fixture here is written to a temp file inline; no real FRED data
is ever used (README.md, "No market data").

Run with `make research-test` from the repository root.
"""

import io
import os
import tempfile
import unittest
from contextlib import redirect_stderr
from datetime import date

import rates


def _write(directory, name, lines):
    path = os.path.join(directory, name)
    with open(path, "w") as handle:
        handle.write("\n".join(lines) + "\n")
    return path


class LoadRateSeriesTests(unittest.TestCase):
    def test_reads_the_fred_header_and_converts_percent_to_a_fraction(self):
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "TB3MS.csv", [
                "DATE,TB3MS",
                "1980-01-01,12.04",
                "1980-02-01,12.81",
            ])
            series = rates.load_rate_series(path)
            self.assertEqual(series, [
                rates.Rate(date(1980, 1, 1), 0.1204),
                rates.Rate(date(1980, 2, 1), 0.1281),
            ])

    def test_sorts_by_date_even_if_the_file_is_out_of_order(self):
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "r.csv", ["DATE,RATE", "1980-02-01,1.0", "1980-01-01,2.0"])
            series = rates.load_rate_series(path)
            self.assertEqual([r.date for r in series], [date(1980, 1, 1), date(1980, 2, 1)])

    def test_fred_missing_observation_placeholder_is_skipped_not_zeroed(self):
        # FRED's DTB3 marks a holiday or missing observation with "." --
        # that day must not become a rate of 0%.
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "DTB3.csv", ["DATE,DTB3", "1980-01-01,12.00", "1980-01-02,.",
                                                   "1980-01-03,12.10"])
            series = rates.load_rate_series(path)
            self.assertEqual([r.date for r in series], [date(1980, 1, 1), date(1980, 1, 3)])

    def test_the_exact_header_and_format_fred_now_downloads_tb3ms_as(self):
        # The owner's saved ~/Desktop/Trend_Investing/data/rates/TB3MS.csv
        # (README.md, "Rate source") has this exact shape: header
        # "observation_date,TB3MS", monthly rows dated the first of the
        # month, annual percent, and FRED's own "." for a missing month.
        # This fixture is synthetic -- three made-up rows, not the real
        # file, which is never read in a test.
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "TB3MS.csv", [
                "observation_date,TB3MS", "1934-01-01,0.72", "1934-02-01,.", "1934-03-01,0.29"])
            series = rates.load_rate_series(path)
            self.assertEqual(series, [
                rates.Rate(date(1934, 1, 1), 0.0072),
                rates.Rate(date(1934, 3, 1), 0.0029),
            ])

    def test_blank_lines_are_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "r.csv", ["DATE,RATE", "1980-01-01,1.0", "", "1980-01-02,1.1"])
            series = rates.load_rate_series(path)
            self.assertEqual(len(series), 2)


class RateCurveTests(unittest.TestCase):
    def test_rate_on_an_exact_date(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10), rates.Rate(date(1980, 2, 1), 0.12)])
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 1)), 0.10)
        self.assertAlmostEqual(curve.rate_on(date(1980, 2, 1)), 0.12)

    def test_a_missing_date_carries_the_last_known_rate_forward(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10), rates.Rate(date(1980, 2, 1), 0.12)])
        # 1980-01-15 has no row of its own: it must use January's rate,
        # not interpolate toward February's.
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 15)), 0.10)
        self.assertAlmostEqual(curve.rate_on(date(1980, 2, 15)), 0.12)

    def test_zero_before_the_first_available_rate_with_a_warning(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10)])
        err = io.StringIO()
        with redirect_stderr(err):
            value = curve.rate_on(date(1979, 12, 31))
        self.assertEqual(value, 0.0)
        self.assertIn("1979-12-31", err.getvalue())

    def test_the_before_first_warning_fires_only_once(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10)])
        err = io.StringIO()
        with redirect_stderr(err):
            curve.rate_on(date(1979, 1, 1))
            curve.rate_on(date(1979, 6, 1))
        self.assertEqual(err.getvalue().count("before the first available rate"), 1)

    def test_haircut_subtracts_a_spread_from_the_rate(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10)], haircut=0.02)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 1)), 0.08)

    def test_haircut_does_not_apply_to_the_zero_before_first_credit(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10)], haircut=0.02)
        with redirect_stderr(io.StringIO()):
            value = curve.rate_on(date(1979, 1, 1))
        self.assertEqual(value, 0.0)

    def test_accrued_fraction_over_a_known_ten_day_period_at_a_flat_rate(self):
        # actual/360, a flat 7.2% annual rate for 10 calendar days:
        # 10 x 0.072 / 360 = 0.002 exactly.
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.072)])
        fraction = curve.accrued_fraction(date(1980, 1, 1), date(1980, 1, 11))
        self.assertAlmostEqual(fraction, 10 * 0.072 / 360, places=12)

    def test_accrued_fraction_is_exclusive_of_the_start_date(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.072)])
        one_day = curve.accrued_fraction(date(1980, 1, 1), date(1980, 1, 2))
        self.assertAlmostEqual(one_day, 0.072 / 360, places=12)
        same_day = curve.accrued_fraction(date(1980, 1, 1), date(1980, 1, 1))
        self.assertEqual(same_day, 0.0)

    def test_accrued_fraction_uses_each_days_own_rate_across_a_rate_change(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10), rates.Rate(date(1980, 1, 5), 0.20)])
        # Days 2,3,4 at 10%; days 5,6 at 20% (5 days total, from day 1
        # exclusive to day 6 inclusive).
        fraction = curve.accrued_fraction(date(1980, 1, 1), date(1980, 1, 6))
        expected = 3 * 0.10 / 360 + 2 * 0.20 / 360
        self.assertAlmostEqual(fraction, expected, places=12)

    def test_accrued_fraction_is_zero_before_the_first_rate_with_one_warning(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 5), 0.10)])
        err = io.StringIO()
        with redirect_stderr(err):
            fraction = curve.accrued_fraction(date(1980, 1, 1), date(1980, 1, 4))
        self.assertEqual(fraction, 0.0)
        self.assertIn("before the first available rate", err.getvalue())

    def test_accrued_fraction_spans_the_first_rates_start(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 3), 0.10)])
        with redirect_stderr(io.StringIO()):
            # Days 1,2 are before the first rate (zero); days 3,4,5 at 10%.
            fraction = curve.accrued_fraction(date(1979, 12, 31), date(1980, 1, 5))
        self.assertAlmostEqual(fraction, 3 * 0.10 / 360, places=12)

    def test_a_haircut_larger_than_the_rate_floors_at_zero_not_negative(self):
        # A 5% haircut against a 1% rate must credit 0%, never -4%
        # (README.md, "Accrual"; paying the broker out of principal is not
        # modelled).
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.01)], haircut=0.05)
        self.assertEqual(curve.rate_on(date(1980, 1, 1)), 0.0)

    def test_the_zero_floor_does_not_affect_a_haircut_smaller_than_the_rate(self):
        curve = rates.RateCurve([rates.Rate(date(1980, 1, 1), 0.10)], haircut=0.02)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 1)), 0.08)


class LoadRateCurveTests(unittest.TestCase):
    def test_monthly_rows_are_shifted_one_month_so_a_day_uses_the_prior_months_value(self):
        # TB3MS dates a row the 1st of the month it AVERAGES -- that average
        # isn't knowable until the month is over, so using it from day one
        # of its own month is a mild look-ahead (README.md, "Accrual").
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "TB3MS.csv", [
                "observation_date,TB3MS", "1980-01-01,10.0", "1980-02-01,12.0", "1980-03-01,14.0"])
            curve = rates.load_rate_curve(path)
        # A day in February must use January's rate (10%), not February's
        # own, still-unknown 12% average.
        self.assertAlmostEqual(curve.rate_on(date(1980, 2, 15)), 0.10)
        self.assertAlmostEqual(curve.rate_on(date(1980, 3, 15)), 0.12)
        # January itself has no prior month in this fixture: zero credit,
        # with the before-first warning.
        with redirect_stderr(io.StringIO()) as err:
            value = curve.rate_on(date(1980, 1, 15))
        self.assertEqual(value, 0.0)
        self.assertIn("before the first available rate", err.getvalue())

    def test_daily_rows_are_used_as_is_not_shifted(self):
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "DTB3.csv", ["DATE,DTB3", "1980-01-02,10.0", "1980-01-03,10.5"])
            curve = rates.load_rate_curve(path)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 2)), 0.10)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 3)), 0.105)

    def test_a_single_monthly_shaped_row_is_left_unshifted(self):
        # One row dated the first of the month is ambiguous on its own
        # (a lone DTB3 observation looks the same); with nothing to compare
        # it to, it is used as loaded.
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "r.csv", ["DATE,RATE", "1980-01-01,10.0"])
            curve = rates.load_rate_curve(path)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 1)), 0.10)

    def test_haircut_is_forwarded_to_the_curve(self):
        with tempfile.TemporaryDirectory() as directory:
            path = _write(directory, "r.csv", ["DATE,RATE", "1980-01-02,10.0"])
            curve = rates.load_rate_curve(path, haircut=0.02)
        self.assertAlmostEqual(curve.rate_on(date(1980, 1, 2)), 0.08)


if __name__ == "__main__":
    unittest.main()
