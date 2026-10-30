"""Fractional quantities are judged by value, not by the broker's spelling (ADR 0026, D7-F)."""
import unittest
from decimal import Decimal

from quantities import units


class UnitsByValueTest(unittest.TestCase):
    def test_trailing_zeros_are_the_same_value(self):
        self.assertEqual(units(Decimal("3.50000"), 10000), 35000)
        self.assertEqual(units(Decimal("0E-8"), 10000), 0)
        self.assertEqual(units(Decimal("1E+2"), 10000), 1000000)

    def test_a_fifth_decimal_is_refused(self):
        with self.assertRaisesRegex(ValueError, "four decimal"):
            units(Decimal("3.50001"), 10000)
