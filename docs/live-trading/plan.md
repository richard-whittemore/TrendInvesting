# Live trading plan (reviewed 2026-10-15)

Status: **reviewed** by the owner and independently (2026-10-15). Nothing here is built. Owner decisions are marked
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
- **How clean that check is.** The combination was declared before its own
  held-out run, but after the full filter's held-out run had shown the
  whipsaw problem that the half fraction answers. So its held-out result is
  weaker evidence than a blind test. Any threshold for "the live strategy
  is not behaving as expected" is set from the tuning years' figures, not
  the held-out ones.
- On the first session of each month, before the open, using closes up to
  the last session of the previous month.
- Universe: the 500 largest US common stocks by one day's dollar volume
  (QuantConnect's coarse `DollarVolume`, `defensive_v2.py:174-176`), price
  at least $5, with fundamental data.
- Momentum is counted in sessions: the close 21 sessions back over the
  close 252 sessions back, minus one (`px.iloc[-22] / px.iloc[-253]`,
  `defensive_v2.py:289`). A stock with fewer than 253 closes is left out, so
  a listing under a year old cannot be picked (287-288).
- Take the top 25, at most 3 per Morningstar sector (a stock with no sector
  is not capped, `defensive_book`, 69-73), weighted by inverse volatility:
  the population standard deviation of the last 252 daily returns (291);
  98% invested in total.
- Risk-off: SPY's last close below the mean of its last 200 closes, that
  close included (280). Half of the book then goes to IEF.
- A held name is resized only when it is more than 25% off its target.
- The live module reproduces each of these exactly; the golden tests and
  the parity backtest (section 6) check them one by one.
- The half filter (`F_HALF`) is the same without the sector cap and
  inverse volatility. It is not to be built.

**Bond sleeve and glide path**: a share *b* of each IRA, set from a
schedule based on the owner's age, held in a bond fund; the rest, 1 − *b*, is the momentum
sleeve. This is exactly what the research measured (two curves weighted and
reset monthly):
- The momentum sleeve runs the rule above on its own 1 − *b* of the
  account, keeping its own 98% invested and its own risk-off IEF. Risk-off
  IEF counts toward the momentum sleeve, not toward *b*.
- The bond sleeve holds 98% of *b* in its fund.
- Both sleeves are reset to 1 − *b* and *b* at the month's rebalance.
  The 25% band applies to the account's holdings after the two sleeves'
  targets are summed: IEF, which both sleeves can hold, is one position
  with one target, resized only past the band like any other.
- When both hold the same fund, the account holds one position, the sum of
  the two targets; the journal records each sleeve's share.
**Decision D2**: the fund is **half IEF, half SHY** (decided 2026-10-14;
tested in `docs/research/retirement-bonds-2026-10.md`, whose held-out
figures for the split are not independent of the IEF and SHY runs they
blend). The schedule
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
  backtest before any IRA trades live. It runs as one QuantConnect account
  (the combination plus a bond sleeve, with the band as above), not as two
  curves blended outside it, on 2002–2015 and on 2016–September 2026.
  Each window is a run of 10–14 years and the path spans 34, so each
  window is run three times, starting the path at age 38, 53 and 65 on its
  first date. Each run is compared with two fixed mixes, run as the same
  one-account backtest on the same dates with the same band, worst drop
  measured on daily equity in all three. The two mixes bracket the bond
  shares the path passes through: the lower has *b* at the path's starting
  share rounded down to 10 points, the upper at its ending share rounded up
  to 10 points.

  | Window | Path starts at age | *b* over the run | Lower mix (bonds) | Upper mix (bonds) |
  | --- | --- | --- | --- | --- |
  | 2002–2015 (14 years) | 38 | 0% → 28.7% | 0% | 30% |
  | 2002–2015 | 53 | 30.8% → 55.7% | 30% | 60% |
  | 2002–2015 | 65 | 50% → 70% | 50% | 70% |
  | 2016–Sep 2026 (10.75 years) | 38 | 0% → 22.1% | 0% | 30% |
  | 2016–Sep 2026 | 53 | 30.8% → 48.0% | 30% | 50% |
  | 2016–Sep 2026 | 65 | 50% → 70% | 50% | 70% |

  It is a gate in section 6 and Phase 2. Its result is recorded whether or
  not it flatters the path, and a run **fails** if its return per year is
  below both of its mixes' or its worst drop is deeper than both. A failure
  goes back to the owner before any IRA trades.

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
- Whether a boost is on is state (`sig` latches in
  `boost/template.py:89-94`). After a restart it is recovered from the
  journal, never recomputed; a missing daily bar fails closed rather than
  computing RSI across the gap.
- The joint account is taxable: its tax lot method is set to LIFO at IBKR
  before the first trade, so a boost's exit sells the shares it bought days
  earlier, not the core holding (the tax study's assumption,
  `research/edge-search/tax/tax_drag.py`).
- **Decision D3**: **margin Boost** (decided 2026-10-14), not SSO. The
  joint account borrows for the step to 147%; the 150% limit (section 5)
  applies.

## 3. Architecture

LEAN cannot be used live without a paid QuantConnect license (its IBKR
connector checks one), so the brokerage side changes. ADR 0025 records that
decision (accepted 2026-10-15) and `docs/architecture.md` now reflects it:
LEAN for backtests, our own IBKR adapter for live trading.

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
- **Reconciliation is an input** (ADR 0019, as amended): every morning the
  adapter reports IBKR's positions and cash. A difference the evidence
  bounds puts the account in **Degraded**: no new risk, risk-reducing orders
  continue. Unverifiable evidence or an unbounded difference puts it in
  **Halted** until a person clears it. For these strategies, which have no
  stops, a **risk-reducing order** is one that moves the account's
  exposure toward 98% or lower: Boost's exit, and a momentum or bond sell.
  ADR 0025 does not define this. It requires a separate target-weight
  strategy ADR before code (its Consequences), a Phase 2 deliverable, and
  that ADR must adopt this definition or replace it with a stricter one.
- **Decision D6** (decided 2026-10-15): a Halted joint account still
  flattens to 98% by itself when Boost's exit fires or the margin alarm is
  urgent, and the owner is told each step:
  - an urgent alert when any account enters Degraded or Halted, with the
    reason (the reconciliation difference or the missing evidence);
  - an alert when the automatic flatten is sent, and another when it fills
    or fails;
  - the flatten is sell-only and sized from IBKR's own figures, never the
    journal's (which is what is in doubt): the smaller of what reaches 98%
    of IBKR's net liquidation value, priced at IBKR's current mark from the
    same account snapshot, and IBKR's reported holding minus every
    working or pending sell IBKR reports, ours or unknown. So no
    combination of fills can open a short;
  - if IBKR's holding, account values or open orders cannot be read, or a
    working sell is one the journal does not recognise, no order is sent
    and the urgent alert asks the owner to act by hand. It is attempted
    once per incident, never retried automatically;
  - this is the one exception to ADR 0019's "no order changes" while
    Halted, recorded as an amendment to ADR 0019 (2026-10-15), so the
    target-weight strategy ADR has a single order rule to follow;
  - it is journalled like any other order, and the account stays Halted
    afterwards until the owner clears it.
- **Alternative considered**: one standalone Python program. Faster to
  write, but it would duplicate the journal and replay guarantees the
  repository already tests. The owner chose the Go core (D4).

## 4. Data

- **Prices**: IBKR historical daily bars (adjusted for splits and
  dividends) for SPY, IEF, SHY and the universe. About 520 symbols a month
  at IBKR's pacing limits takes well under an hour, done overnight.
- **Universe** (the hard part): IBKR gives no ready "500 largest by dollar
  volume with fundamentals" list. The first proposal, IWB's holdings, failed
  the parity study (62% overlap, `docs/research/universe-parity-2026-10.md`).
  **Decision D8** (2026-10-15): build the universe as the backtest does,
  from every US-listed common stock:
  - **listings**: Nasdaq Trader's free daily symbol directory
    (`nasdaqlisted.txt`, `otherlisted.txt`); funds and test issues dropped by
    their `ETF` and `Test Issue` fields, preferred shares, warrants, units
    and rights by security name. Companies headquartered outside the US
    with US listings stay in, as in the backtest;
  - **rank**: one session's dollar volume (close × volume), the last session
    before the rebalance, as QuantConnect's coarse `DollarVolume` is; price
    at least $5 on that close; keep the top 500. Ranking and the $5 test use
    the **unadjusted** trade close and volume (for IBKR, `TRADES` bars, not
    `ADJUSTED_LAST`), since an adjusted close can move a stock across $5 or
    in and out of the top 500. The momentum, volatility and trend
    calculations keep using split- and dividend-adjusted closes;
  - **data source** for that one session's close and volume for every
    listed stock, still to choose: IBKR's own historical bars (free with the
    account, but one request per stock, several thousand a month; whether
    that fits IBKR's pacing limits is unconfirmed, IBKR questions 5–6), or a
    market-data service
    whose one request returns every stock's day (an account the owner
    opens; terms and cost checked first);
  - **known gaps**: QuantConnect's `HasFundamentalData` (Morningstar
    coverage) has no free equivalent and is left out, so a few extra names
    may enter; the sector cap's Morningstar labels need a source (below).
  - **checked against the backtest before Phase 2**: once the data source
    exists, the rule is run on as many recent months as the source's free
    history covers (24 for a two-year history), and each month's 500 names
    are compared with QuantConnect's coarse universe for the same month.
    This replaces the IWB comparison above for the chosen approach; Phase 1
    stays open until it is done. Every input must be as of the tested
    month, never today's:
    - which stocks existed and traded: that session's market-wide daily
      bars, which hold only the stocks that traded that day;
    - which were common stock: a listing or classification snapshot dated
      that month. Nasdaq Trader's files are current only, so they cannot
      serve. The candidate is the data service's reference list of tickers
      as of a given date (its type field separates common stock, ADRs and
      funds), to be confirmed when the source is chosen;
    - if no dated classification is available, the comparison covers only
      months from the start of a daily archive of Nasdaq Trader's files,
      which the system keeps from the day it starts, and says so. The
      comparison is not used as the gate until it rests on dated inputs;
  - **checked continuously**: during dry-run and paper trading, each
    month's live universe and picks are compared with a QuantConnect
    backtest of the same rule run to that month (free), and the
    differences are recorded. A large or growing difference stops the move
    to live trading.
