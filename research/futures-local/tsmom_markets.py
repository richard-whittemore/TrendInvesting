"""The time-series momentum (TSMOM) universe: every liquid, USD-settled
Pinnacle CLC market, not just Faith's 19.

Moskowitz, Ooi & Pedersen (2012), "Time Series Momentum", Journal of
Financial Economics 104(2), 228-250 ("MOP"), Section 2.1, trade 58 liquid
futures and forwards: 24 commodities, 12 cross-currency pairs, 9 equity
indices and 13 government bonds. This table takes the same four asset
classes from Pinnacle's CLC database, preferring Pinnacle's composite or
electronic (24-hour) symbol over the day-session one wherever both exist
(Pinnacle's manual, Appendix B-1, ``MANUAL.DOC``).

**Dollars per 1.00 of Pinnacle's price** (``multiplier``) is derived, as in
markets.py, as the manual's Min $ Move / tick size [M], in the file's own
units, and cross-checked [F] by multiplying the last in-sample ``_NON``
settle (2015, or the file's last row before that) by it to get a contract
notional that matches the exchange's contract size. The manual's
"BigPoint Value" column is not used: it is wrong for some symbols (ZI says
5000 for a file quoted in cents; YM says 10 for CBOT's $5 mini Dow).

**Roll dates** are not tabulated here: tsmom.py reads each roll straight
off the files, as the day on which ``_REV`` minus ``_NON`` steps (markets.py
derived its ``roll_months`` the same way; Pinnacle's back-adjustment moves
only on a roll).

**Excluded** markets and why are in ``EXCLUDED``: non-USD contracts (not
converted), duplicates of an included underlying (day sessions, other
contract sizes), baskets of included markets, thin markets, and one file
with a corrupt in-sample row.
"""

from collections import namedtuple
from datetime import date

import markets as faith_markets

TsmomMarket = namedtuple("TsmomMarket", [
    "symbol",       # the Pinnacle CLC symbol; files are <symbol>_REV / _NON
    "name",
    "sector",       # equity, bond, currency, commodity [MOP Section 2.1]
    "multiplier",   # dollars per 1.00 of Pinnacle's price: a number, or ((effective_date, value), ...)
    "tick_size",    # in Pinnacle's price units; one tick per contract per side is the slippage
])


def _m(symbol, name, sector, multiplier, tick_size):
    return TsmomMarket(symbol, name, sector, multiplier, tick_size)


