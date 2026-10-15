"""Month-by-month comparison of two universe_parity.py runs' holdings, for
the universe parity study in docs/live-trading/plan.md (section 4).
Ordinary local Python, standard library only; it reads an export and writes
nothing.

Input: the tax study's export format (research/edge-search/tax/tax_drag.py),
kept outside the repository because it is derived from market data: a JSON
object mapping run name -> {"fills": [[unix time, symbol, signed quantity,
price, fee], ...]}, built from each backtest's full order list. Symbols are
QuantConnect security IDs, the same in every run, so stocks are compared by
ID and the bond fund is recognised by the ID's ticker part. The order
list is used rather than the logs because the Free plan caps logs at 10 KB.

For each month of the study it rebuilds what each run held once that
month's fills were done (so a stock bought a few days late, once it had a
price, counts in its month), and reports the stocks the two books share,
out of how many each holds, and whether both held the bond fund (the
combination holds IEF only in its risk-off months). A month in which a run
holds nothing is refused: it means the export is incomplete.

Usage: python3 parity_compare.py EXPORT.json BASELINE_RUN OTHER_RUN [FIRST LAST]
       FIRST and LAST are YYYY-MM, by default 2016-01 and 2026-09.
"""
import json
import sys
from datetime import datetime, timezone

FIRST, LAST = "2016-01", "2026-09"


def months(first, last):
    """Every "YYYY-MM" from ``first`` to ``last``, inclusive."""
    y, m = (int(x) for x in first.split("-"))
    out = []
    while "{:04d}-{:02d}".format(y, m) <= last:
        out.append("{:04d}-{:02d}".format(y, m))
        y, m = (y + 1, 1) if m == 12 else (y, m + 1)
    return out


def _month_of(t):
    return datetime.fromtimestamp(t, tz=timezone.utc).strftime("%Y-%m")


def _ticker(symbol):
    """The ticker part of a symbol: the export's QuantConnect security IDs
    are "<ticker at first listing> <code>"; a bare ticker is itself."""
    return symbol.split()[0] if symbol.split() else symbol


def holdings_by_month(fills, study_months, bonds=("IEF",)):
    """{month: (stocks held, bond fund held)} after each study month's fills,
    fills before the study included. Refuses a month with nothing held.
    Symbols may be tickers or security IDs; the bond fund is matched by
    ticker."""
    qty = {}
    ordered = sorted(fills, key=lambda f: f[0])
    i, out = 0, {}
    for month in study_months:
        while i < len(ordered) and _month_of(ordered[i][0]) <= month:
            _, sym, q = ordered[i][:3]
            qty[sym] = qty.get(sym, 0) + q
            i += 1
        held = {s for s, q in qty.items() if abs(q) > 1e-9}
        if not held:
            raise ValueError("nothing held in {}: the export is incomplete".format(month))
        bond = {s for s in held if _ticker(s) in bonds}
        out[month] = (held - bond, bool(bond))
    return out


def compare(a, b):
    """One row per month of ``a``, in month order; ``b`` covers the same months."""
    rows = []
    for month in sorted(a):
        (sa, off_a), (sb, off_b) = a[month], b[month]
        rows.append({"month": month, "common": len(sa & sb), "stocks_a": len(sa), "stocks_b": len(sb),
                     "same_risk_off": off_a == off_b})
    return rows


def summary(rows):
    """Averages over the months compared, and the month with the smallest
    share of the first run's stocks also held by the second."""
    if not rows:
        return {"months": 0}
    share = {r["month"]: (r["common"] / r["stocks_a"] if r["stocks_a"] else 1.0) for r in rows}
    worst = min(share, key=share.get)
    return {
        "months": len(rows),
        "mean_common_share": sum(share.values()) / len(share),
        "worst_month": worst,
        "worst_common_share": share[worst],
        "risk_off_differs": sum(1 for r in rows if not r["same_risk_off"]),
    }


def main(argv):
    if len(argv) not in (4, 6):
        raise SystemExit(__doc__)
    first, last = (argv[4], argv[5]) if len(argv) == 6 else (FIRST, LAST)
    with open(argv[1]) as f:
        export = json.load(f)
    study = months(first, last)
    a = holdings_by_month(export[argv[2]]["fills"], study)
    b = holdings_by_month(export[argv[3]]["fills"], study)
    rows = compare(a, b)
    print("month    common  stocks  same_risk_off")
    for r in rows:
        print("{month}  {common:6d}  {stocks_a:2d}/{stocks_b:<2d}  {same_risk_off}".format(**r))
    for key, value in summary(rows).items():
        print("{}: {}".format(key, round(value, 4) if isinstance(value, float) else value))


if __name__ == "__main__":
    main(sys.argv)
