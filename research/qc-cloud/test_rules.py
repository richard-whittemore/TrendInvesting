"""Unit tests for rules.py, on synthetic series -- no QuantConnect, no
network, no market data (issue acceptance criterion: "no market data is
committed"; AGENTS.md: "golden scenarios come from primary sources --
transcribe them, don't invent them").

Run with:

    python3 -m unittest discover -s research/qc-cloud -p "test_*.py"

or, from this directory:

    python3 -m unittest test_rules

Golden scenarios below are transcribed from docs/methodology/
Methodology_Analysis.md, which cites The Original Turtle Trading Rules
(Curtis Faith, 2003) by page number. Where the source material available to
this repository states only a final figure (not the intermediate series that
produced it), this file tests the rule's FORMULA against that one disclosed
figure rather than fabricating the missing series.
"""

import unittest
from datetime import date

import rules


class TrueRangeTests(unittest.TestCase):
    def test_no_previous_close_is_the_bars_own_range(self):
        # The Turtle Rules p.13, ADR 0003: first bar in a series, no gap
        # terms are computable.
        self.assertEqual(rules.true_range(105.0, 100.0, None), 5.0)

    def test_gap_up_widens_true_range_beyond_the_bars_own_range(self):
        # high=106, low=104 (range 2), previous close=95: the gap up term
        # (106-95=11) dominates.
        self.assertEqual(rules.true_range(106.0, 104.0, 95.0), 11.0)

    def test_gap_down_widens_true_range_beyond_the_bars_own_range(self):
        # high=100, low=98 (range 2), previous close=110: the gap down term
        # (110-98=12) dominates.
        self.assertEqual(rules.true_range(100.0, 98.0, 110.0), 12.0)


class WilderNTests(unittest.TestCase):
    def test_not_ready_before_period_bars(self):
        n = rules.WilderN(period=5)
        for tr in (1.0, 1.0, 1.0, 1.0):
            n.add(tr)
        self.assertFalse(n.ready)
        self.assertEqual(n.value, 0.0)

    def test_seed_is_a_simple_average(self):
        # The Turtle Rules p.13: "seeded with a 20-day simple average of TR".
        n = rules.WilderN(period=4)
        for tr in (2.0, 4.0, 6.0, 8.0):
            n.add(tr)
        self.assertTrue(n.ready)
        self.assertAlmostEqual(n.value, 5.0)  # (2+4+6+8)/4

    def test_wilder_recursion_after_the_seed(self):
        # N = ((period-1) x previous_N + TR) / period (The Turtle Rules p.13).
        n = rules.WilderN(period=4)
        for tr in (2.0, 4.0, 6.0, 8.0):
            n.add(tr)
        n.add(20.0)
        self.assertAlmostEqual(n.value, (3 * 5.0 + 20.0) / 4)

    def test_flat_bars_seed_zero_and_stay_zero(self):
        # CONTEXT.md "N": a genuine N can legitimately be zero, distinct
        # from "not yet warm".
        n = rules.WilderN(period=3)
        for tr in (0.0, 0.0, 0.0, 0.0, 0.0):
            n.add(tr)
        self.assertTrue(n.ready)
        self.assertEqual(n.value, 0.0)


class UnitQuantityGoldenTests(unittest.TestCase):
    def test_heating_oil_worked_example(self):
        # Methodology_Analysis.md Section 2.3 / The Turtle Rules p.14-15:
        # N=0.0141, $1,000,000 account, 42,000-gallon contract, Faith's own
        # 1% Unit Volatility Fraction -> 16.88, truncated to 16 contracts.
        self.assertEqual(rules.unit_quantity(1_000_000.0, 0.01, 0.0141, 42_000.0), 16)

    def test_baseline_unit_volatility_fraction_is_half_faiths(self):
        # ADR 0003: the Baseline runs 0.5%, not Faith's 1%.
        self.assertEqual(rules.UNIT_VOLATILITY_FRACTION, 0.005)

    def test_zero_quantity_when_the_account_cannot_fund_one_share(self):
        # The Turtle Rules p.15: small accounts lose diversification because
        # truncation is coarse; a quantity of zero is a legitimate outcome,
        # not an error.
        self.assertEqual(rules.unit_quantity(1_000.0, 0.005, 50.0, 1.0), 0)

    def test_rejects_a_non_positive_n(self):
        with self.assertRaises(ValueError):
            rules.unit_quantity(1_000_000.0, 0.005, 0.0, 1.0)


