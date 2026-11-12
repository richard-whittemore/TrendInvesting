"""List one account download's keys and currencies, read-only.

Rule: docs/manual/ibkr-adapter.md, "Listing the account's balance keys";
ADR 0025's existing Connection and account-updates path are reused. This
command never places, modifies or cancels orders, never requests open
orders or positions, and sends nothing to the engine. Values are discarded;
all printed text is masked (docs/live-trading/lessons-2026-10.md L12).
"""
import math
from argparse import Namespace

from account import account_tags, ask_account_values, end_account_values
from connection import Connection, NotConnected
from masking import mask_accounts


def command(args, api_factory, out, err):
    """Run the manual's balance-key diagnostic with a separate, non-sending client."""
    from cli import Refused, load_config

    account = ""

    def emit(message, stream=out):
        print(mask_accounts("account-keys: " + message, account), file=stream)

    try:
        if args.client_id <= 0:
            raise Refused("choose an unused positive client ID, never the adapter's own")
        if not math.isfinite(args.timeout) or args.timeout <= 0:
            raise Refused("--timeout must be a finite positive number of seconds")
        cfg = load_config(Namespace(config=args.config, secrets=args.secrets,
                                    account_key=args.account_key, client_id=None))
        account = cfg.account_id
        if args.client_id == cfg.client_id:
            raise Refused("choose an unused client ID, never the adapter's own")
        cfg = cfg._replace(client_id=args.client_id, paper_sending_enabled=False)
    except Refused as refusal:
        emit("refused: {}".format(refusal), err)
        return 1

    conn = None
    values = None
    pairs = set()
    failure = None
    connection_failure = None
    try:
        conn = Connection(api_factory(cfg), cfg, status_sink=lambda status: None)
        conn.connect()
        conn.settle_account_values()
        values = ask_account_values(conn, account)
        for event in conn.events(args.timeout):
            if not conn.connected or conn.last_drop_reason is not None:
                raise NotConnected("IB Gateway link was lost during the account download")
            values.take(event)
            if values.error is not None:
                code, _ = values.error
                failure = "account download refused by IBKR code {}".format(code)
                break
            if event.name == "account_value" and values.open and len(event.args) == 4:
                key, _, currency, row_account = event.args
                if row_account == account:
                    pairs.add((key, currency))
            if values.ended:
                break
    except Exception as stopped:
        connection_failure = "{}: {}".format(type(stopped).__name__, stopped)
    finally:
        if conn is not None:
            try:
                if values is not None:
                    end_account_values(conn, account, values)
            except Exception as stopped:
                connection_failure = "{}: {}".format(type(stopped).__name__, stopped)
            finally:
                try:
                    conn.api.disconnect()
                except Exception as stopped:
                    connection_failure = "{}: {}".format(type(stopped).__name__, stopped)

    for key, currency in sorted(pairs):
        emit("key={} currency={}".format(key, currency))
    ended = values is not None and values.ended
    emit("download ended" if ended else "download did NOT end within {:g}s".format(args.timeout))
    present = {key for key, _ in pairs}
    needed = account_tags(cfg)
    for key in needed:
        emit("needed {} {}".format(key, "present" if key in present else "MISSING"))
    settled = sorted((key, currency) for key, currency in pairs if "settled" in key.lower())
    for key, currency in settled:
        emit("Settled key={} currency={}".format(key, currency))
    if not settled:
        emit("Settled keys: none")
    if connection_failure is not None:
        emit("connection failed: " + connection_failure, err)
        return 2
    if failure is not None:
        emit(failure, err)
    return 0 if ended and failure is None and all(key in present for key in needed) else 1
