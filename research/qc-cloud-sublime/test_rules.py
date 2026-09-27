"""Unit tests for the Sublime rule core (rules.py), on synthetic series only:
no QuantConnect, no network, no market data.

Run with:

    python3 -m unittest discover -s research/qc-cloud-sublime -p "test_*.py"

or `make research-test` from the repository root.

Golden scenarios are transcribed from docs/methodology/Methodology_Analysis.md
(section 9 names them): the Sublime EURUSD 3 x ATR stop [M p.55] and the
reconstructed fixed-risk sizing example ($100k, 1 %, entry 50, stop 44 -> 166
shares). Every other figure is a synthetic series built to exercise one rule.
"""

import unittest
from datetime import date

import rules


class TrueRangeAndAtrTests(unittest.TestCase):
    def test_true_range_takes_the_gap(self):
        self.assertEqual(rules.true_range(106.0, 104.0, 95.0), 11.0)
        self.assertEqual(rules.true_range(100.0, 98.0, 110.0), 12.0)
        self.assertEqual(rules.true_range(105.0, 100.0, None), 5.0)

    def test_atr_is_seeded_by_a_simple_average_then_wilder_smoothed(self):
        atr = rules.WilderAverage(period=4)
        for tr in (2.0, 4.0, 6.0):
            atr.add(tr)
        self.assertFalse(atr.ready)
        atr.add(8.0)
        self.assertTrue(atr.ready)
        self.assertAlmostEqual(atr.value, 5.0)
        atr.add(9.0)
        self.assertAlmostEqual(atr.value, (3 * 5.0 + 9.0) / 4)

    def test_atr_period_is_its_own_parameter_not_n(self):
        # CONTEXT.md "ATR": a separate term from N; its lookback is
        # undisclosed (Methodology_Analysis.md section 3.11), so it is a
        # named parameter of its own.
        self.assertEqual(rules.ATR_PERIOD, 20)
        self.assertEqual(rules.WilderAverage().period, rules.ATR_PERIOD)


class ChannelTests(unittest.TestCase):
    def test_highest_and_lowest_of_the_preceding_bars(self):
        high = rules.Channel(3, highest=True)
        low = rules.Channel(3, highest=False)
        self.assertEqual(high.extreme(), (None, False))
        for value in (5.0, 7.0, 6.0, 4.0):
            high.add(value)
            low.add(value)
        self.assertEqual(high.extreme(), (7.0, True))
        self.assertEqual(low.extreme(), (4.0, True))

    def test_not_ready_until_full(self):
        channel = rules.Channel(3)
        channel.add(1.0)
        self.assertEqual(channel.extreme(), (1.0, False))


class MovingAverageAndBollingerTests(unittest.TestCase):
    def test_sma_of_the_last_period_values(self):
        self.assertEqual(rules.sma([1.0, 2.0, 3.0, 4.0], 2), (3.5, True))
        self.assertEqual(rules.sma([1.0], 2), (None, False))

    def _flat_then(self, last):
        # 19 closes alternating 99/101 (mean 100, sigma ~1) and a last close.
        closes = [99.0 if i % 2 else 101.0 for i in range(19)]
        return closes + [last]

    def test_colour_is_by_closing_price_against_1_and_2_sigma(self):
        # Methodology_Analysis.md section 3.4 [V 00:31:08-00:38:16]: 20-period
        # SMA of closes, bands at 1 and 2 sigma; above +1 sigma green, above
        # +2 sigma dark green, between grey, below -1 red, below -2 dark red.
        self.assertEqual(rules.bollinger_colour(self._flat_then(100.0)), "grey")
        self.assertEqual(rules.bollinger_colour(self._flat_then(102.3)), "green")
        self.assertEqual(rules.bollinger_colour(self._flat_then(110.0)), "dark-green")
        self.assertEqual(rules.bollinger_colour(self._flat_then(98.0)), "red")
        self.assertEqual(rules.bollinger_colour(self._flat_then(90.0)), "dark-red")

    def test_colour_needs_a_full_window(self):
        self.assertIsNone(rules.bollinger_colour([100.0] * 19))

    def test_bullish_colours(self):
        self.assertTrue(rules.is_bullish_colour("green"))
        self.assertTrue(rules.is_bullish_colour("dark-green"))
        self.assertFalse(rules.is_bullish_colour("grey"))
        self.assertFalse(rules.is_bullish_colour(None))


