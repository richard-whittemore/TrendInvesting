"""Synthetic recordings for the rehearsal of the IRA's data job (deploy/rehearsal/inside.sh): the last 210
sessions' grouped bars, the latest session's universe and sector labels, and the action windows the job asks
for, so ti-data --engine momentum runs offline. Invented tickers and prices only; never real market data.

    python3 ira-recordings.py TI_DATA_ZIP DATA_DIR    prints the first session recorded (the warm-up's base)

The job's own modules are imported from the installed zip application, so the recordings are written by the
code that reads them (record.write, nyse_calendar)."""
import datetime as dt
import sys

sys.path.insert(0, sys.argv[1])

from marketdata import nyse_calendar, record, sectors, tidata  # noqa: E402

SESSIONS = 210
NAMES = ["R{:02d}".format(n) for n in range(30)]


def stamp(t):
    return t.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def bar(ticker, close, volume):
    return {"T": ticker, "o": close, "h": close + 1, "l": close - 1, "c": close, "v": volume, "t": 0}


def main(data):
    now = dt.datetime.now(dt.timezone.utc)
    latest = tidata.settled_session(now, dt.timedelta(minutes=60))
    days = [d for d in nyse_calendar.sessions(latest - dt.timedelta(days=400), latest)][-SESSIONS:]
    for i, day in enumerate(days):
        bars = [bar("SPY", 500.0 + i * 0.5, 1000000), bar("IEF", 95.0, 50000), bar("SHY", 82.0, 50000)]
        bars += [bar(t, 20.0 + n + (i % 9) * 0.1, 100000 - 1000 * n) for n, t in enumerate(NAMES)]
        fetched = stamp(nyse_calendar.session_close(day) + dt.timedelta(minutes=70))
        record.write(data, {"kind": "grouped", "source": "massive", "session": day.isoformat(), "fetched_at": fetched,
                            "adjusted": False, "bars": bars})
        if day == latest:
            record.write(data, {"kind": "universe", "source": "massive", "session": day.isoformat(),
                                "fetched_at": fetched, "adjusted": False, "ticker_type": "CS", "bars": bars,
                                "tickers": [{"ticker": t, "type": "CS", "market": "stocks"} for t in NAMES]})
            iwv = "\n".join(["﻿iShares Russell 3000 ETF", 'Fund Holdings as of,"Jan 2, 2026"', "",
                             "Ticker,Name,Sector,Asset Class,Market Value"]
                            + ['"{}","NAME","Information Technology","Equity","1.00"'.format(t) for t in NAMES]
                            + ["", "The content contained herein is owned or licensed by BlackRock"])
            record.write(data, {"kind": "sectors-iwv", "source": sectors.IWV_SOURCE, "session": day.isoformat(),
                                "fetched_at": fetched, "url": sectors.IWV_URL, "as_of": "Jan 2, 2026", "body": iwv})
            record.write(data, {"kind": "sectors-nasdaq", "source": sectors.NASDAQ_SOURCE, "session": day.isoformat(),
                                "fetched_at": fetched, "url": sectors.NASDAQ_URL,
                                "rows": [{"symbol": t, "name": "N", "sector": "Technology"} for t in NAMES]})
    # The whole span's windows, and the one the job's run asks for (the last ACTION_OVERLAP days), both empty.
    for first in (days[0] + dt.timedelta(days=1), latest - tidata.ACTION_OVERLAP):
        for kind in ("splits", "dividends"):
            record.write(data, {"kind": kind, "source": "massive", "session": latest.isoformat(),
                                "from": first.isoformat(), "fetched_at": stamp(now), "rows": []})
    print(days[0].isoformat())


if __name__ == "__main__":
    main(sys.argv[2])