# Each row: [M] Min $ Move / tick = multiplier; [F] last in-sample _NON
# settle x multiplier = contract notional.
TSMOM_MARKETS = {m.symbol: m for m in (
    # --- Equity indices (USD-settled only).
    # SP: day session, 1982-04-21 to 2021; the CME halved the contract from
    # $500 to $250 a point with the December 1997 contracts, so markets.py's
    # schedule is reused. [F] 2035.4 x $250 = $509k.
    _m("SP", "S&P 500 (full size)", "equity", faith_markets.MARKETS["SP"].multiplier, 0.10),
    # EN: NASDAQ-100 mini, electronic, from 1999-06-21. [M] 0.25 = $5 -> $20.
    # [F] 4587.75 x $20 = $92k. (ND, the big contract, ends 2015-06.)
    _m("EN", "NASDAQ-100 (mini)", "equity", 20.0, 0.25),
    # MD: S&P MidCap 400 mini, from 1992-02-13. [M] 0.10 = $10 -> $100.
    # [F] 1393.5 x $100 = $139k.
    _m("MD", "S&P MidCap 400 (mini)", "equity", 100.0, 0.10),
    # YM: mini Dow, CBOT, from 2002-04-03. [M] says tick 1 = $10, but CBOT's
    # mini Dow is $5 x the index (CME Group contract specs, "E-mini Dow
    # ($5)"); [F] 17,341 x $5 = $87k, the known notional. $5 is used.
    _m("YM", "Dow Jones Industrial Average (mini, $5)", "equity", 5.0, 1.0),
    # RL: Russell 2000, CME, 1993-02-04 to 2008-12-10 (the contract moved to
    # ICE). [M] 0.05 = $25 -> $500. [F] 475.7 x $500 = $238k.
    _m("RL", "Russell 2000 (CME, to 2008)", "equity", 500.0, 0.05),
    # NK: Nikkei 225 in dollars, CME, from 1991. [M] 5 = $25 -> $5; USD-
    # settled. [F] 18,805 x $5 = $94k.
    _m("NK", "Nikkei 225 (USD, CME)", "equity", 5.0, 5.0),

    # --- Government bonds. Prices are decimal points. MOP's bond set is
    # government bonds only, no short-rate futures (MOP Section 2.1); the
    # Eurodollar is excluded below.
    # US: [M] composite, 1/32 = $31.25 -> $1,000. [F] 153.75 -> $154k.
    _m("US", "30-Year U.S. Treasury Bond", "bond", 1000.0, 1.0 / 32),
    # TY: [M] composite, 1/64 = $15.63 -> $1,000. [F] 125.9 -> $126k.
    _m("TY", "10-Year U.S. Treasury Note", "bond", 1000.0, 1.0 / 64),
    # FB: [M] 5yr composite, from 1989, 1/128 = $7.8125 -> $1,000.
    # [F] 118.3 -> $118k.
    _m("FB", "5-Year U.S. Treasury Note", "bond", 1000.0, 1.0 / 128),
    # TU: [M] 2yr composite, from 1990, 1/128 = $15.63 -> $2,000 ($200k
    # face). [F] 108.6 x $2,000 = $217k.
    _m("TU", "2-Year U.S. Treasury Note", "bond", 2000.0, 1.0 / 128),

    # --- Currencies, all IMM composites, quoted in US cents per unit (the
    # yen x 10,000; markets.py).
    # AN: [M] from 1988, 0.01 = $10 -> $1,000. [F] 72.66 -> $73k (AUD 100k).
    _m("AN", "Australian Dollar", "currency", 1000.0, 0.01),
    # BN: [M] 0.01 = $6.25 -> $625. [F] 147.34 -> $92k (GBP 62,500).
    _m("BN", "British Pound", "currency", 625.0, 0.01),
    # CN: [M] 0.01 = $10 -> $1,000. [F] 72.33 -> $72k (CAD 100k).
    _m("CN", "Canadian Dollar", "currency", 1000.0, 0.01),
    # JN: [M] 0.01 = $12.50 -> $1,250. [F] 83.33 -> $104k (JPY 12.5M).
    _m("JN", "Japanese Yen", "currency", 1250.0, 0.01),
    # SN: [M] 0.01 = $12.50 -> $1,250. [F] 100.32 -> $125k (CHF 125k).
    _m("SN", "Swiss Franc", "currency", 1250.0, 0.01),
    # FN: [M] Deutschmark to 1999-03, then the euro; 0.01 = $12.50 ->
    # $1,250. [F] 108.86 -> $136k (EUR 125k).
    _m("FN", "Deutschmark, then Euro", "currency", 1250.0, 0.01),
    # MP: [M] from 1995, 0.000025 = $12.50 -> $500,000; dollars per peso.
    # [F] 0.0578 x $500,000 = $29k (MXN 500k).
    _m("MP", "Mexican Peso", "currency", 500000.0, 0.000025),

    # --- Energy. Electronic series splice the day session before 2008 [M].
    # ZU: [M] 0.01 = $10 -> $1,000. [F] 37.04 -> $37k (1,000 bbl).
    _m("ZU", "Crude Oil (WTI)", "commodity", 1000.0, 0.01),
    # ZH: [M] 0.01 cents = $4.20 -> $420 per cent. [F] 112.39 -> $47k.
    _m("ZH", "Heating Oil / ULSD", "commodity", 420.0, 0.01),
    # ZB: [M] 0.01 cents = $4.20 -> $420; unleaded gas, then RBOB
    # (markets.py). [F] 127.1 -> $53k.
    _m("ZB", "Unleaded Gasoline, then RBOB", "commodity", 420.0, 0.01),
    # ZN: [M] from 1991, 0.001 = $10 -> $10,000. [F] 2.337 -> $23k.
    _m("ZN", "Natural Gas (Henry Hub)", "commodity", 10000.0, 0.001),
    # BC: [M] ICE Brent, USD, from 2008-08. 0.01 = $10 -> $1,000.
    # [F] 37.28 -> $37k.
    _m("BC", "Brent Crude", "commodity", 1000.0, 0.01),
    # BG: [M] ICE gasoil, USD, from 2008-08. 0.25 = $25 -> $100.
    # [F] 334.25 $/t x $100 = $33k (100 t).
    _m("BG", "Gasoil", "commodity", 100.0, 0.25),

    # --- Metals.
    # ZG: [M] 0.1 = $10 -> $100. [F] 1060.2 -> $106k.
    _m("ZG", "Gold", "commodity", 100.0, 0.10),
    # ZI: [M] 0.5 cents = $25 -> $50 per cent (BigPoint 5000 is per
    # dollar). [F] 1380.3 x $50 = $69k.
    _m("ZI", "Silver", "commodity", 50.0, 0.5),
    # ZK: [M] from 1989, 0.05 = $12.50 -> $250. [F] 213.5 -> $53k.
    _m("ZK", "Copper", "commodity", 250.0, 0.05),
    # ZA: [M] 0.05 = $5 -> $100. [F] 562 -> $56k (100 oz).
    _m("ZA", "Palladium", "commodity", 100.0, 0.05),
    # ZP: [M] 0.1 = $5 -> $50. [F] 893.2 -> $45k (50 oz).
    _m("ZP", "Platinum", "commodity", 50.0, 0.10),

    # --- Grains and oilseeds, electronic (day session before 2007 [M]).
    # Prices in cents per bushel unless noted.
    # ZC: [M] 1/4 cent = $12.50 -> $50. [F] 358.75 -> $18k (5,000 bu).
    _m("ZC", "Corn", "commodity", 50.0, 0.25),
    # ZS: [M] 1/4 cent = $12.50 -> $50. [F] 864.25 -> $43k.
    _m("ZS", "Soybeans", "commodity", 50.0, 0.25),
    # ZL: [M] 0.01 cents/lb = $6 -> $600. [F] 30.75 -> $18k (60,000 lb).
    _m("ZL", "Soybean Oil", "commodity", 600.0, 0.01),
    # ZM: [M] 0.1 $/short ton = $10 -> $100. [F] 265.5 -> $27k (100 t).
    _m("ZM", "Soybean Meal", "commodity", 100.0, 0.10),
    # ZW: [M] 1/4 cent = $12.50 -> $50. [F] 470 -> $24k.
    _m("ZW", "Wheat (CBOT)", "commodity", 50.0, 0.25),
    # KW: [M] KCBT, 1/4 cent = $12.50 -> $50. [F] 468.5 -> $23k.
    _m("KW", "Wheat (Kansas City)", "commodity", 50.0, 0.25),

    # --- Livestock, electronic (day session before 2008-02 [M]); cents/lb.
    # ZT: [M] 0.025 = $10 -> $400. [F] 136.8 -> $55k (40,000 lb).
    _m("ZT", "Live Cattle", "commodity", 400.0, 0.025),
    # ZZ: [M] 0.025 = $10 -> $400. [F] 59.8 -> $24k.
    _m("ZZ", "Lean Hogs", "commodity", 400.0, 0.025),
    # ZF: [M] 0.025 = $12.50 -> $500. [F] 166.9 -> $83k (50,000 lb).
    _m("ZF", "Feeder Cattle", "commodity", 500.0, 0.025),

    # --- Softs, as markets.py (ICE's product pages agree with [M]).
    _m("SB", "Sugar No. 11", "commodity", 1120.0, 0.01),
    _m("KC", "Coffee C", "commodity", 375.0, 0.05),
    _m("CC", "Cocoa", "commodity", 10.0, 1.0),
    _m("CT", "Cotton No. 2", "commodity", 500.0, 0.01),
)}

