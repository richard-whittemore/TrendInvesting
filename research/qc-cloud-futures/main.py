"""Faith's original Turtle rules on futures -- the Turtles' own market --
for QuantConnect Cloud's Free plan, which includes futures data.

Research-only. This is NOT the production Go engine (internal/strategy,
internal/sizing, internal/indicator, internal/fills) and carries no claim of
bit-for-bit parity with it. It is also a SEPARATE check from
research/qc-cloud, which tests the equity-adapted Baseline (half Faith's
Unit Volatility Fraction, long-only) against US stocks and finds it loses.
This script asks a different question: does Faith's OWN, UNADAPTED system
-- System 2, 55/20, 1% Unit Volatility Fraction, long AND short, on a
diversified futures portfolio -- make money, after realistic costs? See
README.md for how to run it, the market list, roll handling, and every
place this cloud environment forces a deviation from Faith's text, and why.

rules.py is the pure-Python rule core this file drives; it has no
QuantConnect imports and is unit-tested on its own (test_rules.py). This
file is deliberately thin: QuantConnect wiring only -- markets, roll
handling, order placement, fill handling, and the closing summary -- never
a re-derivation of a rule rules.py already states.

The QuantConnect API calls below (AddFuture with a continuous-contract
DataMappingMode/DataNormalizationMode, Future.SetFilter, Future.Mapped,
SymbolProperties.ContractMultiplier, StopLimitOrder/StopMarketOrder,
SetBrokerageModel(..., AccountType.Margin), Transactions.GetOrderTicket,
ticket.Update(UpdateOrderFields(...))) are written against LEAN's
documented Python API as best understood without network access (this
script never fetches market data or documentation); the custom stop-limit
fill/slippage model is adapted from research/qc-cloud/main.py's own
Adr0005FillModel/NSlippageModel, generalised to trade both directions.
README.md's "Uncertain about the API" section names every call this
repository cannot independently verify -- the owner's first cloud run is
the real test.
"""

from AlgorithmImports import *  # noqa: F401,F403

from datetime import date, datetime, timezone

from os.path import abspath, dirname
from sys import path

_HERE = dirname(abspath(__file__))
if _HERE not in path:
    path.insert(0, _HERE)

import rules  # noqa: E402

#: How many trailing daily bars a market needs before it can be judged
#: eligible for a breakout or ranked by Strength: the deeper of the 55-bar
#: Entry Channel and Strength's own 64-bar lookback.
WARMUP_BARS = max(rules.ENTRY_CHANNEL_LENGTH, rules.STRENGTH_LOOKBACK_BARS + 1)

# =============================================================================
# The market list (README.md, "Market list", documents the reasoning) --
# ticker -> (display name, closely-correlated group, loosely-correlated
# group). Root ticker symbols (not QuantConnect's own ``Futures.*`` nested
# constant classes, whose exact member names this script cannot verify
# without network access) are used directly with ``AddFuture``: CME/CBOT/
# ICE root symbols are a market convention independent of any one vendor's
# API, so this sidesteps that particular naming risk (README.md,
# "Uncertain about the API").
#
# Loosely-correlated groups are Faith's own examples generalised to a
# named category: rates, currencies,
# metals, energy, grains, softs, equity indices [T p.16]. Closely-
# correlated subgroups follow Faith's own disclosed pairs where he names
# one (heating oil/crude, gold/silver [T p.16]) and this script's own
# considered, UNVERIFIED extension elsewhere (documented per market below
# and in README.md) -- exactly the caveat research/qc-cloud/README.md
# records for its own Morningstar-derived groups.
# =============================================================================

CORRELATION_GROUPS = {
    # Rates: Faith's own "T-bill/Eurodollar" example [T p.16] shows short
    # and money-market rates closely correlated; by the same logic the two
    # points on the Treasury curve traded here are kept in one closely-
    # correlated group.
    "ZN": ("10-Year U.S. Treasury Note", "rates", "rates"),
    "ZB": ("30-Year U.S. Treasury Bond", "rates", "rates"),
    # Currencies: Faith's own "CHF/DEM" example [T p.16] pairs the Swiss
    # franc with its (pre-EUR) closest European relative; EUR stands in for
    # DEM here. JPY, GBP, CAD and AUD have no disclosed closely-correlated
    # partner in this list, so each is its own closely-correlated group --
    # a considered, unverified choice.
    "6E": ("Euro FX", "currencies-europe", "currencies"),
    "6S": ("Swiss Franc", "currencies-europe", "currencies"),
    "6J": ("Japanese Yen", "currencies-jpy", "currencies"),
    "6B": ("British Pound", "currencies-gbp", "currencies"),
    "6C": ("Canadian Dollar", "currencies-cad", "currencies"),
    "6A": ("Australian Dollar", "currencies-aud", "currencies"),
    # Metals: Faith's own "gold/silver" example [T p.16] is disclosed
    # directly; copper (an industrial, not precious, metal) is its own
    # closely-correlated group.
    "GC": ("Gold", "metals-precious", "metals"),
    "SI": ("Silver", "metals-precious", "metals"),
    "HG": ("Copper", "metals-base", "metals"),
    # Energy: Faith's own "heating oil/crude" example [T p.16] is disclosed
    # directly; natural gas has traded largely decoupled from oil since the
    # shale era and is its own closely-correlated group -- a considered,
    # unverified choice for the modern market.
    "CL": ("Crude Oil WTI", "energy-petroleum", "energy"),
    "HO": ("Heating Oil (NY Harbor ULSD)", "energy-petroleum", "energy"),
    "NG": ("Henry Hub Natural Gas", "energy-natgas", "energy"),
    # Equity indices: one instrument in this run's list, so the closely/
    # loosely distinction does not bind either way.
    "ES": ("S&P 500 E-mini", "equity_indices", "equity_indices"),
    # Grains and softs: Faith's own markets excluded these on Dennis's
    # position-limit grounds [T p.10-11], not on correlation grounds, so no
    # disclosed example exists for either group. Each is kept as a single
    # closely-correlated group (no disclosed finer split) -- a considered,
    # unverified choice, included only if QuantConnect's futures data
    # actually lists the contract (main.py's own Initialize skips one that
    # is not).
    "ZC": ("Corn", "grains", "grains"),
    "ZS": ("Soybeans", "grains", "grains"),
    "ZW": ("Chicago SRW Wheat", "grains", "grains"),
    "SB": ("Sugar No. 11", "softs", "softs"),
    "KC": ("Coffee C", "softs", "softs"),
    "CC": ("Cocoa", "softs", "softs"),
    "CT": ("Cotton No. 2", "softs", "softs"),
}


