"""A tolerant, fail-loud reader for Pinnacle Data CLC continuous-futures
files (ASCII text or CSV, one file per market).

The purchased CLC files (checked 2026-09-28) are headerless CSV, dated
MM/DD/YYYY, in ``HEADERLESS_COLUMNS`` order. The loader still detects:

- the delimiter: comma, tab, semicolon, pipe, or runs of whitespace;
- a header row, whose column names are mapped through ``HEADER_ALIASES``;
- the date format: YYYYMMDD, YYYY-MM-DD, MM/DD/YYYY or MM/DD/YY (two-digit
  years follow ``CENTURY_PIVOT``).

It never repairs a bad row. Any of these raises ``LoaderError``, naming the
file and line: dates that are not strictly increasing, high below low, an
open or settle outside the bar's own high-low range, a missing or
non-numeric price field, a row with a different field count from the
first, or a date format that differs from the first row's. With ``end``,
rows after that date are never read, so corrupt rows in the held-out
period cannot stop an in-sample run.

Back-adjusted prices can legitimately be zero or negative, so no sign
check is made on any price.

Standard-library Python only.
"""

import math
import os
import re
from collections import namedtuple
from datetime import date

# =============================================================================
# THE COLUMN LAYOUT. For a headerless file this tuple names each column in
# order; ``None`` ignores a column (or pass ``columns=``). Confirmed on the
# purchased CLC files: Date, Open, High, Low, Settle, Volume, Open Interest
# (e.g. US_NON's first row, 01/03/1978, 99.3125, 99.3125, 98.875, 98.875,
# 148, 2878).
# =============================================================================
HEADERLESS_COLUMNS = ("date", "open", "high", "low", "close", "volume", "open_interest")

#: Column names a header row may use, per field, matched case-insensitively
#: after collapsing spaces, dots and underscores. First match wins, so the
#: contract's own open interest is preferred over the all-contracts total.
HEADER_ALIASES = {
    "date": ("date", "tradedate", "day"),
    "open": ("open", "o"),
    "high": ("high", "h"),
    "low": ("low", "l"),
    "close": ("settle", "settlement", "close", "last", "c"),
    "volume": ("volume", "vol", "totalvolume", "totvol", "v"),
    "open_interest": ("openinterest", "oi", "openint", "totalopeninterest", "totaloi"),
}

#: Two-digit years at or above this are 19xx; below it, 20xx. Pinnacle's
#: CLC history starts in 1969, and 50 keeps every year from 1950 to 2049
#: unambiguous.
CENTURY_PIVOT = 50

REQUIRED_FIELDS = ("date", "open", "high", "low", "close")
OPTIONAL_FIELDS = ("volume", "open_interest")

#: Extensions ``find_market_file`` accepts, compared case-insensitively.
DATA_EXTENSIONS = ("", ".txt", ".csv", ".asc", ".prn", ".dat")

Bar = namedtuple("Bar", ["date", "open", "high", "low", "close", "volume", "open_interest"])

#: A leading UTF-8 byte-order mark, stripped from the file's first line
#: only (``load_series`` used to rely on the ``"utf-8-sig"`` codec for
#: this; reading raw bytes line by line does it by hand instead).
_UTF8_BOM = b"\xef\xbb\xbf"


class LoaderError(ValueError):
    """A file this loader refuses to read, with the path and line number."""

    def __init__(self, path, line_number, message):
        self.path = path
        self.line_number = line_number
        where = path if line_number is None else "{}: line {}".format(path, line_number)
        super().__init__("{}: {}".format(where, message))


# -----------------------------------------------------------------------------
# Delimiters and dates.
# -----------------------------------------------------------------------------


def detect_delimiter(line):
    """The first of comma, tab, semicolon or pipe present in ``line``, or
    ``None`` for runs of whitespace (``str.split()``)."""
    for candidate in (",", "\t", ";", "|"):
        if candidate in line:
            return candidate
    return None


def _split(line, delimiter):
    fields = line.split() if delimiter is None else line.split(delimiter)
    return [field.strip().strip('"').strip("'").strip() for field in fields]


