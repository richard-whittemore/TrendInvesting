"""The adapter runs only on the ibapi build deploy/versions.env pins (ADR 0025 point 4, amended 2026-10-18).

`deploy/provision.sh twsapi` fetches IBKR's TWS API installer, verifies its
SHA-256 against the pin, installs the bundled Python client into the
adapter's environment and, only then, writes an install record:

    TWSAPI_VERSION=<the pinned version>
    TWSAPI_SHA256=<the pinned installer's SHA-256>

At start the adapter compares that record with the pin in the versions.env
beside its own code (installed by `provision.sh adapter` from the same
checkout) and refuses to start on any difference: code updated to a new pin
without ibapi being installed again, an ibapi installed by hand, or a record
not exactly as the step writes it (fail closed, AGENTS.md). ADR 0025 point 5:
a change of API version repeats the paper-account test before the pin moves.
"""
import re

PIN_KEYS = ("TWSAPI_VERSION", "TWSAPI_SHA256")
_LINE = re.compile(r"([A-Z0-9_]+)=(.*)")
_VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
_SHA256 = re.compile(r"[0-9a-f]{64}")


class IbapiNotPinned(Exception):
    """The installed ibapi is not the pinned build, or nothing records that it is; the adapter does not start."""


def _read(path, what):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except OSError as err:
        raise IbapiNotPinned("cannot read {} {}: {}; install ibapi with `sudo deploy/provision.sh twsapi` "
                             "(docs/manual/paper-trading.md)".format(what, path, err.strerror or err)) from None


def _pin_values(text, path, what):
    values = {}
    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        match = _LINE.fullmatch(line)
        if match is None:
            raise IbapiNotPinned("{} {} has a line that is not KEY=VALUE".format(what, path))
        key, value = match.groups()
        if key in values:
            raise IbapiNotPinned("{} {} names {} twice".format(what, path, key))
        values[key] = value
    for key, shape in zip(PIN_KEYS, (_VERSION, _SHA256)):
        if key not in values or not shape.fullmatch(values[key]):
            raise IbapiNotPinned("{} {} carries no {} in the form the pin takes".format(what, path, key))
    return values


def read_pin(versions_path):
    """(version, SHA-256) of the TWS API build pinned in versions_path (deploy/versions.env), else IbapiNotPinned."""
    values = _pin_values(_read(versions_path, "the pin file"), versions_path, "the pin file")
    return values["TWSAPI_VERSION"], values["TWSAPI_SHA256"]


def check_installed(record_path, versions_path):
    """Return the pinned version if record_path records installing exactly that build, else raise IbapiNotPinned.

    The record holds the two pin keys and nothing else, each once.
    """
    pinned = read_pin(versions_path)
    text = _read(record_path, "the ibapi install record")
    values = _pin_values(text, record_path, "the ibapi install record")
    if set(values) != set(PIN_KEYS):
        raise IbapiNotPinned("the ibapi install record {} carries {}, not exactly {}".format(
            record_path, ", ".join(sorted(values)), ", ".join(PIN_KEYS)))
    installed = (values["TWSAPI_VERSION"], values["TWSAPI_SHA256"])
    if installed != pinned:
        raise IbapiNotPinned("the installed ibapi is TWS API {} (installer {}), but the pin is {} ({}); install the "
                             "pinned build with `sudo deploy/provision.sh twsapi` before starting".format(
                                 installed[0], installed[1][:12], pinned[0], pinned[1][:12]))
    return pinned[0]
