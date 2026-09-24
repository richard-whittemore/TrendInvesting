# ADR 0021: A Session closes before its Adds and entries are decided

- Status: Proposed
- Date: 2026-09-24
- Relates to: ADR 0005, ADR 0008, ADR 0010, ADR 0011, ADR 0015, ADR 0016, ADR 0020

## Context

ADR 0010 orders a trading day's actions across instruments: Protective Stops and Exit-Channel exits, then Adds, then new entries. It ranks simultaneous Signals by Strength, `(close − close 63 bars ago) / N`, highest first [T p.29], with ties broken by 20-day median dollar volume and then by symbol. ADR 0011 requires every Eligible instrument to be evaluated exactly once per completed bar. The day's result must not depend on the order in which instruments arrive.

The reducer cannot do any of that today. Each `market.bar.completed` is decided the moment it arrives (`internal/strategy/reducer.go`, `applyCompletedBar`). The instrument's indicators advance, its Campaign is evaluated, and a Tier A Signal becomes a trade proposal, or a Campaign an Add proposal, before the reducer has seen any other instrument's bar for that day. So:

- An entry for the first instrument in the stream can be emitted before an exit for the last. The ADR 0010 order holds within one instrument (`evaluateCampaign` runs before `evaluateAdd`), but not across instruments.
- Simultaneous Signals cannot be ranked, because they are never simultaneous: each is sized before the next bar is read.
- Two runs that deliver one day's bars in different orders produce different decision streams.

Nothing in the input stream says when a day is complete. Bars carry their own `period_end`, but the reducer cannot tell "the last bar of this session" from "a bar followed by others that have not arrived yet". Ordering needs a boundary that the input states.

## Decision

### 1. `market.session.closed` is a new input

A **Session** is the set of completed bars that share one `period_end`. The producer ends a Session with a new input event:

- Type `market.session.closed`, payload schema version **1**.
- Payload: `period_end`, the Session's shared bar period end; and `instrument_ids`, every instrument whose completed bar for that `period_end` precedes this event in the stream. The list is sorted ascending, contains no duplicates, and is not empty.
- Its envelope `event_time` is the `period_end`, the same way a bar publisher records a bar's own period end.
- It is sent after the Session's last bar and before any input stamped after that Session: the next Session's bars, and the `account.snapshot` that states this Session's close.

The reducer compares the named set with the bars it actually accepted for that `period_end`, and it fails closed if they differ in any way: a bar it received that the event does not name, a named instrument whose bar it did not receive, or a `period_end` that is not the open Session's. A close with no Session open also fails closed.

Sessions follow one another strictly:

- A bar whose `period_end` differs from the open Session's fails closed, so Sessions cannot interleave.
- A bar whose `period_end` is not after the last closed Session's fails closed, even for an instrument that has no bar in that Session, so a decided day is never reopened. This replaces per-instrument chronology as the only ordering across instruments: before this ADR, a second instrument's earlier-dated bar was accepted.
- A second bar for one instrument in one Session fails closed. For a live instrument, per-instrument bar chronology already rejects it. For a delisted instrument, the reducer rejects it by the open Session's record of the bars it has received.
- `replay.run.completed` while a Session is open fails closed, because the Session's Signals and Adds would otherwise end with no proposal, decline or expiry.
- A corporate action for an instrument whose bar the open Session already holds fails closed. That instrument's Add or entry is still undecided, so a fact about its listing cannot be ordered against that decision. A producer states such a fact between Sessions, which is what `cmd/backtest` does.

A bar for a delisted instrument decides nothing (ADR 0009). It is still a bar the producer sent, so the Session names it, and the reducer counts it as received.

The payload is a statement of completeness, not a count. Naming the set lets the reducer find which instrument is missing or extra, and lets a reader see from the journal alone which instruments a Session covered.

### 2. At bar time: evaluate, but propose no entry and no Add

As each bar arrives, the reducer does everything that concerns that instrument alone:

- It advances N, the Entry Channel and the Exit Channel, with the evaluate-then-advance discipline unchanged (CONTEXT.md, "Completed bar").
- It expires the previous bar's unfilled proposals (ADR 0011).
- For an open Campaign, it evaluates the Protective Stop, the Stop Ladder and the Exit Channel, and emits any exit proposal and the resulting Exit Order changes (`exit_order.go`).
- For a Setup, it evaluates the Setup, classifies its Tier and emits `strategy.setup.evaluated`, and `strategy.signal` when the Tier is A.

