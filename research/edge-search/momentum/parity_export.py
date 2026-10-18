# Research script, not production code. The helper file of the remaining
# parity runs (docs/research/parity-2026-10.md, "Remaining scope, plan,
# 2026-10-19"), uploaded to the QuantConnect project beside main.py, since
# the Free plan holds 32,000 characters a file and minimum_lot.py is close to
# it. It builds the order-tag exports cmd/parity reads, and ports two Go
# rules the backtests never had, line for line, so that a run can trade them:
#   ira_plan    internal/orderplan/ira.go PlanIRA (and LimitPrice in
#               orderplan.go): whole shares from the previous close, the
#               rebalance band, sells first;
#   scale_buys  internal/orderplan/ira.go ScaleBuys: every buy scaled to the
#               budget, with the fee-aware fallback.
# Each operation is written in Go's order, so the two give the same floats.
# It imports nothing from QuantConnect: the algorithm is passed in.
import json
import math

#: Order-tag prefixes cmd/parity reads.
PARITY_PREFIX = "PARITY1 "
GLIDE_PREFIX = "GLIDE1 "
LOT_PREFIX = "LOTPARITY1 "
IRA_PREFIX = "IRAPARITY1 "
#: The tag of every order the IRA path places: IRAORD1 <month> <phase>.
IRA_ORDER_PREFIX = "IRAORD1 "


def _dumps(record):
    return json.dumps(record, separators=(",", ":"), allow_nan=False)


def namer(symbols):
    """name(sym): its ticker, or its security ID when two of ``symbols``
    share the ticker; a symbol not in ``symbols`` is its ticker."""
    tickers = {}
    for sym in symbols:
        tickers.setdefault(str(getattr(sym, "Value", sym)), set()).add(sym)

    def name(sym):
        t = str(getattr(sym, "Value", sym))
        return t if len(tickers.get(t, ())) <= 1 else str(getattr(sym, "ID", sym))
    return name


def attach(algo, tag, dummy=False):
    """Put ``tag`` on the newest open order (UpdateTag changes no fill); with
    none open, or when ``dummy``, on a 1-share SPY limit order at 1% of its
    price, cancelled at once."""
    done = False
    if not dummy:
        tickets = sorted(algo.Transactions.GetOpenOrderTickets(), key=lambda t: t.OrderId)
        done = bool(tickets) and bool(tickets[-1].UpdateTag(tag).IsSuccess)
    if not done:
        price = float(algo.Securities[algo.spy].Price)
        ticket = algo.LimitOrder(algo.spy, 1, round(max(price, 1.0) * 0.01, 2), tag=tag)
        ticket.Cancel()
        algo.export_dummies = getattr(algo, "export_dummies", 0) + 1
    algo.export_logged = getattr(algo, "export_logged", 0) + 1
    algo.export_bytes = getattr(algo, "export_bytes", 0) + len(tag)


def stat(algo):
    """The exports' end statistic."""
    return "logged={} bytes={} dummies={}".format(getattr(algo, "export_logged", 0),
                                                   getattr(algo, "export_bytes", 0),
                                                   getattr(algo, "export_dummies", 0))


def _month(algo):
    return "{:04d}-{:02d}".format(algo.Time.year, algo.Time.month)


# --- momentum export: parity_log.py's, unchanged (a test compares the source)

def parity_marks(ranked, sectors, top_n, sector_cap):
    """The mark defensive_book gives each ranked name: P picked, C skipped
    by the sector cap, - once the book is full (defensive_book's loop,
    names with no sector uncapped)."""
    marks, picks, counts = {}, 0, {}
    for sym in ranked:
        sec = sectors.get(sym) if sectors else None
        if picks >= top_n:
            marks[sym] = "-"
        elif sector_cap and sec is not None and counts.get(sec, 0) >= sector_cap:
            marks[sym] = "C"
        else:
            marks[sym] = "P"
            picks += 1
            if sec is not None:
                counts[sec] = counts.get(sec, 0) + 1
    return marks


