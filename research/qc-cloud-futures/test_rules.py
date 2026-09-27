"""Unit tests for rules.py -- Faith's original Turtle rules, applied to
futures, long AND short (docs/methodology/Methodology_Analysis.md; The
Original Turtle Trading Rules, Curtis Faith, 2003). No QuantConnect, no
network, no market data (AGENTS.md: "golden scenarios come from primary
sources -- transcribe them, don't invent them").

Run with:

    python3 -m unittest discover -s research/qc-cloud-futures -p "test_*.py"

or via `make research-test` from the repository root.

The golden scenarios below are transcribed from Methodology_Analysis.md,
which cites The Original Turtle Trading Rules by page number: the Heating
Oil Unit-sizing example [T p.14-15], and the Gold and Crude Add ladders
[T p.20]. This module is research-only and carries no claim of bit-for-bit
parity with the production Go engine.
"""

import unittest
from datetime import date

import rules


class TrueRangeTests(unittest.TestCase):
    def test_no_previous_close_is_the_bars_own_range(self):
        # The Turtle Rules p.13: first bar in a series, no gap terms exist.
        self.assertEqual(rules.true_range(105.0, 100.0, None), 5.0)

    def test_gap_widens_true_range_beyond_the_bars_own_range(self):
        self.assertEqual(rules.true_range(106.0, 104.0, 95.0), 11.0)


class WilderNTests(unittest.TestCase):
    def test_not_ready_before_period_bars(self):
        n = rules.WilderN(period=5)
        for tr in (1.0, 1.0, 1.0, 1.0):
            n.add(tr)
        self.assertFalse(n.ready)

    def test_seed_is_a_simple_average(self):
        # The Turtle Rules p.13: "seeded with a 20-day simple average of TR".
        n = rules.WilderN(period=4)
        for tr in (2.0, 4.0, 6.0, 8.0):
            n.add(tr)
        self.assertTrue(n.ready)
        self.assertAlmostEqual(n.value, 5.0)

    def test_wilder_recursion_after_the_seed(self):
        # The Turtle Rules p.13: N = (19 x previous_N + TR) / 20.
        n = rules.WilderN(period=4)
        for tr in (2.0, 4.0, 6.0, 8.0):
            n.add(tr)
        n.add(20.0)
        self.assertAlmostEqual(n.value, (3 * 5.0 + 20.0) / 4)


class ChannelTests(unittest.TestCase):
    """System 2's 55-day Entry Channel and 20-day Exit Channel (The Turtle
    Rules p.18-19, p.26), symmetric for long and short: one Channel tracks
    both the rolling high and the rolling low, and ``extreme(direction)``
    reads whichever side a breakout (or, for the Exit Channel, a reversal)
    in that direction needs -- the high for a long, the low for a short."""

    def test_not_ready_before_full(self):
        channel = rules.Channel(length=3)
        channel.add(high=10.0, low=9.0)
        _, ready = channel.extreme(direction=1)
        self.assertFalse(ready)

    def test_long_extreme_is_the_rolling_high(self):
        channel = rules.Channel(length=3)
        for high, low in ((10.0, 8.0), (12.0, 9.0), (9.0, 7.0)):
            channel.add(high, low)
        value, ready = channel.extreme(direction=1)
        self.assertTrue(ready)
        self.assertEqual(value, 12.0)

    def test_short_extreme_is_the_rolling_low(self):
        channel = rules.Channel(length=3)
        for high, low in ((10.0, 8.0), (12.0, 9.0), (9.0, 7.0)):
            channel.add(high, low)
        value, ready = channel.extreme(direction=-1)
        self.assertTrue(ready)
        self.assertEqual(value, 7.0)

    def test_evaluate_then_add_ordering(self):
        # Mirrors internal/indicator's documented "evaluate before add"
        # discipline: today's own bar must never count towards its own
        # breakout test.
        channel = rules.Channel(length=3)
        for high, low in ((10.0, 9.0), (12.0, 10.0), (9.0, 8.0)):
            channel.add(high, low)
        extreme_before, _ = channel.extreme(direction=1)
        self.assertLess(extreme_before, 50.0)  # today's high (50) IS a breakout
        channel.add(50.0, 40.0)
        extreme_after, _ = channel.extreme(direction=1)
        self.assertEqual(extreme_after, 50.0)  # now includes today's own high


