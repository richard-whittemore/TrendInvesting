# Reducer transaction verification

Issue [77](https://github.com/richard-whittemore/TrendInvesting/issues/77), based
on `f02e3156afde576e3068a0c816845f348b02b413`, RulesVersion **1.5.0**.

## Boundary and routing

`Reducer.Apply` always calls `transact`, which opens a copy-on-write candidate,
runs the private `transition` handler, and publishes the candidate only on
success. Candidate mutations may feed later payloads. No ordinary emission or
accepted fill survives a rejected transition. Scalars, the Notional Account and
the delisted map are copied for every input. An instrument is deep-copied,
including its indicator ring and seed buffers, the first time the transaction
accesses it. New accepted fills are buffered until commit. See
[Copy-on-write](#copy-on-write) for the access-site routing and the measured
cost.

Routed paths:

- `applyFill`, `openCampaign`, `applyStopFill`, `applyExitFill`, `applyAddFill`,
  including chained Add evaluation, stop raises and Exit Order changes.
- `applyAccountSnapshot` and `applyCashMovement`, including the Notional Account,
  currency pinning, chronology, drawdown counters and spendable-cash basis.
- `applyCompletedBar`, including indicator advancement, entry/Add/exit proposal
  expiries, Setup/Signal/sizing decisions, Campaign evaluation and Exit Orders.
- `applyCorporateAction` / `applyDelisting`, including proposal cancellations,
  forced Campaign exit, and the terminal delisted-instrument map.
- `applyRunCompleted`, including expiries and resulting Exit Orders across the
  whole universe, plus `streamEnded`.
- `applyConfiguration` also uses the common boundary; it emits no payloads.
- `applyAdapterRunStopped` (added on `main` after this branch was cut, and routed on rebase) uses the same boundary. It emits no payloads, but its `runStopped` flag commits only on success.

`TestCloneCoversEveryReferenceTypedField` names the mechanism that isolates every reference-typed path reachable from `Reducer`: eager copy, overlay, copy on first access, or immutable once recorded. A new map, slice or pointer fails until one of them handles it, and `instrumentState.clone` is checked against a real copy.

No reducer input transition is left out. Pure arithmetic and payload helpers
run inside the enclosing transition without nested commits. Independently
usable Notional Account arithmetic remains unchanged; reducer-owned calls
always operate on the candidate account. The existing corrupted-stop halt is
preserved through an explicit failure diagnostic channel, as required by the
replay.Handler contract; it reports pre-existing corruption without committing
candidate state.

See [development guide](../development.md#reducer-transactions) for the ownership
and extension requirements.

## Tests and falsification

The red test commits are `9f32f88` and `e1f491f`; `92003fb` adds the direct Apply
cases before the implementation commit. Initial runs failed because the
transaction and Clone methods did not yet exist. A shallow transaction scaffold
then produced behavioral failures in all ten final-payload scenarios and the
reducer no-aliasing test: state leaked, accepted fills leaked, and fill retries
became empty duplicate deliveries. Required resting levels were supplied in the
synthetic fill fixtures before recording those behavioral failures.

The private build callback is the only injected-failure seam. Tests run the
real handler, check its last output type, decode and validate that payload,
clear its required Rule, and reject it with its real validator before the
transaction can commit. This reaches construction-unreachable failures, notably
cash-adjustment validation, without any production hook or global override.
Each case compares against a separately constructed expected reducer, retries
the identical input twice, checks identical errors and zero output/stream
changes, and finally removes the fault to verify successful commitment.

| Test | Coverage |
| --- | --- |
| `TestCashMovementRejectedLeavesAccountUnscaled` | Account figures, cash and chronology rollback |
| `TestPartialStopRejectedExpiryLeavesStateAndAcceptedFillsUnchanged` | Partial stop and final Add-expiry rejection |
| `TestTransitionRejectsFinalPayload` | Eight cases: entry/openCampaign, Add, full stop, exit, bar Exit Order, snapshot, delisting, stream end |
| `TestTransitionDeepCopyHasNoAliases` | Mutates candidate Campaign Units, all three proposals, indicators, account and delisted map through the accessors, buffers fills and a new instrument; original unchanged |
| `TestClonesOwnTheirBuffers` | Entry/Exit channel ring storage and Wilder seed storage, including nil receivers |
| `TestApplyRejectsInvalidFinalPayloadWithoutCommitting` | Six real builder failures through Apply: entry/Add final proposal, partial-stop expiry, bar/end Exit Order, snapshot final drawdown step |

The direct Apply tests use the existing package-private corrupt-state seam.
For the snapshot case two steps are built: the second step number overflows,
so its final payload fails after the first succeeded. The stream-end fixture
also expires an earlier instrument before the final instrument's Exit Order
fails. These are transaction tests, not new source-derived trading scenarios;
none uses `stream.run()` or adds a decision-corpus entry.

All **20 new leaf cases** were falsified after the implementation passed:

| Temporary mutation | Result |
| --- | --- |
| Replace deep reducer snapshot with `*r` | Exit 1; all ten injected final-payload cases, reducer no-aliasing, and five direct Apply cases fail |
| Return ordinary emissions alongside build error | Exit 1; all ten final-payload cases fail on leaked emissions |
| Shallow-copy indicator buffers | Exit 1; all three indicator no-aliasing cases fail |
| Shallow transaction plus stop mutation/acceptance moved before expiry validation | Exit 1; direct Apply partial-stop case fails, including the swallowed retry |

Every mutant was restored in a `finally` block. Focused commands:

```sh
go test ./internal/strategy ./internal/indicator \
  -run 'Test(CashMovementRejected|PartialStopRejected|Transition|ClonesOwn)'
go test ./internal/strategy -run TestApplyRejectsInvalidFinalPayloadWithoutCommitting
```

Restored results: both packages PASS; all six direct Apply cases PASS. The final
full race-enabled run below includes all these cases and the unchanged existing
suite.

## Byte identity

Before edits, `shasum -a 256` recorded these four files:

| Artifact | SHA-256 |
| --- | --- |
| `internal/strategy/testdata/decision-corpus/1.5.0.json` | `d9352749189378f8e2d78ae2c4a78975ed73803f390e8ee64ac5215ce03d8940` |
| `cmd/backtest/testdata/journal.golden.jsonl` | `770bd1fc53dc4fbb82b4254b0ad74d70a79635de8abea8bdcd95ac9e15dad29f` |
| `cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl` | `b40d78aa9ba4f54df30620f4a638ef03f3b736ea595fe31f15587b83a6093997` |
| Variant `registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json` | `a006c7fcd7c5643c00d63742a122cc4c694e3215a4d19b1ec10157b4fcc42c1e` |

Afterward:

```text
$ shasum -a 256 -c /tmp/77-artifacts-before.sha256
internal/strategy/testdata/decision-corpus/1.5.0.json: OK
cmd/backtest/testdata/journal.golden.jsonl: OK
cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl: OK
cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json: OK
```

The baseline comparison is independently repeatable without that temporary
manifest:

```sh
git diff --exit-code f02e315 -- \
  internal/strategy/testdata/decision-corpus \
  internal/strategy/rules_version.go cmd/backtest/testdata
```

Output: **none; exit 0**. No corpus file was added and no RulesVersion changed.
The full suite's existing decision-corpus, journal generation/replay/rerun, and
registry golden checks passed. No market data was fetched or committed.

## Full checks and coverage audit

Baseline `make check`: PASS, **94.9%**. The sandbox initially denied access to
the normal Go cache; the complete baseline succeeded with cache access.

Final `make check COVERAGE_MIN=94.9`: PASS, **95.0%**. Tail:

```text
total: (statements) 95.0%
COVERAGE_MIN=94.9 ./scripts/check-coverage.sh coverage.out
total coverage: 95.0% (minimum: 94.9%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

Formatting, vet, staticcheck, golangci-lint, race-enabled tests, coverage audit,
vulnerability scan, dependency checks and build all passed. The first post-change
full-suite run failed only the expected stale coverage exclusions; the first
final check also identified shadowing of Go's `copy` builtin, corrected to
`cloned` before the successful check.

`exclusions.json` was reconciled against a newly generated
`COVERAGE_AUDIT_DUMP`: **82 receiver renames**, from `(*Reducer)` to
`(*transition)`, and **six removed exclusions** now exercised by the direct
failure tests. No newly unreachable path was added and **no occurrence shifted**.
Removed blocks:

- `evaluateAdd`: invalid Add payload return.
- `applyAccountSnapshot`: invalid drawdown payload return.
- `exitOrderChanges`: invalid Exit Order payload return.
- `emitExitOrderChanges`: error propagation, occurrence 1.
- `applyCompletedBar`: Exit Order error propagation, occurrence 8.
- `applyRunCompleted`: Exit Order error propagation, occurrence 2.

The remaining bar-propagation reasons now identify five excluded occurrences
out of eleven, with the newly covered occurrence 8 removed.

## Copy-on-write

The first implementation deep-copied the whole reducer for every input: every
instrument's state, the delisted map and the whole accepted-fill history. At
the planned 1,000-instrument universe, that meant copying the whole universe
for every bar. The transaction is now copy-on-write, and the commit contract is
unchanged.

Access-site routing:

| Site | Access | Route |
| --- | --- | --- |
| `applyFill` (instrument lookup) | mutates | `instrument` |
| `applyFill` (duplicate detection) | reads | `acceptedFill`: the transaction's own fills, then the published history |
| `openCampaign`, `applyStopFill`, `applyExitFill`, `applyAddFill` (acceptance) | writes | `recordAcceptedFill` |
| `applyDelisting` | mutates | `instrument` |
| `stateFor` (from `applyCompletedBar`) | mutates or creates | `instrument`, else `addInstrument` |
| `applyRunCompleted` (chronology loop) | reads | `peekInstrument` over `instrumentIDs` |
| `applyRunCompleted` (expiry and Exit Order loop) | mutates | `instrument`, passed to `expireOutstandingProposals` and `emitExitOrderChanges` |
| `applyAdapterRunStopped` (chronology loop) | reads | `peekInstrument` over `instrumentIDs` |
| `instrumentIDs` | reads | published and overlay-only instruments together, sorted |

No handler deletes an instrument or an accepted fill. `acceptedFillState` is
never mutated after it is created. Its UnitIDs come from the fill's own decoded
payload and are only compared with `slices.Equal`. The embedded `instruments`
and `acceptedFills` maps are nil during a transaction, so any direct index that
was missed would fail loudly instead of writing through.
`TestTransitionDeepCopyHasNoAliases` now mutates through the accessors. It
dropped its two direct operations on published maps, the in-place UnitIDs write
and the instrument delete, because the design forbids both.

Only the end-of-stream expiry loop still copies every instrument, and it runs
once per run.

### Benchmark

`BenchmarkApplyBarWideUniverse` (`internal/strategy/transition_bench_test.go`)
applies one completed bar for one instrument to a reducer holding 1,000
instruments, each warmed with 80 bars, and 3,000 accepted fills. It was run
with `go test ./internal/strategy -run '^$' -bench BenchmarkApplyBarWideUniverse -benchmem -count 5`
on Go 1.27.1, darwin/arm64, Apple M2. Measured medians of five runs:

| Transaction | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| Eager whole-reducer copy (commit `df22454`) | 479,001 | 1,672,828 | 12,041 |
| Copy-on-write (this change) | 3,054 | 2,736 | 25 |

### Tests and falsification

| Test | Pins |
| --- | --- |
| `TestRejectedTransitionLeavesTouchedAndUntouchedInstrumentsUnchanged` | A rejected bar for AAPL leaves AAPL and untouched BBB equal and pointer-identical. A committed bar republishes AAPL and leaves BBB's pointer unchanged, because BBB is never copied. |
| `TestRejectedFillIsNotRememberedAndItsRetryFailsAgain` | A fill accepted in a rejected transaction is absent afterwards. The retry is processed in full, not absorbed as a duplicate, and fails again. |
| `TestCommittedTransitionPublishesNewInstrumentAndFill` | A new instrument, a new fill and the Campaign it opened are visible after commit. |
| `TestDuplicateDetectionSeesEveryFillAcceptedEarlierInTheRun` | A re-delivery in a later input is a no-op, and a reused id with changed contents is rejected. A second delivery in the same transaction is a no-op. |
| `TestLaterAccessInOneTransactionSeesEarlierMutation` | Two bars for one period in one transaction: the second sees the first and is rejected. |
| `TestWholeUniversePassSeesInstrumentsCreatedInTheSameTransaction` | The end-of-stream chronology check and expiry both see an instrument created earlier in the same transaction. |

Each temporary mutation of `transition.go` was restored afterwards:

| Mutation | Result |
| --- | --- |
| `instrument` hands out published state (write-through) | Exit 1: rejected-transition, rejected-fill and later-access tests fail |
| `acceptedFill` skips the overlay | Exit 1: same-transaction duplicate and rejected-fill tests fail |
| `acceptedFill` skips the published history | Exit 1: earlier-input duplicate test fails |
| `recordAcceptedFill` writes through to the published history | Exit 1: rejected-fill test fails |
| Shallow `instrumentState.clone` | Exit 1: rejected-transition test fails |
| `commit` drops both overlays | Exit 1: commit, later-access, rejected-transition and whole-universe tests fail |
| Eager whole-universe copy restored | Exit 1: rejected-transition test fails on BBB's pointer |
| `instrumentIDs` ignores overlay-only instruments | Exit 1: both whole-universe cases fail |
| `peekInstrument` ignores the overlay | Exit 1: whole-universe test fails (nil dereference) |
| `instrument` re-copies published state instead of reusing its overlay entry | Exit 1: later-access test fails |

`TestCloneCoversEveryReferenceTypedField` was re-falsified. Each of these fails
it: adding an unhandled `[]int` field to `instrumentState`, dropping the
`exitChannel` copy from `instrumentState.clone`, dropping the Campaign Units
copy, sharing `delisted` in `begin`, and exposing the published instruments map
to the transaction.

After the change,
`git diff --exit-code origin/main -- internal/strategy/testdata/decision-corpus internal/strategy/rules_version.go cmd/backtest/testdata`
produced no output and exited 0. RulesVersion is unchanged. `make check`
passes at **95.1%** total coverage, and `internal/coverageaudit/exclusions.json`
needed no change: every new branch is covered.

Open questions: none.