class WeeklyCloseTests(unittest.TestCase):
    def test_a_week_is_completed_by_the_first_bar_of_the_next(self):
        weekly = rules.WeeklyCloses()
        weekly.add(date(2024, 1, 2), 10.0)   # Tuesday, ISO week 1
        weekly.add(date(2024, 1, 5), 11.0)   # Friday, same week
        self.assertEqual(weekly.series(), [11.0])
        weekly.add(date(2024, 1, 8), 12.0)   # Monday, week 2
        self.assertEqual(weekly.series(), [11.0, 12.0])

    def test_series_is_bounded(self):
        weekly = rules.WeeklyCloses(keep=3)
        for day in range(1, 60, 7):
            weekly.add(date(2024, 1, 1).fromordinal(date(2024, 1, 1).toordinal() + day), float(day))
        self.assertEqual(len(weekly.series()), 3)


class CalendarYearRangeTests(unittest.TestCase):
    def test_previous_calendar_year_high_and_low(self):
        # [V 00:07:32-00:07:43]: the previous CALENDAR year, not the
        # trailing twelve months.
        years = rules.CalendarYearRange()
        years.add(date(2020, 1, 2), 10.0, 8.0)
        years.add(date(2020, 12, 31), 20.0, 9.0)
        years.add(date(2021, 1, 4), 30.0, 5.0)
        self.assertEqual(years.previous_year(date(2021, 1, 5)), (20.0, 8.0))

    def test_not_ready_unless_the_previous_year_was_seen_from_january(self):
        years = rules.CalendarYearRange()
        years.add(date(2020, 6, 1), 10.0, 8.0)
        years.add(date(2021, 1, 4), 11.0, 9.0)
        self.assertEqual(years.previous_year(date(2021, 1, 5)), (None, None))
        self.assertEqual(years.previous_year(date(2022, 1, 5)), (11.0, 9.0))


class MarketRegimeTests(unittest.TestCase):
    def test_monthly_regime_from_last_calendar_year(self):
        # [V 00:08:05-00:08:30; KISS; M p.48].
        self.assertEqual(rules.monthly_regime(101.0, 100.0, 80.0), "bull")
        self.assertEqual(rules.monthly_regime(79.0, 100.0, 80.0), "bear")
        self.assertEqual(rules.monthly_regime(90.0, 100.0, 80.0), "sideways")
        self.assertIsNone(rules.monthly_regime(90.0, None, None))

    def test_full_alignment_is_full_bloom_risk(self):
        # [V 00:28:01]: 1 % when the market is "in full bloom".
        self.assertEqual(rules.market_risk_fraction("bull", True, True, True, True, True), 0.01)

    def test_bull_month_but_not_aligned_is_half(self):
        # [V 00:28:01-00:28:13]: 0.5 % or 0.25 % when indices are not aligned.
        self.assertEqual(rules.market_risk_fraction("bull", True, False, True, True, True), 0.005)
        self.assertEqual(rules.market_risk_fraction("bull", True, True, True, True, False), 0.005)

    def test_below_the_weekly_200_is_a_quarter(self):
        self.assertEqual(rules.market_risk_fraction("bull", False, False, True, True, True), 0.0025)

    def test_sideways_or_bear_stands_aside(self):
        # [V 00:05:09-00:05:19]: flat -> stand aside; shorts only in a bear,
        # and this research check is long-only.
        self.assertEqual(rules.market_risk_fraction("sideways", True, True, True, True, True), 0.0)
        self.assertEqual(rules.market_risk_fraction("bear", True, True, True, True, True), 0.0)

    def test_an_unready_input_fails_closed(self):
        self.assertEqual(rules.market_risk_fraction(None, True, True, True, True, True), 0.0)
        self.assertEqual(rules.market_risk_fraction("bull", None, True, True, True, True), 0.0)

    def test_above_is_none_when_not_ready(self):
        self.assertIsNone(rules.above(10.0, None))
        self.assertTrue(rules.above(10.0, 9.0))
        self.assertFalse(rules.above(9.0, 9.0))


