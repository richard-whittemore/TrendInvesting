"""`python3 -m adapter.ibkr`, run from the directory holding adapter/ (cli.py)."""
import os
import sys

# The adapter's modules import one another by their own names.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from cli import main  # noqa: E402

sys.exit(main())
