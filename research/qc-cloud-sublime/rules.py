"""Sublime Trading rule core, for the free QuantConnect Cloud research check.

Pure, standard-library Python with no QuantConnect imports, so it runs under
`python3 -m unittest` and main.py can import it unchanged once both files are
pasted into a QuantConnect project.

Every rule cites docs/methodology/Methodology_Analysis.md (section and rule
number, with the source key it gives: [V hh:mm:ss] the webinar, [M p.N] the
e-book, [R] the risk rules, [4PS], [2B], [30], [KISS]) or an ADR, and carries
the analysis's provenance tag: DISCLOSED, RECONSTRUCTED or PROXY. EXCLUDED
rules are absent. A parameter the sources leave open is a named constant
whose default README.md lists as an open question.

Research-only: this is not the production Go engine, and the Sublime control
has no Go implementation to be checked against.
"""

from collections import deque, namedtuple
from math import floor, isfinite

# ---------------------------------------------------------------------------
# Parameters. Tags follow Methodology_Analysis.md section 0.
# ---------------------------------------------------------------------------

#: ATR lookback. The sources give none (section 3.8 rule 32, section 3.11);
#: open question. CONTEXT.md "ATR": a separate term and parameter from N.
ATR_PERIOD = 20

#: Initial stop distance in ATR: "~3 x ATR" [M p.55-56], section 3.8 rule 32.
#: DISCLOSED (multiple).
STOP_ATR = 3.0

#: A further position once the previous one has moved 1 ATR in profit
#: [V 01:20:45-01:21:04], section 3.9 rule 39. DISCLOSED.
ADD_SPACING_ATR = 1.0

#: Positions per asset. Not disclosed (section 3.9 rule 42); the e-book's
#: illustration is about five per asset [M p.56-57]. Open question.
MAX_POSITIONS_PER_ASSET = 5

#: Per-position risk by market regime [V 00:28:01-00:28:13; R; M p.55],
#: section 3.7 rule 27 and section 4.5. The map from regime to level is
#: RECONSTRUCTED; 2 % ("optimal conditions") is not used. Open question.
RISK_FULL_BLOOM = 0.01
RISK_NOT_ALIGNED = 0.005
RISK_BELOW_WEEKLY_200 = 0.0025

#: [R]: the top of each range when the S&P prints all-time highs "and the
#: market is in full bloom". A sensitivity variant only (README.md,
#: "Sensitivity variants"); "printing all-time highs" is our reading: an
#: all-time high within the last SCAN_CHANNEL bars. RECONSTRUCTED.
RISK_FULL_BLOOM_AT_ATH = 0.02
DAILY_RISK_CEILING_AT_ATH = 0.08
AGGREGATE_RISK_CEILING_AT_ATH = 0.20

#: Portfolio ceilings [R], section 3.7 rule 29 and section 4.5: risk
#: initiated per day 4-8 %, aggregate open risk 10-20 %; the lower ends,
#: whatever the S&P is doing (open question). No more than 2 % at risk on
#: one asset [M p.56], section 3.9 rule 40.
DAILY_RISK_CEILING = 0.04
AGGREGATE_RISK_CEILING = 0.10
MAX_RISK_PER_ASSET = 0.02

#: Hard filters, section 3.1 rule 3: price >= $20 [V 01:13:32]; US volume
#: > 1 M shares [V 00:59:58-01:00:06]; 5-10 years of history [M p.54], the
#: lower end in daily bars. DISCLOSED.
MIN_PRICE = 20.0
MIN_VOLUME = 1_000_000.0
MIN_HISTORY_BARS = 5 * 252

#: The window the volume floor is a median over (not given; ADR 0009's 20-day
#: median convention). Open question.
VOLUME_WINDOW = 20

#: Second-breakout state machine, section 6 Model E (PROXY for rules 21-23).
#: BASE_LENGTH is the 55-trading-day threshold [2B p.1], section 4.8. The
#: others are "ours" per Model E: RETEST_ATR k1, CANCEL_ATR k2 (a close that
#: would have hit the 3 x ATR stop of a first-breakout entry), RETEST_WINDOW
#: m. Open questions.
BREAKOUT_CHANNEL = 55
BASE_LENGTH = 55

