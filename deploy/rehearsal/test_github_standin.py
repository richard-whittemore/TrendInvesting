"""The GitHub stand-in keeps labels and honours the listing filters the
reporter uses, so a reporter that fails to label its alerts fails a rehearsal
(ADR 0019, "Alerting")."""

import io
import json
import unittest

import github_standin


class Handler:
    def __init__(self, path):
        self.path = path
        self.status = None
        self.wfile = io.BytesIO()

    def send_response(self, status):
        self.status = status

    def send_header(self, *_):
        pass

    def end_headers(self):
        pass


def call(method, path, body=None):
    h = Handler(github_standin.PREFIX + "/repos/owner/repo" + path)
    raw = json.dumps(body).encode() if body is not None else b""
    github_standin.handle(h, method, raw, lambda line, b: None)
    return h.status, json.loads(h.wfile.getvalue())


class StandIn(unittest.TestCase):
    def setUp(self):
        github_standin.issues.clear()

    def test_listing_honours_the_label_filter(self):
        call("POST", "/issues", {"title": "a", "body": "x", "labels": ["alert"]})
        call("POST", "/issues", {"title": "b", "body": "y", "labels": []})
        _, listed = call("GET", "/issues?labels=alert&state=all")
        self.assertEqual([i["number"] for i in listed], [1])
        self.assertEqual([lb["name"] for lb in listed[0]["labels"]], ["alert"])

    def test_listing_honours_the_state_filter(self):
        call("POST", "/issues", {"labels": ["alert"]})
        call("POST", "/issues", {"labels": ["alert"]})
        call("PATCH", "/issues/1", {"state": "closed"})
        self.assertEqual([i["number"] for i in call("GET", "/issues?labels=alert&state=open")[1]], [2])
        self.assertEqual([i["number"] for i in call("GET", "/issues?labels=alert&state=closed")[1]], [1])
        self.assertEqual(len(call("GET", "/issues?labels=alert&state=all")[1]), 2)

    def test_label_changes_move_an_issue_out_of_the_listing(self):
        call("POST", "/issues", {"labels": ["alert"]})
        call("POST", "/issues/1/labels", {"labels": ["alert-resolved"]})
        call("DELETE", "/issues/1/labels/alert")
        self.assertEqual(call("GET", "/issues?labels=alert&state=all")[1], [])
        self.assertEqual(len(call("GET", "/issues?labels=alert-resolved&state=all")[1]), 1)
        self.assertEqual(call("DELETE", "/issues/1/labels/alert")[0], 404)


if __name__ == "__main__":
    unittest.main()
