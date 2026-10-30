"""Private server replay for plan.md section 9 (owner decision 2026-10-28).

The input is a recorded engine log (including the account snapshots and marks),
or a JSONL input stream. The evidence copy is retained even on failure. No
adapter is started; every released order must have send=false (ADR 0026).
"""
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from zoneinfo import ZoneInfo


def inputs(path):
    for raw in path.read_text().splitlines():
        item = json.loads(raw)
        if 'record' in item:
            item = item['record']
            if item['kind'] != 'input':
                continue
            item = item['envelope']
        if 'type' not in item:  # Durable-log header/trailer.
            continue
        if item['type'] in ('strategy.target-weight.configuration', 'operator.run.approved'):
            continue
        if item['type'].startswith('operator.'):
            raise ValueError('operator instructions require a separate recovery drill')
        yield item


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
    rows = list(inputs(evidence / 'recorded.jsonl'))
    end = check_close(rows, close)
    rows = [r for r in rows if dt.datetime.fromisoformat(r['event_time'].replace('Z', '+00:00')) <= end]
    if any(r['payload'].get('account_id', cfg['account_id']) != cfg['account_id'] for r in rows):
        raise ValueError('recorded inputs name a different logical account')
    sockpath = evidence / 'engine.sock'
    engine = os.environ.get('TI_ENGINE_BIN', '/opt/trend-investing/bin/ti-engine')
    live_guard(live)
    with (evidence / 'engine-output.txt').open('w') as output:
        process = subprocess.Popen([engine, '-mode', 'dry-run', '-config', str(evidence / 'config.json'),
                                    '-socket', str(sockpath), '-log', str(evidence / 'replay.log'),
                                    '-out', str(evidence / 'replay.journal'), '-as-of', close], stdout=output, stderr=output)
        try:
            deadline = time.monotonic() + 15
            while not sockpath.exists():
                if process.poll() is not None or time.monotonic() > deadline:
                    raise ValueError('engine did not listen; inspect engine-output.txt in evidence')
                time.sleep(0.05)
            header = json.loads((evidence / 'replay.log').read_text().splitlines()[0])
            sequence, decisions, refusals = 2, [], []
            with socket.socket(socket.AF_UNIX) as conn:
                conn.settimeout(30)
                conn.connect(str(sockpath))
                with conn.makefile('r') as replies:
                    for row in rows:
                        raw = json.dumps(row['payload'], separators=(',', ':'))
                        envelope = dict(row, envelope_version=1, sequence=sequence, source='ira-replay',
                                        strategy_version=header['strategy_version'], configuration_hash=header['configuration_hash'],
                                        recorded_at=row['event_time'], payload_hash=hashlib.sha256(raw.encode()).hexdigest())
                        envelope.pop('correlation_id', None)
                        conn.sendall((json.dumps(envelope) + '\n').encode())
                        reply = json.loads(replies.readline() or '{}').get('envelope', {})
                        if reply.get('type') != 'engine.decisions':
                            raise ValueError('engine refused recorded input; inspect evidence log')
                        sequence = reply['sequence'] + 1
                        for decision in reply['payload'].get('decisions') or []:
                            if decision['type'] == 'risk.order.released' and decision['payload']['send'] is not False:
                                raise ValueError('replay released a sendable order')
                            if decision['type'] == 'risk.order.refused':
                                refusals.append(decision['payload']['reason'])
                            if decision['type'] == 'strategy.targets.decided':
                                decisions.append(decision['payload'])
            selected = [p for p in decisions if p['session_period_end'] == close]
            if len(selected) != 1:
                raise ValueError('requested close did not produce exactly one decision')
            p = selected[0]
            print(close + ' strategy.targets.decided ' + p['outcome'] +
                  ((' ' + p['withheld_reason']) if p.get('withheld_reason') else ''), flush=True)
            for reason in refusals:
                print('risk.order.refused ' + reason, flush=True)
            if p['outcome'] != 'decided' or p.get('withheld_picks') or refusals:
                raise ValueError('withheld decision, picks or refused orders: gate remains pending; inspect evidence')
            print('Replay gate: PASS (owner must still sign the real-month half)', flush=True)
        finally:
            # Only the child this command started (docs/development.md, Ending processes).
            if process.poll() is None:
                process.terminate()
            try:
                status = process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                raise ValueError('engine did not stop cleanly; inspect evidence')
            if status:
                raise ValueError('engine exited unsuccessfully; inspect evidence')


def main():
    if len(sys.argv) != 4:
        sys.exit('usage: ira-replay.sh trad-ira|roth-ira YYYY-MM-DDTHH:MM:SSZ RECORDED_STREAM')
    try:
        replay(*sys.argv[1:])
    except (ValueError, OSError, KeyError) as error:
        # Only our own errors are safe to print: input paths may contain identifying text (lesson L12).
        if isinstance(error, ValueError) and not isinstance(error, json.JSONDecodeError):
            sys.exit('ira-replay: ' + str(error))
        sys.exit('ira-replay: cannot read or replay inputs; inspect the private evidence copy')


if __name__ == '__main__':
    main()
