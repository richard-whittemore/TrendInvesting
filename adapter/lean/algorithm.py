"""Publish daily bars and portfolio snapshots (ADRs 0014 and 0020)."""
from AlgorithmImports import *  # noqa: F401,F403

# Bind stdlib names after AlgorithmImports: its datetime.time shadows time.
from datetime import datetime, timezone
from json import load
from math import isfinite
from os import environ
from os.path import abspath, dirname, join
from re import fullmatch
from sys import path
from time import perf_counter
from zoneinfo import ZoneInfo

for candidate in ("/LeanCLI", dirname(abspath(__file__))):
    if candidate not in path:
        path.insert(0, candidate)

from client import Client
from publisher import Publisher, raw_view

# quantconnect/lean@sha256:<64 lowercase hex>; never a tag such as :latest
# or a version tag, which both float (see validate_lean_image).
_LEAN_IMAGE_DIGEST = r"quantconnect/lean@sha256:[0-9a-f]{64}"


def validate_lean_image(image):
    """Require the run's LEAN engine image, pinned by digest, never a tag.

    A LEAN run is evidence (ADR 0012, ADR 0017): the moving `:latest` tag
    lets the engine that produces a run change silently between one run and
    the next, so a later divergence — including one against cmd/backtest —
    cannot be attributed to anything. Returns the image on success; raises
    ValueError naming the defect otherwise, for the caller to fail closed.
    """
    if type(image) is not str or fullmatch(_LEAN_IMAGE_DIGEST, image) is None:
        raise ValueError(
            "lean_image must be 'quantconnect/lean@sha256:' followed by 64 lowercase hex "
            "characters (a moving tag such as ':latest' is not accepted); got {!r}".format(image))
    return image


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
            # The run's own starting cash, never a default: the first
            # account.snapshot reports it as actual equity, and the engine's
            # Notional Account measures drawdown against the configuration's
            # starting figure (ADR 0007). A mismatched pair reads as a deep
            # drawdown on the first snapshot and halts the run.
            cash = settings["cash"]
            if type(cash) not in (int, float) or not isfinite(cash) or cash <= 0:
                raise ValueError("cash must be a finite positive number")
            self.SetCash(cash)
            # The exact engine image the run was executed under, never a
            # default: pinning by digest is what makes a run evidence (ADR
            # 0012, ADR 0017) rather than a result tied to whichever image
            # happened to be `latest` that day. Logged so every run's own
            # log records which engine produced it.
            lean_image = validate_lean_image(settings.get("lean_image"))
            self.Log("adapter: lean_image={}".format(lean_image))
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
        changed = data.SymbolChangedEvents.get(self.symbol)
        if changed is not None:
            # A rename changes nothing for one instrument whose instrument_id
            # this adapter holds constant, and a ticker change is not a
            # delisting (CorporateActionPayload's closed Kind set), so it is
            # recorded here and never published.
            self.Log("adapter: symbol changed {} -> {}; instrument_id={} unchanged".format(
                changed.OldSymbol, changed.NewSymbol, self.instrument))
        notice = data.Delistings.get(self.symbol)
        bar = data.Bars.get(self.symbol)
        if bar is not None:
            self.publish_completed_bar(bar)
        if notice is not None and not self.failed:
            self.handle_delisting(notice)

    def handle_delisting(self, notice):
        """Stop on LEAN's DELISTED rather than publish it as a fact.

        LEAN derives a delisting from the end of a ticker's map file and
        carries no reason, so a conversion reads exactly like a delisting:
        it reported GOOAV, a when-issued share that became GOOG, as DELISTED
        on 2014-04-03. The reducer treats a delisting as terminal (ADR 0009),
        so publishing an untrustworthy one could record a Delisting Exit that
        never happened. Until a source that states the reason exists, the run
        stops and names the instrument instead (docs/development.md principle
        4: fail closed on uncertain state).
        """
        if notice.Type == DelistingType.Warning:
            self.Log("adapter: LEAN delisting warning for {} at {}; nothing published".format(
                self.instrument, notice.Time))
            return
        reason = ("LEAN reports {} DELISTED at {}; its delisting signal carries no reason and "
                  "also fires for conversions, so the run stops rather than publish a delisting "
                  "that may be false (adapter/lean/README.md)".format(self.instrument, notice.Time))
        # Record the deliberate stop BEFORE ending the stream, so the journal
        # can tell this run apart from one that simply reached its last bar
        # (ADR 0012; internal/event/run_stopped.go).
        self.publish_run_stopped("delisted", reason, self.instrument)
        # End the stream cleanly, so every outstanding proposal reaches its
        # terminal event in the journal rather than being left open.
        self.complete_run()
        # A failed stop notice or completion has already stopped the run
        # with its own reason; keep it, since it is the one that says the
        # journal may lack its terminal event.
        if not self.failed:
            self.stop(reason)

    def publish_run_stopped(self, reason, detail, instrument_id):
        """Send adapter.run.stopped, if the stream is intact and not empty.

        Mirrors complete_run's own guard, for the same two reasons: after a
        transport or reply failure the stream is no longer in step with the
        engine, so nothing further is sent and the run's own failure is the
        record; and a run with no inputs yet has nothing to report a stop
        against either.
        """
        if self.failed or self.client is None or self.publisher.last_event_time is None:
            return
        try:
            self.decision_count += len(self.publisher.publish_run_stopped(reason, detail, instrument_id))
        except Exception as err:
            self.stop("run stop notice failed: {}".format(err))

    def complete_run(self):
        """Send replay.run.completed once, if the stream is intact and not empty.

        Called at the normal end of the algorithm and before a deliberate
        stop. After a transport or reply failure the stream is no longer in
        step with the engine, so nothing further is sent; the run's own
        failure is the record.
        """
        if self.failed or self.client is None or self.publisher.completed \
                or self.publisher.last_event_time is None:
            return
        try:
            self.decision_count += len(self.publisher.publish_run_completed())
        except Exception as err:
            self.stop("run completion failed: {}".format(err))

    def publish_completed_bar(self, bar):
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
            # ADR 0020: this close becomes the next bar's previous-close
            # basis only after the current bar's decisions have arrived.
            snapshot_decisions = self.publisher.publish_snapshot(self.Portfolio, period_end)
            self.bar_count += 1
            self.warmup_seen += int(warming)
            self.decision_count += len(decisions) + len(snapshot_decisions)
            self.Log("adapter: seq={} end={} warmup={} raw={} split-adjusted={} decisions={} snapshot_decisions={}".format(
                self.publisher.sequence, period_end, warming, raw["close"], float(bar.Close),
                len(decisions), len(snapshot_decisions)))
        except Exception as err:
            self.stop("completed bar failed: {}".format(err))

    def OnEndOfAlgorithm(self):
        self.complete_run()
        if self.client is not None:
            self.client.close()
        self.Log("adapter: bars={} warmup_bars={} decisions={} failed={}".format(
            self.bar_count, self.warmup_seen, self.decision_count, self.failed))
        if self.history_ms:
            ordered = sorted(self.history_ms)
            self.Log("adapter: History n={} min={:.3f}ms median={:.3f}ms mean={:.3f}ms max={:.3f}ms".format(
                len(ordered), ordered[0], ordered[len(ordered) // 2],
                sum(ordered) / len(ordered), ordered[-1]))
