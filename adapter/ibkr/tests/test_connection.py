"""Connecting to IB Gateway, reconnecting with backoff, and detecting a lost link."""
import logging
import sys
import unittest
from pathlib import Path

tests_dir = str(Path(__file__).resolve().parent)
if tests_dir not in sys.path:
    sys.path.insert(0, tests_dir)

from fake_tws import FakeTWS, ACCOUNT
from config import AdapterConfig
from connection import Connection, ClientIdInUse, ConnectFailed, NotConnected


def config(**overrides):
    values = dict(host="127.0.0.1", port=4002, client_id=11, account_id=ACCOUNT, mode="dry-run",
                  connect_timeout=0.05, request_timeout=0.05, heartbeat_interval=30.0,
                  heartbeat_timeout=10.0, restore_timeout=60.0, backoff_initial=1.0,
                  backoff_max=8.0, max_connect_attempts=5)
    values.update(overrides)
    return AdapterConfig.from_mapping(values)


class Clock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now


class Harness:
    def __init__(self, **overrides):
        self.fake = FakeTWS()
        self.statuses = []
        self.sleeps = []
        self.clock = Clock()

        def sleep(seconds):
            self.sleeps.append(seconds)
            self.clock.now += seconds

        self.conn = Connection(self.fake, config(**overrides), status_sink=self.statuses.append,
                               sleep=sleep, monotonic=self.clock)

    def states(self):
        return [s.state for s in self.statuses]


class ConnectTests(unittest.TestCase):
    def test_connects_with_the_configured_host_port_and_client_id(self):
        h = Harness()
        h.conn.connect()
        self.assertEqual(h.fake.called("connect"), [("connect", "127.0.0.1", 4002, 11)])
        self.assertTrue(h.conn.connected)
        self.assertEqual(h.conn.next_order_id, 7)
        self.assertEqual(h.states(), ["connected"])
        self.assertEqual(h.statuses[0].client_id, 11)

    def test_every_connect_requires_reconciliation_before_orders(self):
        h = Harness()
        h.conn.connect()
        self.assertTrue(h.conn.needs_reconciliation)
        h.conn.mark_reconciled()
        self.assertFalse(h.conn.needs_reconciliation)

    def test_client_id_conflict_fails_closed_without_retry_or_another_id(self):
        h = Harness()
        h.fake.connect_behaviours = ["client-id-in-use", "ok"]
        with self.assertRaises(ClientIdInUse):
            h.conn.connect_with_backoff()
        self.assertEqual(len(h.fake.called("connect")), 1)
        self.assertEqual(h.sleeps, [])
        self.assertFalse(h.conn.connected)
        self.assertEqual(h.statuses[-1].state, "refused")
        self.assertEqual(h.statuses[-1].ibkr_code, 326)

    def test_silent_gateway_times_out(self):
        h = Harness()
        h.fake.connect_behaviours = ["silent"]
        with self.assertRaises(ConnectFailed):
            h.conn.connect()
        self.assertFalse(h.conn.connected)
        self.assertEqual(len(h.fake.called("disconnect")), 1)

    def test_refused_connection_fails(self):
        h = Harness()
        h.fake.connect_behaviours = ["refused"]
        with self.assertRaises(ConnectFailed):
            h.conn.connect()

    def test_connect_timeout_bounds_the_socket_work_as_well_as_the_handshake(self):
        h = Harness()

        def slow_socket():
            h.clock.now += 1.0  # longer than connect_timeout (0.05)
        h.fake.connect_hook = slow_socket
        with self.assertRaises(ConnectFailed):
            h.conn.connect()
        self.assertEqual(h.fake.connect_timeouts, [0.05])
        self.assertFalse(h.conn.connected)
        self.assertEqual(len(h.fake.called("disconnect")), 1)

    def test_a_socket_connect_that_overruns_is_abandoned_and_retried(self):
        h = Harness()
        h.fake.connect_behaviours = ["hang", "ok"]
        h.conn.connect_with_backoff()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 2)
        self.assertEqual(len(h.fake.called("disconnect")), 1)

    def test_a_socket_error_is_a_failed_connect(self):
        h = Harness()
        h.fake.connect_behaviours = ["socket-error"]
        with self.assertRaises(ConnectFailed):
            h.conn.connect()
        self.assertFalse(h.conn.connected)


