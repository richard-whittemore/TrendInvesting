"""Tests for the sector-label check (docs/live-trading/plan.md, section 4):
snapshot.py's log format and universe filter, and compare.py's parsing and
agreement count. Standard library only; synthetic values, no market data.
"""
import importlib.util
import os
import sys
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name, stub=False):
    if stub:
        mod = types.ModuleType("AlgorithmImports")
        mod.QCAlgorithm = type("QCAlgorithm", (), {})
        mod.Universe = types.SimpleNamespace(Unchanged="UNCHANGED")
        mod.Resolution = types.SimpleNamespace(Daily="daily")
        sys.modules["AlgorithmImports"] = mod
    spec = importlib.util.spec_from_file_location(name, os.path.join(HERE, name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class SnapshotTest(unittest.TestCase):
    def setUp(self):
        self.m = _load("snapshot", stub=True)

    def test_sector_lines_are_short_and_carry_every_pair(self):
        pairs = [("T%03d" % i, 311) for i in range(60)] + [("NOSEC", None)]
        lines = self.m.sector_lines(pairs, width=100)
        self.assertTrue(all(l.startswith("SECT ") and len(l) <= 100 for l in lines))
        body = ";".join(l[5:] for l in lines)
        self.assertEqual(body.count(":"), 61)
        self.assertIn("NOSEC:-", body)

    def test_the_universe_is_the_backtests_coarse_filter(self):
        algo = self.m.SectorSnapshot()
        algo.UNIVERSE_SIZE, algo.picked = 2, False
        row = lambda s, dv, p=10.0, f=True: types.SimpleNamespace(Symbol=s, DollarVolume=dv, Price=p,
                                                                 HasFundamentalData=f)
        rows = [row("a", 1.0), row("b", 9.0), row("c", 5.0), row("cheap", 99.0, p=4.0), row("etf", 98.0, f=False)]
        self.assertEqual(algo.Coarse(rows), ["b", "c"])
        self.assertEqual(algo.Coarse(rows), "UNCHANGED")         # once only


class CompareTest(unittest.TestCase):
    def setUp(self):
        self.m = _load("compare")

    def test_snapshot_lines_are_read_from_a_log(self):
        log = ["2026-10-01 00:00:00 SECT AAPL:311;BRK.B:103", "noise", "SECT XYZ:-"]
        self.assertEqual(self.m.read_snapshot(log), {"AAPL": 311, "BRKB": 103, "XYZ": None})

    def test_the_ishares_file_gives_each_equitys_gics_sector(self):
        text = "\n".join([
            "iShares Russell 3000 ETF", 'Fund Holdings as of,"Oct 14, 2026"', "",
            "Ticker,Name,Sector,Asset Class,Market Value",
            '"AAPL","APPLE","Information Technology","Equity","1.00"',
            '"BRKB","BERKSHIRE","Financials","Equity","1.00"',
            '"USD","USD CASH","Cash and/or Derivatives","Cash","1.00"',
            "", "The content contained herein"])
        self.assertEqual(self.m.read_ishares(text), {"AAPL": "Information Technology", "BRKB": "Financials"})

    def test_tickers_are_compared_without_punctuation(self):
        self.assertEqual(self.m.norm("brk.b"), "BRKB")
        self.assertEqual(self.m.norm("BF/B"), "BFB")

    def test_agreement_maps_gics_onto_morningstar_and_lists_the_rest(self):
        ms = {"AAPL": 311, "GOOGL": 308, "AMZN": 102, "SHOP": 311, "XYZ": None}
        gics = {"AAPL": "Information Technology", "GOOGL": "Communication", "AMZN": "Information Technology"}
        r = self.m.compare(ms, gics)
        self.assertEqual((r["universe"], r["labelled_both"], r["agree"]), (5, 3, 2))
        self.assertEqual(r["disagree"], [("AMZN", 102, "Information Technology")])
        self.assertEqual(r["not_in_fund"], ["SHOP", "XYZ"])

    def test_every_gics_sector_has_a_morningstar_counterpart(self):
        self.assertEqual(len(set(self.m.GICS_TO_MORNINGSTAR.values())), 11)


if __name__ == "__main__":
    unittest.main()
