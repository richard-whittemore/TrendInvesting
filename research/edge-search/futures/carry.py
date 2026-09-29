# Research script, not production code: no ADR citations. Runs locally on
# the research/futures-local engine and Pinnacle CLC files (never committed);
# see docs/research/boost-followup-2026-09.md.
"""Futures carry (Koijen, Moskowitz, Pedersen & Vrugt 2018, "Carry", JFE),
time-series version, on the TSMOM engine: same universe, 40%/sigma sizing,
10% overlay, costs and T-bill interest. Research only.

Carry from Pinnacle files: on a roll day the _REV change is the new
contract's (tsmom.py docstring), so the step in (_REV - _NON) equals the
old-minus-new contract spread on the previous day. Carry = spread / new
price, annualised by the days since the previous roll. Long positive carry
(backwardation), short negative (contango). Signal = latest roll's carry,
held until the next roll (stale after 400 days).
Provenance: PUBLISHED, ADAPTED -- the time-series carry of Koijen et al.
(2018), measured only at roll dates from back-adjusted files; combo is a
PROJECT combination. Both were declared before the single holdout run.
MODE carry: sign(carry). MODE combo: (sign(momentum) + sign(carry)) / 2."""
import sys, os, bisect
from dataclasses import replace
from datetime import date
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, os.path.join(ROOT, "research", "futures-local"))
import tsmom, tsmom_markets, loader, rates  # noqa: E402

DATA = os.environ.get("PINNACLE_DIR", os.path.expanduser("~/Desktop/Trend_Investing/data/pinnacle/DATA/CLCDATA"))
TB3MS = os.environ.get("TB3MS_CSV", os.path.expanduser("~/Desktop/Trend_Investing/data/rates/TB3MS.csv"))

def carry_series(rows):
    out, last_roll = [], None
    for a, b in zip(rows, rows[1:]):
        step = (b.rev - b.non) - (a.rev - a.non)
        if abs(step) < 1e-9:
            continue
        new_prev = b.non - (b.rev - a.rev)        # new contract's previous settle
        if new_prev > 0 and last_roll is not None:
            days = (b.date - last_roll).days
            if days >= 20:
                out.append((b.date, step / new_prev * 365.0 / days))
        last_roll = b.date
    return out

def load(end):
    series = {}
    for s in sorted(tsmom_markets.TSMOM_MARKETS):
        paths = [loader.find_market_file(DATA, s + '_' + k) for k in ('REV', 'NON')]
        if None in paths: continue
        rev, non = (loader.load_series(p, end=end) for p in paths)
        series[s] = tsmom.join_rows(rev, non, s)
    return series

def run(start, end, holdout, mode):
    series = load(end)
    carry = {s: carry_series(r) for s, r in series.items()}
    dates = {s: [d for d, _ in c] for s, c in carry.items()}
    orig_tr, orig_sig = tsmom.trailing_return, tsmom.signal
    owner = {}
    def carry_on(sym, day):
        i = bisect.bisect_right(dates[sym], day) - 1
        if i < 0 or (day - dates[sym][i]).days > 400: return None
        return carry[sym][i][1]
    def tr(history, as_of, months=tsmom.LOOKBACK_MONTHS):
        mom = orig_tr(history, as_of, months)
        c = carry_on(owner[id(history)], as_of)
        if c is None or (mode == 'combo' and mom is None): return None
        return (mom, c)
    def sig(pair):
        mom, c = pair
        sc = orig_sig(c)
        return sc if mode == 'carry' else (orig_sig(mom) + sc) / 2
    tsmom.trailing_return, tsmom.signal = tr, sig
    try:
        cfg = tsmom.Config(start=start, end=end, allow_holdout=holdout, portfolio_target=0.10,
                           interest_rates=rates.load_rate_curve(TB3MS))
        bt = tsmom.Backtester(cfg, tsmom_markets.TSMOM_MARKETS, series)
        for s, st in bt.states.items(): owner[id(st.history)] = s
        tsmom.check_dates(cfg)
        res = bt.run()
    finally:
        tsmom.trailing_return, tsmom.signal = orig_tr, orig_sig
    m = tsmom.metrics(cfg, res)
    per = ' '.join('%s %+.1f%%' % (l, 100 * v) for l, v in m['periods'] if v is not None)
    print('%-6s %s..%s CAGR %+.2f%% vol %.1f%% Sharpe %.2f MaxDD %.1f%% | %s | reconcile %s' % (
        mode, start, end, 100 * m['cagr'], 100 * m['vol'], m['sharpe'], 100 * m['max_drawdown'], per, res.reconcile.ok))
    return res

if __name__ == '__main__':
    if '--diag' in sys.argv:
        s = load(date(2015, 12, 31))
        for sym in ('CL', 'GC', 'ZC', 'US', 'SP', 'JN'):
            if sym in s:
                c = carry_series(s[sym]); print(sym, len(c), [(str(d), round(v, 3)) for d, v in c[-4:]])
        sys.exit()
    hold = '--holdout' in sys.argv
    a, b = (date(2016, 1, 1), date(2025, 10, 31)) if hold else (date(1985, 1, 1), date(2015, 12, 31))
    for mode in ('carry', 'combo'):
        run(a, b, hold, mode)
