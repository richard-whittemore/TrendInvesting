"""Map LEAN bars and portfolio readings to events (ADRs 0004 and 0020)."""
import hashlib
import json
from datetime import datetime
from math import isfinite

# event.AccountSnapshotSchemaVersion (internal/event/account.go); ADR 0015.
ACCOUNT_SNAPSHOT_SCHEMA_VERSION = 2
# event.RunCompletedSchemaVersion (internal/event/run_completed.go); ADR 0015.
RUN_COMPLETED_SCHEMA_VERSION = 1


def raw_view(history, end_time):
    """Require exactly the matching raw bar; never substitute an earlier close."""
    if history is None or history.empty or len(history) != 1:
        raise ValueError("raw History must contain exactly one completed bar")
    if history.index[0][-1] != end_time:
        raise ValueError("raw History period does not match subscription bar")
    row = history.iloc[0]
    return dict(view="raw", **{key: float(row[key]) for key in
                              ("open", "high", "low", "close", "volume")})


class Publisher:
    """Continue cmd/engine's input stream after configuration Sequence 1."""
    def __init__(self, client, configuration_hash, strategy_version, run_id):
        if not all((configuration_hash, strategy_version, run_id)):
            raise ValueError("run identity is required")
        self.client = client
        self.configuration_hash = configuration_hash
        self.strategy_version = strategy_version
        self.run_id = run_id
        self.sequence = 1
        self.last_end = None
        self.last_as_of = None
        self.last_event_time = None
        self.completed = False

    def publish(self, instrument, bar, raw, period_end):
        if self.last_end is not None and bar.EndTime <= self.last_end:
            raise ValueError("duplicate or out-of-order completed bar")
        payload = {
            "instrument_id": instrument, "period_end": period_end,
            "split_adjusted": dict(view="split-adjusted", **{
                key.lower(): float(getattr(bar, key)) for key in
                ("Open", "High", "Low", "Close", "Volume")}),
            "raw": raw,
        }
        decisions = self._publish("market.bar.completed", 1, "bar", payload, period_end)
        self.last_end = bar.EndTime
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
