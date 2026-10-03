"""Estimate the US federal income-tax cost of running a backtested strategy
in a taxable account instead of an IRA, from the backtest's own fills.
Research only (docs/research/boost-followup-2026-09.md, "Taxes: taxable
account or IRA"); not tax advice.

Input: a JSON export (kept outside the repository: it is derived from
market data) mapping run name -> {"fills": [[unix time, symbol, signed
quantity, price, fee], ...], "equity": [[unix time, equity], ...]}.

Model, deliberately simple and stated in the report:
  * Lots are matched FIFO (the brokers' default) or LIFO (the newest lots
    first, which is how Boost sells only its extra shares and keeps the core
    position). Fees are added to cost or taken from proceeds.
  * Long-term means held more than one year by the calendar (IRS Publication
    550: a sale on the anniversary is still short-term).
  * Each year's short- and long-term results are netted the IRS way: within
    each kind, then across; a net loss deducts up to LOSS_LIMIT against
    ordinary income and the rest carries forward with its character.
  * Tax is paid from the account at each year end, so after-tax wealth is the
    pre-tax path times the product over years of (1 - tax / year-end equity).
  * Prices are dividend-adjusted, so a sale's gain includes the dividends
    received while held. For SPY (Boost and buy-and-hold) a dividend yield is
    taxed each year on the SPY held (98% of equity) and added to the cost
    basis, so the final sale does not tax it again. For momentum stocks no
    separate dividend is modelled: their dividends ride inside the sale gains.
  * A run's open lots are valued at its recorded end prices, or, for a
    single-symbol run, at the price implied by its final holdings (equity
    less cash, cash net of any financing paid outside the fills).
"""
import json
import sys
from collections import defaultdict, deque
from datetime import datetime, timezone

SECONDS_PER_DAY = 86400
LOSS_LIMIT = 3000.0


def year_of(t):
    return datetime.fromtimestamp(t, tz=timezone.utc).year


def long_term(bought, sold):
    """IRS Publication 550: long-term if held more than one year, counting
    from the day after purchase, so a sale on the purchase date's anniversary
    is still short-term. A February 29 purchase's anniversary is February 28."""
    a = datetime.fromtimestamp(bought, tz=timezone.utc).date()
    b = datetime.fromtimestamp(sold, tz=timezone.utc).date()
    try:
        anniversary = a.replace(year=a.year + 1)
    except ValueError:
        anniversary = a.replace(year=a.year + 1, day=28)
    return b > anniversary


def realized_by_year(fills, method="fifo", diag=None):
    """{year: [short-term gain, long-term gain]} from signed fills
    (time, symbol, qty, price, fee, ...), and {symbol: open lots} at the end.
    Long positions only: a sell beyond the lots held is ignored (short sales
    are not modelled); its quantity is added to ``diag["unmatched"]`` when a
    ``diag`` dict is given, and main() refuses such a run."""
    lots = defaultdict(deque)          # symbol -> deque of [qty, unit cost, time]
    out = defaultdict(lambda: [0.0, 0.0])
    for fill in sorted(fills, key=lambda f: f[0]):
        t, sym, qty, price, fee = fill[:5]
        if qty > 0:
            lots[sym].append([qty, price + fee / qty, t])
            continue
        remaining = -qty
        unit_fee = fee / remaining if remaining else 0.0
        book = lots[sym]
        while remaining > 1e-9 and book:
            lot = book[-1] if method == "lifo" else book[0]
            take = min(remaining, lot[0])
            gain = take * (price - unit_fee - lot[1])
            out[year_of(t)][1 if long_term(lot[2], t) else 0] += gain
            lot[0] -= take
            remaining -= take
            if lot[0] <= 1e-9:
                book.pop() if method == "lifo" else book.popleft()
        if remaining > 1e-9 and diag is not None:
            diag["unmatched"] = diag.get("unmatched", 0.0) + remaining
    return dict(out), lots


def cash_at_end(fills, capital):
    """Cash after every fill: capital, less purchases and fees, plus sales."""
    cash = capital
    for fill in fills:
        qty, price, fee = fill[2], fill[3], fill[4]
        cash -= qty * price + fee
    return cash