class StockAlignmentTests(unittest.TestCase):
    def test_kiss_gate_and_green_filter(self):
        # Methodology_Analysis.md section 4.7 (KISS gate: last-year high,
        # weekly 200, daily 200) and section 3.4 (daily and weekly colour).
        ok = dict(close=50.0, prev_year_high=45.0, weekly_sma200=40.0, daily_sma200=42.0,
                  daily_colour="green", weekly_colour="dark-green")
        self.assertTrue(rules.stock_aligned(**ok))
        for key, bad in (("prev_year_high", 51.0), ("weekly_sma200", 51.0),
                         ("daily_sma200", 51.0), ("daily_colour", "grey"),
                         ("weekly_colour", "red"), ("weekly_sma200", None)):
            args = dict(ok)
            args[key] = bad
            self.assertFalse(rules.stock_aligned(**args), key)


class FourPhaseSetupTests(unittest.TestCase):
    """Methodology_Analysis.md section 6, Model E: base, first breakout
    (Phase A), retest (Phase B), second breakout (Phase C)."""

    ATR = 1.0

    def _based(self):
        setup = rules.FourPhaseSetup(base_length=3, retest_atr=1.0, cancel_atr=3.0, window=10)
        for _ in range(3):
            self.assertFalse(setup.step(99.0, 98.0, 98.5, self.ATR, 100.0, 99.5, 100.0))
        return setup

    def test_no_first_breakout_without_a_base(self):
        setup = rules.FourPhaseSetup(base_length=3, retest_atr=1.0, cancel_atr=3.0, window=10)
        setup.step(102.0, 99.0, 101.0, self.ATR, 100.0, 100.0, 100.0)
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)

    def test_a_new_55_bar_high_resets_the_base(self):
        setup = rules.FourPhaseSetup(base_length=3, retest_atr=1.0, cancel_atr=3.0, window=10)
        setup.step(99.0, 98.0, 98.5, self.ATR, 100.0, 99.5, 100.0)
        setup.step(99.0, 98.0, 98.5, self.ATR, 100.0, 99.5, 100.0)
        setup.step(100.5, 98.0, 99.9, self.ATR, 100.0, 99.5, 100.0)  # new high, close below
        self.assertEqual(setup.sessions_in_base, 0)

    def test_first_breakout_needs_a_close_above_the_higher_of_55_bar_and_last_year_high(self):
        setup = self._based()
        # Close 101 beats the 55-bar high (100) but not last year's (102).
        setup.step(101.5, 99.0, 101.0, self.ATR, 100.0, 100.0, 102.0)
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)
        setup = self._based()
        setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0)
        self.assertEqual(setup.phase, rules.FourPhaseSetup.BREAKOUT)
        self.assertEqual(setup.level, 102.0)

    def test_retest_then_second_breakout_is_the_signal(self):
        setup = self._based()
        setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0)   # A, level 102
        self.assertFalse(setup.step(105.0, 103.5, 104.5, self.ATR, 103.0, 103.0, 102.0))  # runs on
        self.assertEqual(setup.reaction_high, 105.0)
        self.assertFalse(setup.step(104.0, 102.8, 103.0, self.ATR, 105.0, 105.0, 102.0))  # low within 1 ATR
        self.assertEqual(setup.phase, rules.FourPhaseSetup.RETESTED)
        # A close at the reaction high is not above it.
        self.assertFalse(setup.step(105.2, 103.0, 105.0, self.ATR, 105.0, 105.0, 102.0))
        # Phase C: a close above the reaction high and the prior 20-bar high.
        self.assertTrue(setup.step(106.0, 104.0, 105.5, self.ATR, 105.2, 105.2, 102.0))
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)

    def test_second_breakout_also_needs_the_donchian_20_break_and_close(self):
        # [V 00:47:53-00:48:12]: a break AND close above the 20-day channel.
        setup = self._based()
        setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0)
        setup.step(104.0, 102.5, 103.0, self.ATR, 103.0, 103.0, 102.0)   # retest
        self.assertFalse(setup.step(105.0, 103.0, 104.5, self.ATR, 104.0, 104.8, 102.0))
        self.assertEqual(setup.phase, rules.FourPhaseSetup.RETESTED)

    def test_a_close_3_atr_below_the_level_cancels(self):
        setup = self._based()
        setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0)
        setup.step(102.0, 98.0, 98.9, self.ATR, 103.0, 103.0, 102.0)      # 102 - 3 = 99
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)

    def test_the_window_expiring_cancels(self):
        setup = rules.FourPhaseSetup(base_length=3, retest_atr=1.0, cancel_atr=3.0, window=2)
        for _ in range(3):
            setup.step(99.0, 98.0, 98.5, self.ATR, 100.0, 99.5, 100.0)
        setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0)
        setup.step(106.0, 104.0, 105.0, self.ATR, 103.0, 103.0, 102.0)
        setup.step(107.0, 105.0, 106.0, self.ATR, 106.0, 106.0, 102.0)
        self.assertEqual(setup.phase, rules.FourPhaseSetup.BREAKOUT)
        setup.step(108.0, 106.0, 107.0, self.ATR, 107.0, 107.0, 102.0)
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)

    def test_inactive_setup_counts_the_base_but_never_signals(self):
        # CONTEXT.md "Setup": an instrument in a Campaign is not a Setup.
        setup = self._based()
        self.assertFalse(setup.step(103.0, 100.0, 102.5, self.ATR, 100.0, 100.0, 102.0, active=False))
        self.assertEqual(setup.phase, rules.FourPhaseSetup.IDLE)
        self.assertEqual(setup.sessions_in_base, 0)

    def test_an_unready_channel_is_not_a_base(self):
        setup = rules.FourPhaseSetup(base_length=3, retest_atr=1.0, cancel_atr=3.0, window=10)
        for _ in range(5):
            setup.step(99.0, 98.0, 98.5, self.ATR, None, None, None)
        self.assertEqual(setup.sessions_in_base, 0)

    def test_default_thresholds(self):
        setup = rules.FourPhaseSetup()
        self.assertEqual((setup.base_length, setup.retest_atr, setup.cancel_atr, setup.window),
                         (55, 1.0, 3.0, 55))