def parity_record(month, index, columns, spy, risk_off, spy_close, spy_mean, spy_n, members, ranked, scores,
                  vol, sectors, targets, counts, top_n, sector_cap, sample=5):
    """The export of one rebalance, as the order-tag payload.

    ``index`` is the history's rows as YYYY-MM-DD, ``columns`` maps a symbol
    to its closes on those rows (NaN for none), ``counts`` each member's
    close count. ``ranked`` is every scored member, best first. Names are
    tickers; a ticker two members share is written as its security ID."""
    tickers = {}
    for sym in list(members) + list(ranked) + [spy] + list(targets):
        tickers.setdefault(str(getattr(sym, "Value", sym)), set()).add(sym)
    name = lambda sym: (str(getattr(sym, "Value", sym)) if len(tickers[str(getattr(sym, "Value", sym))]) == 1
                        else str(getattr(sym, "ID", sym)))
    marks = parity_marks(ranked, sectors, top_n, sector_cap)
    sec = lambda sym: (sectors.get(sym) if sectors else None)
    short = [s for s in members if s not in scores]
    gapped = [s for s in ranked if s in columns and any(x != x for x in columns[s][-counts.get(s, 0):])]
    chosen = [s for s in ranked if marks[s] == "P"]
    chosen += [s for s in ranked if marks[s] == "C"][:sample]
    chosen += [s for s in ranked if marks[s] == "-"][:sample]
    chosen += short[:sample] + gapped[:sample]
    closes = {name(spy): [float(x) if x == x else None for x in columns[spy]]}
    for sym in chosen:
        if name(sym) not in closes:
            col = columns.get(sym)
            closes[name(sym)] = ([float(x) if x == x else None for x in col] if col is not None
                                 else [None] * len(index))
    record = {
        "month": month, "risk_off": bool(risk_off), "spy_close": float(spy_close), "spy_mean": float(spy_mean),
        "spy_n": int(spy_n), "members": len(members), "scored": len(ranked), "index": list(index),
        "ranked": [[name(s), float(scores[s]), float(vol.get(s, 0.0)),
                    None if sec(s) is None else int(sec(s)), marks[s]] for s in ranked],
        "weights": {name(s): float(w) for s, w in targets.items()},
        "short": {name(s): int(counts.get(s, 0)) for s in short},
        "closes": closes,
    }
    return PARITY_PREFIX + json.dumps(record, separators=(",", ":"), allow_nan=False)


def momentum_export(algo, closes, spy, risk_off, mom, vol, ranked, book):
    """parity_log.py's _parity_export for ``book``, the momentum book."""
    index = [t.strftime("%Y-%m-%d") for t in closes.index]
    columns = {s: [float(x) for x in closes[s]] for s in closes.columns}
    counts = {s: sum(1 for x in col if x == x) for s, col in columns.items()}
    spy_mean = spy.iloc[-algo.TREND_DAYS:].mean() if len(spy) else 0.0
    tag = parity_record(_month(algo), index, columns, algo.spy, risk_off, spy.iloc[-1], spy_mean, len(spy),
                        algo.members, ranked, mom, vol, algo.sectors if algo.SECTOR_CAP else None, book, counts,
                        algo.TOP_N, algo.SECTOR_CAP, algo.PARITY_SAMPLE)
    attach(algo, tag)


# --- glide path

def glide_record(month, day, glide, start_age, measure_from, age, b, book, targets, name):
    """A rebalance's bond share ``b``, the owner's ``age`` it was read at,
    the momentum ``book`` and the combined ``targets``."""
    return GLIDE_PREFIX + _dumps({
        "month": month, "date": day, "glide": glide, "start_age": float(start_age),
        "measure_from": measure_from, "age": float(age), "b": float(b),
        "book": {name(s): float(w) for s, w in book.items()},
        "targets": {name(s): float(w) for s, w in targets.items()}})


