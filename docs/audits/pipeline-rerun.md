# Whole-pipeline re-run evidence

Implemented `backtest -rerun <journal>` using the existing `drive`,
`inputEnvelope`, fill simulator and reducer. No golden, domain rule, schema,
RulesVersion or coverage exclusion changed. No GitHub tools were called and no
PR was opened. The [development guide](../development.md#local-checks) is the
single explanation of the relationship between the three checks.

## Work log and TDD

Read AGENTS.md, the development guide, ADR 0005 and ADR 0017, then traced
`perform`, `drive`, the journal recorder and reducer replay. Wrote
`TestRerunDetectsSimulatorDriftThatReplayCannot` first. Introduced only the
unexported `runBar` seam and CLI plumbing, temporarily forwarding `-rerun` to
`doReplay` to expose the precise missing property, before implementing re-run.
The test failed because reducer replay incorrectly satisfied the requested
pipeline operation, not because of a compilation error.

Verbatim red output:

```text
=== RUN   TestRerunDetectsSimulatorDriftThatReplayCannot
    rerun_test.go:50: -rerun must detect the changed simulator fill as the first pipeline divergence; got error <nil>, output "journal testdata/journal.golden.jsonl replays byte-identically\n"
--- FAIL: TestRerunDetectsSimulatorDriftThatReplayCannot (0.01s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.686s
FAIL
```

Replaced the temporary forwarding with reconstruction, shared identity checks,
and full pipeline comparison. Added cash, corporate-action, identity, schema,
missing/repeated/unsupported-input, metadata, record-length, time-zone,
CLI exclusivity and output-error tests. A second red test exposed header
comparison happening after record hashes; the generated header now precedes
record comparison on successful runs. A failed new run reports its first
changed/missing record together with its underlying execution error.

The test-only handler changes the first simulated fill's price with
`math.Nextafter(price, +Inf)` and refreshes its payload hash **before** either
recorder or reducer consumes it. It does not mutate the reference journal.
Only tests replace `runSession`; production always delegates to `fills.RunSession` (formerly `runBar` and `fills.RunBar`, before ADR 0021).

## Acceptance (a): both committed goldens

Commands (with local Go cache, `GOPROXY=off`, `GOSUMDB=off`,
`GOTOOLCHAIN=local`):

```sh
go run ./cmd/backtest -rerun cmd/backtest/testdata/journal.golden.jsonl
go run ./cmd/backtest -rerun cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl
```

Verbatim output, both exit status 0:

```text
journal cmd/backtest/testdata/journal.golden.jsonl reruns with no pipeline divergence (all records byte-identical under canonical comparison)
journal cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl reruns with no pipeline divergence (all records byte-identical under canonical comparison)
```

## Acceptance (b) and (c): simulator drift and missing facts

```sh
go test ./cmd/backtest -run 'TestRerun(DetectsSimulatorDriftThatReplayCannot|MissingInputsFailClosed)$' -v
```

Verbatim output (the test itself replaces temporary journal paths with
`<journal>` before asserting and logging the missing-input reason):

```text
=== RUN   TestRerunDetectsSimulatorDriftThatReplayCannot
    rerun_test.go:65: -rerun: backtest: testdata/journal.golden.jsonl: backtest: pipeline divergence at record 47: recorded execution.fill (fill:AAPL:2026-01-22T00:00:00.000000000Z:1) [payload_hash=7bd5c32cb6143857d2f7a45ec20d71acafaa426122cab2ac9ae54c27f4a54c83], regenerated execution.fill (fill:AAPL:2026-01-22T00:00:00.000000000Z:1) [payload_hash=e55c3b4ac8e41ab7a5dccf80ba6259f76636c75504e5fc0ee944ff0bf9192824]; canonical envelope bytes differ
    rerun_test.go:70: -replay: journal testdata/journal.golden.jsonl replays byte-identically
--- PASS: TestRerunDetectsSimulatorDriftThatReplayCannot (0.01s)
=== RUN   TestRerunMissingInputsFailClosed
=== RUN   TestRerunMissingInputsFailClosed/strategy.configuration
    rerun_test.go:139: backtest: <journal>: backtest: the journal records no configuration event; a reducer cannot be built without one
=== RUN   TestRerunMissingInputsFailClosed/account.snapshot
    rerun_test.go:139: backtest: <journal>: backtest: rerun missing required account.snapshot input
=== RUN   TestRerunMissingInputsFailClosed/market.bar.completed
    rerun_test.go:139: backtest: <journal>: backtest: rerun missing required market.bar.completed input
=== RUN   TestRerunMissingInputsFailClosed/replay.run.completed
    rerun_test.go:139: backtest: <journal>: backtest: rerun missing required replay.run.completed input
--- PASS: TestRerunMissingInputsFailClosed (0.01s)
    --- PASS: TestRerunMissingInputsFailClosed/strategy.configuration (0.00s)
    --- PASS: TestRerunMissingInputsFailClosed/account.snapshot (0.00s)
    --- PASS: TestRerunMissingInputsFailClosed/market.bar.completed (0.00s)
    --- PASS: TestRerunMissingInputsFailClosed/replay.run.completed (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.164s
```

## Acceptance (d): one documentation location

```sh
rg -n 'backtest -(verify|replay|rerun)' docs/development.md
```

Verbatim output:

```text
120:- `backtest -verify <journal>` checks the hash chain (ADR 0017): was recorded
122:- `backtest -replay <journal>` feeds all recorded inputs, **including recorded
125:- `backtest -rerun <journal>` reconstructs configuration, completed bars,
```

## Acceptance (e): checks and the no-fetch deviation

`make check` completed with exit status 0. The initial restricted attempt could
not bind the existing transport tests' Unix sockets; the full check was then
allowed local socket access. Cache paths were redirected to `/tmp`, and module
fetches were disabled with `GOPROXY=off` and `GOSUMDB=off`.

**The no-network-fetch instruction was violated by an offline-configuration
mistake.** The command also set `GOVULNDB=file:///tmp/trend-rerun-vulndb-unavailable`,
but installed `govulncheck` v1.1.4 ignores that environment variable: its
`internal/scan/flags.go` accepts `-db`, defaulting to `https://vuln.go.dev`.
Consequently the vulnerability check fetched from its default database. This
was disclosed immediately when its successful scan exposed the mistake. The
scan result is real, but the full check must **not** be described as offline.
No further fetch was made; the explicitly requested git push is separate.

Full check command:

```sh
GOCACHE=/tmp/trend-rerun-go-cache \
STATICCHECK_CACHE=/tmp/trend-rerun-staticcheck-cache \
GOLANGCI_LINT_CACHE=/tmp/trend-rerun-lint-cache \
GOPROXY=off GOSUMDB=off \
GOVULNDB=file:///tmp/trend-rerun-vulndb-unavailable make check
```

Verbatim gate output before the per-function coverage table:

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	11.785s	coverage: 89.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	1.685s	coverage: 66.4% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.121s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	20.363s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	3.036s	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	1.740s	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	22.019s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	3.258s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	1.126s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	2.363s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	1.115s	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	1.173s	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	2.770s	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.470s	coverage: 91.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.220s	coverage: 91.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.132s	coverage: 90.3% of statements
```

Verbatim output after the per-function coverage table:

```text
total:											(statements)			94.2%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.2% (minimum: 80.0%)
go tool govulncheck ./...
=== Symbol Results ===

No vulnerabilities found.

Your code is affected by 0 vulnerabilities.
This scan also found 5 vulnerabilities in packages you import and 42
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
Use '-show verbose' for more details.
go build ./...
```

After that run, the missing-input test was strengthened to exclude its temporary
path from the assertion (the path contains the subtest's name), and failed
pipeline diagnostics were refined to preserve both divergence and execution
error. Every check except the already-run vulnerability scan was repeated on
the final source with `make -o vuln check`, exit status 0. This is explicitly a
partial repeat, not a second complete `make check`. It avoids another database
fetch. Final coverage is **94.2% overall, 89.4% for cmd/backtest**.

```sh
GOCACHE=/tmp/trend-rerun-go-cache \
STATICCHECK_CACHE=/tmp/trend-rerun-staticcheck-cache \
GOLANGCI_LINT_CACHE=/tmp/trend-rerun-lint-cache \
GOPROXY=off GOSUMDB=off make -o vuln check
```

Verbatim final gate output before the per-function table:

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	11.953s	coverage: 89.4% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	2.462s	coverage: 66.4% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.201s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	20.521s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	3.206s	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	1.412s	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	22.224s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	3.485s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	1.100s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	2.094s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	1.122s	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	1.155s	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	2.628s	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.550s	coverage: 91.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.223s	coverage: 90.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.131s	coverage: 90.3% of statements
```

Verbatim final gate output after the per-function table:

```text
total:											(statements)			94.2%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.2% (minimum: 80.0%)
go build ./...
```

The coverage audit passed without exclusions changing. Its actual mechanical
scope is `internal/`, as declared by `auditedPackages`; this change stays in
`cmd/`. All new reconstruction, decoding, record comparison and CLI-reporting
statements are covered. Three defensive return statements in
`pipelineEquivalence` are uncovered, with these named construction reasons:

| Return after | Named reason |
| --- | --- |
| `strategy.NewReducer` | `reconstructRun` validated the exact configuration, and `journalConfiguration` parsed a nonempty strategy version. These are the constructor's only refusal conditions. |
| `fills.New` | The same configuration validation proves finite positive slippage; shared identity checking supplies nonempty strategy version and the recomputed configuration hash. These cover the simulator constructor's refusal conditions. |
| `recorder.Header` | Reached only after successful `drive`, which has recorded configuration, account, at least one valid bar and completion. There is an input span, valid timestamps and nonempty validated identity; the recorder constructs the supported journal version and chain algorithm. |

## Comparison and judgment calls

- Reused the original driver rather than replaying recorded fills or maintaining
  a second bar/action interleaving implementation. The comparison is over all
  ordered records, not only decisions: sequence, kind, chain hash and every
  canonical envelope byte. All header fields are compared too.
- No run-specific field is excluded. The original build suffix is supplied to
  the reconstructed pipeline after the same rules-version refusal as replay.
  RecordedAt and EventTime are regenerated from event times, not a wall clock.
  The normalization boundary is the supported decoded journal schema: outer
  JSON layout and equivalent timestamp zones are representation, while payload
  bytes remain exact. This is canonical record equality, not raw file equality.
- `replay.Equivalent` supplies envelope equality; `replay.Divergence` alone
  cannot represent header, kind, sequence or chain-hash differences, so those
  fields have explicit comparison and diagnostics. Record positions are
  one-based, matching journal sequences.
- Comparing chain hashes deliberately makes this stronger than reducer replay
  and means it also detects chain differences. It remains a separate operation;
  chain verification does not need a reproducible run or matching engine rules.
- The brief's Variant assumption needs qualification: `-variant` is solely a
  registry label and is absent from the journal. It is not consulted by `drive`.
  The configuration does contain Variant strategy identity and every trading
  parameter, so the declared Variant golden can be reproduced without guessing
  the registry label or reading a registry.
- Preserve the complete opening account snapshot, including explicit zero cash.
  There is no default when it is absent. Reject later snapshots, repeated
  configurations/completions, unsupported input types and unsupported independent
  input schemas. Omitted/null `available_cash` is refused by the existing account
  payload decoder and validator, not a duplicate command-side implementation.
- Refuse journals lacking final completion. Abandoned/bounded runs can contain
  fills from an as-yet unrecorded bar, and neither the stopping condition nor
  original record limit is journalled. Assuming completion would invent facts.
- Bound regenerated records at recorded length plus one, with the existing
  recorder's per-input overshoot bound. This detects surplus output without
  relying on an unrecorded CLI limit or imposing the default on a larger journal.
- The isolated perturbation demonstrates a simulator defect replay cannot see;
  it does not assert exhaustive determinism over every possible market history.
- No golden file was modified (`git diff --exit-code -- cmd/backtest/testdata`
  produced no output). No new strategy rule or domain defect was discovered.
  The execution deviation above is retained here rather than hidden. Per the
  explicit brief, there were no GitHub calls, issue updates or PR creation.
