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

A fill is counted as internal/reconcile's FillChange counts one, each
figure rounded to the cent as IBKR books it. Fail closed: a log that cannot
be read raises relay.ResolveRefused (run_chain).
"""
import os
from collections import namedtuple
from decimal import ROUND_HALF_UP, Decimal

from relay import LOG_DECISION, LOG_INPUT, RUN_APPROVED, run_chain
from reports import FILL_TYPE
from snapshot import ACCOUNT_SNAPSHOT_TYPE, RECONCILIATION_TYPE

RECONCILIATION_RESULT_TYPE = "risk.reconciliation.result"
# The classifications whose cash the engine accounted for exactly
# (internal/event ReconciliationMatched, ReconciliationExplained).
EXPLAINED = ("matched", "explained")
TOTAL_CASH_COMPONENT = "total-cash"
CENT = Decimal("0.01")

Basis = namedtuple("Basis", "positions cash fills source own")
"""A book a reading is compared with: positions (symbol to shares), cash (a Decimal, or None when it was not
recorded), the fill payloads committed after it, what it is (for a person), and whether it is the run now
running's own account snapshot (own: the engine then holds it as its book)."""


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


def _book(payload):
    """(positions, cash) of a Run approval's starting book."""
    book = payload["starting_book"]
    return ({p["instrument_id"]: p["quantity"] for p in book["positions"]}, Decimal(str(book["total_cash"])))


def _explained_cash(group):
    """Whether a committed reconciliation's group holds the engine's result explaining its cash."""
    for _, kind, envelope in group[1:]:
        payload = envelope["payload"]
        if (kind == LOG_DECISION and envelope["type"] == RECONCILIATION_RESULT_TYPE and isinstance(payload, dict)
                and payload.get("classification") in EXPLAINED and payload.get("cash_compared") is True):
            return True
    return False


def previous_close(engine_log_path):
    """(daily, unexplained): the two bases of the module, each a Basis, or (None, None) when the chain has neither."""
    daily = unexplained = None
    later = []  # the fill payloads committed after the point the walk has reached
    for n, (path, groups, approval) in enumerate(run_chain(engine_log_path)):
        name = os.path.basename(path)
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
                daily = Basis({p["instrument_id"]: p["quantity"] for p in payload["positions"]}, cash, list(later),
                              "the previous close's account snapshot (as of {}) in {}".format(payload["as_of"], name),
                              n == 0)
            elif kind_of == RECONCILIATION_TYPE and unexplained is None and _explained_cash(group):
                unexplained = Basis({}, Decimal(str(payload["total_cash"])), list(later),
                                    "the reconciliation {} in {}, whose cash the engine explained".format(
                                        envelope["id"], name), n == 0)
            elif kind_of == RUN_APPROVED and isinstance(payload.get("starting_book"), dict):
                positions, cash = _book(payload)
                source = "the starting book the owner stated in the Run approval of {}".format(name)
                if daily is None:
                    daily = Basis(positions, cash, list(later), source, False)
                if unexplained is None:
                    unexplained = Basis({}, cash, list(later), source, False)
            if daily is not None and unexplained is not None:
                return daily, unexplained
    if daily is not None and unexplained is None and daily.cash is not None:
        unexplained = Basis({}, daily.cash, daily.fills, daily.source, daily.own)
    return daily, unexplained
