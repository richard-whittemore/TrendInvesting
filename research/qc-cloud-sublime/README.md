# Sublime research check, on QuantConnect Cloud (Free plan)

## What this is

A **free, research-only** check of one question: does Sublime-style trend
trading on US stocks beat buying and holding SPY, after realistic costs, over
QuantConnect's free 1998-to-date US equity history?

It is the Sublime counterpart of `research/qc-cloud/`, the Turtle Baseline
check. That check matched the production Go engine's fills on AAPL
2003–2014, and on the top 200 US stocks, 1998–2015, it lost: CAGR −5.7 %
against SPY's +6.1 %. Variants with a market filter or smaller Units
reached only about 0 %.

This folder implements **only** the Sublime rules that
`docs/methodology/Methodology_Analysis.md` records, each with the
provenance tag the analysis gives it. It has no Go counterpart, so no
fidelity check exists for it; the local smoke run below is a sanity check,
not a parity check. No market data is committed here.

## Files

- `rules.py`: the rule core. Pure standard-library Python with no
  QuantConnect imports; every rule cites the analysis or an ADR.
- `test_rules.py`: unit tests for `rules.py`, written before it, on synthetic
  series and on the two golden figures section 9 of the analysis names.
- `main.py`: the `QCAlgorithm` (`SublimeResearch`) that drives `rules.py`.
- `build_upload.py`: writes stripped copies of `main.py` and `rules.py` to
  `dist/` (git-ignored). Each copy stays under the Free plan's
  32,000-character limit per file.
- `test_build_upload.py`: checks both stripped files are under the limit and
  compile, and that `test_rules.py` passes against the stripped `rules.py`.
- `smoke/lean_main.py`, `smoke/run_local.sh`: the local AAPL smoke run.

`make research-test` (part of `make check`) runs both test files.

## How to run it on QuantConnect

The steps match `research/qc-cloud/README.md`, "How to run it". In short:

1. From the repository root, run `python3 research/qc-cloud-sublime/build_upload.py`.
   It prints each stripped file's size and fails if either is at or over
   32,000 characters.
2. In QuantConnect's Algorithm Lab, create a new Python project. Replace its
   `main.py` with `research/qc-cloud-sublime/dist/main.py`, then add a file
   named exactly `rules.py` with `dist/rules.py`'s contents. **Paste the
   `dist/` copies, never the originals.**
3. Save the project and click **Backtest**.

To run the in-sample span the Turtle check was judged on, set
`END_DATE = (2015, 12, 31)` near the top of `SublimeResearch` before building.
If the run times out, lower `UNIVERSE_SIZE` (200 by default) to 100.

## How to read the results

Read the backtest's **statistics panel**, not the Logs tab: the Free plan
caps logs at 10 KB. Each figure is a runtime statistic with a short value and
one key per item. The keys match `research/qc-cloud`'s:

- `OVERALL`: CAGR, max drawdown, and CAGR ÷ max drawdown (ADR 0012's
  primary metric).
- `REGIME <window>`: the same, for each of ADR 0012's seven Regime Windows.
  A window reads `no-data` when it has fewer than two marks.
- `Campaigns`: the number of closed Campaigns, their win rate, and their
  average win and average loss in R. R here is a Campaign's money, over
  every position, divided by the initial risk of its first position.
- `Signals`: Phase C Signals, and how many were Grade A and Grade B.
- `Commission`: dollars charged.
- `Declines`, and one `Decline <reason>` key per reason.
- `SPY BUY-AND-HOLD`: SPY's total return over the same span, in the same
  format as `OVERALL`.
- `Config`: the run's sensitivity-variant switches ("Sensitivity variants").

**When trading can start.** The rules need five years of each stock's
history, and SPY's weekly 200 SMA needs 200 weeks. QuantConnect's data
begins in 1998, so a 1998 start stays in cash until about 2002–2003, and
`OVERALL` then includes those cash years, 2000–02 bear market among them.
The Regime Windows from 2003 onward are the fair comparison with SPY.

## Cloud result (in-sample, 1998–2015)