def glide_export(algo, targets, closes, spy, risk_off, mom, vol, ranked):
    """After the month's trades: the glide record of this rebalance (on a
    cancelled order), and in a planned month first the momentum export of
    its book (on the newest open order). ``targets`` are what was traded."""
    last = getattr(algo, "glide_last", None)
    algo.glide_last = None
    if last is None or not algo.GLIDE_PARITY:
        return
    book, b, age = last
    if (algo.Time.year, algo.Time.month) in tuple(tuple(m) for m in algo.PARITY_MONTHS):
        momentum_export(algo, closes, spy, risk_off, mom, vol, ranked, book)
    name = namer(list(book) + list(targets))
    y, m = algo.MEASURE_FROM[0], algo.MEASURE_FROM[1]
    attach(algo, glide_record(_month(algo), algo.Time.strftime("%Y-%m-%d"), algo.GLIDE, algo.PATH_START_AGE,
                              "{:04d}-{:02d}".format(y, m), age, b, book, targets, name), dummy=True)


# --- D7-L

def lot_record(month, mode, nlv, risk_off, ranked, scores, vol, sectors, prices, held, targets, skipped, kept,
               name):
    """D7-L's inputs (every ranked name, best first, with score, volatility
    and sector; raw closes; net liquidation; holdings) and its result (the
    targets, None when withheld; the names skipped and kept)."""
    sec = lambda s: (sectors.get(s) if sectors else None)
    return LOT_PREFIX + _dumps({
        "month": month, "mode": mode, "nlv": float(nlv), "risk_off": bool(risk_off),
        "ranked": [[name(s), float(scores[s]), float(vol.get(s, 0.0)), None if sec(s) is None else int(sec(s))]
                   for s in ranked],
        "prices": {name(s): float(prices[s]) for s in ranked if s in prices},
        "held": {name(s): int(q) for s, q in held.items()},
        "weights": None if targets is None else {name(s): float(w) for s, w in targets.items()},
        "skipped": [name(s) for s in skipped], "kept": sorted(name(s) for s in kept)})


def lot_export(algo):
    """The D7-L record of this rebalance, in a planned month or when D7-L
    skipped, kept or withheld. Holdings are read now, at 8:00, before the
    month's orders fill at the open."""
    last = getattr(algo, "lot_last", None)
    algo.lot_last = None
    if last is None:
        return
    ranked, vol, risk_off, longs, nlv, book, skipped, kept = last
    planned = (algo.Time.year, algo.Time.month) in tuple(tuple(m) for m in algo.LOT_PARITY_MONTHS)
    if not (planned or skipped or kept or book is None):
        return
    ranked_set = set(ranked)
    held = {s: algo.Portfolio[s].Quantity for s in longs if s in ranked_set}
    name = namer(list(ranked) + list(book or {}) + [algo.spy])
    attach(algo, lot_record(_month(algo), algo.MIN_LOT, nlv, risk_off, ranked, algo.lot_scores, vol,
                            algo.sectors if algo.SECTOR_CAP else None, algo.raw_close, held, book, skipped, kept,
                            name))


# --- IRA execution (ADR 0026, point 2, "IRA execution")

def limit_price(side, reference, band):
    """orderplan.LimitPrice: the band's edge, rounded to a cent toward the
    reference, never past it."""
    if side == "buy":
        return max(math.floor(reference * (1 + band) * 100) / 100, reference)
    return min(math.ceil(reference * (1 - band) * 100) / 100, reference)