# =============================================================================
# A custom stop-limit fill model (ADR 0005, as amended 2026-09-24) and
# slippage model (ADR 0013), generalised for both directions -- adapted
# from research/qc-cloud/main.py's own Adr0005FillModel/NSlippageModel
# (equities, long only). LEAN's own native stop-limit fill (a) triggers
# only on a strict cross, not an exact touch; (b) applies no slippage to a
# stop-limit fill. This replaces StopLimitFill only; every stop-market
# order (every Exit Order, in both directions) and every market order
# (roll transactions, and the anomaly liquidations below) keeps LEAN's own
# native fill and slippage handling.
#
# Subclasses ``FutureFillModel`` -- LEAN's own per-security-type fill
# model for futures, matching the pattern research/qc-cloud's own
# Adr0005FillModel follows for ``EquityFillModel``. This class name is one
# of the specific things this repository cannot verify without a live
# QuantConnect run (README.md, "Uncertain about the API").
# =============================================================================


def _stop_limit_fill_model(slippage_model, fill_prices):
    class Adr0005FutureFillModel(FutureFillModel):
        def StopMarketFill(self, asset, order):
            return self._record(order, super().StopMarketFill(asset, order))

        def MarketFill(self, asset, order):
            return self._record(order, super().MarketFill(asset, order))

        def _record(self, order, fill):
            if fill.FillQuantity:
                fill_prices[order.Id] = float(fill.FillPrice)
            return fill

        def StopLimitFill(self, asset, order):
            # order.Quantity's own sign already says which direction this
            # entry/Add is: positive (buy) opens or adds to a long,
            # negative (sell) opens or adds to a short (main.py's own
            # _place_order places every entry/Add this way).
            direction = 1 if order.Quantity > 0 else -1
            fill = OrderEvent(order, Extensions.ConvertToUtc(asset.LocalTime, asset.Exchange.TimeZone),
                              OrderFee.Zero)
            if order.Status == OrderStatus.Canceled or not self.IsExchangeOpen(asset, False):
                return fill
            prices = self.GetPricesCheckingPythonWrapper(asset, order.Direction)
            if Extensions.ConvertToUtc(prices.EndTime, asset.Exchange.TimeZone) <= order.Time:
                return fill
            try:
                slip = float(slippage_model.GetSlippageApproximation(asset, order))
                price = rules.stop_limit_fill_price(direction, float(order.StopPrice),
                                                    float(order.LimitPrice), float(prices.Open),
                                                    float(prices.High), float(prices.Low), slip)
            except Exception as err:  # fail closed: never guess a fill price.
                self.Log("research: order {} could not be priced by the ADR 0005 fill model: {}".format(
                    order.Id, err))
                return fill
            if price is not None:
                fill.Status = OrderStatus.Filled
                fill.FillQuantity = order.Quantity
                fill.FillPrice = price
            return self._record(order, fill)

    return Adr0005FutureFillModel


def _n_slippage_model(n_by_order_id):
    """ADR 0013: 0.05 x N per fill, against the trader, on every entry and
    Add this algorithm placed (n_by_order_id is populated at placement --
    _place_order -- so this model never computes its own N)."""

    class NSlippageModel:
        def GetSlippageApproximation(self, asset, order):
            n = n_by_order_id.get(order.Id)
            return 0.0 if n is None else rules.slippage(n)

        get_slippage_approximation = GetSlippageApproximation

    return NSlippageModel()


def _fmt(value):
    return "n/a" if value is None else "{:.4f}".format(value)


# =============================================================================
# Per-market state.
# =============================================================================


class _SymbolState:
    """Everything this algorithm tracks per market, keyed by the CANONICAL
    continuous Future symbol (never the dated contract symbol, which
    changes at every roll -- README.md, "Roll handling"). rules.py's own
    classes hold the actual rule state; this class is bookkeeping."""

    __slots__ = ("n", "entry_channel", "exit_channel", "closes", "bars_seen", "previous_close",
                 "last_bar_date", "campaign", "entry_tickets", "add_ticket", "add_placed",
                 "unit_tickets", "last_high", "last_low", "mapped_symbol", "dollars_per_point", "last_offset",
                 "closely_group", "loosely_group", "display_name")

    def __init__(self, closely_group, loosely_group, display_name):
        self.n = rules.WilderN()
        self.entry_channel = rules.Channel(rules.ENTRY_CHANNEL_LENGTH)
        self.exit_channel = rules.Channel(rules.EXIT_CHANNEL_LENGTH)
        self.closes = []  # bounded to STRENGTH_LOOKBACK_BARS + 1, for Strength
        self.bars_seen = 0
        self.previous_close = None
        self.last_bar_date = None
        self.campaign = None                # rules.Campaign, or None
        self.entry_tickets = {1: None, -1: None}  # a resting breakout order per direction
        self.add_ticket = None              # the resting Add stop-limit order
        self.add_placed = None              # self.Time when add_ticket was placed
        self.unit_tickets = []              # one resting Exit Order per open Unit
        self.last_high = None
        self.last_low = None
        self.mapped_symbol = None           # the currently tradable dated contract
        self.dollars_per_point = None       # SymbolProperties.ContractMultiplier of mapped_symbol
        self.last_offset = 0.0              # last known back-adjusted minus raw price
        self.closely_group = closely_group
        self.loosely_group = loosely_group
        self.display_name = display_name

    def record_close(self, close):
        self.closes.append(close)
        if len(self.closes) > rules.STRENGTH_LOOKBACK_BARS + 1:
            del self.closes[0]


# =============================================================================
# The algorithm.
# =============================================================================


