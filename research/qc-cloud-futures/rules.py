"""Faith's original Turtle rules, applied to futures -- the Turtles' own
market -- long AND short, symmetric (docs/methodology/Methodology_Analysis.md;
The Original Turtle Trading Rules, Curtis Faith, 2003, cited below as [T
p.N]). This is a SEPARATE research check from research/qc-cloud (the
equity Baseline): that folder halves Faith's Unit Volatility Fraction and
trades long-only, both adaptations argued in ADR 0003 specifically for a
long-only, single-regime EQUITY book. Neither argument applies to a
diversified, symmetric futures book -- the population Faith's own numbers
describe -- so this module's defaults are Faith's own figures, unadapted,
with the equity Baseline's halving available only as an explicit parameter
override for comparison.

This module has NO QuantConnect imports and no dependency on this repo's Go
packages: pure, standard-library Python, unit-tested with
`python3 -m unittest` (test_rules.py) so `main.py` (the thin QCAlgorithm
that runs on QuantConnect's Free plan, which includes futures data) can
import it unchanged after both files are pasted into a QuantConnect cloud
project.

Every rule below cites Methodology_Analysis.md / The Turtle Rules by page,
or the ADR it transcribes or symmetrically generalises (Faith's own text is
written for long entries; this module's short-side mirror is this script's
own reasoned, documented extension of the same stated rule, never a
separate source). research/qc-cloud-futures/README.md lists every place
this differs from Faith's text and from the equity Baseline, and why.

This is research-only. It is not the production Go engine
(internal/strategy, internal/sizing, internal/indicator) and makes no claim
of bit-for-bit parity with it.
"""

from collections import deque, namedtuple
from math import floor, isfinite

# ---------------------------------------------------------------------------
# Faith's own parameters (Methodology_Analysis.md Section 2; The Turtle
# Rules), unadapted -- this module's whole point is to run them as Faith
# specified, on the market Faith specified them for.
# ---------------------------------------------------------------------------

#: N's lookback, in completed bars (The Turtle Rules p.13).
N_PERIOD = 20

#: System 2's Entry Channel length, in completed bars (The Turtle Rules
#: p.18-19: "System 2 ... every 55-day breakout is taken"; ADR 0002: "The
#: Baseline is System 2"). System 1 (20-day, with its own skip-if-the-
#: previous-signal-was-a-winner filter [T p.19]) is out of scope for this
#: module, for the identical reasoning ADR 0002 gives: no phantom,
#: path-dependent second simulation to track signals not taken.
ENTRY_CHANNEL_LENGTH = 55

#: System 2's Exit Channel length, in completed bars (The Turtle Rules p.26:
#: "System 2: 20-day low / high, all Units").
EXIT_CHANNEL_LENGTH = 20

#: The Stop Multiple: N between entry and the Protective Stop (The Turtle
#: Rules p.22: "2N ... so that no trade risks more than 2%").
STOP_MULTIPLE = 2.0

#: Add Ladder spacing, in N, from the actual fill of the previous Unit (The
#: Turtle Rules p.19-20).
ADD_SPACING_N = 0.5

#: Faith's four Unit-cap levels (The Turtle Rules p.16): per market, per
#: closely-correlated group (one direction), per loosely-correlated group
#: (one direction), and total per single direction (all long, or all
#: short).
MAX_UNITS_PER_MARKET = 4
MAX_UNITS_PER_CLOSELY_CORRELATED_GROUP = 6
MAX_UNITS_PER_LOOSELY_CORRELATED_GROUP = 10
MAX_UNITS_PER_DIRECTION = 12

#: Faith's own Unit Volatility Fraction (The Turtle Rules p.14: "1 Unit = 1%
#: of account / (N x dollars per point)"). ADR 0003 halves this to 0.5% for
#: the equity Baseline, reasoned there as "the cheapest available insurance"
#: for a long-only book in one regime lacking the futures Turtles' own
#: cross-asset diversification; this module trades the diversified, long-
#: AND-short book that argument does not apply to, so the default here is
#: Faith's own figure. It remains a parameter (``unit_volatility_fraction``
#: on every call into ``unit_quantity``) so the equity Baseline's 0.5% is a
#: directly runnable comparison against Faith's own figure.
UNIT_VOLATILITY_FRACTION = 0.01

