"""Tests for deploy/tailscale/policy.hujson, the example Tailscale access policy.

The policy is HuJSON (JSON with comments and trailing commas). These tests
strip both, parse it, and hold it to what the owner's manual promises: the
tagged server is reachable on SSH (22), the dashboard (443) and the owner's
ntfy (2586) and on nothing else, notably not the Gateway's API ports. The
runbook ordering is checked here too: the access-rule step of
docs/runbooks/first-paper-order.md B4 comes before the Read-Only switch.
"""
import json
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
POLICY = Path(__file__).resolve().parent / "policy.hujson"
SERVER = "tag:trend-server"


def strip_hujson(text):
    """Remove // and /* */ comments (outside strings) and trailing commas."""
    out = []
    i, n, in_str = 0, len(text), False
    while i < n:
        c = text[i]
        if in_str:
            out.append(c)
            if c == "\\":
                out.append(text[i + 1])
                i += 1
            elif c == '"':
                in_str = False
        elif c == '"':
            in_str = True
            out.append(c)
        elif text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
            continue
        elif text.startswith("/*", i):
            i = text.index("*/", i) + 2
            continue
        else:
            out.append(c)
        i += 1
    return re.sub(r",(\s*[}\]])", r"\1", "".join(out))


def load():
    return json.loads(strip_hujson(POLICY.read_text()))


def to_server(rule):
    return SERVER in rule.get("dst", [])


class PolicyTest(unittest.TestCase):
    def test_strip_keeps_comment_markers_inside_strings(self):
        got = json.loads(strip_hujson('{"a": "http://x", /* c */ "b": [1,], // t\n}'))
        self.assertEqual(got, {"a": "http://x", "b": [1]})

    def test_server_tag_is_owned_by_the_admin(self):
        self.assertEqual(load()["tagOwners"], {SERVER: ["autogroup:admin"]})

    def test_owner_devices_reach_each_other(self):
        rules = [g for g in load()["grants"] if g.get("dst") == ["autogroup:self"]]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["src"], ["autogroup:member"])
        self.assertEqual(rules[0]["ip"], ["*"])

    def test_server_ports_are_exactly_ssh_dashboard_ntfy(self):
        ports = set()
        for rule in load()["grants"]:
            if to_server(rule):
                self.assertEqual(rule["src"], ["autogroup:member"])
                ports.update(rule["ip"])
        self.assertEqual(ports, {"tcp:22", "tcp:443", "tcp:2586"})

    def test_nothing_grants_everything(self):
        policy = load()
        for rule in policy["grants"]:
            self.assertNotIn("*", rule["dst"], rule)
        self.assertNotIn("acls", policy)
        self.assertNotIn(f"{SERVER}:*", json.dumps(policy))
        self.assertNotIn("*:*", json.dumps(policy))

    def test_no_gateway_port_is_named(self):
        text = json.dumps(load())
        for port in ("4001", "4002"):
            self.assertNotIn(port, text)

    def test_tailscale_ssh_for_the_owner_as_ubuntu_only(self):
        rules = load()["ssh"]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["action"], "accept")
        self.assertEqual(rules[0]["src"], ["autogroup:member"])
        self.assertEqual(rules[0]["dst"], [SERVER])
        self.assertEqual(rules[0]["users"], ["ubuntu"])


class RunbookOrderTest(unittest.TestCase):
    def test_b4_closes_ports_before_read_only_is_turned_off(self):
        text = (ROOT / "docs/runbooks/first-paper-order.md").read_text()
        b4 = text[text.index("### B4."):text.index("### B5.")]
        close = b4.index("Close the Gateway's ports")
        read_only = b4.index("Turn the Read-Only API off")
        self.assertLess(close, read_only)
        self.assertIn("docs/manual/server.md", b4[close:read_only])
        self.assertIn("Closing the Gateway's ports to your other devices", b4[close:read_only])

    def test_manual_has_the_section_with_steps_in_order(self):
        text = (ROOT / "docs/manual/server.md").read_text()
        start = text.index("## Closing the Gateway's ports to your other devices")
        sec = text[start:text.index("\n## ", start + 5)]
        paste = sec.index("Access controls")
        tag = sec.index("Edit ACL tags")
        self.assertLess(paste, tag)
        for needle in ("deploy/tailscale/policy.hujson", "ssh ubuntu@trend-investing-1",
                       "nc -vz -G 5 trend-investing-1 4002", "history"):
            self.assertIn(needle, sec)


if __name__ == "__main__":
    unittest.main()
