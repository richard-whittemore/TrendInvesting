# Research script, not production code. Compares each month's universe logged
# by qc_universe.py on QuantConnect with decision D8's universe built from
# Massive by massive.py (docs/live-trading/plan.md, section 4): how many of
# QuantConnect's names the D8 rule also picks, and which differ.
import json
import re
import sys

_LINE = re.compile(r"UNIV (\d{4}-\d{2}) (.*)\|\s*$")


def _norm(ticker):
    """One spelling for share classes: QuantConnect and Massive may write
    BRK.B or BRK/B."""
    return ticker.replace("/", ".").replace("-", ".")


def parse_qc_log(lines):
    """{month: [tickers]} from log lines; a line without its closing "|" (cut
    short) is ignored, which check_counts then catches."""
    months = {}
    for line in lines:
        m = _LINE.search(line)
        if m:
            months.setdefault(m.group(1), []).extend(m.group(2).split(";"))
    return months


def check_counts(months, counts):
    """Raise ValueError unless the log and the counts name the same months
    and every month holds exactly its count."""
    missing = sorted(set(months) - set(counts))
    if missing:
        raise ValueError("no count for {}; cannot tell whether the log was cut short".format(", ".join(missing)))
    for month, n in counts.items():
        got = len(months.get(month, ()))
        if got != n:
            raise ValueError("{}: {} names in the log, {} selected; the log was cut short".format(month, got, n))


def overlap(qc, live):
    a, b = {_norm(t) for t in qc}, {_norm(t) for t in live}
    return {"qc": len(a), "live": len(b), "shared": len(a & b),
            "share": len(a & b) / len(a) if a else 0.0,
            "only_qc": sorted(a - b), "only_live": sorted(b - a)}


def main(argv):
    """compare.py QC_LOG_JSON COUNTS_JSON LIVE_JSON...: QC_LOG_JSON is a list
    of log lines, COUNTS_JSON {month: count}, each LIVE_JSON a massive.py
    output file. Prints one row per month."""
    with open(argv[0]) as f:
        months = parse_qc_log(json.load(f))
    with open(argv[1]) as f:
        check_counts(months, json.load(f))
    for path in argv[2:]:
        with open(path) as f:
            live = json.load(f)
        row = overlap(months[live["month"]], live["names"])
        print("{} {} shared {}/{} ({:.1%})".format(live["month"], live["session"], row["shared"], row["qc"],
                                                    row["share"]))
        print("  only QC: " + " ".join(row["only_qc"]))
        print("  only live: " + " ".join(row["only_live"]))


if __name__ == "__main__":
    main(sys.argv[1:])
