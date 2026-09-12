package event

// ComposeStrategyVersion returns the Envelope.StrategyVersion this project
// stamps on every emission: "<strategy-id>/<rules-version>+<build>" (#50,
// ADR 0016).
//
//   - strategyID is the running configuration's StrategyID (e.g.
//     "turtle-baseline").
//   - rulesVersion is the hand-bumped semantic version of the strategy rules
//     in code, changed only when a rule changes — a new ADR, a changed
//     ladder, a fixed look-ahead — never for a refactor
//     (internal/strategy.RulesVersion).
//   - build is the running build's identifier (internal/buildinfo.Version).
//
// Two builds sharing the same rulesVersion must replay each other's
// journals byte-identically — that is what replay equivalence (#20)
// compares on — and the build suffix exists for traceability only, so a
// journal entry can be traced back to a specific git-derived build without
// that identity ever being mistaken for a claim about the rules that
// produced it.
//
// This is a pure string composition, not a lookup: internal/strategy already
// imports internal/event for the wire contract, so internal/event cannot
// import internal/strategy back without an import cycle, and it has no
// reason to import internal/buildinfo either. Taking all three parts as
// plain strings keeps this usable from any layer, including the eventual
// composition root that owns buildinfo.Version.
func ComposeStrategyVersion(strategyID, rulesVersion, build string) string {
	return strategyID + "/" + rulesVersion + "+" + build
}
