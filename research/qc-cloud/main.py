"""Baseline Turtle research check, for QuantConnect Cloud's Free plan.

Research-only. This is NOT the production Go engine
(internal/strategy, internal/sizing, internal/indicator, internal/fills) and
carries no claim of bit-for-bit parity with it. It exists to answer one
question cheaply: do the Baseline Turtle rules make money on a broad US
equity universe, after realistic costs, using QuantConnect's free 1998-to-date
US equity history? See README.md for how to run it and for every place this
cloud script departs from the ADRs it otherwise follows, and why.

rules.py is the pure-Python rule core this file drives; it has no
QuantConnect imports and is unit-tested on its own (test_rules.py). This
file is deliberately thin: it holds only QuantConnect wiring -- universe
selection, order placement, fill handling, and the closing summary -- never
a re-derivation of a rule rules.py already states.

The QuantConnect API calls below (AddUniverse/CoarseFundamental/
FineFundamental, StopLimitOrder/StopMarketOrder, SetBrokerageModel,
SetSlippageModel/SetFeeModel, Transactions.GetOpenOrderTickets,
ticket.Update(UpdateOrderFields(...))) match the casing and call shape
already verified working in this repository's own LEAN adapter
(adapter/lean/algorithm.py, adapter/lean/orders.py) against the pinned LEAN
image; the custom stop-limit fill/slippage model below is adapted from
adapter/lean/orders.py's own adr_0005_fill_model/stop_limit_buy_fill_price
and NSlippageModel (copied here, not imported, so this file stays
self-contained for pasting into a fresh QuantConnect cloud project).
"""

from AlgorithmImports import *  # noqa: F401,F403

from datetime import date, datetime, timezone

from os.path import abspath, dirname
from sys import path
from types import SimpleNamespace

# rules.py sits beside this file. QuantConnect's cloud IDE runs every file
# in one project folder, so this is normally unnecessary, but it mirrors
# adapter/lean/algorithm.py's own defensive import guard for local runs.
_HERE = dirname(abspath(__file__))
if _HERE not in path:
    path.insert(0, _HERE)

import rules  # noqa: E402

# How many trailing daily bars a symbol needs before it can be sized or
# judged eligible: the deepest of ADR 0009's 250-bar history floor, the
# 55-bar Entry Channel and 20-bar Exit Channel (ADR 0002), and Strength's
# own 64-bar lookback (ADR 0010, as amended). Used both for the
# algorithm's own initial SetWarmUp and for backfilling a stock the
# universe selects only after the algorithm has already started
# (PR #253 review, Greptile: main.py:360 -- "New selections lack prior
# history"; see TurtleBaselineResearch._warm_up_new_symbol).
WARMUP_BARS = max(rules.ENTRY_CHANNEL_LENGTH, rules.UNIVERSE_MIN_HISTORY_BARS,
                  rules.STRENGTH_LOOKBACK_BARS + 1)


# =============================================================================
# A custom stop-limit fill model and slippage model (ADR 0005, as amended
# 2026-09-24; ADR 0013), adapted from adapter/lean/orders.py.
#
# LEAN's OWN native stop-limit fill (a) triggers only on a STRICT high >
# stop, not an exact touch; (b) fills the triggering bar at min(high,
# limit); (c) applies NO slippage to a stop-limit fill (observed by this
# repository's own LEAN adapter). None of that is ADR 0005's rule. This
# subclass of LEAN's own EquityFillModel replaces StopLimitFill for a BUY
# only, with ADR 0005's own rule: the order triggers on an exact touch,
# fills at max(level, open) when that is within the cap, and otherwise
# works as a limit order at the cap (filling there only if the bar trades
# back down to it) -- and it adds ADR 0013's slippage explicitly, since
# LEAN's native mechanism does not. Every sell (a Protective Stop or an
# Exit Order) keeps LEAN's own native EquityFillModel and its native
# slippage application, which the adapter's own observations confirm DOES
# apply slippage to a stop-market fill.
# =============================================================================


def _stop_limit_buy_fill_price(level, price_cap, open_, high, low, slippage_amount):
    """ADR 0005's stop-limit buy, as amended 2026-09-24: the fill price, or
    None for no fill this bar. Mirrors
    adapter/lean/orders.stop_limit_buy_fill_price exactly.

    Triggers on an exact touch (high >= level). Fills at max(level, open)
    when that is within the cap. An open above the cap leaves it working as
    a limit buy at the cap: fills at the cap if the bar trades back down to
    it (low <= cap), otherwise not at all this bar. Slippage is added after
    the cap bounds the price (ADR 0013), so a fill never costs more than
    cap + slippage.
    """
    if high < level:
        return None
    if open_ <= price_cap:
        return max(level, open_) + slippage_amount
    if low > price_cap:
        return None
    return price_cap + slippage_amount


def _n_slippage_model(n_by_order_id):
    """ADR 0013: 0.05 x N per fill, against the trader, on every order this
    algorithm placed. ``n_by_order_id`` is populated the moment an order is
    placed (see TurtleBaselineResearch._decide_add/_decide_entry/
    _maintain_exit_orders), so the model never computes its own N -- the
    same discipline adapter/lean/orders.NSlippageModel follows.
    """

    class NSlippageModel:
        def GetSlippageApproximation(self, asset, order):
            n = n_by_order_id.get(order.Id)
            if n is None:
                return 0.0
            return rules.slippage(n)

        # LEAN has called both the PascalCase and snake_case spellings of
        # this method across versions; adapter/lean/orders.py keeps both
        # bound for the same reason.
        get_slippage_approximation = GetSlippageApproximation

    return NSlippageModel()


def _classification_groups(fine):
    """ADR 0008 maps company -> industry -> sector -> total-long onto
    Faith's four Unit-cap levels (The Turtle Rules p.16: 4 per market/6 per
    closely-correlated group/10 per loosely-correlated group/12 total
    long). QuantConnect's Free plan supplies Morningstar classification on
    the fine/fundamental universe, three levels broad to narrow: Sector,
    Industry Group, Industry.

    - ADR 0008's "sector" is its LOOSELY-correlated, 10-Unit level, so it
      reads Morningstar's own (broadest) Sector code.
    - ADR 0008's "industry" is the TIGHTER, closely-correlated, 6-Unit
      level. Morningstar's finest code, Industry, is narrower than that --
      The Turtle Rules p.16's own examples of close correlation (heating
      oil/crude, gold/silver, CHF/DEM) are related but distinct lines of
      business, not identical ones -- so this reads the middle level,
      Industry Group, for ADR 0008's "industry".

    Returns ``(industry, sector)`` as strings, or ``(None, None)`` /
    ``(None, <sector>)`` etc. when Morningstar has no code for a level (a
    code of 0, or no ``AssetClassification`` at all) -- reaching
    rules.UnitCaps' own Unclassified Group exactly for an instrument
    Morningstar genuinely has no classification for, per ADR 0008's own
    words: "every instrument without a label belongs to a single shared
    'unclassified' group."
    """
    classification = getattr(fine, "AssetClassification", None)
    sector_code = getattr(classification, "MorningstarSectorCode", None) if classification else None
    industry_code = getattr(classification, "MorningstarIndustryGroupCode", None) \
        if classification else None
    sector = str(sector_code) if sector_code else None
    industry = str(industry_code) if industry_code else None
    return industry, sector


