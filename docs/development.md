# Development guide

## Principles

1. Keep strategy and risk calculations deterministic and side-effect free.
2. Treat source methodology rules separately from experimental variants.
3. Record enough immutable evidence to explain every accepted and rejected decision.
4. Fail closed on unknown schemas, missing sequences, stale data, or uncertain brokerage state.
5. Prefer table-driven tests and replay fixtures over behavior hidden inside LEAN callbacks.

## Floating-point determinism: never leave a multiply-add fusible

Go permits an implementation to fuse `a + b*c` into a single fused multiply-add, "possibly across statements", and arm64 does while amd64 does not. The fused form keeps the full-precision product, so the two architectures produce results that differ in the last bits — and a platform whose journal must be byte-identical for replay equivalence (ADR 0017) cannot afford that. This is the determinism rule in `.greptile/rules.md` applied to the arithmetic itself: same inputs, same configuration, same code version, same decisions — on any machine.

An explicit conversion is the only barrier the language guarantees, so every product that feeds an addition or subtraction is rounded before it. `internal/sizing.Product` performs that conversion and names the intent; `internal/indicator` and `internal/fills`, which do not import that package, state the same barrier inline:

```go
total += sizing.Product(risk, dollarsPerPoint)      // an accumulator
level := entryPrice - float64(stopMultiple*campaignN) // inline, same barrier
```

Three things to know:

- **`x += a*b` is the same shape** as `x = x + a*b`, and it is the worst case: the divergence compounds with every term instead of appearing once. The weighted entry and exit prices, and the aggregate open risk, are all accumulators.
- **Assigning the product to a variable first does not help.** The specification allows fusion across statements; only the conversion is a barrier.
- **`a*b*c` with no addition is not fusible** and needs nothing. Do not "fix" it.

Round in the producer *and* in any `event` payload validator that re-derives the same value, or the two will disagree.

It cost a real defect to learn, twice over. `internal/strategy`'s whole-life exit price fused on arm64, and the committed golden journal — the first artifact in this repository that has to be byte-identical across machines — failed in CI on amd64 while passing locally. The first sweep then missed every `+=` site, because the walk that found the others only looked at expressions and not at assignments; the golden passed anyway, because that fixture's numbers happened not to differ at those sites. The fixture now has four Units whose products need more than 53 bits, and `cmd/backtest`'s fusion tests hold that sensitivity in place.

`internal/floatingpointaudit` enforces the local `+`, `-`, `+=` and `-=` shapes
in production and test Go files throughout the module, using `go/types` to exempt integer
arithmetic and compile-time constant products. Explicit product conversions
are rounding barriers. Generic constraints are checked for permitted floating-point
terms, including mixed integer/float unions; embedded constraints intersect their
type sets. The test uses the Go toolchain's package selection, so each CI
architecture checks its own active files, including internal and external test
packages, packages containing only tests, and `CgoFiles`. Test-package import
remapping uses the corresponding export data. Cgo's generated
Go inputs provide type information for C types and source locations for diagnostics;
the original sources also participate in test-cache invalidation. Test expectations
need the same rounding barriers as production calculations: otherwise both can
fuse and agree locally while concealing cross-architecture differences.

The scope includes `cmd/`, `transport/` and `transport/spike`, without a benchmark
exception. The spike generates benchmark traffic rather than journalled trading
decisions, but explicit rounding also serves its repeatable-payload invariant.
An audit of `cmd/backtest` found no floating-point decision arithmetic: it copies
fixture/configuration inputs and delegates calculations to `internal/strategy`
and `internal/fills`. Ordinary transport code frames and validates envelopes
without computing prices or risk. The wider guard enforces rounding wherever
future arithmetic is added. See [the scope audit](audits/floating-point-scope.md)
for the exposed sites and regression evidence.

The guard does not trace products across statements or function calls, including
inlined callees. CI runs the Go suite on amd64 and arm64 against the same committed
goldens; sensitive fixtures remain necessary to expose semantic divergence.
The required check `go` succeeds only when both architecture jobs succeed.

## Source comment standard

A doc comment states three things and no more:

1. **What the thing does** — one sentence, plainly.
2. **The rule it implements, with its citation** — a page/timestamp reference into `docs/methodology/`, an ADR number, or a `CONTEXT.md` term. Principle 3 above.
3. **Any invariant a caller must uphold** — including a warning that exists because the opposite was once wrong (a look-ahead bug, a schema misread, a risk-multiplication defect). That warning is exactly as load-bearing as the code beside it and must survive any later edit to the comment.

A doc comment does **not**:

- Narrate what the next line plainly does.
- Restate its own rationale in a second or third paragraph once the first has made the point.
- Reference a ticket number, a PR number, a review round, or "call site N". That information is reachable from `git blame` to the commit, the linked issue, and the PR — a comment naming a ticket is stale the day it closes, and it is written for the reviewer of that week, not the maintainer of next year.

