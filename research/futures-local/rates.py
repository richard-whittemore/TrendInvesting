"""A loader and lookup for a 3-month Treasury-bill rate series, so the
local backtester can optionally credit interest on idle cash the way a
futures account really did before 2009 (README.md, "Rate source"): margin
and unused equity sat with the broker or in T-bills and earned the prevailing
short rate, which was a large share of trend followers' pre-2009 returns.

**No market or economic data is fetched or committed here.** This module
only reads a CSV the owner has already saved locally
(``~/Desktop/Trend_Investing/data/rates/TB3MS.csv``, README.md, "Rate
source"). The two FRED series it is written for:

- ``TB3MS``, "3-Month Treasury Bill Secondary Market Rate, Discount Basis"
  (monthly, https://fred.stlouisfed.org/series/TB3MS);
- ``DTB3``, the daily version of the same series
  (https://fred.stlouisfed.org/series/DTB3).

FRED's own CSV download for either series is two columns, a date and the
rate in annual percent, with "." marking a missing observation (a holiday,
mostly for the daily series). The owner's saved TB3MS.csv has the header
``observation_date,TB3MS`` and one row per month, dated the first of the
month (e.g. ``1934-01-01,0.72``) -- ``DTB3``'s own header names its rate
column ``DTB3`` instead, and dates every trading day. ``load_rate_series``
below reads the date and rate by column position, not by the header's own
name, so it accepts either series' header unchanged; only the header ROW
itself (whatever it says) is discarded. Standard-library Python only.
"""

import bisect
import csv
import sys
from collections import namedtuple
from datetime import datetime, timedelta

#: One rate observation: ``date`` is the row's own date; ``annual_rate`` is
#: a fraction (0.0525, not FRED's 5.25), since every accrual formula here
#: wants a fraction.
Rate = namedtuple("Rate", ["date", "annual_rate"])

#: FRED's own missing-observation placeholder (DTB3 uses it on days the
#: series has no observation, such as a bank holiday).
_MISSING = "."


def load_rate_series(path):
    """Read a FRED-style two-column date,rate CSV (TB3MS's own
    ``observation_date,TB3MS`` header, or DTB3's ``DATE,DTB3``) into a
    list of ``Rate``, sorted by date. The header row is discarded
    unconditionally; only the first two columns' positions matter.

    The rate column is read as an annual percent (FRED's convention, e.g.
    ``12.04`` for 12.04%/year) and divided by 100 into a fraction. A row
    whose rate is FRED's own ``"."`` placeholder, or blank, is skipped
    entirely -- not read as a 0% rate -- so ``RateCurve``'s carry-forward
    supplies the last known rate for that date instead (README.md, "Rate
    source"; the missing-observation case this loader itself never
    resolves is exactly what carry-forward is for).
    """
    rows = []
    with open(path, newline="", encoding="utf-8-sig") as handle:
        reader = csv.reader(handle)
        next(reader, None)  # header: "DATE,TB3MS" or "DATE,DTB3"
        for line_number, fields in enumerate(reader, start=2):
            if not fields or not "".join(field.strip() for field in fields):
                continue
            if len(fields) < 2:
                raise ValueError("{}: line {}: expected DATE,RATE, got {!r}".format(path, line_number, fields))
            date_text, rate_text = fields[0].strip(), fields[1].strip()
            if rate_text in ("", _MISSING):
                continue
            day = datetime.strptime(date_text, "%Y-%m-%d").date()
            rows.append(Rate(day, float(rate_text) / 100.0))
    rows.sort(key=lambda row: row.date)
    return rows


class RateCurve:
    """The prevailing 3-month T-bill rate on any date, with carry-forward
    of missing dates, an optional haircut spread, and a day-count
    convention for turning the annual rate into a daily accrual
    (README.md, "Accrual").
    """

    #: Actual/360: FRED's DTB3 and TB3MS both report the T-bill's own
    #: bank-discount rate, which the U.S. Treasury and the money market
    #: quote on a 360-day year, not a 365-day one (a bank-discount rate is
    #: defined that way; see the series' own FRED description). Using 360
    #: here matches the rate's own quoting convention -- 365 would
    #: understate the daily accrual a quoted annual rate implies.
    DAY_COUNT = 360

    def __init__(self, rates, haircut=0.0):
        ordered = sorted(rates, key=lambda row: row.date)
        self._dates = [row.date for row in ordered]
        self._rates = [row.annual_rate for row in ordered]
        #: Dollars per year of spread subtracted from the quoted rate
        #: before crediting (README.md, "Accrual"): what a broker or
        #: futures commission merchant kept rather than passing through.
        self.haircut = haircut
        self._warned_before_first = False

    def rate_on(self, day):
        """The prevailing annual rate (a fraction), less ``haircut``, on
        ``day``: the latest rate dated on or before ``day``
        (carry-forward). Before the series' first date there is no rate to
        carry forward, so this returns 0.0 -- never the haircut applied to
        nothing -- and prints a one-time warning to stderr."""
        index = bisect.bisect_right(self._dates, day) - 1
        if index < 0:
            self._warn_before_first(day)
            return 0.0
        return self._rates[index] - self.haircut

    def _warn_before_first(self, day):
        if self._warned_before_first:
            return
        first = self._dates[0] if self._dates else "no rates loaded"
        print("rates: {} is before the first available rate ({}); crediting zero until then".format(
            day, first), file=sys.stderr)
        self._warned_before_first = True

    def accrued_fraction(self, start_exclusive, end_inclusive):
        """The fraction of equity to credit for every calendar day in
        ``(start_exclusive, end_inclusive]``, one term of
        ``rate_on(day) / DAY_COUNT`` per day -- so a multi-day gap between
        Sessions (a weekend or holiday, when the cash balance does not
        change) is credited exactly as it would be had every one of those
        days been its own Session, including a rate change or a
        carry-forward gap that falls inside it."""
        total = 0.0
        day = start_exclusive + timedelta(days=1)
        while day <= end_inclusive:
            total += self.rate_on(day) / self.DAY_COUNT
            day += timedelta(days=1)
        return total
