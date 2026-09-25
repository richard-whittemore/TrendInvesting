# ADR 0024: A symbol change carries an instrument's state to a new id, and a dividend is credited as cash

- Status: Proposed
- Date: 2026-09-25
- Relates to: ADR 0004, ADR 0006, ADR 0008, ADR 0009, ADR 0010, ADR 0015, ADR 0019, ADR 0020, ADR 0021, ADR 0023

## In plain English

*A summary for reading the rest of this ADR against. Where the two differ, the detailed text below governs.*

Two more corporate actions join the one ADR 0023 already added.

A **symbol change** is a stock continuing under a new ticker. The engine's instrument id was already an opaque, adapter-assigned identity, never the ticker itself, so nothing about a Campaign, its indicator history or its universe classification needs to change at all — except that this reducer keys every one of those things by that id in a map, and a symbol change moves the whole entry to a new key, in one step, rather than closing the old one and opening a fresh one. That is the whole engineering problem this ADR solves: a rename, not a delisting.

A **dividend** is cash paid on shares already held. It is credited to the account, exactly the way a split's cash in lieu already is (ADR 0023), and it changes nothing else: no channel level, no N, no price the rules see (ADR 0004).

## Context

Issue #38 asks the domain to handle two of the events the LEAN adapter will eventually emit (#28, still out of scope): a symbol change and a dividend. Both are journalled as a new `market.corporate-action` kind, following ADR 0023's shape and its schema-versioning discipline (ADR 0015).

### Instrument identity: is the engine's id the ticker?

No, and this is already true today, not a new decision this ADR makes. `event.CompletedBarPayload.InstrumentID` and every other payload's own instrument field carry an opaque string a producer assigns; nothing in `internal/event` or `internal/strategy` derives it from, or compares it against, a ticker. `adapter/lean/algorithm.py`'s own handling of `SymbolChangedEvents` already states this in a comment: *"A rename changes nothing for one instrument whose instrument_id this adapter holds constant, and a ticker change is not a delisting... so it is recorded here and never published."* The adapter's own instrument id is a per-`QCAlgorithm` constant, set once, and is never derived from LEAN's `Symbol` object at all.

So the question issue #38 poses — "is the engine's instrument ID the ticker? If it is, a symbol change needs a mapping or a stable ID" — is answered by the existing code: it is already a stable ID, not a ticker. That does not make a symbol change a no-op in the domain, though. `Reducer.instruments` is `map[string]*instrumentState`, keyed by that id, and everything a symbol change must preserve — the open Campaign, its frozen N and Unit size (ADR 0006), its indicator history (N, the Entry/Exit Channels, the ranking windows), its universe classification (ADR 0009) and its resting orders (its pending proposals and their ADR 0020 holds) — lives inside the one `*instrumentState` value that key points at. A symbol change is therefore a **statement that a producer is about to start using a different key for state this reducer already holds**, and the reducer has to move it, not recompute it.

This is deliberately general, not contingent on today's adapter's actual behaviour (which keeps its own id fixed across a LEAN rename and would never need this at all). A future producer — a different adapter, or the same one keying by a listing-derived id — may legitimately need the id to move. This ADR gives the domain a correct, general mechanism for that, matching ADR 0019's own words for the failure mode it must avoid: *"a symbol rename must not merge unrelated instruments."*

### A dividend

ADR 0004's Decision already states the rule: "dividends credited as cash events when they occur," never folded into either price series, because a dividend-adjusted history rewrites the breakout levels that actually existed. Nothing in the domain implements that yet — there is no dividend event at all. ADR 0023's cash in lieu is the closest existing shape: a corporate action's cash reaching the account only through the next previous-close snapshot (ADR 0020), with the reducer recording the credit and never touching spendable cash itself.

## Decision

### 1. The event: `market.corporate-action`, two new kinds

`event.CorporateActionPayload` moves to schema version **3**. Kind is still a closed set, now `delisting`, `split`, `symbol-change` or `dividend`.

