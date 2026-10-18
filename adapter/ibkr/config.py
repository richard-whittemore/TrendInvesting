"""The IBKR adapter's connection configuration (ADR 0025; ADR 0026 point 8)."""
import math
from collections import namedtuple

MODES = ("dry-run", "paper", "live")
# Every IBKR paper trading account ID begins "DU", and no live one does
# (ADR 0025 point 3; the engine's event.PaperAccountPrefix).
PAPER_ACCOUNT_PREFIX = "DU"

_FIELDS = (
    "host", "port", "client_id", "account_id", "mode",
    "connect_timeout", "request_timeout",
    "heartbeat_interval", "heartbeat_timeout", "restore_timeout",
    "backoff_initial", "backoff_max", "max_connect_attempts",
)

# Any key containing one of these is refused outright. The adapter never
# handles the IBKR password: IBC holds it on the server (ADR 0025 point 6).
_CREDENTIAL_WORDS = ("password", "passwd", "secret", "token", "api_key", "apikey", "credential")


class ConfigError(ValueError):
    """The configuration is unusable; the adapter must not start."""


class AdapterConfig(namedtuple("AdapterConfig", _FIELDS)):
    """Where one engine's adapter connects, and its fail-closed timing bounds.

    One engine per account, each with its own client ID to one IB Gateway
    (ADR 0026 point 8, decided 2026-10-16). Every timing value is required:
    ADR 0019 makes them versioned operational configuration with no
    permissive defaults. restore_timeout is how long Gateway may stay cut
    off from IBKR's servers (codes 1100/2110) before the adapter gives up
    waiting for Gateway's own restoration and reconnects.

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
        missing = [f for f in _FIELDS if f not in values]
        if missing:
            raise ConfigError("missing required keys: {}".format(", ".join(missing)))
        cfg = cls(**{f: values[f] for f in _FIELDS})
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
                     "heartbeat_timeout", "restore_timeout", "backoff_initial", "backoff_max"):
            value = getattr(self, name)
            # NaN and infinity pass "> 0" but would switch a deadline off.
            if not _is_number(value) or not math.isfinite(value) or value <= 0:
                raise ConfigError("{} must be a positive number of seconds".format(name))
        if self.backoff_max < self.backoff_initial:
            raise ConfigError("backoff_max must be at least backoff_initial")
        if not _is_int(self.max_connect_attempts) or self.max_connect_attempts < 1:
            raise ConfigError("max_connect_attempts must be a positive integer")


def is_paper_account(account_id):
    """Whether account_id is an IBKR paper trading account's ID."""
    return isinstance(account_id, str) and account_id.startswith(PAPER_ACCOUNT_PREFIX)


def _is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)
