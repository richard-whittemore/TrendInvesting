"""List one account download's keys and currencies, read-only.

Rule: docs/manual/ibkr-adapter.md, "Listing the account's balance keys";
ADR 0025's existing Connection and account-updates path are reused. This
command never places, modifies or cancels orders, never requests open
orders or positions, and sends nothing to the engine. It retains masked value
shapes and finite-number classifications. All printed text is masked
(docs/live-trading/lessons-2026-10.md L12).
"""
import math
import re
from argparse import Namespace

from account import account_tags, ask_account_values, end_account_values
from connection import Connection, NotConnected
from masking import mask_accounts


def value_shape(value, account):
    """Mask accounts before collapsing digit runs and limiting shape length (manual's balance-key diagnostic, L12)."""
    shape = re.sub(r"\d+", "9", mask_accounts(value, account))
    return shape[:80] + "…" if len(shape) > 80 else shape or "(empty)"


def read_summary(conn, account, timeout, emit, cleanup_failures):
    """Read one SettledCash summary and always cancel it (manual's balance-key diagnostic).

    Gateway 10.50's summary-slot lesson (account.py) requires one request
    only, on this diagnostic's throwaway connection.
    """
    req_id = conn.new_req_id()
    rows = []
    ended = False
    try:
        conn.api.req_account_summary(req_id, "All", "SettledCash")
        for event in conn.events(timeout):
            if not conn.connected or conn.last_drop_reason is not None:
                raise NotConnected("summary link lost")
            if event.name == "error" and len(event.args) >= 2 and event.args[0] == req_id:
                emit("summary refused by IBKR code {}".format(event.args[1]))
                break
            if event.name == "account_summary" and len(event.args) == 5:
                row_id, row_account, tag, value, currency = event.args
                if row_id == req_id and row_account == account and tag == "SettledCash":
                    try:
                        number = math.isfinite(float(value))
                    except (ValueError, OverflowError):
                        number = False
                    rows.append((currency, number, value_shape(value, account)))
            if event.name == "account_summary_end" and event.args == (req_id,):
                ended = True
                break
    except Exception as stopped:
        emit("summary failed: {}".format(type(stopped).__name__))
    finally:
        try:
            conn.api.cancel_account_summary(req_id)
        except Exception as stopped:
            cleanup_failures.append("summary cancel {}".format(type(stopped).__name__))
    emit("summary SettledCash rows={}".format(len(rows)))
    for currency, number, shape in rows:
        emit("summary SettledCash currency={} number={} shape={}".format(
            currency, "yes" if number else "no", shape))
    emit("summary ended" if ended else "summary did NOT end within {:g}s".format(timeout))


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
    shapes = {}
    failure = None
    connection_failure = None
    cleanup_failures = []
    summary_lines = []
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
                key, value, currency, row_account = event.args
                if row_account == account:
                    pairs.add((key, currency))
                    if "settled" in key.lower():
                        shapes[key, currency] = value_shape(value, account)
            if values.ended:
                break
    except Exception as stopped:
        connection_failure = "{}: {}".format(type(stopped).__name__, stopped)
    finally:
        if conn is not None:
            try:
                if values is not None:
                    end_account_values(conn, account, values)
                    if values.ended and failure is None and connection_failure is None:
                        read_summary(conn, account, args.timeout, summary_lines.append, cleanup_failures)
            except Exception as stopped:
                cleanup_failures.append("{}: {}".format(type(stopped).__name__, stopped))
            finally:
                try:
                    conn.api.disconnect()
                except Exception as stopped:
                    cleanup_failures.append("{}: {}".format(type(stopped).__name__, stopped))

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
        emit("Settled key={} currency={} shape={}".format(key, currency, shapes[key, currency]))
    if not settled:
        emit("Settled keys: none")
    for line in summary_lines:
        emit(line)
    if connection_failure is not None:
        emit("connection failed: " + connection_failure, err)
    if failure is not None:
        emit(failure, err)
    for cleanup_failure in cleanup_failures:
        emit("cleanup failed: " + cleanup_failure, err)
    if connection_failure is not None or cleanup_failures:
        return 2
    return 0 if ended and failure is None and all(key in present for key in needed) else 1
