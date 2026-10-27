"""The morning reading stands for the close with the Flex executions proof: the owner's decision of 2026-10-27 (ADR 0019,
amended 2026-10-27).

The data job writes a session's file only the next morning (Massive's free
plan serves a session's bars from the morning after it), so the adapter
reads the account then. IBKR's executions reach back only to midnight, so
its own list cannot show an execution between the close and midnight; the
Flex reader's executions proof (flex.py, step 5) covers that span. The
morning reading stands for the close only when every proof holds:

- the proof starts on or before the session's day, lists every execution up
  to at least the midnight the reading's day starts at, and lists none at or
  after the close (a close of the NYSE calendar, an early close included);
- IBKR's own list, since that midnight, shows none;
- the journal holds no fill stamped at or after the close;
- the reading is before the next session's open;

and every other condition of the reading after the close. Otherwise it is
stamped at its read time and the session withheld, the reason naming the
proof that failed. With no proof yet before 09:00 New York, the delivery
waits for the Flex reader's next firing.

Synthetic figures only; nothing here reaches IBKR.
"""
import json
import sys
import tempfile
import unittest
from datetime import date, datetime, timezone
from decimal import Decimal
from pathlib import Path
from types import SimpleNamespace

tests_dir = Path(__file__).resolve().parent
if str(tests_dir) not in sys.path:
    sys.path.insert(0, str(tests_dir))

from engine_double import LoggedEngine, final_hash, run_approval
from fake_tws import ACCOUNT, FakeTWS, contract, execution
from test_connection import Clock, config
from test_delivery import BAR, CLOSED, RUN, SPY_CON_ID, commit, session_envelopes, write_stream
from test_loop import COMMISSION_WAIT, SUMMARY, Ids
import flex
from delivery import nyse_calendar
from loop import AdapterLoop
from reports import FILL_TYPE
from snapshot import BROKER_SNAPSHOT_TYPE

ACCOUNT_SNAPSHOT = "account.snapshot"
MARK = "market.mark.recorded"
RECONCILIATION = "account.reconciliation"
NEW_YORK = nyse_calendar.NEW_YORK
OFFICIAL = 400.5
STARTING_BOOK = ({"SPY": 147}, 100000.0)
AT_CLOSE = 100000 + 147 * OFFICIAL
THURSDAY = date(2027, 1, 14)
FRIDAY_MORNING = date(2027, 1, 15)
FRIDAY = date(2027, 1, 22)
MONDAY = date(2027, 1, 25)
BEFORE_HOLIDAY = date(2027, 1, 15)
AFTER_HOLIDAY = date(2027, 1, 19)
EARLY = date(2027, 11, 26)
AFTER_EARLY = date(2027, 11, 29)


def ny(day, hour, minute=0):
    return datetime(day.year, day.month, day.day, hour, minute, tzinfo=NEW_YORK)


def stamp(at):
    return at.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ymd(day):
    return day.strftime("%Y%m%d")


def day_envelopes(day):
    """session_envelopes for day, its bar and close stamped at the day's own close (an early close's 1 p.m.)."""
    if nyse_calendar.session_close(day).hour == 16:
        return session_envelopes([day])
    import hashlib
    out = []
    for e in session_envelopes([day]):
        e = json.loads(json.dumps(e).replace("{}T21:00:00Z".format(day.isoformat()),
                                             stamp(nyse_calendar.session_close(day))))
        e["payload_hash"] = hashlib.sha256(json.dumps(e["payload"], separators=(",", ":")).encode()).hexdigest()
        out.append(e)
    return out


class MorningTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        root = Path(self.dir.name)
        self.pending = str(root / "pending.json")
        self.engine_log = str(root / (RUN + ".log"))
        self.streams = root / "streams"
        self.folder = self.streams / RUN
        self.folder.mkdir(parents=True)
        (root / "flex").mkdir()
        self.ledger = str(root / "flex" / "cash-movements.jsonl")
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(ACCOUNT, starting_book=STARTING_BOOK))
        self.fake = FakeTWS()
        self.fake.summary = list(SUMMARY)
        self.fake.positions = [(ACCOUNT, contract(), Decimal("147"), 312.5)]
        self.fake.marks = {SPY_CON_ID: 401.17}
        self.clock = Clock()
        self.at(ny(FRIDAY_MORNING, 8, 5))

    def at(self, when):
        """The adapter's clock and IBKR's at when."""
        self.now = when.astimezone(timezone.utc)
        self.fake.server_time = int(self.now.timestamp())

    def proof(self, to_day, fetched=None, executions=(), from_day=date(2026, 12, 16), state="read", account=ACCOUNT):
        """The executions proof as the Flex reader writes it: a statement from from_day to to_day, asked for at
        fetched (05:30 New York on to_day by default), listing executions, each (ID, aware time)."""
        fetched = fetched or ny(to_day, 5, 30)
        if state == "read":
            proof = {"state": "read", "account_id": account, "checked_at": stamp(fetched), "fetched_at": stamp(fetched),
                     "query": "trades",
                     "section": "TradeConfirms", "from_date": ymd(from_day), "to_date": ymd(to_day),
                     "covered_until": stamp(flex.covered_until(ymd(to_day), fetched.astimezone(timezone.utc))),
                     "executions": [{"id": i, "at": stamp(at)} for i, at in executions]}
        elif state == "none":
            proof = {"state": "none", "account_id": account, "checked_at": stamp(fetched),
                     "reason": "no Trades section, synthetic"}
        else:
            proof = {"state": "set-aside", "account_id": account, "checked_at": stamp(fetched), "code": "1012",
                     "reason": "the token has expired, synthetic"}
        Path(flex.executions_path(self.ledger)).write_text(json.dumps(proof, sort_keys=True) + "\n")

    def loop(self, ledger=True):
        def sleep(seconds):
            self.clock.now += seconds
        loop = AdapterLoop(config(mode="paper", paper_sending_enabled=True, commission_wait=COMMISSION_WAIT),
                           self.fake, self.engine, self.pending, self.engine_log,
                           now=lambda: self.now, today=lambda: self.now.astimezone(NEW_YORK).date(), sleep=sleep,
                           monotonic=self.clock, poll=0, new_id=Ids(), streams=str(self.streams),
                           cash_ledger=self.ledger if ledger else None)
        self.addCleanup(loop.close)
        return loop

    def deliver(self, day, loop=None, ledger=True):
        write_stream(self.folder, day, day_envelopes(day))
        loop = loop or self.loop(ledger)
        if loop.conn is None:
            loop.start()
        for _ in range(40):
            loop.step()
        return loop

    def sent(self, event_type):
        return [e for e in self.engine.inputs() if e["type"] == event_type]

    def broker(self):
        return self.sent(BROKER_SNAPSHOT_TYPE)[-1]["payload"]

    def assert_as_of_close(self, day):
        close = stamp(nyse_calendar.session_close(day))
        broker = self.broker()
        self.assertEqual((broker["as_of"], broker["as_of_close"], broker["not_as_of_close"]), (close, True, ""))
        self.assertTrue(broker["read_at"].startswith(stamp(self.now)[:16]), "the morning read time is the evidence")
        account = self.sent(ACCOUNT_SNAPSHOT)[-1]["payload"]
        self.assertEqual((account["as_of"], account["net_liquidation"]), (close, AT_CLOSE))

    def assert_withheld(self, *why):
        broker = self.broker()
        self.assertFalse(broker["as_of_close"])
        self.assertEqual(broker["as_of"], broker["read_at"])
        for text in why:
            self.assertIn(text, broker["not_as_of_close"])
        return broker["not_as_of_close"]

    # -- the proof holds -------------------------------------------------------
    def test_a_morning_reading_with_the_proof_stands_for_the_close(self):
        self.proof(FRIDAY_MORNING)
        self.deliver(THURSDAY)
        self.assertEqual([e["type"] for e in self.engine.inputs()],
                         [ACCOUNT_SNAPSHOT, BROKER_SNAPSHOT_TYPE, RECONCILIATION, BAR, MARK, CLOSED])
        self.assert_as_of_close(THURSDAY)

    def test_an_execution_before_the_close_in_the_proof_stands(self):
        self.proof(FRIDAY_MORNING, executions=[("0001.day.01.01", ny(THURSDAY, 15, 59))])
        self.deliver(THURSDAY)
        self.assert_as_of_close(THURSDAY)

    # -- the proof fails -------------------------------------------------------
    def test_no_proof_withholds(self):
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.deliver(THURSDAY)
        self.assert_withheld("midnight", "executions proof", "no executions proof is written")

    def test_no_flex_ledger_withholds(self):
        self.deliver(THURSDAY, ledger=False)
        self.assert_withheld("midnight", "no Flex ledger")

    def test_a_proof_set_aside_withholds_naming_why(self):
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(FRIDAY_MORNING, state="set-aside")
        self.deliver(THURSDAY)
        self.assert_withheld("set-aside", "the token has expired, synthetic")

    def test_no_trades_in_the_flex_query_withholds(self):
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(FRIDAY_MORNING, state="none")
        self.deliver(THURSDAY)
        self.assert_withheld("no Trades section, synthetic")

    def test_a_proof_ending_on_the_sessions_day_withholds(self):
        # A statement ending on the session's own day (an Activity statement, read the next morning) lists every
        # execution only up to that day's start.
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(THURSDAY, fetched=ny(FRIDAY_MORNING, 5, 30))
        self.deliver(THURSDAY)
        self.assert_withheld("2027-01-14T05:00:00Z", "2027-01-15T05:00:00Z")

    def test_a_proof_asked_for_too_soon_after_midnight_withholds(self):
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(FRIDAY_MORNING, fetched=ny(FRIDAY_MORNING, 0, 30))
        self.deliver(THURSDAY)
        self.assert_withheld("2027-01-15T04:30:00Z")

    def test_a_proof_starting_after_the_session_withholds(self):
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(FRIDAY_MORNING, from_day=FRIDAY_MORNING)
        self.deliver(THURSDAY)
        self.assert_withheld("starts on 2027-01-15")

    def test_an_execution_after_the_close_in_the_proof_withholds(self):
        # An after-hours trade at 20:30 New York: IBKR's own list in the morning no longer shows it.
        self.proof(FRIDAY_MORNING, executions=[("0002.night.01.01", ny(THURSDAY, 20, 30))])
        self.deliver(THURSDAY)
        self.assert_withheld("Flex", "0002.night.01.01")

    def test_an_execution_at_the_close_in_the_proof_withholds(self):
        self.proof(FRIDAY_MORNING, executions=[("0002.auction.01.01", ny(THURSDAY, 16, 0))])
        self.deliver(THURSDAY)
        self.assert_withheld("0002.auction.01.01")

    def test_an_execution_since_midnight_at_ibkr_withholds(self):
        self.proof(FRIDAY_MORNING)
        # Another client's trade, after the start (a start would stop on it: no order the engine released).
        loop = self.loop()
        loop.start()
        ex = execution(exec_id="0003.dawn.01.01", order_id=0, perm_id=6161, order_ref="", client_id=0,
                       time="20270115 03:00:01 US/Eastern")
        self.fake.executions.append((contract(), ex))
        self.fake.commissions[ex.execId] = SimpleNamespace(execId=ex.execId, commission="1.00", currency="USD")
        self.deliver(THURSDAY, loop)
        self.assert_withheld("0003.dawn.01.01")

    def test_a_fill_journalled_after_the_close_withholds(self):
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.late.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00Z"}
        loop.submit(FILL_TYPE, 1, payload, payload["filled_at"])
        # The account holds the share and paid for it, so only the fill's time withholds.
        self.fake.positions = [(ACCOUNT, contract(), Decimal("148"), 312.5)]
        self.fake.summary = [(a, t, "99597.75" if t == "TotalCashValue" else v, c) for a, t, v, c in SUMMARY]
        self.deliver(THURSDAY, loop)
        self.assert_withheld("journal", "0001f4e8.late.01.01")

    def test_a_reading_after_the_next_open_withholds(self):
        self.at(ny(FRIDAY_MORNING, 9, 31))
        self.proof(FRIDAY_MORNING)
        self.deliver(THURSDAY)
        self.assert_withheld("open")

    def test_another_accounts_proof_withholds(self):
        # A proof stands only for the account its statement was checked against.
        self.at(ny(FRIDAY_MORNING, 9, 0))
        self.proof(FRIDAY_MORNING, account="DU0000998")
        self.deliver(THURSDAY)
        why = self.assert_withheld("another account")
        self.assertNotIn("DU0000998", why)

    def test_a_fill_an_earlier_run_journalled_after_the_close_withholds(self):
        # The fills of every run the Run approvals chain back to count, as the previous close's book does
        # (book_basis.previous_close): a fresh run started after the close does not hide its predecessor's fill.
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.earlier.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00Z"}
        commit(self.engine, "fill:earlier", kind=FILL_TYPE, payload=payload, version=5)
        fresh = RUN + "-2"
        earlier_log = self.engine_log
        self.engine_log = str(Path(self.dir.name) / (fresh + ".log"))
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, previous_log=RUN + ".log", previous_log_final_hash=final_hash(earlier_log)))
        self.folder = self.streams / fresh
        write_stream(self.folder, THURSDAY, session_envelopes([THURSDAY], run=fresh))
        self.fake.positions = [(ACCOUNT, contract(), Decimal("148"), 312.5)]
        self.fake.summary = [(a, t, "99597.75" if t == "TotalCashValue" else v, c) for a, t, v, c in SUMMARY]
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        self.assert_withheld("journal", "0001f4e8.earlier.01.01")

    def test_a_fill_two_runs_back_is_found_past_an_empty_run(self):
        # A run between them with nothing but its configuration and approval says nothing about when it started:
        # the walk goes past it (book_basis.previous_close walks the same chain).
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.twoback.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00Z"}
        commit(self.engine, "fill:twoback", kind=FILL_TYPE, payload=payload, version=5)
        first = self.engine_log
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        LoggedEngine(middle, approval=run_approval(ACCOUNT, previous_log=RUN + ".log",
                                                   previous_log_final_hash=final_hash(first)))
        fresh = RUN + "-3"
        self.engine_log = str(Path(self.dir.name) / (fresh + ".log"))
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, previous_log=RUN + "-2.log", previous_log_final_hash=final_hash(middle)))
        self.folder = self.streams / fresh
        write_stream(self.folder, THURSDAY, session_envelopes([THURSDAY], run=fresh))
        self.fake.positions = [(ACCOUNT, contract(), Decimal("148"), 312.5)]
        self.fake.summary = [(a, t, "99597.75" if t == "TotalCashValue" else v, c) for a, t, v, c in SUMMARY]
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        self.assert_withheld("journal", "0001f4e8.twoback.01.01")

    # -- IB Gateway's zone: where IBKR's own list starts -------------------------
    def test_a_gateway_in_another_zone_withholds_with_no_execution_listed(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 05:05:00 US/Pacific"
        self.deliver(THURSDAY)
        self.assert_withheld("US/Pacific", "New York")

    def test_a_connection_time_that_cannot_be_read_withholds(self):
        self.proof(FRIDAY_MORNING)
        for text in (None, "", "20270115", "20270115 08:05:00", "20270115 08:05:00 Mars/Olympus"):
            with self.subTest(text=text):
                self.setUp()
                self.proof(FRIDAY_MORNING)
                self.fake.tws_connection_time = text
                self.deliver(THURSDAY)
                self.assert_withheld("IB Gateway's connection time")

    def test_new_yorks_abbreviation_in_force_is_new_york(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 08:05:00 EST"
        self.deliver(THURSDAY)
        self.assert_as_of_close(THURSDAY)

    def test_the_abbreviation_not_in_force_is_not_new_york(self):
        # EDT in January is no New York zone: a Gateway on a fixed offset, or another place's abbreviation.
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 08:05:00 EDT"
        self.deliver(THURSDAY)
        self.assert_withheld("EDT")

    def test_either_abbreviation_stands_in_the_hour_repeated_when_daylight_saving_ends(self):
        # 01:30 on 2026-11-01 happens twice in New York: first in EDT, then in EST.
        from loop import gateway_zone_problem
        for text in ("20261101 01:30:00 EST", "20261101 01:30:00 EDT", "20261101 01:30:00 Eastern Standard Time",
                     "20261101 01:30:00 Eastern Daylight Time"):
            with self.subTest(text=text):
                self.assertEqual(gateway_zone_problem(text), "")
        self.assertNotEqual(gateway_zone_problem("20261101 02:30:00 EDT"), "", "after the repeated hour: EST only")

    def test_a_time_new_york_skips_in_the_spring_withholds(self):
        # 02:30 on 2026-03-08 never happened in New York: the clocks went from 02:00 EST to 03:00 EDT.
        from loop import gateway_zone_problem
        for zone in ("EST", "EDT", "Eastern Standard Time", "Eastern Daylight Time", "US/Eastern", "America/New_York"):
            with self.subTest(zone=zone):
                text = "20260308 02:30:00 " + zone
                problem = gateway_zone_problem(text)
                self.assertIn(text, problem)
                self.assertIn("did not happen", problem)
        self.assertEqual(gateway_zone_problem("20260308 03:30:00 EDT"), "", "the hour after the gap")
        self.assertEqual(gateway_zone_problem("20260308 01:30:00 EST"), "", "the hour before it")

    def test_new_yorks_long_zone_name_in_force_is_new_york(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 08:05:00 Eastern Standard Time"
        self.deliver(THURSDAY)
        self.assert_as_of_close(THURSDAY)

    def test_the_long_zone_name_not_in_force_withholds_showing_the_raw_text(self):
        self.proof(FRIDAY_MORNING)
        raw = "20270115 08:05:00 Eastern Daylight Time"
        self.fake.tws_connection_time = raw
        with self.assertLogs("ibkr", "INFO") as logs:
            self.deliver(THURSDAY)
        self.assertIn(raw, self.assert_withheld("IB Gateway's connection time"))
        self.assertIn(raw, "\n".join(logs.output), "the journal shows the Gateway's text as it gave it")

    def test_an_execution_time_is_checked_as_the_connection_time_is(self):
        # An execution's time in New York's abbreviation in force stands; one in EDT in January does not.
        for zone, stands in (("EST", True), ("EDT", False)):
            with self.subTest(zone=zone):
                self.setUp()
                self.proof(FRIDAY_MORNING)
                loop = self.loop()
                loop.start()
                ex = execution(exec_id="0006.abbr.01.01", order_id=0, perm_id=7272, order_ref="", client_id=0,
                               time="20270114 10:00:01 " + zone)
                loop._count_execution(SimpleNamespace(name="exec_details", args=(-1, contract(), ex)))
                self.deliver(THURSDAY, loop)
                if stands:
                    self.assert_as_of_close(THURSDAY)
                else:
                    self.assert_withheld("IBKR's execution time", "20270114 10:00:01 EDT")

    def test_a_corrected_gateway_is_trusted_again_after_a_reconnect(self):
        # The zone, and every execution zone seen, belong to one connection: a reconnect reads them afresh.
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 07:00:00 US/Pacific"
        loop = self.loop()
        loop.start()
        ex = execution(exec_id="0005.zone.01.01", order_id=0, perm_id=7171, order_ref="", client_id=0,
                       time="20270114 10:00:01 US/Pacific")
        loop._count_execution(SimpleNamespace(name="exec_details", args=(-1, contract(), ex)))
        self.fake.tws_connection_time = "20270115 08:00:00 US/Eastern"
        self.fake.drop()
        for _ in range(10):
            loop.step()
        self.assertTrue(loop.ready(), "setup: reconnected and reconciled")
        self.deliver(THURSDAY, loop)
        self.assert_as_of_close(THURSDAY)

    def test_a_journalled_fill_with_no_zone_withholds(self):
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.nozone.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00"}
        loop.outbox.append({"id": "fill:nozone", "event_type": FILL_TYPE, "schema_version": 1,
                            "event_time": "2027-01-14T21:30:00Z", "payload": payload})
        self.assertIn("0001f4e8.nozone.01.01", " ".join(loop._journalled_fills_since(
            nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc))))

    def test_an_execution_reported_in_another_zone_withholds(self):
        # IBKR's executions reach back to midnight in the zone of IB Gateway's login, set to New York by its unit;
        # an execution reported in another zone shows the list's midnight is not New York's.
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        ex = execution(exec_id="0005.zone.01.01", order_id=0, perm_id=7171, order_ref="", client_id=0,
                       time="20270114 10:00:01 US/Pacific")
        loop._count_execution(SimpleNamespace(name="exec_details", args=(-1, contract(), ex)))
        self.deliver(THURSDAY, loop)
        self.assert_withheld("US/Pacific", "New York")

    def test_the_gateway_runs_on_new_york_time(self):
        # IBKR's executions reach back only to midnight in IB Gateway's own zone: the unit sets it to New York's.
        unit = (tests_dir.parents[2] / "deploy" / "systemd" / "ti-gateway.service").read_text()
        self.assertRegex(unit, r"(?m)^Environment=TZ=America/New_York$")

    def test_a_proof_replaced_during_the_read_is_waited_for_before_nine(self):
        # The reader's next firing rewrites the proof set aside while it runs; one read before the adapter's account
        # reading began and set aside by its end is waited for, not taken as a reason to withhold.
        self.at(ny(FRIDAY_MORNING, 6, 35))
        self.proof(FRIDAY_MORNING)
        good = Path(flex.executions_path(self.ledger)).read_text()
        loop = self.loop()
        loop.start()
        fake, test = self.fake, self

        original = fake.req_positions
        state = {"armed": True}

        def replaced():
            if state["armed"]:
                state["armed"] = False
                test.proof(FRIDAY_MORNING, state="set-aside")
            original()
        fake.req_positions = replaced
        write_stream(self.folder, THURSDAY, day_envelopes(THURSDAY))
        for _ in range(3):
            loop.step()
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE), [], "the batch waits for the proof")
        Path(flex.executions_path(self.ledger)).write_text(good)
        for _ in range(40):
            loop.step()
        self.assert_as_of_close(THURSDAY)

    # -- waiting for the proof -------------------------------------------------
    def test_the_delivery_waits_for_the_proof_until_nine(self):
        self.at(ny(FRIDAY_MORNING, 6, 5))
        loop = self.deliver(THURSDAY)
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE), [], "no proof yet: the batch waits")
        self.at(ny(FRIDAY_MORNING, 6, 35))
        self.proof(FRIDAY_MORNING, fetched=ny(FRIDAY_MORNING, 6, 30))
        for _ in range(40):
            loop.step()
        self.assert_as_of_close(THURSDAY)

    def test_a_proof_that_does_not_cover_yet_is_waited_for_too(self):
        self.at(ny(FRIDAY_MORNING, 6, 5))
        self.proof(date(2027, 1, 14), fetched=ny(date(2027, 1, 14), 5, 30))
        self.deliver(THURSDAY)
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE), [])

    def test_no_proof_by_nine_withholds(self):
        self.at(ny(FRIDAY_MORNING, 6, 5))
        loop = self.deliver(THURSDAY)
        self.at(ny(FRIDAY_MORNING, 9, 0))
        for _ in range(40):
            loop.step()
        self.assert_withheld("no executions proof is written")

    # -- the calendar ----------------------------------------------------------
    def test_an_early_close(self):
        self.at(ny(AFTER_EARLY, 7, 5))
        self.proof(AFTER_EARLY)
        self.deliver(EARLY)
        self.assert_as_of_close(EARLY)

    def test_an_execution_after_an_early_close_withholds(self):
        # 1:30 p.m. is after the day after Thanksgiving's 1 p.m. close.
        self.at(ny(AFTER_EARLY, 7, 5))
        self.proof(AFTER_EARLY, executions=[("0004.early.01.01", ny(EARLY, 13, 30))])
        self.deliver(EARLY)
        self.assert_withheld("0004.early.01.01")

    def test_monday_morning_delivers_friday(self):
        self.at(ny(MONDAY, 7, 5))
        self.proof(MONDAY)
        self.deliver(FRIDAY)
        self.assert_as_of_close(FRIDAY)

    def test_a_proof_ending_on_the_sunday_withholds_fridays_session_on_monday(self):
        self.at(ny(MONDAY, 9, 0))
        self.proof(date(2027, 1, 24), fetched=ny(MONDAY, 5, 30))
        self.deliver(FRIDAY)
        self.assert_withheld("2027-01-24T05:00:00Z")

    def test_the_day_after_a_holiday_delivers_the_session_before_it(self):
        self.at(ny(AFTER_HOLIDAY, 7, 5))
        self.proof(AFTER_HOLIDAY)
        self.deliver(BEFORE_HOLIDAY)
        self.assert_as_of_close(BEFORE_HOLIDAY)

    def test_a_holidays_proof_does_not_cover_the_day_after_it(self):
        self.at(ny(AFTER_HOLIDAY, 9, 0))
        self.proof(date(2027, 1, 18), fetched=ny(date(2027, 1, 18), 5, 30))
        self.deliver(BEFORE_HOLIDAY)
        self.assert_withheld("2027-01-18T05:00:00Z")


if __name__ == "__main__":
    unittest.main()
