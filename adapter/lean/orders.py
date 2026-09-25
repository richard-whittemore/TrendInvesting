"""Mirror the engine's order decisions into LEAN's order book, and report LEAN's
executions and order changes back to the engine (ADRs 0005, 0013, 0019).

The engine decides every level, quantity and N; this module only validates a
decision against LEAN's current state and places, amends or cancels the one
order it names. It computes no level, combines no two levels, and sizes
nothing (README.md: no methodology in the adapter). What LEAN then does with
those orders it reports as facts: a fill becomes one execution.fill shaped
exactly as the reducer expects for that order's kind, and every other change
of an order becomes one execution.order.lifecycle.
"""
from datetime import datetime, timezone
from decimal import ROUND_FLOOR, Decimal
from fractions import Fraction
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
EXIT_PROPOSED = "strategy.exit.proposed"
UNIT_ADDED = "strategy.campaign.unit-added"
UNITS_STOPPED = "strategy.campaign.units-stopped"
CAMPAIGN_EXITED = "strategy.campaign.exited"
SCHEMA_VERSIONS = {TRADE_PROPOSED: 2, ADD_PROPOSED: 3, PROPOSAL_EXPIRED: 3,
                   CAMPAIGN_OPENED: 1, EXIT_ORDER_SET: 1, EXIT_PROPOSED: 1,
                   UNIT_ADDED: 1, UNITS_STOPPED: 1, CAMPAIGN_EXITED: 2}

# event.OrderType* (internal/event/configuration.go): the order an entry or
# Add rests as (ADR 0005, as amended 2026-09-24). A stop-limit carries its
# price cap as its limit; a stop-market order, the declared Variant
# "uncapped", carries none.
STOP_LIMIT, STOP_MARKET = "stop-limit", "stop-market"

# event.FillKind* (internal/event/fill.go).
KIND_ENTRY, KIND_ADD, KIND_STOP, KIND_EXIT = "entry", "add", "stop", "exit"

# event.ExitOrderSource* (internal/event/exit_order.go).
SOURCE_EXIT_CHANNEL = "exit-channel"

# event.ProposalKind* (internal/event/proposal.go): the expiry kinds that name
# a buy order of their own. An exit-kind expiry names an exit proposal, which
# is never an order: its level reaches the broker only through
# strategy.exit-order.set (ADR 0005's amendment).
_KINDS_WITH_AN_ORDER = frozenset({"entry", "add"})

# sizing.DirectionLong: the only direction this adapter knows how to place,
# as a buy stop above the market.
_LONG = "long"

# The only currency this adapter reports: account.snapshot's, and the one
# event.FillPayload.Commission is stated in.
_USD = "USD"


class Uncertain(Exception):
    """LEAN's state and the engine's disagree; the run must stop (fail closed)."""


# How far a raw price over its split-adjusted one may miss a whole number and
# still be read as that whole split ratio. LEAN's factor files state each
# cumulative split factor to about seven significant digits (AAPL's 1/56 is
# 0.0178571), so the two closes of one bar were observed 56.000134 apart on
# the pinned image; a ratio that is not a product of whole-number splits (a
# 3-for-2's 1.5) misses by far more than this.
_SPLIT_RATIO_TOLERANCE = 1e-4


def whole_split_ratio(ratio):
    """The whole number of split-adjusted shares one raw share is (ADR 0004).

    A split-adjusted price is the raw one divided by the product of every
    split after it, and a split-adjusted share count is the raw one times
    it, so quantity x price is the same in both views. The engine sizes and
    fills in whole split-adjusted shares and LEAN trades whole raw ones, so
    only a whole ratio of at least one lets every fill be stated in both
    views; anything else raises Uncertain, for the run to stop.
    """
    whole = round(ratio) if isfinite(ratio) else 0
    if whole < 1 or abs(ratio - whole) > _SPLIT_RATIO_TOLERANCE * whole:
        raise Uncertain("a raw price is {!r} times its split-adjusted one, which is not a whole "
                        "split ratio of at least 1; a whole share in one view would not be a "
                        "whole number of shares in the other, so no order or fill could be "
                        "stated in both (ADR 0004)".format(ratio))
    return whole


