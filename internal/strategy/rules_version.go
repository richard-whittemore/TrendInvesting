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
//
// Bumped 1.3.0 -> 1.4.0 when the reducer began recording each held Unit's
// Exit Order (CONTEXT.md: "Exit Order"): a strategy.exit-order.set decision
// whenever the level at which that Unit's one sell order rests changes — the
// higher of its Protective Stop and, while an Exit-Channel exit is proposed,
// that exit's level (ADR 0005). No existing decision changes and no fill
// changes, but every journal with a Campaign gains decisions, so the two
// builds no longer replay each other's journals byte-identically.
//
// Bumped 1.4.0 -> 1.5.0 when a backtest began filling each Unit's Exit
// Order as the one sell order it is (ADR 0005's 2026-09-24 amendment),
// rather than its Protective Stop and a proposed Exit-Channel exit as two
// competing orders filled worst price first. No decision this package makes
// from a given input changes, so a journal still replays byte-identically
// under either build. But the fills a backtest feeds this package are part
// of what a Baseline run produces: wherever a bar reaches an exit level
// above a Unit's stop, that Unit now sells at the exit level rather than at
// its stop, and every decision downstream of that fill changes with it —
// the same reason a changed entry fill price moved 1.0.0 to 1.1.0. Rerunning
// a 1.4.0 run's own inputs through this build therefore produces a
// different journal, so the two cannot share a version.
//
// Bumped 1.5.0 -> 1.6.0 when a day's Adds and entries began to be decided
// when its Session closes rather than at each instrument's own bar (ADR
// 0021): a completed bar still evaluates, signals and proposes exits, but
// its entry and Add proposals are emitted in reply to the
// market.session.closed that ends its Session, Adds before entries, entries
// in ranked order. Each proposal's content is unchanged, but it is caused by
// a different input, and every journal gains one input per Session, so no
// 1.5.0 journal replays under this build: it has no session closes, and its
// second Session fails closed.
//
// Bumped 1.6.0 -> 1.7.0 when every entry and Add fill began to be debited,
// at its actual cost, from the cash the next entry or Add is checked against,
// until a snapshot stated as of the fill or later reflects it (ADR 0020:
// "available = basis - every actual fill cost"). Before, each Unit was
// compared with the snapshot's unmoved figure, so a Campaign's Units could
// each pass while together costing several times the cash the account held.
// Given the same inputs, a run whose Units together outspend its cash now
// declines, with insufficient-cash, a Unit the older build proposed, and
// strategy.proposal.declined advances to payload schema 4, so the two builds
// no longer replay each other's journals.
//
// Bumped 1.7.0 -> 1.8.0 when a backtest's simulated account began stating
// every Session's close in an account.snapshot: the opening cash, less every
// buy's cost and commission, plus every sell's proceeds less commission,
// with holdings marked at the split-adjusted close (ADR 0020's Consequences;
// ADR 0021, as amended). Before, a backtest stated one opening snapshot, so
// exit proceeds never returned to the cash later Units are checked against,
// and every Campaign after the first could be declined for cash the account
// in fact held. No decision this package makes from a given input changes,
// so a 1.7.0 journal still replays byte-identically under this build, as a
// 1.4.0 journal did under 1.5.0. But the snapshots are part of what a
// backtest feeds this package: every journal gains one per Session, the
// opening one moves to after the first Session, and a run whose Campaign
// exits now funds entries and Adds the older build declined. Rerunning a
// 1.7.0 run's own inputs through this build produces a different journal,
// so the two cannot share a version.
const RulesVersion = "1.8.0"

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
// internal/strategy/testdata/decision-corpus (decision_corpus_test.go, #100)
// narrows that gap from the other side: it pins what the reducer actually
// DECIDES for every scenario the test suite drives it through, rather than
// what constants it declares, and fails when a decision changes under an
// unchanged RulesVersion — including a changed predicate, which is what
// moved RulesVersion from 1.2.0 to 1.3.0. It has its own, differently honest
// limit: it only catches a predicate change that some recorded scenario
// actually exercises (see its own doc comment).
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
	// Changed from 1.3.0 by the Exit Order's own rule and its ADR citation
	// (event.RuleExitOrderHigherOfStopAndExitChannel,
	// event.ADRExitOrderRestsAtTheLevel).
	"1.4.0": "11413d17f3f22208d7682620c110aa68d3cf4e4c48ea7cd9bc8ea5cbadde7bc5",
	// Unchanged from 1.4.0: the 1.5.0 change is in how the fill simulator
	// fills the Exit Order, not in any constant this surface declares.
	"1.5.0": "11413d17f3f22208d7682620c110aa68d3cf4e4c48ea7cd9bc8ea5cbadde7bc5",
	// Unchanged from 1.5.0: the 1.6.0 change is when Adds and entries are
	// decided, not any constant this surface declares.
	"1.6.0": "11413d17f3f22208d7682620c110aa68d3cf4e4c48ea7cd9bc8ea5cbadde7bc5",
	// Unchanged from 1.6.0: the 1.7.0 change is a changed predicate, the
	// cash a Unit is checked against, not any constant this surface declares.
	"1.7.0": "11413d17f3f22208d7682620c110aa68d3cf4e4c48ea7cd9bc8ea5cbadde7bc5",
	// Unchanged from 1.7.0: the 1.8.0 change is in the account a backtest
	// states, not any constant this surface declares.
	"1.8.0": "11413d17f3f22208d7682620c110aa68d3cf4e4c48ea7cd9bc8ea5cbadde7bc5",
}
