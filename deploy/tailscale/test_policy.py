"""Tests for deploy/tailscale/policy.hujson, the example Tailscale access policy.

The policy is HuJSON (JSON with comments and trailing commas). These tests
strip both, parse it, and hold it to what the owner's manual promises: the
tagged server is reachable on SSH (22), the dashboard (443) and the owner's
ntfy (2586) and on nothing else, notably not the Gateway's API ports. The
runbook ordering is checked here too: the access-rule step of
docs/runbooks/first-paper-order.md B4 comes before the Read-Only switch.
"""
import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
POLICY = Path(__file__).resolve().parent / "policy.hujson"
SERVER = "tag:trend-server"
MINI = "tag:trend-mac-mini"
RESCUE = Path(__file__).resolve().parent / "rescue.hujson"


def strip_hujson(text):
    """Remove // and /* */ comments and trailing commas, leaving strings alone."""
    out = []  # (kind, text): kind is "comma" for a comma outside any string
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c == '"':
            j = i + 1
            while text[j] != '"':
                j += 2 if text[j] == "\\" else 1
            out.append(("str", text[i:j + 1]))
            i = j + 1
        elif text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
        elif text.startswith("/*", i):
            i = text.index("*/", i) + 2
        elif c == ",":
            out.append(("comma", c))
            i += 1
        else:
            out.append(("other", c))
            i += 1
    kept = []
    for idx, (kind, tok) in enumerate(out):
        if kind == "comma":
            nxt = next((t for k, t in out[idx + 1:] if not t.isspace()), "")
            if nxt in ("}", "]"):
                continue
        kept.append(tok)
    return "".join(kept)


def load():
    return json.loads(strip_hujson(POLICY.read_text()))


def load_rescue():
    return json.loads(strip_hujson(RESCUE.read_text()))


def to_server(rule):
    return SERVER in rule.get("dst", [])


# Destinations that cannot include the server: the owner's own devices and the
# other tag. Anything else (the server tag, "*", an address or CIDR, a host
# alias, another autogroup) could include it.
NOT_THE_SERVER = {"autogroup:self", MINI}
GATEWAY_PORTS = (4001, 4002)
INTENDED = {
    (("autogroup:member",), ("tcp:22", "tcp:443", "tcp:2586")),
    ((MINI,), ("tcp:443",)),
}


def server_grants(policy):
    return [g for g in policy.get("grants", []) if set(g["dst"]) - NOT_THE_SERVER]


def exposes(ip_entries, port):
    """True if any grant ip entry could cover this TCP port."""
    for entry in ip_entries:
        proto, _, ports = entry.rpartition(":") if ":" in entry else ("", "", entry)
        if proto not in ("", "tcp", "udp", "*"):
            continue
        if ports == "*":
            return True
        lo, _, hi = ports.partition("-")
        if lo.isdigit() and int(lo) <= port <= int(hi or lo):
            return True
    return False


def audit(policy):
    """List what is wrong with a policy; empty means it exposes only the intended ports."""
    problems = []
    extra = set(policy) - {"tagOwners", "grants", "ssh"}
    if extra:
        problems.append(f"unreviewed sections {sorted(extra)}")
    for grant in policy.get("grants", []):
        if set(grant) - {"src", "dst", "ip"}:
            problems.append(f"unreviewed grant fields {grant}")
    for grant in server_grants(policy):
        if grant["dst"] != [SERVER] or (tuple(grant["src"]), tuple(grant["ip"])) not in INTENDED:
            problems.append(f"unintended grant that could reach the server: {grant}")
        for port in GATEWAY_PORTS:
            if exposes(grant["ip"], port):
                problems.append(f"grant exposes {port}: {grant}")
    return problems


