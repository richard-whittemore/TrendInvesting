"""The adapter starts only on the ibapi build deploy/versions.env pins, as provision.sh twsapi recorded installing it."""
import sys
import tempfile
import unittest
from pathlib import Path

tests_dir = Path(__file__).resolve().parent
adapter_dir = tests_dir.parent
repo = adapter_dir.parents[1]
for p in (str(tests_dir), str(adapter_dir)):
    if p not in sys.path:
        sys.path.insert(0, p)

from ibapi_pin import IbapiNotPinned, check_installed, read_pin

SHA = "152af944be28bcfaf2733aec907e7724f6dda092cb3148848e84d2a0bb866c0a"


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

    def test_the_build_recorded_as_installed_is_the_pinned_build(self):
        self.write_record("TWSAPI_VERSION=10.51.01\nTWSAPI_SHA256={}\n".format(SHA))
        self.assertEqual(check_installed(str(self.record), str(self.versions)), "10.51.01")

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
