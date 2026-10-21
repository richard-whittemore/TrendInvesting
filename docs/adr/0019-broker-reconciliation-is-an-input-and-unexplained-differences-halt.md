# ADR 0019: Broker reconciliation is an input, and unexplained differences halt

- Status: Proposed (amended 2026-09-24: graduated response, alerting, recovery; amended 2026-10-15: one Halted exception for target-weight accounts; amended 2026-10-16: the same sale for a Degraded account's named instrument; amended 2026-10-17: eleven readings of the sale back to 98% (#352); amended 2026-10-21: the engine checks every close snapshot's holdings against its fills (#490))
- Date: 2026-09-17

## In plain English

*A summary for reading the rest of this ADR against. Where the two differ, the detailed text below governs.*

**What this is about.** The system keeps its own record of what it holds: shares, cash and resting orders. The broker keeps the real one. They can drift apart: a fill report goes missing, a dividend posts, someone trades by hand, or the broker cancels an order. This ADR decides how the system notices, and what it does.

**How it checks.** It regularly compares its records with the broker's: at startup, before each trading day, after every fill, before every order, and on a timer in between. A difference is fine **only if the system already has a recorded reason for it**, such as a logged dividend or a logged fill. It never copies the broker's numbers to make a difference go away, because then it would be trading positions it can't explain. Every difference counts, down to one share or one cent.

**What it does when something doesn't add up.** There are three states:

| State | When | What the system still does | What it stops doing |
|---|---|---|---|
| **Normal** | Everything matches or is explained | Everything | Nothing |
| **Degraded** | A difference it can't explain, but it can say *which* cash or *which* stocks are affected | Keeps protecting every position: raises stops and takes exits on unaffected stocks, and **re-places a missing stop** on an affected stock at its last known level (once only) | Opens **no** new positions and adds to none. Leaves the affected stocks' orders alone, apart from that one stop repair |
| **Halted** | It can't trust the broker's data at all, or can't tell what's affected | Nothing. Stops already resting at the broker keep working on their own | Changes no orders |

The guiding rule: **a problem is a reason to stop taking on new risk, not a reason to stop protecting what you already hold.**

**Safety rules that always apply.**
- Every held Unit always has exactly **one** good-till-cancelled stop order at the broker. An exit **moves** that stop rather than placing a second sell order, so the system can never sell more than it owns and end up short.
- If the broker shows more sell orders than shares held, for example because of an unknown order, the alert says **CONTAINMENT REQUIRED** and you fix that first.
- If the broker holds a position the system knows nothing about, for example after a manual trade, the alert marks it **UNPROTECTED**. The system won't invent a stop for it. That's your call.

**How you find out.** Any move out of Normal alerts you **immediately** on at least two channels (your choice: SMS plus push, pager-style), and **keeps repeating until you acknowledge it**. Separately, an outside service watches for the system's regular heartbeat, so if the system crashes and can't alert you itself, you still get paged. Each alert tells you what happened, which positions are affected, whether each has a stop, and what the system is and isn't still doing.

**How it gets back to Normal.** Never on its own, even if the numbers later match, and not after a restart either. You follow the runbook (`docs/runbooks/reconciliation.md`): acknowledge the alert, find the cause in the broker's records, record the missing event (the fill, trade, dividend and so on), decide what to do with any affected order, then approve a restart. The restart must pass a fresh check before trading resumes.

**Before any of this is live.** This ADR records the design; its implementation is partial. *(Note, 2026-10-18: much of it is now built for the target-weight accounts: the states, the sale back to 98%, the morning reconciliation's engine side and the alerts. What is built and what is not is listed in `docs/manual/README.md`, "Built today and not built yet". The text below is the decision as recorded.)* It also needs journal durability (#151), the alert and heartbeat services set up and priced, and timing limits you choose and we prove out in paper trading.

## Context

Every sizing, risk and exit decision depends on believed holdings. Correct arithmetic over a position the broker has already liquidated is still wrong. [Issue #114](https://github.com/richard-whittemore/TrendInvesting/issues/114) identifies missed, late or malformed fill reports, incorrect partial-fill accounting, corporate actions, broker liquidations, manual trades, cash transfers, cancelled or expired orders, fees, interest and dividends as causes of divergence. Checking our holdings alone also misses a position present only at the broker.

ADR 0005 fixes the simulated fill model; the fill-driven position invariant is stated explicitly in `CONTEXT.md`'s Campaign definition and `internal/strategy/campaign.go`. The simulator is the only position-changing authority in a backtest. In live trading that exclusivity is an assumption. `docs/architecture.md` already requires startup reconciliation of holdings and open orders and safe mode for material disagreement, but does not define materiality.

The current reducer holds positions as open Campaign Units with integer quantities, aggregated by `filledQuantity`; proposals are not holdings. Its `availableCash` is a float-valued **previous-close sizing snapshot**, supplied by `account.snapshot`, not a running cash ledger. `account.cash-movement` currently represents deposits and withdrawals and scales the Notional Account; it does not cover dividends, interest or fees. ADR 0010 forbids spending same-day exit proceeds. A broker balance cannot simply replace this sizing basis.

The existing halt from #12 emits `strategy.engine.state` with `state = halted` alongside an error. `replay.Engine.Run` retains the emission and stops. There is no existing resumable halt state machine. ADR 0017 distinguishes recorded inputs from reducer decisions and requires replay to reproduce the latter from the former. ADR 0012 requires provenance and retention of failed results. These constraints make reconciliation a safety rule and evidence contract, not a balance import.

## Decision

### Explain changes by causing events; never adopt a balance

Compare complete positions, cash and open orders for the same account, accounting basis and effective cutoff. A successful comparison is classified as `matched` if nothing changed since the verified anchor, or `explained` if already-journalled events account for the changes exactly. Any unexplained residual is `unexplained`; incomplete, stale, inconsistent or unavailable evidence is `unverifiable`. Both latter classifications halt the whole engine. Success advances reconciliation evidence, never overwrites Campaigns, fills, cash or orders from the snapshot.

> **Amended 2026-09-24** (see *Amendment: a graduated response* below): an `unexplained` result whose scope the evidence bounds enters **Degraded**, not a halt. Only `unverifiable` evidence, or a discrepancy that cannot be bounded, enters **Halted**.

**Explained means all of the following hold:**

1. The starting anchor is a previously verified reconciliation, or an explicitly approved and journalled opening account baseline. The observation being checked cannot serve as its own anchor. An existing holding cannot be seeded as a Campaign merely by approving a balance: its causing history and required Campaign state must be reconstructed, or trading remains blocked.
2. Every change from that anchor to the cutoff is enumerated by a typed, validated event recorded **before this reconciliation input**. Each has a stable source transaction identity, account and instrument or currency, effective time, exact economic effect, and a supported deterministic transition. The journal retains the underlying evidence, not just a broker URL. A description such as “probably dividend,” an announcement of a future corporate action, or a balancing adjustment derived from the residual is not an explanation.
3. Events are uniquely matched to broker activities through the cutoff, with complete activity coverage and no gaps, duplicate use, conflicting identities or unexplained broker activities. Matching uses identity and effect, not just equal net totals: a missing debit and credit that cancel do not explain one another. Corrections and reversals are explicit linked events, never edits to history.
4. Applying those transitions exactly once to the anchor produces the expected positions, cash and orders. The recorded believed state agrees with the reducer's own state at the declared processed-input boundary, and with this projection at the cutoff. A reference to a fill already applied cannot be added again to erase a difference. A valid event still awaiting application must be applied through its ordinary handler before reconciliation can succeed; the comparison is not a second mutation path.
5. The expected projection equals the broker observation component by component. Every differing quantity, monetary component and order attribute has its own bridge and a zero residual. A cash surplus cannot excuse a missing position or protective order.

Thus an overnight dividend with its exact posted amount, identity and effective time recorded and applied through a supported cash-event rule can reconcile without a halt, whatever its size. A journalled and applied delisting transition can likewise explain an old holding becoming zero; merely knowing a delisting was announced cannot. A genuinely missing or malformed fill report cannot be excused by the broker's new quantity. Recover and validate the actual report, preserving the original failure, before a later reconciliation can succeed.

This does **not** authorise inventing fills for splits, spin-offs, mergers or delistings. Position-changing non-fill events require separately approved corporate-action semantics (#38, #113), including Campaign quantities, frozen values and stops. Until that contract exists and its event has been applied, such a change is unverifiable and halts. The fill-driven invariant is not silently broadened here. Manual trades and broker liquidations likewise require supported execution history; an unexplained position never becomes a new Campaign by adoption.

> **Supplied for one case by [ADR 0023](0023-a-split-carries-its-cash-in-lieu-as-a-corporate-action.md) (2026-09-25, Proposed):** a split's cash in lieu is a journalled `market.corporate-action` of kind `split`. It reduces the most recent Units by one raw share each, at most one per Unit held, and credits its cash through the next snapshot. Every other position-changing corporate action still halts.
>
> **Supplied for two more cases by [ADR 0024](0024-symbol-changes-carry-instrument-state-and-dividends-are-cash.md) (2026-09-25, Proposed):** a dividend's exact posted cash is a journalled `market.corporate-action` of kind `dividend`, crediting the account through the next snapshot exactly like a split's cash in lieu — the case this ADR's own Context already names ("an overnight dividend with its exact posted amount... can reconcile without a halt"), now given an actual event. A symbol change is a journalled `market.corporate-action` of kind `symbol-change`: it changes no position, cash or order, only which instrument id the reducer's own state is filed under, so it is not itself a case this section's position/cash/order comparison bears on — but the "stable instrument identity" and "a symbol rename must not merge unrelated instruments" language earlier in this ADR is exactly the invariant ADR 0024's own guard enforces. Every other position-changing corporate action still halts.

Compare the **union of both position sets**, including instruments outside the trading universe and instruments in the bridge. An absent row means zero only when that side explicitly declares a complete snapshot. Aggregate broker lots by stable instrument identity and preserve the contributing rows; a symbol rename must not merge unrelated instruments. A broker-only position, an unexpected short or a fractional holding outside the whole-share contract halts rather than being omitted, netted away or rounded.

> **Amended 2026-09-24** (see *Amendment: a graduated response* below): an `unexplained` result whose scope the evidence bounds enters **Degraded**, not a halt. Only `unverifiable` evidence, or a discrepancy that cannot be bounded, enters **Halted**.

Apply the same two-set rule to open orders. Compare stable order identity, instrument, side, type, original/filled/remaining quantity, limit and stop prices, time-in-force and lifecycle state. Expected broker orders come from journalled acknowledgements and lifecycle events, not unsubmitted proposals. An unknown order, a missing protective order, or an unexplained cancellation or expiry halts even if positions and cash match. An acknowledged, journalled lifecycle transition can explain the change. Client construction, executor election, order fencing and venue-specific mechanics remain out of scope.

> **Amended 2026-09-24** (see *Amendment: a graduated response* below): an `unexplained` result whose scope the evidence bounds enters **Degraded**, not a halt. Only `unverifiable` evidence, or a discrepancy that cannot be bounded, enters **Halted**.

### Quantities and unexplained cash residuals both have zero tolerance

For each instrument, expected signed integer quantity must equal actual quantity exactly. One share is a real disagreement. For cash, use the verified anchor plus **all enumerated cash effects** through the cutoff: execution debits and credits, fees, posted interest, dividends, deposits, withdrawals and supported corporate-action proceeds. Compare like balance components in the same currency and on the same trade-date or settlement basis. Buying power, equity, settled cash and accrued but unposted income are not interchangeable balances.

Use exact decimal monetary values with declared currency and scale for the reconciliation contract, including the source values and any documented booking-rounding rule. Binary floating-point epsilon is not a monetary tolerance. Any rounding movement must be derivable from the identified transaction and the declared rule; it cannot be an arbitrary residual labelled rounding. Unsupported precision, basis or currency makes the observation unverifiable. No unexplained residual, positive or negative, is small enough to ignore.

This requires a cash projection independent of the broker snapshot being tested. The reducer's current `availableCash` cannot supply it. The projection's event coverage and the conversion to the existing sizing representation must be specified before implementation; dividends must not masquerade as deposits and change Notional Account semantics. ADR 0004's cash treatment of dividends and ADR 0010's cash timing remain authoritative.

Reconcile the previous-close balance at that close's cutoff before releasing the day's first strategy decision, and reconcile the current account separately. Journalled overnight movements bridge the two; they do not become previous-close cash. An intraday match never increases the day's sizing allowance. If a withdrawal or other reduction leaves less usable cash than the frozen basis assumes, block affected trading pending a separately approved availability rule; reconciliation success alone does not authorise spending unavailable cash. A correction to an earlier close is appended with its effective time and provenance, never backdated into an old journal or used to rewrite prior decisions.

### Reconcile at decision boundaries and while exposure can change

Require a successful reconciliation at startup and reconnect, before the first strategy decision of each trading day, after every accepted fill, and immediately before each order submission. Also reconcile on a fixed schedule while the account can change, including when no new bars or orders arrive. This catches an externally cancelled resting stop or manual transaction that would otherwise wait until the next decision. Any intervening fill, cash movement, corporate action or order lifecycle event invalidates a previous pre-submission check.

The after-fill check follows state application but precedes any further sizing, risk or exit decision, including the next Add proposal that today's fill handler can emit. Bookkeeping emissions describing the accepted fill retain that fill as their cause. Missing a required check blocks progress. Snapshot collection, input recording, validation and the resulting halt or success must be ordered before dependent decisions; no proposal is emitted while a discrepancy is unresolved. Exact placement in a future live processing loop is implementation work, not permission to retain the current immediate fill-to-Add behavior without this gate.

Positions, cash, orders and activity coverage must describe a coherent cutoff. Record component timestamps and source watermarks or equivalent consistency evidence; an HTTP success or similar receipt is insufficient. A fill racing a snapshot cannot be called explained simply because delivery is eventually consistent. A bounded collection attempt may obtain a coherent view while decisions are blocked; if it cannot, record `unverifiable` and halt. A stale matching snapshot is not success.

The fixed interval, maximum snapshot age and collection deadline are required, versioned operational configuration with no permissive defaults. Their numerical values remain open: the owner must set a maximum acceptable detection delay, then validate it against measured publication lag, consistency guarantees and API capacity of the selected integration. Until those bounds are chosen and demonstrated, this design does not clear the live gate. These are declared safety choices, not performance-fitted strategy parameters (ADR 0012).

### Reconciliation evidence is an input; its consequence is a decision

Introduce a versioned `account.reconciliation` input envelope under ADR 0015 and record it with `kind = input` under ADR 0017. **Reconciliation is an input, not a decision:** the broker observation cannot be recomputed during replay because the historical broker is no longer there. The payload must therefore retain the full comparison evidence, not an instruction to fetch it or a bare pass/fail flag.

The input contract contains:

- Account identity and scope, reconciliation ID, trigger, trading session, requested cutoff, observation and receipt times, component watermarks, completeness and freshness evidence, and collection failures with explicit missing components. An unavailable balance is not zero.
- Prior verified anchor ID/hash, processed-input boundary, configuration and policy versions, currency/balance basis and exact decimal conventions. The normal envelope retains producer, strategy version, configuration hash, sequence and causation/correlation provenance.
- Full believed, projected and observed position sets and cash components, including zero/absent distinctions; full believed and observed open-order sets and the attributes compared. Include source rows needed to verify normalisation, rather than retaining only a digest of an unavailable response.
- The ordered explanation list, with causing event IDs and hashes, source transaction identities, effective times, matched activity records, per-component effects and their application boundaries. Include each pre-bridge difference and post-bridge residual, including unchanged components so completeness can be checked. Referenced evidence must be retained with the journal or its hash-linked evidence bundle, under the same never-overwrite rule.
- The collector's recorded classification (`matched`, `explained`, `unexplained`, `unverifiable`) and structured reasons naming affected instruments, orders or cash components. This classification is a claim to validate, not authority to replace state or bypass a halt.

The domain recomputes the classification from this recorded evidence and its reconstructed state, without broker calls or wall-clock reads. A false claimed success fails closed. It emits a reconciliation-result decision with the validated classification, reasons, evidence reference and action (`continue` or `halt`). An unexplained or unverifiable result additionally reuses `strategy.engine.state` and `EngineStatePayload`, extending its closed reason set for reconciliation divergence or unverifiable evidence; `Detail` identifies the disagreement and `CausationID` points to the input. Return the halt alongside the error, following the existing engine contract. Both decisions are journalled before the run terminates; the input never claims an action was performed before the reducer performed it.

> **Amended 2026-09-24** (see *Amendment: a graduated response* below): an `unexplained` result whose scope the evidence bounds enters **Degraded**, not a halt. Only `unverifiable` evidence, or a discrepancy that cannot be bounded, enters **Halted**.

Replay feeds these same inputs and earlier causing events, reproduces the comparison/result and halt byte for byte, and stops at the same boundary. Stable ordering by instrument, currency/component and order identity, explicit event identities and recorded times make that possible. Unknown schemas fail closed. No snapshot lookup, “latest” policy or current broker status participates in replay.

A halt is terminal for that run, as the existing engine contract provides. No automatic resume follows a later matching snapshot, and callers must not continue using the halted reducer. Human resolution records the recovered or corrected causing events, the disposition of affected orders and approval to restart; a linked recovery run reconstructs state and passes fresh reconciliation before any trading decision. The old failed run remains intact. Durable retention of the input and terminal decisions before further live action is required; the existing end-of-run journal writer alone does not establish live crash durability (ADRs 0017 and 0018).

> **Amended 2026-09-24** (see *Amendment: a graduated response* below): an `unexplained` result whose scope the evidence bounds enters **Degraded**, not a halt. Only `unverifiable` evidence, or a discrepancy that cannot be bounded, enters **Halted**.

### Alternatives rejected

- **Adopt broker balances, even with an adjustment log.** This manufactures state without supported causing events, cannot reconstruct Campaign entry state or stops, and weakens replay into reproducing an unexplained overwrite. The broker establishes what it reports holding, not the history needed to trade it safely.
- **Halt on every change from the last snapshot.** This treats a fully recorded dividend or fill as a failure and makes routine operation unusable. Exact causal reconciliation admits these changes without tolerating unknown ones.
- **Absolute cash floor, equity percentage, or floating-point epsilon.** Each conceals a missing fill or debit below its threshold, including at small account sizes. Enumerated movements remove the reason for a magnitude allowance.
- **Equal net balances or checking only our holdings.** Offsetting missing activities can leave net cash unchanged; an unknown broker position or order can sit outside our list. Complete activity matching and comparison of both sets address different gaps and both are required.
- **Daily-only, after-fill-only, pre-order-only, or periodic-only checks.** Each leaves an avoidable gap: external changes between days, missed fills that never trigger a callback, resting orders while no new orders are submitted, or decisions between timer ticks. The combined schedule costs more calls but bounds these gaps.
- **Journal only the result as a decision, or trust a collector's pass flag.** Neither preserves independently checkable historical inputs. Replay must validate the evidence and reproduce the action, not consult today's broker or accept an assertion of safety.

## Consequences

- Routine supported, journalled cash movements reconcile without halting; unsupported or delayed activity reports can halt an otherwise healthy account. Availability is deliberately subordinate to knowing what is held. Halting cannot stop already-resting broker orders from filling and is not a liquidation or cancellation policy.
- More API calls, larger evidence records and decision latency are accepted. Coherent snapshots still leave an observation-to-action window; reconciliation does not solve order fencing or exclusive execution.
- Implementation requires an independently seeded cash ledger, exact monetary normalisation, complete activity and order-lifecycle contracts, and ordering that gates fill-triggered decisions. Corporate-action semantics (#38/#113), external cash movements (#105/#106) and integration evidence (#29/#30/#81) must settle the identified dependencies. No undocumented adjustment is a fallback for missing support.
- The owner must approve the opening-baseline/recovery procedure and timing bounds. Broker accounting documentation and recorded integration fixtures must establish balance basis, precision, effective-time handling, complete activity coverage and coherent cutoffs. A separate rule must settle intraday cash reductions against ADR 0010. These are explicit prerequisites to live readiness, not guesses embedded in this ADR.
- The implementation ticket must first test one-share discrepancies in both directions, broker-only holdings/orders, supported journalled delisting and dividend bridges, a residual of one smallest supported monetary unit in either direction, offsetting missing cash activities, duplicate or unapplied explanations, stale/incomplete snapshots, cancelled protective orders, and no proposal after an unresolved discrepancy. Recorded success and failure runs must replay byte-identically, including terminal decisions. This design-only ADR adds no implementation or tests.
- Reconciliation policy, opening evidence, successful checks, halts and recovery links are retained under the run's provenance (ADRs 0012, 0017 and 0018). Failure is evidence, never a record to replace with a later successful check.

## Amendment (2026-09-24): a graduated response, alerting, and a recovery procedure

Approved by the owner on 2026-09-24.

### Why

As written above, an `unexplained` or `unverifiable` result halts the whole engine, and a halt stops everything. That treats every discrepancy alike. A cash difference of a few dollars would stop the management of every open position, and a share-count mismatch in one instrument would stop the management of every other. Worse, it stops **risk-reducing** actions: raising a Protective Stop, or taking an Exit-Channel exit. So a halt during a fast market becomes a risk of its own. The owner's concern was exactly this: that stopping the system could itself cause harm, because it would no longer be acting on open positions.

The principle this amendment adopts: **a discrepancy is a reason to stop adding risk, not a reason to stop managing risk already held.**

### Engine states

This supersedes every earlier clause that halts on an `unexplained` result, and each carries a pointer to this section:
- "Both latter classifications halt the whole engine" (*Explain changes by causing events*);
- the halts for a broker-only position and for a missing Protective Stop (the two-set comparisons of positions and of orders);
- the `continue`/`halt` action set of the reconciliation-result decision, which becomes **`continue`, `degrade` or `halt`**;
- the first sentence of the paragraph beginning "A halt is terminal for that run".

Only `unverifiable` evidence, or a discrepancy the evidence cannot bound, still halts.

| State | Entered when | New entries and Adds | Instruments the discrepancy does not name | Instruments it names |
|---|---|---|---|---|
| **Normal** | stays Normal while every check is `matched` or `explained`; returns only through recovery (below) | allowed | managed normally | — |
| **Degraded** | `unexplained`, with a scope the evidence bounds: named cash components and/or named instruments or orders | **blocked everywhere** | **risk-reducing management continues** (below) | **frozen**, except restoring a missing Protective Stop (below) |
| **Halted** | `unverifiable`, or `unexplained` with a scope the evidence cannot bound | blocked | no order changes | no order changes |

**Risk-reducing management** means only actions that cannot increase exposure: raising a Protective Stop under the Stop Ladder, an Exit-Channel exit, and a Delisting Exit where supported. Nothing that opens or adds to a position is allowed while not Normal.

**Exits never add sell quantity.** A resting good-till-cancelled Protective Stop and a separately submitted exit order could both fill, and together sell more than is held. So an exit is never a separate order. Protective Stops are per Unit: each Unit has its own level (the Stop Ladder raises earlier Units' stops separately), and a partial stop-out is a real outcome. So each Unit holds **one** good-till-cancelled sell-stop for **that Unit's quantity**, and the total working sell quantity always equals the holding.
- **Taking an Exit-Channel exit** means **amending** each Unit's stop to the Exit Channel level wherever that level is higher, never submitting a second order.
- **Raising a stop** likewise amends that Unit's order.
- **An amendment the broker has not acknowledged** leaves the previous level in force, so every Unit keeps exactly one stop throughout.

This is the order model #29 must implement in every state, not only while Degraded.

**Frozen** means the system changes no order for that instrument. Its Protective Stop keeps working at the broker, because under the order-lifetime decision recorded on #29 it is good-till-cancelled. There is one exception. If the discrepancy **is** a missing or cancelled Protective Stop **on a position the journal holds a Protective Stop for**, the system **restores** it at that last journalled level. Restoring protection reduces risk; leaving a position unprotected while waiting for a human does not.

- **Restoration is single-owner, idempotent and attempted once.** The restored order's client identifier derives deterministically from the missing stop's own journalled identifier, so a retry can't place a second stop. And the system attempts restoration **once per incident**. A restoration that fails, is rejected, or is cancelled by a person is never retried automatically, so a stop placed by hand can't be duplicated by a later attempt. Only recovery re-enables it. The alert reports the outcome: restored, with the order identifier, or failed. A person places a stop by hand only when the alert says restoration failed or was not attempted, never alongside an attempt in progress.
- **No journalled stop, no automatic restoration.** A broker-held position the journal has no Campaign for, such as a manual trade, has no level the system could restore. Its alert marks it **UNPROTECTED** with no system-held level, and the runbook directs the decision to a person. The system never invents a level.

**Exits and restored stops are sized from the broker-reported quantity of the most recent reconciliation,** never from the believed quantity, and never from the reconciliation that *entered* the state, since a later stop fill may have reduced the holding since then. ADR 0019 already requires a reconciliation immediately before each order submission and after every accepted fill. The order is sized from that one, and those checks don't themselves leave Degraded. When the discrepancy *is* the quantity, the current broker figure is the only one that can't sell shares not held and so accidentally open a short.

**Working sell quantity never exceeds the holding.** Every reconciliation also compares, per instrument, the total quantity of working and pending sell orders at the broker (ours and any unknown ones) against the broker-held quantity. If it exceeds the holding, for example an unknown sell order alongside our stop, both could fill and open a short. The alert marks it **CONTAINMENT REQUIRED**, and the runbook's first step is to bring it back to exactly one stop covering the holding, before any recovery. The system doesn't cancel an order it doesn't recognise; containment is a person's decision, made immediately rather than at recovery.

**An affected order freezes its instrument.** A discrepancy that names an order, such as an unknown order or a cancelled stop, freezes that order's instrument as if the instrument were named, so no management runs against an instrument whose order state is unresolved.

**A cash-only discrepancy names no instrument.** It blocks entries and Adds, because sizing depends on cash, and leaves every position under normal management.

**Degraded and Halted are decisions, not side effects.** The reducer derives the state and its scope from the recorded reconciliation input, and emits them through `strategy.engine.state`. It does this without broker calls or wall-clock reads, so replay reproduces the state, its scope and every order change made under it, byte for byte.

**The schema change (ADR 0015).** `EngineStatePayload` moves to schema version 2, which adds the state `degraded` and fields naming the affected instruments, orders and cash components. Version-1 records stay valid under the version-1 rules: `halted`, with no scope, meaning the whole engine. A version-2 `degraded` record with no scope is invalid. Scope is never inferred from absence.

**Leaving Degraded or Halted is never automatic.** A later matching reconciliation doesn't restore Normal on its own; only the recovery procedure below does, with the owner's approval. This keeps the original rule that the system never quietly resumes after something it couldn't explain.

**The state survives a restart.** A crash or restart while Degraded or Halted resumes in that same state and scope, read back from the run's durable record, and a passing startup reconciliation doesn't return it to Normal. This requires the state to be durably recorded when it's entered, not only in an end-of-run journal. #151 (engine journal durability) is therefore a prerequisite of this amendment going live.

### Alerting

An alert is the only way a person learns the system has stopped adding risk, so it is a safety requirement, not a convenience.

- **Every transition out of Normal alerts immediately**, as does every transition between Degraded and Halted. While the state persists, the alert repeats at a fixed interval until the owner acknowledges it. Acknowledgement is recorded.
- **At least two independent channels**, chosen by the owner. A failed delivery is itself recorded and retried on the other channel.
- **A heartbeat, checked from outside the system.** A crashed or wedged system cannot send its own alert, so the system emits a regular heartbeat and an external monitor alerts when it stops. This dead-man's switch is what catches the failure the system cannot report itself.
- **Every alert states:** the state and when it began; what triggered it (reconciliation ID, classification and reasons); the affected instruments, orders and cash components; for each affected position, **each Unit's** journalled stop level and quantity and whether the system restored that Unit's stop (with the order identifier), failed to, or has none to restore (**UNPROTECTED**); what the system is **still doing**; what it has **stopped doing**; and a link to the recovery procedure.
- **The alert interval, the heartbeat interval and the heartbeat timeout** are operational configuration, set with the timing bounds this ADR already leaves to the owner, and validated in paper trading.

### Recovery

`docs/runbooks/reconciliation.md` is the procedure, with one path per discrepancy class. In outline, recovery always:

1. **acknowledges** the alert;
2. **identifies the cause** from the broker's own activity records, not from our state;
3. **records the missing causing event(s)** through a supported input: a recovered fill report, a manual trade, a corporate action, or a cash movement. It never adopts a balance and never edits history;
4. **decides the disposition** of any affected order, and records it;
5. **approves a restart:** a linked recovery run rebuilds state from the journal plus the recorded events, and must pass a fresh reconciliation before returning to Normal.

The failed run stays intact as evidence, as above.

### Consequences of this amendment

- `docs/architecture.md`'s "safe mode" means **Degraded or Halted** as defined here.
- Positions stay protected in every state: each has a good-till-cancelled stop at the broker, and a missing one is restored while Degraded.
- More behaviour to build and test than a single halt. The implementation ticket must add tests for:
  - a cash-only discrepancy leaving positions managed;
  - an instrument-scoped discrepancy freezing only that instrument;
  - a restored Protective Stop sized from the broker quantity;
  - an exit never exceeding the broker-reported holding;
  - no entry or Add while Degraded;
  - no automatic return to Normal;
  - byte-identical replay of every state transition and every order change made while Degraded.
- Alerting and the heartbeat monitor are live-readiness prerequisites, alongside the timing bounds. The alert channels are the owner's choice.

## Amendment (2026-10-15): one risk-reducing sale while Halted, for target-weight accounts

The owner decided (decision D6, `docs/live-trading/plan.md`) that a Halted
margin account running a target-weight strategy may still sell back to its
unlevered weight by itself. Without it, a Halted account at 147% exposure
would keep its borrowed exposure until a person acted. This is the one
exception to "no order changes" in the Halted row above. It does not apply
to the Turtle reducer, and changes nothing else in this ADR.

- **When:** the strategy's own exit fires, or the margin alarm is urgent
  (`docs/live-trading/plan.md`, section 5), while the account is Halted.
- **What:** one sell-only order per instrument, bringing exposure toward
  98% of net liquidation and never below it.
- **Sized from the broker, net of every working sell.** The quantity is
  the smallest of: what reaches 98% of IBKR's reported net liquidation
  value; and IBKR's reported holding minus the total
  quantity of every working and pending sell order IBKR reports for the
  instrument, ours and unknown ones. *(Refined by the 2026-10-17 amendment,
  reading 1: a third bound, holding − every working or pending sell − keep,
  is added; the quantity is the smallest of the three bounds, never below
  zero, and never more than the holding. The sale is sized from the latest
  broker snapshot only (latest by journal sequence, as reading 1 defines
  it). Where that snapshot's net liquidation or mark is not
  strictly positive, or it is stale (older than `MaxSnapshotAge`, or
  stamped later than the triggering event), the sale is
  withheld without consuming the attempt and re-evaluated later: reading 1.
  Where it cannot be read, the broker's values cannot be read and the
  attempt is consumed: reading 4.)*
  This keeps the invariant above
  ("working sell quantity never exceeds the holding"), so no combination of
  fills can open a short.
- **One price basis.** The 98% target is computed from a single IBKR
  account snapshot: its net liquidation value and its current mark for the
  instrument, taken together, never mixed with the previous close. The
  shares to keep are 98% of that net liquidation value divided by that
  mark, rounded up, so the sale itself never targets less than 98%. For
  example, with 147 shares, $4,700 borrowed and a mark of $90, net
  liquidation is $8,530; keeping 93 shares ($8,370, 98.1%) means selling
  54.
- **Price moves during the sale** shift the result a little either side of
  98%, because the order executes after the snapshot. That is accepted:
  the sale is not resized or repeated, and the account stays Halted for a
  person to review, with the resulting exposure in the fill alert.
- **No order when it cannot be checked:** if IBKR's holding, net
  liquidation value or open orders cannot be read, or any working sell is
  one the journal does not recognise (CONTAINMENT REQUIRED), no order is
  sent, and the alert asks the owner to act by hand.
- **Attempted once per incident**, with a client identifier derived from
  the incident, like a restored stop. A failed or cancelled attempt is not
  retried automatically.
- **Alerts** when the account enters Halted, when the sale is sent, and
  when it fills or fails. It is journalled like any other order, and the
  account stays Halted until recovery.
- **Tests** before use: the sale sized net of a working sell; no order with
  an unknown working sell; no order without readable broker values; one
  attempt only; byte-identical replay.

## Amendment (2026-10-16): the same sale for a Degraded account's named instrument

The owner decided that the one sale back to 98% in the 2026-10-15 amendment
also applies to a Degraded margin account running a target-weight strategy
when the instrument to sell is the one the discrepancy names. Without it,
a joint account boosted on SPY, Degraded over its SPY quantity, would keep
its borrowed exposure until a person acted, while a Halted account would
not. The owner's reasoning: the system should not keep risk it does not
need to.

- **When:** the strategy's own exit fires, or the margin alarm is urgent
  (`docs/live-trading/plan.md`, section 5), while the account is Degraded
  and the instrument to sell is one its discrepancy names.
- **What, sizing, price basis, checks, attempts, alerts and tests:**
  exactly as the 2026-10-15 amendment states. Its sizing reads IBKR's own
  holding, net liquidation value and open orders, never the journal's
  quantity, so the discrepancy does not enter it.
- **Nothing else changes.** Every other order on the named instrument stays
  frozen, and the account stays Degraded until recovery. It does not apply
  to the Turtle reducer or to the IRAs.

## Amendment (2026-10-17, owner decision, #352): eleven readings of the sale back to 98%

The owner accepted eleven readings that slice 5 (#323, PR #353) built where
the 2026-09-24, 2026-10-15 and 2026-10-16 amendments and ADR 0026 were
ambiguous. They are rules from here on. Readings 5, 6 and 11 belong to
ADR 0026 and are stated in its amendment of the same date; they are listed
here so that all eleven are in one place.

1. **Sizing never takes exposure below 98%, even with a working sell.** The
   sale's quantity is the smallest of three bounds: what reaches 98%
   (holding − keep); holding − every working or pending sell; and
   holding − every working or pending sell − keep. This replaces the two-bound
   sizing of the 2026-10-15 amendment and of ADR 0026 point 4. The smallest of the three bounds
   governs, and the sale never sells past the holding: no combination of fills
   can open a short. The one-line form max(0, holding − every working or
   pending sell − keep) holds only when keep is zero or more, which needs a
   net liquidation and a mark that are both strictly positive. The third
   bound means a sell of ours that is still working (Boost's released exit,
   say) and the sale together never take exposure below 98%, whichever
   fills. When the smallest bound is zero or negative (a holding of 147, a
   keep of 93 and working sells of 60 give -6), the quantity is zero and no
   order is sent; it is nothing to sell, not an attempt (reading 4).

   **The snapshot rule.** This is the one statement of which broker snapshot
   sizes the sale, when the sale is withheld instead, whether that spends
   the attempt, and when the sale is evaluated again; the other readings,
   the 2026-10-15 amendment and ADR 0026 points 4 to 6 cite it.
   - **The latest snapshot only.** "The latest snapshot" means, here and
     wherever these readings or ADR 0026 use it, the latest journalled
     `account.broker.snapshot` for the account at or before the triggering
     event's journal sequence, not by its broker timestamp. A snapshot
     journalled after the triggering event is not the latest for that
     evaluation, so live and replay pick the same one. The sale is sized
     from it. An older snapshot is never used in its place, whatever the
     latest one's condition.
   - **Unreadable:** the latest snapshot cannot be read, or none has been
     recorded yet. This counts as
     "the broker's values cannot be read" (reading 4): the sale is
     withheld, the attempt is consumed, and the alert asks the owner to act
     by hand.
   - **Stale:** the latest snapshot is readable but older than
     `MaxSnapshotAge`, or its recorded broker timestamp is later than the
     triggering event's journalled time. The sale is withheld
     (`risk.sale.withheld`, reason `stale-broker-snapshot`). Age is the
     journalled time of the event that triggers the evaluation minus the
     snapshot's recorded broker timestamp; the wall clock is never read, so
     replay withholds exactly where live did. When the triggering event is
     a broker snapshot itself (the retry below), its age is its own
     journalled time minus its own broker timestamp. A negative age (the
     broker's timestamp ahead of our journalled time, from clock skew or a
     malformed timestamp) is stale, never fresh, with no tolerance; the
     withheld payload records the skew. A snapshot is fresh only when its age is
     zero or more and not above `MaxSnapshotAge`. `MaxSnapshotAge` is an
     account configuration field, covered by the configuration hash;
     configuration validation rejects a missing, zero or negative value
     (ADR 0026 point 6, Execution). This reason is the gate's, distinct
     from the strategy decision's `stale-account-snapshot` (ADR 0026
     point 1).
   - **Not strictly positive:** the latest snapshot is readable and fresh,
     but its net liquidation or its mark for the
     instrument is zero or negative, so keep cannot be trusted. The sale is
     withheld (`risk.sale.withheld`, reason `non-positive-broker-values`).
     Freshness is checked first: a snapshot that is both stale and
     non-positive is withheld as `stale-broker-snapshot`.
   - **Readable, fresh and positive:** the sale is sized by the three bounds
     above.

   The two readable-snapshot withholds, `stale-broker-snapshot` and
   `non-positive-broker-values`, leave the engine state unchanged (a
   Normal account stays Normal), leave the alarm, the kill-switch
   engagement or the incident in force, and do not consume the attempt
   (reading 4). The owner hears of them through the `risk.sale.withheld`
   alert, not through a Halt; a stale withhold alerts only when it opens a
   stale episode (reading 10). **Retry:** while the condition that produced
   the withheld sale is still in force, the next latest snapshot that is
   readable, fresh and strictly positive re-evaluates the sale. The exit
   (reading 7) is in force until whichever comes first:
   - no refusal cause of reading 7 still applies to the instrument: the
     kill switch is not engaged, and the account is neither Halted nor
     Degraded on the instrument. The exit stays in force while any one of
     them applies, whichever of them fired it, so a hand-off from the
     engagement to an incident, or from an incident to the engagement,
     does not lapse the sale. An escalation from Degraded to Halted is the
     same incident (reading 3) and does not end it;
   - a `strategy.targets.decided` for the account with a `decided`
     outcome that no longer plans the sell that fired it. A `withheld`
     decision (ADR 0026 point 1) plans nothing and decides nothing, so it
     leaves the exit in force.

   The alarm is in force while it is urgent. **Lapse:** when neither holds
   and the sale is still withheld, the gate journals one
   `risk.sale.withheld` with reason `lapsed`, and the attempt remains
   unspent. The lapse is reported in the daily summary, not paged, because
   the condition that needed the sale has ended. On re-evaluation the gate
   applies this reading afresh and picks the tag by reading 2's order at
   that time, so a change of engine state or of the kill switch between
   the two evaluations re-tags the sale, and the new tag's one attempt
   applies. The incident or the engagement is a tag, not
   a trigger: neither alone produces or retries the sale.
2. **The kill switch does not suppress the sale.** While the kill switch
   is engaged (ADR 0026 point 3), Boost's exit firing or the Excess
   Liquidity alarm turning urgent still produces the one sale back to 98%,
   in any engine state. Each sale carries one tag, and each tag allows one
   attempt per instrument. The tag is a durable incident decision, an
   `operator.kill-switch` engagement or a `risk.margin.alarm` decision;
   ADR 0026 point 2's order identity and point 3's journalled-decision
   limit accept all three (ADR 0026 amendment, 2026-10-17). The gate picks
   the tag in this order: the incident's while the account is Halted, or
   Degraded on that instrument; otherwise, with the switch engaged, the
   engagement's; otherwise the urgent alarm's.
   Engaging again while engaged is the same engagement.
   Engaging the kill switch does not itself start an incident. While the
   engine stays Normal there is no incident, so the limit is the
   engagement's: one sale per instrument. Disengaging and engaging again
   is a new engagement and may send one more sale, even after an earlier
   attempt failed or was withheld (reading 4). The consequences:
   - Once the account is Halted, or Degraded on the instrument, the
     incident's tag applies and the incident limit (reading 3) is what
     stops repeats; re-engaging then sends nothing more for that
     instrument.
   - A sale attempted under the engagement before that does not use up the
     incident's sale. If the account then becomes Halted (or Degraded on
     the instrument) under the same engagement, one more sale goes under
     the incident's tag. "Once per engagement" is therefore not an upper
     bound across an incident.
   - While the account is Degraded on a different instrument and the
     switch is engaged, a sale on this instrument is still tagged with the
     engagement, so the engagement's limit applies, not the incident's;
     disengaging and re-engaging allows another sale while that incident is
     open.
3. **Degraded to Halted is one incident.** An incident runs from the first
   engine-state record that leaves Normal until recovery. Moving from
   Degraded to Halted does not start another, so there is one sale per
   instrument for the whole episode.
4. **A withheld sale counts as the attempt only in two cases: the broker's
   values cannot be read, or a working sell is unknown.** An unreadable
   latest snapshot is one way the broker's values cannot be read (reading
   1's snapshot rule). A released order always consumes the attempt. No
   other withhold does: not "nothing to sell" (below), and not reading 1's
   `stale-broker-snapshot` or `non-positive-broker-values` withholds, which
   leave the attempt unspent and are re-evaluated as reading 1 states. In
   the two cases that do count, the sale counts as attempted and is never
   retried automatically; the alert asks the owner to act by hand, and a
   later automatic sale could duplicate the owner's.
   "Nothing to sell" is not an attempt, and covers three cases as built:
   the snapshot holds none of the instrument; the holding is short (a short
   is never sold further; the alert asks for a person, but a later trigger
   plans again and withholds again); or the smallest bound of reading 1 is
   zero or negative, which includes working sells that already cover the
   excess as well as exposure already at or below 98%.
5. **The urgent Excess Liquidity sale goes once per urgent alarm** (ADR 0026
   amendment, 2026-10-17).
6. **Risk-reducing orders are not counted toward N** (ADR 0026 amendment,
   2026-10-17).
7. **"The strategy's own exit fires"** means a sell the strategy planned
   that the gate refused because the account is Halted, Degraded on the
   instrument, or under the kill switch. The state is read after the gate's
   own refusal, not before it: a sell refused for a limit breach (reading
   10's `broker-holding`, say) while the account is Normal degrades it on
   that instrument in the same check, and the sale back to 98% then fires
   at once.
8. **An unknown working sell is per instrument.** It blocks the sale on that
   instrument only, matching CONTAINMENT REQUIRED.
9. **"98%" is the configured `InvestedFraction`**, the unlevered weight (0.98
   for Boost). The sale keeps ceil(`InvestedFraction` × net liquidation ÷
   mark) shares.
10. **A sell above the broker quantity is refused, not resized.** The
    check applies to every sell, in any engine state, not only Degraded,
    and reads the latest broker snapshot only (reading 1 defines latest);
    an older readable snapshot is never used in its place. When there is
    no readable latest snapshot, the engine state decides (below).
    - **ADR 0019's sale back to 98%** follows reading 1's snapshot rule
      instead of this reading: withheld (`risk.sale.withheld`); unspent and
      re-evaluated for `stale-broker-snapshot` and
      `non-positive-broker-values`; the attempt consumed when the latest
      snapshot is unreadable.
    - **Any other sell:** the snapshot gates only the broker-holding check,
      a quantity no more than the latest snapshot's holding. Net
      liquidation and the mark are not read for these sells; they are used
      only by the sale's keep (reading 1). With a latest snapshot that is
      readable and fresh (age as reading 1 states, measured from the sell's
      `strategy.order.planned`, a negative age being stale), the gate
      refuses a sell above its holding (`broker-holding`) and adds the
      instrument to the Degraded scope (ADR 0026 point 3, limits table); a
      Normal account becomes Degraded on that instrument. When no snapshot
      has yet been readable or the latest is unreadable, no broker-holding
      check is possible yet. A Normal account releases the sell against
      the journal's account book. This rests on this ADR's own premise: a
      Normal account's book has been reconciled against the broker that
      session, since an unexplained difference would have made it
      Degraded, and the broker snapshot input is a later, additional
      check. So a stale snapshot is refused and a missing one is not: a
      stale snapshot is positive evidence that the broker view is out of
      date, while no snapshot is not. Only an account Degraded on another
      instrument reaches the `broker-holding` refusal: every other state
      (Halted, Degraded on this instrument, the kill switch engaged) has
      already refused the sell under reading 7, by the check order below,
      and the exit has fired. An older readable snapshot is never used in
      its place. When the latest is stale, the sell is refused
      (`risk.order.refused`, reason `stale-broker-snapshot`) and the
      engine state is left unchanged (a Normal account stays Normal).
      **Stale episode:** an episode for the account starts at the first
      `stale-broker-snapshot` refusal or withhold (reading 1) and ends at
      whichever comes first: the first journalled `account.broker.snapshot`
      that is fresh by its own journalled time (its age measured as for
      the retry in reading 1), or the first gate check, a refusal, a
      withhold or a release, whose latest snapshot is fresh as measured
      from that check's triggering event. One urgent alert is raised at the
      episode's start, carried by that first refusal or withhold record;
      there is no new event type. The reporter treats the episode's end as
      the clear, so a later outage opens a new episode and alerts again.
      Both ends are journalled records, so replay and a restart rebuild the
      episode from the journal identically and do not send the alert
      again. Episodes are ordered by the triggering event's journal sequence.
      A stale check opens an episode when no episode is open for the account and
      either no episode has yet closed or the check's triggering event comes
      after (by journal sequence) the event that closed the most recent episode;
      a stale check whose triggering event precedes that close belongs to the
      closed episode and opens none. A sell
      refused as stale is re-planned at the next decision, like any refused
      planned sell; it is not retried when a fresh snapshot arrives.
    - **Check order.** The engine-state and kill-switch refusals of reading
      7 (Halted, Degraded on the instrument, the kill switch) are evaluated
      before this reading's snapshot checks. A planned sell refused for one
      of those is the strategy's exit firing, whatever the snapshot's
      condition, so a stale snapshot never prevents the exit from firing;
      the sale it fires then meets reading 1's snapshot rule.

    The planner, not the gate, chooses quantities.
11. **New event types** `account.broker.snapshot`, `risk.margin.alarm` and
    `risk.sale.withheld` are adopted (ADR 0026 amendment, 2026-10-17).
    `account.broker.snapshot` carries the observed side that
    `account.reconciliation` also needs; #227 folds one into the other or
    references it.

## Amendment (2026-10-17, accepted by the owner 2026-10-18): operator instructions count in the order of the times they state

This amendment adds one rule to the kill-switch readings above (readings 2
and 7). The engine applies it now. **The owner accepted it on 2026-10-18**; it is
an owner decision.

The owner records each instruction (an `operator.kill-switch`, or an
`operator.rebalance.cleared` for an IRA) with the operator tool. The tool
stamps the instruction with the time the owner gives with `-at`, or the time
the tool ran. The engine delivers it before the adapter's next input.
Because `-at` can state an earlier time than the one the instruction was
written at, arrival order and stated order can differ. The rule:

1. **Instructions count in the order of the times they state**, not the
   order they arrive in. The engine delivers the instructions waiting in its
   inbox in the order of their stated times.
2. **A backdated kill switch is journalled and refused.** A kill switch
   stating a time earlier than the newest kill switch already applied for
   its account is recorded in the durable log and refused, with no effect,
   naming the later one. A stale `released` therefore never undoes a later
   `engaged`, and the log still shows it was tried. One stating the same
   time is applied, the later recorded winning. A clearance's stated time
   orders it among the instructions waiting in the inbox (rule 1), and so
   puts it before or after a kill switch delivered with it. It is not
   checked against an earlier clearance's time: a clearance is applied when
   it names the rebalance in progress and exactly its orders not terminal,
   whatever time it states. A stale clearance names a rebalance no longer in
   progress, or orders already terminal, and is journalled and refused.
3. **An instruction stating a time more than five minutes ahead of the
   engine's clock is refused before delivery.** It is never journalled: it
   would put every instruction stating the true time before it. Five minutes
   is the difference allowed between the tool's clock and the engine's; an
   instruction within it is delivered. This holds for every operator
   instruction, kill switch or clearance.

Rule 2 changes what the gate decides from a journalled input, so it is a
rules change under ADR 0016: target-weight RulesVersion 1.12.0 (1.10.0 was
reserved for it, but 1.11.0 landed first). The owner's manual states the rule
in [the operator chapter](../manual/operator.md).

## Amendment (2026-10-18, owner decision): the reading after the close counts as the close

The owner decided on 2026-10-18 that one IBKR account reading taken after a
session's close and before the next open stands for the account **as of that
close**, when the conditions below hold. Why it can equal the close: between
the close and the next open the account's positions and open orders change
only through an order that is still working, an execution or a corporate
action IBKR books overnight (a split, a spin-off), and its cash also through
postings that need no order at all (interest, a fee, a dividend). An order
or an execution shows in IBKR's lists, but only to a client that can see it:
IBKR shows other clients' and TWS's executions only to the Gateway's Master
API client ID. A corporate action or a posting shows in neither. So the
adapter checks both kinds of change, the second without relying on any
client ID:

1. **IBKR's lists.** No order is working or pending at IBKR for the account
   (from any client, cancel-pending included), in either account read or in
   the last open-order list IBKR returns, which the reading's cutoff covers:
   an order report or an execution arriving during that last list makes the
   adapter read again. No execution has reached the account at or since the
   close (IBKR's executions, asked for every client, reach back only to
   midnight, so the read must also be on the close's own New York date). An
   execution stamped at the close's own second counts: the engine applies a
   fill stamped in a book's second to that book, so a closing-auction fill
   the close's snapshot already showed would be counted twice.
2. **The previous close, with no client ID.** The holdings read are the
   previous close's book adjusted by the journalled fills since it, share
   for share, and the total cash is that book's cash adjusted by those
   fills' values and commissions, within a bound: **1% of the net
   liquidation at the close**, room for a posting such as a day's interest
   or a dividend (SPY's largest quarterly dividend on a holding at 147% of
   net liquidation exceeds 0.5%). The previous close's book is the latest
   of two, found back through the chain of runs the Run approvals name
   (however old, never only the run now running): the last committed
   `account.snapshot`, its cash the reconciliation recorded from the same
   reading; or the **starting book** a Run approval states (schema version
   3; ADR 0027, amended 2026-10-18), the holdings and total cash the owner
   checked in IBKR. An account's first run must be approved with one; a
   fresh run's approval may state one, the book after a change no fill
   explains (a transfer, a deposit) that the owner has checked. So a fresh
   run's first batch is compared with the previous run's last close, and a
   first run's with the owner's book; IBKR's book is never adopted
   unchecked. The day's difference is recorded on the broker snapshot as
   `cash_drift`, and beside it `unexplained_cash`: IBKR's total cash less
   the cash last explained (the latest reconciliation whose cash the engine
   compared and explained, or the starting book) and every fill since, so a
   posting within the bound each day is still seen as it adds up; the daily
   summary shows both. Any difference in holdings, or cash beyond the
   bound, fails the condition. This catches a trade another client made
   that IBKR did not show this client, a transfer, a split booked overnight
   and a large cash posting.
3. **A valuation at the close.** Every holding has an official close in the
   batch (below), IBKR's own net liquidation at the read is positive (the
   margin alarm's basis, stated beside the valuation), and the engine holds
   no snapshot already stamped at or after that close.

**Residual risk, and its mitigation.** A round trip another client makes
after the close (a sale and a repurchase of the same shares) leaves the
holdings unchanged and shows only as cash drift: a loss and commissions
within the bound pass condition 2, and only IBKR's execution list
(condition 1) shows the trades. The adapter asks IBKR for the executions of
every client, and IBKR shows other clients' executions only to the
Gateway's Master API client ID, so the runbook's precondition is that the
Gateway's Master API client ID is set to the adapter's client ID
(`docs/runbooks/first-paper-order.md`, A4); with it, such a round trip is an
execution since the close and fails condition 1. IBKR's API offers no way
to confirm that setting, so it stays a precondition, and the unexplained
cash in the daily summary is the owner's check on it.

**The engine checks the stamp it is given.** A version 2 snapshot stamped
as of the close is refused unless it states IBKR's own net liquidation at
the read as a positive figure whenever net liquidation was read (an absent
figure is not zero, so it can never fire the sale back to 98%), and unless
its `cash_drift` is within the same 1% of its net liquidation
(`event.CashDriftBound`): a snapshot that states contradictory evidence is
never a usable close.

Then the adapter stamps the `account.broker.snapshot` (schema version 2) with
the close as its as-of time and records the true read time beside it
(`read_at`) as evidence, so this ADR's snapshot rule (reading 1's age,
measured at the trigger's journalled time) finds it fresh for the sell
planned at that close.

**One price basis per rule.** A snapshot as of the close is valued at the
close: each holding is marked at the delivered bar's official close, and its
net liquidation, in both the broker snapshot and the engine's
`account.snapshot`, is the total cash plus each holding times that close.
This keeps "One price basis" (amendment of 2026-10-15) for the sale back to
98%, which takes net liquidation and mark together, and for Boost's sizing
and limits, which take the account snapshot's net liquidation with the
previous close as the reference price. IBKR's own net liquidation at the
read is recorded beside it (`read_net_liquidation`) as evidence. Excess
Liquidity stays IBKR's reading at the read, recorded as such: only the
margin alarm uses it, and the alarm divides it by IBKR's own net liquidation
at the same read (`read_net_liquidation`), never by the close's. A snapshot
stamped at its read time is IBKR's own reading throughout: net liquidation
IBKR's, each holding marked at IBKR's last price at the read (no mark when
IBKR gives none, so the sale back to 98% is withheld and the owner acts by
hand).

If a condition fails, the adapter stamps the true read time and states every
reason (`not_as_of_close`); the snapshot is then later than the close, which
the rule counts as stale (a negative age), and the engine's own account
snapshot, stamped the same way, withholds the session
(`stale-account-snapshot`): fail closed, and the reporter pages the owner
once with the reason.

**Reconciliation with every reading.** The adapter sends an
`account.reconciliation` from the same reading with every daily batch, ahead
of the batch's account snapshot once the engine holds a book: the engine
classifies IBKR's positions against the book it holds and the fills
journalled since, before the new snapshot replaces that book, so holdings no
fill explains are unexplained (Degraded on the instrument, or Halted when
unverifiable), never adopted by the next snapshot. A run's first batch,
before which the engine holds no book, carries it behind its snapshots, as
the start-of-run reconciliation (ADR 0025 point 5), once the adapter has
found the reading to be the previous close's book (condition 2): a reading
that differs from an earlier run's last close or from the starting book, or
has neither to compare with, sends its reconciliation alone instead, its
snapshots withheld, where the engine, holding no book, classifies it
unverifiable and halts, so the difference is never adopted, and no account
snapshot of that reading is committed for a fresh run's first reading to be
compared with later. The reconciliation of a start or
a reconnect is read only once every execution IBKR replayed has its
commission and its fill is recorded, and goes to the engine before any
stream input, so no fill reaches the engine after a reconciliation that
already shows its shares; a read whose positions or cash IBKR does not give
is tried again at most three times in a row, as the batch's reading is, and
then the adapter stops for a person rather than hold the day's stream behind
it in silence. Every reconciliation is stamped with its true read time,
never earlier than the book it is compared with.

Target-weight rules version 1.19.0 reads the new schema versions, and with
them the margin alarm's basis above and the price band's comparison (ADR
0026, point 3, amended 2026-10-18), so a journal holding one does not replay
under 1.18.0.

## Amendment (2026-10-21, #490): the engine checks every close snapshot's holdings against its own fills

**Why.** The book a reconciliation starts from is the latest
`account.snapshot` (*Reconcile with every reading*, above). Until this
amendment the engine took each new snapshot as its book without checking it,
so holdings that changed during a session through a trade the engine did not
place (one made by hand, or by another client), or through a fill it was never
given (a late execution the adapter passed over), arrived in that session's
close snapshot as if they had always been so. Every later reconciliation then
compared IBKR with a book that already held the difference. A difference that
only the owner's acknowledgement records is not a check.

**What the IBKR adapter already checks (the amendment of 2026-10-18), and
what it leaves to the engine.** The adapter checks a reading taken after the
close before it stamps it as the close: its holdings must be the previous
close's book (the latest committed account snapshot back through the chain
of runs, or the starting book a Run approval states) adjusted by the
journalled fills since, share for share (condition 2). A reading that fails
is stamped at its read time and the session is withheld; a run's first
reading that fails is sent as its reconciliation alone, which the engine,
holding no book, classifies unverifiable. Every batch also carries an
`account.reconciliation` from the same reading, ahead of its snapshots once
the engine holds a book, which the engine classifies against its book and the
fills since. Together these catch an intra-session trade in the IBKR flow.
But they are the adapter's checks: the engine adopted whatever snapshot it was
given, so its safety rested on the adapter sending a reconciliation with every
snapshot, from the same reading, and on the adapter's own comparison being
right.

**Decision: the engine checks the snapshot itself.** Every
`account.snapshot` after a run's first is compared, before it becomes the
book, with the book in force adjusted by every change journalled since it
(fills, dividends and splits; *Explained means*), exactly as a reconciliation
is classified, with the snapshot's holdings as the observation:

1. **The same window and the same zero tolerance.** A change counts when it
   is stamped from the book's second up to, not including, the snapshot's: a
   fill stamped in a snapshot's second post-dates it, the convention every
   book follows, so a snapshot that already holds such a fill states holdings
   no fill explains, and is a difference (the safe side). The positions
   compared are the union of the book's, the snapshot's and those a change
   touched. One share counts.
2. **Holdings only.** The snapshot's cash is not compared: it moves with
   postings no event records yet (interest, fees), which the adapter's drift
   bound (condition 2, 1% of net liquidation) and the daily summary's
   unexplained cash watch.
3. **A difference is unexplained.** The engine journals a
   `risk.reconciliation.result` (schema version 1, unchanged) whose input is
   the snapshot (`input_id` its envelope ID) and whose reconciliation ID is
   `close-snapshot:<snapshot envelope ID>`, and the account goes Degraded on
   exactly the instruments that differ (`strategy.engine.state`, reason
   `reconciliation-unexplained`), as for any unexplained reconciliation. A
   change no book can apply (a delisting, a merger, a symbol change) makes the
   check unverifiable, and the account Halted (`reconciliation-unverifiable`).
   The reporter's reconciliation alert pages the owner, urgent and repeated
   until acknowledged, and the engine-state record the check caused is the
   same incident. Recovery is the runbook's, as for any reconciliation.
4. **The snapshot still becomes the book.** It is the book the planner sizes
   from (ADR 0026, point 2), as before; the Degraded scope is what stops the
   engine acting on the difference, and leaving it is never automatic.
5. **Nothing is journalled for a check that continues**: a snapshot the fills
   explain is the ordinary close, and every such journal keeps its bytes.
6. **One difference, one report.** A snapshot whose holdings are exactly the
   positions a reconciliation already classified against the same book is not
   classified again: the adapter sends each reading's reconciliation ahead of
   its snapshot, and that result, matched or not, already stands for those
   holdings.
7. **Not the run's first book.** A run's first snapshot has no book in the
   engine to be checked against; the adapter checks that reading against the
   previous close or the starting book (condition 2), and the Run approval
   states the starting book.

**What this does not cover.** A difference that arises and is reversed within
one session (a round trip by another client) leaves the holdings unchanged and
shows only in cash, which this check does not compare (the residual risk of
the amendment of 2026-10-18). The after-fill, pre-order and scheduled
reconciliations, the comparison of open orders and cash activity matching
remain unbuilt.

This changes what the engine decides from journalled inputs, so it is a rules
change under ADR 0016: target-weight RulesVersion 1.20.0. No schema changes.

