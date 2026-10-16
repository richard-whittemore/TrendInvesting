# Market-data step

Fetches Massive market data once and records it as the input each engine reads (ADR 0026, section 8). Standard library only.

    python3 -m marketdata universe --out DIR            # grouped bars + type-CS tickers for the last session
    python3 -m marketdata bars --out DIR --instruments SPY,IEF,SHY

Run from `adapter/`.

- **Key:** `MASSIVE_API_KEY` in the environment, else `--secrets-file` (server, mode 600, outside the repository), else the Mac Keychain item `MASSIVE_API_KEY`. Sent only as a bearer header; never logged, cached or written.
- **Fail closed:** a missing session, empty ticker list, missing instrument bar, or repeated 429 raises and writes nothing; the CLI exits 1 for the caller to alert on.
- **Recording:** `DIR/<kind>_<session>.json` holds the raw rows, source, fetch time and a SHA-256; never overwritten. The `market.universe.snapshot` envelope is a later layer over these files, once its schema lands.
- **Daily bars:** Massive, three requests per instrument (raw, split-adjusted, dividends); at 5 requests a minute that is about 40 seconds an instrument.
- **History (one-time backfill, resumable):** `python3 -m marketdata backfill --out DIR --sessions 253` records one grouped (all US stocks, unadjusted) request per session, about 53 minutes at 5 a minute; existing recordings are skipped, never overwritten. A weekday with no bars is recorded as closed (holiday); a weekday with no file is a gap and fails the read. `python3 -m marketdata actions --out DIR --lookback-days 400` records the bulk splits and dividends.
- **Adjusted closes:** `adjust.adjusted_closes` derives them from the recorded raw bars and actions; the convention (backward ratio, as QuantConnect's Adjusted mode) is stated in `adjust.py`.
- Ranking is a Go rule and is not done here.