class IsBreakoutTests(unittest.TestCase):
    def test_long_breakout_is_a_strict_high_above_the_channel(self):
        self.assertTrue(rules.is_breakout(1, bar_high=101.0, bar_low=99.0, level=100.0))
        self.assertFalse(rules.is_breakout(1, bar_high=100.0, bar_low=99.0, level=100.0))

    def test_short_breakout_is_a_strict_low_below_the_channel(self):
        self.assertTrue(rules.is_breakout(-1, bar_high=101.0, bar_low=99.0, level=100.0))
        self.assertFalse(rules.is_breakout(-1, bar_high=101.0, bar_low=100.0, level=100.0))


class ExitChannelBreachTests(unittest.TestCase):
    def test_long_exit_breach_is_a_strict_low_below_the_channel(self):
        self.assertEqual(rules.exit_channel_breach(1, bar_high=105.0, bar_low=94.0,
                                                    exit_extreme=95.0, ready=True), 95.0)
        self.assertIsNone(rules.exit_channel_breach(1, bar_high=105.0, bar_low=96.0,
                                                     exit_extreme=95.0, ready=True))

    def test_short_exit_breach_is_a_strict_high_above_the_channel(self):
        self.assertEqual(rules.exit_channel_breach(-1, bar_high=106.0, bar_low=99.0,
                                                    exit_extreme=105.0, ready=True), 105.0)
        self.assertIsNone(rules.exit_channel_breach(-1, bar_high=104.0, bar_low=99.0,
                                                     exit_extreme=105.0, ready=True))

    def test_not_ready_never_breaches(self):
        self.assertIsNone(rules.exit_channel_breach(1, bar_high=10.0, bar_low=0.0,
                                                     exit_extreme=5.0, ready=False))


class UnitQuantityGoldenTests(unittest.TestCase):
    def test_heating_oil_worked_example(self):
        # Methodology_Analysis.md Section 2.3 / The Turtle Rules p.14-15:
        # N=0.0141, $1,000,000 account, 42,000-gallon contract, Faith's own
        # 1% Unit Volatility Fraction -> 16.88, truncated to 16 contracts.
        self.assertEqual(rules.unit_quantity(1_000_000.0, 0.01, 0.0141, 42_000.0), 16)

    def test_default_unit_volatility_fraction_is_faiths_one_percent(self):
        # ADR 0003 halves this (0.5%) for the long-only single-regime
        # equity Baseline; this module trades a diversified, symmetric
        # long/short futures book, the population Faith's 1% describes, so
        # the default here is Faith's own figure, not the equity halving.
        self.assertEqual(rules.UNIT_VOLATILITY_FRACTION, 0.01)

    def test_zero_quantity_when_the_account_cannot_fund_one_contract(self):
        # The Turtle Rules p.15: small accounts lose diversification
        # because truncation is coarse; zero is a legitimate outcome.
        self.assertEqual(rules.unit_quantity(1_000.0, 0.01, 50.0, 1.0), 0)

    def test_rejects_a_non_positive_n(self):
        with self.assertRaises(ValueError):
            rules.unit_quantity(1_000_000.0, 0.01, 0.0, 1.0)


