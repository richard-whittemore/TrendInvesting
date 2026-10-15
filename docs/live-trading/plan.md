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
| Roth IRA (Roth 401(k) rollover) | ~$80k | Momentum + bond sleeve on a glide path | `research/edge-search/momentum/defensive_v2.py` as configured by `F_COMBO` in `research/edge-search/build_variants.py`; bond sleeve: `docs/research/retirement-bonds-2026-10.md` (PRs #291, #295) |
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
**Decision D2**: the fund is **half IEF, half SHY** (decided 2026-10-14;
tested in `docs/research/retirement-bonds-2026-10.md`). The schedule
(decided 2026-10-15) sets *b* by the owner's age, since both IRAs are in
his name. From age 53 it is a straight-line version of Vanguard's
target-date glide path through its published anchor points (90% stocks
until 25 years before retirement, 50% at retirement, 30% seven years later;
Vanguard, "Target-date fund glide path", workplace.vanguard.com). Vanguard
does not publish a straight line between those points, and its funds' own
mix at a given age differs a little (for example 40% conventional bonds at
60, against 42% here) and includes short-term TIPS later on; this schedule
does not copy those details. Before 53 it starts lower, at 0%, and rises
evenly to meet the straight-line path there, as the owner asked:

| Owner's age | *b* (bonds) | Momentum, 1 − *b* |
| --- | --- | --- |
| 38 to 53 | rises evenly, 0% to 30.8% (about 2.05 points a year) | 100% to 69.2% |
| 53 to 65 | Vanguard anchors: rises evenly, 30.8% to 50% (1.6 points a year) | 69.2% to 50% |
| 65 to 72 | Vanguard anchors: rises evenly, 50% to 70% (about 2.9 points a year) | 50% to 30% |
| 72 and over | 70% | 30% |

At every age it holds no more bonds than the straight-line path through
Vanguard's anchors, which is 10% until 40 and 30.8% at 53.

- Age is counted in whole months on the rebalance date, and *b* is
  interpolated linearly within each stretch, so it changes a little each
  month rather than in steps.
- The first stretch starts from age 38, not from the first live
  rebalance, so trading that starts later starts above 0%.
- Retirement is taken as age 65. The birth date is part of the account
  configuration on the server, not of this repository.
- Not tested as a path: the research measured fixed mixes (100/0, 90/10,
  70/30, 50/50), and the schedule moves between them. Mixes beyond 50%
  bonds, reached after 65, were not measured. So the schedule gets its own
  backtest before any IRA trades live: the blend of the momentum and
  half-IEF/SHY curves, reset monthly with *b* from this schedule, run on
  2002–2015 and on 2016–September 2026. Each window is a run of 10–14
  years and the path spans 34, so each window is run three times, starting
  the path at age 38, 53 and 65 on its first date, and compared with the
  fixed mixes. It is a gate in section 6 and
  Phase 2, and its result is recorded whether or not it flatters the path.

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
- **Decision D3**: **margin Boost** (decided 2026-10-14), not SSO. The
  joint account borrows for the step to 147%; the 150% hard limit
  (section 5) applies.

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
  broker adapter (Python, IBKR's TWS API client; ADR 0025): market data, orders, fills, positions
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
- **Where dated holdings come from** (checked 2026-10-14):
  - iShares' site now offers only three holdings dates (the latest, the
    last month-end and the last year-end), and its old dated-download link
    returns the fund page, not a file. It serves live trading, not the
    study.
  - QuantConnect's US ETF Constituents dataset includes IWB: daily from
    January 2015, monthly from June 2009 to 2014. It is free for universe
    selection in QuantConnect Cloud, so the study runs there, comparing a
    universe built from IWB's holdings on each date with the backtest's own
    selection. Both use QuantConnect's Morningstar sectors. Downloading the
    files for local use costs money; the study does not need that.
  - QuantConnect says the dataset may lag the sponsor's files by up to a
    week, and documents no separate availability date. So each month's
    universe uses the latest holdings dated at least 7 calendar days before
    the rebalance, never a later one. That snapshot is older than the
    previous day's iShares file live trading reads; the study reports how
    much the result moves with a 1-day lag instead.
  - SEC EDGAR has IWB's own holdings every quarter (series S000004347):
    N-PORT (CUSIP, shares, value) from September 2019, N-Q and N-CSR
    schedules from 2006. It is a free, independent check of the
    QuantConnect data at a few quarter-ends.
  - None of these settles the sector difference: live trading gets GICS
    sectors from the iShares file, the backtest used Morningstar. The study
    measures it separately, comparing the two labels for today's holdings
    and rerunning the cap with GICS where they differ.
  - It needs each month's holdings and sectors as published at the time. A
    current holdings file would leave out companies that have since gone
    and include others before they joined, which flatters the match.
  - The study runs from January 2016 to September 2026, the last full
    month before it was planned (2026-10-14); QuantConnect's dataset covers
    that span, so it needs no purchased data. Later months are not added.
    If the overlap is poor, the fallback is a paid data source (cost to be
    priced against the edge, `cost-constraint`).

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
   The glide path, untested as a path, has its own backtest (section 2)
   before any IRA trades live.
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
| 0 | Owner decisions: D5 confirmed with IBKR; ADR for the broker adapter; this plan reviewed | Plan merged and the broker-adapter ADR accepted. D5 must be settled before Phase 2 starts, since the strategy modules and limits depend on them |
| 1 | Universe parity study (research, QuantConnect free) | Overlap good enough, or a data source chosen |
| 2 | Go strategy modules and risk limits, test first; backtest parity; the glide-path backtest (section 2) | Parity passes, and the glide-path backtest is recorded |
| 3 | Broker adapter, journal wiring, reporter, dashboard; dry-run on Lightsail | A clean dry-run month for Boost and 2 clean dry-run rebalances for momentum |
| 4 | IBKR paper trading | 2–3 clean months |
| 5 | Limited live, one account at a time | A clean month each |

Each phase is a ticket with its own work log, worked test-first and merged
only after CodeRabbit and Greptile findings are addressed.

## 10. Owner decisions

- **D1** Momentum version: **combination** (decided 2026-10-14).
- **D2** Glide path: **half IEF, half SHY** (decided 2026-10-14), on a
  schedule by the owner's age: 0% at 38 rising to meet Vanguard's glide
  path at 53, then Vanguard's (decided 2026-10-15; section 2).
- **D3** Joint account: **margin Boost** (decided 2026-10-14).
- **D4** Architecture: **Go decision core with a Python broker adapter**
  (decided 2026-10-14).
- **D5** **IBKR Lite for every account**, if IBKR allows it and its API
  data suffices (decided 2026-10-14; to confirm with IBKR).
- Account opening, funding, rollovers and the Lightsail sign-up are the
  owner's to do; nothing here signs up for or pays for anything.
