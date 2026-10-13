"""Estimate the US federal tax on a strategy held in a child's custodial
(UTMA/UGMA) account, where the "kiddie tax" applies, from a backtest's own
fills. Research only (docs/research/boost-followup-2026-09.md, "Taxes in a
custodial account"); not tax advice.

It reuses tax_drag.py's lot matching (FIFO or LIFO), holding periods and
dividend handling, and replaces the flat rates with the kiddie-tax tiers
(IRS Publication 929; Form 8615). The child has only investment income:
  * the first half of the year's threshold is covered by the child's
    standard deduction, ordinary income (short-term gains) first;
  * the rest up to the threshold is taxed at the child's own rates: 10% on
    ordinary income, and 0% on qualified dividends and long-term gains
    (5% in 2006-2007, before the 0% rate began);
  * income above the threshold is taxed at the parents' rates (the study's
    22% / 15%), split between ordinary and preferential income in
    proportion to the child's total.
A year's net capital loss offsets up to LOSS_LIMIT of that year's
dividends; the rest carries forward with its character. The tiers are in
dollars, so the run (made with $1,000,000) is scaled to each starting
account size, and each year's income is scaled by the tax already paid out
of the account. Simplified on purpose: no other income for the child, no
state tax, 2018-2019 taxed at the parents' rates (the SECURE Act let
families elect that retroactively), and the 2026 threshold is assumed equal
to 2025's.
"""
import json
import sys

import tax_drag as td

#: Kiddie-tax threshold (unearned income above it is taxed at the parents'
#: rates); half of it is the child's standard deduction against unearned
#: income. IRS Publication 929 for each year.
THRESHOLD = {2006: 1700, 2007: 1700, 2008: 1800, 2009: 1900, 2010: 1900, 2011: 1900,
             2012: 1900, 2013: 2000, 2014: 2000, 2015: 2100, 2016: 2100, 2017: 2100,
             2018: 2100, 2019: 2200, 2020: 2200, 2021: 2200, 2022: 2300, 2023: 2500,
             2024: 2600, 2025: 2700, 2026: 2700}
CHILD_ORDINARY = 0.10


def child_preferential(year):
    return 0.05 if year < 2008 else 0.0


def net_capital(st, lt, carry_st, carry_lt):
    """(short-term gain, long-term gain, loss to deduct, new carry_st, new
    carry_lt), netted as tax_drag.year_tax does. Gains are non-negative; a
    net loss is returned whole as the loss to deduct, with no carryover yet
    (kiddie_tax decides how much of it the year can use)."""
    st -= carry_st
    lt -= carry_lt
    if st >= 0 and lt >= 0:
        return st, lt, 0.0, 0.0, 0.0
    net = st + lt
    if net >= 0:
        # A loss of one kind offsets the gain of the other; the rest keeps its kind.
        return (net, 0.0, 0.0, 0.0, 0.0) if st > 0 else (0.0, net, 0.0, 0.0, 0.0)
    # A net loss, reported with its character so the unused part can carry:
    # both kinds when both lost, else only the losing kind's remainder.
    if st < 0 and lt < 0:
        return 0.0, 0.0, -net, -st, -lt
    return (0.0, 0.0, -net, -net, 0.0) if st < 0 else (0.0, 0.0, -net, 0.0, -net)


def kiddie_tax(year, ordinary, preferential, parent_ordinary=0.22, parent_preferential=0.15):
    """Federal tax on a child's year of investment income: ``ordinary``
    (short-term gains) and ``preferential`` (qualified dividends and
    long-term gains), both non-negative."""
    total = ordinary + preferential
    if total <= 0:
        return 0.0
    threshold = THRESHOLD[year]
    deduction = threshold / 2.0
    ord_left = ordinary - min(ordinary, deduction)
    pref_left = preferential - min(preferential, max(0.0, deduction - ordinary))
    above = max(0.0, total - threshold)
    ord_share = ordinary / total
    above_ord, above_pref = above * ord_share, above * (1.0 - ord_share)
    mid_ord = max(0.0, ord_left - above_ord)
    mid_pref = max(0.0, pref_left - above_pref)
    return (above_ord * parent_ordinary + above_pref * parent_preferential
            + mid_ord * CHILD_ORDINARY + mid_pref * child_preferential(year))