#: The Phase A level and the base, as FourPhaseSetup options. The control is
#: Model E: the level is max(prior channel high, last calendar year's high)
#: and the base is BASE_LENGTH bars without a new channel high. The
#: alternatives are README.md "Sensitivity variants": the level as last
#: calendar year's high alone, the webinar's breakout [V 00:45:49-00:46:24];
#: and the base as BASE_LENGTH consecutive closes at or below the level, the
#: consolidation "between the high and the low of last year" [V 00:46:00].
LEVEL_CHANNEL_AND_LAST_YEAR = "channel-and-last-year"
LEVEL_LAST_YEAR = "last-year"
BASE_NO_NEW_CHANNEL_HIGH = "no-new-channel-high"
BASE_CLOSES_BELOW_LEVEL = "closes-below-level"
RETEST_ATR = 1.0
CANCEL_ATR = 3.0
RETEST_WINDOW = 55

#: Scanner criterion: a break and close above the 20-day channel
#: [V 00:47:53-00:48:12], section 3.1 rule 2. DISCLOSED.
SCAN_CHANNEL = 20

#: Public simplified exit: the daily Donchian-20 low [30 p.7-8], section 3.8
#: rule 34 and section 4.6 variant (a). DISCLOSED.
EXIT_CHANNEL = 20

#: Moving averages: daily 20/50/200 [V 00:26:06-00:26:45], weekly 50/200
#: [V 00:21:12-00:25:06], section 4.7. Bollinger: 20-period SMA of closes,
#: bands at 1 and 2 sigma [V 00:31:08-00:38:16], section 3.4. DISCLOSED.
BB_PERIOD = 20
LONG_SMA = 200
MID_SMA = 50
SHORT_SMA = 20

#: Faith's Strength (The Turtle Rules p.29; ADR 0010) over ATR, ranking
#: signals within a Grade: a stand-in for "the best-performing stocks"
#: [V 00:02:31-00:02:41]. PROXY.
STRENGTH_LOOKBACK_BARS = 63

#: Infrastructure conventions carried from research/qc-cloud, in ATR: an
#: entry rests as a stop-limit capped at level + 1 ATR (ADR 0005, as
#: amended), and every fill slips 0.05 ATR against the trader (ADR 0013).
GAP_BUFFER_ATR = 1.0
SLIPPAGE_ATR = 0.05

#: ADR 0013's IBKR Pro Fixed schedule, for the pre-trade cash estimate only.
IB_COMMISSION_PER_SHARE = 0.005
IB_COMMISSION_MINIMUM = 1.0
IB_COMMISSION_MAX_FRACTION_OF_TRADE_VALUE = 0.01

# ---------------------------------------------------------------------------
# Volatility, channels, averages.
# ---------------------------------------------------------------------------


def true_range(high, low, previous_close):
    """True Range: the bar's range including any gap from the previous
    close; the bar's own range when there is none."""
    if previous_close is None:
        return high - low
    return max(high - low, high - previous_close, previous_close - low)


class WilderAverage:
    """ATR: a Wilder average of True Range, seeded by a simple average of
    the first ``period`` values. Read ``value`` for a decision before adding
    that decision bar's own True Range (CONTEXT.md "Completed bar")."""

    def __init__(self, period=ATR_PERIOD):
        if period <= 0:
            raise ValueError("period must be positive")
        self.period = period
        self._seed = []
        self._count = 0
        self.value = 0.0

    def add(self, tr):
        self._count += 1
        if self._count <= self.period:
            self._seed.append(tr)
            if self._count == self.period:
                self.value = sum(self._seed) / self.period
            return
        self.value = ((self.period - 1) * self.value + tr) / self.period

    @property
    def ready(self):
        return self._count >= self.period


class Channel:
    """The highest (or lowest) of the last ``length`` values added.
    ``extreme()`` is (value, ready); value is None before any value. Evaluate
    a bar against the channel before adding that bar."""

    def __init__(self, length, highest=True):
        self.length = length
        self.highest = highest
        self._values = deque(maxlen=length)

    def extreme(self):
        if not self._values:
            return None, False
        pick = max if self.highest else min
        return pick(self._values), len(self._values) >= self.length

    def add(self, value):
        self._values.append(value)


def sma(values, period):
    """(simple average of the last ``period`` values, ready)."""
    if len(values) < period:
        return None, False
    window = list(values)[-period:]
    return sum(window) / period, True


def above(value, reference):
    """Whether ``value`` is strictly above ``reference``; None when the
    reference is not ready, so a caller can fail closed on it."""
    if reference is None:
        return None
    return value > reference