class TurtleFuturesResearch(QCAlgorithm):
    """A thin QCAlgorithm driving rules.py's Faith-original Turtle rule core
    over a diversified futures portfolio, long AND short, on QuantConnect's
    Free plan (which includes futures data). See README.md for how to run
    it and read the results, the market list and roll handling, and every
    deviation this cloud environment forces."""

    STARTING_CASH = 1_000_000.0

    #: Faith's own Unit Volatility Fraction, the required default
    #: (rules.py's own module docstring and ADR 0003 explain why
    #: this differs from research/qc-cloud's 0.5% equity halving). Set to
    #: 0.005 to run the equity Baseline's own fraction here for comparison.
    UNIT_VOLATILITY_FRACTION = rules.UNIT_VOLATILITY_FRACTION

    #: A parameter for comparison against Faith's own symmetric long-and-
    #: short system: False (the default)
    #: trades both directions; True skips every short breakout.
    LONG_ONLY = False

    #: The run's span: (year, month, day). README.md, "Start date", explains
    #: why 1998 is provisional pending the first cloud run's own report of
    #: what QuantConnect's futures history actually covers for every market
    #: in CORRELATION_GROUPS. README.md, "Cloud result", records the
    #: in-sample run through this same END_DATE; 2016 onward is the held-out
    #: period and stays out of sample unless this is changed with intent.
    START_DATE = (1998, 1, 1)
    END_DATE = (2015, 12, 31)
    #: Tickers to leave out of CORRELATION_GROUPS for a diagnostic run.
    EXCLUDED_MARKETS = ()

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
        # Futures are margined: a Cash account
        # cannot hold a futures position at all in LEAN. Affordability is
        # therefore QuantConnect's own margin model's job, not rules.py's
        # (README.md, "Deviations" -- unlike research/qc-cloud's own cash
        # ledger for a Cash equity account).
        self.SetBrokerageModel(BrokerageName.InteractiveBrokersBrokerage, AccountType.Margin)

        self.futures = {}
        self.symbol_state = {}
        self.continuous_by_mapped = {}
        self.unavailable_markets = []
        for ticker, (display_name, closely_group, loosely_group) in CORRELATION_GROUPS.items():
            if ticker in self.EXCLUDED_MARKETS:
                continue
            try:
                future = self.AddFuture(
                    ticker, Resolution.Daily, extendedMarketHours=False,
                    dataMappingMode=DataMappingMode.OpenInterest,
                    # Back-adjusted (additive/"Panama Canal"): True Range and
                    # the Donchian channels are absolute price differences,
                    # which an additive back-adjustment preserves across
                    # rolls without compounding a ratio at every one of a
                    # market's many rolls over a multi-decade run (the
                    # task's own requirement: "a documented
                    # DataNormalizationMode (back-adjusted)").
                    dataNormalizationMode=DataNormalizationMode.BackwardsPanamaCanal)
                # A rolling window of listed contracts LEAN keeps mapped
                # into: 0 to 182 days to expiry, QuantConnect's own
                # documented example value for a continuous future.
                future.SetFilter(0, 182)
            except Exception as err:
                self.unavailable_markets.append(ticker)
                self.Log("research: market {} unavailable: {}".format(ticker, err))
                continue
            self.futures[ticker] = future
            self.symbol_state[future.Symbol] = _SymbolState(closely_group, loosely_group, display_name)

        # SPY, for the buy-and-hold comparison the closing summary reports
        # -- never a traded or signalled
        # instrument, on its own dividend-adjusted, total-return basis
        # (research/qc-cloud/README.md, "Deviations", #13, for the
        # identical reasoning).
        self.spy = self.AddEquity(
            "SPY", Resolution.Daily, dataNormalizationMode=DataNormalizationMode.Adjusted).Symbol
        self.spy_curve = []

        self.unit_caps = rules.UnitCaps()
        self.notional_account = rules.NotionalAccount(self.STARTING_CASH)
        self.n_by_order_id = {}
        self.order_kind = {}                      # order id -> "entry"|"add"|"exit"|"roll"
        self.reservations_by_order_id = {}        # order id -> (symbol, closely, loosely, direction)
        self.requested_quantity_by_order_id = {}  # order id -> whole-contract quantity REQUESTED
        self._exit_levels = {}                    # Exit Order id -> the level it rests at
        # LEAN can fill an amended order while the amendment is being made,
        # calling OnOrderEvent from inside this algorithm's own loop
        # (research/qc-cloud's own documented anomaly). Events are
        # therefore queued and handled one at a time.
        self._order_events = []
        self._handling = False
        self._partial_fill_logged = set()

        self.equity_curve = []
        self.closed_campaigns = []
        self.total_commission = 0.0
        self.roll_commission = 0.0
        self.roll_count = 0
        # Reconciliation: the Campaigns' own dollar P&L (price distance x
        # quantity x multiplier) against the account's, and fills this
        # script did not place (LEAN's delisting liquidations).
        self.campaign_dollars = 0.0
        self.campaign_dollars_by_market = {}
        self.untracked_fills = 0
        # Declines are counted, not logged: the Free plan allows 10 KB of
        # log per backtest (research/qc-cloud's own documented limit).
        self.decline_counts = {}

        self.slippage_model = _n_slippage_model(self.n_by_order_id)
        fill_prices = {}
        self.fill_model = _stop_limit_fill_model(self.slippage_model, fill_prices)()

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

        self.SetWarmUp(self.WARMUP_BARS, Resolution.Daily)

    def OnSecuritiesChanged(self, changes):
        for security in changes.AddedSecurities:
            if security.Symbol.SecurityType == SecurityType.Future:
                security.SetFillModel(self.fill_model)
                security.SetSlippageModel(self.slippage_model)
            # Every other model (fee, margin, settlement) is left at
            # SetBrokerageModel's own Interactive-Brokers-Margin default:
            # unlike research/qc-cloud's equity script, no split/dividend/
            # raw-share complication exists here to work around.

    # -------------------------------------------------------------------
    # The daily Session. QuantConnect delivers every market's bar for one
    # trading day in a single Slice, so the Slice itself IS the Session
    # boundary. Order within a Session (mirrors ADR 0010/0021 for
    # equities): rolls first (a contract-housekeeping step, not a trading
    # decision), then exits, then Adds, ascending symbol order, then
    # entries, ranked by Strength within each direction separately (The
    # Turtle Rules p.27-29: "buy the strongest / sell the weakest").
    # -------------------------------------------------------------------

    def OnData(self, slice_):
        self._handling = True
        try:
            self._refresh_mapped_symbols()
            self._session(slice_)
        finally:
            self._drain_order_events()

    def _refresh_mapped_symbols(self):
        """Every Session, before anything else: read each continuous
        future's own currently-Mapped (tradable) contract. The first
        reading is a plain assignment (no position exists yet to roll);
        any later change is a genuine roll, handled by ``_roll_market``.
        Checking ``future.Mapped`` directly, rather than
        ``slice.SymbolChangedEvents``, is this script's own simpler,
        self-correcting choice (README.md, "Roll handling" and "Uncertain
        about the API")."""
        for future in self.futures.values():
            state = self.symbol_state.get(future.Symbol)
            if state is None:
                continue
            mapped = future.Mapped
            if mapped is None:
                continue
            held = state.mapped_symbol
            target = rules.roll_target(
                self.Time.date(), self._contract(held) if held is not None else None,
                self._contract(mapped),
                [self._contract(sym) for sym in self.Securities.Keys
                 if sym.SecurityType == SecurityType.Future and not sym.IsCanonical()
                 and sym.Canonical == future.Symbol] if held is not None else [])
            if target is None:
                continue
            if held is None:
                self._assign_mapped_symbol(state, future.Symbol, target)
            else:
                self._roll_market(state, future.Symbol, held, target)

    def _fresh(self, security):
        """A price this script may trade against: present, positive, and
        from a bar at most five calendar days old. QuantConnect's data for
        some contracts stops weeks before expiry, and a market order there
        fills at the last, stale price."""
        if security is None or not security.HasData or float(security.Price) <= 0:
            return False
        last = security.GetLastData()
        return last is not None and (self.Time - last.EndTime).days <= 5

    def _contract(self, symbol):
        """``(symbol, expiry date, priced, open interest)`` for
        ``rules.roll_target``."""
        security = self.Securities[symbol] if self.Securities.ContainsKey(symbol) else None
        priced = self._fresh(security)
        open_interest = float(security.OpenInterest) if security is not None else 0.0
        return (symbol, symbol.ID.Date.date(), priced, open_interest)

    def _assign_mapped_symbol(self, state, continuous_symbol, mapped):
        state.mapped_symbol = mapped
        self.continuous_by_mapped[mapped] = continuous_symbol
        multiplier = getattr(self.Securities[mapped].SymbolProperties, "ContractMultiplier", None)
        state.dollars_per_point = float(multiplier) if multiplier else None

    def _roll_market(self, state, continuous_symbol, old_mapped, new_mapped):
        """Handle a roll: cancel every resting
        order on the OLD contract, move any open Campaign's whole position
        into the NEW one at the same total Unit count with one closing and
        one opening market order (their commission is this run's own "roll
        cost", reported separately from ordinary trading commission), and
        re-place each Unit's own Exit Order on the NEW contract at its
        existing (unchanged) stop level.

        Neither market order is guaranteed to fill the whole requested
        quantity (thin or stale contract data, README.md, "Known remaining
        gap"). This method only moves the Campaign onto the NEW contract,
        and its Exit Orders with it, once the account's own holdings there
        actually match the Campaign's books; otherwise it re-instates the
        Exit Orders wherever the position actually still is and leaves the
        next Session's own roll_target call to retry -- never moving a
        stop onto a contract this Campaign does not yet, or no longer,
        hold."""
        for direction in (1, -1):
            self._cancel_ticket(state.entry_tickets[direction])
            state.entry_tickets[direction] = None
        self._cancel_ticket(state.add_ticket)
        state.add_ticket = None

        campaign = state.campaign
        if campaign is None:
            self._assign_mapped_symbol(state, continuous_symbol, new_mapped)
            return

        for ticket in state.unit_tickets:
            self._cancel_ticket(ticket)
        state.unit_tickets = []
        total_quantity = campaign.direction * campaign.unit_quantity * campaign.unit_count
        # Close what is actually held: an expiring contract LEAN has
        # already liquidated holds nothing, and a blind close would
        # open the opposite position.
        held = int(self.Portfolio[old_mapped].Quantity)
        if held:
            close_ticket = self.MarketOrder(old_mapped, -held,
                                            tag="roll-close:{}".format(continuous_symbol))
            self.order_kind[close_ticket.OrderId] = "roll"
        open_ticket = self.MarketOrder(new_mapped, total_quantity,
                                       tag="roll-open:{}".format(continuous_symbol))
        self.order_kind[open_ticket.OrderId] = "roll"
        self.roll_count += 1

        if int(self.Portfolio[new_mapped].Quantity) == total_quantity:
            self._assign_mapped_symbol(state, continuous_symbol, new_mapped)
            self._maintain_exit_orders(state, None)
            return
        self.Log("research: {} roll from {} to {} did not fill both legs; retrying next "
                 "Session".format(continuous_symbol, old_mapped, new_mapped))
        if int(self.Portfolio[old_mapped].Quantity) == total_quantity:
            # The close leg did not fill: the Campaign is still exactly
            # where it was. Protect it there again rather than leave it
            # naked until the next Session's retry.
            self._maintain_exit_orders(state, None)
        # else: neither contract's own holding matches the Campaign's own
        # books (a partial fill on one or both legs). Refusing to guess
        # which Exit Orders to place mirrors _handle_unit_exit's own
        # "refusing to guess" anomaly handling below; the next Session's
        # roll_target call retries the whole roll.

    def _offset(self, continuous_symbol):
        """Back-adjusted minus raw price of the mapped contract. Channels,
        N and Campaign levels live in the back-adjusted series; orders
        rest on the raw mapped contract, so every level crosses this
        offset on the way out and every fill price on the way back. The
        back-adjusted (continuous) price is not itself checked against
        zero: README.md, "Continuous futures and roll handling", "Back-
        adjusted prices can be zero or negative" -- only the RAW mapped
        contract's own freshness decides whether a price is available."""
        state = self.symbol_state[continuous_symbol]
        adjusted = float(self.Securities[continuous_symbol].Price)
        held = self.Securities[state.mapped_symbol] if state.mapped_symbol else None
        if not self._fresh(held):
            return None
        return adjusted - float(held.Price)

    def _session(self, slice_):
        if not slice_.Bars:
            return
        bar_date = self.Time.date()

        if self.IsWarmingUp:
            for symbol, bar in slice_.Bars.items():
                state = self.symbol_state.get(symbol)
                if state is not None:
                    self._advance(state, bar, bar_date)
            return

        # A breakout or ordinary Add proposal stays outstanding only until
        # its market's next bar (mirrors ADR 0011 for equities): expire
        # whichever did not fill against this Session's bar.
        for symbol, bar in slice_.Bars.items():
            state = self.symbol_state.get(symbol)
            if state is None:
                continue
            for direction in (1, -1):
                ticket = state.entry_tickets[direction]
                if ticket is not None:
                    self._cancel_ticket(ticket)
                    state.entry_tickets[direction] = None
            if state.add_ticket is not None and state.add_placed < self.Time:
                self._cancel_ticket(state.add_ticket)
                state.add_ticket = None

        add_opportunities = []              # [(symbol, state, rung)]
        candidates = {1: {}, -1: {}}        # direction -> symbol-string -> (symbol, state, level, n, strength)

        for symbol, bar in sorted(slice_.Bars.items(), key=lambda item: str(item[0])):
            state = self.symbol_state.get(symbol)
            if state is None:
                continue
            # Evaluate-then-add: read every channel/N BEFORE folding
            # today's bar into them.
            entry_high, entry_high_ready = state.entry_channel.extreme(1)
            entry_low, entry_low_ready = state.entry_channel.extreme(-1)
            exit_high, exit_high_ready = state.exit_channel.extreme(1)
            exit_low, exit_low_ready = state.exit_channel.extreme(-1)
            pre_advance_n = state.n.value if state.n.ready else None
            high, low = float(bar.High), float(bar.Low)

            long_breakout = (entry_high_ready and pre_advance_n and pre_advance_n > 0
                            and rules.is_breakout(1, high, low, entry_high)
                            and state.campaign is None and state.entry_tickets[1] is None)
            short_breakout = (not self.LONG_ONLY and entry_low_ready and pre_advance_n
                             and pre_advance_n > 0 and rules.is_breakout(-1, high, low, entry_low)
                             and state.campaign is None and state.entry_tickets[-1] is None)

            if state.campaign is not None:
                direction = state.campaign.direction
                exit_extreme, exit_ready = (exit_low, exit_low_ready) if direction > 0 \
                    else (exit_high, exit_high_ready)
                breach = rules.exit_channel_breach(direction, high, low, exit_extreme, exit_ready)
                # No Add is decided in a Session that proposes an exit
                # (exits take precedence).
                self._maintain_exit_orders(state, breach)
                rung = state.campaign.next_add_rung()
                reached = rung is not None and (
                    (direction > 0 and high >= rung) or (direction < 0 and low <= rung))
                if reached and breach is None and state.add_ticket is None:
                    add_opportunities.append((symbol, state, rung))

            self._advance(state, bar, bar_date)

            if long_breakout or short_breakout:
                # Faith's own Strength formula (The Turtle Rules p.29) is
                # "as of day d" on both sides: close[d] and N[d]. state.closes
                # already includes today's close (folded in by _advance,
                # above), so N must be state.n's own post-_advance value here
                # too, not pre_advance_n -- the pre-breakout N the entry
                # itself is sized and stopped from.
                strength_value, strength_ready = rules.strength(state.closes, state.n.value)
                if strength_ready:
                    key = str(symbol)
                    if long_breakout:
                        candidates[1][key] = (symbol, state, entry_high, pre_advance_n, strength_value)
                    if short_breakout:
                        candidates[-1][key] = (symbol, state, entry_low, pre_advance_n, strength_value)
                # else: cannot be ranked; the Signal is declined.

        ledger = rules.SessionCapLedger(self.unit_caps)

        # 1. Adds, ascending symbol order.
        for symbol, state, rung in add_opportunities:
            self._decide_add(state, symbol, rung, ledger)

        # 2. Entries, ranked by Strength within each direction separately
        # (The Turtle Rules p.27-29): every long candidate first (strongest
        # first), then every short candidate (weakest -- most negative --
        # first). Faith does not disclose how a simultaneous long and short
        # Signal in DIFFERENT markets are ordered against each other; this
        # is this script's own considered, documented simplification.
        for direction in (1, -1):
            signals = [rules.Signal(key, entry[4]) for key, entry in candidates[direction].items()]
            for signal in rules.rank_signals(signals, direction):
                symbol, state, level, n, _ = candidates[direction][signal.symbol]
                self._decide_entry(state, symbol, direction, level, n, ledger)

        equity = float(self.Portfolio.TotalPortfolioValue)
        self.notional_account.observe(self.Time.date(), equity)
        self.equity_curve.append((self.Time, equity))
        spy_bar = slice_.Bars.get(self.spy)
        if spy_bar is not None:
            self.spy_curve.append((self.Time, float(spy_bar.Close)))

    def _advance(self, state, bar, bar_date):
        if state.last_bar_date is not None and bar_date <= state.last_bar_date:
            return
        high, low, close = float(bar.High), float(bar.Low), float(bar.Close)
        tr = rules.true_range(high, low, state.previous_close)
        state.n.add(tr)
        state.entry_channel.add(high, low)
        state.exit_channel.add(high, low)
        state.record_close(close)
        state.previous_close = close
        state.last_high = high
        state.last_low = low
        state.bars_seen += 1
        state.last_bar_date = bar_date

    # -------------------------------------------------------------------
    # Deciding Adds and entries.
    # -------------------------------------------------------------------

    def _decide_add(self, state, symbol, rung, ledger):
        campaign = state.campaign
        if campaign is None:
            # The Campaign this rung was measured from can close between
            # collecting the Session's Add opportunities and deciding them
            # (an Exit Order settled in the meantime).
            self._count_decline("add: campaign closed before the add was decided")
            return
        ticket = self._place_order(symbol, "add", campaign.direction, campaign.unit_quantity, rung,
                                   campaign.campaign_n, campaign.closely_group, campaign.loosely_group,
                                   ledger)
        if ticket is not None:
            state.add_ticket, state.add_placed = ticket, self.Time

    def _chain_add(self, state, symbol):
        """Once a Unit fills, the next rung is measured from that fill and
        decided against the last completed bar -- the bar that proposed
        the Unit just filled. If it already reached the rung, the Add is
        proposed now and may fill in the Session after this one; it
        survives this Session's own expiry (mirrors ADR 0011/0021 section
        7 for equities)."""
        campaign = state.campaign
        rung = campaign.next_add_rung() if campaign is not None else None
        if rung is None or state.add_ticket is not None:
            return
        reached = (state.last_high >= rung) if campaign.direction > 0 else (state.last_low <= rung)
        if reached:
            self._decide_add(state, symbol, rung, rules.SessionCapLedger(self.unit_caps))

    def _decide_entry(self, state, symbol, direction, entry_level, n, ledger):
        # Decline BEFORE placing an order whose own fill would leave the
        # initial Protective Stop non-positive (The Turtle Rules p.22).
        offset = self._offset(symbol)
        if offset is None:
            self._count_decline("entry: mapped contract price unavailable")
            return
        if entry_level - offset - direction * rules.STOP_MULTIPLE * n <= 0:
            self._count_decline("entry: stop at or below zero")
            return
        multiplier = state.dollars_per_point
        if not multiplier or multiplier <= 0:
            self._count_decline("entry: contract multiplier unavailable")
            return
        notional = self.notional_account.current
        quantity = rules.unit_quantity(notional, self.UNIT_VOLATILITY_FRACTION, n, multiplier)
        if quantity <= 0:
            self._count_decline("entry: fewer than one contract")
            return
        ticket = self._place_order(symbol, "entry", direction, quantity, entry_level, n,
                                   state.closely_group, state.loosely_group, ledger)
        if ticket is not None:
            state.entry_tickets[direction] = ticket

    def _place_order(self, continuous_symbol, kind, direction, quantity, level, n,
                     closely_group, loosely_group, ledger):
        """An entry or Add: a stop-limit order at ``level``, capped at
        level +/- k x N (ADR 0005), on the market's CURRENTLY MAPPED
        contract, reserving its Unit-cap headroom. Returns the ticket, or
        None when declined."""
        offset = self._offset(continuous_symbol)
        if offset is None:
            self._count_decline("{}: mapped contract price unavailable".format(kind))
            return None
        accepted, reason = ledger.try_reserve(str(continuous_symbol), closely_group, loosely_group,
                                              direction)
        if not accepted:
            self._count_decline("{}: {}".format(kind, reason))
            return None
        state = self.symbol_state[continuous_symbol]
        level -= offset
        cap = rules.price_cap(level, n, direction)
        signed_quantity = direction * quantity
        ticket = self.StopLimitOrder(state.mapped_symbol, signed_quantity, level, cap,
                                     tag="{}:{}".format(kind, continuous_symbol))
        order_id = ticket.OrderId
        self.n_by_order_id[order_id] = n
        self.order_kind[order_id] = kind
        self.reservations_by_order_id[order_id] = (str(continuous_symbol), closely_group, loosely_group,
                                                    direction)
        self.requested_quantity_by_order_id[order_id] = quantity
        return ticket

    # -------------------------------------------------------------------
    # Exit Orders: one resting stop order per Unit, at the more protective
    # of its own Protective Stop and, while an Exit-Channel exit is
    # available, the Exit Channel level (ADR 0005's amendment, symmetric).
    # -------------------------------------------------------------------

    def _maintain_exit_orders(self, state, exit_level):
        campaign = state.campaign
        while len(state.unit_tickets) < len(campaign.units):
            state.unit_tickets.append(None)
        mapped = state.mapped_symbol
        close_quantity = -campaign.direction * campaign.unit_quantity
        offset = self._offset(self.continuous_by_mapped[mapped])
        if offset is None:
            offset = state.last_offset
        state.last_offset = offset
        for index, unit in enumerate(campaign.units):
            adjusted_level = rules.exit_order_level(campaign.direction, unit["stop"], exit_level)
            tag = "{}:{}".format("stop" if adjusted_level == unit["stop"] else "exit",
                                 getattr(mapped, "Value", mapped))
            level = adjusted_level - offset
            ticket = state.unit_tickets[index]
            if ticket is None or ticket.Status in (OrderStatus.Canceled, OrderStatus.Invalid):
                ticket = self.StopMarketOrder(mapped, close_quantity, level, tag=tag)
                self.n_by_order_id[ticket.OrderId] = campaign.campaign_n
                self.order_kind[ticket.OrderId] = "exit"
                state.unit_tickets[index] = ticket
            elif ticket.Status != OrderStatus.Filled and self._exit_levels.get(ticket.OrderId) != level:
                fields = UpdateOrderFields()
                fields.StopPrice, fields.Tag = level, tag
                ticket.Update(fields)
            self._exit_levels[ticket.OrderId] = level

    def _unit_index_for_order(self, state, order_id):
        for index, ticket in enumerate(state.unit_tickets):
            if ticket is not None and ticket.OrderId == order_id:
                return index
        return None

    def _cancel_ticket(self, ticket):
        """Cancel a resting order, releasing its Unit-cap reservation (if
        any -- an Exit Order never registers one) only if nothing has
        filled under it yet; mirrors research/qc-cloud/main.py's own
        identical anomaly-safe cancellation."""
        if ticket is None:
            return
        if ticket.Status not in (OrderStatus.Filled, OrderStatus.Canceled, OrderStatus.Invalid):
            ticket.Cancel()
        if ticket.QuantityFilled == 0:
            self._release_reservation(ticket.OrderId)

    def _release_reservation(self, order_id):
        entry = self.reservations_by_order_id.pop(order_id, None)
        if entry is not None:
            symbol, closely, loosely, direction = entry
            self.unit_caps.remove(symbol, closely, loosely, direction, units=1)

    def _commit_reservation(self, order_id):
        self.reservations_by_order_id.pop(order_id, None)

    def _forget_ticket(self, mapped_symbol, order_id):
        continuous_symbol = self.continuous_by_mapped.get(mapped_symbol)
        state = self.symbol_state.get(continuous_symbol)
        if state is None:
            return
        for direction in (1, -1):
            ticket = state.entry_tickets[direction]
            if ticket is not None and ticket.OrderId == order_id:
                state.entry_tickets[direction] = None
        if state.add_ticket is not None and state.add_ticket.OrderId == order_id:
            state.add_ticket = None

    # -------------------------------------------------------------------
    # Fills (OnOrderEvent): opening/adding to a Campaign, closing Units,
    # and rolling a Campaign's position into a new contract.
    # -------------------------------------------------------------------

    def OnOrderEvent(self, order_event):
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
        order_id = order_event.OrderId
        status = order_event.Status
        ticket = self.Transactions.GetOrderTicket(order_id)
        kind = self.order_kind.get(order_id)

        if order_event.FillQuantity:
            fee = float(order_event.OrderFee.Value.Amount)
            self.total_commission += fee
            if kind == "roll":
                self.roll_commission += fee

        if status == OrderStatus.Invalid:
            self._release_reservation(order_id)
            self.order_kind.pop(order_id, None)
            self._forget_ticket(order_event.Symbol, order_id)
            return

        if status == OrderStatus.PartiallyFilled:
            # PartiallyFilled is an ANOMALY here, exactly as
            # research/qc-cloud/main.py documents for equities: this
            # script's own ADR 0005 stop-limit fill model and LEAN's
            # native stop-market fill each fill an order's whole requested
            # quantity or nothing.
            if order_id not in self._partial_fill_logged:
                self._partial_fill_logged.add(order_id)
                self.Log("research: ANOMALY: {} order {} partially filled ({} of {}); continuing to "
                         "wait for it to resolve".format(order_event.Symbol, order_id,
                                                          ticket.QuantityFilled, ticket.Quantity))
            return

        if status not in (OrderStatus.Filled, OrderStatus.Canceled):
            return

        self._partial_fill_logged.discard(order_id)
        self.order_kind.pop(order_id, None)
        quantity = abs(int(ticket.QuantityFilled))

        if kind is None and status == OrderStatus.Filled:
            self.untracked_fills += 1
        if kind in ("roll", "liquidate"):
            # Commission already booked above; no strategy state to settle
            # -- README.md, "Reconciliation": untracked_fills counts fills
            # this script did not place, and this script placed both of
            # these kinds itself.
            return

        if quantity == 0:
            # LEAN can cancel a resting Entry or Add order this script
            # never asked to cancel (a delisting contract, for example):
            # release its Unit-cap reservation here too, not only in
            # _cancel_ticket's own cancel path. _release_reservation pops,
            # so this is safe even when _cancel_ticket already released it.
            self._release_reservation(order_id)
            self._forget_ticket(order_event.Symbol, order_id)
            return

        price = float(ticket.AverageFillPrice)
        continuous = self.continuous_by_mapped.get(order_event.Symbol)
        offset = self._offset(continuous) if continuous is not None else None
        if offset is None and continuous in self.symbol_state:
            offset = self.symbol_state[continuous].last_offset
        price += offset or 0.0
        try:
            self._settle_order(order_event, kind, quantity, price)
        except Exception as err:
            # A fill handler must never raise: an uncaught exception here
            # aborts the WHOLE backtest over one market's edge case.
            self.Log("research: {} order event could not be settled: {}".format(
                order_event.Symbol, err))

    def _settle_order(self, order_event, kind, quantity, price):
        mapped_symbol = order_event.Symbol
        continuous_symbol = self.continuous_by_mapped.get(mapped_symbol)
        state = self.symbol_state.get(continuous_symbol)
        order_id = order_event.OrderId
        if state is None:
            self._release_reservation(order_id)
            return
        if kind == "entry":
            self._settle_entry(state, continuous_symbol, mapped_symbol, order_id, quantity, price)
        elif kind == "add":
            self._settle_add(state, continuous_symbol, mapped_symbol, order_id, quantity, price)
        elif kind == "exit":
            self._handle_unit_exit(state, continuous_symbol, order_id, quantity, price)

    def _settle_entry(self, state, continuous_symbol, mapped_symbol, order_id, quantity, price):
        # The order's own side, never a slot lookup: a single bar can break
        # both the entry channel's high and low at once (Lines 565-569
        # above check entry_tickets per direction only, so both a long and
        # a short Entry Order can rest at the same time), and the fill
        # model can then fill both on the next bar. _settle_add already
        # derives direction this same way for the identical reason.
        direction = 1 if self.Transactions.GetOrderTicket(order_id).Quantity > 0 else -1
        state.entry_tickets[direction] = None
        opposite = state.entry_tickets[-direction]
        if opposite is not None:
            self._cancel_ticket(opposite)
            state.entry_tickets[-direction] = None
        requested = self.requested_quantity_by_order_id.pop(order_id, None)
        n = self.n_by_order_id.pop(order_id, None)
        if state.campaign is not None:
            # A Campaign already opened -- from the other direction's fill
            # on this same bar, or otherwise -- since this order was
            # placed: this fill cannot open a second one. Liquidate it and
            # leave the existing Campaign untouched, rather than overwrite
            # it and orphan its own open Exit Orders and Unit-cap slots.
            self._release_reservation(order_id)
            liquidate_ticket = self.MarketOrder(
                mapped_symbol, -direction * quantity,
                tag="entry-conflict-liquidate:{}".format(continuous_symbol))
            self.order_kind[liquidate_ticket.OrderId] = "liquidate"
            self.Log("research: {} entry order {} filled after a Campaign was already open; "
                     "liquidated".format(continuous_symbol, order_id))
            return
        if requested is not None and quantity != requested:
            # ANOMALY: never open a Unit smaller than the one this
            # proposal's Unit-cap reservation was sized for.
            self._release_reservation(order_id)
            liquidate_ticket = self.MarketOrder(
                mapped_symbol, -direction * quantity,
                tag="entry-partial-liquidate:{}".format(continuous_symbol))
            self.order_kind[liquidate_ticket.OrderId] = "liquidate"
            self.Log("research: ANOMALY: {} entry order {} filled {} of {} requested; declined and "
                     "liquidated".format(continuous_symbol, order_id, quantity, requested))
            return
        offset = self._offset(continuous_symbol)
        raw_price = price - (state.last_offset if offset is None else offset)
        if n is None or raw_price - direction * rules.STOP_MULTIPLE * n <= 0:
            self._release_reservation(order_id)
            liquidate_ticket = self.MarketOrder(
                mapped_symbol, -direction * quantity,
                tag="entry-declined-liquidate:{}".format(continuous_symbol))
            self.order_kind[liquidate_ticket.OrderId] = "liquidate"
            self.Log("research: {} entry fill at {} could not open a Campaign (N={}); "
                     "liquidated".format(continuous_symbol, price, n))
            return
        state.campaign = rules.Campaign(str(continuous_symbol), direction, price, n, quantity,
                                        state.closely_group, state.loosely_group)
        self._commit_reservation(order_id)
        state.unit_tickets = []
        self._maintain_exit_orders(state, None)
        self._chain_add(state, continuous_symbol)

    def _settle_add(self, state, continuous_symbol, mapped_symbol, order_id, quantity, price):
        state.add_ticket = None
        requested = self.requested_quantity_by_order_id.pop(order_id, None)
        campaign = state.campaign
        # The order's own side, never the Campaign's: the Campaign may
        # already be closed, and an orphaned short Add must be bought back.
        direction = 1 if self.Transactions.GetOrderTicket(order_id).Quantity > 0 else -1
        if requested is not None and quantity != requested:
            self._release_reservation(order_id)
            liquidate_ticket = self.MarketOrder(
                mapped_symbol, -direction * quantity,
                tag="add-partial-liquidate:{}".format(continuous_symbol))
            self.order_kind[liquidate_ticket.OrderId] = "liquidate"
            self.Log("research: ANOMALY: {} Add order {} filled {} of {} requested; declined and "
                     "liquidated".format(continuous_symbol, order_id, quantity, requested))
            return
        if campaign is not None and not campaign.loaded and not campaign.partially_stopped:
            campaign.add_unit(price)
            self._commit_reservation(order_id)
            self._maintain_exit_orders(state, None)
            self._chain_add(state, continuous_symbol)
            return
        # The Campaign this Add was meant for has already fully closed, or
        # already had a Unit stopped out, since the order was placed.
        self._release_reservation(order_id)
        liquidate_ticket = self.MarketOrder(
            mapped_symbol, -direction * quantity,
            tag="add-orphan-liquidate:{}".format(continuous_symbol))
        self.order_kind[liquidate_ticket.OrderId] = "liquidate"
        self.Log("research: {} Add fill at {} arrived with no Campaign able to take it; "
                 "liquidated".format(continuous_symbol, price))

    def _handle_unit_exit(self, state, continuous_symbol, order_id, quantity, price):
        campaign = state.campaign
        if campaign is None:
            return
        unit_index = self._unit_index_for_order(state, order_id)
        if unit_index is None or quantity != campaign.unit_quantity:
            self.Log("research: ANOMALY: {} exit order {} settled {} (expected {} for unit index "
                     "{}); refusing to guess".format(continuous_symbol, order_id, quantity,
                                                      campaign.unit_quantity, unit_index))
            return
        # A resting Add must not survive ANY exit fill, partial or full.
        self._cancel_ticket(state.add_ticket)
        state.add_ticket = None
        self.unit_caps.remove(str(continuous_symbol), campaign.closely_group, campaign.loosely_group,
                              campaign.direction, units=1)
        before = campaign.realized_price_pnl
        campaign.close_units([unit_index], price)
        dollars = ((campaign.realized_price_pnl - before) * campaign.unit_quantity
                   * (state.dollars_per_point or 0.0))
        self.campaign_dollars += dollars
        root = str(continuous_symbol).lstrip("/")
        self.campaign_dollars_by_market[root] = self.campaign_dollars_by_market.get(root, 0.0) + dollars
        del state.unit_tickets[unit_index]
        if not campaign.units:
            r_multiple = campaign.r_multiple()
            self.closed_campaigns.append({"r_multiple": r_multiple, "win": r_multiple > 0})
            state.campaign = None
            state.unit_tickets = []

    # -------------------------------------------------------------------
    # The closing summary.
    # -------------------------------------------------------------------

    def _count_decline(self, reason):
        self.decline_counts[reason] = self.decline_counts.get(reason, 0) + 1

    def _publish(self, key, value):
        """Report one summary figure both as a runtime statistic (kept
        whatever the log budget) and a log line (research/qc-cloud/
        README.md, "How to find the results")."""
        self.SetRuntimeStatistic(key, str(value))
        self.Log("research: {} = {}".format(key, value))

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
        self._publish("Campaigns", "{} win_rate={} avg_win_R={} avg_loss_R={}".format(
            count, _fmt(win_rate), _fmt(avg_win), _fmt(avg_loss)))
        self._publish("Commission", "{:.2f}".format(self.total_commission))
        self._publish("Rolls", "{} commission={:.2f}".format(self.roll_count, self.roll_commission))
        if self.unavailable_markets:
            self._publish("Markets unavailable", ",".join(sorted(self.unavailable_markets)))
        self._publish("Reconcile", "campaigns=${:,.0f} account=${:,.0f} untracked_fills={}".format(
            self.campaign_dollars,
            float(self.Portfolio.TotalPortfolioValue) - self.STARTING_CASH + self.total_commission,
            self.untracked_fills))
        for market, dollars in sorted(self.campaign_dollars_by_market.items()):
            self._publish("Campaign $ " + market, "{:.0f}".format(dollars))
        self._publish("Declines", sum(self.decline_counts.values()))
        for reason, count in sorted(self.decline_counts.items()):
            self._publish("Decline " + reason, count)

        self._log_span("SPY BUY-AND-HOLD", self.spy_curve)

    def _log_span(self, label, curve):
        if len(curve) < 2:
            self._publish(label, "no-data")
            return
        start_time, start_equity = curve[0]
        end_time, end_equity = curve[-1]
        elapsed_days = (end_time - start_time).total_seconds() / 86400.0
        cagr = rules.annualised_return(start_equity, end_equity, elapsed_days)
        mdd = rules.max_drawdown([e for _, e in curve])
        ratio = rules.cagr_over_max_drawdown(cagr, mdd)
        self._publish(label, "{}..{} CAGR={} MaxDD={} Ratio={}".format(
            start_time.date(), end_time.date(), _fmt(cagr), _fmt(mdd), _fmt(ratio)))
