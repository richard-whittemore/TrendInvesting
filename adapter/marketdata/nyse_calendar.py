"""NYSE trading days, from the exchange's published holiday rules (committed,
not fetched: Massive's market-holidays endpoint lists only upcoming holidays,
so it cannot decide a past day, and a decision made on an API's word alone
would turn a transient empty response into a permanent hole).

Source: NYSE "Holidays & Trading Hours" (nyse.com/markets/hours-calendars).
Full-day closures: New Year's Day, Martin Luther King Jr. Day (3rd Monday of
January), Washington's Birthday (3rd Monday of February), Good Friday,
Memorial Day (last Monday of May), Juneteenth (June 19, from 2022),
Independence Day, Labor Day (1st Monday of September), Thanksgiving (4th
Thursday of November), Christmas. A holiday on a Saturday is observed the
Friday before and on a Sunday the Monday after, except that New Year's Day on
a Saturday is not observed on the Friday. Sessions close at 4 p.m. New York
time; early closes (1 p.m.) still produce bars and are trading days: the day
after Thanksgiving, and July 3 and December 24 when they fall Monday to
Thursday (when the holiday itself is Tuesday to Friday). Special closures are listed in SPECIAL:
2025-01-09 (National Day of Mourning for President Carter).
Years outside FIRST_YEAR..LAST_YEAR raise OutOfRange; extend and re-check
against the NYSE page each year.
"""
import datetime as dt
import zoneinfo

FIRST_YEAR, LAST_YEAR = 2022, 2030
SPECIAL = {dt.date(2025, 1, 9)}
NEW_YORK = zoneinfo.ZoneInfo("America/New_York")


class OutOfRange(Exception):
    pass


def _nth_weekday(year, month, weekday, n):
    first = dt.date(year, month, 1)
    return first + dt.timedelta(days=(weekday - first.weekday()) % 7 + 7 * (n - 1))


def _last_monday(year, month):
    last = dt.date(year + (month == 12), month % 12 + 1, 1) - dt.timedelta(days=1)
    return last - dt.timedelta(days=last.weekday())


def _easter(year):
    a, b, c = year % 19, year // 100, year % 100
    d, e = b // 4, b % 4
    g = (8 * b + 13) // 25
    h = (19 * a + b - d - g + 15) % 30
    j, k = c // 4, c % 4
    m = (a + 11 * h) // 319
    r = (2 * e + 2 * j - k - h + m + 32) % 7
    month = (h - m + r + 90) // 25
    return dt.date(year, month, (h - m + r + month + 19) % 32)


def _observed(day):
    if day.weekday() == 5:
        return day - dt.timedelta(days=1)
    if day.weekday() == 6:
        return day + dt.timedelta(days=1)
    return day


def holidays(year):
    out = {_nth_weekday(year, 1, 0, 3), _nth_weekday(year, 2, 0, 3),
           _easter(year) - dt.timedelta(days=2), _last_monday(year, 5),
           _nth_weekday(year, 9, 0, 1), _nth_weekday(year, 11, 3, 4),
           _observed(dt.date(year, 7, 4)), _observed(dt.date(year, 12, 25)),
           _observed(dt.date(year, 6, 19))}
    if dt.date(year, 1, 1).weekday() != 5:
        out.add(_observed(dt.date(year, 1, 1)))
    return out | {d for d in SPECIAL if d.year == year}


def is_trading_day(day):
    if not FIRST_YEAR <= day.year <= LAST_YEAR:
        raise OutOfRange("no NYSE calendar for {}".format(day.year))
    return day.weekday() < 5 and day not in holidays(day.year)


def early_closes(year):
    out = {_nth_weekday(year, 11, 3, 4) + dt.timedelta(days=1)}
    for month, day in ((7, 3), (12, 24)):
        d = dt.date(year, month, day)
        if d.weekday() < 4:
            out.add(d)
    return out


def session_close(day):
    """The close of trading day ``day`` as a New York datetime."""
    if not is_trading_day(day):
        raise ValueError("{} is not an NYSE trading day".format(day))
    hour = 13 if day in early_closes(day.year) else 16
    return dt.datetime(day.year, day.month, day.day, hour, tzinfo=NEW_YORK)


def sessions(first, last):
    """The trading days first..last, oldest first."""
    out, day = [], first
    while day <= last:
        if is_trading_day(day):
            out.append(day)
        day += dt.timedelta(days=1)
    return out


def sessions_after(day, n):
    """The ``n`` trading days after ``day``."""
    out = []
    while len(out) < n:
        day += dt.timedelta(days=1)
        if is_trading_day(day):
            out.append(day)
    return out