def bollinger_colour(closes, period=BB_PERIOD):
    """The trend-filter colour of the last close (section 3.4 rule 13
    [V 00:31:08-00:38:16], DISCLOSED): 20-period SMA of closes with bands at
    1 and 2 population standard deviations; above +2 sigma "dark-green",
    above +1 "green", below -2 "dark-red", below -1 "red", else "grey".
    None without a full window. ``closes`` end with the close being
    coloured."""
    if len(closes) < period:
        return None
    window = list(closes)[-period:]
    mean = sum(window) / period
    sigma = (sum((c - mean) ** 2 for c in window) / period) ** 0.5
    close = window[-1]
    if close > mean + 2 * sigma:
        return "dark-green"
    if close > mean + sigma:
        return "green"
    if close < mean - 2 * sigma:
        return "dark-red"
    if close < mean - sigma:
        return "red"
    return "grey"


def is_bullish_colour(colour):
    """Model D's admission: colour in {green, dark-green} (section 6)."""
    return colour in ("green", "dark-green")


class WeeklyCloses:
    """Weekly closes built from completed daily bars: a week's close is its
    last daily close, and the week in progress counts with its latest close,
    as a weekly chart shows it at a daily close. Weeks are ISO weeks."""

    def __init__(self, keep=LONG_SMA):
        self.keep = keep
        self._completed = deque(maxlen=keep)
        self._week = None
        self._current = None

    def add(self, bar_date, close):
        week = bar_date.isocalendar()[:2]
        if self._week is not None and week != self._week:
            self._completed.append(self._current)
        self._week, self._current = week, close

    def series(self):
        """Completed weekly closes, then the week in progress; at most
        ``keep`` values, oldest first."""
        if self._current is None:
            return []
        return (list(self._completed) + [self._current])[-self.keep:]


class CalendarYearRange:
    """Each calendar year's high and low, for the monthly regime and the
    last-year-high levels (section 3.2 rule 5 [V 00:07:32-00:07:43]: the
    previous calendar year, not the trailing twelve months)."""

    def __init__(self):
        self._years = {}      # year -> [high, low, first bar month]

    def add(self, bar_date, high, low):
        entry = self._years.get(bar_date.year)
        if entry is None:
            self._years[bar_date.year] = [high, low, bar_date.month]
            for old in [y for y in self._years if y < bar_date.year - 1]:
                del self._years[old]
            return
        entry[0], entry[1] = max(entry[0], high), min(entry[1], low)

    def previous_year(self, bar_date):
        """(high, low) of the calendar year before ``bar_date``'s, or
        (None, None) unless that year was observed from January."""
        entry = self._years.get(bar_date.year - 1)
        if entry is None or entry[2] != 1:
            return None, None
        return entry[0], entry[1]


Snapshot = namedtuple("Snapshot", ["prior_channel_high", "prior_20_high", "all_time_high", "atr",
                                   "last_year_high", "last_year_low"])


class Indicators:
    """One instrument's indicators over split-adjusted completed daily bars
    (ADR 0004). ``snapshot(bar_date)`` reads them as they stand BEFORE the
    bar dated ``bar_date``; ``advance`` then folds that bar in. A bar dated on
    or before the last one advanced is ignored, so a History backfill and a
    live delivery of the same day are never both counted."""

    def __init__(self, breakout_channel=BREAKOUT_CHANNEL):
        self.atr = WilderAverage()
        self.high_channel = Channel(breakout_channel)
        self.high_20 = Channel(SCAN_CHANNEL)
        self.low_20 = Channel(EXIT_CHANNEL, highest=False)
        self.closes = deque(maxlen=LONG_SMA)
        self.volumes = deque(maxlen=VOLUME_WINDOW)
        self.weekly = WeeklyCloses()
        self.years = CalendarYearRange()
        self.all_time_high = None
        self.previous_close = None
        self.last_date = None
        self.bars = 0

    def snapshot(self, bar_date):
        high_ch, ready_ch = self.high_channel.extreme()
        high_20, ready_20 = self.high_20.extreme()
        last_high, last_low = self.years.previous_year(bar_date)
        return Snapshot(high_ch if ready_ch else None, high_20 if ready_20 else None,
                        self.all_time_high, self.atr.value if self.atr.ready else None,
                        last_high, last_low)

    def advance(self, bar_date, high, low, close, volume):
        """Fold in one completed bar; False when it was ignored."""
        if self.last_date is not None and bar_date <= self.last_date:
            return False
        self.atr.add(true_range(high, low, self.previous_close))
        self.high_channel.add(high)
        self.high_20.add(high)
        self.low_20.add(low)
        self.closes.append(close)
        self.volumes.append(volume)
        self.weekly.add(bar_date, close)
        self.years.add(bar_date, high, low)
        self.all_time_high = high if self.all_time_high is None else max(self.all_time_high, high)
        self.previous_close, self.last_date = close, bar_date
        self.bars += 1
        return True

    def exit_low(self):
        """The Exit Channel after the last bar: the lowest low of the last
        20 completed bars (section 3.8 rule 34), or None until ready."""
        low, ready = self.low_20.extreme()
        return low if ready else None

    def daily_sma(self, period):
        return sma(self.closes, period)[0]

    def weekly_sma(self, period):
        return sma(self.weekly.series(), period)[0]

    def daily_colour(self):
        return bollinger_colour(list(self.closes))

    def weekly_colour(self):
        return bollinger_colour(self.weekly.series())


