# Owner's manual

Plain-language chapters for the owner, written for someone who did not write
the code. Each says what a tool does, why it exists, how to run it, what it
prints, and what happens when something goes wrong.

- [journal-query](journal-query.md): ask a run's journals what happened, such
  as which orders were planned, released or refused, which fills happened and
  what they cost, and whether the positions add up.
- [states-and-kill-switch](states-and-kill-switch.md): what Degraded, Halted
  and the kill switch do to the target-weight accounts' orders, the margin
  account's one sale back to 98%, the margin-cushion alarm, and what can go
  wrong.
- [alerts-and-dashboard](alerts-and-dashboard.md): the reporter that sends
  alerts and a daily summary to your phone and PagerDuty, and the read-only
  dashboard; what each alert means and what counts as delivered.
- [redelivery](redelivery.md): how an order the engine decided reaches IBKR
  exactly once, even when the engine or the adapter stops at the wrong
  moment.
- [order-lifecycle](order-lifecycle.md): reading an order's state from the
  log (committed, released, acknowledged), and the alert for an order IBKR
  never acknowledged.
- [momentum-minimum-lot](momentum-minimum-lot.md): how the IRAs buy whole
  shares only: a stock one share of which costs more than its target is
  skipped for the next-ranked one, what happens when nothing is affordable,
  and what the program records.
