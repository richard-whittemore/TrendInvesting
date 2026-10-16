"""Tests for the forward (based) adjustment the bar encoder uses. Synthetic
data only."""
import datetime as dt
import math
import unittest

from marketdata import adjust
from marketdata.tests.test_marketdata import bar

D = dt.date
DAYS = [D(2026, 3, 2), D(2026, 3, 3), D(2026, 3, 4), D(2026, 3, 5), D(2026, 3, 6)]


def split(day, frm, to):
    return {"ticker": "A", "execution_date": day, "split_from": frm, "split_to": to}


def div(day, cash):
    return {"ticker": "A", "ex_dividend_date": day, "cash_amount": cash}


def factors(closes, splits=(), dividends=(), base=DAYS[0], sessions=DAYS):
    return adjust.forward_factors("A", sessions, dict(zip(sessions, closes)) if isinstance(closes, list) else closes,
                                  list(splits), list(dividends), base)


class ForwardFactorsTest(unittest.TestCase):
    def test_no_events_leaves_every_session_at_one(self):
        self.assertEqual(factors([10.0] * 5), {d: (1.0, 1.0) for d in DAYS})

    def test_a_split_scales_its_own_and_later_sessions_into_the_base_sessions_shares(self):
        got = factors([100.0, 100.0, 50.0, 51.0, 52.0], splits=[split("2026-03-04", 1, 2)])
        self.assertEqual([got[d] for d in DAYS], [(1.0, 1.0), (1.0, 1.0), (2.0, 2.0), (2.0, 2.0), (2.0, 2.0)])

    def test_a_dividend_scales_later_sessions_by_prior_close_over_prior_close_less_cash(self):
        got = factors([100.0, 100.0, 98.0, 98.0, 99.0], dividends=[div("2026-03-04", 2.0)])
        self.assertEqual(got[DAYS[1]], (1.0, 1.0))
        self.assertEqual(got[DAYS[2]], (1.0, 100.0 / 98.0))
        self.assertEqual(got[DAYS[4]], (1.0, 100.0 / 98.0))

    def test_returns_match_the_backward_adjustment_and_a_bar_is_never_restated(self):
        closes = [200.0, 100.0, 100.0, 90.0, 92.0]
        splits_, divs = [split("2026-03-03", 1, 2)], [div("2026-03-05", 10.0)]
        got = factors(closes, splits_, divs)
        forward = [c * got[d][1] for d, c in zip(DAYS, closes)]
        backward = [c for _, c in adjust.adjusted_closes([(d, [bar("A", c)]) for d, c in zip(DAYS, closes)],
                                                        "A", splits_, divs)]
        for i in range(1, 5):
            self.assertAlmostEqual(forward[i] / forward[i - 1], backward[i] / backward[i - 1], places=12)
        # A shorter history (as on an earlier day) gives the same figures for
        # the sessions both cover: a later event never restates a bar.
        early = factors(dict(zip(DAYS[:3], closes[:3])), splits_, divs, sessions=DAYS[:3])
        self.assertEqual(early, {d: got[d] for d in DAYS[:3]})

    def test_events_on_or_before_the_base_or_after_the_last_session_are_ignored(self):
        got = factors([10.0] * 5, splits=[split("2026-03-03", 1, 2), split("2026-04-01", 1, 2)],
                      dividends=[div("2026-03-02", 1.0)], base=DAYS[1])
        self.assertEqual(got, {d: (1.0, 1.0) for d in DAYS[1:]})

    def test_an_instrument_first_seen_after_the_base_is_based_at_its_first_bar(self):
        closes = {DAYS[2]: 10.0, DAYS[3]: 10.0, DAYS[4]: 9.0}
        got = factors(closes, dividends=[div("2026-03-04", 1.0), div("2026-03-06", 1.0)])
        self.assertEqual(got[DAYS[2]], (1.0, 1.0))
        self.assertEqual(got[DAYS[4]], (1.0, 10.0 / 9.0))

    def test_a_dividend_whose_prior_session_has_no_close_fails_closed(self):
        closes = {DAYS[0]: 10.0, DAYS[2]: 10.0, DAYS[3]: 10.0}
        with self.assertRaisesRegex(adjust.CannotAdjust, "session before"):
            factors(closes, dividends=[div("2026-03-04", 1.0)])

    def test_a_split_and_a_dividend_on_one_date_fail_closed(self):
        # The cash would be per post-split share and the prior close per
        # pre-split share; refused rather than guessed.
        with self.assertRaisesRegex(adjust.CannotAdjust, "same date"):
            factors([10.0] * 5, splits=[split("2026-03-04", 1, 2)], dividends=[div("2026-03-04", 0.1)])

    def test_two_splits_on_one_date_fail_closed(self):
        with self.assertRaisesRegex(adjust.CannotAdjust, "two splits"):
            factors([10.0] * 5, splits=[split("2026-03-04", 1, 2), split("2026-03-04", 1, 3)])

    def test_malformed_figures_fail_closed(self):
        cases = {
            "zero close": ([10.0, 0.0, 10.0, 10.0, 10.0], [], []),
            "NaN close": ([10.0, math.nan, 10.0, 10.0, 10.0], [], []),
            "zero split": ([10.0] * 5, [split("2026-03-04", 1, 0)], []),
            "NaN split": ([10.0] * 5, [split("2026-03-04", math.nan, 2)], []),
            "boolean split": ([10.0] * 5, [split("2026-03-04", True, 2)], []),
            "negative cash": ([10.0] * 5, [], [div("2026-03-04", -1.0)]),
            "NaN cash": ([10.0] * 5, [], [div("2026-03-04", math.nan)]),
            "cash at the prior close": ([10.0] * 5, [], [div("2026-03-04", 10.0)]),
            "bad date": ([10.0] * 5, [], [div("March", 1.0)]),
        }
        for name, (closes, s, d) in cases.items():
            with self.subTest(name), self.assertRaises(adjust.CannotAdjust):
                factors(closes, s, d)

    def test_a_base_that_is_not_a_loaded_session_fails_closed(self):
        with self.assertRaisesRegex(adjust.CannotAdjust, "base"):
            factors([10.0] * 5, base=D(2026, 3, 1))


if __name__ == "__main__":
    unittest.main()