#: ADR 0007's Drawdown Step (The Turtle Rules p.17): a 10% fall of the
#: current Notional Account triggers a 20% reduction. Faith's own numbers,
#: not equity-adapted.
DRAWDOWN_THRESHOLD_FRACTION = 0.10
DRAWDOWN_RETAINED_FRACTION = 0.8

#: ADR 0013: slippage is 0.05N per fill, against the trader, never zero --
#: an instrument-agnostic rule, unchanged for futures.
SLIPPAGE_N = 0.05

#: ADR 0005's amendment: entries and Adds rest as stop-limit orders capped
#: at level +/- k*N (the sign follows direction; see ``price_cap``). The
#: Baseline declares k=1.
GAP_BUFFER_N = 1.0

#: The Turtle Rules p.29 / ADR 0010 (as amended): Strength's lookback, in
#: completed bars -- "(price - price 3 months ago) / N", 63 trading days
#: standing in for three months, as ADR 0010 already reasons for equities.
STRENGTH_LOOKBACK_BARS = 63


# ---------------------------------------------------------------------------
# True Range and N (The Turtle Rules p.13).
# ---------------------------------------------------------------------------


def true_range(high, low, previous_close):
    """True Range (The Turtle Rules p.13):

        True Range = max(high - low, high - previous_close, previous_close - low)

    ``previous_close`` is ``None`` for the very first bar in a series (no
    prior close to measure a gap against): True Range there is the bar's
    own range. Direction-agnostic: True Range measures volatility, not a
    position's own direction.
    """
    if previous_close is None:
        return high - low
    return max(high - low, high - previous_close, previous_close - low)


class WilderN:
    """N: Wilder's 20-day average of True Range (The Turtle Rules p.13):

        N = (19 x previous_N + TR) / 20

    seeded with a simple average of the first ``period`` True Range values.
    Callers MUST read ``value``/``ready`` for the bar under decision BEFORE
    calling ``add`` with that same bar's True Range (evaluate-then-add).
    """

    def __init__(self, period=N_PERIOD):
        if period <= 0:
            raise ValueError("period must be positive")
        self.period = period
        self._seed = []
        self._count = 0
        self._value = 0.0

    def add(self, tr):
        self._count += 1
        if self._count < self.period:
            self._seed.append(tr)
            return
        if self._count == self.period:
            self._seed.append(tr)
            self._value = sum(self._seed) / len(self._seed)
            self._seed = None
            return
        self._value = ((self.period - 1) * self._value + tr) / self.period

    @property
    def ready(self):
        return self._count >= self.period

    @property
    def value(self):
        return self._value


# ---------------------------------------------------------------------------
# System 2's 55-day Entry Channel and 20-day Exit Channel (The Turtle Rules
# p.18-19, p.26), symmetric for long and short.
# ---------------------------------------------------------------------------


class Channel:
    """A rolling window of highs and lows (The Turtle Rules p.18-19, p.26).

    Faith's Donchian breakout is symmetric: a long enters on a strict high
    above the rolling HIGH, a short on a strict low below the rolling LOW,
    and each System's exit is the mirror image at its own shorter length.
    Rather than two directional classes, one ``Channel`` tracks both sides
    and ``extreme(direction)`` reads whichever a caller needs: the rolling
    high for ``direction=1``, the rolling low for ``direction=-1``. The same
    class serves the 55-bar Entry Channel and the 20-bar Exit Channel; only
    ``length`` differs.

    Callers MUST call ``extreme()`` to evaluate the bar under decision
    BEFORE calling ``add()`` with that same bar's high/low -- the
    evaluate-then-add discipline that keeps a channel from including the
    very bar it is being asked to decide.
    """

    def __init__(self, length):
        if length <= 0:
            raise ValueError("length must be positive")
        self.length = length
        self._highs = deque(maxlen=length)
        self._lows = deque(maxlen=length)

    def extreme(self, direction):
        """(value, ready): the rolling high (``direction=1``) or rolling low
        (``direction=-1``), and whether ``length`` bars have been added.
        ``(0.0, False)`` before any bar has been added."""
        if not self._highs:
            return 0.0, False
        ready = len(self._highs) >= self.length
        value = max(self._highs) if direction > 0 else min(self._lows)
        return value, ready

    def add(self, high, low):
        self._highs.append(high)
        self._lows.append(low)