- **Sectors**: the backtest's cap (at most 3 per sector) used Morningstar
  sectors, and no free source gives them. Free candidates, each to be
  compared with Morningstar on today's universe before one is chosen:
  - the GICS sectors in iShares' daily holdings files (IWV covers most US
    companies, but not those headquartered abroad);
  - the SEC's industry code (SIC) for every company that files with it,
    including foreign issuers, mapped to sectors.
  Labels that differ change which names the cap allows; the comparison
  measures how often that changes a month's picks. A paid fundamentals
  source (which would also close the `HasFundamentalData` gap) is the
  fallback.
  **Measured (2026-10-15, `docs/research/sector-labels-2026-10.md`):**
  GICS from IWV agrees with Morningstar on 95.8% of the names it covers and
  changes about one pick a month with no material change in return (20.08%
  against 20.10% a year); SIC agrees on 14 of 20 of the names IWV lacks,
  Nasdaq's screener sectors on 15 of 19.
  **Chosen:** GICS from IWV where it covers a stock, Nasdaq's screener
  sector otherwise, with each label's source recorded and the effect
  measured monthly through dry-run and paper trading. A stock with no label
  from either source (six at the snapshot: APGE, ATAI, CRNX, EA, WBD, WBS)
  is left out of that month's ranking, recorded, and alerted if it would
  otherwise have been among the top 25; it is never left uncapped.
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
  never on margin; at most N orders per run; no order larger than a set
  share of the account; a price-sanity band per order.