class ReconnectTests(unittest.TestCase):
    def test_backs_off_exponentially_up_to_the_cap(self):
        h = Harness()
        h.fake.connect_behaviours = ["refused", "silent", "refused", "refused", "ok"]
        h.conn.connect_with_backoff()
        self.assertTrue(h.conn.connected)
        self.assertEqual(h.sleeps, [1.0, 2.0, 4.0, 8.0])
        attempts = [s.attempt for s in h.statuses if s.state == "reconnecting"]
        self.assertEqual(attempts, [2, 3, 4, 5])

    def test_gives_up_after_the_configured_attempts(self):
        h = Harness(max_connect_attempts=3)
        h.fake.connect_behaviours = ["refused"] * 3
        with self.assertRaises(ConnectFailed):
            h.conn.connect_with_backoff()
        self.assertEqual(len(h.fake.called("connect")), 3)
        self.assertEqual(h.statuses[-1].state, "gave-up")

    def test_reconnect_after_a_drop_uses_the_same_client_id(self):
        h = Harness()
        h.conn.connect()
        h.conn.mark_reconciled()
        h.fake.drop()
        h.conn.pump(0.01)
        self.assertFalse(h.conn.connected)
        h.conn.reconnect()
        self.assertTrue(h.conn.connected)
        self.assertEqual([c[3] for c in h.fake.called("connect")], [11, 11])
        self.assertTrue(h.conn.needs_reconciliation)


class DisconnectDetectionTests(unittest.TestCase):
    def test_socket_close_is_reported(self):
        h = Harness()
        h.conn.connect()
        h.fake.drop()
        h.conn.pump(0.01)
        self.assertEqual(h.statuses[-1].state, "disconnected")
        self.assertEqual(h.statuses[-1].reason, "connection-closed")

    def test_gateway_losing_ibkr_is_reported_and_restoration_with_data_loss_needs_reconciliation(self):
        h = Harness()
        h.conn.connect()
        h.conn.mark_reconciled()
        h.fake.callbacks.error(-1, 1100, "Connectivity between IB and Trader Workstation has been lost.")
        h.conn.pump(0.01)
        self.assertEqual(h.statuses[-1].state, "connectivity-lost")
        self.assertFalse(h.conn.connected)
        h.fake.callbacks.error(-1, 1101, "Connectivity restored - data lost.")
        h.conn.pump(0.01)
        self.assertEqual(h.statuses[-1].state, "connected")
        self.assertTrue(h.statuses[-1].data_lost)
        self.assertTrue(h.conn.needs_reconciliation)

    def test_restoration_with_data_maintained_still_requires_reconciliation(self):
        h = Harness()
        h.conn.connect()
        h.conn.mark_reconciled()
        h.fake.callbacks.error(-1, 1100, "lost")
        h.fake.callbacks.error(-1, 1102, "Connectivity restored - data maintained.")
        h.conn.pump(0.01)
        h.conn.pump(0.01)
        self.assertEqual(h.statuses[-1].data_lost, False)
        self.assertTrue(h.conn.needs_reconciliation)

    def test_informational_farm_notices_change_nothing(self):
        h = Harness()
        h.conn.connect()
        h.fake.callbacks.error(-1, 2104, "Market data farm connection is OK:usfarm")
        h.conn.pump(0.01)
        self.assertEqual(h.states(), ["connected"])

    def test_heartbeat_probes_on_its_interval(self):
        h = Harness()
        h.conn.connect()
        h.conn.check_heartbeat()
        self.assertEqual(len(h.fake.called("req_current_time")), 1)
        h.conn.pump(0.01)
        h.conn.check_heartbeat()
        self.assertEqual(len(h.fake.called("req_current_time")), 1)
        h.clock.now += 30.0
        h.conn.check_heartbeat()
        self.assertEqual(len(h.fake.called("req_current_time")), 2)
        self.assertEqual(h.conn.last_server_time, 1_800_000_000)

    def test_unanswered_heartbeat_is_a_disconnect(self):
        h = Harness()
        h.conn.connect()
        h.fake.answer_current_time = False
        h.conn.check_heartbeat()
        h.clock.now += 10.0
        h.conn.check_heartbeat()
        self.assertFalse(h.conn.connected)
        self.assertEqual(h.statuses[-1].reason, "heartbeat-timeout")

    def test_restoration_clears_a_probe_left_over_from_before_the_outage(self):
        h = Harness()
        h.conn.connect()
        h.fake.answer_current_time = False
        h.conn.check_heartbeat()
        h.fake.callbacks.error(-1, 1100, "lost")
        h.conn.pump(0.01)
        h.clock.now += 100.0  # the outage outlasts heartbeat_timeout
        h.fake.callbacks.error(-1, 1102, "Connectivity restored - data maintained.")
        h.conn.pump(0.01)
        h.conn.check_heartbeat()
        self.assertTrue(h.conn.connected)
        self.assertNotIn("heartbeat-timeout", [s.reason for s in h.statuses])
        # A fresh probe goes out at once, with its own deadline.
        self.assertEqual(len(h.fake.called("req_current_time")), 2)

    def test_requests_refuse_when_disconnected(self):
        h = Harness()
        with self.assertRaises(NotConnected):
            h.conn.require_connected()


