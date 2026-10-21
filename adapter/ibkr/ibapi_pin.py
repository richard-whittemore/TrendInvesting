"""The adapter runs only on the ibapi build deploy/versions.env pins (ADR 0025 point 4, amended 2026-10-18).

`deploy/provision.sh twsapi` fetches the pinned TWS API file (IBKR's Linux
installer on x86_64, the Mac/Unix archive on arm64), verifies its SHA-256
against the pin, installs the pinned protobuf wheel (which ibapi
imports as google.protobuf; docs/dependency-policy.md) and the bundled Python
client into the adapter's environment and, only then, writes an install
record:

    TWSAPI_VERSION=<the pinned version>
    TWSAPI_SHA256=<the pinned file's SHA-256: the installer's, or on arm64 the Mac/Unix archive's>
    TWSAPI_ARTEFACT=<installer, or archive: which pinned file that SHA-256 is>
    PROTOBUF_VERSION=<the pinned protobuf version>
    PROTOBUF_SHA256=<the pinned protobuf wheel's SHA-256>

At start the adapter compares that record with the pin in the versions.env
beside its own code (installed by `provision.sh adapter` from the same
checkout), and the version of the ibapi Python actually imports with the
pinned version, and requires that ibapi, and the google.protobuf it
imports at the pinned protobuf version, to be the ones in the venv the
adapter runs in (their files under sys.prefix), and refuses to start on any
difference: code updated to a new pin without ibapi being installed again,
an environment changed after the record was written, an ibapi from outside
the venv (a system-wide or hand-installed one, whatever version it
reports), a Python that is not running in a venv, an ibapi that does not
import, or a record not exactly as the step writes it (fail closed,
AGENTS.md). The files inside the venv are not hashed: the record attests
the pinned TWS API file (the installer, or on arm64 the Mac/Unix archive) and the wheel, and the venv is root's, rebuilt whole by that step. ADR 0025 point 5: a change of API
version repeats the paper-account test before the pin moves.

The TWS API's version "10.51.01" is ibapi's VERSION {"major": 1051, "minor":
1} (the first two parts joined as hundreds, then the third), as the real
client reports it: compared as whole numbers.
"""
import importlib
import os
import platform
import re
import sys

PIN_KEYS = ("TWSAPI_VERSION", "TWSAPI_SHA256", "PROTOBUF_VERSION", "PROTOBUF_SHA256")
# The install record also names which pinned file its TWSAPI_SHA256 is.
ARTEFACT_KEY = "TWSAPI_ARTEFACT"
ARTEFACTS = ("installer", "archive")
ARCHIVE_KEY = "TWSAPI_ARCHIVE_SHA256"
_LINE = re.compile(r"([A-Z0-9_]+)=(.*)")
_VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
_SHAPES = (_VERSION, re.compile(r"[0-9a-f]{64}"), _VERSION, re.compile(r"[0-9a-f]{64}"))


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
    for key, shape in zip(PIN_KEYS, _SHAPES):
        if key not in values or not shape.fullmatch(values[key]):
            raise IbapiNotPinned("{} {} carries no {} in the form the pin takes".format(what, path, key))
    if ARCHIVE_KEY in values and not _SHAPES[1].fullmatch(values[ARCHIVE_KEY]):
        raise IbapiNotPinned("{} {} carries a {} not in the form the pin takes".format(what, path, ARCHIVE_KEY))
    if ARTEFACT_KEY in values and values[ARTEFACT_KEY] not in ARTEFACTS:
        raise IbapiNotPinned("{} {} carries a {} that is neither {}".format(
            what, path, ARTEFACT_KEY, " nor ".join(ARTEFACTS)))
    return values


def _machine_artefact():
    """The artefact this machine installs from (deploy/versions.env): the Linux installer is x86_64 only and
    the Mac/Unix archive serves arm64; any other machine has none (fail closed, AGENTS.md)."""
    machine = platform.machine()
    if machine.lower() in ("x86_64", "amd64"):
        return machine, "installer"
    if machine.lower() in ("arm64", "aarch64"):
        return machine, "archive"
    return machine, None


def read_pin(versions_path):
    """The pins in versions_path (deploy/versions.env), in PIN_KEYS order: the TWS API's version and SHA-256, then
    protobuf's, else IbapiNotPinned."""
    values = _pin_values(_read(versions_path, "the pin file"), versions_path, "the pin file")
    return tuple(values[key] for key in PIN_KEYS)


def _archive_pin(versions_path):
    """The SHA-256 of the Mac/Unix archive pinned for the same TWS API version (arm64), or None if none is pinned."""
    values = _pin_values(_read(versions_path, "the pin file"), versions_path, "the pin file")
    return values.get(ARCHIVE_KEY)


def _version_numbers(text):
    return tuple(int(part) for part in text.split("."))


def _require_own_environment(module, name="ibapi"):
    """Refuse an ibapi (or protobuf) that is not the adapter's venv's own: the version alone does not say which files run.

    The venv is built with --system-site-packages (deploy/provision.sh), so
    the system's site packages follow its own on sys.path, and a system-wide
    or hand-installed ibapi that reports the pinned version would otherwise
    pass. The Python must run in a venv (sys.prefix is not sys.base_prefix)
    and the module's file must lie under that venv.
    """
    prefix = os.path.realpath(sys.prefix)
    if prefix == os.path.realpath(sys.base_prefix):
        raise IbapiNotPinned("this Python ({}) is not the adapter's environment; run the adapter with the venv "
                             "`sudo deploy/provision.sh twsapi` creates".format(sys.prefix))
    path = getattr(module, "__file__", None)
    if not isinstance(path, str) or not os.path.isabs(path) or \
            os.path.commonpath((prefix, os.path.realpath(path))) != prefix:
        raise IbapiNotPinned("the {} this Python imports ({}) is not the one installed in its environment {}; "
                             "install the pinned build with `sudo deploy/provision.sh twsapi`".format(name, path, prefix))


