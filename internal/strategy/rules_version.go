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
// Such a stop leaves the Unit risk-free rather than corrupted — it is
// "reachable only by raising, never by an initial stop, which must sit
// strictly below entry" (CONTEXT.md: "risk-free"), and contributes exactly
// zero to the Campaign's aggregate open risk.
// The Baseline never reaches that state — its maximum raise is 1.5N against
// a 2N stop — so no Baseline journal changes. A declared Variant with a
// narrow enough Stop Multiple does reach it, and there the two builds
// disagree outright: the older one fails the run where this one records the
// exit. That is a decision the rules now make differently, which is what
// this version names, so the two cannot share it.
const RulesVersion = "1.2.0"

// RulesSurfaceFingerprint is a SHA-256 hash, hex-encoded, over the module's
// declared rule surface: every Rule* and ADR* constant (naming a rule and
// the ADR it cites, e.g. RuleEntryChannelBreakout, ADREntryChannelBreakout),
// plus every declared numeric rule constant in internal/indicator,
// internal/sizing, and internal/strategy — the Wilder period
// (indicator.DefaultPeriod) and the Drawdown Step fractions
// (sizing.DrawdownStepRetainedFraction and this package's own drawdown
// threshold) among them.
//
// TestDeclaredRuleSurfaceMatchesItsPinnedFingerprint recomputes it from the
// source and fails if it no longer matches, naming RulesVersion and ADR 0016
// as the fix.
//
// The numeric sweep is deliberately over-inclusive: it takes every numeric
// constant those packages declare, not a maintained list of the ones that
// are rules, because a maintained list silently omits the next rule someone
// adds and that omission is the whole defect this guards against. So a loop
// bound or a buffer size in one of those packages trips it too. That is a
// re-pin without a version bump, not a bump — the test's own failure says
// so, and bumping for a change that leaves every journal replayable is
// itself a defect.
//
// It catches a changed CONSTANT: a rule renamed, re-cited, or given a
// different numeric value with RulesVersion left where it was. It does not
// catch a changed PREDICATE — validation logic whose behaviour changes with
// no Rule*, ADR*, or numeric rule constant touched, which is exactly what
// moved RulesVersion from 1.1.0 to 1.2.0 above. That gap is real and is not
// closed here.
const RulesSurfaceFingerprint = "593bcc1fe74331b46679b04adbacaa1c51feb09e9811a2c91e1edf3f51939178"
