"""Rebuild the "Boost" re-test with realistic margin rates (see
docs/research/edge-search-2026-09.md, "Boost re-test with realistic margin
rates"). The repository never stores market or economic data, so the rate
table is generated here from a local copy of FRED's TB3MS monthly CSV
(https://fred.stlouisfed.org/series/TB3MS, header "observation_date,TB3MS").

Recipe, exactly as used for the reported +6.85% (1999-2015) and +16.18%
(2016 to mid-2026) runs:
  * RATES maps YYYYMM -> that month's TB3MS value / 100, for 1998 onward;
    a missing month uses the latest earlier month. TB3MS is the average of
    the month's daily rates, so charging it from the first day of that month
    uses a little future information. The reported runs did exactly this.
    The effect on the results is tiny: financing totalled about $53k and
    $61k, and the month-to-month change in the rate is a small fraction of
    that. Pass --prior-month to charge each month the previous month's
    average instead, which uses only data available at the time.
  * The daily financing charge on borrowed exposure is
    borrowed * (RATES[month] + MARGIN_SPREAD) / 252, MARGIN_SPREAD = 1.5%.
  * Everything else is main.py in MODE "boost" (BOOST_SIZE 1.5).

Usage: python3 build_realistic_boost.py TB3MS.csv [--prior-month] [--first-month YYYYMM] > boost_realistic.py
       (add START_DATE/END_DATE/MEASURE_FROM edits for the held-out run).
       --first-month is the run's first month (default 199801). The table
       starts there, and a CSV without a rate for it is refused; pass
       201601 to build the held-out run from a 2015-onward download.
"""
import csv
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
FIRST_MONTH = 199801                 # the in-sample backtests' first month


def build(tb3ms_path, prior_month=False, first_month=FIRST_MONTH):
    rates = {}
    with open(tb3ms_path) as f:
        for row in csv.reader(f):
            if not row or row[0].startswith("observation") or row[1] == ".":
                continue
            year, month, _ = row[0].split("-")
            y, m = int(year), int(month)
            if prior_month:
                y, m = (y + 1, 1) if m == 12 else (y, m + 1)
            if y * 100 + m >= first_month:
                rates[y * 100 + m] = float(row[1]) / 100
    if first_month not in rates:
        # The run starts in first_month; with --prior-month that month needs
        # the previous month's average, and the generated lookup has no
        # earlier month to fall back on.
        raise SystemExit("TB3MS has no rate for {} (with --prior-month, the CSV must include "
                         "the month before); download it from earlier".format(first_month))
    with open(os.path.join(HERE, "main.py")) as f:
        src = f.read()
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
    args = [a for a in sys.argv[1:] if a != "--prior-month"]
    first = FIRST_MONTH
    if "--first-month" in args:
        i = args.index("--first-month")
        if i + 1 >= len(args) or not re.fullmatch(r"\d{6}", args[i + 1]):
            raise SystemExit(__doc__)
        first = int(args[i + 1])
        del args[i:i + 2]
    if len(args) != 1:
        raise SystemExit(__doc__)
    sys.stdout.write(build(args[0], prior_month="--prior-month" in sys.argv[1:], first_month=first))