def is_breakout(direction, bar_high, bar_low, level):
    """A System 2 breakout (The Turtle Rules p.18-19): price STRICTLY
    exceeding the Entry Channel -- above, for a long; below, for a short. A
    tie is not a breakout."""
    return bar_high > level if direction > 0 else bar_low < level


def exit_channel_breach(direction, bar_high, bar_low, exit_extreme, ready):
    """The Turtle Rules p.26: a Campaign's Exit-Channel exit is proposed the
    moment price STRICTLY reverses through the Exit Channel -- below it, for
    a long (a strict low below the rolling low); above it, for a short (a
    strict high above the rolling high). Returns the breached level, or
    ``None`` when no exit is proposed and every Exit Order rests at its
    Unit's own Protective Stop."""
    if not ready:
        return None
    if direction > 0:
        return exit_extreme if bar_low < exit_extreme else None
    return exit_extreme if bar_high > exit_extreme else None


# ---------------------------------------------------------------------------
# Unit sizing (The Turtle Rules p.14-15).
# ---------------------------------------------------------------------------


def unit_quantity(notional_account, unit_volatility_fraction, n, dollars_per_point):
    """Faith's Unit-sizing formula (The Turtle Rules p.14):

        quantity = floor( notional_account x unit_volatility_fraction
                           ------------------------------------------ )
                                   n x dollars_per_point

    Truncated toward zero, never rounded: Faith's own Heating Oil worked
    example (The Turtle Rules p.14-15) is N=0.0141, a $1,000,000 account, a
    42,000-gallon contract (dollars_per_point=42,000), his own 1% Unit
    Volatility Fraction -> 16.88, truncated to 16 (test_rules.py's golden
    test reproduces this exactly). ``dollars_per_point`` is the contract's
    own multiplier -- QuantConnect's ``SymbolProperties.ContractMultiplier``
    (main.py) -- never assumed to be 1 as it is for shares.

    Returns 0, with no error, when the account cannot fund a single
    contract (The Turtle Rules p.15: "small accounts lose diversification"
    -- truncation is this coarse); the caller declines the whole Unit
    (no partial Units).
    """
    if not (isfinite(notional_account) and notional_account > 0):
        raise ValueError("notional_account must be finite and positive")
    if not (isfinite(unit_volatility_fraction) and 0 < unit_volatility_fraction <= 1):
        raise ValueError("unit_volatility_fraction must be in (0, 1]")
    if not (isfinite(n) and n > 0):
        raise ValueError("n must be finite and positive: a zero or not-yet-warm N never sizes a position")
    if not (isfinite(dollars_per_point) and dollars_per_point > 0):
        raise ValueError("dollars_per_point must be finite and positive")

    budget = notional_account * unit_volatility_fraction
    cost_per_unit_per_n = n * dollars_per_point
    quotient = floor(budget / cost_per_unit_per_n)
    # A quotient whose true value sits just below an integer can round UP
    # under floating-point division; one step back is always the
    # conservative correction.
    if quotient > 0 and quotient * cost_per_unit_per_n > budget:
        quotient -= 1
    return int(quotient)


# ---------------------------------------------------------------------------
# The Add Ladder and Stop Ladder (The Turtle Rules p.19-20, p.22-23),
# symmetric for long and short.
# ---------------------------------------------------------------------------