When trimming an existing comment to this standard, treat every citation and every invariant as a fact, not prose: if removing a sentence would delete a fact recorded nowhere else, move the fact to the relevant ADR or the issue's Findings before deleting the sentence — never delete a fact outright.

**Good**, from `internal/indicator/channel.go` — states the rule with its source, then a named invariant that exists because the opposite was a real, shipped bug:

```go
// EntryChannel is a rolling window that reports the highest high among the
// last Length completed bars Added to it (CONTEXT.md: "Entry Channel"). The
// Turtle Rules p.19: System 2 enters when price exceeds the highest high of
// the preceding 55 completed bars (ADR 0002).
//
// # Evaluate-then-add: the ordering that fixes the prototype's headline bug
//
// Extreme reports the channel high computed from the values Added so far,
// and nothing more. Callers MUST call Extreme to evaluate the bar under
// decision BEFORE calling Add with that same bar's high. Getting this
// backwards — adding the current bar to the window and then reading
// Extreme — is the exact defect the legacy QuantConnect prototype shipped
// with...
```

**Bad** — this codebase's own `event.FillSchemaVersion` comment, before this standard was applied to it:

```go
// Bumped to 2 for #12: Kind and CampaignID were added, and Kind is required
// (an empty Kind decodes from a schema-1 record and is not a recognised
// value, so a schema-1 fill is rejected outright rather than silently
// interpreted as an entry — ADR 0015's rule, applied here at the payload
// level, the same way #10 bumped ConfigurationSchemaVersion for
// DollarsPerPoint and RiskAtStopFraction).
```

The ticket numbers age the moment those tickets close, and the same fact (which field forced which version, and why an old record must be rejected rather than reinterpreted) is stated without them, in the current source: a bulleted, per-version list citing ADR 0015 once, with no ticket number anywhere.

## Local checks

Run the same checks used by CI:

```sh
make check
```

To run a backtest and verify the journal it writes, see `docs/running-a-backtest.md`.

Go code must be formatted with `gofmt`. New behavior should include focused tests, including failure cases and invariant checks.

## The uncovered-branch audit

A coverage percentage cannot detect a branch that has become unreachable: dead code raises the denominator and moves the score by a fraction of a point, so a defect that makes a path impossible to execute — and any second defect sitting on that path — passes a coverage floor unremarked. That has happened here twice.

`internal/coverageaudit` compares the SET of statements no test executes against `exclusions.json`, a checked-in list of the blocks that cannot be executed. Every entry names a reason, and only two are admissible:

- **unreachable by construction** — a value this code has just built cannot fail its own contract (`json.Marshal` after the validated-payload-json invariant below is established; `Validate` on a payload assembled from already-validated figures a few lines above);
- **unreachable by a named domain invariant** — a rule makes the state the branch tests for impossible, and the reason says which rule. Where the invariant is cheap to assert, a test asserts it, so the day it stops holding the list is forced to be re-read rather than quietly becoming wrong.

"Nobody has got round to it" is not a category. A missing test is a missing test.

The audit fails in both directions: an unlisted statement that becomes uncovered, and a listed one that becomes covered or no longer exists. It generates its own coverage profile, so `go test ./...` enforces it; set `COVERAGE_AUDIT_PROFILE` to reuse one you already have. After a refactor renames a function, re-author the list wholesale rather than by hand:

```sh
COVERAGE_AUDIT_DUMP=/tmp/uncovered.json go test ./internal/coverageaudit/
```

The audit covers `internal/`, where every strategy, risk and reconciliation rule lives. `cmd/` is composition and is held to its own tests — except the paths that decide whether a failed run still leaves evidence, which are held to the same bar as the domain.

### validated-payload-json

For every event payload `p`, `p.Validate() == nil` implies `json.Marshal(p)`
succeeds, provided the value is not mutated between the two calls. Each
timestamp is checked by `writableTime`, which calls `time.Time.MarshalJSON`;
required timestamps are also checked for zero. Numeric validation checks every
float, including nested structs and slice elements, for finiteness.

`internal/event`'s `TestValidatedPayloadsMarshal` tests this implication for
every payload type with valid fixtures, boundary mutations of every JSON-visible
leaf, and reproducible generated mutations. `TestMarshalInvariantIncludesEveryPayload`
compares the fixtures with the package's declared payload types, so adding a
type requires extending the property test. The traversal fails on unsupported
field types instead of silently skipping them. `TestPayloadTimestampsRejectUnwritableTimes`
independently tests a year outside 0–9999 and a +24:00 offset for every timestamp
field. These are regression checks, not substitutes for asking the encoder.

The timestamp-free payloads are `ConfigurationPayload` (ten finite-checked
floats, including `NotionalAccount` and `Commission`, plus strings and integers),
`EngineStatePayload` (three strings), and `RunCompletedPayload` (empty). They
participate in the same property test. The invariant applies to payloads, not
envelopes or journal headers: `journal.TestWriteReturnsTimestampEncodingErrors`
exercises their encoding-error path, which must not be excluded from coverage.

