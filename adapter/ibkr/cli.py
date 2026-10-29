"""The adapter's start command: `python3 -m adapter.ibkr run ...` (docs/manual/paper-trading.md).

Everything is read and checked before anything connects: the configuration
(config.AdapterConfig, strict), the account number from exactly one place
(the configuration file, or one named KEY=VALUE line of the account's own
file, read bare and never echoed: the repository is public), the time
zones IBKR's reports name (require_zones), the installed ibapi against the
pin (ibapi_pin), and the engine's durable log (read at start by the loop).
Then the loop runs (loop.AdapterLoop) until SIGTERM or SIGINT, or until it
stops for a person. Exit status: 0 for a requested stop, 1 for a refusal or
a stop for a person (the reason is written to standard error), 2 for a
usage error.
"""
import argparse
import json
import logging
import os
import re
import signal
import sys
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from config import AdapterConfig, ConfigError
from ibapi_pin import IbapiNotPinned, check_installed
from loop import AdapterLoop
from masking import mask_accounts

# How long one exchange with the engine may take (seconds): the engine
# flushes the input's decisions to its durable log before it replies.
ENGINE_TIMEOUT = 30.0
# How long one step of the loop waits for IB Gateway's next callback (seconds).
POLL = 1.0

DESCRIPTION = """\
Run one engine's IBKR adapter: connect to IB Gateway with the configured
client ID, read and match IBKR's account, open orders and executions, then
pass IBKR's reports to the engine and hand each released order marked send
to IB Gateway once. Start the engine first: its durable log (--engine-log)
must exist. With --streams, it also delivers the run's market-data stream
(the data job's files in <streams>/<run>/) to the engine, each batch behind
a broker snapshot; the run's folder must exist (the data job's warm-up).

An order is sent only when the configuration is mode "paper", names a paper
account (DU...) that the IBKR login manages, and sets
"paper_sending_enabled": true. Left out, the switch is off and nothing is
sent. Gateway's Read-Only API must also be off for IBKR to accept an order.
"""


class Refused(Exception):
    """The command's inputs are unusable; nothing was started."""


# The time zones IBKR's reports name: execution times are stated in
# "US/Eastern" (orders.parse_execution_time), a legacy name Ubuntu 24.04
# ships only in tzdata-legacy, which `provision.sh base` installs. A missing
# zone is refused at start, before anything connects, rather than stopping
# the adapter at the first execution report mid-session (fail closed,
# AGENTS.md).
IBKR_ZONES = ("US/Eastern",)


def require_zones(lookup=ZoneInfo):
    """Refused unless every zone in IBKR_ZONES resolves through lookup (zoneinfo.ZoneInfo by default)."""
    for name in IBKR_ZONES:
        try:
            lookup(name)
        except ZoneInfoNotFoundError:
            raise Refused("the time zone {} that IBKR's execution times name is not installed; install the "
                          "tzdata-legacy package with `sudo deploy/provision.sh base` and start the adapter "
                          "again".format(name)) from None
        except ValueError:
            # zoneinfo raises ValueError for a zone file that exists but whose content it cannot parse.
            raise Refused("the time zone {} that IBKR's execution times name is present but corrupt (its "
                          "content cannot be parsed); reinstall the package that provides it with `sudo "
                          "apt-get install --reinstall tzdata-legacy tzdata` (or restore the zone database "
                          "from a good copy) and start the adapter again".format(name)) from None
        except OSError as err:
            # OSError covers PermissionError and other read errors; each is refused as an unreadable zone.
            # A directory in the zone's place never gets here: zoneinfo skips non-regular files, so it is refused above as not installed.
            raise Refused("the time zone {} that IBKR's execution times name cannot be read ({}); restore the "
                          "zone database's permissions or reinstall it with `sudo apt-get install --reinstall "
                          "tzdata-legacy tzdata` and start the adapter again".format(
                              name, err.strerror or type(err).__name__)) from None


