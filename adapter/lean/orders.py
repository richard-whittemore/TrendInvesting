"""Mirror the engine's order decisions into LEAN's order book (ADRs 0005, 0013).

The engine decides every level, quantity and N; this module only validates a
decision against LEAN's current state and places, amends or cancels the one
order it names. It computes no level, combines no two levels, and sizes
nothing (README.md: no methodology in the adapter).
"""
from datetime import datetime, timezone
from math import isfinite
from re import fullmatch

# The decision types this module acts on, each at the schema version it was
# written against (internal/event; ADR 0015). Any other version of one of
# these types fails closed rather than being read under the wrong meaning.
TRADE_PROPOSED = "strategy.trade.proposed"
ADD_PROPOSED = "strategy.add.proposed"
PROPOSAL_EXPIRED = "strategy.proposal.expired"
CAMPAIGN_OPENED = "strategy.campaign.opened"
EXIT_ORDER_SET = "strategy.exit-order.set"
SCHEMA_VERSIONS = {TRADE_PROPOSED: 1, ADD_PROPOSED: 1, PROPOSAL_EXPIRED: 3,
                   CAMPAIGN_OPENED: 1, EXIT_ORDER_SET: 1}

# event.ProposalKind* (internal/event/proposal.go): the expiry kinds that name
# a day order of their own. An exit-kind expiry names an exit proposal, which
# is never an order: its level reaches the broker only through
# strategy.exit-order.set (ADR 0005's amendment).
_KINDS_WITH_A_DAY_ORDER = frozenset({"entry", "add"})

# sizing.DirectionLong: the only direction this adapter knows how to place,
# as a buy stop above the market.
_LONG = "long"


class Uncertain(Exception):
    """LEAN's state and the engine's disagree; the run must stop (fail closed)."""


class NSlippageModel:
    """ADR 0013: every fill slips slippage_n x N against the trader.

    N is the figure the engine sent with the order's own decision, looked up
    by the order's tag; the adapter never computes one. An order with no
    supplied N raises rather than slipping by zero, which ADR 0013 declares
    invalid by construction.
    """

    def __init__(self, slippage_n, n_for_tag):
        self.slippage_n = slippage_n
        self.n_for_tag = n_for_tag

    def GetSlippageApproximation(self, asset, order):
        return self.slippage_n * self.n_for_tag(order.Tag)

    get_slippage_approximation = GetSlippageApproximation


def validate_slippage_n(value):
    """Require the run's slippage fraction: finite and positive (ADR 0013)."""
    if type(value) not in (int, float) or not isfinite(value) or value <= 0:
        raise ValueError("slippage_n must be a finite positive number; zero slippage is "
                         "invalid by construction (ADR 0013); got {!r}".format(value))
    return value


def fill_model_report(slippage_n):
    """Every respect in which a LEAN run's fills depart from ADR 0005 and 0013.

    In a LEAN run LEAN's fills are the evidence, and cmd/backtest remains the
    reference implementation of ADR 0005; the two are compared, never forced
    to agree. So each departure is stated at startup rather than silently
    accepted. Where a statement about LEAN's behaviour could not be checked
    against LEAN's source it says so: only the pinned image's own behaviour,
    measured by an acceptance run, confirms it.
    """
    return [
        "slippage is {} x N per fill (ADR 0013), charged by the adapter's NSlippageModel "
        "from the N the engine sent: a trade proposal's n, an Add proposal's campaign_n, "
        "and the Campaign's frozen campaign_n for an Exit Order. The Baseline declares "
        "0.05 x N. LEAN's default equity slippage is believed to be zero (NullSlippageModel); "
        "unconfirmed, as LEAN's source is not available to this adapter, and it applies to "
        "no order here.".format(slippage_n),
        "gap at the open (ADR 0005 rule 1): LEAN's equity fill model is believed to fill a "
        "stop that the bar opens beyond at the open, as ADR 0005 does; unconfirmed. Older "
        "LEAN fill models priced a daily-bar stop fill from the bar's close instead, so the "
        "acceptance run must compare gap fills with cmd/backtest.",
        "touch (ADR 0005): LEAN is believed to trigger a stop only when the bar trades "
        "strictly through the level; ADR 0005 fills in the bar whose range reaches it. A bar "
        "that exactly touches a level may fill in cmd/backtest and not in LEAN; unconfirmed.",
        "same-bar ambiguity (ADR 0005 rule 3): a Unit's Exit Order is placed only after the "
        "engine learns of the Unit's fill and answers with strategy.exit-order.set, so LEAN "
        "can never stop a Unit out in the bar that filled it. ADR 0005 assumes that bar "
        "enters and then stops out; a LEAN run omits those losses and flatters whipsaw bars. "
        "Until fills are returned to the engine, no Exit Order is placed in a LEAN run at all.",
        "intrabar ordering (ADR 0005's amendment): LEAN resolves each working order against "
        "the daily bar independently, with no model of which price came first. ADR 0005 "
        "orders a bar's fills — an entry or Add before any Exit Order, and stop fills before "
        "the exit. Each LEAN fill still follows its own order's level, but the order in "
        "which a bar's fills are reported may differ from cmd/backtest's.",
        "order lifetime: entries and Adds are DAY stop-market orders placed after the "
        "decision bar's close, live for the next session only (ADR 0005's one-bar window); "
        "Exit Orders are good-till-cancelled. Whether LEAN evaluates a daily-resolution fill "
        "for that session before it expires the DAY order is unconfirmed; if it expires "
        "first, no entry or Add fills in a LEAN run, and the acceptance run must show fills.",
        "commission (ADR 0013): LEAN's InteractiveBrokersFeeModel. The Baseline's schedule "
        "is IBKR Pro Fixed — $0.005 per share, $1.00 minimum per order, capped at 1% of "
        "trade value (internal/fills; the configuration's commission block). LEAN's model is "
        "believed to charge the same US-equity fixed tier; unconfirmed. Neither charges "
        "exchange, clearing or regulatory pass-through fees. Pro Fixed is the working "
        "assumption, not a settled choice.",
    ]


