"""Private server replay for plan.md section 9 (owner decision 2026-10-28).

The input is a recorded engine log (including the account snapshots and marks),
validated against its committed hash chain. The evidence copy is retained even on failure. No
adapter is started; every released order must have send=false (ADR 0026).
"""
import datetime as dt
import hashlib
import json
import importlib.util
import re
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from zoneinfo import ZoneInfo


CONFIGURATION = 'strategy.target-weight.configuration'
APPROVAL = 'operator.run.approved'
DECISIONS = {'strategy.targets.decided', 'strategy.order.planned', 'risk.order.released'}


def log_reader():
    # Share the adapter's durable authority and canonical hash implementation (ADR 0027).
    path = Path(__file__).resolve().parents[1] / 'adapter/marketdata/engine_log.py'
    if not path.exists():
        path = Path(os.environ.get('TI_ADAPTER_ROOT', '/opt/trend-investing/adapter')) / 'adapter/marketdata/engine_log.py'
    spec = importlib.util.spec_from_file_location('ira_engine_log', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def recording(path):
    reader = log_reader()
    try:
        log = reader.read(path)
    except reader.LogUnreadable as error:
        raise ValueError('invalid durable log at line ' + str(error.line) + '; inspect private evidence') from None
    if log.chain_break is not None:
        raise ValueError('durable log chain differs at record ' + str(log.chain_break))
    # Keep payload bytes, including number spellings and whitespace (ADR 0016).
    wanted = {n: (kind, env) for group in log.groups for n, kind, env in group}
    rows, decisions = [], []
    with path.open('rb') as source:
        for n, raw in enumerate(source, 1):
            if n not in wanted:
                continue
            kind, env = wanted[n]
            if kind == 'input':
                record = json.loads(raw)['record']
                rows.append((env, reader._payload(path, n, raw.rstrip(b'\n'), record).decode()))
            else:
                decisions.append(env)
    return log, rows, decisions


def inputs(path):
    return (row for row, _ in recording(path)[1])


def rewrite(row, payload, sequence, header):
    envelope = dict(row, sequence=sequence, strategy_version=header['strategy_version'],
                    configuration_hash=header['configuration_hash'],
                    payload_hash=hashlib.sha256(payload.encode()).hexdigest())
    del envelope['payload']
    return json.dumps(envelope, separators=(',', ':'))[:-1] + ',"payload":' + payload + '}'


def check_identity(recorded, current):
    for field in ('configuration_hash', 'strategy_version'):
        if recorded[field] != current[field]:
            raise ValueError('recorded ' + field + ' differs from installed engine/configuration')


def redact(text, account=''):
    # Identifiers can occur in IDs and free-text reasons too (lessons-2026-10.md L12).
    if account:
        text = text.replace(account, '<account>')
    text = re.sub(r'("account_id"\s*:\s*)"[^"]*"', r'\1"<account>"', text)
    return re.sub(r'(?:DU[A-Za-z0-9]+|U[0-9]+)', '<account>', text)


def first_difference(expected, actual, path='decision'):
    if isinstance(expected, dict) and isinstance(actual, dict):
        for key in sorted(set(expected) | set(actual)):
            if key == 'client_order_id':
                continue
            if key not in expected or key not in actual:
                return path + '.' + key + ': missing field'
            difference = first_difference(expected[key], actual[key], path + '.' + key)
            if difference:
                return difference
    elif isinstance(expected, list) and isinstance(actual, list):
        for i, (left, right) in enumerate(zip(expected, actual)):
            difference = first_difference(left, right, path + '[' + str(i) + ']')
            if difference:
                return difference
        if len(expected) != len(actual):
            return path + '.length: recorded ' + str(len(expected)) + ', replay ' + str(len(actual))
    elif expected != actual:
        return path + ': recorded ' + json.dumps(expected) + ', replay ' + json.dumps(actual)
    return None


def month_decisions(decisions, close):
    selected = [d for d in decisions if d['type'] == 'strategy.targets.decided'
                and d['payload']['session_period_end'] == close]
    if len(selected) != 1:
        raise ValueError('requested close did not produce exactly one decision')
    decision = selected[0]
    tag = decision['id']
    selected = []
    for row in decisions:
        if row is not decision and not (row['type'] in DECISIONS and row['payload'].get('tag') == tag):
            continue
        payload = dict(row['payload'])
        # Release plan references contain the ignored client order ID (ADR 0026 point 2).
        if 'client_order_id' in payload and payload.get('plan_id') == 'order-planned:' + payload['client_order_id']:
            payload['plan_id'] = 'order-planned:<client-order-id>'
        envelope = {k: v for k, v in row.items() if k not in
                    ('sequence', 'strategy_version', 'configuration_hash', 'payload_hash')}
        client = payload.get('client_order_id')
        if client:
            for field in ('id', 'causation_id', 'correlation_id'):
                for prefix in ('order-planned:', 'risk-order-released:'):
                    if envelope.get(field) == prefix + client:
                        envelope[field] = prefix + '<client-order-id>'
        envelope['payload'] = payload
        selected.append(envelope)
    return selected


def compare_month(recorded, replayed, close):
    difference = first_difference(month_decisions(recorded, close), month_decisions(replayed, close))
    if difference:
        raise ValueError('replay first difference: ' + difference)


def service_guard(instance):
    # The unit can be activating before its socket exists (ADR 0019 fail-closed state).
    if shutil.which('systemctl') is None and sys.platform != 'linux':
        return
    try:
        state = subprocess.run(['systemctl', 'show', 'ti-engine@' + instance + '.service',
                                '--property=ActiveState', '--value'], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError('cannot establish engine unit state') from None
    if state.returncode or state.stdout.strip() not in ('inactive', 'failed'):
        raise ValueError('engine unit is running, starting, stopping or unknown; stop it before rehearsing')


def finish(process):
    # Only the child this command started (docs/development.md, Ending processes).
    if process.poll() is None:
        process.terminate()
    try:
        status = process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
        raise ValueError('engine did not stop cleanly; inspect evidence') from None
    if status:
        raise ValueError('engine exited unsuccessfully; inspect evidence')


def approval_history(source, rows, folder):
    # Fresh approvals are checked against their immutable predecessor logs (ADR 0027).
    approvals = [(row, raw) for row, raw in rows if row['type'] == APPROVAL]
    if not approvals:
        return None
    if len(approvals) != 1 or rows[1][0]['type'] != APPROVAL:
        raise ValueError('approval must be the recorded second input')
    approval, raw = approvals[0]
    previous_name = approval['payload'].get('previous_log')
    if previous_name:
        if Path(previous_name).name != previous_name or previous_name == source.name or not previous_name.endswith('.log'):
            raise ValueError('invalid approval predecessor')
        previous = source.parent / previous_name
        latest_time = previous.stat().st_mtime_ns
        # Include older state even when the immediate predecessor stopped after its
        # configuration; the engine's stateBefore checks that history (ADR 0027).
        for candidate in source.parent.glob('*.log'):
            if candidate == source or candidate.stat().st_mtime_ns > latest_time:
                continue
            copied = folder / candidate.name
            shutil.copy2(candidate, copied)
            prior, _, _ = recording(copied)
            if candidate.name == previous_name and prior.final_hash != approval['payload']['previous_log_final_hash']:
                raise ValueError('approval predecessor final hash differs')
    path = folder.parent / 'approval.json'
    path.write_text(raw)
    return path


def check_close(rows, close):
    end = dt.datetime.strptime(close, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=dt.timezone.utc)
    day = end.astimezone(ZoneInfo('America/New_York')).date().isoformat()
    sessions = sorted({s for r in rows if r['type'] == 'market.session.calendar' for s in r['payload']['sessions']})
    following = next((s for s in sessions if s > day), None)
    if not following or following[:7] == day[:7] or not any(
            r['type'] == 'market.session.closed' and r['payload']['period_end'] == close for r in rows):
        raise ValueError('recorded stream does not prove the requested month-end close and following session')
    return end


def live_guard(path):
    # Refuse inaccessible and stale sockets too: uncertainty is not an idle instance (ADR 0019).
    try:
        path.lstat()
    except FileNotFoundError:
        return
    raise ValueError('live socket exists; stop the instance and its adapter before rehearsing')


def replay(instance, close, source):
    if instance not in ('trad-ira', 'roth-ira'):
        raise ValueError('instance must be trad-ira or roth-ira')
    service_guard(instance)
    live_guard(Path('/run/ti-engine-' + instance + '/engine.sock'))
    live = Path(os.environ.get('TI_IRA_LIVE_SOCKET', '/run/ti-engine-' + instance + '/engine.sock'))
    live_guard(live)
    config = Path(os.environ.get('TI_IRA_CONFIG', '/etc/trend-investing/engines/' + instance + '.config'))
    cfg = json.loads(config.read_text())
    if cfg.get('mode') != 'dry-run' or cfg.get('strategy_id') != 'momentum':
        raise ValueError('only a momentum dry-run configuration may be replayed')
    # mkdtemp makes new private folders, never an existing run's folders (ADR 0027).
    evidence = Path(tempfile.mkdtemp(prefix='ira-replay-', dir='/tmp'))
    print('Evidence: ' + str(evidence), flush=True)
    shutil.copyfile(source, evidence / 'recorded.jsonl')
    shutil.copyfile(config, evidence / 'config.json')
    log, recorded_rows, recorded_decisions = recording(evidence / 'recorded.jsonl')
    rows = [row for row, _ in recorded_rows]
    check_close(rows, close)
    if not rows or rows[0]['type'] != CONFIGURATION or sum(r['type'] == CONFIGURATION for r in rows) != 1:
        raise ValueError('recorded run must start with exactly one configuration')
    if any(r['payload'].get('account_id', cfg['account_id']) != cfg['account_id'] for r in rows):
        raise ValueError('recorded inputs name a different logical account')
    run = evidence / 'run'
    run.mkdir()
    approval = approval_history(Path(source), recorded_rows, run)
    sockpath = evidence / 'engine.sock'
    engine = os.environ.get('TI_ENGINE_BIN', '/opt/trend-investing/bin/ti-engine')
    service_guard(instance)
    live_guard(live)
    replay_log = run / 'replay.log'
    suffix = 0
    while replay_log.exists():
        suffix += 1
        replay_log = run / ('replay-' + str(suffix) + '.log')
    command = [engine, '-mode', 'dry-run', '-config', str(evidence / 'config.json'),
               '-socket', str(sockpath), '-log', str(replay_log),
               '-out', str(evidence / 'replay.journal'), '-as-of', rows[0]['event_time']]
    if approval:
        command.extend(['-approval', str(approval)])
    with (evidence / 'engine-output.txt').open('w') as output:
        process = subprocess.Popen(command, stdout=output, stderr=output)
        try:
            deadline = time.monotonic() + 15
            while not sockpath.exists():
                if process.poll() is not None or time.monotonic() > deadline:
                    raise ValueError('engine did not listen; inspect engine-output.txt in evidence')
                time.sleep(0.05)
            current, startup, _ = recording(replay_log)
            check_identity(log.header, current.header)
            count = 2 if approval else 1
            if len(startup) != count:
                raise ValueError('engine startup inputs differ from recorded configuration/approval')
            for (original, _), (started, _) in zip(recorded_rows[:count], startup):
                difference = first_difference(original['payload'], started['payload'], original['type'])
                if difference:
                    raise ValueError('startup first difference: ' + redact(difference, cfg['account_id']))
            sequence = startup[-1][0]['sequence'] + 1
            with socket.socket(socket.AF_UNIX) as conn:
                conn.settimeout(30)
                conn.connect(str(sockpath))
                with conn.makefile('r') as replies:
                    # No timestamp filter: a broker reading after close may arrive before the
                    # close-stamped snapshot; every recorded input matters (ADR 0026 account-snapshot amendment).
                    for row, payload in recorded_rows[count:]:
                        conn.sendall((rewrite(row, payload, sequence, current.header) + '\n').encode())
                        frame = json.loads(replies.readline() or '{}')
                        reply = frame.get('envelope', {})
                        if reply.get('type') != 'engine.decisions':
                            raise ValueError(redact('engine refused recorded input ' + row['id'] + ': ' +
                                                    json.dumps(frame), cfg['account_id']))
                        sequence = reply['sequence'] + 1
        finally:
            finish(process)
    _, _, decisions = recording(replay_log)
    if any(d['type'] == 'risk.order.released' and d['payload']['send'] is not False for d in decisions):
        raise ValueError('replay released a sendable order')
    selected = month_decisions(decisions, close)
    p = selected[0]['payload']
    print(redact(close + ' strategy.targets.decided ' + p['outcome'] +
                 ((' ' + p['withheld_reason']) if p.get('withheld_reason') else ''), cfg['account_id']), flush=True)
    compare_error = None
    try:
        compare_month(recorded_decisions, decisions, close)
    except ValueError as error:
        compare_error = redact(str(error), cfg['account_id'])
    refusals = [d['payload']['reason'] for d in decisions if d['type'] == 'risk.order.refused']
    for reason in refusals:
        print(redact('risk.order.refused ' + reason, cfg['account_id']), flush=True)
    if compare_error:
        raise ValueError(compare_error)
    if p['outcome'] != 'decided' or p.get('withheld_picks') or refusals:
        raise ValueError('withheld decision, picks or refused orders: gate remains pending; inspect evidence')
    service_guard(instance)
    print('Replay gate: PASS (owner must still sign the real-month half)', flush=True)


def main():
    if len(sys.argv) != 4:
        sys.exit('usage: ira-replay.sh trad-ira|roth-ira YYYY-MM-DDTHH:MM:SSZ RECORDED_STREAM')
    try:
        replay(*sys.argv[1:])
    except (ValueError, OSError, KeyError) as error:
        # Only our own errors are safe to print: input paths may contain identifying text (lesson L12).
        if isinstance(error, ValueError) and not isinstance(error, json.JSONDecodeError):
            sys.exit('ira-replay: ' + redact(str(error)))
        sys.exit('ira-replay: cannot read or replay inputs; inspect the private evidence copy')


if __name__ == '__main__':
    main()
