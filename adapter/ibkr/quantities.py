"""Exact broker shares and engine units (ADR 0026 D7-F; internal/event/quantity.go).

A missing scale is legacy whole shares; 10000 is ten-thousandths. No other
scale or rounding is allowed. Broker readings use Decimal shares; durable
engine records use signed int64 units, nonnegative for D7-F.
"""
from decimal import Decimal
from values import to_decimal

SCALE = 10000
INT64_MAX = (1 << 63) - 1


def valid_scale(scale):
    return type(scale) is int and scale in (0, SCALE)


def quantity_scale(config):
    """The configured account's input contract selects D7-F, never a reported fraction."""
    return SCALE if config.account_snapshot_version == 6 else 0


def _scale(scale):
    if not valid_scale(scale):
        raise ValueError("quantity_scale must be 0 (whole-share) or 10000 (D7-F)")


def units(value, scale=0, what="quantity"):
    """Convert finite broker shares without rounding or Decimal context arithmetic."""
    _scale(scale)
    value = to_decimal(value, what)
    if scale == 0:
        if value != value.to_integral_value():
            raise ValueError("{} {} is fractional on a whole-share (D7-L) account".format(what, value))
        if value < Decimal(-(1 << 63)) or value > Decimal(INT64_MAX):
            raise ValueError("{} overflows int64 quantity units".format(what))
        result = int(value)
    else:
        if value < 0:
            raise ValueError("{} {} must be nonnegative for D7-F".format(what, value))
        if value.as_tuple().exponent < -4:
            raise ValueError("{} {} exceeds four decimal places".format(what, value))
        if value > Decimal("922337203685477.5807"):
            raise ValueError("{} overflows int64 quantity units".format(what))
        whole, _, fraction = format(value, "f").partition(".")
        result = int(whole + fraction.ljust(4, "0"))
    if result < -(1 << 63) or result > INT64_MAX:
        raise ValueError("{} overflows int64 quantity units".format(what))
    return result


def shares(value, scale=0):
    """Decode exact engine units into broker shares, refusing a malformed wire value."""
    _scale(scale)
    if type(value) is not int or not -(1 << 63) <= value <= INT64_MAX or (scale and value < 0):
        raise ValueError("quantity must be int64 units, nonnegative for D7-F")
    if not scale:
        return Decimal(value)
    return Decimal("{}.{:04d}".format(value // SCALE, value % SCALE))
