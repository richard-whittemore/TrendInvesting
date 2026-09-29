# Research script, not production code: no ADR citations. Runs locally on
# the research/futures-local engine and Pinnacle CLC files (never committed);
# see docs/research/boost-followup-2026-09.md.
"""SPY-proxy + TSMOM blends, monthly rebalanced. Research only.
PROJECT combination: fixed weights chosen before any run (80/20, 60/40,
50% and 100% overlays), not tuned. Holdout run: python3 blend.py (both spans).
S&P total return proxy = SP futures excess return (back-adjusted change over
the prior unadjusted settle) + the T-bill accrual that day."""
import sys, os, math
from dataclasses import replace
from datetime import date
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, os.path.join(ROOT, "research", "futures-local"))
import tsmom, tsmom_markets, loader, rates  # noqa: E402

DATA = os.environ.get("PINNACLE_DIR", os.path.expanduser("~/Desktop/Trend_Investing/data/pinnacle/DATA/CLCDATA"))
TB3MS = os.environ.get("TB3MS_CSV", os.path.expanduser("~/Desktop/Trend_Investing/data/rates/TB3MS.csv"))

def run(start, end, holdout, proxy='SP'):
    curve = rates.load_rate_curve(TB3MS)
    series = {}
    for s in sorted(tsmom_markets.TSMOM_MARKETS):
        paths = [loader.find_market_file(DATA, s + '_' + k) for k in ('REV', 'NON')]
        if None in paths: continue
        rev, non = (loader.load_series(p, end=end) for p in paths)
        series[s] = tsmom.join_rows(rev, non, s)
    base = tsmom.Config(start=start, end=end, allow_holdout=holdout, portfolio_target=0.10)
    out = {}
    for name, rc in (('tsmom_tr', curve), ('tsmom_xs', None)):
        res = tsmom.run_backtest(replace(base, interest_rates=rc), tsmom_markets.TSMOM_MARKETS, series)
        out[name] = tsmom.monthly_returns(res.equity_curve, res.starting_equity, start)
    rev, non = (loader.load_series(loader.find_market_file(DATA, proxy + '_' + k), end=end) for k in ('REV', 'NON'))
    sp = tsmom.join_rows(rev, non, proxy)
    v, spc = 1.0, []
    for a, b in zip(sp, sp[1:]):
        if b.date < start: continue
        v *= 1 + tsmom.daily_return(a, b) + curve.accrued_fraction(a.date, b.date)
        spc.append((b.date, v))
    out['sp'] = tsmom.monthly_returns(spc, 1.0, start)
    cash = {}
    for k, (a, b, _) in out['sp'].items():
        cash[k] = curve.accrued_fraction(a, b)
    out['cash'] = cash
    return out

def stats(rets):
    v, peak, dd, path = 1.0, 1.0, 0.0, []
    for r in rets:
        v *= 1 + r; peak = max(peak, v); dd = max(dd, 1 - v / peak)
    yrs = len(rets) / 12
    m = sum(rets) / len(rets); sd = math.sqrt(sum((x - m) ** 2 for x in rets) / (len(rets) - 1))
    return v ** (1 / yrs) - 1, sd * math.sqrt(12), dd

def report(title, o, extra_years):
    keys = sorted(k for k in o['sp'] if k in o['tsmom_tr'] and k in o['tsmom_xs'])
    sp = [o['sp'][k][2] for k in keys]; tr = [o['tsmom_tr'][k][2] for k in keys]; xs = [o['tsmom_xs'][k][2] for k in keys]
    mixes = [('S&P 100%', lambda i: sp[i]),
             ('S&P 80 / TSMOM 20', lambda i: .8 * sp[i] + .2 * tr[i]),
             ('S&P 60 / TSMOM 40', lambda i: .6 * sp[i] + .4 * tr[i]),
             ('S&P 100 + TSMOM overlay 50%', lambda i: sp[i] + .5 * xs[i]),
             ('S&P 100 + TSMOM overlay 100%', lambda i: sp[i] + xs[i]),
             ('TSMOM 100%', lambda i: tr[i])]
    print('\n' + title, '%s..%s' % (keys[0], keys[-1]))
    hdr = '%-30s %7s %6s %6s' % ('mix', 'CAGR', 'vol', 'MaxDD') + ''.join(' %8s' % (y if isinstance(y, int) else '%d-%02d' % (y[0], y[-1] % 100)) for y in extra_years)
    print(hdr)
    for name, f in mixes:
        rets = [f(i) for i in range(len(keys))]
        c, vol, dd = stats(rets)
        cells = ''
        for y in extra_years:
            ys = [rets[i] for i, k in enumerate(keys) if (k[0] in y if isinstance(y, tuple) else k[0] == y)]
            g = 1.0
            for r in ys: g *= 1 + r
            cells += ' %+7.1f%%' % (100 * (g - 1)) if ys else '      n/a'
        print('%-30s %+6.2f%% %5.1f%% %5.1f%%' % (name, 100 * c, 100 * vol, 100 * dd) + cells)
    print('correlation S&P vs TSMOM (monthly): %.2f' % tsmom.correlation(sp, tr))

if __name__ == '__main__':
    a = run(date(1985, 1, 1), date(2015, 12, 31), False)
    report('IN-SAMPLE (monthly MaxDD; year columns = calendar-year total)', a, [1987, (2000, 2001, 2002), 2008, 2011])
    b = run(date(2016, 1, 1), date(2025, 10, 31), True, proxy='ES')
    report('HOLDOUT', b, [2018, 2020, 2022])