def ira_plan(targets, held, prices, nlv, band, price_band, working):
    """orderplan.PlanIRA on names: (sells, buys, skipped) in ascending name
    order, each order [name, shares, limit], each skip [name, side, reason,
    shares]."""
    sells, buys, skipped = [], [], []
    for inst in sorted(set(targets) | set(held)):
        price, h, t = prices[inst], held.get(inst, 0), targets.get(inst, 0.0)
        if t == 0 and h == 0:
            continue
        if t == 0:
            shares = -float(h)
        elif h == 0:
            shares = t * nlv / price
        else:
            value = h * price
            if not abs(value / nlv - t) > band * t:
                continue
            shares = (t * nlv - value) / price
        delta = math.trunc(shares)
        quantity, side = int(delta), "buy"
        if shares < 0:
            side = "sell"
            quantity = min(-quantity, h - working.get(inst, 0))
        if quantity <= 0:
            reason = "held-by-working-sells" if side == "sell" and delta != 0 else "below-one-share"
            skipped.append([inst, side, reason, abs(shares)])
            continue
        (sells if side == "sell" else buys).append([inst, quantity, limit_price(side, price, price_band)])
    return sells, buys, skipped


def _scaled(buys, scale, fees):
    kept, total = [], 0.0
    for inst, q, limit in buys:
        q = int(math.floor(q * scale))
        if q > 0:
            kept.append([inst, q, limit])
            total += q * limit + fees
    return kept, total


def scale_buys(buys, budget, fees):
    """orderplan.ScaleBuys: (kept, skipped, scale); each skip [name, shares
    it was scaled to]."""
    total = at_limit = 0.0
    for _, q, limit in buys:
        total += q * limit + fees
        at_limit += q * limit
    if total <= budget:
        return [list(b) for b in buys], [], 1.0
    if not buys:                          # Go: budget / 0 is -Inf, floored at 0
        return [], [], 0.0
    scale = max(budget / total, 0.0)
    kept, cost = _scaled(buys, scale, fees)
    if cost > budget:
        scale = max((budget - len(buys) * fees) / at_limit, 0.0)
        kept, _ = _scaled(buys, scale, fees)
    skipped = [[inst, q * scale] for inst, q, _ in buys if math.floor(q * scale) < 1]
    return kept, skipped, scale


def _previous_close(algo, closes, sym):
    """The last close of the rebalance's history, else the security's
    price; None when neither is positive."""
    if sym in closes.columns:
        col = closes[sym].dropna()
        if len(col) and float(col.iloc[-1]) > 0:
            return float(col.iloc[-1])
    secs = algo.Securities
    has = secs.ContainsKey(sym) if hasattr(secs, "ContainsKey") else sym in secs
    sec = secs[sym] if has else None
    if sec is not None and getattr(sec, "HasData", True) and float(sec.Price) > 0:
        return float(sec.Price)
    return None


#: Sessions the buys may wait for every bought security to have a price
#: (one added to the universe at the rebalance has none until its first
#: bar); after that a buy whose security still has none is not placed and
#: is recorded as unplaced.
PRICE_WAIT = 5


def _priced(algo, sym):
    secs = algo.Securities
    has = secs.ContainsKey(sym) if hasattr(secs, "ContainsKey") else sym in secs
    return has and bool(secs[sym].HasData) and float(secs[sym].Price) > 0