class GradeAndRankingTests(unittest.TestCase):
    def test_grade_a_is_a_close_above_every_earlier_high(self):
        # [V 00:48:16, 00:52:15-00:53:03]: prefer stocks at all-time highs.
        self.assertEqual(rules.grade(101.0, 100.0), "A")
        self.assertEqual(rules.grade(100.0, 100.0), "B")
        self.assertEqual(rules.grade(101.0, None), "B")

    def test_grade_b_only_when_no_grade_a_signal(self):
        # [V 01:14:41-01:15:07]; ADR 0011 point 3.
        a1 = rules.Signal("AAA", "A", 1.0, 10.0)
        a2 = rules.Signal("BBB", "A", 3.0, 10.0)
        b1 = rules.Signal("CCC", "B", 9.0, 10.0)
        self.assertEqual([s.symbol for s in rules.admissible_signals([a1, b1, a2])], ["BBB", "AAA"])
        self.assertEqual([s.symbol for s in rules.admissible_signals([b1])], ["CCC"])

    def test_an_ineligible_grade_a_never_suppresses_an_eligible_grade_b(self):
        # README.md, "Deviations": the hard filters (rules.is_eligible) are
        # checked before Grade A's suppression of Grade B, so a Grade A
        # Signal that fails them can never remove an eligible Grade B one.
        a1 = rules.Signal("AAA", "A", 5.0, 10.0, eligible=False)
        b1 = rules.Signal("BBB", "B", 1.0, 10.0, eligible=True)
        self.assertEqual([s.symbol for s in rules.admissible_signals([a1, b1])], ["BBB"])

    def test_ties_break_by_dollar_volume_then_symbol(self):
        s1 = rules.Signal("ZZZ", "A", 1.0, 5.0)
        s2 = rules.Signal("YYY", "A", 1.0, 9.0)
        s3 = rules.Signal("XXX", "A", 1.0, 5.0)
        self.assertEqual([s.symbol for s in rules.admissible_signals([s1, s2, s3])],
                         ["YYY", "XXX", "ZZZ"])

    def test_strength_is_63_bar_change_over_atr(self):
        closes = [10.0] + [11.0] * 62 + [16.0]
        self.assertEqual(rules.strength(closes, 2.0), (3.0, True))
        self.assertEqual(rules.strength(closes[1:], 2.0), (0.0, False))


