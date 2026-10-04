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


class HoldingPeriodTest(unittest.TestCase):
    def test_the_anniversary_is_still_short_term(self):
        bought = td.datetime(2021, 2, 5, tzinfo=td.timezone.utc).timestamp()
        self.assertFalse(td.long_term(bought, td.datetime(2022, 2, 5, tzinfo=td.timezone.utc).timestamp()))
        self.assertTrue(td.long_term(bought, td.datetime(2022, 2, 6, tzinfo=td.timezone.utc).timestamp()))

    def test_a_leap_day_purchase(self):
        bought = td.datetime(2024, 2, 29, tzinfo=td.timezone.utc).timestamp()
        self.assertFalse(td.long_term(bought, td.datetime(2025, 2, 28, tzinfo=td.timezone.utc).timestamp()))
        self.assertTrue(td.long_term(bought, td.datetime(2025, 3, 1, tzinfo=td.timezone.utc).timestamp()))


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


class FinalSaleTest(unittest.TestCase):
    def test_each_open_lot_is_taxed_on_its_own_gain_and_holding_period(self):
        # Old lot: 900 shares at 100, worth 100 (no gain). Young lot: 100 shares at 100, worth 200.
        end = T0 + 800 * DAY
        fills = [(T0, "OLD", 900, 100.0, 0.0), (end - 30 * DAY, "NEW", 100, 100.0, 0.0)]
        run = {"fills": fills, "equity": [(T0, 100_000.0), (end, 110_000.0)],
               "end_prices": {"OLD": 100.0, "NEW": 200.0}}
        r = td.after_tax(run, 2000, st_rate=0.22, lt_rate=0.15, capital=100_000.0)
        # All 10,000 of gain is short-term: 2,200 of tax on the final sale.
        self.assertAlmostEqual(110_000 * (1 + r["after_sold"]) ** r["years"] / (1 + r["after"]) ** r["years"],
                               110_000 - 2_200, places=0)

    def test_a_single_symbol_run_is_valued_from_its_holdings_net_of_financing(self):
        fills = [(T0, "SPY", 1000, 100.0, 0.0)]
        run = {"fills": fills, "equity": [(T0, 100_000.0), (T0 + 800 * DAY, 140_000.0)], "financing": 10_000.0}
        prices = td.end_prices_for(run, td.realized_by_year(fills)[1], 140_000.0, 100_000.0)
        # Cash is 0 from the fills, -10,000 after financing: holdings are worth 150,000.
        self.assertAlmostEqual(prices["SPY"], 150.0)

    def test_dividends_are_taxed_yearly_and_not_again_on_the_final_sale(self):
        fills = [(T0, "SPY", 1000, 100.0, 0.0)]
        equity = [(T0, 100_000.0), (T0 + 364 * DAY, 100_000.0), (T0 + 800 * DAY, 100_000.0)]
        run = {"fills": fills, "equity": equity, "end_prices": {"SPY": 100.0}}
        r = td.after_tax(run, 2000, dividend_yield=0.02, dividend_rate=0.15, capital=100_000.0, dividend_base=1.0)
        self.assertLess(r["after"], r["pre"])
        self.assertGreaterEqual(r["after_sold"], r["after"] - 1e-12)   # no gain left to tax at the end

    def test_a_warm_up_year_loss_carries_into_the_first_measured_year(self):
        fills = [(T0 - 300 * DAY, "A", 100, 100.0, 0.0), (T0 - 200 * DAY, "A", -100, 50.0, 0.0),     # 1999: -5,000
                 (T0 + 10 * DAY, "B", 100, 100.0, 0.0), (T0 + 20 * DAY, "B", -100, 150.0, 0.0)]      # 2000: +5,000
        equity = [(T0 - 300 * DAY, 100_000.0), (T0, 95_000.0), (T0 + 364 * DAY, 100_000.0), (T0 + 800 * DAY, 100_000.0)]
        r = td.after_tax({"fills": fills, "equity": equity}, 2000, capital=100_000.0)
        # 3,000 deducted in 1999, 2,000 carried: 2000 pays 22% on 3,000 (660), not on 5,000.
        self.assertAlmostEqual(95_000 * (1 + r["after"]) ** r["years"], 100_000 * (1 - 660 / 100_000), places=4)

    def test_a_loss_in_a_year_without_an_equity_mark_still_carries(self):
        # The equity curve starts in 2000, but a 1999 trade realized -5,000.
        fills = [(T0 - 300 * DAY, "A", 100, 100.0, 0.0), (T0 - 200 * DAY, "A", -100, 50.0, 0.0),
                 (T0 + 10 * DAY, "B", 100, 100.0, 0.0), (T0 + 20 * DAY, "B", -100, 150.0, 0.0)]
        equity = [(T0, 95_000.0), (T0 + 364 * DAY, 100_000.0), (T0 + 800 * DAY, 100_000.0)]
        r = td.after_tax({"fills": fills, "equity": equity}, 2000, capital=100_000.0)
        self.assertAlmostEqual(95_000 * (1 + r["after"]) ** r["years"], 100_000 * (1 - 660 / 100_000), places=4)

    def test_a_measured_year_without_an_equity_mark_is_refused(self):
        # A 2001 gain, but the equity curve skips 2001: its tax could not be applied.
        fills = [(T0 + 400 * DAY, "A", 100, 100.0, 0.0), (T0 + 410 * DAY, "A", -100, 150.0, 0.0)]
        equity = [(T0, 100_000.0), (T0 + 364 * DAY, 100_000.0), (T0 + 800 * DAY, 105_000.0)]
        with self.assertRaises(SystemExit):
            td.after_tax({"fills": fills, "equity": equity}, 2000, capital=100_000.0)


if __name__ == "__main__":
    unittest.main()
