"""Split- and dividend-adjusted closes, derived from recorded raw bars and
corporate actions.

Convention (backward, proportional "ratio" adjustment, the one QuantConnect's
Adjusted normalization mode uses for equities: LEAN factor files hold a
price factor per event date, and adjusted price = raw price x the product of
the factors of all events dated after the bar):
- a split with execution date D has factor split_from / split_to and applies
  to every bar dated before D;
- a cash dividend with ex-dividend date D has factor
  (P - cash) / P, where P is the ticker's raw (unadjusted) close of the
  session immediately before D, and applies to every bar dated before D;
- bars on or after the last event are unchanged, so the series is adjusted to
  the last recorded session; QuantConnect adjusts to the present, so levels
  differ from its data by a constant factor per ticker while returns, and
  therefore momentum and volatility, agree. #324's parity test confirms this
  against QuantConnect's logs.
Massive's dividend cash_amount is on the basis of its ex-date, not restated
for later splits (checked 2026-10-16: AAPL's 2020-08-07 dividend reads 0.82,
the pre-split amount, though the 4-for-1 split executed 2020-08-31), which
is what the factor above needs: P and cash on the same, ex-date basis.
Anything that cannot be adjusted exactly raises CannotAdjust.
"""
import datetime as dt
import math


class CannotAdjust(Exception):
    pass


def _date(text):
    try:
        return dt.date.fromisoformat(text)
    except (TypeError, ValueError):
        raise CannotAdjust("bad event date {!r}".format(text)) from None


def adjusted_closes(history, ticker, splits, dividends):
    """[(session, adjusted close)] for ``ticker`` over ``history``
    ([(session, bars)] oldest first, as history.load returns)."""
    closes = []   # (session index, session, raw close)
    for i, (day, bars) in enumerate(history):
        for row in bars:
            if row.get("T") == ticker:
                if row.get("c") is None:
                    raise CannotAdjust("{} has no close on {}".format(ticker, day))
                closes.append((i, day, row["c"]))
    if not closes:
        raise CannotAdjust("{} has no bars in the history".format(ticker))
    first_day, last_day = history[0][0], history[-1][0]
    by_day = {day: (i, c) for i, day, c in closes}
    events = []   # (date, factor)
    for r in splits:
        if r.get("ticker") != ticker:
            continue
        d = _date(r.get("execution_date"))
        if not first_day < d <= last_day:
            continue
        frm, to = r.get("split_from"), r.get("split_to")
        if not frm or not to or frm <= 0 or to <= 0:
            raise CannotAdjust("{} split on {} has ratio {}:{}".format(ticker, d, frm, to))
        events.append((d, frm / to))
    for r in dividends:
        if r.get("ticker") != ticker:
            continue
        d = _date(r.get("ex_dividend_date"))
        if not first_day < d <= last_day:
            continue
        cash = r.get("cash_amount")
        if cash is None or cash < 0:
            raise CannotAdjust("{} dividend on {} has cash {}".format(ticker, d, cash))
        pos = next(i for i, (day, _) in enumerate(history) if day >= d)   # first session on or after D
        prior_day = history[pos - 1][0] if pos > 0 else None
        if prior_day is None or prior_day not in by_day:
            if not any(day < d for _, day, _ in closes):
                continue   # no bars before the ex-date: nothing to adjust
            raise CannotAdjust("{} has no close on the session before ex-date {}".format(ticker, d))
        prior_close = by_day[prior_day][1]
        if cash >= prior_close:
            raise CannotAdjust("{} dividend {} on {} is not below the prior close {}".format(ticker, cash, d, prior_close))
        events.append((d, (prior_close - cash) / prior_close))
    out = []
    for _, day, raw in closes:
        factor = 1.0
        for d, f in events:
            if day < d:
                factor *= f
        out.append((day, raw * factor))
    return out


def _positive(value):
    return (isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(value) and value > 0)


def forward_factors(ticker, sessions, closes, splits, dividends, base):
    """{session: (split multiplier, total multiplier)} for every session of
    ``closes`` ({session: raw close}, one ticker) on or after ``base``.

    The forward form of the convention above, for a stream of bars recorded
    one session at a time: a session's multipliers are the products of the
    inverse factors of the events dated after the base (or after the
    ticker's first bar on or after it, if later) and on or before the
    session, so a bar's figures depend only on events up to its own session
    and are never restated by a later one. A split multiplies by
    split_to / split_from; a cash dividend by P / (P - cash), P the raw close
    of the session before its ex-date. Price x total multiplier is the split-
    and dividend-adjusted price; price x split multiplier the split-adjusted
    one; volume / split multiplier the volume in the base session's shares.
    Ratios between sessions equal those of adjusted_closes.

    ``sessions`` is every trading day loaded, oldest first, from the base to
    the last; ``splits`` and ``dividends`` are the ticker's own rows, each
    event once. A split and a dividend on one date, two splits on one date,
    a non-positive or non-finite figure, or a dividend whose prior session
    has no close raises CannotAdjust.
    """
    if base not in sessions:
        raise CannotAdjust("{} base {} is not a loaded session".format(ticker, base))
    days = [s for s in sessions if s >= base]
    for day in days:
        if day in closes and not _positive(closes[day]):
            raise CannotAdjust("{} close on {} is {!r}".format(ticker, day, closes[day]))
    seen = [d for d in days if d in closes]
    if not seen:
        return {}
    first, last = seen[0], days[-1]
    events, split_days = [], set()
    for r in splits:
        d = _date(r.get("execution_date"))
        if not first < d <= last:
            continue
        frm, to = r.get("split_from"), r.get("split_to")
        if not _positive(frm) or not _positive(to):
            raise CannotAdjust("{} split on {} has ratio {!r}:{!r}".format(ticker, d, frm, to))
        if d in split_days:
            raise CannotAdjust("{} has two splits on {}".format(ticker, d))
        split_days.add(d)
        events.append((d, to / frm, to / frm))
    for r in dividends:
        d = _date(r.get("ex_dividend_date"))
        if not first < d <= last:
            continue
        cash = r.get("cash_amount")
        if not (isinstance(cash, (int, float)) and not isinstance(cash, bool) and math.isfinite(cash) and cash >= 0):
            raise CannotAdjust("{} dividend on {} has cash {!r}".format(ticker, d, cash))
        if d in split_days:
            raise CannotAdjust("{} has a split and a dividend on the same date {}".format(ticker, d))
        prior = [s for s in days if s < d][-1]   # exists: first < d
        if prior not in closes:
            raise CannotAdjust("{} has no close on the session before ex-date {}".format(ticker, d))
        p = closes[prior]
        if cash >= p:
            raise CannotAdjust("{} dividend {} on {} is not below the prior close {}".format(ticker, cash, d, p))
        events.append((d, 1.0, p / (p - cash)))
    events.sort(key=lambda e: e[0])
    out, split_m, total_m, i = {}, 1.0, 1.0, 0
    for day in days:
        while i < len(events) and events[i][0] <= day:
            split_m *= events[i][1]
            total_m *= events[i][2]
            i += 1
        if day in closes:
            out[day] = (split_m, total_m)
    return out
