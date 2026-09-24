# Coverage audit hardening

## #179 — supplied profile scope

Verified the parser read every uncovered path and did not reject foreign module paths. It now validates the module prefix for covered and uncovered rows, then limits matching to `internal/`. The development guide names the actual scope explicitly.

Regression tests use an atomic `./...`-shaped profile with nonexistent `cmd/`, `transport/`, and `internalish/` paths, plus foreign-module rows with both zero and nonzero counts.

### red

`go test ./internal/coverageaudit -run '^TestSuppliedProfile' -v`

```text
=== RUN   TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== PAUSE TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule
=== CONT  TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== NAME  TestSuppliedProfileIgnoresPackagesOutsideAuditScope
    regression_test.go:33: read /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestSuppliedProfileIgnoresPackagesOutsideAuditScope1169349236/001/cmd/fabricated/main.go: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestSuppliedProfileIgnoresPackagesOutsideAuditScope1169349236/001/cmd/fabricated/main.go: no such file or directory
--- FAIL: TestSuppliedProfileIgnoresPackagesOutsideAuditScope (0.00s)
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
    regression_test.go:44: audit failure = <nil>; want clean failure containing "outside module github.com/richard-whittemore/TrendInvesting"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
    regression_test.go:44: audit failure = exit status 1; want clean failure containing "outside module github.com/richard-whittemore/TrendInvesting"; output:
        === RUN   TestCoverageAuditFailure
            regression_test.go:72: read /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure290598144/001/example.com/foreign/internal/file.go: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure290598144/001/example.com/foreign/internal/file.go: no such file or directory
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- FAIL: TestSuppliedProfileRejectsPathsOutsideModule (0.00s)
    --- FAIL: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1 (0.01s)
    --- FAIL: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0 (0.01s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.295s
FAIL
```

### green

`go test ./internal/coverageaudit -run '^TestSuppliedProfile' -v`

```text
=== RUN   TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== PAUSE TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule
=== CONT  TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
--- PASS: TestSuppliedProfileIgnoresPackagesOutsideAuditScope (0.00s)
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
    regression_test.go:44: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:72: coverage profile line "example.com/foreign/internal/file.go:1.1,1.2 1 1" names example.com/foreign/internal/file.go, which is outside module github.com/richard-whittemore/TrendInvesting
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
    regression_test.go:44: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:72: coverage profile line "example.com/foreign/internal/file.go:1.1,1.2 1 0" names example.com/foreign/internal/file.go, which is outside module github.com/richard-whittemore/TrendInvesting
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- PASS: TestSuppliedProfileRejectsPathsOutsideModule (0.00s)
    --- PASS: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1 (0.01s)
    --- PASS: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0 (0.01s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.153s
```

### falsification

`go test ./internal/coverageaudit -run '^TestSuppliedProfile' -v`

```text
=== RUN   TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== PAUSE TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule
=== CONT  TestSuppliedProfileIgnoresPackagesOutsideAuditScope
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== RUN   TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== PAUSE TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
=== CONT  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
=== NAME  TestSuppliedProfileIgnoresPackagesOutsideAuditScope
    regression_test.go:33: read /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestSuppliedProfileIgnoresPackagesOutsideAuditScope3889064129/001/cmd/fabricated/main.go: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestSuppliedProfileIgnoresPackagesOutsideAuditScope3889064129/001/cmd/fabricated/main.go: no such file or directory
--- FAIL: TestSuppliedProfileIgnoresPackagesOutsideAuditScope (0.00s)
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0
    regression_test.go:44: audit failure = exit status 1; want clean failure containing "outside module github.com/richard-whittemore/TrendInvesting"; output:
        === RUN   TestCoverageAuditFailure
            regression_test.go:72: read /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure2103539219/001/example.com/foreign/internal/file.go: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure2103539219/001/example.com/foreign/internal/file.go: no such file or directory
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== NAME  TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1
    regression_test.go:44: audit failure = <nil>; want clean failure containing "outside module github.com/richard-whittemore/TrendInvesting"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestSuppliedProfileRejectsPathsOutsideModule (0.00s)
    --- FAIL: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_0 (0.01s)
    --- FAIL: TestSuppliedProfileRejectsPathsOutsideModule/execution_count_1 (0.01s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.121s
FAIL
```