A **symbol change** states:

| Field | Meaning |
| --- | --- |
| `instrument_id`, `effective_at` | as for every kind: which instrument, and when |
| `new_instrument_id` | the id `instrument_id`'s Campaign, indicator history and universe classification continue under |

A **dividend** states:

| Field | Meaning |
| --- | --- |
| `instrument_id`, `effective_at` | as for every kind |
| `cash_amount` | the exact cash the broker paid on `instrument_id`'s held shares |
| `currency` | that currency (shared with a split's `cash_in_lieu`) |

Validate rejects a kind carrying another kind's fields, exactly as ADR 0023 already does for a delisting and a split, and now also for these two: a symbol change carries no split or dividend terms, a dividend carries no split or symbol-change terms, and so on.

**The upcaster (ADR 0015).** A schema-2 record could only ever be a delisting or a split, since schema 2's Kind set held nothing else. `event.UpcastCorporateActionPayload` reads a schema-1 record forward exactly as before, and now also reads a schema-2 record forward unchanged (it never had `new_instrument_id` or `cash_amount` to lose), rejecting a schema-2 record naming a kind or field it could not have had. Every reader uses it: the reducer, and `cmd/backtest`'s re-run.

### 2. The rule: a symbol change moves the whole `*instrumentState`, once, to the new key

On a symbol change from `old` to `new`, the reducer:

1. Refuses if either id is already delisted (ADR 0009), if either id is already retired by an earlier symbol change (`Reducer.renamed`, terminal in both directions — see the amendment below), if `new` already names an instrument this reducer tracks, or if `new` already has a declared universe classification (ADR 0009) with no bar yet — merging two instruments' histories or facts under one id is refused outright, never silently combined (ADR 0019's own rule, quoted above).
2. Refuses if the change is not chronologically between Sessions, at or after `old`'s last completed bar and before either id's bar in the currently open Session — the same ADR 0010/0021 ordering every corporate action keeps.
3. When `old` is genuinely unknown (no bar ever accepted for it), there is nothing to move, and nothing is journalled — mirroring `applyDelisting`'s identical no-op for an instrument this reducer never saw. The id is still retired (below).
4. Otherwise, emits one `strategy.instrument.symbol-changed` decision, then moves the **same** `*instrumentState` value from key `old` to key `new`: no field is copied or recomputed, so there is nothing this move could fail to carry across by forgetting to name it. The Campaign's own denormalised `instrumentID` field (read directly by later Add proposals and Exit Orders) moves with it, in the same step. Every standing ADR 0020 hold naming `old` is re-pointed at `new`, so per-instrument cap accounting (ADR 0008) still recognises it. The instrument's declared universe classification (ADR 0009, held in a separate reducer-level map) moves too.

**Why this is not a delisting plus a new instrument.** A delisting emits `strategy.campaign.exited` with `Reason: delisting` and leaves nothing behind to reopen; a fresh Campaign under a "new" instrument would start with no indicator history and no frozen values, at Unit 1. A symbol change emits neither an exit nor an open: the Campaign's identity (`CampaignID`) is unchanged, its Units are unchanged, and its frozen N and Unit size are unchanged, because it is the same value, filed under a new key.

**The old id is retired.** `Reducer.renamed` records `old -> new`, mirroring `Reducer.delisted`. Unlike a delisted instrument's bar, which is absorbed (a delisting notice may legitimately name an instrument this strategy never had a stake in), a bar or a fill still naming a renamed-away id **fails closed**: the old id DID have a stake here before the rename, so a later input naming it is almost certainly a producer defect (a duplicate feed, or one that never switched to the new id), not a benign coincidence. Absorbing it would let the reducer open a fresh, zero-history Setup under a name that used to mean something else.

### 3. The rule: a dividend is a cash credit, and changes nothing else

On a dividend for `instrument_id`, the reducer:

1. Refuses if the instrument is delisted, or if there is no open Campaign to explain the payment — a dividend is a return on held shares, and a payment this reducer can attribute to no Unit is unexplained (ADR 0019), exactly as a split's shortfall with nothing held is.
2. Refuses a dividend at or before the instrument's last completed bar, or at or before the last dividend already applied to it (each dividend applies once; unlike a split, a Campaign may legitimately be paid several dividends over its life, so a later, genuinely different one is not refused).
3. Otherwise, emits one `strategy.campaign.dividend` decision naming the Campaign, the cash and its currency. Nothing else moves: no Unit's quantity changes, no Exit Order is re-rested, and no channel level, N or price the rules see is touched (ADR 0004). The cash reaches spendable cash only through the next previous-close snapshot, exactly like a split's cash in lieu (ADR 0020): the reducer records the credit and never adds it to spendable cash itself.

### 4. Ordering: before the decision for the bar it affects

Both kinds are dispatched through the same `applyCorporateAction` entry point ADR 0023 built, which already refuses a corporate action for an instrument whose bar the open Session already holds (ADR 0021). A symbol change additionally checks the SAME rule against its own new id, since a producer could otherwise deliver the change after the new id's own first bar of the same Session — the old id's check alone would miss that. `cmd/backtest`'s `-corporate-actions` fixture delivers every action before the Session it is due for; a symbol change is matched against BOTH the id it leaves and the id its bars continue under, since the fixture's next bar for a renamed instrument never again carries the old id.

### 5. The fill simulator learns both from the reducer's emissions

`internal/fills` observes `strategy.campaign.dividend` by crediting the simulated account's cash, with no book effect — a dividend changes no Unit's quantity. It observes `strategy.instrument.symbol-changed` by moving the resting-order book (`Simulator.books[old] -> [new]`), the campaign's own denormalised instrument id inside it, and, when a simulated account is kept, its holding and latest close, from `old` to `new`. `Simulator.observe` still treats the `market.corporate-action` input itself as having no effect on the book, whatever its kind, for both — the same discipline ADR 0023 established.

### 6. Versions

- `market.corporate-action` → schema 3, with the upcaster above.
- New decision `strategy.instrument.symbol-changed`, schema 1.
- New decision `strategy.campaign.dividend`, schema 1.
- `strategy.RulesVersion` 1.15.0 → **1.16.0**. Given the same inputs, a build at 1.15.0 refused both new kinds outright ("kind ... is not implemented by this reducer"); this build carries a Campaign's whole state across a symbol change and credits a dividend as cash, so the two builds do not replay each other's journals (ADR 0016). Every existing Baseline and Variant golden journal is otherwise byte-identical once `strategy_version` and every hash derived from it are excluded: no DISCLOSED rule, ladder or Baseline/Variant setting changed.

### 7. What this does not do

Publishing a symbol change or a dividend FROM LEAN is #28, deliberately out of scope: `adapter/lean` still only carries a split's cash in lieu across the wire. `CORPORATE_ACTION_SCHEMA_VERSION` in `adapter/lean/publisher.py` moves to 3 because the Go/Python contract test (`corporate_action_contract.go`) checks the envelope's schema version exactly, not merely that it is upcastable — the wire shape of a split is unaffected, since both new fields are `omitempty` and a split never sets them.

## Alternatives rejected

- **Represent a symbol change as a delisting followed by a fresh instrument.** This is explicitly what issue #38 asks NOT to build: it would lose the Campaign's frozen values and indicator history, and would report a real, ongoing position as two: one exited (wrongly, at a "last available price" that is really just the day before a ticker changed) and one newly opened at Unit 1 with no history.
- **Copy the instrument's fields into a new state under the new key, leaving the old key in place.** This keeps two live copies of one Campaign's history, doubles every later read (which id is authoritative?), and gives ADR 0019's reconciliation exactly the merge risk it warns against, in the other direction: two ids that both look like they hold something.
- **Give the dividend a per-share amount and have the reducer multiply by the Campaign's held quantity.** The engine's Units are in split-adjusted shares, and a dividend is paid per RAW share at the broker; computing the total from a per-share figure would require a raw/adjusted conversion this ADR has no need to introduce. Stating the exact posted cash directly, the way ADR 0023's `cash_in_lieu` already does, needs no such conversion and matches ADR 0019's own preference for "the exact posted amount," not a derived one.
- **Let a symbol change also carry split-like share adjustments.** Nothing about a ticker change touches share counts; conflating the two kinds would resurrect exactly the field-exclusivity problem ADR 0023 solved for split vs. delisting.

