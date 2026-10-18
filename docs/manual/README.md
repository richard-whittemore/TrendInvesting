# Owner's manual

Plain-language chapters for the owner, written for someone who did not write
the code. Each says what a tool does, why it exists, how to run it, what it
prints, and what happens when something goes wrong.

- [journal-query](journal-query.md): ask a run's journals what happened, such
  as which orders were planned, released or refused, which fills happened and
  what they cost, and whether the positions add up.
- [operator](operator.md): the operator tool: how you engage or release the
  kill switch, approve a fresh run, and clear a stuck rebalance, what each
  records, and how to see it in the log.
- [states-and-kill-switch](states-and-kill-switch.md): what Degraded, Halted
  and the kill switch do to the target-weight accounts' orders, the margin
  account's one sale back to 98%, the margin-cushion alarm, and what can go
  wrong.
- [reconciliation](reconciliation.md): the morning check of IBKR's positions
  and cash against the engine's book, what counts as explained, what puts an
  account in Degraded or Halted, and what the IBKR adapter sends for it.
- [alerts-and-dashboard](alerts-and-dashboard.md): the reporter that sends
  alerts and a daily summary to your phone and PagerDuty, and the read-only
  dashboard; what each alert means and what counts as delivered.
- [redelivery](redelivery.md): how an order the engine decided reaches IBKR
  exactly once, even when the engine or the adapter stops at the wrong
  moment.
- [order-lifecycle](order-lifecycle.md): reading an order's state from the
  log (committed, released, acknowledged), and the alert for an order IBKR
  never acknowledged.
- [market-data](market-data.md): fetching, recording and encoding the
  market data the engines read, including the compact universe stream and
  cancelled sessions.
- [momentum-universe](momentum-universe.md): how the IRAs' momentum engine
  sees the market: one universe snapshot a session, price histories for the
  500 it ranks, how a stock with no sector label is treated, and what
  happens when the exchange cancels a session.
- [momentum-minimum-lot](momentum-minimum-lot.md): how the IRAs buy whole
  shares only: a stock one share of which costs more than its target is
  skipped for the next-ranked one, what happens when nothing is affordable,
  and what the program records.
- [parity](parity.md): the check that the Go momentum and Boost code makes
  the same decisions as the QuantConnect backtests from the same data, its
  thresholds, and how to read its pass/fail report.