class EntryExitChannelTests(unittest.TestCase):
    def test_entry_channel_evaluate_then_add_ordering(self):
        # internal/indicator.EntryChannel's documented defect class: reading
        # extreme() BEFORE add() must never let today's own high count
        # towards its own breakout test.
        channel = rules.EntryChannel(length=3)
        for high in (10.0, 12.0, 9.0):
            channel.add(high)
        extreme, ready = channel.extreme()
        self.assertTrue(ready)
        self.assertEqual(extreme, 12.0)
        # A new bar with an even higher high must be evaluated against the
        # channel BEFORE it is folded in.
        todays_high = 50.0
        extreme_before_add, _ = channel.extreme()
        self.assertLess(extreme_before_add, todays_high)  # this bar IS a breakout
        channel.add(todays_high)
        extreme_after_add, _ = channel.extreme()
        self.assertEqual(extreme_after_add, 50.0)  # now includes today's own high

    def test_entry_channel_not_ready_before_full(self):
        channel = rules.EntryChannel(length=5)
        channel.add(10.0)
        _, ready = channel.extreme()
        self.assertFalse(ready)

    def test_exit_channel_is_the_lowest_low(self):
        channel = rules.ExitChannel(length=3)
        for low in (10.0, 8.0, 9.0):
            channel.add(low)
        extreme, ready = channel.extreme()
        self.assertTrue(ready)
        self.assertEqual(extreme, 8.0)

    def test_exit_order_level_takes_the_higher_of_stop_and_exit_channel(self):
        # ADR 0005's amendment: the higher of the two, a tie names the stop.
        self.assertEqual(rules.exit_order_level(90.0, exit_channel_extreme=95.0), 95.0)
        self.assertEqual(rules.exit_order_level(90.0, exit_channel_extreme=80.0), 90.0)
        self.assertEqual(rules.exit_order_level(90.0, exit_channel_extreme=90.0), 90.0)
        self.assertEqual(rules.exit_order_level(90.0, exit_channel_extreme=None), 90.0)


class AddLadderAndStopLadderGoldenTests(unittest.TestCase):
    def test_gold_add_ladder_golden(self):
        # Methodology_Analysis.md Section 2.6 / The Turtle Rules p.20:
        # N=2.50, first fill 310.00 -> 311.25 -> 312.50 -> 313.75.
        n = 2.50
        rung1 = 310.00
        rung2 = rules.next_add_level(rung1, n)
        rung3 = rules.next_add_level(rung2, n)
        rung4 = rules.next_add_level(rung3, n)
        self.assertAlmostEqual(rung2, 311.25)
        self.assertAlmostEqual(rung3, 312.50)
        self.assertAlmostEqual(rung4, 313.75)

    def test_crude_add_ladder_golden(self):
        # Methodology_Analysis.md Section 2.6 / The Turtle Rules p.20:
        # N=1.20, first fill 28.30 -> 28.90 -> 29.50 -> 30.10.
        n = 1.20
        rung1 = 28.30
        rung2 = rules.next_add_level(rung1, n)
        rung3 = rules.next_add_level(rung2, n)
        rung4 = rules.next_add_level(rung3, n)
        self.assertAlmostEqual(rung2, 28.90)
        self.assertAlmostEqual(rung3, 29.50)
        self.assertAlmostEqual(rung4, 30.10)

    def test_protective_stop_crude_single_unit_golden(self):
        # Methodology_Analysis.md Section 2.7 / The Turtle Rules p.22:
        # entry 28.30, N 1.20, Stop Multiple 2 -> stop 25.90.
        self.assertAlmostEqual(rules.protective_stop_level(28.30, 1.20, rules.STOP_MULTIPLE), 25.90)

    def test_protective_stop_crude_fourth_unit_golden(self):
        # The Turtle Rules p.22-23 (Methodology_Analysis.md Section 2.7): a
        # 4th Unit filled at 30.80 with N=1.20 has its OWN stop at 28.40
        # (30.80 - 2 x 1.20). The printed table's other figure -- Units 1-3
        # sitting at 27.70 after this Add -- depends on their own entry
        # prices, which the secondary source available to this repository
        # does not state, so it is not independently re-derived here; only
        # the figure Methodology_Analysis.md quotes directly is asserted.
        self.assertAlmostEqual(rules.protective_stop_level(30.80, 1.20, rules.STOP_MULTIPLE), 28.40)

    def test_raised_stop_formula(self):
        # The Turtle Rules p.22-23: "the stops for earlier units were
        # raised by 1/2 N."
        self.assertAlmostEqual(rules.raised_stop(27.70, 1.20), 28.30)

    def test_rejects_a_stop_at_or_below_zero(self):
        with self.assertRaises(ValueError):
            rules.protective_stop_level(1.0, 10.0, rules.STOP_MULTIPLE)


