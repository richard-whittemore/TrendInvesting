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

# rules.py sits beside this file. QuantConnect's cloud IDE runs every file
# in one project folder, so this is normally unnecessary, but it mirrors
# adapter/lean/algorithm.py's own defensive import guard for local runs.
_HERE = dirname(abspath(__file__))
if _HERE not in path:
    path.insert(0, _HERE)

import rules  # noqa: E402


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
                 "bars_seen", "previous_close", "campaign", "entry_ticket", "add_ticket",
                 "unit_tickets", "industry", "sector")

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
        self.campaign = None            # rules.Campaign, or None
        self.entry_ticket = None        # the resting entry stop-limit order
        self.add_ticket = None          # the resting Add stop-limit order
        self.unit_tickets = []          # one resting sell order per open Unit
        self.industry = None            # always None: no classifier (README)
        self.sector = None              # always None: no classifier (README)

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
        self.spy = self.AddEquity(
            "SPY", Resolution.Daily, dataNormalizationMode=DataNormalizationMode.SplitAdjusted).Symbol
        self.spy_curve = []

        self.symbol_state = {}
        self._last_eligibility_month = None

        self.notional_account = rules.NotionalAccount(self.STARTING_CASH)
        self.unit_caps = rules.UnitCaps()
        self.n_by_order_id = {}
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

        self.SetWarmUp(max(rules.ENTRY_CHANNEL_LENGTH, rules.UNIVERSE_MIN_HISTORY_BARS), Resolution.Daily)

    # -------------------------------------------------------------------
    # Universe (ADR 0009): coarse dollar-volume/price filter, refreshed
    # only on the first trading day of each month; fine filter for common
    # stock. See README.md, "Deviations", "Universe classification", for
    # what this cannot verify on the Free plan (ADR/SPAC exclusion, and
    # industry/sector, hence every instrument being Unclassified under
    # ADR 0008).
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
        # session (README.md, "Deviations").
        return [f.Symbol for f in fine
                if getattr(getattr(f, "SecurityReference", None), "SecurityType", None) == "ST00000001"]

    def OnSecuritiesChanged(self, changes):
        for security in changes.AddedSecurities:
            security.SetSlippageModel(self.slippage_model)
            security.SetFeeModel(InteractiveBrokersFeeModel())
            security.SetFillModel(self.fill_model)
            if security.Symbol not in self.symbol_state:
                self.symbol_state[security.Symbol] = _SymbolState()
        for security in changes.RemovedSecurities:
            state = self.symbol_state.get(security.Symbol)
            # ADR 0009: losing eligibility never closes an open Campaign;
            # keep tracking state for any symbol still in one, drop it
            # otherwise. An entry/Add proposal still resting is cancelled,
            # since a new Campaign may not open in an ineligible name.
            if state is not None and state.campaign is None:
                self._cancel_ticket(state.entry_ticket)
                del self.symbol_state[security.Symbol]

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
        spy_bar = slice_.Bars.get(self.spy)
        if spy_bar is not None:
            self.spy_curve.append((self.Time, float(spy_bar.Close)))

        if self.IsWarmingUp:
            for symbol, bar in slice_.Bars.items():
                state = self.symbol_state.get(symbol)
                if state is not None:
                    self._advance(state, bar)
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

            self._advance(state, bar)

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

    def _advance(self, state, bar):
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
        accepted, reason = ledger.try_reserve(str(symbol), state.industry, state.sector, quantity,
                                              cap, slip, 1.0, commission)
        if not accepted:
            self.Log("research: {} add declined at rung {:.4f}: {}".format(symbol, rung, reason))
            return
        tag = "add:{}".format(symbol)
        ticket = self.StopLimitOrder(symbol, quantity, rung, self._round_tick(symbol, cap), tag=tag)
        self.n_by_order_id[ticket.OrderId] = n
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
        notional = self.notional_account.current
        quantity = rules.unit_quantity(notional, rules.UNIT_VOLATILITY_FRACTION, n)
        if quantity <= 0:
            self.Log("research: {} entry declined: sizes to fewer than one share".format(symbol))
            return
        cap = rules.price_cap(entry_level, n)
        slip = rules.slippage(n)
        commission = rules.commission_estimate(quantity, cap)
        accepted, reason = ledger.try_reserve(str(symbol), state.industry, state.sector, quantity,
                                              cap, slip, 1.0, commission)
        if not accepted:
            self.Log("research: {} entry declined at level {:.4f}: {}".format(symbol, entry_level, reason))
            return
        tag = "entry:{}:{}".format(symbol, n)
        ticket = self.StopLimitOrder(symbol, quantity, entry_level, self._round_tick(symbol, cap), tag=tag)
        self.n_by_order_id[ticket.OrderId] = n
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
        for index, unit in enumerate(campaign.units):
            level = rules.exit_order_level(unit["stop"], exit_extreme)
            level = self._round_tick(campaign.symbol, level)
            ticket = state.unit_tickets[index]
            if ticket is None:
                symbol_tag = getattr(campaign.symbol, "Value", campaign.symbol)
                tag = "exit:{}:{}:{}".format(symbol_tag, index, campaign.campaign_n)
                ticket = self.StopMarketOrder(campaign.symbol, -campaign.unit_quantity, level, tag=tag)
                self.n_by_order_id[ticket.OrderId] = campaign.campaign_n
                state.unit_tickets[index] = ticket
            else:
                fields = UpdateOrderFields()
                fields.StopPrice = level
                ticket.Update(fields)

    def _cancel_ticket(self, ticket):
        if ticket is not None and ticket.Status not in (OrderStatus.Filled, OrderStatus.Canceled,
                                                          OrderStatus.Invalid):
            ticket.Cancel()

    # -------------------------------------------------------------------
    # Fills (OnOrderEvent): opening/adding to a Campaign, and closing Units
    # out of one. rules.Campaign owns the ladder arithmetic; this method
    # only recognises which order filled and what it means.
    # -------------------------------------------------------------------

    def OnOrderEvent(self, order_event):
        if order_event.Status != OrderStatus.Filled and order_event.Status != OrderStatus.PartiallyFilled:
            return
        self.total_commission += float(order_event.OrderFee.Value.Amount)
        symbol = order_event.Symbol
        state = self.symbol_state.get(symbol)
        if state is None:
            return
        tag = order_event.Ticket.Tag or ""
        fill_price = float(order_event.FillPrice)

        if tag.startswith("entry:"):
            n = float(tag.split(":")[-1])
            quantity = int(order_event.FillQuantity)
            state.campaign = rules.Campaign(symbol, fill_price, n, quantity,
                                            industry=state.industry, sector=state.sector)
            state.entry_ticket = None
            state.unit_tickets = []
            self._maintain_exit_orders(state, None)

        elif tag.startswith("add:"):
            if state.campaign is not None:
                state.campaign.add_unit(fill_price)
            state.add_ticket = None
            self._maintain_exit_orders(state, None)

        elif tag.startswith("exit:"):
            self._handle_unit_exit(state, symbol, tag, fill_price)

    def _handle_unit_exit(self, state, symbol, tag, fill_price):
        campaign = state.campaign
        if campaign is None:
            return
        try:
            # "exit:{symbol}:{index}:{n}"; rsplit from the right so a
            # symbol string that happens to contain a colon cannot shift
            # which field is the index.
            _, index_str, _ = tag.split(":", 1)[1].rsplit(":", 2)
            unit_index = int(index_str)
        except (IndexError, ValueError):
            unit_index = None
        entry_price = campaign.entry_price()
        n = campaign.campaign_n
        # This one Unit's own R multiple: (exit - entry) / (stop distance),
        # i.e. in units of the Campaign's own initial 1-Unit risk (Stop
        # Multiple x N) -- a standard, ADR-consistent way to express a
        # trade's result independently of position size, used only for
        # this script's OWN reporting (not itself an ADR-cited rule).
        r_multiple = (fill_price - entry_price) / (rules.STOP_MULTIPLE * n)
        indices = [unit_index] if unit_index is not None and unit_index < len(campaign.units) else \
            list(range(len(campaign.units)))
        campaign.remove_units(indices)
        if unit_index is not None and unit_index < len(state.unit_tickets):
            state.unit_tickets[unit_index] = None
        if not campaign.units:
            self.closed_campaigns.append({"r_multiple": r_multiple, "win": r_multiple > 0})
            self._cancel_ticket(state.add_ticket)
            state.campaign = None
            state.add_ticket = None
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