_DATE_PATTERNS = (
    ("YYYYMMDD", re.compile(r"^(\d{4})(\d{2})(\d{2})$")),
    ("YYYY-MM-DD", re.compile(r"^(\d{4})-(\d{1,2})-(\d{1,2})$")),
    ("MM/DD/YYYY", re.compile(r"^(\d{1,2})/(\d{1,2})/(\d{4})$")),
    ("MM/DD/YY", re.compile(r"^(\d{1,2})/(\d{1,2})/(\d{2})$")),
)


def detect_date_format(text):
    """The name of the one supported format ``text`` matches, or ``None``.
    A six-digit value (YYMMDD or MMDDYY) is deliberately not guessed."""
    for name, pattern in _DATE_PATTERNS:
        if pattern.match(text):
            return name
    return None


def parse_date(text, date_format, century_pivot=CENTURY_PIVOT):
    """Parse ``text`` in ``date_format``. Raises ``ValueError`` on a
    mismatch or an impossible calendar date (a month above 12 is reported
    as such: a DD/MM file is never silently read as MM/DD)."""
    pattern = dict(_DATE_PATTERNS).get(date_format)
    if pattern is None:
        raise ValueError("unsupported date format {!r}".format(date_format))
    match = pattern.match(text)
    if match is None:
        raise ValueError("date {!r} is not {} like the first row".format(text, date_format))
    a, b, c = (int(group) for group in match.groups())
    if date_format in ("YYYYMMDD", "YYYY-MM-DD"):
        year, month, day = a, b, c
    else:
        month, day, year = a, b, c
        if date_format == "MM/DD/YY":
            year += 1900 if year >= century_pivot else 2000
    if not 1 <= month <= 12:
        raise ValueError("date {!r} has month {} (a DD/MM file?)".format(text, month))
    return date(year, month, day)


# -----------------------------------------------------------------------------
# Columns.
# -----------------------------------------------------------------------------


def _normalise_name(name):
    return re.sub(r"[\s._\-]+", "", name.lower())


def columns_from_header(header_fields, path):
    """Map a header row to a column layout like ``HEADERLESS_COLUMNS``."""
    normalised = [_normalise_name(name) for name in header_fields]
    layout = [None] * len(header_fields)
    for field, aliases in HEADER_ALIASES.items():
        for alias in aliases:
            if alias in normalised and layout[normalised.index(alias)] is None:
                layout[normalised.index(alias)] = field
                break
    missing = [field for field in REQUIRED_FIELDS if field not in layout]
    if missing:
        raise LoaderError(path, 1, "header {} has no column for {}".format(header_fields, ", ".join(missing)))
    return tuple(layout)


def _check_layout(columns, path):
    missing = [field for field in REQUIRED_FIELDS if field not in columns]
    if missing:
        raise LoaderError(path, None, "column layout {} has no {}".format(columns, ", ".join(missing)))


def _number(text, field, required):
    if text == "":
        if required:
            raise ValueError("{} is missing".format(field))
        return None
    try:
        value = float(text)
    except ValueError:
        raise ValueError("{} {!r} is not a number".format(field, text)) from None
    if not math.isfinite(value):
        raise ValueError("{} {!r} is not finite".format(field, text))
    return value


# -----------------------------------------------------------------------------
# The loader.
# -----------------------------------------------------------------------------


