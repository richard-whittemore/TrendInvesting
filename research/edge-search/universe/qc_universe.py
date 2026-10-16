# Research script, not production code. Run on QuantConnect Cloud to log the
# backtest's own universe for chosen months, for compare.py to set against
# decision D8's universe built from Massive (massive.py). It cannot be
# imported outside QuantConnect (AlgorithmImports); the tests use a stand-in.
#
# The filter is momentum/defensive_v2.py's Coarse step exactly: fundamental
# data, price at least $5, the UNIVERSE_SIZE largest by one day's dollar
# volume, chosen on the month's first Coarse call. Each chosen month is
# logged as "UNIV YYYY-MM TICKER;...|" lines, sorted, about 3 KB a month, so
# two months fit the Free plan's 10 KB log cap. Each month's count is a
# runtime statistic, so compare.py refuses a log cut short.
from AlgorithmImports import *


def universe_lines(month, names, width=190):
    """"UNIV month TICKER;...|" lines of at most ``width`` characters; the
    closing "|" marks a line that was not cut short."""
    prefix, lines, cur = "UNIV {} ".format(month), [], []
    for name in names:
        if cur and len(prefix + ";".join(cur + [name]) + "|") > width:
            lines.append(prefix + ";".join(cur) + "|")
            cur = []
        cur.append(name)
    if cur:
        lines.append(prefix + ";".join(cur) + "|")
    return lines


class UniverseLog(QCAlgorithm):
    START = (2024, 10, 25)
    END = (2024, 12, 5)
    MONTHS = ((2024, 11), (2024, 12))   # (year, month) whose universe is logged
    UNIVERSE_SIZE = 500

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(100_000)
        self.UniverseSettings.Resolution = Resolution.Daily
        self.AddUniverse(self.Coarse)
        self.last_month = None
        self.counts = {}

    def Coarse(self, coarse):
        month = (self.Time.year, self.Time.month)
        if month == self.last_month:
            return Universe.Unchanged
        self.last_month = month
        rows = [c for c in coarse if c.HasFundamentalData and c.Price >= 5.0]
        rows.sort(key=lambda c: c.DollarVolume, reverse=True)
        picked = [c.Symbol for c in rows[:self.UNIVERSE_SIZE]]
        if month in tuple(tuple(m) for m in self.MONTHS):
            tag = "{}-{:02d}".format(*month)
            for line in universe_lines(tag, sorted(s.Value for s in picked)):
                self.Log(line)
            self.counts[tag] = len(picked)
        return picked

    def OnEndOfAlgorithm(self):
        for tag, n in sorted(self.counts.items()):
            self.SetRuntimeStatistic(tag, str(n))
        self.SetRuntimeStatistic("Date", self.Time.strftime("%Y-%m-%d"))