class AddLadderGoldenTests(unittest.TestCase):
    def test_gold_add_ladder_golden_long(self):
        # Methodology_Analysis.md Section 2.6 / The Turtle Rules p.20:
        # N=2.50, first fill 310.00 -> 311.25 -> 312.50 -> 313.75.
        n = 2.50
        rung1 = 310.00
        rung2 = rules.next_add_level(rung1, n, direction=1)
        rung3 = rules.next_add_level(rung2, n, direction=1)
        rung4 = rules.next_add_level(rung3, n, direction=1)
        self.assertAlmostEqual(rung2, 311.25)
        self.assertAlmostEqual(rung3, 312.50)
        self.assertAlmostEqual(rung4, 313.75)

    def test_crude_add_ladder_golden_long(self):
        # Methodology_Analysis.md Section 2.6 / The Turtle Rules p.20:
        # N=1.20, first fill 28.30 -> 28.90 -> 29.50 -> 30.10.
        n = 1.20
        rung1 = 28.30
        rung2 = rules.next_add_level(rung1, n, direction=1)
        rung3 = rules.next_add_level(rung2, n, direction=1)
        rung4 = rules.next_add_level(rung3, n, direction=1)
        self.assertAlmostEqual(rung2, 28.90)
        self.assertAlmostEqual(rung3, 29.50)
        self.assertAlmostEqual(rung4, 30.10)

    def test_crude_add_ladder_golden_short_is_the_mirror_image(self):
        # Faith trades long and short identically, mirror-imaged [T
        # p.18-19]; this module's own symmetric generalisation of the
        # printed (long) Crude ladder: same N and spacing, subtracted.
        n = 1.20
        rung1 = 28.30
        rung2 = rules.next_add_level(rung1, n, direction=-1)
        rung3 = rules.next_add_level(rung2, n, direction=-1)
        self.assertAlmostEqual(rung2, 27.70)
        self.assertAlmostEqual(rung3, 27.10)


class ProtectiveStopGoldenTests(unittest.TestCase):
    def test_crude_single_unit_golden_long(self):
        # Methodology_Analysis.md Section 2.7 / The Turtle Rules p.22:
        # entry 28.30, N 1.20, Stop Multiple 2 -> stop 25.90.
        self.assertAlmostEqual(
            rules.protective_stop_level(28.30, 1.20, direction=1, stop_multiple=rules.STOP_MULTIPLE),
            25.90)

    def test_crude_fourth_unit_golden_long(self):
        # The Turtle Rules p.22-23: a 4th Unit filled at 30.80 with N=1.20
        # has its OWN stop at 28.40 (30.80 - 2 x 1.20). The printed table's
        # other figure (Units 1-3 at 27.70) depends on their own entry
        # prices, which the secondary source available to this repository
        # does not state, so only the figure Methodology_Analysis.md quotes
        # directly is asserted here (mirrors research/qc-cloud/test_rules.py).
        self.assertAlmostEqual(
            rules.protective_stop_level(30.80, 1.20, direction=1, stop_multiple=rules.STOP_MULTIPLE),
            28.40)

    def test_crude_single_unit_golden_short_is_the_mirror_image(self):
        self.assertAlmostEqual(
            rules.protective_stop_level(28.30, 1.20, direction=-1, stop_multiple=rules.STOP_MULTIPLE),
            30.70)

    def test_raised_stop_formula_long(self):
        # The Turtle Rules p.22-23: "the stops for earlier units were
        # raised by 1/2 N."
        self.assertAlmostEqual(rules.raised_stop(27.70, 1.20, direction=1), 28.30)

    def test_raised_stop_formula_short_is_lowered(self):
        self.assertAlmostEqual(rules.raised_stop(30.70, 1.20, direction=-1), 30.10)

    def test_back_adjusted_prices_may_be_zero_or_negative(self):
        # Levels live in the additive back-adjusted series, which runs
        # below zero for a contango market far enough back (HO and ZS in
        # 2007). Only the raw contract price must be positive, and main.py
        # checks that before every order.
        self.assertAlmostEqual(rules.protective_stop_level(-0.5, 0.1, direction=1), -0.7)
        self.assertAlmostEqual(rules.next_add_level(-0.5, 0.1, direction=1), -0.45)
        self.assertAlmostEqual(rules.raised_stop(-0.7, 0.1, direction=1), -0.65)
        campaign = rules.Campaign("HO", direction=-1, entry_fill_price=-0.2, campaign_n=0.1,
                                  unit_quantity_value=3, closely_group="g", loosely_group="l")
        self.assertAlmostEqual(campaign.protective_stop(), 0.0)

    def test_rejects_a_non_finite_price(self):
        with self.assertRaises(ValueError):
            rules.protective_stop_level(float("nan"), 1.0, direction=1)
        with self.assertRaises(ValueError):
            rules.next_add_level(float("inf"), 1.0, direction=1)
        with self.assertRaises(ValueError):
            rules.raised_stop(float("nan"), 1.0, direction=1)


