# Reducer transaction verification

Issue [77](https://github.com/richard-whittemore/TrendInvesting/issues/77), based
on `f02e3156afde576e3068a0c816845f348b02b413`, RulesVersion **1.5.0**.

## Boundary and routing

`Reducer.Apply` always calls `transact`, which deep-copies the complete owned
state, runs the private `transition` handler, and swaps state only on success.
Candidate mutations may feed later payloads. No ordinary emission or accepted
fill survives a rejected transition. Every owned map, slice and mutable pointer
is copied, including indicator ring/seed buffers and accepted-fill UnitIDs.
The complete snapshot avoids a per-event write-set list, at a copying cost
proportional to instrument state and retained fill history.

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

`TestCloneCoversEveryReferenceTypedField` lists every reference-typed path reachable from `Reducer`, so a new map, slice or pointer fails until `clone` deep-copies it.

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
| `TestTransitionDeepCopyHasNoAliases` | Mutates candidate Campaign Units, all three proposals, indicators, account, maps and accepted-fill UnitIDs; original unchanged |
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

Open questions: none. Whole-state snapshot allocation cost is documented above;
this change prioritizes complete isolation and makes no performance claim.
