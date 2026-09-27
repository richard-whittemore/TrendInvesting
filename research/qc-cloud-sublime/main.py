"""Sublime Trading research check, for QuantConnect Cloud's Free plan.

Research-only: a Python re-implementation of the Sublime control as
docs/methodology/Methodology_Analysis.md records it, with no Go counterpart
to be checked against. It answers one question cheaply: does Sublime-style
trend trading on US stocks beat buying and holding SPY?

rules.py is the pure rule core; this file is QuantConnect wiring only:
universe selection, order placement, fill handling and the closing summary.
The wiring (fill, slippage and fee models, raw-share handling, dividends,
queued order events, settlement, retained symbols, re-placed Exit Orders,
the SPY benchmark, runtime statistics) is carried from research/qc-cloud,
whose README records why each piece exists; README.md here lists what
differs.
"""

from AlgorithmImports import *  # noqa: F401,F403

from datetime import date, datetime, timezone
from os.path import abspath, dirname
from sys import path
from types import SimpleNamespace

_HERE = dirname(abspath(__file__))
if _HERE not in path:
    path.insert(0, _HERE)

import rules  # noqa: E402

#: Daily bars a symbol needs before it can signal: the 5-year history floor
#: (Methodology_Analysis.md section 3.1 rule 3 [M p.54]), which also covers
#: the weekly 200 SMA. Used for SetWarmUp and a new selection's backfill.
WARMUP_BARS = rules.MIN_HISTORY_BARS


def _stop_limit_buy_fill_price(level, price_cap, open_, high, low, slippage_amount):
    """ADR 0005's stop-limit buy, as amended: triggers on a touch of
    ``level``, fills at max(level, open) within the cap, else works as a
    limit at the cap; slippage (ADR 0013) is added after the cap. None for
    no fill this bar. Copied from research/qc-cloud/main.py."""
    if high < level:
        return None
    if open_ <= price_cap:
        return max(level, open_) + slippage_amount
    if low > price_cap:
        return None
    return price_cap + slippage_amount


def _atr_slippage_model(atr_by_order_id):
    """ADR 0013's slippage, in ATR, on every order this algorithm placed;
    the ATR is recorded when the order is placed, never computed here."""

    class AtrSlippageModel:
        def GetSlippageApproximation(self, asset, order):
            atr = atr_by_order_id.get(order.Id)
            return 0.0 if atr is None else rules.slippage(atr)

        get_slippage_approximation = GetSlippageApproximation

    return AtrSlippageModel()


def _adr_0005_fill_model(slippage_model, fill_prices):
    """LEAN's EquityFillModel with ADR 0005's stop-limit fill for a buy;
    records each fill's price for the fee model, since LEAN's fee-model
    parameters carry none. Copied from research/qc-cloud/main.py."""

    class Adr0005FillModel(EquityFillModel):
        def StopMarketFill(self, asset, order):
            return self._record(order, super().StopMarketFill(asset, order))

        def MarketFill(self, asset, order):
            return self._record(order, super().MarketFill(asset, order))

        def _record(self, order, fill):
            if fill.FillQuantity:
                fill_prices[order.Id] = float(fill.FillPrice)
            return fill

        def StopLimitFill(self, asset, order):
            if order.Direction != OrderDirection.Buy:
                return self._record(order, super().StopLimitFill(asset, order))
            fill = OrderEvent(order, Extensions.ConvertToUtc(asset.LocalTime, asset.Exchange.TimeZone),
                              OrderFee.Zero)
            if order.Status == OrderStatus.Canceled or not self.IsExchangeOpen(asset, False):
                return fill
            prices = self.GetPricesCheckingPythonWrapper(asset, order.Direction)
            if Extensions.ConvertToUtc(prices.EndTime, asset.Exchange.TimeZone) <= order.Time:
                return fill
            try:
                price = _stop_limit_buy_fill_price(
                    float(order.StopPrice), float(order.LimitPrice), float(prices.Open),
                    float(prices.High), float(prices.Low),
                    float(slippage_model.GetSlippageApproximation(asset, order)))
            except Exception:  # fail closed: never guess a fill price.
                return fill
            if price is not None:
                fill.Status = OrderStatus.Filled
                fill.FillQuantity = order.Quantity
                fill.FillPrice = price
            return self._record(order, fill)

    return Adr0005FillModel


