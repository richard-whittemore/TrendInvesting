"""Recorded ECB reference rates for historical dividend adjustment.

Rules: docs/manual/market-data.md, "Dividend currencies". Rates inform
historical ranking only; they never price orders. Unknowns are unavailable,
not dollar amounts or zero (ADR 0019).
"""
import csv
import datetime as dt
import glob
import io
import lzma
import math
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
import zlib

from . import kinds, record
from .massive import MarketDataError
from .steps import MissingData

KIND = kinds.FX
SOURCE = 'ecb-euro-reference-rates'
URL = 'https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist.zip'
MAX_DOWNLOAD_BYTES = 20 * 1024 * 1024
MAX_CSV_BYTES = 50 * 1024 * 1024
DOWNLOAD_SECONDS = 120


class Rates(dict):
    """Fixings and their final coverage date (manual, Dividend currencies)."""

    def __init__(self, rates, covered_through):
        super().__init__(rates)
        self.covered_through = covered_through


class NoRecording(MissingData):
    """No recorded ECB input is available for conversion."""


class Unavailable(Exception):
    """No usable fixing for the dividend's currency and date."""


def parse(text, allow_empty=False):
    """{date: {currency: units per EUR}}; N/A is missing, never zero."""
    try:
        rows = csv.reader(io.StringIO(text), skipinitialspace=True, strict=True)
        header = next(rows)
        header = [s.strip() for s in header]
        if header[-1:] == ['']:
            header.pop()
        if (len(header) < 2 or header[0] != 'Date' or 'USD' not in header
                or len(set(header)) != len(header)
                or any(len(c) != 3 or not c.isupper() or not c.isalpha() for c in header[1:])):
            raise ValueError('invalid columns')
        out = {}
        for row in rows:
            if not row:
                continue
            row = [s.strip() for s in row]
            if row[-1:] == ['']:
                row.pop()
            if len(row) != len(header):
                raise ValueError('wrong row width')
            day = dt.date.fromisoformat(row[0])
            if day in out:
                raise ValueError('duplicate date')
            rates = {}
            for currency, cell in zip(header[1:], row[1:]):
                if cell == 'N/A':
                    continue
                value = float(cell)
                if not math.isfinite(value) or value <= 0:
                    raise ValueError('non-positive or non-finite rate')
                rates[currency] = value
            out[day] = rates
        if not out and not allow_empty:
            raise ValueError('no fixings')
        return out
    except (ValueError, TypeError, IndexError, StopIteration, csv.Error) as e:
        raise MissingData('malformed ECB history: {}'.format(e)) from None


def easter(year):
    """Western Easter by Gregorian computus (manual, Dividend currencies)."""
    a = year % 19
    b, c = divmod(year, 100)
    d, e = divmod(b, 4)
    f = (b + 8) // 25
    g = (b - f + 1) // 3
    h = (19 * a + b - d - g + 15) % 30
    i, k = divmod(c, 4)
    l = (32 + 2 * e + 2 * i - h - k) % 7
    m = (a + 11 * h + 22 * l) // 451
    month, day = divmod(h + l - 7 * m + 114, 31)
    return dt.date(year, month, day + 1)


def target_closed(day):
    """TARGET closing days used for freshness (manual, Dividend currencies)."""
    sunday = easter(day.year)
    return (day.weekday() >= 5 or (day.month, day.day) in
            {(1, 1), (5, 1), (12, 25), (12, 26)} or
            day in {sunday - dt.timedelta(days=2), sunday + dt.timedelta(days=1)})


def require_fresh(rates, fetch_day):
    """A complete archive reaches the prior TARGET day (manual, Dividend currencies)."""
    expected = fetch_day - dt.timedelta(days=1)
    while target_closed(expected):
        expected -= dt.timedelta(days=1)
    if max(rates, default=dt.date.min) < expected:
        raise MarketDataError('stale ECB archive: newest fixing precedes {}'.format(expected))


def valid_currency(currency):
    """Currency syntax accepted by conversion (manual, Dividend currencies)."""
    return isinstance(currency, str) and len(currency) == 3 and currency.isalpha() and currency.isupper()


