"""A stand-in for Massive's REST API, for the first-day rehearsal only.

Serves the three endpoints ti-data's warm-up and daily job use, with
synthetic figures (SPY's close is invented, a slow ramp), over HTTPS on
loopback with a throwaway certificate the rehearsal trusts through
SSL_CERT_FILE. Every figure here is made up; nothing real is stored. The
path of each request is appended to the log named by argv[3], so the
rehearsal can count what a firing fetched.

usage: massive-standin.py CERT KEY REQUEST_LOG
"""
import datetime as dt
import http.server
import json
import ssl
import sys

cert, key, request_log = sys.argv[1:4]
ORIGIN = dt.date(2025, 1, 2)


def bar(symbol, day, offset):
    c = round(400.0 + offset + (day - ORIGIN).days * 0.05, 2)
    return {"T": symbol, "o": c, "h": c + 1, "l": c - 1, "c": c, "v": 1000, "t": 0}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = self.path.split("?")[0]
        with open(request_log, "a") as f:
            f.write(path + "\n")
        if path.startswith("/v2/aggs/grouped/locale/us/market/stocks/"):
            day = dt.date.fromisoformat(path.rsplit("/", 1)[1])
            body = {"status": "OK", "results": [bar("SPY", day, 0), bar("OTHER", day, -300)]}
        elif path in ("/v3/reference/splits", "/v3/reference/dividends"):
            body = {"status": "OK", "results": []}
        else:
            self.send_error(404)
            return
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


server = http.server.ThreadingHTTPServer(("127.0.0.1", 443), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.minimum_version = ssl.TLSVersion.TLSv1_2
context.load_cert_chain(cert, key)
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
