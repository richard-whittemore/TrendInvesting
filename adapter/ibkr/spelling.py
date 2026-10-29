"""One stock's name in the data job's spelling and in IBKR's (ADR 0025, amended 2026-10-29).

Massive, the universe sources and so the engine spell a share class with a
dot (BRK.B); IBKR's contract symbol spells it with a space ("BRK B"). The
adapter keeps every name in the engine's spelling inside it, and turns it
into IBKR's only where it names a contract to IBKR (a contract lookup, a
market-data request, an order's contract), and back where it reads one from
IBKR (a position, an open order, an execution), so the book, the closes, the
marks and the configuration's con_ids all join on one name. The rule is
fixed and checked against IBKR's own answer (snapshot._one_contract), never
a guess of which contract is meant. A name with neither a dot nor a space
is the same in both.
"""


def ibkr_symbol(instrument):
    """IBKR's spelling of a name the engine uses: a class share's dot as a space (BRK.B -> "BRK B")."""
    return instrument.replace(".", " ")


def engine_symbol(symbol):
    """The engine's spelling of a symbol IBKR gives: a class share's space as a dot ("BRK B" -> BRK.B)."""
    return symbol.replace(" ", ".") if isinstance(symbol, str) else symbol