The run covered the top 200 US stocks by dollar volume, with $1M starting capital, on QuantConnect Cloud. It stopped at 2015-12-31, so everything from 2016 on stays held out. Backtest `e947267a6cb9dfaec6badf86e3a5e56f`, 2026-09-27, run on this code after the review fixes. An earlier run on the pre-fix code, backtest `00f865ad3335ec1f38de8fc5990c9547`, gave +1.36% CAGR with a 13.6% max drawdown.

| Run | CAGR | Max drawdown | Return / drawdown |
|---|---|---|---|
| Sublime control (this folder) | +1.45% | 13.6% | 0.107 |
| Turtle Baseline (`research/qc-cloud`) | −5.74% | 87% | — |
| Turtle + SPY above its 200-day SMA | −0.35% | 52% | — |
| Turtle, 0.1% Unit Volatility Fraction | −0.26% | 36% | — |
| Turtle, both of the above | −1.26% | 31% | — |
| SPY buy-and-hold, total return | +6.07% | 55% | 0.110 |

- **Campaigns:** 48, of which 27% won. The average win was +3.83R and the average loss −0.77R.
- **Signals:** 327 Phase C Signals were detected. 122 were declined before grading because the stock wasn't aligned (`Decline entry: stock not aligned`; see `_signal()` in `main.py`). The remaining 205 were graded: 53 Grade A and 152 Grade B, producing 259 orders.
- **Late start:** it holds cash from 1998 to 2002, because the prior-year-high and weekly 200-bar rules need about 5 years of history.
- **By period (CAGR):** 2003–07 +1.98%, 2008–09 −0.85%, 2009–15 +2.63%.

It beats every Turtle stock variant in the table on both return and drawdown (the Turtle variants were temporary cloud builds of `research/qc-cloud`, run on the same universe and span, 2026-09-26), but still trails SPY by about 4.6 points a year. That's mostly because it rarely has much capital invested. Sublime's proprietary pieces are proxies here (see the provenance tags below), so read this as a lower bound on the real methodology rather than a verdict on it.

## The rules, with provenance

Tags follow `Methodology_Analysis.md` section 0. Rule numbers are the ones
section 3 gives.

### Universe and hard filters

| Rule | Implementation | Tag |
|---|---|---|
| Universe | `research/qc-cloud`'s monthly top 200 US common stocks by dollar volume. A held or working symbol stays subscribed. | Carried infrastructure |
| 3: price ≥ $20 [V 01:13:32] | Raw close at the Signal | DISCLOSED |
| 3: volume > 1 M shares [V 00:59:58] | 20-day median raw share volume ≥ 1,000,000 | DISCLOSED (window: open question) |
| 3: 5–10 years of history [M p.54] | ≥ 1,260 completed daily bars (the lower end) | DISCLOSED |

### Market regime (S&P 500, read from SPY)

| Rule | Implementation | Tag |
|---|---|---|
| 5: monthly regime [V 00:07:32–00:08:30] | SPY close against last *calendar* year's high and low: bull, bear or sideways | DISCLOSED |
| 4: bull buys, flat stands aside [V 00:05:09] | No new position unless the month is bull. Long-only: bears are not shorted | DISCLOSED |
| 6, 7, 8: weekly 200/50, daily 200/50/20, alignment | Each is "SPY close above the SMA". A weekly SMA counts the week in progress | DISCLOSED |
| 27: risk by conditions [V 00:28:01; R; M p.55] | Full alignment: 1 %. Bull month above the weekly 200 but not fully aligned: 0.5 %. Bull month below the weekly 200: 0.25 % | RECONSTRUCTED map (open question) |
| 9: never go straight to cash | A regime change never closes a position | DISCLOSED |

### The entry: 4PS second breakout (section 6, Model E)