def parse_time(text):
    """Read a Go RFC 3339 timestamp (event.Envelope times) as an aware UTC datetime.

    Go writes up to nine fractional digits; Python 3.9 reads at most six, and
    no bar or fill in this system is finer than a microsecond.
    """
    if not isinstance(text, str):
        raise ValueError("timestamp must be a string, got {!r}".format(text))
    match = fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)", text)
    if match is None:
        raise ValueError("timestamp {!r} is not RFC 3339".format(text))
    whole, fraction, zone = match.groups()
    micro = (fraction or "").ljust(6, "0")[:6]
    zone = "+00:00" if zone == "Z" else zone
    return datetime.fromisoformat("{}.{}{}".format(whole, micro, zone)).astimezone(timezone.utc)


def _positive_price(value):
    return type(value) in (int, float) and isfinite(value) and value > 0


def _positive_quantity(value):
    return type(value) is int and value > 0


class OrderDesk:
    """Place, amend and cancel LEAN orders exactly as the engine's decisions say.

    LEAN's order book is the record of what has been submitted: every order's
    tag is the id of the decision that placed it (or, for an Exit Order, the
    decision now in force), so a redelivered decision is recognised from the
    book, never from this object's memory. What this object does remember is
    only what the engine sent and LEAN cannot hold: each tag's N for the
    slippage model, each Campaign's frozen N, and which LEAN order is each
    Unit's Exit Order.
    """

    def __init__(self, algorithm, symbol, instrument, lean):
        self.algorithm = algorithm
        self.symbol = symbol
        self.instrument = instrument
        # OrderProperties, TimeInForce, UpdateOrderFields and OrderStatus from
        # AlgorithmImports, passed in so this module never imports LEAN.
        self.lean = lean
        self.n_by_tag = {}
        self.campaign_n = {}
        self.exit_orders = {}
        self.submitted = 0
        self.rejected = 0

    def n_for_tag(self, tag):
        n = self.n_by_tag.get(tag)
        if n is None:
            raise ValueError("no N was supplied for order tag {!r}; refusing to slip "
                             "it by zero (ADR 0013)".format(tag))
        return n

    def act(self, decisions, period_end, warming):
        """Act on one completed bar's decisions, in the order the engine sent them.

        A decision answering a warm-up bar is never acted on: LEAN's warm-up
        exists to build history, not to trade (README.md).
        """
        actionable = [d for d in decisions if d.get("type") in SCHEMA_VERSIONS]
        for decision in actionable:
            if decision.get("schema_version") != SCHEMA_VERSIONS[decision["type"]]:
                raise Uncertain("{} {} has schema version {!r}; this adapter reads version "
                                "{} (ADR 0015)".format(decision["type"], decision.get("id"),
                                                       decision.get("schema_version"),
                                                       SCHEMA_VERSIONS[decision["type"]]))
        if warming:
            if actionable:
                self.algorithm.Log("adapter: {} decision(s) answer warm-up bar {}; "
                                   "not acted on".format(len(actionable), period_end))
            return
        bar_end = parse_time(period_end)
        for decision in actionable:
            kind, payload = decision["type"], decision.get("payload") or {}
            if kind == TRADE_PROPOSED:
                self._propose(decision, payload, bar_end, payload.get("entry_level"),
                              payload.get("n"), require_long=True)
            elif kind == ADD_PROPOSED:
                self._propose(decision, payload, bar_end, payload.get("level"),
                              payload.get("campaign_n"), require_long=False)
            elif kind == PROPOSAL_EXPIRED:
                self._expire(payload)
            elif kind == CAMPAIGN_OPENED:
                self._campaign_opened(decision, payload)
            else:
                self._exit_order(decision, payload)

    def _reject(self, decision, reason):
        self.rejected += 1
        self.algorithm.Log("adapter: REJECTED {} {}: {}".format(
            decision.get("type"), decision.get("id"), reason))

    def _tradable_reason(self, payload):
        instrument = payload.get("instrument_id")
        if instrument != self.instrument:
            return "instrument {!r} is not this run's tradable instrument {!r}".format(
                instrument, self.instrument)
        if not self.algorithm.Securities[self.symbol].IsTradable:
            return "instrument {!r} is not tradable in LEAN".format(instrument)
        return None

    def _already_submitted(self, tag):
        return self.algorithm.Transactions.GetOrderTickets(lambda t: t.Tag == tag)

    def _propose(self, decision, payload, bar_end, level, n, require_long):
        """An entry or Add: a buy stop-market DAY order at the proposal's level.

        Valid only for the bar after the one that produced it — ADR 0005's
        one-bar window, the same one event.ProposalExpiredPayload's
        EarliestFillAt bounds — so it must answer the bar just published.
        """
        tag = decision.get("id")
        reason = self._tradable_reason(payload)
        if reason is None and not _positive_quantity(payload.get("quantity")):
            reason = "quantity {!r} is not a positive whole number of shares".format(
                payload.get("quantity"))
        if reason is None and not _positive_price(level):
            reason = "level {!r} is not a positive price".format(level)
        if reason is None and not _positive_price(n):
            reason = "N {!r} is not a positive figure to slip by (ADR 0013)".format(n)
        if reason is None and require_long and payload.get("direction") != _LONG:
            reason = "direction {!r} is not {!r}".format(payload.get("direction"), _LONG)
        if reason is None:
            try:
                produced = parse_time(payload.get("period_end"))
            except ValueError as err:
                produced, reason = None, "period_end unreadable: {}".format(err)
            if produced is not None and produced != bar_end:
                reason = ("stale: produced by the bar ending {}, but valid only for the bar "
                          "after it and this is the bar ending {} (ADR 0005)".format(
                              payload.get("period_end"), bar_end.strftime("%Y-%m-%dT%H:%M:%SZ")))
        if reason is None and not tag:
            reason = "no decision id to tag the order with"
        if reason is None:
            existing = self._already_submitted(tag)
            if existing:
                reason = "already submitted as LEAN order {}".format(existing[0].OrderId)
        if reason is not None:
            self._reject(decision, reason)
            return
        properties = self.lean.OrderProperties()
        properties.TimeInForce = self.lean.TimeInForce.Day
        self.n_by_tag[tag] = n
        self._submit(decision, payload["quantity"], level, tag, properties)

    def _submit(self, decision, quantity, level, tag, properties):
        ticket = self.algorithm.StopMarketOrder(self.symbol, quantity, level, tag, properties)
        if ticket.Status == self.lean.OrderStatus.Invalid:
            self._reject(decision, "LEAN refused the order (status {})".format(ticket.Status))
            return None
        self.submitted += 1
        self.algorithm.Log("adapter: order {} {} {} @ {} tag={}".format(
            ticket.OrderId, "buy" if quantity > 0 else "sell", abs(quantity), level, tag))
        return ticket

    def _expire(self, payload):
        """Cancel a still-working day order whose proposal the engine expired (ADR 0011)."""
        if payload.get("kind") not in _KINDS_WITH_A_DAY_ORDER:
            return
        proposal_id = payload.get("proposal_id")
        for ticket in self.algorithm.Transactions.GetOpenOrderTickets(self.symbol):
            if ticket.Tag == proposal_id:
                ticket.Cancel("proposal expired: {}".format(payload.get("reason")))
                self.algorithm.Log("adapter: cancelled order {} tag={}: its proposal expired".format(
                    ticket.OrderId, proposal_id))

    def _campaign_opened(self, decision, payload):
        """Remember the Campaign's frozen N (ADR 0006), for its Exit Orders' slippage."""
        reason = None
        if payload.get("instrument_id") != self.instrument:
            reason = "instrument {!r} is not this run's {!r}".format(
                payload.get("instrument_id"), self.instrument)
        elif not _positive_price(payload.get("campaign_n")):
            reason = "campaign_n {!r} is not a positive figure".format(payload.get("campaign_n"))
        if reason is not None:
            self._reject(decision, reason)
            return
        self.campaign_n[payload["campaign_id"]] = payload["campaign_n"]

    def _working_sell_quantity(self):
        return sum(-(t.Quantity - t.QuantityFilled)
                   for t in self.algorithm.Transactions.GetOpenOrderTickets(self.symbol)
                   if t.Quantity < 0)

    def _exit_order(self, decision, payload):
        """Mirror one Unit's Exit Order (CONTEXT.md: "Exit Order"; ADR 0005's amendment).

        Each held Unit rests exactly one good-till-cancelled sell stop for its
        own quantity, at the level the engine set. A later level for the same
        Unit amends that order and never adds a second, and an amendment LEAN
        does not acknowledge leaves the previous level in force (ADR 0019's
        amendment). A new sell order that would take the working sell
        quantity past the holding stops the run.
        """
        tag = decision.get("id")
        reason = self._tradable_reason(payload)
        quantity, level = payload.get("quantity"), payload.get("level")
        if reason is None and not _positive_quantity(quantity):
            reason = "quantity {!r} is not a positive whole number of shares".format(quantity)
        if reason is None and not _positive_price(level):
            reason = "level {!r} is not a positive price".format(level)
        if reason is None and not tag:
            reason = "no decision id to tag the order with"
        as_of = None
        if reason is None:
            try:
                as_of = parse_time(payload.get("as_of"))
            except ValueError as err:
                reason = "as_of unreadable: {}".format(err)
        if reason is not None:
            self._reject(decision, reason)
            return
        if self._already_submitted(tag):
            self.algorithm.Log("adapter: {} {} is already the order in force; nothing to do".format(
                decision["type"], tag))
            return
        campaign_id = payload.get("campaign_id")
        n = self.campaign_n.get(campaign_id)
        if n is None:
            raise Uncertain("exit order {} names campaign {!r}, whose frozen N the engine never "
                            "sent; its slippage cannot be charged (ADR 0013)".format(tag, campaign_id))
        unit = (campaign_id, payload.get("unit_index"))
        in_force = self.exit_orders.get(unit)
        if in_force is None:
            self._place_exit_order(decision, unit, quantity, level, tag, n, as_of)
        else:
            self._amend_exit_order(decision, unit, in_force, quantity, level, tag, n, as_of)

    def _place_exit_order(self, decision, unit, quantity, level, tag, n, as_of):
        holding = self.algorithm.Portfolio[self.symbol].Quantity
        working = self._working_sell_quantity()
        if working + quantity > holding:
            # LEAN's holding and the engine's Exit Orders disagree. Refusing
            # the order and carrying on would leave this Unit silently without
            # a stop, so the run stops instead: containment is a person's
            # decision (ADR 0019's amendment), never the system's.
            raise Uncertain("instrument {!r} campaign {!r} unit {}: its Exit Order {} for {} "
                            "shares would bring working sell orders to {}, past the holding "
                            "of {} (working sell quantity {}); the Unit cannot be protected "
                            "as the engine believes".format(
                                self.instrument, unit[0], unit[1], tag, quantity,
                                working + quantity, holding, working))
        properties = self.lean.OrderProperties()
        properties.TimeInForce = self.lean.TimeInForce.GoodTilCanceled
        self.n_by_tag[tag] = n
        ticket = self._submit(decision, -quantity, level, tag, properties)
        if ticket is not None:
            self.exit_orders[unit] = {"order_id": ticket.OrderId, "as_of": as_of}

    def _amend_exit_order(self, decision, unit, in_force, quantity, level, tag, n, as_of):
        tickets = self.algorithm.Transactions.GetOrderTickets(
            lambda t: t.OrderId == in_force["order_id"])
        closed = (self.lean.OrderStatus.Filled, self.lean.OrderStatus.Canceled,
                  self.lean.OrderStatus.Invalid)
        if len(tickets) != 1 or tickets[0].Status in closed:
            raise Uncertain("campaign {!r} unit {}'s exit order {} is no longer working in LEAN, "
                            "but the engine still sets its level".format(
                                unit[0], unit[1], in_force["order_id"]))
        ticket = tickets[0]
        if -ticket.Quantity != quantity:
            raise Uncertain("campaign {!r} unit {}'s exit order {} sells {}, but the engine says "
                            "the Unit holds {}".format(unit[0], unit[1], ticket.OrderId,
                                                       -ticket.Quantity, quantity))
        if as_of < in_force["as_of"]:
            self._reject(decision, "older than the level in force for campaign {!r} unit {}".format(
                unit[0], unit[1]))
            return
        fields = self.lean.UpdateOrderFields()
        fields.StopPrice = level
        fields.Tag = tag
        self.n_by_tag[tag] = n
        response = ticket.Update(fields)
        if not response.IsSuccess:
            self.algorithm.Log("adapter: amendment of order {} to {} (tag={}) not acknowledged; "
                               "the previous level stays in force".format(ticket.OrderId, level, tag))
            return
        in_force["as_of"] = as_of
        self.algorithm.Log("adapter: amended order {} to {} tag={}".format(ticket.OrderId, level, tag))