def custodial(run, start_year, start_value, method="lifo", dividend_yield=0.017, dividend_base=0.98,
              parent_rates=(0.22, 0.15)):
    """Pre-tax and after-tax CAGR and the final values of the run scaled so
    its first mark from ``start_year`` is worth ``start_value`` dollars, with
    each year's kiddie tax paid from the account at year end; open lots are
    held at the end (the child decides later when to sell)."""
    fills, equity = run["fills"], run["equity"]
    realized, _ = td.realized_by_year(fills, method)
    ends = td.year_end_equity(equity)
    marks = [(t, e) for t, e in sorted(equity) if td.year_of(t) >= start_year]
    first_t, first_e = marks[0]
    last_t, last_e = marks[-1]
    scale = start_value / first_e
    years = (last_t - first_t) / (365.25 * td.SECONDS_PER_DAY)
    first_year = min([td.year_of(f[0]) for f in fills] + [start_year])
    factor, carry_st, carry_lt, paid = 1.0, 0.0, 0.0, 0.0
    for year in sorted(y for y in set(ends) | set(realized) if y >= first_year):
        if year not in ends:
            raise SystemExit("no equity mark in {}".format(year))
        k = scale * factor
        st, lt = realized.get(year, (0.0, 0.0))
        # Carryovers are dollars; this year's results are the run's scaled to the account.
        st, lt, loss, carry_st, carry_lt = net_capital(st * k, lt * k, carry_st, carry_lt)
        dividends = ends[year] * dividend_base * dividend_yield * k
        used = min(td.LOSS_LIMIT, loss, dividends)
        if loss:
            # The deduction uses short-term losses first; the rest carries.
            from_st = min(used, carry_st)
            carry_st -= from_st
            carry_lt -= used - from_st
        tax = kiddie_tax(year, st, lt + dividends - used, *parent_rates)
        value = ends[year] * k
        if year >= start_year and value > 0:
            factor *= max(0.0, 1.0 - tax / value)
            paid += tax
    pre = (last_e / first_e) ** (1 / years) - 1
    post = (last_e * factor / first_e) ** (1 / years) - 1
    return {"pre": pre, "after": post, "final_pre": start_value * last_e / first_e,
            "final_after": start_value * factor * last_e / first_e, "tax_paid": paid, "years": years}


def hold_one_lot(run, from_time):
    """A buy-and-hold control from ``from_time``: the run's equity from then
    on, renormalised, and a single purchase of 98% of it at a nominal price
    of 1 (only the dividends and the final value matter)."""
    equity = [(t, e) for t, e in run["equity"] if t >= from_time]
    t0, e0 = equity[0]
    return {"fills": [[t0, "SPY", 0.98 * e0, 1.0, 0.0]], "equity": equity}


SIZES = (10_000, 25_000, 50_000, 100_000)


def main(sso_path, spy_path):
    sso = td.load(sso_path)["BE_SSO_F"]
    sso["fills"] = sorted(sso["fills"], key=lambda f: f[0])
    spy = hold_one_lot(td.load(spy_path)["BX_SPY_BH"], sso["equity"][0][0])
    print("custodial account from {} (kiddie tax, parents at 22% / 15%)".format(
        td.datetime.fromtimestamp(sso["equity"][0][0], tz=td.timezone.utc).date()))
    print("{:<26} {:>9} {:>8} {:>8} {:>12} {:>12} {:>10}".format(
        "strategy", "start", "pre-tax", "after", "final pre", "final after", "tax paid"))
    for size in SIZES:
        for label, run, method in (("Boost via SSO, LIFO", sso, "lifo"), ("Boost via SSO, FIFO", sso, "fifo"),
                                   ("SPY buy-and-hold", spy, "fifo")):
            r = custodial(run, 2006, size, method)
            print("{:<26} {:>9,} {:>7.2f}% {:>7.2f}% {:>12,.0f} {:>12,.0f} {:>10,.0f}".format(
                label, size, 100 * r["pre"], 100 * r["after"], r["final_pre"], r["final_after"], r["tax_paid"]))


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: custodial.py SSO_EXPORT.json BOOST_SPY_EXPORT.json")
    main(*sys.argv[1:])
