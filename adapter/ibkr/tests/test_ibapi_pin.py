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
# The same release's Mac/Unix archive, which provision.sh twsapi installs
# from on arm64 (the Linux installer is x86_64 only).
ARCHIVE_SHA = "50a459ed09f0030df25125a177e57b69bdf576ec3a89fcb1f81fdff95bbf6316"
# The adapter's environment as the tests see it: a venv over a system Python.
VENV = "/opt/trend-investing/adapter-venv"
SYSTEM = "/usr"
VENV_IBAPI = VENV + "/lib/python3.12/site-packages/ibapi/__init__.py"


def ibapi_stub(version=(10, 51, 1), path=VENV_IBAPI):
    stub = types.ModuleType("ibapi")
    if version is not None:
        stub.VERSION = dict(zip(("major", "minor", "micro"), version))
    if path is not None:
        stub.__file__ = path
    return stub


def in_venv(stub, prefix=VENV, base_prefix=SYSTEM):
    """sys.modules and sys's prefixes as the adapter's unit runs: its venv's Python importing stub as ibapi."""
    patches = [mock.patch.dict(sys.modules, {"ibapi": stub}), mock.patch.object(sys, "prefix", prefix),
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
                                 "TWSAPI_SHA256={}\n".format(SHA))
        self.record = Path(self.dir.name) / "INSTALLED"

    def write_record(self, text):
        self.record.write_text(text)

    def test_the_shipped_pin_is_read_from_deploy_versions_env(self):
        version, sha = read_pin(str(repo / "deploy" / "versions.env"))
        self.assertRegex(version, r"^[0-9]+\.[0-9]+\.[0-9]+$")
        self.assertRegex(sha, r"^[0-9a-f]{64}$")

    def test_the_shipped_pin_names_the_same_release_s_mac_unix_archive(self):
        text = (repo / "deploy" / "versions.env").read_text()
        version, _ = read_pin(str(repo / "deploy" / "versions.env"))
        self.assertIn("\nTWSAPI_ARCHIVE_SHA256=", text)
        self.assertRegex(text, r"\nTWSAPI_ARCHIVE_URL=https://\S+\.{}\.zip\n".format(
            version.replace(".", "", 1).replace(".", r"\.")))

    def test_the_mac_unix_archive_recorded_as_installed_is_the_pinned_build(self):
        # On arm64 the record names the archive's SHA-256, the file that was
        # verified and installed; it passes only while the pin file pins it.
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(ARCHIVE_SHA))
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1))
        with self.versions.open("a") as f:
            f.write("TWSAPI_ARCHIVE_URL=https://example.invalid/x.1051.01.zip\nTWSAPI_ARCHIVE_SHA256={}\n".format(ARCHIVE_SHA))
        self.assertEqual(check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1)),
                         "10.51.01")
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format("0" * 64))
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1))

    def test_a_malformed_archive_pin_is_refused(self):
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        with self.versions.open("a") as f:
            f.write("TWSAPI_ARCHIVE_SHA256=not-a-checksum\n")
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1))

    def test_the_build_recorded_as_installed_is_the_pinned_build(self):
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        self.assertEqual(check_installed(str(self.record), str(self.versions), installed_version=lambda: (10, 51, 1)),
                         "10.51.01")

    def test_the_ibapi_the_adapter_imports_must_be_the_pinned_version(self):
        # The record alone does not prove what Python imports: a venv changed
        # after the record was written, or a system-wide ibapi, is refused.
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        for imported in ((10, 50, 2), (10, 51, 0), (9, 51, 1)):
            with self.subTest(imported=imported), self.assertRaises(IbapiNotPinned) as caught:
                check_installed(str(self.record), str(self.versions), installed_version=lambda: imported)
            self.assertIn("10.51.01", str(caught.exception))
        with in_venv(ibapi_stub()):
            self.assertEqual(check_installed(str(self.record), str(self.versions)), "10.51.01")
        with in_venv(ibapi_stub((10, 50, 2))), self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))

    def test_an_ibapi_of_the_pinned_version_from_outside_the_adapter_venv_is_refused(self):
        # The venv sees the system's site packages (it is built with
        # --system-site-packages), so a system-wide or hand-installed ibapi
        # reporting the pinned version could be imported in its place: the
        # module must be the venv's own, and the Python must run in a venv.
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
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
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        with mock.patch.dict(sys.modules, {"ibapi": None}), self.assertRaises(IbapiNotPinned) as caught:
            check_installed(str(self.record), str(self.versions))
        self.assertIn("provision.sh twsapi", str(caught.exception))
        for version in (None, {"major": 10, "minor": 51}, {"major": "10", "minor": 51, "micro": 1}):
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
        for text in ("TWSAPI_VERSION=10.50.02\nTWSAPI_SHA256={}\n".format(SHA),
                     "TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format("0" * 64)):
            with self.subTest(text=text):
                self.write_record(text)
                with self.assertRaises(IbapiNotPinned) as caught:
                    check_installed(str(self.record), str(self.versions))
                self.assertIn("provision.sh twsapi", str(caught.exception))

    def test_a_record_not_exactly_as_the_step_writes_it_is_refused(self):
        for text in ("", "TWSAPI_VERSION=10.51.01\n", "TWSAPI_SHA256={}\n".format(SHA),
                     "TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\nEXTRA=1\n".format(SHA),
                     "TWSAPI_VERSION=10.51.01\nTWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA),
                     "TWSAPI_VERSION = 10.51.01\nTWSAPI_SHA256={}\n".format(SHA),
                     "not a key value\n"):
            with self.subTest(text=text):
                self.write_record(text)
                with self.assertRaises(IbapiNotPinned):
                    check_installed(str(self.record), str(self.versions))

    def test_a_pin_file_without_the_tws_api_pin_is_refused(self):
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        self.versions.write_text("IBC_VERSION=3.24.2\n")
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))
        self.versions.unlink()
        with self.assertRaises(IbapiNotPinned):
            check_installed(str(self.record), str(self.versions))


if __name__ == "__main__":
    unittest.main()
