# ADR 0019: Broker reconciliation is an input, and unexplained differences halt

- Status: Proposed (amended 2026-09-24: graduated response, alerting, recovery)
- Date: 2026-09-17

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

**One working exit order per position.** A resting good-till-cancelled Protective Stop and a separately submitted exit order could both fill, and together sell more than is held. So the two are never separate orders. For a long position, the Protective Stop and the Exit-Channel exit are both "sell if price falls to *X*". They are held as **one** working sell-stop at the higher of the two levels, and "taking the exit" or "raising the stop" means **amending that one order**, never adding another. An amendment the broker has not acknowledged leaves the previous level in force: the position keeps exactly one stop throughout. This is the order model #29 must implement in every state, not only while Degraded.

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
- **Every alert states:** the state and when it began; what triggered it (reconciliation ID, classification and reasons); the affected instruments, orders and cash components; for each affected position, its journalled stop level and whether the system restored it (with the order identifier), failed to, or has none to restore (**UNPROTECTED**); what the system is **still doing**; what it has **stopped doing**; and a link to the recovery procedure.
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