class MaintainTests(unittest.TestCase):
    """maintain() is what brings a lost link back; nothing else reconnects."""

    def test_a_socket_close_is_reconnected(self):
        h = Harness()
        h.conn.connect()
        h.conn.mark_reconciled()
        h.fake.drop()
        h.conn.pump(0.01)
        h.conn.maintain()
        self.assertTrue(h.conn.connected)
        self.assertEqual([c[3] for c in h.fake.called("connect")], [11, 11])
        self.assertTrue(h.conn.needs_reconciliation)

    def test_an_unanswered_heartbeat_is_reconnected(self):
        h = Harness()
        h.conn.connect()
        h.fake.answer_current_time = False
        h.conn.maintain()
        h.clock.now += 10.0
        h.conn.maintain()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 2)
        self.assertEqual(h.states()[-2:], ["disconnected", "connected"])

    def test_lost_ibkr_connectivity_waits_for_gateway_to_restore_it(self):
        h = Harness()
        h.conn.connect()
        h.fake.callbacks.error(-1, 2110, "Connectivity between TWS and server is broken.")
        h.conn.pump(0.01)
        h.clock.now += 59.0
        h.conn.maintain()
        self.assertFalse(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 1)
        h.fake.callbacks.error(-1, 1101, "Connectivity restored - data lost.")
        h.conn.pump(0.01)
        h.clock.now += 120.0
        h.conn.maintain()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 1)

    def test_connectivity_not_restored_in_time_drops_the_link_and_reconnects(self):
        h = Harness()
        h.conn.connect()
        h.conn.mark_reconciled()
        h.fake.callbacks.error(-1, 1100, "lost")
        h.conn.pump(0.01)
        h.clock.now += 60.0
        h.conn.maintain()
        self.assertTrue(h.conn.connected)
        self.assertEqual(len(h.fake.called("connect")), 2)
        self.assertEqual(len(h.fake.called("disconnect")), 1)
        self.assertIn("connectivity-not-restored", [s.reason for s in h.statuses])
        self.assertTrue(h.conn.needs_reconciliation)

    def test_a_healthy_link_is_left_alone(self):
        h = Harness()
        h.conn.connect()
        h.conn.maintain()
        h.conn.pump(0.01)
        h.conn.maintain()
        self.assertEqual(len(h.fake.called("connect")), 1)

    def test_a_client_id_conflict_on_reconnect_still_fails_closed(self):
        h = Harness()
        h.conn.connect()
        h.fake.drop()
        h.conn.pump(0.01)
        h.fake.connect_behaviours = ["client-id-in-use"]
        with self.assertRaises(ClientIdInUse):
            h.conn.maintain()
        self.assertEqual(len(h.fake.called("connect")), 2)


class LoggingTests(unittest.TestCase):
    def test_connection_logs_name_no_credential(self):
        with self.assertLogs("ibkr", level=logging.INFO) as logs:
            h = Harness()
            h.fake.connect_behaviours = ["refused", "ok"]
            h.conn.connect_with_backoff()
        text = "\n".join(logs.output).lower()
        self.assertIn("client_id=11", text)
        for word in ("password", "passwd", "secret", "token"):
            self.assertNotIn(word, text)


if __name__ == "__main__":
    unittest.main()
