"""Measure Python-to-Go round trips over the Unix-socket transport.

    python3 bench.py --socket /tmp/trend-spike.sock --rounds 500 --universe 1000

Prints one row in the same shape as the Go client's, so the two are directly
comparable in docs/adr/0014-lean-go-transport.md.
"""

import argparse
import json
import sys
import time

import client as spike


def percentile(sorted_samples, p):
    """Nearest-rank percentile: never invents a value that was not measured."""
    rank = (p * len(sorted_samples) + 99) // 100
    rank = max(1, min(rank, len(sorted_samples)))
    return sorted_samples[rank - 1]


def summarise(samples):
    ordered = sorted(samples)
    return {
        "n": len(ordered),
        "min": ordered[0],
        "median": percentile(ordered, 50),
        "p95": percentile(ordered, 95),
        "p99": percentile(ordered, 99),
        "max": ordered[-1],
        "mean": sum(ordered) / len(ordered),
    }


def run(path, rounds, warmup, universe, label, transport_timeout):
    sequence = 1
    samples = []
    request_bytes = 0
    with spike.Client(path, timeout=transport_timeout) as conn:
        for _ in range(warmup):
            conn.decide(spike.envelope("market.bar.completed", sequence,
                                       spike.bar_payload(universe, sequence)))
            sequence += 1
        for _ in range(rounds):
            bar = spike.envelope("market.bar.completed", sequence,
                                 spike.bar_payload(universe, sequence))
            if request_bytes == 0:
                request_bytes = len(json.dumps(bar, separators=(",", ":")).encode()) + 1
            started = time.perf_counter()
            conn.decide(bar)
            samples.append((time.perf_counter() - started) * 1000.0)
            sequence += 1

    stats = summarise(samples)
    # Serialising the envelope happens inside the measured exchange, so report
    # it separately: it tells the ADR how much of the round trip is Python's
    # JSON encoder rather than the transport.
    reference = spike.envelope("market.bar.completed", 1, spike.bar_payload(universe, 1))
    encode_samples = []
    for _ in range(50):
        started = time.perf_counter()
        json.dumps(reference, separators=(",", ":")).encode("utf-8")
        encode_samples.append((time.perf_counter() - started) * 1000.0)
    encode = summarise(encode_samples)

    print(
        "{} universe={} n={} req={}B min={:.3f}ms median={:.3f}ms p95={:.3f}ms "
        "p99={:.3f}ms max={:.3f}ms mean={:.3f}ms encode_median={:.3f}ms".format(
            label, universe, stats["n"], request_bytes, stats["min"], stats["median"],
            stats["p95"], stats["p99"], stats["max"], stats["mean"], encode["median"],
        )
    )
    stats["encode_median"] = encode["median"]
    stats["request_bytes"] = request_bytes
    return stats


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default="/tmp/trend-spike.sock")
    parser.add_argument("--rounds", type=int, default=500)
    parser.add_argument("--warmup", type=int, default=50)
    parser.add_argument("--universe", type=int, default=1000)
    parser.add_argument("--label", default="python-client")
    parser.add_argument("--timeout", type=float, default=30.0,
                        help="socket timeout in seconds")
    args = parser.parse_args(argv)
    try:
        run(args.socket, args.rounds, args.warmup, args.universe, args.label, args.timeout)
    except spike.Unavailable as err:
        print("engine unavailable: {}".format(err), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
