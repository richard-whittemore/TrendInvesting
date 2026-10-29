"""The IBKR adapter's main loop: one engine's link to IB Gateway, kept up, reconciled, and drained into the engine.

ADR 0025 points 2 and 5, ADR 0027 section 4. At start the loop connects
with the engine's own client ID and, before any order can be sent, reads
IBKR's open orders and the session's executions
(orders.collect_settled_broker_orders: read again until two reads in a row
show the same open orders) and then the account (snapshot.read_account:
positions, balances and open orders, which must show the same working
orders, else both are read again), refusing to go on if any of it could
not be read (ReconcileIncomplete; an unavailable balance is not zero, ADR
0019). A link lost during those reads is connected again and reconciled,
never a stop.
It then matches IBKR's open orders and executions against the orders the
engine's durable log released to be sent, and the logs of the earlier runs
its Run approvals chain back to (relay.run_logs), since IBKR's day lists
still show an earlier run's orders after a fresh run starts
(orders.match_on_reconnect, by client order ID under this engine's client
ID): an open order or execution in the account that none of them explains
(an order placed by hand or by another client, or one no run of this
engine released) stops the adapter for a person
(ReconcileMismatch) before anything is sent. A released order IBKR no
longer shows is not a refusal here: a DAY order ended or filled on an
earlier day leaves nothing in today's lists, and the relay checks IBKR for
the one order it is about to send. Positions and balances are read in full
but not matched here: that is the engine's reconciliation (ADR 0019).
Only then is the link marked reconciled (Connection.mark_reconciled, which
refuses an account the login does not manage). It does the same after
every reconnect or restoration. It then settles the input left awaiting
its reply by a stop, if any, before anything new: only an input of the
run now running is asked for again, and one an earlier run left stops the
adapter for a person (relay.EarlierRunInput) before anything is sent.

Each step keeps the link up (Connection.maintain), takes one callback from
IB Gateway (Connection.pump), checks for executions whose commission report
is overdue (OrderReportJournal.check_overdue: the invariant reports.py
states, that the check runs after each pump, so a missing commission stops
the adapter on time and not only when IBKR next writes), and, while the
link is up and reconciled, sends the engine every input recorded since,
oldest first, through the relay, which hands each released order marked
send to the sender once (relay.Relay). While the engine decides, the
exchange runs on a thread of its own and this thread keeps reading IB
Gateway's callbacks and checking for overdue commissions, so a slow reply
neither leaves the heartbeat's answer unread (which would drop a healthy
link) nor lets an arrived commission count as missing.

Inputs are recorded in the outbox (OUTBOX_SUFFIX beside the pending file)
before they are sent, written durably, so a report IBKR will not repeat (a
cancellation of an order no longer open) survives a stop. An outbox entry
carries its input ID from the moment it is recorded; its sequence and the
run's identity are taken from the engine's durable log when it is sent
(relay.read_engine_log). An entry is struck off once its reply has been
acted on and before the input awaiting its reply is cleared (Relay's
settled), so a stop between the two leaves that input awaiting its reply,
which only its own run is asked for again: no entry an earlier run
answered is ever left for a fresh run to take. A stop before the strike
sends nothing twice: an entry the log already holds as a committed input,
with the same type, schema version and payload, is struck off unsent, and
one whose ID the log holds with any other content stops the adapter
(OutboxInvalid). The records already committed, in the outbox, or awaiting
their reply seed the report journal in the order they were made (each
run of the chain oldest first with the settlements of the runs it ended,
then the input awaiting its reply, then the outbox), never by their
times, so IBKR's replay after a restart records nothing twice and each
order keeps its latest status; a report whose event time cannot be read
stops the start (ReportRecordInvalid).

With a streams folder (delivery.py), each step while the link is up and
reconciled also delivers the run's market-data stream: the files of
<streams>/<run> it has not delivered, oldest session first, behind one
account reading (_post_close_reading), all recorded in the outbox in one
write, so the account snapshot is journalled before the bars, the mark
before the close and each session's bars before its close (ADR 0019's
snapshot rule; ADR 0021). The owner's decision of 2026-10-18 (ADR 0019,
ADR 0025 point 5, ADR 0026 point 1, amended): the reading is the account
as of the batch's last close when it is taken after that close on its own
New York date, or (the owner's decision of 2026-10-27, ADR 0019 amended
that day) on a later date before the next open with the Flex reader's
executions proof covering the close to the midnight IBKR's own list starts
at and no fill journalled since the close (_morning_proof; until
PROOF_WAIT_UNTIL the batch waits for that proof, _awaits_proof), with no open order for the account at IBKR (in either
account read or the last open-order list), no execution in it at or since
the close, every holding valued at an official close of the batch, IBKR's
own net liquidation positive, and its holdings and cash those of the
previous close adjusted by the fills since, the cash within
CASH_DRIFT_BOUND (_not_as_of_close). The previous close is the latest
account snapshot of this run or of the runs its Run approvals chain back to,
or the latest starting book an approval states, an account's first run's
included (book_basis): a fresh run's first batch is never adopted
unchecked. Both snapshots are then stamped as of the close and valued at it
(cash plus each holding times its official close), IBKR's own net
liquidation, the cash drift and the unexplained cash (that drift added up
since the last cash explained) recorded beside it. Otherwise both are
stamped at the read time, with the reason, and are IBKR's own reading, so
the engine withholds the session; when the engine holds no book and the
reading differs from the earlier run's close or the starting book, or has
neither to compare with, its reconciliation goes alone, its snapshots
withheld, so the engine, with no book, classifies it unverifiable and halts
(ADR 0019).
A stream entry is sent as the data job
wrote it, with the engine's next sequence (the file's own is a file-local
ordinal) and its send time as its recorded time, as every input the
adapter sends. An earlier run's batch left in the outbox, its adapter
entries and its stream entries, is marked dropped, so that run gets its
files again if it is resumed (delivery.py), and then struck off unsent in
one outbox write, since a run's own stream carries its whole history from
its warm-up: a stop leaves the batch whole or wholly gone, never its stream
entries without their snapshots.

Once the engine holds an account snapshot (its book), every reconcile
(each start and reconnect) is followed by an account.reconciliation of
IBKR's positions and cash, for the engine to classify against that book
(ADR 0025 point 5, ADR 0019), read once every execution IBKR replayed has
its commission and its fill is recorded, and sent before any stream entry
(_send_reconciliation), read again at most SNAPSHOT_ATTEMPTS times in a row
while its positions or cash cannot be read, then stopping the adapter for a
person (ReconcileIncomplete); every batch carries one too, from its own
reading, ahead of its snapshots once the engine holds a book (delivery.py);
before it, a run's first batch carries one behind them, or alone when the
reading is unverified (above).

The account reading is one consistent reading (ADR 0019: its components
share one cutoff): every callback already received is applied first, so a
report IBKR sent before the read is recorded ahead of it; it is not taken
while an execution awaits its commission (its fill is not yet recorded);
the account is read, then IBKR's last prices (beside each mark, the price band's comparison),
then the account again, then IBKR's open orders and executions, and the
snapshots are the second read. It is read again, up to
BROKER_SNAPSHOT_ATTEMPTS times (after a reading with a component unread,
only once SNAPSHOT_RETRY_WAITS has passed), if an order report is recorded or an execution arrives (of any
client's order in the account, not only this adapter's, each counted once
by its execution ID) between the start of the first read and the end of
the last list, if the two reads' holdings,
open orders or cash differ (a trade IBKR reports to another client only,
such as one placed in its own app), or if any component could not be read.
A reading still incomplete then stops the adapter for a person
(delivery.SnapshotRefused); one still overtaken waits for the next step.
IBKR's positions and open orders carry no request ID, so a read cut off
before either ended (it timed out, or Gateway lost IBKR's servers, which it
restores on the same socket) is never followed by another on that link,
where the late end of the abandoned list would complete the new one: the
link is dropped (Connection.drop; after Gateway's restoration or
restore_timeout if it had lost IBKR's servers), and the snapshot waits
until it is back and reconciled. The order due is held the same way when
the link went part-way through the relay's open-order read; a read that
times out with the link up stops the adapter (BrokerOrdersIncomplete).
Each snapshot's cutoff is logged: the last report recorded before it.

The outbox goes oldest first, but a broker report recorded behind a stream
entry (a fill or a cancellation IBKR reported while the engine decided a
bar) is sent before that stream entry, so no decision runs on a state
behind a report already received. While an execution awaits its
commission, no stream entry is sent at all: its fill is not recorded yet,
and goes ahead of the stream once its commission arrives; one that never
arrives stops the adapter for a person after commission_wait
(OrderReportJournal.check_overdue).

The engine must be running, with its durable log begun, before the adapter
starts: the log is the run's identity. The loop connects to the engine's
socket before it reads that log or IBKR (EngineUnavailable otherwise): the
engine opens its socket only after it has recorded its start, a fresh run's
Run approval included, so the chain of runs is never read without it. The
chain stops at the first earlier run whose inputs all predate yesterday's
session and whose released orders have all ended, at IBKR or in the
adapter's own records (relay.run_logs, PendingInput.settled_orders), so an
older run's log is never needed. Nothing here sends when the
configuration is not paper (sender.make_sender), and the real client sends
only with paper_sending_enabled on (real_client.IbapiClient.can_place_orders).
"""
import hashlib
import json
import logging
import os
import threading
import time
import uuid
from datetime import datetime, time as clock_time, timedelta, timezone
from decimal import Decimal

from connection import Connection, LinkChanged, NotConnected
from delivery import (DELIVERED_SUFFIX, SESSION_OPEN, Deliverer, PostCloseReading, SnapshotRefused, market_open,
                      nyse_calendar, run_name)
from orders import (EASTERN_LABELS, NEW_YORK, NEW_YORK_ZONES, OPEN_ORDER_DISAGREEMENTS, BrokerOrdersIncomplete,
                    OpenOrdersUnsettled, OrderTracker, agrees_with_account, collect_broker_orders,
                    collect_settled_broker_orders, gateway_clock_trusted, match_on_reconnect, new_york_instants,
                    order_key, unverified_receipt)
from relay import (LOG_INPUT, OUTBOX_SUFFIX, EarlierRunInput, Outbox, OutboxInvalid, PendingInput,
                   Relay, ResolveRefused, earlier_run, read_engine_log, released_client_order_ids, run_chain, run_identity,
                   run_logs)
from reports import FILL_TYPE, LIFECYCLE_TYPE, OrderReportJournal
from sender import NotReconciled, make_sender, release_to_request
from account import NET_LIQUIDATION, SETTLED_CASH, TOTAL_CASH
from book_basis import cash_change, fills_since, holdings_after, previous_close
from book_basis import cents as _cents
import flex
from snapshot import (ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE, CASH, NET, POSITIONS, RECONCILIATION_TYPE, Record,
                      account_snapshot_record, broker_snapshot_record, close_valuation, mark_record, read_account,
                      read_marks, reconciliation_record, resolve_contracts)

log = logging.getLogger("ibkr")

