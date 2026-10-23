#!/usr/bin/env python3
"""Check from another machine whether an IB Gateway API port serves this client.

Usage: python3 deploy/ibapi-probe.py HOST PORT [--timeout SECONDS]

Sends only the IB API opening handshake ("API\\0" and a length-prefixed
version range, v100..187), reads at most the first reply, and closes. It
never sends a client ID, a request or an order.

  EXPOSED       exit 1  the server answered with an API server version
  REJECTED      exit 0  refused, reset, or closed without a reply
  INCONCLUSIVE  exit 2  no answer in time, or the host could not be reached

Only REJECTED is a pass (rules/ADR 0026: the Gateway serves programs on its
own server only). A bare TCP connect proves nothing, because the IB API
waits for the client to speak first.
"""
import socket
import struct
import sys

VERSIONS = b"v100..187"
EXIT = {"REJECTED": 0, "EXPOSED": 1, "INCONCLUSIVE": 2}


def probe(host, port, timeout):
    try:
        with socket.create_connection((host, port), timeout=timeout) as s:
            s.settimeout(timeout)
            s.sendall(b"API\x00" + struct.pack(">I", len(VERSIONS)) + VERSIONS)
            data = s.recv(64)
    except (ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
        return "REJECTED"
    except (socket.timeout, TimeoutError):
        return "INCONCLUSIVE"
    except OSError:
        return "INCONCLUSIVE"
    return "EXPOSED" if data else "REJECTED"


def main(argv):
    timeout = 5.0
    args = list(argv)
    if "--timeout" in args:
        i = args.index("--timeout")
        try:
            timeout = float(args[i + 1])
        except (IndexError, ValueError):
            args = []
        else:
            del args[i:i + 2]
    try:
        host, port = args[0], int(args[1])
        if len(args) != 2:
            raise ValueError
    except (IndexError, ValueError):
        print("usage: ibapi-probe.py HOST PORT [--timeout SECONDS]", file=sys.stderr)
        return 2
    result = probe(host, port, timeout)
    print(f"{result}: {host}:{port}")
    return EXIT[result]


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