class NSlippageModel:
    """ADR 0013: every fill slips slippage_n x N against the trader.

    N is the figure the engine sent with the order's own decision, looked up
    by the order's tag; the adapter never computes one. An order with no
    supplied N raises rather than slipping by zero, which ADR 0013 declares
    invalid by construction. What was charged is recorded per LEAN order, so
    the fill reports the slippage LEAN actually applied.
    """

    def __init__(self, slippage_n, n_for_tag, record=None):
        self.slippage_n = slippage_n
        self.n_for_tag = n_for_tag
        self.record = record

    def GetSlippageApproximation(self, asset, order):
        slippage = self.slippage_n * self.n_for_tag(order.Tag)
        if self.record is not None:
            self.record(getattr(order, "Id", None), slippage)
        return slippage

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
    accepted. Each statement about LEAN's own behaviour was observed in a
    backtest on the pinned image (README.md, "Observed LEAN behaviour").
    """
    return [
        "price views (ADR 0004): LEAN trades, holds and charges commission in raw shares at "
        "raw prices; the engine's levels, quantities and N are in the split-adjusted view, "
        "and each fill is returned to it in that view. The adapter converts at LEAN's "
        "boundary by the whole number of split-adjusted shares a raw share is (the raw close "
        "over the split-adjusted one), so quantity x price is the same in both. A Unit is "
        "rounded down to whole raw shares, so it can fill up to one raw share short of the "
        "engine's quantity (cmd/backtest fills the whole of it). Only an n-for-1 split is "
        "carried across a run; any other stops it. LEAN applies a split to the holding and "
        "every open order itself, and the run stops, before the next session can fill "
        "anything, unless the result is exactly the engine's position and orders at the new "
        "ratio.",
        "account (ADR 0010): no partial Units, no borrowing. LEAN runs on a cash account "
        "(AccountType.Cash via InteractiveBrokersBrokerageModel), so LEAN itself refuses an "
        "order it cannot fund at submission: status Invalid, rejected and logged, the same "
        "path as any other order LEAN refuses. The engine reserves each entry's and Add's "
        "worst-case cost, at its price cap with slippage and commission, when it proposes it "
        "(ADR 0020, as amended 2026-09-24), and a Baseline order is a stop-limit that cannot "
        "execute above that cap. That a fill never costs more than was reserved is exact in "
        "cmd/backtest, whose commission is ADR 0013's schedule; in LEAN it holds only up to the "
        "difference between LEAN's fee model and that schedule, since InteractiveBrokersFeeModel "
        "charges a $1.00 minimum per order where the hold reserved ADR 0013's charge (a "
        "one-share order at $5 reserves about $0.05 of commission and LEAN charges $1.00), "
        "the fee-model gap #81 tracks. Under the "
        "declared Variant 'uncapped' the order is a stop-market one, and a gap that fills far "
        "above the level can still cost more than the cash on hand, in which case LEAN fills it "
        "anyway and cash goes negative; account.snapshot refuses a negative figure and the run "
        "stops, as it must, since the adapter reports what LEAN actually holds rather than "
        "clamping it.",
        "slippage is {} x N per fill (ADR 0013), charged by the adapter's NSlippageModel "
        "from the N the engine sent: a trade proposal's n, an Add proposal's campaign_n, "
        "and the Campaign's frozen campaign_n for an Exit Order, each at the raw ratio in "
        "force when LEAN fills the order. The Baseline declares "
        "0.05 x N. LEAN's default equity slippage is zero (NullSlippageModel; observed: a "
        "gapped buy with no slippage model filled exactly at the open), and applies to no "
        "order here.".format(slippage_n),
        "tick: LEAN rounds every raw stop price to the cent, the equity's minimum price "
        "variation, including a split's adjustment of an open stop, and logs only the first "
        "such rounding. The fill's level is LEAN's rounded stop, so it can differ from the "
        "engine's level by up to half a cent raw, and by up to a cent after a split. "
        "cmd/backtest fills at the engine's "
        "unrounded level.",
        "gap at the open (ADR 0005 rule 1): observed to match. A stop the bar opens beyond "
        "fills at the open, less slippage for a sell and plus it for a buy, and LEAN says so "
        "in the fill's message ('Due to an unfavorable gap ... filled using the open price'). "
        "In the raw acceptance run every one of 68 fills, 33 of them gaps, was priced at ADR "
        "0005's max(level, open) for a buy or min(level, open) for a sell, plus or minus "
        "slippage, in raw prices.",
        "touch (ADR 0005): observed to match. A bar whose high exactly equals a buy stop, or "
        "whose low exactly equals a sell stop, fills at the level (plus or minus slippage). "
        "Observed in a probe of the pinned image; the acceptance run had no exact touch.",
        "same-bar ambiguity (ADR 0005 rule 3): a Unit's Exit Order is placed only after the "
        "engine learns of the Unit's fill and answers with strategy.exit-order.set, and LEAN "
        "never fills a new order against the bar it was placed after, so a LEAN run cannot "
        "stop a Unit out in the bar that filled it. ADR 0005 assumes that bar enters and then "
        "stops out; a LEAN run omits those losses and flatters whipsaw bars.",
        "amendments: LEAN evaluates an amended order against the bar it was amended after "
        "(observed: a sell stop raised above that bar's low filled at the new level in the "
        "same slice). So an Exit Order moved to an Exit Channel the bar breached, or a stop "
        "the Stop Ladder raised after an Add filled, can fill in that same bar, as it does "
        "in cmd/backtest.",
        "intrabar ordering (ADR 0005's amendment): LEAN resolves each working order against "
        "the daily bar independently, with no model of which price came first. The adapter "
        "reports a slice's fills in ADR 0005's order - buys, then each Unit's stop fill, "
        "worst price first, then the Units resting at the Exit Channel as one exit fill - "
        "but LEAN's prices do not depend on that order.",
        "entry timing: the engine proposes an entry or Add at a Session's close, and LEAN "
        "can fill it only in the next session. cmd/backtest fills it inside the bar that "
        "signalled it, so a LEAN run enters one bar later.",
        "order lifetime: entries and Adds are good-till-cancelled stop-limit orders (stop-market "
        "under the declared Variant 'uncapped') that "
        "the adapter cancels when the engine expires their proposal (ADR 0011): entries and "
        "ordinary Adds at the next bar, fill-chained Adds one bar later. The explicit "
        "valid_for_sessions field gives a fill-chained Add one extra session. LEAN's DAY "
        "orders are not used: at "
        "daily resolution LEAN expires a DAY order before it evaluates the session's fill "
        "(observed: DAY orders whose next bar crossed their level expired unfilled). Exit "
        "Orders are good-till-cancelled. A cancellation is confirmed asynchronously: LEAN "
        "answers CancelPending and reports Canceled after the slice, before the next one.",
        "commission (ADR 0013): LEAN's InteractiveBrokersFeeModel, observed to charge $0.005 "
        "per share with a $1.00 minimum per order (500 shares: $2.50; 50 shares: $1.00), "
        "capped at 0.5% of the order's value at LEAN's market price, not IBKR Pro Fixed's 1% "
        "of trade value that internal/fills applies. The minimum wins over the cap (1 share "
        "at $12.01 was charged $1.00, not a capped $0.06), where internal/fills lets the cap "
        "win. LEAN charges it on the raw shares it trades (ADR 0004); internal/fills charges "
        "the split-adjusted shares it fills, which for AAPL in 2003 are 56 times as many. In "
        "the raw acceptance run the cap bound on none of 68 fills. Neither model "
        "charges exchange, clearing or regulatory pass-through fees. Pro Fixed is the "
        "working assumption, not a settled choice.",
        "partial fills: a partial fill stops the run. The engine accepts one fill per order "
        "and does not accumulate partial fills into one Unit yet.",
        "price cap (ADR 0005, as amended 2026-09-24): UNCONFIRMED, not yet observed on the "
        "pinned image. A proposal carrying a price cap becomes a LEAN StopLimitOrder, stop at "
        "the level and limit at the cap, rounded down to the tick so LEAN's limit never exceeds "
        "the engine's cap. ADR 0005 fills a triggered stop-limit at max(level, open) when that "
        "is within the cap, at the cap itself when the bar opened above it and traded back down "
        "to it, and not at all otherwise, always plus slippage. LEAN's own stop-limit fill model "
        "is believed, from its source rather than any observation, to trigger only when the "
        "bar's high exceeds the stop, to fill only if the bar's close is below the limit, at "
        "the lower of the bar's high and the limit, and to charge no slippage on a limit fill, "
        "in which case the fill reports a slippage of zero. If so, LEAN skips gaps ADR 0005 "
        "fills, fills some at a different price, and never costs more than the limit; each of "
        "these must be observed before any paper-trading gate. The stop-limit signature "
        "(symbol, quantity, stop, limit, asynchronous, tag, properties) is likewise unconfirmed.",
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


def format_time(moment):
    """Write LEAN's UtcTime as the RFC 3339 UTC text Go reads.

    LEAN's UtcTime is UTC by definition, so a naive value is read as UTC.
    """
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    moment = moment.astimezone(timezone.utc)
    if moment.microsecond:
        return moment.strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    return moment.strftime("%Y-%m-%dT%H:%M:%SZ")


def _positive_price(value):
    return type(value) in (int, float) and isfinite(value) and value > 0


def _positive_quantity(value):
    return type(value) is int and value > 0


def floor_to_tick(price, tick):
    """price rounded DOWN to a whole number of ticks.

    A stop-limit's limit is the engine's price cap, the most the order may
    pay (ADR 0005, as amended 2026-09-24), and LEAN rounds a price to the tick.
    Rounded down here, the limit LEAN holds never exceeds the cap the
    engine's hold was computed from, so rounding cannot let a fill cost more
    than its hold. Decimal arithmetic on the shortest repr, so a price that
    is a whole number of ticks is never floored a tick lower.
    """
    ticks = (Decimal(repr(price)) / Decimal(repr(tick))).to_integral_value(rounding=ROUND_FLOOR)
    return float(ticks * Decimal(repr(tick)))


def _whole(value):
    """A LEAN quantity (a decimal, read as a float) as an exact int, or None."""
    number = float(value)
    return int(number) if isfinite(number) and number == int(number) else None


class OrderDesk:
    """Place, amend and cancel LEAN orders exactly as the engine's decisions say,
    and turn what LEAN reports about them into the engine's inputs.

    LEAN's order book is the record of what has been submitted: every order's
    tag is the id of the decision that placed it (or, for an Exit Order, the
    decision now in force), so a redelivered decision is recognised from the
    book, never from this object's memory. What this object does remember is
    only what the engine sent and LEAN cannot hold: each tag's N for the
    slippage model, each Campaign's frozen N, which LEAN order is each Unit's
    Exit Order and at which source it rests, each Unit's opening fill id, and
    each Campaign's outstanding exit proposal. That memory lives for this run
    only; recovering it after a restart is not solved here.

    Two views (ADR 0004). LEAN trades, holds and charges commission in raw
    shares at raw prices. Every level, quantity and N the engine sends is in
    the split-adjusted view its signals and sizing read, and ADR 0004's
    amendment keeps a Campaign's money in the one view its fills are priced
    in, which is split-adjusted until a split can adjust a held position. So
    this desk converts at LEAN's boundary with one figure, ratio: the whole
    number of split-adjusted shares one raw share is, equal to a raw price
    over its split-adjusted one (whole_split_ratio). A level or N is
    multiplied by it on the way into LEAN and a fill price divided by it on
    the way out; a share count the other way round. quantity x price, and
    so every cash amount and the commission, is the same in both views.
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
        # LEAN order id -> what this adapter placed it for.
        self.orders = {}
        # (campaign_id, unit_index) -> the fill id that opened the Unit, which
        # a stop fill names (event.FillPayload.UnitIDs).
        self.unit_fill_ids = {}
        # campaign_id -> the outstanding strategy.exit.proposed id, which an
        # exit fill names (event.FillPayload.ProposalID).
        self.exit_proposals = {}
        # LEAN order id -> slippage LEAN charged on it (NSlippageModel).
        self.slippage_applied = {}
        # LEAN order id -> proposal id, for a cancellation LEAN has not yet
        # confirmed.
        self.pending_cancels = {}
        # LEAN order id -> proposal id, for every order whose cancellation
        # this adapter has requested, confirmed or not: the engine expired
        # its proposal, so no fill of it may ever reach the engine.
        self.cancel_requested = {}
        # LEAN order ids whose fill LEAN has reported but the engine has not
        # yet been told of.
        self.undelivered = set()
        self.submitted = 0
        self.rejected = 0
        self.first_order_placed = False
        # Split-adjusted shares per raw share (see the class doc); None until
        # the first bar states it (observe_ratio).
        self.ratio = None
        # When a split LEAN applied to a position or order is still to be
        # checked (require_split_applied); None otherwise.
        self.split_to_check = None
        # Actual instrument bars, never fill times or deferred snapshots.
        # ADR 0011 gives fill-chained Adds one additional observed session.
        self.bar_ends = []

    def _closed(self):
        """The LEAN order statuses in which an order no longer works at the broker."""
        return (self.lean.OrderStatus.Filled, self.lean.OrderStatus.Canceled,
                self.lean.OrderStatus.Invalid)

    def _tickets(self, predicate):
        # LEAN returns an enumerable that supports neither len nor indexing.
        return list(self.algorithm.Transactions.GetOrderTickets(predicate))

    def _open_tickets(self):
        return list(self.algorithm.Transactions.GetOpenOrderTickets(self.symbol))

    def require_flat(self, when):
        """Reconcile before trading: LEAN holds nothing and works no order for the instrument.

        docs/architecture.md: reconciliation precedes any executor's first
        submission. This adapter can mirror only positions and orders the
        engine produced in this run, so anything already present means the
        engine's state and LEAN's disagree, and the run must not trade on it.
        """
        holding = self.algorithm.Portfolio[self.symbol].Quantity
        working = len(self._open_tickets())
        if holding != 0 or working:
            raise Uncertain("reconciliation {}: LEAN holds {} shares of {!r} and has {} open "
                            "order(s) for it, but the engine starts from nothing; the adapter "
                            "trades only from a flat start (docs/architecture.md)".format(
                                when, holding, self.instrument, working))

    def _require_ratio(self):
        if self.ratio is None:
            raise Uncertain("no bar has yet stated how a raw share relates to a split-adjusted "
                            "one, so no engine figure can be placed in LEAN (ADR 0004)")
        return self.ratio

    def _flat(self):
        return self.algorithm.Portfolio[self.symbol].Quantity == 0 and not self._open_tickets()

    def observe_ratio(self, ratio, period_end):
        """Take the bar's own split ratio (whole_split_ratio of its two closes).

        Between splits every bar states the same ratio. A different one with
        no split reported (apply_split) is taken only while LEAN holds nothing
        and works no order, since no raw figure then depends on the old one;
        otherwise LEAN's raw shares and the engine's split-adjusted ones no
        longer agree, and the run stops.
        """
        if ratio == self.ratio:
            return
        if self.ratio is not None and not self._flat():
            raise Uncertain("the bar ending {} states {} split-adjusted shares per raw share where "
                            "the last stated {}, with no split reported, while LEAN holds {} "
                            "shares of {!r} or works an order for it; its raw position and the "
                            "engine's split-adjusted one no longer agree".format(
                                period_end, ratio, self.ratio,
                                self.algorithm.Portfolio[self.symbol].Quantity, self.instrument))
        if self.ratio is not None:
            self.algorithm.Log("adapter: split ratio {} -> {} at the bar ending {}, while flat".format(
                self.ratio, ratio, period_end))
        self.ratio = ratio

    def apply_split(self, split_factor, when):
        """Take the new ratio from a split LEAN reports as having occurred.

        The engine's split-adjusted view is adjusted for every split, later
        ones included, so a split changes none of its levels, quantities or
        N: only how many split-adjusted shares a raw share is. Only an n-for-1
        split, n a whole number of at least 2 that divides the current ratio,
        is carried across: every raw share becomes exactly n, and the ratio
        falls to ratio / n, still whole. Any other split (a 3-for-2, a reverse
        split) stops the run, whether or not LEAN holds anything, even where
        its quantities happen to divide.

        LEAN, under Raw normalisation, applies the split itself (observed on
        the pinned image). It has split the holding before this slice's
        OnData, but not the open orders: it divides each order's quantity by
        the factor, multiplies its stop by it rounded to the tick, and reports
        it as UpdateSubmitted after OnData, in the same time step. So the
        holding, and that every stored Exit Order is still working, are
        checked now; the orders' new figures once LEAN has made them
        (require_split_applied), before the next session can fill anything.
        """
        if self.ratio is None:
            return
        per_share = 1 / split_factor
        n = round(per_share) if isfinite(per_share) else 0
        if n < 2 or abs(per_share - n) > _SPLIT_RATIO_TOLERANCE * n or self.ratio % n:
            shares = Fraction(per_share).limit_denominator(1000) if isfinite(per_share) else None
            raise Uncertain("a {} split of {} at {} (factor {}), at {} split-adjusted shares per "
                            "raw share: only an n-for-1 split, n a whole number of at least 2 "
                            "that divides that ratio, is carried across (ADR 0004); any other "
                            "needs a corporate-action contract that does not exist yet".format(
                                "{}-for-{}".format(shares.numerator, shares.denominator)
                                if shares else "non-finite", self.instrument, when, split_factor,
                                self.ratio))
        ratio = self.ratio // n
        self.algorithm.Log("adapter: split of {} at {} (factor {}): split ratio {} -> {}".format(
            self.instrument, when, split_factor, self.ratio, ratio))
        self.ratio = ratio
        if self._flat():
            return
        self.split_to_check = when
        problems = self._holding_problems() + self._exit_orders_not_working()
        if problems:
            raise Uncertain("in the split of {} at {}, at {} split-adjusted shares per raw "
                            "share: {}".format(self.instrument, when, ratio, "; ".join(problems)))

    def _holding_problems(self):
        """Every held Unit rests one Exit Order (ADR 0005's amendment), so LEAN's
        raw holding is the sum of their quantities at the ratio in force."""
        protected = sum(order["quantity"] for order in self.exit_orders.values()) // self.ratio
        holding = self.algorithm.Portfolio[self.symbol].Quantity
        if holding != protected:
            return ["LEAN holds {} raw shares, but the engine's Units are {} raw shares".format(
                holding, protected)]
        return []

    def _exit_orders_not_working(self):
        """Each stored Exit Order whose LEAN order no longer works: its Unit has
        no stop, whatever the holding says."""
        problems = []
        for unit, order in sorted(self.exit_orders.items(), key=lambda item: str(item[0])):
            tickets = self._tickets(lambda t, oid=order["order_id"]: t.OrderId == oid)
            status = tickets[0].Status if tickets else "absent"
            if not tickets or status in self._closed():
                problems.append("LEAN order {} carrying campaign {!r} unit {}'s Exit Order is not "
                                "working (status {})".format(order["order_id"], unit[0], unit[1],
                                                             status))
        return problems

    def require_split_applied(self, when, final=True):
        """After a split, LEAN's raw position and orders are exactly the engine's.

        The holding is the sum of the Units at the new ratio; every stored
        Exit Order is still working; and each working order is for its
        split-adjusted quantity at the new ratio, resting within one tick of
        its split-adjusted level at it, since LEAN rounds a split stop to the
        tick. Anything else means the position or a stop is not what the
        engine believes, and the run stops: a split that needs anything more
        than LEAN's own adjustment has no corporate-action contract to carry
        it yet. Run first by the adapter's scheduled check, after the split's
        time step and before the next session's fills; then again, final, at
        the next slice's start, as a second line.
        """
        split_at = self.split_to_check
        if split_at is None:
            return
        if final:
            self.split_to_check = None
        ratio = self.ratio
        tick = float(self.algorithm.Securities[self.symbol].SymbolProperties.MinimumPriceVariation)
        problems = self._holding_problems() + self._exit_orders_not_working()
        holding = self.algorithm.Portfolio[self.symbol].Quantity
        for ticket in self._open_tickets():
            placed = self.orders.get(ticket.OrderId)
            if placed is None:
                problems.append("LEAN works order {}, which this adapter did not place".format(
                    ticket.OrderId))
                continue
            quantity, level = placed["quantity"] // ratio, placed["level"] * ratio
            stop = float(ticket.Get(self.lean.OrderField.StopPrice))
            if ticket.Quantity != quantity or abs(stop - level) > tick + 1e-9:
                problems.append("LEAN order {} (tag={}) is for {} raw shares at {:.4f}, but the "
                                "engine's {} split-adjusted shares at {} are {} raw shares at "
                                "{:.4f}".format(ticket.OrderId, ticket.Tag, ticket.Quantity, stop,
                                                placed["quantity"], placed["level"], quantity,
                                                level))
            if placed.get("price_cap") is not None:
                problems += self._split_limit_problems(ticket, stop, placed["price_cap"] * ratio,
                                                       placed["price_cap"], tick)
        if problems:
            raise Uncertain("after the split of {} at {}, at {} split-adjusted shares per raw "
                            "share, {}: {}".format(self.instrument, split_at, ratio, when,
                                                   "; ".join(problems)))
        self.algorithm.Log("adapter: split at {} reconciled {}: LEAN holds {} raw shares and "
                           "works {} order(s), as the engine's figures are at split ratio {}".format(
                               split_at, when, holding, len(self._open_tickets()), ratio))

    def _split_limit_problems(self, ticket, stop, cap, engine_cap, tick):
        """A working stop-limit's limit after a split, against its cap at the
        new ratio (ADR 0005 and ADR 0020, as amended 2026-09-24).

        LEAN rounds a split limit to the tick, either way. Rounded down, within
        a tick of the cap, it is kept: it can only pay less. Rounded above the
        cap, it could fill above what the engine's hold reserved, so it is
        amended down to the cap floored to the tick, here, before the next
        session can fill; if LEAN does not acknowledge the amendment, or the
        limit is still above the cap, the run stops. A limit further than a
        tick below the cap is not the order the engine placed, and stops the
        run too.

        Whatever it is kept or amended to, the limit must stay at or above the
        working stop (ADR 0005, as amended: the order fills at max(stop, open)
        within the cap). If the split rounded the stop above the cap floored
        to the tick, as it can when the cap is the level itself (gap_buffer_n
        0), no limit is both within the cap and at or above the stop, so the
        run stops rather than rest an order a touch of its stop could not
        fill. Moving the stop is not a correction ADR 0005 makes: the stop is
        the engine's level.
        """
        limit = float(ticket.Get(self.lean.OrderField.LimitPrice))
        if limit > cap + 1e-9:
            target = floor_to_tick(cap, tick)
            if target < stop - 1e-9:
                return ["LEAN order {} (tag={}) is limited at {:.4f}, above the engine's price cap "
                        "{} at {:.4f} raw, but the cap floored to the tick, {:.4f}, is below its "
                        "stop {:.4f}: no limit within the cap can fill at the stop".format(
                            ticket.OrderId, ticket.Tag, limit, engine_cap, cap, target, stop)]
            fields = self.lean.UpdateOrderFields()
            fields.LimitPrice = target
            response = ticket.Update(fields)
            amended = float(ticket.Get(self.lean.OrderField.LimitPrice))
            if not response.IsSuccess or amended > cap + 1e-9:
                return ["LEAN order {} (tag={}) is limited at {:.4f}, above the engine's price cap "
                        "{} at {:.4f} raw, and amending it to {:.4f} was not acknowledged "
                        "(limit now {:.4f})".format(ticket.OrderId, ticket.Tag, limit, engine_cap,
                                                    cap, target, amended)]
            self.algorithm.Log("adapter: amended order {} (tag={}) limit {} -> {} raw: the split "
                               "rounded it above the price cap {:.4f}".format(
                                   ticket.OrderId, ticket.Tag, limit, amended, cap))
            return []
        if limit < cap - tick - 1e-9:
            return ["LEAN order {} (tag={}) is limited at {:.4f}, but the engine's price cap {} "
                    "is {:.4f} raw".format(ticket.OrderId, ticket.Tag, limit, engine_cap, cap)]
        if limit < stop - 1e-9:
            return ["LEAN order {} (tag={}) is limited at {:.4f}, below its stop {:.4f}: a touch "
                    "of the stop could not fill".format(ticket.OrderId, ticket.Tag, limit, stop)]
        return []

    def n_for_tag(self, tag):
        """The raw N LEAN slips the tagged order by: the engine's split-adjusted
        N (ADR 0013) at the ratio in force when LEAN fills it."""
        n = self.n_by_tag.get(tag)
        if n is None:
            raise ValueError("no N was supplied for order tag {!r}; refusing to slip "
                             "it by zero (ADR 0013)".format(tag))
        return n * self._require_ratio()

    def record_slippage(self, order_id, slippage):
        """Record the raw slippage LEAN charged, restated in the split-adjusted view."""
        self.slippage_applied[order_id] = slippage / self._require_ratio()

    def observe_bar(self, period_end):
        """Remember the last two instrument bars for ADR 0011's Add window.

        No calendar-day forecast: a weekend, holiday or missing instrument
        bar is not another observed session. Replies and fills are not bars.
        """
        end = parse_time(period_end)
        if self.bar_ends and end <= self.bar_ends[-1]:
            raise Uncertain("completed bar period ends must advance")
        self.bar_ends = (self.bar_ends + [end])[-2:]

    def act(self, decisions, period_end, warming):
        """Act on one input's decisions, in the order the engine sent them.

        period_end is the session the decisions answer: a bar's period end, or
        the time of the fill that caused them. A decision answering a warm-up
        bar is never acted on: LEAN's warm-up exists to build history, not to
        trade (README.md).
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
                              payload.get("n"), entry=True)
            elif kind == ADD_PROPOSED:
                self._propose(decision, payload, bar_end, payload.get("level"),
                              payload.get("campaign_n"), entry=False)
            elif kind == PROPOSAL_EXPIRED:
                self._expire(payload)
            elif kind == CAMPAIGN_OPENED:
                self._campaign_opened(decision, payload)
            elif kind == EXIT_PROPOSED:
                self.exit_proposals[payload.get("campaign_id")] = decision.get("id")
            elif kind == UNIT_ADDED:
                self.unit_fill_ids[(payload.get("campaign_id"), payload.get("unit_index"))] = \
                    payload.get("fill_id")
            elif kind == UNITS_STOPPED:
                for index in payload.get("unit_indexes") or ():
                    self.exit_orders.pop((payload.get("campaign_id"), index), None)
            elif kind == CAMPAIGN_EXITED:
                self._campaign_exited(payload.get("campaign_id"))
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
        return self._tickets(lambda t: t.Tag == tag)

    def _price_cap_reason(self, payload, level):
        """Why the proposal's order type and price cap cannot be placed, or None.

        A stop-limit's cap must be a positive price at or above its level; a
        stop-market order carries none (event.TradeProposalPayload's own
        rule, ADR 0005 as amended 2026-09-24). Any other order type is one
        this adapter does not know how to place.
        """
        order_type, price_cap = payload.get("order_type"), payload.get("price_cap")
        if order_type == STOP_LIMIT:
            if not _positive_price(price_cap) or price_cap < level:
                return "price cap {!r} is not a positive price at or above level {!r}".format(
                    price_cap, level)
            return None
        if order_type == STOP_MARKET:
            if price_cap != 0:
                return "a stop-market proposal carries price cap {!r}; it has none".format(price_cap)
            return None
        return "order type {!r} is not one this adapter places".format(order_type)

    def _window_reason(self, payload, answered_at, entry):
        """Validate only the engine's explicit lifetime (ADR 0011 amendment).

        Before the next bar arrives, a fill's timestamp can be later than
        the last completed bar. After that bar arrives, only its own end
        still names the extra session; a later fill is already too old.
        """
        sessions = 1 if entry else payload.get("valid_for_sessions")
        if type(sessions) is not int or sessions not in (1, 2):
            return "valid_for_sessions {!r} must be integer 1 or 2 (ADR 0011)".format(sessions)
        try:
            produced = parse_time(payload.get("period_end"))
        except ValueError as err:
            return "period_end unreadable: {}".format(err)
        valid = produced == answered_at
        if sessions == 2:
            valid = bool(self.bar_ends) and (
                produced == self.bar_ends[-1] and answered_at >= produced
                or len(self.bar_ends) == 2 and produced == self.bar_ends[0]
                and answered_at == self.bar_ends[-1])
        if not valid:
            return ("stale: produced by the bar ending {}, valid for {} session(s), "
                    "but this answers {} (ADR 0011)".format(
                        payload.get("period_end"), sessions,
                        answered_at.strftime("%Y-%m-%dT%H:%M:%SZ")))
        return None

    def _propose(self, decision, payload, bar_end, level, n, entry):
        """An entry or Add: a good-till-cancelled buy order at the proposal's level.

        A stop-limit, limited at the proposal's price cap rounded down to the
        tick, when the proposal carries one (the Baseline); a stop-market order
        when it does not (the declared Variant "uncapped"; ADR 0005, as amended
        2026-09-24). The adapter computes no cap: it places the engine's.

        Entries and ordinary Adds answer their own bar. A fill-chained Add
        carries valid_for_sessions=2 and may also answer the first following
        session (ADR 0011 and ADR 0021 section 7, amended 2026-09-24). Actual
        instrument bars bound that window; no calendar date is forecast.
        The engine expires the proposal and the adapter cancels its order.
        A rejected proposal is logged and dropped: the engine re-issues it on a later bar
        if its setup still holds.
        """
        tag = decision.get("id")
        reason = self._tradable_reason(payload)
        if reason is None and entry:
            # The engine proposes an entry only for an instrument it holds no
            # Campaign in (CONTEXT.md: "Campaign"), so a holding in LEAN means
            # the two disagree about the position.
            holding = self.algorithm.Portfolio[self.symbol].Quantity
            if holding != 0:
                raise Uncertain("trade proposal {} for {!r} arrived while LEAN holds {} shares "
                                "of it; the engine believes it holds no Campaign there".format(
                                    tag, self.instrument, holding))
        if reason is None and not _positive_quantity(payload.get("quantity")):
            reason = "quantity {!r} is not a positive whole number of shares".format(
                payload.get("quantity"))
        if reason is None and not _positive_price(level):
            reason = "level {!r} is not a positive price".format(level)
        if reason is None and not _positive_price(n):
            reason = "N {!r} is not a positive figure to slip by (ADR 0013)".format(n)
        if reason is None:
            reason = self._price_cap_reason(payload, level)
        if reason is None and entry and payload.get("direction") != _LONG:
            reason = "direction {!r} is not {!r}".format(payload.get("direction"), _LONG)
        if reason is None:
            reason = self._window_reason(payload, bar_end, entry)
        if reason is None and not tag:
            reason = "no decision id to tag the order with"
        if reason is None:
            existing = self._already_submitted(tag)
            if existing:
                reason = "already submitted as LEAN order {}".format(existing[0].OrderId)
        ratio = self._require_ratio()
        # Whole raw shares, rounded down: never more than the Unit the engine
        # sized (ADR 0003). The fill reports what executed, which the engine
        # accepts as a Unit of at most the proposal's quantity.
        raw_quantity = payload["quantity"] // ratio if reason is None else 0
        if reason is None and raw_quantity == 0:
            reason = ("quantity {} split-adjusted shares is less than one raw share at {} "
                      "split-adjusted shares per raw share".format(payload["quantity"], ratio))
        if reason is not None:
            self._reject(decision, reason)
            return
        properties = self.lean.OrderProperties()
        properties.TimeInForce = self.lean.TimeInForce.GoodTilCanceled
        self.n_by_tag[tag] = n
        price_cap = payload["price_cap"] if payload.get("order_type") == STOP_LIMIT else None
        limit = None if price_cap is None else floor_to_tick(price_cap * ratio, self._tick())
        placed_stop = round(round(level * ratio / self._tick()) * self._tick(), 10)
        if limit is not None and limit < placed_stop:
            # LEAN places the stop rounded to the nearest tick, and the limit
            # is never raised above the engine's cap (ADR 0005 and ADR 0020,
            # as amended), so a cap whose tick floor falls below that placed
            # stop could never fill at the stop: rejected, not placed.
            self._reject(decision, "the tick-rounded limit {} is below the stop {} as placed".format(
                limit, placed_stop))
            return
        ticket = self._submit(raw_quantity, level * ratio, tag, properties, limit)
        if ticket.Status == self.lean.OrderStatus.Invalid:
            self._reject(decision, "LEAN refused the order (status {})".format(ticket.Status))
            return
        # level, cap and quantity in the engine's split-adjusted view;
        # quantity is what the raw order is, which may be less than the
        # proposal's.
        self.orders[ticket.OrderId] = {"kind": KIND_ENTRY if entry else KIND_ADD, "tag": tag,
                                       "campaign_id": payload.get("campaign_id", ""),
                                       "level": level, "price_cap": price_cap,
                                       "quantity": raw_quantity * ratio}

    def _tick(self):
        """The instrument's minimum price variation, which LEAN rounds prices to."""
        return float(self.algorithm.Securities[self.symbol].SymbolProperties.MinimumPriceVariation)

    def _submit(self, quantity, level, tag, properties, limit=None):
        """Submit one order, in raw shares at a raw level, reconciling first if
        it is the run's first: a stop-limit when limit is given, else a
        stop-market order.

        LEAN's StopMarketOrder signature is (symbol, quantity, stop_price,
        asynchronous, tag, order_properties): the tag is the fifth argument,
        never the fourth. StopLimitOrder is taken to follow it with the limit
        after the stop, (symbol, quantity, stop_price, limit_price,
        asynchronous, tag, order_properties); that signature is not yet
        observed on the pinned image (README.md).
        """
        if not self.first_order_placed:
            self.require_flat("before the first order")
        if limit is None:
            ticket = self.algorithm.StopMarketOrder(self.symbol, quantity, level, False, tag,
                                                    properties)
        else:
            ticket = self.algorithm.StopLimitOrder(self.symbol, quantity, level, limit, False, tag,
                                                   properties)
        self.first_order_placed = True
        if ticket.Status == self.lean.OrderStatus.Invalid:
            return ticket
        self.submitted += 1
        self.algorithm.Log("adapter: order {} {} {} raw shares @ {} raw{} (split ratio {}) "
                           "tag={}".format(ticket.OrderId, "buy" if quantity > 0 else "sell",
                                           abs(quantity), level,
                                           "" if limit is None else ", limit {} raw".format(limit),
                                           self.ratio, tag))
        return ticket

    def _expire(self, payload):
        """Cancel a still-working buy order whose proposal the engine expired (ADR 0011).

        LEAN answers a cancellation with CancelPending and confirms it with a
        Canceled event after the slice (README.md, "Observed LEAN behaviour"),
        so the request is remembered until that event arrives; a request LEAN
        refuses outright stops the run.
        """
        if payload.get("kind") not in _KINDS_WITH_AN_ORDER:
            if payload.get("kind") == "exit":
                for campaign, proposal in list(self.exit_proposals.items()):
                    if proposal == payload.get("proposal_id"):
                        del self.exit_proposals[campaign]
            return
        proposal_id = payload.get("proposal_id")
        for ticket in self._open_tickets():
            if ticket.Tag != proposal_id:
                continue
            response = ticket.Cancel()
            if not response.IsSuccess or ticket.Status not in (
                    self.lean.OrderStatus.CancelPending, self.lean.OrderStatus.Canceled):
                # An order the engine expired that is still working could fill
                # into a holding the engine does not expect.
                raise Uncertain("LEAN did not confirm cancelling order {} (tag={}) after its "
                                "proposal expired: response success={}, status {}".format(
                                    ticket.OrderId, proposal_id, response.IsSuccess, ticket.Status))
            self.cancel_requested[ticket.OrderId] = proposal_id
            if ticket.Status == self.lean.OrderStatus.Canceled:
                self._cancel_confirmed(ticket.OrderId, proposal_id)
            else:
                self.pending_cancels[ticket.OrderId] = proposal_id
                self.algorithm.Log("adapter: cancel requested for order {} tag={}: its proposal "
                                   "expired".format(ticket.OrderId, proposal_id))

    def _cancel_confirmed(self, order_id, proposal_id):
        self.pending_cancels.pop(order_id, None)
        self.algorithm.Log("adapter: cancelled order {} tag={}: its proposal expired".format(
            order_id, proposal_id))

    def require_cancels_confirmed(self, when, requested_earlier, queued=()):
        """Every cancellation requested in an earlier slice has been confirmed.

        LEAN confirms a cancellation after the slice it was requested in; one
        still unconfirmed when the next slice starts is an order that could
        fill into a holding the engine does not expect. requested_earlier is
        the set of orders whose cancellation was pending when this slice
        began. A cancellation this slice's own fill replies requested (a stop
        fill that closes the Campaign expires its pending Add at once; ADR
        0011, as amended 2026-09-24) cannot be confirmed until the slice
        ends, and is checked at the start of the next one. queued is the
        slice's undrained order reports: a Canceled report among them
        confirms its order, so the check can run before any of the slice's
        fills is sent.
        """
        confirmed = {r["order_id"] for r in queued if r["status"] == self.lean.OrderStatus.Canceled}
        unconfirmed = {o: t for o, t in self.pending_cancels.items()
                       if o in requested_earlier and o not in confirmed}
        if unconfirmed:
            raise Uncertain("LEAN did not confirm cancelling order(s) {} {}; their proposals "
                            "expired, so a fill would be one the engine does not expect".format(
                                ", ".join("{} (tag={})".format(o, t) for o, t in
                                          sorted(unconfirmed.items())), when))

    def _campaign_opened(self, decision, payload):
        """Remember the Campaign's frozen N (ADR 0006), for its Exit Orders' slippage,
        and Unit 1's opening fill id, which a stop fill names."""
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
        self.unit_fill_ids[(payload["campaign_id"], 1)] = payload.get("fill_id")

    def _campaign_exited(self, campaign_id):
        for unit in [u for u in self.exit_orders if u[0] == campaign_id]:
            del self.exit_orders[unit]
        self.exit_proposals.pop(campaign_id, None)

    def _working_sell_quantity(self):
        return sum(-(t.Quantity - t.QuantityFilled) for t in self._open_tickets() if t.Quantity < 0)

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
        if reason is None and quantity % self._require_ratio():
            # A Unit's quantity is its fill's, always whole raw shares, so
            # this can only be a disagreement about the Unit.
            reason = ("quantity {} split-adjusted shares is not a whole number of raw shares at "
                      "{} split-adjusted shares per raw share".format(quantity, self.ratio))
        if reason is not None:
            # Unlike an entry or Add, which the engine re-issues next bar, an
            # Exit Order the adapter cannot place leaves its Unit without a
            # stop, so the run stops rather than carrying on unprotected
            # (ADR 0019's amendment).
            raise Uncertain("exit order {} for campaign {!r} unit {}: {}; the Unit cannot be "
                            "protected as the engine believes".format(
                                tag, payload.get("campaign_id"), payload.get("unit_index"), reason))
        existing = self._already_submitted(tag)
        if existing:
            working = [t for t in existing if t.Status not in self._closed()]
            if not working:
                # The engine still sets this level, but no working order
                # carries it: the Unit is unprotected.
                raise Uncertain("exit order {} for campaign {!r} unit {}: LEAN order {} carrying "
                                "it is no longer working (status {}), but the engine still sets "
                                "this level".format(tag, payload.get("campaign_id"),
                                                    payload.get("unit_index"),
                                                    existing[0].OrderId, existing[0].Status))
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
            self._place_exit_order(decision, unit, quantity, level, tag, n, as_of, payload.get("source"))
        else:
            self._amend_exit_order(decision, unit, in_force, quantity, level, tag, n, as_of,
                                   payload.get("source"))

    def _place_exit_order(self, decision, unit, quantity, level, tag, n, as_of, source):
        """quantity and level are the engine's split-adjusted figures; LEAN's
        holding and working orders, and the order placed, are raw."""
        raw_quantity = quantity // self.ratio
        holding = self.algorithm.Portfolio[self.symbol].Quantity
        working = self._working_sell_quantity()
        if working + raw_quantity > holding:
            # LEAN's holding and the engine's Exit Orders disagree. Refusing
            # the order and carrying on would leave this Unit silently without
            # a stop, so the run stops instead: containment is a person's
            # decision (ADR 0019's amendment), never the system's.
            raise Uncertain("instrument {!r} campaign {!r} unit {}: its Exit Order {} for {} "
                            "raw shares would bring working sell orders to {}, past the holding "
                            "of {} (working sell quantity {}); the Unit cannot be protected "
                            "as the engine believes".format(
                                self.instrument, unit[0], unit[1], tag, raw_quantity,
                                working + raw_quantity, holding, working))
        properties = self.lean.OrderProperties()
        properties.TimeInForce = self.lean.TimeInForce.GoodTilCanceled
        self.n_by_tag[tag] = n
        ticket = self._submit(-raw_quantity, level * self.ratio, tag, properties)
        if ticket.Status == self.lean.OrderStatus.Invalid:
            # A refused Exit Order leaves its Unit without a stop.
            raise Uncertain("LEAN refused exit order {} for campaign {!r} unit {} (status {}); "
                            "the Unit cannot be protected as the engine believes".format(
                                tag, unit[0], unit[1], ticket.Status))
        # level and quantity in the engine's split-adjusted view.
        self.exit_orders[unit] = {"order_id": ticket.OrderId, "as_of": as_of, "source": source,
                                  "level": level, "quantity": quantity}
        self.orders[ticket.OrderId] = {"kind": KIND_STOP, "tag": tag, "campaign_id": unit[0],
                                       "unit": unit, "level": level, "quantity": -quantity}

    def _amend_exit_order(self, decision, unit, in_force, quantity, level, tag, n, as_of, source):
        tickets = self._tickets(lambda t: t.OrderId == in_force["order_id"])
        if len(tickets) == 1 and in_force["order_id"] in self.undelivered:
            # LEAN has already filled this Unit's order in the slice being
            # reported, and that fill is still to reach the engine: it states
            # what executed, so there is nothing left to amend (ADR 0005 rule 3:
            # a bar that fills an Add and a stop enters, then stops).
            self.algorithm.Log("adapter: {} {} for campaign {!r} unit {} not applied: its order "
                               "{} has already filled and that fill is reported next".format(
                                   decision["type"], tag, unit[0], unit[1], in_force["order_id"]))
            return
        if len(tickets) != 1 or tickets[0].Status in self._closed():
            raise Uncertain("campaign {!r} unit {}'s exit order {} is no longer working in LEAN, "
                            "but the engine still sets its level".format(
                                unit[0], unit[1], in_force["order_id"]))
        ticket = tickets[0]
        if -ticket.Quantity != quantity // self.ratio:
            raise Uncertain("campaign {!r} unit {}'s exit order {} sells {} raw shares, but the "
                            "engine says the Unit holds {} split-adjusted shares, which is {} raw "
                            "at {} per raw share".format(unit[0], unit[1], ticket.OrderId,
                                                         -ticket.Quantity, quantity,
                                                         quantity // self.ratio, self.ratio))
        if as_of < in_force["as_of"]:
            self._reject(decision, "older than the level in force for campaign {!r} unit {}".format(
                unit[0], unit[1]))
            return
        fields = self.lean.UpdateOrderFields()
        fields.StopPrice = level * self.ratio
        fields.Tag = tag
        self.n_by_tag[tag] = n
        response = ticket.Update(fields)
        if not response.IsSuccess:
            self.algorithm.Log("adapter: amendment of order {} to {} (tag={}) not acknowledged; "
                               "the previous level stays in force".format(ticket.OrderId, level, tag))
            return
        in_force.update(as_of=as_of, source=source, level=level)
        self.orders[ticket.OrderId].update(tag=tag, level=level)
        self.algorithm.Log("adapter: amended order {} to {} raw (split ratio {}) tag={}".format(
            ticket.OrderId, level * self.ratio, self.ratio, tag))

    def observe(self, order_event):
        """What LEAN reported about one order, read once, when it reported it.

        OnOrderEvent can fire in the middle of placing an order, so nothing is
        sent from it; the record waits in the algorithm's queue until the
        adapter reaches a point where an input may be sent (algorithm.py,
        drain_order_events).
        """
        tickets = self._tickets(lambda t: t.OrderId == order_event.OrderId)
        fee = order_event.OrderFee.Value
        stop_price = order_event.StopPrice
        return {
            "order_id": order_event.OrderId,
            "event_id": order_event.Id,
            "status": order_event.Status,
            "time": order_event.UtcTime,
            "tag": tickets[0].Tag if tickets else None,
            "quantity": order_event.Quantity,
            "fill_quantity": order_event.FillQuantity,
            "fill_price": order_event.FillPrice,
            "fee": fee.Amount,
            "fee_currency": fee.Currency,
            "stop_price": None if stop_price is None else float(stop_price),
            "message": order_event.Message or "",
        }

    def is_execution(self, record):
        status = self.lean.OrderStatus
        if record["status"] in (status.Filled, status.PartiallyFilled) \
                and record["order_id"] in self.cancel_requested:
            # The engine expired this order's proposal and the adapter asked
            # LEAN to cancel it, so a fill contradicts the engine's state and
            # is never sent: what to do about the position is a person's
            # decision (ADR 0011, as amended 2026-09-24; ADR 0019).
            raise Uncertain("LEAN reports order {} (tag={}) filled {} at {}, but its proposal "
                            "expired and this adapter requested its cancellation ({}); the fill "
                            "is not sent to the engine".format(
                                record["order_id"], record["tag"], record["fill_quantity"],
                                record["fill_price"],
                                "confirmed" if record["order_id"] not in self.pending_cancels
                                else "not yet confirmed"))
        if record["status"] == status.PartiallyFilled:
            # The engine accepts one fill per order: a second partial of the
            # same entry is refused, and a partial stop or exit cannot close
            # its Units. Until successive partial fills accumulate into one
            # Unit, a partial fill stops the run rather than invent position
            # logic here (internal/strategy, applyFillToOpenCampaign).
            raise Uncertain("LEAN reports order {} (tag={}) partially filled: {} of {} at {}; "
                            "partial fills are not yet accumulated into one Unit (#67), so the "
                            "run stops".format(record["order_id"], record["tag"],
                                               record["fill_quantity"], record["quantity"],
                                               record["fill_price"]))
        return record["status"] == status.Filled

    def lifecycle(self, record):
        """One order change that is not an execution, as event.OrderLifecyclePayload.

        Its quantity and stop price are the order's own, as LEAN states them:
        raw, like the broker's order book that reconciliation compares them
        with (ADR 0019). The engine decides nothing from them.
        """
        status = self.lean.OrderStatus
        names = {status.Submitted: "submitted", status.UpdateSubmitted: "updated",
                 status.CancelPending: "cancel-pending", status.Canceled: "canceled",
                 status.Invalid: "invalid"}
        name = names.get(record["status"])
        if name is None:
            raise Uncertain("LEAN reports order {} (tag={}) in status {}, which this adapter "
                            "cannot state as an order lifecycle change".format(
                                record["order_id"], record["tag"], record["status"]))
        stop_price = record["stop_price"]
        level = (self.orders.get(record["order_id"]) or {}).get("level")
        if stop_price is None and level is not None:
            stop_price = level * self._require_ratio()
        quantity = _whole(record["quantity"])
        if not quantity or not _positive_price(stop_price):
            raise Uncertain("LEAN reports order {} (tag={}) {} with quantity {} and stop price {}; "
                            "an order change needs both to be stated".format(
                                record["order_id"], record["tag"], name, record["quantity"],
                                stop_price))
        if name == "canceled" and record["order_id"] in self.pending_cancels:
            self._cancel_confirmed(record["order_id"], self.pending_cancels[record["order_id"]])
        return {"instrument_id": self.instrument, "order_id": str(record["order_id"]),
                "tag": record["tag"] or "", "status": name, "quantity": quantity,
                "stop_price": stop_price, "occurred_at": format_time(record["time"]),
                "message": record["message"]}

    def fills(self, records):
        """One slice's fills as the engine's fill inputs, in ADR 0005's order.

        records are every fill LEAN reported at one instant. Each becomes the
        FillPayload the reducer expects for its order's kind, mirroring
        internal/fills: an entry or Add names its proposal; an Exit Order
        resting at its own stop is a stop fill naming its one Unit; and the
        Exit Orders resting at the Exit Channel together are one exit fill for
        the exit proposal, since an exit closes the whole remaining holding.
        Buys come first, then stop fills worst price first, then the exit. The
        kind of every Exit Order is the source the engine last set for it;
        nothing here compares a price with a level.

        LEAN's raw execution is restated in the engine's split-adjusted view
        (ADR 0004's amendment: a Campaign's money stays in the one view its
        fills are priced in): quantity x ratio, and price, level and slippage
        / ratio. The commission is cash, the same in both views, and is LEAN's
        charge on the raw shares it traded.

        Returns [(payload, [order ids])]; the order ids are marked undelivered
        until the caller has sent their fill.
        """
        buys, stops, exits = [], [], []
        for record in records:
            placed = self.orders.get(record["order_id"])
            if placed is None:
                raise Uncertain("LEAN reports a fill of order {} (tag={}), which this adapter did "
                                "not place".format(record["order_id"], record["tag"]))
            quantity = _whole(record["fill_quantity"])
            ordered = _whole(record["quantity"])
            if quantity is None or ordered is None or quantity == 0 or quantity != ordered:
                raise Uncertain("LEAN reports order {} (tag={}) filled {} of {}; only a complete "
                                "fill of a whole number of shares can be returned to the engine "
                                "(#67)".format(record["order_id"], record["tag"],
                                               record["fill_quantity"], record["quantity"]))
            price = float(record["fill_price"])
            fee = float(record["fee"])
            if not _positive_price(price) or not isfinite(fee) or fee < 0 \
                    or record["fee_currency"] != _USD:
                raise Uncertain("LEAN reports order {} (tag={}) filled at {} for a fee of {} {}; "
                                "a fill needs a positive price and a finite, non-negative USD "
                                "fee".format(record["order_id"], record["tag"], price, fee,
                                             record["fee_currency"]))
            buy = placed["kind"] in (KIND_ENTRY, KIND_ADD)
            if buy != (quantity > 0):
                raise Uncertain("LEAN reports order {} (tag={}) filled {} shares, but it is the "
                                "adapter's {} order".format(record["order_id"], record["tag"],
                                                            quantity, placed["kind"]))
            self.algorithm.Log("adapter: LEAN filled order {} (tag={}): {} raw shares @ {} raw, "
                               "commission {} {} (split ratio {})".format(
                                   record["order_id"], record["tag"], quantity, price, fee,
                                   record["fee_currency"], self._require_ratio()))
            fill = {"record": record, "placed": placed, "quantity": abs(quantity),
                    "price": price, "fee": fee}
            if buy:
                buys.append(fill)
            elif self.exit_orders.get(placed["unit"], {}).get("source") == SOURCE_EXIT_CHANNEL:
                exits.append(fill)
            else:
                stops.append(fill)
        if len(buys) > 1:
            raise Uncertain("LEAN reports {} buy fills at one instant; the engine has at most one "
                            "entry or Add outstanding".format(len(buys)))
        stops.sort(key=lambda f: (f["price"], f["placed"]["unit"][1]))
        out = [self._single(f) for f in buys + stops]
        if exits:
            out.append(self._exit(exits))
        for _, order_ids in out:
            self.undelivered.update(order_ids)
        return out

    def delivered(self, order_ids):
        self.undelivered.difference_update(order_ids)

    def _costs(self, fills):
        """The fill's price, level, slippage and commission, in the split-adjusted view."""
        record = fills[0]["record"]
        ratio = self._require_ratio()
        # The level LEAN rested the order at, raw; the engine's own level only
        # if LEAN's event states none.
        levels = {f["record"]["stop_price"] / ratio if f["record"]["stop_price"] is not None
                  else f["placed"]["level"] for f in fills}
        slippages = {self.slippage_applied.get(f["record"]["order_id"], 0.0) for f in fills}
        if len(levels) != 1 or len(slippages) != 1 or len({f["price"] for f in fills}) != 1 \
                or len({f["record"]["time"] for f in fills}) != 1:
            raise Uncertain("LEAN filled the Exit Orders of one exit at more than one level, "
                            "price, slippage or time (orders {}); one exit fill cannot state "
                            "them".format(sorted(f["record"]["order_id"] for f in fills)))
        return {"instrument_id": self.instrument, "direction": _LONG,
                "price": fills[0]["price"] / ratio, "filled_at": format_time(record["time"]),
                "level": levels.pop(), "slippage_applied": slippages.pop(),
                "commission": sum(f["fee"] for f in fills)}

    def _single(self, fill):
        record, placed = fill["record"], fill["placed"]
        payload = self._costs([fill])
        payload.update(kind=placed["kind"], quantity=fill["quantity"] * self.ratio, unit_ids=[],
                       fill_id="lean:{}:{}".format(record["order_id"], record["event_id"]))
        if placed["kind"] == KIND_ENTRY:
            payload.update(proposal_id=placed["tag"], campaign_id="")
        elif placed["kind"] == KIND_ADD:
            payload.update(proposal_id=placed["tag"], campaign_id=placed["campaign_id"])
        else:
            unit_fill = self.unit_fill_ids.get(placed["unit"])
            if not unit_fill:
                raise Uncertain("LEAN filled order {} for campaign {!r} unit {}, whose opening "
                                "fill the engine never named; a stop fill must name the Unit it "
                                "closes".format(record["order_id"], *placed["unit"]))
            payload.update(proposal_id="", campaign_id=placed["campaign_id"], unit_ids=[unit_fill])
        return payload, [record["order_id"]]

    def _exit(self, fills):
        campaigns = {f["placed"]["campaign_id"] for f in fills}
        campaign_id = campaigns.pop()
        if campaigns:
            raise Uncertain("LEAN filled Exit Orders of more than one Campaign at the Exit Channel "
                            "at one instant")
        resting = {u for u, order in self.exit_orders.items()
                   if u[0] == campaign_id and order["source"] == SOURCE_EXIT_CHANNEL}
        filled = {f["placed"]["unit"] for f in fills}
        if resting != filled:
            # An exit fill closes everything the Campaign still holds
            # (CONTEXT.md: "Campaign"), so it cannot describe a slice in which
            # only some of the Units resting at the exit level filled.
            raise Uncertain("campaign {!r}: LEAN filled the Exit Orders of unit(s) {} at the Exit "
                            "Channel, but unit(s) {} rest there; one exit fill cannot state "
                            "that".format(campaign_id, sorted(u[1] for u in filled),
                                          sorted(u[1] for u in resting)))
        proposal_id = self.exit_proposals.get(campaign_id)
        if not proposal_id:
            raise Uncertain("campaign {!r}: LEAN filled its Exit Orders at the Exit Channel, but "
                            "the engine has no exit proposal outstanding for it".format(campaign_id))
        fills = sorted(fills, key=lambda f: f["placed"]["unit"][1])
        payload = self._costs(fills)
        payload.update(kind=KIND_EXIT, proposal_id=proposal_id, campaign_id=campaign_id,
                       unit_ids=[], quantity=sum(f["quantity"] for f in fills) * self.ratio,
                       fill_id="lean:" + "+".join("{}:{}".format(f["record"]["order_id"],
                                                                 f["record"]["event_id"])
                                                  for f in fills))
        return payload, [f["record"]["order_id"] for f in fills]