It emits **no trade proposal and no Add proposal**. It records on that instrument's state that the instrument has an Add opportunity or a Signal waiting for the Session to close.

Exits stay at bar time because their only input is the instrument's own Campaign, and nothing across instruments competes for an exit. All of a Session's exit decisions are therefore emitted before its session-close pass. That is ADR 0010's first stage.

### 3. At `market.session.closed`: one pass, in ADR 0010's order

The session-close pass runs once per Session, in a single transaction:

1. **Adds**, for every held Campaign that has an Add opportunity from this Session's bar and did not propose an exit on it (ADR 0010's exit precedence, unchanged). They are taken in ascending instrument order. Adds are not ranked, because ADR 0010 ranks Signals and an Add is not one.
2. **Entries**, for every Tier A Signal of this Session, in the order a named ranking seam, `rankSignals`, returns. ADR 0010 ranks them by descending Strength, with ties broken by 20-day median dollar volume (descending), then by instrument ID (ascending). **The seam is stubbed pending #34**: neither Strength nor median dollar volume is computed yet (see "Open"), so `rankSignals` applies only the last tie-break, ascending instrument ID. That order is total and does not depend on arrival order, and it stays in place when #34 adds the two leading keys. Each entry is sized with `sizeUnit`, exactly as it is today.

Before either stage, the pass re-reads each instrument's state. A fill between the bar and the close, which is possible only from a live producer, may have closed a Campaign or put it under an exit it must now yield to. Such a Campaign has no Add to decide.

Every Add and entry is checked against the Unit caps and cash that the repository implements at the time. This ADR adds no cap and changes no cash rule. What it fixes is the order in which those checks are made, which is the order in which a check that consumes a shared budget has to be made. The previous-close basis and cap headroom of ADR 0010, and the per-Session running debit of ADR 0020, attach at exactly this point. Stage 1 runs before stage 2, so any such budget is spent by Adds before entries, as ADR 0010 requires.

Proposals are stamped with the Session's `period_end`, and their causation is the `market.session.closed` envelope. They are attributed to the Session, not to any one bar. Proposal IDs keep their instrument-and-period-end form, so the identity of the thing proposed does not change.

### 4. Every Eligible instrument is evaluated exactly once per Session

A bar is evaluated once, at arrival. The session-close pass reads only what was recorded then. It never re-evaluates a Setup or a Campaign, so there is one answer per instrument per Session (ADR 0011, rule 1). A duplicate bar fails closed, and so does a `market.session.closed` that names an instrument twice.

### 5. Order-independence

Everything the session-close pass emits is a function of the set of the Session's bars, not of the order they arrived in. Adds are ordered by instrument ID and entries by a total order. The per-bar emissions (Setup evaluations, Signals and exits) do follow arrival order, because each is caused by its own bar envelope. That is the order of the input stream, and replay reproduces it exactly. The claim tested is therefore:

> Two streams that differ only in the order of one Session's bars produce the same session-close emissions, and the same per-instrument decisions.

A byte-identical decision stream across reordered inputs is not claimed, because each decision's hash chain includes the sequence of its input. The shuffled-input test compares decisions with their stream position and causation stripped. The decisions that matter to capital, which are the proposals, their order and their contents, are compared byte for byte.

### 6. Producers

- **`cmd/backtest`** groups its bars by `period_end`, in ascending order, with each Session's bars in fixture order. Before a Session opens, it delivers every corporate action due before any of that Session's bars. It then calls `fills.RunSession`, which delivers every bar with the existing open-instant and bar steps, then `market.session.closed`, which it builds from the bars so that the close always names exactly what was delivered, and only then runs the intrabar fixpoint for each instrument, in ascending instrument order. Entries and Adds therefore still rest and fill inside the Session's own bar (ADR 0005), after they have been proposed. `fills.RunBar` is a Session of one bar. With one instrument, a run is one bar followed by one session close, repeated. `fills.Deliver` refuses a session close, just as it refuses a bar, because only `RunSession` runs the fixpoints that follow it.
- **The LEAN adapter** sends `market.session.closed` after each slice's bars and before that slice's `account.snapshot`. It uses the same publisher, reply-identity checks and sequence rules that ADR 0020's producer amendment set out for snapshots. The engine's configuration is at sequence 1, then bar 2, session close 3, snapshot 4, then the next bar at 5.

