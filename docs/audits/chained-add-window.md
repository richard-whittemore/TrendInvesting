# Fill-chained Add window

Investigation for #211, 2026-09-24, written against RulesVersion 1.10.0,
before the owner's decision. It explains why relaxing the adapter's
staleness check alone places the chained order but cannot let it fill under
that expiry contract.

**Outcome.** Richard chose the second option below on 2026-09-24 (option B):
a fill-chained Add stays valid for one additional Session. ADR 0011 and ADR
0021 §7 record the amendment, RulesVersion 1.11.0 implements it, and
`strategy.add.proposed` schema 3 states the window as `valid_for_sessions`.
The adapter now places a fill-chained Add from the fill's reply, and the
real-engine end-to-end test shows it fill. The rest of this document is the
pre-decision analysis, kept as it was written; its schema numbers and
coverage figure are those of the build it examined.

## The existing contract

Let `B` be the last completed bar the engine received for the instrument,
`P` its preceding bar, and `N` its next bar. These are trading observations,
not calendar-day offsets.

- `evaluateAdd` in `internal/strategy/campaign.go` uses `B`'s high to test
  whether the rung was reached. All three callers (session close, opening
  fill, Add fill) use the same stored bar. The proposal's `period_end`,
  envelope `event_time`, and ID name **B**, even when a later fill caused it.
- The pending proposal's internal `earliestFillAt` is **P.period_end**.
  `applyAddFill` requires `filled_at > earliestFillAt`, and also requires
  `filled_at >=` the previous Unit's actual fill timestamp. Equality between
  successive Unit fills permits several fills in one bar.
- The Add proposal payload (`internal/event/add.go`, schema 1) contains
  **no earliest-fill field**. `earliest_fill_at` is exposed in the later
  proposal-expired payload. It is a lower bound, not a next-session label.
- A fill must arrive before `N`'s bar input expires the pending proposal.
  `checkBarConfirmsCampaignOpening` then verifies its timestamp is no later
  than `N.period_end`. Thus the reducer's timestamp window is
  **(P.period_end, N.period_end]**, further bounded by the preceding Unit's
  fill. Arrival ordering and timestamp validity are separate requirements.
- `pendingAddProposalState` explicitly applies next-bar expiry to chained
  Adds too. `applyCompletedBar` calls `expireAddProposal`, which clears the
  pending proposal and emits `superseded-by-next-bar`. ADR 0011 supplies the
  expiry rule; ADR 0021 section 7 preserves the fill-driven chain's existing
  behaviour. ADR 0010 orders decisions, not executions, as ADR 0020 explains.
- The decision envelope's `recorded_at` comes from its causing input
  (`transition.stamp`), and replay supplies `causation_id`. They identify
  when/why the reply was produced; neither renews the proposal's lifetime.

## What the reference actually fills

`cmd/backtest` calls `fills.RunSession`. After the bars and session close,
`fillPass` repeatedly fills covered orders and folds the reducer's reply back
into the book before selecting the next fill. A fill-created order uses its
creating fill's price as its reference (`internal/fills/price.go`), not an
earlier open. This implements ADR 0005 and ADR 0021 section 6.

With the current reducer, **a proposed chained Add fills in that same bar**:
`evaluateAdd` only proposes a rung the stored high already covers. This is
also pinned by `TestProposalsAreAlwaysCoveredByTheBarThatRaisedThem` in
`internal/fills/runbar_test.go`. There is no unfilled chained Add carried
into the next bar in that composed reference run. The generic simulator can
fill a previously resting order in the next bar's open-instant pass before
expiry, and the reducer permits next-session fills delivered before the next
bar, but those are broader protocol capabilities, not how this reference
chain executes.

## Why accepting the reply is insufficient in LEAN

The README's observed behaviour and `algorithm.py` state that a session's
fills arrive before `OnData` for **that session**, stamped at its close. The
new daily order cannot fill against that already completed bar. Naming
sessions by their bar period ends avoids confusing slice delivery with the
session that traded:

1. Bar **B** and its session close propose an entry or ordinary Add.
2. LEAN fills that order in **N**. Before sending bar N, `OnData` drains the
   fill into the engine. The reply chains an Add still attributed to B.
3. Even if the adapter accepts it, `OnData` next flushes B's snapshot and
   sends bar N. The engine immediately expires this B-attributed Add.
4. `OrderDesk._expire` cancels it in the same slice. Cancellation settles
   before the following session, the earliest one that could fill it.

Sending the fill after N's bar instead would fail because its parent
proposal has already expired. Ignoring the expiry would leave an order
whose eventual fill the reducer refuses. Changing `recorded_at` validation
cannot change either fact. A fresh Add at N's session close is a separate
proposal evaluated against N's own high; its later fill does not prove the
fill-chained proposal executed.

## Reproduction and falsification

First ran the unchanged adapter suite: **173 tests passed**. Then added the
following assertions immediately after the existing `self.assertTrue(chained,
...)` in `tests/test_end_to_end.py`, before changing production code:

```python
chained_ids = {d["id"] for d in chained}
placed_chains = [t for t in algo.Transactions.tickets if t.Tag in chained_ids]
self.assertTrue(placed_chains, "no fill-chained Add was placed")
filled_chains = [e for e in inputs if e["type"] == "execution.fill"
                 and e["payload"]["proposal_id"] in chained_ids]
self.assertTrue(filled_chains, "no fill-chained Add filled; ticket states: {}".format(
    [(t.Tag, t.Status) for t in placed_chains]))
```

Ran `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p
test_end_to_end.py` from `adapter/lean`. The real socket/engine test failed
with **no fill-chained Add was placed**. The cash-decline test passed.

Temporarily changed `_propose`'s comparison from
`if produced is not None and produced != bar_end:` to
`if entry and produced is not None and produced != bar_end:`. This deliberately
overbroad diagnostic bypass admits every readable Add timestamp, isolating
expiry from staleness. The same test then passed the placement assertion but
failed the fill assertion with:

```text
no fill-chained Add filled; ticket states:
[('add-proposal-unit-2:AAPL:2014-01-26T21:00:00.000000000Z', 'canceled')]
```

The cash-decline test still passed, including its assertions that the fill
reply declined the Add for `insufficient-cash` and no Add proposal or order
existed. Both temporary edits were reverted. This falsifies the sufficiency
of a staleness-only fix; it is not a completed red/green implementation or
the requested full mutation-test matrix.

The unchanged suite verifies the journal hash chain and byte-identical
replay. `make check` passed with no Go changes and 95.2% total coverage:

```text
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 95.2% (minimum: 80.0%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

The first sandboxed adapter invocation was blocked by local socket and Go
cache permissions; the successful invocation used the same command with
those permissions enabled. No market data, LEAN, Docker, GitHub or push was
used.

## Decision required

The existing window is explicit in code and consistent with ADR 0011 and
ADR 0021. The requested extra LEAN session is not covered by that window:

1. **Preserve the lifetime.** Document the missing same-bar chains and keep
   ordinary session-close re-evaluation. Accepting then canceling a chained
   order would not meet the requested end-to-end fill criterion.
2. **Declare a different lifetime for delayed fill chains.** Specify which
   bar supersedes such a proposal, how it interacts with that bar's new Add
   evaluation, exit precedence, and cash-snapshot eligibility. This requires
   an explicit ADR amendment and a RulesVersion change under ADR 0016.
   Version any changed wire meaning/shape under ADR 0015. Merely exposing an
   earliest-fill bound would not solve expiry.

No rule, schema, RulesVersion, adapter behaviour or recorded experiment was
changed. The missing chained fills remain unresolved pending that decision.
