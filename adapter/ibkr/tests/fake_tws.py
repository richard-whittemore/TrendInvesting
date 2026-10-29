"""A scripted stand-in for IB Gateway behind the adapter's TWSApi interface.

It answers each request with synthetic callbacks in the shapes ibapi delivers
them (attribute objects for contracts, orders and executions), so the adapter
code under test is exactly the code that runs against the real client. Every
figure here is invented; none is a real price or account.
"""
import sys
from collections import deque
from datetime import datetime
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

adapter_dir = str(Path(__file__).resolve().parents[1])
if adapter_dir not in sys.path:
    sys.path.insert(0, adapter_dir)

from connection import ConnectFailed
from orders import NEW_YORK
from tws import TWSApi

ACCOUNT = "DU0000001"
OTHER_ACCOUNT = "DU0000002"
# IBKR's statuses of an order that has ended.
ENDED = ("Cancelled", "ApiCancelled", "Inactive")
SERVER_TIME = 1_800_000_000
# tws_connection_time's default: the fake's server time, on New York's clock.
GATEWAY_CLOCK = object()
# IBKR's refusal of an account summary while two are still counted as active
# ("Message Codes": 322, error processing request).
SUMMARY_LIMIT_CODE = 322
SUMMARY_LIMIT_MESSAGE = ("Error processing request.-'bW' : cause - Maximum number of account summary requests "
                         "exceeded; desubscribe to previous request first")
# The pseudo-currency label a per-currency cash row (CashBalance,
# TotalCashBalance) carries for its total in the account's base currency.
BASE = "BASE"


def contract(con_id=756733, symbol="SPY", sec_type="STK", currency="USD", exchange="SMART"):
    return SimpleNamespace(conId=con_id, symbol=symbol, secType=sec_type,
                           currency=currency, exchange=exchange, primaryExchange="ARCA")


def contract_details(symbol, con_id, primary_exchange="NASDAQ", currency="USD", sec_type="STK", exchange="SMART"):
    """One contractDetails answer as ibapi 10.51.01's decoder hands it to EWrapper (testdata/ibapi_shapes.json): a
    ContractDetails whose contract carries conId (int), symbol, secType, exchange (SMART, as asked), primaryExchange,
    currency, localSymbol and tradingClass (str), beside marketName, longName, stockType, validExchanges (str) and
    minTick (float). Synthetic values."""
    return SimpleNamespace(
        contract=SimpleNamespace(conId=con_id, symbol=symbol, secType=sec_type, exchange=exchange,
                                 primaryExchange=primary_exchange, currency=currency, localSymbol=symbol,
                                 tradingClass="NMS"),
        marketName="NMS", minTick=0.01, longName="SYNTHETIC " + symbol, stockType="COMMON",
        validExchanges="SMART,{},ARCA".format(primary_exchange))


def order(order_ref="tag-1", account=ACCOUNT, action="BUY", total_quantity="10",
          order_type="MKT", lmt_price=None, aux_price=None, tif="DAY", perm_id=9001,
          client_id=11):
    return SimpleNamespace(orderRef=order_ref, account=account, action=action,
                           totalQuantity=total_quantity, orderType=order_type,
                           lmtPrice=lmt_price, auxPrice=aux_price, tif=tif, permId=perm_id,
                           clientId=client_id)


def execution(exec_id="0001f4e8.6512a1b2.01.01", order_id=7, perm_id=9001, order_ref="tag-1",
              account=ACCOUNT, side="BOT", shares="4", price=101.25, cum_qty="4",
              avg_price=101.25, time="20261016 09:30:01 US/Eastern", client_id=11):
    return SimpleNamespace(execId=exec_id, orderId=order_id, permId=perm_id, orderRef=order_ref,
                           clientId=client_id,
                           acctNumber=account, side=side, shares=shares, price=price,
                           cumQty=cum_qty, avgPrice=avg_price, time=time)