class EligibilityTests(unittest.TestCase):
    def test_sublime_hard_filters(self):
        # [V 01:13:32] price >= $20; [V 00:59:58-01:00:06] > 1M shares;
        # [M p.54] 5 years of history.
        self.assertTrue(rules.is_eligible(20.0, 1_000_000, 1260))
        self.assertFalse(rules.is_eligible(19.99, 1_000_000, 1260))
        self.assertFalse(rules.is_eligible(20.0, 999_999, 1260))
        self.assertFalse(rules.is_eligible(20.0, 1_000_000, 1259))

    def test_median_of_an_even_window(self):
        self.assertEqual(rules.median([4.0, 1.0, 3.0, 2.0], 4), (2.5, True))
        self.assertEqual(rules.median([1.0], 4), (None, False))


class SizingGoldenTests(unittest.TestCase):
    def test_reconstructed_fixed_risk_example(self):
        # Methodology_Analysis.md section 3.7 rule 30 (RECONSTRUCTED) and
        # section 9: $100k, 1 %, entry 50, stop 44 -> 166 shares.
        self.assertEqual(rules.shares_for_risk(100_000.0, 0.01, 50.0, 44.0), 166)

    def test_eurusd_three_atr_stop(self):
        # [M p.55-56]: 90 pips x 3 = 270 pips.
        entry = 1.1000
        stop = rules.initial_stop(entry, 0.0090)
        self.assertAlmostEqual(entry - stop, 0.0270)

    def test_position_size_is_risk_over_three_atr(self):
        # ADR 0003: fixed-risk-at-stop, so a wider stop means fewer shares.
        self.assertEqual(rules.position_size(100_000.0, 0.01, 2.0), 166)
        self.assertEqual(rules.position_size(100_000.0, 0.01, 4.0), 83)

    def test_nothing_is_sized_without_risk(self):
        self.assertEqual(rules.position_size(100_000.0, 0.0, 2.0), 0)
        self.assertEqual(rules.shares_for_risk(100_000.0, 0.01, 50.0, 50.0), 0)


class CampaignTests(unittest.TestCase):
    def _campaign(self):
        # One position: fill 100, ATR 2, stop 94 (3 x ATR), 50 shares.
        return rules.Campaign("XYZ", 100.0, 50, 2.0)

    def test_initial_stop_is_three_atr_from_the_fill(self):
        campaign = self._campaign()
        self.assertEqual(campaign.units[0]["stop"], 94.0)
        self.assertEqual(campaign.open_risk(), 50 * 6.0)

    def test_exit_level_is_the_higher_of_the_stop_and_the_20_day_low(self):
        # [30 p.7-8]: exit on a break of the 4-week low; the initial stop
        # stays in force below it. This is the theoretical level main.py
        # aims its Exit Order at, before it is placed at a raw tick.
        campaign = self._campaign()
        self.assertEqual(campaign.exit_level(0, 90.0), 94.0)
        self.assertEqual(campaign.exit_level(0, 97.0), 97.0)

    def test_open_risk_and_the_risk_free_check_use_the_resting_stop(self):
        # README.md, "Deviations": the resting Exit Order can sit a raw tick
        # below the theoretical exit_level (a bar-low adjustment). open_risk
        # and add_ready's risk-free check must read the actual resting
        # price, never the higher theoretical one.
        campaign = self._campaign()
        campaign.set_resting_stop(0, 99.99)   # exit_level would read 100.0
        self.assertAlmostEqual(campaign.open_risk(), 50 * 0.01)
        self.assertFalse(campaign.add_ready(103.0))
        campaign.set_resting_stop(0, 100.0)
        self.assertEqual(campaign.open_risk(), 0.0)
        self.assertTrue(campaign.add_ready(102.0))

    def test_add_needs_risk_free_first_position_and_one_atr_profit(self):
        # [M p.56]: only when the first position has no remaining risk;
        # [V 01:20:45]: after the previous one has moved 1 ATR in profit.
        campaign = self._campaign()
        campaign.set_resting_stop(0, 99.0)
        self.assertFalse(campaign.add_ready(103.0))   # not risk-free
        campaign.set_resting_stop(0, 100.0)
        self.assertFalse(campaign.add_ready(101.9))  # risk-free, < 1 ATR
        self.assertTrue(campaign.add_ready(102.0))

    def test_each_add_is_separately_sized_and_stopped(self):
        # [M p.56]: each addition is a separately sized and stopped position.
        campaign = self._campaign()
        campaign.add_unit(103.0, 40, 2.5)
        self.assertEqual(campaign.units[1], {"fill_price": 103.0, "quantity": 40, "atr": 2.5,
                                             "stop": 95.5, "resting_stop": 95.5})
        self.assertEqual(campaign.units[0]["stop"], 94.0)
        campaign.set_resting_stop(0, 100.0)
        # The next add is measured from the newest position's fill.
        self.assertFalse(campaign.add_ready(105.4))
        self.assertTrue(campaign.add_ready(105.5))

    def test_positions_per_asset_are_capped(self):
        campaign = rules.Campaign("XYZ", 100.0, 50, 2.0, max_positions=2)
        campaign.add_unit(103.0, 40, 2.0)
        self.assertFalse(campaign.add_ready(200.0))

    def test_r_multiple_is_all_positions_over_the_first_positions_risk(self):
        campaign = self._campaign()
        campaign.add_unit(103.0, 40, 2.5)
        campaign.close_unit(1, 101.0)      # -2 x 40 = -80
        self.assertEqual(len(campaign.units), 1)
        campaign.close_unit(0, 106.0)      # +6 x 50 = +300
        self.assertAlmostEqual(campaign.r_multiple(), 220.0 / 300.0)

    def test_reduce_unit_realizes_the_fill_and_shrinks_the_position(self):
        # main.py "Deviations": a partial fill on a position's Exit Order,
        # then cancelled, realizes the shares that sold and reduces the
        # position's recorded quantity by exactly that many, so a
        # replacement stop is never sized for shares no longer held.
        campaign = self._campaign()
        campaign.reduce_unit(0, 20, 96.0)   # 20 of 50 shares sold at 96
        self.assertEqual(campaign.units[0]["quantity"], 30)
        self.assertAlmostEqual(campaign.realized, 20 * (96.0 - 100.0))


