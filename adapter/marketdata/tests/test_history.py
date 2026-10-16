"""Tests for the bar history, corporate-action recording and adjustment.
Synthetic data only."""
import datetime as dt
import json
import os
import tempfile
import unittest

from marketdata import adjust, history, massive, record, steps
from marketdata.tests.test_marketdata import LATER, NOW, bar, grouped, make_client, ny

D = dt.date


def put(directory, day, *rows):
    record.write(directory, {"kind": "grouped", "source": "massive", "session": day.isoformat(),
                             "fetched_at": "x", "adjusted": False, "bars": list(rows)})


class BackfillTest(unittest.TestCase):
    def test_records_one_request_per_session_and_skips_existing(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, D(2026, 3, 26), bar("A"))
            client, opener, _ = make_client(grouped(bar("A", 11)), grouped(bar("A", 12)))
            got = history.backfill(client, d, asof=D(2026, 3, 27), sessions=3, now=lambda: NOW)
            self.assertEqual(len(opener.requests), 2)        # 03-27 and 03-25; 03-26 was on disk
            self.assertEqual(got["fetched"], 2)
            self.assertEqual(sorted(os.listdir(d)), ["grouped_2026-03-25.json", "grouped_2026-03-26.json",
                                                     "grouped_2026-03-27.json"])

    def test_a_calendar_holiday_costs_no_request_and_leaves_no_file(self):
        with tempfile.TemporaryDirectory() as d:
            client, opener, _ = make_client(grouped(bar("A")), grouped(bar("A")))
            history.backfill(client, d, asof=D(2026, 4, 3), sessions=2, now=lambda: LATER)   # Good Friday
            self.assertEqual(len(opener.requests), 2)
            self.assertFalse(os.path.exists(os.path.join(d, "grouped_2026-04-03.json")))

    def test_an_empty_response_on_a_trading_day_records_nothing_and_is_retried_next_run(self):
        with tempfile.TemporaryDirectory() as d:
            client, _, _ = make_client(grouped(bar("A")), {"status": "OK", "resultsCount": 0})
            with self.assertRaises(history.MissingData):
                history.backfill(client, d, asof=D(2026, 3, 27), sessions=3, now=lambda: NOW)
            self.assertEqual(os.listdir(d), ["grouped_2026-03-27.json"])   # 03-26 not recorded
            client, opener, _ = make_client(grouped(bar("A")), grouped(bar("A")))
            history.backfill(client, d, asof=D(2026, 3, 27), sessions=3, now=lambda: NOW)
            self.assertIn("2026-03-26", opener.requests[0].full_url)

    def test_a_rerun_after_an_interruption_never_overwrites(self):
        with tempfile.TemporaryDirectory() as d:
            client, _, _ = make_client(grouped(bar("A", 1)), 403)
            with self.assertRaises(massive.MarketDataError):
                history.backfill(client, d, asof=D(2026, 3, 27), sessions=3, now=lambda: NOW)
            before = record.read(os.path.join(d, "grouped_2026-03-27.json"))
            client, opener, _ = make_client(grouped(bar("A", 2)), grouped(bar("A", 3)))
            history.backfill(client, d, asof=D(2026, 3, 27), sessions=3, now=lambda: NOW)
            self.assertEqual(record.read(os.path.join(d, "grouped_2026-03-27.json")), before)
            self.assertEqual(len(opener.requests), 2)


class LoadTest(unittest.TestCase):
    def test_load_returns_sessions_in_order_skipping_calendar_holidays(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, D(2026, 4, 2), bar("A", 2))
            put(d, D(2026, 4, 6), bar("A", 3))
            h = history.load(d, D(2026, 4, 2), D(2026, 4, 6))   # Good Friday and the weekend have no file
            self.assertEqual([s for s, _ in h], [D(2026, 4, 2), D(2026, 4, 6)])

    def test_a_recording_with_no_bars_on_a_trading_day_is_a_gap(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, D(2026, 4, 2))
            with self.assertRaises(history.Gap):
                history.load(d, D(2026, 4, 2), D(2026, 4, 2))

    def test_a_missing_weekday_is_a_gap(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, D(2026, 4, 1), bar("A"))
            put(d, D(2026, 4, 6), bar("A"))
            with self.assertRaises(history.Gap) as cm:
                history.load(d, D(2026, 4, 1), D(2026, 4, 6))
            self.assertIn("2026-04-02", str(cm.exception))


