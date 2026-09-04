# Methodology Analysis — Turtle Trading and Sublime Trading

**Version:** 1.0 (2026-09-04)
**Status:** Source-grounded analysis. Supersedes the ChatGPT/Codex analysis of 2026-08-28 (archived in `~/Desktop/Trend_Investing/Trend_Investing_Thread.md`).
**Scope:** Everything needed to write the strategy specification — what each methodology's rules actually are, where each rule comes from, where the sources conflict, and which pieces can be automated. It does not decide the specification; that happens in the grilling session and is recorded in `CONTEXT.md` and ADRs.

## 0. How to read this document

- **Sources are cited inline** as `[T p.N]`, `[M p.N]`, `[V hh:mm:ss]`, etc. — see §1 for the key. Page numbers are the *printed* page numbers in each PDF (for the Turtle Rules, PDF page = printed page + 4).
- Every rule carries a **provenance tag**:
  - **DISCLOSED** — stated plainly in a primary source.
  - **RECONSTRUCTED** — mathematically implied by what the source says, but the formula itself is not printed.
  - **PROXY** — a measurable stand-in for something the source describes qualitatively or leaves proprietary.
  - **EXCLUDED** — discretionary or proprietary; cannot be automated faithfully from public material.
- Rules are also tagged **BASELINE** (belongs in the Turtle stock control) or **EXPERIMENT** (a Sublime or hybrid variant to be tested as an ablation) where that decision is clear from the sources; the rest are decided in grilling.

---

## 1. Source and evidence register