- **The joint account's 150% is a pre-trade check**: an order is refused if
  the exposure it would leave, valued at the previous close, exceeds 150%.
  Held exposure is not capped. After a buy to 147%, a fall of about 4% in
  SPY takes held exposure past 150% by itself (147(1 − x)/(100 − 147x) =
  1.5 at x ≈ 4.1%), in exactly the dips Boost buys, and selling there would
  not be the backtested rule.
- **A margin-cushion alarm**, separate from that check: the adapter reads
  IBKR's Excess Liquidity for the joint account every few minutes while the
  market is open, and alerts by Excess Liquidity as a share of the
  account's net liquidation value:
  - **warning below 40%**: at 147% exposure under the standard 25%
    maintenance requirement it is about 63%, so this needs a deep fall or a
    higher IBKR house requirement. The owner checks the position the same
    day;
  - **urgent below 20%**: the system sells back to 98% (D6) and alerts the
    owner; the sale is journalled like any other order;
  - **no reading for 30 minutes** while the market is open counts as
    urgent.
  The thresholds are configuration with their own hash (ADR 0016). The
  runbook, `docs/runbooks/margin-warning.md`, is written in Phase 3 with
  the action, who acts (the owner), and when the alarm clears (back above
  40%). It is rehearsed in paper trading before Phase 5.
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
- **IRA execution** (not settled by the backtest, which ran as a margin
  account, `defensive_v2.py:137-139`, to avoid settlement lag):
  - sells go first; buys are sent only once the sells have filled, and
    are sized from the cash actually realised, so they execute after the
    open, not at it. The parity backtest is rerun with that delay
    (`momentum/execution.py`'s one-session delay is the nearest measured
    case);
  - the IBKR IRA account type, and whether it lets a buy use unsettled sale
    proceeds, are confirmed with IBKR (D5);
  - **whole shares**: on the ~$30k IRA a name gets about $1,000. A stock
    priced above its target cannot be held, and one priced at a large share
    of it cannot be kept within the band. Decision D7: fractional shares
    through the API, if IBKR offers them for IRAs on Lite, or else a
    minimum-lot rule (for example, skip a name whose one share exceeds its
    target and give the weight to the next eligible name). Whichever is
    chosen goes into the parity backtest.
