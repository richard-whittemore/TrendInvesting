"""Unit tests for backtest.py, the local Pinnacle CLC Turtle backtester.

Every price series here is synthetic and built in this file; no real
market data is ever used (research/futures-local/README.md, "No market
data"). The hand-computed Campaigns use a flat warm-up that makes N exactly
2.0 and both Entry Channel edges exact, so every level, fill and dollar
below can be checked by hand.

Run with `make research-test` from the repository root.
"""

import io
import os
import random
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from datetime import date, timedelta

import backtest
import loader
import markets

DAY0 = date(1990, 1, 1)
MULTIPLIER = 1000.0


def _market(symbol="TST", roll_months=(), roll_day=1, closely="g1", loosely="L1"):
    return markets.Market(symbol, "test market " + symbol, symbol, MULTIPLIER, 0.01, closely, loosely,
                          tuple(roll_months), roll_day, None, None, ())


def _bars(rows, day0=DAY0):
    """``rows`` of (open, high, low, close), one per calendar day."""
    return [loader.Bar(day0 + timedelta(days=i), o, h, l, c, None, None) for i, (o, h, l, c) in enumerate(rows)]


# 70 flat bars: every True Range is 2.0, so N is exactly 2.0; the Entry
# Channel is exactly 101 (high) / 99 (low); Strength is ready (64+ closes).
FLAT = [(100.0, 101.0, 99.0, 100.0)] * 70
LONG_BREAKOUT = (100.0, 102.0, 99.5, 101.5)   # day 70: high 102 > 101
LONG_ENTRY_FILL = (101.5, 102.5, 101.0, 102.0)  # day 71: fills the entry at 101.5 + 0.1
ADD_SIGNAL = (102.4, 103.0, 102.2, 102.8)     # day 72: high reaches the 102.6 rung
ADD_FILL = (102.7, 103.2, 102.5, 103.0)       # day 73: fills the Add at 102.7 + 0.1


def _config(**overrides):
    values = dict(start=DAY0, end=date(2015, 12, 31))
    values.update(overrides)
    return backtest.Config(**values)


def _run(rows_by_symbol, config=None, market_overrides=None):
    market_overrides = market_overrides or {}
    universe = {symbol: market_overrides.get(symbol, _market(symbol)) for symbol in rows_by_symbol}
    series = {symbol: rows if rows and isinstance(rows[0], loader.Bar) else _bars(rows)
              for symbol, rows in rows_by_symbol.items()}
    return backtest.run_backtest(config or _config(), universe, series)


class HandComputedLongCampaignTests(unittest.TestCase):
    """Entry, one Add, and a stop-out, each checked by hand.

    Entry (The Turtle Rules p.18-19; ADR 0005): day 70 breaks the 101
    Entry Channel; the order rests at 101, capped at 101 + 1N = 103, and
    fills on day 71 at max(101, open 101.5) + 0.05N slippage = 101.6.
    Unit (The Turtle Rules p.14): floor(1,000,000 x 1% / (2 x 1,000)) = 5
    contracts. Stop (The Turtle Rules p.22): 101.6 - 2N = 97.6.
    Add (The Turtle Rules p.19-20): the rung is 101.6 + 1/2N = 102.6; day 72
    reaches it, the Add fills on day 73 at 102.7 + 0.1 = 102.8. The first
    Unit's stop rises by 1/2N to 98.6; the new Unit's stop is 98.8.
    """

    def run_with_exit_bar(self, exit_bar):
        rows = FLAT + [LONG_BREAKOUT, LONG_ENTRY_FILL, ADD_SIGNAL, ADD_FILL, exit_bar]
        return _run({"TST": rows})

    def test_entry_add_and_stop_out(self):
        # Day 74 trades down through both stops from an open above them.
        result = self.run_with_exit_bar((99.5, 99.8, 98.0, 98.2))
        fills = [(f.kind, f.date, f.quantity, round(f.price, 10)) for f in result.fills]
        self.assertEqual(fills, [
            ("entry", DAY0 + timedelta(days=71), 5, 101.6),
            ("add", DAY0 + timedelta(days=73), 5, 102.8),
            ("exit", DAY0 + timedelta(days=74), 5, 98.5),   # 98.6 - 0.1
            ("exit", DAY0 + timedelta(days=74), 5, 98.7),   # 98.8 - 0.1
        ])
        self.assertEqual(len(result.campaigns), 1)
        campaign = result.campaigns[0]
        # (98.5 - 101.6) + (98.7 - 102.8) = -7.2 points x 5 x $1,000.
        self.assertAlmostEqual(campaign.gross_pnl, -36000.0, places=6)
        self.assertAlmostEqual(campaign.commission, 4 * 5 * 2.50, places=6)
        self.assertAlmostEqual(campaign.net_pnl, -36050.0, places=6)
        # R = -7.2 / (2 x 2.0).
        self.assertAlmostEqual(campaign.r_multiple, -1.8, places=10)
        self.assertEqual(campaign.max_units, 2)
        self.assertAlmostEqual(result.final_equity, 1_000_000.0 - 36050.0, places=6)
        self.assertTrue(result.reconcile.ok)

    def test_gap_through_both_stops_fills_at_the_open(self):
        # Day 74 opens at 98.0, below both stops: each fills at
        # min(stop, open) - slippage = 97.9, never at its stop.
        result = self.run_with_exit_bar((98.0, 98.5, 97.0, 97.5))
        exits = [round(f.price, 10) for f in result.fills if f.kind == "exit"]
        self.assertEqual(exits, [97.9, 97.9])
        # (97.9 - 101.6) + (97.9 - 102.8) = -8.6 points x 5 x $1,000.
        self.assertAlmostEqual(result.campaigns[0].gross_pnl, -43000.0, places=6)
        self.assertTrue(result.reconcile.ok)

    def test_a_bar_that_stays_above_the_stops_exits_nothing(self):
        result = self.run_with_exit_bar((103.0, 103.1, 102.9, 103.0))
        self.assertEqual([f.kind for f in result.fills], ["entry", "add"])
        self.assertEqual(result.campaigns, [])
        self.assertEqual(len(result.open_campaigns), 1)
        self.assertTrue(result.reconcile.ok)