def _parser():
    parser = argparse.ArgumentParser(prog="python3 -m adapter.ibkr", description=DESCRIPTION,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)
    run = commands.add_parser("run", help="run the adapter until stopped", description=DESCRIPTION,
                              formatter_class=argparse.RawDescriptionHelpFormatter)
    run.add_argument("--config", required=True, metavar="FILE",
                     help="the adapter's configuration, a JSON object (config.py): host, port, client_id, mode, "
                          "the timing values, con_ids, and paper_sending_enabled (true to send in paper; "
                          "left out means false)")
    run.add_argument("--client-id", type=int, metavar="N",
                     help="the client ID the instance's settings give (TI_ADAPTER_CLIENT_ID in "
                          "/etc/trend-investing/engines/<instance>.env); the adapter refuses to start unless its "
                          "configuration's client_id is the same. Left out, not checked")
    run.add_argument("--pending", required=True, metavar="FILE",
                     help="the adapter's pending-input file; its other files sit beside it with suffixes "
                          "(.handover, .refused, .outbox, .held, ...). On the server: "
                          "/var/lib/trend-investing/adapters/<account>/pending.json")
    run.add_argument("--engine-log", required=True, metavar="FILE",
                     help="the engine's durable log, the file given to the engine as -log")
    run.add_argument("--socket", required=True, metavar="PATH", help="the engine's socket, as given to it as -socket")
    run.add_argument("--streams", metavar="DIR",
                     help="the folder holding the engine's stream folders, one per run, as the data job writes "
                          "them: the run's own is DIR/<run>, <run> being --engine-log's name without .log. On the "
                          "server: /var/lib/trend-investing/data/streams/boost. Left out, no stream is delivered")
    run.add_argument("--cash-ledger", metavar="FILE",
                     help="the instance's Flex ledger of deposits and withdrawals (flex.py), whose journalled "
                          "transfers the post-close cash check subtracts before its 1%% bound; it must be in a "
                          "folder named for the instance, as --engine-log's is. On the server: "
                          "/var/lib/trend-investing/flex/<instance>/cash-movements.jsonl. Left out, none is "
                          "subtracted")
    run.add_argument("--secrets", metavar="FILE",
                     help="a KEY=VALUE file holding the account number (with --account-key), when the "
                          "configuration does not name account_id. On the server: the adapter's own account file, "
                          "/etc/trend-investing/accounts/boost.env")
    run.add_argument("--account-key", metavar="NAME", help="the line of --secrets naming the account, e.g. "
                                                           "IBKR_ACCOUNT_BOOST")
    run.add_argument("--ibapi-record", required=True, metavar="FILE",
                     help="the install record provision.sh twsapi writes (/opt/trend-investing/twsapi/INSTALLED)")
    run.add_argument("--versions", required=True, metavar="FILE",
                     help="the pin file installed with the adapter's code (deploy/versions.env)")
    reader = commands.add_parser(
        "flex", help="read IBKR's Cash Transactions once and record each new deposit or withdrawal",
        description="Read the owner's Flex query from IBKR's Flex Web Service once (flex.py; "
                    "docs/manual/reconciliation.md, \"Deposits and withdrawals, read from IBKR\") and add each "
                    "deposit or withdrawal not yet in the ledger to it. Exit status 0 for a reading, 1 when the "
                    "reader is set aside (why on standard error and in the ledger's status file). With --resolve, "
                    "only --ledger is needed: the owner's decision is recorded beside the ledger and nothing is read.")
    reader.add_argument("--secrets", metavar="FILE",
                        help="a KEY=VALUE file holding TI_IBKR_FLEX_TOKEN and TI_IBKR_FLEX_QUERY_ID (on the server: "
                             "/etc/trend-investing/secrets.env, handed over as a credential)")
    reader.add_argument("--account-secrets", metavar="FILE",
                        help="the KEY=VALUE file naming the instance's IBKR account (on the server: "
                             "/etc/trend-investing/accounts/<instance>.env)")
    reader.add_argument("--instance", choices=("boost", "trad-ira", "roth-ira"),
                        help="the account the reading is for; its line in --account-secrets is IBKR_ACCOUNT_ and the "
                             "instance in capitals, a dash as an underscore (IBKR_ACCOUNT_TRAD_IRA)")
    reader.add_argument("--engine-log", metavar="FILE",
                        help="the engine's durable log, read only, for the net liquidation before each transfer")
    reader.add_argument("--ledger", required=True, metavar="FILE",
                        help="the instance's cash-movement ledger, one record a line; its status file is beside it "
                             "(on the server: /var/lib/trend-investing/flex/<instance>/cash-movements.jsonl)")
    reader.add_argument("--resolve", action="append", metavar="TRANSACTION_ID=DECISION",
                        help="record the owner's decision on a transfer IBKR dates by day alone on the day of a "
                             "first-run approval given after that session's close: in-first-book (the starting book "
                             "holds it) or after-approval; a recorded decision is never changed, and nothing is read "
                             "(docs/manual/reconciliation.md)")
    probe = commands.add_parser(
        "probe", help="the same-day probe: check one adapter start's journal and the engine's log, read only",
        description="Read the adapter's journal lines for one start (journalctl _SYSTEMD_INVOCATION_ID=<its "
                    "invocation> -o cat) and the engine's durable log, and print one line per check, then \"probe: "
                    "passed\" (exit 0) or \"probe: FAILED, n check(s)\" (exit 1). Connects to nothing "
                    "(probe.py; docs/manual/ibkr-adapter.md, \"The same-day probe\").")
    probe.add_argument("--journal", required=True, metavar="FILE", help="the adapter's journal lines for one start")
    probe.add_argument("--engine-log", required=True, metavar="FILE", help="the engine's durable log")
    probe.add_argument("--client-id", required=True, type=int, metavar="N",
                       help="the instance's client ID (TI_ADAPTER_CLIENT_ID): 12 for trad-ira")
    probe.add_argument("--snapshot-version", type=int, choices=(3, 4), default=4,
                       help="the account snapshot version the engine reads: 4 for an IRA (the default), 3 for Boost")
    probe.add_argument("--mode", choices=("dry-run", "paper", "live"), default="dry-run",
                       help="the mode the adapter must have started in (default dry-run)")
    return parser