- **Day-trading margin rules**: which regime applies is to be confirmed
  with IBKR (D5). FINRA replaced the pattern-day-trader designation and its
  $25,000 minimum with intraday margin standards in Rule 4210(d)(2),
  effective 2026-06-04, and lets brokers phase the change in until
  2027-10-20 (FINRA Regulatory Notice 26-10). Under the new standards the
  constraint is the account's intraday margin, which the Excess Liquidity
  alarm above already watches. Under the legacy rule, which IBKR may still
  apply during the transition, the following holds.
- **Legacy pattern-day-trader rule**: the joint account is a margin account under
  $25,000. Boost's normal round trip (buy at one open, sell at a later
  open) is not a day trade, but an emergency same-day sell after a morning
  buy is. The margin runbook counts day trades and keeps to at most three
  in five sessions, with one exception: **a risk-reducing sale always takes
  priority** (an urgent Excess Liquidity sale, a halt-driven flatten to
  98%), even if it is a fourth day trade. A broker's pattern-day-trader
  restriction limits new buying, not closing sales, so the cost is that
  Boost stops: the system makes no boost buy while the restriction lasts,
  and holds SPY at 98%. How long IBKR's restriction lasts and how it is
  lifted are confirmed with IBKR (D5) and written into the runbook before
  the joint account goes live.
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
| IBKR market data for API bars, if required | about $10/month (~$120/year). Lite pays no commissions, so no commission waiver applies; budget it unless IBKR confirms the bars are free |
| Commissions | Lite: $0. Pro: ~$28 for Boost, and for momentum's ~25 names a month perhaps $100–200 per IRA |

**Decision D5**: the owner chose IBKR Lite for every account, if IBKR
allows it. To confirm with IBKR:
- that Lite is allowed on both IRAs and on a joint margin account;
- what the API's daily bars cost on Lite;
- that the three accounts can be linked under one username, so one IB
  Gateway and one weekly 2FA serve them all. Otherwise it is three
  gateways, three 2FA prompts and possibly three data subscriptions;
- the IRA account type, settlement of sale proceeds, and fractional shares
  through the API (D7);
- which day-trading margin regime the joint account is under (legacy
  pattern-day-trader rule or FINRA's new intraday margin standards), and,
  under the legacy rule, how a restriction works and is lifted (section 5).
  This is settled before Phase 2, since the risk limits depend on it.

Momentum's monthly trades
make Pro's $1 minimum matter for the smaller IRA, and the IRAs never borrow,
so Pro's margin rate does not help them.

## 9. Phases

| Phase | Work | Gate to move on |
| --- | --- | --- |
| 0 | Owner decisions D5 and D7 (D6 decided 2026-10-15); the broker-adapter ADR (0025, accepted 2026-10-15); this plan reviewed (owner, and an independent review on 2026-10-15) | Done except D5 and D7, which must be settled before Phase 2 starts, since the strategy modules and limits depend on them |
| 1 | Universe parity study (research, QuantConnect free) | IWB tested and failed (2026-10-15); D8 builds the universe from all US listings. Open until: the data source is chosen and the D8 rule's recent-months comparison with QuantConnect's universe is recorded (section 4). The sector check is done (2026-10-15) |
| 2 | The target-weight strategy ADR (decision contract, risk limits, the risk-reducing definition in section 3); Go strategy modules and risk limits, test first; backtest parity; the glide-path backtest (section 2) | Parity passes, and the glide-path backtest passes its stated test |
| 3 | Broker adapter, journal wiring, reporter, dashboard; dry-run on Lightsail | A clean dry-run month for Boost and 2 clean dry-run rebalances for momentum |
| 4 | IBKR paper trading | 2–3 clean months |
| 5 | Limited live, one account at a time | A clean month each. The joint account goes live only after the margin-warning runbook has been rehearsed in paper trading (an injected low Excess Liquidity reading, handled end to end) |

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
- **D6** A Halted joint account **flattens to 98% by itself**, with an
  alert when the account freezes, when the sale is sent and when it fills
  or fails (decided 2026-10-15; section 3).
- **D7** Whole shares in the IRAs: fractional shares through the API, or a
  minimum-lot rule (section 5). Open; depends on what IBKR confirms.
- **D8** Live universe: **every US-listed common stock ranked by one
  session's dollar volume, as the backtest does** (decided 2026-10-15;
  section 4). Its data source is still to choose.
- Account opening, funding, rollovers and the Lightsail sign-up are the
  owner's to do; nothing here signs up for or pays for anything.
