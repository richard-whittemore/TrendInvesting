"""Map LEAN bars to the event contract without strategy arithmetic (ADR 0004)."""
import hashlib
import json


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
        encoded = json.dumps(payload, separators=(",", ":"), allow_nan=False).encode()
        sequence = self.sequence + 1
        envelope = {
            "id": "{}:bar:{}".format(self.run_id, sequence),
            "type": "market.bar.completed", "envelope_version": 1,
            "schema_version": 1, "event_time": period_end,
            # A backtest learns of a bar the moment it ends, exactly as
            # cmd/backtest's inputEnvelope records it, so two runs over the
            # same bars write the same inputs. This adapter refuses LiveMode;
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
        self.last_end = bar.EndTime
        return decisions
