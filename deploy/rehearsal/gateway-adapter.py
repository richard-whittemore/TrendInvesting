"""Stands in for /opt/trend-investing/adapter-venv/bin/python3 in the
first-day rehearsal: the adapter unit's own command line (python3 -m
adapter.ibkr run ...) runs the real adapter code, with the scripted fake
gateway (adapter/ibkr/tests/fake_tws.py, invented account and figures) in
place of ibapi and IB Gateway, which the container does not have.
"""
import sys
import time
from datetime import datetime, timezone
from decimal import Decimal

# The installed adapter, laid out as adapter/__main__.py lays it out: its
# modules import one another by their own names.
sys.path.insert(0, "/opt/trend-investing/adapter")
sys.path.insert(0, "/opt/trend-investing/adapter/adapter/ibkr")
sys.path.append("/opt/rehearsal/tests")

import ibapi_pin  # noqa: E402
from cli import main  # noqa: E402
from fake_tws import FakeTWS  # noqa: E402

# The rehearsal's stand-in paper account (synthetic, never a real number): it has a
# letter after DU, the shape the runbook's account check must accept.
ACCOUNT = "DUX1234567"

if sys.argv[1:4] != ["-m", "adapter.ibkr", "run"]:
    sys.exit("gateway-adapter: not the adapter's command: " + " ".join(sys.argv[1:]))

pin = ibapi_pin.read_pin("/opt/trend-investing/adapter/versions.env")
gateway = FakeTWS()
# IB Gateway's clock is the (rehearsal's) clock the adapter sees.
gateway.server_time = int(datetime.now(timezone.utc).timestamp())
gateway.managed_accounts = (ACCOUNT,)
gateway.summary = [(ACCOUNT, "NetLiquidation", Decimal("100000"), "USD"),
                   (ACCOUNT, "TotalCashValue", Decimal("100000"), "USD"),
                   (ACCOUNT, "ExcessLiquidity", Decimal("100000"), "USD")]
gateway.positions = []
# As IB Gateway 10.50 did on 2026-10-25: an account summary asked for within
# a second of cancelling the one before is refused with IBKR's error 322
# (two at a time, a cancel counted late), which the adapter must wait out.
gateway.clock = time.monotonic
gateway.summary_cancel_grace = 1.0
sys.exit(main(sys.argv[3:], api_factory=lambda cfg: gateway,
              ibapi_version=lambda: tuple(int(p) for p in pin[0].split(".")),
              protobuf_version=lambda: pin[2]))
