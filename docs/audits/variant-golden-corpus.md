# Declared Variant golden corpus: predicate regression evidence

The `profit-protecting-stop` Variant is declared in
[`testdata/variants/profit-protecting-stop/README.md`](../../cmd/backtest/testdata/variants/profit-protecting-stop/README.md).
The declaration and failing test were committed before generating the fixture.
Only the fixture strategy identity and Stop Multiple differ from the original
configuration. No production rules, RulesVersion, constants, or original golden
were changed.

## Golden review

The generated journal has 179 records: configuration, opening account snapshot,
32 authored daily bars, simulated fills, decisions, and run completion. Its
configuration hash is
`sha256:1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1`.
The matching registry fixture declares `profit-protecting-stop`, records status
`completed`, rules version `1.2.0`, and span 2026-01-02 through 2026-02-02. It
anchors the journal with 179 records and final hash
`sha256:451b3442b9da7b9642c250705b2fe04a417a1ac7496b840f3f4b1d918f31a419`.

The journal's inputs and decision payloads were read before accepting it:

- Twenty warmup bars precede the 2026-01-22 breakout, with N = 1.
- Four Units of 5,000 shares enter at 127.06, 127.61,
  128.16000000000003 and 128.71000000000004. Their initial stops are 0.1N
  below their own fill prices; six half-N Stop Ladder raises protect earlier
  Units above their own entries.
- Four stop fills close those Units. Journal record 76 is the Campaign exit:
  average entry 127.885, average exit 128.485, and recorded protective stop
  128.61000000000004. The final Unit's own initial stop is above the
  Campaign's lifetime average entry; the exit payload does not distinguish
  an initial Unit stop from a raised one. This reaches the validator's
  formerly refused relation while also recording the actual Ladder raises.
- The later synthetic bars open and stop ten single-Unit Campaigns, with
  ordinary below-entry stops. The last bar evaluates Setup; the final input
  ends the run. Those losses remain in the golden.

The test requires an above-Unit-entry Ladder raise and an at-or-above-average-
entry stop exit in the **same Campaign**, so regeneration cannot silently
remove the state this fixture is intended to guard. It also verifies the hash
chain, checks the registry attribution and anchor, compares both artifacts
byte-for-byte, and replays the recorded inputs.

## Predicate perturbation

On Go 1.24.4, darwin/arm64, temporarily changed only this argument inside
`CampaignExitedPayload.Validate` in `internal/event/campaign.go`:

```diff
- sizing.ValidStopLevel(p.EntryPrice, p.ProtectiveStopLevel, sizing.StopKindRaised)
+ sizing.ValidStopLevel(p.EntryPrice, p.ProtectiveStopLevel, sizing.StopKindInitial)
```

This restores the below-entry restriction without changing any declared rule
constant or RulesVersion. Ran:

```sh
go test ./cmd/backtest -run '^TestDeclaredVariantGolden$' -count=1 -v
```

Verbatim output (exit status 1):

```text
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:67: possible rules change in declared Variant "profit-protecting-stop" (ADR 0016): journal bytes changed or run failed; review decisions and RulesVersion before accepting -update
        run error: backtest: fills: apply event fill:AAPL:2026-01-22T00:00:00.000000000Z:8 at sequence 31: strategy: instrument "AAPL": stop fill "fill:AAPL:2026-01-22T00:00:00.000000000Z:8" would close campaign "campaign:AAPL:2026-01-22T00:00:00.000000000Z" with an invalid exit: invalid campaign exited payload: sizing: invalid protective stop level: protective stop level 128.61000000000004 must be below the entry price 127.885 for an initial stop: a stop only reaches entry once the stop ladder has raised it
        decision diff: the got stream ends here; the other continues with sequence 44, event units-stopped-fill:AAPL:2026-01-22T00:00:00.000000000Z:8:AAPL:2026-01-22T00:00:00.000000000Z (strategy.campaign.units-stopped)
--- FAIL: TestDeclaredVariantGolden (0.05s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.296s
FAIL
```

The first absent decision is `strategy.campaign.units-stopped`, immediately
before `strategy.campaign.exited`: validation happens before either final
emission. The test reports the actual first difference instead of inventing an
exit-only diff. The predicate perturbation was restored in a `finally` block;
`git diff --stat -- internal/event/campaign.go` then produced no output.

## Pure refactor control

Temporarily renamed local `entryPriceFinite` to `entryIsFinite`, including all
uses within `CampaignExitedPayload.Validate`, leaving its expression unchanged.
Neither golden nor RulesVersion was updated. Both golden tests passed; the
rename was then reverted. Verbatim output:

```text
=== RUN   TestTheCommandTurnsABarFixtureIntoTheGoldenJournal
--- PASS: TestTheCommandTurnsABarFixtureIntoTheGoldenJournal (0.02s)
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:140: Variant journal and registry reproduced byte-for-byte
--- PASS: TestDeclaredVariantGolden (0.05s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.285s
```

