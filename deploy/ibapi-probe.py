#!/usr/bin/env python3
"""Check from another machine whether an IB Gateway API port serves this client.

Usage: python3 deploy/ibapi-probe.py HOST PORT [--timeout SECONDS]

Sends only the IB API opening handshake ("API\\0" and a length-prefixed
version range, v100..187), reads at most the first reply, and closes. It
never sends a client ID, a request or an order. SECONDS (default 5, at most
300) bounds the whole probe, name lookup included.

  EXPOSED             exit 1  the server sent any reply bytes to the handshake
                              (not checked to be an API server version)
  NOT-SERVED <why>    exit 0  no API was served to this client. <why> is
                              refused (nothing accepted the connection),
                              closed (accepted, then closed or reset with no
                              reply) or silent (no answer before the timeout)
  ERROR <why>         exit 2  proves nothing: lookup (the name did not
                              resolve in time), no usable address (no socket
                              could be made or tried), or a usage error

Only EXPOSED fails (rules/ADR 0026: the Gateway serves programs on its own
server only). A pass from another machine means something only next to a
control: the same probe run on the server itself, against 127.0.0.1, must
print EXPOSED, which shows the Gateway is up and the probe works.
"""
import math
import socket
import struct
import sys
import threading
import time

VERSIONS = b"v100..187"
MAX_TIMEOUT = 300.0  # keeps the value inside what thread and socket timeouts accept
EXIT = {"EXPOSED": 1, "NOT-SERVED": 0, "ERROR": 2}


def resolve(host, port, timeout, resolver):
    """Addresses for host, or None if the lookup fails or outlasts timeout."""
    box = []

    def work():
        try:
            box.append(resolver(host, port, type=socket.SOCK_STREAM))
        except Exception:  # any lookup failure is an ERROR
            box.append(None)

    t = threading.Thread(target=work, daemon=True)
    t.start()
    t.join(timeout)
    return box[0] if box else None


def probe(host, port, timeout, resolver=socket.getaddrinfo):
    deadline = time.monotonic() + timeout

    def left():
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("the probe's time is used up")
        return remaining

    infos = resolve(host, port, timeout, resolver)
    if not infos:
        return "ERROR lookup"
    why = None  # the first reason an address served nothing
    for family, kind, proto, _, addr in infos:
        if deadline <= time.monotonic():
            break
        s = None
        try:
            s = socket.socket(family, kind, proto)
            s.settimeout(left())
            s.connect(addr)
        except OSError as e:  # no such socket here, refused, unreachable, timed out
            if s is not None:
                s.close()
                why = why or ("refused" if isinstance(e, ConnectionRefusedError) else "silent")
            continue
        try:
            s.settimeout(left())
            s.sendall(b"API\x00" + struct.pack(">I", len(VERSIONS)) + VERSIONS)
            s.settimeout(left())
            data = s.recv(64)
        except (ConnectionResetError, BrokenPipeError):
            return "NOT-SERVED closed"
        except OSError:  # includes a timeout: connected but silent
            return "NOT-SERVED silent"
        finally:
            s.close()
        return "EXPOSED" if data else "NOT-SERVED closed"
    return f"NOT-SERVED {why}" if why else "ERROR no usable address"


def usage():
    print("usage: ibapi-probe.py HOST PORT [--timeout SECONDS]  "
          "(PORT 1-65535, SECONDS above 0 and at most 300)", file=sys.stderr)
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
    if not (math.isfinite(timeout) and 0 < timeout <= MAX_TIMEOUT):
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
    return EXIT[result.split()[0]]


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
