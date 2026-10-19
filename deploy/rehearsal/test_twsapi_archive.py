"""Tests for deploy/twsapi-archive.py, which unpacks the Python client from the pinned TWS API Mac/Unix archive on arm64.

The archive is verified against its pinned SHA-256 before anything is
written, and only its one source/pythonclient tree is unpacked; any refusal
leaves nothing at the destination (deploy/versions.env, TWSAPI_ARCHIVE_*).
Synthetic archives only: no IBKR file is needed.
"""
import hashlib
import io
import os
import stat
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "twsapi-archive.py"
CLIENT = "IBJts/source/pythonclient/"
GOOD = {
    "IBJts/API_VersionNum.txt": "API_Version=10.51.01\n",
    CLIENT + "setup.py": "from setuptools import setup\n",
    CLIENT + "ibapi/__init__.py": 'VERSION = {"major": 1051, "minor": 1}\n',
    CLIENT + "ibapi/protobuf/__init__.py": "",
    "IBJts/source/cppclient/client/EClient.cpp": "// not unpacked\n",
}


def archive(entries, symlinks=()):
    """Zip bytes holding entries ({name: text}) and symlinks ((name, target), ...)."""
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as z:
        for name, text in entries.items():
            z.writestr(name, text)
        for name, target in symlinks:
            info = zipfile.ZipInfo(name)
            info.external_attr = (stat.S_IFLNK | 0o777) << 16
            z.writestr(info, target)
    return out.getvalue()


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.work = Path(self.dir.name)
        self.dest = self.work / "out"

    def run_script(self, data, sha=None):
        path = self.work / "twsapi.zip"
        path.write_bytes(data)
        sha = sha or hashlib.sha256(data).hexdigest()
        return subprocess.run([sys.executable, "-I", str(SCRIPT), str(path), sha, str(self.dest)],
                              capture_output=True, text=True)

    def refused(self, data, sha=None, says=None):
        done = self.run_script(data, sha)
        self.assertNotEqual(done.returncode, 0, done.stdout)
        self.assertFalse(self.dest.exists(), "a refusal left something at the destination")
        if says:
            self.assertIn(says, done.stderr)
        return done

    def test_the_pinned_archive_s_python_client_alone_is_unpacked(self):
        done = self.run_script(archive(GOOD))
        self.assertEqual(done.returncode, 0, done.stderr)
        files = sorted(str(p.relative_to(self.dest)) for p in self.dest.rglob("*") if p.is_file())
        self.assertEqual(files, ["pythonclient/ibapi/__init__.py", "pythonclient/ibapi/protobuf/__init__.py",
                                 "pythonclient/setup.py"])
        self.assertEqual((self.dest / "pythonclient/ibapi/__init__.py").read_text(), GOOD[CLIENT + "ibapi/__init__.py"])
        for p in self.dest.rglob("*"):
            mode = stat.S_IMODE(p.stat().st_mode)
            self.assertEqual(mode, 0o755 if p.is_dir() else 0o644, str(p))

    def test_a_tampered_download_is_refused_with_nothing_unpacked(self):
        good = archive(GOOD)
        tampered = archive(dict(GOOD, **{CLIENT + "ibapi/__init__.py": "VERSION = {}\n"}))
        self.refused(tampered, sha=hashlib.sha256(good).hexdigest(), says="checksum mismatch")

    def test_a_checksum_not_in_the_pin_s_form_is_refused(self):
        self.refused(archive(GOOD), sha="not-a-checksum")

    def test_a_file_that_is_not_a_zip_is_refused(self):
        self.refused(b"not a zip archive")

    def test_an_archive_without_exactly_one_python_client_is_refused(self):
        without = {k: v for k, v in GOOD.items() if not k.startswith(CLIENT)}
        self.refused(archive(without), says="pythonclient")
        two = dict(GOOD, **{"other/source/pythonclient/setup.py": ""})
        self.refused(archive(two), says="pythonclient")

    def test_a_python_client_without_setup_py_or_pyproject_is_refused(self):
        self.refused(archive({k: v for k, v in GOOD.items() if k != CLIENT + "setup.py"}), says="setup.py")

    def test_an_entry_that_would_land_outside_the_destination_is_refused(self):
        for name in (CLIENT + "../../../../escape.py", CLIENT + "ibapi/../../../x.py", "/" + CLIENT + "abs.py"):
            with self.subTest(name=name):
                self.refused(archive(dict(GOOD, **{name: "x"})))
        self.assertFalse((self.work / "escape.py").exists())

    def test_a_symbolic_link_in_the_python_client_is_refused(self):
        self.refused(archive(GOOD, symlinks=[(CLIENT + "ibapi/link.py", "/etc/passwd")]))

    def test_an_existing_destination_is_refused(self):
        self.dest.mkdir()
        done = self.run_script(archive(GOOD))
        self.assertNotEqual(done.returncode, 0)
        self.assertEqual(os.listdir(self.dest), [])


if __name__ == "__main__":
    unittest.main()
