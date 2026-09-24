# Journal verification and run completeness

## Findings and judgments

- `Verify` reused neither writer validator before this change. It now calls
  `Header.validate` and `Envelope.Validate` directly, with the same validation
  error text as `Write`. Validation reports the first invalid envelope in
  journal sequence order, including decision records.
- Chain/sequence/kind checks precede content validation. This preserves existing
  `ChainBrokenError` behavior when an unrepaired edit also invalidates content,
  including an edited chain algorithm or payload. Validation errors are distinct
  when the chain is intact. `Read` still rejects unsupported format versions
  before either pass; its established diagnostics are unchanged.
- `Complete` describes the final input, not the last record or header span.
  Terminal decisions do not clear it, a later non-completion input does, and a
  completion-type decision cannot set it. No input means false. An empty journal
  remains an error. This follows `event.RunCompletedEventType` and ADR 0011.
- Incomplete journals with valid content and chains still verify successfully.
  Evidence installation and registry status are unchanged (ADR 0012).
- `CheckIdentity` and `CheckSpan` remain separate, as ADR 0017 specifies. The
  original issue suggested adding them, but the supplied brief scopes this fix
  to the writer's header and envelope validators.
- `-replay` compares the recorded prefix and says it replays byte-identically;
  it does not require or claim completion. `-decisions` displays recorded
  decisions and compares entire recorded decision streams, not finished runs.
  Neither command synthesizes missing end-of-run decisions. Their success can
  occur on incomplete evidence, so the operator guide now explicitly directs
  users to `-verify` for completeness. No command behavior was widened.
  Corrected the adjacent guide's stale statement that `CheckSpan` was absent:
  the function exists, but the decisions path does not call it.
- `-rerun` already refuses missing `replay.run.completed`, repeated markers, or
  a marker followed by another input in `reconstructRun`. It cannot reconstruct
  an interruption from an incomplete journal and already fails closed.
- The completion marker is a recorded claim, not proof of successful execution
  or producer authenticity. A rewritten and rehashed journal remains forgeable.
- All three supplied RED test files were preserved unchanged. Existing output
  assertions were substring checks, not a pinned `-verify` output golden; no
  golden expectation needed updating. RulesVersion remains 1.3.0.
- No GitHub tools or PR were used. This committed report holds the work log and
  verbatim evidence requested by the brief. Tests run on Go 1.27.1, darwin/arm64.

## Work log

1. Read AGENTS.md, both supplied issue briefs, ADRs 0012/0017, the event contract,
   writer/reader/verifier and identity/span checks, CLI paths, and development
   guidance. Reconfirmed both defects in this checkout.
2. Ran the supplied tests before implementing. After relocating the sandboxed
   Go cache, they failed compilation because `Verification.Complete` was absent.
   Added only the zero-valued field to expose the behavioral RED failures below.
3. Added validator reuse, completeness derivation, and the aligned CLI status
   line. Both full journal/backtest packages passed without update flags.
4. Removed only the new validator calls: every invalid-header/envelope case
   failed. Forced the reported completeness to true (`complete || true`): all
   incomplete unit cases and the installed failed-run regression failed.
   Restored production code in a `finally` block after both mutations.
5. Captured complete/incomplete CLI reports from the unmodified regression,
   including the injected second-bar failure and successful evidence install.
6. Initial `make check` was blocked by sandbox cache writes. Relocated Go,
   staticcheck and golangci-lint caches to `/private/tmp` and reran the gate.
   Transport tests then required Unix-socket access outside the sandbox; the
   approved full rerun passed, including race tests, coverage/corpus audits,
   dependency checks, lint, govulncheck and build. No exclusions changed.


## Behavioral RED output (exit 1)

```sh
go test ./internal/journal ./cmd/backtest -run "TestVerifyRejectsInvalid|TestVerificationCompleteUsesFinalInput|TestVerifyReportsRunCompleteness" -count=1
```

Verbatim output:

```text
--- FAIL: TestVerificationCompleteUsesFinalInput (0.00s)
    --- FAIL: TestVerificationCompleteUsesFinalInput/final_input_marker (0.00s)
        completeness_test.go:47: Complete = false, want true
    --- FAIL: TestVerificationCompleteUsesFinalInput/terminal_decision_after_marker (0.00s)
        completeness_test.go:47: Complete = false, want true
--- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain (0.00s)
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_configuration_hash (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming configuration hash
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_span_start (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_strategy_version (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming strategy version
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_span_end (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/backwards_span (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/unsupported_chain_algorithm (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming chain algorithm
--- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain (0.00s)
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming strategy version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_3 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_3 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming strategy version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming strategy version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming payload hash
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming payload hash
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_3 (0.01s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming payload hash
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/journal	0.198s
--- FAIL: TestVerifyReportsRunCompleteness (0.04s)
    --- FAIL: TestVerifyReportsRunCompleteness/complete (0.03s)
        completeness_test.go:55: Complete = false, want true
        completeness_test.go:66: verify report missing "run                complete\n" or verified chain:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenesscomplete3376287896/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-02-02T00:00:00Z
            records            101
            final record hash  sha256:c62b2cfaac10ef37a469b5c298f70758c47bdad4daf1d902e93483a3522527a2
            chain              verified
        completeness_test.go:68: -verify output:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenesscomplete3376287896/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-02-02T00:00:00Z
            records            101
            final record hash  sha256:c62b2cfaac10ef37a469b5c298f70758c47bdad4daf1d902e93483a3522527a2
            chain              verified
    --- FAIL: TestVerifyReportsRunCompleteness/incomplete (0.01s)
        completeness_test.go:66: verify report missing "run                INCOMPLETE — final input is not replay.run.completed; the run did not finish\n" or verified chain:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenessincomplete968701451/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-01-02T00:00:00Z
            records            4
            final record hash  sha256:72ac9592d2bf702ea167dd49a37b50c2abfe47ca1086fbb8f18fa8d86206e99b
            chain              verified
        completeness_test.go:68: -verify output:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenessincomplete968701451/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-01-02T00:00:00Z
            records            4
            final record hash  sha256:72ac9592d2bf702ea167dd49a37b50c2abfe47ca1086fbb8f18fa8d86206e99b
            chain              verified
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.234s
FAIL
```

## Both package suites GREEN (exit 0)

```sh
go test ./internal/journal ./cmd/backtest -count=1
```

Verbatim output:

```text
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	0.311s
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	2.058s
```

## Removed validation falsification (exit 1)

```sh
go test ./internal/journal -run TestVerifyRejectsInvalid -count=1
```

Verbatim output:

```text
--- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain (0.00s)
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/backwards_span (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_span_end (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_span_start (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming span
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/unsupported_chain_algorithm (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming chain algorithm
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_configuration_hash (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming configuration hash
    --- FAIL: TestVerifyRejectsInvalidHeadersWithAnIntactChain/missing_strategy_version (0.00s)
        verify_validation_test.go:82: Verify accepted an intact chain over invalid content; want invalid header naming strategy version
--- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain (0.00s)
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming payload hash
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming payload hash
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/mismatched_payload_hash/record_3 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming payload hash
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_3 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming strategy version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/unsupported_envelope_version/record_1 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 1: invalid event envelope naming envelope version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_2 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 2: invalid event envelope naming strategy version
    --- FAIL: TestVerifyRejectsInvalidEnvelopesWithAnIntactChain/missing_strategy_version/record_3 (0.00s)
        verify_validation_test.go:41: Verify accepted an intact chain over invalid content; want record 3: invalid event envelope naming strategy version
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/journal	0.243s
FAIL
```

## Forced complete falsification (exit 1)

```sh
go test ./internal/journal ./cmd/backtest -run "TestVerificationCompleteUsesFinalInput|TestVerifyReportsRunCompleteness" -count=1
```

Verbatim output:

```text
--- FAIL: TestVerificationCompleteUsesFinalInput (0.00s)
    --- FAIL: TestVerificationCompleteUsesFinalInput/no_marker (0.00s)
        completeness_test.go:47: Complete = true, want false
    --- FAIL: TestVerificationCompleteUsesFinalInput/later_input_after_marker (0.00s)
        completeness_test.go:47: Complete = true, want false
    --- FAIL: TestVerificationCompleteUsesFinalInput/decision_is_not_a_marker (0.00s)
        completeness_test.go:47: Complete = true, want false
    --- FAIL: TestVerificationCompleteUsesFinalInput/no_input (0.00s)
        completeness_test.go:47: Complete = true, want false
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/journal	0.129s
--- FAIL: TestVerifyReportsRunCompleteness (0.03s)
    --- FAIL: TestVerifyReportsRunCompleteness/incomplete (0.01s)
        completeness_test.go:55: Complete = true, want false
        completeness_test.go:66: verify report missing "run                INCOMPLETE — final input is not replay.run.completed; the run did not finish\n" or verified chain:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenessincomplete826496336/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-01-02T00:00:00Z
            records            4
            final record hash  sha256:72ac9592d2bf702ea167dd49a37b50c2abfe47ca1086fbb8f18fa8d86206e99b
            chain              verified
            run                complete
        completeness_test.go:68: -verify output:
            journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenessincomplete826496336/001/journal.jsonl
            configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
            strategy version   backtest-fixture/1.3.0+test
            span               2026-01-02T00:00:00Z to 2026-01-02T00:00:00Z
            records            4
            final record hash  sha256:72ac9592d2bf702ea167dd49a37b50c2abfe47ca1086fbb8f18fa8d86206e99b
            chain              verified
            run                complete
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.189s
FAIL
```

## Complete and incomplete verification reports (exit 0)

```sh
go test ./cmd/backtest -run TestVerifyReportsRunCompleteness -count=1 -v
```

Verbatim output:

```text
=== RUN   TestVerifyReportsRunCompleteness
=== RUN   TestVerifyReportsRunCompleteness/complete
    completeness_test.go:68: -verify output:
        journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenesscomplete2617271971/001/journal.jsonl
        configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
        strategy version   backtest-fixture/1.3.0+test
        span               2026-01-02T00:00:00Z to 2026-02-02T00:00:00Z
        records            101
        final record hash  sha256:c62b2cfaac10ef37a469b5c298f70758c47bdad4daf1d902e93483a3522527a2
        chain              verified
        run                complete
=== RUN   TestVerifyReportsRunCompleteness/incomplete
    completeness_test.go:68: -verify output:
        journal            /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestVerifyReportsRunCompletenessincomplete2602124067/001/journal.jsonl
        configuration hash sha256:cdc17c5bc8270b98a78f3574624d98beb89afde9448d370dd020f5977ce8d956
        strategy version   backtest-fixture/1.3.0+test
        span               2026-01-02T00:00:00Z to 2026-01-02T00:00:00Z
        records            4
        final record hash  sha256:72ac9592d2bf702ea167dd49a37b50c2abfe47ca1086fbb8f18fa8d86206e99b
        chain              verified
        run                INCOMPLETE — final input is not replay.run.completed; the run did not finish
--- PASS: TestVerifyReportsRunCompleteness (0.04s)
    --- PASS: TestVerifyReportsRunCompleteness/complete (0.02s)
    --- PASS: TestVerifyReportsRunCompleteness/incomplete (0.01s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.201s
```

## Full gate and coverage

`make check` exited 0. Verbatim output, omitting only per-function coverage:

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	1.643s	coverage: 69.2% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.140s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	(cached)	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	(cached)	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	(cached)	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	19.213s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	2.518s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	(cached)	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	(cached)	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	(cached)	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	(cached)	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.028s	coverage: 94.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.247s	coverage: 92.4% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.156s	coverage: 90.3% of statements
go tool cover -func=coverage.out | tee coverage.txt
total:											(statements)			94.9%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.9% (minimum: 80.0%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

Total coverage: **94.9%** (floor 80.0%); journal: **100.0%**.

Cache settings for the gate:

```sh
export PATH="$HOME/sdk/go1.27.1/bin:$PATH"
export GOCACHE=/private/tmp/trend-172-go-cache
export STATICCHECK_CACHE=/private/tmp/trend-172-staticcheck-cache
export GOLANGCI_LINT_CACHE=/private/tmp/trend-172-golangci-cache
make check
```

## Golden, corpus and rules diff

Each command exited 0 with zero output bytes. No update flags were used.
The unfiltered strategy suite and all byte-identical golden tests passed.

```sh
git diff --stat HEAD -- ':(glob)**/testdata/**'
git diff --stat HEAD -- internal/strategy/testdata/decision-corpus
git diff --stat HEAD -- internal/strategy/rules_version.go
```
