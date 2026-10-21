"""What a reading after the close is compared with: the previous close's book, found back through the chain of runs.

ADR 0019 and ADR 0027, amended 2026-10-18. The adapter stamps a reading as
of a close only when its holdings are the previous account snapshot's
adjusted by the fills since, share for share, and its total cash that
snapshot's reading's adjusted by those fills, within a bound
(loop.AdapterLoop._not_as_of_close). The previous close is found here, and
never assumed absent because the run now running has none: a fresh run's
first batch is compared with the last account snapshot of the run its Run
approval names, or of the one before that, back through the chain
(relay.run_chain), and an account's first run with the starting book its
approval states (schema version 3, owner-confirmed); whichever is latest.
A fresh run's approval may state a starting book too, the book the owner
confirms after a change no fill explains, and then that is the latest.

Two bases are read in one walk, newest first:

- the daily basis (previous_close's first value): the latest account
  snapshot committed in the chain, its total cash the reconciliation the
  adapter recorded from the same reading (delivery._derived names it), or
  the latest starting book; with every fill committed after it;
- the unexplained-cash basis (its second): the latest reconciliation whose
  cash the engine compared and explained (its risk.reconciliation.result,
  matched or explained with cash_compared), or the latest starting book,
  with every fill committed after it. A Boost book (account.snapshot
  version 3) states no cash, so the engine compares none, and the basis is
  then the starting book: the cash IBKR shows beyond it and every fill
  since is the drift accepted day after day, added up. A chain with
  neither takes the daily basis, so the figure is the day's drift.

A starting book is stated in the account's currency, the one the adapter
reads IBKR's balances in (snapshot.BASE_CURRENCY); one stated in another is
never compared (its Basis is refused), so the reading fails the comparison
rather than subtract cash in two currencies.

The runs before the one now running never change: each is the log its
successor's Run approval names by its final record hash (ADR 0027). So the
bases their walk finds are kept, once per run, in the adapter's own file
beside its pending file (cache_path), under the approval's previous_log and
previous_log_final_hash, and read from there on every later reading of the
run; only the run's own log is walked each time. A kept file that does not
read, or names another approval, is walked again and replaced; a file reads
only when every field is as the adapter writes it (each fill as the engine
reads one, a finite cash), and one that does not is logged, naming it.

A basis's fills are those committed after it and stamped, to the second,
at or after its as-of time (an account snapshot's or reconciliation's as_of,
a starting book's approval time) and before the reading (fills_since,
internal/reconcile's window): a fill already in the book, such as an
execution IBKR replays after the owner checked the holdings, is never
counted twice. A fill is counted as internal/reconcile's FillChange counts
one, each figure rounded to the cent as IBKR books it. Fail closed: a log that cannot
be read raises relay.ResolveRefused (run_chain), an earlier run's naming
that log.
"""
import json
import logging
import os
from collections import namedtuple
from datetime import datetime
from decimal import ROUND_HALF_UP, Decimal

from relay import LOG_DECISION, LOG_INPUT, RUN_APPROVED, is_fill, run_chain
from reports import FILL_TYPE
from snapshot import ACCOUNT_SNAPSHOT_TYPE, BASE_CURRENCY, RECONCILIATION_TYPE

RECONCILIATION_RESULT_TYPE = "risk.reconciliation.result"
# The classifications whose cash the engine accounted for exactly
# (internal/event ReconciliationMatched, ReconciliationExplained).
EXPLAINED = ("matched", "explained")
TOTAL_CASH_COMPONENT = "total-cash"
CENT = Decimal("0.01")
# The kept bases' file format (see the module).
CACHE_VERSION = 1

log = logging.getLogger("ibkr")

Basis = namedtuple("Basis", "positions cash fills source own refused as_of", defaults=("", None))
"""A book a reading is compared with: positions (symbol to shares), cash (a Decimal, or None when it was not
recorded), the fill payloads committed after it and stamped at or after its as-of time (fills_since), what
it is (for a person), whether it is the run now running's own account snapshot (own: the engine then holds
it as its book), refused: why it cannot be compared with at all ("" when it can), such as a starting book
stated in another currency, and as_of: the time it stands for (an aware datetime), its observation
cutoff."""


def cents(amount):
    """amount rounded to the cent, half away from zero, as IBKR books it (internal/reconcile's roundCents)."""
    return amount.quantize(CENT, rounding=ROUND_HALF_UP)


def cash_change(fills):
    """The cash fills move: a buy's value and commission paid, a sell's value less commission received."""
    change = Decimal(0)
    for fill in fills:
        value = cents(Decimal(str(fill["price"])) * fill["quantity"])
        commission = cents(Decimal(str(fill["commission"])))
        change += value - commission if fill["side"] == "sell" else -(value + commission)
    return change