#: Pinnacle CLC symbols not traded, and why.
EXCLUDED = {
    # Non-USD contracts. Converting them needs a daily FX series per
    # currency; not done here.
    "AP": "non-USD (AUD): Australian SPI 200",
    "AX": "non-USD (EUR): DAX",
    "CA": "non-USD (EUR): CAC 40",
    "XU": "non-USD (EUR): Euro STOXX 50",
    "XX": "non-USD: STOXX 50 is a Eurex EUR contract, though [M] says USD",
    "LX": "non-USD (GBP): FTSE 100",
    "HS": "non-USD (HKD): Hang Seng",
    "DT": "non-USD (EUR): Bund",
    "UB": "non-USD (EUR): Bobl",
    "UZ": "non-USD (EUR): Schatz",
    "GS": "non-USD (GBP): Long Gilt",
    "SS": "non-USD (GBP): Short Sterling",
    "CB": "non-USD (CAD): Canadian 10-year bond",
    # Duplicates of an included underlying.
    "ES": "duplicate of SP (S&P 500 mini)",
    "SC": "duplicate of SP (S&P 500 composite, from 1994 only)",
    "ND": "duplicate of EN (NASDAQ-100 big contract; ends 2015-06)",
    "DJ": "duplicate of YM (Dow, day session; ends 2015-06)",
    "ZD": "duplicate of YM (Dow composite; ends 2015-06)",
    "TA": "duplicate of TY (day session)",
    "UA": "duplicate of US (day session)",
    "FA": "duplicate of FB (day session)",
    "JY": "duplicate of JN (day session)",
    "SF": "duplicate of SN (day session)",
    "FX": "duplicate of FN (day session)",
    "AD": "duplicate of AN (day session)",
    "CL": "duplicate of ZU (pit)", "HO": "duplicate of ZH (pit)", "NG": "duplicate of ZN (pit)",
    "GC": "duplicate of ZG (pit)", "SI": "duplicate of ZI (pit)", "HG": "duplicate of ZK (pit)",
    "PA": "duplicate of ZA (pit)", "PL": "duplicate of ZP (pit)",
    "C_": "duplicate of ZC (pit)", "S_": "duplicate of ZS (pit)", "W_": "duplicate of ZW (pit)",
    "BO": "duplicate of ZL (pit)", "SM": "duplicate of ZM (pit)", "O_": "duplicate of ZO (pit)",
    "NR": "duplicate of ZR (pit)",
    "LC": "duplicate of ZT (pit)", "LH": "duplicate of ZZ (pit)", "FC": "duplicate of ZF (pit)",
    # Baskets of markets already in the universe.
    "DX": "basket: U.S. Dollar Index (its components are traded)",
    "CR": "basket: CRB index",
    "GI": "basket: GSCI",
    # Short rates. MOP trade no short-rate futures (Section 2.1), and 40%/sigma
    # sizing breaks on them: near the zero bound (2009-2015) the
    # Eurodollar's ex-ante volatility fell to 0.04% a year (August 2014),
    # so 40%/sigma asked for notional of about 25 times equity: up to
    # 1,719 contracts in the $1M run ($2,500 per point, tick 0.0025 =
    # $6.25 [M]). An in-sample run with it is in README.md.
    "EC": "short rate: Eurodollar, not in MOP's universe; vol scaling explodes at the zero bound",
    # Thin.
    "FF": "thin: 30-day Fed Funds",
    "MW": "thin: Minneapolis wheat",
    "ZO": "thin: oats",
    "ZR": "thin: rough rice",
    "JO": "thin: orange juice",
    "LB": "thin: lumber",
    "DA": "thin: Class III milk",
    # Data.
    "ER": "corrupt in-sample row (ER_REV line 2402: close outside the bar's range)",
}


def dollars_per_point(market, on_date):
    """The market's dollars per 1.00 of Pinnacle's price on ``on_date``
    (markets.py's own lookup: a constant or a dated schedule)."""
    return faith_markets.dollars_per_point(market, on_date)


def by_sector():
    """sector -> sorted list of symbols."""
    out = {}
    for m in TSMOM_MARKETS.values():
        out.setdefault(m.sector, []).append(m.symbol)
    return {sector: sorted(symbols) for sector, symbols in out.items()}


#: markets.py's SP schedule is effective from these dates; kept visible for tests.
SP_MULTIPLIER_CHANGE = date(1997, 11, 3)