## Consequences

- A symbol change mid-Campaign, a dividend during an open Campaign, and a split, all have failing-tests-first coverage at the reducer seam (`internal/strategy`) and the fill-simulator seam (`internal/fills`).
- `Reducer.renamed`, `Reducer.begin`'s eager copy of it, and `transition.renameInstrument`'s commit-time deletion are new reducer-transaction machinery (docs/development.md), the first to remove an instrument's map entry rather than only add or mutate one.
- `campaignState.instrumentID` (a denormalised copy of the map key, read directly by Add proposals, Exit Orders, cap checks and hold placement) and `hold.instrumentID` (read by per-instrument cap accounting) both need their own rewrite at a symbol change; a rename that moved only the map key would leave every subsequent decision for that Campaign silently citing the old id.
- Every journal with a symbol change or a dividend gains a corporate-action input and one decision. Journals without either are unchanged except for the rules version.
- Live trading needs the broker's own dividend statement, and its own confirmation of a symbol change, to be journalled as these events, with source transaction identity, before ADR 0019's reconciliation can use either as an explanation — the same live-readiness gap ADR 0023 already states for a split's cash in lieu.

## Amendment (2026-09-25): four review findings, closed

Greptile and CodeRabbit reviewed the first cut of this ADR's implementation and raised four findings, each checked against the code and fixed failing-test-first.

**1 and 2: `Reducer.renamed` was not itself terminal.** `applySymbolChange` checked `r.instrument(oldID)`/`r.instrument(newID)` to decide whether an id had live state, but an id this reducer had already renamed AWAY has no live `*instrumentState` — it was moved to its successor and removed from `Reducer.instruments` by `transition.renameInstrument` — so it looked exactly like an id never seen at all. Two consequences followed:

- **A second change from an already-retired old id** (`A -> B`, then `A -> C`) reached the "genuinely unknown instrument" no-op branch and silently overwrote `Reducer.renamed["A"]` from `"B"` to `"C"`, while the Campaign it actually carries still lives under `B`. A later bar for `A` then fails closed pointing at `C`, not `B`.
- **A change INTO an already-retired id** (`A -> B`, then `C -> A` or `B -> A`) passed the "already tracked" guard, since `A` has no live state to be tracked, and moved a fresh Campaign's state under `A` while `Reducer.renamed["A"]` still points at `B`. The very next bar or fill for `A` then fails closed against its own, brand new state.

Fixed by checking `Reducer.renamed` for both `oldID` and `newID`, after the delisted checks but before the "already tracked" and declared-classification checks below: a retired id is never reused, as either the source or the destination of a later change (`TestASecondSymbolChangeFromAnAlreadyRetiredInstrumentFailsClosed`, `TestASymbolChangeIntoAnAlreadyRetiredInstrumentFailsClosed`). `symbol_change.go`'s own guard order, matching the code exactly, is: delisted (`oldID`, then `newID`), retired (`oldID`, then `newID`), the open-Session check on `newID`, "already tracked" (`newID`), then the declared-classification check (`newID`).