# The adapter's name in every input it sends (event.AdapterIBKR; ADR 0025
# point 3).
SOURCE = "ibkr"
ENVELOPE_VERSION = 1
# The adapter's files beside its pending file (relay.PendingInput's own
# suffixes are .handover, .refused, .not-handed-over, .run, .run-ended,
# .corrections, .late-fills and .lock, and relay.OUTBOX_SUFFIX the outbox's).
HELD_SUFFIX = ".held"
# The bases the runs before the one now running give its readings, kept once
# per run (book_basis).
BASIS_SUFFIX = ".basis"
# The input types whose event time is in their payload.
EVENT_TIME_FIELD = {LIFECYCLE_TYPE: "occurred_at", FILL_TYPE: "filled_at"}
# How long, in seconds, the loop waits on the engine's thread between reads
# of IB Gateway's callbacks while the engine decides.
ENGINE_WAIT_POLL = 0.05
# How many reads in a row of the reconciliation owed after a start or
# reconnect may leave its positions or cash unread before the adapter stops.
# Rule: ADR 0019 (an unavailable balance is not zero): a read left unread is
# never sent as zero; the adapter stops for a person instead.
SNAPSHOT_ATTEMPTS = 3
# The seconds one delivery waits before reading the account again for a
# broker snapshot after a reading left a component unread: a Gateway briefly
# without IBKR's servers, or an account-values download IBKR did not finish,
# passes in seconds, and three reads back to back stopped the adapter for
# good on 2026-10-25. So a broker snapshot is read at most
# BROKER_SNAPSHOT_ATTEMPTS times, waiting 50 s in all between them. Each
# attempt's reads come on top: up to two account reads (the second only
# after a complete first), each up to its request_timeout, and the marks up
# to mark_timeout, so no total is promised. The stop (SnapshotRefused)
# states the time the attempts actually took. Rule: ADR 0019 (an
# unavailable balance is not zero): a wait only postpones a read; a snapshot
# still incomplete after the last attempt is refused, never completed with zeros.
SNAPSHOT_RETRY_WAITS = (5.0, 15.0, 30.0)
BROKER_SNAPSHOT_ATTEMPTS = len(SNAPSHOT_RETRY_WAITS) + 1
# How many reconciles in a row the link may be dropped during (the open
# orders never quiet, a close) before the adapter stops for a person: a
# Gateway that keeps the socket up while its stream never pauses would
# otherwise drop, reconnect and reconcile without end, sending nothing and
# telling no one (fail closed and tell a person, AGENTS.md; ADR 0025 point 5).
RECONCILE_CUTOFFS = 3
# The source a mark names: the data job, whose delivered bar's official close
# it is (adapter/marketdata/kinds.py SOURCE).
MARK_SOURCE = "marketdata"
# The cash drift since the previous close a reading may show and still stand
# for this close, as a share of its net liquidation at the close: room for
# the postings IBKR makes with no order or execution (interest, a fee, a
# dividend: SPY's largest quarterly dividend on a holding at 147% of net
# liquidation exceeds 0.5%), recorded as the broker snapshot's cash_drift;
# beyond it the reading is stamped at its read time (ADR 0019, amended
# 2026-10-18; the engine checks the same bound, event.CashDriftBound).
CASH_DRIFT_BOUND = Decimal("0.01")
# Until this New York time on a later date than a batch's close, a batch whose
# executions proof is not yet written or does not yet cover waits for the Flex
# reader's next firing (05:30, 06:30 and 07:30; ti-flex@.timer) rather than
# have its session withheld; at or after it, it is withheld. Before the 09:30
# open, so a session decided is decided before the next one starts (ADR 0019,
# amended 2026-10-27).
PROOF_WAIT_UNTIL = clock_time(9, 0)
# IBKR's executions reach back only to midnight in IB Gateway's own zone (ADR 0025 point 5);
# ti-gateway.service sets TZ=America/New_York, and a connection or execution time in any zone but New York's
# (orders.NEW_YORK_ZONES, orders.EASTERN_LABELS) shows the list's midnight is not New York's, so a morning reading is
# withheld (ADR 0019, amended 2026-10-27). IBKR's Eastern label does not follow daylight saving time, so it says
# nothing of the season: the Gateway's clock is shown to be New York's by its connection time, read on New York's
# clock, falling within GATEWAY_CLOCK_TOLERANCE of the adapter's own clock (NTP-synced) when the link connected (ADR
# 0019, amended 2026-10-28). A Gateway on fixed EST in summer is an hour off; another zone labelled EST is hours off.
GATEWAY_CLOCK_TOLERANCE = timedelta(minutes=5)
_SET_NEW_YORK = ("set IB Gateway to New York time (ti-gateway.service, TZ=America/New_York, and its login's time "
                 "zone)")


def _new_york_readings(text, what):
    """(problem, readings): why text, a time IB Gateway gave as "YYYYMMDD HH:MM:SS Zone", is not a New York reading
    ("" when it is), and when it is, the instants (UTC) it can stand for on New York's clock: two in the hour repeated
    when daylight saving time ends, one otherwise. what names the text in the reason, which quotes it as given.

    IBKR documents the call, not the format, so it is read defensively (ADR 0019, amended 2026-10-27; AGENTS.md: fail
    closed): the zone, everything after the date and time, must be a New York zone name or an Eastern label, the label
    taken on New York's clock whatever its season (ADR 0019, amended 2026-10-28); a local time New York skipped when
    daylight saving time began, any other zone, or a text not of that shape, is a problem.
    """
    parts = text.split(None, 2) if isinstance(text, str) else []
    if len(parts) != 3:
        return ("{} {!r} is not 'YYYYMMDD HH:MM:SS Zone', so the zone its list of executions starts its midnight "
                "in is not known".format(what, text)), ()
    try:
        local = datetime.strptime(parts[0] + " " + parts[1], "%Y%m%d %H:%M:%S")
    except ValueError:
        return ("{} {!r} does not state a date and time that can be read, so the zone it is in is not "
                "trusted".format(what, text)), ()
    zone = parts[2].strip()
    if zone not in NEW_YORK_ZONES and zone not in EASTERN_LABELS:
        return ("{} {!r} is in the zone {!r}, not New York's: IBKR's executions reach back only to midnight in the "
                "Gateway's own zone, so the midnight its list starts at is not New York's; {}".format(
                    what, text, zone, _SET_NEW_YORK)), ()
    try:
        # A wall-clock time New York skipped (the hour the clocks go forward) is no New York reading at all; the
        # repeated autumn hour is, at either reading (orders.new_york_instants).
        return "", new_york_instants(local)
    except ValueError:
        return ("{} {!r} is a time that did not happen in New York (the clocks went forward over it), so it "
                "is not a New York reading".format(what, text)), ()
    except (OverflowError, OSError) as err:
        # Fail closed (AGENTS.md): a time the calendar cannot place is no New York reading.
        return ("{} {!r} cannot be placed in New York's calendar ({}), so it is not a New York "
                "reading".format(what, text, type(err).__name__)), ()


def gateway_zone_problem(text, what="IBKR's execution time"):
    """Why text, a time IB Gateway gave as "YYYYMMDD HH:MM:SS Zone", is not a New York reading; "" when it is.

    The zone check alone (_new_york_readings), for each execution's time: an execution may be hours old, so its
    time cannot be held against the adapter's clock; the connection time's clock check (gateway_clock_problem) is
    what shows the Gateway's clock, and so its executions' times, are New York's.
    """
    return _new_york_readings(text, what)[0]


def _span(delta):
    seconds = int(abs(delta).total_seconds())
    minutes, seconds = divmod(seconds, 60)
    return "{} minutes".format(minutes) + (" {} seconds".format(seconds) if seconds else "")