## The decision corpus: a rule change without a RulesVersion bump

`internal/strategy.RulesVersion` is hand-bumped (ADR 0016). Two guards check that a bump actually happened:

- `RuleSurfaceFingerprints` (`rules_version.go`) hashes every declared `Rule*`/`ADR*` and numeric rule constant. It catches a renamed or re-valued constant.
- `internal/strategy/testdata/decision-corpus/<RulesVersion>.json` pins `{scenario name: hash}` for every scenario `stream.run()` (`campaign_test.go`) drives the reducer through, hashing its emitted decisions and any run error, with `strategy_version` excluded so relabelling a build never trips it. `TestMain` (`decision_corpus_test.go`) compares the current run against the pinned file for the CURRENT `RulesVersion` once every test has finished.

Neither guard reads a rule's own logic. The fingerprint sees a changed CONSTANT; the corpus sees a changed DECISION. Between them they catch a rule change that either renames/re-values a declared constant or alters the outcome of some recorded scenario — but a changed PREDICATE that no recorded scenario happens to exercise is invisible to both, honestly (see the corpus's own doc comment). This is why the fixture set matters: a scenario that never exercises the new behavior gives the corpus nothing to catch it with.

**What trips the corpus:**

- A pinned scenario's hash differs from what this run recorded: the reducer decided something different under an unchanged `RulesVersion`. Per ADR 0016 this is a rule change, not a journal-replay divergence, and needs (1) a `RulesVersion` bump, (2) a new `testdata/decision-corpus/<new-version>.json`, and (3) a new row in `RuleSurfaceFingerprints`.
- A scenario recorded but not pinned (a new test) fails, asking for `-update-decision-corpus` (adds only).
- On an unfiltered run (no `-run`, no `-short`) a pinned scenario that was not recorded at all (a renamed or deleted test) fails, asking for `-update-decision-corpus` to drop it. A filtered run never enforces this, since it necessarily recorded only a subset.

**After a deliberate rule change:**

1. Bump `internal/strategy.RulesVersion` and append its row to `RuleSurfaceFingerprints` (or record that the fingerprint is unchanged, as the 1.2.0 -> 1.3.0 predicate changes did — see that map's comments).
2. Run `go test ./internal/strategy/... -update-decision-corpus` unfiltered. Because the new version has no pinned file yet, this generates `testdata/decision-corpus/<new-version>.json` from the current tree in full.
3. Review the generated file's diff against the previous version's like any other evidence: it is what the reducer now decides for every scenario in the suite.
4. Never edit a previous version's corpus file — it is append-only history, exactly like `RuleSurfaceFingerprints`.

`-update-decision-corpus` never overwrites a changed hash for the CURRENT version, flag or no flag: that refusal is the tripwire itself. If it fired unexpectedly, the change was not the refactor it looked like; find out what state it changes before touching `RulesVersion`.

## Package boundaries

- `cmd/` contains executable composition only.
- `internal/event/` owns the shared event envelope and transport-level validation.
- `internal/replay/` owns deterministic application of recorded events.
- `internal/indicator/` owns pure, side-effect-free strategy arithmetic (True Range, N) with no knowledge of events or replay.
- `internal/sizing/` owns the pure risk arithmetic that turns a volatility reading into a whole number of shares and the Risk at Stop it implies (ADR 0003), kept separate from `internal/indicator/` because sizing commits capital rather than measuring a price series, and likewise knowing nothing of events or replay.
- `internal/strategy/` owns the `replay.Handler` reducers that turn a validated event stream into decision events.
- `internal/journal/` owns the run's journal: the append-only file of input and decision records, the hash chain that makes an edit to one detectable (ADR 0017), and the `Verify` path that recomputes it — knowing nothing of strategy rules, and deliberately not answering replay equivalence's question.
- `internal/registry/` owns the run registry: the record of every run under its configuration hash (ADR 0012), the closed status vocabulary, and the layout that makes the record append-only and mergeable — one file per run, in a directory named for the hash, with no index file for two branches to conflict over. It performs no I/O of its own: like `internal/journal/` it defines the record and where it belongs, and `cmd/` installs it.
- `internal/fills/` owns the intraday fill model (ADR 0005) and the cost model (ADR 0013): the resting orders in force for an instrument, learned from the reducer's own emissions, and the per-bar protocol (`RunBar`) that turns one completed bar into `execution.fill` events indistinguishable in shape from adapter-produced ones — kept separate from `internal/strategy/` because it decides what a venue did, never what the strategy should do.
- Future strategy packages must not import LEAN, database, or transport implementations.
- `adapter/lean/` documents and will contain the deliberately thin Python boundary.

## Pull requests

Each change should reference its GitHub issue (`#N`) and state:

- the rule, risk, or operational outcome addressed;
- the evidence and tests added;
- replay or audit impact;
- any new schema or configuration version; and
- unresolved assumptions.

Never hide a failed research result by rewriting or removing its recorded configuration and evidence.
