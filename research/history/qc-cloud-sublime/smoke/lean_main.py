"""The LEAN entry point for the local smoke run (README.md, "Local smoke
run").

run_local.sh copies this file, as main.py, into a LEAN project beside
research_main.py (a copy of ../main.py) and rules.py, so LEAN runs the
research algorithm unchanged with only its run settings overridden: AAPL
alone, 2003-2014, and the full history-floor warm-up.

The base class is reached through its module, never imported by name, so
this subclass is the only QCAlgorithm LEAN finds in this file.
"""

from AlgorithmImports import *  # noqa: F401,F403

import research_main


class SublimeSmoke(research_main.SublimeResearch):
    FIXED_SYMBOLS = ("AAPL",)
    START_DATE = (2003, 1, 1)
    END_DATE = (2014, 12, 31)