# ---------------------------------------------------------------------------
# Market regime (S&P 500) and stock alignment.
# ---------------------------------------------------------------------------


def monthly_regime(close, last_year_high, last_year_low):
    """Section 3.2 rule 5 [V 00:08:05-00:08:30; KISS; M p.48], DISCLOSED:
    above last calendar year's high "bull", below its low "bear", between
    "sideways". None when last year is not known."""
    if last_year_high is None or last_year_low is None:
        return None
    if close > last_year_high:
        return "bull"
    if close < last_year_low:
        return "bear"
    return "sideways"


def market_risk_fraction(monthly, weekly_200, weekly_50, daily_200, daily_50, daily_20,
                         at_all_time_high=False):
    """Per-position risk for a new position given the S&P regime (section
    3.2 rules 4-8, section 3.7 rule 27, section 4.5; the map RECONSTRUCTED).
    Each weekly/daily argument says whether the index closed above that
    average, or is None when it is not ready.

    - Not a bull month: 0, stand aside (rule 4 [V 00:05:09-00:05:19]; shorts
      are out of scope).
    - Every timeframe aligned ("full bloom", rule 8): RISK_FULL_BLOOM, or
      RISK_FULL_BLOOM_AT_ATH when ``at_all_time_high`` [R].
    - Above the weekly 200 but otherwise not aligned: RISK_NOT_ALIGNED.
    - Below the weekly 200: RISK_BELOW_WEEKLY_200.

    Any input not ready fails closed to 0."""
    inputs = (weekly_200, weekly_50, daily_200, daily_50, daily_20)
    if monthly != "bull" or any(flag is None for flag in inputs):
        return 0.0
    if all(inputs):
        return RISK_FULL_BLOOM_AT_ATH if at_all_time_high else RISK_FULL_BLOOM
    if weekly_200:
        return RISK_NOT_ALIGNED
    return RISK_BELOW_WEEKLY_200


def risk_ceilings(at_all_time_high=False, full_bloom=False):
    """(daily-initiated, aggregate) risk ceilings [R], section 3.7 rule 29:
    the lower ends, or the upper ends only when the S&P prints all-time
    highs AND every timeframe is aligned (full bloom) -- the same condition
    under which market_risk_fraction returns RISK_FULL_BLOOM_AT_ATH."""
    if at_all_time_high and full_bloom:
        return DAILY_RISK_CEILING_AT_ATH, AGGREGATE_RISK_CEILING_AT_ATH
    return DAILY_RISK_CEILING, AGGREGATE_RISK_CEILING


def printing_all_time_highs(recent_high, all_time_high):
    """Whether the index set its all-time high within the recent window:
    ``recent_high`` is the SCAN_CHANNEL-bar high including the last bar."""
    return recent_high is not None and all_time_high is not None and recent_high >= all_time_high


def stock_aligned(close, prev_year_high, weekly_sma200, daily_sma200, daily_colour, weekly_colour):
    """The stock's alignment gate at its Signal (section 4.7: the KISS
    minimal gate, close above last calendar year's high, the weekly 200 and
    the daily 200 SMA; section 3.4 and Model D: daily and weekly trend-filter
    colour green or dark green). DISCLOSED. Any input not ready fails."""
    levels = (prev_year_high, weekly_sma200, daily_sma200)
    if any(level is None for level in levels):
        return False
    return (all(close > level for level in levels)
            and is_bullish_colour(daily_colour) and is_bullish_colour(weekly_colour))


# ---------------------------------------------------------------------------
# The second-breakout entry (4PS Phase A/B/C).
# ---------------------------------------------------------------------------


