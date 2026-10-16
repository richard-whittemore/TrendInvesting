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
