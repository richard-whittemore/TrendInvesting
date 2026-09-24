# Coverage exclusion occurrence keys — #181 rework

The key is `(file, enclosing function, normalised statement text, occurrence)`.
Occurrence is the 1-based ordinal among **all** blocks with identical text in
the same function, including covered blocks, sorted by source position before
filtering the uncovered set. The key survives edits outside the function and
edits inside it that do not add, remove or reorder identical statements.
An omitted occurrence defaults to 1; generated lists omit that default.
Diagnostics retain file:line, function, and occurrence N.

No strategy rules or production behavior changed. #179, #180 and #182 remain
in history unchanged. Legacy counts of zero (default) or one remain accepted;
negative counts still fail cleanly, and counts above one are now explicitly
rejected. No exclusion carries a count or byte offset.

## Regeneration

A fresh generated profile and `COVERAGE_AUDIT_DUMP` produced 91 exclusions:
77 default occurrence-1 entries and 14 entries needing an explicit ordinal.
The prior categories and invariant reasons were transferred by file, function,
text and source order; existing entry order was retained for review. The five
`applyCompletedBar` reasons now describe occurrence identity instead of their
obsolete count claim. The underlying invariant explanations are unchanged.

Entries with occurrence > 1:

| File | Function | Statement | Occurrence |
| --- | --- | --- | --- |
| `internal/replay/divergence_report.go` | `numberIsFloat64Safe` | `return false` | 3 |
| `internal/strategy/campaign.go` | `(*Reducer).applyAddFill` | `return nil, fmt.Errorf("strategy: marshal protective stop set payload: %w", err)` | 2 |
| `internal/strategy/campaign.go` | `(*Reducer).evaluateAdd` | `return nil, err` | 2 |
| `internal/strategy/campaign.go` | `(*Reducer).evaluateAdd` | `return nil, err` | 3 |
| `internal/strategy/delisting.go` | `(*Reducer).applyDelisting` | `return nil, err` | 2 |
| `internal/strategy/delisting.go` | `(*Reducer).applyDelisting` | `return nil, err` | 3 |
| `internal/strategy/end_of_stream.go` | `(*Reducer).expireOutstandingProposals` | `return nil, err` | 2 |
| `internal/strategy/end_of_stream.go` | `(*Reducer).expireOutstandingProposals` | `return nil, err` | 3 |
| `internal/strategy/reducer.go` | `(*Reducer).applyCompletedBar` | `return nil, err` | 4 |
| `internal/strategy/reducer.go` | `(*Reducer).applyCompletedBar` | `return nil, err` | 5 |
| `internal/strategy/reducer.go` | `(*Reducer).applyCompletedBar` | `return nil, err` | 6 |
| `internal/strategy/reducer.go` | `(*Reducer).applyCompletedBar` | `return nil, err` | 10 |
| `internal/strategy/reducer.go` | `(*Reducer).stateFor` | `return nil, fmt.Errorf("strategy: %w", err)` | 2 |
| `internal/strategy/reducer.go` | `(*Reducer).stateFor` | `return nil, fmt.Errorf("strategy: %w", err)` | 3 |

## Work log and verification

Every shell first exported `PATH="$HOME/sdk/go1.27.1/bin:$PATH"`; Go reports
`go1.27.1 darwin/arm64`. Commands used
`GOCACHE=/tmp/coverage-audit-evidence/go-build`.

1. Added the edit-stability regression and grouped-count rejection regression
   before implementation; both failed against the original offset-based code.
2. Assigned source-order ordinals before filtering covered blocks. Strengthened
   the exchange regression to `{1,2}` → `{1,3}` across three identical guards,
   with the coverage-profile records deliberately in reverse source order.
3. Regenerated exclusions, updated the key and stability documentation, and
   verified omitted/explicit first occurrences match. Existing negative-count
   and default/single-count tests remain green.
4. Removed the ordinal from the key temporarily: the exchange regression failed
   because the child audit incorrectly accepted the swap. Restored the fix.
5. Temporarily keyed by starting byte offset again: both edit-stability cases
   failed. Restored the fix. The diagnostic format remained occurrence-based
   during this mutation; the identity actually compared was the byte offset.
6. Ran the focused tests and full quality gate. The first sandboxed quality-gate
   attempt stopped at Staticcheck cache permissions; the authorized rerun with
   cache/socket/network access passed. No GitHub tools or PR were used.

## Required tests — verbatim passing output

`go test -count=1 ./internal/coverageaudit -run '^Test(IdenticalGuardsCannotExchangeCoverage|EditsAboveExcludedGuardPreserveExclusion)$' -v`