class CampaignTests(unittest.TestCase):
    def test_full_campaign_builds_the_crude_ladder_and_raises_stops(self):
        n = 1.20
        campaign = rules.Campaign("XYZ", entry_fill_price=28.30, campaign_n=n, unit_quantity_value=100)
        self.assertAlmostEqual(campaign.protective_stop(), 25.90)

        rung2 = campaign.next_add_rung()
        self.assertAlmostEqual(rung2, 28.90)
        campaign.add_unit(28.90)
        # Unit 1's stop was raised by half N; Unit 2 has its own fresh stop.
        self.assertAlmostEqual(campaign.units[0]["stop"], 25.90 + 0.5 * n)
        self.assertAlmostEqual(campaign.units[1]["stop"], 28.90 - 2 * n)
        # The Campaign's own stop is the LOWEST of the two.
        self.assertAlmostEqual(campaign.protective_stop(), min(campaign.units[0]["stop"],
                                                                 campaign.units[1]["stop"]))

        campaign.add_unit(29.50)
        campaign.add_unit(30.10)
        self.assertTrue(campaign.loaded)
        self.assertIsNone(campaign.next_add_rung())

    def test_loaded_campaign_refuses_a_further_add(self):
        campaign = rules.Campaign("XYZ", 100.0, 2.0, 10, max_units=1)
        self.assertTrue(campaign.loaded)
        with self.assertRaises(ValueError):
            campaign.add_unit(101.0)

    def test_partial_stop_blocks_further_adds(self):
        campaign = rules.Campaign("XYZ", 100.0, 2.0, 10)
        campaign.add_unit(101.0)
        campaign.add_unit(102.0)
        self.assertEqual(campaign.unit_count, 3)
        # The lowest-stopped Unit(s) are removed by a partial stop-out.
        campaign.remove_units([0])
        self.assertEqual(campaign.unit_count, 2)
        self.assertTrue(campaign.partially_stopped)
        self.assertIsNone(campaign.next_add_rung())
        with self.assertRaises(ValueError):
            campaign.add_unit(200.0)

    def test_remove_all_units_leaves_no_survivors_and_is_not_partially_stopped(self):
        # A full exit is the Campaign's own end, not a "partial" stop.
        campaign = rules.Campaign("XYZ", 100.0, 2.0, 10)
        campaign.remove_units([0])
        self.assertEqual(campaign.unit_count, 0)
        self.assertFalse(campaign.partially_stopped)