def usd(cash, currency, day, rates):
    """Use a complete prior five-day window (manual, Dividend currencies).

    The ex-date fixing is never eligible: the factor uses the prior close.
    Incomplete coverage must refuse before any unavailable-conversion skip.
    """
    if currency == 'USD':
        return cash
    if not valid_currency(currency):
        raise Unavailable('missing or malformed dividend currency')
    if rates is None:
        raise MissingData('ECB history recording is required for {}'.format(currency))
    covered = max(max(rates, default=dt.date.min), getattr(rates, 'covered_through', dt.date.min))
    if covered < day - dt.timedelta(days=1):
        raise MissingData('ECB coverage incomplete for dividend on {}'.format(day))
    if currency != 'EUR' and not any(currency in row for row in rates.values()):
        raise Unavailable('currency {} not covered by ECB'.format(currency))
    fixing = None
    for i in range(1, 6):
        candidate = rates.get(day - dt.timedelta(days=i), {})
        if 'USD' in candidate and (currency == 'EUR' or currency in candidate):
            fixing = candidate
            break
    if fixing is None:
        raise Unavailable('no usable ECB fixing within 5 calendar days')
    return cash * fixing['USD'] / (1.0 if currency == 'EUR' else fixing[currency])


def _source(url):
    parts = urllib.parse.urlsplit(url)
    return parts.scheme == 'https' and parts.netloc == 'www.ecb.europa.eu'


class ECBOnlyRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if not _source(urllib.parse.urljoin(req.full_url, newurl)):
            raise urllib.error.HTTPError(req.full_url, code, 'redirect off ECB refused', headers, fp)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


OPENER = urllib.request.build_opener(ECBOnlyRedirect())


def http_get(url, opener=OPENER.open):
    if not _source(url):
        raise MarketDataError('refusing a URL outside HTTPS on ECB')
    deadline = time.monotonic() + DOWNLOAD_SECONDS
    try:
        with opener(urllib.request.Request(url, headers={'User-Agent': 'TrendInvesting market-data'}), timeout=60) as resp:
            actual = resp.geturl()
            if not _source(actual):
                raise MarketDataError('refusing a response outside HTTPS on ECB')
            chunks, size = [], 0
            while True:
                if time.monotonic() >= deadline:
                    raise MarketDataError('ECB download exceeded overall deadline')
                chunk = resp.read1(65536)
                if time.monotonic() >= deadline:
                    raise MarketDataError('ECB download exceeded overall deadline')
                size += len(chunk)
                if size > MAX_DOWNLOAD_BYTES:
                    raise MarketDataError('ECB download exceeded byte limit')
                if not chunk:
                    break
                chunks.append(chunk)
            return b''.join(chunks), actual
    except (urllib.error.URLError, OSError) as e:
        raise MarketDataError('ECB request failed: {}'.format(type(e).__name__)) from None


def csv_body(body):
    try:
        with zipfile.ZipFile(io.BytesIO(body)) as archive:
            member = archive.getinfo('eurofxref-hist.csv')
            if member.file_size > MAX_CSV_BYTES:
                raise MarketDataError('ECB CSV exceeds uncompressed size limit')
            chunks, size = [], 0
            with archive.open(member) as contents:
                while True:
                    chunk = contents.read(min(65536, MAX_CSV_BYTES - size + 1))
                    size += len(chunk)
                    if size > MAX_CSV_BYTES:
                        raise MarketDataError('ECB CSV exceeds uncompressed size limit')
                    if not chunk:
                        break
                    chunks.append(chunk)
            text = b''.join(chunks).decode('utf-8-sig')
        parse(text)
        return text
    except (zipfile.BadZipFile, KeyError, UnicodeError, OSError, EOFError, zlib.error, lzma.LZMAError,
            RuntimeError, ValueError, MissingData) as e:
        raise MarketDataError('malformed ECB archive: {}'.format(type(e).__name__)) from None


