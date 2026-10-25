"""An IBKR account number is masked whether its letters come before the digits or after DU.

A paper account number can contain a letter after DU (DUX1234567); a masking
pattern that wants digits straight after the prefix lets that number through
into pasted messages.
"""
import unittest

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
        text = "CANCELLED order SPY500 at 09:30 DAY"
        self.assertEqual(cli.mask_accounts(text, ""), text)
        self.assertEqual(flex.mask(text), text)


if __name__ == "__main__":
    unittest.main()
