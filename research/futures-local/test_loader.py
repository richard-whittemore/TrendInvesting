"""Unit tests for loader.py: the tolerant Pinnacle CLC text/CSV reader.

Every fixture here is a few hand-written synthetic lines, never real market
data (AGENTS.md; research/futures-local/README.md, "No market data").

Run with `make research-test` from the repository root.
"""

import os
import tempfile
import unittest
from datetime import date

import loader


def _write(text, suffix=".txt"):
    handle = tempfile.NamedTemporaryFile("w", suffix=suffix, delete=False)
    handle.write(text)
    handle.close()
    return handle.name


class LoaderTestCase(unittest.TestCase):
    def load(self, text, **kwargs):
        path = _write(text)
        self.addCleanup(os.remove, path)
        return loader.load_series(path, **kwargs)

    def assertRejected(self, text, fragment, **kwargs):
        with self.assertRaises(loader.LoaderError) as caught:
            self.load(text, **kwargs)
        self.assertIn(fragment, str(caught.exception))
        return caught.exception


class DelimiterAndHeaderTests(LoaderTestCase):
    def test_headerless_comma_yyyymmdd_uses_the_positional_layout(self):
        bars = self.load("19800102,10.5,11.0,10.0,10.75,120,3400\n"
                         "19800103,10.75,11.25,10.5,11.0,130,3500\n")
        self.assertEqual(len(bars), 2)
        first = bars[0]
        self.assertEqual(first.date, date(1980, 1, 2))
        self.assertEqual((first.open, first.high, first.low, first.close), (10.5, 11.0, 10.0, 10.75))
        self.assertEqual((first.volume, first.open_interest), (120.0, 3400.0))

    def test_tab_delimited(self):
        bars = self.load("19800102\t1\t2\t0.5\t1.5\t0\t0\n")
        self.assertEqual(bars[0].close, 1.5)

    def test_whitespace_delimited(self):
        bars = self.load("19800102   1  2   0.5 1.5  7 8\n")
        self.assertEqual(bars[0].high, 2.0)
        self.assertEqual(bars[0].open_interest, 8.0)

    def test_semicolon_delimited(self):
        bars = self.load("19800102;1;2;0.5;1.5;7;8\n")
        self.assertEqual(bars[0].low, 0.5)

    def test_header_row_maps_columns_by_name_in_any_order(self):
        bars = self.load("Date,Settle,High,Low,Open,Total Volume,Open Interest\n"
                         "01/02/1980,10.75,11.0,10.0,10.5,120,3400\n")
        bar = bars[0]
        self.assertEqual((bar.open, bar.high, bar.low, bar.close), (10.5, 11.0, 10.0, 10.75))
        self.assertEqual((bar.volume, bar.open_interest), (120.0, 3400.0))

    def test_header_missing_a_required_column_is_rejected(self):
        self.assertRejected("Date,Open,High,Low,Volume\n19800102,1,2,0.5,3\n", "close")

    def test_explicit_column_layout_is_a_one_line_change(self):
        layout = ("date", "open", "high", "low", "close", "open_interest", "volume", None)
        bars = self.load("19800102,1,2,0.5,1.5,900,40,999\n", columns=layout)
        self.assertEqual((bars[0].volume, bars[0].open_interest), (40.0, 900.0))

    def test_blank_lines_are_skipped(self):
        bars = self.load("\n19800102,1,2,0.5,1.5,0,0\n\n19800103,1,2,0.5,1.5,0,0\n")
        self.assertEqual(len(bars), 2)

    def test_back_adjusted_negative_and_zero_prices_are_accepted(self):
        bars = self.load("19800102,-3.5,0,-4.25,-1.0,0,0\n")
        self.assertEqual((bars[0].open, bars[0].high, bars[0].low, bars[0].close), (-3.5, 0.0, -4.25, -1.0))

    def test_blank_volume_and_open_interest_are_none(self):
        bars = self.load("19800102,1,2,0.5,1.5,,\n")
        self.assertIsNone(bars[0].volume)
        self.assertIsNone(bars[0].open_interest)


