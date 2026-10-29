"""A stand-in for the Traditional IRA's warm-up in the first-day rehearsal: the run's first compact stream file, written
as the data job writes one (adapter/marketdata/tidata.py, run_momentum; the file contract delivery.read_stream reads),
from invented prices. Never real market data.

    python3 ira-stream.py ENGINE_LOG STREAMS BASE LAST    prints the file it wrote

The engine's durable log gives the run's identity (its configuration hash, strategy version and name, the log's name
without .log); the file holds the NYSE calendar from BASE to the session after LAST, then for each session from BASE
to LAST the bars of SPY, IEF and SHY and its close, and the universe snapshot of LAST before LAST's close. LAST must
not be a month's last session: a decision session's ranked histories are the real job's (its own rehearsal in
deploy/rehearsal/inside.sh runs it on synthetic recordings). The data job's real warm-up command is rehearsed there;
this stand-in only lets the rehearsal start the IRA adapter's unit on a stream of the run's own.
"""
import datetime as dt
import hashlib
import json
import os
import sys

# The installed adapter's copy of the data job's calendar (provision.sh adapter); TI_ADAPTER_ROOT elsewhere.
sys.path.insert(0, os.path.join(os.environ.get("TI_ADAPTER_ROOT", "/opt/trend-investing/adapter"), "adapter"))
from marketdata import nyse_calendar  # noqa: E402

PRICES = {"IEF": 95.0, "SHY": 82.0, "SPY": 500.0}
ROWS = [("R00", 60.0, "Information Technology"), ("R01", 45.0, "Health Care"), ("R02", 30.0, "Energy")]


def stamp(t):
    return t.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def main(engine_log, streams, base, last):
    with open(engine_log, encoding="utf-8") as f:
        header = json.loads(f.readline())
    run = os.path.basename(engine_log)[:-len(".log")]
    base, last = dt.date.fromisoformat(base), dt.date.fromisoformat(last)
    sessions, day = [], base
    while day <= last:
        if nyse_calendar.is_trading_day(day):
            sessions.append(day)
        day += dt.timedelta(days=1)
    following = nyse_calendar.sessions_after(last, 1)[0]
    if following.month != last.month:
        sys.exit("ira-stream: {} is a month's last session; the stand-in writes no decision session".format(last))
    out = [("market.session.calendar", 1, "marketdata:calendar:" + run, stamp(dt.datetime.combine(
        base, dt.time(9), nyse_calendar.NEW_YORK)), {"source": "nyse", "sessions": [d.isoformat() for d in sessions + [following]]})]
    for i, day in enumerate(sessions):
        end = stamp(nyse_calendar.session_close(day))
        for symbol in sorted(PRICES):
            p = round(PRICES[symbol] * (1 + 0.0002 * i), 2)
            view = lambda label: {"view": label, "open": p, "high": p, "low": p, "close": p, "volume": 1e6}  # noqa: E731
            out.append(("market.bar.completed", 2, "marketdata:bar:{}:{}".format(symbol, day), end, {
                "instrument_id": symbol, "period_end": end, "split_adjusted": view("split-adjusted"), "raw": view("raw"),
                "adjusted": view("split-and-dividend-adjusted"), "adjusted_base": base.isoformat()}))
        if day == last:
            rows = [{"instrument_id": s, "type": "CS", "close": c, "volume": 2e6, "sector": sector,
                     "sector_source": "ishares-iwv-gics"} for s, c, sector in ROWS]
            out.append(("market.universe.snapshot", 1, "marketdata:universe:{}".format(day), end,
                        {"session": day.isoformat(), "source": "massive", "rows": rows}))
        out.append(("market.session.closed", 1, "marketdata:session-closed:{}".format(day), end,
                    {"period_end": end, "instrument_ids": sorted(PRICES)}))
    folder = os.path.join(streams, run)
    os.makedirs(folder, exist_ok=True)
    path = os.path.join(folder, "trad-ira-{}.jsonl".format(last.isoformat()))
    with open(path + ".part", "w", encoding="utf-8") as f:
        for n, (kind, version, ident, at, payload) in enumerate(out, 1):
            raw = json.dumps(payload, separators=(",", ":"))
            f.write(json.dumps({"id": ident, "type": kind, "envelope_version": 1, "schema_version": version,
                                "event_time": at, "recorded_at": at, "sequence": n, "correlation_id": run,
                                "source": "marketdata", "strategy_version": header["strategy_version"],
                                "configuration_hash": header["configuration_hash"],
                                "payload_hash": hashlib.sha256(raw.encode()).hexdigest(), "payload": payload},
                               separators=(",", ":")) + "\n")
    os.replace(path + ".part", path)
    print(path)


if __name__ == "__main__":
    main(*sys.argv[1:5])
