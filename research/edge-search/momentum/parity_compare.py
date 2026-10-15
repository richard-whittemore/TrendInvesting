"""Month-by-month comparison of two universe_parity.py runs' picks, for the
universe parity study in docs/live-trading/plan.md (section 4). Ordinary
local Python, standard library only; it reads the runs' downloaded logs and
writes nothing.

Each rebalance logs one line, "PICKS YYYY-MM-DD risk_off=0|1 TICKER:w,...".
For every month both runs logged, it reports the stocks the two books share,
out of how many each holds, and the weight overlap: the sum over every
holding of the smaller of the two weights, bonds included (0.98 when the two
books are identical, since 2% is kept in cash).

Usage: python3 parity_compare.py BASELINE.log OTHER.log
"""
import re
import sys

LINE = re.compile(r"PICKS (\d{4})-(\d{2})-\d{2} risk_off=([01]) (\S*)")


def read_picks(lines):
    """{"YYYY-MM": (risk_off, {ticker: weight})} from a log's PICKS lines.
    A month logged twice is refused: each run rebalances once a month."""
    months = {}
    for line in lines:
        m = LINE.search(line)
        if not m:
            continue
        month = "{}-{}".format(m.group(1), m.group(2))
        if month in months:
            raise ValueError("month {} is logged twice".format(month))
        weights = {}
        for pair in filter(None, m.group(4).split(",")):
            name, w = pair.rsplit(":", 1)
            weights[name] = float(w)
        months[month] = (m.group(3) == "1", weights)
    return months


def compare(a, b, bonds=("IEF",)):
    """One row per month both runs logged, in month order."""
    rows = []
    for month in sorted(set(a) & set(b)):
        (off_a, wa), (off_b, wb) = a[month], b[month]
        sa = {n for n in wa if n not in bonds}
        sb = {n for n in wb if n not in bonds}
        rows.append({
            "month": month,
            "common": len(sa & sb),
            "stocks_a": len(sa),
            "stocks_b": len(sb),
            "weight_overlap": sum(min(wa[n], wb[n]) for n in set(wa) & set(wb)),
            "same_risk_off": off_a == off_b,
        })
    return rows


def unmatched(a, b):
    """Months only the first run logged, and months only the second did."""
    return sorted(set(a) - set(b)), sorted(set(b) - set(a))


def summary(rows):
    """Averages over the months compared, and the month with the least
    weight overlap."""
    if not rows:
        return {"months": 0}
    share = [r["common"] / r["stocks_a"] if r["stocks_a"] else 1.0 for r in rows]
    worst = min(rows, key=lambda r: r["weight_overlap"])
    return {
        "months": len(rows),
        "mean_common_share": sum(share) / len(share),
        "mean_weight_overlap": sum(r["weight_overlap"] for r in rows) / len(rows),
        "worst_month": worst["month"],
        "worst_weight_overlap": worst["weight_overlap"],
        "risk_off_differs": sum(1 for r in rows if not r["same_risk_off"]),
    }


def main(argv):
    if len(argv) != 3:
        raise SystemExit(__doc__)
    with open(argv[1]) as f:
        a = read_picks(f)
    with open(argv[2]) as f:
        b = read_picks(f)
    rows = compare(a, b)
    print("month    common  stocks  weight_overlap  same_risk_off")
    for r in rows:
        print("{month}  {common:6d}  {stocks_a:2d}/{stocks_b:<2d}  {weight_overlap:14.3f}  {same_risk_off}".format(**r))
    only_a, only_b = unmatched(a, b)
    print("only in {}: {}".format(argv[1], only_a or "none"))
    print("only in {}: {}".format(argv[2], only_b or "none"))
    for key, value in summary(rows).items():
        print("{}: {}".format(key, round(value, 4) if isinstance(value, float) else value))


if __name__ == "__main__":
    main(sys.argv)
