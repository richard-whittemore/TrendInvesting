"""Compare Morningstar sectors (snapshot.py's log) with the GICS sectors in
an iShares holdings file, for the sector-label check in
docs/live-trading/plan.md (section 4). Ordinary local Python, standard
library only; it reads two downloaded files (kept outside the repository:
both are derived from market data) and writes nothing.

GICS sectors are mapped onto Morningstar's eleven by name. The mapping is a
judgment, not an official concordance; a disagreement means the cap would
count that stock in a different sector, which is what the check measures.

Usage: python3 compare.py SNAPSHOT.log ISHARES_HOLDINGS.csv COUNT
       COUNT is the snapshot run's "Count" runtime statistic; a log that
       does not hold exactly that many names is refused (the Free plan's
       log cap can cut it short).
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


def read_snapshot(lines, expected=None):
    """({ticker: Morningstar code or None}, items skipped) from snapshot.py's
    log lines. Each line must end with "|"; on a line without it (cut short)
    the last item is skipped, since it may be cut too ("MSFT:311" to
    "MSFT:3"). A malformed item, or a code that is not a Morningstar sector,
    is skipped as well. Every skip is counted. With ``expected`` (which must
    be positive), anything other than exactly that many names, or any skip,
    is refused."""
    if expected is not None and expected <= 0:
        raise ValueError("expected count {} is not positive: the snapshot run found no names".format(expected))
    out, skipped = {}, 0
    for line in lines:
        m = SECT.search(line)
        if not m:
            continue
        body = m.group(1)
        items = list(filter(None, body.rstrip("|").split(";")))
        if not body.endswith("|") and items:
            items.pop()
            skipped += 1
        for item in items:
            ticker, sep, code = item.rpartition(":")
            if not sep or not ticker or not (code == "-" or (code.isdigit() and int(code) in MORNINGSTAR)):
                skipped += 1
                continue
            out[norm(ticker)] = None if code == "-" else int(code)
    if expected is not None and (len(out) != expected or skipped):
        raise ValueError("snapshot holds {} names ({} skipped), expected {}: the log is incomplete"
                         .format(len(out), skipped, expected))
    return out, skipped


def read_ishares(text):
    """{ticker: GICS sector} for the equity rows of an iShares holdings CSV."""
    lines = [l.lstrip("\ufeff") for l in text.splitlines()]
    start = next((i for i, l in enumerate(lines) if l.startswith("Ticker,")), None)
    if start is None:
        raise ValueError("no 'Ticker,' header: not an iShares holdings file")
    out = {}
    for row in csv.DictReader(io.StringIO("\n".join(lines[start:]))):
        if row.get("Asset Class") == "Equity" and row.get("Ticker"):
            out[norm(row["Ticker"])] = row["Sector"]
    return out


def compare(ms, gics):
    """Agreement between the two labels over the snapshot's universe."""
    both = [t for t in sorted(ms) if t in gics and ms[t] is not None]
    unmapped = [(t, gics[t]) for t in both if gics[t] not in GICS_TO_MORNINGSTAR]
    mapped = [t for t in both if gics[t] in GICS_TO_MORNINGSTAR]
    disagree = [(t, ms[t], gics[t]) for t in mapped if GICS_TO_MORNINGSTAR[gics[t]] != ms[t]]
    return {
        "universe": len(ms),
        "labelled_both": len(mapped),
        "agree": len(mapped) - len(disagree),
        "disagree": disagree,
        "unmapped": unmapped,
        "not_in_fund": sorted(t for t in ms if t not in gics or ms[t] is None),
    }


def main(argv):
    if len(argv) != 4:
        raise SystemExit(__doc__)
    try:
        with open(argv[1], encoding="utf-8") as f:
            ms, _ = read_snapshot(f, expected=int(argv[3]))
        with open(argv[2], encoding="utf-8-sig") as f:
            gics = read_ishares(f.read())
    except ValueError as e:
        raise SystemExit("refused: {}".format(e))
    r = compare(ms, gics)
    if r["unmapped"]:
        print("GICS labels with no mapping ({}): {}".format(
            len(r["unmapped"]), ", ".join("{} ({})".format(t, g) for t, g in r["unmapped"])))
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
