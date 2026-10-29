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
    configuration (AGENTS.md rule 5), and only while is_connected() and
    can_place_orders() are true, which the sender asks before any order is
    marked or any order ID used. Its contract and order are the adapter's own request shape,
    not ibapi objects: plain objects carrying exactly these attributes, by
    ibapi's names (contract: conId, symbol, secType, currency, exchange;
    order: orderRef, account, action, totalQuantity, orderType, lmtPrice,
    tif, transmit). The real client builds ibapi.contract.Contract and
    ibapi.order.Order from them (real_client.ibapi_contract and
    ibapi_order), and sets every other Order field that changes what is
    sent to its plain value (real_client.ORDER_FIELDS_SET: outsideRth
    false, no parent, OCA group or conditions, whatIf false, an auxPrice of
    ibapi's unset value, and more); real_client.DEFAULTS_TO_CONFIRM lists
    the defaults the paper test confirms from IBKR's own view of the order
    (not every field ibapi's Order has). IBKR answers on the Inbox.

    req_account_updates(True, account_id) subscribes to the account's values
    (TWS API reqAccountUpdates): IBKR answers each as account_value and ends
    the download with account_download_end, both naming the account and
    neither carrying a request ID; req_account_updates(False, account_id)
    unsubscribes. One subscription per client, asked for again freely.

    req_mkt_data asks for one market-data snapshot of a contract (a plain
    object with conId, symbol, secType, currency and exchange, as
    place_order's contract): IBKR answers with tick_price callbacks under
    req_id and ends it with tick_snapshot_end, or answers error under
    req_id. cancel_mkt_data cancels one not yet ended.

    req_contract_details asks IBKR for the contracts matching a name (a
    plain object with symbol, secType, currency and exchange, and no conId:
    the one lookup by name the adapter makes, snapshot.resolve_contracts):
    IBKR answers each match as contract_details under req_id and ends the
    list with contract_details_end, or answers error under req_id.

    can_place_orders is the sending switch alone (configuration and login);
    is_connected reports the link, and the sender asks it first, so a link
    down holds the input rather than read as the switch being off.
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

    def req_account_updates(self, subscribe, account_id):
        raise NotImplementedError

    def req_all_open_orders(self):
        raise NotImplementedError

    def req_executions(self, req_id, account_id):
        raise NotImplementedError

    def req_mkt_data(self, req_id, contract):
        raise NotImplementedError

    def cancel_mkt_data(self, req_id):
        raise NotImplementedError

    def req_contract_details(self, req_id, contract):
        raise NotImplementedError

    def can_place_orders(self):
        raise NotImplementedError

    def place_order(self, order_id, contract, order):
        raise NotImplementedError

    def connection_time(self):
        """The time IB Gateway gave as the session's connection time, as it gave it (TWS API
        EClient.twsConnectionTime), or None when the session has none: "YYYYMMDD HH:MM:SS Zone", the zone being
        the Gateway's own (loop.gateway_zone_problem reads it)."""
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
        "account_value",        # (key, value, currency, account)
        "account_download_end",  # (account)
        "open_order",           # (order_id, contract, order, order_state)
        "open_order_end",       # ()
        "order_status",         # (order_id, status, filled, remaining, avg_fill_price, perm_id,
                                #  parent_id, last_fill_price, client_id, why_held, mkt_cap_price)
        "exec_details",         # (req_id, contract, execution)
        "exec_details_end",     # (req_id)
        "commission_report",    # (report)
        "tick_price",           # (req_id, tick_type, price, attrib)
        "tick_snapshot_end",    # (req_id)
        "contract_details",     # (req_id, contract_details): ibapi's ContractDetails, its contract inside
        "contract_details_end",  # (req_id)
    )

    def __init__(self):
        self._queue = queue.Queue()

    def note(self, name, *args):
        """Queue the adapter's own note name (with args) behind every callback already received.

        Not a callback: Connection.ask_all_open_orders and
        Connection.ask_account_values note a request here just before it is
        sent, so whatever is queued ahead of the note arrived before the
        request left and cannot be its answer.
        """
        self._queue.put(Event(name, args))

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