Falsification restored the entire pre-fix audit implementation while retaining the regression tests, then restored the fix.

### Repository-wide reuse

`COVERAGE_AUDIT_CHILD=1 go test -covermode=atomic -coverprofile=/tmp/coverage-audit-evidence/all.out ./...` passed with transport socket permissions. `COVERAGE_AUDIT_PROFILE=/tmp/coverage-audit-evidence/all.out go test ./internal/coverageaudit -run '^TestEveryUncoveredStatementIsExcludedWithANamedReason$' -v`:

```text
=== RUN   TestEveryUncoveredStatementIsExcludedWithANamedReason
=== PAUSE TestEveryUncoveredStatementIsExcludedWithANamedReason
=== CONT  TestEveryUncoveredStatementIsExcludedWithANamedReason
--- PASS: TestEveryUncoveredStatementIsExcludedWithANamedReason (0.01s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	(cached)
```

## #180 — stale supplied profiles

Verified that profile reuse checked neither timestamps nor test changes. Supplied profiles now fail cleanly when repository Go source/tests, module files, or testdata files are newer. Regression fixtures keep the zero-count profile unchanged and make each input newer; fresh inputs remain accepted. No wall clock assumptions: fixtures use explicit timestamps.

The development guide now requires immediate reuse after a successful full run with identical inputs and explicitly describes the timestamp check's limits (including deletions and preserved timestamps). This is not a content attestation; those cases require regeneration.

### red

`go test ./internal/coverageaudit -run '^TestSuppliedProfile(RejectsNewerCoverageInputs|AcceptsFreshInputs)$' -v`

```text
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/sample_test.go; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/sample.go; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.mod
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than go.mod; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.sum
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than go.sum; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/testdata/input.json; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs (0.04s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/go.mod (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/go.sum (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json (0.01s)
=== RUN   TestSuppliedProfileAcceptsFreshInputs
--- PASS: TestSuppliedProfileAcceptsFreshInputs (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.299s
FAIL
```

### green

`go test ./internal/coverageaudit -run '^TestSuppliedProfile(RejectsNewerCoverageInputs|AcceptsFreshInputs)$' -v`

```text
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go
    regression_test.go:53: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:108: cannot reuse /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure200666165/001/coverage.out: coverage profile is older than internal/sample/sample_test.go; regenerate it
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go
    regression_test.go:53: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:108: cannot reuse /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure2084381498/001/coverage.out: coverage profile is older than internal/sample/sample.go; regenerate it
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.mod
    regression_test.go:53: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:108: cannot reuse /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure3867126781/001/coverage.out: coverage profile is older than go.mod; regenerate it
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.sum
    regression_test.go:53: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:108: cannot reuse /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure2017744329/001/coverage.out: coverage profile is older than go.sum; regenerate it
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json
    regression_test.go:53: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:108: cannot reuse /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestCoverageAuditFailure2475179571/001/coverage.out: coverage profile is older than internal/sample/testdata/input.json; regenerate it
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- PASS: TestSuppliedProfileRejectsNewerCoverageInputs (0.03s)
    --- PASS: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go (0.01s)
    --- PASS: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go (0.01s)
    --- PASS: TestSuppliedProfileRejectsNewerCoverageInputs/go.mod (0.01s)
    --- PASS: TestSuppliedProfileRejectsNewerCoverageInputs/go.sum (0.01s)
    --- PASS: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json (0.01s)
=== RUN   TestSuppliedProfileAcceptsFreshInputs
--- PASS: TestSuppliedProfileAcceptsFreshInputs (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.252s
```

### falsification

`go test ./internal/coverageaudit -run '^TestSuppliedProfile(RejectsNewerCoverageInputs|AcceptsFreshInputs)$' -v`

```text
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/sample_test.go; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/sample.go; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.mod
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than go.mod; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/go.sum
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than go.sum; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
=== RUN   TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json
    regression_test.go:53: audit failure = <nil>; want clean failure containing "coverage profile is older than internal/sample/testdata/input.json; regenerate it"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs (0.03s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample_test.go (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/sample.go (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/go.mod (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/go.sum (0.01s)
    --- FAIL: TestSuppliedProfileRejectsNewerCoverageInputs/internal/sample/testdata/input.json (0.01s)
=== RUN   TestSuppliedProfileAcceptsFreshInputs
--- PASS: TestSuppliedProfileAcceptsFreshInputs (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.144s
FAIL
```

