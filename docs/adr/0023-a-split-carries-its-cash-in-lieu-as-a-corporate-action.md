# ADR 0023: A split carries its cash in lieu as a corporate action, and reduces the most recent Units first

- Status: Proposed
- Date: 2026-09-25
- Relates to: ADR 0004, ADR 0005, ADR 0009, ADR 0015, ADR 0016, ADR 0017, ADR 0019, ADR 0020, ADR 0021

## In plain English

*A summary for reading the rest of this ADR against. Where the two differ, the detailed text below governs.*

When a stock splits, the broker converts every old share into new ones. If the conversion leaves a fraction of a share, the broker keeps the fraction and pays its value in cash instead: **cash in lieu**. The engine never counts fractional shares, so after such a split the broker can hold one share fewer than the engine's Units say, plus a small amount of cash.

Until now that difference stopped the run, which was correct: the engine never adopts the broker's number (ADR 0019). This ADR gives the difference a recorded cause instead. The split is journalled as a `market.corporate-action` input of kind `split`, stating the ratio, the shares lost and the cash paid. The engine then takes one share off each affected Unit, **the most recent Unit first**, records why, and re-states those Units' sell orders at their new size. Any other difference still stops the run.

## Context

The 2003–2014 AAPL LEAN acceptance run stopped at AAPL's 7-for-1 split of 2014-06-09. LEAN's factor file stores the split factor rounded (`0.1428572`, not exactly 1/7). LEAN divides the holding by that factor and truncates it: 3,516 raw shares became 24,611, not 24,612, and the 0.99 of a share left over was paid as cash. The engine's Units, converted at the exact whole ratio, were 24,612 raw shares. The adapter's split check stopped the run, as it must when LEAN and the engine disagree about a holding. The earlier 2-for-1 of 2005-02-28 showed the same artefact without losing a share: 11,056 shares became 22,112 and $2.75 of cash.

This is mainly an artefact of backtest data, since real brokers apply exact split ratios. But the same shape arises live whenever a broker pays cash for fractional shares, for example on a split whose ratio does not divide a lot, or on a reverse split. ADR 0019 already names corporate actions as a cause of divergence, and says a position-changing non-fill event needs separately approved corporate-action semantics before it can explain anything. Until then such a change is unverifiable and halts. This ADR supplies those semantics for one case: a split's cash in lieu.

### What a split already changes in the engine, and what it does not

ADR 0004, as amended, keeps a Campaign's money in the split-adjusted view its fills are priced in. That view is adjusted for every split, later ones included. So a split changes none of the engine's levels, quantities or N. It changes only how many split-adjusted shares one raw share is, and the adapter already carries that across (`adapter/lean/orders.py`, `OrderDesk.apply_split`). The engine therefore does **not** need a split event to re-price anything. The event has to carry only what the split did *not* do exactly: the whole raw shares the broker could not deliver, and the cash it paid for them.

The design notes from `market.corporate-action`'s first integration into `internal/fills` settle two further constraints:

1. `internal/fills` learns its resting-order book entirely from the reducer's emissions, and its corporate-action case has no effect on the book. A split must keep that true: the fill simulator must never branch on a corporate action's kind.
2. `event.CorporateActionPayload` carries no ratio and no cash, so it needs a new schema version and an upcaster (ADR 0015).

## Decision

### 1. The event: `market.corporate-action`, kind `split`

`event.CorporateActionPayload` moves to schema version **2**. Kind is still a closed set, now `delisting` or `split`. A split states:

| Field | Meaning |
| --- | --- |
| `instrument_id`, `effective_at` | as for a delisting: which instrument, and when the split took effect |
| `new_shares`, `old_shares` | the split's ratio: `new_shares` new shares for every `old_shares` old ones (7 and 1 for a 7-for-1) |
| `engine_shares_per_raw_share` | how many of the engine's split-adjusted shares one raw share is **after** the split: the whole number the adapter already converts by |
| `raw_shares_lost` | the whole raw shares, after the split, that the broker holds fewer than the engine's Units at the exact ratio: the shares paid as cash |
| `cash_in_lieu` | the cash the broker paid for the fractional shares, in the account currency |
| `currency` | that currency |

A split with `raw_shares_lost` above zero must carry `cash_in_lieu` above zero. A share taken away with nothing paid is not cash in lieu, and a producer that states one has a different corporate action to report. A delisting carries none of the split fields. Validate rejects either kind carrying the other's fields.

**The upcaster (ADR 0015).** A schema-1 record could only ever be a delisting, since schema 1's Kind set held nothing else. `event.UpcastCorporateActionPayload` reads a schema-1 record as the schema-2 delisting it is, and rejects a schema-1 record that is not one. It rejects any schema it does not know, older or newer. Every reader of the event uses it: the reducer, and `cmd/backtest`'s re-run.

### 2. The rule: most recent Unit first, one raw share each

