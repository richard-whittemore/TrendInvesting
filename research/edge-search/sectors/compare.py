"""Compare Morningstar sectors (snapshot.py's log) with the GICS sectors in
an iShares holdings file, for the sector-label check in
docs/live-trading/plan.md (section 4). Ordinary local Python, standard
library only; it reads two downloaded files (kept outside the repository:
both are derived from market data) and writes nothing.

GICS sectors are mapped onto Morningstar's eleven by name. The mapping is a
judgment, not an official concordance; a disagreement means the cap would
count that stock in a different sector, which is what the check measures.

Usage: python3 compare.py SNAPSHOT.log ISHARES_HOLDINGS.csv
"""
import csv
import io
import re
import sys
from collections import Counter

#: Morningstar sector codes (QuantConnect's MorningstarSectorCode).
MORNINGSTAR = {101: "Basic Materials", 102: "Consumer Cyclical", 103: "Financial Services",
               104: "Real Estate", 205: "Consumer Defensive", 206: "Healthcare", 207: "Utilities",
               308: "Communication Services", 309: "Energy", 310: "Industrials", 311: "Technology"}

#: The GICS sector names iShares uses, mapped to the nearest Morningstar sector.
GICS_TO_MORNINGSTAR = {"Materials": 101, "Consumer Discretionary": 102, "Financials": 103,
                       "Real Estate": 104, "Consumer Staples": 205, "Health Care": 206, "Utilities": 207,
                       "Communication": 308, "Energy": 309, "Industrials": 310,
                       "Information Technology": 311}

SECT = re.compile(r"SECT (\S+)")


def norm(ticker):
    """A ticker without punctuation, upper case: QuantConnect writes "BRK.B",
    iShares "BRKB"."""
    return re.sub(r"[^A-Z0-9]", "", ticker.upper())


def read_snapshot(lines):
    """{ticker: Morningstar code or None} from snapshot.py's log lines."""
    out = {}
    for line in lines:
        m = SECT.search(line)
        if not m:
            continue
        for item in filter(None, m.group(1).split(";")):
            ticker, code = item.rsplit(":", 1)
            out[norm(ticker)] = None if code == "-" else int(code)
    return out


def read_ishares(text):
    """{ticker: GICS sector} for the equity rows of an iShares holdings CSV."""
    lines = text.splitlines()
    start = next(i for i, l in enumerate(lines) if l.startswith("Ticker,"))
    out = {}
    for row in csv.DictReader(io.StringIO("\n".join(lines[start:]))):
        if row.get("Asset Class") == "Equity" and row.get("Ticker"):
            out[norm(row["Ticker"])] = row["Sector"]
    return out


def compare(ms, gics):
    """Agreement between the two labels over the snapshot's universe."""
    both = [t for t in sorted(ms) if t in gics and ms[t] is not None]
    disagree = [(t, ms[t], gics[t]) for t in both if GICS_TO_MORNINGSTAR.get(gics[t]) != ms[t]]
    return {
        "universe": len(ms),
        "labelled_both": len(both),
        "agree": len(both) - len(disagree),
        "disagree": disagree,
        "not_in_fund": sorted(t for t in ms if t not in gics or ms[t] is None),
    }


def main(argv):
    if len(argv) != 3:
        raise SystemExit(__doc__)
    with open(argv[1]) as f:
        ms = read_snapshot(f)
    with open(argv[2]) as f:
        gics = read_ishares(f.read())
    r = compare(ms, gics)
    print("universe {universe}, labelled by both {labelled_both}, agree {agree}".format(**r))
    print("not in the fund or no Morningstar sector ({}): {}".format(len(r["not_in_fund"]),
                                                                      " ".join(r["not_in_fund"])))
    pairs = Counter((MORNINGSTAR.get(m, m), g) for _, m, g in r["disagree"])
    print("disagreements by pair:")
    for (m, g), n in pairs.most_common():
        print("  {:3d}  Morningstar {} / GICS {}".format(n, m, g))
    for t, m, g in r["disagree"]:
        print("  {}: Morningstar {}, GICS {}".format(t, MORNINGSTAR.get(m, m), g))


if __name__ == "__main__":
    main(sys.argv)