Falsification restored the pre-fix audit implementation with tests retained, then restored the fix.

## #181 — identical guards

Verified that exchanging coverage between two identical guards passed the old matcher. Block keys and diagnostics now include the starting byte offset; dumps retain separate occurrences. Extracted the unchanged matching loop into `checkExclusions` so synthetic profiles exercise the actual audit. The regression checks both the swap failure and separate JSON dump identities.

Migrated 78 aggregate entries into 91 position-specific entries. Compared the old counts to the new per-key multiplicities before writing; every category and reason was preserved verbatim. Existing entry ordering is retained, with formerly grouped entries expanded in source order. Byte offsets deliberately require renewed review when preceding source text moves; the guide documents that tradeoff. No domain files or decision rules changed.

### red

`go test ./internal/coverageaudit -run '^TestIdenticalGuards' -v`

```text
=== RUN   TestIdenticalGuardsCannotExchangeCoverage
    regression_test.go:95: audit failure = <nil>; want clean failure containing "1 statement(s) in internal/ are executed by no test and are not in exclusions.json"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestIdenticalGuardsCannotExchangeCoverage (0.01s)
=== RUN   TestIdenticalGuardsDumpSeparately
    regression_test.go:111: dump must name both guards separately; got {
          "note": "generated by COVERAGE_AUDIT_DUMP; every entry still needs a category and a reason",
          "categories": [
            "unreachable-by-construction",
            "unreachable-by-invariant"
          ],
          "exclusions": [
            {
              "file": "internal/sample/sample.go",
              "function": "f",
              "statement": "return nil",
              "count": 2
            }
          ]
        }
--- FAIL: TestIdenticalGuardsDumpSeparately (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.164s
FAIL
```

### green

`go test ./internal/coverageaudit -run '^TestIdenticalGuards' -v`

```text
=== RUN   TestIdenticalGuardsCannotExchangeCoverage
    regression_test.go:95: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:144: 1 statement(s) in internal/ are executed by no test and are not in exclusions.json.
                Write a test for each, or add it to the list with the invariant that makes it unreachable:
                  internal/sample/sample.go:4 (f, byte 86)
                      return nil
                Re-authoring the whole list: COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/

            regression_test.go:144: internal/sample/sample.go:3 (f, byte 60) is listed in exclusions.json 1 time(s) but fewer blocks than that are uncovered: it is now covered, moved, or no longer exists. Recheck the reason and update or remove the entry.
                      return nil
        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- PASS: TestIdenticalGuardsCannotExchangeCoverage (0.01s)
=== RUN   TestIdenticalGuardsDumpSeparately
--- PASS: TestIdenticalGuardsDumpSeparately (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.138s
```

### falsification

`go test ./internal/coverageaudit -run '^TestIdenticalGuards' -v`

```text
=== RUN   TestIdenticalGuardsCannotExchangeCoverage
    regression_test.go:95: audit failure = <nil>; want clean failure containing "1 statement(s) in internal/ are executed by no test and are not in exclusions.json"; output:
        === RUN   TestCoverageAuditFailure
        --- PASS: TestCoverageAuditFailure (0.00s)
        PASS
--- FAIL: TestIdenticalGuardsCannotExchangeCoverage (0.01s)
=== RUN   TestIdenticalGuardsDumpSeparately
    regression_test.go:111: dump must name both guards separately; got {
          "note": "generated by COVERAGE_AUDIT_DUMP; every entry still needs a category and a reason",
          "categories": [
            "unreachable-by-construction",
            "unreachable-by-invariant"
          ],
          "exclusions": [
            {
              "file": "internal/sample/sample.go",
              "function": "f",
              "statement": "return nil",
              "count": 2
            }
          ]
        }
--- FAIL: TestIdenticalGuardsDumpSeparately (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.097s
FAIL
```

Falsification restored the old key and dump aggregation while retaining the matching-loop extraction and regression tests, then restored the fix.

Package check after migration: `go test ./internal/coverageaudit -count=1`

```text
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	19.904s
```