class PolicyTest(unittest.TestCase):
    def test_strip_keeps_comment_markers_inside_strings(self):
        got = json.loads(strip_hujson('{"a": "http://x", /* c */ "b": [1,], // t\n}'))
        self.assertEqual(got, {"a": "http://x", "b": [1]})

    def test_strip_keeps_commas_inside_strings(self):
        got = json.loads(strip_hujson('{"v": "abc,]", "w": "x,}", "k": ["a,",],}'))
        self.assertEqual(got, {"v": "abc,]", "w": "x,}", "k": ["a,"]})

    def test_strip_handles_escaped_quotes_and_comments_between_comma_and_bracket(self):
        got = json.loads(strip_hujson('{"v": "a\\",]", "k": [1, /* c */ ],}'))
        self.assertEqual(got, {"v": 'a",]', "k": [1]})

    def test_server_tag_is_owned_by_the_admin(self):
        self.assertEqual(load()["tagOwners"][SERVER], ["autogroup:admin"])

    def test_policy_has_only_the_reviewed_sections(self):
        self.assertLessEqual(set(load()), {"tagOwners", "grants", "ssh"})

    def test_owner_devices_reach_each_other(self):
        rules = [g for g in load()["grants"] if g.get("dst") == ["autogroup:self"]]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["src"], ["autogroup:member"])
        self.assertEqual(rules[0]["ip"], ["*"])

    def test_the_real_policy_exposes_only_the_intended_ports(self):
        self.assertEqual(audit(load()), [])

    def test_server_grants_are_exactly_the_two_intended(self):
        got = sorted((tuple(g["src"]), tuple(g["ip"])) for g in server_grants(load()))
        self.assertEqual(got, [
            (("autogroup:member",), ("tcp:22", "tcp:443", "tcp:2586")),
            (("tag:trend-mac-mini",), ("tcp:443",)),
        ])

    def test_injected_grants_that_reach_the_gateway_ports_are_caught(self):
        injected = [
            {"src": ["autogroup:member"], "dst": ["100.64.0.0/10"], "ip": ["tcp:4000-4010"]},
            {"src": ["autogroup:member"], "dst": ["100.64.0.0/10"], "ip": ["*"]},
            {"src": ["autogroup:member"], "dst": ["*"], "ip": ["tcp:4001"]},
            {"src": ["autogroup:member"], "dst": ["tag:trend-server"], "ip": ["tcp:4002"]},
            {"src": ["autogroup:member"], "dst": ["tag:trend-server"], "ip": ["*"]},
            {"src": ["autogroup:member"], "dst": ["trend-host"], "ip": ["tcp:1-65535"]},
            {"src": ["autogroup:member"], "dst": ["autogroup:tagged"], "ip": ["4001"]},
            {"src": ["tag:trend-mac-mini"], "dst": ["tag:trend-server"], "ip": ["tcp:4002"]},
        ]
        for grant in injected:
            policy = load()
            policy["grants"].append(grant)
            self.assertNotEqual(audit(policy), [], grant)

    def test_an_added_hosts_or_acls_section_is_caught(self):
        for extra in ({"acls": []}, {"hosts": {"trend-host": "100.64.0.1"}}):
            policy = load()
            policy.update(extra)
            self.assertNotEqual(audit(policy), [], extra)

    def test_no_gateway_port_is_named(self):
        text = json.dumps(load())
        for port in ("4001", "4002"):
            self.assertNotIn(port, text)

    def test_default_ssh_rule_for_the_owners_own_devices_is_kept(self):
        rules = [r for r in load()["ssh"] if r["dst"] == ["autogroup:self"]]
        self.assertEqual(rules, [{
            "action": "check",
            "src": ["autogroup:member"],
            "dst": ["autogroup:self"],
            "users": ["autogroup:nonroot", "root"],
        }])

    def test_tailscale_ssh_for_the_owner_to_the_server_as_ubuntu_only(self):
        rules = [r for r in load()["ssh"] if to_server(r)]
        self.assertEqual(len(rules), 1)
        self.assertEqual(rules[0]["action"], "accept")
        self.assertEqual(rules[0]["src"], ["autogroup:member"])
        self.assertEqual(rules[0]["dst"], [SERVER])
        self.assertEqual(rules[0]["users"], ["ubuntu"])
        self.assertEqual(len(load()["ssh"]), 3)

    def test_mac_mini_gets_no_ssh_to_anything(self):
        for rule in load()["ssh"]:
            self.assertNotIn(MINI, rule["src"])

    def test_owner_keeps_the_mac_mini_once_it_is_tagged(self):
        check_mini_access(self, load())


def check_mini_access(test, policy):
    """A tagged Mac mini leaves autogroup:member, so the owner needs rules of their own to it."""
    grants = [g for g in policy["grants"] if g["dst"] == [MINI]]
    test.assertEqual(grants, [{"src": ["autogroup:member"], "dst": [MINI], "ip": ["*"]}])
    ssh = [r for r in policy["ssh"] if r["dst"] == [MINI]]
    test.assertEqual(ssh, [{
        "action": "check",
        "src": ["autogroup:member"],
        "dst": [MINI],
        "users": ["autogroup:nonroot"],
    }])