**3: A declared classification on the destination id was invisible to the merge guard.** `Reducer.classifications` (ADR 0009) is a separate reducer-level map, deliberately not part of `instrumentState`, because "a classification legitimately arrives for an instrument this reducer has no bar for yet" (its own doc comment). The existing "already tracked" guard reads only `r.instrument(newID)`, so a new id with a declared classification but no bar yet passed it, and the rename's own `r.classifications[newID] = record` line then silently overwrote that classification with the old id's — a merge exactly as real as the one ADR 0019 already forbids, just through a map the existing guard never looked at. Fixed by checking `r.classifications[newID]` alongside `r.instrument(newID)` (`TestASymbolChangeIntoAnInstrumentWithADeclaredClassificationFailsClosed`).

**4: A terminal dividend or cash in lieu never reached any account.snapshot.** `cmd/backtest`'s `drive()` computes and freezes each Session's own closing statement inside `fills.RunSession`'s per-Session lifecycle (`closeOf`), before `fills.StateLastClose` ever delivers it; the last Session's statement is frozen the same way, immediately after its own bars, well before `deliverRemainingActions` runs. So a dividend or a split's cash in lieu effective at or after its instrument's own last bar — delivered by `deliverRemainingActions`, strictly after every Session's bars — credited the simulated account's ledger (`internal/fills.observeDividend`/`observeCashInLieu`) with **no account.snapshot ever stating it**: the run's one and only closing statement had already been computed before the credit existed. Simply reordering `drive()`'s calls does not fix this, because the statement's FIGURES are captured at `closeOf` time, not at `StateLastClose`'s delivery time — confirmed by writing the reordering first and watching the regression test still fail.

Checked against ADR 0023's own cash in lieu, as this finding asked: the identical gap exists there, for the identical reason, since both credits reach the same ledger the same way.

**Fixed by refusing, not by inventing a second statement.** `deliverRemainingActions` now checks every remaining action with `refuseIfCashCrediting` before delivering any of them: a dividend, or a split stating `CashInLieu > 0`, effective at or after its instrument's own last bar refuses the whole run outright, naming the action and citing this ADR. This is the conservative choice between the two the finding named — inventing an artificial second closing statement outside this run's existing one-statement-per-Session contract was rejected as a larger, less certain change than refusing a case this fixture happens to construct (`TestATerminalDividendIsRefused`, `TestATerminalCashInLieuIsRefused`; `TestATerminalSplitWithNoCashInLieuIsNotRefused` pins that a split crediting no cash is unaffected).

**`EffectiveAt` states when the action truly took effect, and this refusal gives no reason to state it otherwise.** A producer cannot make this refusal go away by moving a dividend's or a split's stated `EffectiveAt` earlier than the broker's own posted date merely to land it before the instrument's last bar in some fixture — that would journal a false fact, exactly the "an announcement of a future corporate action... is not an explanation" failure ADR 0019 already refuses to accept the other way round. The refusal is not telling a producer to pick a more convenient timestamp; it is reporting that THIS run's own bars end before the action's true effect could ever reach a statement. Avoiding it legitimately needs a run whose bars for that instrument continue past the action's real effective time — a later bar arriving in the same run, so the ordinary per-Session statement that follows it reflects the credit — never a rewritten `EffectiveAt`. If the action's true effective time already precedes the instrument's last bar, it is not "terminal" in the sense this section means at all, and `deliverActionsDueFor` delivers it ahead of the bar it precedes, exactly as ADR 0010/0021 already require; this refusal is reached only when the true effective time is genuinely at or after that last bar.

No RulesVersion bump: `internal/strategy.RulesVersion` stays at 1.16.0, still unreleased. These are corrections to that version's own intended behaviour, not a new rule; `testdata/decision-corpus/1.16.0.json` gained the three new reducer-seam regression tests as an addition, with every pre-existing entry and every earlier version's file untouched.

## Amendment (2026-09-25): four more review findings, closed

A second review pass on the amendment above (Greptile and CodeRabbit again) raised four more findings.