```text
=== RUN   TestIdenticalGuardsCannotExchangeCoverage
    regression_test.go:103: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:219: 1 statement(s) in internal/ are executed by no test and are not in exclusions.json.
                Write a test for each, or add it to the list with the invariant that makes it unreachable:
                  internal/sample/sample.go:5 (f, occurrence 3)
                      return nil
                Re-authoring the whole list: COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/
                
            regression_test.go:219: internal/sample/sample.go:4 (f, occurrence 2) is listed in exclusions.json 1 time(s) but fewer blocks than that are uncovered: it is now covered, changed, or no longer exists. Recheck the reason and update or remove the entry.
                      return nil
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- PASS: TestIdenticalGuardsCannotExchangeCoverage (0.01s)
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion/outside_function
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion/inside_function
--- PASS: TestEditsAboveExcludedGuardPreserveExclusion (0.00s)
    --- PASS: TestEditsAboveExcludedGuardPreserveExclusion/outside_function (0.00s)
    --- PASS: TestEditsAboveExcludedGuardPreserveExclusion/inside_function (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.305s
```

## Exchange falsification — verbatim output

`go test -count=1 ./internal/coverageaudit -run '^TestIdenticalGuardsCannotExchangeCoverage$' -v`

Expected exit status: 1.

```text
=== RUN   TestIdenticalGuardsCannotExchangeCoverage
    regression_test.go:103: audit failure = <nil>; want clean failure containing "1 statement(s) in internal/ are executed by no test and are not in exclusions.json"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestIdenticalGuardsCannotExchangeCoverage (0.01s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.270s
FAIL
```

## Edit-stability falsification (offset key restored) — verbatim output

`go test -count=1 ./internal/coverageaudit -run '^TestEditsAboveExcludedGuardPreserveExclusion$' -v`

Expected exit status: 1.

```text
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion/outside_function
    regression_test.go:125: 1 statement(s) in internal/ are executed by no test and are not in exclusions.json.
        Write a test for each, or add it to the list with the invariant that makes it unreachable:
          internal/sample/sample.go:6 (f, occurrence 1)
              panic("unreachable")
        Re-authoring the whole list: COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/
        
    regression_test.go:125: internal/sample/sample.go:3 (f, occurrence 1) is listed in exclusions.json 1 time(s) but fewer blocks than that are uncovered: it is now covered, changed, or no longer exists. Recheck the reason and update or remove the entry.
              panic("unreachable")
=== RUN   TestEditsAboveExcludedGuardPreserveExclusion/inside_function
    regression_test.go:125: 1 statement(s) in internal/ are executed by no test and are not in exclusions.json.
        Write a test for each, or add it to the list with the invariant that makes it unreachable:
          internal/sample/sample.go:5 (f, occurrence 1)
              panic("unreachable")
        Re-authoring the whole list: COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/
        
    regression_test.go:125: internal/sample/sample.go:3 (f, occurrence 1) is listed in exclusions.json 1 time(s) but fewer blocks than that are uncovered: it is now covered, changed, or no longer exists. Recheck the reason and update or remove the entry.
              panic("unreachable")
--- FAIL: TestEditsAboveExcludedGuardPreserveExclusion (0.00s)
    --- FAIL: TestEditsAboveExcludedGuardPreserveExclusion/outside_function (0.00s)
    --- FAIL: TestEditsAboveExcludedGuardPreserveExclusion/inside_function (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.167s
FAIL
```

## Full quality gate

`make check` exited 0. Verbatim output below omits only the per-function coverage table and its total row.

```text
go mod tidy -diff
go mod verify
all modules verified
test -z "$(gofmt -l $(find . -name '*.go' -not -path './vendor/*'))"
go vet ./...
go tool staticcheck ./...
go tool golangci-lint run ./...
0 issues.
go test -race -covermode=atomic -coverprofile=coverage.out ./...
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	(cached)	coverage: 90.1% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	1.724s	coverage: 69.2% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	(cached)	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	17.837s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	(cached)	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	(cached)	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	19.669s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	2.868s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	(cached)	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	(cached)	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	(cached)	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.515s	coverage: 94.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	(cached)	coverage: 90.8% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	(cached)	coverage: 90.3% of statements
go tool cover -func=coverage.out | tee coverage.txt
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.8% (minimum: 80.0%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

## Evidence preservation

Direct byte comparisons against the starting commit:

```text
All 22 tracked testdata/golden/decision-corpus files are byte-identical to 0d34a15.
internal/strategy/rules_version.go is byte-identical to 0d34a15.
```

No new out-of-scope concerns were found.