class RiskBudgetTests(unittest.TestCase):
    def _budget(self, **overrides):
        args = dict(equity=100_000.0, open_risk=0.0, cash=1_000_000.0)
        args.update(overrides)
        return rules.RiskBudget(**args)

    def test_daily_initiated_risk_ceiling(self):
        # [R]: risk initiated per day 4-8 %; the lower end is the default.
        budget = self._budget()
        for _ in range(4):
            self.assertEqual(budget.try_reserve(0.0, 1000.0, 1.0), (True, None))
        self.assertEqual(budget.try_reserve(0.0, 1000.0, 1.0), (False, "daily-risk"))

    def test_aggregate_open_risk_ceiling(self):
        # [R]: aggregate risk allocation 10-20 %; the lower end is the default.
        budget = self._budget(open_risk=9_500.0)
        self.assertEqual(budget.try_reserve(0.0, 600.0, 1.0), (False, "aggregate-risk"))
        self.assertEqual(budget.try_reserve(0.0, 500.0, 1.0), (True, None))

    def test_per_asset_risk_ceiling(self):
        # [M p.56]: never more than 2 % at risk on one asset.
        budget = self._budget()
        self.assertEqual(budget.try_reserve(1_500.0, 600.0, 1.0), (False, "asset-risk"))
        self.assertEqual(budget.try_reserve(1_500.0, 500.0, 1.0), (True, None))

    def test_cash_binds_last_and_is_spent(self):
        budget = self._budget(cash=1_000.0)
        self.assertEqual(budget.try_reserve(0.0, 100.0, 600.0), (True, None))
        self.assertEqual(budget.try_reserve(0.0, 100.0, 600.0), (False, "insufficient-cash"))

    def test_default_ceilings(self):
        self.assertEqual((rules.DAILY_RISK_CEILING, rules.AGGREGATE_RISK_CEILING,
                          rules.MAX_RISK_PER_ASSET), (0.04, 0.10, 0.02))