def check_cash_ledger(ledger, engine_log):
    """Refused unless ledger, when given, is in a folder named for the instance engine_log's folder is.

    The Flex reader's records carry no account (flex.py): each instance's
    ledger is in its own folder, flex/<instance>/, as the engine's state
    folder is engines/<instance>/, so a ledger in another instance's folder
    is another account's (ADR 0019, amended 2026-10-21)."""
    if ledger is None:
        return
    instance = os.path.basename(os.path.dirname(os.path.abspath(ledger)))
    state = os.path.basename(os.path.dirname(os.path.abspath(engine_log)))
    if instance != state:
        raise Refused("the Flex ledger {} is instance {}'s, but the engine's log is in {}'s folder: a ledger is read "
                      "only by its own instance".format(ledger, instance, state))


def _read_config(path):
    try:
        with open(path, encoding="utf-8") as f:
            values = json.load(f)
    except OSError as err:
        raise Refused("cannot read the configuration {}: {}".format(path, err.strerror or err)) from None
    except ValueError:
        raise Refused("the configuration {} is not JSON".format(path)) from None
    if not isinstance(values, dict):
        raise Refused("the configuration {} is not a JSON object".format(path))
    return values


# What may not appear in an account number read from a KEY=VALUE line: a
# quote or a space would make an account IBKR's login does not name, and
# the refusal could not show the value to explain it.
_NOT_BARE = re.compile(r"[\s\"']")


def _read_secret(path, key):
    """The value of key's line in a KEY=VALUE file, read bare, never echoed in an error.

    Whitespace around the value (a trailing space, a CRLF line end) is
    dropped; a value with a quote or a space inside it is refused.
    """
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
    except OSError as err:
        raise Refused("cannot read the secrets file {}: {}".format(path, err.strerror or err)) from None
    found = [line.split("=", 1)[1] for line in lines if line.split("=", 1)[0] == key and "=" in line]
    value = found[0].strip() if len(found) == 1 else ""
    if not value:
        raise Refused("the secrets file {} does not hold exactly one filled-in {} line".format(path, key))
    if _NOT_BARE.search(value):
        raise Refused("the {} line of {} is not KEY=VALUE with a bare value: write the account number with no "
                      "quotes and no spaces".format(key, path))
    return value