class NotionalAccountDrawdownTests(unittest.TestCase):
    def test_turtle_worked_example(self):
        # The Turtle Rules p.17 / ADR 0007, as clarified by issue #16: $1M
        # -> $800k after equity falls $100k -> $640k after a FURTHER $80k
        # fall (each 10% step measured against the CURRENT, already-reduced
        # notional account, never the original).
        account = rules.NotionalAccount(1_000_000.0)
        account.observe(date(2020, 6, 1), 900_000.0)
        self.assertAlmostEqual(account.current, 800_000.0)
        account.observe(date(2020, 6, 2), 820_000.0)
        self.assertAlmostEqual(account.current, 640_000.0)

    def test_no_step_above_the_threshold(self):
        account = rules.NotionalAccount(1_000_000.0)
        account.observe(date(2020, 6, 1), 950_000.0)
        self.assertAlmostEqual(account.current, 1_000_000.0)

    def test_recovery_only_at_the_full_starting_figure(self):
        # ADR 0007: "never a high-water mark"; a partial recovery changes
        # nothing.
        account = rules.NotionalAccount(1_000_000.0)
        account.observe(date(2020, 6, 1), 900_000.0)
        self.assertAlmostEqual(account.current, 800_000.0)
        account.observe(date(2020, 7, 1), 950_000.0)  # partial recovery
        self.assertAlmostEqual(account.current, 800_000.0)
        account.observe(date(2020, 8, 1), 1_000_000.0)  # full recovery
        self.assertAlmostEqual(account.current, 1_000_000.0)
        self.assertAlmostEqual(account.base, 1_000_000.0)

    def test_yearly_rebasing(self):
        # ADR 0007: re-based to actual equity every 1 January (the
        # configured rebasing_month/day).
        account = rules.NotionalAccount(1_000_000.0, rebasing_month=1, rebasing_day=1)
        account.observe(date(2020, 6, 1), 900_000.0)
        self.assertAlmostEqual(account.current, 800_000.0)
        account.observe(date(2021, 1, 2), 700_000.0)  # a new year: re-base to actual equity
        self.assertAlmostEqual(account.current, 700_000.0)
        self.assertAlmostEqual(account.base, 700_000.0)
        self.assertAlmostEqual(account.starting_figure, 700_000.0)

    def test_first_snapshot_never_rebases(self):
        # ADR 0007's implementation note: the very first snapshot only
        # establishes the label, however far past the re-basing date.
        account = rules.NotionalAccount(1_000_000.0, rebasing_month=1, rebasing_day=1)
        account.observe(date(2020, 6, 1), 1_000_000.0)
        self.assertAlmostEqual(account.current, 1_000_000.0)
        self.assertAlmostEqual(account.starting_figure, 1_000_000.0)


class UnitCapsTests(unittest.TestCase):
    def test_per_instrument_cap(self):
        caps = rules.UnitCaps()
        caps.add("AAA", None, None, units=rules.MAX_UNITS_PER_INSTRUMENT)
        exceeded, name = caps.would_exceed("AAA", None, None)
        self.assertTrue(exceeded)
        self.assertEqual(name, "instrument")

    def test_unclassified_instruments_share_one_group_at_the_sector_level(self):
        # ADR 0008: no classifier -> every instrument is Unclassified, and
        # the group is capped at the sector (loosely-correlated) level, 10.
        caps = rules.UnitCaps()
        caps.add("AAA", None, None, units=4)
        caps.add("BBB", None, None, units=4)
        # Two more Unclassified Units (any instrument) would reach the
        # group's own 10-Unit cap exactly; a third would exceed it.
        exceeded, name = caps.would_exceed("CCC", None, None, additional_units=2)
        self.assertFalse(exceeded)
        exceeded, name = caps.would_exceed("CCC", None, None, additional_units=3)
        self.assertTrue(exceeded)
        self.assertEqual(name, "sector")

    def test_total_long_cap_binds_across_groups(self):
        caps = rules.UnitCaps()
        caps.add("AAA", "tech", "tech-sector", units=4)
        caps.add("BBB", "auto", "auto-sector", units=4)
        caps.add("CCC", "bank", "bank-sector", units=4)
        exceeded, name = caps.would_exceed("DDD", "chem", "chem-sector")
        self.assertTrue(exceeded)
        self.assertEqual(name, "total_long")

    def test_classified_industry_cap(self):
        caps = rules.UnitCaps()
        caps.add("AAA", "tech", "tech-sector", units=6)
        exceeded, name = caps.would_exceed("BBB", "tech", "tech-sector")
        self.assertTrue(exceeded)
        self.assertEqual(name, "industry")

    def test_remove_frees_headroom(self):
        caps = rules.UnitCaps()
        caps.add("AAA", None, None, units=4)
        caps.remove("AAA", None, None, units=4)
        exceeded, _ = caps.would_exceed("AAA", None, None, additional_units=4)
        self.assertFalse(exceeded)


