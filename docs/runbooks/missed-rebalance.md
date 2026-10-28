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