class DateFormatTests(LoaderTestCase):
    def test_mm_dd_yyyy(self):
        self.assertEqual(self.load("12/31/1979,1,2,0.5,1.5,0,0\n")[0].date, date(1979, 12, 31))

    def test_single_digit_month_and_day(self):
        self.assertEqual(self.load("1/2/1980,1,2,0.5,1.5,0,0\n")[0].date, date(1980, 1, 2))

    def test_iso_dates(self):
        self.assertEqual(self.load("1980-01-02,1,2,0.5,1.5,0,0\n")[0].date, date(1980, 1, 2))

    def test_two_digit_year_century_rule(self):
        # CENTURY_PIVOT = 50: 50..99 -> 19xx (Pinnacle's CLC history starts
        # in 1969), 00..49 -> 20xx.
        bars = self.load("12/31/69,1,2,0.5,1.5,0,0\n"
                         "01/02/99,1,2,0.5,1.5,0,0\n"
                         "01/04/00,1,2,0.5,1.5,0,0\n"
                         "12/31/15,1,2,0.5,1.5,0,0\n")
        self.assertEqual([b.date for b in bars],
                         [date(1969, 12, 31), date(1999, 1, 2), date(2000, 1, 4), date(2015, 12, 31)])

    def test_century_pivot_is_configurable(self):
        bars = self.load("01/02/40,1,2,0.5,1.5,0,0\n", century_pivot=30)
        self.assertEqual(bars[0].date, date(1940, 1, 2))

    def test_mixed_date_formats_are_rejected(self):
        self.assertRejected("19800102,1,2,0.5,1.5,0,0\n01/03/1980,1,2,0.5,1.5,0,0\n", "line 2")

    def test_day_month_order_is_rejected_not_guessed(self):
        self.assertRejected("31/12/1979,1,2,0.5,1.5,0,0\n", "month")

    def test_ambiguous_six_digit_date_is_rejected(self):
        self.assertRejected("800102,1,2,0.5,1.5,0,0\n", "date")

    def test_impossible_calendar_date_is_rejected(self):
        self.assertRejected("19800230,1,2,0.5,1.5,0,0\n", "line 1")


class BadRowTests(LoaderTestCase):
    def test_non_monotonic_dates_are_rejected(self):
        error = self.assertRejected("19800103,1,2,0.5,1.5,0,0\n19800102,1,2,0.5,1.5,0,0\n", "monotonic")
        self.assertIn("line 2", str(error))

    def test_duplicate_dates_are_rejected(self):
        self.assertRejected("19800102,1,2,0.5,1.5,0,0\n19800102,1,2,0.5,1.5,0,0\n", "monotonic")

    def test_high_below_low_is_rejected(self):
        self.assertRejected("19800102,1,0.5,2,1.5,0,0\n", "high")

    def test_open_outside_the_range_is_rejected(self):
        self.assertRejected("19800102,3,2,0.5,1.5,0,0\n", "open")

    def test_close_outside_the_range_is_rejected(self):
        self.assertRejected("19800102,1,2,0.5,2.5,0,0\n", "close")

    def test_missing_field_is_rejected(self):
        self.assertRejected("19800102,1,2,0.5,1.5,0,0\n19800103,1,2,0.5\n", "line 2")

    def test_blank_price_field_is_rejected(self):
        self.assertRejected("19800102,1,,0.5,1.5,0,0\n", "high")

    def test_non_numeric_price_is_rejected(self):
        self.assertRejected("19800102,1,abc,0.5,1.5,0,0\n", "high")

    def test_non_finite_price_is_rejected(self):
        self.assertRejected("19800102,1,nan,0.5,1.5,0,0\n", "high")

    def test_empty_file_is_rejected(self):
        self.assertRejected("\n\n", "no data")

    def test_error_names_the_file(self):
        error = self.assertRejected("19800102,1,0.5,2,1.5,0,0\n", ".txt")
        self.assertTrue(error.path.endswith(".txt"))


class EndDateTests(LoaderTestCase):
    """``load_series(end=...)``: rows dated after ``end`` are neither
    validated nor returned. Pinnacle's real files carry corrupt rows long
    after the 2015 in-sample end (a high below the low, a close outside the
    range); they must not stop an in-sample run, and the held-out period
    is never read at all (README.md, "Data problems found")."""

    def test_a_corrupt_row_after_the_end_is_ignored(self):
        bars = self.load("20151230,1,2,0.5,1.5,0,0\n"
                         "20151231,1,2,0.5,1.5,0,0\n"
                         "20160104,1,0.5,2,1.5,0,0\n"      # high below low
                         "20160105,1,2,0.5,9.5,0,0\n",     # close outside the range
                         end=date(2015, 12, 31))
        self.assertEqual([bar.date for bar in bars], [date(2015, 12, 30), date(2015, 12, 31)])

    def test_a_corrupt_row_on_or_before_the_end_still_raises(self):
        self.assertRejected("20151230,1,2,0.5,1.5,0,0\n"
                            "20151231,1,0.5,2,1.5,0,0\n", "high", end=date(2015, 12, 31))

    def test_without_an_end_every_row_is_validated(self):
        self.assertRejected("20151231,1,2,0.5,1.5,0,0\n"
                            "20160104,1,0.5,2,1.5,0,0\n", "line 2")


