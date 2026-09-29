"""Faith's futures portfolio, as a config table for the local Pinnacle CLC
backtester.

The portfolio is Faith's own list [T p.10-11] (The Original Turtle Trading
Rules, Curtis Faith, 2003; docs/methodology/Methodology_Analysis.md
Section 2): CBOT's 30-year bond and 10-year note; the NYCSCE's coffee,
cocoa, sugar and cotton; the CME's Swiss franc, Deutschmark, British pound,
French franc, Japanese yen, Canadian dollar, S&P 500, Eurodollar and
90-day T-bill; COMEX's gold, silver and copper; and NYMEX's crude oil,
heating oil and unleaded gas. Grains and meats are excluded, as Faith
excluded them [T p.10-11].

**Pinnacle mapping.** Every traded market's ``file_stem`` is its Pinnacle
CLC symbol; the backtester reads ``<stem>_REV`` (back-adjusted) for trading
and ``<stem>_NON`` (non-adjusted) for the price-scale check. Two sources
settle each row, both checked on 2026-09-28 against the purchased CLC
database:

- **[M]** Pinnacle's manual, Appendix B-1 (``MANUAL.DOC``): symbol, start
  date, tick size, "Min $ Move" and "RollOverDate". The manual's "BigPoint
  Value" column is *not* used directly: for some symbols it is in a
  different unit from the file's prices (for ZI it says 5000, the dollars
  per $1.00/oz, but the file quotes cents). Dollars per 1.00 of the file's
  own price is derived as **Min $ Move / tick size**, in the file's units.
- **[F]** The files themselves: the ``_NON`` settle on known dates, times
  the multiplier, must equal the real contract's notional value (e.g. ZI
  on 2015-12-31, about 1,380 cents x $50 = $69,000 = 5,000 oz x $13.80),
  and the roll months and days are read off the dates on which ``_REV``
  minus ``_NON`` changes (Pinnacle's back-adjustment steps only on a
  roll), 1990-2015.

``multiplier`` is dollars per 1.00 of Pinnacle's price, *in Pinnacle's
units*: cents for silver, copper, heating oil and gasoline; the currencies
scaled up (Swiss franc, Deutschmark/euro, pound and Canadian dollar x 100,
the yen x 10,000).

**Excluded.** Pinnacle's CLC database has no 90-day T-bill (TB) or French
franc (FR) contract; both stay in the table with ``excluded`` set so the
portfolio's full list is still visible.

**Correlation groups** reuse research/qc-cloud-futures/main.py's
``CORRELATION_GROUPS`` wherever that table lists the same market. Faith's
own disclosed pairs are heating oil/crude, gold/silver, CHF/DEM and
T-bill/Eurodollar [T p.16]; every other grouping is that table's own
considered, unverified choice (research/qc-cloud-futures/README.md,
"Correlation groups").

**TO-VERIFY.** ``to_verify`` names any field the files and the manual did
not settle. After the real data, only BP's ``multiplier`` history is left
(below).
"""

from collections import namedtuple
from datetime import date

Market = namedtuple("Market", [
    "symbol",          # short code used in reports
    "name",
    "file_stem",       # Pinnacle CLC symbol; files are <stem>_REV / _NON / _RAD
    "multiplier",      # dollars per 1.00 of Pinnacle's price: a number, or ((effective_date, value), ...)
    "tick_size",       # in Pinnacle's price units; informational
    "closely_group",
    "loosely_group",
    "roll_months",     # months in which a roll is charged
    "roll_day",        # first calendar day of the month on which it is charged
    "trade_from",      # first date a Campaign may open, or None
    "trade_until",     # last date a Campaign may open, or None
    "to_verify",       # tuple of field names still unverified
    "excluded",        # None, or why the market is not traded (no data)
], defaults=(None,))

_ALL_MONTHS = tuple(range(1, 13))
_QUARTERLY = (3, 6, 9, 12)