| Rule | Implementation | Tag |
|---|---|---|
| 23: base [2B p.1; section 4.8] | At least 55 completed bars without a new 55-bar high | DISCLOSED (one instance) |
| 21, 22: Phase A | Close above max(prior 55-bar high, last calendar year's high). That level is the breakout level | PROXY (Model E) |
| 21: Phase B, the retest | A later low at or below the breakout level + 1 ATR. A deeper undercut also counts; only the cancel rule's close guards the downside (Model E) | PROXY (k₁ = 1: open question) |
| 21: Phase C, the Signal | A close above the highest high between A and B | PROXY (Model E) |
| 2: Donchian 20 break and close [V 00:47:53] | Phase C's close must also be above the prior 20-bar high | DISCLOSED |
| Cancel | A close more than 3 ATR below the level, or more than 55 bars after A | PROXY (k₂ = 3, m = 55: open questions) |
| 8, 13, 4.7: stock alignment | At the Signal: close above last year's high, the weekly 200 SMA and the daily 200 SMA (§4.7's KISS gate). Daily and weekly trend-filter colour green or dark green | DISCLOSED |
| 13: trend filter [V 00:31:08–00:38:16] | 20-period SMA of closes with 1σ and 2σ bands (population σ). Colour is by closing price | DISCLOSED |
| 16: at all-time highs [V 00:52:15] | Grade A: the Signal's close is above every earlier high the symbol's indicators hold: its 1,260-bar backfill when it joined the universe, plus every bar since | PROXY (a 5-year high for most symbols, not a true all-time high) |
| 18: Grade A before Grade B [V 01:14:41] | Grade B is taken only in a Session with no Grade A Signal, once ineligible Signals are dropped (ADR 0011 point 3) | PROXY |
| Ranking within a Grade | Strength, (close − close 63 bars earlier) / ATR (Turtle p.29, ADR 0010), then median dollar volume, then symbol | PROXY for "best-performing stocks" [V 00:02:31] |
| 25: order above the breakout bar's high [M p.55] | Stop-limit buy one raw tick above the Signal bar's high, for the next Session only (ADR 0011) | Level DISCLOSED; the offset formula is EXCLUDED, so one tick |

### Sizing, stops, exits, adds, risk ceilings