class FourPhaseSetup:
    """Section 6 Model E, the PROXY for the 4PS entry (section 3.6 rules
    21-23 [4PS p.2-3; V 00:45:49-00:47:32; 2B p.1]):

    - BASE: at least ``base_length`` completed bars without a new channel
      high (``base_rule`` BASE_CLOSES_BELOW_LEVEL: closes at or below the
      level instead).
    - Phase A: close above max(prior channel high, last calendar year's
      high), or last year's high alone with ``level_rule`` LEVEL_LAST_YEAR;
      that level is the breakout level.
    - Phase B: a low at or below the level + ``retest_atr`` ATR.
    - Phase C, the Signal: a close above the highest high between A and B,
      that is also a break and close above the prior 20-bar high (rule 2).
    - Cancel: a close more than ``cancel_atr`` ATR below the level, or more
      than ``window`` bars after A.

    State carries memory across bars (ADR 0011's consequences). Call ``step``
    once per completed bar with the channels and ATR as they stood before it."""

    IDLE, BREAKOUT, RETESTED = "idle", "phase-a", "phase-b"

    def __init__(self, base_length=BASE_LENGTH, retest_atr=RETEST_ATR, cancel_atr=CANCEL_ATR,
                 window=RETEST_WINDOW, level_rule=LEVEL_CHANNEL_AND_LAST_YEAR,
                 base_rule=BASE_NO_NEW_CHANNEL_HIGH):
        self.base_length = base_length
        self.level_rule, self.base_rule = level_rule, base_rule
        self.retest_atr = retest_atr
        self.cancel_atr = cancel_atr
        self.window = window
        self.sessions_in_base = 0
        self.reset()

    def reset(self):
        self.phase = self.IDLE
        self.level = self.reaction_high = None
        self.sessions_since_breakout = 0

    def _candidate_level(self, prior_channel_high, last_year_high):
        if self.level_rule == LEVEL_LAST_YEAR:
            return last_year_high
        if prior_channel_high is None or last_year_high is None:
            return None
        return max(prior_channel_high, last_year_high)

    def step(self, high, low, close, atr, prior_channel_high, prior_20_high, last_year_high,
             active=True):
        """Advance by one completed bar; True when it is the Signal. With
        ``active`` False (the instrument is in a Campaign, so not a Setup:
        CONTEXT.md) only the base is counted."""
        signal = False
        candidate = self._candidate_level(prior_channel_high, last_year_high)
        if not active:
            self.reset()
        elif self.phase == self.IDLE:
            if (self.sessions_in_base >= self.base_length and atr and candidate is not None
                    and close > candidate):
                self.phase = self.BREAKOUT
                self.level = candidate
                self.reaction_high = high
                self.sessions_since_breakout = 0
        else:
            self.sessions_since_breakout += 1
            if (not atr or close < self.level - self.cancel_atr * atr
                    or self.sessions_since_breakout > self.window):
                self.reset()
            elif self.phase == self.BREAKOUT:
                self.reaction_high = max(self.reaction_high, high)
                if low <= self.level + self.retest_atr * atr:
                    self.phase = self.RETESTED
            elif (close > self.reaction_high and prior_20_high is not None
                  and close > prior_20_high):
                signal = True
                self.reset()
        if self.base_rule == BASE_CLOSES_BELOW_LEVEL:
            broken = candidate is None or close > candidate
        else:
            broken = prior_channel_high is None or high > prior_channel_high
        if broken:
            self.sessions_in_base = 0
        else:
            self.sessions_in_base += 1
        return signal


def grade(close, prior_all_time_high):
    """"A" when the Signal's close is above every earlier high in the
    available history ("at all-time highs", section 3.5 rule 16
    [V 00:48:16, 00:52:15-00:53:03]; PROXY for history before the data
    starts), else "B" (rule 18; CONTEXT.md "Grade")."""
    if prior_all_time_high is not None and close > prior_all_time_high:
        return "A"
    return "B"


#: ``eligible`` defaults True: most callers (and every existing test) only
#: care about grading, not the hard filters.
Signal = namedtuple("Signal", ["symbol", "grade", "strength", "median_dollar_volume", "eligible"])
Signal.__new__.__defaults__ = (True,)


def admissible_signals(signals):
    """A Session's Signals in the order they are decided: an ineligible
    Signal (rules.is_eligible: price, volume, history floors) is dropped
    first, so it can never suppress an eligible Grade B; Grade B is then
    taken only when no Grade A Signal remains (section 3.5 rule 18
    [V 01:14:41-01:15:07]; ADR 0011 point 3); within a Grade by descending
    Strength, then descending median dollar volume, then symbol (ADR 0010's
    tie-breaks)."""
    signals = [s for s in signals if s.eligible]
    if any(s.grade == "A" for s in signals):
        signals = [s for s in signals if s.grade == "A"]
    return sorted(signals, key=lambda s: (-s.strength, -s.median_dollar_volume, s.symbol))