def year_tax(st, lt, carry_st, carry_lt, st_rate, lt_rate, loss_limit=LOSS_LIMIT):
    """(tax, new short-term carryover, new long-term carryover). Carryovers
    are non-negative loss amounts. A negative tax is the benefit of the loss
    deduction against ordinary income."""
    st -= carry_st
    lt -= carry_lt
    if st >= 0 and lt >= 0:
        return st * st_rate + lt * lt_rate, 0.0, 0.0
    if st < 0 and lt < 0:
        loss = -(st + lt)
        deduct = min(loss_limit, loss)
        # The deduction uses short-term losses first.
        st_left = -st - min(deduct, -st)
        lt_left = -lt - max(0.0, deduct - (-st))
        return -deduct * st_rate, st_left, lt_left
    net = st + lt
    if net >= 0:
        # A loss of one kind offsets the gain of the other; tax the remainder at its rate.
        return (net * st_rate if st > 0 else net * lt_rate), 0.0, 0.0
    deduct = min(loss_limit, -net)
    left = -net - deduct
    return -deduct * st_rate, (left if st < 0 else 0.0), (left if lt < 0 else 0.0)


def year_end_equity(equity):
    """{year: last equity of that year} from [(time, equity)]."""
    out = {}
    for t, e in sorted(equity):
        out[year_of(t)] = e
    return out


def end_prices_for(run, lots, last_e, capital):
    """{symbol: last price} for the open lots: the run's recorded end prices,
    or for a single-symbol run the price implied by its holdings (final
    equity less cash, where cash is the fills' cash less any financing the
    run paid outside its fills)."""
    if run.get("end_prices"):
        return run["end_prices"]
    open_syms = {sym: sum(q for q, _, _ in book) for sym, book in lots.items()
                 if sum(q for q, _, _ in book) > 1e-9}
    if not open_syms:
        return {}
    if len(open_syms) > 1:
        raise SystemExit("open lots in several symbols and no recorded end prices")
    (sym, qty), = open_syms.items()
    cash = cash_at_end(run["fills"], capital) - run.get("financing", 0.0)
    return {sym: (last_e - cash) / qty}


def after_tax(run, start_year, st_rate=0.22, lt_rate=0.15, method="fifo",
              dividend_yield=0.0, dividend_rate=0.15, capital=1_000_000.0, dividend_base=1.0):
    """Pre- and after-tax CAGR from ``start_year`` to the run's last mark:
    "after" keeps holding at the end (unrealized gains untaxed), "after_sold"
    sells everything at the end, each open lot at its own gain and holding
    period. Tax years from the run's first trade are processed so a loss in a
    warm-up year carries forward; only years from ``start_year`` count.

    Dividends (``dividend_yield`` x ``dividend_base`` x year-end equity, the
    base being the share of equity in the dividend payer) are taxed each year
    and, being reinvested, added to the cost basis, so the final sale does not
    tax them again (prices are dividend-adjusted, so they are inside the sale
    price)."""
    fills, equity = run["fills"], run["equity"]
    realized, lots = realized_by_year(fills, method)
    ends = year_end_equity(equity)
    marks = [(t, e) for t, e in sorted(equity) if year_of(t) >= start_year]
    first_t, first_e = marks[0]
    last_t, last_e = marks[-1]
    years = (last_t - first_t) / (365.25 * SECONDS_PER_DAY)
    first_year = min([year_of(f[0]) for f in fills] + [start_year])
    factor, carry_st, carry_lt, dividend_basis = 1.0, 0.0, 0.0, 0.0
    for year in sorted(y for y in ends if y >= first_year):
        st, lt = realized.get(year, (0.0, 0.0))
        tax, carry_st, carry_lt = year_tax(st, lt, carry_st, carry_lt, st_rate, lt_rate)
        dividends = ends[year] * dividend_base * dividend_yield
        tax += dividends * dividend_rate
        dividend_basis += dividends
        if year >= start_year and ends[year] > 0:
            factor *= max(0.0, 1.0 - tax / ends[year])
    prices = end_prices_for(run, lots, last_e, capital)
    st_u = lt_u = 0.0
    for sym, book in lots.items():
        for qty, cost, t in book:
            if qty <= 1e-9:
                continue
            if sym not in prices:
                raise SystemExit("no end price for open lot in {}".format(sym))
            gain = qty * (prices[sym] - cost)
            if long_term(t, last_t):
                lt_u += gain
            else:
                st_u += gain
    lt_u -= dividend_basis
    final_tax, _, _ = year_tax(st_u, lt_u, carry_st, carry_lt, st_rate, lt_rate, loss_limit=0.0)
    pre = (last_e / first_e) ** (1 / years) - 1
    post = (last_e * factor / first_e) ** (1 / years) - 1
    post_sold = (factor * (last_e - max(0.0, final_tax)) / first_e) ** (1 / years) - 1
    return {"pre": pre, "after": post, "after_sold": post_sold, "years": years,
            "tax_drag": pre - post}