| Rule | Implementation | Tag |
|---|---|---|
| 30: size [M p.56]; ADR 0003's fixed-risk-at-stop | shares = floor(equity × risk / (3 × ATR)), in whole raw shares. Equity is actual, not notional: no drawdown rule is disclosed (section 3.11) | RECONSTRUCTED |
| 32: initial stop ≈ 3 × ATR [M p.55–56] | 3 × ATR below each position's own fill | DISCLOSED multiple. The ATR period is undisclosed (20: open question) |
| 34 and §4.6(a): Donchian-20 exit [30 p.7–8] | Each position's Exit Order rests at the higher of its own stop and the 20-bar low, so the channel trails it | DISCLOSED |
| 38, 39: add to winners, 1 ATR apart [V 01:20:45] | An add is proposed when the close is at least 1 ATR (the newest position's entry ATR) above the newest position's fill | DISCLOSED |
| 40: only once the first position is risk-free [M p.56] | The first position's Exit Order level is at or above its fill | DISCLOSED |
| 41: each add sized and stopped separately [M p.56] | An add is sized like an entry at the current regime risk and ATR, has its own 3 × ATR stop, and is ordered one tick above the bar's high | DISCLOSED |
| 42: maximum adds | Five positions per asset (the e-book's illustration [M p.56–57]) | NOT DISCLOSED (open question) |
| 29: ceilings [R] | Risk initiated per day ≤ 4 % of equity; aggregate open risk ≤ 10 % (the lower ends) | DISCLOSED ranges (level: open question) |
| 40: ≤ 2 % at risk on one asset [M p.56] | Checked for every add | DISCLOSED |
| 31: a stop on every position [R; M p.57] | Every held position rests a stop-market sell at all times; a cancelled one is re-placed | DISCLOSED |

"Risk" everywhere means money at the stop. A position whose Exit Order level
is at or above its fill contributes nothing (CONTEXT.md "risk-free").

### Not implemented

- **EXCLUDED by the analysis:** round-number and resistance tightening, and
  overriding an automated exit (rule 36); "what do I not see?" (rule 20);
  the entry-offset formula (rule 25); the human-in-the-loop selection
  (rule 44).
- **Left out for want of data:** the earnings blackout (rule 26) needs an
  earnings calendar, and the Free plan's data offers none this script can
  rely on. The results therefore include entries a Sublime trader would
  have skipped.
- **Left out because the sources give no threshold:**
  - Avoiding "uber expensive" mega-caps (rule 3). The top-200 dollar-volume
    universe is itself mega-cap heavy.
  - The smooth-history proxies (R², efficiency ratio; rule 15).
  - Sector strength (rule 19; Model C's k).
  - Pivot levels (rules 11–12). Grade A implies that price is above every
    pivot.
  - The weekly 50-SMA rebalance (rule 6).
  - Seasonality as a sizing input (rule 10: "whether to encode it is a
    decision").
- **Experiments, not the control:** the 3 × ATR chandelier trail (rule 33)
  and the 50-SMA breach exit (rule 35), per section 4.6.
- **Sublime has no Unit caps** (section 3.11: no disclosed position limits
  or correlation control), so ADR 0008's caps are absent. The risk ceilings
  and cash bind instead.

## Open questions

Each question names the default this check uses. None was chosen by looking
at a result.

1. **ATR period:** 20, Wilder-smoothed, so that it equals N's arithmetic
   (the sources give none).
2. **4PS thresholds:** retest within k₁ = 1 ATR; cancel on a close
   k₂ = 3 ATR below the level (the point where a first-breakout entry's
   3 × ATR stop would have been hit); window m = 55 bars.
   - The base resets on any new 55-bar high, so a V-shaped recovery into
     last year's high never qualifies. On AAPL, 2003–2014, only three Phase A
     breakouts occurred.
   - A Signal whose order does not fill is consumed; it is not carried
     forward.
3. **Regime-to-risk map:** 1 %, 0.5 % and 0.25 %. The source's 2 % for
   "optimal conditions" is unused.
4. **Ceilings:** the lower ends, 4 % daily and 10 % aggregate. Rule 29
   raises them when the S&P prints all-time highs, and the sources do not
   define that measurably.
5. **Volume floor:** 1 M shares, as a 20-day median. The source allows
   500 k "at the very least".
6. **Maximum positions per asset:** 5.
7. **Adds after a stop-out:** no rule stops them, so one continues when the
   remaining first position is still risk-free. The AAPL smoke run does this
   on 2007-08-02.
8. **Exit on a break, intraday or at the close:** intraday, as a resting
   stop at the 20-bar low (the analysis calls this the Turtle System-2 exit
   applied to a stock).
9. **The initial stop's anchor:** the actual fill (ADR 0006's convention);
   the source gives none.
10. **Regime source:** SPY's Adjusted (total-return) closes stand in for the
    S&P 500 index (next section).

## Fidelity audit (2026-09-27)

The rules above were checked against the code and against the sources
themselves: the webinar transcript, [M], [R], [4PS], [2B], [30] and [KISS].
The code does what the tables say. Where the sources and this control
differ, the difference comes from Model E's thresholds or from the
universe, not from the sources:

1. **The base (changes results a lot).** The sources define consolidation as
   time spent below the level that is later broken. That is the range
   "between the high and the low of last year" [V 00:46:00], or a base
   lasting months or years under a prior high (CBOE 2018 to July 2023 and
   PGR April to October 2023 [4PS]). Model E's base instead resets on any
   new 55-bar high. A rally inside the range therefore resets it, and so
   does an intraday poke above the range. This is why AAPL gave only three
   Phase A breakouts in twelve years.
2. **The universe (changes results a lot).** Sublime scans more than 10,000
   assets and rejects "uber expensive stocks like Amazon" in favour of
   "cheap stocks creating new ATHs" [M p.54]. The top 200 by dollar volume
   is the opposite: it is mostly mega-caps.
3. **Breakout timeframes (matches the sources).** All three phases are read
   on daily bars ("We look for these 3 mini phases on the daily timeframe"
   [4PS]). The level combines the 55-bar high with last calendar year's
   high, the monthly-chart level. The all-time high only grades a Signal,
   and the weekly timeframe only gates alignment. The sources name last
   year's high [V 00:46:20], the all-time high [V 00:51:14; M p.55] and the
   consolidation's resistance [4PS; 30 p.5]. None of them names a 55-bar
   high: [2B]'s 55 days is a length, not a channel.
4. **Retest (changes results a little to a lot).** A Phase A close is
   usually within 1 ATR of the level, so the next bar's low nearly always
   "retests" it. Phase B is then close to automatic.
5. **History (a little).** A stock needs 5 years of history, which the
   sources disclose [M p.54]. The 1998–2002 cash spell comes from SPY's
   weekly 200 SMA (200 weeks from QuantConnect's 1998 data start), not from
   the stock floor, so no stock-side variant can move it.
6. **Risk (a little to a lot).** The control always sits at [R]'s lower
   end. [R] moves to the upper end (2 %, 8 %, 20 %) when the S&P prints
   all-time highs in full bloom.
7. **Alignment (a little).** The webinar also requires the stock to be above
   its weekly 50 SMA and its daily 50 and 20 SMAs [V 00:46:24, 00:44:35].
   The control uses §4.7's minimal KISS gate.

## Sensitivity variants

Each variant is a copy of the control with only the listed `SublimeResearch`
class constants changed in `main.py`. Every run, the control included, sets
`END_DATE = (2015, 12, 31)`: nothing from 2016 onward is run or read. Rerun
the control first, because Deviations 11 changes it. Compare the variants on
the Regime Windows from 2003 onward, since every run is in cash until about
2002 (Fidelity audit 5). The first eight were chosen before any of them was run; PRICE_CAP, the ninth, was added afterwards to test the other reading of "uber expensive".

| Name | Constants | Question it answers |
|---|---|---|
| Control | defaults | The baseline, rerun with the re-add fix |
| `LAST_YEAR` | `BREAKOUT_LEVEL = rules.LEVEL_LAST_YEAR`, `BASE_RULE = rules.BASE_CLOSES_BELOW_LEVEL` | Is the webinar's breakout (55 closes under last year's high, then a close above it) better than Model E's? |
| `BASE_BELOW` | `BASE_RULE = rules.BASE_CLOSES_BELOW_LEVEL` | How much does the new-55-bar-high base reset starve the control of Signals (audit 1)? |
| `CH252` | `BREAKOUT_CHANNEL = 252` | Does a 1-year channel, the top of a longer base, beat the 55-bar one? |
| `RETEST_05` | `RETEST_ATR = 0.5` | Does requiring a deeper pullback (a real retest) help (audit 4)? |
| `RETEST_2` | `RETEST_ATR = 2.0` | Does a looser retest help? |
| `WIDE` | `UNIVERSE_SIZE = 500`, `UNIVERSE_SKIP = 50` | Does dropping the 50 largest by dollar volume, over a wider pool, help (audit 2)? |
| `HIST2Y` | `MIN_HISTORY_BARS = 504` | Does the 5-year floor exclude young leaders? The stock's own weekly 200 SMA still needs about 3.9 years |
| `RISK_ATH` | `RISK_UPPER_AT_SPY_ATH = True` | Does [R]'s upper end (2 % a position, 8 % a day, 20 % in aggregate), used when SPY set an all-time high within 20 sessions and is in full bloom, deploy more capital without a worse drawdown? |
| `PRICE_CAP` | `MAX_SHARE_PRICE = 200` | The other reading of [M p.54]'s "uber expensive stocks like Amazon": a high *share price* rather than a large company. Stocks above $200 (raw) are left out of the monthly universe; the $200 line is a judgement, since the source gives no number. |

`WIDE` subscribes 2.5 times as many symbols as the control. If it times out,
use `UNIVERSE_SIZE = 400`. Every variant stays well under the Free plan's
10,000 orders: the control placed 259.


### Results (in-sample 1998–2015, run 2026-09-27)

Every run includes the universe re-entry fix. Periods are CAGR; the drawdown is the whole run's maximum. SPY buy-and-hold made +6.07% CAGR with a 55% max drawdown over the same span.

| Variant | CAGR | Max DD | 2003–07 | 2009–15 | Campaigns | Won | Avg win / loss R |
|---|---|---|---|---|---|---|---|
| CONTROL | +0.38% | 15.4% | −0.49% | +1.37% | 70 | 23% | 2.73 / −0.76 |
| CH252 | +1.37% | 18.8% | +4.12% | +0.66% | 103 | 31% | 2.35 / −0.73 |
| BASE_BELOW | +0.61% | 17.2% | −1.46% | +2.88% | 162 | 33% | 1.43 / −0.75 |
| RETEST_05 | +0.46% | 12.8% | −0.41% | +1.52% | 69 | 23% | 2.75 / −0.76 |
| PRICE_CAP | +0.42% | 16.0% | −0.49% | +1.49% | 68 | 21% | 3.05 / −0.73 |
| RETEST_2 | +0.36% | 15.7% | −0.57% | +1.37% | 71 | 23% | 2.76 / −0.76 |
| HIST2Y | +0.36% | 16.0% | −0.20% | +1.12% | 74 | 24% | 2.53 / −0.75 |
| LAST_YEAR | +0.24% | 14.3% | +0.63% | +0.18% | 114 | 36% | 1.43 / −0.85 |
| WIDE | +0.20% | 18.1% | −2.38% | +2.31% | 112 | 32% | 1.66 / −0.81 |
| RISK_ATH | −0.29% | 20.7% | +0.17% | −0.84% | 61 | 25% | 2.01 / −0.83 |

**Conclusion.** No variant comes close to SPY. The best, CH252, trails it by 4.7 points a year. The corrected base rules (BASE_BELOW, LAST_YEAR) add trades but cut the average win, and neither reading of "uber expensive" (WIDE, PRICE_CAP) helps. None is promoted to the control: the owner approved promoting a *winner*, and nothing here wins.
## Deviations from `research/qc-cloud`'s infrastructure

The rest is carried over unchanged:
- whole raw-share Units at the raw tick;
- `RawShareFeeModel`;
- dividends paid on raw shares;
- queued order events;
- immediate settlement;
- retained symbols;
- re-placed Exit Orders;
- SPY as a never-traded benchmark;
- runtime statistics;
- decline counters;
- the `FIXED_SYMBOLS`, `START_DATE` and `END_DATE` switches.

What differs:

1. **Cost conventions are in ATR.** The price cap is level + 1 ATR (ADR
   0005), and slippage is 0.05 ATR per fill (ADR 0013).
2. **Entries and adds are decided at the close and live one Session.**
   Sublime decides on completed bars (rule 43), so its orders are not
   fill-chained (no ADR 0011 amendment).
3. **Exit Orders rest below the bar they were decided after.** LEAN
   evaluates an amended order against the bar it was amended after
   (`research/qc-cloud` Deviations #20). So an amendment only ever raises an
   Exit Order, and rests it at least a raw tick below that bar's low
   (`rules.exit_stop_price`). The only effect is on a day that sets the new
   20-bar low: that evening the order rests one tick under the channel.
   `Campaign.open_risk` and `add_ready`'s risk-free check read that actual
   resting price (`Campaign.set_resting_stop`), never the higher theoretical
   `exit_level`, so a position a tick below its fill is never read as
   risk-free.
4. **SPY drives the regime from its Adjusted series.** It is subscribed once,
   Adjusted, for the benchmark. Dividends lower earlier Adjusted prices, so
   last year's high reads about one year's dividend yield (≈ 2 %) lower than
   on the index. This slightly favours "bull".
5. **Volume is raw by the split ratio.** LEAN's split-adjusted volume is raw
   volume times the split ratio. This was observed on the pinned image:
   AAPL 2003-05-07 read 1,068,886,829 against 19,087,219 raw shares at a
   ratio of 56. The volume floor divides by the ratio; the dollar-volume
   tie-break uses split-adjusted close × volume.
6. **A holding LEAN no longer has closes its Campaign.** For example, after
   LEAN liquidates a delisting itself. The Campaign closes at the last
   close, counted as `Decline exit: holding vanished`.
7. **The Sublime filters apply in fixed mode too.** They are strategy rules,
   not universe membership. `research/qc-cloud` skips eligibility in fixed
   mode only to match a Go run.
8. **A partial fill on an Exit Order, then cancelled, shrinks the position.**
   `Campaign.reduce_unit` realizes the shares that sold and reduces the
   recorded quantity by exactly that many, before `_maintain_exit_orders`
   re-places the replacement stop, so it is never sized for shares no
   longer held.
9. **A cached split ratio is refreshed by one retry read when unset.**
   `_split_ratio_for` re-reads it in `_signal` and `_decide` alike, so a
   transient `History` failure at `OnSecuritiesChanged` cannot permanently
   suppress a symbol's eligibility or sizing.
10. **A rejected Exit Order amendment leaves the resting stop unchanged.**
    `_maintain_exit_orders` only records the new price
    (`Campaign.set_resting_stop`) once `ticket.Update`'s response reports
    success; open_risk and add_ready never rely on a stop that LEAN did not
    actually place.
11. **A symbol that re-enters the universe is rebuilt from a fresh
    backfill.** LEAN delivers no bars and no split events for a symbol
    while it is out of the universe. Reusing its old state would run the
    channels, the base count and the Setup over a gap, and would keep a
    split ratio that a missed split made stale. A symbol with a Campaign is
    never removed (it is retained), so only idle symbols are rebuilt.

## Local smoke run

`smoke/run_local.sh <lean-workspace> [project-dir]` runs the algorithm on
local LEAN with the pinned image and `--no-update`. It uses only data
already in the workspace, never pulls an image and never fetches data.
`smoke/lean_main.py` sets `FIXED_SYMBOLS = ("AAPL",)` and the span
2003-01-01 to 2014-12-31, and keeps the 1,260-bar warm-up, which reaches back
to the data's 1998 start.

**Result, AAPL alone, $1,000,000.** It ran cleanly: no errors, no anomalies,
and no failed data requests.

| | |
|---|---|
| `OVERALL` | 2003-01-02..2014-12-31 CAGR = 0.25 %, MaxDD = 9.25 %, Ratio 0.027 |
| `SPY BUY-AND-HOLD` | CAGR = 9.16 %, MaxDD = 55.17 %, Ratio 0.166 |
| `REGIME 2003-07 bull` | CAGR = 0.69 %, MaxDD = 8.27 % |
| `REGIME 2008-09 crash` | flat, in cash |
| `REGIME 2009-19 bull` (to 2014) | CAGR = −0.07 %, MaxDD = 2.41 % |
| `Campaigns` | 3, win rate 0.667, average win +1.76 R, average loss −1.79 R |
| `Signals` | 3, all Grade A |
| `Declines` | 16: `add: asset-risk` 9, `add: market regime` 7 |
| `Commission` | $64.92 |
| LEAN | 18 orders, net profit 3.04 %, end equity $1,030,417.43 |

**Hand-checked trades.** Each check below compares LEAN's order against the
local raw bars and an offline replay of `rules.py` over the same zips.

- **Entry, 2007-05-08.**
  - Phase A came on 2007-04-26: the close, 3.5300 split-adjusted, was above
    max(55-bar high 3.4582, 2006 high 3.3268).
  - Phase B came on 04-27: a low of 3.4889, within 1 ATR (0.0752) of 3.4582.
  - Phase C came on 05-07: a close of 3.7114, above the reaction high of
    3.6507 and the 20-bar high. The stock was aligned and Grade A, and SPY
    was fully aligned, so risk was 1 %.
  - The order's stop was the 05-07 raw high, $104.35, plus one tick:
    $104.36 / 28 = 3.727143. Its limit was that plus 1 ATR (0.0719): 3.7989.
  - Size: $1,000,000 × 1 % / (3 × 0.0719) = 46,361, rounded down to whole
    raw shares: 1,655 × 28 = 46,340. LEAN filled 46,340 at 3.727143 +
    0.05 ATR = 3.730739.
- **Stop-out, 2007-08-01.** The third add, filled at 4.955037, had its own
  3 × ATR stop at 4.588571, which is $128.48 raw. The 08-01 low of $127.80
  went through it, and it filled at the stop less slippage, 4.582465.
- **Exit, 2007-08-09.** The remaining four positions exited at the 20-bar
  low, $127.80 raw (4.564286): the 08-01 low. The Exit Orders had trailed
  up to it.
- **Entry, 2010-09-21.** The order was one tick above the 09-20 high of
  $283.78: 10.135357. The size was small, 2,184 shares, for two reasons.
  SPY was below its weekly 200 SMA, so risk was 0.25 %. And ATR was inflated
  (0.394) by a bad print in the local data: 2010-08-31's low reads $25.30
  against a true low of about $241. The same print pins the 20-bar low near
  0.90 through 2010-09-28.

The strategy barely trades AAPL. It took three Campaigns in twelve years,
because the 55-bar base rarely forms in a stock that trends strongly (open
question 2). That is a property of the rules as the analysis records them,
not a fault in the run.

## Reading the results against costs

`research/qc-cloud/README.md`, "Reading the results against costs", applies
unchanged. This is not financial advice.
