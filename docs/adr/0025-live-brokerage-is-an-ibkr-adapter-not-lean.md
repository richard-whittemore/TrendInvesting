# ADR 0025: Live brokerage goes through our own IBKR adapter, not LEAN

- Status: Accepted (2026-10-15, by the owner)
- Date: 2026-10-14

## Context

`docs/architecture.md` and AGENTS.md give LEAN market data and brokerage,
with a thin Python adapter joining it to the Go application. That was
written for backtests and assumed LEAN could also trade live.

It cannot, at this project's cost limit:
- LEAN's Interactive Brokers connector (`Lean.Brokerages.InteractiveBrokers`,
  Apache-2.0) calls `ValidateSubscription()`, which checks for a paid
  QuantConnect licence, and shuts down without one, even on our own server.
- QuantConnect live trading costs about $1,100 a year or more (Researcher
  plan plus a live node). On-premises live trading needs the Institution
  plan. Both are large against the strategies' edge at the owner's account
  sizes (`docs/live-trading/plan.md`, section 8).

The owner chose (decision D4, 2026-10-14) to keep decisions in the Go
application and replace LEAN for live trading only.

## Decision

1. **Live market data, orders, fills, positions and cash go through a new
   Python broker adapter** that talks to IB Gateway over IBKR's TWS API.
   It contains no strategy logic. It turns IBKR events into envelopes and
   decision envelopes into IBKR orders.
2. **The adapter speaks the existing transport**: a Unix-domain socket
   with newline-delimited JSON, one exchange at a time, and the same
   `internal/event.Envelope` (ADR 0014, ADR 0015). The Go side cannot tell
   which adapter it is talking to.
3. **LEAN stays for backtests** and research. The LEAN adapter is
   unchanged.
   - **Only one adapter can drive a live engine.** A live engine's
     configuration names its adapter (`ibkr`), and that name is part of the
     configuration hash (ADR 0016). The adapter states its name in its first
     envelope; the engine refuses any other, and refuses a second connection
     while one is open. LEAN is not installed on the live server, and the
     live socket path is not one the LEAN container mounts.
4. **The client library is IBKR's own TWS API Python client (`ibapi`)**,
   pinned to one released version and its published checksum, vendored
   from IBKR's distribution because the PyPI package lags it. It has no
   runtime dependencies of its own.
   - Considered: `ib_async` (the maintained successor of `ib_insync`). It
     is easier to use, but adds third-party code and transitive
     dependencies between our orders and the broker. The dependency policy
     prefers the smaller surface for code that moves money.
5. **The broker is authoritative for what happened** (architecture.md:
   Go never assumes an intended order filled). The adapter reports:
   - every order acknowledgement, rejection, cancellation and fill
     (ADR 0022);
   - on every start and reconnect, before any order is accepted, the
     account's open orders, the session's executions, positions and cash.
     Differences are classified as ADR 0019 (as amended) sets out: an
     unexplained difference the evidence bounds puts the account in
     **Degraded** (no new risk; risk-reducing orders continue), and only
     unverifiable evidence or an unbounded difference puts it in
     **Halted**. What counts as risk-reducing for a target-weight strategy
     is defined in that strategy's own ADR.
   - **How far back the executions reach.** Through IB Gateway, the TWS
     API's `reqExecutions` returns only executions since midnight, and
     Gateway cannot extend that (IBKR's TWS API documentation, "Executions
     and Commissions"). So:
     - if the journal's last reconciled state is from the same day, the
       session's executions are complete evidence;
     - if it is older (an outage across midnight), the missing executions
       come from IBKR's Flex Web Service trade-confirmation report, a
       durable record of the account's activity, before the account may
       leave Halted. Until that report covers the gap, the evidence is
       `unverifiable` and the account is Halted;
     - the scheduled nightly Gateway restart is set after the day's
       reconciliation and outside trading hours, so a normal restart never
       crosses an open order.
   - The coverage is tested against the pinned `ibapi` and Gateway
     versions in the paper account, and a version change repeats that
     test.
6. **IB Gateway runs headless under IBC** on the Lightsail server, with a
   nightly restart and a weekly second-factor approval on the owner's
   phone. Credentials live only in IBC's configuration on that server.
   They are never in this repository, the journal or the logs.

## Consequences

- AGENTS.md's summary line and `docs/architecture.md`'s boundary diagram
  are amended, now that this ADR is accepted, to read "LEAN owns market data
  for backtests; the IBKR adapter owns live market data and brokerage."
  architecture.md's list of deferred decisions also drops brokerage. Those
  are the only changes to either file this ADR makes.
- The new adapter gets its own test suite in `make check` beside
  `make adapter-test`:
  - unit tests against recorded TWS API callbacks;
  - an end-to-end test against `cmd/engine` like the LEAN adapter's;
  - a paper-account test that is run by hand, not in CI, because it needs
    IBKR's servers.
- Live strategies (momentum, Boost, the bond glide path) are new
  target-weight modules beside the Turtle reducer, not changes to it
  (AGENTS.md rule 4). They need their own ADR before code: the
  target-weight decision contract and its risk limits.
- The adapter is a new place a defect can lose money. Its own limits
  (maximum orders per run, a price band per order, no order without a
  journalled decision) are checked again in Go, so neither side alone can
  send a bad order.
- If QuantConnect's licensing or prices change, this decision can be
  revisited; nothing in the Go application depends on which adapter runs.