def strength(closes, atr):
    """(close now - close 63 bars earlier) / ATR, and whether it is ready."""
    closes = list(closes)
    if len(closes) < STRENGTH_LOOKBACK_BARS + 1 or not (atr and atr > 0):
        return 0.0, False
    return (closes[-1] - closes[-1 - STRENGTH_LOOKBACK_BARS]) / atr, True


def median(values, window):
    """(median of the last ``window`` values, ready); an even window takes
    the mean of the two middle values."""
    values = list(values)
    if len(values) < window:
        return None, False
    ordered = sorted(values[-window:])
    mid = window // 2
    if window % 2:
        return ordered[mid], True
    return (ordered[mid - 1] + ordered[mid]) / 2.0, True


def is_eligible(raw_price, median_volume, history_bars, min_history_bars=MIN_HISTORY_BARS):
    """Section 3.1 rule 3's hard filters, DISCLOSED: raw price >= $20, median
    raw share volume >= 1 M, >= 5 years of completed bars (fewer only as a
    README.md "Sensitivity variants" run)."""
    return (raw_price >= MIN_PRICE and median_volume is not None
            and median_volume >= MIN_VOLUME and history_bars >= min_history_bars)


# ---------------------------------------------------------------------------
# Sizing, positions and risk.
# ---------------------------------------------------------------------------


def shares_for_risk(equity, risk_fraction, entry, stop):
    """Section 3.7 rule 30 [M p.56], RECONSTRUCTED; ADR 0003's
    fixed-risk-at-stop Sizing Mode: floor(equity x risk / (entry - stop)).
    0 when there is no risk budget or no distance to the stop."""
    distance = entry - stop
    if not (risk_fraction > 0 and distance > 0 and equity > 0):
        return 0
    budget = equity * risk_fraction
    shares = floor(budget / distance)
    if shares > 0 and shares * distance > budget:
        shares -= 1
    return int(shares)


def initial_stop(entry, atr):
    """Section 3.8 rule 32 [M p.55-56]: entry - 3 x ATR."""
    return entry - STOP_ATR * atr


def position_size(equity, risk_fraction, atr):
    """Shares whose 3 x ATR stop risks ``risk_fraction`` of equity."""
    return shares_for_risk(equity, risk_fraction, STOP_ATR * atr, 0.0)


class Campaign:
    """Sublime positions in one instrument (CONTEXT.md "Campaign"). Each
    position is separately sized and stopped (section 3.9 rule 41 [M p.56]):
    a dict of fill_price, quantity, atr and its own initial stop, 3 x ATR
    below its own fill. Its Exit Order rests at the higher of that stop and
    the Exit Channel (section 3.8 rule 34), so the 20-day low trails it."""

    def __init__(self, symbol, fill_price, quantity, atr, max_positions=MAX_POSITIONS_PER_ASSET):
        self.symbol = symbol
        self.max_positions = max_positions
        self.units = []
        self.realized = 0.0
        self.add_unit(fill_price, quantity, atr)
        self._initial_risk = quantity * STOP_ATR * atr

    def add_unit(self, fill_price, quantity, atr):
        stop = initial_stop(fill_price, atr)
        # resting_stop starts at the initial stop and is kept in sync with
        # the actual Exit Order price by set_resting_stop, since that order
        # can rest a raw tick below the theoretical exit_level (README.md
        # "Deviations").
        self.units.append({"fill_price": fill_price, "quantity": quantity, "atr": atr,
                           "stop": stop, "resting_stop": stop})

    def exit_level(self, index, exit_low):
        """A position's target Exit Order level: max(its stop, the Exit
        Channel), before main.py places it at a raw tick."""
        stop = self.units[index]["stop"]
        return stop if exit_low is None else max(stop, exit_low)

    def set_resting_stop(self, index, price):
        """Record the price a position's Exit Order actually rests at
        (main.py's raw-tick floor, and any further tick below the bar it
        was amended after), for open_risk and add_ready's risk-free check
        to read instead of the theoretical exit_level (README.md
        "Deviations")."""
        self.units[index]["resting_stop"] = price

    def open_risk(self):
        """Money lost if every position exits at its resting Exit Order
        price; a position at or above its fill contributes 0 (CONTEXT.md
        "risk-free")."""
        return sum(u["quantity"] * max(0.0, u["fill_price"] - u["resting_stop"])
                   for u in self.units)

    def add_ready(self, close):
        """Whether a further position may be proposed at this close: fewer
        than the maximum; the first position's resting Exit Order has no
        remaining risk (section 3.9 rule 40 [M p.56]); and the close is at
        least 1 ATR above the newest position's fill (rule 39
        [V 01:20:45], its ATR at entry)."""
        if len(self.units) >= self.max_positions:
            return False
        if self.units[0]["resting_stop"] < self.units[0]["fill_price"]:
            return False
        newest = self.units[-1]
        return close >= newest["fill_price"] + ADD_SPACING_ATR * newest["atr"]

    def close_unit(self, index, exit_price):
        unit = self.units.pop(index)
        self.realized += unit["quantity"] * (exit_price - unit["fill_price"])

    def reduce_unit(self, index, filled_quantity, exit_price):
        """A partial fill on the position's Exit Order, then cancelled:
        realize the shares that sold and reduce the position's recorded
        quantity by exactly that many, so a replacement stop is never sized
        for shares no longer held (README.md, "Deviations")."""
        unit = self.units[index]
        self.realized += filled_quantity * (exit_price - unit["fill_price"])
        unit["quantity"] -= filled_quantity

    def r_multiple(self):
        """Every closed position's money over the first position's initial
        risk (this script's reporting convention)."""
        return self.realized / self._initial_risk