class RescueTest(unittest.TestCase):
    def test_rescue_parses_with_the_same_tag_owners(self):
        self.assertEqual(load_rescue()["tagOwners"][SERVER], ["autogroup:admin"])
        self.assertEqual(load_rescue()["tagOwners"], load()["tagOwners"])

    def test_rescue_exposes_only_the_intended_ports_and_never_the_gateway(self):
        self.assertEqual(audit(load_rescue()), [])
        self.assertNotIn("4001", json.dumps(load_rescue()))
        self.assertNotIn("4002", json.dumps(load_rescue()))

    def test_rescue_server_grants_are_the_main_policys_exactly(self):
        key = lambda g: json.dumps(g, sort_keys=True)
        self.assertEqual(sorted(map(key, server_grants(load_rescue()))),
                         sorted(map(key, server_grants(load()))))

    def test_rescue_keeps_the_mac_minis_443(self):
        got = [g for g in server_grants(load_rescue()) if g["src"] == [MINI]]
        self.assertEqual(got, [{"src": [MINI], "dst": [SERVER], "ip": ["tcp:443"]}])

    def test_rescue_has_the_reviewed_sections_only(self):
        self.assertLessEqual(set(load_rescue()), {"tagOwners", "grants", "ssh"})

    def test_rescue_ssh_to_own_devices_is_the_default_check_rule(self):
        rules = [r for r in load_rescue()["ssh"] if r["dst"] == ["autogroup:self"]]
        self.assertEqual(rules, [{
            "action": "check",
            "src": ["autogroup:member"],
            "dst": ["autogroup:self"],
            "users": ["autogroup:nonroot", "root"],
        }])

    def test_rescue_ssh_to_the_tagged_server_is_accept_for_ubuntu_and_root(self):
        rules = [r for r in load_rescue()["ssh"] if r["dst"] == [SERVER]]
        self.assertEqual(rules, [{
            "action": "accept",
            "src": ["autogroup:member"],
            "dst": [SERVER],
            "users": ["ubuntu", "root"],
        }])

    def test_rescue_grants_the_owner_the_tagged_mac_mini(self):
        check_mini_access(self, load_rescue())

    def test_rescue_gives_the_mac_mini_no_ssh(self):
        for rule in load_rescue()["ssh"]:
            self.assertNotIn(MINI, rule["src"])


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
        for needle in ("deploy/tailscale/policy.hujson", "deploy/tailscale/rescue.hujson",
                       "Disable key expiry", "do not restore the previous policy", "ssh ubuntu@trend-investing-1",
                       "nc -vz -G 5 trend-investing-1 4002"):
            self.assertIn(needle, sec)

    def test_step_zero_does_not_rely_on_an_open_ssh_session(self):
        text = (ROOT / "docs/manual/server.md").read_text()
        start = text.index("## Closing the Gateway's ports to your other devices")
        sec = text[start:text.index("\n## ", start + 5)]
        step0 = sec[sec.index("\n0. "):sec.index("\n1. ")]
        self.assertNotIn("leave it open", step0)
        self.assertIn("ends", step0)
        self.assertIn("admin console", step0)
        self.assertIn("rescue.hujson", step0)

    def test_server_section_says_the_mac_mini_is_not_tagged_yet(self):
        text = (ROOT / "docs/manual/server.md").read_text()
        start = text.index("## Closing the Gateway's ports to your other devices")
        sec = text[start:text.index("\n## ", start + 5)]
        self.assertIn("not tagged yet", sec)
        self.assertNotIn("Do the same for the Mac mini", sec)
        self.assertIn("mac-mini.md", sec)

    def test_mac_mini_runbook_tags_it_and_disables_its_key_expiry(self):
        text = (ROOT / "docs/runbooks/mac-mini.md").read_text()
        sec = text[text.index("## 2. The alert triage job"):text.index("## Checking the launcher itself")]
        for needle in ("tag:trend-mac-mini", "Edit ACL tags", "Disable key expiry",
                       "no longer reachable", "autogroup:member"):
            self.assertIn(needle, sec)

    def test_no_runbook_tells_the_owner_to_advertise_tags(self):
        for rel in ("docs/runbooks/lightsail-deployment.md", "docs/runbooks/mac-mini.md",
                    "docs/manual/server.md"):
            self.assertNotIn("advertise-tags", (ROOT / rel).read_text(), rel)
        step12 = (ROOT / "docs/runbooks/lightsail-deployment.md").read_text()
        step12 = step12[step12.index("\n12. "):step12.index("**The data step.**")]
        self.assertIn("docs/manual/server.md", step12)
        self.assertIn("mac-mini.md", step12)


if __name__ == "__main__":
    unittest.main()
