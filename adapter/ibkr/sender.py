"""The order-sending seam: dry-run journals an order and sends nothing; live sending is disabled.

AGENTS.md rule 5: no live-order behaviour before the paper-trading and
limited-live gates. plan.md section 5 ("Modes") and ADR 0026 point 3:
dry-run decides and journals with send false, and never sends. Paper and
live refuse to start until those gates are passed and this module is changed
under review to allow them.

The re-check of ADR 0026 point 3's limits before sending is not here yet: it
needs risk.order.released and the hashed configuration, which are built in
Go first.
"""
from collections import namedtuple
from decimal import Decimal

ACTIONS = ("BUY", "SELL")
PRICED_ORDER_TYPES = ("LMT",)
UNPRICED_ORDER_TYPES = ("MKT",)

OrderRequest = namedtuple("OrderRequest", (
    "client_order_id account_id con_id symbol action quantity order_type limit_price tif"))
"""An order the adapter was asked to place.

client_order_id is the decision's tag, sent to IBKR as orderRef; IBKR's own
order ID is never part of a request (plan.md section 5).
"""

DryRunRecord = namedtuple("DryRunRecord", "request send")


class LiveSendingDisabled(Exception):
    """Sending to IBKR is disabled until the paper and limited-live gates (AGENTS.md rule 5)."""


def validate(request):
    """Refuse a malformed request before anything records or sends it."""
    if not isinstance(request.client_order_id, str) or not request.client_order_id:
        raise ValueError("client_order_id is required")
    if not isinstance(request.account_id, str) or not request.account_id:
        raise ValueError("account_id is required")
    if request.action not in ACTIONS:
        raise ValueError("action must be BUY or SELL")
    if not isinstance(request.quantity, Decimal) or not request.quantity.is_finite() \
            or request.quantity <= 0:
        raise ValueError("quantity must be a positive Decimal")
    if request.order_type in PRICED_ORDER_TYPES:
        if not isinstance(request.limit_price, Decimal) or not request.limit_price.is_finite() \
                or request.limit_price <= 0:
            raise ValueError("{} needs a positive Decimal limit_price".format(request.order_type))
    elif request.order_type in UNPRICED_ORDER_TYPES:
        if request.limit_price is not None:
            raise ValueError("{} takes no limit_price".format(request.order_type))
    else:
        raise ValueError("unsupported order_type {!r}".format(request.order_type))


class DryRunSender:
    """Validates and journals each request with send=False; holds no IBKR client at all."""

    def __init__(self, journal):
        self._journal = journal

    def send(self, request):
        validate(request)
        record = DryRunRecord(request=request, send=False)
        self._journal(record)
        return record


def make_sender(mode, journal):
    """Return the sender for a configured mode; only dry-run exists before the gates."""
    if mode == "dry-run":
        return DryRunSender(journal)
    if mode in ("paper", "live"):
        raise LiveSendingDisabled("mode {!r} is disabled until the paper and limited-live "
                                  "gates (AGENTS.md rule 5)".format(mode))
    raise ValueError("unknown mode {!r}".format(mode))
