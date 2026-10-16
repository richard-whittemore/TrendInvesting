import datetime as dt
import unittest

from marketdata import nyse_calendar as calendar

D = dt.date


class CalendarTest(unittest.TestCase):
    def test_nyse_full_day_holidays_2024_to_2027(self):
        holidays = [
            "2024-01-01", "2024-01-15", "2024-02-19", "2024-03-29", "2024-05-27", "2024-06-19",
            "2024-07-04", "2024-09-02", "2024-11-28", "2024-12-25",
            "2025-01-01", "2025-01-09", "2025-01-20", "2025-02-17", "2025-04-18", "2025-05-26",
            "2025-06-19", "2025-07-04", "2025-09-01", "2025-11-27", "2025-12-25",
            "2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19",
            "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25",
            "2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18",
            "2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24"]
        for h in holidays:
            self.assertFalse(calendar.is_trading_day(D.fromisoformat(h)), h)

    def test_ordinary_weekdays_and_early_closes_trade(self):
        for t in ("2026-03-27", "2026-07-02", "2025-07-03", "2025-11-28", "2025-12-24", "2026-12-24"):
            self.assertTrue(calendar.is_trading_day(D.fromisoformat(t)), t)

    def test_weekends_do_not_trade(self):
        self.assertFalse(calendar.is_trading_day(D(2026, 3, 28)))
        self.assertFalse(calendar.is_trading_day(D(2026, 3, 29)))

    def test_new_years_day_on_a_saturday_is_not_observed_on_friday(self):
        self.assertTrue(calendar.is_trading_day(D(2027, 12, 31)))

    def test_outside_the_table_fails_closed(self):
        with self.assertRaises(calendar.OutOfRange):
            calendar.is_trading_day(D(2035, 1, 3))


class SessionCloseTest(unittest.TestCase):
    # NYSE's published early closes (1 p.m.); the 2026-2028 ones were checked
    # against nyse.com/markets/hours-calendars on 2026-10-16.
    EARLY = ("2022-11-25", "2023-07-03", "2023-11-24", "2024-07-03", "2024-11-29", "2024-12-24",
             "2025-07-03", "2025-11-28", "2025-12-24", "2026-11-27", "2026-12-24", "2027-11-26",
             "2028-07-03", "2028-11-24")

    def test_early_closes_close_at_one_in_new_york(self):
        for day in self.EARLY:
            self.assertTrue(calendar.is_early_close(D.fromisoformat(day)), day)
        utc = calendar.close_utc(D(2026, 11, 27))   # EST: 13:00 is 18:00 UTC
        self.assertEqual(utc, dt.datetime(2026, 11, 27, 18, 0, tzinfo=dt.timezone.utc))

    def test_other_sessions_close_at_four_in_new_york(self):
        for day in ("2026-07-02", "2027-07-02", "2027-12-23", "2022-12-23", "2026-03-27"):
            self.assertFalse(calendar.is_early_close(D.fromisoformat(day)), day)
        self.assertEqual(calendar.close_utc(D(2026, 7, 2)), dt.datetime(2026, 7, 2, 20, 0, tzinfo=dt.timezone.utc))
        self.assertEqual(calendar.close_utc(D(2026, 1, 2)), dt.datetime(2026, 1, 2, 21, 0, tzinfo=dt.timezone.utc))

    def test_a_day_that_does_not_trade_has_no_close(self):
        with self.assertRaises(ValueError):
            calendar.close_utc(D(2026, 11, 26))

    def test_sessions_lists_trading_days_and_the_ones_after(self):
        self.assertEqual(calendar.sessions(D(2026, 4, 1), D(2026, 4, 7)),
                         [D(2026, 4, 1), D(2026, 4, 2), D(2026, 4, 6), D(2026, 4, 7)])
        self.assertEqual(calendar.sessions_after(D(2026, 4, 2), 2), [D(2026, 4, 6), D(2026, 4, 7)])