The snapshot must come after the session close. A snapshot stamped at this Session's `period_end` is later than the Session's previous close, so `cashAtPreviousClose` would refuse it. If it arrived before the session close, it would replace the only eligible figure and leave the session-close pass with no cash basis at all.

### 7. What stays within one instrument

A proposal emitted by a fill, not by the session-close pass, follows its fill. That covers the next Add rung after an Add fill, and Unit 2's rung after an opening fill (`campaign.go`, `evaluateAdd` call sites 2 and 3). ADR 0010 orders **decisions made from a Session's bars**. ADR 0020 already records that it does not order **executions**, and a rung that depends on an earlier rung's actual fill price is a consequence of an execution. These proposals keep their present behaviour, their per-instrument cap check, and the cash check in force when they are made.

### Amendment: `cmd/backtest` states each Session's close (2026-09-24)

§6's `cmd/backtest` producer gains the account snapshot a LEAN run already
sends, stated by the simulated account (ADR 0020's RulesVersion 1.8.0
implementation note). `fills.RunSession` now delivers a Session as:

1. every bar's open-instant pass, in the order the bars are given;
2. the previous Session's `account.snapshot`, as of that Session's period end;
3. every bar;
4. `market.session.closed`;
5. each instrument's intrabar fixpoint, in ascending instrument order.

After the last Session, `fills.StateLastClose` delivers its snapshot, before
any remaining corporate action and `replay.run.completed`. Corporate actions
due before a Session still precede it, and so precede the previous Session's
snapshot, which was taken at that close and is unaffected by them. There is
no opening snapshot: the first Session cannot size a Unit, because N and the
channels are computed from preceding bars, so the snapshot delivered in the
second Session arrives before any sizing is possible.

This is the LEAN adapter's order: a slice's fills, then the previous close's
snapshot, then its bars and close. Each position has a reason:

- **After the Session's open-instant fills.** A fill can propose the next
  rung at once (§7), and that proposal is checked against the cash known at
  its signalling bar's previous close, one Session earlier than this
  snapshot. Arriving first, the snapshot would replace the only eligible
  figure, and `cashAtPreviousClose` would stop the run.
- **Before the bars and the close.** The close decides the Session's Adds and
  entries against this snapshot, its previous-close basis (ADR 0010).
- **Taken at the previous close, delivered now.** By delivery the
  open-instant fills have moved the account; they happened after the close,
  are stamped after the snapshot's as-of, and stay debited on top of it (ADR
  0020).

Step 1 used to interleave with step 3: each bar's open-instant pass, then
that bar. The passes now all come first so the snapshot can follow every one
of them. A single-instrument Session is delivered exactly as before, apart
from the snapshot.

## Alternatives rejected

- **One batched input carrying a whole Session's bars.** It would make completeness trivial, because the batch is the Session. But it replaces the bar input the whole codebase is built around, and its journal records, replay tooling, fill simulator and adapter protocol. The journal would then hold one very large input per day, in place of one small input per instrument. And the adapter would have to buffer a whole universe's slice before sending anything. A terminating event keeps every existing bar record as it is, and adds one small record per Session.
- **Keep evaluating at bar time, and have the producer sort the stream.** The producer would send exits first and then ranked Signals. But ranking needs each Signal's Strength, and a Signal is known only after its bar has been evaluated, so the producer would have to reimplement the strategy. It also moves a strategy rule, ADR 0010's order, out of the reducer and into a component that is not replayed.
- **Infer the end of a Session from the next Session's first bar.** No input would change. But the last Session of a run, and any Session followed by a gap, would stay undecided until something later arrived. And a missing bar could not be told apart from an instrument that simply did not trade.
- **A count of bars in place of the set of instrument IDs.** It is smaller. But when the check fails, the reducer cannot say which instrument is wrong, and a reader of the journal cannot see what the Session covered.