class GapAndCapTests(unittest.TestCase):
    def test_entry_that_gaps_beyond_the_price_cap_and_never_trades_back_is_skipped(self):
        # ADR 0005's amendment: the cap is 101 + 1N = 103; day 71 opens at
        # 104 and its low (103.5) never trades back down to 103.
        result = _run({"TST": FLAT + [LONG_BREAKOUT, (104.0, 105.0, 103.5, 104.5)]})
        self.assertEqual(result.fills, [])

    def test_entry_that_gaps_beyond_the_cap_but_trades_back_fills_at_the_cap(self):
        result = _run({"TST": FLAT + [LONG_BREAKOUT, (104.0, 105.0, 102.5, 104.5)]})
        self.assertEqual([round(f.price, 10) for f in result.fills], [103.1])

    def test_short_entry_and_gap_up_through_the_stop(self):
        # Mirror image. Day 70 breaks the 99 low; the sell stop rests at 99,
        # capped at 97, and fills on day 71 at min(99, open 98.5) - 0.1 =
        # 98.4. Stop 98.4 + 2N = 102.4. Day 72 opens at 103, above it: the
        # buy stop fills at max(102.4, 103) + 0.1 = 103.1.
        rows = FLAT + [(100.0, 100.5, 98.0, 98.5), (98.5, 99.0, 97.5, 98.0), (103.0, 103.5, 102.8, 103.2)]
        result = _run({"TST": rows})
        self.assertEqual([(f.kind, f.direction, round(f.price, 10)) for f in result.fills],
                         [("entry", -1, 98.4), ("exit", -1, 103.1)])
        # -(103.1 - 98.4) = -4.7 points x 5 x $1,000.
        self.assertAlmostEqual(result.campaigns[0].gross_pnl, -23500.0, places=6)
        self.assertAlmostEqual(result.campaigns[0].r_multiple, -4.7 / 4.0, places=10)
        self.assertTrue(result.reconcile.ok)


def _exit_channel_rows():
    # After the entry fill (day 71), 20 bars trade 101.8-102.4: below the
    # 102.6 Add rung, so no Add. Day 92's low (101.7) breaks their 101.8
    # low; the Exit Order moves to 101.8 (above the 97.6 stop) and day 93,
    # opening at 101.6, fills it at min(101.8, 101.6) - 0.1 = 101.5.
    quiet = [(102.0, 102.4, 101.8, 102.1)] * 20
    return FLAT + [LONG_BREAKOUT, LONG_ENTRY_FILL] + quiet + [
        (102.0, 102.2, 101.7, 101.75), (101.6, 101.7, 101.0, 101.2)]


class ExitChannelAndRollTests(unittest.TestCase):
    def test_exit_channel_exit(self):
        result = _run({"TST": _exit_channel_rows()})
        self.assertEqual([(f.kind, f.date, round(f.price, 10)) for f in result.fills], [
            ("entry", DAY0 + timedelta(days=71), 101.6),
            ("exit", DAY0 + timedelta(days=93), 101.5),
        ])
        self.assertAlmostEqual(result.campaigns[0].gross_pnl, -500.0, places=6)
        self.assertTrue(result.reconcile.ok)

    def test_roll_cost_is_one_round_trip_per_open_position(self):
        # 1990-03-20 is day 78, inside the Campaign (days 71-93). One roll:
        # 5 contracts x (2 x $2.50 commission + 2 x 0.05N x $1,000 slippage)
        # = 5 x (5 + 200) = $1,025.
        market = _market(roll_months=(3,), roll_day=20)
        result = _run({"TST": _exit_channel_rows()}, market_overrides={"TST": market})
        self.assertAlmostEqual(result.total_roll_cost, 1025.0, places=6)
        self.assertEqual(result.roll_count, 1)
        campaign = result.campaigns[0]
        self.assertAlmostEqual(campaign.roll_cost, 1025.0, places=6)
        self.assertAlmostEqual(campaign.net_pnl, -500.0 - 25.0 - 1025.0, places=6)
        self.assertTrue(result.reconcile.ok)

    def test_no_campaign_opens_before_the_start_date(self):
        result = _run({"TST": _exit_channel_rows()}, config=_config(start=DAY0 + timedelta(days=80)))
        self.assertEqual(result.fills, [])