#: (label, run name, first measured year, lot method, dividend yield taxed
#: yearly, initial purchase only). SPY buy-and-hold's monthly 98% re-sizing is
#: not a real investor's behaviour, so only its first purchase is kept: one
#: lot, taxed on dividends yearly and on its gain at the final sale. 1.7% is
#: roughly SPY's average yield since 1999.
CASES = [
    ("Momentum, half filter", "OOS_F_HALF", 2016, "fifo", 0.0, False),
    ("Momentum, combination", "OOS_F_COMBO", 2016, "fifo", 0.0, False),
    ("Momentum, no filter", "OOS_F_NOFILTER", 2016, "fifo", 0.0, False),
    ("Momentum, full filter", "OOS_F_FULL", 2016, "fifo", 0.0, False),
    ("Momentum, half filter", "F_HALF", 1999, "fifo", 0.0, False),
    ("Momentum, combination", "F_COMBO", 1999, "fifo", 0.0, False),
    ("Momentum, no filter", "F_NOFILTER", 1999, "fifo", 0.0, False),
    ("Momentum, full filter", "F_FULL", 1999, "fifo", 0.0, False),
    ("Boost on margin", "BX_SPY", 1999, "lifo", 0.017, False),
    ("SPY buy-and-hold", "BX_SPY_BH", 1999, "fifo", 0.017, True),
]
RATES = [(0.22, 0.15), (0.24, 0.15), (0.12, 0.0)]


def load(*paths):
    """Merge exports: each maps run name -> {"fills", "equity"}."""
    runs = {}
    for path in paths:
        with open(path) as f:
            raw = json.load(f)
        if isinstance(raw, str):
            raw = json.loads(raw)
        runs.update(raw)
    return runs


def main(*paths):
    runs = load(*paths)
    for st_rate, lt_rate in RATES:
        print("\nfederal rates: short-term {:.0%}, long-term and dividends {:.0%}".format(st_rate, lt_rate))
        print("{:<30} {:>6} {:>9} {:>8} {:>9} {:>8}".format("strategy", "from", "pre-tax", "taxable", "sold-end", "drag"))
        for label, name, year, method, dy, no_fills in CASES:
            if name not in runs:
                continue
            if not runs[name]["fills"]:
                raise SystemExit("{}: no fills in the export; re-export it (an empty export would read as no tax)".format(name))
            if not runs[name]["equity"]:
                raise SystemExit("{}: no equity curve in the export".format(name))
            fills = sorted(runs[name]["fills"], key=lambda f: f[0])
            run = {"fills": fills[:1] if no_fills else fills, "equity": runs[name]["equity"]}
            diag = {}
            realized_by_year(run["fills"], method, diag)
            if diag.get("unmatched"):
                raise SystemExit("{}: {:.0f} shares sold with no matching purchase; reconcile the export first".format(
                    name, diag["unmatched"]))
            for key in ("end_prices", "financing"):
                if key in runs[name]:
                    run[key] = runs[name][key]
            r = after_tax(run, year, st_rate, lt_rate, method, dy, lt_rate, dividend_base=0.98 if dy else 1.0)
            print("{:<30} {:>6} {:>8.2f}% {:>7.2f}% {:>8.2f}% {:>7.2f}".format(
                label, year, 100 * r["pre"], 100 * r["after"], 100 * r["after_sold"], 100 * (r["pre"] - r["after"])))


if __name__ == "__main__":
    if len(sys.argv) < 2:
        raise SystemExit("usage: tax_drag.py EXPORT.json [EXPORT.json ...]")
    main(*sys.argv[1:])