## Consequences

- **The wire contract gains an input type.** `market.session.closed` schema 1 joins the reducer's recognised inputs, and an unknown schema fails closed (ADR 0015). Every producer must send it: `cmd/backtest`, the LEAN adapter, and any test or tool that drives the reducer with bars. A stream with no session closes proposes no entry and no Add, and nothing fails until the reducer sees a bar from a later Session with the earlier one still open. That is the fail-closed point: a stream that has not been migrated stops at its second Session. It does not quietly trade nothing.
- **The decision stream changes, so RulesVersion moves 1.5.0 → 1.6.0.** Proposals move from being caused by a bar to being caused by a session close, and take the close's recorded-at instant, and the journal gains one input record per Session. Under ADR 0016 that is a rules change, since an older build's journal no longer replays byte for byte. It needs a new decision corpus and a new rule-surface fingerprint row; the fingerprint itself is unchanged, since no Rule, ADR or numeric constant changes. The golden diffs contain only the new inputs, the re-attributed proposals, and their hash-chain and sequence consequences.
- **A bar and its sizing are no longer one transaction.** A bar's Setup evaluation and Signal commit when the bar is accepted. If sizing then fails closed at the close, for example for want of an eligible cash snapshot, the run stops at the close with those decisions already journalled. Before this ADR, the same failure rejected the bar and discarded them.
- **The session-close pass reads every instrument.** It must clone only the instruments it proposes for, and read the rest with `peekInstrument` (docs/development.md, "Reducer transactions"). A universe-wide benchmark at 1,000 instruments pins that cost.
- **ADR 0010's day is reproducible from the previous-close snapshot and the Session's inputs**, and the Session is now a first-class, journalled unit.
- **Glossary.** CONTEXT.md gains **Session**: the set of completed bars sharing one period end, ended by `market.session.closed`. It is the unit ADR 0010's daily order and ADR 0011's once-per-bar evaluation apply to.

## Open, deferred to #34

This ADR's session boundary, completeness checks and ADR 0010 order are implemented now (RulesVersion 1.6.0). What remains is the ranking, and three prerequisites for it are missing. The definitions are listed on #34.

1. **Strength and its tie-breaks are not computed anywhere.** Nothing in `internal/strategy` or `internal/indicator` computes Strength or a 20-day median dollar volume. The reducer keeps no close history beyond the previous close, and no volume at all. Both measures are #34's to build. The dollar-volume tie-break needs rule decisions that nothing in the repository currently makes: raw or split-adjusted view (ADR 0004), whether the window includes the decision bar, how the median of an even-sized window is taken, and what happens while the window is not yet full. The same measure appears in ADR 0009's eligibility criterion, which is also unimplemented, so one definition should serve both. Until those decisions exist, `rankSignals` is the stub described in §3.
2. **Nothing currently binds across instruments at proposal time, so a ranking cannot select anything.** The per-instrument cap (ADR 0008) is the only cap implemented. The industry, sector and total-long caps are not (`reducer.go`, `sizeUnit`). The cash check compares each Unit with the snapshot less the fills it does not yet reflect (ADR 0020's implementation note), so a fill in one Session constrains the next; but within one session-close pass nothing is spent, and ADR 0020 deliberately reserves nothing at proposal time, placing reservations at order placement (#105) instead. So today, however the entries are ranked, every Tier A Signal that fits alone is proposed. Ranking changes only the order of emission. The test that "more Signals than cap or cash allows go to the highest Strength" therefore moves to #34, together with a shared budget (#33 or #105). That is ADR 0008's total-long cap, ADR 0020's placement ledger, or #33's previous-close headroom. For cash, ADR 0020's "Open" section says which competing entry is funded is decided by placement order. This ADR makes that the ranked session-close order, but only once placement consumes a budget.
3. **The Strength basis under the completed-bar rule.** CONTEXT.md applies the completed-bar rule to every input of a decision. That makes Strength for a Signal on bar *t* equal to `(close(t−1) − close(t−64)) / N(t−1)`, not a figure using *t*'s own close. The resting-order model (ADR 0005) needs the order to exist before *t* opens. ADR 0010's formula does not say which. This ADR reads it as the preceding bars, and that reading needs confirming when #34 is specified.
