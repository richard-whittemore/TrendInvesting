"""python3 -m marketdata {universe|bars} --out DIR [--asof YYYY-MM-DD] [--instruments SPY,IEF,SHY]

Exit status 1 with a message on stderr when data is missing (nothing is
written), for the caller to alert on. The key comes from MASSIVE_API_KEY,
--secrets-file, or the Mac Keychain, and is never printed."""
import argparse
import datetime as dt
import logging
import sys

from . import massive, steps


def main(argv):
    ap = argparse.ArgumentParser(prog="marketdata")
    ap.add_argument("what", choices=("universe", "bars"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--asof", type=dt.date.fromisoformat, default=dt.date.today())
    ap.add_argument("--instruments", default="SPY,IEF,SHY")
    ap.add_argument("--secrets-file")
    ap.add_argument("--lookback-days", type=int, default=400)
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    try:
        client = massive.Client(massive.load_key(secrets_file=args.secrets_file))
        session, body = steps.find_session(client, args.asof)
        if args.what == "universe":
            path = steps.run_universe(client, session, args.out, grouped_body=body)
        else:
            path = steps.run_bars(client, [s for s in args.instruments.split(",") if s], session,
                                  args.out, args.lookback_days)
    except Exception as e:   # includes MissingData, MarketDataError, AlreadyRecorded
        print("marketdata failed: {}: {}".format(type(e).__name__, e), file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
