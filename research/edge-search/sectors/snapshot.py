# Research script, not production code. Run once on QuantConnect Cloud for
# the sector-label check in docs/live-trading/plan.md (section 4): it lists
# the backtest's universe on one date with each stock's Morningstar sector,
# for compare.py to set against the GICS sectors in iShares' holdings file.
# It cannot be imported outside QuantConnect (AlgorithmImports); the tests use
# a stand-in.
#
# The universe filter is momentum/defensive_v2.py's Coarse step: fundamental
# data, price at least $5, the UNIVERSE_SIZE largest by one day's dollar
# volume. The output is logged as "SECT TICKER:code;..." lines (about 5 KB
# for 500 names, inside the Free plan's 10 KB log cap) and the count is a
# runtime statistic, so a cut-off log is noticed.
from AlgorithmImports import *


def sector_lines(pairs, width=190):
    """"SECT TICKER:code;..." lines of at most ``width`` characters; a stock
    with no sector is written with "-"."""
    lines, cur = [], []
    for ticker, code in pairs:
        item = "{}:{}".format(ticker, "-" if code is None else code)
        if cur and len("SECT " + ";".join(cur + [item])) > width:
            lines.append("SECT " + ";".join(cur))
            cur = []
        cur.append(item)
    if cur:
        lines.append("SECT " + ";".join(cur))
    return lines


class SectorSnapshot(QCAlgorithm):
    START = (2026, 10, 1)
    END = (2026, 10, 6)
    UNIVERSE_SIZE = 500

    def Initialize(self):
        self.SetStartDate(*self.START)
        self.SetEndDate(*self.END)
        self.SetCash(100_000)
        self.UniverseSettings.Resolution = Resolution.Daily
        self.AddUniverse(self.Coarse, self.Fine)
        self.picked = False
        self.logged = 0

    def Coarse(self, coarse):
        if self.picked:
            return Universe.Unchanged
        self.picked = True
        rows = [c for c in coarse if c.HasFundamentalData and c.Price >= 5.0]
        rows.sort(key=lambda c: c.DollarVolume, reverse=True)
        return [c.Symbol for c in rows[:self.UNIVERSE_SIZE]]

    def Fine(self, fine):
        if self.logged:
            return []
        pairs = []
        for f in fine:
            code = None
            for path in ("AssetClassification.MorningstarSectorCode",
                         "asset_classification.morningstar_sector_code"):
                cur = f
                try:
                    for name in path.split("."):
                        cur = getattr(cur, name)
                    code = int(cur) or None
                    break
                except Exception:
                    continue
            pairs.append((f.Symbol.Value, code))
        for line in sector_lines(sorted(pairs)):
            self.Log(line)
        self.logged = len(pairs)
        self.SetRuntimeStatistic("Count", str(len(pairs)))
        self.SetRuntimeStatistic("Date", self.Time.strftime("%Y-%m-%d"))
        return []