def checked(rec, session=None):
    try:
        if rec['kind'] != KIND or rec['source'] != SOURCE or not _source(rec['url']):
            raise ValueError('wrong provenance')
        recorded_day = 'bootstrap' if rec['session'] == 'bootstrap' else dt.date.fromisoformat(rec['session'])
        if session is not None and recorded_day != session:
            raise ValueError('session differs from filename')
        fetched_day = dt.datetime.strptime(rec['fetched_at'], '%Y-%m-%dT%H:%M:%SZ').date()
        rates = parse(rec['body'], allow_empty=True)
        if max(rates, default=dt.date.min) > fetched_day:
            raise MissingData('ECB fixing dated after UTC fetch date')
        return Rates(rates, max(max(rates, default=dt.date.min), fetched_day - dt.timedelta(days=1)))
    except (KeyError, TypeError, ValueError):
        raise MissingData('malformed ECB recording') from None


REMEDY = ('Check the dated fixing on the ECB site. If the ECB really revised it, '
          'move the fx recordings aside into quarantine and let the next firing bootstrap again. '
          'Any IRA run whose files used the old rate needs a fresh run '
          '(docs/manual/market-data.md, What can go wrong).')


def merge(rates, incoming):
    """Union immutable fixings, refusing revisions (manual, Dividend currencies)."""
    for day, row in incoming.items():
        if day in rates and rates[day] != row:
            raise MissingData('ECB revised the {} fixing. {}'.format(day, REMEDY))
        rates[day] = row


def check_history(recorded, incoming):
    """Refuse revised, withdrawn or backdated fixings (manual, Dividend currencies)."""
    for day in recorded:
        if day not in incoming:
            raise MissingData('ECB history no longer contains the {} fixing. {}'.format(day, REMEDY))
    merge(dict(recorded), incoming)
    covered = getattr(recorded, 'covered_through', max(recorded, default=dt.date.min))
    for day in sorted(incoming):
        if day <= covered and day not in recorded:
            raise MissingData('ECB published a {} fixing after {} was treated as complete. {}'.format(day, covered, REMEDY))


def body(rates):
    """CSV for the supplied fixings, preserving unavailable currency cells."""
    currencies = sorted({'USD'} | {c for row in rates.values() for c in row})
    out = io.StringIO()
    writer = csv.writer(out, lineterminator='\n')
    writer.writerow(['Date'] + currencies)
    for day, row in sorted(rates.items(), reverse=True):
        writer.writerow([day.isoformat()] + [row.get(c, 'N/A') for c in currencies])
    return out.getvalue()


def load(directory, session=None, listing=None, read=None):
    """Return the full union offline, filtering dates only if session is passed.

    Validate predecessor coverage in fetch order (manual, Dividend currencies).
    Coverage includes every recording's newest fixing and UTC fetch date minus
    one day, including when dates are filtered.
    """
    names = (os.path.basename(p) for p in glob.glob(os.path.join(directory, KIND + '_*.json'))) if listing is None else listing
    candidates = sorted(n for n in names if n.startswith(KIND + '_') and n.endswith('.json'))
    if not candidates:
        raise NoRecording('no ECB history recording')
    recordings = []
    for name in candidates:
        try:
            suffix = name[len(KIND) + 1:-len('.json')]
            recorded_day = 'bootstrap' if suffix == 'bootstrap' else dt.date.fromisoformat(suffix.split('_')[0])
            rec = (read or record.read)(os.path.join(directory, name))
            incoming = checked(rec, recorded_day)
            if '_' in suffix and name != record.filename(rec):
                raise ValueError('fetch timestamp differs from filename')
            recordings.append((rec['fetched_at'], name, rec, incoming))
        except (record.Corrupt, OSError, ValueError, KeyError, TypeError) as e:
            raise MissingData('unreadable ECB recording: {}'.format(e)) from None
    rates = {}
    covered = dt.date.min
    for _, name, rec, incoming in sorted(recordings):
        expected = covered.isoformat() if covered != dt.date.min else None
        if 'covered_before' not in rec or rec['covered_before'] != expected:
            raise MissingData('ECB recording chain is broken at {}'.format(name))
        merge(rates, incoming)
        covered = max(covered, incoming.covered_through)
    return Rates({day: row for day, row in rates.items() if session is None or day <= session}, covered)
