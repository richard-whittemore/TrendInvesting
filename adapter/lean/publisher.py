"""Map LEAN bars and portfolio readings to events (ADRs 0004 and 0020)."""
import hashlib
import json
from datetime import datetime
from math import isfinite

# event.AccountSnapshotSchemaVersion (internal/event/account.go); ADR 0015.
ACCOUNT_SNAPSHOT_SCHEMA_VERSION = 2
# event.SessionClosedSchemaVersion (internal/event/session.go); ADR 0015.
SESSION_CLOSED_SCHEMA_VERSION = 1
# event.RunCompletedSchemaVersion (internal/event/run_completed.go); ADR 0015.
RUN_COMPLETED_SCHEMA_VERSION = 1
# event.AdapterRunStoppedSchemaVersion (internal/event/run_stopped.go); ADR 0015.
RUN_STOPPED_SCHEMA_VERSION = 1
# event.FillEventType / FillSchemaVersion (internal/event/fill.go); ADR 0015.
FILL_EVENT_TYPE = "execution.fill"
FILL_SCHEMA_VERSION = 4
# event.OrderLifecycleEventType / OrderLifecycleSchemaVersion
# (internal/event/order_lifecycle.go); ADR 0015.
ORDER_LIFECYCLE_EVENT_TYPE = "execution.order.lifecycle"
ORDER_LIFECYCLE_SCHEMA_VERSION = 1
# event.MarketCorporateActionEventType / MarketCorporateActionSchemaVersion
# (internal/event/corporate_action.go); ADR 0015. Schema 2 carries a split's
# cash in lieu (ADR 0023). Schema 3 adds a symbol-change and a dividend kind
# (ADR 0024); this adapter still publishes only splits (#28 tracks the rest),
# but every corporate action it sends must be labelled with the CURRENT
# schema so a build that no longer knows schema 2 does not refuse it.
CORPORATE_ACTION_EVENT_TYPE = "market.corporate-action"
CORPORATE_ACTION_SCHEMA_VERSION = 3
# event.AdapterRunStoppedReason* (internal/event/run_stopped.go): the closed
# set of reasons this adapter may report, mirrored here so an unrecognised
# reason fails at the source rather than reaching the engine, which would
# reject it anyway (event.AdapterRunStoppedPayload.Validate).
RUN_STOPPED_REASONS = frozenset({"delisted"})


def split_adjusted_view(history, end_time):
    """Require exactly the split-adjusted bar that ends with the subscription
    bar; never substitute an earlier close.

    The subscription is raw, because LEAN trades and accounts in whatever
    view it is subscribed to and ADR 0004 requires that to be raw; the
    split-adjusted view every signal reads is LEAN's own split-adjusted
    History for the same bar, so the adapter does no adjustment arithmetic
    of its own. LEAN's one-bar History can also hold the previous session's
    bar: on an early-close session it returned both 2002-12-23 16:00 and
    2002-12-24 13:00 (observed on the pinned image). The bar is chosen by its
    end time, and anything other than exactly one bar ending then is refused.
    """
    if history is None or history.empty:
        raise ValueError("split-adjusted History must contain the completed bar")
    matches = [i for i, key in enumerate(history.index) if key[-1] == end_time]
    if len(matches) != 1:
        raise ValueError("split-adjusted History must contain exactly one bar ending with the "
                         "subscription bar at {}; it holds {} such bar(s)".format(
                             end_time, len(matches)))
    row = history.iloc[matches[0]]
    return dict(view="split-adjusted", **{key: float(row[key]) for key in
                                         ("open", "high", "low", "close", "volume")})


class Refused(Exception):
    """An input the publisher will not send: the run is stopping (fail closed)."""


