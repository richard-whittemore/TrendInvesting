# Trend Investing

A stock-first research and trading platform for one owner's three IBKR
accounts. It backtests declared strategies, and it runs the two that
survived the research as automated engines on one small server, where every
decision is written down before anything is sent. It is not a claim that any
strategy will be profitable.

## What it trades

| Account | Rule | Where it stands |
| --- | --- | --- |
| Joint taxable account, on margin | **Boost**: SPY held at 98%, raised to 147% for a few days after a sharp drop in an uptrend | In dry run on the server since late October 2026; the paper gate is three cleanly decided sessions in a row (owner decision of 2026-10-24) |
| Traditional IRA and Roth IRA | **Momentum**: each month, 25 large US stocks that rose most over the past year, with a bond sleeve that grows with the owner's age, in fractional shares (plan decision D7-F) | The Traditional IRA engine decides month-ends in dry run; no IRA order is sent before its own paper gate |

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
  month before any change ships.

`docs/architecture.md` draws the boundaries, `docs/adr/` records every
decision, `docs/live-trading/plan.md` is the owner's plan and its gates, and
`docs/runbooks/` are the step-by-step procedures (first dry-run day, first
paper order, missed rebalance, Gateway login, halted account).

## Quick start

Requirements: Go 1.27.3 or newer (see `.go-version`; `GOTOOLCHAIN=local`
means no other version is fetched automatically), Python 3 for the
adapters, Docker for the rehearsal.

```sh
make check          # format, vet, staticcheck, race tests, coverage, govulncheck, build, adapter suites
deploy/rehearse.sh  # the server's first day and an IRA month, in a container
```

`docs/running-a-backtest.md` runs a fixture end to end and verifies the
journal it writes. `docs/development.md` holds the engineering rules,
including the source-comment standard and strict test-first development.
`CONTRIBUTING.md` covers the toolchain.

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
