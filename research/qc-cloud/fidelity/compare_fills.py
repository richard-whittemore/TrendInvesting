"""Compare a local LEAN backtest of the research algorithm with a Go engine
journal, fill by fill (README.md, "Checking fidelity against the Go engine").

Standard library only. Usage:

    python3 compare_fills.py <lean-backtest-dir> <go-journal.jsonl>
        [--price-tolerance 0.002] [--quantity-tolerance 0] [--show 10]

<lean-backtest-dir> is one run's folder under a LEAN project's backtests/,
holding LEAN's <id>.json (orders, statistics) and <id>-order-events.json.
The journal's execution.fill envelopes supply the reference: filled_at,
kind (entry, add, stop, exit), quantity and price, all in the split-adjusted
view (ADR 0004), which is also the view the research algorithm's
SplitAdjusted subscription trades in.

Fills are grouped by (session date, kind), since the two sides split the
same trade differently: the Go engine reports an Exit Channel exit as one
fill for every Unit it closes, where the research algorithm rests one Exit
Order per Unit. A group matches when both sides have it, their total
quantities agree within --quantity-tolerance (a fraction) and their
volume-weighted prices within --price-tolerance (a fraction). The research
side's kind is read from its order tag: entry, add, stop (a Unit's
Protective Stop) or exit (the Exit Channel). Exit status is 0 when every
group matches, 1 otherwise.
"""

import argparse
import glob
import json
import os
import sys
from collections import OrderedDict
from datetime import datetime, timezone

KINDS = ("entry", "add", "stop", "exit")


def research_kind(tag):
    """The fill kind a research order's tag names, or "other" (a
    liquidation, for example) when it names none of the four."""
    prefix = (tag or "").split(":", 1)[0]
    return prefix if prefix in KINDS else "other"


def load_research_fills(backtest_dir):
    """[(date, kind, quantity, price)] for every fill LEAN reported, in
    order. quantity is unsigned."""
    events_path = _one(backtest_dir, "*-order-events.json")
    result_path = [p for p in glob.glob(os.path.join(backtest_dir, "*.json"))
                   if p[-5:] == ".json" and "-" not in os.path.basename(p)[:-5]]
    if len(result_path) != 1:
        raise SystemExit("expected one <id>.json result file in {}".format(backtest_dir))
    with open(result_path[0]) as source:
        orders = json.load(source)["orders"]
    with open(events_path) as source:
        events = json.load(source)
    fills = []
    for event in events:
        if event.get("status") not in ("filled", "partiallyFilled") or not event.get("fillQuantity"):
            continue
        order = orders.get(str(event["orderId"]), {})
        when = datetime.fromtimestamp(event["time"], tz=timezone.utc).date()
        fills.append((when, research_kind(order.get("tag")), abs(float(event["fillQuantity"])),
                      float(event["fillPrice"])))
    return fills


def load_research_statistics(backtest_dir):
    """(statistics from the equity marks, LEAN's own statistics). The marks
    are LEAN's daily Strategy Equity closes, measured exactly as load_go
    measures the journal's, so the two sides are compared like for like."""
    path = [p for p in glob.glob(os.path.join(backtest_dir, "*.json"))
            if "-" not in os.path.basename(p)[:-5]]
    with open(path[0]) as source:
        result = json.load(source)
    stats = result.get("statistics", {})
    values = result["charts"]["Strategy Equity"]["series"]["Equity"]["values"]
    marks = [(datetime.fromtimestamp(v[0], tz=timezone.utc), float(v[-1])) for v in values]
    lean = {"net profit": stats.get("Net Profit"), "CAGR": stats.get("Compounding Annual Return"),
            "max drawdown": stats.get("Drawdown"), "end equity": stats.get("End Equity"),
            "fees": stats.get("Total Fees")}
    return measure(marks), lean


def measure(marks):
    """Net profit, ADR 0012's CAGR ((E1/E0)^(1/Y) - 1, Y in years of
    365.25 days) and max drawdown over [(datetime, equity)] in order."""
    if len(marks) < 2:
        return {}
    (t0, e0), (t1, e1) = marks[0], marks[-1]
    years = (t1 - t0).total_seconds() / (86400 * 365.25)
    peak, worst = e0, 0.0
    for _, equity in marks:
        peak = max(peak, equity)
        worst = max(worst, (peak - equity) / peak)
    return {"net profit": "{:.3f}%".format((e1 / e0 - 1) * 100),
            "CAGR": "{:.3f}%".format(((e1 / e0) ** (1 / years) - 1) * 100),
            "max drawdown": "{:.3f}%".format(worst * 100), "end equity": "{:.2f}".format(e1)}