class RiskBudget:
    """One Session's risk and cash budget (section 3.7 rule 29 [R], section
    3.9 rule 40 [M p.56]; ADR 0010's no borrowing). ``try_reserve`` checks, in
    order, the per-asset ceiling, the daily-initiated ceiling, the aggregate
    open-risk ceiling and cash, and spends the budget only when all pass.
    Risk is money at the stop; ``equity`` is the previous close's."""

    def __init__(self, equity, open_risk, cash, daily=DAILY_RISK_CEILING,
                 aggregate=AGGREGATE_RISK_CEILING, per_asset=MAX_RISK_PER_ASSET):
        self.equity = equity
        self.open_risk = open_risk
        self.cash = cash
        self.initiated = 0.0
        self.daily, self.aggregate, self.per_asset = daily, aggregate, per_asset

    def try_reserve(self, asset_open_risk, new_risk, cost):
        if asset_open_risk + new_risk > self.per_asset * self.equity:
            return False, "asset-risk"
        if self.initiated + new_risk > self.daily * self.equity:
            return False, "daily-risk"
        if self.open_risk + new_risk > self.aggregate * self.equity:
            return False, "aggregate-risk"
        if cost > self.cash:
            return False, "insufficient-cash"
        self.initiated += new_risk
        self.open_risk += new_risk
        self.cash -= cost
        return True, None


# ---------------------------------------------------------------------------
# Price views, orders and costs (ADR 0004, 0005, 0013, 0024), carried from
# research/qc-cloud/rules.py.
# ---------------------------------------------------------------------------

#: The US equity minimum price variation, in raw dollars.
RAW_TICK = 0.01
#: How far a raw/split-adjusted price ratio may miss a whole number and
#: still be that whole split ratio (factor files carry about seven digits).
SPLIT_RATIO_TOLERANCE = 1e-4
#: LEAN's InteractiveBrokersFeeModel as observed on the pinned image: $0.005
#: per raw share, a $1.00 minimum that wins over a 0.5 %-of-value cap.
LEAN_IB_PER_SHARE = 0.005
LEAN_IB_MINIMUM = 1.0
LEAN_IB_MAX_FRACTION_OF_VALUE = 0.005


def _whole_if_close(ratio):
    whole = round(ratio)
    if whole >= 1 and abs(ratio - whole) <= SPLIT_RATIO_TOLERANCE * whole:
        return whole
    return ratio


def split_ratio(raw_price, split_adjusted_price):
    """Split-adjusted shares per raw share from one bar's two prices; None
    unless both are finite and positive, so no view is ever guessed."""
    if not all(isfinite(p) and p > 0 for p in (raw_price, split_adjusted_price)):
        return None
    return _whole_if_close(raw_price / split_adjusted_price)


def ratio_after_split(ratio, split_factor):
    """The ratio once a split with LEAN's factor (new price / old) applies."""
    return _whole_if_close(ratio * split_factor)


def whole_raw_shares(quantity, ratio):
    """``quantity`` split-adjusted shares rounded down to whole raw shares,
    restated split-adjusted: never more than was sized (ADR 0010)."""
    raw = floor(quantity / ratio + 1e-9)
    return int(floor(raw * ratio + 1e-9))


def raw_tick_round(price, ratio, tick=RAW_TICK):
    """A split-adjusted price at the nearest raw tick, restated."""
    return round(round(price * ratio / tick) * tick, 10) / ratio