def next_add_level(previous_fill, campaign_n, direction, spacing_n=ADD_SPACING_N):
    """The Add Ladder's next rung (The Turtle Rules p.19-20):

        level = previous_fill + direction x spacing_n x campaign_n

    ``previous_fill`` is the ACTUAL fill price of the Unit immediately
    before this one, never its proposed level, so slippage on one fill
    shifts every later rung [T p.19]. ``direction`` is +1 for a long
    Campaign (the rung sits ABOVE the previous fill, The Turtle Rules p.20's
    printed Gold/Crude ladders) or -1 for a short (this module's own
    symmetric mirror image: the rung sits BELOW). ``campaign_n`` is the
    Campaign's FROZEN N (ADR 0006, unchanged for futures).
    """
    if not (isfinite(previous_fill) and previous_fill > 0):
        raise ValueError("previous_fill must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    if direction not in (1, -1):
        raise ValueError("direction must be 1 (long) or -1 (short)")
    return previous_fill + direction * spacing_n * campaign_n


def protective_stop_level(entry_price, campaign_n, direction, stop_multiple=STOP_MULTIPLE):
    """A Unit's own initial Protective Stop (The Turtle Rules p.22):

        level = entry_price - direction x stop_multiple x campaign_n

    For a long (``direction=1``) the stop sits BELOW entry, Faith's own
    printed single-Unit Crude example: entry 28.30, N 1.20, Stop Multiple 2
    -> stop 25.90. For a short (``direction=-1``) this module's own
    symmetric mirror places it ABOVE entry by the same distance -- Faith
    trades long and short identically [T p.18-19], and "no trade risks more
    than 2%" [T p.22] does not depend on which side of the market the trade
    is on.
    """
    if not (isfinite(entry_price) and entry_price > 0):
        raise ValueError("entry_price must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    if not (isfinite(stop_multiple) and stop_multiple > 0):
        raise ValueError("stop_multiple must be finite and positive")
    if direction not in (1, -1):
        raise ValueError("direction must be 1 (long) or -1 (short)")
    level = entry_price - direction * stop_multiple * campaign_n
    if level <= 0:
        raise ValueError("derived protective stop level is not positive")
    return level


def raised_stop(previous_stop, campaign_n, direction, spacing_n=ADD_SPACING_N):
    """The Stop Ladder's raise (The Turtle Rules p.22-23): applied to EVERY
    earlier Unit's own current stop each time a further Unit is added --
    "the stops for earlier units were raised by 1/2 N", literally raising
    each Unit's OWN stop, never resetting every stop to a fixed distance
    below the newest fill (Faith's own Crude gap example shows stops
    diverging per Unit when a later Unit fills away from its rung). For a
    short, this module's mirror LOWERS the stop by the same distance.
    """
    if not (isfinite(previous_stop) and previous_stop > 0):
        raise ValueError("previous_stop must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    if direction not in (1, -1):
        raise ValueError("direction must be 1 (long) or -1 (short)")
    return previous_stop + direction * spacing_n * campaign_n


class Campaign:
    """One Campaign's life (CONTEXT.md "Campaign"), direction-aware:
    opened by a first fill, Units added along the Add Ladder up to
    ``max_units`` (4, The Turtle Rules p.16), each Unit's own Protective
    Stop moved by the Stop Ladder as later Units are added, N and the
    opening Unit quantity frozen at first entry (ADR 0006, unchanged for
    futures). ``direction`` is +1 (long) or -1 (short); every ladder
    formula above already takes it, so this class is otherwise identical
    for both sides.

    ``closely_group``/``loosely_group`` are the correlation-group labels
    (main.py's ``CORRELATION_GROUPS``) this Campaign's Unit-cap headroom
    was reserved under -- read once, at entry, and never re-read (the same
    freeze-at-entry discipline ADR 0006 applies to N and the Unit size).
    """

    def __init__(self, symbol, direction, entry_fill_price, campaign_n, unit_quantity_value,
                 closely_group, loosely_group, stop_multiple=STOP_MULTIPLE,
                 max_units=MAX_UNITS_PER_MARKET, spacing_n=ADD_SPACING_N):
        if direction not in (1, -1):
            raise ValueError("direction must be 1 (long) or -1 (short)")
        self.symbol = symbol
        self.direction = direction
        self.campaign_n = campaign_n
        self.unit_quantity = unit_quantity_value
        self.closely_group = closely_group
        self.loosely_group = loosely_group
        self.stop_multiple = stop_multiple
        self.max_units = max_units
        self.spacing_n = spacing_n
        initial_stop = protective_stop_level(entry_fill_price, campaign_n, direction, stop_multiple)
        self.units = [{"fill_price": entry_fill_price, "stop": initial_stop}]
        self._original_entry_price = entry_fill_price
        self.realized_price_pnl = 0.0
        # The Baseline never re-adds once any Unit has been stopped out
        # (The Turtle Rules p.23-24's Whipsaw re-entry alternative is a
        # declared Variant, not run here).
        self.partially_stopped = False

    @property
    def unit_count(self):
        return len(self.units)

    @property
    def loaded(self):
        return self.unit_count >= self.max_units

    def next_add_rung(self):
        """The next Add Ladder rung, or ``None`` when no further Add is
        possible (Loaded, or partially stopped)."""
        if self.loaded or self.partially_stopped:
            return None
        last_fill = self.units[-1]["fill_price"]
        return next_add_level(last_fill, self.campaign_n, self.direction, self.spacing_n)

    def add_unit(self, fill_price):
        """Record a new Unit's actual fill: move every EARLIER Unit's own
        stop by the Stop Ladder, then give the new Unit its own initial
        stop from ITS OWN fill (The Turtle Rules p.22-23)."""
        if self.loaded:
            raise ValueError("campaign already Loaded: no further Add")
        if self.partially_stopped:
            raise ValueError("campaign already partially stopped: no further Add")
        for unit in self.units:
            unit["stop"] = raised_stop(unit["stop"], self.campaign_n, self.direction, self.spacing_n)
        self.units.append({
            "fill_price": fill_price,
            "stop": protective_stop_level(fill_price, self.campaign_n, self.direction, self.stop_multiple),
        })

    def protective_stop(self):
        """The Campaign's own summary Protective Stop: for a long, the
        LOWEST of its Units' own stops; for a short, the HIGHEST -- in both
        cases the stop furthest from current price, the level beyond which
        every Unit has already been stopped out (CONTEXT.md "Protective
        Stop"). Each Unit still rests its OWN order at its OWN level; this
        is a reporting convenience, not itself a traded order."""
        return min(u["stop"] for u in self.units) if self.direction > 0 \
            else max(u["stop"] for u in self.units)

    def remove_units(self, indices):
        """Remove the Units at ``indices``, and mark the Campaign partially
        stopped if any Unit survives -- a full exit (no survivors) is the
        Campaign's own end, not a "partial" stop."""
        surviving = set(range(len(self.units))) - set(indices)
        self.units = [self.units[i] for i in sorted(surviving)]
        if self.units:
            self.partially_stopped = True

    def close_units(self, indices, exit_price):
        """Realise each given Unit's own (exit - fill) price distance,
        SIGNED BY DIRECTION so a short's profit (price falling) is
        positive, into the Campaign's running ``realized_price_pnl``, then
        remove them -- so ``r_multiple()`` reflects every Unit the Campaign
        ever held, not only whichever happens to close last."""
        for index in indices:
            self.realized_price_pnl += self.direction * (exit_price - self.units[index]["fill_price"])
        self.remove_units(indices)

    def r_multiple(self):
        """The Campaign's own R multiple, once fully closed: total realised
        price P&L (already direction-signed) over the Campaign's 1-Unit
        initial risk (Stop Multiple x campaign N). Every Unit shares the
        same frozen unit_quantity (ADR 0006), so that common factor -- and
        dollars_per_point, also common across a Campaign's own Units --
        cancels out of both the numerator and the denominator."""
        return self.realized_price_pnl / (self.stop_multiple * self.campaign_n)

    def entry_price(self):
        """The Campaign's own ORIGINAL entry: Unit 1's fill price, frozen at
        construction, never ``self.units[0]`` (which after a partial stop
        is whichever Unit SURVIVED, not necessarily the first)."""
        return self._original_entry_price


# ---------------------------------------------------------------------------
# The Notional Account and drawdown re-basing (The Turtle Rules p.17).
# ---------------------------------------------------------------------------


class NotionalAccount:
    """The Notional Account (The Turtle Rules p.17; ADR 0007, unchanged for
    futures): re-based to actual equity every 1 January by default; a 10%
    fall of the CURRENT (already-reduced) Notional Account multiplies it by
    0.8 -- Faith's own $1,000,000 -> $800,000 -> $640,000 worked example --
    and it recovers to the full yearly starting figure only once actual
    equity regains it, never on a partial recovery or a new high.
    """

    def __init__(self, starting_equity, rebasing_month=1, rebasing_day=1):
        if not (isfinite(starting_equity) and starting_equity > 0):
            raise ValueError("starting_equity must be finite and positive")
        self.starting_figure = float(starting_equity)
        self.current = float(starting_equity)
        self.base = float(starting_equity)
        self.rebasing_month = rebasing_month
        self.rebasing_day = rebasing_day
        self._rebased_label = None
        self.steps_applied = 0

    def _rebasing_label(self, as_of_date):
        from datetime import date as _date
        rebasing_date = _date(as_of_date.year, self.rebasing_month, self.rebasing_day)
        return as_of_date.year if as_of_date >= rebasing_date else as_of_date.year - 1

    def observe(self, as_of_date, equity):
        """Apply, in order: (1) yearly re-basing, (2) the Drawdown Step
        ladder, (3) recovery to the yearly starting figure."""
        if not (isfinite(equity) and equity > 0):
            raise ValueError("equity must be finite and positive")
        self._maybe_rebase(as_of_date, equity)
        self._apply_drawdown_steps(equity)
        self._maybe_recover(equity)

    def _maybe_rebase(self, as_of_date, equity):
        label = self._rebasing_label(as_of_date)
        if self._rebased_label is None:
            self._rebased_label = label
            return
        if label > self._rebased_label:
            self._rebased_label = label
            self.starting_figure = equity
            self.current = equity
            self.base = equity
            self.steps_applied = 0

    def _apply_drawdown_steps(self, equity):
        for _ in range(1024):  # a generous, belt-and-braces bound
            threshold = self.base - DRAWDOWN_THRESHOLD_FRACTION * self.current
            if equity > threshold:
                return
            self.current *= DRAWDOWN_RETAINED_FRACTION
            self.base = threshold
            self.steps_applied += 1

    def _maybe_recover(self, equity):
        if self.current < self.starting_figure and equity >= self.starting_figure:
            self.current = self.starting_figure
            self.base = self.starting_figure
            self.steps_applied = 0


# ---------------------------------------------------------------------------
# Unit caps (The Turtle Rules p.16): market / closely-correlated group /
# loosely-correlated group / single direction, each cap independent per
# direction (cap 4 is explicitly "single direction: all long or all
# short" -- this module reads that as applying to every level, so a fully
# Loaded long book leaves the short side's headroom untouched).
# ---------------------------------------------------------------------------


class UnitCaps:
    """Faith's four Unit caps (The Turtle Rules p.16), checked in the order
    market, closely-correlated group, loosely-correlated group, single
    direction -- whichever binds first. Every count is kept PER DIRECTION:
    a (symbol, direction) pair for the market cap, a (group, direction)
    pair for the two correlation caps, and a plain per-direction total for
    the direction cap. ``main.py``'s ``CORRELATION_GROUPS`` supplies each
    market's fixed (closely, loosely) labels (README.md documents the
    mapping and its reasoning); every market this script trades has both,
    so unlike ADR 0008's equity Unclassified Group, no fallback group is
    needed here.
    """

    def __init__(self, per_market=MAX_UNITS_PER_MARKET,
                 per_closely=MAX_UNITS_PER_CLOSELY_CORRELATED_GROUP,
                 per_loosely=MAX_UNITS_PER_LOOSELY_CORRELATED_GROUP,
                 per_direction=MAX_UNITS_PER_DIRECTION):
        self.per_market = per_market
        self.per_closely = per_closely
        self.per_loosely = per_loosely
        self.per_direction = per_direction
        self._market_units = {}
        self._closely_units = {}
        self._loosely_units = {}
        self._direction_units = {}

    def would_exceed(self, symbol, closely_group, loosely_group, direction, additional_units=1):
        """(exceeded, cap_name): would ``additional_units`` more, in
        ``direction``, push any applicable cap past its limit? ``cap_name``
        is one of "market", "closely_correlated", "loosely_correlated",
        "direction" -- whichever binds first."""
        checks = (
            ("market", self._market_units.get((symbol, direction), 0), self.per_market),
            ("closely_correlated", self._closely_units.get((closely_group, direction), 0), self.per_closely),
            ("loosely_correlated", self._loosely_units.get((loosely_group, direction), 0), self.per_loosely),
            ("direction", self._direction_units.get(direction, 0), self.per_direction),
        )
        for name, current, limit in checks:
            if current + additional_units > limit:
                return True, name
        return False, None

    def add(self, symbol, closely_group, loosely_group, direction, units=1):
        self._market_units[(symbol, direction)] = self._market_units.get((symbol, direction), 0) + units
        self._closely_units[(closely_group, direction)] = \
            self._closely_units.get((closely_group, direction), 0) + units
        self._loosely_units[(loosely_group, direction)] = \
            self._loosely_units.get((loosely_group, direction), 0) + units
        self._direction_units[direction] = self._direction_units.get(direction, 0) + units

    def remove(self, symbol, closely_group, loosely_group, direction, units=1):
        self._market_units[(symbol, direction)] = self._market_units.get((symbol, direction), 0) - units
        self._closely_units[(closely_group, direction)] = \
            self._closely_units.get((closely_group, direction), 0) - units
        self._loosely_units[(loosely_group, direction)] = \
            self._loosely_units.get((loosely_group, direction), 0) - units
        self._direction_units[direction] = self._direction_units.get(direction, 0) - units


class SessionCapLedger:
    """ADR 0008/0020's "a proposal reserves its cap headroom" rule, for
    Unit caps only. This script trades on a MARGIN account, since futures
    are margined: affordability is left to
    QuantConnect's own margin model, which fails an unaffordable order
    outright at placement (``OrderStatus.Invalid``, handled in main.py
    exactly as research/qc-cloud's own anomaly path releases a reservation
    on that status) rather than modelled here in pure Python -- unlike
    research/qc-cloud's own ``SessionLedger``, which also reserves cash for
    a CASH account. One ``SessionCapLedger`` is built fresh each Session,
    seeded with the caps already committed by open Campaigns, so every Add
    and entry decided within one session-close pass is checked against
    what EARLIER proposals in the SAME pass already claimed, not only
    against already-committed positions.
    """

    def __init__(self, unit_caps):
        self.caps = unit_caps

    def try_reserve(self, symbol, closely_group, loosely_group, direction):
        """Attempt to reserve one proposed Unit's cap headroom. Returns
        ``(accepted, reason)``; ``reason`` is ``None`` on acceptance, else
        ``"unit-cap-exceeded:<name>"``."""
        exceeded, cap_name = self.caps.would_exceed(symbol, closely_group, loosely_group, direction)
        if exceeded:
            return False, "unit-cap-exceeded:" + cap_name
        self.caps.add(symbol, closely_group, loosely_group, direction)
        return True, None


# ---------------------------------------------------------------------------
# Strength and ranking (The Turtle Rules p.27-29).
# ---------------------------------------------------------------------------

Signal = namedtuple("Signal", ["symbol", "strength"])


def strength(closes, n):
    """Faith's Strength measure (The Turtle Rules p.29):

        strength = (close[d] - close[d - 63]) / N[d]

    ``closes`` must be in chronological order, oldest first; only the last
    64 entries matter. Returns ``(value, ready)``; ``ready`` is False with
    fewer than 64 closes or a non-positive N."""
    if len(closes) < STRENGTH_LOOKBACK_BARS + 1 or not (isfinite(n) and n > 0):
        return 0.0, False
    return (closes[-1] - closes[-1 - STRENGTH_LOOKBACK_BARS]) / n, True


def rank_signals(signals, direction):
    """The Turtle Rules p.27-29: "on simultaneous signals, buy the
    strongest / sell the weakest within a correlated group." For a long
    (``direction=1``), the strongest (most positive Strength) is ranked
    first; for a short (``direction=-1``), the weakest (most negative
    Strength) is ranked first. Ties break by symbol, ascending, for a total
    order (symbols are unique)."""
    if direction > 0:
        return sorted(signals, key=lambda s: (-s.strength, s.symbol))
    return sorted(signals, key=lambda s: (s.strength, s.symbol))


# ---------------------------------------------------------------------------
# The price cap and slippage (ADR 0005's amendment, ADR 0013), symmetric.
# ---------------------------------------------------------------------------


def price_cap(level, n, direction, gap_buffer_n=GAP_BUFFER_N):
    """ADR 0005's amendment: an entry or Add rests as a stop-limit order,
    capped at ``level + direction x k x N``. For a long, the cap sits ABOVE
    the level (a buy that gaps up is capped how much it may overpay); for a
    short, this module's mirror places it BELOW (a sell that gaps down is
    capped how little it may underpay). The Baseline declares k=1."""
    if direction not in (1, -1):
        raise ValueError("direction must be 1 (long) or -1 (short)")
    return level + direction * gap_buffer_n * n


def slippage(n, slippage_n=SLIPPAGE_N):
    """ADR 0013: slippage is 0.05N per fill, against the trader, on every
    entry, Add, stop, and exit -- never zero, regardless of direction (its
    sign is applied at the fill, not here: see ``stop_limit_fill_price``)."""
    return slippage_n * n


def exit_order_level(direction, unit_stop, exit_channel_extreme=None):
    """A held Unit's own resting Exit Order (ADR 0005's amendment): the
    MORE PROTECTIVE of the Unit's own Protective Stop and, while an
    Exit-Channel exit is proposed for its Campaign, the Exit Channel level.
    For a long that is the HIGHER of the two (a tighter stop sits closer to
    price from below); for a short, this module's mirror takes the LOWER (a
    tighter stop sits closer to price from above). A tie names the stop.
    Pass ``exit_channel_extreme=None`` when no exit is proposed."""
    if exit_channel_extreme is None:
        return unit_stop
    return max(unit_stop, exit_channel_extreme) if direction > 0 else min(unit_stop, exit_channel_extreme)


def stop_limit_fill_price(direction, level, price_cap_value, open_, high, low, slippage_amount):
    """ADR 0005's stop-limit fill rule, symmetric for long and short. LEAN's
    own native stop-limit fill (a) triggers only on a strict cross, not an
    exact touch; (b) applies no slippage. This mirrors
    research/qc-cloud/main.py's own ``_stop_limit_buy_fill_price`` (ADR
    0005 as amended 2026-09-24), generalised to a direction argument so one
    function serves the buy-side entry/Add (``direction=1``) and this
    module's symmetric sell-side short entry/Add (``direction=-1``).

    Long: triggers on ``high >= level``; fills at ``max(level, open)`` when
    that is within the cap (``price_cap_value``, ABOVE level); otherwise
    works as a limit buy at the cap, filling only if the bar trades back
    down to it. Short: triggers on ``low <= level``; fills at
    ``min(level, open)`` when that is within the cap (BELOW level);
    otherwise works as a limit sell at the cap, filling only if the bar
    trades back up to it. Slippage is added after the cap bounds the price,
    against the trader in both cases (a higher buy price, a lower sell
    price). Returns the fill price, or ``None`` for no fill this bar.
    """
    if direction > 0:
        if high < level:
            return None
        if open_ <= price_cap_value:
            return max(level, open_) + slippage_amount
        if low > price_cap_value:
            return None
        return price_cap_value + slippage_amount
    if low > level:
        return None
    if open_ >= price_cap_value:
        return min(level, open_) - slippage_amount
    if high < price_cap_value:
        return None
    return price_cap_value - slippage_amount


# ---------------------------------------------------------------------------
# CAGR, max drawdown, and the primary metric (ADR 0012), instrument- and
# direction-agnostic -- unchanged from research/qc-cloud.
# ---------------------------------------------------------------------------


def annualised_return(start_equity, end_equity, elapsed_days):
    """(E1 / E0) ^ (1 / Y) - 1, Y = elapsed days / 365.25. Returns ``None``
    for a non-positive starting equity or elapsed span, or a non-finite
    result."""
    if not (isfinite(start_equity) and start_equity > 0) or not (isfinite(elapsed_days) and elapsed_days > 0):
        return None
    years = elapsed_days / 365.25
    try:
        result = (end_equity / start_equity) ** (1.0 / years) - 1.0
    except (ZeroDivisionError, OverflowError, ValueError):
        return None
    return result if isfinite(result) else None


def max_drawdown(equity_curve):
    """max((running_peak - equity) / running_peak) over the equity marks
    given, in order. Returns ``None`` for an empty curve."""
    if not equity_curve:
        return None
    peak = equity_curve[0]
    worst = 0.0
    for equity in equity_curve:
        peak = max(peak, equity)
        if peak > 0:
            worst = max(worst, (peak - equity) / peak)
    return worst


def cagr_over_max_drawdown(cagr, mdd):
    """Annualised return / max drawdown. ``None`` for a null CAGR, a null
    drawdown, or a zero drawdown -- never an infinite winning score."""
    if cagr is None or mdd is None or mdd == 0:
        return None
    return cagr / mdd
