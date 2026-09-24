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

## #182 — negative exclusion counts

Verified the matcher panicked on an in-memory exclusion with `Count: -1`. It now reports the file, function, byte offset and negative count with `t.Errorf`, then skips that invalid entry before slicing. The subprocess regression requires failure with that diagnostic and rejects panic output. Separate cases retain omitted/zero and explicit-one count behavior.

### red

`go test ./internal/coverageaudit -run '^Test(NegativeExclusionCountFailsWithoutPanic|DefaultAndExplicitSingleCountsMatch)$' -v`

```text
=== RUN   TestNegativeExclusionCountFailsWithoutPanic
    regression_test.go:121: audit failure = exit status 2; want clean failure containing "internal/sample/sample.go:0 (f, byte 60): count -1 is negative"; output:
        === RUN   TestCoverageAuditFailure
        --- FAIL: TestCoverageAuditFailure (0.00s)
        panic: runtime error: slice bounds out of range [-1:] [recovered, repanicked]

        goroutine 7 [running]:
        testing.tRunner.func1.2({0x104481230, 0x2dc4b444a150})
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2123 +0x1a0
        testing.tRunner.func1()
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2126 +0x2c8
        panic({0x104481230?, 0x2dc4b444a150?})
                /Users/richardwhittemore/sdk/go1.27.1/src/runtime/panic.go:859 +0x120
        github.com/richard-whittemore/TrendInvesting/internal/coverageaudit.checkExclusions(0x2dc4b44f4248, {0x2dc4b44a6ef0, 0x1, 0x1?}, {0x2dc4b44a6e88, 0x1, 0x1?})
                /private/tmp/claude-501/-Users-richardwhittemore-Desktop-Trend-Investing/2e05d6c0-69da-44df-b35d-b2d6696a572b/scratchpad/wt-cov/internal/coverageaudit/audit_test.go:154 +0xdac
        github.com/richard-whittemore/TrendInvesting/internal/coverageaudit.TestCoverageAuditFailure(0x2dc4b44f4248)
                /private/tmp/claude-501/-Users-richardwhittemore-Desktop-Trend-Investing/2e05d6c0-69da-44df-b35d-b2d6696a572b/scratchpad/wt-cov/internal/coverageaudit/regression_test.go:157 +0x154
        testing.tRunner(0x2dc4b44f4248, 0x104490108)
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2193 +0xc4
        created by testing.(*T).Run in goroutine 1
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2258 +0x3b8
--- FAIL: TestNegativeExclusionCountFailsWithoutPanic (0.01s)
=== RUN   TestDefaultAndExplicitSingleCountsMatch
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_0
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_1
--- PASS: TestDefaultAndExplicitSingleCountsMatch (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_0 (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_1 (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.142s
FAIL
```

### green

`go test ./internal/coverageaudit -run '^Test(NegativeExclusionCountFailsWithoutPanic|DefaultAndExplicitSingleCountsMatch)$' -v`

```text
=== RUN   TestNegativeExclusionCountFailsWithoutPanic
    regression_test.go:121: expected audit failure:
        === RUN   TestCoverageAuditFailure
            regression_test.go:157: internal/sample/sample.go:0 (f, byte 60): count -1 is negative
            regression_test.go:157: 1 statement(s) in internal/ are executed by no test and are not in exclusions.json.
                Write a test for each, or add it to the list with the invariant that makes it unreachable:
                  internal/sample/sample.go:0 (f, byte 60)
                      return nil
                Re-authoring the whole list: COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/

        --- FAIL: TestCoverageAuditFailure (0.00s)
        FAIL
--- PASS: TestNegativeExclusionCountFailsWithoutPanic (0.01s)
=== RUN   TestDefaultAndExplicitSingleCountsMatch
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_0
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_1
--- PASS: TestDefaultAndExplicitSingleCountsMatch (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_0 (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_1 (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.153s
```

### falsification

`go test ./internal/coverageaudit -run '^Test(NegativeExclusionCountFailsWithoutPanic|DefaultAndExplicitSingleCountsMatch)$' -v`

