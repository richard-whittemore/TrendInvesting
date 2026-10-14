# Live trading plan (draft for review, 2026-10-14)

Status: **proposed**. Nothing here is built. Owner decisions are marked
**Decision** and are listed together at the end.

This plan moves the strategies the research kept from QuantConnect
backtests into automated trading at Interactive Brokers (IBKR). This system
will trade real money: the AGENTS.md non-negotiables apply in full,
especially rule 5 (no live orders before the paper-trading and limited-live
gates).

## 1. What runs where

| Account (IBKR) | Size at start | Strategy | Source of the rule |
| --- | --- | --- | --- |
| Roth IRA (Roth 401(k) rollover) | ~$80k | Momentum + bond sleeve on a glide path | `research/edge-search/momentum/defensive_v2.py` as configured by `F_COMBO` in `research/edge-search/build_variants.py`; bond sleeve: `docs/research/retirement-bonds-2026-10.md` (PR #291, not yet merged) |
| Traditional IRA (401(k) rollover) | ~$30k | Same as the Roth IRA | Same |
| Joint taxable, margin | ~$4k | Boost on SPY | `research/edge-search/boost/template.py`; execution study `BE_IB_OPEN` |

Out of scope: the M1 accounts (stay manual at M1), the custodial account
(VOO held at M1), the wife's accounts, and any strategy not listed.

## 2. The rules, as backtested

The live rules must be the backtested rules. Each is restated from its
source file, and a parity test (section 6) checks the live code against the
backtest's own decisions.

**Momentum, combination** (chosen, D1). `defensive_v2.py` as configured by
the `F_COMBO` variant in `build_variants.py` (`mom_trend`, TOP_N 25,
RISK_OFF_FRACTION 0.5, SECTOR_CAP 3, INV_VOL; the file's own defaults
differ). Provenance: 12-1 momentum, Jegadeesh & Titman 1993; the trend
filter, motivated by Daniel & Moskowitz 2016; the half fraction, sector cap
and inverse volatility are this project's research settings, chosen on
1999–2015 and checked once on 2016–mid-2026
(`docs/research/boost-followup-2026-09.md`). Recorded runs: F_COMBO_R4
24bdb0a9, OOS_F_COMBO_R4 dd45f8ec.
- On the first session of each month, before the open, using closes up to
  the last session of the previous month.
- Universe: the 500 largest US common stocks by dollar volume, price at
  least $5, with fundamental data.
- Rank by 12-1 momentum (return from 12 months ago to 1 month ago).
- Take the top 25, at most 3 per Morningstar sector, weighted by inverse
  volatility; 98% invested in total.
- If SPY closed below its 200-day average, half of the book goes to IEF
  instead.
- A held name is resized only when it is more than 25% off its target.
- The half filter (`F_HALF`) is the same without the sector cap and
  inverse volatility. It is not to be built.

**Bond sleeve and glide path**: a share *b* of each IRA, set from a
schedule by year, held in a bond fund; the rest, 1 − *b*, is the momentum
sleeve. This is exactly what the research measured (two curves weighted and
reset monthly):
- The momentum sleeve runs the rule above on its own 1 − *b* of the
  account, keeping its own 98% invested and its own risk-off IEF. Risk-off
  IEF counts toward the momentum sleeve, not toward *b*.
- The bond sleeve holds 98% of *b* in its fund.
- Both sleeves are reset to 1 − *b* and *b* at the month's rebalance.
- When both hold the same fund, the account holds one position, the sum of
  the two targets; the journal records each sleeve's share.
**Decision D2**: the fund (IEF, SHY or half each; #292 pending) and the
schedule. Illustration: 0–10% until 2028, rising evenly to 40–50% by 2053.
The schedule is configuration with its own hash (ADR 0016), so a change is
recorded, never silent.

**Boost** (`boost/template.py`, SPY, BOOST_SIZE 1.5, RSI_ENTRY 10,
TREND_DAYS 200, EXIT_SMA 5):
- Hold SPY at 98%. When SPY's 2-day RSI is below 10 and its close is above
  its 200-day average, go to 147%; return to 98% once a close is above the
  5-day average.
- Decide after the close, trade at the next open: the execution study kept
  the whole edge this way (`BE_IB_OPEN` 10.19% a year against 10.31% for
  market-on-close), and the decision uses final closes, not a provisional
  15:44 price.
- **Decision D3**: margin Boost or Boost through SSO (no borrowing).

## 3. Architecture

**Today's architecture** (docs/architecture.md): LEAN owns market data and
brokerage, a thin Python adapter talks to the Go application, which owns
decisions, the journal and replay. LEAN cannot be used live without a paid
QuantConnect license (its IBKR connector checks one), so the brokerage side
changes. That is an architecture change and needs an ADR before code.

**Proposed** (ADR to follow on approval):

```text
Lightsail VM (Ubuntu, $12/month, US East)
  IB Gateway, kept logged in by IBC (nightly restart, weekly 2FA on the phone)
  broker adapter (Python, ib_async): market data, orders, fills, positions
     -> same envelope transport as today (Unix socket, NDJSON; ADR 0014)
  decision core (Go): momentum, Boost, glide path, risk checks, journal
  reporter: daily summary, equity against SPY, alerts
  dashboard (read-only, reachable only over Tailscale)
```

- **Go keeps the decisions**, so the existing envelope, journal,
  tamper-evidence and replay machinery (ADRs 0014–0019, 0022) carry over.
  The Turtle reducer does not: these strategies are target-weight
  rebalancers, so they are new strategy modules beside it, not changes to
  it (rule 4).
- **The broker adapter replaces LEAN for live trading only.** LEAN stays for
  backtests. It has no strategy logic: it turns envelopes into IBKR orders
  and IBKR events into envelopes.
- **Reconciliation is an input** (ADR 0019): every morning the adapter
  reports IBKR's positions and cash; an unexplained difference halts that
  account until a person clears it.
- **Alternative considered**: one standalone Python program. Faster to
  write, but it would duplicate the journal and replay guarantees the
  repository already tests. The owner chose the Go core (D4).

## 4. Data

- **Prices**: IBKR historical daily bars (adjusted for splits and
  dividends) for SPY, IEF, SHY and the universe. About 520 symbols a month
  at IBKR's pacing limits takes well under an hour, done overnight.
- **Universe** (the hard part): IBKR gives no ready "500 largest by dollar
  volume with fundamentals" list. Proposed: take the holdings file of a
  broad US index fund (iShares IWB, Russell 1000, published daily and free),
  drop non-common shares, rank by dollar volume from IBKR bars, keep 500.
- **Sectors**: the backtest used Morningstar sectors; the holdings file
  gives GICS sectors. They differ for some companies, which changes which
  names the sector cap allows.
- **Universe parity study first** (research, before any build): for each
  month of 2016–2026, compare the 25 names this approach would pick with
  the backtest's own picks, and the return difference.
  - It needs each month's holdings and sectors as published at the time. A
    current holdings file would leave out companies that have since gone
    and include others before they joined, which flatters the match.
  - Obtaining dated monthly holdings files (iShares publishes holdings by
    as-of date) is therefore a prerequisite. If they reach back only part
    of the way, the study covers only those months and says so. If the overlap is
  poor, the fallback is a paid data source (cost to be priced against the
  edge, `cost-constraint`).

## 5. Safety

- **Fail closed** on anything unexpected: stale data, missing bars,
  reconciliation differences, an order rejected or partly filled, a
  restarted gateway mid-rebalance.
- **Hard limits outside the strategy code**: IRA exposure at most 100% and
  never on margin; joint account at most 150%; at most N orders per run; no
  order larger than a set share of the account; a price-sanity band per
  order.
- **No order sent twice**: a stable ID derived from the decision is only a
  label. What prevents a resend is that:
  - every order is written to the journal before it is sent, and its
    acknowledgement and fills are written as they arrive (ADR 0022);
  - the decision's tag is kept separate from IBKR's own order ID, as the
    existing contract does;
  - on every start and every reconnect, IBKR's open orders, executions of
    the session and positions are read and matched to the journal before
    anything is sent. An order IBKR has but the journal lacks (a crash after
    IBKR accepted it), or the other way round, halts the account (ADR
    0019).
- **Sell before buy** in the IRAs (no margin to cover the gap).
- **Kill switch**: a file or command that stops all new orders.
- **Modes**: dry-run (decide and journal, send nothing) → IBKR paper →
  live, set per account in configuration.

## 6. Testing and gates

1. **Unit tests first** (rule 1) for every rule, with golden cases taken
   from the backtest's own runs.
2. **Backtest parity**: feed the Go strategy the same daily data the
   research used for a stretch of months and require the same targets as
   the QuantConnect run, month by month (momentum) and day by day (Boost).
3. **Dry-run on live data**: 1 month for Boost, 2 rebalances for momentum.
4. **IBKR paper trading**: 2–3 months, all three accounts, with daily
   reconciliation and the dashboard. Gate: no unexplained difference, no
   missed rebalance, fills close to the paper model.
5. **Limited live**: the $4k joint account first, then the Traditional IRA,
   then the Roth IRA, one at a time, each after a clean month.

## 7. Operations

- Daily: data refresh after the close, decisions, a summary to the phone
  (ntfy), heartbeat to Healthchecks.io; an alert if the heartbeat is missed.
- Weekly: approve IBC's 2FA prompt on the phone (Sunday).
- Monthly: momentum rebalance on the first session; report against SPY.
- Logs and the journal are kept on the VM and copied off it nightly; a
  weekly Lightsail snapshot.
- Runbooks for a halted account, a missed rebalance, a gateway that will
  not log in, and a margin warning.

## 8. Costs

| Item | Per year |
| --- | --- |
| Lightsail 2 GB plan, snapshots | ~$170 |
| Tailscale, Healthchecks.io, ntfy (free tiers) | $0 |
| IBKR data for US stocks on IBKR Pro, if required for API bars | about $10/month unless waived by commissions; to confirm |
| Commissions | Lite: $0. Pro: ~$28 for Boost, and for momentum's ~25 names a month perhaps $100–200 per IRA |

**Decision D5**: the owner chose IBKR Lite for every account, if IBKR
allows it; to confirm that Lite accounts get the API data the system needs. Momentum's monthly trades
make Pro's $1 minimum matter for the smaller IRA, and the IRAs never borrow,
so Pro's margin rate does not help them.

## 9. Phases

| Phase | Work | Gate to move on |
| --- | --- | --- |
| 0 | Owner decisions D2–D3, D5 confirmed with IBKR; ADR for the broker adapter; this plan reviewed | Plan merged |
| 1 | Universe parity study (research, QuantConnect free) | Overlap good enough, or a data source chosen |
| 2 | Go strategy modules and risk limits, test first; backtest parity | Parity passes |
| 3 | Broker adapter, journal wiring, reporter, dashboard; dry-run on Lightsail | A clean dry-run month for Boost and 2 clean dry-run rebalances for momentum |
| 4 | IBKR paper trading | 2–3 clean months |
| 5 | Limited live, one account at a time | A clean month each |

Each phase is a ticket with its own work log, worked test-first and merged
only after CodeRabbit and Greptile findings are addressed.

## 10. Owner decisions

- **D1** Momentum version: **combination** (decided 2026-10-14).
- **D2** Glide path: bond fund (after #292) and the schedule by year, for
  each of the two of you.
- **D3** Joint account: margin Boost or SSO Boost.
- **D4** Architecture: **Go decision core with a Python broker adapter**
  (decided 2026-10-14).
- **D5** **IBKR Lite for every account**, if IBKR allows it and its API
  data suffices (decided 2026-10-14; to confirm with IBKR).
- Account opening, funding, rollovers and the Lightsail sign-up are the
  owner's to do; nothing here signs up for or pays for anything.