On a split with `raw_shares_lost` = *k*, the engine takes **one raw share** (`engine_shares_per_raw_share` of its own shares) off each of the *k* most recent Units the Campaign holds, starting with the most recent. A Unit is "more recent" when it was added later, which is its Unit index.

Why this rule:

- **One share per Unit** is the shape of the artefact. A broker pays cash for a fraction of a share, and a fraction is less than one. Each Unit's own position can be short by at most that fraction. After the broker rounds the whole holding down, the shortfall can therefore be at most one whole raw share per Unit.
- **Most recent first** is deterministic and needs no input the event does not carry. It also reduces the Unit whose entry is highest, and so whose stop is highest, first. That Unit has least open risk, so it is the least consequential to shrink. A different order would change nothing about the account, only which Unit's journal line shows the reduction. The rule exists so that choice is never left to a producer.

### 3. Only a shortfall that arises at that split, and is smaller than one raw share per Unit

The reducer accepts a split's shortfall only when **all** of the following hold. Anything else fails closed, as any unexplained difference must (ADR 0019).

- **It arises at that split.** The shortfall is carried by the split's own event, effective between Sessions, and not before the instrument's last completed bar or any fill the Campaign has already accepted. Each split of an instrument applies once. A second split with the same or an earlier `effective_at` is refused, not applied twice. Every later split of the same instrument applies on its own terms. No split, and no other input, can reduce a Unit by any other route.
- **It is smaller than one raw share per Unit.** `raw_shares_lost` is at most the number of Units the Campaign holds, and every reduced Unit keeps at least one share. A larger shortfall is not rounding. It is a missing fill, a manual trade or a broker error, and it halts.
- **Something is held.** A split stating a shortfall or cash for an instrument with no open Campaign halts, because no Unit the engine holds could have produced it. A split with no shortfall and no cash changes nothing and is recorded without a decision. So is one for an instrument the engine has never seen.
- **The cash is in the account's currency**, once an account event has pinned it.

On the adapter's side, the same rule is enforced where the shortfall is measured (§6).

**This does not adopt a balance.** The engine does not copy the broker's holding. It applies a recorded, typed, validated cause through a deterministic rule, and it still halts if the holding differs by anything that cause does not explain. That is ADR 0019's "explained" standard: an enumerated event recorded before the comparison, with a stable identity, an exact effect and a supported transition.

### 4. What the engine records and changes

The reducer emits, in one transaction and in this order:

1. **`strategy.campaign.cash-in-lieu`** (`event.CampaignCashInLieuPayload`, schema 1). It names the Campaign and the corporate action, restates the split's terms, and lists each reduced Unit with its quantity before and after, most recent first. It states the Campaign's holding before and after. It validates exactly: one reduction per raw share lost, each exactly `engine_shares_per_raw_share`, in strictly descending Unit order, summing to the holding's change. Its Rule is `campaign.cash-in-lieu.most-recent-units-first` and its ADR is this one.
2. **`strategy.exit-order.set`** for each reduced Unit, at its unchanged level and source and its new quantity. The Exit Orders of a Campaign's held Units still cover exactly its holding (CONTEXT.md: "Exit Order").

A split with no shortfall but some cash (the 2005 case) emits only the first, with no reductions. A split with neither emits nothing.

The Campaign's whole-life figures account for the lost shares as a partial disposal at the cash paid. The shares lost join the Campaign's closed quantity at their Units' own entry prices, and the cash, divided by DollarsPerPoint, joins its closed exit value. They do so exactly as a partial stop-out's shares do (`campaignState.lifeAggregate`). So the Campaign's final `strategy.campaign.exited` reports the whole life, lost shares included, and its realised result includes the cash. Nothing else moves. Unit count, frozen N and Unit size (ADR 0006), every Protective Stop, the Add Ladder and every proposal are unchanged. An Add still proposes the Campaign's frozen Unit size. An outstanding exit proposal keeps the quantity it proposed, and its fill is checked, as always, against what the Campaign holds when the fill arrives.

**Cash and the ledger (ADR 0020).** The cash is credited to the account's ledger, the one that states `account.snapshot`: `internal/fills`' simulated account in a backtest, and the broker's own cash, which the LEAN adapter reports, in a LEAN run. It reaches the reducer's spendable cash the way every credit does under ADR 0020: through the next snapshot as of a previous close, never within the decision bar. Sale proceeds and deposits follow the same rule. The reducer itself records the credit in the decision above and does not add it to spendable cash, so no credit can be spent twice or early.

### 5. The fill simulator learns it from the reducer's emissions

`internal/fills` handles `strategy.campaign.cash-in-lieu` like any other decision it observes. It shrinks each named Unit from its `quantity_before` to its `quantity_after`, failing closed if its book disagrees. It removes the shares from the simulated account's holding and credits the cash. The `strategy.exit-order.set` decisions that follow then re-rest those Units' Exit Orders at the new quantity through the existing path. `fills.Simulator.observe` still treats `market.corporate-action` itself as having no effect on the book, whatever its kind.

