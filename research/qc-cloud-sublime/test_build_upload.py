"""Tests for build_upload.py, the Free-plan upload build (README.md, "How to
run it"): both stripped files stay under the 32,000-character limit, both
compile, and test_rules.py passes against the stripped rules.py. Run via
`make research-test`.
"""

import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import build_upload

HERE = Path(__file__).resolve().parent
DIST = HERE / "dist"


class BuildUploadTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Build fresh for this run, so every assertion below (including the
        # subprocess one) reads the CURRENT source, never a stale dist/
        # copy left over from an earlier run or another test module.
        cls.sizes = build_upload.build()

    def test_main_py_is_under_the_free_plan_character_limit(self):
        self.assertLess(self.sizes["main.py"], build_upload.MAX_CHARS)

    def test_rules_py_is_under_the_free_plan_character_limit(self):
        self.assertLess(self.sizes["rules.py"], build_upload.MAX_CHARS)

    def test_built_main_py_compiles(self):
        source = (DIST / "main.py").read_text()
        compile(source, str(DIST / "main.py"), "exec")

    def test_built_rules_py_compiles(self):
        source = (DIST / "rules.py").read_text()
        compile(source, str(DIST / "rules.py"), "exec")

    def test_full_rules_suite_passes_against_the_built_rules_py(self):
        # Run test_rules.py's own, unmodified suite against dist/rules.py,
        # never the original: copy both into an isolated temporary
        # directory (so "import rules" there can only resolve to the
        # stripped copy, never this directory's own rules.py) and run the
        # suite in a subprocess.
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            shutil.copy(DIST / "rules.py", tmp_path / "rules.py")
            shutil.copy(HERE / "test_rules.py", tmp_path / "test_rules.py")
            result = subprocess.run(
                [sys.executable, "-m", "unittest", "test_rules"],
                cwd=str(tmp_path), capture_output=True, text=True)
            self.assertEqual(result.returncode, 0,
                              "test_rules.py failed against dist/rules.py:\n{}\n{}".format(
                                  result.stdout, result.stderr))


if __name__ == "__main__":
    unittest.main()
