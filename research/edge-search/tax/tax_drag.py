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
  * Held more than 365 days is long-term, otherwise short-term.
  * Each year's short- and long-term results are netted the IRS way: within
    each kind, then across; a net loss deducts up to LOSS_LIMIT against
    ordinary income and the rest carries forward with its character.
  * Tax is paid from the account at each year end, so after-tax wealth is the
    pre-tax path times the product over years of (1 - tax / year-end equity).
  * Prices are dividend-adjusted, so a sale's gain includes the dividends
    received while held; an unsold position's dividends are untaxed here
    unless DIVIDEND_YIELD is set, which taxes that yield on the year-end value
    of the positions still held at the dividend rate (used for SPY).
"""
import json
import sys
from collections import defaultdict, deque
from datetime import datetime, timezone

SECONDS_PER_DAY = 86400
LOSS_LIMIT = 3000.0


def year_of(t):
    return datetime.fromtimestamp(t, tz=timezone.utc).year


def realized_by_year(fills, method="fifo", diag=None):
    """{year: [short-term gain, long-term gain]} from signed fills
    (time, symbol, qty, price, fee), and {symbol: open lots} at the end.
    Long positions only: a sell beyond the lots held is ignored (short sales
    are not modelled); its quantity is added to ``diag["unmatched"]`` when a
    ``diag`` dict is given, so an export keyed by a changing ticker shows up."""
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
            long_term = (t - lot[2]) > 365 * SECONDS_PER_DAY
            out[year_of(t)][1 if long_term else 0] += gain
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


def after_tax(run, start_year, st_rate=0.22, lt_rate=0.15, method="fifo",
              dividend_yield=0.0, dividend_rate=0.15, capital=1_000_000.0):
    """Pre- and after-tax CAGR from ``start_year`` to the run's last mark:
    "after" keeps holding at the end (unrealized gains untaxed), "after_sold"
    sells everything at the end. ``capital`` is the run's starting cash: the
    unrealized gain at the end is the final equity less that capital and
    every realized gain, split short/long by the cost of the open lots older
    than a year."""
    fills, equity = run["fills"], run["equity"]
    realized, lots = realized_by_year(fills, method)
    ends = year_end_equity(equity)
    marks = [(t, e) for t, e in sorted(equity) if year_of(t) >= start_year]
    first_t, first_e = marks[0]
    last_t, last_e = marks[-1]
    years = (last_t - first_t) / (365.25 * SECONDS_PER_DAY)
    factor, carry_st, carry_lt = 1.0, 0.0, 0.0
    for year in sorted(y for y in ends if y >= start_year):
        st, lt = realized.get(year, (0.0, 0.0))
        tax, carry_st, carry_lt = year_tax(st, lt, carry_st, carry_lt, st_rate, lt_rate)
        if dividend_yield:
            tax += ends[year] * dividend_yield * dividend_rate
        if ends[year] > 0:
            factor *= max(0.0, 1.0 - tax / ends[year])
    unrealized = last_e - capital - sum(a + b for a, b in realized.values())
    old = young = 0.0
    for book in lots.values():
        for qty, cost, t in book:
            if qty > 1e-9:
                if (last_t - t) > 365 * SECONDS_PER_DAY:
                    old += qty * cost
                else:
                    young += qty * cost
    share_lt = old / (old + young) if old + young > 0 else 1.0
    final_tax, _, _ = year_tax(unrealized * (1 - share_lt), unrealized * share_lt,
                               carry_st, carry_lt, st_rate, lt_rate, loss_limit=0.0)
    pre = (last_e / first_e) ** (1 / years) - 1
    post = (last_e * factor / first_e) ** (1 / years) - 1
    post_sold = (factor * (last_e - max(0.0, final_tax)) / first_e) ** (1 / years) - 1
    return {"pre": pre, "after": post, "after_sold": post_sold, "years": years,
            "tax_drag": pre - post, "unrealized_share": unrealized / last_e if last_e else 0.0}


def main(path):
    raw = json.load(open(path))
    if isinstance(raw, str):
        raw = json.loads(raw)
    return raw


if __name__ == "__main__":
    main(sys.argv[1])