def raw_tick_floor(price, ratio, tick=RAW_TICK):
    """A split-adjusted price rounded down to a raw tick, restated: a limit
    never rises above the cap its hold was computed from (ADR 0005)."""
    ticks = floor(round(price * ratio / tick, 9))
    return round(ticks * tick, 10) / ratio


def above_by_a_tick(price, ratio, tick=RAW_TICK):
    """One raw tick above ``price``: "above the high of the qualifying
    breakout bar" (section 3.6 rule 25 [M p.55]; the level is DISCLOSED, the
    offset formula EXCLUDED, so the smallest step above is used)."""
    return (round(round(price * ratio / tick) * tick, 10) + tick) / ratio


def exit_stop_price(level, bar_low, ratio, tick=RAW_TICK):
    """An Exit Order's resting price for the next Session: ``level`` floored
    to a raw tick, and at least one raw tick below the low of the bar it was
    decided after. LEAN evaluates an amended order against the bar it was
    amended after (research/qc-cloud/README.md, Deviations #20); resting
    below that bar's low means it can never fill from a bar already seen."""
    return min(raw_tick_floor(level, ratio, tick), raw_tick_floor(bar_low, ratio, tick) - tick / ratio)


def lean_ib_commission(quantity, price, ratio):
    """ADR 0013: LEAN's IB fee on the raw shares an order is (ADR 0004)."""
    fee = abs(quantity) / ratio * LEAN_IB_PER_SHARE
    if fee < LEAN_IB_MINIMUM:
        return LEAN_IB_MINIMUM
    return min(fee, LEAN_IB_MAX_FRACTION_OF_VALUE * abs(quantity) * price)


def dividend_cash(quantity, distribution, reference_price, held_view_close):
    """ADR 0024: a dividend is the raw distribution on the raw shares held.
    ``quantity`` is in the view of ``held_view_close``; ``distribution`` is
    per share of ``reference_price``'s view."""
    if not quantity or not (reference_price > 0 and held_view_close > 0):
        return 0.0
    return quantity * distribution * held_view_close / reference_price


def price_cap(level, atr, gap_buffer_atr=GAP_BUFFER_ATR):
    """The highest price an entry may fill at before slippage (ADR 0005)."""
    return level + gap_buffer_atr * atr


def slippage(atr, slippage_atr=SLIPPAGE_ATR):
    """Slippage per fill, against the trader, never zero (ADR 0013)."""
    return slippage_atr * atr


def commission_estimate(quantity, price, per_share=IB_COMMISSION_PER_SHARE,
                        minimum=IB_COMMISSION_MINIMUM,
                        max_fraction=IB_COMMISSION_MAX_FRACTION_OF_TRADE_VALUE):
    """ADR 0013's schedule for the pre-trade cash estimate: rate, then
    floor, then cap."""
    if quantity <= 0 or price <= 0:
        return 0.0
    return min(max(quantity * per_share, minimum), max_fraction * quantity * price)


def worst_case_cost(quantity, cap, slippage_value, commission):
    """ADR 0020: an order's hold, at its price cap plus slippage."""
    return quantity * (cap + slippage_value) + commission


# ---------------------------------------------------------------------------
# Universe cadence and results (ADR 0009, ADR 0012).
# ---------------------------------------------------------------------------


def is_new_eligibility_month(previous_trading_date, current_trading_date):
    """True on the first trading day seen in a new calendar month."""
    if previous_trading_date is None:
        return True
    return (current_trading_date.year, current_trading_date.month) != \
        (previous_trading_date.year, previous_trading_date.month)


def annualised_return(start_equity, end_equity, elapsed_days):
    """ADR 0012: (E1/E0)^(1/Y) - 1 with Y in 365.25-day years; None when
    undefined."""
    if not (isfinite(start_equity) and start_equity > 0) or not (isfinite(elapsed_days) and elapsed_days > 0):
        return None
    try:
        result = (end_equity / start_equity) ** (365.25 / elapsed_days) - 1.0
    except (ZeroDivisionError, OverflowError, ValueError):
        return None
    return result if isfinite(result) else None


def max_drawdown(equity_curve):
    """ADR 0012: the largest fall from a running peak, as a fraction."""
    if not equity_curve:
        return None
    peak, worst = equity_curve[0], 0.0
    for equity in equity_curve:
        peak = max(peak, equity)
        if peak > 0:
            worst = max(worst, (peak - equity) / peak)
    return worst


def cagr_over_max_drawdown(cagr, mdd):
    """ADR 0012's primary metric; None rather than an infinite score."""
    if cagr is None or mdd is None or mdd == 0:
        return None
    return cagr / mdd
