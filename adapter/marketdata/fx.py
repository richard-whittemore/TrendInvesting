"""Recorded ECB reference rates for historical dividend adjustment.

Rules: docs/manual/market-data.md, "Dividend currencies". Rates inform
historical ranking only; they never price orders. Unknowns are unavailable,
not dollar amounts or zero (ADR 0019).
"""
import csv
import datetime as dt
import glob
import io
import math
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile

from . import kinds, record
from .massive import MarketDataError
from .steps import MissingData

KIND = kinds.FX
SOURCE = 'ecb-euro-reference-rates'
URL = 'https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist.zip'
MAX_DOWNLOAD_BYTES = 20 * 1024 * 1024
MAX_CSV_BYTES = 50 * 1024 * 1024
DOWNLOAD_SECONDS = 120


class NoRecording(MissingData):
    """No recorded ECB input is available for conversion."""


class Unavailable(Exception):
    """No usable fixing for the dividend's currency and date."""


def parse(text):
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
        if not out:
            raise ValueError('no fixings')
        return out
    except (ValueError, TypeError, IndexError, StopIteration, csv.Error) as e:
        raise MissingData('malformed ECB history: {}'.format(e)) from None


def usd(cash, currency, day, rates):
    """Ex-date cross rate, or the latest fixing at most five calendar days earlier."""
    if currency == 'USD':
        return cash
    if not isinstance(currency, str) or len(currency) != 3 or not currency.isalpha() or not currency.isupper():
        raise Unavailable('missing or malformed dividend currency')
    if rates is None:
        raise MissingData('ECB history recording is required for {}'.format(currency))
    if currency != 'EUR' and not any(currency in row for row in rates.values()):
        raise Unavailable('currency {} not covered by ECB'.format(currency))
    fixing = None
    for i in range(6):
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
                raise MissingData('ECB CSV exceeds uncompressed size limit')
            chunks, size = [], 0
            with archive.open(member) as contents:
                while True:
                    chunk = contents.read(min(65536, MAX_CSV_BYTES - size + 1))
                    size += len(chunk)
                    if size > MAX_CSV_BYTES:
                        raise MissingData('ECB CSV exceeds uncompressed size limit')
                    if not chunk:
                        break
                    chunks.append(chunk)
            text = b''.join(chunks).decode('utf-8-sig')
        parse(text)
        return text
    except (zipfile.BadZipFile, KeyError, UnicodeError, OSError, RuntimeError, ValueError) as e:
        raise MissingData('malformed ECB archive: {}'.format(type(e).__name__)) from None


def checked(rec, session=None):
    try:
        if rec['kind'] != KIND or rec['source'] != SOURCE or not _source(rec['url']):
            raise ValueError('wrong provenance')
        recorded_day = dt.date.fromisoformat(rec['session'])
        if session is not None and recorded_day != session:
            raise ValueError('session differs from filename')
        dt.datetime.strptime(rec['fetched_at'], '%Y-%m-%dT%H:%M:%SZ')
        return parse(rec['body'])
    except (KeyError, TypeError, ValueError):
        raise MissingData('malformed ECB recording') from None


def merge(rates, incoming):
    """Union immutable fixings, refusing revisions (manual, Dividend currencies)."""
    for day, row in incoming.items():
        if day in rates and rates[day] != row:
            raise MissingData('ECB revised the {} fixing'.format(day))
        rates[day] = row


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
    """Union every recording offline, excluding fixings after the stream end."""
    names = (os.path.basename(p) for p in glob.glob(os.path.join(directory, KIND + '_*.json'))) if listing is None else listing
    candidates = sorted(n for n in names if n.startswith(KIND + '_') and n.endswith('.json'))
    if not candidates:
        raise NoRecording('no ECB history recording')
    rates = {}
    for name in candidates:
        try:
            recorded_day = dt.date.fromisoformat(name[len(KIND) + 1:-len('.json')])
            incoming = checked((read or record.read)(os.path.join(directory, name)), recorded_day)
            merge(rates, incoming)
        except (record.Corrupt, OSError, ValueError, KeyError, TypeError) as e:
            raise MissingData('unreadable ECB recording: {}'.format(e)) from None
    return {day: row for day, row in rates.items() if session is None or day <= session}