def installed_ibapi_version():
    """(major, minor, micro) of the TWS API this Python's ibapi reports, from its own venv, else IbapiNotPinned.

    ibapi.VERSION is {"major": 1051, "minor": 1} for 10.51.01.
    """
    try:
        ibapi = importlib.import_module("ibapi")
    except ImportError as err:
        raise IbapiNotPinned("ibapi does not import ({}); install the pinned build with `sudo deploy/provision.sh "
                             "twsapi`".format(type(err).__name__)) from None
    _require_own_environment(ibapi)
    version = getattr(ibapi, "VERSION", None)
    parts = tuple(version.get(k) for k in ("major", "minor")) if isinstance(version, dict) and set(version) == {"major", "minor"} else ()
    if len(parts) != 2 or not all(isinstance(p, int) and not isinstance(p, bool) for p in parts) or \
            parts[0] < 100 or parts[1] < 0:
        raise IbapiNotPinned("the ibapi this Python imports names no version (ibapi.VERSION); install the pinned "
                             "build with `sudo deploy/provision.sh twsapi`")
    return parts[0] // 100, parts[0] % 100, parts[1]


def installed_protobuf_version():
    """The version string of the google.protobuf this Python imports from its own venv, else IbapiNotPinned."""
    try:
        protobuf = importlib.import_module("google.protobuf")
    except ImportError as err:
        raise IbapiNotPinned("protobuf does not import ({}); install the pinned build with `sudo deploy/provision.sh "
                             "twsapi`".format(type(err).__name__)) from None
    _require_own_environment(protobuf, "protobuf")
    version = getattr(protobuf, "__version__", None)
    if not isinstance(version, str):
        raise IbapiNotPinned("the protobuf this Python imports names no version; install the pinned build with "
                             "`sudo deploy/provision.sh twsapi`")
    return version


def check_installed(record_path, versions_path, installed_version=installed_ibapi_version,
                    installed_protobuf=installed_protobuf_version):
    """Return the pinned version if exactly that build is recorded as installed and imports, else raise IbapiNotPinned.

    The record holds the four pin keys (PIN_KEYS) and TWSAPI_ARTEFACT, each
    once and nothing else. Its artefact must be the one this machine
    installs from (installer on x86_64, archive on arm64) and its
    TWSAPI_SHA256 that artefact's pin.
    installed_version returns the imported ibapi's (major, minor, micro) and
    installed_protobuf the imported protobuf's version string; by default
    those this Python imports.
    """
    pinned = read_pin(versions_path)
    archive = _archive_pin(versions_path)
    text = _read(record_path, "the ibapi install record")
    values = _pin_values(text, record_path, "the ibapi install record")
    if set(values) == set(PIN_KEYS):
        raise IbapiNotPinned("the ibapi install record {} does not name the artefact it was installed from "
                             "({}); reinstall the pinned build with `sudo deploy/provision.sh twsapi` before "
                             "starting".format(record_path, ARTEFACT_KEY))
    if set(values) != set(PIN_KEYS) | {ARTEFACT_KEY}:
        raise IbapiNotPinned("the ibapi install record {} carries {}, not exactly {}".format(
            record_path, ", ".join(sorted(values)), ", ".join(PIN_KEYS + (ARTEFACT_KEY,))))
    machine, wanted = _machine_artefact()
    if wanted is None:
        raise IbapiNotPinned("no pinned TWS API file serves a {} machine; the adapter runs on x86_64 (installer) "
                             "or arm64 (archive) only".format(machine))
    artefact = values[ARTEFACT_KEY]
    if artefact != wanted:
        raise IbapiNotPinned("the ibapi install record {} was written from the {}, but a {} machine installs from "
                             "the {}; install the pinned build here with `sudo deploy/provision.sh twsapi` "
                             "before starting".format(record_path, artefact, machine, wanted))
    file_pin = pinned[1] if artefact == "installer" else archive
    installed = tuple(values[key] for key in PIN_KEYS)
    expected = (pinned[0], file_pin, pinned[2], pinned[3])
    if installed != expected:
        raise IbapiNotPinned("the installed ibapi is TWS API {} ({} {}) with protobuf {} ({}), but the pin is "
                             "{} ({} {}) with protobuf {} ({}); install the pinned build with `sudo "
                             "deploy/provision.sh twsapi` before starting".format(
                                 installed[0], artefact, installed[1][:12], installed[2], installed[3][:12],
                                 pinned[0], artefact, (file_pin or "not pinned")[:12], pinned[2], pinned[3][:12]))
    imported = installed_version()
    if tuple(imported) != _version_numbers(pinned[0]):
        raise IbapiNotPinned("the ibapi this Python imports is {}, but the pin is TWS API {}; install the pinned "
                             "build with `sudo deploy/provision.sh twsapi` before starting".format(
                                 ".".join(map(str, imported)), pinned[0]))
    protobuf = installed_protobuf()
    if protobuf != pinned[2]:
        raise IbapiNotPinned("the protobuf this Python imports is {}, but the pin is {}; install the pinned build "
                             "with `sudo deploy/provision.sh twsapi` before starting".format(protobuf, pinned[2]))
    return pinned[0]