def ira_rebalance(algo, targets, closes):
    """Trade the month's book as the IRA planner does: plan from the account
    now (as of the previous close), send the sells, and plan the buys at
    once when there is no sell; they are sent once every bought security
    has a price. A target with no price at all waits in pending, outside
    the plan, as fresh_universe.py's unpriced targets do."""
    month = _month(algo)
    held = {kvp.Key: int(kvp.Value.Quantity) for kvp in algo.Portfolio if kvp.Value.Invested and kvp.Value.IsLong}
    syms = list(targets) + [s for s in held if s not in targets]
    name = namer(syms + [algo.spy])
    prices, unpriced = {}, []
    for s in syms:
        p = _previous_close(algo, closes, s)
        if p is None:
            unpriced.append(s)
        else:
            prices[s] = p
    working = {}
    for t in algo.Transactions.GetOpenOrderTickets():
        left = -(float(t.Quantity) - float(t.QuantityFilled))
        if float(t.Quantity) < 0 and left > 0:
            working[name(t.Symbol)] = working.get(name(t.Symbol), 0) + int(left)
    nlv = float(algo.Portfolio.TotalPortfolioValue)
    plan_targets = {name(s): w for s, w in targets.items() if s in prices}
    plan_held = {name(s): q for s, q in held.items() if s in prices}
    plan_prices = {name(s): p for s, p in prices.items()}
    sells, buys, skipped = ira_plan(plan_targets, plan_held, plan_prices, nlv, algo.REBALANCE_BAND,
                                    algo.PRICE_BAND, working)
    algo.pending, algo.pending_tickets = {s: targets[s] for s in unpriced if s in targets}, {}
    for s in unpriced:
        if s not in targets:
            algo.Liquidate(s)
    sym_of = {name(s): s for s in syms}
    for inst, q, _ in sells:
        algo.MarketOrder(sym_of[inst], -q, tag=IRA_ORDER_PREFIX + month + " sell")
    algo.ira = {"month": month, "day": algo.Time.date(), "decision": algo.Time.strftime("%Y-%m-%d"), "nlv": nlv,
                "targets": plan_targets, "held": plan_held, "prices": plan_prices, "working": working,
                "unpriced": [name(s) for s in unpriced], "sells": sells, "buys": buys, "skipped": skipped,
                "sym_of": sym_of, "wait": algo.EXEC_DELAY if sells else 0, "waited": 0, "for_sells": 0,
                "for_price": 0, "done": False}
    if not sells:
        if all(_priced(algo, sym_of[b[0]]) for b in buys):
            _ira_buys(algo)
        else:
            algo.ira["for_price"] = 1             # the next session's buy phase sends them


def ira_buy_phase(algo):
    """Each session at 8:00: once EXEC_DELAY sessions have passed since the
    rebalance (none when it had no sell), none of its sells is still working
    and every bought security has a price (for at most PRICE_WAIT sessions),
    budget and send its buys. The sessions waited for each reason are
    counted, so that waited = the delay + for_sells + for_price."""
    q = getattr(algo, "ira", None)
    if not q or q["done"] or algo.Time.date() == q["day"]:
        return
    q["waited"] += 1
    if q["waited"] < q["wait"]:
        return
    sell_tag = IRA_ORDER_PREFIX + q["month"] + " sell"
    if any(getattr(t, "Tag", "") == sell_tag for t in algo.Transactions.GetOpenOrderTickets()):
        q["for_sells"] += 1
        return
    if q["for_price"] < PRICE_WAIT and not all(_priced(algo, q["sym_of"][b[0]]) for b in q["buys"]):
        q["for_price"] += 1
        return
    _ira_buys(algo)


def _ira_buys(algo):
    q = algo.ira
    budget = float(algo.Portfolio.Cash)
    kept, scaled_skipped, scale = scale_buys(q["buys"], budget, algo.MAX_FEES)
    unplaced = []
    for inst, n, _ in kept:
        if _priced(algo, q["sym_of"][inst]):
            algo.MarketOrder(q["sym_of"][inst], n, tag=IRA_ORDER_PREFIX + q["month"] + " buy")
        else:
            unplaced.append(inst)
    record = {k: q[k] for k in ("month", "decision", "nlv", "targets", "held", "prices", "working", "unpriced",
                                "sells", "buys", "skipped", "waited", "for_sells", "for_price")}
    record.update({"buy_session": algo.Time.strftime("%Y-%m-%d"), "delay": algo.EXEC_DELAY,
                   "band": algo.REBALANCE_BAND, "price_band": algo.PRICE_BAND, "max_fees": algo.MAX_FEES,
                   "budget": budget, "scale": scale, "kept": [[i, n] for i, n, _ in kept],
                   "scaled_skipped": scaled_skipped, "unplaced": unplaced})
    q["done"] = True
    attach(algo, IRA_PREFIX + _dumps(record), dummy=True)