class CampaignTests(unittest.TestCase):
    def test_long_campaign_builds_the_crude_ladder_and_raises_stops(self):
        n = 1.20
        campaign = rules.Campaign("CL", direction=1, entry_fill_price=28.30, campaign_n=n,
                                  unit_quantity_value=16, closely_group="energy-petroleum",
                                  loosely_group="energy")
        self.assertAlmostEqual(campaign.protective_stop(), 25.90)
        campaign.add_unit(28.90)
        self.assertAlmostEqual(campaign.units[0]["stop"], 26.50)  # raised by 0.5N
        self.assertAlmostEqual(campaign.units[1]["stop"], 26.50)
        campaign.add_unit(29.50)
        campaign.add_unit(30.10)
        self.assertTrue(campaign.loaded)
        self.assertIsNone(campaign.next_add_rung())

    def test_short_campaign_is_the_mirror_image(self):
        n = 1.20
        campaign = rules.Campaign("CL", direction=-1, entry_fill_price=28.30, campaign_n=n,
                                  unit_quantity_value=16, closely_group="energy-petroleum",
                                  loosely_group="energy")
        self.assertAlmostEqual(campaign.protective_stop(), 30.70)
        self.assertAlmostEqual(campaign.next_add_rung(), 27.70)
        campaign.add_unit(27.70)
        self.assertAlmostEqual(campaign.units[0]["stop"], 30.10)  # lowered by 0.5N

    def test_loaded_campaign_refuses_a_further_add(self):
        campaign = rules.Campaign("CL", direction=1, entry_fill_price=100.0, campaign_n=2.0,
                                  unit_quantity_value=10, closely_group="g", loosely_group="l")
        for fill in (100.0 + 2.0 * i * 0.5 for i in (1, 2, 3)):
            campaign.add_unit(fill)
        self.assertTrue(campaign.loaded)
        with self.assertRaises(ValueError):
            campaign.add_unit(999.0)

    def test_partial_stop_blocks_further_adds(self):
        campaign = rules.Campaign("CL", direction=1, entry_fill_price=100.0, campaign_n=2.0,
                                  unit_quantity_value=10, closely_group="g", loosely_group="l")
        campaign.add_unit(101.0)
        campaign.close_units([0], 96.0)
        self.assertTrue(campaign.partially_stopped)
        self.assertIsNone(campaign.next_add_rung())
        with self.assertRaises(ValueError):
            campaign.add_unit(999.0)

    def test_entry_price_stays_the_original_even_after_a_partial_stop(self):
        campaign = rules.Campaign("CL", direction=1, entry_fill_price=100.0, campaign_n=2.0,
                                  unit_quantity_value=10, closely_group="g", loosely_group="l")
        campaign.add_unit(101.0)
        campaign.close_units([0], 96.0)
        self.assertEqual(campaign.entry_price(), 100.0)

    def test_r_multiple_aggregates_every_unit_not_only_the_last_exit(self):
        campaign = rules.Campaign("CL", direction=1, entry_fill_price=100.0, campaign_n=2.0,
                                  unit_quantity_value=10, closely_group="g", loosely_group="l")
        campaign.add_unit(101.0)  # two units: fills 100, 101; stop_multiple*N = 4
        campaign.close_units([0], 96.0)   # unit 0 lost 4
        campaign.close_units([0], 105.0)  # surviving unit (index 0 now) gained 4
        self.assertEqual(campaign.units, [])
        self.assertAlmostEqual(campaign.r_multiple(), 0.0)  # net flat, not a "win"


class NotionalAccountDrawdownTests(unittest.TestCase):
    def test_turtle_worked_example(self):
        # The Turtle Rules p.17: $1M -> $800k after -$100k -> $640k after a
        # further -$80k (10% of the already-reduced $800k).
        from datetime import date
        account = rules.NotionalAccount(1_000_000.0)
        account.observe(date(2026, 1, 2), 900_000.0)
        self.assertAlmostEqual(account.current, 800_000.0)
        account.observe(date(2026, 1, 3), 820_000.0)
        self.assertAlmostEqual(account.current, 640_000.0)