def _adr_0005_fill_model(slippage_model):
    """A subclass of LEAN's own EquityFillModel that replaces StopLimitFill
    for a BUY only (ADR 0005, as amended). Every other order (a sell, or a
    stop-market Add/entry, which this algorithm never places) keeps
    EquityFillModel's own fill."""

    class Adr0005FillModel(EquityFillModel):
        def __init__(self):
            super().__init__()
            self.failure = None

        def StopLimitFill(self, asset, order):
            if order.Direction != OrderDirection.Buy:
                return super().StopLimitFill(asset, order)
            fill = OrderEvent(order, Extensions.ConvertToUtc(asset.LocalTime, asset.Exchange.TimeZone),
                              OrderFee.Zero)
            if order.Status == OrderStatus.Canceled or not self.IsExchangeOpen(asset, False):
                return fill
            prices = self.GetPricesCheckingPythonWrapper(asset, order.Direction)
            if Extensions.ConvertToUtc(prices.EndTime, asset.Exchange.TimeZone) <= order.Time:
                return fill
            try:
                slip = float(slippage_model.GetSlippageApproximation(asset, order))
                price = _stop_limit_buy_fill_price(float(order.StopPrice), float(order.LimitPrice),
                                                   float(prices.Open), float(prices.High),
                                                   float(prices.Low), slip)
            except Exception as err:  # fail closed: never guess a fill price.
                self.failure = "order {} could not be priced by the ADR 0005 fill model: {}".format(
                    order.Id, err)
                return fill
            if price is not None:
                fill.Status = OrderStatus.Filled
                fill.FillQuantity = order.Quantity
                fill.FillPrice = price
            return fill

    return Adr0005FillModel


# =============================================================================
# Per-symbol state.
# =============================================================================


class _SymbolState:
    """Everything this algorithm tracks per instrument. rules.py's own
    classes (WilderN, EntryChannel, ExitChannel, Campaign) hold the actual
    rule state; this class is bookkeeping around them."""

    __slots__ = ("n", "entry_channel", "exit_channel", "closes", "raw_closes", "raw_volumes",
                 "bars_seen", "previous_close", "last_bar_date", "campaign", "entry_ticket",
                 "add_ticket", "unit_tickets")

    def __init__(self):
        self.n = rules.WilderN()
        self.entry_channel = rules.EntryChannel()
        self.exit_channel = rules.ExitChannel()
        # Strength needs 64 split-adjusted closes; the dollar-volume window
        # needs 20 raw closes/volumes (ADR 0010, as amended).
        self.closes = []       # split-adjusted, for Strength (bounded below)
        self.raw_closes = []   # raw, for the dollar-volume window
        self.raw_volumes = []  # raw, for the dollar-volume window
        self.bars_seen = 0
        self.previous_close = None
        # The date of the last bar actually fed into N/the channels
        # (_advance), from EITHER a live/warm-up bar or a History
        # backfill row -- whichever arrives first for a given date wins,
        # and a later arrival for the SAME date is skipped (PR #253
        # review, Greptile main.py:413: "Warm-up bars counted twice").
        self.last_bar_date = None
        self.campaign = None            # rules.Campaign, or None
        self.entry_ticket = None        # the resting entry stop-limit order
        self.add_ticket = None          # the resting Add stop-limit order
        self.unit_tickets = []          # one resting sell order per open Unit

    def record_close(self, raw_close, raw_volume, split_adjusted_close):
        self.closes.append(split_adjusted_close)
        if len(self.closes) > rules.STRENGTH_LOOKBACK_BARS + 1:
            del self.closes[0]
        self.raw_closes.append(raw_close)
        self.raw_volumes.append(raw_volume)
        if len(self.raw_closes) > rules.DOLLAR_VOLUME_WINDOW:
            del self.raw_closes[0]
            del self.raw_volumes[0]


# =============================================================================
# The algorithm.
# =============================================================================