class Publisher:
    """Continue cmd/engine's input stream after configuration Sequence 1.

    Every input reaches the engine through _publish, the one chokepoint.
    refusal, if given, is asked there before each input is sent: while it
    returns a reason, no input of any type is sent and Refused is raised for
    the caller to stop the run. The algorithm answers with the adapter's
    ADR 0005 fill or ADR 0013 slippage model's recorded failure, which LEAN
    can record at any point in a slice, whenever it rescans the working
    orders after one is placed or amended.
    """
    def __init__(self, client, configuration_hash, strategy_version, run_id, refusal=None):
        if not all((configuration_hash, strategy_version, run_id)):
            raise ValueError("run identity is required")
        self.client = client
        self.refusal = refusal
        self.configuration_hash = configuration_hash
        self.strategy_version = strategy_version
        self.run_id = run_id
        self.sequence = 1
        self.last_end = None
        self.last_as_of = None
        self.last_event_time = None
        self.completed = False
        # The open Session's period end and the instruments whose bars it
        # holds (ADR 0021); None and empty between Sessions.
        self.session_end = None
        self.session_ids = []

    def publish(self, instrument, bar, split_adjusted, period_end):
        """Send one market.bar.completed: bar is LEAN's raw subscription bar,
        split_adjusted its split_adjusted_view (ADR 0004)."""
        if self.last_end is not None and bar.EndTime <= self.last_end:
            raise ValueError("duplicate or out-of-order completed bar")
        payload = {
            "instrument_id": instrument, "period_end": period_end,
            "split_adjusted": split_adjusted,
            "raw": dict(view="raw", **{
                key.lower(): float(getattr(bar, key)) for key in
                ("Open", "High", "Low", "Close", "Volume")}),
        }
        if self.session_end is not None and period_end != self.session_end:
            raise ValueError("a bar for another period end while a session is open")
        decisions = self._publish("market.bar.completed", 1, "bar", payload, period_end)
        self.last_end = bar.EndTime
        self.session_end = period_end
        self.session_ids.append(instrument)
        return decisions

    def publish_session_closed(self, period_end):
        """End the slice's Session after its bars (ADR 0021).

        Names every instrument whose bar this Session published, sorted, so the
        engine can check it received exactly those bars before it decides the
        day's Adds and entries. Sent before the snapshot: a snapshot as of this
        close is not cash known at the Session's previous close (ADR 0010).
        """
        if self.session_end != period_end:
            raise ValueError("no open session ends at {}".format(period_end))
        payload = {"period_end": period_end, "instrument_ids": sorted(self.session_ids)}
        decisions = self._publish("market.session.closed", SESSION_CLOSED_SCHEMA_VERSION,
                                  "session-closed", payload, period_end)
        self.session_end = None
        self.session_ids = []
        return decisions

    def publish_snapshot(self, portfolio, period_end):
        """Report LEAN's close after its bar reply (ADR 0020's producer amendment).

        period_end is the bar publisher's UTC timestamp; readings must advance
        strictly, as required by event.AccountSnapshotPayload.AsOf.
        """
        as_of = datetime.strptime(period_end, "%Y-%m-%dT%H:%M:%SZ")
        if self.last_as_of is not None and as_of <= self.last_as_of:
            raise ValueError("duplicate or out-of-order account snapshot")
        equity, cash = float(portfolio.TotalPortfolioValue), float(portfolio.Cash)
        # Match event.AccountSnapshotPayload.Validate; never substitute or
        # clamp an invalid account figure (ADR 0015).
        if not isfinite(equity) or equity <= 0:
            raise ValueError("equity must be finite and positive")
        if not isfinite(cash) or cash < 0:
            raise ValueError("available cash must be finite and not negative")
        payload = {"as_of": period_end, "equity": equity,
                   "available_cash": cash, "currency": "USD"}
        decisions = self._publish("account.snapshot", ACCOUNT_SNAPSHOT_SCHEMA_VERSION,
                                  "snapshot", payload, period_end)
        self.last_as_of = as_of
        return decisions

    def publish_fill(self, payload):
        """Report one LEAN execution as execution.fill (event.FillPayload).

        Stamped at the fill's own time: a fact about when LEAN executed the
        order, delivered before the bar of the session it executed in
        (algorithm.py, drain_order_events).
        """
        return self._publish(FILL_EVENT_TYPE, FILL_SCHEMA_VERSION, "fill", payload,
                             payload["filled_at"])

    def publish_corporate_action(self, payload):
        """Report a split LEAN applied to a held position as
        market.corporate-action (event.CorporateActionPayload; ADR 0023),
        stamped at the split's own effective time. The desk derives the
        payload (OrderDesk.apply_split); this adds no figure of its own."""
        return self._publish(CORPORATE_ACTION_EVENT_TYPE, CORPORATE_ACTION_SCHEMA_VERSION,
                             "corporate-action", payload, payload["effective_at"])

    def publish_order_lifecycle(self, payload):
        """Report one LEAN order change that is not an execution
        (event.OrderLifecyclePayload), stamped when LEAN reported it."""
        return self._publish(ORDER_LIFECYCLE_EVENT_TYPE, ORDER_LIFECYCLE_SCHEMA_VERSION, "order",
                             payload, payload["occurred_at"])

    def publish_run_stopped(self, reason, detail, instrument_id=None):
        """Send adapter.run.stopped immediately BEFORE replay.run.completed.

        Records that the adapter deliberately stopped this run, so the
        journal can tell it apart from one that simply reached its last bar
        (ADR 0012; internal/event/run_stopped.go). Only called on a
        deliberate stop already in progress (algorithm.py's
        handle_delisting) — a clean end sends no event of this kind, and a
        startup failure before the first exchange with the engine cannot
        send anything at all (README.md).
        """
        if reason not in RUN_STOPPED_REASONS:
            raise ValueError("unrecognised run stop reason: {!r}".format(reason))
        if not detail:
            raise ValueError("detail is required")
        # Matches event.AdapterRunStoppedPayload.Validate's own per-reason
        # requirement: today's one reason, "delisted", always names an
        # instrument.
        if reason == "delisted" and not instrument_id:
            raise ValueError('instrument id is required for reason "delisted"')
        if self.last_event_time is None:
            raise ValueError("a run with no inputs has nothing to stop")
        payload = {"reason": reason, "instrument_id": instrument_id or "", "detail": detail}
        return self._publish("adapter.run.stopped", RUN_STOPPED_SCHEMA_VERSION,
                              "run-stopped", payload, self.last_event_time)

    def publish_run_completed(self):
        """End the input stream the way cmd/backtest ends a run.

        replay.run.completed tells the reducer no further input exists, so it
        expires every proposal still outstanding and each reaches exactly one
        terminal event (event.RunCompletedEventType). It is stamped with the
        last input's own time, as cmd/backtest's drive stamps it with the last
        bar's period end, so the expiries fall inside the journal's span.
        Nothing may follow it.
        """
        if self.completed:
            raise ValueError("the run's input stream has already been completed")
        if self.last_event_time is None:
            raise ValueError("a run with no inputs has nothing to complete")
        decisions = self._publish("replay.run.completed", RUN_COMPLETED_SCHEMA_VERSION,
                                  "run-completed", {}, self.last_event_time)
        self.completed = True
        return decisions

    def _publish(self, event_type, schema_version, id_kind, payload, period_end):
        reason = self.refusal() if self.refusal is not None else None
        if reason is not None:
            raise Refused("{} not sent: {}".format(event_type, reason))
        if self.completed:
            raise ValueError("no input may follow the run's completion")
        encoded = json.dumps(payload, separators=(",", ":"), allow_nan=False).encode()
        sequence = self.sequence + 1
        envelope = {
            "id": "{}:{}:{}".format(self.run_id, id_kind, sequence),
            "type": event_type, "envelope_version": 1,
            "schema_version": schema_version, "event_time": period_end,
            # A backtest records bars and portfolio readings at the close, as
            # cmd/backtest's inputEnvelope records it, so two runs over the
            # same data write the same inputs. This adapter refuses LiveMode;
            # a live producer records when it actually received the data.
            "recorded_at": period_end,
            "sequence": sequence, "correlation_id": self.run_id,
            "source": "lean-adapter", "strategy_version": self.strategy_version,
            "configuration_hash": self.configuration_hash,
            "payload_hash": hashlib.sha256(encoded).hexdigest(), "payload": payload,
        }
        reply = self.client.decide(envelope)
        expected = {"type": "engine.decisions", "envelope_version": 1,
                    "schema_version": 1, "sequence": sequence,
                    "causation_id": envelope["id"],
                    "correlation_id": self.run_id,
                    "configuration_hash": self.configuration_hash,
                    "strategy_version": self.strategy_version}
        if any(reply.get(k) != v for k, v in expected.items()):
            raise ValueError("unexpected engine decision envelope or run identity")
        decisions = reply.get("payload", {}).get("decisions")
        if not isinstance(decisions, list):
            raise ValueError("engine decisions must be an array")
        self.sequence = sequence
        self.last_event_time = period_end
        return decisions
