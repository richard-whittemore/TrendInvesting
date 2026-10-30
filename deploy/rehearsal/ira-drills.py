"""The missed-rebalance runbook's isolated fake-venue drill (ADR 0026 points 2/5).

Run as root in the provisioned rehearsal container, or on the server with
--interactive for the owner to perform the printed clearance. The only venue
is this process: it never starts an adapter, imports ibapi or connects to IBKR.
All prices, positions and logical account names come from the synthetic month.
Evidence remains in /tmp. The installed engine and operator are used unchanged.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import pwd
import shlex
import socket
import subprocess
import tempfile
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
# ADR 0025 binds paper mode to the DU prefix: a synthetic word, never a broker number.
ACCOUNT = 'DUSYNTHETIC'


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def shifted(value, days):
    if isinstance(value, dict):
        return {k: shifted(v, days) for k, v in value.items()}
    if isinstance(value, list):
        return [shifted(v, days) for v in value]
    if isinstance(value, str):
        try:
            day = dt.date.fromisoformat(value[:10])
            return (day + dt.timedelta(days=days)).isoformat() + value[10:]
        except ValueError:
            pass
    return value


HELD, HELD_QUANTITY = 'SHY', 10


def month(path, days=0):
    rows = []
    held_close = None  # the last SHY close seen: bars precede the session's snapshot
    for raw in path.read_text().splitlines():
        row = shifted(json.loads(raw), days)
        row['id'] += ':drill:' + str(days)
        if days and row['type'] != 'market.session.calendar' and row['event_time'][:10] < '2025-02-01':
            continue
        if row['type'] == 'market.bar.completed':
            row['payload']['adjusted_base'] = '2025-01-02'
            if row['payload'].get('instrument_id') == HELD:
                held_close = row['payload']['raw']['close']
        if row['type'] == 'account.snapshot':
            row['payload']['account_id'] = ACCOUNT
            # An extra holding the rule never picks (an ETF, not a common stock, and not the trend or
            # risk-off instrument) forces a sell; it has bars every session, so the decision is not
            # withheld for missing bars. No fills occur in this fake.
            row['payload']['positions'] = [{'instrument_id': HELD, 'quantity': HELD_QUANTITY}]
            row['payload']['net_liquidation'] = round(10000 + HELD_QUANTITY * (held_close or 0), 2)
        rows.append(row)
    return rows


class FakeVenue:
    def __init__(self, folder, engine, config, asof, approval):
        self.folder = folder
        self.folder.mkdir(mode=0o750, exist_ok=True)
        trader, operator = pwd.getpwnam('ti-trader'), pwd.getpwnam('ti-operator')
        os.chown(folder, trader.pw_uid, trader.pw_gid)
        self.inbox = folder / 'operator'
        self.inbox.mkdir(mode=0o750, exist_ok=True)
        os.chown(self.inbox, operator.pw_uid, trader.pw_gid)
        # The socket lives in its own 0700 directory, as the service's runtime directory does:
        # the transport refuses any other mode, and the operator inbox needs the group to traverse.
        self.sockdir = folder / 'sock'
        self.sockdir.mkdir(mode=0o700, exist_ok=True)
        os.chmod(self.sockdir, 0o700)
        os.chown(self.sockdir, trader.pw_uid, trader.pw_gid)
        self.log = folder / ('fresh.log' if (folder / 'run.log').exists() else 'run.log')
        self.output = (folder / (self.log.stem + '-output.txt')).open('w')
        self.process = subprocess.Popen([engine, '-mode', 'paper', '-config', str(config), '-as-of', asof,
                                         '-socket', str(self.sockdir / 'engine.sock'), '-log', str(self.log),
                                         '-out', str(self.log.with_suffix('.journal')), '-operator', str(self.inbox),
                                         '-approval', str(approval)], stdout=self.output, stderr=self.output,
                                        user=trader.pw_uid, group=trader.pw_gid, extra_groups=[trader.pw_gid])
        self.conn = None
        try:
            deadline = time.monotonic() + 15
            while not (self.sockdir / 'engine.sock').exists():
                if self.process.poll() is not None or time.monotonic() >= deadline:
                    self.output.flush()
                    # The engine's own words say why (the evidence folder is private to the container).
                    require(False, 'engine did not listen; inspect ' + str(folder) + '\n' +
                            (folder / (self.log.stem + '-output.txt')).read_text()[-1500:])
                time.sleep(0.05)
            self.header = json.loads(self.log.read_text().splitlines()[0])
            self.conn = socket.socket(socket.AF_UNIX)
            self.conn.settimeout(20)
            self.conn.connect(str(self.sockdir / 'engine.sock'))
            self.replies = self.conn.makefile('r')
            self.sequence = 3
            self.decisions = []
        except BaseException:
            self.stop()
            raise

    def send(self, row):
        raw = json.dumps(row['payload'], separators=(',', ':'))
        env = dict(row, envelope_version=1, source='fake-venue', recorded_at=row['event_time'],
                   sequence=self.sequence, configuration_hash=self.header['configuration_hash'],
                   strategy_version=self.header['strategy_version'], payload_hash=hashlib.sha256(raw.encode()).hexdigest())
        # Compact, as hashed: the engine rejects a payload whose bytes differ from its hash (ADR 0016).
        self.conn.sendall((json.dumps(env, separators=(',', ':')) + '\n').encode())
        frame = json.loads(self.replies.readline() or '{}')
        require(frame.get('envelope', {}).get('type') == 'engine.decisions', 'fake input refused: ' + str(frame))
        reply = frame['envelope']
        self.sequence = reply['sequence'] + 1
        decisions = reply['payload'].get('decisions') or []
        self.decisions.extend(decisions)
        return decisions

    def feed(self, rows):
        for row in rows:
            self.send(row)

    def cancel_all(self, at):
        # The local fake acknowledges cancellation, without executing or changing the book.
        pending = [d['payload'] for d in self.decisions if d['type'] == 'risk.order.released']
        seen = set()
        while pending:
            order = pending.pop(0)
            ident = order['client_order_id']
            if ident in seen:
                continue
            seen.add(ident)
            payload = {k: order[k] for k in ('account_id', 'client_order_id', 'instrument_id', 'side', 'quantity', 'limit_price')}
            payload.update(order_id='fake-' + ident, status='canceled', occurred_at=at)
            out = self.send(dict(type='execution.order.lifecycle', schema_version=2, id='cancel:' + ident,
                                 event_time=at, payload=payload))
            pending.extend(d['payload'] for d in out if d['type'] == 'risk.order.released')

    def stop(self):
        if self.conn:
            self.conn.close()
        if self.process.poll() is None:
            self.process.terminate()
        try:
            status = self.process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait()
            raise RuntimeError('engine did not stop cleanly')
        self.output.close()
        require(status == 0, 'engine failed; inspect ' + str(self.folder))


def operate(binary, user, *args):
    identity = pwd.getpwnam(user)
    return subprocess.check_output([binary, *map(str, args)], text=True, user=identity.pw_uid,
                                   group=identity.pw_gid, extra_groups=[pwd.getpwnam('ti-trader').pw_gid])


def plans(venue, tag):
    return [{k: v for k, v in d['payload'].items() if k not in ('client_order_id', 'tag')}
            for d in venue.decisions if d['type'] == 'strategy.order.planned' and d['payload']['tag'] == tag]


def drill(interactive=False):
    require(os.geteuid() == 0, 'run with sudo: separate engine and operator users are required')
    engine = os.environ.get('TI_DRILL_ENGINE', '/opt/trend-investing/bin/ti-engine')
    operator = os.environ.get('TI_DRILL_OPERATOR', '/opt/trend-investing/bin/operator')
    fixture = Path(os.environ.get('TI_DRILL_MONTH', str(ROOT / 'cmd/engine/testdata/momentum-month.jsonl')))
    config_source = Path(os.environ.get('TI_DRILL_CONFIG', str(ROOT / 'cmd/engine/testdata/momentum-dry-run.json')))
    base = Path(tempfile.mkdtemp(prefix='ira-drill-', dir='/tmp'))
    trader = pwd.getpwnam('ti-trader')
    os.chown(base, trader.pw_uid, trader.pw_gid)
    os.chmod(base, 0o750)
    print('Evidence: ' + str(base), flush=True)
    cfg = json.loads(config_source.read_text())
    cfg['mode'], cfg['account_id'] = 'paper', ACCOUNT
    config = base / 'config.json'
    config.write_text(json.dumps(cfg))
    config.chmod(0o640)
    os.chown(config, trader.pw_uid, trader.pw_gid)
    jan, feb = month(fixture), month(fixture, 28)
    tag = 'targets-decided:' + ACCOUNT + ':2025-01-31T21:00:00Z'
    febtag = 'targets-decided:' + ACCOUNT + ':2025-02-28T21:00:00Z'
    folder = base / 'restart'
    folder.mkdir(mode=0o750)
    os.chown(folder, trader.pw_uid, trader.pw_gid)
    first = base / 'first.json'
    operate(operator, 'ti-trader', 'approve', '-first-run', '-strategy', 'momentum', '-account', ACCOUNT,
            '-by', 'owner', '-positions', HELD + '=' + str(HELD_QUANTITY), '-cash', '10000', '-currency', 'USD',
            '-at', '2025-01-02T13:00:00Z', '-out', first)
    venue = FakeVenue(folder, engine, config, '2025-01-02T21:00:00Z', first)
    try:
        venue.feed(jan)
        released = [d['payload'] for d in venue.decisions if d['type'] == 'risk.order.released']
        decided = [d['payload'] for d in venue.decisions if d['type'] == 'strategy.targets.decided']
        require(released and all(o['side'] == 'sell' for o in released),
                'expected only the sell phase before stop; decisions: ' +
                str([(p.get('session_period_end'), p.get('outcome'), p.get('withheld_reason'), p.get('withheld_detail'))
                     for p in decided]) + '; released sides: ' + str([o['side'] + ' ' + o['instrument_id'] for o in released]) +
                '; other decision types: ' + str(sorted({d['type'] for d in venue.decisions})))
    finally:
        venue.stop()
    print('STOPPED: sells working; buys not released', flush=True)
    fresh = base / 'approval.json'
    text = operate(operator, 'ti-trader', 'approve', '-log', venue.log, '-account', ACCOUNT,
                   '-by', 'owner', '-out', fresh, '-at', '2025-01-31T22:00:00Z')
    require('ends mid-rebalance' in text and 'sells-working' in text, 'approval did not carry sells in progress')
    print(text, end='', flush=True)
    venue = FakeVenue(folder, engine, config, '2025-01-31T21:00:00Z', fresh)
    try:
        venue.feed(jan)
        targets = [d['payload'] for d in venue.decisions if d['type'] == 'strategy.targets.decided']
        require(len(targets) == 1 and targets[0]['withheld_reason'] == 'rebalance-in-progress', 'restart did not hold')
        require(not any(d['type'] == 'risk.order.released' for d in venue.decisions), 'hold released an order')
        print('HELD: rebalance-in-progress; no orders released', flush=True)
        command = [operator, 'rebalance-cleared', '-inbox', str(venue.inbox), '-account', ACCOUNT,
                   '-decision', tag, '-orders', ','.join(sorted(o['client_order_id'] for o in released)),
                   '-by', 'owner', '-detail', 'fake venue confirms cancelled; no broker connection',
                   '-at', '2025-01-31T22:01:00Z']
        if interactive:
            print('The fake confirms every sell cancelled, holdings ' + HELD + '=' + str(HELD_QUANTITY) + ' and cash=10000 unchanged.\n'
                  'In a second owner shell run exactly:\nsudo -u ti-operator ' + shlex.join(command), flush=True)
            input('After recording the clearance, press Enter to resume the fake feed: ')
        else:
            operate(operator, 'ti-operator', *command[1:])
        venue.feed(feb)
        records = [json.loads(s).get('record', {}) for s in venue.log.read_text().splitlines()]
        require(any(r.get('envelope', {}).get('type') == 'operator.rebalance.cleared' for r in records), 'clearance not recorded')
        require(not list((folder / 'operator-notes').glob('*.refused')), 'clearance refused')
        print('CLEARED: operator.rebalance.cleared recorded', flush=True)
        require(not plans(venue, tag), 'abandoned January buys were released')
        recovered = [d['payload'] for d in venue.decisions if d['type'] == 'strategy.targets.decided'][-1]
        require(recovered['outcome'] == 'decided', 'next month did not decide')
        venue.cancel_all('2025-03-03T14:30:00Z')
    finally:
        venue.stop()
    end = operate(operator, 'ti-trader', 'approve', '-log', venue.log, '-account', ACCOUNT,
                  '-by', 'owner', '-out', base / 'end.json')
    require('no rebalance in progress' in end, 'fake did not reach a terminal end state')
    control = FakeVenue(base / 'control', engine, config, '2025-02-28T21:00:00Z', first)
    try:
        control.feed(jan + feb)
        ontime = [d['payload'] for d in control.decisions if d['type'] == 'strategy.targets.decided'][-1]
        require(recovered['targets'] == ontime['targets'], 'targets differ from on-time control')
        require(plans(venue, febtag) and plans(venue, febtag) == plans(control, febtag), 'orders differ from on-time control')
        control.cancel_all('2025-03-03T14:30:00Z')
    finally:
        control.stop()
    control_end = operate(operator, 'ti-trader', 'approve', '-log', control.log, '-account', ACCOUNT,
                          '-by', 'owner', '-out', base / 'control-end.json')
    require('no rebalance in progress' in control_end, 'control did not reach a terminal end state')
    print('RECOVERED: next month equals on-time targets and orders; no old buys\n'
          'FAKE ONLY: no adapter or broker connection', flush=True)
    return base


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--interactive', action='store_true')
    drill(parser.parse_args().interactive)
