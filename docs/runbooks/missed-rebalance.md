# Runbook: a missed or stuck rebalance (momentum IRAs)

Each IRA rebalances once a month: at the close of the month's last session
the engine decides the month's targets, sends the sells at the next open and
the buys once every sell has finished (ADR 0026, points 1 and 2, "IRA
execution"). Every decision time records one `strategy.targets.decided`,
decided or withheld with a reason, so a month is never skipped silently
(ADR 0026, point 1). A "missed" rebalance is one of these:

| What `journal-query` shows | Reason | What it means |
| --- | --- | --- |
| withheld `missing-bars` | a held or targeted instrument had no bar in the decision session, or the decision session never closed | the market-data step did not deliver; nothing was ordered |
| withheld `stale-account-snapshot` | no account snapshot as of the close | the adapter did not deliver IBKR's account; nothing was ordered |
| withheld `missing-universe` | no universe snapshot for the session | the market-data step did not record the ranking; nothing was ordered |
| withheld `rebalance-in-progress` | an order of the previous rebalance is still not finished | the previous month is stuck; nothing more is sent until you clear it |
| decided, but orders refused | a hard limit, the kill switch, Degraded or Halted | `halted-account.md` or the kill switch |

Find it: `journal-query orders <durable log> -account <IRA>` and
`journal-query refusals <durable log> -account <IRA>`; the decision's
detail names the instrument, session or order.

## A withheld month (missing bars, snapshot or universe)

Nothing was sent, so there is nothing to undo. Fix the feed (the market-data
step's log, or the gateway: `gateway-login.md`). The rule decides again at the
next month's decision time; there is no catch-up rebalance mid-month (ADR
0026, point 1: one decision per decision time). If you want the month's
targets now, trade by hand at IBKR. The next decision then starts from the
holdings IBKR reports.

## A stuck rebalance (rebalance-in-progress)

1. **Find the open orders**: in `journal-query orders`, the orders of the
   previous decision (its tag is `targets-decided:<account>:<close time>`)
   with no final report (filled, cancelled or rejected).
2. **Deal with each at IBKR**: cancel it, or confirm IBKR shows it finished
   or gone. Wait until IBKR shows none of them working.
3. **Clear the rebalance**, naming the decision and every open order, in
   ascending order or not (the tool sorts them):

   ```
   sudo -u ti-operator operator rebalance-cleared -inbox <inbox> -account <IRA> \
     -decision targets-decided:<account>:<close time> \
     -orders <client order id>,<client order id> -by owner -detail "cancelled at IBKR"
   ```

4. **Check it took**: `journal-query operator <log>` shows the clearance; if
   the engine refused it, a `<file>.refused` note in the engine's notes folder (`operator-notes`, beside the inbox) says which
   orders are open. The rest of that rebalance (its buys, if they were still
   waiting) is never sent; the next decision time decides afresh from IBKR's
   holdings.

If the sells finished but the buys never went, the account holds cash until
the next decision time. Buy by hand if you want the targets sooner.

## A restart mid-rebalance

A crash is resumed from the durable log and carries on where it was (ADR
0027). A fresh run (you stopped the engine, a reboot, a new build) starts with
no record of the previous run's orders, so the engine holds it instead: a
restart mid-rebalance sends nothing for that decision time until you clear it
(ADR 0026, point 5).

1. Write the approval with `operator approve -log <the latest log>`. If that
   log ends mid-rebalance, the approval states the rebalance, its phase and
   its orders not finished, and the tool prints them and the clearance to
   record ([operator](../manual/operator.md), "An approval"). The engine
   refuses an approval that states none, or another rebalance, so the fresh
   run cannot start as if nothing were working.
2. Start the fresh run with that approval. Every month it decides is
   withheld as `rebalance-in-progress`, nothing planned, while the rebalance
   stands.
3. At IBKR, cancel each order still working, or confirm it finished, then
   record the clearance the tool printed (`operator rebalance-cleared
   -decision <decision> -orders <id,...>`), naming exactly those orders. The
   next decision time decides afresh from IBKR's holdings.

Better still, when you can choose the moment, stop an IRA's engine only once
every order of the rebalance has finished: then the approval states none in
progress and nothing is held.

## Rehearsing a month

The [Phase 3 gate](../live-trading/plan.md#9-phases) is **one real
month-end decided cleanly plus one rehearsed month replayed through the
server with `-as-of`** (2026-10-28 owner decision). Record both halves in
[the checklist](ira-drill-checklist.md); a withheld decision is a failed
attempt, never a quiet pass (lesson L11). Richard signs the gate.

Use the installed, stamped engine and the same dry-run configuration/build
as the recorded month. The recorded stream must include the calendar with
the following session, warm-up bars/histories, universe, account snapshots
and marks. Use the engine's **durable `.log`**, which has all these inputs;
the market-data job's stream alone lacks the account and marks. Operator
instructions require the separate recovery drill below. A `.journal`
without durable input records is not a replay source.

In the commands below, replace the example close with the recorded month's
actual NYSE close (UTC, including early closes) and `recorded.log` with its
path. Set `ira=roth-ira` for the Roth. Run in a shell with `set -euo pipefail`.
Stop the adapter first and the engine second, then freeze the input by
copying it. Leave their logs, inbox, notes and `run.env` intact:

```bash
ira=trad-ira
sudo systemctl stop "ti-adapter@$ira.service" "ti-data@$ira.timer"
sudo systemctl stop "ti-engine@$ira.service"
sudo -u ti-trader /usr/local/libexec/trend-investing/ira-replay.sh \
  "$ira" 2025-01-31T21:00:00Z "/var/lib/trend-investing/engines/$ira/recorded.log"
```

Expected lines (evidence suffix is unique for every attempt):

```text
Evidence: /tmp/ira-replay-<unique>
2025-01-31T21:00:00Z strategy.targets.decided decided
Replay gate: PASS (owner must still sign the real-month half)
```

The script passes `-as-of` equal to that close, replays earlier inputs as
warm-up, and waits synchronously for each engine reply. It refuses an
existing live socket, even a stale one, and any configuration outside
momentum dry-run. It starts no adapter and releases only `send=false` orders.
Every run gets a new private socket, log and journal under `/tmp`; it never
uses the original run's output folders or operator inbox. The byte-for-byte
input copy, configuration, durable replay log, journal and engine output
stay there on success or failure. Preserve the folder in private durable
storage before `/tmp` is cleared. No command prints an IBKR identifier.

A withheld attempt instead prints, for example:

```text
2025-01-31T21:00:00Z strategy.targets.decided withheld insufficient-trend-history
```

It exits nonzero, retains evidence, and leaves the gate pending. A decided
month with withheld picks or refused orders also fails. Read the decision payload and the
original dry-run's decision: with identical inputs, configuration and build,
intended targets, planned orders and `send=false` releases must agree. This
replay never repairs a missed real month and does not prove fractional
adapter/reporting readiness (M6b). Return the production instance via a
**fresh approval** and the restart procedure in [durable-log.md](durable-log.md#restarting-an-engine),
then start its data timer and adapter; never reuse the old `run.env` after a
deliberate stop. Run the same-day IRA probe before leaving (lesson L10).

## Restart and missed-rebalance drill with a fake venue

A normal dry-run completes its sells and buys at the same close using
synthetic cancellations (ADR 0026 point 2); stopping it cannot leave sells
working. Use the isolated fake below to exercise the real paper-mode engine
and operator CLI **without an adapter or broker connection**. Do this before
any IRA paper order; keep the production IRA units stopped as above. Do not
change their mode or start a paper adapter. The real account remains No
Borrow Margin and needs fractional trading enabled before M6b/paper.

From the installed repository checkout containing the synthetic fixture,
run this exact command on the server. The driver requires root only to run
the engine as `ti-trader` and the operator as `ti-operator`, preserving the
inbox's security boundary. It creates entirely private state under `/tmp`.

```bash
sudo python3 deploy/rehearsal/ira-drills.py --interactive
```

Expected lines, in order:

```text
Evidence: /tmp/ira-drill-<unique>
STOPPED: sells working; buys not released
```

The driver uses a synthetic paper-only identity (the required DU prefix plus
a word, never a real account number). The fake feeds the synthetic January month with FFF held, stops its engine
with SIGTERM after sells are released and before it gives any sell terminal
report (the buys are still waiting), then runs:

```bash
sudo -u ti-trader /opt/trend-investing/bin/operator approve \
  -log <evidence>/restart/run.log -account <printed-synthetic-account> -by owner \
  -out <evidence>/approval.json -at 2025-01-31T22:00:00Z
```

Expect `ends mid-rebalance`, `sells-working` and the exact decision and open
client order IDs. It starts a **fresh** engine with `-approval` naming that
file (never `-resume` for a deliberate stop), replays January and checks:

```text
HELD: rebalance-in-progress; no orders released
```

The fake then confirms its sells cancelled, with FFF=10 and cash=10000
unchanged. In a second owner shell, run **the complete command printed by
the driver**, with its actual evidence path and exact order list. Its form is:

```bash
sudo -u ti-operator /opt/trend-investing/bin/operator rebalance-cleared \
  -inbox <evidence>/restart/operator -account <printed-synthetic-account> \
  -decision <printed-exact-decision> \
  -orders <printed-exact-order-list> -by owner \
  -detail 'fake venue confirms cancelled; no broker connection' \
  -at 2025-01-31T22:01:00Z
```

Only Richard records this server-drill clearance. Return to the first shell
and press Enter. The driver resumes the **fake feed**, checks the clearance
was journalled and has no `.refused` note, feeds February, and compares its
targets and orders against an on-time run over identical inputs. The fake
cancels every released order; nothing fills or changes holdings/cash. Expected:

```text
CLEARED: operator.rebalance.cleared recorded
RECOVERED: next month equals on-time targets and orders; no old buys
FAKE ONLY: no adapter or broker connection
```

Both runs must end with `operator approve` reporting `no rebalance in
progress`. Clearance abandons January's remaining buys; it does **not**
catch up January. February decides afresh. For a missed real month caused
by missing inputs, follow "A withheld month" above: fix the feed and wait
for the next decision time, or trade by hand after reconciling at IBKR.
Never backdate a production feed or insert synthetic facts into it.

The container runs the same driver without `--interactive`, against only
its fake, with these focused tests (no real account or market data):

```bash
TMPDIR=/tmp python3 -m unittest discover -s deploy/rehearsal -p 'test_ira*.py'
```

The replay test uses `ira-stream.py --month-fixture` to stamp the existing
synthetic month; the default `ira-stream.py` mode remains the non-decision
warm-up. Preserve the evidence, record dated PASS/FAIL rows, restore the
production instance by fresh approval, and perform the same-day probe.
Gateway restart, logged-out pages, withheld-rebalance pages and real
month-end observations remain Richard's server checks (lessons L4/L10/L11).