`cmd/backtest` can declare a split in its `-corporate-actions` fixture. Every split for an instrument is delivered, not only the first. Several actions due at one boundary are delivered in a total order: effective time, then instrument, then kind, then the whole payload. The file's order therefore never decides the outcome.

### 6. The LEAN adapter derives the shortfall and publishes it

At a split LEAN reports as having occurred, while LEAN holds a position, the adapter (`OrderDesk.apply_split`):

1. takes the new whole ratio, as before;
2. derives what LEAN should hold at the exact ratio: the engine's Units at the new ratio. It also derives what LEAN held before the split: the Units at the old ratio;
3. accepts LEAN's holding only if it is exactly that exact-ratio figure, or short of it by at most one raw share per held Unit **and** equal to LEAN's own truncation of the pre-split holding divided by the split factor. The second test ties the shortfall to this split's own rounding, so a share missing for any other reason is not mistaken for one;
4. computes the cash as LEAN pays it: the fraction left over, times the split's reference price, times the factor. This matches the observed 2005 split ($2.75 on 11,056 shares; $0.25 on 1,000);
5. publishes the `market.corporate-action` split, and holds the reducer's reply;
6. at the 00:01 pre-session check, when LEAN has split its open orders, amends each reduced Unit's Exit Order to the engine's new quantity. LEAN splits each open order on its own and truncates it with the same rounded factor. At the 2014 split every Unit's order lost a share while the holding lost only one. So any working order LEAN left within one raw share of the engine's figure at the new ratio is amended to exactly that figure, reductions before increases so that working sells never exceed the holding. LEAN accepts a quantity amendment at once and applies it at the start of the next time step, before that session's fills (observed on the pinned image). The adapter reads the quantity an order will work from LEAN's own update requests, not from the ticket, which lags. The next slice's check amends nothing and verifies what LEAN made of the requests. A difference of more than one raw share on any order, an amendment LEAN refuses, or any other difference stops the run as before, and the failure names LEAN's own reason.

The adapter publishes a split whenever LEAN held a position across it, including one with no shortfall and no cash, so a LEAN run's journal records every split that touched a held position.

### 7. Versions

- `market.corporate-action` → schema 2, with the upcaster above.
- New decision `strategy.campaign.cash-in-lieu`, schema 1.
- `strategy.RulesVersion` 1.11.0 → **1.12.0**. Given the same inputs, a split with cash in lieu now changes a Campaign's Units and emits decisions where the older build refused the input, so the two builds do not replay each other's journals (ADR 0016).

## Alternatives rejected

- **Round-trip the Units' raw quantities through LEAN's own factor, and accept LEAN's figure within that tolerance.** This makes the engine's position depend on a vendor's rounding of a number the engine never sees, and it has no live counterpart: a broker's cash in lieu is a statement, not a factor. It also leaves the cash unexplained for ADR 0019.
- **Reduce the oldest Unit first, or spread the shortfall pro rata.** Pro rata would produce fractional shares, which is exactly what cannot exist. Oldest-first is as deterministic as most-recent-first but reduces the Unit with most open risk, whose stop is lowest. It has no advantage to set against that.
- **Let `fills` apply the split itself** when it observes the corporate action. That would make the simulator branch on a corporate action's kind and keep a second, independent account of the Units' quantities. The book would no longer be learned only from the reducer's emissions.
- **Credit the cash to spendable cash at once.** ADR 0020 credits only through a previous-close snapshot, because an intraday credit could fund an order the bar's unknown sequence did not allow. Cash in lieu is a credit like any other.
- **Adopt LEAN's holding after the split.** Rejected by ADR 0019 for every case: it manufactures state without a cause.

## Consequences

- The 2003–2014 AAPL acceptance run can carry the 2014 7-for-1 split across instead of stopping, and must be re-run to show it.
- A split whose shortfall exceeds one raw share per Unit, or does not match LEAN's own truncation, still stops the run. So do a split to a ratio that is not whole, and a reverse split, in the adapter, until a corporate-action contract for those exists. The engine's event already admits any ratio, so the adapter is the only place to change.
- Live trading needs the broker's own cash-in-lieu statement to be journalled as this event, with its source transaction identity, before ADR 0019's reconciliation can use it as an explanation. The LEAN adapter's derived figure is a backtest producer's, and is not that evidence.
- Every journal with a split that touched a held position gains a corporate-action input, and usually one decision. Journals without one are unchanged except for the rules version.
- ADR 0019's paragraph on position-changing non-fill events now points here for a split's cash in lieu. Other corporate actions (spin-offs, mergers, reverse splits' own adapter support) remain unsupported and halt.
