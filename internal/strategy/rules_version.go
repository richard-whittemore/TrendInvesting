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
// plus every declared numeric-shaped constant in internal/indicator,
// internal/sizing, and internal/strategy — the Wilder period
// (indicator.DefaultPeriod), the Drawdown Step fractions
// (sizing.DrawdownStepRetainedFraction and this package's own drawdown
// threshold), and sizing.StopKind's own iota values among them — except the
// ones rule_surface_exceptions.json names.
//
// TestDeclaredRuleSurfaceMatchesItsPinnedFingerprint recomputes it from the
// source and fails if it no longer matches, naming RulesVersion and ADR 0016
// as the fix.
//
// The sweep itself takes every numeric-shaped constant those packages
// declare, not a maintained list of the ones that are rules, because a
// maintained list silently omits the next rule someone adds and that
// omission is the whole defect this guards against. Some of what it finds is
// not a trading rule at all — a loop bound, a buffer size —
// and rule_surface_exceptions.json names those, each with its own reason,
// the same shape internal/coverageaudit/exclusions.json uses.
//
// A maintained list is safe here in a way a maintained list of RULES would
// not be: forgetting to list an exception is safe, because the guard simply
// trips on the next change to that constant and a person looks; forgetting
// to list a rule would not be, which is why there is no equivalent list of
// rules to maintain and the sweep finds those on its own.
//
// It catches a changed CONSTANT: a rule renamed, re-cited, or given a
// different numeric value with RulesVersion left where it was. It does not
// catch a changed PREDICATE — validation logic whose behaviour changes with
// no Rule*, ADR*, or numeric rule constant touched, which is exactly what
// moved RulesVersion from 1.1.0 to 1.2.0 above. That gap is real and is not
// closed here.
const RulesSurfaceFingerprint = "97b7134f2e406f62eb4db56b5bd29d1cde664262a82ffba022553d7924219447"
