# Trend Investing

A stock-first research and trading platform for one owner's IBKR
accounts. It backtests declared strategies, and it runs the two that
survived the research as automated engines on one small server, where
every decision is written down before anything is sent. Today Boost runs
there on IBKR paper trading, and the Traditional IRA's Momentum runs in dry
run. It is not a claim that any
strategy will be profitable.

## What it trades

| Account | Rule | Where it stands |
| --- | --- | --- |
| Joint taxable account, on margin | **Boost**: SPY held at 98%, raised to 147% after a sharp drop in an uptrend, and held there until SPY closes back above its 5-day average | On IBKR paper trading since 2026-11-20, after a dry run on the server from late October (on the paper account from 2026-11-02). Its first paper order is expected around 2026-11-23. No live order goes out before the limited-live gate |
| Traditional IRA | **Momentum**: each month, 25 large US stocks that rose most over the past year, with a bond sleeve that grows with the owner's age, in fractional shares (plan decision D7-F); buys spend the month's sale proceeds at once, settled or not (plan decision D5), and keep 1% of the account in cash so No Borrow Margin is never crossed | In dry run on the server, under its own Gateway (`ti-gateway@trad-ira`) and its own adapter (`ti-adapter@trad-ira`). No IRA order is sent before its own paper gate. The Roth IRA is on hold (owner decision of 2026-11-03): its configuration is kept, and nothing runs for it |

The research lines that produced them are still here and still run as
backtests: a source-verified **Turtle** control adapted to stocks, a
rules-based **Sublime** control, predeclared **hybrid** experiments, and
the momentum and boost edge searches under `research/`. The two controls
stay separate configurations and are never blended (a non-negotiable in
`AGENTS.md`).

The owner's manual, `docs/manual/README.md`, says in plain language what
each piece does, which decision backs it, and what can go wrong. Its table
"Built today and not built yet" is the one list of what is finished.

## How it is built

- **Go** owns strategy rules, portfolio risk, state transitions, deterministic
  replay and the durable log (`internal/`, pure; `cmd/` is composition only).
- **LEAN** owns market data for backtests. The **IBKR adapter**
  (`adapter/ibkr/`, Python) owns live market data and brokerage through IB
  Gateway (ADR 0025); a data job fetches daily prices from Massive.
- Every consequential input and output is an ordered, versioned event. Each
  engine streams its inputs to a hash-chained durable log before answering,
  and a crash resumes from that log (ADR 0027). Projections are rebuilt
  from it; there is no database.
- One **engine** per account decides; a separate **adapter** carries orders
  to IBKR and answers back; a **reporter** and a **dashboard** say what
  happened; an **operator** command records the owner's approvals and
  clearances. All of it runs on one Lightsail server under systemd
  (`deploy/`), with a rehearsal container that replays a first day and a
  month, which the owner runs before shipping a change.
- **IB Gateway** runs twice on the server: `ti-gateway` for the joint login
  and `ti-gateway@trad-ira` for the IRA login. A non-secret selector (`joint`
  or `live`) names the login the first one uses. Each Gateway has its own
  encrypted login file and sees only that login. If another login displaces
  one, it takes its IBKR session back and pages once. Each listens on the
  server's loopback only, behind a port guard (`ti-gateway-guard.service`).
  Each waits out IBKR's nightly reset and scheduled maintenance, and a
  **healer** restarts it once when IBKR's connection never returns after a
  scheduled window.
- **Alerts** go to a private issue tracker and a phone. Capital-safety
  alerts reach the phone at once; operational ones (a unit down, the
  Gateway logged out) reach it only when a session is near, and are handled
  without the owner otherwise.

`docs/architecture.md` draws the boundaries, `docs/adr/` records every
decision, `docs/live-trading/plan.md` is the owner's plan and its gates, and
`docs/runbooks/` are the step-by-step procedures (for example the first
dry-run day, first paper order, missed rebalance, Gateway login and halted
account).

## What runs on the server

The server is an AWS Lightsail Ubuntu host, reached only over Tailscale. It
runs one engine per account, each in its own mode. It runs eight Go programs
(`ti-engine`, `ti-reporter`, `ti-dashboard`, `operator`, `journal-query`,
`ti-gateway-start`, `ti-gateway-heal` and `universe-rank`), the Python IBKR
adapters, the `ti-data@` market-data jobs, the `ti-flex@` Flex reader and the
ntfy server. Alerts go to Pushover (urgent pages), a Healthchecks heartbeat,
the project's own ntfy (the daily summary) and issues in a private alerts
repository. When something goes wrong, start with `docs/runbooks/halted-account.md`.

## Quick start

Requirements: Go 1.27.4 (the exact version in `.go-version`; `GOTOOLCHAIN=local`
means no other version is fetched automatically), Python 3 for the
adapters, Docker for the rehearsal.

```sh
make check          # format, vet, staticcheck, golangci-lint, race tests, coverage, govulncheck,
                    # go mod tidy -diff and go mod verify, build, adapter suites, research-test, deploy-test
deploy/rehearse.sh  # the server's first day and an IRA month, in a container
```

`docs/running-a-backtest.md` runs a fixture end to end and verifies the
journal it writes. `docs/development.md` holds the engineering rules,
including the source-comment standard and strict test-first development.
`CONTRIBUTING.md` covers the toolchain. CI runs `make check` on every pull
request.

## Declared Variants

The declared wider-cap Variants are
[`total-long-cap-24`](cmd/backtest/testdata/variants/total-long-cap-24/README.md) and
[`total-long-cap-36`](cmd/backtest/testdata/variants/total-long-cap-36/README.md).
They change only the total-long cap; the Baseline stays at 12 Units (ADR 0008).
Every run's [research report](docs/running-a-backtest.md#regime-reports-and-held-out-evaluations)
carries peak sector concentration and the count of independent Campaigns.

## Safety

This software is under active development. It must not be used to place
live orders until the paper-trading and limited-live readiness gates in the
plan are explicitly satisfied. Nothing in this repository holds an account
id, a credential, or real market data: fixtures are synthetic, and the
server's secrets live only on the server.