```text
=== RUN   TestNegativeExclusionCountFailsWithoutPanic
    regression_test.go:121: audit failure = exit status 2; want clean failure containing "internal/sample/sample.go:0 (f, byte 60): count -1 is negative"; output:
        === RUN   TestCoverageAuditFailure
        --- FAIL: TestCoverageAuditFailure (0.00s)
        panic: runtime error: slice bounds out of range [-1:] [recovered, repanicked]

        goroutine 18 [running]:
        testing.tRunner.func1.2({0x100e35230, 0x517612468018})
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2123 +0x1a0
        testing.tRunner.func1()
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2126 +0x2c8
        panic({0x100e35230?, 0x517612468018?})
                /Users/richardwhittemore/sdk/go1.27.1/src/runtime/panic.go:859 +0x120
        github.com/richard-whittemore/TrendInvesting/internal/coverageaudit.checkExclusions(0x51761246a248, {0x517612398ef0, 0x1, 0x1?}, {0x517612398e88, 0x1, 0x1?})
                /private/tmp/claude-501/-Users-richardwhittemore-Desktop-Trend-Investing/2e05d6c0-69da-44df-b35d-b2d6696a572b/scratchpad/wt-cov/internal/coverageaudit/audit_test.go:154 +0xdac
        github.com/richard-whittemore/TrendInvesting/internal/coverageaudit.TestCoverageAuditFailure(0x51761246a248)
                /private/tmp/claude-501/-Users-richardwhittemore-Desktop-Trend-Investing/2e05d6c0-69da-44df-b35d-b2d6696a572b/scratchpad/wt-cov/internal/coverageaudit/regression_test.go:157 +0x154
        testing.tRunner(0x51761246a248, 0x100e44108)
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2193 +0xc4
        created by testing.(*T).Run in goroutine 1
                /Users/richardwhittemore/sdk/go1.27.1/src/testing/testing.go:2258 +0x3b8
--- FAIL: TestNegativeExclusionCountFailsWithoutPanic (0.01s)
=== RUN   TestDefaultAndExplicitSingleCountsMatch
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_0
=== RUN   TestDefaultAndExplicitSingleCountsMatch/count_1
--- PASS: TestDefaultAndExplicitSingleCountsMatch (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_0 (0.00s)
    --- PASS: TestDefaultAndExplicitSingleCountsMatch/count_1 (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.123s
FAIL
```

Falsification removed the negative-count guard with regression tests retained, observed the original panic, then restored the fix.

## Final batch verification

Go 1.27.1 (`export PATH="$HOME/sdk/go1.27.1/bin:$PATH"` first in every shell). `GOCACHE=/tmp/coverage-audit-evidence/go-build` avoids the restricted default build cache. The initial sandboxed repository-wide test run could not bind transport Unix sockets; the full rerun and quality gate ran with the necessary permissions and succeeded. No GitHub tools or PR were used.

`make check` exited 0. Output below includes all quality-gate stages and package results; per-function coverage rows are omitted. Total statement coverage is 94.8%, above the 80.0% floor.

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	(cached)	coverage: 69.2% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	(cached)	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	18.891s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	(cached)	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	(cached)	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	20.840s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	2.769s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	(cached)	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	(cached)	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	(cached)	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.457s	coverage: 94.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	(cached)	coverage: 90.8% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	(cached)	coverage: 90.3% of statements
total coverage: 94.8% (minimum: 80.0%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

Immediate reuse of the profile from `make check`:

`COVERAGE_AUDIT_PROFILE=coverage.out go test -count=1 ./internal/coverageaudit -run '^TestEveryUncoveredStatementIsExcludedWithANamedReason$' -v`

```text
=== RUN   TestEveryUncoveredStatementIsExcludedWithANamedReason
=== PAUSE TestEveryUncoveredStatementIsExcludedWithANamedReason
=== CONT  TestEveryUncoveredStatementIsExcludedWithANamedReason
--- PASS: TestEveryUncoveredStatementIsExcludedWithANamedReason (0.01s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	0.130s
```

`git diff --stat 054eced -- ':(glob)**/testdata/**'` produced no output. Direct byte comparisons against the starting commit also passed:

```text
All 18 tracked testdata files are byte-identical to 054eced.
internal/strategy/rules_version.go is byte-identical to 054eced.
```

No decision-rule changes, journal changes, corpus changes, or new out-of-scope concerns were found. The supplied-profile timestamp limitations and the source-position migration cost are documented in the development guide.
