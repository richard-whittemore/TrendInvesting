"""Combine strategies' QuantConnect equity curves into one portfolio, as
separate accounts that are never rebalanced into one another (an IRA's money
cannot move to a taxable account and back), and measure the result.

Research only (docs/research/boost-followup-2026-09.md, "The strategies
together"). Inputs are exported "Strategy Equity" charts, one JSON list of
[unix_seconds, equity] per run (not committed: they are backtest output).
Standard library only.
"""
import json
import math
import os
import sys
from datetime import datetime, timezone


#: Unix seconds beyond this are taken to be milliseconds (year 5138).
MAX_SECONDS = 1e11


def month_ends(points):
    """{(year, month): last equity of that month} from [unix seconds,
    equity] points. Equity at or below zero is an error: a wiped-out account
    cannot be compounded, and dropping the mark would hide the loss."""
    out = {}
    for t, v in sorted(points):
        if t > MAX_SECONDS:
            raise ValueError("timestamp {} looks like milliseconds; export unix seconds".format(t))
        d = datetime.fromtimestamp(t, tz=timezone.utc)
        value = float(v) if v is not None else float("nan")
        if not math.isfinite(value) or value <= 0:
            raise ValueError("equity {} on {}: only a positive, finite mark can be blended".format(v, d.date()))
        out[(d.year, d.month)] = value
    return out


def growth(curve, months):
    """Each month's value of 1 invested at the first of ``months``; a month
    the curve lacks is an error, not a silent gap."""
    missing = [m for m in months if m not in curve]
    if missing:
        raise ValueError("no month-end value for {}".format(missing[:3]))
    first = curve[months[0]]
    return [curve[m] / first for m in months]


def combine(paths, weights):
    """Value of a book split by ``weights`` at the start and then left alone:
    each part compounds on its own path."""
    total = float(sum(weights))
    return [sum(w / total * p[i] for p, w in zip(paths, weights)) for i in range(len(paths[0]))]


def cagr(path, months):
    return path[-1] ** (12.0 / months) - 1.0


def max_drawdown(path):
    peak, worst = 0.0, 0.0
    for v in path:
        peak = max(peak, v)
        worst = max(worst, 1.0 - v / peak)
    return worst


def returns(path):
    return [path[i] / path[i - 1] - 1.0 for i in range(1, len(path))]


def correlation(a, b):
    n = len(a)
    if n < 2:
        return float("nan")
    ma, mb = sum(a) / n, sum(b) / n
    cov = sum((x - ma) * (y - mb) for x, y in zip(a, b))
    va = sum((x - ma) ** 2 for x in a)
    vb = sum((y - mb) ** 2 for y in b)
    return cov / math.sqrt(va * vb) if va and vb else float("nan")


def longest_behind(path, benchmark):
    """The most months the path spent below its best level relative to
    ``benchmark`` (how long it trailed before catching up)."""
    best, run, longest = 0.0, 0, 0
    for p, b in zip(path, benchmark):
        rel = p / b
        if rel >= best:
            best, run = rel, 0
        else:
            run += 1
            longest = max(longest, run)
    return longest


def span(first, last):
    """Every (year, month) from ``first`` to ``last`` inclusive."""
    if not (1 <= first[1] <= 12 and 1 <= last[1] <= 12):
        raise ValueError("months must be 1..12: {}..{}".format(first, last))
    y, m = first
    out = []
    while (y, m) <= last:
        out.append((y, m))
        y, m = (y + 1, 1) if m == 12 else (y, m + 1)
    return out


def report(curves, books, first, last, benchmark="SPY"):
    months = span(first, last)
    if len(months) < 2:
        raise ValueError("period {}..{} needs at least two month-ends".format(first, last))
    paths = {name: growth(c, months) for name, c in curves.items()}
    bench = paths[benchmark]
    n = len(months) - 1
    rows = []
    for label, parts in books:
        missing = [k for k in parts if k not in paths]
        if missing:
            raise ValueError("book {!r} names curves not loaded for this period: {}".format(label, ", ".join(missing)))
        if sum(parts.values()) <= 0:
            raise ValueError("book {!r} has no positive total weight".format(label))
        path = combine([paths[k] for k in parts], list(parts.values()))
        rows.append((label, cagr(path, n), max_drawdown(path), longest_behind(path, bench)))
    corr = {}
    names = list(curves)
    for i, a in enumerate(names):
        for b in names[i + 1:]:
            corr[(a, b)] = correlation(returns(paths[a]), returns(paths[b]))
    return rows, corr


def load(path):
    with open(path) as f:
        return month_ends(json.load(f))


def main(spec_path, export_dir):
    """``spec_path``: JSON {"curves": {name: file in ``export_dir``},
    "periods": [[label, [y, m], [y, m], {book name: curve name}]], "books":
    [[label, {book name: weight}]]}. The first month of a period is the base
    month-end; its returns start the month after."""
    with open(spec_path) as f:
        spec = json.load(f)
    for label, first, last, use in spec["periods"]:
        curves = {name: load(os.path.join(export_dir, spec["curves"][src])) for name, src in use.items()}
        rows, corr = report(curves, [(b, w) for b, w in spec["books"]], tuple(first), tuple(last))
        print("\n{} ({}-{:02d} to {}-{:02d})".format(label, first[0], first[1], last[0], last[1]))
        print("{:<44} {:>8} {:>9} {:>14}".format("book", "CAGR", "worst DD", "months behind"))
        for name, c, dd, behind in rows:
            print("{:<44} {:>7.2f}% {:>8.1f}% {:>14}".format(name, 100 * c, 100 * dd, behind))
        print("monthly-return correlations: " + ", ".join(
            "{}/{} {:.2f}".format(a, b, v) for (a, b), v in corr.items()))


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: blend.py SPEC.json EXPORT_DIR")
    main(*sys.argv[1:])
