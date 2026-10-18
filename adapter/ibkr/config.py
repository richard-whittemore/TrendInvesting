"""The IBKR adapter's connection configuration (ADR 0025; ADR 0026 point 8)."""
import math
from collections import namedtuple
from collections.abc import Mapping
from types import MappingProxyType

MODES = ("dry-run", "paper", "live")
# Every IBKR paper trading account ID begins "DU", and no live one does
# (ADR 0025 point 3; the engine's event.PaperAccountPrefix).
PAPER_ACCOUNT_PREFIX = "DU"

_FIELDS = (
    "host", "port", "client_id", "account_id", "mode",
    "connect_timeout", "request_timeout",
    "heartbeat_interval", "heartbeat_timeout", "restore_timeout",
    "backoff_initial", "backoff_max", "max_connect_attempts", "commission_wait",
    "con_ids", "paper_sending_enabled", "mark_timeout",
)
# The recommended mark_timeout, as the deployed example sets it: IBKR ends a
# market-data snapshot only at the end of its 11-second snapshot window (TWS
# API "Market Data: Snapshot", tickSnapshotEnd), so the wait outlasts it. A
# configuration must still set it (ADR 0019): there is no default.
MARK_TIMEOUT = 15

# The keys a configuration may leave out, with the value an omission means.
# paper_sending_enabled is the switch that lets the real IBKR client place
# an order (real_client.IbapiClient.can_place_orders): left out, it is off.
_OPTIONAL = {"paper_sending_enabled": False}

# Any key containing one of these is refused outright. The adapter never
# handles the IBKR password: IBC holds it on the server (ADR 0025 point 6).
_CREDENTIAL_WORDS = ("password", "passwd", "secret", "token", "api_key", "apikey", "credential")


class ConfigError(ValueError):
    """The configuration is unusable; the adapter must not start."""