def load_config(args):
    """The AdapterConfig the arguments name, with the account from exactly one place; Refused otherwise."""
    values = _read_config(args.config)
    from_secrets = args.secrets is not None or args.account_key is not None
    if from_secrets:
        if args.secrets is None or args.account_key is None:
            raise Refused("--secrets and --account-key go together")
        if "account_id" in values:
            raise Refused("the account is named in one place only: the configuration's account_id or the secrets "
                          "file, not both")
        values["account_id"] = _read_secret(args.secrets, args.account_key)
    elif "account_id" not in values:
        raise Refused("no account: name account_id in the configuration or give --secrets and --account-key")
    try:
        cfg = AdapterConfig.from_mapping(values)
    except ConfigError as err:
        message = str(err)
        # A refusal may quote the account; it is masked (mask_accounts).
        account = values.get("account_id")
        message = mask_accounts(message, account if isinstance(account, str) else "")
        raise Refused("the configuration {} is refused: {}".format(args.config, message)) from None
    # One adapter per account, each with its own client ID (ADR 0026 point 8):
    # the settings the unit hands over and the configuration must agree, so
    # a configuration copied from another account's never connects as it.
    if getattr(args, "client_id", None) is not None and args.client_id != cfg.client_id:
        raise Refused("the configuration {} names client_id {}, but the instance's settings give {} "
                      "(TI_ADAPTER_CLIENT_ID): each account's adapter has its own client ID".format(
                          args.config, cfg.client_id, args.client_id))
    return cfg


def _real_api(cfg):
    from real_client import IbapiClient
    return IbapiClient(config=cfg)


def _engine_connect(path, timeout):
    from engine_client import connector
    return connector(path, timeout)


def main(argv=None, api_factory=None, engine_connect=None, should_stop=None, err=None, poll=POLL,
         ibapi_version=None, protobuf_version=None, today=None, now=None, zone_lookup=None, flex_get=None,
         flex_sleep=None, out=None):
    """Run the command; returns the process exit status.

    api_factory, engine_connect, should_stop, ibapi_version and protobuf_version replace the
    real IBKR client, the engine's socket, the signal handlers and the
    imported ibapi's version (for the tests and the rehearsal, which run
    without ibapi); today replaces the session's date (the rehearsal's
    synthetic session) and now the adapter's clock (the rehearsal's, kept
    with its synthetic IBKR clock); zone_lookup replaces zoneinfo.ZoneInfo
    in the check that IBKR's time zones are installed; poll is how long one
    step waits for a callback; flex_get and flex_sleep replace the Flex
    reader's HTTP GET and its wait (`flex`; the tests, which use no network).
    """
    err = err or sys.stderr
    args = _parser().parse_args(argv)
    if args.command == "probe":
        import probe
        lines, status = probe.run(args.journal, args.engine_log, args.client_id, args.snapshot_version, args.mode)
        print("\n".join(lines), file=out or sys.stdout)
        return status
    if args.command == "flex":
        import flex
        return flex.command(args, err, **({"get": flex_get} if flex_get else {}),
                            **({"sleep": flex_sleep} if flex_sleep else {}), **({"now": now} if now else {}))
    try:
        cfg = load_config(args)
        check_cash_ledger(args.cash_ledger, args.engine_log)
        require_zones(zone_lookup or ZoneInfo)
        version = check_installed(args.ibapi_record, args.versions,
                                  **({"installed_version": ibapi_version} if ibapi_version else {}),
                                  **({"installed_protobuf": protobuf_version} if protobuf_version else {}))
    except (Refused, IbapiNotPinned) as refusal:
        print("refused: {}".format(refusal), file=err)
        return 1
    handler = logging.StreamHandler(err)
    handler.setFormatter(logging.Formatter("%(asctime)s %(message)s"))
    logger = logging.getLogger("ibkr")
    logger.addHandler(handler)
    logger.setLevel(logging.INFO)
    stop = {"requested": False}
    if should_stop is None:
        def request_stop(signum, frame):
            stop["requested"] = True
        signal.signal(signal.SIGTERM, request_stop)
        signal.signal(signal.SIGINT, request_stop)
        should_stop = lambda: stop["requested"]  # noqa: E731
    try:
        logger.info("ibkr adapter starting mode=%s client_id=%d sending=%s ibapi=%s", cfg.mode, cfg.client_id,
                    "on" if cfg.paper_sending_enabled else "off", version)
        loop = AdapterLoop(cfg, (api_factory or _real_api)(cfg),
                           (engine_connect or _engine_connect)(args.socket, ENGINE_TIMEOUT), args.pending,
                           args.engine_log, poll=poll, streams=args.streams, cash_ledger=args.cash_ledger,
                           **({"today": today} if today is not None else {}),
                           **({"now": now} if now is not None else {}))
        loop.run(should_stop)
        logger.info("ibkr adapter stopped on request")
    except Exception as stopped:  # every stop is reported, never swallowed
        print("adapter stopped: {}: {}".format(type(stopped).__name__, mask_accounts(str(stopped), cfg.account_id)),
              file=err)
        return 1
    finally:
        logger.removeHandler(handler)
    return 0
