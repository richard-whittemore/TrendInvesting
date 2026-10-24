"""A loopback stand-in for the GitHub REST API's issue endpoints.

The rehearsals point the reporter's github-issues channel at it
(TI_GITHUB_API_URL=http://127.0.0.1:<port>/gh) so the channel's units are
exercised without a real token or a real repository. It keeps issues in
memory, answers only the requests the channel makes, and holds synthetic
data only (ADR 0019, "Alerting"; docs/manual/alerts-and-dashboard.md,
"Alerts as GitHub Issues").
"""

import json
import re

PREFIX = "/gh"
REPO = PREFIX + "/repos/owner/repo"
SELF_ID = 1

issues = []


def _issue_json(i):
    return {"number": i["number"], "state": i["state"], "body": i["body"], "comments": len(i["comments"])}


def _find(number):
    for i in issues:
        if i["number"] == number:
            return i
    return None


def handle(handler, method, body, log):
    """Answer a request under PREFIX; return False for any other path.

    log is called with "METHOD path" for each write, so a rehearsal can count
    what the channel did without mixing it into the other channels' posts.
    """
    path = handler.path.split("?", 1)[0]
    if not path.startswith(PREFIX + "/"):
        return False
    if method != "GET":
        log(method + " " + path, body)
    status, out = 404, {"message": "Not Found"}
    m = re.fullmatch(re.escape(REPO) + r"/issues(?:/(\d+))?(?:/(comments|labels)(?:/[^/]+)?)?", path)
    if method == "GET" and path == PREFIX + "/user":
        status, out = 200, {"id": SELF_ID, "login": "rehearsal"}
    elif m:
        number, sub = (int(m.group(1)) if m.group(1) else None), m.group(2)
        issue = _find(number) if number else None
        if number is None and method == "GET":
            status, out = 200, [_issue_json(i) for i in reversed(issues)]
        elif number is None and method == "POST":
            data = json.loads(body or b"{}")
            issue = {"number": len(issues) + 1, "state": "open", "body": data.get("body", ""), "comments": []}
            issues.append(issue)
            status, out = 201, _issue_json(issue)
        elif issue is not None and sub is None and method == "GET":
            status, out = 200, _issue_json(issue)
        elif issue is not None and sub is None and method == "PATCH":
            issue["state"] = json.loads(body or b"{}").get("state", issue["state"])
            status, out = 200, _issue_json(issue)
        elif issue is not None and sub == "comments" and method == "GET":
            status, out = 200, [{"body": c, "user": {"id": SELF_ID}} for c in issue["comments"]]
        elif issue is not None and sub == "comments" and method == "POST":
            issue["comments"].append(json.loads(body or b"{}").get("body", ""))
            status, out = 201, {"id": len(issue["comments"])}
        elif issue is not None and sub == "labels":
            status, out = 200, []
    payload = json.dumps(out).encode()
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(payload)))
    handler.end_headers()
    handler.wfile.write(payload)
    return True