class IndicatorsTests(unittest.TestCase):
    def test_snapshot_reads_the_bars_before_the_one_advanced(self):
        ind = rules.Indicators()
        start = date(2024, 1, 1).toordinal()
        for i in range(60):
            ind.advance(date.fromordinal(start + i), 10.0 + i, 9.0 + i, 9.5 + i, 1000.0)
        snap = ind.snapshot(date.fromordinal(start + 60))
        self.assertEqual(snap.prior_55_high, 69.0)
        self.assertEqual(snap.prior_20_high, 69.0)
        self.assertEqual(snap.all_time_high, 69.0)
        self.assertEqual(ind.bars, 60)
        self.assertEqual(ind.exit_low(), 49.0)

    def test_a_bar_dated_on_or_before_the_last_is_ignored(self):
        ind = rules.Indicators()
        ind.advance(date(2024, 1, 2), 10.0, 9.0, 9.5, 1.0)
        ind.advance(date(2024, 1, 2), 99.0, 9.0, 9.5, 1.0)
        self.assertEqual(ind.bars, 1)
        self.assertEqual(ind.snapshot(date(2024, 1, 3)).all_time_high, 10.0)


class PriceViewTests(unittest.TestCase):
    """ADR 0004, as amended, carried from research/qc-cloud: raw shares, raw
    ticks, and per-share costs on raw shares."""

    def test_split_ratio_and_whole_raw_shares(self):
        self.assertEqual(rules.split_ratio(85.38, 1.524639198), 56)
        self.assertEqual(rules.ratio_after_split(56, 0.49999860000056007), 28)
        self.assertEqual(rules.whole_raw_shares(615898, 56), 615888)
        self.assertIsNone(rules.split_ratio(0.0, 1.0))

    def test_raw_ticks(self):
        self.assertAlmostEqual(rules.raw_tick_round(15.304, 1), 15.30, places=12)
        self.assertAlmostEqual(rules.raw_tick_floor(15.309 / 56, 56), 15.30 / 56, places=12)
        self.assertAlmostEqual(rules.above_by_a_tick(15.304 / 56, 56), 15.31 / 56, places=12)

    def test_exit_rests_below_the_bar_it_was_decided_on(self):
        # The level is floored to a raw tick and kept a tick below the
        # decision bar's low, so an order amended after a bar can never be
        # filled by that same bar.
        self.assertAlmostEqual(rules.exit_stop_price(10.0, 10.0, 1), 9.99, places=12)
        self.assertAlmostEqual(rules.exit_stop_price(9.456, 10.0, 1), 9.45, places=12)

    def test_lean_commission_on_raw_shares(self):
        self.assertAlmostEqual(rules.lean_ib_commission(615888, 0.30, 56), 54.99)
        self.assertAlmostEqual(rules.lean_ib_commission(50, 20.0, 1), 1.0)
        self.assertAlmostEqual(rules.lean_ib_commission(10000, 0.50, 1), 25.0)

    def test_dividend_on_raw_shares(self):
        self.assertAlmostEqual(rules.dividend_cash(24808, 2.65, 620.0, 620.0 / 28), 2347.9)
        self.assertEqual(rules.dividend_cash(0, 2.65, 620.0, 22.0), 0.0)

    def test_price_cap_and_slippage_in_atr(self):
        # ADR 0005 and ADR 0013 conventions, carried over in ATR.
        self.assertEqual(rules.price_cap(10.0, 2.0), 12.0)
        self.assertAlmostEqual(rules.slippage(2.0), 0.1)

    def test_commission_estimate_and_worst_case_cost(self):
        self.assertAlmostEqual(rules.commission_estimate(100, 10.0), 1.0)
        self.assertAlmostEqual(rules.worst_case_cost(100, 12.0, 0.1, 1.0), 1211.0)


class MetricTests(unittest.TestCase):
    def test_metrics(self):
        self.assertAlmostEqual(rules.annualised_return(100.0, 200.0, 365.25), 1.0)
        self.assertIsNone(rules.annualised_return(100.0, 200.0, 0))
        self.assertAlmostEqual(rules.max_drawdown([100.0, 120.0, 90.0, 110.0]), 0.25)
        self.assertIsNone(rules.max_drawdown([]))
        self.assertIsNone(rules.cagr_over_max_drawdown(0.1, 0.0))
        self.assertAlmostEqual(rules.cagr_over_max_drawdown(0.2, 0.1), 2.0)

    def test_eligibility_month(self):
        self.assertTrue(rules.is_new_eligibility_month(None, date(2024, 1, 2)))
        self.assertFalse(rules.is_new_eligibility_month(date(2024, 1, 2), date(2024, 1, 3)))
        self.assertTrue(rules.is_new_eligibility_month(date(2024, 1, 31), date(2024, 2, 1)))


if __name__ == "__main__":
    unittest.main()
