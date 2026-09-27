"""A tolerant, fail-loud reader for Pinnacle Data CLC continuous-futures
files (ASCII text or CSV, one file per market).

The real files had not arrived when this was written, so nothing here
hard-codes a guess about their layout beyond the one clearly marked
default below. The loader detects:

- the delimiter: comma, tab, semicolon, pipe, or runs of whitespace;
- a header row, whose column names are mapped through ``HEADER_ALIASES``;
- the date format: YYYYMMDD, YYYY-MM-DD, MM/DD/YYYY or MM/DD/YY (two-digit
  years follow ``CENTURY_PIVOT``).

It never repairs a bad row. Any of these raises ``LoaderError``, naming the
file and line: dates that are not strictly increasing, high below low, an
open or settle outside the bar's own high-low range, a missing or
non-numeric price field, a row with a different field count from the
first, or a date format that differs from the first row's.

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
# THE COLUMN LAYOUT -- confirm against the real Pinnacle files first
# (README.md, "Verify the real files first"). For a headerless file this
# tuple names each column in order; ``None`` ignores a column. If the real
# files differ, this line is the one change to make (or pass ``columns=``).
# Default: Pinnacle's historical CLC ASCII order, Date, Open, High, Low,
# Settle, Volume, Open Interest -- UNVERIFIED against the purchased files.
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


def load_series(path, columns=None, date_format=None, century_pivot=CENTURY_PIVOT):
    """Read one market's file into a list of ``Bar``, oldest first.

    ``columns`` overrides both the header row and ``HEADERLESS_COLUMNS``;
    ``date_format`` (one of YYYYMMDD, YYYY-MM-DD, MM/DD/YYYY, MM/DD/YY)
    overrides detection. Raises ``LoaderError`` on anything it cannot read
    exactly (module docstring)."""
    with open(path, "r", encoding="utf-8-sig", errors="strict") as handle:
        lines = [(number, raw.rstrip("\r\n")) for number, raw in enumerate(handle, start=1)]
    lines = [(number, text) for number, text in lines if text.strip() and not text.lstrip().startswith("#")]
    if not lines:
        raise LoaderError(path, None, "no data rows")

    delimiter = detect_delimiter(lines[0][1])
    first_fields = _split(lines[0][1], delimiter)
    if columns is None and detect_date_format(first_fields[0]) is None:
        columns = columns_from_header(first_fields, path)
        lines = lines[1:]
    elif columns is None:
        columns = HEADERLESS_COLUMNS
    columns = tuple(columns)
    _check_layout(columns, path)
    if not lines:
        raise LoaderError(path, None, "no data rows after the header")

    index = {field: position for position, field in enumerate(columns) if field is not None}
    expected_count = None
    bars = []
    for number, text in lines:
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
