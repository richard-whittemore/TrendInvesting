#!/usr/bin/env python3
"""Check from another machine whether an IB Gateway API port serves this client.

Usage: python3 deploy/ibapi-probe.py HOST PORT [--timeout SECONDS]

Sends only the IB API opening handshake ("API\\0" and a length-prefixed
version range, v100..187), reads at most the first reply, and closes. It
never sends a client ID, a request or an order. SECONDS (default 5) bounds
the whole probe, name lookup included.

  EXPOSED       exit 1  the server sent any reply bytes to the handshake (they
                        are not checked to be an API server version)
  REJECTED      exit 0  the connection was accepted, then closed or reset
                        without any reply byte
  INCONCLUSIVE  exit 2  proves nothing: nothing is listening (refused), no
                        answer in time (a silent but connected Gateway too),
                        or the host could not be reached

Only REJECTED is a pass (rules/ADR 0026: the Gateway serves programs on its
own server only). A bare TCP connect proves nothing, because the IB API
waits for the client to speak first, and a refused connection is the same as
a Gateway that is not up yet, so it is not a pass. A bad argument exits 2.
"""
import math
import socket
import struct
import sys
import threading
import time

VERSIONS = b"v100..187"
EXIT = {"REJECTED": 0, "EXPOSED": 1, "INCONCLUSIVE": 2}


def resolve(host, port, timeout, resolver):
    """Addresses for host, or None if the lookup fails or outlasts timeout."""
    box = []

    def work():
        try:
            box.append(resolver(host, port, type=socket.SOCK_STREAM))
        except Exception:  # any lookup failure is "could not be reached"
            box.append(None)

    t = threading.Thread(target=work, daemon=True)
    t.start()
    t.join(timeout)
    return box[0] if box else None


def probe(host, port, timeout, resolver=socket.getaddrinfo):
    deadline = time.monotonic() + timeout

    def left():
        return max(deadline - time.monotonic(), 0.001)

    infos = resolve(host, port, timeout, resolver)
    if not infos:
        return "INCONCLUSIVE"
    for family, kind, proto, _, addr in infos:
        s = socket.socket(family, kind, proto)
        try:
            s.settimeout(left())
            s.connect(addr)
        except OSError:
            # Refused, unreachable or timed out: nothing answered, which is
            # not the same as the Gateway refusing this client.
            s.close()
            continue
        try:
            s.settimeout(left())
            s.sendall(b"API\x00" + struct.pack(">I", len(VERSIONS)) + VERSIONS)
            s.settimeout(left())
            data = s.recv(64)
        except (ConnectionResetError, BrokenPipeError):
            return "REJECTED"
        except OSError:  # includes a timeout: connected but silent
            return "INCONCLUSIVE"
        finally:
            s.close()
        return "EXPOSED" if data else "REJECTED"
    return "INCONCLUSIVE"


def usage():
    print("usage: ibapi-probe.py HOST PORT [--timeout SECONDS]  "
          "(PORT 1-65535, SECONDS finite and above 0)", file=sys.stderr)
    return 2


def main(argv):
    timeout = 5.0
    args = list(argv)
    if "--timeout" in args:
        i = args.index("--timeout")
        try:
            timeout = float(args[i + 1])
        except (IndexError, ValueError):
            return usage()
        del args[i:i + 2]
    if not (math.isfinite(timeout) and timeout > 0):
        return usage()
    try:
        if len(args) != 2:
            raise ValueError
        host, port = args[0], int(args[1])
    except ValueError:
        return usage()
    if not 1 <= port <= 65535:
        return usage()
    result = probe(host, port, timeout)
    print(f"{result}: {host}:{port}")
    return EXIT[result]


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
