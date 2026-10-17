"""python3 -m marketdata {universe|bars|backfill|actions|sectors|encode|retract} --out DIR [--asof YYYY-MM-DD]
    [--instruments SPY,IEF,SHY] [--sessions N] [--lookback-days N] [--settle-minutes N]

--asof defaults to today's New York date. A session is refused (exit 1,
nothing written) until its close plus the settle delay.

Exit status 1 with a message on stderr when data is missing (nothing is
written), for the caller to alert on. The Massive key comes from
MASSIVE_API_KEY, --secrets-file, or the Mac Keychain, and is never printed;
``sectors`` needs no key, and ``encode`` reads only the recordings in DIR,
never the network, and writes the engines' input stream to --stream.
``retract --session D --reason TEXT`` records that the exchange cancelled
the session D (no key, no network); the encoder then writes it as a
market.session.retracted."""
import argparse
import datetime as dt
import logging
import shlex
import sys

from . import encode, history, massive, nyse_calendar, sectors, steps


def parse(argv, now=steps._now):
    ap = argparse.ArgumentParser(prog="marketdata")
    ap.add_argument("what", choices=("universe", "bars", "backfill", "actions", "sectors", "encode", "retract"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--asof", type=dt.date.fromisoformat, default=None)
    ap.add_argument("--instruments", default="SPY,IEF,SHY")
    ap.add_argument("--secrets-file")
    ap.add_argument("--lookback-days", type=int, default=400)
    ap.add_argument("--sessions", type=int, default=253, help="backfill: sessions of history to record")
    ap.add_argument("--settle-minutes", type=int, default=int(steps.SETTLE.total_seconds() // 60),
                    help="minutes after the close before a session may be recorded")
    ap.add_argument("--first", type=dt.date.fromisoformat, help="encode: first session written")
    ap.add_argument("--session", type=dt.date.fromisoformat, help="retract: the session the exchange cancelled")
    ap.add_argument("--reason", default="", help="retract: why, for the record")
    ap.add_argument("--base", type=dt.date.fromisoformat, help="encode: adjusted views' base (default --first)")
    ap.add_argument("--optional", default="", help="encode: instruments written where they traded")
    ap.add_argument("--universe-members", action="store_true", help="encode: also every universe reference ticker with a bar")
    ap.add_argument("--ranker", help="encode: the compact stream's ranker command, for example "
                                     "'bin/universe-rank -config momentum.json': no member bars, and a close "
                                     "history for each member it ranks at a decision session")
    ap.add_argument("--configuration-hash", help="encode: the engine's configuration hash")
    ap.add_argument("--strategy-version", help="encode: the engine's strategy version")
    ap.add_argument("--run-id", help="encode: the run's correlation id")
    ap.add_argument("--first-sequence", type=int, default=2, help="encode: sequence of the first envelope")
    ap.add_argument("--stream", help="encode: the new file the envelopes are written to")
    ap.add_argument("--engine", choices=encode.ENGINES, default="momentum",
                    help="encode: the engine whose inputs are written (boost: one instrument's bars and closes)")
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
    split = lambda text: [s for s in text.split(",") if s]   # noqa: E731
    try:
        if args.what == "encode":
            if not args.stream or not args.first:
                raise ValueError("encode needs --first and --stream")
            excluded = {}
            envelopes = encode.encode(
                args.out, args.first, args.asof,
                encode.Identity(args.configuration_hash, args.strategy_version, args.run_id),
                required=split(args.instruments), optional=split(args.optional),
                universe_members=args.universe_members, base=args.base, first_sequence=args.first_sequence,
                engine=args.engine, excluded=excluded,
                ranker=encode.go_ranker(shlex.split(args.ranker)) if args.ranker else None)
            for ticker in sorted(excluded):
                for run in excluded[ticker]:
                    print("marketdata: excluded {} from {} to {} ({} sessions): {}".format(
                        ticker, run.first, run.last, run.sessions, run.reason), file=sys.stderr)
            print(encode.write_stream(args.stream, envelopes))
            return 0
        if args.what == "retract":
            if not args.session:
                raise ValueError("retract needs --session")
            print(encode.record_retraction(args.out, args.session, args.reason))
            return 0
        if args.what == "sectors":
            day = args.asof
            while not nyse_calendar.is_trading_day(day):
                day -= dt.timedelta(days=1)
            print("\n".join(sectors.record_sectors(args.out, day, settle=settle)))
            return 0
        client = massive.Client(massive.load_key(secrets_file=args.secrets_file))
        if args.what == "backfill":
            print(history.backfill(client, args.out, args.asof, args.sessions, settle=settle))
            return 0
        if args.what == "actions":
            first = args.asof - dt.timedelta(days=args.lookback_days)
            print("\n".join(history.record_actions(client, first, args.asof, args.out, settle=settle)))
            return 0
        session, body = steps.find_session(client, args.asof, settle=settle)
        if args.what == "universe":
            path = steps.run_universe(client, session, args.out, grouped_body=body, settle=settle)
        else:
            path = steps.run_bars(client, split(args.instruments), session,
                                  args.out, args.lookback_days, settle=settle)
    except Exception as e:   # includes MissingData, MarketDataError, AlreadyRecorded, CannotAdjust
        print("marketdata failed: {}: {}".format(type(e).__name__, e), file=sys.stderr)
        return 1
    print(path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
