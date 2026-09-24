# Journal payload-byte preservation

## Findings

Journal records and headers now use `json.Encoder.SetEscapeHTML(false)`.
`Encoder.Encode` supplies exactly one newline. Payloads are never decoded
and re-encoded, and neither payload hashes nor chain hashes are repaired
after serialization. This preserves ADR 0017's original-byte invariant.

`Envelope.Validate` now compares payload bytes with `json.Compact` output,
rejecting non-compact JSON with `payload must contain compact JSON`.
Malformed JSON retains the existing `payload must contain valid JSON`
diagnostic. Validation neither mutates payload bytes nor replaces their hash.

The engine installs its socket server at
[`cmd/engine/engine.go:214`](../../cmd/engine/engine.go#L214).
[`transport/server.go:368`](../../transport/server.go#L368) calls
`Envelope.Validate` after decoding each frame and before invoking the
decider at line 371. Thus the new rule rejects input before reducer or
recorder execution, while the session is still running. The engine serves
at line 292 and writes the completed journal after serving ends.

The Python publisher already hashes compact payloads:
[`adapter/lean/publisher.py:98`](../../adapter/lean/publisher.py#L98)
uses `json.dumps(payload, separators=(",", ":"), allow_nan=False).encode()`.
The wire client also uses compact separators:
[`adapter/lean/client.py:8`](../../adapter/lean/client.py#L8) defines
`_SEPARATORS = (",", ":")`, used by `json.dumps` at line 94.
No adapter changes are required.

## Judgments

- Used `json.Compact` for validity and compactness in one pass instead of
  retaining a separate `json.Valid` scan. Empty and malformed input still fail.
- Encoded each line into a temporary byte buffer before writing it, retaining
  the existing distinction between encoding failures and I/O failures.
  Header and record use the same helper, and no extra newline is appended.
- Used a reproducible combinatorial property test: nine raw string fragments
  paired in all orders, in three JSON shapes (243 payloads). It covers literal
  `&<>`, UTF-8 `é` and `€`, escaped quotes/backslashes, existing Unicode
  escapes, significant string whitespace, nested values, and a numeric
  representation that must not be normalized. Both input and decision records
  participate in the same chain. This is bounded coverage, not an exhaustive
  claim about every JSON value.
- Used the transport socket test permitted by the brief. The frame splices
  payload bytes after envelope serialization so the test producer cannot
  silently compact them. Matching original-byte hashes ensure rejection is
  specifically for compactness, not an incidental hash mismatch. An atomic
  call count proves the invalid payload never reaches the decider, and a
  compact retry proves the same connection remains usable.
- Tested a trailing newline directly on an Envelope, and insignificant
  internal whitespace over the newline-framed socket. String whitespace
  remains valid.
- Kept RulesVersion at 1.3.0, all strategy rules, decision-corpus files,
  goldens, adapter code, and hash algorithms unchanged. No update flags ran.
- Kept this work log and verbatim evidence in the repository because the
  task explicitly prohibits GitHub tools and a PR. No new concerns remain.
  Testing was performed on Go 1.27.1, darwin/arm64; other architectures were
  not executed locally.

## Work log

1. Read AGENTS.md, ADRs 0015–0017, the envelope/canonical-envelope/journal
   implementation, engineering guidance, ingress wiring, and Python producer.
2. Added the three regression tests before production changes. All failed
   for the intended reasons; output follows.
3. Implemented the encoder and compactness changes; all three tests passed.
4. Temporarily restored the original `json.Marshal`-based `writeLine`;
   the journal property failed. Restored the fix in a `finally` block.
5. Temporarily removed only the compactness comparison; event validation and
   socket-ingress tests failed. Restored the fix in a `finally` block.
6. The first sandboxed test invocation could not access the Go build cache;
   reran with approved cache/socket access. The first `make check` found an
   unchecked socket-close result in test cleanup; corrected it to the existing
   best-effort cleanup convention. The subsequent complete `make check`
   passed, including race tests, coverage audit, corpus checks, lint,
   govulncheck, and build. No coverage exclusions changed.
7. Checked all testdata, the decision corpus, and RulesVersion with
   `git diff --stat HEAD`; all outputs are empty.

## Initial red run

Command (exit 1):

```sh
export PATH="$HOME/sdk/go1.27.1/bin:$PATH"
go test ./internal/journal ./internal/event ./transport -run 'TestWritePreservesCompactPayloadBytes|TestEnvelopeValidateRequiresCompactPayload|TestSocketRejectsNonCompactPayloadBeforeDecider' -count=1 -v
```

Verbatim output:

```text
=== RUN   TestWritePreservesCompactPayloadBytes
=== PAUSE TestWritePreservesCompactPayloadBytes
=== CONT  TestWritePreservesCompactPayloadBytes
    payload_bytes_test.go:44: Verify untouched journal: journal: the chain is broken at record 1: the record states sha256:2bbc34bf3a26daa4b2d4dc386216c36b13b625074c6cdd7f513b7330bbb8a21b, but its envelope following the previous record hashes to sha256:0c643a5caf5d689a11500af45ffe4a5cb5b28dfd4a052b2b2c62466ffdc38e6b
    payload_bytes_test.go:56: record 1 payload = "AT\u0026TAT\u0026T", want byte-identical "AT&TAT&T"
--- FAIL: TestWritePreservesCompactPayloadBytes (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/journal	0.118s
=== RUN   TestEnvelopeValidateRequiresCompactPayload
=== PAUSE TestEnvelopeValidateRequiresCompactPayload
=== CONT  TestEnvelopeValidateRequiresCompactPayload
=== RUN   TestEnvelopeValidateRequiresCompactPayload/internal_whitespace
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/trailing_newline
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/leading_whitespace
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/compact
=== RUN   TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string
--- FAIL: TestEnvelopeValidateRequiresCompactPayload (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/internal_whitespace (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/trailing_newline (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/leading_whitespace (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/compact (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/event	0.150s
=== RUN   TestSocketRejectsNonCompactPayloadBeforeDecider
=== PAUSE TestSocketRejectsNonCompactPayloadBeforeDecider
=== CONT  TestSocketRejectsNonCompactPayloadBeforeDecider
    compact_payload_test.go:53: reply = {Envelope:0x1a3d685a02a0 Error:<nil>}, want invalid_envelope naming compact JSON and causation bar-1
    compact_payload_test.go:56: decider called 1 times for non-compact payload, want 0
    compact_payload_test.go:63: decider called 2 times, want only the compact retry
--- FAIL: TestSocketRejectsNonCompactPayloadBeforeDecider (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/transport	0.146s
FAIL
```

## Green run

Same command after the fix (exit 0). Verbatim output:

```text
=== RUN   TestWritePreservesCompactPayloadBytes
=== PAUSE TestWritePreservesCompactPayloadBytes
=== CONT  TestWritePreservesCompactPayloadBytes
    payload_bytes_test.go:65: verified 243 compact payloads with byte-identical round trips
--- PASS: TestWritePreservesCompactPayloadBytes (0.01s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	0.233s
=== RUN   TestEnvelopeValidateRequiresCompactPayload
=== PAUSE TestEnvelopeValidateRequiresCompactPayload
=== CONT  TestEnvelopeValidateRequiresCompactPayload
=== RUN   TestEnvelopeValidateRequiresCompactPayload/internal_whitespace
=== RUN   TestEnvelopeValidateRequiresCompactPayload/trailing_newline
=== RUN   TestEnvelopeValidateRequiresCompactPayload/leading_whitespace
=== RUN   TestEnvelopeValidateRequiresCompactPayload/compact
=== RUN   TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string
--- PASS: TestEnvelopeValidateRequiresCompactPayload (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/internal_whitespace (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/trailing_newline (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/leading_whitespace (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/compact (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	0.119s
=== RUN   TestSocketRejectsNonCompactPayloadBeforeDecider
=== PAUSE TestSocketRejectsNonCompactPayloadBeforeDecider
=== CONT  TestSocketRejectsNonCompactPayloadBeforeDecider
--- PASS: TestSocketRejectsNonCompactPayloadBeforeDecider (0.00s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/transport	0.115s
```

## Falsification: restore json.Marshal

Restored the original `writeLine` function only, retaining compactness validation.

```sh
go test ./internal/journal -run '^TestWritePreservesCompactPayloadBytes$' -count=1 -v
```

Verbatim output (exit 1):

```text
=== RUN   TestWritePreservesCompactPayloadBytes
=== PAUSE TestWritePreservesCompactPayloadBytes
=== CONT  TestWritePreservesCompactPayloadBytes
    payload_bytes_test.go:44: Verify untouched journal: journal: the chain is broken at record 1: the record states sha256:2bbc34bf3a26daa4b2d4dc386216c36b13b625074c6cdd7f513b7330bbb8a21b, but its envelope following the previous record hashes to sha256:0c643a5caf5d689a11500af45ffe4a5cb5b28dfd4a052b2b2c62466ffdc38e6b
    payload_bytes_test.go:56: record 1 payload = "AT\u0026TAT\u0026T", want byte-identical "AT&TAT&T"
--- FAIL: TestWritePreservesCompactPayloadBytes (0.01s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/journal	0.159s
FAIL
```

## Falsification: remove compactness validation

Removed only the `else if !bytes.Equal(compact.Bytes(), e.Payload)` branch,
retaining malformed-JSON validation and the fixed journal encoder.

```sh
go test ./internal/event ./transport -run 'TestEnvelopeValidateRequiresCompactPayload|TestSocketRejectsNonCompactPayloadBeforeDecider' -count=1 -v
```

Verbatim output (exit 1):

```text
=== RUN   TestEnvelopeValidateRequiresCompactPayload
=== PAUSE TestEnvelopeValidateRequiresCompactPayload
=== CONT  TestEnvelopeValidateRequiresCompactPayload
=== RUN   TestEnvelopeValidateRequiresCompactPayload/internal_whitespace
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/trailing_newline
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/leading_whitespace
    compact_payload_test.go:35: Validate() = <nil>, want payload must contain compact JSON
=== RUN   TestEnvelopeValidateRequiresCompactPayload/compact
=== RUN   TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string
--- FAIL: TestEnvelopeValidateRequiresCompactPayload (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/internal_whitespace (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/trailing_newline (0.00s)
    --- FAIL: TestEnvelopeValidateRequiresCompactPayload/leading_whitespace (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/compact (0.00s)
    --- PASS: TestEnvelopeValidateRequiresCompactPayload/whitespace_inside_string (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/internal/event	0.157s
=== RUN   TestSocketRejectsNonCompactPayloadBeforeDecider
=== PAUSE TestSocketRejectsNonCompactPayloadBeforeDecider
=== CONT  TestSocketRejectsNonCompactPayloadBeforeDecider
    compact_payload_test.go:53: reply = {Envelope:0x7ba84059a000 Error:<nil>}, want invalid_envelope naming compact JSON and causation bar-1
    compact_payload_test.go:56: decider called 1 times for non-compact payload, want 0
    compact_payload_test.go:63: decider called 2 times, want only the compact retry
--- FAIL: TestSocketRejectsNonCompactPayloadBeforeDecider (0.00s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/transport	0.152s
FAIL
```

## Full verification

`make check` exited 0. Verbatim output, omitting only the per-function
coverage listing:

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	13.467s	coverage: 90.1% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/engine	2.803s	coverage: 69.2% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.260s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	22.291s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	3.905s	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	1.845s	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	23.630s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	3.595s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	1.159s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	2.705s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	1.164s	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	1.205s	coverage: 99.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	2.487s	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	3.514s	coverage: 94.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.288s	coverage: 91.2% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.169s	coverage: 90.3% of statements
total:											(statements)			94.8%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.8% (minimum: 80.0%)
go tool govulncheck ./...
No vulnerabilities found.
go build ./...
```

Total coverage is **94.8%** (minimum 80.0%); journal is **100.0%**,
event is **99.9%**, and transport is **91.2%**. The unfiltered strategy
suite passed against the unchanged decision corpus. The backtest suite
passed its byte-for-byte golden checks without `-update`.

## Golden/corpus diff-stat

Each command exited 0 and produced zero output bytes:

```sh
git diff --stat HEAD -- ':(glob)**/testdata/**'
git diff --stat HEAD -- internal/strategy/testdata/decision-corpus
git diff --stat HEAD -- internal/strategy/rules_version.go
```
