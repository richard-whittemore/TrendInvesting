# Market-data step

Fetches Massive market data once and records it as the input each engine reads (ADR 0026, section 8). Standard library only.

    python3 -m marketdata universe --out DIR            # grouped bars + type-CS tickers for the last session
    python3 -m marketdata bars --out DIR --instruments SPY,IEF,SHY

Run from `adapter/`.

- **Key:** `MASSIVE_API_KEY` in the environment, else `--secrets-file` (server, mode 600, outside the repository), else the Mac Keychain item `MASSIVE_API_KEY`. Sent only as a bearer header; never logged, cached or written.
- **Fail closed:** a missing session, empty ticker list, missing instrument bar, or repeated 429 raises and writes nothing; the CLI exits 1 for the caller to alert on.
- **Recording:** `DIR/<kind>_<session>.json` holds the raw rows, source, fetch time and a SHA-256; never overwritten.
- **Daily bars:** Massive, three requests per instrument (raw, split-adjusted, dividends); at 5 requests a minute that is about 40 seconds an instrument.
- **History (one-time backfill, resumable):** `python3 -m marketdata backfill --out DIR --sessions 253` records one grouped (all US stocks, unadjusted) request per session, about 53 minutes at 5 a minute; existing recordings are skipped, never overwritten. Closed days come from the committed NYSE calendar (`nyse_calendar.py`), never from an empty response: an empty response on a trading day fails with nothing recorded and is retried next run; a trading day with no recording is a gap and fails the read. `python3 -m marketdata actions --out DIR --lookback-days 400` records the bulk splits and dividends.
- **Adjusted closes:** `adjust.adjusted_closes` derives them from the recorded raw bars and actions; the convention (backward ratio, as QuantConnect's Adjusted mode) is stated in `adjust.py`.
- Ranking is a Go rule and is not done here.
- **Sectors:** `python3 -m marketdata sectors --out DIR` records iShares' IWV holdings file (GICS) and Nasdaq's screener (no key for either) for the session. Labels are GICS where IWV lists a stock, else Nasdaq's, both mapped by name onto Morningstar's eleven sectors so the cap counts one vocabulary (`sectors.py`; plan.md section 4). The two URLs have not yet been fetched by this code: check the first real run.
- **Engine inputs (no network):** `python3 -m marketdata encode --out DIR --first YYYY-MM-DD --asof YYYY-MM-DD --instruments SPY,IEF --optional ... [--universe-members] [--base YYYY-MM-DD] --configuration-hash H --strategy-version V --run-id R --stream FILE` reads only the recordings and writes a new file of envelopes, one per line: a `market.session.calendar` v1 reaching one session past the last, then per session each instrument's `market.bar.completed` v2 (raw, split-adjusted, and split- and dividend-adjusted, adjusted forward from the base so no bar is ever restated), the `market.universe.snapshot` v1 where a universe was recorded, and a `market.session.closed`. Keep `--base` fixed for an engine's journal: bars on two bases are refused. The convention and every fail-closed case are in `encode.py`; `tests/testdata/envelope_contract.go` checks the output against the Go validators.
