# Plan and Ticket Review

**Date:** 2026-09-04
**Reviewer:** Claude (taking over from the ChatGPT/Codex thread of 2026-08-28/29)
**Reviewed:** `~/Desktop/Trend_Investing/Overall_Plan.md`, `Linear_Backlog_Plan.md`, the 121 issues in Linear team `TRI`, this Go repo, and the legacy QuantConnect prototype.
**Companion:** `Methodology_Analysis.md` (the redone source analysis).

The purpose of this document is to (1) say what in the existing plan should be kept, changed, or dropped, (2) record where the repo and Linear have already drifted apart, and (3) lay out the pros and cons of the three ways to handle the existing Linear backlog so that a decision can be made after reading the methodology analysis.

---

## 1. Overall_Plan.md — section-by-section verdict

| § | Topic | Verdict | Notes |
|---|---|---|---|
| 1 | Objective (three declared strategies: Turtle control, rules-based Sublime, hybrid) | **Keep** | Still the right framing. One refinement: the hybrid is not a third *strategy* to build, it is a *set of predeclared ablations* on the Turtle control. Treat R3 as an experiment matrix, not a product. |
| 2 | Go engine + thin Python LEAN adapter | **Keep, but gate on the spike** | Sound, and already recorded as ADR-0001. The unproven part is the Python.NET → local-transport → Go round trip *inside the LEAN Docker container*. Nothing else in the architecture should be built until P2-02 (transport spike) has a measured result. The plan already says this in §9 ("no provider should be purchased until…"), but the backlog does not sequence it first. |
| 3 | Modular monolith, replayable event journal | **Keep** | Correct and already partially implemented (`internal/event`, `internal/replay`). |
| 4 | Source-of-truth strategy specifications | **Keep; superseded in detail by `Methodology_Analysis.md`** | Three corrections to the Turtle list: (a) "Equal Unit size within a campaign" is *not* stated in Faith — Units were re-sized weekly from a Unit sheet (Faith p.15). Whether later Units in a campaign reuse the first Unit's size is a decision we must make and record, not a source rule. (b) "Explicit stop progression" — Faith is explicit (p.22–23): earlier stops move up by ½N per added Unit, *except* when a later Unit fills further away because of a gap, in which case that Unit carries its own 2N stop. (c) The drawdown rule is measured against the *original* notional account and resets at the yearly re-basing (p.17), not a rolling high-water mark. The Sublime list is materially right but needs the disclosed/reconstructed/proxy/excluded tagging that the analysis now provides. |
| 5 | Correctness architecture (deterministic events, trade proposals, safe recovery) | **Keep; defer most of it** | All correct for live trading. None of it is needed to get a *backtest* to run. Only the deterministic-reducer and replay pieces belong in the first vertical slice. |
| 6 | Data and audit plan (PostgreSQL) | **Defer** | No database is needed until there is something to persist beyond a fixture file. The journal interface (P1-06) should be a Go interface with a file-backed implementation first. |
| 7 | Testing strategy | **Keep** | Good list. Golden Turtle scenarios (R1-10) should be authored *from Faith's own worked examples* (Heating Oil N table p.14–15, Gold/Crude add ladders p.20, Crude stop ladders p.22–23) so the first tests are source-verified, not invented. |
| 8 | Observability | **Defer** | Structured logging yes; OpenTelemetry/vendor selection no, until paper trading. |
| 9 | Hosting shortlist | **Drop from the near-term plan** | Correct content, wrong time. Nothing here is decidable before P2-13 (resource profile) exists. |
| 10 | Delivery phases and gates | **Keep the gates; re-cut the phases** | Phase 0's gate ("same input stream → identical decisions") is exactly the first vertical slice. Phases 0 and 1 should be merged into a sequence of slices, each ending in a runnable backtest. |
| 11 | Decisions still required | **Keep; re-prioritise** | See §4 below. Two of the nine (data provider; long-only scope) block *research*, not just operations, and should be decided first. |

## 2. Linear backlog review

### 2.1 Structure

- Team `Trend Investing` / key `TRI`, 9 projects under 3 initiatives, 121 issues (TRI-5 … TRI-125), each mapped 1:1 to a row of `Linear_Backlog_Plan.md` with native blocking relations and milestones. The **structure is good** and worth keeping: labels, priorities, milestones, and the dependency graph all exist.
- Issue bodies are **thin**: each carries the acceptance-criteria cell from the plan table, a "Declared dependencies" line, a boilerplate "Replay and audit impact" paragraph, and a pointer back to the plan file. None describe a user-observable outcome, inputs, or testing approach — the plan's own issue template (§1 of the backlog plan) asked for seven sections and the tickets have three.
- Only **TRI-44 [P1-01]** is Done (2026-08-30). Everything else is Backlog. TRI-1…4 are Linear onboarding boilerplate and can be deleted.

### 2.2 Shape problem (the main finding)