class AdapterConfig(namedtuple("AdapterConfig", _FIELDS)):
    """Where one engine's adapter connects, and its fail-closed timing bounds.

    One engine per account, each with its own client ID to one IB Gateway
    (ADR 0026 point 8, decided 2026-10-16). Every timing value is
    required: ADR 0019 makes them versioned operational configuration with
    no permissive defaults. restore_timeout is how long Gateway may stay cut
    off from IBKR's servers (codes 1100/2110) before the adapter gives up
    waiting for Gateway's own restoration and reconnects. commission_wait
    is how long an execution may wait for IBKR's commission report, without
    which its fill cannot be recorded, before the adapter stops for a
    person (reports.OrderReportJournal). con_ids is the contract map: the
    engine's instrument ID to IBKR's contract ID (conId), the only way the
    adapter names a contract (sender.release_to_request), never a lookup by
    name; no two instruments may share a contract ID; it is kept as a
    read-only copy. paper_sending_enabled is the switch that lets the real
    IBKR client place an order (AGENTS.md rule 5; real_client.IbapiClient.
    can_place_orders): optional, off when left out, strictly a bool, and
    refused on any mode but paper. mark_timeout is how long the marks wait
    for IBKR to end its market-data snapshots (snapshot.read_marks), apart
    from request_timeout, which bounds the account reads: IBKR ends a
    snapshot only at the end of its 11-second window, so MARK_TIMEOUT (15
    seconds) is the recommended value; a value at or under 11 seconds can give no mark for a price IBKR did send.

    Invariant: holds no credential. from_mapping refuses any credential-like
    key and never echoes a refused value, so neither this object's repr nor
    an error built from it can leak one into a log.
    """

    __slots__ = ()

    @classmethod
    def from_mapping(cls, values):
        for key in values:
            lowered = str(key).lower()
            if any(word in lowered for word in _CREDENTIAL_WORDS):
                raise ConfigError("refused key {!r}: the adapter never holds a credential; "
                                  "IBC holds the IBKR login".format(key))
        unknown = sorted(set(values) - set(_FIELDS))
        if unknown:
            raise ConfigError("unknown keys: {}".format(", ".join(map(str, unknown))))
        missing = [f for f in _FIELDS if f not in values and f not in _OPTIONAL]
        if missing:
            raise ConfigError("missing required keys: {}".format(", ".join(missing)))
        fields = {f: values.get(f, _OPTIONAL.get(f)) for f in _FIELDS}
        fields["con_ids"] = _contract_map(values["con_ids"])
        cfg = cls(**fields)
        cfg._validate()
        return cfg

    def _validate(self):
        if not isinstance(self.host, str) or not self.host:
            raise ConfigError("host must be a non-empty string")
        if not _is_int(self.port) or not 1 <= self.port <= 65535:
            raise ConfigError("port must be an integer from 1 to 65535")
        # Client ID 0 is IBKR's master binding: it can see and take over
        # orders placed by hand in TWS, which no engine should own.
        if not _is_int(self.client_id) or self.client_id < 1:
            raise ConfigError("client_id must be a positive integer")
        if not isinstance(self.account_id, str) or not self.account_id:
            raise ConfigError("account_id must be a non-empty string")
        if self.mode not in MODES:
            raise ConfigError("mode must be one of {}".format(", ".join(MODES)))
        # The account is bound to the mode, as the engine binds it: paper
        # names a paper account and live does not, so a paper adapter can
        # never send to a live account. A dry-run sends nothing.
        paper = is_paper_account(self.account_id)
        if self.mode == "paper" and not paper:
            raise ConfigError("mode paper names account {!r}, but an IBKR paper account ID begins {!r}".format(
                self.account_id, PAPER_ACCOUNT_PREFIX))
        if self.mode == "live" and paper:
            raise ConfigError("mode live names account {!r}, but an account ID beginning {!r} is a paper "
                              "account".format(self.account_id, PAPER_ACCOUNT_PREFIX))
        for name in ("connect_timeout", "request_timeout", "heartbeat_interval",
                     "heartbeat_timeout", "restore_timeout", "backoff_initial", "backoff_max",
                     "commission_wait", "mark_timeout"):
            value = getattr(self, name)
            # NaN and infinity pass "> 0" but would switch a deadline off.
            if not _is_number(value) or not math.isfinite(value) or value <= 0:
                raise ConfigError("{} must be a positive number of seconds".format(name))
        if self.backoff_max < self.backoff_initial:
            raise ConfigError("backoff_max must be at least backoff_initial")
        if not _is_int(self.max_connect_attempts) or self.max_connect_attempts < 1:
            raise ConfigError("max_connect_attempts must be a positive integer")
        # Strictly a JSON true or false: a 1 or "yes" is a typo, not a decision.
        if not isinstance(self.paper_sending_enabled, bool):
            raise ConfigError("paper_sending_enabled must be true or false")
        if self.paper_sending_enabled and self.mode != "paper":
            raise ConfigError("paper_sending_enabled is set on a {} configuration; only paper "
                              "sends".format(self.mode))


def _contract_map(value):
    """value as a read-only map from instrument ID to IBKR's positive contract ID, one each, else ConfigError."""
    if not isinstance(value, Mapping):
        raise ConfigError("con_ids must be an object from instrument ID to IBKR contract ID")
    for instrument, con_id in value.items():
        if not isinstance(instrument, str) or not instrument:
            raise ConfigError("con_ids names instrument {!r}; an instrument ID is a non-empty string".format(
                instrument))
        if not _is_int(con_id) or con_id < 1:
            raise ConfigError("con_ids gives {!r} contract ID {!r}; IBKR's contract ID is a positive "
                              "integer".format(instrument, con_id))
    # One contract per instrument: a contract ID shared by two instruments
    # would send one instrument's order on the other's contract.
    named = {}
    for instrument, con_id in value.items():
        if con_id in named:
            raise ConfigError("con_ids gives {!r} and {!r} the same contract ID {}; each instrument names its own "
                              "IBKR contract".format(named[con_id], instrument, con_id))
        named[con_id] = instrument
    return MappingProxyType(dict(value))


def is_paper_account(account_id):
    """Whether account_id is an IBKR paper trading account's ID."""
    return isinstance(account_id, str) and account_id.startswith(PAPER_ACCOUNT_PREFIX)


def _is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)
