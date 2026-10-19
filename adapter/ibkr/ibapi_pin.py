"""The adapter runs only on the ibapi build deploy/versions.env pins (ADR 0025 point 4, amended 2026-10-18).

`deploy/provision.sh twsapi` fetches IBKR's TWS API installer (on x86_64)
or the same release's Mac/Unix archive (on arm64, where the x86_64-only
installer cannot run; owner decision 2026-10-18), verifies its SHA-256
against the pin, installs the bundled Python client into the adapter's
environment and, only then, writes an install record:

    TWSAPI_VERSION=<the pinned version>
    TWSAPI_SHA256=<the SHA-256 of the pinned file it installed from>

The pin file pins the installer as TWSAPI_SHA256 and the archive as
TWSAPI_ARCHIVE_SHA256, both of TWSAPI_VERSION; a record naming either is the
pinned build.

At start the adapter compares that record with the pin in the versions.env
beside its own code (installed by `provision.sh adapter` from the same
checkout), and the version of the ibapi Python actually imports with the
pinned version, and requires that ibapi to be the one in the venv the
adapter runs in (its file under sys.prefix), and refuses to start on any
difference: code updated to a new pin without ibapi being installed again,
an environment changed after the record was written, an ibapi from outside
the venv (a system-wide or hand-installed one, whatever version it
reports), a Python that is not running in a venv, an ibapi that does not
import, or a record not exactly as the step writes it (fail closed,
AGENTS.md). The files inside the venv are not hashed: the record attests
the installer, and the venv is root's, rebuilt whole by that step. ADR 0025 point 5: a change of API
version repeats the paper-account test before the pin moves.

The TWS API's version "10.51.01" is compared with ibapi's VERSION as whole
numbers, (10, 51, 1). The pinned release states it as {"major": 1051,
"minor": 1} (its ibapi/__init__.py; API_VersionNum.txt says 10.51.01): the
first two parts run together as a four-digit major, the third as minor.
The older shape {"major": 10, "minor": 51, "micro": 1} is read as it says.
Any other shape names no version.
"""
import importlib
import os
import re
import sys

PIN_KEYS = ("TWSAPI_VERSION", "TWSAPI_SHA256")
ARCHIVE_KEY = "TWSAPI_ARCHIVE_SHA256"
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
    if ARCHIVE_KEY in values and not _SHA256.fullmatch(values[ARCHIVE_KEY]):
        raise IbapiNotPinned("{} {} carries a {} not in the form the pin takes".format(what, path, ARCHIVE_KEY))
    return values


def read_pin(versions_path):
    """(version, SHA-256) of the TWS API build pinned in versions_path (deploy/versions.env), else IbapiNotPinned."""
    values = _pin_values(_read(versions_path, "the pin file"), versions_path, "the pin file")
    return values["TWSAPI_VERSION"], values["TWSAPI_SHA256"]


def _pinned_files(versions_path):
    """(version, the SHA-256s of the files pinned for it): the installer's, and the archive's if pinned."""
    values = _pin_values(_read(versions_path, "the pin file"), versions_path, "the pin file")
    shas = (values["TWSAPI_SHA256"],) + ((values[ARCHIVE_KEY],) if ARCHIVE_KEY in values else ())
    return values["TWSAPI_VERSION"], shas


def _whole_number(value):
    return isinstance(value, int) and not isinstance(value, bool)


def _version_parts(version):
    """(major, minor, micro) from ibapi.VERSION in either shape the module docstring names, else ()."""
    if not isinstance(version, dict) or not all(_whole_number(v) for v in version.values()):
        return ()
    if set(version) == {"major", "minor", "micro"}:
        return version["major"], version["minor"], version["micro"]
    if set(version) == {"major", "minor"} and 1000 <= version["major"] <= 9999:
        return version["major"] // 100, version["major"] % 100, version["minor"]
    return ()


def _version_numbers(text):
    return tuple(int(part) for part in text.split("."))


def _require_own_environment(ibapi):
    """Refuse an ibapi that is not the adapter's venv's own: the version alone does not say which files run.

    The venv is built with --system-site-packages (deploy/provision.sh), so
    the system's site packages follow its own on sys.path, and a system-wide
    or hand-installed ibapi that reports the pinned version would otherwise
    pass. The Python must run in a venv (sys.prefix is not sys.base_prefix)
    and ibapi's file must lie under that venv.
    """
    prefix = os.path.realpath(sys.prefix)
    if prefix == os.path.realpath(sys.base_prefix):
        raise IbapiNotPinned("this Python ({}) is not the adapter's environment; run the adapter with the venv "
                             "`sudo deploy/provision.sh twsapi` creates".format(sys.prefix))
    path = getattr(ibapi, "__file__", None)
    if not isinstance(path, str) or not os.path.isabs(path) or \
            os.path.commonpath((prefix, os.path.realpath(path))) != prefix:
        raise IbapiNotPinned("the ibapi this Python imports ({}) is not the one installed in its environment {}; "
                             "install the pinned build with `sudo deploy/provision.sh twsapi`".format(path, prefix))


def installed_ibapi_version():
    """(major, minor, micro) of the ibapi this Python imports from its own venv, else IbapiNotPinned."""
    try:
        ibapi = importlib.import_module("ibapi")
    except ImportError as err:
        raise IbapiNotPinned("ibapi does not import ({}); install the pinned build with `sudo deploy/provision.sh "
                             "twsapi`".format(type(err).__name__)) from None
    _require_own_environment(ibapi)
    parts = _version_parts(getattr(ibapi, "VERSION", None))
    if not parts:
        raise IbapiNotPinned("the ibapi this Python imports names no version (ibapi.VERSION); install the pinned "
                             "build with `sudo deploy/provision.sh twsapi`")
    return parts


def check_installed(record_path, versions_path, installed_version=installed_ibapi_version):
    """Return the pinned version if exactly that build is recorded as installed and imports, else raise IbapiNotPinned.

    The record holds the two pin keys and nothing else, each once.
    installed_version returns the imported ibapi's (major, minor, micro);
    by default the ibapi this Python imports.
    """
    version, shas = _pinned_files(versions_path)
    text = _read(record_path, "the ibapi install record")
    values = _pin_values(text, record_path, "the ibapi install record")
    if set(values) != set(PIN_KEYS):
        raise IbapiNotPinned("the ibapi install record {} carries {}, not exactly {}".format(
            record_path, ", ".join(sorted(values)), ", ".join(PIN_KEYS)))
    installed = (values["TWSAPI_VERSION"], values["TWSAPI_SHA256"])
    if installed[0] != version or installed[1] not in shas:
        raise IbapiNotPinned("the installed ibapi is TWS API {} (file {}), but the pin is {} ({}); install the "
                             "pinned build with `sudo deploy/provision.sh twsapi` before starting".format(
                                 installed[0], installed[1][:12], version, " or ".join(s[:12] for s in shas)))
    imported = installed_version()
    if tuple(imported) != _version_numbers(version):
        raise IbapiNotPinned("the ibapi this Python imports is {}, but the pin is TWS API {}; install the pinned "
                             "build with `sudo deploy/provision.sh twsapi` before starting".format(
                                 ".".join(map(str, imported)), version))
    return version