The backlog is **horizontal**: complete all specification (R1, 12 tickets) → complete the Go event/replay layer (P1, 14) → complete the adapter (P2, 14) → complete persistence/observability (P3, 15) → then run the first real backtest (R2-12). Counting blockers, the first runnable, diversified backtest sits behind roughly 40 tickets. In the meantime 38 tickets (O1–O3) describe hosting, paper-trading soak, and limited-live launch for a strategy that has never been backtested.

That ordering has two costs:

1. **No feedback until very late.** The single most likely thing to invalidate the plan — the LEAN ↔ Go boundary being awkward, or the stock-Turtle control simply not being tradeable in equities — is discovered last.
2. **Spec work without a harness.** R1 asks for twelve specification tickets to be "frozen" before any code exists to check them against. Golden scenarios (R1-10) cannot be executed until R2-02…R2-10 exist.

The alternative is **vertical tracer-bullet slices** (what mattpocock's `to-tickets` produces): each ticket cuts a narrow but complete path — a rule from the spec, its Go implementation, its golden test, its event/replay fixture, and (from the second slice on) its LEAN adapter path and a backtest that exercises it. Each slice is demoable on its own.

### 2.3 Repo ↔ Linear drift

| Evidence | Ticket state | Action needed |
|---|---|---|
| Commit `8abfc7c Add versioned event envelope` → `internal/event/envelope.go` | TRI-47 [P1-02] Backlog | The envelope exists but lacks the P1-02 fields (source, strategy/config version, payload integrity). Either move TRI-47 to In Progress with a note, or re-scope. |
| Commit `e21abec Add deterministic replay engine` → `internal/replay/engine.go` | TRI-48 [P1-07] Backlog | Engine exists (sequence-contiguity, fail-closed) but has no fixture I/O, diff, or machine-readable result. Same choice as above. |
| `docs/adr/0001-modular-monolith.md` | No ticket | Fine — ADRs need not be tickets. |
| `adapter/lean/README.md` (reserved, no code) | TRI-57 [P2-01] Backlog | Consistent. |

Nothing else has drifted. The repo is small enough that this is a 10-minute reconciliation whichever Linear option is chosen.

### 2.4 Ticket-level notes worth carrying forward

- **R1-03 / R1-04** (Unit-risk terminology; stop progression) are the two tickets whose content most often gets wrong in implementations, including the QC prototype. They should become the first two golden-scenario fixtures rather than standalone documents.
- **R1-05** (stock adaptations) is the ticket that decides *long-only*, *integer shares*, *no leverage*, *earnings handling*. It is on the critical path for everything in R2 and must be decided during grilling, not deferred to "spec v1 approval".
- **R2-01** (point-in-time universe) is the single largest research risk in the whole backlog: survivorship-free US equity universe data is not free. It depends on D-02 (data provider), which the backlog files under *operations* (O1). It belongs in research.
- **R3-05** (pullback / second-breakout proxy) is where Sublime's discretion bites hardest. Its acceptance criteria say "ambiguous chart judgment is excluded" but do not say what replaces it. The 4PS A/B/C state machine in `Methodology_Analysis.md` §5 is the candidate definition and needs thresholds chosen and sensitivity-tested (R3-12).

## 3. Legacy QuantConnect prototype — disposition

**Recommendation: archive as reference; do not extend.**

- It is AAPL-only, checks stops on daily closes, re-sizes each added Unit from current equity, overwrites the campaign stop with the newest Unit's 2N, uses 1N add spacing, and resets its drawdown ladder at every new equity peak. Each of those is a strategy decision it made silently; several contradict Faith. Fixing them one by one would just re-create the Go design in Python.
- Its 28 backtests are not evidence of anything (2–13 orders, one symbol, three most recent runs crashed in `Initialize()`).
- The four review documents (`TurtleTradingStrategy_ToDo.md`, `QuantConnect_Review.md`, `GoogleStyleGuide_Review.md`, `MASTER_PLAN.md`) contain a useful **bug taxonomy** (look-ahead in the Donchian read, SMA-vs-Wilder ATR, close-based stops, unit-risk multiplication, calendar-day warm-up) that should become negative test cases in the Go golden catalog.
- **Open decision (yours):** the working tree holds an uncommitted +56/−11 fix for the `Initialize()` crash. Options: (a) commit it as a final "archive: last working-tree state" commit so the history is complete; (b) `git stash` it; (c) leave it. I recommend (a) — it costs nothing and prevents the diff from being lost to iCloud sync.

## 4. Decisions that block research (not just operations)

The backlog lists ten decisions (D-01…D-10) and files most under O1. Two of them block the first backtest and should be decided during grilling:

| Decision | Why it blocks research | Options to weigh |
|---|---|---|
| **D-02 Market-data provider** | R2-01 needs a survivorship-free, point-in-time US equity universe with corporate actions. The local `lean` workspace has a `data/` tree but it is the lean-cli sample set, not a full history. | QuantConnect data via `lean data download` (pay-per-dataset, integrates natively with LEAN, includes delistings); Norgate (survivorship-free, popular for exactly this, needs an adapter); Polygon/Tiingo (cheap daily bars, weaker corporate-action/delisting coverage). |
| **D-03 Long-only, unleveraged initial scope** | Determines whether `RiskPerShare × Shares ≤ cash` is a hard constraint, whether the short side of every Donchian rule is implemented, and whether Sublime's 20%-of-equity margin model applies. | You have already said long-only first, then shorts, then futures. Record it as an ADR and remove the short paths from slice 1. |

The remaining decisions (broker, hosting, observability vendor, managed-vs-self-hosted Postgres, RTO/RPO, operator workflow) stay deferred.

## 5. The Linear decision — pros and cons

Three options were named in the plan. Assessed against: preserving the good structure, getting to a runnable backtest quickly, effort, and audit trail.

### (a) Keep + reconcile

Keep all 121 tickets; fix TRI-47/48; enrich bodies as they are picked up; add vertical-slice tickets under R1/P1/P2 as children or related issues.

| Pros | Cons |
|---|---|
| Zero destruction; dependency graph and milestones survive. | The horizontal shape survives too — the first backtest still sits behind ~40 blockers unless relations are also rewired. |
| Cheapest immediate action. | Two ticket vocabularies coexist (layer tickets and slice tickets), which is confusing for an agent picking "the frontier". |
| Full audit trail. | Enriching 121 thin bodies is more work than writing ~15 good ones. |

### (b) Wipe and regenerate

Cancel TRI-5…TRI-125 (and delete TRI-1…4), keep the team, projects, labels; regenerate the whole backlog with `to-tickets` from the spec.

| Pros | Cons |
|---|---|
| One vocabulary, one shape (vertical), tickets sized for a single context window. | Loses 121 blocking relations and 9 milestone assignments; O1–O3 content (which is *good* content, just early) would need to be re-derived later. |
| Forces every ticket to state a demoable outcome. | `to-tickets` should be run from a spec, and the spec covers R1/R2/P1/P2 territory only — regenerating O1–O3 from it would produce nothing, so those would simply vanish. |
| Clean start for the Linear ↔ repo mapping. | Cancelled tickets still show in history; "why was this cancelled" needs a comment on each or one project-level note. |

### (c) Hybrid — keep the skeleton, re-cut the near-term (recommended)

Keep initiatives, projects, labels, milestones. Leave **P3, O1, O2, O3** (53 tickets) untouched in Backlog — they are correct content that becomes relevant after a validated strategy exists. Cancel the **R1, R2, R3, P1, P2** tickets (68) and replace them with `to-tickets` vertical slices generated from the spec, published into the same projects, with a comment on each cancelled ticket naming its replacement.

| Pros | Cons |
|---|---|
| Fixes the shape where it matters (the next 3–6 months) without throwing away the ops plan. | Still cancels 68 tickets; needs a scripted pass to add the "superseded by TRI-nnn" comment. |
| Projects and milestones keep meaning; project-level dependency map (§3 of the backlog plan) stays valid. | R3 (Sublime/hybrid) slices cannot be fully written until the Turtle control runs, so R3 will be regenerated twice (skeleton now, detail later). |
| The 53 deferred tickets act as a parking lot with their original reasoning intact. | Two generations of ticket style in the same team (mitigated by the cancellation comments). |

**Recommendation: (c).** It is the only option that both changes the shape of the next several months of work and keeps the parts of ChatGPT's plan that were genuinely good (the operations gates, the capital-safety labelling, the project dependency map).

Whichever option is chosen, the first three slices are the same:

1. **Slice 1 — one-symbol Turtle reducer with golden tests.** Wilder N; prior-bar 55/20 channels; Unit sizing for integer shares; 2N stop; ½N adds to four Units with Faith's stop-progression; 20-day exit. All driven by an ordered bar-event fixture through the existing `replay.Engine`, emitting decision events. Golden tests transcribe Faith's Heating Oil, Gold, and Crude examples. *Demo:* `go test` replays a fixture and prints an explainable decision log.
2. **Slice 2 — LEAN adapter round trip on that symbol.** Minimal `QCAlgorithm` publishes completed daily bars over the chosen transport; Go returns proposals; adapter submits market orders; fills come back; the recorded journal replays offline to identical decisions. *Demo:* an AAPL backtest whose journal replays byte-for-byte.
3. **Slice 3 — portfolio and universe.** Point-in-time liquid-stock universe (needs D-02), per-stock / sector / aggregate risk caps, drawdown re-basing, buy-strength ranking on simultaneous signals. *Demo:* a diversified long-only backtest with a reconcilable trade ledger — the R2 exit criterion.

Sublime filters (regime, sector strength, MTF alignment, 4PS entry, 3×ATR stop, risk-free-before-add) become slices 4+, each an ablation against slice 3's baseline.

## 6. Summary of recommended actions

1. Record D-03 (long-only, unleveraged, integer shares) and the Turtle baseline parameters as ADRs during grilling.
2. Decide D-02 (data provider) during grilling; it gates slice 3.
3. Reconcile TRI-47 / TRI-48 with the code that already exists.
4. Choose Linear option (c) unless you prefer otherwise; run `to-tickets` after `to-spec`.
5. Archive the QC prototype with a final commit of its working-tree fix.
6. Delete TRI-1…4.
