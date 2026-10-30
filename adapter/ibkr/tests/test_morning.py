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
HOLIDAY = date(2027, 1, 18)
AFTER_HOLIDAY = date(2027, 1, 19)
EARLY = date(2027, 11, 26)
AFTER_EARLY = date(2027, 11, 29)


def commit_at(engine, ident, recorded_at, kind="test.input", payload=None, version=1):
    """An input committed to engine's durable log under ident, recorded at recorded_at (test_delivery.commit's)."""
    import hashlib
    payload = payload or {"input": ident}
    raw = json.dumps(payload, separators=(",", ":"))
    engine.decide({"id": ident, "type": kind, "envelope_version": 1, "schema_version": version,
                   "event_time": recorded_at, "recorded_at": recorded_at,
                   "sequence": engine.next_sequence, "source": "engine",
                   "configuration_hash": engine.configuration_hash, "strategy_version": engine.strategy_version,
                   "payload": payload, "payload_hash": hashlib.sha256(raw.encode()).hexdigest()})


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

    def test_a_fill_within_the_tolerance_two_runs_back_is_found_past_a_run_with_business_in_it(self):
        # For a 16:00 close, the oldest run's fill stamped 15:57 counts as at the close, and a run between them whose
        # business was recorded at 15:58 is within the same window: the walk goes past it to the fill.
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.window.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T20:57:00Z"}
        commit_at(self.engine, "fill:window", "2027-01-14T20:57:00Z", kind=FILL_TYPE, payload=payload, version=5)
        first = self.engine_log
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        between = LoggedEngine(middle, approval=run_approval(ACCOUNT, previous_log=RUN + ".log",
                                                             previous_log_final_hash=final_hash(first)))
        commit_at(between, "between:business", "2027-01-14T20:58:00Z")
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
        self.assert_withheld("journal", "0001f4e8.window.01.01", "within 5 minutes before the close")

    def chain_of_three(self, fill_recorded, fill_stamped, between_recorded, ident):
        """Run A holds one fill recorded at fill_recorded and stamped fill_stamped (a fast Gateway's, on an unchecked
        link); run B, its successor, holds business recorded at each of between_recorded; run C, B's successor, is
        the fresh run whose morning reading is delivered. Returns the reading's withheld reason."""
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": ident,
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": fill_stamped}
        commit_at(self.engine, "fill:" + ident, fill_recorded, kind=FILL_TYPE, payload=payload, version=5)
        first = self.engine_log
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        between = LoggedEngine(middle, approval=run_approval(ACCOUNT, previous_log=RUN + ".log",
                                                             previous_log_final_hash=final_hash(first)))
        for n, recorded in enumerate(between_recorded):
            commit_at(between, "between:business:{}".format(n), recorded)
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
        return self.assert_withheld("journal", ident)

    def test_a_fill_stamped_after_the_close_is_found_behind_a_run_that_ended_before_it(self):
        # A fast Gateway's stamp is not bounded by when the fill was recorded: a run whose business was all recorded
        # before the close window proves nothing of the stamps of the runs behind it, so the walk reads every run of
        # the chain. Run A's fill is recorded at 15:30 and stamped 18:30; run B's business is recorded at 15:40.
        self.chain_of_three("2027-01-14T20:30:00Z", "2027-01-14T23:30:00Z", ["2027-01-14T20:40:00Z"],
                            "0001f4e8.behind.01.01")

    def test_a_morning_fill_stamped_after_the_close_is_found_behind_a_day_of_business(self):
        # Run A's 10:00 fill stamped 17:00; run B's business from 10:10 to 15:50; run C from 15:52.
        self.chain_of_three("2027-01-14T15:00:00Z", "2027-01-14T22:00:00Z",
                            ["2027-01-14T15:10:00Z", "2027-01-14T18:00:00Z", "2027-01-14T20:50:00Z"],
                            "0001f4e8.morning.01.01")

    def mark_delivered(self, snapshot_id, run, checked=True):
        """The delivery record's batch for snapshot_id, as the adapter records one; checked marks it recorded under
        the journal check of the close window (delivery.JOURNAL_CHECKED), as this adapter records every batch."""
        from delivery import DELIVERED_SUFFIX, DeliveryRecord, JOURNAL_CHECKED
        record = DeliveryRecord(self.pending + DELIVERED_SUFFIX)
        batch = {"run": run, "snapshot": snapshot_id,
                 "files": [{"name": "{}-2027-01-13.jsonl".format(run), "sha256": "0" * 64, "lines": 1}]}
        if checked:
            batch["journal_checked"] = JOURNAL_CHECKED
        record.begin(batch)
        record.mark_queued()

    def archived_chain(self, snapshot_in_between, outbox_fill=False, as_of_close=True, marked=True,
                       starting_book=None, fill_after_snapshot=None, approved_at=None):
        """Run A, whose log is then archived (gone from the state directory); run B, its successor, holding business
        recorded on Wednesday and, given snapshot_in_between, a broker snapshot the engine committed stamped as of
        Wednesday's close (marked: recorded under the journal check of the close window), then any
        fill_after_snapshot payload; run C, B's successor, the run now running. starting_book is B's approval's
        stated book. Returns an AdapterLoop on C, not started."""
        first = self.engine_log
        commit_at(self.engine, "a:business", "2027-01-12T15:00:00Z")
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        approval = run_approval(ACCOUNT, previous_log=RUN + ".log", previous_log_final_hash=final_hash(first),
                                starting_book=starting_book)
        if approved_at is not None:
            approval["approved_at"] = approved_at
        between = LoggedEngine(middle, approval=approval)
        commit_at(between, "b:business", "2027-01-13T15:00:00Z")
        if snapshot_in_between:
            commit_at(between, "b:snapshot", "2027-01-13T22:05:00Z", kind=BROKER_SNAPSHOT_TYPE, version=1,
                      payload={"account_id": ACCOUNT, "as_of": "2027-01-13T21:00:00Z" if as_of_close else
                               "2027-01-13T22:05:00Z", "as_of_close": as_of_close,
                               "not_as_of_close": "" if as_of_close else "synthetic: withheld",
                               "read_at": "2027-01-13T22:05:00Z"})
            if marked:
                self.mark_delivered("b:snapshot", RUN + "-2")
        if fill_after_snapshot is not None:
            commit_at(between, "fill:" + fill_after_snapshot["fill_id"], "2027-01-14T21:00:00Z", kind=FILL_TYPE,
                      payload=fill_after_snapshot, version=5)
        fresh = RUN + "-3"
        self.engine_log = str(Path(self.dir.name) / (fresh + ".log"))
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, previous_log=RUN + "-2.log", previous_log_final_hash=final_hash(middle)))
        Path(first).unlink()
        loop = self.loop()
        if outbox_fill:
            payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.found.01.01",
                       "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                       "filled_at": "2027-01-14T21:30:00Z"}
            loop.outbox.append({"id": "fill:found", "event_type": FILL_TYPE, "schema_version": 1,
                                "event_time": "2027-01-14T21:30:00Z", "payload": payload})
        return loop

    def test_an_archived_log_behind_a_run_with_a_close_stamped_snapshot_is_never_read(self):
        # The snapshot as of Wednesday's close stood only because its own journal check, reading every run behind
        # it, found no fill stamped from Wednesday's close window on: every older stamp is before it, so before
        # Thursday's too, and the walk stops at that run (ADR 0019, amended 2026-10-28).
        loop = self.archived_chain(snapshot_in_between=True)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertEqual(loop._journal_fills(close), ([], ""))

    def test_a_snapshot_not_recorded_under_the_close_window_check_does_not_end_the_walk(self):
        # A close-stamped snapshot recorded before the journal check of the close window (an earlier adapter's, not
        # marked in the delivery record) proves nothing of the journal behind it: the walk reads on.
        loop = self.archived_chain(snapshot_in_between=True, marked=False)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertIn("the chain of runs' logs cannot be read", loop._journal_fills(close)[1])

    def test_a_window_fill_behind_a_legacy_snapshot_is_found(self):
        # Run A's fill is stamped in Thursday's window; run B holds a close-stamped snapshot of Wednesday recorded by
        # an earlier adapter, never marked: the walk reads past it to A.
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.legacy.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00Z"}
        commit_at(self.engine, "fill:legacy", "2027-01-13T15:00:00Z", kind=FILL_TYPE, payload=payload, version=5)
        first = self.engine_log
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        between = LoggedEngine(middle, approval=run_approval(ACCOUNT, previous_log=RUN + ".log",
                                                             previous_log_final_hash=final_hash(first)))
        commit_at(between, "b:snapshot", "2027-01-13T22:05:00Z", kind=BROKER_SNAPSHOT_TYPE, version=2,
                  payload={"account_id": ACCOUNT, "as_of": "2027-01-13T21:00:00Z", "as_of_close": True,
                           "not_as_of_close": "", "read_at": "2027-01-13T22:05:00Z"})
        fresh = RUN + "-3"
        self.engine_log = str(Path(self.dir.name) / (fresh + ".log"))
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, previous_log=RUN + "-2.log", previous_log_final_hash=final_hash(middle)))
        loop = self.loop()
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertIn("0001f4e8.legacy.01.01", " ".join(loop._journal_fills(close)[0]))

    def test_a_window_fill_in_the_bound_run_after_its_snapshot_is_found(self):
        # The run holding the bound snapshot is itself read in full: a fill it journalled after that snapshot, in
        # Thursday's window, is named.
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.after.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T20:58:00Z"}
        loop = self.archived_chain(snapshot_in_between=True, fill_after_snapshot=payload)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        fills, refused = loop._journal_fills(close)
        self.assertEqual(refused, "")
        self.assertIn("0001f4e8.after.01.01", " ".join(fills))

    def test_a_fresh_run_approved_with_a_starting_book_ends_the_walk(self):
        # The owner's stated book is the account at the approval (ADR 0027): the runs behind it are not read, so a
        # lost log behind it withholds nothing. The way out of a log that cannot be restored.
        loop = self.archived_chain(snapshot_in_between=False, starting_book=({"SPY": 147}, 100000.0))
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertEqual(loop._journal_fills(close), ([], ""))

    def test_a_starting_book_stated_after_the_close_does_not_end_the_walk(self):
        # A book stated after the close may hold a fill after it, from a run behind the approval: the walk reads on.
        loop = self.archived_chain(snapshot_in_between=False, starting_book=({"SPY": 147}, 100000.0),
                                   approved_at="2027-01-14T23:00:00Z")
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertIn("the chain of runs' logs cannot be read", loop._journal_fills(close)[1])

    def test_a_run_approved_without_a_starting_book_does_not_end_the_walk(self):
        loop = self.archived_chain(snapshot_in_between=False)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertIn("the chain of runs' logs cannot be read", loop._journal_fills(close)[1])

    def test_a_withheld_snapshot_does_not_end_the_walk(self):
        # A reading stamped at its read time proves nothing of the journal behind it.
        loop = self.archived_chain(snapshot_in_between=True, as_of_close=False)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        self.assertIn("the chain of runs' logs cannot be read", loop._journal_fills(close)[1])

    def test_an_archived_log_before_the_nearest_close_stamped_snapshot_withholds(self):
        loop = self.archived_chain(snapshot_in_between=False)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        fills, refused = loop._journal_fills(close)
        self.assertIn("the chain of runs' logs cannot be read", refused)

    def test_fills_found_before_the_chain_breaks_are_reported_with_it(self):
        loop = self.archived_chain(snapshot_in_between=False, outbox_fill=True)
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        reason = loop._journal_reason(close)
        self.assertIn("the chain of runs' logs cannot be read", reason)
        self.assertIn("0001f4e8.found.01.01 (stamped 2027-01-14T21:30:00Z, at or after the close)", reason)
        self.assertIn("the journal keeps them", reason)

    def test_a_reading_whose_journal_walk_alone_needs_a_missing_log_is_sent_withheld_naming_it(self):
        # End to end: run B holds a withheld reading's account snapshot and its reconciliation, whose cash the engine
        # explained, so the comparison with the previous close stops in B; no reading in B stood for a close, so the
        # journal check reads on to run A, whose log is gone.
        from book_basis import RECONCILIATION_RESULT_TYPE
        self.proof(FRIDAY_MORNING)
        first = self.engine_log
        commit_at(self.engine, "a:business", "2027-01-12T15:00:00Z")
        middle = str(Path(self.dir.name) / (RUN + "-2.log"))
        between = LoggedEngine(middle, approval=run_approval(ACCOUNT, previous_log=RUN + ".log",
                                                             previous_log_final_hash=final_hash(first)))
        recon = "b:wed:account.reconciliation"
        between.decisions[recon] = [{"type": RECONCILIATION_RESULT_TYPE, "payload": {
            "input_id": recon, "classification": "matched", "cash_compared": True}}]
        commit_at(between, "b:wed:account.snapshot", "2027-01-13T22:05:00Z", kind=ACCOUNT_SNAPSHOT, version=3,
                  payload={"account_id": ACCOUNT, "as_of": "2027-01-13T22:05:00Z",
                           "positions": [{"instrument_id": "SPY", "quantity": 147}], "net_liquidation": 145937.5})
        commit_at(between, recon, "2027-01-13T22:05:00Z", kind="account.reconciliation", version=1,
                  payload={"account_id": ACCOUNT, "as_of": "2027-01-13T22:05:00Z", "total_cash": 100000.0,
                           "unreadable": []})
        fresh = RUN + "-3"
        self.engine_log = str(Path(self.dir.name) / (fresh + ".log"))
        self.engine = LoggedEngine(self.engine_log, approval=run_approval(
            ACCOUNT, previous_log=RUN + "-2.log", previous_log_final_hash=final_hash(middle)))
        Path(first).unlink()
        self.folder = self.streams / fresh
        write_stream(self.folder, THURSDAY, session_envelopes([THURSDAY], run=fresh))
        loop = self.loop()
        loop.start()
        for _ in range(40):
            loop.step()
        self.assert_withheld("the chain of runs' logs cannot be read, so their fills cannot be checked")

    def test_a_reading_over_a_chain_with_a_missing_log_stops_the_adapter_naming_it(self):
        # End to end, no stand-in for the chain: a log gone from the state directory that the reading needs. The
        # comparison with the previous close (book_basis) reads the same chain back to the last account snapshot or
        # starting book, and stops the adapter on it before anything is sent; the journal check's reason
        # (test_an_archived_log_before_the_nearest_close_stamped_snapshot_withholds) is for a log only it reads.
        from relay import ResolveRefused
        self.proof(FRIDAY_MORNING)
        loop = self.archived_chain(snapshot_in_between=False)
        self.folder = self.streams / (RUN + "-3")
        write_stream(self.folder, THURSDAY, session_envelopes([THURSDAY], run=RUN + "-3"))
        loop.start()
        with self.assertRaises(ResolveRefused) as caught:
            for _ in range(40):
                loop.step()
        self.assertIn("engine.log", str(caught.exception))
        self.assertIn("put it back", str(caught.exception))
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE), [], "nothing is sent")

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

    def test_clock_recheck_detects_an_hour_jump_before_the_next_morning_reading(self):
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.start()
        self.assertEqual(loop._gateway_zone, "")
        self.at(ny(FRIDAY_MORNING, 8, 5))
        # The Gateway now runs an hour slow; its next handshake carries that clock.
        self.fake.connect_hook = lambda: setattr(self.fake, "tws_connection_time", "20270115 07:05:00 EST")
        self.proof(FRIDAY_MORNING)
        with self.assertLogs("ibkr", "INFO") as logs:
            self.deliver(THURSDAY, loop)
        self.assert_withheld("IB Gateway's connection time", "60 minutes behind")
        self.assertIsNone(loop.conn.gateway_clock_offset)
        connects = [c for c in self.fake.calls if c[0] == "connect"]
        self.assertEqual([c[-1] for c in connects], [11, 11])
        observations = [line for line in logs.output if "ibkr gateway clock recheck" in line]
        self.assertEqual(len(observations), 1, observations)
        self.assertIn("offset_seconds=-3600", observations[0])
        self.assertIn("verdict=unchecked", observations[0])

    def test_clock_recheck_within_tolerance_keeps_the_reading_at_the_close(self):
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.start()
        self.at(ny(FRIDAY_MORNING, 8, 5))
        self.fake.connect_hook = lambda: setattr(self.fake, "tws_connection_time", "20270115 08:03:00 EST")
        self.proof(FRIDAY_MORNING)
        with self.assertLogs("ibkr", "INFO") as logs:
            self.deliver(THURSDAY, loop)
        self.assert_as_of_close(THURSDAY)
        self.assertIsNone(loop.tracker._unverified_receipt())
        self.assertEqual(len([c for c in self.fake.calls if c[0] == "connect"]), 2)
        observations = [line for line in logs.output if "ibkr gateway clock recheck" in line]
        self.assertEqual(len(observations), 1, observations)
        self.assertIn("offset_seconds=-120", observations[0])
        self.assertIn("verdict=checked", observations[0])
        self.assertFalse(any(c[0] == "place_order" for c in self.fake.calls))

    def test_clock_recheck_expiry_stamps_executions_at_receipt_before_any_reading(self):
        from orders import collect_broker_orders
        self.at(ny(THURSDAY, 15))
        loop = self.loop()
        loop.start()
        self.at(ny(THURSDAY, 16, 30))
        self.assertEqual(loop.tracker._unverified_receipt(), self.now)
        self.fake.executions = [(contract(), execution(time="20270114 15:30:00 EST"))]
        self.assertEqual(collect_broker_orders(loop.conn).executions[0].occurred_at, self.now)
        self.assertIn("stale", loop._gateway_problem())

    def test_clock_recheck_does_not_extend_a_cached_handshakes_lifetime(self):
        from datetime import timedelta
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.start()
        first = loop.conn.connected_at
        self.at(ny(FRIDAY_MORNING, 7, 59))
        loop.reconcile()
        self.assertEqual(loop.conn.connected_at, first)
        self.assertIsNone(loop.tracker._unverified_receipt())
        self.at(ny(FRIDAY_MORNING, 8, 1))
        self.assertEqual(loop.tracker._unverified_receipt(), self.now)
        loop.reconcile()
        self.assertEqual(loop.conn.connected_at, first + timedelta(minutes=61))
        self.assertIsNone(loop.tracker._unverified_receipt())

    def test_clock_recheck_uses_the_configured_interval_and_includes_its_boundary(self):
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.config = loop.config._replace(gateway_clock_check_interval=120)
        loop.start()
        self.at(ny(FRIDAY_MORNING, 7, 2))
        loop.reconcile()
        self.assertIsNone(loop.tracker._unverified_receipt())
        self.assertEqual(len([c for c in self.fake.calls if c[0] == "connect"]), 1)
        self.at(ny(FRIDAY_MORNING, 7, 3))
        loop.reconcile()
        self.assertIsNone(loop.tracker._unverified_receipt())
        self.assertEqual(len([c for c in self.fake.calls if c[0] == "connect"]), 2)

    def test_clock_recheck_monotonic_expiry_cannot_be_extended_by_wall_clock_rollback(self):
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.start()
        self.clock.now += 3601
        # The wall clock still reads 07:00; elapsed monotonic time expires the check anyway.
        self.assertEqual(loop.tracker._unverified_receipt(), self.now)
        loop.reconcile()
        self.assertIsNone(loop.tracker._unverified_receipt())
        self.assertEqual(len([c for c in self.fake.calls if c[0] == "connect"]), 2)

    def test_clock_recheck_failed_reconnect_logs_unchecked_and_sends_nothing(self):
        from connection import ConnectFailed
        self.at(ny(FRIDAY_MORNING, 7))
        loop = self.loop()
        loop.start()
        self.at(ny(FRIDAY_MORNING, 8, 5))
        self.proof(FRIDAY_MORNING)
        self.fake.connect_behaviours = ["socket-error"] * loop.config.max_connect_attempts
        with self.assertLogs("ibkr", "INFO") as logs, self.assertRaises(ConnectFailed):
            loop._post_close_reading(nyse_calendar.session_close(THURSDAY), {"SPY": OFFICIAL})
        self.assertIsNone(loop.conn.gateway_clock_offset)
        self.assertEqual(loop.tracker._unverified_receipt(), self.now)
        self.assertTrue(any("clock recheck" in line and "verdict=unchecked" in line for line in logs.output))
        self.assertEqual(self.sent(ACCOUNT_SNAPSHOT), [])

    # -- IB Gateway's zone: where IBKR's own list starts -------------------------
    def test_a_gateway_in_another_zone_withholds_with_no_execution_listed(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 05:05:00 US/Pacific"
        self.deliver(THURSDAY)
        self.assert_withheld("US/Pacific", "New York")

    def test_ibkrs_est_label_in_summer_time_on_new_yorks_clock_is_new_york(self):
        # IB Gateway 10.50 on TZ=America/New_York labelled 09:49:19 EDT "EST" (2026-10-28): IBKR's short label does not
        # follow daylight saving time, so the clock, not the label's season, shows the Gateway is on New York time.
        self.at(datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc))
        self.fake.tws_connection_time = "20261028 09:49:19 EST"
        loop = self.loop()
        loop.start()
        self.assertEqual(loop._gateway_zone, "")

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

    def test_the_eastern_label_out_of_season_with_new_yorks_clock_is_new_york(self):
        # IBKR's short label does not follow daylight saving time (ADR 0019, amended 2026-10-28): EDT in January on a
        # clock that reads New York's time is New York's.
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 08:05:00 EDT"
        self.deliver(THURSDAY)
        self.assert_as_of_close(THURSDAY)

    def test_a_gateway_on_fixed_eastern_standard_time_in_summer_withholds(self):
        # A clock on EST all year reads 08:49:19 at 09:49:19 New York (EDT): an hour behind.
        self.at(datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc))
        self.fake.tws_connection_time = "20261028 08:49:19 EST"
        loop = self.loop()
        with self.assertLogs("ibkr", "INFO") as logs:
            loop.start()
        self.assertIn("20261028 08:49:19 EST", loop._gateway_zone)
        self.assertIn("60 minutes 3 seconds behind", loop._gateway_zone)
        self.assertIn("not on New York time", loop._gateway_zone)
        self.assertIn("20261028 08:49:19 EST", "\n".join(logs.output))

    def test_a_gateway_clock_far_from_the_adapters_withholds(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 05:05:00 EST"
        self.deliver(THURSDAY)
        self.assert_withheld("IB Gateway's connection time '20270115 05:05:00 EST'", "180 minutes behind")

    def test_the_clock_is_compared_at_the_handshake_not_at_a_later_reconcile(self):
        # Within the check interval a link restored by IB Gateway (1101/1102) keeps its handshake:
        # compare its connection time with the adapter's clock when that link connected.
        self.at(datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc))
        self.fake.tws_connection_time = "20261028 09:49:19 EST"
        loop = self.loop()
        loop.start()
        self.at(datetime(2026, 10, 28, 14, 19, 22, tzinfo=timezone.utc))
        loop.reconcile()
        self.assertEqual(loop._gateway_zone, "")

    def test_a_gateway_that_kept_its_connection_time_is_trusted_at_a_later_reconcile(self):
        # The fake Gateway, like IB Gateway, gives one connection time per session, however late it is asked.
        self.at(datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc))
        loop = self.loop()
        loop.start()
        self.at(datetime(2026, 10, 28, 15, 49, 22, tzinfo=timezone.utc))
        loop.reconcile()
        self.assertEqual(loop._gateway_zone, "")

    def test_a_passed_clock_check_marks_the_link_and_a_failed_one_clears_it(self):
        from datetime import timedelta
        self.at(datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc))
        self.fake.tws_connection_time = "20261028 09:49:19 EST"
        loop = self.loop()
        loop.start()
        self.assertEqual(loop.conn.gateway_clock_offset, timedelta(hours=-4))
        self.assertIsNone(loop.tracker._unverified_receipt(), "the tracker believes the Gateway's execution times")
        self.fake.tws_connection_time = "20261028 08:49:19 EST"
        self.fake.drop()
        for _ in range(10):
            loop.step()
        self.assertTrue(loop.ready(), "setup: reconnected and reconciled")
        self.assertIsNone(loop.conn.gateway_clock_offset)
        self.assertEqual(loop.tracker._unverified_receipt(), self.now, "nor once the check fails: its receipt time")

    def test_a_clock_checked_before_new_yorks_clocks_change_is_not_trusted_after(self):
        # A Gateway on fixed EST matches New York's clock until the clocks go forward (2027-03-14): a link that
        # connected before then, kept or restored by Gateway (1101/1102) with no new handshake, is not trusted after.
        from datetime import timedelta
        self.at(datetime(2027, 3, 12, 14, 0, tzinfo=timezone.utc))
        self.fake.tws_connection_time = "20270312 09:00:00 EST"
        loop = self.loop()
        loop.start()
        self.assertEqual((loop._gateway_zone, loop.conn.gateway_clock_offset), ("", timedelta(hours=-5)))
        self.at(datetime(2027, 3, 15, 12, 5, tzinfo=timezone.utc))
        problem = loop._gateway_problem()
        for text in ("New York's clocks changed", "UTC-05:00", "UTC-04:00", "connects again"):
            self.assertIn(text, problem)
        # The expired link now reconnects; fixed EST is an hour slow in the new handshake too.
        self.fake.connect_hook = lambda: setattr(self.fake, "tws_connection_time", "20270315 07:05:00 EST")
        loop.reconcile()
        self.assertIn("60 minutes behind", loop._gateway_zone)
        self.assertIsNone(loop.conn.gateway_clock_offset)

    def test_a_morning_reading_on_a_link_whose_clock_check_failed_withholds_on_the_connection_time(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 07:05:00 EST"
        loop = self.loop()
        loop.start()
        self.deliver(THURSDAY, loop)
        self.assert_withheld("IB Gateway's connection time '20270115 07:05:00 EST'", "60 minutes behind")

    def test_an_execution_on_a_link_whose_clock_failed_its_check_is_never_before_the_close(self):
        # A Gateway an hour slow in winter stamps a 16:30 New York execution "15:30:00 EST": every reading of its
        # label is 20:30Z, before the 21:00Z close. On a link whose clock check failed, the adapter's own receipt time
        # (an upper bound on when it happened) stands for it, so a same-date reading finds it after the close (ADR
        # 0019, amended 2026-10-28; AGENTS.md: fail closed). Read as the Gateway gave it, this fails.
        self.at(ny(THURSDAY, 17, 0))
        self.fake.tws_connection_time = "20270114 16:00:00 EST"
        loop = self.loop()
        loop.start()
        self.assertIn("60 minutes behind", loop._gateway_zone, "setup: the check failed")
        self.fake.executions = [(contract(), execution(exec_id="0008.slow.01.01", order_id=0, perm_id=7474,
                                                       order_ref="", client_id=0, time="20270114 15:30:00 EST"))]
        self.deliver(THURSDAY, loop)
        reason = self.assert_withheld("since the close", "0008.slow.01.01")
        # The cause comes first: the Gateway's clock, not a trade after the close.
        self.assertTrue(reason.startswith("IB Gateway's connection time '20270114 16:00:00 EST'"), reason)
        self.assertIn("taken at the time the adapter received", reason)

    def test_the_journal_reason_says_which_side_of_the_close_each_fill_is_and_what_it_costs(self):
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        for ident, filled_at in (("0001f4e8.near.01.01", "2027-01-14T20:57:00Z"),
                                 ("0001f4e8.after.01.01", "2027-01-14T21:30:00Z")):
            payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": ident,
                       "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                       "filled_at": filled_at}
            loop.outbox.append({"id": "fill:" + ident, "event_type": FILL_TYPE, "schema_version": 1,
                                "event_time": filled_at, "payload": payload})
        found, refused = loop._journal_fills(close)
        self.assertEqual(refused, "")
        self.assertIn("0001f4e8.near.01.01 (stamped 2027-01-14T20:57:00Z, within 5 minutes before the close)", found)
        self.assertIn("0001f4e8.after.01.01 (stamped 2027-01-14T21:30:00Z, at or after the close)", found)

    def test_a_chain_that_cannot_be_read_is_not_reported_as_a_fill_kept_for_good(self):
        from relay import ResolveRefused
        import loop as loop_module
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)

        def refused(path):
            raise ResolveRefused("synthetic: the log breaks its chain")
        original = loop_module.run_chain
        loop_module.run_chain = refused
        self.addCleanup(setattr, loop_module, "run_chain", original)
        reason = loop._journal_reason(close)
        self.assertIn("the chain of runs' logs cannot be read", reason)
        self.assertNotIn("fill(s)", reason)
        self.assertNotIn("keeps them", reason)

    def test_a_journalled_fill_in_the_window_withholds_the_session_for_good(self):
        self.proof(FRIDAY_MORNING)
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.near.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T20:57:00Z"}
        commit_at(self.engine, "fill:near", "2027-01-14T20:57:00Z", kind=FILL_TYPE, payload=payload, version=5)
        self.fake.positions = [(ACCOUNT, contract(), Decimal("148"), 312.5)]
        self.fake.summary = [(a, t, "99597.75" if t == "TotalCashValue" else v, c) for a, t, v, c in SUMMARY]
        self.deliver(THURSDAY)
        reason = self.assert_withheld("0001f4e8.near.01.01 (stamped 2027-01-14T20:57:00Z, within 5 minutes before "
                                      "the close)")
        self.assertIn("the journal keeps them, so this session's reading stays withheld", reason)

    def test_a_same_date_reading_finds_a_fill_journalled_after_the_close(self):
        # A fill journalled on an unchecked link whose Gateway was fast is stamped after the close; IBKR re-lists it on
        # a corrected link at its true time, before the close. The journal's stamp is the one the engine applies on top
        # of the close's book, so a same-date reading that stood would count the fill twice.
        self.at(ny(THURSDAY, 17, 0))
        loop = self.loop()
        loop.start()
        self.assertEqual(loop._gateway_zone, "", "setup: the corrected link's check holds")
        payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": "0001f4e8.fast.01.01",
                   "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                   "filled_at": "2027-01-14T21:30:00Z"}
        loop.outbox.append({"id": "fill:fast", "event_type": FILL_TYPE, "schema_version": 1,
                            "event_time": "2027-01-14T21:30:00Z", "payload": payload})
        # The reading shows the fill: one more share, and the cash it cost.
        self.fake.positions = [(ACCOUNT, contract(), Decimal("148"), 312.5)]
        self.fake.summary = [(a, t, "99597.75" if t == "TotalCashValue" else v, c) for a, t, v, c in SUMMARY]
        self.fake.executions = [(contract(), execution(exec_id="0001f4e8.fast.01.01", order_id=0, perm_id=7676,
                                                       order_ref="", client_id=0, time="20270114 15:30:00 EST"))]
        self.deliver(THURSDAY, loop)
        reason = self.assert_withheld("journal", "0001f4e8.fast.01.01")
        self.assertNotIn("since the close", reason, "IBKR's own list shows it before the close")

    def test_a_same_date_reading_on_an_unchecked_link_with_no_execution_stands(self):
        # The clock matters only to the executions' times: with none listed, the account read stands for the close.
        self.at(ny(THURSDAY, 17, 0))
        self.fake.tws_connection_time = "20270114 16:00:00 EST"
        loop = self.loop()
        loop.start()
        self.deliver(THURSDAY, loop)
        self.assert_as_of_close(THURSDAY)

    def test_an_execution_within_the_clock_tolerance_before_the_close_is_taken_as_at_it(self):
        # A Gateway up to GATEWAY_CLOCK_TOLERANCE slow passes its check, so a 16:02 execution may be stamped 15:58:
        # one stamped within the tolerance before the close is taken as at the close (ADR 0019, amended 2026-10-28).
        for stamped, listed in (("15:58:00", True), ("15:55:00", True), ("15:54:59", False)):
            with self.subTest(stamped=stamped):
                self.setUp()
                self.at(ny(THURSDAY, 17, 0))
                loop = self.loop()
                loop.start()
                self.assertEqual(loop._gateway_zone, "", "setup: the clock check holds")
                self.fake.executions = [(contract(), execution(
                    exec_id="0009.edge.01.01", order_id=0, perm_id=7575, order_ref="", client_id=0,
                    time="20270114 {} US/Eastern".format(stamped)))]
                self.deliver(THURSDAY, loop)
                if listed:
                    self.assert_withheld("since the close", "0009.edge.01.01", "within 5 minutes before it")
                else:
                    self.assert_as_of_close(THURSDAY)

    def test_a_journalled_fill_within_the_clock_tolerance_before_the_close_is_taken_as_at_it(self):
        self.proof(FRIDAY_MORNING)
        loop = self.loop()
        loop.start()
        close = nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc)
        for n, (filled_at, listed) in enumerate((("2027-01-14T20:58:00Z", True), ("2027-01-14T20:55:00Z", True),
                                                 ("2027-01-14T20:54:59Z", False))):
            ident = "0001f4e8.edge{}.01.01".format(n)
            payload = {"account_id": ACCOUNT, "client_order_id": "tw-synthetic", "fill_id": ident,
                       "instrument_id": "SPY", "side": "buy", "quantity": 1, "price": 401.25, "commission": 1.0,
                       "filled_at": filled_at}
            loop.outbox.append({"id": "fill:edge{}".format(n), "event_type": FILL_TYPE, "schema_version": 1,
                                "event_time": filled_at, "payload": payload})
            with self.subTest(filled_at=filled_at):
                self.assertEqual(ident in " ".join(loop._journal_fills(close)[0]), listed)

    def test_the_same_execution_on_a_link_whose_clock_holds_is_read_as_the_gateway_gave_it(self):
        # The control: with the check passed, "15:30:00 EST" is 20:30Z, before the close, and not listed after it.
        self.at(ny(THURSDAY, 17, 0))
        loop = self.loop()
        loop.start()
        self.assertEqual(loop._gateway_zone, "")
        self.fake.executions = [(contract(), execution(exec_id="0008.slow.01.01", order_id=0, perm_id=7474,
                                                       order_ref="", client_id=0, time="20270114 15:30:00 EST"))]
        self.deliver(THURSDAY, loop)
        self.assertNotIn("0008.slow.01.01", self.broker()["not_as_of_close"])

    def test_the_connection_time_is_compared_with_the_adapters_clock_within_the_tolerance(self):
        from loop import GATEWAY_CLOCK_TOLERANCE, gateway_clock_problem
        from datetime import timedelta
        self.assertEqual(GATEWAY_CLOCK_TOLERANCE, timedelta(minutes=5))
        at = datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc)
        for text in ("20261028 09:49:19 EST", "20261028 09:49:19 EDT", "20261028 09:49:19 US/Eastern",
                     "20261028 09:49:19 America/New_York", "20261028 09:49:19 EST5EDT",
                     "20261028 09:49:19 Eastern Standard Time", "20261028 09:49:19 Eastern Daylight Time",
                     "20261028 09:44:23 EST", "20261028 09:54:21 EST"):
            with self.subTest(text=text):
                self.assertEqual(gateway_clock_problem(text, at), "")
        for text, off in (("20261028 08:49:19 EST", "60 minutes 3 seconds behind"),
                          ("20261028 09:44:21 EST", "5 minutes 1 seconds behind"),
                          ("20261028 09:54:23 EDT", "5 minutes 1 seconds ahead of"),
                          ("20261028 06:49:19 US/Eastern", "180 minutes 3 seconds behind")):
            with self.subTest(text=text):
                problem = gateway_clock_problem(text, at)
                self.assertIn(text, problem)
                self.assertIn(off, problem)
        winter = datetime(2027, 1, 15, 13, 5, tzinfo=timezone.utc)
        for zone in ("EST", "EDT", "US/Eastern"):
            with self.subTest(winter=zone):
                self.assertEqual(gateway_clock_problem("20270115 08:05:00 " + zone, winter), "")
        problem = gateway_clock_problem("20270115 09:05:00 EDT", winter)
        self.assertIn("60 minutes ahead", problem)

    def test_the_clock_picks_the_reading_of_the_repeated_autumn_hour(self):
        # 01:30 on 2026-11-01 happens twice in New York, at 05:30 and at 06:30 UTC: the reading near the adapter's
        # clock is the one taken, whatever the label, and neither near it withholds.
        from loop import gateway_clock_problem
        for utc_hour in (5, 6):
            at = datetime(2026, 11, 1, utc_hour, 30, 3, tzinfo=timezone.utc)
            for zone in ("EST", "EDT", "US/Eastern", "Eastern Daylight Time"):
                with self.subTest(utc_hour=utc_hour, zone=zone):
                    self.assertEqual(gateway_clock_problem("20261101 01:30:00 " + zone, at), "")
        problem = gateway_clock_problem("20261101 01:30:00 EST", datetime(2026, 11, 1, 7, 30, tzinfo=timezone.utc))
        self.assertIn("60 minutes behind", problem)

    def test_a_connection_with_no_handshake_time_withholds(self):
        from loop import gateway_clock_problem
        problem = gateway_clock_problem("20261028 09:49:19 EST", None)
        self.assertIn("20261028 09:49:19 EST", problem)
        self.assertIn("the adapter's clock at the handshake", problem)

    def test_the_connection_time_in_another_zone_is_refused_before_its_clock(self):
        from loop import gateway_clock_problem
        at = datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc)
        self.assertIn("US/Pacific", gateway_clock_problem("20261028 06:49:19 US/Pacific", at))
        self.assertIn("did not happen", gateway_clock_problem("20260308 02:30:00 EST",
                                                              datetime(2026, 3, 8, 7, 30, tzinfo=timezone.utc)))

    def test_either_label_stands_in_the_hour_repeated_when_daylight_saving_ends(self):
        # 01:30 on 2026-11-01 happens twice in New York: an execution's time there is New York's whatever its label.
        from loop import gateway_zone_problem
        for text in ("20261101 01:30:00 EST", "20261101 01:30:00 EDT", "20261101 01:30:00 Eastern Standard Time",
                     "20261101 01:30:00 Eastern Daylight Time", "20261101 02:30:00 EDT"):
            with self.subTest(text=text):
                self.assertEqual(gateway_zone_problem(text), "")

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

    def test_a_time_at_the_edge_of_the_calendar_withholds_and_never_raises(self):
        from loop import gateway_clock_problem, gateway_zone_problem
        at = datetime(2026, 10, 28, 13, 49, 22, tzinfo=timezone.utc)
        for text in ("99991231 23:59:59 US/Eastern", "99991231 23:59:59 EST", "00010101 00:00:00 EDT"):
            with self.subTest(text=text):
                problem = gateway_zone_problem(text)
                if text.startswith("9999"):
                    self.assertIn(text, problem, "a time whose UTC instant is past the calendar's end")
                self.assertIn(text, gateway_clock_problem(text, at))

    def test_new_yorks_long_zone_name_in_force_is_new_york(self):
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 08:05:00 Eastern Standard Time"
        self.deliver(THURSDAY)
        self.assert_as_of_close(THURSDAY)

    def test_a_long_zone_name_off_new_yorks_clock_withholds_showing_the_raw_text(self):
        self.proof(FRIDAY_MORNING)
        raw = "20270115 09:05:00 Eastern Daylight Time"
        self.fake.tws_connection_time = raw
        with self.assertLogs("ibkr", "INFO") as logs:
            self.deliver(THURSDAY)
        self.assertIn(raw, self.assert_withheld("IB Gateway's connection time", "60 minutes ahead"))
        self.assertIn(raw, "\n".join(logs.output), "the journal shows the Gateway's text as it gave it")

    def test_an_execution_time_is_checked_as_the_connection_time_is(self):
        # An execution's time under either Eastern label, or a New York zone name, stands whatever the season; one in
        # another zone, or skipped by New York's clocks, does not.
        for time, stands in (("20270114 10:00:01 EST", True), ("20270114 10:00:01 EDT", True),
                             ("20270114 10:00:01 Eastern Daylight Time", True), ("20270114 10:00:01 CST", False),
                             ("20260308 02:30:00 EDT", False)):
            with self.subTest(time=time):
                self.setUp()
                self.proof(FRIDAY_MORNING)
                loop = self.loop()
                loop.start()
                ex = execution(exec_id="0006.abbr.01.01", order_id=0, perm_id=7272, order_ref="", client_id=0,
                               time=time)
                loop._count_execution(SimpleNamespace(name="exec_details", args=(-1, contract(), ex)))
                self.deliver(THURSDAY, loop)
                if stands:
                    self.assert_as_of_close(THURSDAY)
                else:
                    self.assert_withheld("IBKR's execution time", time)

    def test_a_corrected_gateway_is_trusted_again_after_a_reconnect(self):
        # The zone, and every execution zone seen, belong to one connection: a reconnect reads them afresh.
        self.proof(FRIDAY_MORNING)
        self.fake.tws_connection_time = "20270115 07:00:00 US/Pacific"
        loop = self.loop()
        loop.start()
        ex = execution(exec_id="0005.zone.01.01", order_id=0, perm_id=7171, order_ref="", client_id=0,
                       time="20270114 10:00:01 US/Pacific")
        loop._count_execution(SimpleNamespace(name="exec_details", args=(-1, contract(), ex)))
        self.fake.tws_connection_time = "20270115 08:05:00 US/Eastern"
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
        self.assertIn("0001f4e8.nozone.01.01", " ".join(loop._journal_fills(
            nyse_calendar.session_close(THURSDAY).astimezone(timezone.utc))[0]))

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

    def test_a_holiday_mornings_proof_ending_on_the_session_before_waits_then_withholds(self):
        # The holiday's firing (Monday 2027-01-18): the Flex reader's statement ends on Friday, the session before,
        # so it covers only to Friday's start; Friday's file delivered that morning waits for a proof that reaches
        # the holiday's midnight, and at 09:00 is withheld.
        self.at(ny(HOLIDAY, 8, 5))
        self.proof(BEFORE_HOLIDAY, fetched=ny(HOLIDAY, 5, 30), from_day=date(2027, 1, 11))
        loop = self.deliver(BEFORE_HOLIDAY)
        self.assertEqual(self.sent(BROKER_SNAPSHOT_TYPE), [], "the batch waits for a proof that reaches midnight")
        self.at(ny(HOLIDAY, 9, 0))
        for _ in range(40):
            loop.step()
        self.assert_withheld("2027-01-15T05:00:00Z", "2027-01-18T05:00:00Z")

    def test_a_holidays_proof_does_not_cover_the_day_after_it(self):
        self.at(ny(AFTER_HOLIDAY, 9, 0))
        self.proof(date(2027, 1, 18), fetched=ny(date(2027, 1, 18), 5, 30))
        self.deliver(BEFORE_HOLIDAY)
        self.assert_withheld("2027-01-18T05:00:00Z")


if __name__ == "__main__":
    unittest.main()
