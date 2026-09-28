"""Rebuild the "Boost" re-test with realistic margin rates (see
docs/research/edge-search-2026-09.md, "Boost re-test with realistic margin
rates"). The repository never stores market or economic data, so the rate
table is generated here from a local copy of FRED's TB3MS monthly CSV
(https://fred.stlouisfed.org/series/TB3MS, header "observation_date,TB3MS").

Recipe, exactly as used for the reported +6.85% (1999-2015) and +16.18%
(2016 to mid-2026) runs:
  * RATES maps YYYYMM -> that month's TB3MS value / 100, for 1998 onward;
    a missing month uses the latest earlier month.
  * The daily financing charge on borrowed exposure is
    borrowed * (RATES[month] + MARGIN_SPREAD) / 252, MARGIN_SPREAD = 1.5%.
  * Everything else is main.py in MODE "boost" (BOOST_SIZE 1.5).

Usage: python3 build_realistic_boost.py TB3MS.csv > boost_realistic.py
       (add START_DATE/END_DATE/MEASURE_FROM edits for the held-out run).
"""
import csv
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def build(tb3ms_path):
    rates = {}
    with open(tb3ms_path) as f:
        for row in csv.reader(f):
            if not row or row[0].startswith("observation") or row[1] == ".":
                continue
            year, month, _ = row[0].split("-")
            if int(year) >= 1998:
                rates[int(year) * 100 + int(month)] = float(row[1]) / 100
    src = open(os.path.join(HERE, "main.py")).read()
    table = "RATES = {" + ", ".join("{}: {:.4f}".format(k, v) for k, v in sorted(rates.items())) + "}\n"
    src = src.replace("class TimingResearch(QCAlgorithm):",
                      "# Monthly 3-month T-bill rate (FRED TB3MS), for margin financing only.\n"
                      + table + "\n\nclass TimingResearch(QCAlgorithm):", 1)
    old = "            cost = borrowed * self.FINANCING_RATE / 252.0\n            if cost > 0:\n                self.Portfolio.CashBook[\"USD\"].AddAmount(-cost)\n                self.financing_paid += cost\n            return"
    new = ("            key = self.Time.year * 100 + self.Time.month\n"
           "            rate = RATES.get(key, RATES[max(k for k in RATES if k <= key)]) + self.MARGIN_SPREAD\n"
           "            cost = borrowed * rate / 252.0\n            if cost > 0:\n"
           "                self.Portfolio.CashBook[\"USD\"].AddAmount(-cost)\n"
           "                self.financing_paid += cost\n            return")
    if src.count(old) != 1:
        raise SystemExit("main.py financing block not found; the recipe needs updating")
    src = src.replace(old, new)
    src = src.replace("    BOOST_SIZE = 1.5", "    MARGIN_SPREAD = 0.015          # broker margin rate = T-bill + this spread\n    BOOST_SIZE = 1.5", 1)
    src = re.sub(r'^    MODE = [^\n]*', '    MODE = "boost"', src, flags=re.M)
    return src


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    sys.stdout.write(build(sys.argv[1]))
