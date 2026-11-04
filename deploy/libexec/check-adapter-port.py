#!/usr/bin/env python3
"""Check the adapter's Gateway port before startup (docs/manual/server.md)."""
import json
from pathlib import Path
import re
import sys


def main():
    config, gateway = map(Path, sys.argv[1:])
    try:
        if str(gateway) == "4002":
            expected = 4002
        else:
            ports = [line.split("=", 1)[1] for line in gateway.read_text().splitlines()
                     if line.startswith("TI_GATEWAY_API_PORT=")]
            if len(ports) != 1 or not re.fullmatch(r"[0-9]{1,5}", ports[0]):
                raise ValueError()
            expected = int(ports[0])
            if not 1 <= expected <= 65535:
                raise ValueError()
    except (OSError, ValueError):
        print(f"adapter port check: {gateway}: requires a valid TI_GATEWAY_API_PORT", file=sys.stderr)
        return 64
    try:
        port = json.loads(config.read_text())["port"]
        if type(port) is not int or port != expected:
            raise ValueError()
    except (OSError, ValueError, KeyError, TypeError):
        print(f"adapter port check: {config}: expected port {expected} for its Gateway ({gateway})", file=sys.stderr)
        return 64
    return 0


if __name__ == "__main__":
    sys.exit(main())