def holdings_after(positions, fills):
    """positions with fills applied, share for share, an instrument at zero left out."""
    expected = dict(positions)
    for fill in fills:
        shares = fill["quantity"] if fill["side"] == "buy" else -fill["quantity"]
        expected[fill["instrument_id"]] = expected.get(fill["instrument_id"], 0) + shares
    return {s: q for s, q in expected.items() if q != 0}


def _time(text):
    """An RFC 3339 time as an aware datetime."""
    return datetime.fromisoformat(text.replace("Z", "+00:00"))


def fills_since(fills, after, before=None):
    """The fills of fills stamped, to the second, at or after after and before before (either None: unbounded).

    internal/reconcile's window, which the engine applies to a book: a fill
    stamped before a book's as-of time is already in that book, however
    late it was committed (an execution IBKR replays after the owner
    checked the holdings, say), so it is never counted again (ADR 0019: a
    book and the movements that explain it share one cutoff).
    """
    kept = []
    for fill in fills:
        at = _time(fill["filled_at"]).replace(microsecond=0)
        if (after is None or at >= after) and (before is None or at < before):
            kept.append(fill)
    return kept


def _book(payload, name):
    """(positions, cash, refused) of a Run approval's starting book, in the log called name."""
    book = payload["starting_book"]
    refused = ""
    if book.get("currency") != BASE_CURRENCY:
        refused = ("the starting book the owner stated in the Run approval of {} is in {!r}, not the account's "
                   "currency {}, in which the adapter reads IBKR's cash, so it is never compared: approve again with "
                   "IBKR's {} TotalCashValue (operator approve -currency {}; docs/runbooks/first-paper-order.md)"
                   .format(name, book.get("currency"), BASE_CURRENCY, BASE_CURRENCY, BASE_CURRENCY))
    return ({p["instrument_id"]: p["quantity"] for p in book["positions"]}, Decimal(str(book["total_cash"])),
            refused)


def _explained_cash(group):
    """Whether a committed reconciliation's group holds the engine's result explaining its cash."""
    for _, kind, envelope in group[1:]:
        payload = envelope["payload"]
        if (kind == LOG_DECISION and envelope["type"] == RECONCILIATION_RESULT_TYPE and isinstance(payload, dict)
                and payload.get("classification") in EXPLAINED and payload.get("cash_compared") is True):
            return True
    return False


def _walk(runs, own):
    """(daily, unexplained, later) of the module's walk over runs, each (log path, groups, approval), newest first.

    Each basis found carries the fills committed after it within runs; later
    is every fill the walk passed. own says whether runs' first is the run
    now running. A basis not found is None; the walk stops once both are.
    """
    daily = unexplained = None
    later = []  # the fill payloads committed after the point the walk has reached
    for n, (path, groups, _) in enumerate(runs):
        name = os.path.basename(path)
        mine = own and n == 0
        by_id = {group[0][2]["id"]: group[0][2] for group in groups if group[0][1] == LOG_INPUT}
        for group in reversed(groups):
            at, kind, envelope = group[0]
            kind_of = envelope["type"]
            if kind != LOG_INPUT:
                continue
            payload = envelope["payload"]
            if kind_of == FILL_TYPE:
                later.append(payload)
            elif kind_of == ACCOUNT_SNAPSHOT_TYPE and daily is None:
                paired = (envelope["id"][:-len(ACCOUNT_SNAPSHOT_TYPE)] + RECONCILIATION_TYPE
                          if envelope["id"].endswith(":" + ACCOUNT_SNAPSHOT_TYPE) else None)
                reading = by_id.get(paired, {}).get("payload") if paired else None
                cash = (Decimal(str(reading["total_cash"])) if reading is not None
                        and TOTAL_CASH_COMPONENT not in reading["unreadable"] else None)
                as_of = _time(payload["as_of"])
                daily = Basis({p["instrument_id"]: p["quantity"] for p in payload["positions"]}, cash,
                              fills_since(later, as_of),
                              "the previous close's account snapshot (as of {}) in {}".format(payload["as_of"], name),
                              mine, "", as_of)
            elif kind_of == RECONCILIATION_TYPE and unexplained is None and _explained_cash(group):
                as_of = _time(payload["as_of"])
                unexplained = Basis({}, Decimal(str(payload["total_cash"])), fills_since(later, as_of),
                                    "the reconciliation {} in {}, whose cash the engine explained".format(
                                        envelope["id"], name), mine, "", as_of)
            elif kind_of == RUN_APPROVED and isinstance(payload.get("starting_book"), dict):
                positions, cash, refused = _book(payload, name)
                # The owner checks IBKR, then approves: the approval's time
                # is the book's cutoff.
                as_of = _time(payload["approved_at"])
                source = "the starting book the owner stated in the Run approval of {}".format(name)
                if daily is None:
                    daily = Basis(positions, cash, fills_since(later, as_of), source, False, refused, as_of)
                if unexplained is None:
                    unexplained = Basis({}, cash, fills_since(later, as_of), source, False, refused, as_of)
            if daily is not None and unexplained is not None:
                return daily, unexplained, later
    return daily, unexplained, later


