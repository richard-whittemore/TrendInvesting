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

## Package boundaries

- `cmd/` contains executable composition only.
- `internal/event/` owns the shared event envelope and transport-level validation.
- `internal/replay/` owns deterministic application of recorded events.
- `internal/indicator/` owns pure, side-effect-free strategy arithmetic (True Range, N) with no knowledge of events or replay.
- `internal/sizing/` owns the pure risk arithmetic that turns a volatility reading into a whole number of shares and the Risk at Stop it implies (ADR 0003), kept separate from `internal/indicator/` because sizing commits capital rather than measuring a price series, and likewise knowing nothing of events or replay.
- `internal/strategy/` owns the `replay.Handler` reducers that turn a validated event stream into decision events.
- `internal/journal/` owns the run's journal: the append-only file of input and decision records, the hash chain that makes an edit to one detectable (ADR 0017), and the `Verify` path that recomputes it — knowing nothing of strategy rules, and deliberately not answering replay equivalence's question.
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
