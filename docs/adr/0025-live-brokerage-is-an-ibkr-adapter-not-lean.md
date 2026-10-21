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
   `internal/event.Envelope` (ADR 0014, ADR 0015). Strategy, risk and
   journal code do not depend on which adapter is connected; only the
   engine's identity check (point 3), still to be built, will look at it.
3. **LEAN stays for backtests** and research. The LEAN adapter is
   unchanged.
   - **Only one adapter can drive a live engine.** This is new work for
     the live engine, not something the engine does today: at present
     `cmd/engine` accepts any envelope with a non-empty source. A live engine's
     configuration names its adapter (`ibkr`), and that name is part of the
     configuration hash (ADR 0016). The adapter states its name in its first
     envelope; the live engine will refuse any other, and a second
     connection while one is open, with tests before the adapter is used. LEAN is not installed on the live server, and the
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

## Amendment (2026-10-16): `ibapi` is fetched and verified, not vendored

Point 4 said `ibapi` is "vendored from IBKR's distribution". That is
withdrawn. This repository is public, and IBKR's TWS API is distributed under
IBKR's own licence terms, which may not allow publishing its source here.

*Owner decision 2026-10-16 (#327, option b):*

- **What will be committed (not yet implemented):** one pinned `ibapi` version
  and the SHA-256 of IBKR's distribution archive for it. No IBKR source is
  committed.
- **How it will be installed (not yet implemented):** a fetch-and-verify step
  downloads IBKR's archive and refuses to install it unless its SHA-256 equals the committed one. It
  runs on the live server and on the paper-test machine only.
- **CI** keeps running against the fake TWS client
  (`adapter/ibkr/tests/fake_tws.py`), so it needs no `ibapi`.
- **Unchanged:** the pin to a single released version, IBKR's own client
  rather than PyPI's lagging package or `ib_async`, and repeating the
  paper-account coverage test on a version change.
- **The version, checksum and installation step are not chosen or added
  here.** They are set, from IBKR's official download page, in #327's next PR;
  until then neither `deploy/versions.env` nor `deploy/provision.sh` carries
  `ibapi`.

## Amendment (2026-10-18): the `ibapi` pin

The version and checksum the amendment of 2026-10-16 left open are set in
`deploy/versions.env` (`TWSAPI_VERSION`, `TWSAPI_URL`, `TWSAPI_SHA256`):

- **Version:** IBKR's TWS API 10.51.01, its "Latest" Linux build. IBKR's
  download page says only the Latest Mac/Linux build includes the Python
  API; "Stable" (10.50.02) has Java and C++ only. So the pin is Latest, as
  the one released build that carries `ibapi`.
- **Checksum:** IBKR publishes none. The SHA-256 is of the installer as
  downloaded on 2026-10-18, as for the Gateway installer. IBKR's link is a
  moving one, so when IBKR replaces the file provisioning stops, which is
  intended (point 5: a version change repeats the paper-account test).
- **Fetch and verify:** `deploy/provision.sh twsapi`, a step of its own that
  `all` does not run, downloads the installer and refuses it unless its
  SHA-256 matches; kept apart so that IBKR moving its "latest" link never
  blocks provisioning the Gateway. Installing `ibapi` from it into
  the adapter's environment is written with, and first run by, the
  paper-account test, since that is the first use of the real client.
  CI still runs against the fake.
- The Gateway is pinned at 10.50.1f (stable) and the API at 10.51.01: the API
  client negotiates the server version on connect, and the paper-account
  test confirms the pair works before any account sends.

## Amendment (2026-10-18, owner decision): the adapter's stamping duty after the close

To point 5. The owner decided on 2026-10-18 that the adapter's one account
reading after a session's close is the account as of that close when all of
ADR 0019's conditions (amended 2026-10-18) hold:

- it is taken after the close and before the next open, on the close's own
  New York date (`reqExecutions` reaches back only to midnight, so only then
  does IBKR's list cover everything since the close);
- no order is working or pending at IBKR for the account, from any client,
  in either account read or the last open-order list, and no execution is in
  the account at or since the close, both read from IBKR's own lists as the
  reconciliation at a start reads them;
- every holding has an official close in the batch, so the account is valued
  at the close, and IBKR's own net liquidation at the read is positive;
- the holdings are the previous close's book adjusted by the journalled fills
  since, share for share, and the total cash that book's adjusted by those
  fills within 1% of the net liquidation at the close. The previous close's
  book is the latest account snapshot of the run or of the runs its Run
  approvals chain back to, or the latest starting book an approval states
  (an account's first run's included; ADR 0027, amended 2026-10-18), so a
  fresh run's first delivery is compared too;
- the engine holds no snapshot already stamped at or after that close.

The adapter's duty is to check those conditions at every delivery and stamp
accordingly: from the one reading it sends the engine's `account.snapshot`
and the `account.broker.snapshot` stamped as of the close, the broker
snapshot carrying the true read time, IBKR's own net liquidation, the day's
cash drift and the unexplained cash beside it, and the
`market.mark.recorded` for each instrument the session delivers a bar for,
from the delivered bar's official close, with IBKR's last price beside it.
Nothing is sized from IBKR's price, so no decision needs IBKR market-data
permissions, but the price band compares a checked order's reference price
with it when IBKR gives one (ADR 0026, point 3, amended 2026-10-18). When a
condition fails it stamps the true read time and states why, so the engine
withholds (ADR 0019, amended 2026-10-18).

With an account snapshot now produced, the adapter also sends the
reconciliation record this point calls for, in this flow:

- every delivery carries a reconciliation from its own reading. Once the
  engine holds a book it goes ahead of the delivery's snapshots, so the
  engine classifies the reading against that book before the new snapshot
  replaces it. In a run's first delivery, before which the engine holds no
  book, it goes behind them when the reading was found to be the previous
  close's book; when it was not (or there was none to compare with) it
  goes alone, the snapshots withheld, and the engine classifies it
  unverifiable and halts;
- after every start and reconnect, once the engine holds a book, a
  reconciliation is read afresh once every execution IBKR replayed has its
  commission and its fill is recorded, and sent before any stream input; a
  read whose positions or cash IBKR does not give is tried at most three
  times in a row, then the adapter stops for a person.

A difference is classified as ADR 0019 sets out.
