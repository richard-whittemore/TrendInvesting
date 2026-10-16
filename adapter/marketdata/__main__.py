"""python3 -m marketdata {universe|bars} --out DIR [--asof YYYY-MM-DD] [--instruments SPY,IEF,SHY]
    [--settle-minutes N]

--asof defaults to today's New York date. A session is refused (exit 1,
nothing written) until its close plus the settle delay.

Exit status 1 with a message on stderr when data is missing (nothing is
written), for the caller to alert on. The key comes from MASSIVE_API_KEY,
--secrets-file, or the Mac Keychain, and is never printed."""
import argparse
import datetime as dt
import logging
import sys

from . import massive, steps


def parse(argv, now=steps._now):
    ap = argparse.ArgumentParser(prog="marketdata")
    ap.add_argument("what", choices=("universe", "bars"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--asof", type=dt.date.fromisoformat, default=None)
    ap.add_argument("--instruments", default="SPY,IEF,SHY")
    ap.add_argument("--secrets-file")
    ap.add_argument("--lookback-days", type=int, default=400)
    ap.add_argument("--settle-minutes", type=int, default=int(steps.SETTLE.total_seconds() // 60),
                    help="minutes after the close before a session may be recorded")
    args = ap.parse_args(argv)
    if args.settle_minutes < 0:
        ap.error("--settle-minutes must not be negative")
    if args.asof is None:
        args.asof = steps.new_york_date(now)
    return args


def main(argv):
    args = parse(argv)
    settle = dt.timedelta(minutes=args.settle_minutes)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    try:
        client = massive.Client(massive.load_key(secrets_file=args.secrets_file))
        session, body = steps.find_session(client, args.asof, settle=settle)
        if args.what == "universe":
            path = steps.run_universe(client, session, args.out, grouped_body=body, settle=settle)
        else:
            path = steps.run_bars(client, [s for s in args.instruments.split(",") if s], session,
                                  args.out, args.lookback_days, settle=settle)
    except Exception as e:   # includes MissingData, MarketDataError, AlreadyRecorded
        print("marketdata failed: {}: {}".format(type(e).__name__, e), file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
