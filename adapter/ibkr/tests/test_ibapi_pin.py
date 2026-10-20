"""The adapter starts only on the ibapi build deploy/versions.env pins, as provision.sh twsapi recorded installing it."""
import contextlib
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

from ibapi_pin import IbapiNotPinned, check_installed, read_pin

SHA = "152af944be28bcfaf2733aec907e7724f6dda092cb3148848e84d2a0bb866c0a"
PB_SHA = "6cf42630262c59b2d8de33954443d94b746c952b01434fc58a417fdbd2e84bd5"
PB_VERSION = "5.29.5"
PINS = "PROTOBUF_VERSION={}\nPROTOBUF_SHA256={}\n".format(PB_VERSION, PB_SHA)
# The adapter's environment as the tests see it: a venv over a system Python.
VENV = "/opt/trend-investing/adapter-venv"
SYSTEM = "/usr"
VENV_IBAPI = VENV + "/lib/python3.12/site-packages/ibapi/__init__.py"
VENV_PROTOBUF = VENV + "/lib/python3.12/site-packages/google/protobuf/__init__.py"


def record(version="10.51.01", sha=SHA, protobuf=PB_VERSION, protobuf_sha=PB_SHA):
    """The install record text provision.sh twsapi writes."""
    return "TWSAPI_VERSION={}\nTWSAPI_SHA256={}\nPROTOBUF_VERSION={}\nPROTOBUF_SHA256={}\n".format(
        version, sha, protobuf, protobuf_sha)


def protobuf_stub(version=PB_VERSION, path=VENV_PROTOBUF):
    stub = types.ModuleType("google.protobuf")
    if version is not None:
        stub.__version__ = version
    if path is not None:
        stub.__file__ = path
    return stub


def ibapi_stub(version=(10, 51, 1), path=VENV_IBAPI):
    stub = types.ModuleType("ibapi")
    if version is not None:
        # ibapi 10.51.01 reports VERSION = {"major": 1051, "minor": 1}.
        stub.VERSION = {"major": version[0] * 100 + version[1], "minor": version[2]}
    if path is not None:
        stub.__file__ = path
    return stub


def in_venv(stub, prefix=VENV, base_prefix=SYSTEM, protobuf=None):
    """sys.modules and sys's prefixes as the adapter's unit runs: its venv's Python importing stub as ibapi.

    protobuf is what google.protobuf imports as (the venv's own pinned build by default).
    """
    modules = {"ibapi": stub, "google": types.ModuleType("google"),
               "google.protobuf": protobuf if protobuf is not None else protobuf_stub()}
    patches = [mock.patch.dict(sys.modules, modules), mock.patch.object(sys, "prefix", prefix),
               mock.patch.object(sys, "base_prefix", base_prefix)]
    stack = contextlib.ExitStack()
    for p in patches:
        stack.enter_context(p)
    return stack


class PinTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.versions = Path(self.dir.name) / "versions.env"
        self.versions.write_text("# pins\nIBC_VERSION=3.24.2\nTWSAPI_VERSION=10.51.01\nTWSAPI_URL=https://example.invalid/x.sh\n"
                                 "TWSAPI_SHA256={}\n".format(SHA) + PINS)
        self.record = Path(self.dir.name) / "INSTALLED"

    def write_record(self, text):
        self.record.write_text(text)

    def test_the_shipped_pin_is_read_from_deploy_versions_env(self):
        version, sha, protobuf, protobuf_sha = read_pin(str(repo / "deploy" / "versions.env"))
        self.assertRegex(version, r"^[0-9]+\.[0-9]+\.[0-9]+$")
        self.assertRegex(sha, r"^[0-9a-f]{64}$")
        self.assertEqual((protobuf, protobuf_sha), (PB_VERSION, PB_SHA))

    def test_the_build_recorded_as_installed_is_the_pinned_build(self):
        self.write_record(record())
        self.assertEqual(check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1),
                                         installed_protobuf=lambda: PB_VERSION),
                         "10.51.01")

    def test_the_ibapi_the_adapter_imports_must_be_the_pinned_version(self):
        # The record alone does not prove what Python imports: a venv changed
        # after the record was written, or a system-wide ibapi, is refused.
        self.write_record(record())
        for imported in ((10, 50, 2), (10, 51, 0), (9, 51, 1)):
            with self.subTest(imported=imported), self.assertRaises(IbapiNotPinned) as caught:
                check_installed(str(self.record), str(self.versions), installed_version=lambda: imported,
                              installed_protobuf=lambda: PB_VERSION)
            self.assertIn("10.51.01", str(caught.exception))
        with in_venv(ibapi_stub()):
            self.assertEqual(check_installed(str(self.record), str(self.versions)), "10.51.01")
        with in_venv(ibapi_stub((10, 50, 2))), self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))

    def test_the_protobuf_the_adapter_imports_must_be_the_pinned_version(self):
        # ibapi 10.51.01 imports google.protobuf and needs the pinned build:
        # the record alone, or another version in the venv, is refused.
        self.write_record(record())
        for imported in ("3.21.12", "5.29.4", "6.31.1", "5.29.5rc1"):
            with self.subTest(imported=imported), in_venv(ibapi_stub(), protobuf=protobuf_stub(imported)), \
                    self.assertRaises(IbapiNotPinned) as caught:
                check_installed(str(self.record), str(self.versions))
            self.assertIn(PB_VERSION, str(caught.exception))
        with in_venv(ibapi_stub()):
            self.assertEqual(check_installed(str(self.record), str(self.versions)), "10.51.01")

    def test_a_protobuf_that_does_not_import_or_is_not_the_venvs_own_is_refused(self):
        self.write_record(record())
        outside = (SYSTEM + "/lib/python3/dist-packages/google/protobuf/__init__.py",
                   VENV + "-old/lib/python3.12/site-packages/google/protobuf/__init__.py", "relative/protobuf/__init__.py", None)
        for path in outside:
            with self.subTest(path=path), in_venv(ibapi_stub(), protobuf=protobuf_stub(path=path)), \
                    self.assertRaises(IbapiNotPinned) as caught:
                check_installed(str(self.record), str(self.versions))
            self.assertIn("provision.sh twsapi", str(caught.exception))
        with in_venv(ibapi_stub(), protobuf=protobuf_stub(None)), self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))
        with in_venv(ibapi_stub()), mock.patch.dict(sys.modules, {"google.protobuf": None}), \
                self.assertRaises(IbapiNotPinned) as caught:
            check_installed(str(self.record), str(self.versions))
        self.assertIn("protobuf", str(caught.exception))

    def test_a_pin_file_without_the_protobuf_pin_is_refused(self):
        self.write_record(record())
        self.versions.write_text(self.versions.read_text().replace(PINS, ""))
        with in_venv(ibapi_stub()), self.assertRaises(IbapiNotPinned) as caught:
            check_installed(str(self.record), str(self.versions))
        self.assertIn("PROTOBUF", str(caught.exception))

    def test_an_ibapi_of_the_pinned_version_from_outside_the_adapter_venv_is_refused(self):
        # The venv sees the system's site packages (it is built with
        # --system-site-packages), so a system-wide or hand-installed ibapi
        # reporting the pinned version could be imported in its place: the
        # module must be the venv's own, and the Python must run in a venv.
        self.write_record(record())
        outside = (SYSTEM + "/lib/python3/dist-packages/ibapi/__init__.py",
                   "/home/someone/.local/lib/python3.12/site-packages/ibapi/__init__.py",
                   VENV + "-old/lib/python3.12/site-packages/ibapi/__init__.py",
                   VENV + "/../elsewhere/ibapi/__init__.py",
                   "relative/ibapi/__init__.py", None)
        for path in outside:
            with self.subTest(path=path), in_venv(ibapi_stub(path=path)), \
                    self.assertRaises(IbapiNotPinned) as caught:
                check_installed(str(self.record), str(self.versions))
            self.assertIn("provision.sh twsapi", str(caught.exception))
        # Not in a venv at all: the prefix is the system's.
        with in_venv(ibapi_stub(path=SYSTEM + "/lib/python3/dist-packages/ibapi/__init__.py"), prefix=SYSTEM), \
                self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))
        with in_venv(ibapi_stub(path=VENV_IBAPI), prefix=VENV, base_prefix=VENV), self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))

    def test_an_ibapi_that_does_not_import_or_names_no_version_is_refused(self):
        self.write_record(record())
        with mock.patch.dict(sys.modules, {"ibapi": None}), self.assertRaises(IbapiNotPinned) as caught:
            check_installed(str(self.record), str(self.versions))
        self.assertIn("provision.sh twsapi", str(caught.exception))
        for version in (None, {"major": 10, "minor": 51, "micro": 1}, {"major": 1051}, {"major": "1051", "minor": 1},
                        {"major": True, "minor": 1}, {"major": 1051, "minor": -1}):
            stub = ibapi_stub(None)
            if version is not None:
                stub.VERSION = version
            with self.subTest(version=version), in_venv(stub), self.assertRaises(IbapiNotPinned):
                check_installed(str(self.record), str(self.versions))

    def test_no_record_means_ibapi_was_never_installed_by_the_provisioning_step(self):
        with self.assertRaises(IbapiNotPinned) as caught:
            check_installed(str(self.record), str(self.versions))
        self.assertIn("provision.sh twsapi", str(caught.exception))

    def test_another_build_is_refused(self):
        for text in (record(version="10.50.02"),
                     record(sha="0" * 64), record(protobuf="5.29.4"), record(protobuf_sha="0" * 64)):
            with self.subTest(text=text):
                self.write_record(text)
                with self.assertRaises(IbapiNotPinned) as caught:
                    check_installed(str(self.record), str(self.versions))
                self.assertIn("provision.sh twsapi", str(caught.exception))

    def test_a_record_not_exactly_as_the_step_writes_it_is_refused(self):
        for text in ("", "TWSAPI_VERSION=10.51.01\n", record().replace("TWSAPI_VERSION=10.51.01\n", ""),
                     record().replace("PROTOBUF_VERSION=" + PB_VERSION + "\n", ""),
                     record().replace("PROTOBUF_SHA256=" + PB_SHA + "\n", ""),
                     "TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256=" + SHA + "\n",
                     record() + "EXTRA=1\n",
                     "TWSAPI_VERSION=10.51.01\n" + record(),
                     record().replace("TWSAPI_VERSION=", "TWSAPI_VERSION = "),
                     "not a key value\n"):
            with self.subTest(text=text):
                self.write_record(text)
                with self.assertRaises(IbapiNotPinned):
                    check_installed(str(self.record), str(self.versions))

    def test_a_pin_file_without_the_tws_api_pin_is_refused(self):
        self.write_record(record())
        self.versions.write_text("IBC_VERSION=3.24.2\n")
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))
        self.versions.unlink()
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))


if __name__ == "__main__":
    unittest.main()
