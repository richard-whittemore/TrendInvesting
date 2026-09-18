package strategy

// RulesVersion is the semantic version of this package's trading rules —
// System 2's Entry/Exit Channels, the half-N Add ladder, the 2N Protective
// Stop, and the Sizing Modes ADR 0003 declares (ADR 0016).
//
// It is hand-bumped only when a RULE changes: a new ADR, a changed ladder, a
// fixed look-ahead (a one-bar N shift would have bumped it) — never for a
// refactor, a performance change, or any other change that leaves every
// existing journal replayable byte-identically. It composes into
// Envelope.StrategyVersion alongside the running configuration's StrategyID
// and the build (event.ComposeStrategyVersion). It shares StrategyID's
// [A-Za-z0-9._-]{1,64} constraint to be safely usable as a directory name
// (ADR 0012).
//
// Replay equivalence compares two runs on this value alone: two builds
// sharing a RulesVersion must replay each other's journals byte-identically,
// and bumping it is the declaration that they no longer will. The build
// suffix that comes with it (internal/buildinfo.Version) is traceability
// only, never part of that comparison.
//
// Bumped 1.0.0 -> 1.1.0 when the entry level a trade proposal names changed
// from the breakout bar's own high to the Entry Channel high, which changes
// the price every entry fills at and so every journal the Baseline produces
// from here on.
//
// Bumped 1.1.0 -> 1.2.0 when a Campaign exiting at a Protective Stop the
// Stop Ladder had raised to or above its entry price stopped being refused.
// The Baseline never reaches that state — its maximum raise is 1.5N against
// a 2N stop — so no Baseline journal changes. A declared Variant with a
// narrow enough Stop Multiple does reach it, and there the two builds
// disagree outright: the older one fails the run where this one records the
// exit. That is a decision the rules now make differently, which is what
// this version names, so the two cannot share it.
//
// A future change should pin a fingerprint over the declared rule surface so
// a rule change like this one fails a test if RulesVersion is not moved with
// it; until then these are plain, hand-made bumps with this comment as their
// record.
const RulesVersion = "1.2.0"