class TurtleBaselineResearch(QCAlgorithm):
    """A thin QCAlgorithm driving rules.py's Baseline Turtle rule core over
    a broad, point-in-time US equity universe (ADR 0009), 1998 to today, on
    QuantConnect's Free plan.

    See README.md, "How to run it", for how a non-programmer pastes this
    project's four files into QuantConnect and reads the results, and
    "Deviations" for everything the Free-plan cloud environment forces this
    file to do differently from the Go engine's own reducer.
    """

    # The Free-plan universe size (README.md, "Universe size", explains the
    # choice and its effect): the top N Baseline-eligible names by dollar
    # volume, re-selected on the first trading day of each month (ADR 0009).
    UNIVERSE_SIZE = 200

    STARTING_CASH = 1_000_000.0

    def Initialize(self):
        self.SetStartDate(1998, 1, 1)
        today = datetime.now(timezone.utc).date()
        self.SetEndDate(today.year, today.month, today.day)
        self.SetCash(self.STARTING_CASH)
        self.SetTimeZone(TimeZones.NewYork)
        # ADR 0010: no partial Units, no borrowing -- a cash account makes
        # LEAN itself refuse an order it cannot fund (mirrors
        # adapter/lean/algorithm.py's own Initialize).
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Cash)

        self.UniverseSettings.Resolution = Resolution.Daily
        self.UniverseSettings.Leverage = 1.0
        # ADR 0004: signals run on split-adjusted prices, which is also the
        # view this repository's own reference engine prices every fill in
        # (see README.md, "Deviations", "Price view"). Splits are neutral;
        # dividends still arrive as cash into the account separately, never
        # folded into this series.
        self.UniverseSettings.DataNormalizationMode = DataNormalizationMode.SplitAdjusted
        self.AddUniverse(self.CoarseSelectionFunction, self.FineSelectionFunction)

        # SPY, for the buy-and-hold comparison the closing summary reports.
        # This is the one series in this script priced on QuantConnect's
        # split-AND-DIVIDEND-adjusted ("Adjusted") basis, deliberately
        # unlike every traded instrument's own SplitAdjusted subscription
        # (ADR 0004 governs the strategy's own signals and fills, not an
        # external comparison benchmark): a buy-and-hold total-return
        # figure needs dividends folded in, or it understates SPY and
        # flatters the strategy by comparison (PR #253 review, Greptile
        # and CodeRabbit: main.py:384, "SPY comparison omits dividends").
        self.spy = self.AddEquity(
            "SPY", Resolution.Daily, dataNormalizationMode=DataNormalizationMode.Adjusted).Symbol
        self.spy_curve = []

        self.symbol_state = {}
        self._last_eligibility_month = None
        # symbol -> (industry, sector), from Morningstar classification
        # (ADR 0008; _classification_groups), refreshed every month
        # FineSelectionFunction runs. A new Campaign reads this at the
        # moment it opens and freezes it (rules.Campaign); it is never
        # re-read afterward (ADR 0008's own freeze-at-entry discipline).
        self._classification = {}

        self.notional_account = rules.NotionalAccount(self.STARTING_CASH)
        self.unit_caps = rules.UnitCaps()
        self.n_by_order_id = {}
        # entry order id -> the (industry, sector) read at proposal time,
        # so the Unit-cap check made when the order was placed and the
        # classification the resulting Campaign freezes are the SAME
        # reading, never two separate lookups that a mid-flight monthly
        # reclassification could pull apart.
        self.classification_by_order_id = {}
        # order id -> "entry" | "add" | "exit", set the moment an order is
        # placed (_decide_entry/_decide_add/_maintain_exit_orders) and read
        # by OnOrderEvent. Replaces parsing meaning out of the order's own
        # tag string, which went stale the moment a Unit's own index moved
        # (PR #253 review, Greptile main.py:647 and CodeRabbit main.py:647:
        # "Keep state.unit_tickets aligned ... locate the ticket by
        # order_event.OrderId rather than relying on the now-stale index
        # in the order tag").
        self.order_kind = {}
        # order id -> (symbol, industry, sector): the Unit-cap headroom an
        # entry or Add order reserved at placement (rules.SessionLedger).
        # Committed (popped, kept reserved) the moment the order fully
        # fills; released (popped, and unit_caps.remove called) the moment
        # it is cancelled or LEAN refuses it outright -- otherwise every
        # unfilled proposal exhausts its Unit's headroom forever (PR #253
        # review, Greptile rules.py:841 and CodeRabbit main.py:411,
        # Critical: "Unfilled orders exhaust Unit caps").
        self.reservations_by_order_id = {}
        # entry/Add order id -> the whole-share quantity REQUESTED at
        # placement (always positive: a buy). Compared against what an
        # order actually settles at (OnOrderEvent, via
        # ticket.QuantityFilled) so a Unit is only ever opened, or added
        # to, at the exact size it was sized for -- never a smaller one
        # from a partial fill (PR #253 review, Greptile main.py:841 and
        # CodeRabbit main.py:875: "Partial buys become Units").
        self.requested_quantity_by_order_id = {}
        # Order ids this run has already logged one ANOMALY line for
        # (OnOrderEvent's own PartiallyFilled handling), so a single
        # stubborn order cannot flood the log with one line per event
        # while it works towards resolving.
        self._partial_fill_logged = set()
        self.slippage_model = _n_slippage_model(self.n_by_order_id)
        self.fill_model = _adr_0005_fill_model(self.slippage_model)()

        self.equity_curve = []           # (utc datetime, equity), once per Session
        self.closed_campaigns = []       # list of dicts: {r_multiple, win}
        self.total_commission = 0.0

        # ADR 0012's seven Regime Windows: [start, next-year-start) UTC.
        self.regime_windows = {
            "1998-2000 late bull": (date(1998, 1, 1), date(2001, 1, 1)),
            "2000-02 bear": (date(2000, 1, 1), date(2003, 1, 1)),
            "2003-07 bull": (date(2003, 1, 1), date(2008, 1, 1)),
            "2008-09 crash": (date(2008, 1, 1), date(2010, 1, 1)),
            "2009-19 bull": (date(2009, 1, 1), date(2020, 1, 1)),
            "2020 COVID": (date(2020, 1, 1), date(2021, 1, 1)),
            "2022 correction": (date(2022, 1, 1), date(2023, 1, 1)),
        }

        self.SetWarmUp(WARMUP_BARS, Resolution.Daily)

    # -------------------------------------------------------------------
    # Universe (ADR 0009): coarse dollar-volume/price filter, refreshed
    # only on the first trading day of each month; fine filter for common
    # stock and Morningstar sector/industry classification (ADR 0008). See
    # README.md, "Deviations", for what the common-stock filter cannot yet
    # verify on the Free plan (ADR/SPAC exclusion).
    # -------------------------------------------------------------------

    def CoarseSelectionFunction(self, coarse):
        current = self.Time.date()
        if not rules.is_new_eligibility_month(self._last_eligibility_month, current):
            return Universe.Unchanged
        self._last_eligibility_month = current
        filtered = [c for c in coarse
                    if c.HasFundamentalData
                    and c.Price >= rules.UNIVERSE_MIN_PRICE
                    and c.DollarVolume >= rules.UNIVERSE_MIN_DOLLAR_VOLUME]
        filtered.sort(key=lambda c: c.DollarVolume, reverse=True)
        return [c.Symbol for c in filtered[:self.UNIVERSE_SIZE]]

    def FineSelectionFunction(self, fine):
        # ADR 0009: common stock only, best effort. "ST00000001" is
        # Morningstar's own common-stock code in QuantConnect's Fundamental
        # data set; this is the one part of the universe filter this
        # script cannot independently verify without a live QuantConnect
        # session (README.md, "Deviations" -- which also names the log
        # line just below as how to confirm it on the first real run).
        selected = []
        kept = dropped = 0
        for f in fine:
            if getattr(getattr(f, "SecurityReference", None), "SecurityType", None) != "ST00000001":
                dropped += 1
                continue
            kept += 1
            # ADR 0008: recorded now, read (and frozen) only when a
            # Campaign later opens in this symbol (_decide_entry,
            # OnOrderEvent) -- never re-read by an already-open Campaign.
            self._classification[f.Symbol] = _classification_groups(f)
            selected.append(f.Symbol)
        self.Log("research: universe common-stock filter (SecurityType=='ST00000001') kept {} "
                 "dropped {} of {} fine candidates this month".format(kept, dropped, kept + dropped))
        return selected

    def OnSecuritiesChanged(self, changes):
        for security in changes.AddedSecurities:
            security.SetSlippageModel(self.slippage_model)
            security.SetFeeModel(InteractiveBrokersFeeModel())
            security.SetFillModel(self.fill_model)
            is_new = security.Symbol not in self.symbol_state
            if is_new:
                self.symbol_state[security.Symbol] = _SymbolState()
                # PR #253 review, Greptile main.py:360: a stock the
                # universe selects only after the algorithm's own start
                # has none of its own history yet, so it could never clear
                # ADR 0009's >= 250-bar requirement, or warm up N or the
                # channels, however long it then remains selected.
                # Backfill it immediately with QuantConnect's own History.
                self._warm_up_new_symbol(security.Symbol)
        for security in changes.RemovedSecurities:
            state = self.symbol_state.get(security.Symbol)
            # ADR 0009: losing eligibility never closes an open Campaign,
            # and never un-happens a fill LEAN reports anyway (the
            # broker's reality is authoritative). symbol_state is
            # therefore never deleted here -- only an entry proposal no
            # longer eligible to open a NEW Campaign is cancelled, and its
            # Unit-cap reservation released (_cancel_ticket). Deleting the
            # state risked a stray late fill for an order this method had
            # already "released" arriving with nowhere to be recorded,
            # silently leaking a real position out of every cap and out of
            # the equity/Campaign accounting alike.
            if state is not None and state.campaign is None:
                self._cancel_ticket(state.entry_ticket)
                state.entry_ticket = None

    def _warm_up_new_symbol(self, symbol):
        """Backfill a newly-selected symbol's indicators from QuantConnect's
        own History, in the same split-adjusted view OnData itself reads
        (ADR 0004), so a stock the universe only selects well into the run
        is not permanently unable to satisfy ADR 0009's 250-bar history
        floor or warm up N/the channels (PR #253 review, Greptile
        main.py:360). Feeds each historical bar through the same
        evaluate-then-advance path a live bar would (_advance), so this
        symbol's state is indistinguishable, once the backfill is done,
        from one that had been tracked from the algorithm's own start.

        _advance itself refuses a bar dated on or before the last one it
        already advanced through, so a History row that happens to cover
        a date OnData has already delivered (or will independently
        re-deliver during the algorithm's own SetWarmUp) is silently
        skipped rather than counted twice (PR #253 review, Greptile
        main.py:413: "Warm-up bars counted twice").
        """
        state = self.symbol_state[symbol]
        history = self.History([symbol], WARMUP_BARS, Resolution.Daily,
                               dataNormalizationMode=DataNormalizationMode.SplitAdjusted)
        if history is None or len(history) == 0:
            return
        try:
            rows = history.loc[symbol]
        except (KeyError, TypeError):
            return
        for bar_time, row in rows.iterrows():
            bar = SimpleNamespace(High=float(row["high"]), Low=float(row["low"]),
                                  Close=float(row["close"]), Volume=float(row.get("volume", 0.0)))
            self._advance(state, bar, bar_time.date())

    # -------------------------------------------------------------------
    # The daily Session (ADR 0021): QuantConnect delivers every security's
    # bar for one trading day in a single Slice, so the Slice itself IS
    # the Session boundary the Go engine needs a separate
    # market.session.closed event to construct. Order within a Session
    # (ADR 0010/0021): exits are resting orders LEAN already filled before
    # this Slice is delivered (see OnOrderEvent); Adds are decided next,
    # in ascending symbol order; entries are ranked and decided last.
    # -------------------------------------------------------------------

    def OnData(self, slice_):
        # Read now, appended later (after the warm-up check) so the SPY
        # curve covers exactly the Sessions equity_curve does -- both, or
        # neither (PR #253 review, CodeRabbit main.py:384: "Record SPY
        # marks only after warm-up ends").
        spy_bar = slice_.Bars.get(self.spy)

        bar_date = self.Time.date()

        if self.IsWarmingUp:
            for symbol, bar in slice_.Bars.items():
                state = self.symbol_state.get(symbol)
                if state is not None:
                    self._advance(state, bar, bar_date)
            return

        # ADR 0011: "a Signal belongs to one bar and expires with it" -- an
        # entry or ordinary Add proposal stays outstanding only until its
        # instrument's next bar. By the time OnData is called for this
        # Session, LEAN has already reported this Session's fills for
        # every order resting from the last one (adapter/lean/algorithm.py
        # observes this LEAN ordering too), so an entry_ticket/add_ticket
        # still set here did NOT fill against this Session's bar and must
        # now expire, rather than rest indefinitely at an increasingly
        # stale level.
        for symbol, bar in slice_.Bars.items():
            state = self.symbol_state.get(symbol)
            if state is None:
                continue
            if state.entry_ticket is not None:
                self._cancel_ticket(state.entry_ticket)
                state.entry_ticket = None
            if state.add_ticket is not None:
                self._cancel_ticket(state.add_ticket)
                state.add_ticket = None

        add_opportunities = []   # [(symbol, state, rung)], ascending symbol order
        candidates = {}          # symbol-string -> (symbol, state, entry_level, entry_n)

        for symbol, bar in sorted(slice_.Bars.items(), key=lambda item: str(item[0])):
            state = self.symbol_state.get(symbol)
            if state is None:
                continue
            # Evaluate-then-add (CONTEXT.md "Completed bar"): read every
            # channel/N BEFORE folding today's bar into them.
            entry_extreme, entry_ready = state.entry_channel.extreme()
            exit_extreme, exit_ready = state.exit_channel.extreme()
            pre_advance_n = state.n.value if state.n.ready else None
            is_breakout = (entry_ready and pre_advance_n and pre_advance_n > 0
                          and float(bar.High) > entry_extreme and state.entry_ticket is None
                          and state.campaign is None)

            if state.campaign is not None:
                self._maintain_exit_orders(state, exit_extreme if exit_ready else None)
                rung = state.campaign.next_add_rung()
                if rung is not None and state.add_ticket is None and float(bar.High) >= rung:
                    add_opportunities.append((symbol, state, rung))

            self._advance(state, bar, bar_date)

            if is_breakout:
                # Strength reads THIS Session's own close and N, once the
                # bar has been folded in (ADR 0010, as amended 2026-09-25);
                # the entry LEVEL and its own N (entry_extreme,
                # pre_advance_n) stay pre-advance, since ADR 0005 needs
                # them computable before this Session opens.
                strength_value, strength_ready = rules.strength(state.closes, state.n.value)
                dv_value, dv_ready = rules.median_dollar_volume(state.raw_closes, state.raw_volumes)
                if strength_ready and dv_ready:
                    key = str(symbol)
                    candidates[key] = (symbol, state, entry_extreme, pre_advance_n,
                                       rules.Signal(key, strength_value, dv_value))
                # else: cannot be ranked; the Signal is declined (ADR 0010).

        available_cash = float(self.Portfolio.Cash)
        ledger = rules.SessionLedger(available_cash, self.unit_caps)

        # 1. Adds, ascending symbol order (ADR 0021 section 3, step 1).
        for symbol, state, rung in add_opportunities:
            self._decide_add(state, symbol, rung, ledger)

        # 2. Entries, ranked by Strength/dollar-volume/symbol (ADR 0010,
        # ADR 0021 section 3, step 2).
        ranked = rules.rank_signals(entry[4] for entry in candidates.values())
        for signal in ranked:
            symbol, state, entry_level, entry_n, _ = candidates[signal.symbol]
            self._decide_entry(state, symbol, entry_level, entry_n, ledger)

        equity = float(self.Portfolio.TotalPortfolioValue)
        self.notional_account.observe(self.Time.date(), equity)
        self.equity_curve.append((self.Time, equity))
        if spy_bar is not None:
            self.spy_curve.append((self.Time, float(spy_bar.Close)))

    def _advance(self, state, bar, bar_date):
        # PR #253 review, Greptile main.py:413 ("Warm-up bars counted
        # twice"): refuse a bar dated on or before the last one already
        # advanced, so History's own backfill (_warm_up_new_symbol) and
        # the algorithm's own warm-up/live bar delivery can never both
        # feed the same calendar day into N/the channels. At Daily
        # resolution one bar per Session shares its whole date with its
        # own end time, so comparing dates is exactly comparing bar end
        # times here.
        if state.last_bar_date is not None and bar_date <= state.last_bar_date:
            return
        high, low, close = float(bar.High), float(bar.Low), float(bar.Close)
        tr = rules.true_range(high, low, state.previous_close)
        state.n.add(tr)
        state.entry_channel.add(high)
        state.exit_channel.add(low)
        raw_close = float(getattr(bar, "Close", close))
        raw_volume = float(getattr(bar, "Volume", 0.0))
        state.record_close(raw_close, raw_volume, close)
        state.previous_close = close
        state.bars_seen += 1
        state.last_bar_date = bar_date

    # -------------------------------------------------------------------
    # Deciding Adds and entries (ADR 0005's price cap, ADR 0013's
    # slippage, ADR 0009's eligibility, ADR 0008's caps, ADR 0020's
    # affordability -- all via rules.py).
    # -------------------------------------------------------------------

    def _decide_add(self, state, symbol, rung, ledger):
        n = state.campaign.campaign_n
        quantity = state.campaign.unit_quantity
        cap = rules.price_cap(rung, n)
        slip = rules.slippage(n)
        commission = rules.commission_estimate(quantity, cap)
        # ADR 0008: an Add checks against the CAMPAIGN's own frozen
        # industry/sector, never a fresh classification lookup -- the same
        # freeze-at-entry discipline ADR 0006 applies to N and Unit size.
        industry, sector = state.campaign.industry, state.campaign.sector
        accepted, reason = ledger.try_reserve(str(symbol), industry, sector, quantity, cap, slip,
                                              1.0, commission)
        if not accepted:
            self.Log("research: {} add declined at rung {:.4f}: {}".format(symbol, rung, reason))
            return
        tag = "add:{}".format(symbol)
        ticket = self.StopLimitOrder(symbol, quantity, rung, self._round_tick(symbol, cap), tag=tag)
        self.n_by_order_id[ticket.OrderId] = n
        self.order_kind[ticket.OrderId] = "add"
        self.reservations_by_order_id[ticket.OrderId] = (str(symbol), industry, sector)
        self.requested_quantity_by_order_id[ticket.OrderId] = quantity
        state.add_ticket = ticket

    def _decide_entry(self, state, symbol, entry_level, n, ledger):
        # ADR 0009's eligibility test: common stock (best effort -- see
        # README.md, "Deviations"; FineSelectionFunction already filtered
        # by security type), price >= $5, 20-day median dollar volume >=
        # $5M, and >= 250 completed bars of history.
        dv_value, dv_ready = rules.median_dollar_volume(state.raw_closes, state.raw_volumes)
        eligible = dv_ready and rules.is_eligible(
            float(self.Securities[symbol].Price), dv_value, state.bars_seen, is_common_stock=True)
        if not eligible:
            self.Log("research: {} entry declined: ineligible".format(symbol))
            return
        # PR #253 review, Greptile main.py:603: decline BEFORE placing an
        # order whose own fill would leave rules.protective_stop_level
        # unable to compute a positive initial stop (The Turtle Rules
        # p.22: entry_level - Stop Multiple x N must be positive). A fill
        # at or above entry_level only widens this margin, so checking the
        # level here is the conservative, sufficient bound -- and it must
        # be checked here, before submission, never left to raise inside
        # OnOrderEvent, which would abort the whole backtest.
        if entry_level - rules.STOP_MULTIPLE * n <= 0:
            self.Log("research: {} entry declined: level {:.4f} minus {}xN ({:.4f}) is not "
                     "positive".format(symbol, entry_level, rules.STOP_MULTIPLE,
                                       rules.STOP_MULTIPLE * n))
            return
        notional = self.notional_account.current
        quantity = rules.unit_quantity(notional, rules.UNIT_VOLATILITY_FRACTION, n)
        if quantity <= 0:
            self.Log("research: {} entry declined: sizes to fewer than one share".format(symbol))
            return
        cap = rules.price_cap(entry_level, n)
        slip = rules.slippage(n)
        commission = rules.commission_estimate(quantity, cap)
        # ADR 0008: the classification read HERE, at proposal time, is the
        # one the resulting Campaign freezes at its fill (OnOrderEvent) --
        # one reading, via classification_by_order_id, never two separate
        # lookups a mid-flight monthly reclassification could pull apart.
        industry, sector = self._classification.get(symbol, (None, None))
        accepted, reason = ledger.try_reserve(str(symbol), industry, sector, quantity, cap, slip,
                                              1.0, commission)
        if not accepted:
            self.Log("research: {} entry declined at level {:.4f}: {}".format(symbol, entry_level, reason))
            return
        tag = "entry:{}:{}".format(symbol, n)
        ticket = self.StopLimitOrder(symbol, quantity, entry_level, self._round_tick(symbol, cap), tag=tag)
        self.n_by_order_id[ticket.OrderId] = n
        self.order_kind[ticket.OrderId] = "entry"
        self.classification_by_order_id[ticket.OrderId] = (industry, sector)
        self.reservations_by_order_id[ticket.OrderId] = (str(symbol), industry, sector)
        self.requested_quantity_by_order_id[ticket.OrderId] = quantity
        state.entry_ticket = ticket

    def _round_tick(self, symbol, price):
        tick = float(self.Securities[symbol].SymbolProperties.MinimumPriceVariation)
        if tick <= 0:
            return price
        return (int(price / tick)) * tick

    # -------------------------------------------------------------------
    # Exit Orders (ADR 0005's amendment): one resting sell order per Unit,
    # at the higher of its own Protective Stop and, while an Exit-Channel
    # exit is available, the Exit Channel level.
    # -------------------------------------------------------------------

    def _maintain_exit_orders(self, state, exit_extreme):
        campaign = state.campaign
        while len(state.unit_tickets) < len(campaign.units):
            state.unit_tickets.append(None)
        symbol_tag = getattr(campaign.symbol, "Value", campaign.symbol)
        for index, unit in enumerate(campaign.units):
            level = rules.exit_order_level(unit["stop"], exit_extreme)
            level = self._round_tick(campaign.symbol, level)
            ticket = state.unit_tickets[index]
            if ticket is None:
                # The tag is a label for LEAN's own order blotter only:
                # WHICH Unit an exit order belongs to is looked up by
                # OrderId (_unit_index_for_order), never parsed back out of
                # this string, so a later index shift (a Unit closing)
                # cannot make it stale (PR #253 review, Greptile
                # main.py:647 and CodeRabbit main.py:647).
                tag = "exit:{}".format(symbol_tag)
                ticket = self.StopMarketOrder(campaign.symbol, -campaign.unit_quantity, level, tag=tag)
                self.n_by_order_id[ticket.OrderId] = campaign.campaign_n
                self.order_kind[ticket.OrderId] = "exit"
                state.unit_tickets[index] = ticket
            else:
                fields = UpdateOrderFields()
                fields.StopPrice = level
                ticket.Update(fields)

    def _unit_index_for_order(self, state, order_id):
        """Which of campaign.units (by position) this order_id's resting
        Exit Order belongs to, found by identity in state.unit_tickets --
        never by parsing an index out of the order's own tag, which a
        earlier Unit's closure can shift (PR #253 review, Greptile
        main.py:647 and CodeRabbit main.py:647)."""
        for index, ticket in enumerate(state.unit_tickets):
            if ticket is not None and ticket.OrderId == order_id:
                return index
        return None

    def _cancel_ticket(self, ticket):
        """Cancel a resting order, if it is still open.

        If NOTHING has traded under it yet (``ticket.QuantityFilled == 0``
        -- true for the overwhelming majority of cancellations: ADR 0011
        expiry, a universe removal, or abandoning a resting Add on an
        exit), its Unit-cap reservation is released right now: a
        cancelled or otherwise abandoned proposal must give back the
        headroom it claimed at placement, or unfilled proposals silently
        exhaust every cap over the life of a run (PR #253 review, Greptile
        rules.py:841 and CodeRabbit main.py:411, Critical).

        If something HAS already filled under it -- an ANOMALY this
        fill-model combination is not expected to produce (OnOrderEvent's
        own PartiallyFilled handling) -- the reservation is left standing
        on purpose: OnOrderEvent's own settlement of the resulting
        Canceled event is what decides, exactly once, whether to commit
        or release it (PR #253 review, Greptile main.py:726: "Partial
        fills lose cap reservations" -- releasing it here as well would
        make _settle_entry/_settle_add's own release a second, needless
        one, which is harmless by itself, but ONLY because nothing else
        depends on release happening exactly once elsewhere; leaving it
        to the one settlement path keeps that invariant obviously true
        rather than incidentally true).
        """
        if ticket is None:
            return
        if ticket.Status not in (OrderStatus.Filled, OrderStatus.Canceled, OrderStatus.Invalid):
            ticket.Cancel()
        if ticket.QuantityFilled == 0:
            self._release_reservation(ticket.OrderId)

    def _release_reservation(self, order_id):
        """Give back the Unit-cap headroom order_id's reservation was
        holding, if any. Idempotent (pop-based): calling it twice for the
        same order, from two different code paths, is harmless."""
        entry = self.reservations_by_order_id.pop(order_id, None)
        if entry is not None:
            symbol, industry, sector = entry
            self.unit_caps.remove(symbol, industry, sector, units=1)

    def _commit_reservation(self, order_id):
        """An order that fully fills converts its Unit-cap reservation into
        a real, committed Unit: stop tracking it as a pending reservation
        (so a later, unrelated cancellation can never release it), but do
        NOT call unit_caps.remove -- the Unit stays counted in unit_caps
        until its own exit (_handle_unit_exit) releases it."""
        self.reservations_by_order_id.pop(order_id, None)

    # -------------------------------------------------------------------
    # Fills (OnOrderEvent): opening/adding to a Campaign, and closing Units
    # out of one. rules.Campaign owns the ladder arithmetic; this method
    # only recognises which order filled and what it means.
    # -------------------------------------------------------------------

    def OnOrderEvent(self, order_event):
        """PartiallyFilled is treated as an ANOMALY, not a routine case to
        build accumulation machinery for (PR #253 review round 2,
        simplifying round 1's response to Greptile main.py:585): on daily
        equity data, this algorithm's own ADR 0005 buy fill model
        (_stop_limit_buy_fill_price returns a price or None -- never a
        partial quantity) and LEAN's own native stop-market sell fill
        (used for every Exit Order) each fill an order's WHOLE requested
        quantity or nothing. A PartiallyFilled event is logged, once, in
        full, and otherwise ignored: the order simply keeps resting, and
        is handled the ordinary way once LEAN reports it Filled or
        Canceled -- at which point ``order_event.Ticket`` (LEAN's own
        order-ticket object) already knows the TOTAL quantity filled
        across every partial fill it took (``QuantityFilled``) and the
        volume-weighted average price across them (``AverageFillPrice``),
        so this method never accumulates either figure itself.
        """
        order_id = order_event.OrderId
        status = order_event.Status
        ticket = order_event.Ticket

        if order_event.FillQuantity:
            # A real execution occurred in THIS event (Filled,
            # PartiallyFilled, or the last increment of a Canceled order);
            # the fee LEAN charged for it is real regardless of what
            # happens to the rest of the order.
            self.total_commission += float(order_event.OrderFee.Value.Amount)

        if status == OrderStatus.Invalid:
            # LEAN refused the order outright -- its own affordability or
            # margin check disagreed with this script's own pre-check (ADR
            # 0020). Nothing was bought; give back its Unit-cap headroom
            # (PR #253 review, Greptile rules.py:841 and CodeRabbit
            # main.py:411, Critical) rather than leaving it reserved
            # forever, and forget the now-dead ticket.
            self._release_reservation(order_id)
            self._forget_ticket(order_event.Symbol, order_id)
            return

        if status == OrderStatus.PartiallyFilled:
            if order_id not in self._partial_fill_logged:
                self._partial_fill_logged.add(order_id)
                self.Log("research: ANOMALY: {} order {} partially filled ({} of {} shares so "
                         "far, at {}); this fill-model combination is not expected to do this -- "
                         "continuing to wait for it to resolve".format(
                             order_event.Symbol, order_id, ticket.QuantityFilled, ticket.Quantity,
                             order_event.FillPrice))
            return

        if status not in (OrderStatus.Filled, OrderStatus.Canceled):
            return

        self._partial_fill_logged.discard(order_id)
        # abs(): QuantityFilled carries the order's own sign (negative for
        # a sell -- every Exit Order), and every quantity this algorithm
        # and rules.py deal in from here on is an unsigned share count
        # (PR #253 review, Greptile main.py:834: "Exit fills are
        # discarded" -- a prior guard here treated a negative quantity as
        # no fill at all, so a Unit's own Exit Order never actually closed
        # it).
        quantity = abs(int(ticket.QuantityFilled))
        if quantity == 0:
            # The ordinary Canceled case (ADR 0011 expiry, or a universe
            # removal): _cancel_ticket already released this order's
            # reservation, and nothing ever traded under it.
            self._forget_ticket(order_event.Symbol, order_id)
            return

        price = float(ticket.AverageFillPrice)
        try:
            self._settle_order(order_event, quantity, price)
        except Exception as err:
            # PR #253 review, Greptile main.py:603 ("High volatility fills
            # abort backtests") and CodeRabbit main.py:612, Critical: a
            # fill handler must never raise. LEAN does not guard a
            # Python exception raised from OnOrderEvent the way it guards
            # one from a fill or slippage model, so an uncaught one aborts
            # the WHOLE backtest over one instrument's edge case. Log it
            # and let the run continue; this defends every branch below,
            # not only the specific cases already handled explicitly.
            self.Log("research: {} order event could not be settled: {}".format(
                order_event.Symbol, err))

    def _forget_ticket(self, symbol, order_id):
        """Clear a state's own reference to order_id if it still holds one
        (an entry/Add order LEAN refused outright or that resolved with
        nothing filled), so a later Session's ADR 0011 expiry loop does
        not try to cancel an order that no longer needs it."""
        state = self.symbol_state.get(symbol)
        if state is None:
            return
        if state.entry_ticket is not None and state.entry_ticket.OrderId == order_id:
            state.entry_ticket = None
        if state.add_ticket is not None and state.add_ticket.OrderId == order_id:
            state.add_ticket = None

    def _settle_order(self, order_event, quantity, price):
        """Act on an order LEAN reports as fully resolved (Filled), or as
        Canceled after the ANOMALY of a partial fill (OnOrderEvent's own
        doc comment): ``quantity``/``price`` are LEAN's own ticket-level
        totals across the order's whole life, never assumed to equal the
        original request (PR #253 review, Greptile main.py:585 and its
        follow-ups)."""
        symbol = order_event.Symbol
        state = self.symbol_state.get(symbol)
        order_id = order_event.OrderId
        if state is None:
            self._release_reservation(order_id)
            return
        kind = self.order_kind.pop(order_id, None)

        if kind == "entry":
            self._settle_entry(state, symbol, order_id, quantity, price)
        elif kind == "add":
            self._settle_add(state, symbol, order_id, quantity, price)
        elif kind == "exit":
            self._handle_unit_exit(state, symbol, order_id, quantity, price)

    def _settle_entry(self, state, symbol, order_id, quantity, price):
        # Cleared unconditionally, before any branch below: this order is
        # now fully resolved one way or the other, and state.entry_ticket
        # must never keep pointing at it -- a resting reference to an
        # already-resolved order would permanently read as "still
        # awaiting this Session's decision" and block every future entry
        # attempt for this symbol.
        state.entry_ticket = None
        requested = self.requested_quantity_by_order_id.pop(order_id, None)
        n = self.n_by_order_id.pop(order_id, None)
        industry, sector = self.classification_by_order_id.pop(order_id, (None, None))
        if requested is not None and quantity != requested:
            # ANOMALY (OnOrderEvent's own doc comment): never open a Unit
            # smaller than the one this proposal's Unit-cap reservation
            # and cash check were sized for -- decline it exactly as ADR
            # 0010's "no partial Units" already requires, by selling back
            # whatever quantity did trade (PR #253 review, Greptile
            # main.py:841 and CodeRabbit main.py:875: "Partial buys
            # become Units").
            self._release_reservation(order_id)
            self.MarketOrder(symbol, -quantity, tag="entry-partial-liquidate:{}".format(symbol))
            self.Log("research: ANOMALY: {} entry order {} filled {} of {} requested shares; "
                     "declined and liquidated".format(symbol, order_id, quantity, requested))
            return
        # PR #253 review, Greptile main.py:603 ("High volatility fills
        # abort backtests"): the pre-placement check in _decide_entry
        # already declines an entry level too close to 2N of itself; this
        # is defence in depth against the ACTUAL fill price (slippage, a
        # gap, or a missing N) still leaving the initial stop non-positive.
        # Never let opening the Campaign raise -- decline the fill by
        # selling the shares straight back out instead.
        if n is None or price - rules.STOP_MULTIPLE * n <= 0:
            self._release_reservation(order_id)
            self.MarketOrder(symbol, -quantity, tag="entry-declined-liquidate:{}".format(symbol))
            self.Log("research: {} entry fill at {} could not open a Campaign (N={}); "
                     "liquidated immediately".format(symbol, price, n))
            return
        # ADR 0008: the SAME (industry, sector) reading _decide_entry's own
        # Unit-cap check already used, frozen onto the Campaign now and
        # never re-read afterward. Construct the Campaign, THEN commit the
        # reservation: if rules.Campaign's own construction were somehow to
        # raise despite the check above, the reservation must stay pending
        # (recoverable by a later cancellation) rather than being marked
        # committed against a Campaign that was never actually created.
        state.campaign = rules.Campaign(symbol, price, n, quantity, industry=industry, sector=sector)
        self._commit_reservation(order_id)
        state.unit_tickets = []
        self._maintain_exit_orders(state, None)

    def _settle_add(self, state, symbol, order_id, quantity, price):
        state.add_ticket = None
        requested = self.requested_quantity_by_order_id.pop(order_id, None)
        campaign = state.campaign
        if requested is not None and quantity != requested:
            # ANOMALY: see _settle_entry's own identical check. An Add
            # must add exactly the frozen Unit size (ADR 0006) or not at
            # all -- never a smaller one.
            self._release_reservation(order_id)
            self.MarketOrder(symbol, -quantity, tag="add-partial-liquidate:{}".format(symbol))
            self.Log("research: ANOMALY: {} Add order {} filled {} of {} requested shares; "
                     "declined and liquidated".format(symbol, order_id, quantity, requested))
            return
        if campaign is not None and not campaign.loaded and not campaign.partially_stopped:
            # add_unit BEFORE committing the reservation, for the identical
            # reason _settle_entry orders its own two calls this way.
            campaign.add_unit(price)
            self._commit_reservation(order_id)
            self._maintain_exit_orders(state, None)
            return
        # PR #253 review, CodeRabbit main.py:612, Critical ("Handle an Add
        # fill that arrives after a partial or full stop-out"): the
        # Campaign this Add was meant for has already fully closed, or
        # already stopped a Unit out, since the order was placed -- a
        # same-bar stop-then-add race, or a bar wide enough to touch both
        # the Add rung and a stop. The fill is real; selling it straight
        # back out is the fail-safe response, never silently dropping it
        # (which would leave real shares LEAN holds untracked) and never
        # raising inside add_unit (which would abort the backtest).
        self._release_reservation(order_id)
        self.MarketOrder(symbol, -quantity, tag="add-orphan-liquidate:{}".format(symbol))
        self.Log("research: {} Add fill at {} arrived with no Campaign able to take it; "
                 "liquidated immediately".format(symbol, price))

    def _handle_unit_exit(self, state, symbol, order_id, quantity, price):
        campaign = state.campaign
        if campaign is None:
            return
        # Which Unit this specific resting order belonged to, found by
        # identity (never by parsing an index out of its own tag, which an
        # earlier Unit's own closure would have made stale -- PR #253
        # review, Greptile main.py:647 and CodeRabbit main.py:647).
        unit_index = self._unit_index_for_order(state, order_id)
        if unit_index is None or quantity != campaign.unit_quantity:
            # ANOMALY (OnOrderEvent's own doc comment): either this
            # order's own Unit could not be identified at all (a bug this
            # script has no safe guess for), or it settled a quantity
            # different from that Unit's own recorded size -- this
            # fill-model combination is not expected to partially fill a
            # Unit's own Exit Order, and this script has no
            # representation for a Unit smaller than a whole one. Failing
            # closed -- leaving campaign.units, unit_tickets and the caps
            # exactly as they were, and mutating nothing -- never
            # silently mis-states the Campaign's own result or its
            # remaining Unit caps (PR #253 review, CodeRabbit main.py:952:
            # "Do not close a Unit on a partial canceled exit"). This
            # script's own code never itself cancels a Unit's Exit Order
            # (only a resting Add, below, and never mid-fill), so this
            # path can only follow from a cause outside a backtest this
            # script fully controls.
            self.Log("research: ANOMALY: {} exit order {} settled {} shares (expected {} for "
                     "unit index {}); refusing to guess -- this Unit's own bookkeeping is left "
                     "unchanged".format(symbol, order_id, quantity, campaign.unit_quantity,
                                        unit_index))
            return

        # PR #253 review, CodeRabbit main.py:612, Critical: a resting Add
        # must not survive ANY exit fill, partial or full, not only the
        # Campaign's very last one -- the Campaign it was meant to extend
        # may no longer be able to take it the moment ANY Unit closes (ADR
        # 0012: no further Add once a Campaign is partially stopped).
        self._cancel_ticket(state.add_ticket)
        state.add_ticket = None

        # ADR 0008: a closed Unit frees its own instrument/industry/sector/
        # total-long headroom, against the SAME frozen classification it
        # was reserved under (campaign.industry/.sector) -- otherwise the
        # caps only ever fill up over a run and never reflect what is
        # actually still open.
        self.unit_caps.remove(str(symbol), campaign.industry, campaign.sector, units=1)
        # PR #253 review, Greptile main.py:649 ("Campaign results omit
        # earlier Units"): realise this closed Unit's own (exit - fill)
        # into the Campaign's running total; r_multiple() is read only
        # once the whole Campaign has closed, below.
        campaign.close_units([unit_index], price)
        # Keep state.unit_tickets exactly aligned with campaign.units: the
        # ONE ticket at this SAME index is deleted together with it (PR
        # #253 review, Greptile main.py:647 and CodeRabbit main.py:647).
        del state.unit_tickets[unit_index]

        if not campaign.units:
            r_multiple = campaign.r_multiple()
            self.closed_campaigns.append({"r_multiple": r_multiple, "win": r_multiple > 0})
            state.campaign = None
            state.unit_tickets = []

    # -------------------------------------------------------------------
    # The closing summary (issue's own required output).
    # -------------------------------------------------------------------

    def OnEndOfAlgorithm(self):
        self._log_span("OVERALL", self.equity_curve)
        for name, (start, end) in self.regime_windows.items():
            window = [(t, e) for t, e in self.equity_curve if start <= t.date() < end]
            self._log_span("REGIME " + name, window)

        wins = [c["r_multiple"] for c in self.closed_campaigns if c["win"]]
        losses = [c["r_multiple"] for c in self.closed_campaigns if not c["win"]]
        count = len(self.closed_campaigns)
        win_rate = (len(wins) / count) if count else None
        avg_win = (sum(wins) / len(wins)) if wins else None
        avg_loss = (sum(losses) / len(losses)) if losses else None
        self.Log("research: campaigns={} win_rate={} avg_win_R={} avg_loss_R={}".format(
            count, win_rate, avg_win, avg_loss))
        self.Log("research: total_commission={:.2f}".format(self.total_commission))

        self._log_span("SPY BUY-AND-HOLD", self.spy_curve)

    def _log_span(self, label, curve):
        if len(curve) < 2:
            self.Log("research: {}: no-data (fewer than two marks)".format(label))
            return
        start_time, start_equity = curve[0]
        end_time, end_equity = curve[-1]
        elapsed_days = (end_time - start_time).total_seconds() / 86400.0
        cagr = rules.annualised_return(start_equity, end_equity, elapsed_days)
        mdd = rules.max_drawdown([e for _, e in curve])
        ratio = rules.cagr_over_max_drawdown(cagr, mdd)
        self.Log("research: {}: start={} end={} CAGR={} max_drawdown={} CAGR/MaxDD={}".format(
            label, start_time.date(), end_time.date(), cagr, mdd, ratio))
