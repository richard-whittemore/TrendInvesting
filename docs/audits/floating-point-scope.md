# Floating-point scope audit

Work log and findings for issues #98 and #102, against base `6e095d6`.

## Scope and decision path

The guard now uses `go list -deps -test -export -compiled -json ./...` and
checks active production, same-package test, external test and test-only packages
throughout the module. Export-data import remapping preserves augmented test
packages. Source and directory reads retain Go test-cache invalidation; duplicate
production/test diagnostics are reported once. Cgo still uses generated compiler
inputs with original source locations. Toolchain-generated test runners are
identified by `main`, a `.test` import suffix and absent compiled inputs; a real
command ending in `.test` is explicitly regression-tested and remains covered.

Test expectations require rounding independently of producers; otherwise a fused
producer and fused expectation can agree on each architecture while disagreeing
across architectures. `cmd/`, `transport/` and `transport/spike` are all included.
There are no repository-source exclusions. The spike is a latency benchmark, but
rounding its three products also supports its repeatable-payload invariant.

Read-through of `cmd/backtest/{main,backtest,replay}.go` found no floating-point
arithmetic feeding journalled decisions outside `internal/`. The command copies
fixture/configuration values (including starting cash), validates inputs, composes
`internal/strategy` and `internal/fills`, and writes/replays their envelopes.
Its remaining arithmetic is integer indexing/reporting. Ordinary transport code
frames and validates envelopes without calculating prices or risk. The spike is
imported by `cmd/transport-spike`, not the backtest command; its generated prices
and rounding do not feed the committed trading journal.

## Existing violations and impact

The wider guard rejected **74 products across 63 source lines**: 71 products in
internal tests and three in the spike. Every product received an explicit
`float64(...)` rounding barrier; there were no exclusions or strategy-rule changes.

The table lists every rejected product using **base-revision line and column**.
All `internal/` entries are `_test.go`: they cannot directly change a production
journal. They can affect synthetic reducer inputs, emitted test decisions or
expected values, and can conceal a production rounding defect. In particular,
`fills/fixture_test.go` generates bars for the composed simulator/reducer tests;
weighted entry/exit and result sums check values represented in journal payloads.
The three spike entries affect only benchmark payloads. No exposed site directly
feeds `cmd/backtest`'s committed golden journal, which was neither regenerated nor
edited. Detection identifies unguarded expression shapes, not proof that every
fixture's particular operands produce different bits (some products are exact).