def load_series(path, columns=None, date_format=None, century_pivot=CENTURY_PIVOT, end=None):
    """Read one market's file into a list of ``Bar``, oldest first.

    ``columns`` overrides both the header row and ``HEADERLESS_COLUMNS``;
    ``date_format`` (one of YYYYMMDD, YYYY-MM-DD, MM/DD/YYYY, MM/DD/YY)
    overrides detection. Raises ``LoaderError`` on anything it cannot read
    exactly (module docstring).

    ``end`` (a ``date``) stops reading at the first row dated after it:
    that row and every later one are neither validated nor returned --
    and, because the file is streamed line by line rather than read whole
    up front, never even decoded. The real Pinnacle files carry corrupt
    rows, including bytes that are not valid UTF-8, long after the
    in-sample end (README.md, "Data problems found"), and a run should
    never read the held-out period at all: a bad byte out there must not
    abort an in-sample run. A row whose date itself cannot be parsed is
    still an error, since its place in time is unknown."""
    with open(path, "rb") as handle:

        def _rows():
            # Binary mode, decoded one line at a time: a buffered reader may
            # still pull many kilobytes of raw bytes ahead of whatever line
            # was actually asked for, but that is harmless undecoded data.
            # Decoding (where a bad byte would raise) happens here, per
            # line, only for a line this generator is actually asked to
            # produce -- so a ``break`` below, once a row after ``end`` is
            # seen, guarantees no later line's bytes are ever decoded.
            first = True
            for number, raw in enumerate(handle, start=1):
                if first:
                    if raw.startswith(_UTF8_BOM):
                        raw = raw[len(_UTF8_BOM):]
                    first = False
                try:
                    text = raw.decode("utf-8").rstrip("\r\n")
                except UnicodeDecodeError as err:
                    raise LoaderError(path, number, "not valid utf-8: {}".format(err)) from None
                if text.strip() and not text.lstrip().startswith("#"):
                    yield number, text

        row_iter = _rows()
        try:
            number, text = next(row_iter)
        except StopIteration:
            raise LoaderError(path, None, "no data rows") from None

        delimiter = detect_delimiter(text)
        first_fields = _split(text, delimiter)
        if columns is None and detect_date_format(first_fields[0]) is None:
            columns = columns_from_header(first_fields, path)
            try:
                number, text = next(row_iter)
            except StopIteration:
                raise LoaderError(path, None, "no data rows after the header") from None
        elif columns is None:
            columns = HEADERLESS_COLUMNS
        columns = tuple(columns)
        _check_layout(columns, path)

        index = {field: position for position, field in enumerate(columns) if field is not None}
        expected_count = None
        bars = []
        while True:
            fields = _split(text, delimiter)
            if expected_count is None:
                expected_count = len(fields)
                if expected_count < len(columns):
                    raise LoaderError(path, number, "{} fields, but the column layout needs {}".format(
                        expected_count, len(columns)))
                if date_format is None:
                    date_format = detect_date_format(fields[index["date"]])
                    if date_format is None:
                        raise LoaderError(path, number, "unrecognised date {!r}; pass date_format".format(
                            fields[index["date"]]))
            if end is not None and len(fields) > index["date"]:
                try:
                    row_date = parse_date(fields[index["date"]], date_format, century_pivot)
                except ValueError as err:
                    raise LoaderError(path, number, str(err)) from None
                if row_date > end:
                    break
            if len(fields) != expected_count:
                raise LoaderError(path, number, "{} fields, expected {} (a missing field?)".format(
                    len(fields), expected_count))
            try:
                bar = _parse_row(fields, index, date_format, century_pivot)
            except ValueError as err:
                raise LoaderError(path, number, str(err)) from None
            if bars and bar.date <= bars[-1].date:
                raise LoaderError(path, number, "dates are not strictly monotonic: {} follows {}".format(
                    bar.date, bars[-1].date))
            bars.append(bar)
            try:
                number, text = next(row_iter)
            except StopIteration:
                break
    return bars


def _parse_row(fields, index, date_format, century_pivot):
    bar_date = parse_date(fields[index["date"]], date_format, century_pivot)
    values = {}
    for field in ("open", "high", "low", "close"):
        values[field] = _number(fields[index[field]], field, required=True)
    for field in OPTIONAL_FIELDS:
        values[field] = _number(fields[index[field]], field, required=False) if field in index else None
    if values["high"] < values["low"]:
        raise ValueError("high {} is below low {}".format(values["high"], values["low"]))
    for field in ("open", "close"):
        if not values["low"] <= values[field] <= values["high"]:
            raise ValueError("{} {} is outside the bar's range {}..{}".format(
                field, values[field], values["low"], values["high"]))
    return Bar(bar_date, values["open"], values["high"], values["low"], values["close"],
               values["volume"], values["open_interest"])


# -----------------------------------------------------------------------------
# Price-scale sanity check (markets.py's "Important caveat": Pinnacle's own
# price scaling may not match the unit its multiplier assumes, and this
# can't be resolved by exchange-spec research alone -- it takes one real
# bar). This never guesses which unit is right; it only flags whether a
# real settle price is consistent with the assumed unit, the one named
# alternative markets.py's comments call out, or neither.
# -----------------------------------------------------------------------------

