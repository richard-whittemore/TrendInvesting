"""Unit tests for markets.py: the integrity of Faith's portfolio table.

Run with `make research-test` from the repository root.
"""

import unittest
from datetime import date

import markets


class MarketTableTests(unittest.TestCase):
    def test_faiths_softs_are_in_the_portfolio(self):
        # The Turtle Rules p.10-11: coffee, cocoa, sugar and cotton.
        for symbol in ("SB", "KC", "CC", "CT"):
            self.assertIn(symbol, markets.MARKETS)
            self.assertEqual(markets.MARKETS[symbol].loosely_group, "softs")

    def test_every_market_is_complete(self):
        for symbol, market in markets.MARKETS.items():
            with self.subTest(symbol=symbol):
                self.assertEqual(market.symbol, symbol)
                self.assertTrue(market.file_stem)
                self.assertTrue(market.closely_group)
                self.assertTrue(market.loosely_group)
                self.assertGreater(market.tick_size, 0)
                self.assertGreater(markets.dollars_per_point(market, date(1985, 1, 1)), 0)
                self.assertTrue(market.roll_months)
                for month in market.roll_months:
                    self.assertIn(month, range(1, 13))
                self.assertIn(market.roll_day, range(1, 29))

    def test_correlation_groups_match_the_qc_version_where_both_list_a_market(self):
        # research/qc-cloud-futures/main.py's CORRELATION_GROUPS, keyed by
        # CME/ICE root ticker; markets.py names the same market's ticker.
        shared = {"GC": ("metals-precious", "metals"), "SI": ("metals-precious", "metals"),
                  "HG": ("metals-base", "metals"), "CL": ("energy-petroleum", "energy"),
                  "HO": ("energy-petroleum", "energy"), "SB": ("softs", "softs"),
                  "KC": ("softs", "softs"), "CC": ("softs", "softs"), "CT": ("softs", "softs"),
                  "US": ("rates", "rates"), "TY": ("rates", "rates"),
                  "SF": ("currencies-europe", "currencies"), "EC": ("currencies-europe", "currencies"),
                  "JY": ("currencies-jpy", "currencies"), "BP": ("currencies-gbp", "currencies"),
                  "CD": ("currencies-cad", "currencies")}
        for symbol, groups in shared.items():
            market = markets.MARKETS[symbol]
            self.assertEqual((market.closely_group, market.loosely_group), groups, symbol)

    def test_a_multiplier_schedule_changes_on_its_effective_date(self):
        # The CME halved the S&P 500 contract from $500 to $250 a point in
        # November 1997.
        sp = markets.MARKETS["SP"]
        self.assertEqual(markets.dollars_per_point(sp, date(1997, 10, 31)), 500.0)
        self.assertEqual(markets.dollars_per_point(sp, date(1997, 11, 3)), 250.0)

    def test_tradable_window(self):
        dm = markets.MARKETS["DM"]
        self.assertTrue(markets.tradable(dm, date(1998, 12, 31)))
        self.assertFalse(markets.tradable(dm, date(1999, 1, 4)))
        self.assertTrue(markets.tradable(markets.MARKETS["GC"], date(1980, 1, 2)))

    def test_to_verify_list_names_real_fields(self):
        allowed = {"file_stem", "multiplier", "tick_size", "roll_months", "price_units", "trade_window"}
        for market in markets.MARKETS.values():
            self.assertTrue(set(market.to_verify) <= allowed, market.symbol)


if __name__ == "__main__":
    unittest.main()
