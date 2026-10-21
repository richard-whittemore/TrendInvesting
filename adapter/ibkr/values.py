"""Exact decimal reading of the numbers IBKR reports (ADR 0019: no binary-float money)."""
from decimal import Decimal, InvalidOperation


def to_decimal(value, what):
    """Return value as a finite Decimal, or raise ValueError naming what.

    IBKR delivers account values as strings, quantities as Decimal, and prices
    as Python floats. A float is read through repr, its shortest round-trip
    form, so 101.25 becomes Decimal("101.25") and not the binary expansion.
    Booleans, NaN and infinities are refused rather than coerced.
    """
    if isinstance(value, bool) or value is None:
        raise ValueError("{} is not a number: {!r}".format(what, value))
    if isinstance(value, Decimal):
        result = value
    elif isinstance(value, int):
        result = Decimal(value)
    elif isinstance(value, float):
        result = Decimal(repr(value))
    elif isinstance(value, str):
        try:
            result = Decimal(value.strip())
        except InvalidOperation:
            raise ValueError("{} is not a number: {!r}".format(what, value)) from None
    else:
        raise ValueError("{} is not a number: {!r}".format(what, value))
    if not result.is_finite():
        raise ValueError("{} is not finite: {!r}".format(what, value))
    return result