#: For each traded market, the settle-price range Pinnacle's non-adjusted
#: (``_NON``) file shows in its own units -- confirmed against the real
#: files (markets.py, [F]) -- and the one alternate unit a mis-scaled file
#: would show instead, usually the exchange's own quote (dollars where
#: Pinnacle stores cents; dollars per unit where Pinnacle scales a currency
#: up). Both ranges cover the market's 1980-2015 history. ``None`` for the
#: alternate means no plausible mis-scaling lands in a distinct range.
PRICE_SCALE_HINTS = {
    # Decimal points: 55 (early-1980s yields) to 175 (2015).
    "US": ((55.0, 175.0), None),
    "TY": ((55.0, 145.0), None),
    # 100 minus a 0.1%-20% rate.
    "ED": ((79.0, 100.0), None),
    # Cents per franc / mark-then-euro / pound / Canadian dollar vs dollars.
    "SF": ((25.0, 130.0), (0.25, 1.30)),
    "EC": ((28.0, 165.0), (0.28, 1.65)),
    "BP": ((100.0, 250.0), (1.0, 2.5)),
    "CD": ((60.0, 112.0), (0.60, 1.12)),
    # Dollars per yen x 10,000 vs dollars per yen: USDJPY ran 75-280.
    "JY": ((10000.0 / 280, 10000.0 / 75), (1.0 / 280, 1.0 / 75)),
    # Index points.
    "SP": ((100.0, 2200.0), None),
    # Dollars per troy oz.
    "GC": ((250.0, 1950.0), None),
    # Cents per troy oz vs dollars: silver ran $3.50-$50.
    "SI": ((300.0, 5500.0), (3.0, 55.0)),
    # Cents per lb vs dollars: copper ran $0.50-$4.60.
    "HG": ((50.0, 500.0), (0.5, 5.0)),
    # Dollars per barrel.
    "CL": ((9.0, 150.0), None),
    # Cents per gallon vs dollars: $0.25-$4.50.
    "HO": ((25.0, 450.0), (0.25, 4.5)),
    "HU": ((25.0, 450.0), (0.25, 4.5)),
    # Cents per lb (sugar, coffee, cotton); dollars per metric ton (cocoa).
    "SB": ((2.0, 50.0), None),
    "KC": ((40.0, 340.0), None),
    "CC": ((650.0, 4500.0), None),
    "CT": ((25.0, 220.0), None),
}


def classify_price_scale(symbol, price):
    """Whether ``price`` -- one real settle from Pinnacle's non-adjusted
    file for ``symbol`` -- looks like markets.py's unit, the one named
    alternate, or neither.

    Returns ``"assumed"``, ``"alternate"``, ``"unknown"`` (outside both
    ranges -- something else is wrong, not just a units mismatch), or
    ``None`` when ``symbol`` has no hint. It flags; it never guesses.
    """
    hints = PRICE_SCALE_HINTS.get(symbol)
    if hints is None:
        return None
    assumed, alternate = hints
    if assumed[0] <= price <= assumed[1]:
        return "assumed"
    if alternate is not None and alternate[0] <= price <= alternate[1]:
        return "alternate"
    return "unknown"


def check_series_scale(symbol, bars):
    """``classify_price_scale`` on the *last* bar's close in ``bars``, or
    ``None`` for an empty series or a symbol with no hint.

    Pass the non-adjusted (``_NON``) series, clipped at the run's end: its
    every settle is a real price. A back-adjusted series matches the real
    price only at the file's own last date, which is in the held-out
    period.
    """
    if not bars:
        return None
    return classify_price_scale(symbol, bars[-1].close)


def find_market_file(directory, stem):
    """The one file in ``directory`` whose name is ``stem`` plus one of
    ``DATA_EXTENSIONS`` (case-insensitive), or ``None`` when there is none.
    Two candidates raise ``LoaderError`` rather than pick one."""
    wanted = stem.lower()
    matches = []
    for name in sorted(os.listdir(directory)):
        base, extension = os.path.splitext(name)
        if base.lower() == wanted and extension.lower() in DATA_EXTENSIONS:
            matches.append(os.path.join(directory, name))
    if len(matches) > 1:
        raise LoaderError(directory, None, "more than one file for {}: {}".format(stem, matches))
    return matches[0] if matches else None
