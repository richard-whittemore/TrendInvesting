"""Baseline Turtle rule core, for the free QuantConnect Cloud research check
(docs/methodology/Methodology_Analysis.md; the ADRs in docs/adr/).

This module has NO QuantConnect imports and no dependency on this repo's Go
packages. It is pure, standard-library Python so it can be unit-tested with
`python3 -m unittest` on a laptop, and so `main.py` (the thin QCAlgorithm
that runs on QuantConnect's Free plan) can import it unchanged after both
files are pasted into a QuantConnect cloud project.

Every rule below cites the ADR (or Methodology_Analysis.md section) it
transcribes, and every parameter value is taken from that source, never from
memory or from the Go engine's test fixtures. Where a fixture in
cmd/backtest/testdata/configuration.json uses a different number (its
entry/exit channel lengths are shortened, and its per-industry/per-sector/
total-long caps are set to an effectively unlimited 1,000,000, so the fixture
scenarios stay small and cap-free), this module uses the actual Baseline
values ADR 0002, ADR 0003, ADR 0008, ADR 0009, ADR 0013 and ADR 0012's
"parameter provenance" section declare, never the fixture's own numbers,
which exist to keep unrelated Go test fixtures small and fast, not to state
the Baseline.

This is research-only. It is not the production Go engine
(internal/strategy, internal/sizing, internal/indicator) and makes no claim
of bit-for-bit parity with it. research/qc-cloud/README.md lists every known
and deliberate deviation the cloud environment forces.
"""

from collections import deque, namedtuple
from datetime import date
from math import floor, isfinite

# ---------------------------------------------------------------------------
# Source-fixed parameters (ADR 0012 "parameter provenance": transcribed from
# The Turtle Rules and not changed in the Baseline).
# ---------------------------------------------------------------------------

#: N's lookback, in completed bars (The Turtle Rules p.13; ADR 0003).
N_PERIOD = 20

#: System 2's Entry Channel length, in completed bars (The Turtle Rules
#: p.18-19; ADR 0002: "The Baseline is System 2").
ENTRY_CHANNEL_LENGTH = 55

#: System 2's Exit Channel length, in completed bars (The Turtle Rules p.26;
#: ADR 0002).
EXIT_CHANNEL_LENGTH = 20

#: The Stop Multiple: N between entry and the Protective Stop (The Turtle
#: Rules p.22; ADR 0003, ADR 0012).
STOP_MULTIPLE = 2.0

#: Add Ladder spacing, in N, from the actual fill of the previous Unit (The
#: Turtle Rules p.19; ADR 0006, ADR 0012).
ADD_SPACING_N = 0.5

#: Faith's four-Unit maximum per Campaign (The Turtle Rules p.16; ADR 0008,
#: ADR 0012).
MAX_UNITS_PER_INSTRUMENT = 4

#: ADR 0008's remaining three Unit caps (The Turtle Rules p.16: 6 per
#: closely-correlated group, 10 per loosely-correlated group, 12 total
#: long).
MAX_UNITS_PER_INDUSTRY = 6
MAX_UNITS_PER_SECTOR = 10
MAX_UNITS_TOTAL_LONG = 12

#: ADR 0007's Drawdown Step: a 10% fall of the current Notional Account
#: (measured against the measurement base) triggers a 20% reduction (The
#: Turtle Rules p.17).
DRAWDOWN_THRESHOLD_FRACTION = 0.10
DRAWDOWN_RETAINED_FRACTION = 0.8

# ---------------------------------------------------------------------------
# Baseline-declared adaptations (ADR 0012: chosen by this project, each
# justified by a recorded argument, never by a result).
# ---------------------------------------------------------------------------

#: Half Faith's 1% (ADR 0003's concentration argument for a long-only,
#: single-regime equity book).
UNIT_VOLATILITY_FRACTION = 0.005

#: ADR 0013: slippage is 0.05N per fill, against the trader, never zero.
SLIPPAGE_N = 0.05

#: ADR 0005 (as amended 2026-09-24): entries and Adds rest as stop-limit
#: orders capped at level + k*N. The Baseline declares k=1.
GAP_BUFFER_N = 1.0

#: ADR 0009: the Baseline universe eligibility thresholds (deliberately
#: permissive so stricter, Sublime-style filters stay measurable ablations).
UNIVERSE_MIN_PRICE = 5.0
UNIVERSE_MIN_DOLLAR_VOLUME = 5_000_000.0
UNIVERSE_MIN_HISTORY_BARS = 250

#: ADR 0013: LEAN's Interactive Brokers fee schedule, IBKR Pro Fixed, as
#: cmd/backtest/testdata/configuration.json's own "commission" block states
#: it (per_share, minimum_per_order, maximum_fraction_of_trade_value). Used
#: only by this module's own pre-trade affordability ESTIMATE
#: (affordable_quantity, below); the commission actually charged on
#: QuantConnect Cloud is LEAN's InteractiveBrokersFeeModel, a materially
#: different schedule (README.md, "Deviations").
IB_COMMISSION_PER_SHARE = 0.005
IB_COMMISSION_MINIMUM = 1.0
IB_COMMISSION_MAX_FRACTION_OF_TRADE_VALUE = 0.01

