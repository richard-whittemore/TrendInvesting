"""Faith's futures portfolio, as a config table for the local Pinnacle CLC
backtester.

The portfolio is Faith's own list [T p.10-11] (The Original Turtle Trading
Rules, Curtis Faith, 2003; docs/methodology/Methodology_Analysis.md
Section 2): CBOT's 30-year bond and 10-year note; the NYCSCE's coffee,
cocoa, sugar and cotton; the CME's Swiss franc, Deutschmark, British pound,
French franc, Japanese yen, Canadian dollar, S&P 500, Eurodollar and
90-day T-bill; COMEX's gold, silver and copper; and NYMEX's crude oil,
heating oil and unleaded gas. Grains and meats are excluded, as Faith
excluded them [T p.10-11]. The euro (EC) is added as the Deutschmark and
French franc's successor from 1999, trading only once they stop.

Correlation groups reuse research/qc-cloud-futures/main.py's
``CORRELATION_GROUPS`` wherever that table lists the same market. Faith's
own disclosed pairs are heating oil/crude, gold/silver, CHF/DEM and
T-bill/Eurodollar [T p.16]; every other grouping is that table's own
considered, unverified choice (research/qc-cloud-futures/README.md,
"Correlation groups").

**TO-VERIFY.** Each market's ``to_verify`` names what has not been checked
against the purchased files or the exchange's own history:

- ``file_stem``: every stem is a placeholder. Pinnacle's real file names
  are unknown until the data arrives.
- ``price_units``: the file may quote the price in different units from
  the ones the multiplier assumes, such as cents instead of dollars, 32nds
  instead of decimals, or per 100 yen. A units mismatch scales every Unit
  size and every dollar of P&L by the same factor, so check it first.
- ``multiplier`` or ``tick_size``: the contract size or minimum tick
  changed over the market's life, or is not certain.
- ``roll_months``: the months in which this backtester charges a roll. It
  approximates the exchange's delivery cycle, not Pinnacle's own roll
  dates.
- ``trade_window``: the dates between which new Campaigns may open.

Multipliers are dollars per one point of the quoted price. Where a comment
cites a contract spec, it is the exchange's current published spec (CME
Group or ICE Futures U.S.); historical contract sizes can differ, which is
why the market is marked.
"""

from collections import namedtuple
from datetime import date

Market = namedtuple("Market", [
    "symbol",          # short code used in reports
    "name",
    "file_stem",       # Pinnacle file name, without extension -- TO-VERIFY for all
    "multiplier",      # dollars per point: a number, or ((effective_date, value), ...) ascending
    "tick_size",       # in the quoted price's own units; informational
    "closely_group",
    "loosely_group",
    "roll_months",     # months in which a roll is charged
    "roll_day",        # first calendar day of the month on which it is charged
    "trade_from",      # first date a Campaign may open, or None
    "trade_until",     # last date a Campaign may open, or None
    "to_verify",       # tuple of field names still unverified
])

_ALL_MONTHS = tuple(range(1, 13))
_QUARTERLY = (3, 6, 9, 12)


def _m(symbol, name, multiplier, tick_size, closely, loosely, roll_months, roll_day,
       to_verify, trade_from=None, trade_until=None):
    return Market(symbol, name, symbol, multiplier, tick_size, closely, loosely, tuple(roll_months),
                  roll_day, trade_from, trade_until, ("file_stem",) + tuple(to_verify))