def _utc_offset(at):
    offset = at.astimezone(NEW_YORK).utcoffset()
    sign, offset = ("-", -offset) if offset < timedelta(0) else ("+", offset)
    hours, rest = divmod(int(offset.total_seconds()), 3600)
    return "UTC{}{:02d}:{:02d}".format(sign, hours, rest // 60)


def clock_change_problem(connected_at, now):
    """Why IB Gateway's clock, checked when the link connected at connected_at, is not known to be New York's at
    now: New York's clocks changed between them ("" when they did not). A Gateway on fixed EST matches New York's
    clock until the clocks go forward, so a link kept (or restored by Gateway, 1101/1102) with no new handshake across
    the change is trusted again only once it connects again (ADR 0019, amended 2026-10-28)."""
    if connected_at is None or now is None:
        return ""
    before, after = connected_at.astimezone(NEW_YORK).utcoffset(), now.astimezone(NEW_YORK).utcoffset()
    if before == after:
        return ""
    return ("New York's clocks changed ({} to {}) since IB Gateway's clock was checked when the link connected ({}), "
            "so it is not known to be on New York time until the link connects again".format(
                _utc_offset(connected_at), _utc_offset(now),
                connected_at.astimezone(NEW_YORK).strftime("%Y-%m-%d %H:%M:%S %Z")))


def gateway_clock_problem(text, connected_at, now=None, what="IB Gateway's connection time"):
    """Why text, the session's connection time IB Gateway gave at the handshake (TWS API EClient.twsConnectionTime,
    "YYYYMMDD HH:MM:SS Zone" in the Gateway's own zone), does not show its clock on New York time; "" when it does.

    It must be a New York reading (_new_york_readings), and one of the instants it can stand for must be within
    GATEWAY_CLOCK_TOLERANCE of connected_at, the adapter's own clock when the link connected (Connection.connected_at;
    ADR 0019, amended 2026-10-28): that one is taken, so the clock settles the hour repeated when daylight saving
    time ends, and with none so near, or no connected_at, it is a problem quoting the text as given. With now, the
    check holds only while New York is at the UTC offset it was at connected_at (clock_change_problem).
    """
    problem, readings = _new_york_readings(text, what)
    if problem:
        return problem
    if connected_at is None:
        return ("{} {!r} cannot be compared with the adapter's clock at the handshake (none was recorded), so the "
                "Gateway's clock is not known to be on New York time".format(what, text))
    off = min((at - connected_at for at in readings), key=abs)
    if abs(off) <= GATEWAY_CLOCK_TOLERANCE:
        return clock_change_problem(connected_at, now)
    return ("{} {!r}, read on New York's clock, is {} {} the adapter's clock when it connected ({}), more than the "
            "{} allowed: the Gateway is not on New York time; {}".format(
                what, text, _span(off), "ahead of" if off > timedelta(0) else "behind",
                connected_at.astimezone(NEW_YORK).strftime("%Y-%m-%d %H:%M:%S %Z"),
                _span(GATEWAY_CLOCK_TOLERANCE), _SET_NEW_YORK))


class ReconcileIncomplete(Exception):
    """IBKR's account could not be read in full, so the link is not reconciled and nothing is sent."""


class ReconcileMismatch(Exception):
    """IBKR shows an open order or execution the journal does not explain; the link is not reconciled."""


class ReportRecordInvalid(ValueError):
    """A report the adapter keeps (in an engine's log, the outbox, a settlement or awaiting its reply) is not as it writes one."""


def _utc_now():
    return datetime.now(timezone.utc)


def _batch_run(entry):
    """The run a stream batch's outbox entry was recorded for (its envelope's, or its snapshot's), or None for any other."""
    if "envelope" in entry:
        return entry["envelope"].get("correlation_id")
    return entry.get("run")


def _holdings(reading):
    """What a trade or an order changes in an account read: its positions, open orders, total cash and, for an IRA's
    reading (account.account_tags), its settled cash, which a settlement moves too (ADR 0019: one cutoff). A Boost
    reading holds no settled cash, so its comparison is unchanged."""
    return reading.positions, reading.open_orders, reading.balances.get(TOTAL_CASH), reading.balances.get(SETTLED_CASH)


def _next_entry(entries):
    """The outbox entry to send next: the oldest, except that a broker report behind a stream entry goes before it."""
    if "envelope" in entries[0]:
        for entry in entries[1:]:
            if _batch_run(entry) is None:
                return entry
    return entries[0]


def _order_name(record):
    """An open order's name in a broker snapshot (snapshot._take_order's broker_order_id)."""
    key = order_key(record.client_id, record.broker_order_id, record.perm_id)
    return "perm:{}".format(key[2]) if key[0] == "perm" else "order:{}:{}".format(key[1], key[2])


def _holding_text(positions, symbols):
    return ", ".join("{} {}".format(s, positions.get(s, 0)) for s in symbols)


def _event_time_readable(payload, field):
    """Whether payload holds field as a time with its zone, as the adapter stamps every report it records."""
    text = payload.get(field) if isinstance(payload, dict) else None
    try:
        at = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return False
    return at.tzinfo is not None


def close_window(close):
    """The earliest instant a Gateway's stamp counts as at the close: GATEWAY_CLOCK_TOLERANCE before it, since a
    Gateway up to that slow passes its clock check and may stamp a 16:02 execution 15:58 (ADR 0019, amended
    2026-10-28). Every at-or-after-close check on the Gateway's stamps or the journal uses it."""
    return close - GATEWAY_CLOCK_TOLERANCE


def _book_stated_by(approval, close):
    """Whether a run's approval states a starting book the owner approved no later than close (_journal_fills stops
    behind it)."""
    if not isinstance(approval, dict) or not isinstance(approval.get("starting_book"), dict):
        return False
    try:
        at = datetime.fromisoformat(str(approval["approved_at"]).replace("Z", "+00:00"))
    except (KeyError, TypeError, ValueError):
        return False
    return at.tzinfo is not None and at <= close


def _stood_for_a_close(envelope, close, checked):
    """Whether a logged input is a broker snapshot the engine committed stamped as of a close no later than close,
    one of checked, the snapshots whose readings ran the journal check of the close window (its reading stood for that
    close under that check; _journal_fills stops behind it)."""
    if envelope.get("type") != BROKER_SNAPSHOT_TYPE or envelope.get("id") not in checked:
        return False
    payload = envelope.get("payload")
    if not isinstance(payload, dict) or payload.get("as_of_close") is not True:
        return False
    try:
        as_of = datetime.fromisoformat(str(payload["as_of"]).replace("Z", "+00:00"))
    except (KeyError, TypeError, ValueError):
        return False
    return as_of.tzinfo is not None and as_of <= close


def _side_of_close(at, close):
    """Which side of close a stamp from close_window(close) on is, for a reason to name."""
    return "at or after the close" if at >= close else "within {} before the close".format(
        _span(GATEWAY_CLOCK_TOLERANCE))


def _stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _new_id(event_type):
    return "{}:{}:{}".format(SOURCE, event_type, uuid.uuid4().hex)


class _AskedOnItsOwnThread:
    """An engine client whose decide runs on a thread of its own while await_engine(worker) keeps this one busy."""

    def __init__(self, engine, await_engine):
        self._engine = engine
        self._await = await_engine

    def close(self):
        """Close the engine connection, if it can be closed (engine_client.EngineClient can)."""
        if hasattr(self._engine, "close"):
            self._engine.close()

    def decide(self, envelope):
        outcome = {}

        def ask():
            try:
                outcome["reply"] = self._engine.decide(envelope)
            except BaseException as err:  # handed back to the caller below
                outcome["error"] = err
        worker = threading.Thread(target=ask, name="engine-exchange", daemon=True)
        worker.start()
        self._await(worker)
        if "error" in outcome:
            raise outcome["error"]
        return outcome["reply"]


class AdapterLoop:
    """One engine's adapter: config's account on IB Gateway through api, and the engine through engine_connect.

    api is a tws.TWSApi (real_client.IbapiClient on the server, the fake in
    tests). engine_connect is the relay's connect (engine_client.connector).
    pending_path is the relay's pending-input file; every other file of the
    adapter's sits beside it. engine_log_path is the engine's durable log
    (its -log file). now returns the adapter's receipt time with its zone;
    today the session's New York date; new_id an input ID for an event type;
    poll how long one step waits for a callback (seconds). streams, when
    given, is the folder holding each run's stream folder (delivery.py);
    without it no stream is delivered. cash_ledger, when given, is the
    instance's Flex ledger (flex.py), whose journalled deposits and
    withdrawals the post-close reading's cash check subtracts before its
    bound is applied (_transfers); without it none is.
    """

    def __init__(self, config, api, engine_connect, pending_path, engine_log_path, now=_utc_now, today=None,
                 sleep=time.sleep, monotonic=time.monotonic, poll=1.0, new_id=_new_id, streams=None, cash_ledger=None):
        self.config = config
        self._cash_ledger = cash_ledger
        self._api = api
        self._engine_connect = engine_connect
        self._pending_path = pending_path
        self._engine_log_path = engine_log_path
        self._now = now
        self._today = today
        self._sleep = sleep
        self._monotonic = monotonic
        self._poll = poll
        self._new_id = new_id
        self.conn = None
        self.relay = None
        self.reports = None
        self.tracker = None
        self.outbox = Outbox(pending_path + OUTBOX_SUFFIX)
        self.pending = PendingInput(pending_path)
        # The order reports recorded by this adapter: how many, and the last
        # one's input ID (a broker snapshot's cutoff).
        self._reports_recorded = 0
        self._last_report = None
        # Every execution IBKR reports in the account, whichever client or
        # app placed its order, by its execution ID, so a replay of the
        # day's list counts nothing new (a broker snapshot's cutoff
        # watermark too).
        self._account_executions = set()
        # Set when a reconcile finds the engine holding a book: the
        # reconciliation it is owed, sent once no execution awaits its
        # commission (_send_reconciliation).
        self._reconciliation_due = False
        # How many reads in a row of that reconciliation left positions or
        # cash unread (_send_reconciliation).
        self._reconciliation_unread = 0
        # How many reconciles in a row the link was dropped during, since it
        # was last reconciled (reconcile, RECONCILE_CUTOFFS).
        self._reconcile_cutoffs = 0
        # The close whose batch last logged that it waits for the executions proof (_awaits_proof).
        self._proof_waited = None
        # Set by _morning_proof when the proof failed in a way a later firing of the Flex reader can clear,
        # before PROOF_WAIT_UNTIL: the batch then waits rather than withhold (_post_close_reading).
        self._proof_retryable = False
        # Every execution time IBKR reported in the account on this connection, as it gave it
        # (_count_execution), and why IB Gateway's own zone, read at this connection's reconcile, is not New
        # York's ("" when it is): both are read afresh at every reconcile (gateway_zone_problem,
        # gateway_clock_problem).
        self._execution_zones = set()
        self._gateway_zone = "IB Gateway's connection time has not been read yet (no reconcile has run)"
        self._last_wait = ""
        self.delivery = None if streams is None else Deliverer(
            streams, pending_path + DELIVERED_SUFFIX, self.outbox, engine_log_path, self._batch_reading, new_id,
            wait_reason=lambda: self._last_wait)

    def start(self):
        """Take the pending file's lock, connect, reconcile, and settle any input left awaiting its reply."""
        cfg = self.config
        self.conn = Connection(self._api, cfg, self._status, sleep=self._sleep, monotonic=self._monotonic,
                               wall_clock=self._now)
        sender = make_sender(cfg, self._sent, conn=self.conn, today=self._today)
        # The relay takes the adapter's exclusive lock first; the report
        # journal reads its held file only while that lock is held.
        self.relay = Relay(lambda: _AskedOnItsOwnThread(self._engine_connect(), self._await_engine), sender,
                           self.pending, lambda: collect_broker_orders(self.conn),
                           lambda order, session: release_to_request(order, cfg.mode, cfg.con_ids, session),
                           today=self._today, settled=lambda envelope: self.outbox.strike(envelope["id"]),
                           now=self._now)
        # The engine opens its socket only once its start is recorded, a
        # fresh run's Run approval included (cmd/engine), so nothing of the
        # log is read, and nothing of IBKR's journalled, before it listens:
        # a log read earlier could lack the approval naming the run before.
        # So the run's stream folder, found by the run's identity from the
        # log (delivery.stream_run), is looked up only now: a resume's copy
        # of the crashed run's records is whole once the engine listens.
        self.relay.connect_engine()
        if self.delivery is not None:
            self.delivery.check()
        # The earlier runs' reports too: IBKR replays the day's executions
        # and the session's orders after a fresh run starts, and a report an
        # earlier run's engine recorded is never recorded again.
        logs = run_logs(self._engine_log_path, self._session(), settled=self.pending.settled_orders())
        chain = [(run_identity(path), read_engine_log(path)) for path in logs]
        self.reports = OrderReportJournal(
            cfg.account_id, cfg.client_id, self._record, now=self._now, commission_wait=cfg.commission_wait,
            held_path=self._pending_path + HELD_SUFFIX, owns_lock=self.relay.holds, ready=self.ready,
            journalled=self._journalled(chain), acknowledged=self.pending.acknowledged_corrections(),
            late_fills=self.pending.acknowledged_late_fills())
        self.tracker = OrderTracker(cfg.account_id, cfg.client_id, self.reports.on_record,
                                    unverified_receipt=lambda: unverified_receipt(self.conn))
        self.conn.subscribe(self.tracker.on_event)
        self.conn.subscribe(self._count_execution)
        self.conn.connect_with_backoff()
        self.reconcile()
        # A link that went during that reconcile is made again and
        # reconciled, as step() does, before any input is settled
        # (Connection.drop; ADR 0025 point 5).
        while not self.ready():
            self.conn.maintain()
            if self.conn.connected and self.conn.needs_reconciliation:
                self.reconcile()
            else:
                self.conn.pump(self._poll)
        self._settle_waiting()

    def _gateway_problem(self):
        """Why IB Gateway's clock is not known to be New York's now ("" when it is): its reconcile's check
        (gateway_clock_problem), or New York's clocks having changed since (clock_change_problem)."""
        if self._gateway_zone or gateway_clock_trusted(self.conn):
            return self._gateway_zone
        return (clock_change_problem(self.conn.connected_at, self._now())
                or "IB Gateway's clock has not been checked on this link")

    def _session(self):
        """Today's session date (New York) as the loop was given it, or None for the relay's own default."""
        return self._today() if self._today is not None else None

    def ready(self):
        """Whether the link is up and reconciled since it last connected or was restored."""
        return self.conn is not None and self.conn.connected and not self.conn.needs_reconciliation

    def reconcile(self):
        """Read IBKR's account in full, match its open orders and executions to the journal, then mark the link reconciled.

        A link lost or dropped during the reads (the open orders never
        quiet, a socket close, Gateway losing IBKR's servers even if it
        restores them before the reads end: LinkChanged) leaves the link not reconciled and the
        adapter running: maintain() connects again and the new link is
        reconciled (Connection.drop), so nothing is sent meanwhile (ADR
        0025 point 5). RECONCILE_CUTOFFS such cut-offs in a row, with no
        reconcile completed between them, stop the adapter for a person
        (ReconcileIncomplete, naming the last drop's reason): a link that
        drops in every reconcile would otherwise cycle with nothing sent
        and no one told (fail closed and tell a person, AGENTS.md).

        Each reconcile starts this connection's evidence of IB Gateway's zone
        afresh: the zones of the executions it reports, and its connection
        time's zone and clock against the adapter's own when the link
        connected (gateway_clock_problem), so a Gateway set right and
        reconnected is trusted again without a restart (ADR 0019, amended
        2026-10-27 and 2026-10-28).
        """
        self._execution_zones = set()
        self.conn.gateway_clock_offset = None
        try:
            self._gateway_zone = gateway_clock_problem(self._api.connection_time(), self.conn.connected_at,
                                                       self._now())
            if not self._gateway_zone:
                # Executions read from here on are stamped as the Gateway gives them (orders.execution_report).
                self.conn.gateway_clock_offset = self.conn.connected_at.astimezone(NEW_YORK).utcoffset()
        except Exception as err:  # any failure to read it is a zone not known (fail closed)
            self._gateway_zone = "IB Gateway's connection time could not be read ({})".format(type(err).__name__)
        if self._gateway_zone:
            log.warning("ibkr %s; a morning reading on this connection is withheld", self._gateway_zone)
        try:
            self._reconcile()
        except (NotConnected, BrokerOrdersIncomplete, ReconcileIncomplete) as err:
            if self.conn.connected and not isinstance(err, LinkChanged):
                raise
            self._reconcile_cutoffs += 1
            reason = self.conn.last_drop_reason or "link lost, no drop reported"
            if self._reconcile_cutoffs >= RECONCILE_CUTOFFS:
                raise ReconcileIncomplete(
                    "the link to IB Gateway was dropped during {} reconciles in a row (last: {}), so it is not "
                    "reconciled and nothing is sent (ADR 0025 point 5: matched in full before anything is sent). "
                    "Check IB Gateway and whether another client or IBKR's own app is working the account's "
                    "orders, then start the adapter again (docs/manual/ibkr-adapter.md)".format(
                        self._reconcile_cutoffs, reason)) from None
            log.info("ibkr reconcile cut off (%d of %d): the link went during its reads (%s, %s); it is connected "
                     "again and reconciled before anything is sent", self._reconcile_cutoffs, RECONCILE_CUTOFFS,
                     type(err).__name__, reason)

    def _reconcile(self):
        """reconcile's reads and match, on a link that stays up.

        The open orders matched are two clean reads that agree
        (collect_settled_broker_orders), and the account read is taken after
        them and must agree with them (orders.agrees_with_account). An
        account read that differs, or during or after which a row or end of
        an open-order list arrived with no list outstanding (it is pumped
        quiet, Connection.resync_open_orders), or during or after which IBKR
        reported an execution in the account that the settled pair's
        executions do not hold (an order placed by hand that filled at
        once shows in no open-order list), is a fresh read of a book that
        changed: the orders are settled and the account read again, up
        to OPEN_ORDER_DISAGREEMENTS + 1 account reads, and only then
        ReconcileIncomplete (ADR 0025 point 5: matched in full before
        anything is sent; ADR 0019: a list not read in full is unread).
        """
        for _ in range(OPEN_ORDER_DISAGREEMENTS + 1):
            seen = set(self._account_executions)
            changes = self.conn.link_changes
            try:
                broker = collect_settled_broker_orders(self.conn)
            except OpenOrdersUnsettled as err:
                raise ReconcileIncomplete("{}; the link is not reconciled and nothing is sent (ADR 0019: a list not "
                                          "read in full is unread)".format(err)) from None
            strays = self.conn.open_order_strays
            reading = read_account(self.conn)
            if self.conn.link_changes != changes:
                # A read the outage cut off before IBKR ended its positions
                # or open orders leaves their late ends on this socket, where
                # one would complete the next read's: the link is dropped,
                # never read on again (see the module; ADR 0025 point 5: a late end
                # must not complete a later read).
                self._unended(reading)
            self._link_unchanged(changes)
            if reading.unread:
                raise ReconcileIncomplete("IBKR's account could not be read in full ({} unread; the account values "
                                          "{}); nothing is sent until a read is complete (ADR 0019: an unavailable "
                                          "balance is not zero)".format(", ".join(sorted(reading.unread)),
                                                                        reading.summary))
            self.conn.resync_open_orders()
            self._link_unchanged(changes)
            new_executions = self._account_executions - seen - {r.exec_id for r in broker.executions}
            if (self.conn.open_order_strays == strays and not new_executions
                    and agrees_with_account(broker, reading.open_orders)):
                break
            log.info("ibkr reconcile reads IBKR's open orders again: the account read differed from them, or a stray "
                     "order message or a new execution arrived during or after it")
        else:
            raise ReconcileIncomplete("IBKR's open orders and the account read did not agree in {} account reads; the "
                                      "link is not reconciled and nothing is sent (ADR 0019: a list not read in full "
                                      "is unread)".format(OPEN_ORDER_DISAGREEMENTS + 1))
        # An order an earlier run of this engine released (the runs its Run
        # approvals chain back to) is this engine's too: IBKR's day lists
        # still show it after a fresh run starts.
        expected = set(self.pending.handovers())
        for path in run_logs(self._engine_log_path, self._session(), settled=self.pending.settled_orders()):
            expected |= released_client_order_ids(path)
        match = match_on_reconnect(expected, broker, self.config.client_id)
        if match.unknown_at_broker or match.unknown_executions:
            unexplained = ["open order orderRef={} client_id={} order_id={} perm_id={}".format(
                r.client_order_id or "-", r.client_id, r.broker_order_id, r.perm_id) for r in match.unknown_at_broker]
            unexplained += ["execution {} orderRef={} client_id={} order_id={}".format(
                r.exec_id, r.client_order_id or "-", r.client_id, r.broker_order_id) for r in match.unknown_executions]
            raise ReconcileMismatch(
                "IBKR shows {} unexplained open order(s) or execution(s) in the account, which no order the engine "
                "released explains: {}. Nothing is sent: check them in IBKR (an order placed by hand or by another "
                "client, or one no run of this engine released) and reconcile the account before starting again "
                "(docs/manual/paper-trading.md)".format(len(unexplained), "; ".join(unexplained)))
        self.conn.mark_reconciled()
        self._reconcile_cutoffs = 0
        log.info("ibkr reconciled client_id=%d positions=%d open_orders=%d executions=%d", self.config.client_id,
                 len(reading.positions), len(broker.open_orders), len(broker.executions))
        # The start-of-run and reconnect reconciliation (ADR 0025 point 5,
        # ADR 0019): IBKR's positions and cash, for the engine to classify
        # against its book, which exists once it has committed an account
        # snapshot. Before that a run's first stream batch carries it, behind
        # its account snapshot (delivery.py). It is read and recorded only
        # once every execution IBKR replayed has its commission and its fill
        # is recorded (_send_reconciliation), so no fill reaches the engine
        # after a reconciliation that already shows its shares.
        if any(i.event_type == ACCOUNT_SNAPSHOT_TYPE for i in read_engine_log(self._engine_log_path).inputs):
            self._reconciliation_due = True

    def _link_unchanged(self, changes):
        """Raise LinkChanged if the link changed state since link_changes was changes: the reconcile is cut off.

        Its orders and executions, read before an outage, are never matched
        with an account read after it, even on a socket Gateway restored
        (ADR 0025 point 5; ADR 0019: one cutoff): the restored link is
        reconciled again from fresh reads.

        Callers must treat it as a cut-off: call this after each read of the
        reconcile, and never use a read taken before it.
        """
        if self.conn.link_changes != changes:
            raise LinkChanged("the link to IB Gateway was lost or dropped during the reconcile's reads")

    def _send_reconciliation(self):
        """Record the reconciliation a reconcile owes the engine, from a fresh read with nothing awaiting its commission.

        The read holds one cutoff, as the broker snapshot's does (ADR 0019):
        every callback already received is applied first; while an execution
        awaits its commission, or an order report or execution arrives during
        the read, or positions or cash could not be read, nothing is recorded
        and the next step tries again. Positions or cash unread in
        SNAPSHOT_ATTEMPTS reads in a row stop the adapter for a person
        (ReconcileIncomplete), as the broker snapshot's reading does. True
        once it is recorded.
        """
        self._pump_all()
        if not self.ready():
            return False
        if self.reports.awaiting_commission():
            log.info("ibkr reconciliation waits: an execution awaits its commission, so its fill is not recorded yet")
            return False
        before = self._activity()
        reading = read_account(self.conn)
        if self._unended(reading) or not self.ready():
            return False
        unread = sorted(reading.unread & {POSITIONS, CASH})
        if self._activity() != before:
            log.info("ibkr reconciliation read again next step: an order report or execution arrived during its "
                     "read")
            return False
        if unread:
            # Read again at most SNAPSHOT_ATTEMPTS times in a row
            # (snapshot.read_account): a component IBKR never gives
            # would otherwise hold the day's stream behind the reconciliation
            # in silence (fail closed and tell a person, AGENTS.md). An unread
            # component is never sent as zero (ADR 0019: an unavailable balance
            # is not zero).
            self._reconciliation_unread += 1
            log.info("ibkr reconciliation read again next step (attempt %d of %d): %s could not be read; the "
                     "account values %s", self._reconciliation_unread, SNAPSHOT_ATTEMPTS, ",".join(unread),
                     reading.summary)
            if self._reconciliation_unread >= SNAPSHOT_ATTEMPTS:
                raise ReconcileIncomplete(
                    "the reconciliation owed after the start or reconnect could not be read in {} attempts ({} "
                    "could not be read; the last account values {}), so no stream input is sent: a reconciliation with values IBKR did not "
                    "give would compare nothing (ADR 0019: an unavailable balance is not zero). Check IB "
                    "Gateway's link and the account's currency and positions, then start the adapter again "
                    "(docs/manual/market-data.md, \"What happens to the file\")".format(
                        SNAPSHOT_ATTEMPTS, ", ".join(unread), reading.summary))
            return False
        self._reconciliation_unread = 0
        input_id = self._new_id(RECONCILIATION_TYPE)
        record = reconciliation_record(reading, input_id)
        self.submit(record.event_type, record.schema_version, record.payload, record.payload["as_of"],
                    input_id=input_id)
        self._reconciliation_due = False
        log.info("ibkr reconciliation recorded for the engine id=%s as_of=%s", input_id, record.payload["as_of"])
        return True

    def submit(self, event_type, schema_version, payload, event_time, input_id=None):
        """Record an input for the engine in the outbox; it is sent, in order, while the link is ready."""
        self.outbox.append({"id": input_id or self._new_id(event_type), "event_type": event_type,
                            "schema_version": schema_version, "event_time": event_time, "payload": payload})

    def step(self):
        """One turn of the loop: keep the link up, reconcile it if needed, take one callback, check, and drain."""
        self.conn.maintain()
        if self.conn.connected and self.conn.needs_reconciliation:
            self.reconcile()
        self.conn.pump(self._poll)
        self.reports.check_overdue()
        if self.ready():
            if not self._reconciliation_due or self._send_reconciliation():
                if self.delivery is not None:
                    self.delivery.deliver()
            self._drain()

    def run(self, should_stop):
        """start, then step until should_stop() is true; the adapter's files are released however it ends."""
        try:
            self.start()
            while not should_stop():
                self.step()
        finally:
            self.close()

    def close(self):
        if self.relay is not None and self.relay.holds():
            self.relay.close()
        if self.conn is not None:
            try:
                self._api.disconnect()
            except Exception as err:  # the adapter is stopping either way
                log.info("ibkr disconnect at stop failed: %s", type(err).__name__)

    # -- the engine side ---------------------------------------------------
    def _journalled(self, chain):
        """The report records this run's or an earlier run's engine holds, the outbox holds, or that await a reply: never recorded again.

        chain is each run's (identity, EngineLog), newest first
        (relay.run_logs, relay.run_identity). The journal keeps the last
        status seeded for each order, so the records are seeded in the
        order they were made, never by their times: a lifecycle report's
        time is the adapter's clock at receipt (IBKR's orderStatus carries
        none), whole seconds only, and a clock set back would reorder them.
        That order is total: each run of the chain oldest first, its
        committed inputs in log order, then the run-ended settlements of
        that run in the order they were recorded (each input, then the
        outbox entries struck off with it, which the outbox held behind the
        run's committed inputs); then the input awaiting its reply, which is
        the outbox's head while it is exchanged; then the outbox's entries
        in order. A settlement whose run is no run of the chain was made
        before every run it reaches; one with no recorded run (its .run
        record was missing or named another input) has no place that can
        be known, so run_ended refuses it and the start stops for a person.
        The outbox's own copy of a
        record placed earlier is not seeded again. A record whose event
        time cannot be read stops the start (ReportRecordInvalid; fail
        closed, AGENTS.md), as every other record the adapter keeps is read
        strictly.
        """
        oldest = list(reversed(chain))
        position = {run: n for n, (run, _) in enumerate(oldest)}
        settled = self.pending.run_ended()
        placed = [None] * len(settled)
        bound = len(oldest) - 2
        for k in reversed(range(len(settled))):
            placed[k] = bound = min(position.get(settled[k]["run"], -1), bound)
        sources = []

        def add_settled(at):
            for k, entry in enumerate(settled):
                if placed[k] != at:
                    continue
                sources.append((self.pending.run_ended_path, entry["input"].get("id"), entry["input"].get("type"),
                                entry["input"].get("schema_version"), entry["input"].get("payload") or {}))
                sources.extend((self.pending.run_ended_path, o["id"], o["event_type"], o["schema_version"],
                                o["payload"]) for o in entry["outbox"])
        add_settled(-1)
        for n, (run, log_) in enumerate(oldest):
            sources.extend(("the durable log of run {}".format(run), i.id, i.event_type, i.schema_version, i.payload)
                           for i in log_.inputs)
            add_settled(n)
        # The outbox's copies of what is placed earlier (the input awaiting
        # its reply, an entry a settlement names, an input this run
        # committed before its strike) are never sent, so never seeded again.
        placed_ids = {i.id for i in oldest[-1][1].inputs} if oldest else set()
        placed_ids |= {e["input"].get("id") for e in settled} | {o["id"] for e in settled for o in e["outbox"]}
        waiting = self.pending.load()
        if waiting is not None:
            sources.append((self.pending.path, waiting.get("id"), waiting.get("type"), waiting.get("schema_version"),
                            waiting.get("payload") or {}))
            placed_ids.add(waiting.get("id"))
        sources.extend((self.outbox.path, e["id"], e["event_type"], e["schema_version"], e["payload"])
                       for e in self.outbox.load() if e["id"] not in placed_ids)
        records = []
        for where, input_id, event_type, schema_version, payload in sources:
            if event_type not in EVENT_TIME_FIELD:
                continue
            field = EVENT_TIME_FIELD[event_type]
            if not _event_time_readable(payload, field):
                raise ReportRecordInvalid(
                    "the report {} in {} has no {} that can be read (a time with its zone): it is not as the "
                    "adapter records one, so it is never seeded as an order's status; check it against IBKR "
                    "before starting (docs/manual/redelivery.md)".format(input_id, where, field))
            records.append(Record(event_type, schema_version, payload))
        return records

    def _record(self, record):
        """The report journal's sink: record IBKR's report as the engine's next input."""
        input_id = self._new_id(record.event_type)
        self.submit(record.event_type, record.schema_version, record.payload,
                    record.payload[EVENT_TIME_FIELD[record.event_type]], input_id=input_id)
        self._reports_recorded += 1
        self._last_report = input_id

    def _sent(self, record):
        log.info("ibkr sent client_order_id=%s client_id=%d order_id=%d", record.request.client_order_id,
                 record.client_id, record.broker_order_id)
        self.reports.sent(record)
        self.tracker.placed(record.broker_order_id)

    def _await_engine(self, worker):
        """While worker asks the engine, read every callback IB Gateway has sent and check for overdue commissions."""
        while True:
            # Read after the worker is seen finished too, so whatever arrived
            # during the exchange is applied before anything else.
            alive = worker.is_alive()
            self._pump_all()
            if not alive:
                return
            worker.join(ENGINE_WAIT_POLL)

    def _pump_all(self):
        """Apply every callback IB Gateway has already sent, then check for overdue commissions."""
        while self.conn.pump(0) is not None:
            pass
        self.reports.check_overdue()

    def _count_execution(self, event):
        """Count an execution IBKR reports in the account, of any client's order or none (see _activity)."""
        if event.name == "exec_details" and len(event.args) == 3:
            if getattr(event.args[2], "acctNumber", None) == self.config.account_id:
                self._account_executions.add(getattr(event.args[2], "execId", None))
                self._execution_zones.add(getattr(event.args[2], "time", None))

    def _activity(self):
        """A broker snapshot's cutoff watermark: the order reports recorded, and the executions seen.

        The executions are this adapter's (IBKR's replays not counted again)
        and every one IBKR reports in the account from any client: an order
        placed in IBKR's own app or by another client changes the holdings
        and cash as much as this adapter's do.
        """
        return self._reports_recorded, self.reports.executions_seen(), len(self._account_executions)

    def _batch_reading(self, close, closes):
        """_post_close_reading, keeping the last line it logged when it gives no reading: the reason the batch's
        outcome line states (delivery.Deliverer, outcome=waiting)."""
        said = []

        class Last(logging.Handler):
            def emit(self, record):
                # Observation only: a log call that cannot be formatted is
                # reported as logging's own handlers report it, never raised
                # into the reading (handleError prints and goes on).
                try:
                    said.append(record.getMessage())
                except Exception:  # noqa: BLE001 - logging's own contract for emit
                    self.handleError(record)
        handler = Last(logging.INFO)
        log.addHandler(handler)
        try:
            reading = self._post_close_reading(close, closes)
        finally:
            log.removeHandler(handler)
        # A reading that waits again without a new word (the proof wait is
        # said once per close) keeps the last reason it gave, so the batch's
        # waiting line names it, never a generic one.
        self._last_wait = "" if reading is not None else (said[-1] if said else self._last_wait)
        return reading

    def _post_close_reading(self, close, closes):
        """The batch's account read (delivery.PostCloseReading) for the session closing at close (see the module).

        closes maps each instrument the batch's last session delivers a bar
        for to that bar's official close: the market.mark.recorded of each,
        with IBKR's last price beside it, the price band's comparison (read_marks; a missing
        market-data permission leaves it unset). The reading is stamped as
        of close when it can stand for it (_not_as_of_close), and is then
        valued at the close: each holding marked at its official close and
        the net liquidation its cash plus those holdings (snapshot
        close_valuation), IBKR's own beside it; else it is stamped at its
        read time with why not, IBKR's own reading, each holding marked at
        IBKR's last price (ADR 0019, amended 2026-10-18: one price basis per
        rule).

        None when it cannot be taken now (the link went, an execution awaits
        its commission, a read was cut off before answers that carry no
        request ID ended, the account changed on every attempt, or the read
        time does not follow the last snapshot the engine holds);
        SnapshotRefused when the account could not be read in full within
        BROKER_SNAPSHOT_ATTEMPTS, each read after one that left a component
        unread waiting its SNAPSHOT_RETRY_WAITS first. market_open is the
        NYSE session's state at the stamp.

        Rule: ADR 0019 (an unavailable balance is not zero; one cutoff): a
        snapshot is taken only from one complete reading whose holdings agree.
        Callers must not deliver anything when it raises SnapshotRefused.
        """
        if self._awaits_proof(close):
            return None
        unread = None
        summary = ""
        started = self._monotonic()
        contracts = None
        for attempt in range(1, BROKER_SNAPSHOT_ATTEMPTS + 1):
            if unread is not None:
                wait = SNAPSHOT_RETRY_WAITS[attempt - 2]
                log.info("ibkr broker snapshot waits %.0fs before attempt %d of %d", wait, attempt,
                         BROKER_SNAPSHOT_ATTEMPTS)
                self.conn.pause(wait)
            self._pump_all()
            if not self.ready():
                log.info("ibkr link went during the broker snapshot; the stream waits until it is reconciled")
                return None
            if self.reports.awaiting_commission():
                log.info("ibkr broker snapshot waits: an execution awaits its commission, so its fill is not "
                         "recorded yet")
                return None
            if self.config.resolve_contracts and contracts is None:
                contracts = self._contracts(closes)
                if contracts is None:
                    return None
            before = self._activity()
            reading = read_account(self.conn)
            if self._unended(reading):
                return None
            symbols = set(closes) | set(reading.positions)
            con_ids = {s: self.config.con_ids[s] for s in symbols if s in self.config.con_ids}
            if contracts is not None:
                con_ids.update({s: contracts[s].con_id for s in symbols if s in contracts})
            last_prices = read_marks(self.conn, con_ids)
            # Read again after the marks: a trade IBKR reports to another
            # client only (an order placed in its own app) still changes
            # the holdings, open orders and cash, and only a second read
            # shows it.
            confirm = read_account(self.conn) if not reading.unread else None
            if confirm is not None and self._unended(confirm):
                return None
            # IBKR's open orders and executions, read last and inside the
            # same cutoff: the open-order list checked is the last one IBKR
            # returned, and an execution arriving during it reads again.
            broker = self._broker_lists() if confirm is not None else "unread"
            if broker is None:
                return None
            if not self.ready():
                log.info("ibkr link went during the broker snapshot; the stream waits until it is reconciled")
                return None
            if self._activity() != before:
                log.info("ibkr broker snapshot read again (attempt %d of %d): an order report or execution "
                         "arrived between its reads", attempt, BROKER_SNAPSHOT_ATTEMPTS)
                unread = None
                continue
            incomplete = reading.unread | (confirm.unread if confirm is not None else frozenset())
            if incomplete:
                unread = sorted(incomplete)
                summary = (confirm if confirm is not None and confirm.unread else reading).summary
                log.info("ibkr broker snapshot read again (attempt %d of %d): %s could not be read; the account "
                         "values %s", attempt, BROKER_SNAPSHOT_ATTEMPTS, ",".join(unread), summary)
                continue
            if _holdings(reading) != _holdings(confirm):
                log.info("ibkr broker snapshot read again (attempt %d of %d): the account's holdings, open orders "
                         "or cash changed between its reads", attempt, BROKER_SNAPSHOT_ATTEMPTS)
                unread = None
                continue
            judged = self._not_as_of_close(confirm, close, closes, broker)
            if judged is None:
                return None
            if self._proof_retryable:
                log.info("ibkr broker snapshot waits: the Flex reader's executions proof changed during the read "
                         "and does not hold now; it is read again until %s New York", PROOF_WAIT_UNTIL.strftime("%H:%M"))
                return None
            why, valuation, drift, unexplained, unverified = judged
            stamp = None if why else close
            log.info("ibkr broker snapshot read_at=%s as_of=%s cutoff: no order report, execution or account change "
                     "between its reads; the last report recorded before it %s%s", _stamp(confirm.as_of),
                     _stamp(stamp or confirm.as_of), self._last_report or "none",
                     "; cash drift {}, unexplained cash {}".format(drift, unexplained) if stamp
                     else "; not as of the close: " + why)
            # The second read's as_of is IBKR's time after every component
            # and the marks were read.
            if stamp:
                marks = {s: closes[s] for s in confirm.positions}
                account = account_snapshot_record(confirm, stamp, net_liquidation=valuation,
                                                  version=self.config.account_snapshot_version)
                snapshot = broker_snapshot_record(confirm, market_open(stamp), marks, close=stamp,
                                                  net_liquidation=valuation, cash_drift=drift,
                                                  unexplained_cash=unexplained)
            else:
                marks = {s: last_prices[s] for s in confirm.positions if s in last_prices}
                account = account_snapshot_record(confirm, confirm.as_of, version=self.config.account_snapshot_version)
                snapshot = broker_snapshot_record(confirm, market_open(confirm.as_of), marks, why=why)
            if unverified:
                log.warning("ibkr broker snapshot read_at=%s: the engine holds no book and the reading is not the "
                            "book it was compared with, so only its reconciliation is sent, its snapshots withheld: "
                            "the engine halts on it (docs/manual/reconciliation.md)", _stamp(confirm.as_of))
            return PostCloseReading(
                account=account, broker=snapshot,
                marks=[mark_record(s, close, closes[s], MARK_SOURCE, last_prices.get(s),
                                   contract=None if contracts is None else contracts[s])
                       for s in sorted(closes) if contracts is None or s in contracts],
                reading=confirm, unverified=unverified)
        if unread is not None:
            raise SnapshotRefused(
                "the account could not be read in full for the broker snapshot in {} attempts over {:.0f} seconds "
                "({} could not be read; the last account values {}), so the day's stream is not delivered: a "
                "snapshot with values IBKR did not give would withhold the sale back to 98% for nothing. Check IB "
                "Gateway's link and the account's currency and positions, then start the adapter again "
                "(docs/manual/ibkr-adapter.md, \"What can go wrong\")".format(
                    BROKER_SNAPSHOT_ATTEMPTS, self._monotonic() - started, ", ".join(unread), summary))
        return None

    def _contracts(self, closes):
        """The IBKR contract of each instrument closes marks (snapshot.resolve_contracts), or None if the link went.

        ADR 0026 point 3: the engine values every order it plans against a
        mark as of the close, and a mark states the contract it is for. A
        name IBKR does not resolve to exactly one US-dollar stock on SMART is
        left out of the marks, each named in a warning with why, so the
        engine refuses any order for it (missing-mark), never sizes it on a
        guessed contract (docs/manual/ibkr-adapter.md, "Contracts and marks
        for an IRA").
        """
        resolved = resolve_contracts(self.conn, sorted(closes), self.config.con_ids)
        if resolved is None:
            log.info("ibkr link went while contracts were resolved; the stream waits until it is reconciled")
            return None
        contracts, refused = resolved
        log.info("ibkr contracts resolved=%d of %d", len(contracts), len(closes))
        for symbol in sorted(refused):
            log.warning("ibkr contract for %s not resolved, so it has no mark and the engine refuses any order for "
                        "it (missing-mark): %s", symbol, refused[symbol])
        return contracts

    def _broker_lists(self):
        """IBKR's open orders and executions for a broker snapshot: orders.BrokerOrders, "unread", or None to wait.

        None when the link went or the open-order list was cut off (it carries
        no request ID, so the link is dropped first; see the module);
        "unread" when only the executions could not be read in full.
        """
        if not self.ready():
            return None
        try:
            return collect_broker_orders(self.conn)
        except NotConnected:
            return None
        except BrokerOrdersIncomplete as err:
            if "open-orders" in err.missing and "connection" not in err.missing:
                self.conn.drop("untagged-read-interrupted")
            if "connection" in err.missing or "open-orders" in err.missing:
                return None
            return "unread"

    def _not_as_of_close(self, reading, close, closes, broker):
        """Whether reading stands for the account as of close: (why, valuation, drift, unexplained, unverified), or None.

        None when it cannot be stamped now. why is "" when it stands for the
        close, and then valuation is its net liquidation at the close
        (snapshot.close_valuation), drift the cash drift accepted and
        unexplained that drift added up (_since_previous_close); otherwise
        why names every condition that failed. unverified is
        _since_previous_close's.

        The owner's decision of 2026-10-18 (ADR 0019, ADR 0025 point 5, ADR
        0026 point 1, amended): after the close and before the next open,
        with no order working or pending at IBKR for the account and no
        execution since the close, the account does not change, so the
        reading is the account as of the close. Each condition, checked
        against IBKR's own lists, as the start's reconciliation reads them:

        - read after the close, on the session's own New York date: IBKR's
          executions reach back only to midnight (ADR 0025 point 5), so only
          then does its list cover every execution since the close (and the
          next open is on a later date); or on a later date with the morning
          proof (_morning_proof; the owner's decision of 2026-10-27, ADR
          0019 amended that day): before the next open, the Flex reader's
          executions proof covering the close to the reading's midnight with
          nothing in it at or after the close, and no fill journalled from
          close_window(close) on (within 5 minutes before the close or after);
        - no open order for the account, of any client (working, pending or
          cancel-pending, snapshot.OPEN_STATUS), in the account read and in
          the last open-order list IBKR returned (broker);
        - no execution in the account stamped from close_window(close) on
          (within 5 minutes before the close, a Gateway clock's tolerance,
          or after it) in IBKR's list (orders.collect_broker_orders), which
          shows other clients' executions only when this client ID is
          Gateway's Master API client ID; one stamped at the close's own
          second counts, since the engine applies a fill stamped in a book's
          second to that book; on the session's own date, no fill journalled
          from close_window(close) on either (_journal_reason);
        - every holding has an official close in the batch, so the account
          can be valued at the close, and IBKR's own net liquidation at the
          read is positive (the margin alarm's basis beside the valuation);
        - a check that needs no client ID: the holdings are the previous
          close's adjusted by the fills since, and the total cash is the
          previous close's adjusted by those fills, within CASH_DRIFT_BOUND
          of the net liquidation at the close, or IBKR's own at the read
          when the account cannot be valued at the close (a posting such as
          interest or a dividend),
          the difference recorded as the cash drift; a corporate action, a
          transfer or a trade this client cannot see fails it. The previous
          close is found back through the chain of runs, or is the owner's
          starting book (_since_previous_close, book_basis), so a fresh
          run's first batch is compared too. Its residual: a round trip
          another client makes after the close, out of this client's view,
          leaves the holdings unchanged and shows only as cash drift; the
          executions are asked for every client and Gateway's Master API
          client ID is set to the adapter's (a runbook precondition), so
          IBKR's list shows it;
        - the close after every account or broker snapshot the engine has
          committed: each is stamped later than the one before it.

        A stamp at the read time must itself follow the last the engine
        holds; when it does not (a clock behind), None: the batch waits.
        """
        reasons = []
        self._proof_retryable = False
        read_ny, close_ny = reading.as_of.astimezone(nyse_calendar.NEW_YORK), close.astimezone(nyse_calendar.NEW_YORK)
        if not reading.as_of > close:
            reasons.append("it was read at {}, not after the close at {}".format(_stamp(reading.as_of), _stamp(close)))
        elif read_ny.date() != close_ny.date():
            reasons.extend(self._morning_proof(reading, close))
        gateway = self._gateway_problem() if read_ny.date() == close_ny.date() else ""
        if reading.open_orders:
            reasons.append("IBKR shows {} open order(s) for the account (working, pending or cancel-pending): "
                           "{}".format(len(reading.open_orders),
                                       ", ".join(o["broker_order_id"] for o in reading.open_orders)))
        if broker == "unread":
            reasons.append("IBKR's executions could not be read in full")
        else:
            if broker.open_orders:
                reasons.append("IBKR's last open-order list shows {} open order(s) for the account: {}".format(
                    len(broker.open_orders), ", ".join(_order_name(o) for o in broker.open_orders)))
            # At or after the close's second: the engine applies a fill
            # stamped in the book's second to that book (internal/reconcile
            # Classify), so a closing-auction fill stamped at the close is
            # one the close's snapshot must not already show.
            # An execution stamped within the clock tolerance before the close is taken as at it (close_window).
            since = [e for e in broker.executions if e.occurred_at >= close_window(close)]
            if since:
                if gateway:
                    # The cause first: on a link whose clock is not believed, each execution is stamped no earlier
                    # than the adapter received it (orders.execution_report), so all of the day's are after the close.
                    reasons.insert(0, "{}; so IBKR's execution times on this link are not believed, and each is "
                                      "taken at the time the adapter received it if that is later".format(gateway))
                reasons.append("IBKR reports {} execution(s) in the account since the close, or within {} before "
                               "it (a Gateway clock up to that slow passes its check): {}".format(
                                   len(since), _span(GATEWAY_CLOCK_TOLERANCE),
                                   ", ".join("{} ({})".format(e.exec_id, _side_of_close(e.occurred_at, close))
                                             for e in since)))
        if read_ny.date() == close_ny.date() and reading.as_of > close:
            # The journal's stamps are the ones the engine applies on top of the close's book: one journalled after
            # the close on an earlier link (a fast Gateway's, before its check) withholds a same-date reading too,
            # though IBKR's own list now shows the execution before the close.
            journal = self._journal_reason(close)
            if journal:
                reasons.append(journal)
        valuation = None
        try:
            valuation = close_valuation(reading, closes)
        except ValueError as err:
            reasons.append(str(err))
        if NET not in reading.unread and not reading.balances[NET_LIQUIDATION].amount > 0:
            # The margin alarm's basis at the read: a snapshot as of the
            # close states IBKR's own beside its valuation, positive (ADR
            # 0019: an unavailable balance is not zero); one that is not is
            # IBKR's own reading, at the read time, for the alarm to read.
            reasons.append("IBKR's own net liquidation at the read is {}, not positive".format(
                reading.balances[NET_LIQUIDATION].amount))
        drift, unexplained, unverified = self._since_previous_close(reading, valuation, reasons)
        last = self._last_stamp(read_engine_log(self._engine_log_path))
        if last is not None and not close > last:
            reasons.append("the engine already holds an account or broker snapshot as of {}, not before this "
                           "close".format(_stamp(last)))
        if reasons and last is not None and not reading.as_of > last:
            log.info("ibkr broker snapshot waits: its read time %s does not follow the last snapshot the engine "
                     "holds, as of %s", _stamp(reading.as_of), _stamp(last))
            return None
        return "; ".join(reasons), valuation, drift, unexplained, unverified

    def _morning_proof(self, reading, close):
        """Why a reading on a later New York date than close cannot stand for it: [] when every proof holds.

        The owner's decision of 2026-10-27 (ADR 0019, amended 2026-10-27):
        IBKR's executions reach back only to midnight (ADR 0025 point 5), so
        a reading the next morning sees in IBKR's own list only those since
        the midnight its New York day starts at. The span from the close to
        that midnight is proven by the Flex reader's executions proof
        (_flex_proof), and the rest by the journal and the reading itself:

        - the reading is before the next session's open (by the NYSE
          calendar), so the account has not traded since;
        - the executions proof lists every execution from the session's day
          up to that midnight, and none at or after the close;
        - IBKR's own list shows none from close_window(close) on (checked
          with the other conditions, _not_as_of_close);
        - the journal (the engine's durable log and the outbox) holds no fill
          stamped from close_window(close) on: within 5 minutes before the
          close or after it.
        """
        reasons = []
        read_ny = reading.as_of.astimezone(nyse_calendar.NEW_YORK)
        day = close.astimezone(nyse_calendar.NEW_YORK).date()
        later = ("it was read at {}, on a later New York date than the close at {}: IBKR's executions reach back only "
                 "to midnight, so those between the close and midnight are proven only by the Flex reader's "
                 "executions proof, and ".format(_stamp(reading.as_of), _stamp(close)))
        try:
            opens = datetime.combine(nyse_calendar.sessions_after(day, 1)[0], SESSION_OPEN, nyse_calendar.NEW_YORK)
        except nyse_calendar.OutOfRange:
            opens = None
        if opens is None or not reading.as_of < opens:
            reasons.append("it was read at {}, not before the next session's open{}: the account may have traded "
                           "since the close".format(_stamp(reading.as_of),
                                                    " at " + _stamp(opens) if opens is not None else ""))
        midnight = datetime.combine(read_ny.date(), clock_time(0), nyse_calendar.NEW_YORK)
        proof, decisive = self._flex_proof(close, midnight)
        reasons.extend(later + why for why in proof)
        # A proof a later firing of the Flex reader can clear is waited for until PROOF_WAIT_UNTIL, as before the
        # read (_awaits_proof): one replaced while the account was read is read again, not withheld on.
        now = self._now().astimezone(nyse_calendar.NEW_YORK)
        self._proof_retryable = (bool(proof) and not decisive and self._cash_ledger is not None and now > close
                                 and now.date() != day and now.time() < PROOF_WAIT_UNTIL)
        gateway = self._gateway_problem()
        if gateway:
            reasons.append(gateway)
        # Each execution time is checked as the connection time is, quoted as IBKR gave it.
        for text in sorted(self._execution_zones, key=repr):
            problem = gateway_zone_problem(text, "IBKR's execution time")
            if problem:
                reasons.append(problem)
        journal = self._journal_reason(close)
        if journal:
            reasons.append(journal)
        return reasons

    def _journal_reason(self, close):
        """Why the journal shows the account may have traded at the close or after it ("" when it does not): the
        fills _journal_fills finds, each saying which side of the close it is, and why the chain of runs' logs cannot
        be read, each when there is one."""
        fills, refused = self._journal_fills(close)
        reasons = [refused] if refused else []
        if fills:
            reasons.append("the journal records {} fill(s) after the close, within {} before it (a Gateway clock up to "
                           "that slow passes its check), or with a time that cannot be compared with it: {}; the "
                           "journal keeps them, so this session's reading stays withheld".format(
                               len(fills), _span(GATEWAY_CLOCK_TOLERANCE), ", ".join(fills)))
        return "; ".join(reasons)

    def _flex_proof(self, close, midnight):
        """(reasons, decisive): why the Flex reader's executions proof does not show that nothing executed in the
        account from close up to midnight ([] when it does), and whether it lists such an execution (a reason no later
        reading of the proof can clear). See flex.py, step 5."""
        day = close.astimezone(nyse_calendar.NEW_YORK).date()
        if self._cash_ledger is None:
            return ["no Flex ledger is configured (--cash-ledger), so no executions proof is read"], False
        path = flex.executions_path(self._cash_ledger)
        try:
            proof = flex.read_executions(path, self.config.account_id)
        except flex.StatementRefused as refused:
            return [str(refused)], False
        if proof is None:
            return ["no executions proof is written ({}): the Flex reader (ti-flex@<instance>) has not read a "
                    "statement with the account's executions (docs/manual/reconciliation.md, \"The executions "
                    "proof\")".format(path)], False
        if proof.state != flex.PROOF_READ:
            return ["the executions proof is {}: {}".format(proof.state, proof.reason)], False
        after = [i for i, at in proof.executions if at >= close]
        if after:
            return ["IBKR's Flex statement lists {} execution(s) in the account at or after the close: {}".format(
                len(after), ", ".join(after))], True
        reasons = []
        if proof.from_day > day:
            reasons.append("the executions proof starts on {}, after the session's day {}".format(
                proof.from_day.isoformat(), day.isoformat()))
        if proof.covered_until < midnight:
            reasons.append("the executions proof lists every execution only up to {}, not up to {}, the midnight "
                           "IBKR's own list starts at (a statement lists them up to the start of its last day, "
                           "{})".format(_stamp(proof.covered_until), _stamp(midnight), proof.to_day.isoformat()))
        return reasons, False

    def _journal_fills(self, close):
        """(fills, refused): the fills journalled stamped from close_window(close) on (at or after the close, or
        within GATEWAY_CLOCK_TOLERANCE before it), each named with its stamp and which of the two it is, in the
        outbox, this run's durable log, and the log of every earlier run its Run approvals chain back to
        (relay.run_chain), so a fresh run hides no fill of a run before it; and why the chain cannot be read ("" when
        it can; fail closed, AGENTS.md).

        When a run's business was recorded proves nothing of the stamps of the runs behind it: a fill read on a link
        whose Gateway clock was not believed is stamped with the later of the Gateway's reading and its receipt
        (orders.execution_report), and a fast Gateway's reading can be hours after the fill was recorded. The walk
        stops instead after the first run, newest first, that either
        - holds a broker snapshot the engine committed stamped as of a close, whose reading this adapter recorded
          under this same check (Deliverer.journal_checked_snapshots): it stood only because the check, reading every
          run behind it, found no fill stamped from that close's window on, so every fill behind it is stamped before
          that earlier close's window and so before this one's; a snapshot an earlier adapter recorded bounds
          nothing; or
        - was approved, no later than close, with a starting book the owner stated (ADR 0027): the account at the
          approval, which every fill of the runs behind it is in, each made before it and so before the close. A
          fresh run so approved is the way past a log that cannot be restored.
        (ADR 0019, amended 2026-10-28.) A chain that cannot be read up to that run is a reason, given with the fills
        found before it (fail closed, AGENTS.md).
        """
        payloads = [e["payload"] for e in self.outbox.load() if e["event_type"] == FILL_TYPE]
        checked = self.delivery.journal_checked_snapshots() if self.delivery is not None else frozenset()
        refused = ""
        try:
            for _, groups, approval in run_chain(self._engine_log_path):
                inputs = [envelope for group in groups for _, kind, envelope in group if kind == LOG_INPUT]
                payloads += [e["payload"] for e in inputs if e["type"] == FILL_TYPE]
                if any(_stood_for_a_close(e, close, checked) for e in inputs) or _book_stated_by(approval, close):
                    break
        except ResolveRefused as err:
            refused = "the chain of runs' logs cannot be read, so their fills cannot be checked ({})".format(err)
        found, seen = [], set()
        for payload in payloads:
            ident = str(payload.get("fill_id", "a fill with no ID")) if isinstance(payload, dict) else "a fill"
            try:
                at = datetime.fromisoformat(payload["filled_at"].replace("Z", "+00:00"))
            except (KeyError, AttributeError, TypeError, ValueError):
                found.append("{} (its time cannot be read)".format(ident))
                continue
            if at.tzinfo is None:
                # Fail closed (AGENTS.md): a time with no zone cannot be placed against the close.
                found.append("{} (its time has no zone)".format(ident))
            elif at >= close_window(close) and ident not in seen:
                seen.add(ident)
                found.append("{} (stamped {}, {})".format(ident, _stamp(at), _side_of_close(at, close)))
        return found, refused

    def _awaits_proof(self, close):
        """Whether a batch for the session closing at close waits, unread, for the Flex reader's executions proof.

        The owner's decision of 2026-10-27 (ADR 0019, amended 2026-10-27):
        the reader runs each weekday morning before the data job, and again
        an hour later; until PROOF_WAIT_UNTIL New York on a later date than
        the close, a proof not yet written, set aside or not yet covering up
        to that day's midnight is waited for rather than the session
        withheld. One that lists an execution after the close, or none with
        no Flex ledger configured, is decided at once.
        """
        if self._cash_ledger is None:
            return False
        now = self._now().astimezone(nyse_calendar.NEW_YORK)
        if not now > close or now.date() == close.astimezone(nyse_calendar.NEW_YORK).date():
            return False
        if now.time() >= PROOF_WAIT_UNTIL:
            return False
        reasons, decisive = self._flex_proof(close, datetime.combine(now.date(), clock_time(0),
                                                                     nyse_calendar.NEW_YORK))
        if not reasons or decisive:
            return False
        if self._proof_waited != close:
            self._proof_waited = close
            log.info("ibkr broker snapshot waits for the Flex reader's executions proof until %s New York: %s",
                     PROOF_WAIT_UNTIL.strftime("%H:%M"), "; ".join(reasons))
        return True

    def _since_previous_close(self, reading, valuation, reasons):
        """(drift, unexplained, unverified) of reading against the previous close, adding to reasons what fails.

        The previous close is book_basis.previous_close's daily basis: the
        latest account snapshot in this run or the runs its Run approvals
        chain back to, its total cash the reconciliation read with it, or
        the latest starting book an approval states (ADR 0019 and ADR 0027,
        amended 2026-10-18), with every fill committed since and those still
        in the outbox, stamped at or after its as-of time and before the
        read (book_basis.fills_since). The holdings must be its own adjusted by those fills,
        share for share, and the total cash its own adjusted by them within
        CASH_DRIFT_BOUND of the net liquidation at the close (IBKR's own at
        the read when valuation is None); drift is the
        cash difference, and unexplained the cash IBKR shows beyond the
        unexplained-cash basis and every fill since it, the drift added up.
        unverified is set when the engine holds no book (the basis is not
        this run's own account snapshot) and the reading failed the
        comparison or had nothing to be compared with: its reconciliation
        then goes alone, its snapshots withheld (delivery._entries), never
        adopted. A cash check that cannot be bounded fails the comparison,
        and so does a basis that cannot be compared with (Basis.refused).
        """
        daily, cumulative = previous_close(self._engine_log_path, self._pending_path + BASIS_SUFFIX)
        committed = {i.id for i in read_engine_log(self._engine_log_path).inputs}
        pending = [e["payload"] for e in self.outbox.load()
                   if e["event_type"] == FILL_TYPE and e["id"] not in committed]
        if daily is None:
            reasons.append("no account snapshot of a previous close is committed in this run or the runs its Run "
                           "approvals chain back to, and no approval states the starting book, so the reading has "
                           "nothing to be compared with and is never adopted as the book: approve the account's "
                           "first run with the holdings and cash checked in IBKR (operator approve -first-run; "
                           "docs/runbooks/first-paper-order.md)")
            return Decimal(0), Decimal(0), True
        if daily.refused:
            reasons.append(daily.refused)
            return Decimal(0), Decimal(0), not daily.own
        # Only the fills stamped at or after the basis's as-of time and before
        # the read move it (book_basis.fills_since, internal/reconcile's
        # window): one already in the book is never counted again.
        fills = fills_since(daily.fills + pending, daily.as_of, reading.as_of)
        failed = False
        expected = holdings_after(daily.positions, fills)
        if expected != reading.positions:
            differ = sorted(set(expected) | set(reading.positions))
            reasons.append("IBKR holds {}, but {} and the {} fill(s) committed since give {}: a change no execution "
                           "this adapter saw explains (another client's trade, a transfer or a corporate "
                           "action)".format(_holding_text(reading.positions, differ), daily.source, len(fills),
                                            _holding_text(expected, differ)))
            failed = True
        if daily.cash is None:
            reasons.append("the total cash of {} is not recorded (no reconciliation of the same reading, or its cash "
                           "unread), so the cash since cannot be checked".format(daily.source))
            return Decimal(0), Decimal(0), not daily.own
        cash = reading.balances[TOTAL_CASH].amount
        # A deposit or withdrawal the Flex reader journalled is subtracted
        # before the bound is applied, the bound itself unchanged (ADR 0019,
        # amended 2026-10-21); a ledger that cannot be read fails the check.
        transfers, refused = self._transfers(daily.as_of, reading.as_of)
        if refused:
            reasons.append(refused)
            failed = True
        drift = _cents(cash - (daily.cash + cash_change(fills) + transfers))
        # The cash bound is part of the comparison (ADR 0019 condition 2): an
        # account that cannot be valued at the close is bounded by IBKR's
        # own net liquidation at the read, and with neither the check fails
        # rather than pass unchecked (AGENTS.md: fail closed).
        bound, basis = valuation, "net liquidation"
        if bound is None and NET not in reading.unread and reading.balances[NET_LIQUIDATION].amount > 0:
            bound, basis = reading.balances[NET_LIQUIDATION].amount, "IBKR's own net liquidation at the read"
        if bound is None:
            reasons.append("the account has no net liquidation the cash since {} can be bounded by, so the cash "
                           "cannot be checked".format(daily.source))
            failed = True
        elif abs(drift) > CASH_DRIFT_BOUND * abs(bound):
            reasons.append("IBKR's total cash differs from that of {} and the {} fill(s) committed since by {}, "
                           "beyond the {}% of {} ({}) allowed for postings such as interest or a "
                           "dividend".format(daily.source, len(fills), drift, (CASH_DRIFT_BOUND * 100).normalize(),
                                             basis, _cents(CASH_DRIFT_BOUND * abs(bound))))
            failed = True
        if cumulative.refused:
            reasons.append(cumulative.refused)
            return drift, Decimal(0), not daily.own
        since = fills_since(cumulative.fills + pending, cumulative.as_of, reading.as_of)
        transferred, _ = self._transfers(cumulative.as_of, reading.as_of)
        unexplained = _cents(cash - (cumulative.cash + cash_change(since) + transferred))
        return drift, unexplained, failed and not daily.own

    def _transfers(self, since, until):
        """(total, refusal): the deposits less the withdrawals in the Flex ledger dated from the moment a basis as of
        since holds transfers until and before until, and why the ledger cannot be read, if it cannot.

        A basis stamped at its session's close was read after the close, so it holds the rest of that New York day,
        as the reader takes it to (flex._held_until; ADR 0019, "the reading after the close counts as the close");
        any other holds what came before it. Each line is checked as the reader writes it (flex.read_ledger); one
        that is not refuses the whole ledger, and the cash check then fails rather than pass unchecked. With no
        ledger configured, or none written yet, there is nothing to subtract."""
        if self._cash_ledger is None:
            return Decimal(0), None
        try:
            ledger = flex.read_ledger(self._cash_ledger)
            start = flex._held_until(since)
        except flex.StatementRefused as refused:
            return Decimal(0), "the Flex ledger cannot be read, so the cash since the previous close cannot be " \
                               "checked: {}".format(refused)
        total = Decimal(0)
        for record in ledger.values():
            at = flex._parse_stamp(record["payload"]["as_of"])
            if start <= at < until:
                total += Decimal(str(record["payload"]["amount"]))
        return total, None

    def _last_stamp(self, engine_log):
        """The latest as-of time of an account or broker snapshot engine_log holds committed, or None."""
        stamps = [datetime.fromisoformat(i.payload["as_of"].replace("Z", "+00:00"))
                  for i in engine_log.inputs
                  if i.event_type in (ACCOUNT_SNAPSHOT_TYPE, BROKER_SNAPSHOT_TYPE)]
        return max(stamps, default=None)

    def _unended(self, reading):
        """Whether reading was cut off before answers that carry no request ID ended; if so the link is dropped (see the module)."""
        if not reading.unended:
            return False
        log.info("ibkr account read: IBKR had not ended its %s when the read gave up, and a late end would "
                 "complete a later read on this link, so the link is made again and reconciled first",
                 ",".join(sorted(reading.unended)))
        self.conn.drop("untagged-read-interrupted")
        return True

    def _envelope(self, entry, engine_log):
        """The input envelope for an outbox entry: the run's identity and next sequence from the engine's log.

        A stream entry is its envelope as the data job wrote it, with the
        engine's next sequence in place of the file's own and its send time
        as its recorded time, as every input's (see the module).
        """
        if "envelope" in entry:
            envelope = dict(entry["envelope"])
            envelope["sequence"] = engine_log.next_sequence
            envelope["recorded_at"] = _stamp(self._now())
            return envelope
        encoded = json.dumps(entry["payload"], separators=(",", ":"), allow_nan=False).encode("utf-8")
        return {"id": entry["id"], "type": entry["event_type"], "envelope_version": ENVELOPE_VERSION,
                "schema_version": entry["schema_version"], "event_time": entry["event_time"],
                "recorded_at": _stamp(self._now()), "sequence": engine_log.next_sequence, "source": SOURCE,
                "strategy_version": engine_log.strategy_version,
                "configuration_hash": engine_log.configuration_hash,
                "payload_hash": hashlib.sha256(encoded).hexdigest(), "payload": entry["payload"]}

    def _settle_waiting(self):
        """Ask again, byte for byte, for the reply to the input a stop left awaiting one; strike it off the outbox.

        Only this run's input is asked for again (ADR 0027, the restart
        model: a crash resumes the same run; any other restart is a fresh
        run): one an earlier run left stops the adapter for a person
        (EarlierRunInput), and nothing of it is sent.
        """
        waiting = self.pending.load()
        if waiting is None:
            return
        ended = earlier_run(self.pending, self._engine_log_path)
        if ended is not None:
            raise EarlierRunInput(ended, self._pending_path, self._engine_log_path)
        self._exchange(waiting)

    def _exchange(self, envelope):
        # The relay strikes the entry off before it clears the input
        # awaiting its reply (its settled).
        reply = self.relay.exchange(envelope)
        kinds = [d.get("type") for d in reply["payload"]["decisions"]]
        log.info("ibkr input=%s type=%s answered with %d decisions %s", envelope["id"], envelope["type"],
                 len(kinds), ",".join(kinds))
        # Every answered exchange settles its entry for the batch's outcome,
        # an input a stop left awaiting its reply and asked for again
        # (_settle_waiting) included.
        self._settled(envelope["id"], answered=True)

    def _drain(self):
        try:
            self._settle_waiting()
            while True:
                entries = self.outbox.load()
                if not entries:
                    return
                entry = _next_entry(entries)
                settled = self.pending.run_ended()
                if entry["id"] in {e["input"]["id"] for e in settled} | {o["id"] for e in settled
                                                                        for o in e["outbox"]}:
                    # Settled as left by an earlier run, or recorded behind
                    # an input so settled: never sent.
                    self.outbox.strike(entry["id"])
                    self._settled(entry["id"], answered=False)
                    continue
                batch_run = _batch_run(entry)
                # The run's stream folder name: a crash resume keeps the
                # first run's (delivery.stream_run), so its batch goes on.
                running = self.delivery.run if self.delivery is not None else run_name(self._engine_log_path)
                if batch_run is not None and batch_run != running:
                    # An earlier run's stream batch: the run now running is a
                    # fresh run with its own, and the earlier run gets it
                    # again if resumed.
                    # Marked dropped first, then struck off whole in one
                    # write, so a stop leaves the batch whole in the outbox
                    # (sent whole to that run if it is resumed) or wholly
                    # gone, never its stream without its snapshot.
                    batch = [e["id"] for e in entries if _batch_run(e) == batch_run]
                    log.info("ibkr stream batch of run %s (%d inputs) struck off unsent: the engine now runs %s",
                             batch_run, len(batch), running)
                    if self.delivery is not None:
                        self.delivery.dropped(batch_run)
                    self.outbox.strike_all(batch)
                    continue
                engine_log = read_engine_log(self._engine_log_path)
                committed = {i.id: i for i in engine_log.inputs}.get(entry["id"])
                if committed is not None:
                    if (committed.event_type, committed.schema_version, committed.payload) != (
                            entry["event_type"], entry["schema_version"], entry["payload"]):
                        raise OutboxInvalid(
                            "the outbox entry {} carries other content than the input the engine committed under "
                            "that ID; it is neither sent nor struck off: a person decides which is right "
                            "(docs/manual/paper-trading.md)".format(entry["id"]))
                    self.outbox.strike(entry["id"])
                    self._settled(entry["id"], answered=False)
                    continue
                if batch_run is not None and self._reconciliation_due:
                    # The reconciliation a reconcile owes goes ahead of the
                    # stream (ADR 0019): no session is decided before it.
                    log.info("ibkr stream input %s waits behind the reconciliation still to be read", entry["id"])
                    return
                if batch_run is not None and self.reports.awaiting_commission():
                    # An execution IBKR reported awaits its commission, so its
                    # fill is not recorded yet: no stream input goes ahead of
                    # it (ADR 0019: no decision on a book behind a report
                    # received). The next steps keep reading IB Gateway, and
                    # check_overdue stops the adapter for a person once the
                    # commission is commission_wait late.
                    log.info("ibkr stream input %s waits: an execution awaits its commission, so its fill is "
                             "not recorded yet", entry["id"])
                    return
                # Recorded before it is first sent, so a restart tells this
                # run's input from an earlier run's.
                self.pending.record_run(entry["id"], run_identity(self._engine_log_path),
                                        os.path.basename(self._engine_log_path))
                self._exchange(self._envelope(entry, engine_log))
        except (NotReconciled, NotConnected) as err:
            # The link went while an order was due: the input stays on disk
            # and is asked for again once the link is back and reconciled.
            log.info("ibkr input held until the link is reconciled: %s", err)
        except BrokerOrdersIncomplete as err:
            if "connection" not in err.missing:
                raise
            # The link went while IBKR's orders were read for an order due:
            # nothing was marked or sent, so the input is held as above.
            log.info("ibkr input held until the link is reconciled: %s", err)
            if "open-orders" in err.missing:
                # IBKR's open-order list carries no request ID, and Gateway
                # restores a link it lost IBKR's servers on as the same
                # socket, where the abandoned list's late end would complete
                # the next read: the link is made again (see the module).
                self.conn.drop("untagged-read-interrupted")

    def _settled(self, entry_id, answered):
        """Tell the delivery an outbox entry is settled: answered by the engine, or struck off as committed."""
        if self.delivery is not None:
            self.delivery.settled(entry_id, answered)

    def _status(self, status):
        log.info("ibkr link %s %s", status.state, status.reason)
