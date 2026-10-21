# Runbook: a margin warning on the joint account

Boost borrows to hold 147% of the joint account in SPY while its boost is
on (its target, 98% x 1.5); the separate hard limit no buy may exceed is
150% ([boost](../manual/boost.md)). The
margin-cushion alarm watches IBKR's Excess Liquidity as a share of net
liquidation from every broker snapshot (plan.md section 5; ADR 0026,
point 6). Its thresholds are in the account's hashed configuration
(`excess_liquidity_warning`, `excess_liquidity_urgent`,
`excess_liquidity_stale_minutes`). Each change of level is recorded as a
`risk.margin.alarm` decision and alerted: "margin cushion warning" and
"margin cushion clear" as routine messages, "margin cushion urgent" as an
urgent one, each with the reading and the thresholds
([alerts-and-dashboard](../manual/alerts-and-dashboard.md); on the server the
reporter checks every 5 minutes, around the clock, seven days a week).

| Level | When | What the program does | What you do |
| --- | --- | --- | --- |
| **warning** | below the warning threshold (40%) | records and alerts; orders as usual | check the position the same day (below) |
| **urgent** | below the urgent threshold (20%), a net liquidation of zero or less, or no reading for the stale period while the market is open | records, alerts, and sells back to 98% once per urgent alarm (ADR 0019, amended 2026-10-15; ADR 0026, point 4) | below |
| **clear** | back above the warning threshold | records and alerts the clear | nothing |

## A warning

1. **Look at the account at IBKR**: Excess Liquidity, the SPY holding, any
   open order, and IBKR's house margin requirement for SPY (it can rise
   without notice).
2. **Compare with the program's view**: the alarm's reading and cause are in
   its record in the durable log (`grep '"risk.margin.alarm"' <log>`), and
   `journal-query positions <log>` gives the holding the journal holds.
3. **Decide.** At 147% under the standard 25% maintenance requirement the
   ratio is about 63%, so a warning means either a deep fall in SPY or a
   higher house requirement. If the requirement rose, the boost is riskier
   than the backtest assumed: you may engage the kill switch to stop the
   next boost buy (`sudo -u ti-operator operator kill-switch ... -state engaged`),
   which still lets the sale back to 98% go. Selling by
   hand is your call; if you do, stop the engine and the adapter first
   (`sudo systemctl stop ti-engine@boost ti-adapter-boost`), as
   the manual's "Selling by hand safely" says
   (`docs/manual/states-and-kill-switch.md`): the kill switch does not stop
   the sale back to 98%, so the program could sell beside you.

## An urgent alarm

1. **The program sells back to 98%** from the latest IBKR snapshot, once for
   this alarm. Watch for the alert that it was sent ("sale back to 98%
   sent"), one for each fill ("filled N of M", which says how many are left),
   or the urgent one that IBKR canceled or refused it ("sale back to 98%
   failed": sell the rest by hand). `journal-query orders` and
   `journal-query fills` show the same.
2. **If the sale was withheld or refused** (`sale-withheld` or `refused`
   in `journal-query refusals`):
   - `broker-values-unreadable` or `unknown-working-sell`: the program will
     not try again for this alarm, and the urgent "sale withheld, sell by
     hand" alert says so. **Act by hand at IBKR now**: sell back to 98% of
     net liquidation, following the manual's "Selling by hand safely" (stop
     the engine and the adapter, cancel and confirm nothing is working in
     IBKR's app, then sell).
   - `stale-broker-snapshot` or `non-positive-broker-values`: the program
     decides it again from the next good snapshot while the alarm is urgent.
     The first stale one of an outage alerts (once per stale episode); a
     non-positive one pages "sale withheld, sell by hand" once per sale,
     with the hand-sale steps, since nothing sells until a positive
     snapshot comes.
     If the snapshots stay bad (the gateway is down: `gateway-login.md`), act
     by hand, but stop the engine and the adapter first ("Selling by hand
     safely"): the attempt is unspent, so the program can still sell from
     the next good snapshot, beside your sale.
   - **Refused at the gate as `broker-holding`** (the sale is larger than
     IBKR's holding): the refusal spends the attempt, and the urgent "sale
     back to 98% refused, sell by hand" page says so. Check the holding at
     IBKR and, if still above 98%, sell back to 98% by hand, following the
     steps the page gives ("Selling by hand safely").
3. **No reading** (the cause says `no-reading`): the adapter has had no
   Excess Liquidity for the stale period. Check the gateway first
   (`gateway-login.md`).
4. **After the sale**, the account stays Normal unless something else
   moved it; the boost may buy again when Boost's rule says so. If you want
   it not to, engage the kill switch or stop the
   engine.

## Day trades (legacy pattern-day-trader rule)

Until IBKR confirms which regime applies (plan.md section 5, D5), count day
trades by hand: a buy and a sell of SPY in the same session is one. Boost's
normal round trip is not a day trade; an emergency same-day sell after a
morning buy is. Keep to at most three in five sessions. **A risk-reducing
sale always goes, even as a fourth day trade**: the urgent sale back to
98% and a halt-driven sale are never held back for the count. A
pattern-day-trader restriction limits new buying, not closing sales, so its
cost is that Boost stops buying while it lasts. To count:
`journal-query fills -account <account> -from <five sessions ago>` and look
for a buy and a sell on one date. The program has no day-trade counter yet.

## Before live money

The joint account goes live only after this runbook has been rehearsed in
paper trading with an injected low Excess Liquidity reading, handled end to
end (plan.md section 9, phase 5).
