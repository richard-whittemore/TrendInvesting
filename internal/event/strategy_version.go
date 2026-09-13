package event

import (
	"fmt"
	"strings"
)

// ComposeStrategyVersion returns the Envelope.StrategyVersion this project
// stamps on every emission: "<strategy-id>/<rules-version>+<build>" (ADR
// 0016).
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
// journals byte-identically — that is what replay equivalence compares on —
// and the build suffix exists for traceability only, so a
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

// DecomposeStrategyVersion is ComposeStrategyVersion's inverse: it splits a
// StrategyVersion back into the strategy id, rules version, and build it was
// composed from.
//
// Replay equivalence compares two runs on rulesVersion alone — the build
// suffix is traceability only (ADR 0016) — so this is exposed as its own
// function rather than requiring every caller that needs just that one axis
// to re-derive the split. The format is fixed: the FIRST "/" separates
// strategyID from the rest, and the FIRST "+" in what remains separates
// rulesVersion from build. A string that does not contain both separators,
// or that contains one where any resulting part is empty, is refused rather
// than partially decomposed: a caller comparing rules versions must not
// silently accept a strategyVersion this project never composed.
func DecomposeStrategyVersion(strategyVersion string) (strategyID, rulesVersion, build string, err error) {
	id, rest, ok := strings.Cut(strategyVersion, "/")
	if !ok {
		return "", "", "", fmt.Errorf("event: strategy version %q does not contain the \"/\" separating strategy id from rules version and build", strategyVersion)
	}
	rules, buildPart, ok := strings.Cut(rest, "+")
	if !ok {
		return "", "", "", fmt.Errorf("event: strategy version %q does not contain the \"+\" separating rules version from build", strategyVersion)
	}
	if id == "" || rules == "" || buildPart == "" {
		return "", "", "", fmt.Errorf("event: strategy version %q has an empty strategy id, rules version, or build", strategyVersion)
	}
	return id, rules, buildPart, nil
}