def load_go(journal_path):
    """(fills, statistics) from a Go engine journal: fills as
    [(date, kind, quantity, price)]; statistics from its account.snapshot
    equity marks from the run's own start (its configuration's event
    time) onward, with ADR 0012's CAGR and max drawdown."""
    fills, marks, start = [], [], None
    with open(journal_path) as source:
        for line in source:
            envelope = json.loads(line).get("envelope") or {}
            kind = envelope.get("type")
            payload = envelope.get("payload") or {}
            if kind == "strategy.configuration":
                start = envelope["event_time"]
            elif kind == "execution.fill":
                fills.append((_date(payload["filled_at"]), payload["kind"],
                              float(payload["quantity"]), float(payload["price"])))
            elif kind == "account.snapshot" and (start is None or envelope["event_time"] >= start):
                marks.append((_instant(envelope["event_time"]), float(payload["equity"])))
    return fills, measure(marks)


def group(fills):
    """OrderedDict (date, kind) -> [count, total quantity, value], in date
    order."""
    groups = OrderedDict()
    for when, kind, quantity, price in sorted(fills, key=lambda f: (f[0], KINDS.index(f[1])
                                                                     if f[1] in KINDS else 9)):
        entry = groups.setdefault((when, kind), [0, 0.0, 0.0])
        entry[0] += 1
        entry[1] += quantity
        entry[2] += quantity * price
    return groups


def compare(research, go, price_tolerance, quantity_tolerance):
    """[(key, research group or None, go group or None, verdict)] over the
    union of both sides' groups, in date order."""
    r_groups, g_groups = group(research), group(go)
    keys = sorted(set(r_groups) | set(g_groups),
                  key=lambda k: (k[0], KINDS.index(k[1]) if k[1] in KINDS else 9))
    rows = []
    for key in keys:
        r, g = r_groups.get(key), g_groups.get(key)
        if r is None or g is None:
            verdict = "only-go" if r is None else "only-research"
        else:
            r_price, g_price = r[2] / r[1], g[2] / g[1]
            quantity_ok = abs(r[1] - g[1]) <= quantity_tolerance * g[1]
            price_ok = abs(r_price - g_price) <= price_tolerance * g_price
            verdict = "match" if quantity_ok and price_ok else \
                "mismatch:" + ",".join(n for n, ok in (("quantity", quantity_ok), ("price", price_ok))
                                       if not ok)
        rows.append((key, r, g, verdict))
    return rows


def _fmt_group(g):
    if g is None:
        return "{:>34}".format("-")
    return "{:>2}x {:>12.0f} @ {:>13.8f}".format(g[0], g[1], g[2] / g[1])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("backtest_dir")
    parser.add_argument("journal")
    parser.add_argument("--price-tolerance", type=float, default=0.002)
    parser.add_argument("--quantity-tolerance", type=float, default=0.0)
    parser.add_argument("--show", type=int, default=10,
                        help="rows to print in full from the start (every mismatch is printed)")
    args = parser.parse_args(argv)

    research = load_research_fills(args.backtest_dir)
    go, go_stats = load_go(args.journal)
    rows = compare(research, go, args.price_tolerance, args.quantity_tolerance)

    def counts(fills):
        return " ".join("{}={}".format(k, sum(1 for f in fills if f[1] == k))
                        for k in KINDS + ("other",))

    print("fills: research {} ({})".format(len(research), counts(research)))
    print("fills: go       {} ({})".format(len(go), counts(go)))
    print("{:<10} {:<5} {:>34}   {:>34}   {}".format("date", "kind", "research", "go", "verdict"))
    for index, (key, r, g, verdict) in enumerate(rows):
        if index < args.show or verdict != "match":
            print("{} {:<5} {}   {}   {}".format(key[0], key[1], _fmt_group(r), _fmt_group(g), verdict))
    matched = sum(1 for row in rows if row[3] == "match")
    print("groups: {} matched of {} ({} research-only, {} go-only, {} mismatched)".format(
        matched, len(rows), sum(1 for row in rows if row[3] == "only-research"),
        sum(1 for row in rows if row[3] == "only-go"),
        sum(1 for row in rows if row[3].startswith("mismatch"))))
    research_stats, lean_stats = load_research_statistics(args.backtest_dir)
    print("from equity marks (ADR 0012):")
    for name in ("net profit", "CAGR", "max drawdown", "end equity"):
        print("  {:<13} research {:>14}   go {:>14}".format(name, str(research_stats.get(name)),
                                                          str(go_stats.get(name))))
    print("LEAN's own statistics for the research run: " + ", ".join(
        "{} {}".format(k, v) for k, v in lean_stats.items()))
    return 0 if matched == len(rows) else 1


def _one(directory, pattern):
    paths = glob.glob(os.path.join(directory, pattern))
    if len(paths) != 1:
        raise SystemExit("expected one {} in {}, found {}".format(pattern, directory, len(paths)))
    return paths[0]


def _instant(text):
    return datetime.strptime(text[:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc)


def _date(text):
    return _instant(text).date()


if __name__ == "__main__":
    sys.exit(main())
