"""The narrow interface the IBKR adapter uses over IBKR's TWS API client (ADR 0025).

Everything the adapter asks of IB Gateway goes through TWSApi, and everything
Gateway answers arrives at an Inbox. real_client.py implements TWSApi with
IBKR's own `ibapi` package; the tests implement it with a scripted fake. No
other module imports ibapi, so the adapter's logic is tested against exactly
the calls and callbacks the real client makes.

Callbacks keep ibapi's argument order and its attribute objects (Contract,
Order, Execution, ...); only the names are snake_case, and EWrapper.error's
two published signatures are reduced to one here.
"""
import queue
from collections import namedtuple

Event = namedtuple("Event", "name args")


class TWSApi:
    """The requests the adapter makes of IB Gateway.

    connect hands the Inbox to the client, which delivers every later callback
    to it, possibly from its own reader thread, and raises ConnectFailed if
    the socket connect and IBKR's handshake take longer than timeout seconds.
    Each connect is a new session: no callback of an earlier session may reach
    its Inbox. managedAccounts arrives as managed_accounts as the session
    starts.

    place_order is called only by sender.PaperSender, on a paper
    configuration (AGENTS.md rule 5), and only while can_place_orders()
    is true, which the sender asks before any order is marked or any order
    ID used. Its contract and order are the adapter's own request shape,
    not ibapi objects: plain objects carrying exactly these attributes, by
    ibapi's names (contract: conId, symbol, secType, currency, exchange;
    order: orderRef, account, action, totalQuantity, orderType, lmtPrice,
    tif, transmit). The real client builds ibapi.contract.Contract and
    ibapi.order.Order from them (real_client.ibapi_contract and
    ibapi_order), and sets every other Order field that changes what is
    sent to its plain value (real_client.ORDER_FIELDS_SET: outsideRth
    false, no parent, OCA group or conditions, whatIf false, an auxPrice of
    ibapi's unset value, and more); the fields it leaves at ibapi's default
    are listed in real_client.DEFAULTS_TO_CONFIRM, for the paper-account
    test to confirm from IBKR's own view of the order. IBKR answers on the
    Inbox.
    """

    def connect(self, host, port, client_id, callbacks, timeout):
        raise NotImplementedError

    def disconnect(self):
        raise NotImplementedError

    def is_connected(self):
        raise NotImplementedError

    def req_current_time(self):
        raise NotImplementedError

    def req_positions(self):
        raise NotImplementedError

    def cancel_positions(self):
        raise NotImplementedError

    def req_account_summary(self, req_id, group, tags):
        raise NotImplementedError

    def cancel_account_summary(self, req_id):
        raise NotImplementedError

    def req_all_open_orders(self):
        raise NotImplementedError

    def req_executions(self, req_id, account_id):
        raise NotImplementedError

    def can_place_orders(self):
        raise NotImplementedError

    def place_order(self, order_id, contract, order):
        raise NotImplementedError


def _recorder(name):
    def record(self, *args):
        self._queue.put(Event(name, args))
    record.__name__ = name
    return record


class Inbox:
    """A thread-safe queue of IB Gateway's callbacks, in the order they arrived.

    The client's reader thread only enqueues; the adapter consumes on its own
    thread through Connection.pump, so no adapter state is touched from two
    threads. Arguments are queued as received and never logged here.
    """

    CALLBACKS = (
        "next_valid_id",        # (order_id)
        "managed_accounts",     # (accounts_list): IBKR's comma-separated account IDs
        "connection_closed",    # ()
        "error",                # (req_id, code, message[, advanced_order_reject_json])
        "current_time",         # (epoch_seconds)
        "position",             # (account, contract, position, avg_cost)
        "position_end",         # ()
        "account_summary",      # (req_id, account, tag, value, currency)
        "account_summary_end",  # (req_id)
        "open_order",           # (order_id, contract, order, order_state)
        "open_order_end",       # ()
        "order_status",         # (order_id, status, filled, remaining, avg_fill_price, perm_id,
                                #  parent_id, last_fill_price, client_id, why_held, mkt_cap_price)
        "exec_details",         # (req_id, contract, execution)
        "exec_details_end",     # (req_id)
        "commission_report",    # (report)
    )

    def __init__(self):
        self._queue = queue.Queue()

    def get(self, timeout):
        """Return the next callback, or None if none arrives within timeout seconds."""
        try:
            if timeout is not None and timeout <= 0:
                return self._queue.get_nowait()
            return self._queue.get(timeout=timeout)
        except queue.Empty:
            return None


for _name in Inbox.CALLBACKS:
    setattr(Inbox, _name, _recorder(_name))