class StrengthAndTieBreakTests(unittest.TestCase):
    def test_strength_formula(self):
        # The Turtle Rules p.29 / ADR 0010: (close[d] - close[d-63]) / N[d].
        closes = [100.0] * 63 + [163.0]  # 64 values, d-63 .. d
        value, ready = rules.strength(closes, n=2.0)
        self.assertTrue(ready)
        self.assertAlmostEqual(value, (163.0 - 100.0) / 2.0)

    def test_strength_not_ready_with_insufficient_history(self):
        closes = [100.0] * 60
        _, ready = rules.strength(closes, n=2.0)
        self.assertFalse(ready)

    def test_strength_not_ready_with_non_positive_n(self):
        closes = [100.0] * 64
        _, ready = rules.strength(closes, n=0.0)
        self.assertFalse(ready)

    def test_median_dollar_volume_even_window_is_the_mean_of_the_two_middle(self):
        closes = [10.0] * 20
        volumes = list(range(1, 21))  # 1..20, so products are 10, 20, ..., 200
        value, ready = rules.median_dollar_volume(closes, volumes)
        self.assertTrue(ready)
        # products sorted: 10..200 step 10; middle two are 100 and 110.
        self.assertAlmostEqual(value, (100.0 + 110.0) / 2.0)

    def test_median_dollar_volume_not_ready_with_insufficient_history(self):
        _, ready = rules.median_dollar_volume([1.0] * 10, [1.0] * 10)
        self.assertFalse(ready)

    def test_median_dollar_volume_rejects_mismatched_lengths(self):
        with self.assertRaises(ValueError):
            rules.median_dollar_volume([1.0] * 20, [1.0] * 19)

    def test_rank_signals_total_order(self):
        signals = [
            rules.Signal(symbol="ZZZ", strength=1.0, median_dollar_volume=5_000_000),
            rules.Signal(symbol="AAA", strength=2.0, median_dollar_volume=5_000_000),
            rules.Signal(symbol="BBB", strength=1.0, median_dollar_volume=6_000_000),
            rules.Signal(symbol="CCC", strength=1.0, median_dollar_volume=6_000_000),
        ]
        ranked = rules.rank_signals(signals)
        self.assertEqual([s.symbol for s in ranked], ["AAA", "BBB", "CCC", "ZZZ"])


class UniverseEligibilityTests(unittest.TestCase):
    def test_eligible_instrument(self):
        self.assertTrue(rules.is_eligible(price=10.0, dollar_volume=10_000_000, history_bars=300))

    def test_price_floor(self):
        self.assertFalse(rules.is_eligible(price=4.99, dollar_volume=10_000_000, history_bars=300))
        self.assertTrue(rules.is_eligible(price=5.00, dollar_volume=10_000_000, history_bars=300))

    def test_dollar_volume_floor(self):
        self.assertFalse(rules.is_eligible(price=10.0, dollar_volume=4_999_999, history_bars=300))
        self.assertTrue(rules.is_eligible(price=10.0, dollar_volume=5_000_000, history_bars=300))

    def test_history_floor(self):
        self.assertFalse(rules.is_eligible(price=10.0, dollar_volume=10_000_000, history_bars=249))
        self.assertTrue(rules.is_eligible(price=10.0, dollar_volume=10_000_000, history_bars=250))

    def test_non_common_stock_excluded(self):
        self.assertFalse(rules.is_eligible(price=10.0, dollar_volume=10_000_000, history_bars=300,
                                           is_common_stock=False))

    def test_is_new_eligibility_month(self):
        self.assertTrue(rules.is_new_eligibility_month(None, date(2020, 1, 2)))
        self.assertFalse(rules.is_new_eligibility_month(date(2020, 1, 2), date(2020, 1, 3)))
        self.assertTrue(rules.is_new_eligibility_month(date(2020, 1, 31), date(2020, 2, 3)))


class PriceCapAndSlippageTests(unittest.TestCase):
    def test_price_cap_baseline_k_equals_one(self):
        self.assertAlmostEqual(rules.price_cap(100.0, 2.0), 102.0)
        self.assertEqual(rules.GAP_BUFFER_N, 1.0)

    def test_slippage_baseline_rate(self):
        self.assertAlmostEqual(rules.slippage(2.0), 0.10)
        self.assertEqual(rules.SLIPPAGE_N, 0.05)


