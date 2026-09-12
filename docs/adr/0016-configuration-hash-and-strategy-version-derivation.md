# ADR 0016: ConfigurationHash and StrategyVersion derivation

- Status: Accepted
- Date: 2026-09-11

## Context

The envelope carries `ConfigurationHash` and `StrategyVersion` as opaque non-empty strings (#5). That is correct for transport-level validation, but nothing defined how either is **computed**.

ADR 0012 depends on every run being retained under its configuration hash, and on "the same configuration always produces the same hash; any parameter change produces a different one" (#39). That requires a deterministic derivation from a declared Baseline or Variant configuration: which fields are included, in what order, with what encoding, and what happens to fields added later.

`StrategyVersion` had no defined relationship to `internal/buildinfo.Version`. A journal that says `turtle-baseline-1.0.0` must be traceable to a specific build, and to whether that build's rules can be trusted to replay another build's journal byte-for-byte.

Without both, a configuration hash is a label, not evidence.

## Decision

### Both values are engine mechanics, not strategy parameters

Neither `ConfigurationHash` nor `StrategyVersion` falls into ADR 0012's parameter provenance taxonomy (source-fixed, Baseline-declared adaptation, Variant-tunable): no source citation applies to either. They are derived identifiers the *engine* computes from a configuration and a build, not values chosen by anyone transcribing a rule or fitting a threshold. This is why their derivation belongs in an engineering ADR rather than a methodology one.

### ConfigurationHash

1. **Input** is the `strategy.configuration` payload exactly as journaled (`event.ConfigurationPayload`, whatever schema version is current), plus that schema version. Nothing else: not the strategy version, not the run span, not the data provenance tag. Those are run-level facts recorded beside the hash by #39, so the same configuration run over a different span or dataset is still recognisably the same configuration.
2. **Canonical encoding**: JSON with keys sorted lexicographically, no insignificant whitespace, numbers rendered by Go's shortest round-trip float64 formatting (`strconv.FormatFloat(v, 'g', -1, 64)`), booleans and strings as JSON, nested objects canonicalised recursively. `internal/event`'s `canonicalJSON` builds this by walking the value with reflection rather than delegating to `encoding/json.Marshal`, so the result depends on neither `encoding/json`'s map-iteration order nor its own float formatting — both of which are not part of Go's compatibility promise. This makes the hash deterministic across Go versions by construction.
3. **Hash** = SHA-256 over `"configuration/v" + schemaVersion + "\n" + canonicalJSON`, hex-encoded, prefixed `sha256:` so a reader can tell the algorithm from the string.

   The schema version is **inside the hashed bytes**, not stored beside them. This is the answer to "what happens to fields added later": a schema-3 configuration and a schema-4 configuration carrying the same numbers are *different configurations*, because a field was added and the strategy's behaviour space changed with it. Putting the version inside the hash means that difference is visible from the hash alone, with no need to separately track and compare a schema-version field every time two hashes are compared.
4. **One exported function**: `event.ConfigurationHash(payload ConfigurationPayload) string`. A stability test pins the Baseline fixture's hash (changing the pin is a deliberate act, expected to accompany a schema bump or an encoding change) and a sensitivity test confirms every field, changed one at a time, changes the hash.
5. `strategy.NewReducer` takes the *payload*, not a caller-supplied hash, and derives `event.ConfigurationHash(payload)` itself. This is the single place in the codebase a configuration hash is computed: a caller can no longer construct a `Reducer` whose stored hash disagrees with what `event.ConfigurationHash` would compute for the same payload. `NewReducer` also validates the payload (`ConfigurationPayload.Validate`) before deriving a hash from it, so an invalid configuration can never produce a hash at all, rather than surfacing only later when a matching configuration event arrives. The existing fail-closed behaviour is unchanged: a configuration event whose envelope hash does not match the constructor's is rejected, and a second configuration event is rejected regardless of whether its hash matches (ADR 0006 freezes configuration at entry).

### StrategyVersion

`StrategyVersion = "<strategy-id>/<rules-version>+<build>"`, where:

- `strategy-id` is the configuration's `StrategyID` (e.g. `turtle-baseline`);
- `rules-version` is a hand-bumped semantic version declared in code (`internal/strategy.RulesVersion`) that changes **only when a rule changes** — a new ADR, a changed ladder, a fixed look-ahead (#10's one-bar N shift would have bumped it) — never for a refactor or any other change that leaves every existing journal replayable byte-identically;
- `build` is `buildinfo.Version` (the git-derived build identifier).

`event.ComposeStrategyVersion(strategyID, rulesVersion, build string) string` performs the composition as a pure string operation. It takes all three parts as plain strings rather than importing `internal/strategy` or `internal/buildinfo`: `internal/strategy` already imports `internal/event` for the wire contract, so `internal/event` cannot import `internal/strategy` back without a cycle, and it has no reason to import `internal/buildinfo` either. This keeps the composition usable from any layer, including the composition root that will eventually own `buildinfo.Version` and call it (#19).

`NewReducer` is unaffected by this half of the decision: it still takes `strategyVersion` as an opaque caller-supplied string, exactly as before. `ConfigurationHash` has exactly one payload to derive itself from, which is why its derivation moved inside the constructor; `StrategyVersion`'s composition has three independently-supplied inputs (a configuration's `StrategyID`, code's `RulesVersion`, and the running build), and remains the caller's responsibility to assemble via `event.ComposeStrategyVersion` before calling `NewReducer`.

**Rationale for the rules-version axis:** two builds with the same rules version must replay each other's journals byte-identically, and a rules-version bump is the declaration that they no longer will. **Replay equivalence (#20) compares runs on the rules version alone; the build suffix is traceability only** — it lets a journal entry be traced back to a specific git-derived build without that identity ever being mistaken for a claim about the rules that produced it.

## Consequences

- A configuration hash is now evidence, not a label: given a `ConfigurationPayload`, anyone can independently recompute the hash and confirm a journal's provenance claim, and a schema bump is guaranteed to be visible as a hash change even when a run's numbers did not otherwise change.
- `NewReducer`'s signature changed from `NewReducer(strategyVersion, configurationHash string)` to `NewReducer(strategyVersion string, payload event.ConfigurationPayload) (*Reducer, error)`. Every existing test fixture that passed a literal configuration-hash string was updated to pass a payload (or, where the fixture's own point was an invalid payload, to expect the resulting error).
- `event.canonicalJSON` is a general-purpose canonical encoder (structs, maps with string keys, slices, and JSON scalars), not `ConfigurationPayload`-specific, so it is directly testable for the two properties that make it trustworthy: independence from Go struct field declaration order, and independence from map insertion order.
- `internal/strategy.RulesVersion` starts at `1.0.0`. The first ticket that changes a rule (as opposed to refactoring, adding provenance, or fixing a non-rule bug) must bump it and say so in that ticket's Work log — this ADR does not itself define a process for detecting a missed bump, which is a gap worth a future ticket if a rules change ever lands without one.
- #19 (the backtest command and journal) and #39 (the run registry) can now be built against a concrete, tested derivation for both fields instead of an unspecified opaque string.
