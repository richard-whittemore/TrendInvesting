# Market-data step

Fetches Massive market data once and records it as the input each engine reads (ADR 0026, section 8). Standard library only.

    python3 -m marketdata universe --out DIR            # grouped bars + type-CS tickers for the last session
    python3 -m marketdata bars --out DIR --instruments SPY,IEF,SHY

Run from `adapter/`.

- **Key:** `MASSIVE_API_KEY` in the environment, else `--secrets-file` (server, mode 600, outside the repository), else the Mac Keychain item `MASSIVE_API_KEY`. Sent only as a bearer header; never logged, cached or written.
- **Fail closed:** a missing session, empty ticker list, missing instrument bar, or repeated 429 raises and writes nothing; the CLI exits 1 for the caller to alert on.
- **Recording:** `DIR/<kind>_<session>.json` holds the raw rows, source, fetch time and a SHA-256; never overwritten. The `market.universe.snapshot` envelope is a later layer over these files, once its schema lands.
- **Daily bars:** Massive, three requests per instrument (raw, split-adjusted, dividends); at 5 requests a minute that is about 40 seconds an instrument.
- Ranking is a Go rule and is not done here.
