"""Stands in for /opt/trend-investing/adapter-venv/bin/python3 in the
first-day rehearsal: the adapter units' own command lines (python3 -m
adapter.ibkr run ..., Boost's ti-adapter-boost and the Traditional IRA's
ti-adapter@trad-ira) run the real adapter code, with the scripted fake
gateway (adapter/ibkr/tests/fake_tws.py, invented accounts and figures, in
the shapes ibapi delivers them: every account value a string) in place of
ibapi and IB Gateway, which the container does not have. The same-day probe
(python3 -m adapter.ibkr probe ...) runs as installed.
"""
import sys
from datetime import datetime, timezone

# The installed adapter, laid out as adapter/__main__.py lays it out: its
# modules import one another by their own names.
sys.path.insert(0, "/opt/trend-investing/adapter")
sys.path.insert(0, "/opt/trend-investing/adapter/adapter/ibkr")
sys.path.append("/opt/rehearsal/tests")

import hashlib  # noqa: E402

import ibapi_pin  # noqa: E402
from cli import main  # noqa: E402
from fake_tws import FakeTWS, contract_details  # noqa: E402

# The rehearsal's stand-in paper accounts (synthetic, never a real number):
# each has a letter after DU, the shape the runbook's account check must
# accept. The one this start's account file names (--secrets, --account-key)
# is the one the scripted Gateway's login manages: Boost's DUX1234567, the
# Traditional IRA's DUX1234568.
ACCOUNT = "DUX1234567"

if sys.argv[1:4] == ["-m", "adapter.ibkr", "probe"]:
    # The same-day probe reads two files and talks to no Gateway: it runs as installed.
    sys.exit(main(sys.argv[3:]))
if sys.argv[1:4] != ["-m", "adapter.ibkr", "run"]:
    sys.exit("gateway-adapter: not the adapter's command: " + " ".join(sys.argv[1:]))
args = sys.argv[4:]
if "--secrets" in args and "--account-key" in args:
    key = args[args.index("--account-key") + 1]
    with open(args[args.index("--secrets") + 1], encoding="utf-8") as f:
        named = [line.split("=", 1)[1].strip() for line in f.read().splitlines() if line.startswith(key + "=")]
    ACCOUNT = named[0] if named else ACCOUNT


class AnyNameGateway(FakeTWS):
    """The scripted Gateway, answering a contract lookup for any name as ibapi delivers one (fake_tws.contract_details):
    one US-dollar stock on SMART, its invented conId derived from the name."""

    def req_contract_details(self, req_id, contract_):
        if contract_.symbol not in self.contract_details:
            con_id = 800000000 + int(hashlib.sha256(contract_.symbol.encode()).hexdigest()[:6], 16)
            self.contract_details[contract_.symbol] = [contract_details(contract_.symbol, con_id)]
        super().req_contract_details(req_id, contract_)


pin = ibapi_pin.read_pin("/opt/trend-investing/adapter/versions.env")
gateway = AnyNameGateway()
# IB Gateway's clock is the (rehearsal's) clock the adapter sees.
gateway.server_time = int(datetime.now(timezone.utc).timestamp())
gateway.managed_accounts = (ACCOUNT,)
gateway.summary = [(ACCOUNT, "NetLiquidation", "100000.00", "USD"),
                   (ACCOUNT, "TotalCashValue", "100000.00", "USD"),
                   (ACCOUNT, "ExcessLiquidity", "100000.00", "USD"),
                   # IBKR's settled cash, which only an IRA's adapter reads (account_snapshot_version 4); a
                   # Boost reading takes no notice of it.
                   (ACCOUNT, "SettledCash", "100000.00", "USD")]
gateway.positions = []
# As IB Gateway 10.50 did on 2026-10-25: two account summaries a connection,
# a cancel frees neither, every later one refused with IBKR's error 322. The
# adapter reads its balances from the account values, so it never meets it.
gateway.summary_slots = 2
sys.exit(main(sys.argv[3:], api_factory=lambda cfg: gateway,
              ibapi_version=lambda: tuple(int(p) for p in pin[0].split(".")),
              protobuf_version=lambda: pin[2]))
