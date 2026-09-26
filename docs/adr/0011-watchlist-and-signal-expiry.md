# ADR 0011: The Watchlist is ranked in every configuration, filtered only by caps in the Baseline, and a Signal never outlives its bar

- Status: Accepted
- Date: 2026-09-08

## Context

On a broad equity universe, Signals are not rare: Sublime's own scanner reports 150–200 candidates a day from 20,000 instruments [V 00:03:22], and 50–300 normally, up to 1,000 in a trending market [M p.54]. The Baseline may hold 12 Units in total. Capacity, not scarcity, is the binding constraint, so the ranking of Setups is load-bearing nearly every day — and it is tempting to let a quality judgement decide which Signals to take. Faith is explicit that skipping valid signals was the behaviour that separated the Turtles who failed from those who did not [T p.20].

Separately, a Signal that could not be acted on (a cap or the cash rule bound) raises the question of whether it is still live the next day.

## Decision

1. **Every Eligible instrument outside a Campaign is a Setup, evaluated exactly once per completed bar.** There is no separate "review the Watchlist, then rescan" pass: a Setup that was Tier B yesterday is re-evaluated today alongside every other Setup, against today's bar. One pass, one answer per instrument per day.
2. **The Watchlist — all Tier B and Tier A Setups, ranked by Strength — exists in every configuration** and is a first-class observable: it is the pre-image of tomorrow's Signals and the basis for reconciling why a Signal did or did not become a Campaign.
3. **In the Baseline, the only thing that stops a Tier A Setup from becoming a Campaign is a cap or the cash rule.** Grade plays no part. In the Sublime Variant, Grade is an additional gate — "Grade B only when no Grade A Setup is in Tier A" — and that difference is a declared ablation.
4. **A Signal belongs to one bar and expires with it.** A Tier A Setup that was not entered is not carried forward; it must re-qualify on a later bar. The operator's rule, verbatim: *never enter a Tier A stock without first checking that it still belongs in Tier A.* A genuinely trending stock will make a new 55-bar high and re-signal on its own. Persistent Signals are a declared Variant.

## Consequences

- The reducer holds no "pending signal" memory in the Baseline; Setup state for the Baseline is a pure function of the bar and the frozen configuration. The Sublime Variant's 4PS phases are the one place Setup state carries memory, and those transitions are journaled.
- A future reader who wants to "fix" the Baseline by adding a quality filter, or by carrying a good Signal forward a day, should find this record first.
- Scanning cost is not a design input: the per-bar evaluation across the whole universe is a few arithmetic operations per instrument on rolling windows and is dominated by data loading, which the adapter owns.

## Amendment: a fill-chained Add has one additional Session (2026-09-24)

Decided by Richard on 2026-09-24, option B in issue #211, with the
clarification that a live adapter must relay fills intraday. This is a
declared execution-lifetime change, not a change to a DISCLOSED Turtle
ladder or Signal rule.

An Add proposed in reply to an entry or Add fill remains attributed to the
last completed bar that covered its rung. Unlike an ordinary session-close
Add, it survives the next completed bar for that instrument and expires on
the second subsequent bar if still unfilled. Fills must arrive before the
expiring bar; their timestamp is still checked against the first bar received
after the fill. Entries and ordinary Adds retain their one-bar lifetime.
A Session means an actual trading observation, never a calendar-day offset.

The Add payload, schema 3 under ADR 0015, states `valid_for_sessions`: **1**
for an ordinary Add, **2** for a fill-chained Add, measured from its
`period_end`. This explicit window lets consumers validate placement against
observed Sessions without implementing strategy or forecasting a trading
calendar. The adapter may place the chain during the first subsequent
Session's reply, so it can execute in the second. It must reject placement
older than that window. No entry receives this extension.

A standing Add is not replaced or reserved a second time at the intervening
session close. Exit evaluation still runs first (ADR 0010); the extension
never suppresses an exit, and an exit ends the extension: if the intervening
bar proposes an exit, the standing Add expires with that bar, because a bar
that would both Add and exit results in the exit only. Nor does any Add
outlive its Campaign: a stop fill that closes the Campaign, in part or in
full, cancels a pending Add, ordinary or fill-chained, in that same
transition (reason `superseded-by-stop`). It is not left for a later bar,
because a daily adapter reports the next Session's fills before that bar and
the Add could otherwise fill into a Campaign already closed. Existing fill
and cancellation paths remain in force. Its ADR 0020 cash and cap hold
remains until fill, cancellation, expiry (at the second bar, or at the
intervening bar if it proposes an exit), or end of stream; snapshots never
release it.

RulesVersion changes from 1.10.0 to 1.11.0 (ADR 0016). The reference backtest
still fills covered chained rungs intrabar (ADR 0005), so this lifetime
extension must not change its trading decisions. Schema and version
propagation are separately accounted for in the evidence.

## Amendment: Watchlist emission timing and unrankable Setups (Accepted, 2026-09-26)

Decision 2 declares the Watchlist a first-class observable, ranked by
Strength, but does not say when it is published or what happens to a Setup
this Session cannot rank at all. Richard's owner decision on 2026-09-26
accepts the publication timing and omission of unrankable Setups, and
requires an empty Watchlist when no Setup qualifies. This supersedes the
2026-09-25 proposal to omit publication in that case.

1. **The Watchlist is published once per Session, at the session-close pass,
   immediately after ADR 0009's universe evaluation and before that
   Session's Adds and entries are decided.** Ranking needs Strength, which
   reads a completed bar's own close and N (ADR 0010, as amended
   2026-09-25) and — for the dollar-volume tie-break — the same shared
   ranking rankSignals already uses (session.go: rankSignal). Both are only
   available once the Session's bars have all been evaluated, so "every bar"
   in #35's acceptance criteria is read as "every Session" (CONTEXT.md:
   "Session" already stands in for "day" throughout this project's
   vocabulary), matching where ADR 0021 decides everything else that needs
   more than one instrument's own bar.
2. **A Setup this Session cannot rank at all — fewer than
   `indicator.StrengthLookbackBars+1` split-adjusted closes or fewer than
   `indicator.DollarVolumeWindow` raw closes/volumes — is left off the
   Watchlist entirely, never ranked last.** This extends ADR 0010's own rule
   for an unrankable Signal (declined rather than ranked last, since "an
   incomparable instrument has no place in a total order") to the Watchlist:
   an entry the Watchlist cannot rank is not observable in a way that is
   consistent with everything ranked beside it, so it is omitted rather than
   given an arbitrary position or a placeholder Strength.
3. **A Session with no rankable Tier A or Tier B Setup publishes an empty
   Watchlist.** The empty Entries list explicitly records that the Session
   was evaluated and nothing qualified, including warm-up Sessions and
   Sessions whose only Setups cannot be ranked. It distinguishes that
   outcome from a Session for which no Watchlist decision was recorded.
   `WatchlistPublishedPayload.Validate` therefore accepts empty Entries
   while retaining all per-entry rules and non-increasing Strength order.

Both (2) and (3) change nothing about which Signals are funded (ADR 0010's
own Item 2, already resolved): they only
decide what the Watchlist itself — a purely observational decision — reports,
never a proposal, a decline, or a cap check. RulesVersion moves from 1.16.0 to
1.17.0 for the new decision type alone; no numeric rule constant, ladder, or
cap changes.