class ActionsTest(unittest.TestCase):
    def test_splits_and_dividends_are_fetched_by_date_and_paginated_and_recorded(self):
        with tempfile.TemporaryDirectory() as d:
            sp = {"status": "OK", "results": [{"ticker": "A", "execution_date": "2026-03-10", "split_from": 1, "split_to": 2}],
                  "next_url": "https://api.massive.com/v3/reference/splits?cursor=2"}
            sp2 = {"status": "OK", "results": [{"ticker": "B", "execution_date": "2026-03-11", "split_from": 10, "split_to": 1}]}
            dv = {"status": "OK", "results": [{"ticker": "A", "ex_dividend_date": "2026-03-12", "cash_amount": 0.5}]}
            client, opener, _ = make_client(sp, sp2, dv)
            paths = history.record_actions(client, D(2026, 3, 1), D(2026, 3, 27), d, now=lambda: NOW)
            self.assertEqual(len(paths), 2)
            self.assertIn("execution_date.gte=2026-03-01", opener.requests[0].full_url)
            self.assertIn("ex_dividend_date.lte=2026-03-27", opener.requests[2].full_url)
            self.assertEqual(len(record.read(paths[0])["rows"]), 2)
            with self.assertRaises(record.AlreadyRecorded):
                history.record_actions(make_client(sp, sp2, dv)[0], D(2026, 3, 1), D(2026, 3, 27), d, now=lambda: NOW)

    def test_load_actions_merges_recordings(self):
        with tempfile.TemporaryDirectory() as d:
            for kind, rows, a, b in (("splits", [{"ticker": "A"}], "2026-01-01", "2026-02-01"),
                                     ("splits", [{"ticker": "B"}], "2026-02-02", "2026-03-01"),
                                     ("dividends", [{"ticker": "C"}], "2026-01-01", "2026-03-01")):
                write_window(d, kind, a, b, rows)
            got = history.load_actions(d, "splits", D(2026, 1, 1), D(2026, 3, 1))
            self.assertEqual([r["ticker"] for r in got], ["A", "B"])

    def test_an_action_in_overlapping_windows_counts_once(self):
        split = {"id": "S1", "ticker": "A", "execution_date": "2026-03-04", "split_from": 1, "split_to": 2}
        div = {"id": "D1", "ticker": "A", "ex_dividend_date": "2026-03-05", "cash_amount": 1.0}
        with tempfile.TemporaryDirectory() as d:
            write_window(d, "splits", "2026-01-01", "2026-03-20", [split])
            write_window(d, "splits", "2026-01-02", "2026-03-23", [dict(split)])
            write_window(d, "dividends", "2026-01-01", "2026-03-20", [div])
            write_window(d, "dividends", "2026-01-02", "2026-03-23", [dict(div)])
            splits = history.load_actions(d, "splits", D(2026, 3, 2), D(2026, 3, 23))
            divs = history.load_actions(d, "dividends", D(2026, 3, 2), D(2026, 3, 23))
            self.assertEqual(splits, [split])
            self.assertEqual(divs, [div])
            h = hist(*zip(DAYS, [100.0, 100.0, 50.0, 50.0]))
            out = [c for _, c in adjust.adjusted_closes(h, "A", splits, divs)]
            self.assertEqual([round(c, 9) for c in out], [49.0, 49.0, 49.0, 50.0])   # 0.5 once, 0.98 once

    def test_an_identical_row_without_an_id_counts_once(self):
        row = {"ticker": "A", "execution_date": "2026-03-04", "split_from": 1, "split_to": 2}
        with tempfile.TemporaryDirectory() as d:
            write_window(d, "splits", "2026-01-01", "2026-03-20", [row])
            write_window(d, "splits", "2026-01-02", "2026-03-23", [dict(row)])
            self.assertEqual(history.load_actions(d, "splits", D(2026, 1, 1), D(2026, 3, 23)), [row])

    def test_conflicting_copies_of_one_action_are_refused(self):
        with tempfile.TemporaryDirectory() as d:
            write_window(d, "dividends", "2026-01-01", "2026-03-20",
                         [{"id": "D1", "ticker": "A", "ex_dividend_date": "2026-03-05", "cash_amount": 1.0}])
            write_window(d, "dividends", "2026-01-02", "2026-03-23",
                         [{"id": "D1", "ticker": "A", "ex_dividend_date": "2026-03-05", "cash_amount": 1.5}])
            with self.assertRaises(history.ConflictingActions):
                history.load_actions(d, "dividends", D(2026, 1, 1), D(2026, 3, 23))

    def test_no_recorded_window_is_a_gap_not_an_empty_list(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(history.Gap):
                history.load_actions(d, "splits", D(2026, 3, 2), D(2026, 3, 5))

    def test_windows_that_do_not_cover_the_range_are_a_gap(self):
        with tempfile.TemporaryDirectory() as d:
            write_window(d, "splits", "2026-01-01", "2026-02-01", [])
            write_window(d, "splits", "2026-02-03", "2026-03-01", [])   # 02-02 uncovered
            with self.assertRaises(history.Gap) as cm:
                history.load_actions(d, "splits", D(2026, 1, 15), D(2026, 2, 20))
            self.assertIn("2026-02-02", str(cm.exception))
            with self.assertRaises(history.Gap):
                history.load_actions(d, "splits", D(2026, 2, 10), D(2026, 3, 2))   # past the last window
            self.assertEqual(history.load_actions(d, "splits", D(2026, 2, 3), D(2026, 3, 1)), [])

    def test_a_window_with_only_splits_recorded_resumes_with_dividends(self):
        dv = {"status": "OK", "results": [{"ticker": "A", "ex_dividend_date": "2026-03-12", "cash_amount": 0.5}]}
        with tempfile.TemporaryDirectory() as d:
            existing = write_window(d, "splits", "2026-03-01", "2026-03-27", [{"ticker": "A"}])
            before = slurp(existing)
            client, opener, _ = make_client(dv)
            paths = history.record_actions(client, D(2026, 3, 1), D(2026, 3, 27), d, now=lambda: NOW)
            self.assertEqual(len(opener.requests), 1)   # dividends only
            self.assertIn("/v3/reference/dividends", opener.requests[0].full_url)
            self.assertEqual(paths[0], existing)
            self.assertEqual(slurp(existing), before)
            self.assertEqual(record.read(paths[1])["rows"][0]["cash_amount"], 0.5)

    def test_a_corrupt_existing_window_is_not_resumed_over(self):
        with tempfile.TemporaryDirectory() as d:
            existing = write_window(d, "splits", "2026-03-01", "2026-03-27", [{"ticker": "A"}])
            with open(existing) as f:
                doc = json.load(f)
            doc["record"]["rows"][0]["ticker"] = "B"
            with open(existing, "w") as f:
                json.dump(doc, f)
            client, opener, _ = make_client()
            with self.assertRaises(record.Corrupt):
                history.record_actions(client, D(2026, 3, 1), D(2026, 3, 27), d, now=lambda: NOW)
            self.assertEqual(opener.requests, [])


class SettleTest(unittest.TestCase):
    def test_backfill_refuses_a_session_that_has_not_settled(self):
        day = D(2026, 3, 27)
        with tempfile.TemporaryDirectory() as d:
            client, opener, _ = make_client(grouped(bar("A")))
            with self.assertRaises(steps.NotSettled):
                history.backfill(client, d, asof=day, sessions=1, now=ny(day, 10))
            self.assertEqual(opener.requests, [])
            self.assertEqual(os.listdir(d), [])

    def test_actions_refuse_a_window_ending_on_an_unsettled_session(self):
        day = D(2026, 3, 27)
        with tempfile.TemporaryDirectory() as d:
            client, opener, _ = make_client()
            with self.assertRaises(steps.NotSettled):
                history.record_actions(client, D(2026, 3, 1), day, d, now=ny(day, 15))
            self.assertEqual(opener.requests, [])
            self.assertEqual(os.listdir(d), [])


def slurp(path):
    with open(path) as f:
        return f.read()


def write_window(directory, kind, first, last, rows):
    return record.write(directory, {"kind": kind, "source": "massive", "session": last, "from": first,
                                    "fetched_at": "x", "rows": rows})


def hist(*closes_by_day):
    """[(session, [bars])] for ticker A from (day, close) pairs."""
    return [(day, [bar("A", c)]) for day, c in closes_by_day]


DAYS = [D(2026, 3, 2), D(2026, 3, 3), D(2026, 3, 4), D(2026, 3, 5)]


class AdjustTest(unittest.TestCase):
    def test_no_actions_leaves_closes_unchanged(self):
        h = hist(*zip(DAYS, [10.0, 11.0, 12.0, 13.0]))
        self.assertEqual([c for _, c in adjust.adjusted_closes(h, "A", [], [])], [10.0, 11.0, 12.0, 13.0])

    def test_a_two_for_one_split_halves_earlier_closes(self):
        h = hist(*zip(DAYS, [100.0, 100.0, 50.0, 51.0]))
        split = [{"ticker": "A", "execution_date": "2026-03-04", "split_from": 1, "split_to": 2}]
        self.assertEqual([c for _, c in adjust.adjusted_closes(h, "A", split, [])], [50.0, 50.0, 50.0, 51.0])

    def test_a_reverse_split_scales_earlier_closes_up(self):
        h = hist(*zip(DAYS, [1.0, 1.0, 10.0, 10.0]))
        split = [{"ticker": "A", "execution_date": "2026-03-04", "split_from": 10, "split_to": 1}]
        self.assertEqual([c for _, c in adjust.adjusted_closes(h, "A", split, [])], [10.0, 10.0, 10.0, 10.0])

    def test_a_dividend_scales_earlier_closes_by_one_minus_cash_over_prior_close(self):
        h = hist(*zip(DAYS, [100.0, 100.0, 98.0, 98.0]))
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 2.0}]
        out = [c for _, c in adjust.adjusted_closes(h, "A", [], div)]
        self.assertEqual([round(c, 9) for c in out], [98.0, 98.0, 98.0, 98.0])

    def test_events_compound_and_other_tickers_are_ignored(self):
        h = hist(*zip(DAYS, [200.0, 100.0, 100.0, 100.0]))
        split = [{"ticker": "A", "execution_date": "2026-03-03", "split_from": 1, "split_to": 2},
                 {"ticker": "Z", "execution_date": "2026-03-03", "split_from": 1, "split_to": 9}]
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-05", "cash_amount": 10.0}]
        out = [round(c, 9) for _, c in adjust.adjusted_closes(h, "A", split, div)]
        self.assertEqual(out, [90.0, 90.0, 90.0, 100.0])   # 200 * 0.5 * 0.9 ; 100 * 0.9 ; 100*0.9

    def test_an_event_outside_the_history_is_ignored(self):
        h = hist(*zip(DAYS, [10.0] * 4))
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-02", "cash_amount": 1.0},
               {"ticker": "A", "ex_dividend_date": "2026-06-01", "cash_amount": 1.0}]
        self.assertEqual([c for _, c in adjust.adjusted_closes(h, "A", [], div)], [10.0] * 4)

    def test_a_dividend_whose_prior_session_lacks_the_ticker_fails_closed(self):
        h = [(DAYS[0], [bar("A", 10.0)]), (DAYS[1], [bar("B", 10.0)]), (DAYS[2], [bar("A", 10.0)])]
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 1.0}]
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(h, "A", [], div)

    def test_a_session_missing_between_the_first_and_last_bar_fails_closed(self):
        h = [(DAYS[0], [bar("A", 10.0)]), (DAYS[1], [bar("B", 10.0)]), (DAYS[2], [bar("A", 11.0)])]
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(h, "A", [], [])

    def test_sessions_before_the_first_or_after_the_last_bar_are_allowed(self):
        h = [(DAYS[0], [bar("B", 1.0)]), (DAYS[1], [bar("A", 10.0)]), (DAYS[2], [bar("A", 11.0)]),
             (DAYS[3], [bar("B", 1.0)])]
        self.assertEqual([c for _, c in adjust.adjusted_closes(h, "A", [], [])], [10.0, 11.0])

    def test_a_dividend_at_or_above_the_prior_close_fails_closed(self):
        h = hist(*zip(DAYS, [10.0] * 4))
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 10.0}]
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(h, "A", [], div)

    def test_a_malformed_split_fails_closed(self):
        h = hist(*zip(DAYS, [10.0] * 4))
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(h, "A", [{"ticker": "A", "execution_date": "2026-03-04",
                                             "split_from": 1, "split_to": 0}], [])

    def test_a_dividend_on_a_split_date_uses_the_post_split_prior_close(self):
        # Cash is on the ex-date share basis; on a split's execution date that
        # is the post-split share, so the prior close 100 is 50 on that basis.
        h = hist(*zip(DAYS, [100.0, 100.0, 48.0, 48.0]))
        split = [{"ticker": "A", "execution_date": "2026-03-04", "split_from": 1, "split_to": 2}]
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 2.0}]
        out = [round(c, 9) for _, c in adjust.adjusted_closes(h, "A", split, div)]
        self.assertEqual(out, [48.0, 48.0, 48.0, 48.0])

    def test_dividends_on_one_ex_date_are_summed_into_one_factor(self):
        # Ordinary 2 and special 3 against a prior close of 100: (100 - 5) / 100,
        # not 0.98 x 0.97.
        h = hist(*zip(DAYS, [100.0, 100.0, 95.0, 95.0]))
        div = [{"id": "D1", "ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 2.0},
               {"id": "D2", "ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 3.0}]
        out = [round(c, 9) for _, c in adjust.adjusted_closes(h, "A", [], div)]
        self.assertEqual(out, [95.0, 95.0, 95.0, 95.0])

    def test_same_date_dividends_summing_to_the_prior_close_fail_closed(self):
        h = hist(*zip(DAYS, [10.0] * 4))
        div = [{"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 6.0},
               {"ticker": "A", "ex_dividend_date": "2026-03-04", "cash_amount": 4.0}]
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(h, "A", [], div)

    def test_non_finite_or_non_positive_inputs_fail_closed(self):
        nan, inf = float("nan"), float("inf")
        for closes in ([10.0, 0.0, 10.0, 10.0], [10.0, -1.0, 10.0, 10.0], [10.0, nan, 10.0, 10.0],
                       [10.0, inf, 10.0, 10.0], [10.0, "10", 10.0, 10.0], [10.0, True, 10.0, 10.0]):
            with self.assertRaises(adjust.CannotAdjust, msg=repr(closes)):
                adjust.adjusted_closes(hist(*zip(DAYS, closes)), "A", [], [])
        h = hist(*zip(DAYS, [10.0] * 4))
        for frm, to in ((nan, 1), (1, inf), (inf, 1), ("1", 2), (-1, 2)):
            with self.assertRaises(adjust.CannotAdjust, msg=repr((frm, to))):
                adjust.adjusted_closes(h, "A", [{"ticker": "A", "execution_date": "2026-03-04",
                                                 "split_from": frm, "split_to": to}], [])
        for cash in (nan, inf, -0.5, "1"):
            with self.assertRaises(adjust.CannotAdjust, msg=repr(cash)):
                adjust.adjusted_closes(h, "A", [], [{"ticker": "A", "ex_dividend_date": "2026-03-04",
                                                     "cash_amount": cash}])

    def test_unknown_ticker_has_no_series(self):
        with self.assertRaises(adjust.CannotAdjust):
            adjust.adjusted_closes(hist((DAYS[0], 1.0)), "NOPE", [], [])


if __name__ == "__main__":
    unittest.main()