| Key | Source | Type | Authority | Notes |
|---|---|---|---|---|
| **T** | *The Original Turtle Trading Rules*, Curtis Faith, OriginalTurtles.org, 2003 (37 pp) — iCloud `Investing/Trend Investing/The Turtle Rules.pdf` | Primary | **Authoritative for Turtle** | Written by an original Turtle; complete rule set. |
| **N-T** | Notion *Turtle Trading Rules Distilled* (Richard's notes on Covel, *The Complete TurtleTrader*) | Secondary | Non-authoritative | Useful for the equity-sizing simplification and Covel's framing; contains two errors and one unresolved dual citation (§4.9). |
| **V** | Webinar *Sublime Trading Philosophy* (85:30, presenter Zaheer Anwari, recorded ≈ Aug 2026) — `~/Desktop/Sublime_Trading_Philosophy.mov`; transcript `Sublime_Trading_Philosophy.transcript.md` (this folder) | Primary for Sublime | Authoritative for what Sublime *says*; not a rulebook | Analysis-only session; the presenter states that entries/stops/TSL live on "another page" [V 00:04:03]. |
| **M** | *The Complete Methodology* (Sublime Trading e-book, 59 pp) | Primary for Sublime | Authoritative for disclosed rules | Chapters 1–6 are motivational; Brain Workout 2 (pp.42–59) contains the rules. |
| **R** | *The Sublime Trading Risk Management Rules* (community post, Zaheer Anwari, 2 pp) | Primary | Authoritative | Three risk ceilings scaled by S&P regime. |
| **4PS** | *Don't Add CBOE…* (4PS Method) and *Phase 3 To Phase 4 … Explained* (community posts, Nov 2023) | Primary | Authoritative | The entry sequence (Phase 1–4; A/B/C). |
| **2B** | *Why We Wait For The Second Breakout* (Kola Gbadamasi, Nov 2023) | Primary | Authoritative | Contains the only numeric consolidation threshold (55 trading days). |
| **30** | *I Generate 30% Profit Per Year…* (Kola Gbadamasi, DataDrivenInvestor, Dec 2023) | Primary | Authoritative | Explicit Turtle lineage; Donchian-20 exit; volume/routine. |
| **KISS** | *The KISSer Approach* (Zaheer Anwari, Jan 2024) | Primary | Authoritative | Simplified public rule set (last-year H/L + weekly/daily 200sma). |
| **3S** | *How To Know If A Stock Is Worth Investing In 3 Seconds* (Oct 2023) | Primary | Supporting | Same rules as KISS; worked examples. |
| **MA** | *Trading with the 20, 50 & 200 Moving Averages* (Jan 2016) | Primary (old) | Supporting | The 20/50/200 stacking rule; predates the trend filter. |
| **BP** | *The Stock Market Blueprint* (2 pp) | Primary (marketing) | Supporting | Three-step process; S&P regime. |
| **N-S** | Notion *Trend Investing* and *Moving Average Indicators* | Secondary | Non-authoritative | Richard's notes on KISS/30/MA; faithful to those articles. |
| **QC** | Legacy QuantConnect prototype (`turtle-trading-strategy/main.py`) | Implementation | Non-authoritative | Records prior decisions, not rules. |
| **GPT** | ChatGPT/Codex thread of 2026-08-28/29 | Analysis | Non-authoritative | Reviewed in §8. |

Not used: *Riley's Favorite Trading Patterns* and Notion *Riley Coleman Future Trading Strategy* (unrelated discretionary/intraday material); *Wealth Calculator ST.xlsm* (compound-growth projection, no trading rules).

---

## 2. The Turtle Trading rules (complete, from Faith)

Faith organises a "Complete Trading System" into six components: markets, position sizing, entries, stops, exits, tactics [T p.8]. Every rule below is **DISCLOSED** unless marked.

### 2.1 Markets
- Liquid US futures on Chicago and New York exchanges; ~21 named contracts across rates, FX, index, metals, energy, softs. Grains excluded (Dennis's own position limits), meats excluded (pit integrity) [T p.10–11].
- A trader may opt out of a market, but then must never trade it — no inconsistent participation [T p.11].
- *Stock adaptation (decision, not source):* the equity analogue is a point-in-time, liquidity-screened universe; the "never trade it inconsistently" rule maps to a rule that universe membership changes only through declared eligibility criteria.

### 2.2 Volatility: N
- True Range = max(H−L, H−PDC, PDC−L) [T p.13].
- N = (19 × PDN + TR) / 20, seeded with a 20-day simple average of TR [T p.13]. This is Wilder-style smoothing, **not** an SMA of TR and **not** an average of price.
- Dollar volatility = N × dollars per point [T p.13].

### 2.3 Position sizing (Units)
- 1 Unit = 1 % of account ÷ (N × dollars per point); sized so a 1N move ≈ 1 % of equity [T p.14]. Worked example: Heating Oil, N = 0.0141, $1 M account, 42,000 gal → 16.88 → **truncate to 16** contracts [T p.15].
- Unit sizes were recomputed weekly (Monday Unit sheet) [T p.15]. Whether later Units of one campaign re-use the first Unit's size is **not stated** — a decision to record.
- Small accounts lose diversification because truncation is coarse [T p.15].
- **Equity form (RECONSTRUCTED, from N-T):** for shares, dollars-per-point = 1, so `Shares = (Equity × 1 %) / N`, and the 2N stop makes initial risk ≈ 2 % of equity per Unit. N-T writes this as `Unit = AmountRisked / (M × N)` with M = stop multiplier = 2 and AmountRisked = 2 % — algebraically identical to Faith's form when M = 2, but it silently couples the sizing to the stop multiplier. The specification must pick one form and name the variables so that "1 % per N" and "2 % at the stop" cannot be confused (see §4.2).

### 2.4 Position limits (Units as risk)
| Level | Scope | Max Units |
|---|---|---|
| 1 | Single market | 4 |
| 2 | Closely correlated markets, one direction | 6 |
| 3 | Loosely correlated markets, one direction | 10 |
| 4 | Single direction (all long or all short) | 12 |
[T p.16]. Examples of close correlation: heating oil/crude, gold/silver, CHF/DEM, T-bill/Eurodollar. The Oct-1987 aftermath is given as the reason the caps exist [T p.16].
- *Stock adaptation (decision):* company → industry → sector → total-long maps naturally onto the four levels; the numbers do not. Candidates are discussed in §6.

### 2.5 Notional account and drawdown re-basing
- Turtles traded *notional* accounts re-based yearly by Dennis [T p.17].
- Reduce the notional account by **20 % each time equity falls 10 % of the original account**; continue trading at the reduced size until back at the yearly starting equity. Example: $1 M → $800 k after −$100 k → $640 k after a further −$80 k [T p.17].
- Faith explicitly says other schemes may be better; these are just the rules used [T p.17].

### 2.6 Entries
- Donchian channel breakouts. **System 1**: 20-day; **System 2**: 55-day. Traders allocated equity between the two at their discretion [T p.18].
- A breakout = price exceeding the high/low of the preceding N days *by a single tick*; taken **intraday**, not at the close; entered on the open if the market gapped through [T p.18–19].
- **System 1 filter:** skip the signal if the *previous* breakout in that market (taken or not, either direction) would have been a winner; a breakout is a loser if price moved 2N against it before a profitable 10-day exit. If a signal is skipped, enter at the **55-day failsafe** [T p.19].
- **System 2:** every 55-day breakout is taken [T p.19].
- **Adding Units:** add 1 Unit every **½N** measured from the *actual fill* of the previous Unit, up to the maximum; slippage on the first fill pushes later adds out accordingly; all four could be added in one day [T p.19–20]. Gold and Crude ladders are printed [T p.20].
- Consistency: most of a year's profit comes from two or three trades; skipped signals are the main cause of failure [T p.20]. Adding *slowly* is called out as a subtle rule change that can hurt [T p.31].

### 2.7 Stops
- Stops always exist; they were usually *not* placed with the broker (to hide size) but were watched and executed at the level [T p.21].
- **2N** below entry (long) / above (short) so that no trade risks more than 2 % [T p.22].
- When Units are added, earlier Units' stops are raised by ½N, so normally all stops sit at 2N below the most recent Unit; where a later Unit fills further away (gap/skid), stops differ per Unit. Crude ladder printed, including the gap case (4th Unit at 30.80 → its own stop at 28.40 while Units 1–3 stay at 27.70) [T p.22–23].
- **Whipsaw alternative:** ½N stops for ½ % risk, re-enter if price returns to the original entry; more losses, better profitability, and no stop-moving needed because four Units never exceed 2 % total [T p.23–24].

### 2.8 Exits
- **System 1:** 10-day low (long) / 10-day high (short), all Units. **System 2:** 20-day low / high, all Units [T p.26].
- Exits, like entries, were executed intraday when the level traded [T p.26].
- The exits are described as the hardest part of the system: giving back 20–100 % of open profit is normal [T p.26].

### 2.9 Tactics
- Prefer limit orders; wait out fast markets; on simultaneous signals **buy the strongest / sell the weakest** within a correlated group; only one Unit per market at a time (one contract month) [T p.27–29].
- Strength measures: visual; N advanced since breakout; (price − price 3 months ago) / N [T p.29]. The last two are automatable.
- Roll a few weeks before expiry and only if the new contract's own price action would have produced the position [T p.29–30]. (Not applicable to stocks.)

### 2.10 What Faith does *not* specify (decisions to record)
1. Whether later Units re-use the first Unit's share count or are re-sized from current equity and N.
2. Which N (current vs. entry-day) is used for add spacing and stop placement after the first Unit.
3. Execution price model for a daily-bar backtest (Faith traded intraday; a daily-bar system must choose close-of-signal-day vs. next open vs. stop-limit at the level).
4. How the yearly notional re-basing works for a self-managed account.
5. Any equity-specific rule (corporate actions, delistings, earnings, borrow).

---

## 3. The Sublime Trading methodology (step-by-step)

Sublime's lineage from the Turtles is explicit: the presenter credits *Way of the Turtle* as the origin of his approach [V 00:00:44–00:01:08]; Gbadamasi says his system "incorporates some elements of the Turtle Trading system by using breakouts" [30 p.5]. Sublime was founded in 2018 and a Luxembourg trend-following fund in 2023 [V 00:01:21].

The steps below are in the order Sublime performs them. Provenance tags follow each rule.

### 3.1 Universe and scan
1. **Scan ~20,000 assets** — US stocks, commodities, currencies (the presenter repeats "commodities" [V 00:03:07]; whether crypto/FX are in the daily scan is ambiguous) — with a professional scanner, producing ~150–200 daily candidates [V 00:03:07–00:03:28]. Earlier documents say 10,000+ → 50–300 (up to 500–1,000 in strong markets) [M p.54; 30 p.6]. **DISCLOSED** (numbers vary by date).
2. **Scanner criterion:** Donchian 20 — a **break and close** above the 20-day channel, not an intraday touch [V 00:47:53–00:48:12]. Then judge the break relative to last year's high and the all-time high [V 00:48:12]. **DISCLOSED**.
3. **Hard filters:** price ≥ $20 (below $20 "very volatile") [V 01:13:32]; US volume > 1 M shares, 500 k "at the very least"; UK 100 k [V 00:59:58–01:00:06]; 5–10 years of history [M p.54]; avoid "uber expensive" mega-caps, prefer cheap stocks setting ATHs [M p.54]. **DISCLOSED**.

### 3.2 Market regime (S&P 500)
4. Use the S&P 500 as the regime guide: bullish → buy; flat → stand aside; confirmed bear → shorts allowed [V 00:05:09–00:05:19; BP p.2]. The presenter has not shorted since 2008 [V 00:05:24]. **DISCLOSED**.
5. **Monthly regime = previous *calendar* year's high/low** (not trailing 12 months) [V 00:07:32–00:07:43]: above last year's high → bull; below last year's low → bear; between → sideways / "capital protection mode" [V 00:08:05–00:08:30, 00:10:01; KISS; M p.48]. **DISCLOSED, automatable as stated.**
6. **Weekly regime:** above the 200 SMA = long-term bullish; a sustained break below it is what distinguishes a bear market (2001, 2008) from a correction [V 00:21:12–00:22:35]. Above the 50 SMA = momentum; a break below the 50 SMA is the trigger to **rebalance** (cut weaker holdings, keep leaders) [V 00:24:41–00:25:06]. **DISCLOSED**.
7. **Daily regime:** above the 200, 50 and 20 SMAs; the 20 SMA acts as a "trampoline" for bull flags; the 50 SMA is the next support in slower trends [V 00:26:06–00:26:45, 00:44:56–00:45:03]. **DISCLOSED**.
8. **Alignment** = monthly, weekly, daily all bullish ("big sister / little sister") [V 00:06:46–00:07:08, 00:42:05; M p.51]. **DISCLOSED**.
9. **Never go straight to cash** on media fear; rebalance instead [V 00:06:14]. Sector rotation matters because the index is cap-weighted — a tech correction does not mean the whole market is correcting [V 00:10:40–00:11:22]. **DISCLOSED (qualitative)**; a sector-strength ranking is the **PROXY**.
10. **Seasonality** is used as a *risk-sizing* input, not a signal: February and September are historically weak, summer is flat, December often strong [V 00:08:55–00:13:01]. **DISCLOSED**; whether to encode it is a decision.

### 3.3 Key levels (support/resistance)
11. **Monthly pivots:** a high/low with **≥ 9 bars on each side** ("PIV9"); plotted for the life of the pivot [V 00:18:22–00:19:25]. **Weekly pivots: ≥ 4 bars each side** [V 00:29:10–00:30:12]. Daily levels are deliberately not plotted [V 00:30:34–00:30:43]. **DISCLOSED, automatable.**
12. Desired state: price above last year's high *and* above all monthly/weekly pivot levels; a strong resistance directly above is a reason for caution [V 00:20:36–00:20:52]. Round numbers (S&P 8,000; gold $5,000) are treated as resistance and a reason to tighten stops [V 00:20:46, 01:02:47]. **DISCLOSED**; round-number tightening is **EXCLUDED** (discretionary).

### 3.4 Trend-strength filter
13. Two Bollinger Bands on the **weekly and daily** charts, both **20-period SMA of closes**, one at **1 σ** and one at **2 σ** (verified on screen at 00:35:00: "BB 20 SMA close 1" / "BB 20 SMA close 2") [V 00:31:08–00:34:13]. Three zones: above +1σ = buy, between ±1σ = neutral, below −1σ = sell [V 00:34:13–00:34:27]. Bars are coloured by **closing price**: green / grey / red; dark green or dark red = close *outside* the ±2σ band, which flags over-extension and possible whipsaw [V 00:36:33–00:38:16]. A bull trend is "predominantly green with little bits of gray" [V 00:39:47]; alternating green/grey/red ("Italian flag") = flat [V 01:04:02]. **DISCLOSED — this resolves what earlier sources called "proprietary"; it is fully reconstructible.**
14. Donchian channels are for breakouts/entries; Bollinger Bands are for momentum; use both [V 00:58:39–00:58:59]. MACD/RSI/Stochastics are not used [V 00:59:11–00:59:39]. Volume is the only sub-chart indicator (breakouts on high volume) [V 00:59:52]. **DISCLOSED**.

### 3.5 Candidate quality and ranking
15. Prefer **smooth, linear trend histories** on the monthly chart (Visa 2012–2020 as the model; MATX and Merck as "volatile history" counter-examples); a choppy history is a reason to demote, though "personalities change" [V 00:50:12–00:50:58, 00:56:58–00:57:25, 01:14:06–01:14:12]. **PROXY** candidates: regression R², efficiency ratio, drawdown count/depth, ATR/price, gap frequency.
16. Prefer stocks **at all-time highs** ("nothing above price to force it down"); a stock below its ATH is a "no" when alternatives exist [V 00:48:16, 00:52:15–00:53:03]. **DISCLOSED, automatable.**
17. No fundamentals: a stock setting record highs after a long consolidation is assumed healthy [V 01:06:20–01:07:31]. **DISCLOSED**.
18. **Tier A / Tier B** watch-lists: Tier A = clean, aligned, ATH leaders; Tier B = meets criteria but weaker history; Tier B is considered only when Tier A is empty [V 01:14:41–01:15:07; N-S]. **PROXY** (the tiering criteria are the ranking features above).
19. "Best-performing stocks in the best-performing sector" [V 00:02:31–00:02:41]; sector leadership is inferred from money flow (financials, healthcare when tech corrected) [V 00:11:16, 00:27:03]. **PROXY** (sector momentum ranking).
20. Ask "what do I not see?" — look for reasons *not* to enter [V 01:22:08; M p.54]. **EXCLUDED** (discretion).

### 3.6 Entry
21. **4PS sequence:** Phase 1 proven history → Phase 2 consolidation base → Phase 3 breakout → Phase 4 confirmed trend [4PS p.2]. Phase 3 = **A** (first breakout from the base) → **B** (pullback that retests resistance as support) → **C** (second breakout, confirming Phase 4); the first position is taken at **C** [4PS-2 p.2–3]. **DISCLOSED**.
22. In the webinar the same sequence is stated as: consolidation between last year's high and low → break above last year's high → weekly alignment (above 200/50, filter green) → **daily bull flag** → **enter on the next breakout**, because the first breakout can be fake and trap capital in a year-long consolidation [V 00:45:49–00:47:32, 00:51:14]. **DISCLOSED**; the bull-flag geometry is **PROXY**.
23. Consolidation length: the DXY example treats 144 trading days as "well over the **55 trading day threshold**" [2B p.1]; the presenter repeats "the longer the consolidation, the bigger the breakout" [V 00:46:05, 01:07:07]. **DISCLOSED (one instance)** — treat 55 days as a candidate minimum-base length, not a proven rule.
24. Enter early: first 1–2 months of Phase 4; a stock ~5 months into a trend is considered late [4PS p.3]. **DISCLOSED**.
25. Order placement: **above the high of the qualifying breakout bar** (long) / below its low (short), computed by a tool from a "simple mathematical formula" [M p.55]. **DISCLOSED (level) / EXCLUDED (formula)**.
26. **Earnings:** no new positions in the two weeks before earnings; do not close, do not tighten, do not panic; reassess after [V 00:43:59–00:44:13]. Earnings gaps can *start* trends (NVDA, Eaton) [V 00:43:21–00:43:47]. **DISCLOSED, automatable with an earnings calendar.**

### 3.7 Risk and sizing
27. **Per-position risk:** 1–2 % [R; M p.55]; 2 % only in "optimal conditions", 1 % or less when conditions are close but not perfect [M p.55]. Webinar examples: 1 % when the market is "in full bloom", **0.5 % or 0.25 %** in summer / when indices are not aligned [V 00:28:01–00:28:13]; BNY compounding example uses 0.5 % or 1 % [V 01:20:45]. **DISCLOSED**.
28. **Stop stays wide; size shrinks.** Risk reduction is achieved by position size, not by tightening the stop [V 00:28:22–00:28:34]. **DISCLOSED**.
29. **Portfolio ceilings:** risk initiated per day 4–8 %; maximum aggregate risk allocation 10–20 %; lower end when the S&P is not printing ATHs, upper end when it is [R]. The e-book instead says "cap maximum exposure at 20 %" with the other 80 % as margin in spread-bet/CFD accounts [M p.55]. New setups wait on the watch-list when the cap is hit [R]. **DISCLOSED** (see conflict §4.5).
30. **Sizing:** shares such that entry-to-stop distance × shares ≤ chosen risk % of account [M p.56]. **RECONSTRUCTED** formula: `Shares = floor(Equity × risk% / (Entry − Stop))`.
31. **A stop on every position, non-negotiable** [R; M p.57]. **DISCLOSED**.

### 3.8 Stops and exits
32. **Initial stop ≈ 3 × ATR** (EURUSD example: 90 pips × 3 = 270), same formula for every asset class, deliberately wide to avoid "tree-shaking" [M p.55–56]. ATR period, price anchor and rounding are **not** given. **DISCLOSED (multiple) / EXCLUDED (exact formula)**.
33. **Trailing stop uses the same ATR formula** [M p.57]; recalculated by a tool as the trend develops. Anchor (highest close? highest high?) and update cadence **not** given. **RECONSTRUCTED** as a Chandelier-style 3×ATR trail; **EXPERIMENT**.
34. **Public simplified exit:** daily **Donchian-20 lower band** — exit on a break of the 4-week low [30 p.7–8; N-S]. **DISCLOSED**; this is the Turtle System-2 exit applied to a stock and is the natural **BASELINE** exit.
35. **Moving-average management:** a breach of the daily 50 SMA lasting one or two bars is a reason to tighten or exit; strong trends hold the 20 SMA [V 00:45:03–00:45:14]. **DISCLOSED**; **EXPERIMENT**.
36. **Discretionary tightening** at round numbers / major resistance (gold at $5,000, Jan 2026) [V 01:02:41–01:02:53] and human override of an automated sell signal when structure suggests holding [V 00:49:22–00:49:32]. **EXCLUDED**.
37. Rebalance / take profit typically after **12–18 months** [4PS p.3; KISS; N-S]; e-book says 12–24 months for stocks and commodities, 3–12 for currencies [M p.39]. Not a time-based exit rule. **DISCLOSED (range)**.

### 3.9 Compounding (pyramiding)
38. Add only to winners; never to losers [M p.56]. **DISCLOSED**.
39. Add the second position after the first has moved **1 ATR** in profit; the third after the second has moved 1 ATR; the earlier positions stay open (BNY example, 0.5–1 % per position) [V 01:20:45–01:21:04]. (The machine transcript renders "1 ATR" as "180R" at 01:20:47; the surrounding sentence makes the meaning unambiguous.) **DISCLOSED**.
40. Add **only when the first position has no remaining risk** (stop at or above entry), so that no more than 2 % is ever at risk on one asset [M p.56]. **DISCLOSED**.
41. Each addition is a **separately sized and stopped position** [M p.56]. **DISCLOSED**.
42. No maximum number of additions is stated; the e-book's illustration is ~4 assets × ~5 positions [M p.56–57]. **NOT DISCLOSED**.

### 3.10 Routine
43. Scan after the close (needs closing prices), 1–2 hours daily, at minimum once at the weekend [V 01:10:22–01:10:44]; other documents say 15–20 min/day [M p.54], 90 min/week [BP], or 1 hour + 10 min/day [30 p.8–9]. **DISCLOSED (inconsistent)** — irrelevant to automation except as a statement that decisions are made on **completed daily bars**.
44. Human-in-the-loop by design: technology for the scan, "expert eye" for selection and exit management; "do not outsource all your intelligence to technology" [V 00:48:43–00:49:32; M p.46]. **EXCLUDED** from the automated strategy, but the research system should produce the same charts/features for review.

### 3.11 What Sublime does not disclose (cannot be reconstructed)
- The exact entry-level formula ("above the high of the breakout bar" is the level; the offset is not given).
- The ATR lookback and the anchor/cadence of the trailing stop.
- The Position Sizing Calculator internals, the APL, the A.O.T column.
- A maximum number of positions per asset, per sector, or in total.
- Any correlation control.
- Any account-drawdown re-basing rule (Sublime's drawdown defence is prospective: smaller risk in weaker regimes, caps, rebalancing).
- A quantitative definition of "bull flag", "clean trend", "Tier A".
- Short-side rules beyond symmetry.

---

## 4. Conflict ledger

Each conflict lists the sources, the recommended resolution, and where the losing variant goes (baseline vs. experiment). Final decisions are made in grilling and recorded as ADRs.

| # | Topic | Sources in tension | Recommended resolution |
|---|---|---|---|
| 4.1 | **Add spacing** | Faith: ½N from actual fill [T p.19]. N-T: cites "one source ½N, another 1N". Sublime: 1 ATR [V 01:20:47]. QC prototype: 1N. | **BASELINE = ½N.** 1N/1ATR is an **EXPERIMENT** (Sublime variant). Never blend silently. |
| 4.2 | **Unit-risk terminology** | Faith: Unit sized so 1N = 1 % of equity; 2N stop ⇒ 2 % risk [T p.14, p.22]. N-T: "AmountRisked (2 %) / (M×N)". QC: `RISK_PER_TRADE = 0.02` divided by 2N distance. | Name two parameters: `unitVolatilityFraction` (1 % per N) and `stopMultiple` (2). Risk-at-stop is derived, never an input. Prevents the "each Unit risks a fresh 2 %" bug. |
| 4.3 | **N definition** | Faith: 20-day Wilder EMA of True Range, SMA-seeded [T p.13]. N-T: one line says "20 Day EMA on price" (error), later correct. QC: `MovingAverageType.Simple`. | **Faith's formula, exactly.** Golden test from the Heating Oil table [T p.14–15]. Fix the Notion line. |
| 4.4 | **Drawdown rule** | Faith: notional account −20 % per 10 % loss *of the original*, until back at yearly start [T p.17]. N-T: also records Covel's "cut unit risk 2 % → 1.6 % → 1.28 %" form and prefers Faith's. QC: trailing high-water-mark lookup with reset at every new peak. | **BASELINE = Faith's notional-account form** with an explicit annual re-basing date. Covel's form is arithmetically the same only if the risk % is applied to the *original* equity; treat as a naming alternative, not a variant. High-water-mark reset is a different strategy — **EXPERIMENT** at most. |
| 4.5 | **Sublime risk ceilings** | M: ≤2 %/position, max exposure 20 % [M p.55]. R: 1–2 % / 4–8 % daily / 10–20 % aggregate, regime-scaled. V: 0.25–1 %. | R is the operative rule set (later, more specific, explicitly "the rules we follow"). "Max exposure 20 %" in M reads as *risk allocation*, not notional value — the 80 % margin remark is spread-bet specific. Encode: per-position ∈ {0.25, 0.5, 1, 2} % by regime state; daily-initiated ≤ 4–8 %; aggregate open risk ≤ 10–20 %. |
| 4.6 | **Sublime exit** | M: ATR-formula trailing stop [M p.57]. 30/N-S: Donchian-20 low. V: 50 SMA breach 1–2 bars → tighten/exit. | Three declared **EXPERIMENT** variants: (a) Donchian-20 (= Turtle S2), (b) 3×ATR chandelier trail, (c) 50-SMA breach. (a) is the baseline. |
| 4.7 | **Weekly MAs** | M/MA: 20/50/200 on the weekly [M p.49]. V: weekly uses 200 and 50; the 20 is a *daily* tool [V 00:24:41, 00:26:06]. KISS: 200 only. | Implement weekly 200 + 50 (V is the most recent and most specific). Weekly 20 as an optional feature. Alignment gate = KISS minimal rule (last-year high, weekly 200, daily 200) with the extra MAs as ranking features. |
| 4.8 | **Consolidation threshold** | 2B: 55 trading days (single example). V: "longer is better", no number. | 55 days as the *default* minimum base length, parameter under sensitivity test. |
| 4.9 | **Holding period** | 12–18 months (4PS, KISS, V) vs 12–24 (M p.39). | Not an exit rule; used only as a research expectation (median hold ≈ 1 yr) when judging results. |
| 4.10 | **Scan scale / routine** | 20k→150–200 (V) vs 10k→50–300 (M). 1–2 h/day (V) vs 15–20 min (M) vs 90 min/wk (BP). | Irrelevant to rules. Record only: decisions on completed daily bars; weekly full rescan. |
| 4.11 | **Execution timing** | Faith: intraday at the tick [T p.18]. Sublime: close-based scans, entry order above the breakout bar's high. Daily-bar backtest must pick. | Baseline: signal on the completed bar, execute at next open (conservative, reproducible). Turtle-faithful intraday stop-limit at the channel level as an **EXPERIMENT** once intraday data exists. |
| 4.12 | **Entry timing (the big one)** | Faith: buy the *first* breakout immediately. Sublime: skip the first, enter on the confirmed second (Phase C). | Not a conflict to resolve — it *is* the primary hybrid hypothesis. Baseline = Turtle immediate; Phase A/B/C state machine = **EXPERIMENT** (§6). |

---

## 5. Comparison

| Dimension | Turtle (Faith) | Sublime (V, M, R, 4PS, 30) |
|---|---|---|
| Universe | ~21 liquid US futures; grains/meats excluded | Stocks first (US, also UK), plus commodities/FX; price ≥ $20, volume > 1 M (≥ 500 k), 5–10 yr history, prefer ATHs, avoid mega-caps |
| Direction | Symmetric long/short | Long-biased; shorts only in a confirmed bear regime (none since 2008) |
| Regime filter | None — diversification + caps instead | S&P 500: last-year H/L (monthly), 200/50 SMA (weekly), 200/50/20 SMA (daily), trend filter green |
| Sector | Correlation groups (caps) | "Best stocks in the best sector"; rotation awareness; no numeric cap |
| Trend definition | Price channel only | Multi-timeframe alignment + 2-band Bollinger colour filter + pivot levels |
| Entry | First breakout of 20/55-day channel, intraday, one tick | Second breakout (Phase C) after break of last-year high, pullback, bull flag; order above breakout bar's high |
| Entry filter | S1 skip-after-winner; S2 none | Alignment, ATH, clean history, volume, sector, no earnings within 2 weeks, Tier A first |
| Volatility unit | N = Wilder-20 ATR | ATR (period undisclosed) |
| Position size | 1 Unit: 1N = 1 % equity | Risk % of equity at the stop: 0.25–2 % by regime |
| Initial stop | 2N (or ½N whipsaw) | ≈ 3×ATR, wide |
| Adds | Every ½N, to 4 Units, stops trail up ½N | Every 1 ATR, only once prior position is risk-free; each add separately stopped; no stated max |
| Exit (winner) | 10-/20-day opposite channel, all Units | ATR trail (proprietary) / Donchian-20 low (public) / 50-SMA breach (webinar) |
| Portfolio caps | 4 / 6 / 10 / 12 Units | 1–2 % pos, 4–8 % day, 10–20 % aggregate; ≤ 2 % per asset |
| Drawdown | Notional −20 % per −10 %, yearly reset | None disclosed; size down in weak regimes; rebalance |
| Time horizon | Weeks–months, exit-determined | Typically 12–18 months; rebalance |
| Discretion | None (consistency is the thesis) | Deliberate human-in-the-loop for selection and exit management |
| Events | None | Earnings blackout for new entries; seasonality as sizing input |
| Execution | Intraday, limit orders preferred | Decisions on completed daily bars |

### The consequential differences
1. **First vs. second breakout** — the single largest signal-level difference and the core hybrid hypothesis. Sublime trades fewer false starts for later entries; Turtles accept many false starts to never miss the big one [T p.20, p.26].
2. **Top-down regime and sector gating vs. bottom-up diversification.** Turtles could be long bonds and short energy simultaneously; Sublime requires the index, sector, and stock to agree.
3. **Completeness.** Faith's rules can be executed from the document alone. Sublime's cannot: stop anchor, ATR period, entry offset, max adds, and correlation limits are undisclosed, and two of its steps (chart quality, exit tightening) are explicitly human.
4. **Risk arithmetic.** Turtles cap *Units* (volatility-normalised exposure); Sublime caps *risk-at-stop* percentages. Both are implementable; only the Turtle scheme addresses correlation.
5. **Stock-specific hazards** (earnings gaps, sector crowding, delisting) are addressed by Sublime only partially (earnings blackout) and by Faith not at all — the specification must add them.

---

## 6. Hybrid candidates (research programme)

The right experimental structure is a **Turtle risk engine with Sublime-inspired admission and ranking**, tested as ablations. The models below refine the ChatGPT progression (Model A–E) with the corrections from §4.

**Model A — Turtle stock control (BASELINE).** Long-only liquid US stocks, point-in-time universe; System 2 (prior-bar 55-day entry, 20-day exit); Wilder-20 N; Unit = 1 % of *notional* equity per N; 2N stop; ½N adds to 4 Units with Faith's stop ladder; Faith drawdown re-basing; per-stock / industry / sector / total caps (numbers TBD); buy-strength ranking on simultaneous signals using (price − price 3 months ago)/N [T p.29]; signal on completed bar, fill at next open; no earnings filter. Start with reduced Unit risk (0.25–0.5 % per N) rather than Faith's 1 %.

**Model B — Regime gate.** A: plus long entries only when S&P > last calendar year's high (Sublime monthly rule) and > weekly 200 SMA. Variant B′: risk scaled by regime state (full / half / quarter) instead of a hard gate — this is what the presenter actually does [V 00:28:01].

**Model C — Sector strength.** B: plus entries only in the top-k sectors by 6- or 12-month volatility-adjusted momentum; within sector, rank by normalised momentum. PROXY for "best stocks in the best sector".

**Model D — Multi-timeframe alignment + trend filter.** C: plus stock > last-year high, > weekly 200 & 50 SMA, > daily 200/50/20 SMA, and daily & weekly Bollinger colour ∈ {green, dark-green} at signal time (§3.4 makes this fully reconstructible).

**Model E — Second-breakout entry (4PS).** D: replace the immediate breakout with a state machine:
```
BASE          : ≥ 55 sessions without a new 55-day high (parameter)
FIRST_BREAKOUT: close > max(prior 55-day high, last-year high)        (Phase A)
RETEST        : low within k₁·ATR of the breakout level, no close
                more than k₂·ATR below it, within m sessions          (Phase B)
SECOND_BREAKOUT: close > post-breakout reaction high → ENTER          (Phase C)
CANCEL        : close < breakout level − k₂·ATR, or m exceeded
```
k₁, k₂, m are sensitivity-tested; the *hypothesis* is Sublime's, the *thresholds* are ours.

**Model F — Risk/exit variants (2×2×2 matrix on the best of A–E).** Stops 2N vs 3×ATR; adds ½N vs 1 ATR; adds unconditional vs only-when-risk-free; exits Donchian-20 vs 3×ATR trail vs 50-SMA breach. Predeclare; keep every result.

**Not automated (EXCLUDED):** visual "cleanness" beyond the R²/efficiency proxies; Tier A/B by impression; round-number stop tightening; overriding automated exits; "what do I not see?".

**Selection rule:** a filter earns its place only if it improves out-of-sample return per unit of drawdown *and* survives ±30 % parameter perturbation *and* does not reduce independent trades below a floor. A filter that raises CAGR but concentrates sectors or trades is making the system more fragile.

---

## 7. Stock-adaptation questions the specification must answer

Neither source answers these; they go to grilling.
1. Universe: price floor ($20 per V; $10 per GPT), dollar-volume floor, history floor (5 yr), exchange set, exclusion of ADRs/ETFs, point-in-time membership source (→ data-provider decision).
2. Integer shares and cash constraint: skip a Unit that would exceed available cash? Never borrow (unleveraged)?
3. Corporate actions: split-adjusted channel and N; dividends in returns but not in channels; delisting = forced exit at last price; symbol changes.
4. Earnings: none (Model A) vs. Sublime blackout (Model D+) — and where the calendar comes from.
5. Correlation caps: company (4 Units?), industry, sector, total long; whether to use rolling-return clusters instead of GICS.
6. Notional account: what "yearly re-basing" means for a self-managed account; deposits/withdrawals.
7. Execution model for daily bars: next-open fills with slippage vs. stop-limit at the level.
8. Gap-through-stop: fill at open, model slippage.
9. Shorts: excluded from v1 by decision; the code should not carry dead short paths.

---

## 8. Review of the ChatGPT/Codex analysis

**Overall:** the video summary was accurate. Every timestamp cluster it cited checks out against the new transcript within a minute or two, and the substantive claims (Turtle credit, 20k scan, Donchian-20 break-and-close, previous-calendar-year levels, 9/4-bar pivots, weekly 200/50, daily 200/50/20, Bollinger 1σ/2σ filter with light/dark shading, earnings rule, 0.25–1 % risk by conditions, 50-SMA trailing, 1-ATR adds, Tier A/B, 1–2 h routine) are all confirmed. Its folder review correctly added the 3×ATR stop, the 1–2/4–8/10–20 % caps, the risk-free-before-add rule, the 4PS A/B/C sequence, the 55-day threshold, and the entry above the breakout bar. Its recommendation (Turtle control first, Sublime as tested filters, no Kubernetes yet) stands.

**Missed or under-stated:**
1. **$20 minimum price** [V 01:13:32] — not mentioned.
2. **Volume floors** with the 500 k minimum and the UK 100 k figure [V 00:59:58] — only the 1 M figure was reported (from M).
3. **Bollinger parameters** — GPT said "appears to be derived from two Bollinger Bands, SD 1 and SD 2"; the transcript and on-screen settings make it definite: 20-period SMA of closes, colour by closing price, dark shade = close outside ±2σ. The filter is not proprietary in any meaningful sense and should be tagged DISCLOSED, not "unknown formula".
4. **Seasonality as a sizing input** [V 00:08:55–00:13:01] — omitted.
5. **"Never go straight to cash"** and the cap-weighting argument for sector rotation [V 00:06:14, 00:10:40] — omitted; they matter for the regime-gate design (B′ scaling rather than a hard gate).
6. **Daily levels deliberately not plotted** [V 00:30:34] — it reported pivots as if all three timeframes had them.
7. **Short since 2008 = presenter's practice**, not a rule — GPT got this right; noting for completeness.
8. **Faith citations** — GPT cited the Turtle document without page numbers; this document adds them so the golden scenarios can be traced.
9. **"Equal Unit size within a campaign"** — GPT listed it as a Turtle rule; it is not in Faith (§2.10 item 1).
10. **Notion "EMA on price" error** — GPT caught it. The Notion **dual ½N/1N citation** GPT also caught. Both still need fixing in Notion.
11. **Drawdown** — GPT described the QC prototype's reset-at-new-peak as "defensible but different"; more precisely it is a *different strategy* and must not be the baseline (§4.4).

**Where GPT's later thread went beyond the sources:** the "Rules-based Sublime control" list in `Overall_Plan.md` §4 mixes disclosed rules with reconstructions (e.g. "approximately 3ATR initial-stop experiment", "each addition tracked as a separately managed position") without tags. This document supplies the tags.

---

## 9. What this document feeds

- `CONTEXT.md` — glossary terms: N, Unit, notional account, campaign, ladder, breakout bar, Phase A/B/C, base, retest, alignment, regime state, trend-filter colour, pivot (PIV9/PIV4), risk-free position, compound, APL (external), Tier A/B.
- ADRs: Turtle baseline parameters (§4.1–4.4, 4.11); Sublime rule classification (§3 tags); backtest conventions (§4.11, §7.7–7.8); long-only scope (§7.9); data provider (§7.1).
- Golden scenarios: Heating Oil N table [T p.14–15]; Gold and Crude add ladders [T p.20]; Crude stop ladders incl. gap case [T p.22–23]; drawdown $1 M → $800 k → $640 k [T p.17]; Sublime EURUSD 3×ATR [M p.55]; sizing example (RECONSTRUCTED) $100 k, 1 %, entry 50, stop 44 → 166 shares.
