package strategy

// RulesVersion is the semantic version of this package's trading rules —
// System 2's Entry/Exit Channels, the half-N Add ladder, the 2N Protective
// Stop, and the Sizing Modes ADR 0003 declares (#50, ADR 0016).
//
// It is hand-bumped only when a RULE changes: a new ADR, a changed ladder, a
// fixed look-ahead (#10's one-bar N shift would have bumped it) — never for
// a refactor, a performance change, or any other change that leaves every
// existing journal replayable byte-identically. It composes into
// Envelope.StrategyVersion alongside the running configuration's StrategyID
// and the build (event.ComposeStrategyVersion).
//
// Replay equivalence (#20) compares two runs on this value alone: two builds
// sharing a RulesVersion must replay each other's journals byte-identically,
// and bumping it is the declaration that they no longer will. The build
// suffix that comes with it (internal/buildinfo.Version) is traceability
// only, never part of that comparison.
//
// Bumped 1.0.0 -> 1.1.0 for #79: the entry level a trade proposal names
// changed from the breakout bar's own high to the Entry Channel high, which
// changes the price every entry fills at and so every journal the Baseline
// produces from here on. #84 (not yet built when this landed) will pin a
// fingerprint over the declared rule surface so a future rule change like
// this one fails a test if RulesVersion is not moved with it; until then this
// is a plain, hand-made bump with this comment as its record.
const RulesVersion = "1.1.0"
