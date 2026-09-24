"""Publish daily completed bars; Go owns every strategy decision (ADR 0014)."""
from AlgorithmImports import *  # noqa: F401,F403

# Bind stdlib names after AlgorithmImports: its datetime.time shadows time.
from datetime import datetime, timezone
from json import load
from os import environ
from os.path import abspath, dirname, join
from sys import path
from time import perf_counter
from zoneinfo import ZoneInfo

for candidate in ("/LeanCLI", dirname(abspath(__file__))):
    if candidate not in path:
        path.insert(0, candidate)

from client import Client
from publisher import Publisher, raw_view


def load_settings():
    with open(environ.get("TREND_ADAPTER_SETTINGS", join(dirname(abspath(__file__)), "run.json"))) as source:
        return load(source)


class CompletedBarsAlgorithm(QCAlgorithm):
    def Initialize(self):
        self.client = None
        self.failed = False
        self.bar_count = 0
        self.warmup_seen = 0
        self.decision_count = 0
        self.history_ms = []
        try:
            settings = load_settings()
            if self.LiveMode:
                raise ValueError("completed-bar adapter is backtest-only")
            warmup = settings["warmup_bars"]
            if type(warmup) is not int or warmup < 0:
                raise ValueError("warmup_bars must be a nonnegative integer")
            self.SetStartDate(*map(int, settings["start"].split("-")))
            self.SetEndDate(*map(int, settings["end"].split("-")))
            self.SetCash(100000)
            self.SetTimeZone(TimeZones.NewYork)
            self.instrument = settings["symbol"]
            self.symbol = self.AddEquity(
                self.instrument, Resolution.Daily, fillForward=False,
                dataNormalizationMode=DataNormalizationMode.SplitAdjusted).Symbol
            self.SetWarmUp(warmup, Resolution.Daily)
            self.client = Client(settings["socket"], timeout=5)
            self.publisher = Publisher(self.client, settings["configuration_hash"],
                                       settings["strategy_version"], settings["run_id"])
        except Exception as err:
            self.stop("engine unavailable or invalid startup: {}".format(err))

    def stop(self, reason):
        self.failed = True
        if self.client is not None:
            self.client.close()
        self.Log("adapter: FAILURE " + reason)
        self.Quit(reason)

    def OnData(self, data):
        if self.failed or self.client is None:
            return
        bar = data.Bars.get(self.symbol)
        if bar is None:
            return
        # Warm-up bars are published exactly like any other bar. The reducer
        # owns readiness: it builds N and the Entry/Exit Channels from every
        # completed bar it is given (internal/strategy/reducer.go), so
        # withholding LEAN's warm-up bars would starve those figures of
        # exactly the history they need and make the engine start its own
        # warm-up from scratch after LEAN's already ended — and which bars a
        # strategy gets to see is itself a methodology decision, which this
        # adapter does not make (adapter/lean/README.md). warming is recorded
        # for the log and stays available on the reply below for a future
        # order-submission path, which must never act on a decision answering
        # a warm-up bar (adapter/lean/README.md); nothing here acts on
        # decisions at all yet.
        warming = self.IsWarmingUp
        try:
            started = perf_counter()
            history = self.History([self.symbol], 1, Resolution.Daily,
                                   dataNormalizationMode=DataNormalizationMode.Raw)
            self.history_ms.append((perf_counter() - started) * 1000)
            raw = raw_view(history, bar.EndTime)
            end = bar.EndTime.replace(tzinfo=ZoneInfo("America/New_York"))
            period_end = end.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            decisions = self.publisher.publish(self.instrument, bar, raw, period_end)
            self.bar_count += 1
            self.warmup_seen += int(warming)
            self.decision_count += len(decisions)
            self.Log("adapter: seq={} end={} warmup={} raw={} split-adjusted={} decisions={}".format(
                self.publisher.sequence, period_end, warming, raw["close"], float(bar.Close), len(decisions)))
        except Exception as err:
            self.stop("completed bar failed: {}".format(err))

    def OnEndOfAlgorithm(self):
        if self.client is not None:
            self.client.close()
        self.Log("adapter: bars={} warmup_bars={} decisions={} failed={}".format(
            self.bar_count, self.warmup_seen, self.decision_count, self.failed))
        if self.history_ms:
            ordered = sorted(self.history_ms)
            self.Log("adapter: History n={} min={:.3f}ms median={:.3f}ms mean={:.3f}ms max={:.3f}ms".format(
                len(ordered), ordered[0], ordered[len(ordered) // 2],
                sum(ordered) / len(ordered), ordered[-1]))
