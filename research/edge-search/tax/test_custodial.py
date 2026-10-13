"""Tests for custodial.py on synthetic fills (no market data)."""
import unittest

import custodial as cu
import tax_drag as td

DAY = td.SECONDS_PER_DAY
T0 = 1136073600                     # 2006-01-01 UTC


class KiddieTaxTest(unittest.TestCase):
    def test_income_within_the_standard_deduction_is_untaxed(self):
        self.assertEqual(cu.kiddie_tax(2025, 1000.0, 350.0), 0.0)

    def test_the_middle_tier_is_taxed_at_the_childs_rates(self):
        # 2025: 1,350 deduction (ordinary first), then up to 2,700 at 10% / 0%.
        self.assertAlmostEqual(cu.kiddie_tax(2025, 2000.0, 0.0), 650.0 * 0.10)
        self.assertAlmostEqual(cu.kiddie_tax(2025, 1000.0, 1000.0), 0.0)
        # 2007: threshold 1,700. 300 above it, split 150 / 150 at the parents' rates;
        # 850 of long-term income in the middle tier at 2007's 5%.
        self.assertAlmostEqual(cu.kiddie_tax(2007, 1000.0, 1000.0), 150.0 * 0.22 + 150.0 * 0.15 + 850.0 * 0.05)

    def test_income_above_the_threshold_is_taxed_at_the_parents_rates_by_character(self):
        # 6,700 of income: 4,000 above 2,700, split 50/50 like the income.
        tax = cu.kiddie_tax(2025, 3350.0, 3350.0)
        above = 2000.0 * 0.22 + 2000.0 * 0.15
        middle_ord = (3350.0 - 1350.0) - 2000.0          # none left for the middle tier
        middle_pref = 3350.0 - 2000.0                    # taxed at 0%
        self.assertAlmostEqual(tax, above + max(0.0, middle_ord) * 0.10 + middle_pref * 0.0)

    def test_large_income_approaches_the_parents_flat_rates(self):
        tax = cu.kiddie_tax(2025, 1e6, 0.0)
        self.assertAlmostEqual(tax / 1e6, 0.22, places=2)

    def test_no_income_no_tax(self):
        self.assertEqual(cu.kiddie_tax(2025, 0.0, 0.0), 0.0)


class NetCapitalTest(unittest.TestCase):
    def test_gains_pass_through(self):
        self.assertEqual(cu.net_capital(100.0, 50.0, 0.0, 0.0), (100.0, 50.0, 0.0, 0.0, 0.0))

    def test_a_carryover_reduces_its_own_kind_first(self):
        self.assertEqual(cu.net_capital(100.0, 50.0, 30.0, 0.0), (70.0, 50.0, 0.0, 0.0, 0.0))

    def test_a_loss_of_one_kind_offsets_a_gain_of_the_other(self):
        self.assertEqual(cu.net_capital(-40.0, 100.0, 0.0, 0.0), (0.0, 60.0, 0.0, 0.0, 0.0))

    def test_a_net_loss_carries_only_the_losing_kinds_remainder(self):
        self.assertEqual(cu.net_capital(-100.0, 30.0, 0.0, 0.0), (0.0, 0.0, 70.0, 70.0, 0.0))
        self.assertEqual(cu.net_capital(-100.0, -30.0, 0.0, 0.0), (0.0, 0.0, 130.0, 100.0, 30.0))


def _run(gain_per_year, years=3, start=1_000_000.0):
    """Buys 1,000 shares a year at 1,000 and sells them 10 days later at
    1,000 + gain_per_year / 1,000 (all short-term); equity flat at ``start``."""
    fills, equity = [], []
    for y in range(years):
        t = T0 + int(y * 365.25 * DAY) + 100 * DAY
        fills += [[t, "A", 1000, 1000.0, 0.0], [t + 10 * DAY, "A", -1000, 1000.0 + gain_per_year / 1000, 0.0]]
        equity += [[t, start], [t + 200 * DAY, start]]
    return {"fills": fills, "equity": equity}


class CustodialTest(unittest.TestCase):
    def test_a_small_account_pays_nothing_and_a_large_one_pays_about_the_parents_rate(self):
        run = _run(gain_per_year=10_000.0)               # 1% of the $1M run each year
        small = cu.custodial(run, 2006, 50_000, "fifo", dividend_yield=0.0)
        self.assertEqual(small["tax_paid"], 0.0)          # $500 a year: under the deduction
        big = cu.custodial(run, 2006, 10_000_000, "fifo", dividend_yield=0.0)
        self.assertAlmostEqual(big["tax_paid"] / (3 * 100_000.0), 0.22, places=2)

    def test_dividends_are_taxed_yearly_as_preferential_income(self):
        run = _run(gain_per_year=0.0)
        r = cu.custodial(run, 2006, 1_000_000, "fifo", dividend_yield=0.02, dividend_base=1.0)
        # 20,000 a year of dividends: above the threshold at 15%, the middle tier at 0% or 5%.
        expect = sum((20_000.0 - cu.THRESHOLD[y]) * 0.15 + (cu.THRESHOLD[y] / 2) * cu.child_preferential(y)
                     for y in (2006, 2007, 2008))
        self.assertAlmostEqual(r["tax_paid"], expect, delta=500.0)  # later years shrink by the tax paid

    def test_a_loss_carries_forward_into_the_next_years_gain(self):
        run = _run(gain_per_year=10_000.0, years=2)
        run["fills"][1][3] = 1000.0 - 50.0             # year 1: a 50,000 short-term loss on $1M
        r = cu.custodial(run, 2006, 1_000_000, "fifo", dividend_yield=0.0)
        self.assertEqual(r["tax_paid"], 0.0)            # 10,000 gain in year 2 is under the carryover

    def test_hold_one_lot_buys_98_percent_once(self):
        run = {"equity": [[T0, 100.0], [T0 + DAY, 200.0], [T0 + 2 * DAY, 300.0]]}
        self.assertEqual(cu.hold_one_lot(run, T0 + DAY)["fills"], [[T0 + DAY, "SPY", 196.0, 1.0, 0.0]])


if __name__ == "__main__":
    unittest.main()