class PriceScaleTests(unittest.TestCase):
    """classify_price_scale/check_series_scale on synthetic settle prices
    only (never real Pinnacle data, per AGENTS.md and README.md's "No
    market data"). The assumed unit is now Pinnacle's own, confirmed
    against the real files (markets.py): cents for silver, copper, heating
    oil and gasoline; currencies scaled up (the yen x 10,000, the others x
    100). The alternate is the exchange-quote unit a mis-scaled file would
    show instead. The check runs on the non-adjusted (``_NON``) series,
    whose every settle is a real price."""

    def test_yen_at_pinnacles_scale(self):
        # USDJPY ~110 -> $0.0091 per yen, stored x 10,000 as 90.91.
        self.assertEqual(loader.classify_price_scale("JY", 90.91), "assumed")

    def test_yen_at_the_exchange_dollars_per_yen_scale(self):
        self.assertEqual(loader.classify_price_scale("JY", 0.0091), "alternate")

    def test_silver_in_cents_per_ounce(self):
        self.assertEqual(loader.classify_price_scale("SI", 2450.0), "assumed")

    def test_silver_in_dollars_per_ounce_is_the_alternate(self):
        self.assertEqual(loader.classify_price_scale("SI", 24.50), "alternate")

    def test_copper_in_cents_per_pound(self):
        self.assertEqual(loader.classify_price_scale("HG", 350.0), "assumed")
        self.assertEqual(loader.classify_price_scale("HG", 3.50), "alternate")

    def test_heating_oil_and_gasoline_in_cents_per_gallon(self):
        for symbol in ("HO", "HU"):
            self.assertEqual(loader.classify_price_scale(symbol, 210.0), "assumed")
            self.assertEqual(loader.classify_price_scale(symbol, 2.10), "alternate")

    def test_swiss_franc_in_cents_per_franc(self):
        self.assertEqual(loader.classify_price_scale("SF", 100.5), "assumed")
        self.assertEqual(loader.classify_price_scale("SF", 1.005), "alternate")

    def test_treasury_note_has_only_an_assumed_range_no_alternate(self):
        self.assertEqual(loader.classify_price_scale("TY", 95.0), "assumed")
        self.assertIsNone(loader.PRICE_SCALE_HINTS["TY"][1])

    def test_every_traded_market_has_a_hint(self):
        import markets
        for symbol, market in markets.MARKETS.items():
            if market.excluded is None:
                self.assertIn(symbol, loader.PRICE_SCALE_HINTS)

    def test_a_price_outside_every_hinted_range_is_unknown(self):
        self.assertEqual(loader.classify_price_scale("SI", 999999.0), "unknown")

    def test_a_symbol_with_no_hint_returns_none(self):
        self.assertIsNone(loader.classify_price_scale("XX", 1800.0))

    def test_check_series_scale_uses_the_last_bars_close(self):
        bars = [
            loader.Bar(date(2015, 12, 30), 1.0, 1.0, 1.0, 1.0, None, None),
            loader.Bar(date(2015, 12, 31), 2400.0, 2460.0, 2390.0, 2450.0, None, None),
        ]
        self.assertEqual(loader.check_series_scale("SI", bars), "assumed")

    def test_check_series_scale_on_an_empty_series_is_none(self):
        self.assertIsNone(loader.check_series_scale("SI", []))


class FindMarketFileTests(unittest.TestCase):
    def test_finds_a_stem_case_insensitively_across_extensions(self):
        with tempfile.TemporaryDirectory() as directory:
            open(os.path.join(directory, "GC_B.TXT"), "w").close()
            open(os.path.join(directory, "SI_B.CSV"), "w").close()
            self.assertEqual(os.path.basename(loader.find_market_file(directory, "gc_b")), "GC_B.TXT")
            self.assertIsNone(loader.find_market_file(directory, "CL_B"))

    def test_two_candidates_for_one_stem_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            open(os.path.join(directory, "GC.TXT"), "w").close()
            open(os.path.join(directory, "GC.CSV"), "w").close()
            with self.assertRaises(loader.LoaderError):
                loader.find_market_file(directory, "GC")


if __name__ == "__main__":
    unittest.main()
