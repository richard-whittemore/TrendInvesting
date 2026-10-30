"""Account masking and captured fractional callback formats (ADR 0026 D7-F).

An IBKR account number is masked whether its letters come before the digits or after DU.

A paper account number can contain a letter after DU (DUX1234567); a masking
pattern that wants digits straight after the prefix lets that number through
into pasted messages.
"""
import unittest
import json
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

from account import _add_position
from orders import execution_report

import cli
import flex


class AccountNumberMaskingTests(unittest.TestCase):
    def test_a_paper_number_with_a_letter_after_du_is_masked(self):
        for number in ("DUX1234567", "DUA0B1C2D3", "U1234567", "DU0000001"):
            with self.subTest(number=number):
                text = "refused {} at the gateway".format(number)
                self.assertNotIn(number, cli.mask_accounts(text, ""))
                self.assertNotIn(number, flex.mask(text))

    def test_ordinary_words_are_not_masked(self):
        text = "CANCELLED order SPY500 at 09:30 DAY DURATION DUPLICATE DUXXXXXXX"
        self.assertEqual(cli.mask_accounts(text, ""), text)
        self.assertEqual(flex.mask(text), text)


def captured_fractional(kind, protocol="protobuf"):
    """Synthetic callback attributes captured from the pinned decoder (lessons-2026-10.md L5)."""
    shapes = json.loads((Path(__file__).parent / "testdata" / "ibapi_shapes.json").read_text())

    def restore(value):
        if "type" in value:
            return Decimal(value["value"]) if value["type"] == "Decimal" else value["value"]
        return SimpleNamespace(**{key: restore(item) for key, item in value.items()})

    return restore(shapes[protocol][kind])


class FractionalCallbackShapeTests(unittest.TestCase):
    def test_fractional_position_uses_decoder_decimal_without_truncating(self):
        for protocol in ("protobuf", "text"):
            with self.subTest(protocol=protocol):
                row = captured_fractional("fractional_position", protocol)
                self.assertIsInstance(row.position, Decimal)
                self.assertEqual(row.position, Decimal("12.3456"))
                positions = {}
                _add_position(positions, row.account, row.account, row.contract, row.position, row.avgCost)
                self.assertEqual(positions[row.contract.conId].quantity, Decimal("12.3456"))

    def test_fractional_fill_uses_decoder_decimal_without_truncating(self):
        for protocol in ("protobuf", "text"):
            with self.subTest(protocol=protocol):
                row = captured_fractional("fractional_fill", protocol)
                self.assertIsInstance(row.execution.shares, Decimal)
                report = execution_report(row.contract, row.execution)
                self.assertEqual(report.shares, Decimal("3.5"))
                self.assertEqual(report.cum_qty, Decimal("3.5"))
                self.assertEqual(report.account_id, "synthetic")


if __name__ == "__main__":
    unittest.main()