## Judgments and limits

Reused the existing authored bars instead of adding market data or a second
bar fixture. Kept their shortened 20/10 channels, which are fixture settings,
and the existing 0.05N slippage. The 0.1N choice was the predeclared singleton
from the reachable regression test, not a fitted threshold. This software
regression makes no adoption or out-of-sample performance claim under ADR 0012.
The registry is explicitly a golden test snapshot; no research run record was
rewritten. Actual failed research evidence remains append-only.

The test detects behavior changes on this declared Variant, not every possible
predicate or Variant. It cannot decide whether a change is an intentional rule
change or a bug; its diagnostic requires that judgment before any update, in
accordance with ADR 0016. No new issues were identified.

## Unperturbed reproduction and original golden

Ran without `-update`, after restoring production code:

```sh
go test ./cmd/backtest -run '^(TestDeclaredVariantGolden|TestTheCommandTurnsABarFixtureIntoTheGoldenJournal)$' -count=1 -v
```

Verbatim output:

```text
=== RUN   TestTheCommandTurnsABarFixtureIntoTheGoldenJournal
--- PASS: TestTheCommandTurnsABarFixtureIntoTheGoldenJournal (0.09s)
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:140: Variant journal and registry reproduced byte-for-byte
--- PASS: TestDeclaredVariantGolden (0.08s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.396s
```

The following command produced no output:

```sh
git diff --stat origin/main -- cmd/backtest/testdata/journal.golden.jsonl
```

A direct byte comparison against `git show origin/main:cmd/backtest/testdata/journal.golden.jsonl`
also passed. Both versions have SHA-256 `caf5bc3c9f76deb673c84b1a5638d92c45605bd7953c2177743dc2027c4f0cb3`.

## Full check

`make check` completed with exit status 0 on Go 1.24.4, darwin/arm64.
Total coverage was **94.9%**, above the 80.0% floor; `cmd/backtest` was 87.6%.
The initial restricted attempt could not bind the transport tests' Unix sockets;
the complete check was rerun with socket access. Tool caches were redirected
inside this worktree. No test or check was disabled.

```sh
GOCACHE="$PWD/bin/task-cache/go-build" \
STATICCHECK_CACHE="$PWD/bin/task-cache/staticcheck" \
GOLANGCI_LINT_CACHE="$PWD/bin/task-cache/golangci" make check
```

Verbatim gate output before the per-function coverage listing:

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	21.351s	coverage: 87.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.158s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	31.708s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	7.634s	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	1.736s	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	35.605s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	5.868s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	1.238s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	6.610s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	1.126s	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	1.323s	coverage: 98.8% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	6.440s	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	4.968s	coverage: 91.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.234s	coverage: 90.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.159s	coverage: 90.3% of statements
```

Verbatim coverage total and remaining gates (the per-function listing is omitted):

```text
total:											(statements)			94.9%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.9% (minimum: 80.0%)
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

## Replay gates fixture updates

Replay now runs before the update branch, so a replay error cannot replace
either committed fixture. Verified the order with a temporary candidate build
(`build: testBuild + "-replay-order-probe"`) in `TestDeclaredVariantGolden`.
This changes both generated snapshots, making an accidental write observable
in their bytes and in Git status. For the failing replay probe, temporarily
changed `doReplay(path, &log)` to `doReplay(path + ".missing", &log)`.
The real replay function then fails opening its input. Neither probe changes
production logic or the fixture declaration.

Ran the failure probe before moving the check (red), then after moving it
(green). The harness saved and restored the test source and both fixtures in
a `finally` block. Git status and byte comparisons below were captured **before**
that restoration. The harness asserted the command failed and both fixtures
changed before the fix; after the fix it asserted the replay diagnostic,
nonzero exit, zero changed fixtures, and empty Git status.

Before the fix:

```text
$ go test ./cmd/backtest -run ^TestDeclaredVariantGolden$ -count=1 -v -update
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:131: Variant golden journal and registry fixture rewritten; review both before accepting
    variant_golden_test.go:144: Variant replay: backtest: open the journal to replay: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestDeclaredVariantGolden1308912240/001/journal.golden.jsonl.missing: no such file or directory
--- FAIL: TestDeclaredVariantGolden (0.04s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.194s
FAIL
exit status: 1
$ git status --porcelain -- cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json
 M cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl
 M cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json
fixture files changed: 2
RED: replay failed after rewriting both fixtures
```

After the fix, with the same failing replay probe:

```text
$ go test ./cmd/backtest -run ^TestDeclaredVariantGolden$ -count=1 -v -update
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:124: Variant replay: backtest: open the journal to replay: open /var/folders/wg/63d7lf093s5gnkj6gp_l3h8c0000gn/T/TestDeclaredVariantGolden2914439266/001/journal.golden.jsonl.missing: no such file or directory
--- FAIL: TestDeclaredVariantGolden (0.04s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.199s
FAIL
exit status: 1
$ git status --porcelain -- cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json
fixture files changed: 0
PASS: replay failure preserved both fixtures byte-for-byte
```

For the successful replay control, kept the candidate build suffix and used
the original `doReplay(path, &log)` call. The harness asserted exit status 0
and different bytes in **both** fixtures, then restored both. This demonstrates
actual regeneration, rather than merely rewriting already-identical bytes:

```text
$ go test ./cmd/backtest -run ^TestDeclaredVariantGolden$ -count=1 -v -update
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:135: Variant golden journal and registry fixture rewritten; review both before accepting
--- PASS: TestDeclaredVariantGolden (0.05s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.201s
exit status: 0
$ git status --porcelain -- cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json
 M cmd/backtest/testdata/variants/profit-protecting-stop/journal.golden.jsonl
 M cmd/backtest/testdata/variants/profit-protecting-stop/registry/sha256-1f28d95f466d57e9493d4410d1db740ff594353eb8dc2f6d100be14ac6e95cb1/golden.json
fixture files changed: 2
PASS: successful replay rewrote both fixtures with the candidate build
```

With all temporary changes restored, the normal update command also passed
and left both committed fixtures byte-identical:

```text
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:135: Variant golden journal and registry fixture rewritten; review both before accepting
--- PASS: TestDeclaredVariantGolden (0.06s)
PASS
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.208s
```

Repeated the `StopKindRaised` → `StopKindInitial` predicate perturbation
shown above after moving replay. It still fails with the real decision diff;
restored the predicate in a `finally` block:

```text
=== RUN   TestDeclaredVariantGolden
    variant_golden_test.go:67: possible rules change in declared Variant "profit-protecting-stop" (ADR 0016): journal bytes changed or run failed; review decisions and RulesVersion before accepting -update
        run error: backtest: fills: apply event fill:AAPL:2026-01-22T00:00:00.000000000Z:8 at sequence 31: strategy: instrument "AAPL": stop fill "fill:AAPL:2026-01-22T00:00:00.000000000Z:8" would close campaign "campaign:AAPL:2026-01-22T00:00:00.000000000Z" with an invalid exit: invalid campaign exited payload: sizing: invalid protective stop level: protective stop level 128.61000000000004 must be below the entry price 127.885 for an initial stop: a stop only reaches entry once the stop ladder has raised it
        decision diff: the got stream ends here; the other continues with sequence 44, event units-stopped-fill:AAPL:2026-01-22T00:00:00.000000000Z:8:AAPL:2026-01-22T00:00:00.000000000Z (strategy.campaign.units-stopped)
--- FAIL: TestDeclaredVariantGolden (0.04s)
FAIL
FAIL	github.com/richard-whittemore/TrendInvesting/cmd/backtest	0.187s
FAIL
exit status: 1
```

After restoring all mutations, `make check` passed with exit status 0 on
Go 1.24.4, darwin/arm64: total coverage **94.9%**, `cmd/backtest` **87.6%**.
The restricted attempt failed because transport tests could not bind local
Unix sockets; reran the complete gate with socket access using the cache
settings shown above. No check was disabled. Gate output (per-function
coverage listing omitted):

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
ok  	github.com/richard-whittemore/TrendInvesting/cmd/backtest	11.463s	coverage: 87.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/cmd/transport-spike	1.142s	coverage: 76.0% of statements
	github.com/richard-whittemore/TrendInvesting/cmd/trend-investing		coverage: 0.0% of statements
?   	github.com/richard-whittemore/TrendInvesting/internal/buildinfo	[no test files]
ok  	github.com/richard-whittemore/TrendInvesting/internal/coverageaudit	21.427s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/event	4.190s	coverage: 99.9% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/fills	1.597s	coverage: 99.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/floatingpointaudit	23.543s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/glossaryaudit	3.321s	coverage: [no statements]
ok  	github.com/richard-whittemore/TrendInvesting/internal/indicator	1.114s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/journal	2.865s	coverage: 100.0% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/registry	1.134s	coverage: 99.3% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/replay	1.162s	coverage: 98.8% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/sizing	3.780s	coverage: 99.5% of statements
ok  	github.com/richard-whittemore/TrendInvesting/internal/strategy	2.994s	coverage: 91.6% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport	4.251s	coverage: 90.7% of statements
ok  	github.com/richard-whittemore/TrendInvesting/transport/spike	1.154s	coverage: 90.3% of statements
total:											(statements)			94.9%
COVERAGE_MIN=80.0 ./scripts/check-coverage.sh coverage.out
total coverage: 94.9% (minimum: 80.0%)
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