def _random_walk(seed, count, start_price, skip_every=None):
    rng = random.Random(seed)
    rows, price, day = [], start_price, DAY0
    for i in range(count):
        day += timedelta(days=1)
        if skip_every and i % skip_every == 0:
            continue  # a holiday this market alone observes
        drift = 0.15 * (1 if (i // 150) % 2 == 0 else -1)  # alternating trends, so Campaigns happen
        open_ = price + rng.gauss(0, 0.5)
        close = open_ + drift + rng.gauss(0, 1.0)
        high = max(open_, close) + abs(rng.gauss(0, 0.6))
        low = min(open_, close) - abs(rng.gauss(0, 0.6))
        rows.append(loader.Bar(day, open_, high, low, close, None, None))
        price = close
    return rows


class ReconcileTests(unittest.TestCase):
    def test_campaign_pnl_sums_to_the_account_pnl(self):
        # Three synthetic markets, one with its own holidays, two sharing a
        # correlation group, rolls charged quarterly; the price level
        # crosses zero, as back-adjusted series can.
        series = {"AAA": _random_walk(1, 1500, 20.0), "BBB": _random_walk(2, 1500, 5.0, skip_every=7),
                  "CCC": _random_walk(3, 1500, 50.0)}
        universe = {"AAA": _market("AAA", (3, 6, 9, 12), 10, "g1", "L1"),
                    "BBB": _market("BBB", (2, 5, 8, 11), 20, "g1", "L1"),
                    "CCC": _market("CCC", (1, 4, 7, 10), 15, "g2", "L2")}
        result = backtest.run_backtest(_config(), universe, series)
        self.assertGreaterEqual(len(result.campaigns), 5)
        self.assertGreater(result.total_roll_cost, 0.0)
        self.assertTrue(any(c.direction == -1 for c in result.campaigns))
        self.assertTrue(any(c.max_units > 1 for c in result.campaigns))
        self.assertTrue(result.reconcile.ok, result.reconcile)
        self.assertLess(abs(result.reconcile.difference), 0.01)
        per_market = sum(result.pnl_by_market.values())
        self.assertAlmostEqual(per_market, result.reconcile.campaign_pnl, places=4)


class HoldoutTests(unittest.TestCase):
    def test_an_end_past_2015_is_refused_by_default(self):
        with self.assertRaises(backtest.HoldoutError):
            _run({"TST": FLAT}, config=_config(end=date(2016, 1, 4)))

    def test_the_default_config_is_in_sample(self):
        config = backtest.Config()
        self.assertEqual((config.start, config.end), (date(1980, 1, 1), date(2015, 12, 31)))
        backtest.check_dates(config)

    def test_allow_holdout_permits_it(self):
        result = _run({"TST": FLAT}, config=_config(end=date(2016, 1, 4), allow_holdout=True))
        self.assertEqual(result.fills, [])

    def test_bars_past_the_end_are_never_read(self):
        rows = _bars(FLAT + [LONG_BREAKOUT, LONG_ENTRY_FILL])
        result = _run({"TST": rows}, config=_config(end=rows[70].date))
        self.assertEqual(result.fills, [])
        self.assertEqual(result.equity_curve[-1][0], rows[70].date)

    def test_the_command_line_refuses_without_the_flag(self):
        with tempfile.TemporaryDirectory() as directory:
            err = io.StringIO()
            with redirect_stderr(err), redirect_stdout(io.StringIO()):
                code = backtest.main(["--data-dir", directory, "--end", "2016-06-30"])
            self.assertEqual(code, 2)
            self.assertIn("--allow-holdout", err.getvalue())


class CommandLineTests(unittest.TestCase):
    def test_runs_on_files_in_a_data_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            rows = FLAT + [LONG_BREAKOUT, LONG_ENTRY_FILL, ADD_SIGNAL, ADD_FILL, (99.5, 99.8, 98.0, 98.2)]
            with open(os.path.join(directory, "GC.TXT"), "w") as handle:
                for bar in _bars(rows):
                    handle.write("{:%Y%m%d},{},{},{},{},0,0\n".format(bar.date, bar.open, bar.high, bar.low,
                                                                      bar.close))
            out = io.StringIO()
            with redirect_stdout(out), redirect_stderr(io.StringIO()):
                code = backtest.main(["--data-dir", directory, "--start", "1990-01-01", "--markets", "GC"])
            self.assertEqual(code, 0)
            report = out.getvalue()
            self.assertIn("Campaigns", report)
            self.assertIn("Reconcile", report)
            self.assertIn("OK", report)


if __name__ == "__main__":
    unittest.main()