class UnitCapsTests(unittest.TestCase):
    def test_per_market_cap(self):
        caps = rules.UnitCaps()
        for _ in range(4):
            caps.add("CL", "energy-petroleum", "energy", direction=1)
        exceeded, name = caps.would_exceed("CL", "energy-petroleum", "energy", direction=1)
        self.assertTrue(exceeded)
        self.assertEqual(name, "market")

    def test_closely_correlated_cap_binds_across_markets(self):
        # The Turtle Rules p.16: heating oil/crude are Faith's own
        # closely-correlated example.
        caps = rules.UnitCaps()
        caps.add("CL", "energy-petroleum", "energy", direction=1, units=3)
        caps.add("HO", "energy-petroleum", "energy", direction=1, units=3)
        exceeded, name = caps.would_exceed("CL", "energy-petroleum", "energy", direction=1)
        self.assertTrue(exceeded)
        self.assertEqual(name, "closely_correlated")

    def test_loosely_correlated_cap_binds_across_closely_groups(self):
        # Three markets, each its own closely-correlated group (so neither
        # the 4-per-market nor the 6-per-closely-group cap binds first),
        # sharing one loosely-correlated group at Faith's 10-Unit cap.
        caps = rules.UnitCaps()
        caps.add("A", "close-a", "loose", direction=1, units=4)
        caps.add("B", "close-b", "loose", direction=1, units=4)
        caps.add("C", "close-c", "loose", direction=1, units=2)
        exceeded, name = caps.would_exceed("C", "close-c", "loose", direction=1)
        self.assertTrue(exceeded)
        self.assertEqual(name, "loosely_correlated")

    def test_direction_cap_is_faiths_single_direction_twelve(self):
        caps = rules.UnitCaps()
        markets = [("ZN", "rates", "rates"), ("6E", "currencies-europe", "currencies"),
                   ("GC", "metals-precious", "metals")]
        for symbol, closely, loosely in markets:
            caps.add(symbol, closely, loosely, direction=1, units=4)
        exceeded, name = caps.would_exceed("ES", "equity_indices", "equity_indices", direction=1)
        self.assertTrue(exceeded)
        self.assertEqual(name, "direction")

    def test_long_and_short_caps_are_independent(self):
        # The Turtle Rules p.16: cap 4 is explicitly PER direction ("all
        # long or all short"), so a fully-Loaded long book leaves the short
        # side's headroom untouched.
        caps = rules.UnitCaps()
        caps.add("CL", "energy-petroleum", "energy", direction=1, units=4)
        exceeded, _ = caps.would_exceed("CL", "energy-petroleum", "energy", direction=-1)
        self.assertFalse(exceeded)

    def test_remove_frees_headroom(self):
        caps = rules.UnitCaps()
        caps.add("CL", "energy-petroleum", "energy", direction=1, units=4)
        caps.remove("CL", "energy-petroleum", "energy", direction=1, units=1)
        exceeded, _ = caps.would_exceed("CL", "energy-petroleum", "energy", direction=1)
        self.assertFalse(exceeded)


class SessionCapLedgerTests(unittest.TestCase):
    def test_two_proposals_in_one_pass_share_one_cap_budget(self):
        # ADR 0008's "same-pass headroom is reserved" rule, for the
        # direction cap here: two entries that would each fit the 12-per-
        # direction cap alone, decided in the same session-close pass,
        # must not both be accepted once together they would exceed it.
        caps = rules.UnitCaps(per_direction=1)
        ledger = rules.SessionCapLedger(caps)
        accepted1, reason1 = ledger.try_reserve("CL", "energy-petroleum", "energy", direction=1)
        accepted2, reason2 = ledger.try_reserve("GC", "metals-precious", "metals", direction=1)
        self.assertTrue(accepted1)
        self.assertIsNone(reason1)
        self.assertFalse(accepted2)
        self.assertEqual(reason2, "unit-cap-exceeded:direction")