# ---------------------------------------------------------------------------
# ADR 0003 / Methodology_Analysis.md Section 2.2: True Range and N.
# ---------------------------------------------------------------------------


def true_range(high, low, previous_close):
    """True Range (The Turtle Rules p.13, transcribed via ADR 0003):

        True Range = max(high - low, high - previous_close, previous_close - low)

    ``previous_close`` is ``None`` for the very first bar in a series, where
    no prior close exists to measure a gap against: True Range there is the
    bar's own range, high - low. This mirrors
    internal/indicator.TrueRange's identical choice for the identical
    reason (no source settles this case either way; Faith's own worked
    table starts mid-series, with a previous close already available for
    every printed row).
    """
    if previous_close is None:
        return high - low
    return max(high - low, high - previous_close, previous_close - low)


class WilderN:
    """N: Wilder's 20-day average of True Range (The Turtle Rules p.13; ADR
    0003; CONTEXT.md "N" -- "never an average of price").

    Seeded with a simple average of the first N_PERIOD True Range values,
    then updated by Wilder's recursion:

        N = ((period - 1) x previous_N + TR) / period

    Warm-up is counted in completed bars, never calendar days (CONTEXT.md:
    "Completed bar"). Mirrors internal/indicator.WilderAverage.

    Callers MUST read ``value``/``ready`` for the bar under decision BEFORE
    calling ``add`` with that same bar's True Range -- the evaluate-then-add
    ordering CONTEXT.md's "Completed bar" entry requires for every input to
    a decision, not only the Entry/Exit Channels.
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
        """Whether at least ``period`` True Range values have been added --
        NOT whether ``value`` is a usable (nonzero) volatility reading; a
        run of flat bars can legitimately leave ``value`` at exactly zero
        once ready (mirrors internal/indicator.WilderAverage.Ready's own
        documented distinction)."""
        return self._count >= self.period

    @property
    def value(self):
        return self._value


# ---------------------------------------------------------------------------
# ADR 0002 / CONTEXT.md: the 55-day Entry Channel and 20-day Exit Channel,
# evaluated on completed bars only.
# ---------------------------------------------------------------------------


class EntryChannel:
    """The 55-day Entry Channel (The Turtle Rules p.18-19; ADR 0002;
    CONTEXT.md "Entry Channel"): the highest high of the preceding
    ``length`` completed bars. A Breakout is price STRICTLY exceeding this
    value (the caller's own comparison; a tie is not a Breakout).

    Callers MUST call ``extreme()`` to evaluate the bar under decision
    BEFORE calling ``add()`` with that same bar's high -- mirrors
    internal/indicator.EntryChannel's own "evaluate-then-add" discipline,
    which exists specifically to prevent a channel from including the very
    bar it is being asked to decide (that defect silently suppresses every
    Signal, since a channel already containing today's own high can never
    be exceeded by it).
    """

    def __init__(self, length=ENTRY_CHANNEL_LENGTH):
        if length <= 0:
            raise ValueError("length must be positive")
        self.length = length
        self._values = deque(maxlen=length)

    def extreme(self):
        """(value, ready): the channel high, and whether ``length`` bars
        have been added. (0, False) before any bar has been added."""
        if not self._values:
            return 0.0, False
        return max(self._values), len(self._values) >= self.length

    def add(self, high):
        self._values.append(high)


class ExitChannel:
    """The 20-day Exit Channel (The Turtle Rules p.26; ADR 0002; CONTEXT.md
    "Exit Channel"): the lowest low of the preceding ``length`` completed
    bars, at which an open Campaign is closed in full. The mirror image of
    EntryChannel; see its doc comment for the evaluate-then-add discipline,
    identically required here.
    """

    def __init__(self, length=EXIT_CHANNEL_LENGTH):
        if length <= 0:
            raise ValueError("length must be positive")
        self.length = length
        self._values = deque(maxlen=length)

    def extreme(self):
        """(value, ready): the channel low, and whether ``length`` bars
        have been added. (0, False) before any bar has been added."""
        if not self._values:
            return 0.0, False
        return min(self._values), len(self._values) >= self.length

    def add(self, low):
        self._values.append(low)


def exit_order_level(unit_stop, exit_channel_extreme=None):
    """A held Unit's own resting sell order (ADR 0005's amendment, "a
    Protective Stop and an Exit-Channel exit rest as one order per Unit"):
    the HIGHER of the Unit's own Protective Stop and, while an Exit-Channel
    exit is proposed for its Campaign, the Exit Channel level -- a tie names
    the stop. Pass ``exit_channel_extreme=None`` when no exit is proposed
    (equivalent to the Unit's own stop alone).
    """
    if exit_channel_extreme is None:
        return unit_stop
    return max(unit_stop, exit_channel_extreme)


# ---------------------------------------------------------------------------
# ADR 0003 / Methodology_Analysis.md Section 2.3: Unit sizing.
# ---------------------------------------------------------------------------


def unit_quantity(notional_account, unit_volatility_fraction, n, dollars_per_point=1.0):
    """Faith's Unit-sizing formula (The Turtle Rules p.14; ADR 0003):

        quantity = floor( notional_account x unit_volatility_fraction
                           ------------------------------------------ )
                                   n x dollars_per_point

    Truncated toward zero, never rounded: the Heating Oil worked example
    (The Turtle Rules p.14-15) is N=0.0141, a $1,000,000 account, a
    42,000-gallon contract, Faith's own 1% Unit Volatility Fraction ->
    16.88, truncated to 16 (see test_rules.py's golden test, which
    reproduces this exactly). ``dollars_per_point`` is 1 for shares.

    Returns 0, with no error, when the account cannot fund a single share
    (The Turtle Rules p.15: "small accounts lose diversification" -- exactly
    because truncation is this coarse); the caller declines the whole Unit,
    never a fraction of one (ADR 0010: "no partial Units").
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
    # conservative correction (mirrors internal/sizing.truncate's own
    # comment).
    if quotient > 0 and quotient * cost_per_unit_per_n > budget:
        quotient -= 1
    return int(quotient)


# ---------------------------------------------------------------------------
# ADR 0006 / Methodology_Analysis.md Section 2.6: the ½N Add Ladder with a
# frozen Campaign N, and ADR 0003 / p.22-23: the 2N stop and Stop Ladder.
# ---------------------------------------------------------------------------


def next_add_level(previous_fill, campaign_n, spacing_n=ADD_SPACING_N):
    """The Add Ladder's next rung (The Turtle Rules p.19; ADR 0006):

        level = previous_fill + 0.5 x campaign_n

    ``previous_fill`` is the ACTUAL fill price of the Unit immediately
    before this one -- never its proposed or intended level -- so that
    slippage on one fill shifts every later rung (The Turtle Rules p.19:
    "slippage on the first fill pushes later adds out accordingly").
    ``campaign_n`` is the Campaign's FROZEN N (ADR 0006), never one
    recomputed after entry.
    """
    if not (isfinite(previous_fill) and previous_fill > 0):
        raise ValueError("previous_fill must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    return previous_fill + spacing_n * campaign_n


def protective_stop_level(entry_price, campaign_n, stop_multiple=STOP_MULTIPLE):
    """A Unit's own initial Protective Stop (The Turtle Rules p.22; ADR
    0003):

        level = entry_price - stop_multiple x campaign_n

    ``campaign_n`` is the Campaign's FROZEN N (ADR 0006). Faith's own
    single-Unit Crude example: entry 28.30, N 1.20, Stop Multiple 2 -> stop
    25.90 (see test_rules.py's golden test).
    """
    if not (isfinite(entry_price) and entry_price > 0):
        raise ValueError("entry_price must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    if not (isfinite(stop_multiple) and stop_multiple > 0):
        raise ValueError("stop_multiple must be finite and positive")
    level = entry_price - stop_multiple * campaign_n
    if level <= 0:
        raise ValueError("derived protective stop level is not positive: a long position cannot be "
                          "stopped out at or below zero")
    return level


def raised_stop(previous_stop, campaign_n, spacing_n=ADD_SPACING_N):
    """The Stop Ladder's raise (The Turtle Rules p.22-23; ADR 0006):

        level = previous_stop + 0.5 x campaign_n

    Applied to EVERY earlier Unit's own current stop each time a further
    Unit is added -- literally "raise the earlier Unit's OWN stop by half
    N", never "reset every stop to 2N below the newest fill". The two
    readings coincide only when every Unit lands exactly on its own rung;
    they diverge the moment a later Unit fills away from its rung (a gap or
    fast-market skid -- The Turtle Rules p.22-23's own Crude gap example),
    which is exactly why this function takes the Unit's own current stop,
    never its original fill price and a recomputed count of Adds since.
    """
    if not (isfinite(previous_stop) and previous_stop > 0):
        raise ValueError("previous_stop must be finite and positive")
    if not (isfinite(campaign_n) and campaign_n > 0):
        raise ValueError("campaign_n must be finite and positive")
    return previous_stop + spacing_n * campaign_n


class Campaign:
    """One Campaign's life (CONTEXT.md "Campaign"): opened by a first fill,
    Units added along the Add Ladder up to ``max_units``, each Unit's own
    Protective Stop raised by the Stop Ladder as later Units are added, N
    and the opening Unit quantity frozen at first entry (ADR 0006).

    ``units`` is a list of ``{"fill_price": float, "stop": float}`` dicts,
    oldest first. The Campaign's own Protective Stop is the LOWEST of its
    Units' own stops (CONTEXT.md "Protective Stop": "the level at which its
    protection is FIRST breached").
    """

    def __init__(self, symbol, entry_fill_price, campaign_n, unit_quantity_value,
                 industry=None, sector=None, stop_multiple=STOP_MULTIPLE,
                 max_units=MAX_UNITS_PER_INSTRUMENT, spacing_n=ADD_SPACING_N):
        self.symbol = symbol
        self.campaign_n = campaign_n            # frozen (ADR 0006)
        self.unit_quantity = unit_quantity_value  # frozen (ADR 0006)
        self.industry = industry
        self.sector = sector
        self.stop_multiple = stop_multiple
        self.max_units = max_units
        self.spacing_n = spacing_n
        initial_stop = protective_stop_level(entry_fill_price, campaign_n, stop_multiple)
        self.units = [{"fill_price": entry_fill_price, "stop": initial_stop}]
        # Frozen at construction (ADR 0006), for entry_price()'s own use --
        # never re-derived from self.units[0], which after a partial
        # stop-out is whichever Unit SURVIVED, not necessarily the
        # Campaign's own first one (PR #253 review, Greptile: main.py:649).
        self._original_entry_price = entry_fill_price
        # The sum of every CLOSED Unit's own (exit - fill) price distance
        # (close_units), so the Campaign's own r_multiple() reflects EVERY
        # Unit it ever held, not only the last one to close (PR #253
        # review, Greptile: main.py:649 -- "Campaign results omit earlier
        # Units").
        self.realized_price_pnl = 0.0
        # The Baseline never re-adds once any Unit has been stopped out
        # (The Turtle Rules p.23-24's Whipsaw re-entry alternative is a
        # declared Variant, not the Baseline -- ADR 0012; mirrors
        # internal/strategy.evaluateAdd's own "campaign.partiallyStopped"
        # gate).
        self.partially_stopped = False

    @property
    def unit_count(self):
        return len(self.units)

    @property
    def loaded(self):
        """Holding the maximum permitted Units for this Campaign
        (CONTEXT.md "Loaded")."""
        return self.unit_count >= self.max_units

    def next_add_rung(self):
        """The next Add Ladder rung, or ``None`` when no further Add is
        possible (Loaded, or partially stopped -- see the class doc
        comment)."""
        if self.loaded or self.partially_stopped:
            return None
        last_fill = self.units[-1]["fill_price"]
        return next_add_level(last_fill, self.campaign_n, self.spacing_n)

    def add_unit(self, fill_price):
        """Record a new Unit's actual fill: raise every EARLIER Unit's own
        stop by ½N (the Stop Ladder), then give the new Unit its own
        initial stop from ITS OWN fill (The Turtle Rules p.22-23)."""
        if self.loaded:
            raise ValueError("campaign already Loaded: no further Add")
        if self.partially_stopped:
            raise ValueError("campaign already partially stopped: no further Add (ADR 0012)")
        for unit in self.units:
            unit["stop"] = raised_stop(unit["stop"], self.campaign_n, self.spacing_n)
        self.units.append({
            "fill_price": fill_price,
            "stop": protective_stop_level(fill_price, self.campaign_n, self.stop_multiple),
        })

    def protective_stop(self):
        """The Campaign's own Protective Stop: the lowest of its Units' own
        stops (CONTEXT.md "Protective Stop")."""
        return min(unit["stop"] for unit in self.units)

    def remove_units(self, indices):
        """Remove the Units at ``indices`` (a stop-out or an Exit-Channel
        exit closed them), and mark the Campaign partially stopped if any
        Unit survives -- a full exit (no survivors) is not a "partial"
        stop, it is the Campaign's own end. Does not itself realise any
        P&L; see close_units for the priced version callers with an actual
        exit price should use."""
        surviving_indices = set(range(len(self.units))) - set(indices)
        self.units = [self.units[i] for i in sorted(surviving_indices)]
        if self.units:
            self.partially_stopped = True

    def close_units(self, indices, exit_price):
        """Realise each of the given Units' own (exit_price - fill_price)
        into the Campaign's running realized_price_pnl, THEN remove them
        (remove_units) -- so a Campaign's eventual r_multiple() reflects
        every Unit it ever held, not only whichever one happens to close
        last (PR #253 review, Greptile: main.py:649). ``indices`` are
        positions in the CURRENT self.units list, exactly as remove_units
        already requires; each Unit may exit at its own price (its own
        stop, or its own Exit Order's fill), so this takes one exit_price
        per call rather than per index -- callers with several Units
        closing at DIFFERENT prices in the same Session call this once per
        price, exactly as they already call remove_units once per fill."""
        for index in indices:
            self.realized_price_pnl += exit_price - self.units[index]["fill_price"]
        self.remove_units(indices)

    def r_multiple(self):
        """The Campaign's own R multiple, once it has fully closed: the sum
        of every closed Unit's own (exit - fill) price distance
        (realized_price_pnl), divided by the Campaign's 1-Unit initial risk
        (Stop Multiple x campaign N) -- CONTEXT.md "Campaign": a Campaign's
        result reflects every Unit it held, not only the last to close.
        Every Unit shares the same frozen unit_quantity (ADR 0006), so that
        common factor cancels out of both the price-distance numerator and
        the risk denominator, and this is exactly the Campaign's total
        dollar P&L over its total initial 1-Unit dollar risk. Meaningful
        once no Units remain; this class does not itself track that --
        the caller already knows the moment its last Unit closes."""
        return self.realized_price_pnl / (self.stop_multiple * self.campaign_n)

    def entry_price(self):
        """The Campaign's own ORIGINAL entry: Unit 1's own fill price,
        frozen at construction (ADR 0006), for reporting. Never
        self.units[0], which after a partial stop-out is whichever Unit
        SURVIVED, not necessarily the Campaign's own first one (PR #253
        review, Greptile: main.py:649)."""
        return self._original_entry_price


# ---------------------------------------------------------------------------
# ADR 0007 / Methodology_Analysis.md Section 2.5: the drawdown rule.
# ---------------------------------------------------------------------------


class NotionalAccount:
    """ADR 0007: the Notional Account -- the equity figure position sizing
    is measured against, reduced during a drawdown, and NOT the same as
    actual account equity (CONTEXT.md "Notional Account").

    Tracks two figures, both initialised to ``starting_equity``:

    - ``current``: the Notional Account itself (what sizing reads).
    - ``base``: the figure a further 10% fall is measured against. It only
      moves when a Drawdown Step fires (to the threshold just crossed) or
      when the account re-bases or recovers.

    ``rebasing_month``/``rebasing_day`` name the yearly re-basing date (ADR
    0007: "re-based to actual equity every 1 January" by default -- a
    parameter here exactly as it is in
    cmd/backtest/testdata/configuration.json's own
    ``notional_account.rebasing_month``/``rebasing_day`` fields, so the
    sensitivity ADR 0007 calls for can be measured).

    Call ``observe(as_of_date, equity)`` once per Session, in date order,
    with the account's actual equity as of that Session's close.
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
        """The re-basing "year label" ``as_of_date`` belongs to: the
        calendar year of the most recent re-basing date at or before it
        (mirrors internal/strategy.NotionalAccount.rebasingLabelFor)."""
        rebasing_date = date(as_of_date.year, self.rebasing_month, self.rebasing_day)
        return as_of_date.year if as_of_date >= rebasing_date else as_of_date.year - 1

    def observe(self, as_of_date, equity):
        """Apply, in order (ADR 0007's ObserveSnapshot, as
        internal/strategy/notional.go implements it): (1) yearly
        re-basing, (2) the Drawdown Step ladder, (3) recovery to the
        yearly starting figure. Mutates ``current``/``base``; returns
        nothing."""
        if not (isfinite(equity) and equity > 0):
            raise ValueError("equity must be finite and positive")
        self._maybe_rebase(as_of_date, equity)
        self._apply_drawdown_steps(equity)
        self._maybe_recover(equity)

    def _maybe_rebase(self, as_of_date, equity):
        label = self._rebasing_label(as_of_date)
        if self._rebased_label is None:
            # The very first snapshot establishes the label without
            # re-basing, however far past the re-basing date it falls
            # (ADR 0007's implementation note: a run starting mid-year has
            # no record of actual equity AT that year's re-basing date).
            self._rebased_label = label
            return
        if label > self._rebased_label:
            self._rebased_label = label
            self.starting_figure = equity
            self.current = equity
            self.base = equity
            self.steps_applied = 0

    def _apply_drawdown_steps(self, equity):
        # A generous bound, not a claim about how deep a real drawdown
        # ladder should go (mirrors internal/strategy's own
        # belt-and-braces maxDrawdownStepsPerObservation).
        for _ in range(1024):
            threshold = self.base - DRAWDOWN_THRESHOLD_FRACTION * self.current
            if equity > threshold:
                return
            self.current *= DRAWDOWN_RETAINED_FRACTION
            self.base = threshold
            self.steps_applied += 1

    def _maybe_recover(self, equity):
        # Never a high-water mark, and never a partial recovery (The
        # Turtle Rules p.17; ADR 0007): the full Notional Account returns
        # ONLY when actual equity regains the yearly starting figure.
        if self.current < self.starting_figure and equity >= self.starting_figure:
            self.current = self.starting_figure
            self.base = self.starting_figure
            self.steps_applied = 0


# ---------------------------------------------------------------------------
# ADR 0008: Unit caps for equities, with an Unclassified Group.
# ---------------------------------------------------------------------------

#: CONTEXT.md "Unclassified Group": the single correlation group that holds
#: every instrument whose industry and sector are unknown. QuantConnect's
#: Free plan supplies no reliable point-in-time industry/sector classifier
#: this script can afford to depend on (README.md, "Deviations"), so every
#: instrument this script trades is Unclassified throughout, exactly the
#: fail-safe case ADR 0008 describes.
UNCLASSIFIED_GROUP = "unclassified"


class UnitCaps:
    """ADR 0008: Faith's Unit caps (The Turtle Rules p.16) applied to
    equities -- 4 per instrument, 6 per industry, 10 per sector, 12 total
    long -- checked against POST-TRADE exposure (ADR 0008's 2026-09-24
    implementation note): would adding the proposed Unit(s) push any cap
    past its limit?

    An instrument with no industry/sector label belongs to the single,
    shared Unclassified Group, capped ONCE at the loosely-correlated
    (sector) level -- ADR 0008's own words: "unclassified names are treated
    as correlated", "every instrument without a label belongs to a single
    shared 'unclassified' group capped at the loosely-correlated level (10
    Units)", and its 2026-09-24 implementation note: "The Unclassified
    Group is capped at MaxUnitsPerSector's own value ... rather than by a
    fifth, separately configurable number." An Unclassified instrument is
    therefore checked against the per-instrument, sector and total-long
    caps only, never against a separate, tighter per-industry cap of its
    own -- it has no industry group to be capped by.
    """

    def __init__(self, per_instrument=MAX_UNITS_PER_INSTRUMENT, per_industry=MAX_UNITS_PER_INDUSTRY,
                 per_sector=MAX_UNITS_PER_SECTOR, total_long=MAX_UNITS_TOTAL_LONG):
        self.per_instrument = per_instrument
        self.per_industry = per_industry
        self.per_sector = per_sector
        self.total_long = total_long
        self._instrument_units = {}
        self._industry_units = {}
        self._sector_units = {}
        self._total = 0

    @staticmethod
    def _groups(industry, sector):
        """(industry_group, sector_group): industry_group is ``None`` when
        both labels are unknown -- an Unclassified instrument has no
        industry group of its own to be capped by (see the class doc
        comment) -- otherwise each defaults to the shared Unclassified
        Group individually, exactly as ADR 0008 states for a label that is
        missing on its own."""
        if not industry and not sector:
            return None, UNCLASSIFIED_GROUP
        return industry or UNCLASSIFIED_GROUP, sector or UNCLASSIFIED_GROUP

    def would_exceed(self, symbol, industry, sector, additional_units=1):
        """(exceeded, cap_name): would ``additional_units`` more, added to
        ``symbol`` (industry/sector, or the Unclassified Group when
        unknown), push any of the applicable caps past its limit?
        ``cap_name`` is one of "instrument", "industry", "sector",
        "total_long" -- whichever binds first, checked in that order
        (mirrors internal/strategy.evaluateAdd's own
        per-instrument-before-group-before-total ordering)."""
        industry_group, sector_group = self._groups(industry, sector)
        checks = [("instrument", self._instrument_units.get(symbol, 0), self.per_instrument)]
        if industry_group is not None:
            checks.append(("industry", self._industry_units.get(industry_group, 0), self.per_industry))
        checks.append(("sector", self._sector_units.get(sector_group, 0), self.per_sector))
        checks.append(("total_long", self._total, self.total_long))
        for name, current, limit in checks:
            if current + additional_units > limit:
                return True, name
        return False, None

    def add(self, symbol, industry, sector, units=1):
        industry_group, sector_group = self._groups(industry, sector)
        self._instrument_units[symbol] = self._instrument_units.get(symbol, 0) + units
        if industry_group is not None:
            self._industry_units[industry_group] = self._industry_units.get(industry_group, 0) + units
        self._sector_units[sector_group] = self._sector_units.get(sector_group, 0) + units
        self._total += units

    def remove(self, symbol, industry, sector, units=1):
        industry_group, sector_group = self._groups(industry, sector)
        self._instrument_units[symbol] = self._instrument_units.get(symbol, 0) - units
        if industry_group is not None:
            self._industry_units[industry_group] = self._industry_units.get(industry_group, 0) - units
        self._sector_units[sector_group] = self._sector_units.get(sector_group, 0) - units
        self._total -= units


# ---------------------------------------------------------------------------
# ADR 0010 (as amended 2026-09-25): Strength and the ranking tie-breaks.
# ---------------------------------------------------------------------------

#: The Turtle Rules p.29 / ADR 0010: Strength's lookback, in completed
#: Sessions.
STRENGTH_LOOKBACK_BARS = 63

#: ADR 0009 / ADR 0010: the trailing window, in completed Sessions
#: inclusive of the decision Session, that both the $5M eligibility test
#: and the ranking tie-break read.
DOLLAR_VOLUME_WINDOW = 20

Signal = namedtuple("Signal", ["symbol", "strength", "median_dollar_volume"])


def strength(closes, n):
    """Faith's Strength measure (The Turtle Rules p.29; ADR 0010 as amended
    2026-09-25):

        strength = (close[d] - close[d - 63]) / N[d]

    ``closes`` must be in chronological order, oldest first; only the last
    64 entries matter. Returns ``(value, ready)``; ``ready`` is False with
    fewer than 64 closes or a non-positive N -- an instrument that cannot
    be compared has no place in the ranking's total order (ADR 0010: "never
    ranked last, since an incomparable instrument has no place in a total
    order") and its Signal is declined instead.
    """
    if len(closes) < STRENGTH_LOOKBACK_BARS + 1 or not (isfinite(n) and n > 0):
        return 0.0, False
    return (closes[-1] - closes[-1 - STRENGTH_LOOKBACK_BARS]) / n, True


def median_dollar_volume(closes, volumes):
    """ADR 0010's ranking tie-break, shared with ADR 0009's $5M eligibility
    test: the median of RAW close x RAW volume (ADR 0004: raw, not
    split-adjusted -- dollar volume is real money traded) over the trailing
    20 completed Sessions, inclusive of the decision Session.

    ``closes``/``volumes`` must be the same length, in chronological order
    (oldest first); only the last 20 entries matter. The median of this
    always-even 20-wide window is the mean of the two middle values (ADR
    0010, 2026-09-25's own decision). Returns ``(value, ready)``; ``ready``
    is False with fewer than 20 Sessions of history.
    """
    if len(closes) != len(volumes):
        raise ValueError("closes and volumes must be the same length: one raw close and one raw "
                          "volume per completed Session, fed in lockstep")
    if len(closes) < DOLLAR_VOLUME_WINDOW:
        return 0.0, False
    window = sorted(c * v for c, v in zip(closes[-DOLLAR_VOLUME_WINDOW:], volumes[-DOLLAR_VOLUME_WINDOW:]))
    mid = len(window) // 2
    return (window[mid - 1] + window[mid]) / 2.0, True


def rank_signals(signals):
    """ADR 0010 (as amended 2026-09-25) / ADR 0021 section 3: rank a
    Session's simultaneous Signals by descending Strength, ties by
    descending 20-day median dollar volume, then ascending symbol -- a
    total order, since symbols are unique. ``signals`` is an iterable of
    ``Signal`` (or any object with ``.strength``, ``.median_dollar_volume``
    and ``.symbol`` attributes)."""
    return sorted(signals, key=lambda s: (-s.strength, -s.median_dollar_volume, s.symbol))


# ---------------------------------------------------------------------------
# ADR 0009: monthly universe eligibility.
# ---------------------------------------------------------------------------


def is_eligible(price, dollar_volume, history_bars, is_common_stock=True):
    """ADR 0009: an instrument is Baseline-eligible when it is common stock
    on a US primary exchange (no ETFs, ADRs, or SPACs), its price is at
    least $5, its 20-day median dollar volume is at least $5M, and it has
    at least 250 completed bars of history. Re-evaluated point-in-time on
    the first trading day of each month (``is_new_eligibility_month``,
    below, drives that cadence from ``main.py``).
    """
    return bool(is_common_stock
                and price >= UNIVERSE_MIN_PRICE
                and dollar_volume >= UNIVERSE_MIN_DOLLAR_VOLUME
                and history_bars >= UNIVERSE_MIN_HISTORY_BARS)


def is_new_eligibility_month(previous_trading_date, current_trading_date):
    """ADR 0009: eligibility is re-evaluated on the first trading day of
    each month. Given the previous and current trading day (QuantConnect's
    own calendar already excludes weekends and holidays), reports whether
    ``current_trading_date`` is the first trading day this script has seen
    in a new calendar month -- True unconditionally for the very first
    Session it is ever called with.
    """
    if previous_trading_date is None:
        return True
    return (current_trading_date.year, current_trading_date.month) != \
        (previous_trading_date.year, previous_trading_date.month)


# ---------------------------------------------------------------------------
# ADR 0005 (as amended) / ADR 0013: the price cap and slippage.
# ---------------------------------------------------------------------------


def price_cap(level, n, gap_buffer_n=GAP_BUFFER_N):
    """ADR 0005 (as amended 2026-09-24): an entry or Add rests as a
    stop-limit buy, capped at ``level + k x N``. The Baseline declares
    k=1 (``GAP_BUFFER_N``). ``n`` is the decision N for an entry, or the
    Campaign's frozen N for an Add."""
    return level + gap_buffer_n * n


def slippage(n, slippage_n=SLIPPAGE_N):
    """ADR 0013: slippage is 0.05N per fill, applied against the trader on
    every entry, Add, stop, and exit -- never zero."""
    return slippage_n * n


# ---------------------------------------------------------------------------
# ADR 0010 / ADR 0020: previous-close cash and Unit-cap headroom.
# ---------------------------------------------------------------------------


def commission_estimate(quantity, price, per_share=IB_COMMISSION_PER_SHARE,
                         minimum=IB_COMMISSION_MINIMUM,
                         max_fraction_of_trade_value=IB_COMMISSION_MAX_FRACTION_OF_TRADE_VALUE):
    """ADR 0013's commission schedule (IBKR Pro Fixed), for this script's
    OWN pre-trade affordability estimate only: a per-share rate, floored at
    a per-order minimum, then capped at a fraction of trade value (the cap
    wins over the floor on a very small, very cheap order -- mirrors
    internal/fills' own "rate, then floor, then ceiling" arithmetic, cited
    in ADR 0013's amendment). The commission LEAN actually charges on
    QuantConnect Cloud is InteractiveBrokersFeeModel, a materially
    different schedule (README.md, "Deviations")."""
    if quantity <= 0 or price <= 0:
        return 0.0
    trade_value = quantity * price
    floored = max(quantity * per_share, minimum)
    return min(floored, max_fraction_of_trade_value * trade_value)


def affordable_quantity(quantity, price_cap_value, slippage_value, dollars_per_point, commission,
                         available_cash):
    """ADR 0010 (the previous close is the sizing/affordability basis) and
    ADR 0020 (affordability binds order PLACEMENT, at the order's
    WORST-CASE cost, never a veto on an already-recorded fill): can this
    whole Unit be funded from ``available_cash`` -- the cash known at the
    previous close, less this Session's earlier fills and standing holds?

        worst_case = quantity x (price_cap + slippage) x dollars_per_point + commission

    Returns ``(affordable, worst_case_cost)``. No partial Units, no
    borrowing (ADR 0010): the WHOLE Unit is skipped if its worst-case cost
    exceeds what is available.
    """
    worst_case = quantity * (price_cap_value + slippage_value) * dollars_per_point + commission
    return worst_case <= available_cash, worst_case


class SessionLedger:
    """One Session's reserved cash and Unit-cap headroom (ADR 0020's
    "a proposal reserves its cash and its cap headroom" amendment, and ADR
    0021 section 3's session-close pass): every Add and entry decided in
    ONE session-close pass is checked against what EARLIER proposals in
    that SAME pass have already claimed, not only against already-committed
    positions -- otherwise two proposals that would each fit alone, decided
    in the same pass, could together overspend the account or a cap.

    ``main.py`` constructs a fresh ``SessionLedger`` once per trading day,
    seeded with that day's available cash (see README.md, "Deviations", for
    what this simplifies away: the Go engine's full hold lifecycle, where a
    hold is released mid-Session by a fill, an expiry or a cancellation --
    this ledger only ever grows more committed across one Session, which is
    the conservative direction).
    """

    def __init__(self, available_cash, unit_caps):
        self.available_cash = available_cash
        self.caps = unit_caps

    def try_reserve(self, symbol, industry, sector, quantity, price_cap_value, slippage_value,
                     dollars_per_point, commission):
        """Attempt to reserve one proposed Unit's worst-case cost and cap
        headroom. Returns ``(accepted, reason)``; ``reason`` is ``None`` on
        acceptance, else ``"unit-cap-exceeded:<name>"`` or
        ``"insufficient-cash"`` (ADR 0008's and ADR 0020's own decline
        reasons), checked caps-then-cash to match
        internal/strategy.evaluateAdd's own ordering."""
        exceeded, cap_name = self.caps.would_exceed(symbol, industry, sector)
        if exceeded:
            return False, "unit-cap-exceeded:" + cap_name
        affordable, cost = affordable_quantity(quantity, price_cap_value, slippage_value,
                                                dollars_per_point, commission, self.available_cash)
        if not affordable:
            return False, "insufficient-cash"
        self.available_cash -= cost
        self.caps.add(symbol, industry, sector)
        return True, None


# ---------------------------------------------------------------------------
# ADR 0012: CAGR, max drawdown, and the primary metric.
# ---------------------------------------------------------------------------


def annualised_return(start_equity, end_equity, elapsed_days):
    """ADR 0012 (Amendment: executable windows and opening records,
    2026-09-25): the primary numerator is annualised return,

        (E1 / E0) ^ (1 / Y) - 1,   Y = elapsed hours / (24 x 365.25)

    Returns ``None`` (a "null" figure, in the ADR's own words) for a
    non-positive starting equity or elapsed span, or a non-finite result.
    """
    if not (isfinite(start_equity) and start_equity > 0) or not (isfinite(elapsed_days) and elapsed_days > 0):
        return None
    years = elapsed_days / 365.25
    try:
        result = (end_equity / start_equity) ** (1.0 / years) - 1.0
    except (ZeroDivisionError, OverflowError, ValueError):
        return None
    return result if isfinite(result) else None


def max_drawdown(equity_curve):
    """ADR 0012: max drawdown is ``max((running_peak - equity) / running_peak)``
    over the net account-equity marks given, in order. Returns ``None`` for
    an empty curve."""
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
    """ADR 0012: the primary metric, annualised return / max drawdown.
    Returns ``None`` (a "null ratio", never an infinite winning score) for
    a null CAGR, a null drawdown, or a zero drawdown."""
    if cagr is None or mdd is None or mdd == 0:
        return None
    return cagr / mdd
