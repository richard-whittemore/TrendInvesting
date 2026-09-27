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
  size and every dollar of P&L by the same factor, so check it first. This
  is Pinnacle's own choice, not the exchange's, so exchange research alone
  can't settle it (the "Important caveat" below); ``loader.classify_price_scale``
  and ``loader.check_series_scale`` are the automated check to run once
  the real files arrive.
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

**Important caveat.** A comment's source confirms what the exchange itself
publishes or published; it does not confirm what Pinnacle's own file
stores. Pinnacle may scale a price differently from the exchange's own
quotation (bonds in decimal instead of 32nds, yen as 0.0092 instead of
92.00). Where the correct value here depends on that scaling, the market
stays TO-VERIFY for ``price_units`` even after the exchange's own
convention is confirmed and cited, and ``loader.PRICE_SCALE_HINTS`` records
both possibilities for the loader to check against one real bar once the
files arrive (``loader.classify_price_scale``, ``loader.check_series_scale``).
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
    # Quoted in points and 32nds of a point, not decimal
    # (https://www.cmegroup.com/markets/interest-rates/us-treasury/30-year-us-treasury-bond.contractSpecs.html;
    # Ironbeam's mirror of the same spec confirms the "150'16" format).
    # ``price_units`` stays TO-VERIFY regardless: that is the exchange's own
    # convention, not confirmation of how Pinnacle's file stores it (this
    # file's "Important caveat"); loader.PRICE_SCALE_HINTS["US"] has no
    # alternate range because 32nds-as-decimal and true decimal land in the
    # same span and can't be told apart by range alone.
    _m("US", "30-Year U.S. Treasury Bond", 1000.0, 1.0 / 32, "rates", "rates",
       (2, 5, 8, 11), 20, ("price_units",)),
    # CME Group, 10-Year T-Note Futures (ZN): $1,000 per point. Tick is half
    # of 1/32 today; the Richmond Fed's 1993 survey of money-market futures
    # (chapter 14 of "Instruments of the Money Market",
    # https://www.richmondfed.org/-/media/richmondfedorg/publications/research/special_reports/instruments_of_the_money_market/pdf/chapter_14.pdf)
    # predates the tick's later refinement to half-32nds, and the Fed Board
    # staff report on Treasury tick size ("Tick Size, Competition for
    # Liquidity Provision, and Price Efficiency",
    # https://www.newyorkfed.org/medialibrary/media/research/staff_reports/sr886.pdf)
    # dates that refinement to 1999, i.e. after Faith's own trading era and
    # within this backtester's 1980-2015 span. ``tick_size`` is informational
    # only (never used in P&L, per the ``Market`` field comment) and this file
    # keeps a single value rather than a schedule, so the current, later
    # value is recorded. ``price_units`` stays TO-VERIFY for the same reason
    # as US above.
    _m("TY", "10-Year U.S. Treasury Note", 1000.0, 1.0 / 64, "rates", "rates",
       (2, 5, 8, 11), 20, ("price_units",)),
    # CME Group, Eurodollar Futures (GE, delisted 2023): $1,000,000 3-month
    # deposit, quoted as 100 minus the rate, so 1 basis point = 0.01 index
    # points. The Richmond Fed's 1993 survey (URL on TY above) gives the
    # minimum fluctuation actually in force through the Turtle era and this
    # backtester's early years as "1 basis point ... valued at $25" for both
    # Eurodollar and T-bill futures (a fact confirmed for both contracts in
    # the same chapter). CME's current rulebook (Chapter 452) instead splits
    # the tick into 0.0025 for the lead month and 0.005 for back months, a
    # later refinement this single, informational field doesn't track.
    _m("ED", "Eurodollar (3-month)", 2500.0, 0.01, "rates-short", "rates",
       _QUARTERLY, 1, ()),
    # CME 13-week T-bill futures: $1,000,000 face, same 100-minus-yield
    # index quote as Eurodollar, so the same "1 basis point = 0.01 index
    # points = $25" minimum fluctuation applies (Richmond Fed source above).
    # Volume fell far behind Eurodollar from the mid-1980s on (by 1997 CME
    # Eurodollar volume was roughly 450x CME T-bill volume -- BIS Quarterly
    # Review, https://www.bis.org/publ/r_qt0103e.pdf), and CME's 2023
    # relisting of "13-Week U.S. Treasury Bill futures" as a new product
    # implies the original contract was gone by then, but no source found
    # gives the year Pinnacle's 1980-2015 window would need for an exact
    # ``trade_until``, or confirms Pinnacle carries this market at all.
    _m("TB", "90-Day U.S. Treasury Bill", 2500.0, 0.01, "rates-short", "rates",
       _QUARTERLY, 1, ("trade_window",)),

    # --- Currencies. Faith's CHF/DEM pair [T p.16]; main.py's
    # currencies-europe group holds the franc and the euro, and here also
    # the Deutschmark and French franc the euro replaced. CME currency
    # futures expire two days before the third Wednesday of H M U Z.
    # CME Group, Swiss Franc Futures (6S): CHF 125,000, $ per franc.
    _m("SF", "Swiss Franc", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, ()),
    # CME Deutschmark futures: DEM 125,000, $ per mark, contract months H M
    # U Z. The mark was fixed to the euro from 1999-01-01 (Council of the EU
    # regulation 2866/98), and the standard IMM last-trading-day rule (two
    # business days before the third Wednesday) puts the final DM contract's
    # last trading day at 1998-12-14, inside the trade_until bound already
    # set here. No CME notice pinning that exact date was found, but every
    # source checked agrees on the contract months and the delisting cause;
    # the euro-conversion date is fixed by EU law, not by exchange records.
    _m("DM", "Deutschmark", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, (), trade_until=date(1998, 12, 31)),
    # CME French franc futures. Contract size, tick and quote convention are
    # still genuinely unconfirmed: no CME rulebook chapter, spec page or
    # secondary source for the historical FRF contract could be found (CME
    # Group's own site blocks automated fetches, and third-party archives
    # cover only the currencies still traded today). The 500,000/0.00002
    # values here remain placeholders, not a verified figure. Like DM above,
    # the franc was fixed to the euro from 1999-01-01, so the same
    # last-trading-day reasoning applies to ``trade_window``.
    _m("FR", "French Franc", 500000.0, 0.00002, "currencies-europe", "currencies",
       _QUARTERLY, 1, ("multiplier", "tick_size", "price_units"),
       trade_until=date(1998, 12, 31)),
    # CME Group, Euro FX Futures (6E): EUR 125,000, $ per euro
    # (https://www.cmegroup.com/markets/fx/g10/euro-fx.contractSpecs.html);
    # tick $0.00005 today, $0.0001 at launch, matching every other IMM
    # currency in this file at the time (SF and DM both still use $0.0001).
    # The euro's cash debut was 1999-01-04, the first business day of 1999
    # (1999-01-01 to 01-03 was a New Year holiday weekend; e.g. "On this day
    # in history (Jan. 4) 1999 - The euro debuts",
    # https://www.heraldchronicle.com/news/history/on-this-day-in-history-jan-4-1999-the-euro-debuts/article_db4f5e8e-0e82-57df-a7c9-a68168f07159.html),
    # and CME FX futures list from the currency's first trading day.
    _m("EC", "Euro FX", 125000.0, 0.0001, "currencies-europe", "currencies",
       _QUARTERLY, 1, (), trade_from=date(1999, 1, 4)),
    # CME Group, British Pound Futures (6B): GBP 62,500 today
    # (https://www.cmegroup.com/markets/fx/g10/british-pound.contractSpecs.html).
    # markets.py's note that the contract was GBP 25,000 in its early years
    # (from the IMM's 1972 launch) could not be confirmed against any
    # reachable primary source in this pass -- CME Group's site blocks
    # automated fetches, and no secondary source with the change's date
    # could be found either. Left TO-VERIFY.
    _m("BP", "British Pound", 62500.0, 0.0001, "currencies-gbp", "currencies",
       _QUARTERLY, 1, ("multiplier",)),
    # CME Group, Japanese Yen Futures (6J): JPY 12,500,000. Tick $0.0000005
    # per yen ($6.25/contract) is confirmed current (CME Group education
    # material, and Ironbeam's mirror of the same spec,
    # https://www.ironbeam.com/knowledge-base/japanese-yen-futures-6j-contract-specifications/);
    # no evidence of a historical change was found. ``price_units`` stays
    # TO-VERIFY: data vendors often quote per 100 yen instead (then the
    # multiplier would be 125,000), and this is Pinnacle's own scaling
    # choice, not the exchange's (loader.PRICE_SCALE_HINTS["JY"]).
    _m("JY", "Japanese Yen", 12500000.0, 0.0000005, "currencies-jpy", "currencies",
       _QUARTERLY, 1, ("price_units",)),
    # CME Group, Canadian Dollar Futures (6C): CAD 100,000. Tick $0.00005
    # per CME Group's own education material ("Canadian Dollar Product
    # Overview"); some third-party spec pages still show the earlier
    # $0.0001, so this may itself be a historical change, but $0.00005
    # matches the value already used here and is the exchange's own current
    # figure.
    _m("CD", "Canadian Dollar", 100000.0, 0.00005, "currencies-cad", "currencies",
       _QUARTERLY, 1, ()),

    # --- Equity index. The CME halved the S&P 500 contract from $500 to
    # $250 a point, and doubled the tick from 0.05 to 0.10, effective with
    # the December 1997 contracts (CFTC Federal Register notice of the
    # amendment, https://www.cftc.gov/foia/fedreg97/foi970814a.htm; the
    # 1997-11-03 date already used here is corroborated by multiple
    # secondary accounts of the same change, e.g. the E-mini S&P 500's
    # introduction context). ``tick_size`` records only the post-change
    # value (0.10), consistent with how the multiplier schedule's last
    # entry is also the current one; the pre-change tick was 0.05.
    _m("SP", "S&P 500 (full size)", ((date(1900, 1, 1), 500.0), (date(1997, 11, 3), 250.0)), 0.10,
       "equity_indices", "equity_indices", _QUARTERLY, 1, ()),

    # --- Metals. Faith's gold/silver pair [T p.16]; main.py's metals-base
    # group holds copper alone.
    # CME Group, Gold Futures (GC): 100 troy oz, $ per oz, tick $0.10.
    # Delivery G J M Q V Z; first notice is at the end of the month before.
    _m("GC", "Gold", 100.0, 0.10, "metals-precious", "metals",
       (1, 3, 5, 7, 9, 11), 20, ()),
    # CME Group, Silver Futures (SI): 5,000 troy oz, quoted in dollars and
    # cents per troy oz (i.e. the number itself is a dollar amount, such as
    # 24.155), tick $0.005 (multiple current spec pages agree on both).
    # ``price_units`` stays TO-VERIFY: some data vendors instead store the
    # cents-as-the-unit convention (then the multiplier would be 50), and
    # this is Pinnacle's own choice (loader.PRICE_SCALE_HINTS["SI"]).
    _m("SI", "Silver", 5000.0, 0.005, "metals-precious", "metals",
       (2, 4, 6, 8, 11), 20, ("price_units",)),
    # CME Group, Copper Futures (HG): 25,000 lb. COMEX's own quotation
    # convention for copper -- unlike gold and silver -- is cents per lb,
    # which would make the multiplier $250 and the tick a small fraction of
    # a cent; this file still assumes $ per lb (multiplier 25,000). Which
    # one Pinnacle's file actually stores is exactly the caveat this file
    # opens with, so both ``price_units`` and ``tick_size`` (they are
    # coupled: the tick is expressed in whichever unit the price is) stay
    # TO-VERIFY (loader.PRICE_SCALE_HINTS["HG"]).
    _m("HG", "Copper", 25000.0, 0.0005, "metals-base", "metals",
       (2, 4, 6, 8, 11), 20, ("price_units", "tick_size")),

    # --- Energy. Faith's heating oil/crude pair [T p.16]; unleaded gas
    # joins the same petroleum group.
    # CME Group, Crude Oil Futures (CL): 1,000 barrels, $ per barrel, tick
    # $0.01. Monthly; expires around the 20th of the month before delivery.
    _m("CL", "Crude Oil (WTI)", 1000.0, 0.01, "energy-petroleum", "energy",
       _ALL_MONTHS, 10, ()),
    # CME Group, NY Harbor ULSD Futures (HO): 42,000 gallons, quoted in U.S.
    # dollars and cents per gallon (confirmed current for both HO and RB),
    # tick $0.0001 -- the multiplier Faith's own Heating Oil sizing example
    # uses [T p.14]. ``price_units`` stays TO-VERIFY: older vendor data is
    # sometimes in cents per gallon instead, Pinnacle's own choice
    # (loader.PRICE_SCALE_HINTS["HO"]).
    _m("HO", "Heating Oil", 42000.0, 0.0001, "energy-petroleum", "energy",
       _ALL_MONTHS, 20, ("price_units",)),
    # NYMEX unleaded gasoline (HU) and its successor, CME Group RBOB
    # Gasoline Futures (RB): both 42,000 gallons, $ per gallon. RBOB began
    # trading alongside HU in October 2005 (EIA's "New York Harbor
    # Reformulated RBOB Regular Gasoline" series starts the same month,
    # https://www.eia.gov/dnav/pet/hist/eer_epmrr_pe1_y35ny_dpgD.htm), with
    # HU phased out during 2006 as the U.S. gasoline market itself switched
    # from MTBE-oxygenated conventional gasoline to the ethanol-compatible
    # RBOB blendstock. Whether Pinnacle's own "HU" file (if any) splices the
    # two into one continuous series, stops at the 2006 changeover, or
    # covers only one of them is still unknown, so ``trade_window`` stays
    # TO-VERIFY along with ``price_units`` (same cents-vs-dollars caveat as
    # HO, loader.PRICE_SCALE_HINTS["HU"]).
    _m("HU", "Unleaded Gasoline / RBOB", 42000.0, 0.0001, "energy-petroleum", "energy",
       _ALL_MONTHS, 20, ("price_units", "trade_window")),

    # --- Softs [T p.10-11]. main.py's single softs group. All four
    # contract sizes and ticks below were double-checked against ICE's own
    # product pages (https://www.ice.com/products/23/Sugar-No-11-Futures,
    # /15/Coffee-C-Futures, /7/Cocoa-Futures, and Cotton No. 2's spec sheet
    # at https://www.ice.com/api/productguide/spec/254/pdf) and match
    # exactly: e.g. Sugar's "0.01 cents/lb = $11.20/contract" and Cotton's
    # "1/100 of a cent per lb = $5.00/contract" are both the same tick this
    # file already stored, just in ICE's own words.
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