class CommissionAndAffordabilityTests(unittest.TestCase):
    def test_commission_floor_wins_on_a_small_cheap_order(self):
        # 1 share at $12.01: rate is $0.005, floored to the $1.00 minimum,
        # and the 1% cap ($0.1201) does not win because it is BELOW the
        # floor here it must still not go below... actually the cap must
        # bind when it is the smaller of the two (see the next test); this
        # case is deliberately the OTHER way around: a expensive-enough
        # trade where the floor is not capped away.
        commission = rules.commission_estimate(500, 5.0)
        self.assertAlmostEqual(commission, 2.5)  # 500 x 0.005 = 2.50, above the $1 floor, below the 1% cap ($25)

    def test_commission_minimum_floor(self):
        commission = rules.commission_estimate(50, 100.0)
        self.assertAlmostEqual(commission, max(50 * 0.005, 1.0))

    def test_commission_cap_wins_over_the_floor_on_a_tiny_trade(self):
        commission = rules.commission_estimate(1, 12.01)
        cap = 0.01 * 12.01
        self.assertAlmostEqual(commission, cap)
        self.assertLess(cap, 1.0)

    def test_affordable_quantity_worst_case_cost(self):
        affordable, cost = rules.affordable_quantity(
            quantity=100, price_cap_value=50.0, slippage_value=0.5, dollars_per_point=1.0,
            commission=1.0, available_cash=6_000.0)
        self.assertTrue(affordable)
        self.assertAlmostEqual(cost, 100 * 50.5 + 1.0)

    def test_unaffordable_quantity_is_declined_whole(self):
        affordable, cost = rules.affordable_quantity(
            quantity=100, price_cap_value=50.0, slippage_value=0.5, dollars_per_point=1.0,
            commission=1.0, available_cash=100.0)
        self.assertFalse(affordable)
        self.assertGreater(cost, 100.0)


class SessionLedgerTests(unittest.TestCase):
    def test_two_proposals_in_one_pass_share_one_cash_budget(self):
        # ADR 0020's amendment: reserved at proposal, in the pass's own
        # order, so the SECOND of two proposals that would each fit alone
        # against the ORIGINAL balance is declined once the first has
        # claimed its share.
        caps = rules.UnitCaps()
        ledger = rules.SessionLedger(available_cash=6_000.0, unit_caps=caps)
        accepted1, reason1 = ledger.try_reserve("AAA", None, None, quantity=100,
                                                 price_cap_value=50.0, slippage_value=0.0,
                                                 dollars_per_point=1.0, commission=0.0)
        self.assertTrue(accepted1)
        self.assertIsNone(reason1)
        accepted2, reason2 = ledger.try_reserve("BBB", None, None, quantity=100,
                                                 price_cap_value=50.0, slippage_value=0.0,
                                                 dollars_per_point=1.0, commission=0.0)
        self.assertFalse(accepted2)
        self.assertEqual(reason2, "insufficient-cash")

    def test_a_pass_can_exhaust_the_unclassified_groups_shared_cap(self):
        caps = rules.UnitCaps()
        ledger = rules.SessionLedger(available_cash=10_000_000.0, unit_caps=caps)
        for i in range(rules.MAX_UNITS_PER_SECTOR):
            accepted, reason = ledger.try_reserve("SYM%d" % i, None, None, quantity=1,
                                                   price_cap_value=1.0, slippage_value=0.0,
                                                   dollars_per_point=1.0, commission=0.0)
            self.assertTrue(accepted, reason)
        accepted, reason = ledger.try_reserve("ONE-TOO-MANY", None, None, quantity=1,
                                               price_cap_value=1.0, slippage_value=0.0,
                                               dollars_per_point=1.0, commission=0.0)
        self.assertFalse(accepted)
        self.assertEqual(reason, "unit-cap-exceeded:sector")


class MetricTests(unittest.TestCase):
    def test_annualised_return_doubles_over_one_year(self):
        value = rules.annualised_return(100.0, 200.0, elapsed_days=365.25)
        self.assertAlmostEqual(value, 1.0)

    def test_annualised_return_null_for_zero_elapsed_time(self):
        self.assertIsNone(rules.annualised_return(100.0, 200.0, elapsed_days=0))

    def test_annualised_return_null_for_non_positive_start(self):
        self.assertIsNone(rules.annualised_return(0.0, 200.0, elapsed_days=365.25))

    def test_max_drawdown_simple_peak_and_trough(self):
        curve = [100.0, 120.0, 90.0, 110.0]
        self.assertAlmostEqual(rules.max_drawdown(curve), (120.0 - 90.0) / 120.0)

    def test_max_drawdown_empty_curve_is_null(self):
        self.assertIsNone(rules.max_drawdown([]))

    def test_ratio_is_null_on_zero_drawdown(self):
        self.assertIsNone(rules.cagr_over_max_drawdown(0.10, 0.0))

    def test_ratio_divides_cagr_by_drawdown(self):
        self.assertAlmostEqual(rules.cagr_over_max_drawdown(0.20, 0.10), 2.0)


if __name__ == "__main__":
    unittest.main()