def _raw_share_fee_model(ratio_of, fill_prices):
    """LEAN's IB schedule charged on the raw shares an order is (ADR 0004,
    ADR 0013): orders are stated in split-adjusted shares, and LEAN's own
    model would charge per split-adjusted share."""

    class RawShareFeeModel(FeeModel):
        def GetOrderFee(self, parameters):
            security, order = parameters.Security, parameters.Order
            price = fill_prices.get(order.Id, float(security.Price))
            fee = rules.lean_ib_commission(float(order.AbsoluteQuantity), price,
                                           ratio_of(security.Symbol) or 1.0)
            return OrderFee(CashAmount(fee, "USD"))

    return RawShareFeeModel()


class _SymbolState:
    """Per-instrument state: indicators, the 4PS Setup, and orders."""

    __slots__ = ("ind", "setup", "campaign", "entry_ticket", "add_ticket", "unit_tickets")

    def __init__(self):
        self.ind = rules.Indicators()
        self.setup = rules.FourPhaseSetup()
        self.campaign = None
        self.entry_ticket = None
        self.add_ticket = None
        self.unit_tickets = []      # one Exit Order per held position


def _fmt(value):
    return "n/a" if value is None else "{:.4f}".format(value)


class SublimeResearch(QCAlgorithm):
    """The Sublime control over a monthly top-N US common-stock universe,
    1998 to today, on QuantConnect's Free plan (README.md)."""

    UNIVERSE_SIZE = 200
    STARTING_CASH = 1_000_000.0
    # A tuple of tickers replaces the monthly universe (local runs; local
    # data has no universe files). The span is (year, month, day); None for
    # END_DATE means today.
    FIXED_SYMBOLS = None
    START_DATE = (1998, 1, 1)
    END_DATE = None
    WARMUP_BARS = WARMUP_BARS

    def Initialize(self):
        self.SetStartDate(*self.START_DATE)
        end = self.END_DATE
        if end is None:
            today = datetime.now(timezone.utc).date()
            end = (today.year, today.month, today.day)
        self.SetEndDate(*end)
        self.SetCash(self.STARTING_CASH)
        self.SetTimeZone(TimeZones.NewYork)
        # Long-only, unleveraged cash account (ADR 0010).
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Cash)
        self.UniverseSettings.Resolution = Resolution.Daily
        self.UniverseSettings.Leverage = 1.0
        # ADR 0004: signals on split-adjusted prices; a fill-forward bar is
        # not a completed bar (CONTEXT.md).
        self.UniverseSettings.DataNormalizationMode = DataNormalizationMode.SplitAdjusted
        self.UniverseSettings.FillForward = False
        if self.FIXED_SYMBOLS is None:
            self.AddUniverse(self.CoarseSelectionFunction, self.FineSelectionFunction)
        else:
            for ticker in self.FIXED_SYMBOLS:
                self.AddEquity(ticker, Resolution.Daily, fillForward=False,
                               dataNormalizationMode=DataNormalizationMode.SplitAdjusted)
        # SPY: never traded. Its Adjusted (total-return) close is both the
        # buy-and-hold benchmark and the S&P 500 stand-in the regime reads
        # (README.md, "Deviations").
        self.spy = self.AddEquity("SPY", Resolution.Daily, fillForward=False,
                                  dataNormalizationMode=DataNormalizationMode.Adjusted).Symbol
        self.market = rules.Indicators()
        self.spy_curve = []

        self.symbol_state = {}
        self.split_ratio = {}           # symbol -> split-adjusted shares per raw share
        self._pending_cash = (self.STARTING_CASH, 0.0)
        self._basis_cash = None
        self._fill_debits = 0.0
        self._holds = {}                # entry/add order id -> worst-case cost
        self._order_events = []
        self._handling = False
        self._exit_prices = {}          # Exit Order id -> the price it rests at
        self._last_eligibility_month = None
        self.atr_by_order_id = {}
        self.order_kind = {}            # order id -> "entry" | "add" | "exit"
        self.requested = {}             # entry/add order id -> quantity
        self.slippage_model = _atr_slippage_model(self.atr_by_order_id)
        fill_prices = {}
        self.fill_model = _adr_0005_fill_model(self.slippage_model, fill_prices)()
        self.fee_model = _raw_share_fee_model(self.split_ratio.get, fill_prices)

        self.equity_curve = []
        self.closed_campaigns = []      # R multiples
        self.total_commission = 0.0
        # Counted, never logged one by one: the Free plan's 10 KB log limit.
        self.decline_counts = {}
        self.signal_counts = {}
        self._filter_logged = False
        # ADR 0012's Regime Windows, [start, next-year-start).
        self.regime_windows = {
            "1998-2000 late bull": (date(1998, 1, 1), date(2001, 1, 1)),
            "2000-02 bear": (date(2000, 1, 1), date(2003, 1, 1)),
            "2003-07 bull": (date(2003, 1, 1), date(2008, 1, 1)),
            "2008-09 crash": (date(2008, 1, 1), date(2010, 1, 1)),
            "2009-19 bull": (date(2009, 1, 1), date(2020, 1, 1)),
            "2020 COVID": (date(2020, 1, 1), date(2021, 1, 1)),
            "2022 correction": (date(2022, 1, 1), date(2023, 1, 1)),
        }
        self.SetWarmUp(self.WARMUP_BARS, Resolution.Daily)

    # ----- Universe: research/qc-cloud's monthly top-N common stock --------

    def CoarseSelectionFunction(self, coarse):
        current = self.Time.date()
        if not rules.is_new_eligibility_month(self._last_eligibility_month, current):
            return Universe.Unchanged
        self._last_eligibility_month = current
        filtered = [c for c in coarse
                    if c.HasFundamentalData and c.Price >= 5.0 and c.DollarVolume >= 5_000_000.0]
        filtered.sort(key=lambda c: c.DollarVolume, reverse=True)
        return [c.Symbol for c in filtered[:self.UNIVERSE_SIZE]]

    def FineSelectionFunction(self, fine):
        selected = [f.Symbol for f in fine
                    if getattr(getattr(f, "SecurityReference", None), "SecurityType", None) == "ST00000001"]
        if selected and not self._filter_logged:
            self._filter_logged = True
            self.Log("research: common-stock filter kept {} fine candidates".format(len(selected)))
        # A held or working symbol stays subscribed: dropping it would let
        # LEAN cancel its Exit Orders and leave its proceeds unsettled.
        retained = {sym for sym, st in self.symbol_state.items()
                    if st.campaign is not None or st.entry_ticket is not None
                    or st.add_ticket is not None}
        retained.update(o.Symbol for o in self.Transactions.GetOpenOrders())
        chosen = set(selected)
        return selected + [sym for sym in retained if sym not in chosen and sym != self.spy]

    def OnSecuritiesChanged(self, changes):
        for security in changes.AddedSecurities:
            if security.Symbol == self.spy:
                continue
            # Sale proceeds settle at once, or a stock that left the
            # universe would never release them.
            security.SetSettlementModel(ImmediateSettlementModel())
            security.SetSlippageModel(self.slippage_model)
            security.SetFeeModel(self.fee_model)
            security.SetFillModel(self.fill_model)
            if security.Symbol not in self.symbol_state:
                self.symbol_state[security.Symbol] = _SymbolState()
                self.split_ratio[security.Symbol] = self._read_split_ratio(security.Symbol)
                if self.FIXED_SYMBOLS is None:
                    self._warm_up_new_symbol(security.Symbol)
        for security in changes.RemovedSecurities:
            state = self.symbol_state.get(security.Symbol)
            if state is not None and state.campaign is None:
                self._cancel(state.entry_ticket)
                state.entry_ticket = None

    def _warm_up_new_symbol(self, symbol):
        """Backfill a new selection from History, through the same path a
        live bar takes, so it can meet the history floor."""
        state = self.symbol_state[symbol]
        history = self.History([symbol], self.WARMUP_BARS, Resolution.Daily,
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

    def _read_split_ratio(self, symbol):
        closes = []
        for mode in (DataNormalizationMode.Raw, DataNormalizationMode.SplitAdjusted):
            history = self.History([symbol], 1, Resolution.Daily, dataNormalizationMode=mode)
            closes.append(float(history["close"].iloc[-1]) if history is not None and len(history) else 0.0)
        return rules.split_ratio(*closes)

    # ----- The daily Session -------------------------------------------------

    def OnData(self, slice_):
        self._handling = True
        try:
            self._session(slice_)
        finally:
            self._drain_order_events()

    def _session(self, slice_):
        for symbol, split in slice_.Splits.items():
            ratio = self.split_ratio.get(symbol)
            if ratio and split.Type == SplitType.SplitOccurred:
                self.split_ratio[symbol] = rules.ratio_after_split(ratio, float(split.SplitFactor))
        # ADR 0024: LEAN credits a dividend on split-adjusted shares; pay it
        # on the raw shares held instead.
        for symbol, dividend in slice_.Dividends.items():
            state, held = self.symbol_state.get(symbol), float(self.Portfolio[symbol].Quantity)
            if state is not None and held and state.ind.previous_close:
                paid = held * float(dividend.Distribution)
                due = rules.dividend_cash(held, float(dividend.Distribution),
                                          float(dividend.ReferencePrice), state.ind.previous_close)
                self.Portfolio.CashBook["USD"].AddAmount(due - paid)
        if not slice_.Bars:
            return
        # ADR 0010, ADR 0020: this Session is funded from the last reading.
        self._basis_cash, self._pending_cash = self._pending_cash, (
            float(self.Portfolio.Cash), self._fill_debits)
        bar_date = self.Time.date()
        spy_bar = slice_.Bars.get(self.spy)
        if spy_bar is not None:
            self.market.advance(bar_date, float(spy_bar.High), float(spy_bar.Low),
                                float(spy_bar.Close), float(spy_bar.Volume))
        if self.IsWarmingUp:
            for symbol, bar in slice_.Bars.items():
                state = self.symbol_state.get(symbol)
                if state is not None:
                    self._advance(state, bar, bar_date)
            return

        self._close_vanished_campaigns()
        # ADR 0011: an entry or add order lives for one Session.
        for symbol, _ in slice_.Bars.items():
            state = self.symbol_state.get(symbol)
            if state is not None:
                for ticket in (state.entry_ticket, state.add_ticket):
                    self._cancel(ticket)
                state.entry_ticket = state.add_ticket = None

        adds, signals = [], {}
        for symbol, bar in sorted(slice_.Bars.items(), key=lambda item: str(item[0])):
            state = self.symbol_state.get(symbol)
            if state is None:
                continue
            snap = self._advance(state, bar, bar_date)
            if state.campaign is not None:
                self._maintain_exit_orders(state, float(bar.Low))
                if state.campaign.add_ready(float(bar.Close), state.ind.exit_low()):
                    adds.append((symbol, state, bar))
            elif snap is not None:
                signal = self._signal(symbol, state, bar, snap)
                if signal is not None:
                    signals[signal.symbol] = (symbol, state, bar, signal)

        risk = self._risk_fraction()
        equity = float(self.Portfolio.TotalPortfolioValue)
        open_risk = sum(st.campaign.open_risk(st.ind.exit_low())
                        for st in self.symbol_state.values() if st.campaign is not None)
        budget = rules.RiskBudget(equity, open_risk, self._spendable_cash())
        for symbol, state, bar in adds:
            self._decide(symbol, state, bar, "add", risk, budget)
        for signal in rules.admissible_signals(entry[3] for entry in signals.values()):
            symbol, state, bar, _ = signals[signal.symbol]
            self._decide(symbol, state, bar, "entry", risk, budget)
        passed_over = len(signals) - len(rules.admissible_signals(e[3] for e in signals.values()))
        if passed_over:
            self._count_decline("entry: grade B with grade A present", passed_over)

        self.equity_curve.append((self.Time, equity))
        if spy_bar is not None:
            self.spy_curve.append((self.Time, float(spy_bar.Close)))

    def _advance(self, state, bar, bar_date):
        """Fold one completed bar into the indicators and step the Setup
        with the channels as they stood before it (CONTEXT.md "Completed
        bar"). Returns that pre-bar Snapshot when the bar is a Signal."""
        snap = state.ind.snapshot(bar_date)
        high, low, close = float(bar.High), float(bar.Low), float(bar.Close)
        if not state.ind.advance(bar_date, high, low, close, float(bar.Volume)):
            return None
        signal = state.setup.step(high, low, close, snap.atr, snap.prior_55_high, snap.prior_20_high,
                                  snap.last_year_high, active=state.campaign is None
                                  and state.entry_ticket is None)
        return snap if signal else None

    def _risk_fraction(self):
        """Per-position risk from the S&P regime (rules.market_risk_fraction)."""
        m = self.market
        close = m.previous_close
        if close is None:
            return 0.0
        high, low = m.years.previous_year(self.Time.date())
        return rules.market_risk_fraction(
            rules.monthly_regime(close, high, low),
            rules.above(close, m.weekly_sma(rules.LONG_SMA)), rules.above(close, m.weekly_sma(rules.MID_SMA)),
            rules.above(close, m.daily_sma(rules.LONG_SMA)), rules.above(close, m.daily_sma(rules.MID_SMA)),
            rules.above(close, m.daily_sma(rules.SHORT_SMA)))

    def _signal(self, symbol, state, bar, snap):
        """A Phase C Signal, admitted only when the stock is aligned; graded
        and ranked (rules.stock_aligned, rules.grade, rules.strength)."""
        ind, close = state.ind, float(bar.Close)
        self.signal_counts["all"] = self.signal_counts.get("all", 0) + 1
        if not rules.stock_aligned(close, snap.last_year_high, ind.weekly_sma(rules.LONG_SMA),
                                   ind.daily_sma(rules.LONG_SMA), ind.daily_colour(), ind.weekly_colour()):
            self._count_decline("entry: stock not aligned")
            return None
        strength, ready = rules.strength(ind.closes, ind.atr.value)
        if not ready:
            self._count_decline("entry: strength not ready")
            return None
        dollar_volume = rules.median([v * c for v, c in zip(ind.volumes, list(ind.closes)[-len(ind.volumes):])],
                                     rules.VOLUME_WINDOW)[0] or 0.0
        grade = rules.grade(close, snap.all_time_high)
        self.signal_counts[grade] = self.signal_counts.get(grade, 0) + 1
        return rules.Signal(str(symbol), grade, strength, dollar_volume)

    def _spendable_cash(self):
        cash, debits = self._basis_cash or self._pending_cash
        return cash - (self._fill_debits - debits) - sum(self._holds.values())

    # ----- Deciding entries and adds -----------------------------------------

    def _decide(self, symbol, state, bar, kind, risk, budget):
        """One new position, entry or add: sized so its 3 x ATR stop risks
        ``risk`` of equity (rules.position_size), a stop-limit buy one raw
        tick above this bar's high (section 3.6 rule 25), subject to the
        hard filters (entries), and to the risk ceilings and cash
        (rules.RiskBudget)."""
        if risk <= 0:
            self._count_decline(kind + ": market regime")
            return
        if self.split_ratio.get(symbol) is None:
            self.split_ratio[symbol] = self._read_split_ratio(symbol)
        ratio = self.split_ratio[symbol]
        if ratio is None:
            self._count_decline(kind + ": split ratio unreadable")
            return
        ind = state.ind
        if kind == "entry":
            volume = rules.median(ind.volumes, rules.VOLUME_WINDOW)[0]
            if not rules.is_eligible(float(bar.Close) * ratio, volume and volume / ratio, ind.bars):
                self._count_decline("entry: ineligible")
                return
        atr = ind.atr.value
        quantity = rules.whole_raw_shares(rules.position_size(budget.equity, risk, atr), ratio)
        if quantity <= 0:
            self._count_decline(kind + ": fewer than one share")
            return
        level = rules.above_by_a_tick(float(bar.High), ratio)
        cap, slip = rules.price_cap(level, atr), rules.slippage(atr)
        limit = rules.raw_tick_floor(cap, ratio)
        if limit < level:
            self._count_decline(kind + ": limit below stop at the tick")
            return
        cost = rules.worst_case_cost(quantity, cap, slip, rules.commission_estimate(quantity, cap))
        asset_risk = state.campaign.open_risk(ind.exit_low()) if kind == "add" else 0.0
        accepted, reason = budget.try_reserve(asset_risk, quantity * rules.STOP_ATR * atr, cost)
        if not accepted:
            self._count_decline("{}: {}".format(kind, reason))
            return
        ticket = self.StopLimitOrder(symbol, quantity, level, limit, tag="{}:{}".format(kind, symbol))
        order_id = ticket.OrderId
        self.atr_by_order_id[order_id] = atr
        self.order_kind[order_id] = kind
        self.requested[order_id] = quantity
        self._holds[order_id] = cost
        if kind == "entry":
            state.entry_ticket = ticket
        else:
            state.add_ticket = ticket

    # ----- Exit Orders ------------------------------------------------------

    def _maintain_exit_orders(self, state, bar_low=None):
        """Each position's one resting stop-market sell, at the higher of
        its own 3 x ATR stop and the Exit Channel (rules.Campaign). A new
        order rests at that level floored to a raw tick; an amendment only
        ever raises it, and rests below ``bar_low``, the bar it is decided
        after (rules.exit_stop_price). A cancelled order is re-placed."""
        campaign = state.campaign
        exit_low = state.ind.exit_low()
        ratio = self.split_ratio[campaign.symbol]
        while len(state.unit_tickets) < len(campaign.units):
            state.unit_tickets.append(None)
        for index, unit in enumerate(campaign.units):
            level = campaign.exit_level(index, exit_low)
            ticket = state.unit_tickets[index]
            tag = "{}:{}".format("stop" if level == unit["stop"] else "exit", campaign.symbol.Value)
            if ticket is None or ticket.Status in (OrderStatus.Canceled, OrderStatus.Invalid):
                price = rules.raw_tick_floor(level, ratio)
                ticket = self.StopMarketOrder(campaign.symbol, -unit["quantity"], price, tag=tag)
                self.atr_by_order_id[ticket.OrderId] = unit["atr"]
                self.order_kind[ticket.OrderId] = "exit"
                state.unit_tickets[index] = ticket
                self._exit_prices[ticket.OrderId] = price
                continue
            if bar_low is None or ticket.Status == OrderStatus.Filled:
                continue
            price = rules.exit_stop_price(level, bar_low, ratio)
            if price > self._exit_prices.get(ticket.OrderId, 0.0) + 1e-12:
                fields = UpdateOrderFields()
                fields.StopPrice, fields.Tag = price, tag
                ticket.Update(fields)
                self._exit_prices[ticket.OrderId] = price

    def _close_vanished_campaigns(self):
        """A Campaign whose shares LEAN no longer holds (a delisting LEAN
        liquidated itself) is closed at its last close: CONTEXT.md
        "Delisting Exit"."""
        for symbol, state in self.symbol_state.items():
            if state.campaign is not None and not self.Portfolio[symbol].Invested:
                campaign = state.campaign
                for ticket in state.unit_tickets:
                    self._cancel(ticket)
                while campaign.units:
                    campaign.close_unit(0, state.ind.previous_close or campaign.units[0]["fill_price"])
                self._end_campaign(state)
                self._count_decline("exit: holding vanished")

    def _cancel(self, ticket):
        if ticket is None:
            return
        if ticket.Status not in (OrderStatus.Filled, OrderStatus.Canceled, OrderStatus.Invalid):
            ticket.Cancel()
        if ticket.QuantityFilled == 0:
            self._holds.pop(ticket.OrderId, None)

    # ----- Fills --------------------------------------------------------------

    def OnOrderEvent(self, order_event):
        """Queue the event; LEAN can call this from inside an order call
        (an amendment can fill while being made), and acting on it there
        would change a Campaign under the loop that made the call."""
        self._order_events.append(order_event)
        if not self._handling:
            self._handling = True
            self._drain_order_events()

    def _drain_order_events(self):
        try:
            while self._order_events:
                self._handle_order_event(self._order_events.pop(0))
        finally:
            self._handling = False

    def _handle_order_event(self, order_event):
        order_id, status = order_event.OrderId, order_event.Status
        if order_event.FillQuantity:
            fee = float(order_event.OrderFee.Value.Amount)
            self.total_commission += fee
            if order_event.FillQuantity > 0:
                self._fill_debits += float(order_event.FillQuantity) * float(order_event.FillPrice) + fee
        if status not in (OrderStatus.Filled, OrderStatus.Canceled, OrderStatus.Invalid):
            return   # PartiallyFilled: wait for the order to resolve.
        self._holds.pop(order_id, None)
        ticket = self.Transactions.GetOrderTicket(order_id)
        quantity = abs(int(ticket.QuantityFilled))
        state = self.symbol_state.get(order_event.Symbol)
        kind = self.order_kind.pop(order_id, None)
        if state is None or quantity == 0:
            if state is not None:
                for name in ("entry_ticket", "add_ticket"):
                    held = getattr(state, name)
                    if held is not None and held.OrderId == order_id:
                        setattr(state, name, None)
            return
        try:
            self._settle(state, order_event.Symbol, order_id, kind, quantity,
                         float(ticket.AverageFillPrice))
        except Exception as err:
            # A fill handler must never raise: LEAN would abort the run.
            self.Log("research: {} order {} not settled: {}".format(order_event.Symbol, order_id, err))

    def _settle(self, state, symbol, order_id, kind, quantity, price):
        if kind == "exit":
            self._settle_exit(state, order_id, quantity, price)
            return
        if kind not in ("entry", "add"):
            return
        setattr(state, kind + "_ticket", None)
        requested = self.requested.pop(order_id, None)
        atr = self.atr_by_order_id.get(order_id)
        campaign = state.campaign
        usable = (requested == quantity and atr and price - rules.STOP_ATR * atr > 0
                  and ((kind == "entry" and campaign is None)
                       or (kind == "add" and campaign is not None)))
        if not usable:
            # A partial fill, a stop at or below zero, or an add whose
            # Campaign has closed: the shares are sold straight back.
            self.MarketOrder(symbol, -quantity, tag="{}-liquidate:{}".format(kind, symbol))
            self._count_decline(kind + ": fill could not be used")
            return
        if kind == "entry":
            state.campaign = rules.Campaign(symbol, price, quantity, atr)
            state.unit_tickets = []
        else:
            campaign.add_unit(price, quantity, atr)
        self._maintain_exit_orders(state)

    def _settle_exit(self, state, order_id, quantity, price):
        campaign = state.campaign
        if campaign is None:
            return
        index = next((i for i, t in enumerate(state.unit_tickets)
                      if t is not None and t.OrderId == order_id), None)
        if index is None or quantity != campaign.units[index]["quantity"]:
            self.Log("research: ANOMALY: {} exit order {} settled {} shares".format(
                campaign.symbol, order_id, quantity))
            return
        self._cancel(state.add_ticket)
        state.add_ticket = None
        campaign.close_unit(index, price)
        del state.unit_tickets[index]
        if not campaign.units:
            self._end_campaign(state)

    def _end_campaign(self, state):
        self.closed_campaigns.append(state.campaign.r_multiple())
        state.campaign = None
        state.unit_tickets = []

    # ----- The closing summary ---------------------------------------------

    def _count_decline(self, reason, count=1):
        self.decline_counts[reason] = self.decline_counts.get(reason, 0) + count

    def _publish(self, key, value):
        """One figure as a runtime statistic (kept short: long values are
        truncated) and as a log line (which the 10 KB limit may cut)."""
        self.SetRuntimeStatistic(key, str(value))
        self.Log("research: {} = {}".format(key, value))

    def OnEndOfAlgorithm(self):
        self._log_span("OVERALL", self.equity_curve)
        for name, (start, end) in self.regime_windows.items():
            self._log_span("REGIME " + name, [(t, e) for t, e in self.equity_curve if start <= t.date() < end])
        wins = [r for r in self.closed_campaigns if r > 0]
        losses = [r for r in self.closed_campaigns if r <= 0]
        count = len(self.closed_campaigns)
        self._publish("Campaigns", "{} win_rate={} avg_win_R={} avg_loss_R={}".format(
            count, _fmt(len(wins) / count if count else None),
            _fmt(sum(wins) / len(wins) if wins else None),
            _fmt(sum(losses) / len(losses) if losses else None)))
        self._publish("Signals", "{} A={} B={}".format(self.signal_counts.get("all", 0),
                                                       self.signal_counts.get("A", 0),
                                                       self.signal_counts.get("B", 0)))
        self._publish("Commission", "{:.2f}".format(self.total_commission))
        self._publish("Declines", sum(self.decline_counts.values()))
        for reason, count in sorted(self.decline_counts.items()):
            self._publish("Decline " + reason, count)
        self._log_span("SPY BUY-AND-HOLD", self.spy_curve)

    def _log_span(self, label, curve):
        if len(curve) < 2:
            self._publish(label, "no-data")
            return
        (start_time, start_equity), (end_time, end_equity) = curve[0], curve[-1]
        cagr = rules.annualised_return(start_equity, end_equity,
                                       (end_time - start_time).total_seconds() / 86400.0)
        mdd = rules.max_drawdown([e for _, e in curve])
        self._publish(label, "{}..{} CAGR={} MaxDD={} Ratio={}".format(
            start_time.date(), end_time.date(), _fmt(cagr), _fmt(mdd),
            _fmt(rules.cagr_over_max_drawdown(cagr, mdd))))