| File | Base line:column | Product before rounding |
|---|---|---|
| `internal/event/add_test.go` | 31:39 | `0.5*n` |
| `internal/event/add_test.go` | 284:39 | `0.5*proposalN` |
| `internal/event/add_test.go` | 296:43 | `stopMultiple*n` |
| `internal/event/campaign_test.go` | 38:41 | `2*proposalN` |
| `internal/event/campaign_test.go` | 179:46 | `2*proposalN` |
| `internal/event/campaign_test.go` | 190:53 | `p.StopMultiple*p.CampaignN` |
| `internal/event/campaign_test.go` | 199:53 | `p.StopMultiple*p.CampaignN` |
| `internal/event/campaign_test.go` | 409:39 | `rung.inN*decoded.CampaignN` |
| `internal/event/campaign_test.go` | 465:54 | `2*proposalN` |
| `internal/event/campaign_test.go` | 488:35 | `2*n` |
| `internal/event/campaign_test.go` | 791:24 | `2*n` |
| `internal/event/campaign_test.go` | 863:60 | `float64(quantities[i]) * (exit - fills[i]) * dpp` |
| `internal/event/campaign_test.go` | 866:47 | `float64(quantities[i]) * fills[i]` |
| `internal/event/campaign_test.go` | 897:49 | `2*n` |
| `internal/event/proposal_test.go` | 60:34 | `2*proposalN` |
| `internal/event/proposal_test.go` | 76:34 | `3*proposalN` |
| `internal/event/stop_test.go` | 25:40 | `2*proposalN` |
| `internal/event/stop_test.go` | 39:36 | `2*proposalN` |
| `internal/event/stop_test.go` | 46:32 | `0.5*proposalN` |
| `internal/event/stop_test.go` | 143:37 | `2*proposalN` |
| `internal/event/stop_test.go` | 171:36 | `0.5*proposalN` |
| `internal/event/stop_test.go` | 187:36 | `0.5*proposalN` |
| `internal/event/stop_test.go` | 204:36 | `0.5*proposalN` |
| `internal/fills/fixture_test.go` | 174:32 | `float64(k-60)*0.2` |
| `internal/indicator/wilder_test.go` | 54:21 | `v*10000` |
| `internal/sizing/ladder_test.go` | 34:28 | `0.5*campaignN` |
| `internal/sizing/ladder_test.go` | 42:25 | `0.5*campaignN` |
| `internal/sizing/ladder_test.go` | 50:24 | `0.5*campaignN` |
| `internal/sizing/result_test.go` | 79:59 | `float64(unitQuantity) * (exit - fill) * dpp` |
| `internal/sizing/stop_test.go` | 36:35 | `stopMultiple*campaignN` |
| `internal/sizing/stop_test.go` | 93:43 | `tt.stopMultiple*tt.campaignN` |
| `internal/sizing/stop_test.go` | 487:33 | `0.5*tt.campaignN` |
| `internal/strategy/add_test.go` | 213:51 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 226:27 | `0.5*campaignN` |
| `internal/strategy/add_test.go` | 319:47 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 329:54 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 338:35 | `0.5*campaignN` |
| `internal/strategy/add_test.go` | 499:54 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 508:46 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 516:34 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/add_test.go` | 749:15 | `q*campaignFillPrice` |
| `internal/strategy/add_test.go` | 749:37 | `q*fill2Price` |
| `internal/strategy/add_test.go` | 749:52 | `q*fill3Price` |
| `internal/strategy/campaign_test.go` | 101:36 | `2*campaignN` |
| `internal/strategy/campaign_test.go` | 522:50 | `cfg.StopMultiple*wantN` |
| `internal/strategy/campaign_test.go` | 1488:55 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/campaign_test.go` | 1843:51 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/exit_test.go` | 145:51 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/notional_test.go` | 189:45 | `0.10*account.Current()` |
| `internal/strategy/notional_test.go` | 315:28 | `0.10*h.current` |
| `internal/strategy/notional_test.go` | 948:28 | `0.10*h.current` |
| `internal/strategy/reconciliation_test.go` | 134:33 | `cfg.StopMultiple*campaignN` |
| `internal/strategy/reducer_test.go` | 2012:58 | `cfg.StopMultiple*wantN` |
| `internal/strategy/reducer_test.go` | 2013:131 | `cfg.StopMultiple*wantN` |
| `internal/strategy/reducer_test.go` | 2064:43 | `3*wantN` |
| `internal/strategy/reducer_test.go` | 2065:116 | `3*wantN` |
| `internal/strategy/reducer_test.go` | 2446:26 | `float64(period-1)*n` |
| `internal/strategy/stop_ladder_test.go` | 114:29 | `20*campaignN` |
| `internal/strategy/stop_ladder_test.go` | 690:26 | `133.0*fixture.unit4Fill` |
| `internal/strategy/stop_ladder_test.go` | 690:52 | `133.0*fixture.unit1Fill` |
| `internal/strategy/stop_ladder_test.go` | 690:78 | `133.0*fixture.unit2Fill` |
| `internal/strategy/stop_ladder_test.go` | 690:104 | `133.0*fixture.unit3Fill` |
| `internal/strategy/stop_ladder_test.go` | 691:25 | `133.0*stop4Price` |
| `internal/strategy/stop_ladder_test.go` | 691:44 | `399.0*stop123Price` |
| `internal/strategy/stop_ladder_test.go` | 853:26 | `133.0*fixture.unit4Fill` |
| `internal/strategy/stop_ladder_test.go` | 853:52 | `133.0*fixture.unit1Fill` |
| `internal/strategy/stop_ladder_test.go` | 853:78 | `133.0*fixture.unit2Fill` |
| `internal/strategy/stop_ladder_test.go` | 853:104 | `133.0*fixture.unit3Fill` |
| `internal/strategy/stop_ladder_test.go` | 854:25 | `133.0*stop4Price` |
| `internal/strategy/stop_ladder_test.go` | 854:44 | `399.0*exitFillPrice` |
| `internal/strategy/stop_ordering_test.go` | 177:51 | `cfg.StopMultiple*campaignN` |
| `transport/spike/spike.go` | 80:44 | `float64(sequence%1000)*7919` |
| `transport/spike/spike.go` | 80:60 | `float64(i)*104729` |
| `transport/spike/spike.go` | 120:24 | `v*100` |

## Regression evidence

Environment: Go 1.24.4, darwin/arm64, cgo enabled. A writable cache was necessary:
the default sandboxed cache initially produced `cache entry not found` errors;
those infrastructure failures are not counted as regression evidence.

Before implementation, command:

```sh
GOCACHE=/tmp/trend-fma-go-cache go test ./internal/floatingpointaudit -run '^TestSourceScope$' -count=1 -v
```

Relevant output, verbatim:

```text
=== RUN   TestSourceScope
=== RUN   TestSourceScope/internal_test/add
    scope_test.go:66: expected source-located fusion rejection, got <nil>:
        ok  	fixture/internal/floatingpointaudit	0.195s
=== RUN   TestSourceScope/internal_test/accumulate
    scope_test.go:66: expected source-located fusion rejection, got <nil>:
        ok  	fixture/internal/floatingpointaudit	0.181s
=== RUN   TestSourceScope/internal_test/rounded
--- FAIL: TestSourceScope (8.61s)
    --- FAIL: TestSourceScope/internal_test/add (0.41s)
    --- FAIL: TestSourceScope/internal_test/accumulate (0.40s)
    --- PASS: TestSourceScope/internal_test/rounded (0.41s)
    --- FAIL: TestSourceScope/external_test/add (0.42s)
    --- FAIL: TestSourceScope/external_test/accumulate (0.39s)
    --- PASS: TestSourceScope/external_test/rounded (0.41s)
    --- FAIL: TestSourceScope/test_only/add (0.38s)
    --- FAIL: TestSourceScope/test_only/accumulate (0.41s)
    --- PASS: TestSourceScope/test_only/rounded (0.42s)
    --- FAIL: TestSourceScope/cmd/add (0.41s)
    --- FAIL: TestSourceScope/cmd/accumulate (0.41s)
    --- PASS: TestSourceScope/cmd/rounded (0.40s)
    --- FAIL: TestSourceScope/cmd_test/add (0.42s)
    --- FAIL: TestSourceScope/cmd_test/accumulate (0.38s)
    --- PASS: TestSourceScope/cmd_test/rounded (0.41s)
    --- FAIL: TestSourceScope/transport/add (0.41s)
    --- FAIL: TestSourceScope/transport/accumulate (0.42s)
    --- PASS: TestSourceScope/transport/rounded (0.42s)
    --- FAIL: TestSourceScope/spike/add (0.42s)
    --- FAIL: TestSourceScope/spike/accumulate (0.42s)
    --- PASS: TestSourceScope/spike/rounded (0.43s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	8.782s
FAIL
```

The `add` fixture is `return x + a*b`; `accumulate` is `x += a*b; return x`.
Each invokes a copied real guard in a temporary module. `<nil>` means the old
guard wrongly succeeded, so the enclosing regression failed. Safe rounded
fixtures already passed. This red state is committed as `0647d7d`.

A subsequent runner-selection regression used:

```sh
GOCACHE=/tmp/trend-fma-go-cache go test ./internal/floatingpointaudit -run '^TestSourceScope/cmd_dot_test' -count=1 -v
```

Before requiring absent compiled inputs for generated runners, it printed:

```text
=== RUN   TestSourceScope
=== RUN   TestSourceScope/cmd_dot_test/add
    scope_test.go:67: expected source-located fusion rejection, got <nil>:
        ok  	fixture/internal/floatingpointaudit	0.300s
=== RUN   TestSourceScope/cmd_dot_test/accumulate
    scope_test.go:67: expected source-located fusion rejection, got <nil>:
        ok  	fixture/internal/floatingpointaudit	0.268s
=== RUN   TestSourceScope/cmd_dot_test/rounded
--- FAIL: TestSourceScope (1.62s)
    --- FAIL: TestSourceScope/cmd_dot_test/add (0.55s)
    --- FAIL: TestSourceScope/cmd_dot_test/accumulate (0.53s)
    --- PASS: TestSourceScope/cmd_dot_test/rounded (0.54s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	1.842s
FAIL
```

After widening the guard and rounding all existing sites:

```sh
GOCACHE=/tmp/trend-fma-go-cache go test ./internal/floatingpointaudit -count=1 -v
```

The two deliberately bad internal test files were rejected (verbatim output):

```text
=== RUN   TestSourceScope/internal_test/add
    scope_test.go:69: guard rejected internal/fixture/shape_test.go:
        --- FAIL: TestNoFusibleMultiplyAdd (0.20s)
            audit_test.go:122: internal/fixture/shape_test.go:2:47: fusible floating-point multiply-add; round the product explicitly (docs/development.md: Floating-point determinism)
        FAIL
        FAIL	fixture/internal/floatingpointaudit	0.323s
        FAIL
=== RUN   TestSourceScope/internal_test/accumulate
    scope_test.go:69: guard rejected internal/fixture/shape_test.go:
        --- FAIL: TestNoFusibleMultiplyAdd (0.14s)
            audit_test.go:122: internal/fixture/shape_test.go:2:41: fusible floating-point multiply-add; round the product explicitly (docs/development.md: Floating-point determinism)
        FAIL
        FAIL	fixture/internal/floatingpointaudit	0.265s
        FAIL
```

