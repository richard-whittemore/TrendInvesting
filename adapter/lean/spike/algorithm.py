"""A QCAlgorithm that exchanges a bar and a decision with Go on every bar.

This is the load-bearing part of the spike: everything else measures Python
talking to Go, but LEAN runs Python under pythonnet inside the .NET process,
holding the GIL across the call. Only running it here shows whether a blocking
socket call per bar is survivable in the environment that will actually host
the adapter.

Run it with `lean backtest` and a socket mounted from a Docker *named volume*
(see README.md); a socket on a macOS bind mount cannot be connected to from
inside the container.

The algorithm is deliberately not a strategy: it sends the real universe as a
completed-bar envelope, requires a decision envelope citing it, and reports the
latency distribution at the end. No orders are placed.
"""

import json
import os
import sys

from AlgorithmImports import *  # noqa: F401,F403 - LEAN's required import shape

# Imported AFTER the star import, and by name. AlgorithmImports exports
# datetime.time, which shadows the stdlib time module and turns
# perf_counter() into an AttributeError at the first bar. Anything the
# adapter needs from a shadowed stdlib module must be bound like this.
from time import perf_counter  # noqa: E402

# The project directory is mounted at /LeanCLI; client.py sits beside this file.
for candidate in ("/LeanCLI", os.path.dirname(os.path.abspath(__file__))):
    if candidate not in sys.path:
        sys.path.insert(0, candidate)

import client as spike  # noqa: E402 - path must be set first

SOCKET_PATH = os.environ.get("TREND_SPIKE_SOCKET", "/run/spike/s.sock")
# Fail closed: if the engine does not answer within this, the algorithm stops
# rather than trading on stale or absent decisions.
DECISION_TIMEOUT_S = float(os.environ.get("TREND_SPIKE_TIMEOUT", "5"))


class TransportSpikeAlgorithm(QCAlgorithm):  # noqa: F405 - from AlgorithmImports

    def Initialize(self):
        self.SetStartDate(2013, 10, 7)
        self.SetEndDate(2013, 10, 11)
        self.SetCash(100000)
        self.symbols = [
            self.AddEquity(ticker, Resolution.Daily).Symbol  # noqa: F405
            for ticker in ("SPY", "AAPL", "IBM", "BAC", "GOOG")
        ]
        self.sequence = 0
        self.latencies_ms = []
        self.failures = []
        self.client = None
        try:
            self.client = spike.Client(SOCKET_PATH, timeout=DECISION_TIMEOUT_S)
            self.Log("transport-spike: connected to {}".format(SOCKET_PATH))
        except spike.Unavailable as err:
            # The safety invariant: no engine, no trading. Recording it and
            # continuing would be exactly the failure mode this spike exists
            # to rule out.
            self.Log("transport-spike: engine unavailable at startup: {}".format(err))
            self.Quit("decision engine unavailable")

    def OnData(self, data):
        if self.client is None:
            return
        bars = []
        for symbol in self.symbols:
            bar = data.Bars.get(symbol)
            if bar is None:
                continue
            bars.append({
                "symbol": str(symbol.Value),
                "open": float(bar.Open),
                "high": float(bar.High),
                "low": float(bar.Low),
                "close": float(bar.Close),
                "volume": int(bar.Volume),
            })
        if not bars:
            return

        self.sequence += 1
        envelope = spike.envelope(
            "market.bar.completed",
            self.sequence,
            {"as_of": str(self.Time.date()), "bars": bars},
        )
        started = perf_counter()
        try:
            decision = self.client.decide(envelope)
        except spike.Rejected as err:
            self.failures.append("bar {} rejected: {}".format(self.sequence, err.code))
            return
        except (spike.Unavailable, spike.OutOfOrder) as err:
            self.failures.append("bar {}: {}".format(self.sequence, err))
            self.Quit("decision engine unavailable mid-run")
            return
        elapsed_ms = (perf_counter() - started) * 1000.0
        self.latencies_ms.append(elapsed_ms)

        payload = json.loads(json.dumps(decision["payload"]))
        self.Log("transport-spike: bar {} at {} -> {} considered, {} proposals, {:.3f} ms".format(
            self.sequence, self.Time.date(), payload.get("considered"),
            len(payload.get("proposals", [])), elapsed_ms))

    def OnEndOfAlgorithm(self):
        if self.client is not None:
            self.client.close()
        if self.latencies_ms:
            ordered = sorted(self.latencies_ms)
            self.Log("transport-spike: round trips n={} min={:.3f}ms median={:.3f}ms max={:.3f}ms".format(
                len(ordered), ordered[0], ordered[len(ordered) // 2], ordered[-1]))
        for failure in self.failures:
            self.Log("transport-spike: FAILURE {}".format(failure))
