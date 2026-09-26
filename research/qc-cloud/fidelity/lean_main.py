"""The LEAN project entry point for a local fidelity run (README.md,
"Checking fidelity against the Go engine").

run_local.sh copies this file, as main.py, into a LEAN project beside
research_main.py (a copy of ../main.py) and rules.py, so the algorithm LEAN
runs is the research algorithm itself, unchanged, with only its class-level
run settings overridden: one instrument, a fixed span, and the Go engine
acceptance run's warm-up.

The base class is reached through its module, never imported by name, so
this subclass is the only QCAlgorithm LEAN finds in this file.
"""

from AlgorithmImports import *  # noqa: F401,F403

import research_main


class ResearchFidelity(research_main.TurtleBaselineResearch):
    FIXED_SYMBOLS = ("AAPL",)
    START_DATE = (2003, 1, 1)
    END_DATE = (2014, 12, 31)
    # The Go engine acceptance run's warm-up (its run.json "warmup_bars").
    WARMUP_BARS = 60