**1: Does a terminal symbol change leave a stale name in the closing statement?** Checked, and found not to be the same gap as a terminal dividend or cash in lieu. `event.AccountSnapshotPayload` states only the account's aggregate `Equity` and `AvailableCash` — it has no per-instrument field a rename could leave stale, unlike a cash figure a credit genuinely changes. `internal/fills.observeSymbolChanged` moves map keys only (`Simulator.books`, `account.holdings`, `account.closes`); it moves no cash and changes no holding's quantity or value. So the aggregate figures the closing statement already carries are identical whether the rename lands before or after `fills.StateLastClose` computes them. `TestATerminalSymbolChangeLeavesTheClosingSnapshotCorrect` proves this rather than asserting it: a run ending with a terminal symbol change produces the SAME final statement, field for field, as the identical bars with no corporate action at all. Refusing a terminal symbol change the way a terminal cash credit is refused would reject a legitimate, ordinary scenario (an instrument's last recorded bar under an old ticker, renamed before any bar under the new one arrives in this particular run) for no correctness reason, so no refusal was added.

**2: A symbol-change CHAIN landing entirely between two bars only delivered its last hop.** `deliverActionsDueFor` matched a symbol change against a bar's own instrument id via `NewInstrumentID == instrumentID` — a single hop. Given `A -> B` and `B -> C`, both effective before `C`'s first bar with no bar for `B` in between, only `B -> C` matched `C`'s own boundary check (its `NewInstrumentID` is `C`); `A -> B`'s `NewInstrumentID` is `B`, which never equals `C`, so it was never delivered ahead of `C`'s bar at all. The reducer then received `B -> C` for an instrument (`B`) it had never actually reached — recorded as a retired id with nothing to carry (§2 item 3, above) the first time this was possible, or refused outright once `Reducer.renamed["B"]` was already set from an earlier, unrelated case — either way losing or blocking a Campaign a well-formed chain should carry across intact. Fixed with `symbolChangeChain` (`cmd/backtest/backtest.go`): before checking a bar's own boundary, walk the chain of undelivered symbol changes backward from `instrumentID` — whichever change produced it, whichever produced THAT change's own old id, and so on — and deliver every hop found, not only the last one. `TestASymbolChangeChainIsFullyDeliveredBeforeTheDestinationsBar` pins a two-hop chain (`AAPL -> AAPL2 -> AAPL3`, `AAPL2` never receiving a bar) and confirms the Campaign opened under `AAPL` is still open, under its own unchanged `CampaignID`, when `AAPL3`'s bar arrives — and that `AAPL3`'s bar is evaluated as that Campaign's own, never as a fresh Setup with no history.

**3: The documented guard order didn't match the code.** The first amendment above said the retired-id checks (finding 1/2) run "before either the delisted or the 'already tracked' checks" — true of the second half, false of the first: `symbol_change.go` checks `Reducer.delisted` before `Reducer.renamed`, not after. Corrected to state the actual order: delisted, then retired, then the open-Session check, then "already tracked", then the declared-classification check.

**4: The first amendment's own advice was wrong, and dangerous for a system whose evidence must be trustworthy.** It said a producer "can always avoid" the terminal cash-crediting refusal "by placing the action before the instrument's own last bar" — read as license to move a dividend's or a split's stated `EffectiveAt` earlier than its true effective time purely to dodge the refusal. `EffectiveAt` is when the action truly took effect (this ADR's own §1: "as for every kind: which instrument, and when"); backdating it to avoid a validation rule would journal a false fact, exactly the kind of fabricated explanation ADR 0019 already refuses in the other direction ("an announcement of a future corporate action... is not an explanation"). Corrected: the refusal reports that the run's OWN bars end before the action's true effect could ever reach a statement, and the only legitimate way to avoid it is a run whose bars for that instrument genuinely continue past the action's real effective time — never a rewritten timestamp.

No RulesVersion bump and no further corpus change beyond `1.16.0.json` remaining an addition-only file: none of these four findings changed a reducer decision. Findings 1 and 3-4 are documentation and a proof, not a code change; finding 2's fix lives entirely in `cmd/backtest`, outside `internal/strategy`'s own decision surface.
