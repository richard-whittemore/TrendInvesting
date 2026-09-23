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
//
// Bumped 1.2.0 -> 1.3.0 when an accepted withdrawal began reducing the cash
// an entry or Add is checked against, immediately and with a floor of zero,
// while a deposit still waits for a snapshot (ADR 0020's cash-movement
// amendment). Given the same inputs, a run containing a withdrawal can now
// decline a Unit the older build proposed. A run with no cash movement
// decides exactly as before, but the two builds no longer replay each
// other's journals in general, so they cannot share a version.
const RulesVersion = "1.3.0"

// RuleSurfaceFingerprints records, for every RulesVersion this package has
// ever declared, a SHA-256 hash (hex-encoded) over the module's declared
// rule surface at that version: every Rule* and ADR* constant (naming a
// rule and the ADR it cites, e.g. RuleEntryChannelBreakout,
// ADREntryChannelBreakout), plus every declared numeric-shaped constant in
// internal/indicator, internal/sizing, and internal/strategy — the Wilder
// period (indicator.DefaultPeriod), the Drawdown Step fractions
// (sizing.DrawdownStepRetainedFraction and this package's own drawdown
// threshold), and sizing.StopKind's own iota values among them — except the
// ones rule_surface_exceptions.json names.
//
// TestDeclaredRuleSurfaceMatchesItsPinnedFingerprint recomputes the current
// fingerprint from source and compares it against the row for the CURRENT
// RulesVersion, failing if that version has no row at all. A row keyed by
// version, rather than one value that gets replaced, is what closes the gap
// a single pinned constant left open: changing a rule and then re-pinning,
// with RulesVersion left untouched, used to still pass, because nothing
// distinguished "the surface changed for THIS version" from "the surface
// changed to a DIFFERENT version's". Now the current version's row is a
// specific, named target, and a version with no row at all fails just as
// loudly as one whose row no longer matches.
//
// This is not unforgeable. Nothing stops a commit from editing an existing
// row instead of appending a new one, exactly as nothing stops a commit from
// rewriting a journal or a registry entry (ADR 0017, ADR 0018) — no
// checked-in value is. What it achieves instead is what those two do:
// editing the row for a version already released is a visibly different act
// from appending one for a new version — a diff that rewrites an old key
// instead of adding one — in a repository whose evidence is never supposed
// to be rewritten once recorded.
//
// The sweep itself takes every numeric-shaped constant those packages
// declare, not a maintained list of the ones that are rules, because a
// maintained list silently omits the next rule someone adds and that
// omission is the whole defect this guards against. Some of what it finds is
// not a trading rule at all — a loop bound, a buffer size —
// and rule_surface_exceptions.json names those, each with its own reason,
// the same shape internal/coverageaudit/exclusions.json uses. A maintained
// list is safe here in a way a maintained list of RULES would not be:
// forgetting to list an exception is safe, because the guard simply trips on
// the next change to that constant and a person looks; forgetting to list a
// rule would not be, which is why there is no equivalent list of rules to
// maintain and the sweep finds those on its own.
//
// It catches a changed CONSTANT: a rule renamed, re-cited, or given a
// different numeric value with RulesVersion left where it was. It does not
// catch a changed PREDICATE — validation logic whose behaviour changes with
// no Rule*, ADR*, or numeric rule constant touched, which is exactly what
// moved RulesVersion from 1.1.0 to 1.2.0 above. That gap is real and is not
// closed here.
//
// Only 1.2.0 has a row: the surfaces 1.0.0 and 1.1.0 actually declared
// cannot be recomputed from today's source, since the constants and rules
// that made them up have since changed or been renamed, so no entry is
// invented for either. This table starts where it can first be computed
// honestly.
var RuleSurfaceFingerprints = map[string]string{
	"1.2.0": "655e43354aa890c64ae02fc078c73274157657cd792adfa24d12fdf9b6bab57b",
	// Unchanged from 1.2.0: the 1.3.0 change is a changed predicate, not a
	// changed constant — exactly the gap described above.
	"1.3.0": "655e43354aa890c64ae02fc078c73274157657cd792adfa24d12fdf9b6bab57b",
}
