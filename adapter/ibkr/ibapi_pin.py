"""The adapter runs only on the ibapi build deploy/versions.env pins (ADR 0025 point 4, amended 2026-10-18).

`deploy/provision.sh twsapi` fetches IBKR's TWS API installer, verifies its
SHA-256 against the pin, installs the bundled Python client into the
adapter's environment and, only then, writes an install record:

    TWSAPI_VERSION=<the pinned version>
    TWSAPI_SHA256=<the pinned installer's SHA-256>

At start the adapter compares that record with the pin in the versions.env
beside its own code (installed by `provision.sh adapter` from the same
checkout), and the version of the ibapi Python actually imports with the
pinned version, and refuses to start on any difference: code updated to a
new pin without ibapi being installed again, an environment changed after
the record was written or an ibapi installed by hand (a system-wide one
included), an ibapi that does not import, or a record not exactly as the
step writes it (fail closed, AGENTS.md). ADR 0025 point 5: a change of API
version repeats the paper-account test before the pin moves.

The TWS API's version "10.51.01" is ibapi's VERSION {"major": 10, "minor":
51, "micro": 1}: compared as whole numbers.
"""
import importlib
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


def _version_numbers(text):
    return tuple(int(part) for part in text.split("."))


def installed_ibapi_version():
    """(major, minor, micro) of the ibapi this Python imports, else IbapiNotPinned."""
    try:
        ibapi = importlib.import_module("ibapi")
    except ImportError as err:
        raise IbapiNotPinned("ibapi does not import ({}); install the pinned build with `sudo deploy/provision.sh "
                             "twsapi`".format(type(err).__name__)) from None
    version = getattr(ibapi, "VERSION", None)
    parts = tuple(version.get(k) for k in ("major", "minor", "micro")) if isinstance(version, dict) else ()
    if len(parts) != 3 or not all(isinstance(p, int) and not isinstance(p, bool) for p in parts):
        raise IbapiNotPinned("the ibapi this Python imports names no version (ibapi.VERSION); install the pinned "
                             "build with `sudo deploy/provision.sh twsapi`")
    return parts


def check_installed(record_path, versions_path, installed_version=installed_ibapi_version):
    """Return the pinned version if exactly that build is recorded as installed and imports, else raise IbapiNotPinned.

    The record holds the two pin keys and nothing else, each once.
    installed_version returns the imported ibapi's (major, minor, micro);
    by default the ibapi this Python imports.
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
    imported = installed_version()
    if tuple(imported) != _version_numbers(pinned[0]):
        raise IbapiNotPinned("the ibapi this Python imports is {}, but the pin is TWS API {}; install the pinned "
                             "build with `sudo deploy/provision.sh twsapi` before starting".format(
                                 ".".join(map(str, imported)), pinned[0]))
    return pinned[0]
