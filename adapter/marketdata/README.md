# Market-data step

Fetches Massive market data once and records it as the input each engine reads (ADR 0026, section 8). Standard library only.

    python3 -m marketdata universe --out DIR            # grouped bars + type-CS tickers for the last session
    python3 -m marketdata bars --out DIR --instruments SPY,IEF,SHY

Run from `adapter/`.

- **Key:** `MASSIVE_API_KEY` in the environment, else `--secrets-file` (server, mode 600, outside the repository), else the Mac Keychain item `MASSIVE_API_KEY`. Sent only as a bearer header, and only to `https://api.massive.com`: a redirect anywhere else is refused, so the header is never forwarded; never logged, cached or written.
- **Session:** `--asof` defaults to today's New York date (not the host's). The session is the latest NYSE trading day on or before it, from the committed calendar (`nyse_calendar.py`), never from an empty response. It is refused until its close (4 p.m. New York, 1 p.m. on an early close) plus `--settle-minutes` (default 60), so intraday bars are never recorded as a close.
- **Fail closed:** a session not yet settled, an empty response on a trading day, an empty ticker list, a missing instrument bar, a repeated pagination cursor, or a repeated 429 raises and writes nothing; the CLI exits 1 for the caller to alert on.
- **Recording:** `DIR/<kind>_<session>.json` holds the raw rows, source, fetch time and a SHA-256; never overwritten. Installed by hard link, then the directory (and the parent of any directory created) is synced (ADR 0018); a failed sync reports that the file is recorded. The `market.universe.snapshot` envelope is a later layer over these files, once its schema lands.
- **Daily bars:** Massive, three requests per instrument (raw, split-adjusted, dividends); at 5 requests a minute that is about 40 seconds an instrument.
- **History (one-time backfill, resumable):** `python3 -m marketdata backfill --out DIR --sessions 253` records one grouped (all US stocks, unadjusted) request per session, about 53 minutes at 5 a minute; existing recordings are skipped, never overwritten. Closed days come from the committed NYSE calendar (`nyse_calendar.py`), never from an empty response: an empty response on a trading day fails with nothing recorded and is retried next run; a trading day with no recording is a gap and fails the read. `python3 -m marketdata actions --out DIR --lookback-days 400` records the bulk splits and dividends.
- **Adjusted closes:** `adjust.adjusted_closes` derives them from the recorded raw bars and actions; the convention (backward ratio, as QuantConnect's Adjusted mode) is stated in `adjust.py`.
- Ranking is a Go rule and is not done here.