class FakeTWS(TWSApi):
    """Records every call and answers from a script.

    connect_behaviours is consumed one entry per connect attempt:
    "ok", "client-id-in-use", "refused", "silent" (no answer at all),
    "hang" (the socket work overran its timeout, as IbapiClient reports it)
    or "socket-error". connect_hook, if set, runs first, so a test can let
    the clock move while the socket is being opened.

    place_order records each order in placed and hands it to on_place, if
    set, which answers as IB Gateway would (accept, reject below). An
    order it accepts stays among open_orders, so a later read of IBKR's
    open orders shows it, across a reconnect, until it is filled.
    """

    def __init__(self):
        self.calls = []
        self.callbacks = None
        self.connected = False
        self.connect_behaviours = []
        self.connect_hook = None
        self.connect_timeouts = []
        self.next_order_id = 7
        self.server_time = SERVER_TIME
        # IB Gateway's connection time for each session (TWS API twsConnectionTime), in the Gateway's zone:
        # GATEWAY_CLOCK gives the fake's server time on New York's clock, as a Gateway set right does.
        self.tws_connection_time = GATEWAY_CLOCK
        self.answer_current_time = True
        self.positions = []
        self.summary = []
        self.end_positions = True
        self.end_summary = True
        self.summary_error = None
        # The message IBKR sends with summary_error.
        self.summary_error_message = "synthetic summary failure"
        # IBKR allows two account-summary subscriptions per connection
        # (TWS API, reqAccountSummary), and on 2026-10-25 IB Gateway 10.50
        # never freed one when it was cancelled: exactly two summaries were
        # answered on a connection and every later one was refused with
        # error 322, for minutes. So here too: summary_slots summaries a
        # connection, a cancel frees none, a new connection frees them all.
        self.summary_slots = 2
        self._summaries_held = set()
        # The account values (reqAccountUpdates) are answered from summary's
        # rows, each as updateAccountValue, then accountDownloadEnd (with
        # end_summary). With ledger_rows, a TotalCashValue in US dollars
        # also brings the rows IBKR's key list and its own sample client
        # (apidemo AccountInfoPanel) show beside it: the security segment's
        # TotalCashValue-S, and the per-currency cash rows CashBalance and
        # TotalCashBalance in US dollars and as the "BASE" total. IBKR's
        # documentation gives TotalCashValue itself no "BASE" row.
        # summary_error, drop_during_summary and refuse_next_summary act on
        # that download as on a summary; an error carries IBKR's request ID
        # -1, as an account-updates error does.
        self.ledger_rows = True
        # Callbacks IBKR has already sent but the adapter has not yet
        # received (in flight): each lands at the adapter's next wait
        # (waited) or just ahead of the answer to its next subscription,
        # whichever comes first.
        self.in_flight = []
        # Whether an unsubscription is answered with IBKR's 2100 ("API client
        # has been unsubscribed from account data"), in flight as above.
        self.unsubscribe_notice = False
        # A tag the next account-values download alone leaves out (it still
        # ends), arming after_refusal for the adapter's next wait as
        # refuse_next_summary does.
        self.omit_next = None
        # A code the next account read alone is refused with, and what
        # happens at the adapter's next wait after that refusal (run by
        # waited(), which a test's sleep calls).
        self.refuse_next_summary = None
        self.after_refusal = None
        self._at_next_wait = None
        self.drop_during_summary = False
        self.open_orders = []
        self.end_open_orders = True
        # reqAllOpenOrders answered in batches of this many orders, each
        # batch and the list's end arriving only as the adapter next waits
        # for a callback, so the list ends behind the answers to the
        # requests asked beside it (IBKR answers each request in its own
        # time); None answers the whole list at once.
        self.open_order_batch = None
        # As a session starts, Gateway sends this client's open orders and
        # an end-of-list marker that nothing asked for, after nextValidId,
        # one row (and then the end) each time the adapter next waits, so
        # they can still be arriving after connect has returned.
        self.volunteer_open_orders = False
        # Whether the volunteered list ends with its end-of-list marker.
        self.volunteer_end = True
        # The volunteered rows and end arrive only behind the adapter's
        # first reqAllOpenOrders, ahead of its answer: the ordering in which
        # an end nothing asked for lands inside an asked-for list.
        self.volunteer_after_request = False
        # Each reqAllOpenOrders answered with the next of these lists of
        # rows, then with open_orders once they are used up: IBKR's list
        # changing between reads.
        self.open_order_answers = []
        # While set, reqAllOpenOrders' answers are held back, to arrive only
        # once release_held() is called (an answer delayed past its read).
        self.hold_open_orders = False
        self._held = []
        # Answers still to come, one arriving each time the adapter waits
        # with nothing else left to take.
        self._later = deque()
        # Called before each answer still to come is delivered, so a test
        # can let the clock move between them.
        self.later_hook = None
        self.executions = []
        # (code, message) IBKR answers reqExecutions with under its request
        # ID, before the end of the list; None for none.
        self.executions_error = None
        # IBKR's last orderStatus for each order and its commission report
        # for each execution, which it replays on a later read.
        self.order_statuses = {}
        self.commissions = {}
        self.client_id = None
        self.placed = []
        self.on_place = None
        self.place_error = None
        self.can_place = True
        self.managed_accounts = (ACCOUNT,)
        # IBKR's market-data snapshots: the price for each conId, sent as
        # mark_tick (LAST) and ended; an (code, message) error for a conId in
        # mark_errors instead; nothing at all with answer_marks false; the
        # link dropped as the snapshot is asked for with drop_on_mark;
        # mark_ticks gives a conId's (tick type, price) ticks in place of
        # its one mark, and end_marks false leaves every snapshot unended;
        # mark_end_delay ends each snapshot that many seconds after its
        # price instead, as IBKR does at the end of its 11-second snapshot
        # window (late_end passes the time).
        self.marks = {}
        self.mark_tick = 4
        self.mark_ticks = {}
        self.end_marks = True
        self.mark_errors = {}
        self.answer_marks = True
        self.drop_on_mark = False
        self.mark_end_delay = None
        self._late_ends = []
        # IBKR's contract details for each symbol asked for by name
        # (reqContractDetails): each answer in contract_details[symbol] is
        # sent as contract_details under the request's ID, then
        # contract_details_end (unless end_contract_details is false); a
        # symbol in contract_detail_errors gets IBKR's (code, message) error
        # under the request's ID instead, as IBKR answers 200 for a name it
        # does not know; drop_on_contract_details drops the link as the
        # request is asked.
        self.contract_details = {}
        self.contract_detail_errors = {}
        self.end_contract_details = True
        self.drop_on_contract_details = False
        # Symbols whose first lookup IBKR answers but never ends (a slow
        # Gateway); a second lookup is answered and ended as usual.
        self.unended_contract_details_once = set()
        # The server version the session negotiates (EClient.serverVersion);
        # 205 and later read contract details from IBKR's protobuf messages.
        self.negotiated_server_version = 205

    # -- connection ------------------------------------------------------
    def connect(self, host, port, client_id, callbacks, timeout):
        self.calls.append(("connect", host, port, client_id))
        self.client_id = client_id
        self.connect_timeouts.append(timeout)
        self.callbacks = callbacks
        if self.connect_hook is not None:
            self.connect_hook()
        behaviour = self.connect_behaviours.pop(0) if self.connect_behaviours else "ok"
        if behaviour == "hang":
            raise ConnectFailed("socket connect did not finish within {}s".format(timeout))
        if behaviour == "socket-error":
            raise ConnectionRefusedError("synthetic connection refused")
        # An abandoned session's answers never reach the new one, and its
        # account-summary subscriptions end with it.
        self._summaries_held = set()
        self._later.clear()
        self._held = []
        waiting = callbacks.get

        # One answer still to come arrives each time the adapter waits on
        # an empty Inbox: what has already arrived is taken first.
        def get(timeout):
            if self._later and callbacks._queue.empty():
                if self.later_hook is not None:
                    self.later_hook()
                self._later.popleft()()
            return waiting(timeout)
        callbacks.get = get
        if behaviour == "ok":
            self.connected = True
            # One connection time per session, as IB Gateway gives it: the server time at its handshake.
            self._session_started = self.server_time
            # IBKR names the login's accounts as the session starts.
            callbacks.managed_accounts(",".join(self.managed_accounts))
            callbacks.next_valid_id(self.next_order_id)
            if self.volunteer_open_orders and not self.volunteer_after_request:
                self._volunteer(callbacks, self._later.append)
        elif behaviour == "client-id-in-use":
            callbacks.error(-1, 326, "Unable to connect as the client id is already in use. "
                                     "Retry with a unique client id.")
            callbacks.connection_closed()
        elif behaviour == "refused":
            callbacks.error(-1, 502, "Couldn't connect to TWS. Confirm that API is enabled.")
        elif behaviour != "silent":
            raise AssertionError("unknown behaviour " + behaviour)

    def disconnect(self):
        self.calls.append(("disconnect",))
        self.connected = False

    def is_connected(self):
        return self.connected

    def drop(self):
        """The socket closes underneath the adapter, as a Gateway restart does."""
        self.connected = False
        self.callbacks.connection_closed()

    # -- account ---------------------------------------------------------
    def req_current_time(self):
        self.calls.append(("req_current_time",))
        if self.answer_current_time:
            self.callbacks.current_time(self.server_time)
            self.server_time += 1

    def req_positions(self):
        self.calls.append(("req_positions",))
        for account, con, pos, avg in self.positions:
            self.callbacks.position(account, con, pos, avg)
        if self.end_positions:
            self.callbacks.position_end()

    def cancel_positions(self):
        self.calls.append(("cancel_positions",))

    def req_account_summary(self, req_id, group, tags):
        self.calls.append(("req_account_summary", req_id, group, tags))
        if len(self._summaries_held) >= self.summary_slots:
            self.callbacks.error(req_id, SUMMARY_LIMIT_CODE, SUMMARY_LIMIT_MESSAGE)
            return
        self._summaries_held.add(req_id)
        if self.refuse_next_summary is not None:
            code, self.refuse_next_summary = self.refuse_next_summary, None
            self._at_next_wait, self.after_refusal = self.after_refusal, None
            self.callbacks.error(req_id, code, "synthetic summary refusal")
            return
        if self.summary_error is not None:
            self.callbacks.error(req_id, self.summary_error, self.summary_error_message)
            return
        for i, (account, tag, value, currency) in enumerate(self.summary):
            if self.drop_during_summary and i == 1:
                self.drop()
                return
            self.callbacks.account_summary(req_id, account, tag, value, currency)
        if self.end_summary:
            self.callbacks.account_summary_end(req_id)

    def cancel_account_summary(self, req_id):
        # Recorded, but the subscription stays counted (see summary_slots).
        self.calls.append(("cancel_account_summary", req_id))

    def req_account_updates(self, subscribe, account_id):
        self.calls.append(("req_account_updates", subscribe, account_id))
        if not subscribe:
            if self.unsubscribe_notice:
                self.in_flight.append(lambda: self.callbacks.error(
                    -1, 2100, "API client has been unsubscribed from account data."))
            return
        self.land_in_flight()
        if self.refuse_next_summary is not None:
            code, self.refuse_next_summary = self.refuse_next_summary, None
            self._at_next_wait, self.after_refusal = self.after_refusal, None
            self.callbacks.error(-1, code, "synthetic account-values refusal")
            return
        if self.summary_error is not None:
            self.callbacks.error(-1, self.summary_error, self.summary_error_message)
            return
        omit, self.omit_next = self.omit_next, None
        if omit is not None:
            self._at_next_wait, self.after_refusal = self.after_refusal, None
        rows = []
        for account, tag, value, currency in self.summary:
            if tag == omit:
                continue
            rows.append((tag, value, currency, account))
            if self.ledger_rows and tag == "TotalCashValue" and currency == "USD":
                rows.extend([("TotalCashValue-S", value, currency, account),
                             ("CashBalance", value, currency, account), ("CashBalance", value, BASE, account),
                             ("TotalCashBalance", value, currency, account),
                             ("TotalCashBalance", value, BASE, account)])
        for i, row in enumerate(rows):
            if self.drop_during_summary and i == 1:
                self.drop()
                return
            self.callbacks.account_value(*row)
        if self.end_summary:
            self.callbacks.account_download_end(account_id)

    def waited(self):
        """The adapter waited (its sleep): land what was in flight and run what refuse_next_summary's refusal left."""
        self.land_in_flight()
        act, self._at_next_wait = self._at_next_wait, None
        if act is not None:
            act()

    def land_in_flight(self):
        """Deliver every callback in flight (in_flight), in the order IBKR sent them."""
        landing, self.in_flight = self.in_flight, []
        for send in landing:
            send()

    # -- orders ----------------------------------------------------------
    def req_all_open_orders(self):
        self.calls.append(("req_all_open_orders",))
        callbacks = self.callbacks
        rows = list(self.open_order_answers.pop(0) if self.open_order_answers else self.open_orders)
        if self.volunteer_open_orders and self.volunteer_after_request:
            self.volunteer_after_request = False
            self._volunteer(callbacks, lambda send: send())
        answer = []
        if self.open_order_batch is None:
            answer.append(lambda: self._send_open_orders(callbacks, rows))
        else:
            for i in range(0, len(rows), self.open_order_batch):
                batch = rows[i:i + self.open_order_batch]
                answer.append(lambda batch=batch: self._send_open_orders(callbacks, batch))
        if self.end_open_orders:
            answer.append(callbacks.open_order_end)
        if self.hold_open_orders:
            self._held.extend(answer)
        elif self.open_order_batch is None:
            for send in answer:
                send()
        else:
            self._later.extend(answer)

    def release_held(self):
        """The held answers arrive late, one each time the adapter next waits."""
        self._later.extend(self._held)
        self._held = []

    def _volunteer(self, callbacks, deliver):
        """Gateway's unasked list: this client's open orders, one at a time, then (if volunteer_end) its end."""
        for row in self.open_orders:
            if getattr(row[2], "clientId", None) == self.client_id:
                deliver(lambda row=row: self._send_open_orders(callbacks, [row]))
        if self.volunteer_end:
            deliver(callbacks.open_order_end)

    def _send_open_orders(self, callbacks, rows):
        for order_id, con, ord_, state in rows:
            callbacks.open_order(order_id, con, ord_, state)
            # IBKR follows each open order with its status.
            if order_id in self.order_statuses:
                callbacks.order_status(*self.order_statuses[order_id])

    def req_executions(self, req_id, account_id):
        self.calls.append(("req_executions", req_id, account_id))
        if self.executions_error is not None:
            self.callbacks.error(req_id, *self.executions_error)
        for con, ex in self.executions:
            self.callbacks.exec_details(req_id, con, ex)
            # IBKR follows each execution with its commission report.
            if ex.execId in self.commissions:
                self.callbacks.commission_report(self.commissions[ex.execId])
        self.callbacks.exec_details_end(req_id)

    # -- market data -----------------------------------------------------
    def req_mkt_data(self, req_id, contract_):
        self.calls.append(("req_mkt_data", req_id, contract_))
        if self.drop_on_mark:
            self.drop()
            return
        if not self.answer_marks:
            return
        if contract_.conId in self.mark_errors:
            self.callbacks.error(req_id, *self.mark_errors[contract_.conId])
            return
        if contract_.conId in self.mark_ticks:
            for tick, price in self.mark_ticks[contract_.conId]:
                self.callbacks.tick_price(req_id, tick, price, SimpleNamespace())
        elif contract_.conId in self.marks:
            self.callbacks.tick_price(req_id, self.mark_tick, self.marks[contract_.conId], SimpleNamespace())
        if self.mark_end_delay is not None:
            self._late_ends.append(req_id)
        elif self.end_marks:
            self.callbacks.tick_snapshot_end(req_id)

    def server_version(self):
        """The negotiated server version, as EClient.serverVersion() gives it (an int; synthetic here)."""
        return self.negotiated_server_version if self.connected else None

    def req_contract_details(self, req_id, contract_):
        self.calls.append(("req_contract_details", req_id, contract_))
        if self.drop_on_contract_details:
            self.drop()
            return
        if contract_.symbol in self.contract_detail_errors:
            self.callbacks.error(req_id, *self.contract_detail_errors[contract_.symbol])
            return
        for details in self.contract_details.get(contract_.symbol, []):
            self.callbacks.contract_details(req_id, details)
        if contract_.symbol in self.unended_contract_details_once:
            self.unended_contract_details_once.discard(contract_.symbol)
            return
        if self.end_contract_details:
            self.callbacks.contract_details_end(req_id)

    def late_end(self, clock):
        """A Connection subscriber: once a delayed snapshot's price is taken, clock passes mark_end_delay and IBKR ends it."""
        def handler(event):
            if event.name == "tick_price" and event.args[0] in self._late_ends:
                self._late_ends.remove(event.args[0])
                clock.now += self.mark_end_delay
                self.callbacks.tick_snapshot_end(event.args[0])
        return handler

    def cancel_mkt_data(self, req_id):
        self.calls.append(("cancel_mkt_data", req_id))

    def can_place_orders(self):
        return self.can_place

    def connection_time(self):
        """The session's connection time as IB Gateway gives it (tws_connection_time; by default the server time at the
        session's handshake, on New York's clock)."""
        if not self.connected:
            return None
        if self.tws_connection_time is GATEWAY_CLOCK:
            return datetime.fromtimestamp(self._session_started, NEW_YORK).strftime("%Y%m%d %H:%M:%S US/Eastern")
        return self.tws_connection_time

    def place_order(self, order_id, contract_, order_):
        self.calls.append(("place_order", order_id))
        if self.place_error is not None:
            raise self.place_error
        self.placed.append((order_id, contract_, order_))
        # IBKR's next valid ID on a later connect is past every ID used.
        self.next_order_id = max(self.next_order_id, order_id + 1)
        if self.on_place is not None:
            self.on_place(order_id, contract_, order_)

    # -- IB Gateway's answers to a placed order --------------------------
    def echoed(self, order_id, order_):
        """The order as IBKR echoes it in openOrder: the request plus IBKR's permId and the client ID."""
        fields = dict(vars(order_))
        fields.update(permId=9000 + order_id, clientId=self.client_id, auxPrice=None)
        return SimpleNamespace(**fields)

    def accept(self, order_id, contract_, order_, status="Submitted"):
        """IBKR takes the order and it works: openOrder, then orderStatus."""
        echo = self.echoed(order_id, order_)
        self.open_orders = [o for o in self.open_orders if o[0] != order_id]
        self.open_orders.append((order_id, contract_, echo, SimpleNamespace(status=status)))
        self.callbacks.open_order(order_id, contract_, echo, SimpleNamespace(status=status))
        self.status(order_id, status, filled="0", remaining=str(echo.totalQuantity))

    def reject(self, order_id, contract_, order_, message="Order rejected - reason: synthetic"):
        """IBKR refuses the order: code 201, then Inactive. It never becomes an open order."""
        self.callbacks.error(order_id, 201, message)
        self.status(order_id, "Inactive", filled="0", remaining=str(order_.totalQuantity))

    def status(self, order_id, status, filled, remaining, avg_fill_price=0.0, last_fill_price=0.0):
        args = (order_id, status, filled, remaining, avg_fill_price, 9000 + order_id, 0, last_fill_price,
                self.client_id, "", 0.0)
        self.order_statuses[order_id] = args
        self.callbacks.order_status(*args)

    def fill(self, order_id, shares, price, exec_id, cum_qty, commission="1.00",
             time="20261016 09:30:01 US/Eastern", currency="USD"):
        """One execution of an accepted order: execDetails, its commission report, then orderStatus.

        The order leaves open_orders once cum_qty reaches its quantity, and
        the execution stays among executions, as IBKR's lists show it.
        commission None sends no commission report, as when IBKR's is lost.
        A second execution under an execId with the same base and another
        last segment is IBKR's correction of the first.
        """
        entry = next(o for o in self.open_orders if o[0] == order_id)
        con, echo = entry[1], entry[2]
        ex = execution(exec_id=exec_id, order_id=order_id, perm_id=echo.permId, order_ref=echo.orderRef,
                       account=echo.account, side="BOT" if echo.action == "BUY" else "SLD", shares=shares,
                       price=price, cum_qty=cum_qty, avg_price=price, time=time, client_id=self.client_id)
        self.executions.append((con, ex))
        self.callbacks.exec_details(-1, con, ex)
        if commission is not None:
            report = SimpleNamespace(execId=exec_id, commission=commission, currency=currency)
            self.commissions[exec_id] = report
            self.callbacks.commission_report(report)
        remaining = str(Decimal(str(echo.totalQuantity)) - Decimal(cum_qty))
        done = Decimal(remaining) == 0
        if done:
            self.open_orders = [o for o in self.open_orders if o[0] != order_id]
        # An order IBKR already reported ended keeps that status: its
        # orderStatus after a late execution repeats it, never Submitted.
        last = self.order_statuses.get(order_id)
        ended = last is not None and last[1] in ENDED
        self.status(order_id, last[1] if ended else "Filled" if done else "Submitted", filled=cum_qty,
                    remaining=remaining, avg_fill_price=price, last_fill_price=price)

    def called(self, name):
        return [c for c in self.calls if c[0] == name]