def _key(path, approval):
    """What the kept bases of the runs before path's are kept under: its name and its approval's previous log, or None."""
    if not isinstance(approval, dict) or not approval.get("previous_log"):
        return None
    return {"run": os.path.basename(path), "previous_log": approval["previous_log"],
            "previous_log_final_hash": approval.get("previous_log_final_hash")}


def _to_json(basis):
    if basis is None:
        return None
    return {"positions": basis.positions, "cash": None if basis.cash is None else str(basis.cash),
            "fills": basis.fills, "source": basis.source, "refused": basis.refused,
            "as_of": basis.as_of.isoformat()}


BASIS_FIELDS = ("as_of", "cash", "fills", "positions", "refused", "source")


def _from_json(raw):
    """The Basis _to_json wrote as raw, every field checked, else ValueError (see the module: walked again)."""
    if raw is None:
        return None
    if not isinstance(raw, dict) or tuple(sorted(raw)) != BASIS_FIELDS:
        raise ValueError("not a kept basis")
    positions = raw["positions"]
    if not isinstance(positions, dict) or not all(
            isinstance(s, str) and s != "" and isinstance(q, int) and not isinstance(q, bool) for s, q in positions.items()):
        raise ValueError("positions")
    # Each fill as the engine reads one (relay.is_fill), so fills_since and
    # cash_change never meet a field they cannot count.
    if not isinstance(raw["fills"], list) or not all(is_fill(fill) for fill in raw["fills"]):
        raise ValueError("fills")
    if not isinstance(raw["source"], str) or not isinstance(raw["refused"], str):
        raise ValueError("source or refused")
    cash = raw["cash"]
    if cash is not None:
        if not isinstance(cash, str):
            raise ValueError("cash")
        cash = Decimal(cash)
        if not cash.is_finite():  # Decimal reads "NaN" and "Infinity"; a book's cash is neither
            raise ValueError("cash")
    if not isinstance(raw["as_of"], str):
        raise ValueError("as_of")
    as_of = datetime.fromisoformat(raw["as_of"])
    if as_of.tzinfo is None:
        raise ValueError("as_of")
    return Basis(positions, cash, raw["fills"], raw["source"], False, raw["refused"], as_of)


def _kept(cache_path, key):
    """The (daily, unexplained) kept at cache_path under key, or None when there are none that read.

    A file that is missing, or kept for another approval, is simply walked
    again; one that is there but malformed is logged, naming it, before it
    is walked again and replaced (see the module), so a person reading the
    adapter's log learns the file was discarded.
    """
    try:
        with open(cache_path, encoding="utf-8") as f:
            text = f.read()
    except OSError:
        return None
    try:
        raw = json.loads(text)
        if not isinstance(raw, dict):
            raise ValueError("not an object")
        if raw.get("version") != CACHE_VERSION or raw.get("key") != key:
            return None
        return _from_json(raw["daily"]), _from_json(raw["unexplained"])
    except (ValueError, TypeError, KeyError, AttributeError, ArithmeticError) as err:
        log.warning("the kept bases file %s does not read (%s): walking the runs before this one again and "
                    "replacing it", cache_path, err)
        return None


def _keep(cache_path, key, bases):
    """Write bases at cache_path under key, whole or not at all."""
    tmp = cache_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump({"version": CACHE_VERSION, "key": key, "daily": _to_json(bases[0]),
                   "unexplained": _to_json(bases[1])}, f, separators=(",", ":"), allow_nan=False)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, cache_path)


def _after(basis, later):
    """basis, kept from the runs before, with later (this run's fills) committed after it too."""
    return None if basis is None else basis._replace(fills=fills_since(later, basis.as_of) + list(basis.fills))


def previous_close(engine_log_path, cache_path=None):
    """(daily, unexplained): the two bases of the module, each a Basis, or (None, None) when the chain has neither.

    cache_path, when given, is where the bases of the runs before this one
    are kept (see the module).
    """
    runs = run_chain(engine_log_path)
    path, groups, approval = next(runs)
    daily, unexplained, later = _walk([(path, groups, approval)], own=True)
    if daily is None or unexplained is None:
        key = _key(path, approval) if cache_path else None
        before = _kept(cache_path, key) if key else None
        if before is None:
            before = _walk(runs, own=False)[:2]
            if key:
                _keep(cache_path, key, before)
        daily = daily if daily is not None else _after(before[0], later)
        unexplained = unexplained if unexplained is not None else _after(before[1], later)
    if daily is not None and unexplained is None and daily.cash is not None:
        unexplained = daily._replace(positions={})
    return daily, unexplained