class StrengthAndRankingTests(unittest.TestCase):
    def test_strength_formula(self):
        # The Turtle Rules p.29: (close - close 63 bars ago) / N.
        closes = [100.0] * 63 + [110.0]
        value, ready = rules.strength(closes, n=2.0)
        self.assertTrue(ready)
        self.assertAlmostEqual(value, 5.0)

    def test_strength_not_ready_with_insufficient_history(self):
        _, ready = rules.strength([100.0] * 10, n=2.0)
        self.assertFalse(ready)

    def test_rank_signals_long_is_strongest_first(self):
        # The Turtle Rules p.27-29: "buy the strongest ... within a
        # correlated group" on simultaneous signals.
        signals = [rules.Signal("A", 1.0), rules.Signal("B", 3.0), rules.Signal("C", 2.0)]
        ranked = rules.rank_signals(signals, direction=1)
        self.assertEqual([s.symbol for s in ranked], ["B", "C", "A"])

    def test_rank_signals_short_is_weakest_first(self):
        # The Turtle Rules p.27-29: "... sell the weakest": the most
        # negative Strength (weakest price action) goes first.
        signals = [rules.Signal("A", -1.0), rules.Signal("B", -3.0), rules.Signal("C", -2.0)]
        ranked = rules.rank_signals(signals, direction=-1)
        self.assertEqual([s.symbol for s in ranked], ["B", "C", "A"])


class PriceCapAndSlippageTests(unittest.TestCase):
    def test_price_cap_long_is_above_the_level(self):
        # ADR 0005's amendment, k=1 (GAP_BUFFER_N): level + k x N.
        self.assertAlmostEqual(rules.price_cap(100.0, 2.0, direction=1), 102.0)

    def test_price_cap_short_is_below_the_level(self):
        self.assertAlmostEqual(rules.price_cap(100.0, 2.0, direction=-1), 98.0)

    def test_slippage_baseline_rate(self):
        # ADR 0013: 0.05N.
        self.assertAlmostEqual(rules.slippage(2.0), 0.1)


class ExitOrderLevelTests(unittest.TestCase):
    def test_long_takes_the_higher_of_stop_and_exit_channel(self):
        self.assertEqual(rules.exit_order_level(1, unit_stop=90.0, exit_channel_extreme=95.0), 95.0)
        self.assertEqual(rules.exit_order_level(1, unit_stop=90.0, exit_channel_extreme=80.0), 90.0)

    def test_short_takes_the_lower_of_stop_and_exit_channel(self):
        self.assertEqual(rules.exit_order_level(-1, unit_stop=110.0, exit_channel_extreme=105.0), 105.0)
        self.assertEqual(rules.exit_order_level(-1, unit_stop=110.0, exit_channel_extreme=120.0), 110.0)

    def test_no_exit_channel_proposed_is_the_stop_alone(self):
        self.assertEqual(rules.exit_order_level(1, unit_stop=90.0, exit_channel_extreme=None), 90.0)


class StopLimitFillPriceTests(unittest.TestCase):
    def test_long_triggers_on_touch_and_fills_at_open_or_level(self):
        # ADR 0005: high >= level triggers; fills at max(level, open) + slippage.
        price = rules.stop_limit_fill_price(1, level=100.0, price_cap_value=102.0, open_=99.0,
                                            high=101.0, low=98.0, slippage_amount=0.1)
        self.assertAlmostEqual(price, 100.1)

    def test_long_gap_above_cap_works_as_a_limit_at_the_cap(self):
        price = rules.stop_limit_fill_price(1, level=100.0, price_cap_value=102.0, open_=105.0,
                                            high=106.0, low=101.0, slippage_amount=0.1)
        self.assertAlmostEqual(price, 102.1)

    def test_long_gap_above_cap_that_never_returns_does_not_fill(self):
        price = rules.stop_limit_fill_price(1, level=100.0, price_cap_value=102.0, open_=105.0,
                                            high=106.0, low=103.0, slippage_amount=0.1)
        self.assertIsNone(price)

    def test_short_triggers_on_touch_and_fills_at_open_or_level(self):
        price = rules.stop_limit_fill_price(-1, level=100.0, price_cap_value=98.0, open_=101.0,
                                            high=102.0, low=99.0, slippage_amount=0.1)
        self.assertAlmostEqual(price, 99.9)

    def test_short_gap_below_cap_works_as_a_limit_at_the_cap(self):
        price = rules.stop_limit_fill_price(-1, level=100.0, price_cap_value=98.0, open_=95.0,
                                            high=99.0, low=94.0, slippage_amount=0.1)
        self.assertAlmostEqual(price, 97.9)

    def test_no_trigger_when_the_bar_never_reaches_the_level(self):
        self.assertIsNone(rules.stop_limit_fill_price(1, 100.0, 102.0, 95.0, 99.0, 90.0, 0.1))
        self.assertIsNone(rules.stop_limit_fill_price(-1, 100.0, 98.0, 105.0, 106.0, 101.0, 0.1))


