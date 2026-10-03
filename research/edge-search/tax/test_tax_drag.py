"""Tests for tax_drag.py on synthetic fills (no market data)."""
import unittest

import tax_drag as td

DAY = td.SECONDS_PER_DAY
T0 = 946684800                      # 2000-01-01 UTC


class RealizedTest(unittest.TestCase):
    def test_fifo_sells_the_oldest_lot_and_dates_the_holding_period(self):
        fills = [(T0, "A", 10, 100.0, 0.0), (T0 + 200 * DAY, "A", 10, 150.0, 0.0),
                 (T0 + 400 * DAY, "A", -10, 200.0, 0.0)]
        out, lots = td.realized_by_year(fills, "fifo")
        self.assertEqual(out, {2001: [0.0, 1000.0]})            # long-term, from the 100 lot
        self.assertEqual([lot[1] for lot in lots["A"]], [150.0])

    def test_lifo_sells_the_newest_lot(self):
        fills = [(T0, "A", 10, 100.0, 0.0), (T0 + 200 * DAY, "A", 10, 150.0, 0.0),
                 (T0 + 400 * DAY, "A", -10, 200.0, 0.0)]
        out, lots = td.realized_by_year(fills, "lifo")
        self.assertEqual(out, {2001: [500.0, 0.0]})             # short-term, from the 150 lot
        self.assertEqual([lot[1] for lot in lots["A"]], [100.0])

    def test_fees_raise_cost_and_cut_proceeds(self):
        fills = [(T0, "A", 10, 100.0, 10.0), (T0 + 10 * DAY, "A", -10, 110.0, 10.0)]
        out, _ = td.realized_by_year(fills)
        self.assertAlmostEqual(out[2000][0], 10 * (110 - 1) - 10 * (100 + 1))

    def test_a_sale_with_no_lots_is_reported(self):
        diag = {}
        td.realized_by_year([(T0, "OLD", 10, 100.0, 0.0), (T0 + DAY, "NEW", -10, 110.0, 0.0)], diag=diag)
        self.assertEqual(diag, {"unmatched": 10})

    def test_cash_at_end(self):
        self.assertAlmostEqual(td.cash_at_end([(T0, "A", 10, 100.0, 1.0), (T0, "A", -4, 120.0, 1.0)], 5000), 5000 - 1001 + 479)

    def test_a_partial_sale_spans_lots(self):
        fills = [(T0, "A", 5, 100.0, 0.0), (T0 + DAY, "A", 5, 120.0, 0.0), (T0 + 2 * DAY, "A", -7, 130.0, 0.0)]
        out, lots = td.realized_by_year(fills)
        self.assertAlmostEqual(out[2000][0], 5 * 30 + 2 * 10)
        self.assertEqual(lots["A"][0][0], 3)


class YearTaxTest(unittest.TestCase):
    def test_gains_of_both_kinds(self):
        self.assertEqual(td.year_tax(1000, 2000, 0, 0, 0.22, 0.15), (220 + 300, 0.0, 0.0))

    def test_a_long_term_loss_offsets_a_short_term_gain_first(self):
        tax, cs, cl = td.year_tax(1000, -400, 0, 0, 0.22, 0.15)
        self.assertAlmostEqual(tax, 600 * 0.22)

    def test_a_net_loss_deducts_3000_and_carries_the_rest(self):
        tax, cs, cl = td.year_tax(-5000, -1000, 0, 0, 0.22, 0.15)
        self.assertAlmostEqual(tax, -3000 * 0.22)
        self.assertEqual((cs, cl), (2000, 1000))

    def test_a_carryover_reduces_next_years_gain(self):
        tax, cs, cl = td.year_tax(3000, 0, 2000, 0, 0.22, 0.15)
        self.assertAlmostEqual(tax, 1000 * 0.22)
        self.assertEqual((cs, cl), (0.0, 0.0))


class AfterTaxTest(unittest.TestCase):
    def _run(self, sell_after_days):
        # Buy at 100, sell at 121 after the given days, flat afterwards; one year later the run ends.
        sell = T0 + sell_after_days * DAY
        fills = [(T0, "A", 10000, 100.0, 0.0), (sell, "A", -10000, 121.0, 0.0)]
        equity = [(T0, 1_000_000.0), (sell, 1_210_000.0), (T0 + 730 * DAY, 1_210_000.0)]
        return {"fills": fills, "equity": equity}

    def test_short_term_gains_cost_more_than_long_term(self):
        short = td.after_tax(self._run(100), 2000)
        long = td.after_tax(self._run(500), 2000)
        self.assertAlmostEqual(short["pre"], long["pre"])
        self.assertLess(short["after"], long["after"])
        # 210,000 short-term gain at 22%, paid at the end of 2000.
        self.assertAlmostEqual(1_000_000 * (1 + short["after"]) ** short["years"], 1_210_000 * (1 - 46_200 / 1_210_000), places=0)

    def test_an_ira_is_the_pre_tax_result(self):
        r = td.after_tax(self._run(100), 2000, st_rate=0.0, lt_rate=0.0)
        self.assertAlmostEqual(r["after"], r["pre"])


if __name__ == "__main__":
    unittest.main()