The enclosing regressions pass because rejection is the required outcome. All
24 scope cases pass, including rounded controls, external tests, test-only
packages, commands, a command whose directory ends in `.test`, transport and the
spike. `shapes_test.go` and `cgo_test.go` are unchanged; their constant-product,
unary-sign, integer, generic-constraint and cgo cases all pass:

```text
--- PASS: TestNoFusibleMultiplyAdd (0.81s)
--- PASS: TestCgoSources (3.65s)
--- PASS: TestSourceScope (21.66s)
--- PASS: TestFusibleShapes (0.00s)
--- PASS: TestGenericFusibleShapes (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	26.271s
```

## Comment changes, file by file

All command edits preserve non-comment Go tokens. No ticket/PR/review reference
remains in command comments. No density target was used.

| File | Change and retained facts |
|---|---|
| `cmd/backtest/integrity_test.go` | Replace ticket narrative with ADR 0017; preserve independent chain/replay checks, unknown-kind refusal, repaired/unrepaired hashes, and comparison against original decisions. |
| `cmd/backtest/main_test.go` | Replace ticket narrative; retain fixed build provenance, deliberate golden review, contiguous sequence, terminal expiry, no overwrite, concurrent-write cleanup and complete-journal installation. Correct stale rename description. |
| `cmd/backtest/backtest.go` | Cite ADR 0011 for expiry; retain injected build identity and early positive-slippage validation. Correct stale rename wording; retain same-filesystem hard link, atomic EEXIST, cleanup and operator-owned evidence movement. |
| `cmd/backtest/replay_test.go` | Remove “this ticket” and repeated narration; preserve committed-golden independence, rules/build distinction, identity refusals and conflicting-operation regression. |
| `cmd/backtest/replay.go` | Condense independent verification, journal-owned configuration, simulator/reducer distinction and identity-refusal rules. |
| `cmd/backtest/main.go` | Consolidate exclusive-operation invariant and warning against silently discarding a requested operation. |
| `cmd/backtest/journal_failure_test.go` | Preserve reporting of persistence/report failures and journal survival; accurately name temporary-file creation. |
| `cmd/backtest/fusion_test.go` | Preserve architecture difference, accumulator examples, four Units, products needing more than 53 bits and the exact-product blind spot. |
| `cmd/transport-spike/main.go` | Replace issue citation with ADR 0014; retain benchmark-only role. |

Other `cmd/` files were reviewed and needed no changes. Beyond the two named
files, ticket references occurred in `backtest.go`, `replay_test.go` and
`transport-spike/main.go`; redundant prose and stale rename claims also occurred.
No comment facts were dropped to meet a ratio.

The remaining changed files are `internal/floatingpointaudit/audit_test.go`
(package selection, import remapping and diagnostic deduplication), its new
`scope_test.go` (temporary-module regression matrix), `docs/development.md`
(scope policy), this audit record, and the rounding-only files inventoried above.

## Full validation

```sh
GOCACHE=/tmp/trend-fma-go-cache make check
```

Exit status 0. Dependency tidy/verification, format, vet, staticcheck,
golangci-lint (`0 issues.`), race-enabled tests, uncovered-statement audit,
coverage floor, govulncheck and build passed. Total coverage: **94.8%** (80% floor).
The first full run stopped on a De Morgan simplification lint finding; it was
fixed before the successful full rerun. Socket tests and vulnerability database
access ran with the required sandbox escalation.

| Package | Coverage |
|---|---:|
| `cmd/backtest` | 82.1% |
| `cmd/transport-spike` | 76.0% |
| `cmd/trend-investing` | 0.0% |
| `internal/coverageaudit` | [no statements] |
| `internal/event` | 99.9% |
| `internal/fills` | 99.7% |
| `internal/floatingpointaudit` | [no statements] |
| `internal/indicator` | 100.0% |
| `internal/journal` | 99.3% |
| `internal/replay` | 100.0% |
| `internal/sizing` | 99.3% |
| `internal/strategy` | 90.2% |
| `transport` | 91.7% |
| `transport/spike` | 90.3% |
| `internal/buildinfo` | no test files / no executable statements |

`govulncheck` reported no called vulnerabilities; it also listed five findings
in imported packages and 42 in required modules whose vulnerable symbols are not
called. This is the tool's reachability result, not a claim that all dependencies
are free of advisories.

## Limits and handoff

Local execution was arm64 only; amd64 runtime validation remains for the existing
CI matrix. The guard still checks local expression shapes, not cross-statement
or inlined-call fusion; sensitive committed goldens remain necessary. Package
selection follows the active build configuration, so inactive architecture/tag
files require their corresponding CI build. No new out-of-scope defect was found.
Issues were read via GitHub MCP but not edited or closed: this delivery is local
commits only for orchestrator review and publication.