class MetricTests(unittest.TestCase):
    def test_annualised_return_doubles_over_one_year(self):
        self.assertAlmostEqual(rules.annualised_return(100.0, 200.0, 365.25), 1.0)

    def test_max_drawdown_simple_peak_and_trough(self):
        self.assertAlmostEqual(rules.max_drawdown([100.0, 120.0, 90.0, 110.0]), 0.25)

    def test_ratio_is_null_on_zero_drawdown(self):
        self.assertIsNone(rules.cagr_over_max_drawdown(0.1, 0.0))



class RollTargetTests(unittest.TestCase):
    """Which contract to hold next: LEAN's mapped contract once it is
    later-dated and priced, otherwise, within ROLL_DAYS_BEFORE_EXPIRY of
    the held contract's expiry, the nearest later-dated priced contract."""

    TODAY = date(2008, 6, 5)
    HELD = ("M8", date(2008, 6, 20), True)

    def test_first_assignment_takes_the_mapped_contract(self):
        mapped = ("M8", date(2008, 6, 20), True)
        self.assertEqual(rules.roll_target(self.TODAY, None, mapped, []), "M8")

    def test_no_roll_when_mapped_is_held_and_expiry_is_far(self):
        self.assertIsNone(rules.roll_target(self.TODAY, self.HELD, self.HELD, []))

    def test_rolls_to_a_priced_later_mapped_contract(self):
        mapped = ("U8", date(2008, 9, 19), True)
        self.assertEqual(rules.roll_target(self.TODAY, self.HELD, mapped, []), "U8")

    def test_waits_while_the_mapped_contract_has_no_price(self):
        mapped = ("U8", date(2008, 9, 19), False)
        self.assertIsNone(rules.roll_target(self.TODAY, self.HELD, mapped, []))

    def test_near_expiry_rolls_to_the_nearest_priced_later_contract(self):
        today = date(2008, 6, 12)
        candidates = [("Z8", date(2008, 12, 19), True), ("U8", date(2008, 9, 19), True),
                      ("M8", date(2008, 6, 20), True), ("H8", date(2008, 3, 21), True)]
        self.assertEqual(rules.roll_target(today, self.HELD, self.HELD, candidates), "U8")

    def test_near_expiry_skips_unpriced_candidates(self):
        today = date(2008, 6, 12)
        candidates = [("U8", date(2008, 9, 19), False), ("Z8", date(2008, 12, 19), True)]
        self.assertEqual(rules.roll_target(today, self.HELD, self.HELD, candidates), "Z8")

    def test_near_expiry_with_no_priced_candidate_stays(self):
        today = date(2008, 6, 12)
        candidates = [("U8", date(2008, 9, 19), False)]
        self.assertIsNone(rules.roll_target(today, self.HELD, self.HELD, candidates))

    def test_never_rolls_back_to_an_earlier_mapped_contract(self):
        mapped = ("H8", date(2008, 3, 21), True)
        self.assertIsNone(rules.roll_target(self.TODAY, self.HELD, mapped, []))


if __name__ == "__main__":
    unittest.main()
