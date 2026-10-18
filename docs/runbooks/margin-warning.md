# Runbook: a margin warning on the joint account

Boost borrows to hold up to 147% of the joint account in SPY. The
margin-cushion alarm watches IBKR's Excess Liquidity as a share of net
liquidation from every broker snapshot (plan.md section 5; ADR 0026,
point 6). Its thresholds are in the account's hashed configuration
(`excess_liquidity_warning`, `excess_liquidity_urgent`,
`excess_liquidity_stale_minutes`). Each change of level is recorded as a
`risk.margin.alarm` decision and alerted: "margin cushion warning" and
"margin cushion clear" as routine messages, "margin cushion urgent" as an
urgent one, each with the reading and the thresholds
([alerts-and-dashboard](../manual/alerts-and-dashboard.md); on the server the
reporter checks every 5 minutes on weekdays, 09:00 to 20:55 New York).

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
   next boost buy (`sudo -u ti-trader operator kill-switch ... -state engaged`), which still
   lets the sale back to 98% go. Selling by hand is your call; if you do,
   engage the kill switch first, so the program sends nothing new while your
   order works.

## An urgent alarm

1. **The program sells back to 98%** from the latest IBKR snapshot, once for
   this alarm. Watch for the alert that it was sent ("sale back to 98%
   sent"), one for each fill ("filled N of M", which says how many are left),
   or the urgent one that IBKR canceled or refused it ("sale back to 98%
   failed": sell the rest by hand). `journal-query orders` and
   `journal-query fills` show the same.
2. **If the sale was withheld** (`sale-withheld` in `journal-query
   refusals`):
   - `broker-values-unreadable` or `unknown-working-sell`: the program will
     not try again, and the urgent "sale withheld, sell by hand" alert says
     so. **Act by hand at IBKR now**: sell back to 98% of net liquidation,
     after checking no other sell is working.
   - `stale-broker-snapshot` or `non-positive-broker-values`: the program
     decides it again from the next good snapshot while the alarm is urgent.
     The first stale one of an outage alerts (once per stale episode); a
     non-positive one sends no alert, so look for it here.
     If the snapshots stay bad (the gateway is down: `gateway-login.md`), act
     by hand.
3. **No reading** (the cause says `no-reading`): the adapter has had no
   Excess Liquidity for the stale period. Check the gateway first
   (`gateway-login.md`).
4. **After the sale**, the account stays Normal unless something else
   moved it; the boost may buy again when Boost's rule says so. If you want
   it not to, engage the kill switch.

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