def _m(symbol, name, stem, multiplier, tick_size, closely, loosely, roll_months, roll_day,
       to_verify=(), trade_from=None, trade_until=None, excluded=None):
    return Market(symbol, name, stem, multiplier, tick_size, closely, loosely, tuple(roll_months),
                  roll_day, trade_from, trade_until, tuple(to_verify), excluded)


MARKETS = {m.symbol: m for m in (
    # --- Rates. main.py puts the two Treasury points in one closely-
    # correlated group; Faith's T-bill/Eurodollar pair [T p.16] is the
    # short-rate group.
    # US: [M] "T-BONDS, composite", CBOT, from 1978-01-03, tick 1/32 =
    # $31.25, so $1,000 per point. [F] prices are decimal points, not
    # 32nds (1978-01-03 settle 98.875 = 98-28/32). Rolls in Feb/May/Aug/Nov
    # from the 22nd ([M] says "24th of MPDM"; [F] shows the 22nd-28th).
    _m("US", "30-Year U.S. Treasury Bond", "US", 1000.0, 1.0 / 32, "rates", "rates",
       (2, 5, 8, 11), 22),
    # TY: [M] "T-NOTE, 10yr composite", from 1983-01-03, tick 1/64 = $15.63
    # -> $1,000 per point; decimal points [F]. Rolls as US.
    _m("TY", "10-Year U.S. Treasury Note", "TY", 1000.0, 1.0 / 64, "rates", "rates",
       (2, 5, 8, 11), 22),
    # ED: Pinnacle's "EC" is the Eurodollar ([M] "EURODOLLAR, composite",
    # from 1982-02-01, ends 2023-05-19 when the contract was delisted), tick
    # 0.0025 = $6.25 -> $2,500 per point; quoted 100 minus the rate [F]
    # (1982 settles near 84). Rolls Feb/May/Aug/Nov from the 22nd [M][F].
    _m("ED", "Eurodollar (3-month)", "EC", 2500.0, 0.0025, "rates-short", "rates",
       (2, 5, 8, 11), 22),
    # TB: not in Pinnacle's CLC database.
    _m("TB", "90-Day U.S. Treasury Bill", "TB", 2500.0, 0.01, "rates-short", "rates",
       _QUARTERLY, 1, excluded="not in Pinnacle's CLC database"),

    # --- Currencies. Faith's CHF/DEM pair [T p.16]; main.py's
    # currencies-europe group. Every currency: [M] composite (day session
    # to 1990), tick 0.01 in Pinnacle's scaled price, rolls on the 8th of
    # the delivery month, H M U Z [M][F].
    # SF: [M] "SWISS FRANC, composite" (SN), tick 0.01 = $12.50 -> $1,250;
    # [F] prices are US cents per franc (1976 settle 38.44 = $0.3844;
    # CHF 125,000 x $0.01 = $1,250).
    _m("SF", "Swiss Franc", "SN", 1250.0, 0.01, "currencies-europe", "currencies",
       _QUARTERLY, 8),
    # EC: [M] "EURO, composite" (FN), "with DMark History to 3/7/99": one
    # series, the Deutschmark to March 1999, then the euro. Both contracts
    # are 125,000 units; tick 0.01 = $12.50 -> $1,250, cents per unit [F].
    # The splice is absorbed into the back-adjustment like a roll ([F]: a
    # -53.42 step in REV minus NON on 1999-03-08), so REV has no false
    # jump; one market, no trade window, replaces the separate DM and euro
    # rows the table had before the data arrived.
    _m("EC", "Deutschmark to 1999, then Euro FX", "FN", 1250.0, 0.01, "currencies-europe", "currencies",
       _QUARTERLY, 8),
    # FR: not in Pinnacle's CLC database.
    _m("FR", "French Franc", "FR", 500000.0, 0.00002, "currencies-europe", "currencies",
       _QUARTERLY, 1, trade_until=date(1998, 12, 31), excluded="not in Pinnacle's CLC database"),
    # BP: [M] "BRITISH POUND, composite" (BN), tick 0.01 = $6.25 -> $625,
    # cents per pound [F] (1976 settle 200.55 = $2.0055; GBP 62,500 x $0.01
    # = $625). ``multiplier`` stays TO-VERIFY for the contract's early
    # years only: the IMM contract was reportedly GBP 25,000 before the
    # mid-1980s, and no source dates the change. It changes only how many
    # whole contracts a Unit rounds to, not the P&L per point per pound.
    _m("BP", "British Pound", "BN", 625.0, 0.01, "currencies-gbp", "currencies",
       _QUARTERLY, 8, ("multiplier",)),
    # JY: [M] "JAPANESE YEN, composite" (JN), tick 0.01 = $12.50 ->
    # $1,250; [F] prices are dollars per yen x 10,000 (1978 settle 42.71 =
    # $0.004271 = 234 yen per dollar; JPY 12,500,000 x $0.000001 = $12.50).
    _m("JY", "Japanese Yen", "JN", 1250.0, 0.01, "currencies-jpy", "currencies",
       _QUARTERLY, 8),
    # CD: [M] "CANADIAN $$, composite" (CN), tick 0.01 = $10 -> $1,000;
    # cents per Canadian dollar [F] (1978 settle 91.70 = $0.917).
    _m("CD", "Canadian Dollar", "CN", 1000.0, 0.01, "currencies-cad", "currencies",
       _QUARTERLY, 8),

    # --- Equity index. SP: [M] "S & P 500, day session", from 1982-04-21
    # to 2021-09-17 [F], tick 0.1 = $25 -> $250, but that is today's
    # contract. The CME halved it from $500 to $250 a point with the
    # December 1997 contracts (CFTC Federal Register notice,
    # https://www.cftc.gov/foia/fedreg97/foi970814a.htm), and [F] shows
    # Pinnacle's prices are the plain index (1982 settle 117.45), not
    # rescaled, so the schedule is kept. Rolls: [M] "Thur prior 2nd Fri of
    # DM", [F] the 7th-17th of H M U Z.
    _m("SP", "S&P 500 (full size)", "SP", ((date(1900, 1, 1), 500.0), (date(1997, 11, 3), 250.0)), 0.10,
       "equity_indices", "equity_indices", _QUARTERLY, 7),

    # --- Metals. Faith's gold/silver pair [T p.16]; main.py's metals-base
    # group holds copper alone. Pinnacle's electronic ("Z") series splice
    # the day session before April 2008 [M].
    # GC: [M] "GOLD, Electronic" (ZG), from 1975, tick 0.1 = $10 -> $100;
    # dollars per troy oz [F]. [F] rolls Jan/Mar/May/Jul/Nov from the 22nd
    # (Pinnacle skips the October contract).
    _m("GC", "Gold", "ZG", 100.0, 0.10, "metals-precious", "metals",
       (1, 3, 5, 7, 11), 22),
    # SI: [M] "SILVER, Electronic" (ZI), from 1973, tick 0.5 = $25 -> $50
    # per 1.00 (the manual's "BigPoint Value" 5000 is per dollar, not per
    # cent); [F] prices are cents per troy oz (1973 settle 203.4 = $2.034).
    # [F] rolls Feb/Apr/Jun/Aug/Nov from the 22nd.
    _m("SI", "Silver", "ZI", 50.0, 0.5, "metals-precious", "metals",
       (2, 4, 6, 8, 11), 22),
    # HG: [M] "COPPER, electronic" (ZK), from 1989-01-03 only, tick 0.05 =
    # $12.50 -> $250; cents per lb [F] (1989 settle 140.95). [F] rolls as
    # silver. ZK has corrupt rows in 2026 (README.md, "Data problems
    # found"), outside the in-sample span.
    _m("HG", "Copper", "ZK", 250.0, 0.05, "metals-base", "metals",
       (2, 4, 6, 8, 11), 22),

    # --- Energy. Faith's heating oil/crude pair [T p.16]; unleaded gas
    # joins the same petroleum group. Monthly rolls from the 11th [M][F].
    # CL: [M] "CRUDE OIL, Electronic" (ZU), from 1984-01-03, tick 0.01 = $10
    # -> $1,000; dollars per barrel [F].
    _m("CL", "Crude Oil (WTI)", "ZU", 1000.0, 0.01, "energy-petroleum", "energy",
       _ALL_MONTHS, 11),
    # HO: [M] "HEATING OIL, electronic" (ZH), from 1980-01-02, tick 0.01 =
    # $4.20 -> $420; cents per gallon [F] (1980 settle 82.05).
    _m("HO", "Heating Oil", "ZH", 420.0, 0.01, "energy-petroleum", "energy",
       _ALL_MONTHS, 11),
    # HU: [M] "RBOB, Electronic" (ZB), tick 0.01 = $4.20 -> $420; the manual
    # dates it from 10/16/06, but [F] the file runs from 1985-01-02, so
    # Pinnacle splices NYMEX unleaded gas before RBOB into one
    # back-adjusted series. Cents per gallon [F] (1985 settle 70.20).
    _m("HU", "Unleaded Gasoline, then RBOB", "ZB", 420.0, 0.01, "energy-petroleum", "energy",
       _ALL_MONTHS, 11),

    # --- Softs [T p.10-11]. main.py's single softs group. ICE's own product
    # pages (https://www.ice.com/products/23/Sugar-No-11-Futures,
    # /15/Coffee-C-Futures, /7/Cocoa-Futures, Cotton No. 2's spec sheet at
    # https://www.ice.com/api/productguide/spec/254/pdf) agree with [M].
    # SB: [M] tick 0.01 = $11.20 -> $1,120; cents per lb [F]. Rolls Feb/Apr/
    # Jun/Sep from the 22nd [M][F].
    _m("SB", "Sugar No. 11", "SB", 1120.0, 0.01, "softs", "softs", (2, 4, 6, 9), 22),
    # KC: [M] tick 0.05 = $18.75 -> $375; cents per lb [F]. Rolls Feb/Apr/
    # Jun/Aug/Nov from the 11th [M][F].
    _m("KC", "Coffee C", "KC", 375.0, 0.05, "softs", "softs", (2, 4, 6, 8, 11), 11),
    # CC: [M] from 1981-01-05, tick 1 = $10 -> $10; dollars per metric ton
    # [F]. Rolls Feb/Apr/Jun/Aug/Nov from the 9th [M][F].
    _m("CC", "Cocoa", "CC", 10.0, 1.0, "softs", "softs", (2, 4, 6, 8, 11), 9),
    # CT: [M] tick 0.01 = $5 -> $500; cents per lb [F]. Rolls Feb/Apr/Jun/
    # Nov from the 15th [M][F].
    _m("CT", "Cotton No. 2", "CT", 500.0, 0.01, "softs", "softs", (2, 4, 6, 11), 15),
)}


def dollars_per_point(market, on_date):
    """The market's dollars per point on ``on_date``: a constant, or the
    last ``(effective_date, value)`` entry at or before ``on_date``."""
    multiplier = market.multiplier
    if isinstance(multiplier, (int, float)):
        return float(multiplier)
    value = None
    for effective, amount in multiplier:
        if effective <= on_date:
            value = amount
    if value is None:
        raise ValueError("{} has no multiplier on {}".format(market.symbol, on_date))
    return float(value)


def tradable(market, on_date):
    """Whether a new Campaign may open in ``market`` on ``on_date``. An open
    Campaign is never closed for leaving the window (CONTEXT.md,
    "Eligible")."""
    if market.trade_from is not None and on_date < market.trade_from:
        return False
    if market.trade_until is not None and on_date > market.trade_until:
        return False
    return True


def to_verify_report():
    """One line per market: symbol, file stem, and what is still unverified
    (or why it is excluded)."""
    return ["{:<3} {:<4} {}".format(m.symbol, m.file_stem,
                                    "EXCLUDED: " + m.excluded if m.excluded else ", ".join(m.to_verify))
            for m in MARKETS.values()]