MARKETS = {m.symbol: m for m in (
    # --- Rates. main.py puts the two Treasury points in one closely-
    # correlated group; Faith's T-bill/Eurodollar pair [T p.16] is the
    # short-rate group.
    # CME Group, 30-Year U.S. Treasury Bond Futures (ZB): $100,000 face,
    # $1,000 per point, tick 1/32. Delivery H M U Z; first notice is at the
    # end of the month before, so the roll is charged in Feb/May/Aug/Nov.
    _m("US", "30-Year U.S. Treasury Bond", 1000.0, 1.0 / 32, "rates", "rates",
       (2, 5, 8, 11), 20, ("price_units",)),
    # CME Group, 10-Year T-Note Futures (ZN): $1,000 per point, tick half
    # of 1/32 today (a full 1/32 in earlier decades).
    _m("TY", "10-Year U.S. Treasury Note", 1000.0, 1.0 / 64, "rates", "rates",
       (2, 5, 8, 11), 20, ("price_units", "tick_size")),
    # CME Group, Eurodollar Futures (GE, delisted 2023): $1,000,000 3-month
    # deposit, $25 per basis point = $2,500 per point.
    _m("ED", "Eurodollar (3-month)", 2500.0, 0.005, "rates-short", "rates",
       _QUARTERLY, 1, ("tick_size",)),
    # CME 13-week T-bill futures: $1,000,000 face, $25 per basis point. Long
    # delisted; whether Pinnacle carries it at all is unknown.
    _m("TB", "90-Day U.S. Treasury Bill", 2500.0, 0.005, "rates-short", "rates",
       _QUARTERLY, 1, ("tick_size", "trade_window")),

    # --- Currencies. Faith's CHF/DEM pair [T p.16]; main.py's
    # currencies-europe group holds the franc and the euro, and here also
    # the Deutschmark and French franc the euro replaced. CME currency
    # futures expire two days before the third Wednesday of H M U Z.
    # CME Group, Swiss Franc Futures (6S): CHF 125,000, $ per franc.
    _m("SF", "Swiss Franc", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, ()),
    # CME Deutschmark futures: DEM 125,000, $ per mark. Traded until the
    # euro replaced it.
    _m("DM", "Deutschmark", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, ("trade_window",), trade_until=date(1998, 12, 31)),
    # CME French franc futures: contract size and quote convention both
    # unconfirmed (FRF 250,000 or 500,000; $ per franc assumed).
    _m("FR", "French Franc", 500000.0, 0.00002, "currencies-europe", "currencies",
       _QUARTERLY, 1, ("multiplier", "tick_size", "price_units", "trade_window"),
       trade_until=date(1998, 12, 31)),
    # CME Group, Euro FX Futures (6E): EUR 125,000, $ per euro; tick
    # $0.00005 today, $0.0001 earlier. The DM/FR successor from 1999.
    _m("EC", "Euro FX", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, ("tick_size", "trade_window"), trade_from=date(1999, 1, 4)),
    # CME Group, British Pound Futures (6B): GBP 62,500, $ per pound. The
    # contract was GBP 25,000 in its early years.
    _m("BP", "British Pound", 62500.0, 0.0001, "currencies-gbp", "currencies",
       _QUARTERLY, 1, ("multiplier",)),
    # CME Group, Japanese Yen Futures (6J): JPY 12,500,000, $ per yen. Data
    # vendors often quote per 100 yen instead (then the multiplier is
    # 125,000).
    _m("JY", "Japanese Yen", 12500000.0, 0.0000005, "currencies-jpy", "currencies",
       _QUARTERLY, 1, ("price_units", "tick_size")),
    # CME Group, Canadian Dollar Futures (6C): CAD 100,000, $ per CAD.
    _m("CD", "Canadian Dollar", 100000.0, 0.00005, "currencies-cad", "currencies",
       _QUARTERLY, 1, ("tick_size",)),

    # --- Equity index. The CME halved the S&P 500 contract from $500 to
    # $250 a point in November 1997 (CME Group, S&P 500 Futures (SP)).
    _m("SP", "S&P 500 (full size)", ((date(1900, 1, 1), 500.0), (date(1997, 11, 3), 250.0)), 0.10,
       "equity_indices", "equity_indices", _QUARTERLY, 1, ("multiplier", "tick_size")),

    # --- Metals. Faith's gold/silver pair [T p.16]; main.py's metals-base
    # group holds copper alone.
    # CME Group, Gold Futures (GC): 100 troy oz, $ per oz, tick $0.10.
    # Delivery G J M Q V Z; first notice is at the end of the month before.
    _m("GC", "Gold", 100.0, 0.10, "metals-precious", "metals",
       (1, 3, 5, 7, 9, 11), 20, ()),
    # CME Group, Silver Futures (SI): 5,000 troy oz, $ per oz, tick
    # $0.005. Vendors often quote cents per oz (then the multiplier is 50).
    _m("SI", "Silver", 5000.0, 0.005, "metals-precious", "metals",
       (2, 4, 6, 8, 11), 20, ("price_units",)),
    # CME Group, Copper Futures (HG): 25,000 lb, $ per lb, tick $0.0005.
    # Historically quoted in cents per lb (then the multiplier is 250).
    _m("HG", "Copper", 25000.0, 0.0005, "metals-base", "metals",
       (2, 4, 6, 8, 11), 20, ("price_units", "tick_size")),

    # --- Energy. Faith's heating oil/crude pair [T p.16]; unleaded gas
    # joins the same petroleum group.
    # CME Group, Crude Oil Futures (CL): 1,000 barrels, $ per barrel, tick
    # $0.01. Monthly; expires around the 20th of the month before delivery.
    _m("CL", "Crude Oil (WTI)", 1000.0, 0.01, "energy-petroleum", "energy",
       _ALL_MONTHS, 10, ()),
    # CME Group, NY Harbor ULSD Futures (HO): 42,000 gallons, $ per gallon,
    # tick $0.0001 -- the multiplier Faith's own Heating Oil sizing example
    # uses [T p.14]. Older data is sometimes in cents per gallon.
    _m("HO", "Heating Oil", 42000.0, 0.0001, "energy-petroleum", "energy",
       _ALL_MONTHS, 20, ("price_units",)),
    # NYMEX unleaded gasoline (delisted 2006) and its successor, CME Group
    # RBOB Gasoline Futures (RB): 42,000 gallons, $ per gallon. Whether
    # Pinnacle splices the two into one series is unknown.
    _m("HU", "Unleaded Gasoline / RBOB", 42000.0, 0.0001, "energy-petroleum", "energy",
       _ALL_MONTHS, 20, ("price_units", "trade_window")),

    # --- Softs [T p.10-11]. main.py's single softs group.
    # ICE Futures U.S., Sugar No. 11 (SB): 112,000 lb, cents per lb, tick
    # 0.01. Delivery H K N V; the contract expires at the end of the month
    # before.
    _m("SB", "Sugar No. 11", 1120.0, 0.01, "softs", "softs", (2, 4, 6, 9), 15, ()),
    # ICE Futures U.S., Coffee "C" (KC): 37,500 lb, cents per lb, tick
    # 0.05. Delivery H K N U Z.
    _m("KC", "Coffee C", 375.0, 0.05, "softs", "softs", (2, 4, 6, 8, 11), 10, ()),
    # ICE Futures U.S., Cocoa (CC): 10 metric tons, $ per ton, tick $1.
    # Delivery H K N U Z.
    _m("CC", "Cocoa", 10.0, 1.0, "softs", "softs", (2, 4, 6, 8, 11), 10, ()),
    # ICE Futures U.S., Cotton No. 2 (CT): 50,000 lb, cents per lb, tick
    # 0.01. Liquid months H K N Z.
    _m("CT", "Cotton No. 2", 500.0, 0.01, "softs", "softs", (2, 4, 6, 11), 15, ()),
)}

# Every roll schedule above is an approximation of the delivery cycle,
# not Pinnacle's own roll dates.
MARKETS = {symbol: market._replace(to_verify=market.to_verify + ("roll_months",))
           for symbol, market in MARKETS.items()}


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
    """One line per market: symbol, file stem, and what is still unverified."""
    return ["{:<3} {:<8} {}".format(m.symbol, m.file_stem, ", ".join(m.to_verify)) for m in MARKETS.values()]
