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
import re
import signal
import sys
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from config import AdapterConfig, ConfigError
from ibapi_pin import IbapiNotPinned, check_installed
from loop import AdapterLoop

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


# An IBKR account number: one or two capital letters and digits (U1234567,
# DU1234567).
ACCOUNT_NUMBER = re.compile(r"\b[A-Z]{1,2}[0-9]{5,9}\b")


def mask_accounts(text, account_id):
    """text with the configured account and anything shaped like an IBKR account number replaced by <account>.

    A stop message is pasted into tickets and chats, and the repository is
    public: no account number leaves the machine that way.
    """
    if account_id:
        text = text.replace(account_id, "<account>")
    return ACCOUNT_NUMBER.sub("<account>", text)


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
            # An unreadable zone file (permissions, a directory in its place) is refused like a missing one.
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
    return parser


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
        return AdapterConfig.from_mapping(values)
    except ConfigError as err:
        message = str(err)
        # A refusal may quote the account; it is masked (mask_accounts).
        account = values.get("account_id")
        message = mask_accounts(message, account if isinstance(account, str) else "")
        raise Refused("the configuration {} is refused: {}".format(args.config, message)) from None


def _real_api(cfg):
    from real_client import IbapiClient
    return IbapiClient(config=cfg)


def _engine_connect(path, timeout):
    from engine_client import connector
    return connector(path, timeout)


def main(argv=None, api_factory=None, engine_connect=None, should_stop=None, err=None, poll=POLL,
         ibapi_version=None, protobuf_version=None, today=None, now=None, zone_lookup=None):
    """Run the command; returns the process exit status.

    api_factory, engine_connect, should_stop, ibapi_version and protobuf_version replace the
    real IBKR client, the engine's socket, the signal handlers and the
    imported ibapi's version (for the tests and the rehearsal, which run
    without ibapi); today replaces the session's date (the rehearsal's
    synthetic session) and now the adapter's clock (the rehearsal's, kept
    with its synthetic IBKR clock); zone_lookup replaces zoneinfo.ZoneInfo
    in the check that IBKR's time zones are installed; poll is how long one
    step waits for a callback.
    """
    err = err or sys.stderr
    args = _parser().parse_args(argv)
    try:
        cfg = load_config(args)
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
                           args.engine_log, poll=poll, streams=args.streams,
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
