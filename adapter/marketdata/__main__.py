"""python3 -m marketdata {universe|bars|backfill|actions|sectors|encode} --out DIR [--asof YYYY-MM-DD] ...

Exit status 1 with a message on stderr when data is missing (nothing is
written), for the caller to alert on. The Massive key comes from
MASSIVE_API_KEY, --secrets-file, or the Mac Keychain, and is never printed;
``sectors`` needs no key, and ``encode`` reads only the recordings in DIR,
never the network, and writes the engines' input stream to --stream."""
import argparse
import datetime as dt
import logging
import sys

from . import encode, history, massive, sectors, steps


def main(argv):
    ap = argparse.ArgumentParser(prog="marketdata")
    ap.add_argument("what", choices=("universe", "bars", "backfill", "actions", "sectors", "encode"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--asof", type=dt.date.fromisoformat, default=dt.date.today())
    ap.add_argument("--instruments", default="SPY,IEF,SHY")
    ap.add_argument("--secrets-file")
    ap.add_argument("--lookback-days", type=int, default=400)
    ap.add_argument("--sessions", type=int, default=253, help="backfill: sessions of history to record")
    ap.add_argument("--first", type=dt.date.fromisoformat, help="encode: first session written")
    ap.add_argument("--base", type=dt.date.fromisoformat, help="encode: adjusted views' base (default --first)")
    ap.add_argument("--optional", default="", help="encode: instruments written where they traded")
    ap.add_argument("--universe-members", action="store_true", help="encode: also every type-CS ticker with a bar")
    ap.add_argument("--configuration-hash", help="encode: the engine's configuration hash")
    ap.add_argument("--strategy-version", help="encode: the engine's strategy version")
    ap.add_argument("--run-id", help="encode: the run's correlation id")
    ap.add_argument("--first-sequence", type=int, default=2, help="encode: sequence of the first envelope")
    ap.add_argument("--stream", help="encode: the new file the envelopes are written to")
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    split = lambda text: [s for s in text.split(",") if s]   # noqa: E731
    try:
        if args.what == "encode":
            if not args.stream or not args.first:
                raise ValueError("encode needs --first and --stream")
            envelopes = encode.encode(
                args.out, args.first, args.asof,
                encode.Identity(args.configuration_hash, args.strategy_version, args.run_id),
                required=split(args.instruments), optional=split(args.optional),
                universe_members=args.universe_members, base=args.base, first_sequence=args.first_sequence)
            print(encode.write_stream(args.stream, envelopes))
            return 0
        if args.what == "sectors":
            print("\n".join(sectors.record_sectors(args.out, args.asof)))
            return 0
        client = massive.Client(massive.load_key(secrets_file=args.secrets_file))
        if args.what == "backfill":
            print(history.backfill(client, args.out, args.asof, args.sessions))
            return 0
        if args.what == "actions":
            first = args.asof - dt.timedelta(days=args.lookback_days)
            print("\n".join(history.record_actions(client, first, args.asof, args.out)))
            return 0
        session, body = steps.find_session(client, args.asof)
        if args.what == "universe":
            path = steps.run_universe(client, session, args.out, grouped_body=body)
        else:
            path = steps.run_bars(client, split(args.instruments), session,
                                  args.out, args.lookback_days)
    except Exception as e:   # includes MissingData, MarketDataError, AlreadyRecorded, CannotAdjust
        print("marketdata failed: {}: {}".format(type(e).__name__, e), file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
