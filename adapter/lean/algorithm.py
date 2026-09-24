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
from types import SimpleNamespace
from zoneinfo import ZoneInfo

for candidate in ("/LeanCLI", dirname(abspath(__file__))):
    if candidate not in path:
        path.insert(0, candidate)

from client import Client
from orders import (NSlippageModel, OrderDesk, fill_model_report, validate_slippage_n,
                    whole_split_ratio)
from publisher import Publisher, split_adjusted_view

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
        self.fill_count = 0
        self.lifecycle_count = 0
        # What LEAN reported about this run's orders, not yet sent to the
        # engine (drain_order_events).
        self.order_events = []
        # The last Session's close, read but not yet sent (flush_snapshot).
        self.pending_snapshot = None
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
            # The run's own slippage fraction, never a default: it must equal
            # the configuration's slippage_n (ADR 0013; 0.05 in the Baseline),
            # which this adapter cannot read, exactly as cash must equal its
            # notional_account.starting_equity.
            slippage_n = validate_slippage_n(settings.get("slippage_n"))
            # Every way this run's LEAN fills depart from ADR 0005 and ADR
            # 0013, stated before the first bar rather than silently accepted.
            for line in fill_model_report(slippage_n):
                self.Log("adapter: fill model: " + line)
            self.SetTimeZone(TimeZones.NewYork)
            self.instrument = settings["symbol"]
            # ADR 0004: order pricing, fills and portfolio accounting are raw.
            # LEAN trades, holds and charges commission in whatever view the
            # subscription is in, so the subscription is raw; the
            # split-adjusted view the engine's signals read comes from
            # History (publish_completed_bar).
            security = self.AddEquity(
                self.instrument, Resolution.Daily, fillForward=False,
                dataNormalizationMode=DataNormalizationMode.Raw)
            self.symbol = security.Symbol
            self.desk = OrderDesk(self, self.symbol, self.instrument, SimpleNamespace(
                OrderProperties=OrderProperties, TimeInForce=TimeInForce,
                UpdateOrderFields=UpdateOrderFields, OrderStatus=OrderStatus))
            # ADR 0013: slippage_n x the N the engine supplied with each
            # order's decision, and Interactive Brokers commissions.
            security.SetSlippageModel(NSlippageModel(slippage_n, self.desk.n_for_tag,
                                                     self.desk.record_slippage))
            security.SetFeeModel(InteractiveBrokersFeeModel())
            # docs/architecture.md: reconcile before any executor submits.
            self.desk.require_flat("at startup")
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
        # LEAN reports a session's fills before it delivers that session's
        # bar, and the engine must hear of them in the same order: a proposal
        # stays outstanding only until the instrument's next bar expires it
        # (ADR 0011), so a fill delivered after that bar would name a proposal
        # the engine no longer offers (drain_order_events).
        self.drain_order_events()
        self.flush_snapshot()
        if self.failed:
            return
        try:
            self.desk.require_cancels_confirmed("before the next session's bar")
        except Exception as err:
            self.stop("order state uncertain: {}".format(err))
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

    def OnOrderEvent(self, order_event):
        """Record what LEAN reports about an order; never send from here.

        LEAN raises this in the middle of placing, amending or cancelling an
        order (a submission is reported before StopMarketOrder returns), and
        before OnData for a session's fills. Sending from here would
        interleave an input with the exchange in progress, so the report is
        queued and sent at the next point where an input may be:
        drain_order_events.
        """
        publisher = getattr(self, "publisher", None)
        if self.failed or getattr(self, "desk", None) is None or publisher is None \
                or publisher.completed:
            return
        try:
            self.order_events.append(self.desk.observe(order_event))
        except Exception as err:
            self.stop("order event unreadable: {}".format(err))

    def drain_order_events(self):
        """Send every queued order report to the engine, and act on its replies.

        Called before a slice's bar, after the slice's decisions have been
        acted on, and before the stream ends. The order is:

        - reports in the order LEAN made them, except that
        - every fill LEAN reported at one instant is sent together, at the
          place of the first, in ADR 0005's order (OrderDesk.fills): an Exit
          Channel exit is one fill for all its Units, so the group must be
          whole before any of it is sent.

        Acting on a fill's decisions can place or amend orders, and LEAN
        reports those too, so the queue is drained until it is empty. A fill
        is sent as execution.fill and every other change as
        execution.order.lifecycle. Any failure stops the run.
        """
        try:
            while self.order_events and not self.failed:
                pending, self.order_events = self.order_events, []
                while pending and not self.failed:
                    record = pending.pop(0)
                    if not self.desk.is_execution(record):
                        self.publish_order_lifecycle(record)
                        continue
                    group = [record] + [r for r in pending
                                        if r["time"] == record["time"] and self.desk.is_execution(r)]
                    pending = [r for r in pending if all(r is not g for g in group)]
                    self.publish_fills(group)
        except Exception as err:
            self.stop("order event failed: {}".format(err))

    def publish_order_lifecycle(self, record):
        decisions = self.publisher.publish_order_lifecycle(self.desk.lifecycle(record))
        self.lifecycle_count += 1
        self.decision_count += len(decisions)

    def publish_fills(self, records):
        for record in records:
            if record["message"]:
                # LEAN's own account of how it priced the fill, such as a gap
                # filled at the open: evidence for the fill-model report.
                self.Log("adapter: LEAN on order {}: {}".format(record["order_id"], record["message"]))
        for payload, order_ids in self.desk.fills(records):
            decisions = self.publisher.publish_fill(payload)
            self.desk.delivered(order_ids)
            self.fill_count += 1
            self.decision_count += len(decisions)
            self.Log("adapter: fill {} {} {} @ {} at {} level={} slippage={} commission={} "
                     "decisions={}".format(payload["fill_id"], payload["kind"], payload["quantity"],
                                           payload["price"], payload["filled_at"], payload["level"],
                                           payload["slippage_applied"], payload["commission"],
                                           len(decisions)))
            # The fill's decisions answer the session it executed in, so a
            # proposal they carry for an earlier bar is stale (OrderDesk._propose).
            self.desk.act(decisions, payload["filled_at"], self.IsWarmingUp)

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
        # The Session's own close is still reported, before the stop.
        self.flush_snapshot()
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
        # adapter does not make (adapter/lean/README.md). A decision answering
        # a warm-up bar is never acted on (OrderDesk.act).
        warming = self.IsWarmingUp
        try:
            started = perf_counter()
            history = self.History([self.symbol], 1, Resolution.Daily,
                                   dataNormalizationMode=DataNormalizationMode.SplitAdjusted)
            self.history_ms.append((perf_counter() - started) * 1000)
            adjusted = split_adjusted_view(history, bar.EndTime)
            end = bar.EndTime.replace(tzinfo=ZoneInfo("America/New_York"))
            period_end = end.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            # ADR 0004: the ratio the desk converts the engine's
            # split-adjusted figures to LEAN's raw ones by, checked before
            # the bar reaches the engine.
            self.desk.observe_ratio(whole_split_ratio(float(bar.Close) / adjusted["close"]),
                                    period_end)
            decisions = self.publisher.publish(self.instrument, bar, adjusted, period_end)
            # ADR 0021: the slice's bars are its Session; closing it lets the
            # engine decide the day's Adds and entries.
            decisions += self.publisher.publish_session_closed(period_end)
            # ADR 0020: LEAN's close, read before this slice's decisions move
            # anything, and sent by flush_snapshot before the next bar.
            self.pending_snapshot = (SimpleNamespace(
                TotalPortfolioValue=float(self.Portfolio.TotalPortfolioValue),
                Cash=float(self.Portfolio.Cash)), period_end, warming)
            self.bar_count += 1
            self.warmup_seen += int(warming)
            self.decision_count += len(decisions)
            self.Log("adapter: seq={} end={} warmup={} raw={} split-adjusted={} ratio={} "
                     "decisions={}".format(self.publisher.sequence, period_end, warming,
                                           float(bar.Close), adjusted["close"], self.desk.ratio,
                                           len(decisions)))
            # Only once both of this bar's exchanges have succeeded: a failed
            # exchange leaves the stream out of step with the engine, and the
            # run then stops with nothing submitted (README.md: safe mode).
            self.desk.act(decisions, period_end, warming)
        except Exception as err:
            self.stop("completed bar failed: {}".format(err))
            return
        # What LEAN reported while those decisions were acted on.
        self.drain_order_events()

    def flush_snapshot(self):
        """Send the last Session's account.snapshot, once, before anything later.

        The snapshot states LEAN's close and is the next Session's
        previous-close basis (ADR 0010, ADR 0020), so it must reach the engine
        before the next bar. It is sent only after the fills of the orders
        that Session's close placed, which LEAN reports at the start of the
        next slice. The engine attributes a fill's follow-on Add to the bar
        that signalled it and checks it against cash known at that bar's
        previous close (ADR 0021, section 7; evaluateAdd), so a snapshot sent
        between that bar and the fill would leave the Add no eligible cash
        basis and stop the run. Its figures were read at the close, so what
        it says is unchanged by when it is sent.
        """
        pending = getattr(self, "pending_snapshot", None)
        if pending is None or self.failed or self.client is None:
            return
        self.pending_snapshot = None
        portfolio, period_end, warming = pending
        try:
            decisions = self.publisher.publish_snapshot(portfolio, period_end)
            self.decision_count += len(decisions)
            self.desk.act(decisions, period_end, warming)
        except Exception as err:
            self.stop("account snapshot failed: {}".format(err))

    def OnEndOfAlgorithm(self):
        if not self.failed and self.client is not None:
            self.drain_order_events()
            if self.desk.pending_cancels:
                # No session follows in which the order could fill.
                self.Log("adapter: cancellation of order(s) {} unconfirmed at the end of the "
                         "run".format(sorted(self.desk.pending_cancels)))
            self.flush_snapshot()
        self.complete_run()
        if self.client is not None:
            self.client.close()
        self.Log("adapter: bars={} warmup_bars={} fills={} order_changes={} decisions={} "
                 "failed={}".format(self.bar_count, self.warmup_seen, self.fill_count,
                                    self.lifecycle_count, self.decision_count, self.failed))
        desk = getattr(self, "desk", None)
        if desk is not None:
            self.Log("adapter: orders submitted={} decisions rejected={}".format(
                desk.submitted, desk.rejected))
        if self.history_ms:
            ordered = sorted(self.history_ms)
            self.Log("adapter: History n={} min={:.3f}ms median={:.3f}ms mean={:.3f}ms max={:.3f}ms".format(
                len(ordered), ordered[0], ordered[len(ordered) // 2],
                sum(ordered) / len(ordered), ordered[-1]))
