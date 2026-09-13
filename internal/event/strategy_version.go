package event

import (
	"fmt"
	"strings"
)

// StrategyVersionDelimiters are the two characters ADR 0016 makes structural
// in a composed StrategyVersion. A strategy id or rules version containing
// either cannot be recovered from the composed string, so neither may appear
// in one (ConfigurationPayload.Validate rejects them in StrategyID).
const StrategyVersionDelimiters = "/+"

// ComposeStrategyVersion returns the Envelope.StrategyVersion this project
// stamps on every emission: "<strategy-id>/<rules-version>+<build>" (ADR
// 0016).
//
//   - strategyID is the running configuration's StrategyID (e.g.
//     "turtle-baseline"), which must contain neither delimiter.
//   - rulesVersion is the hand-bumped semantic version of the strategy rules
//     in code, changed only when a rule changes — a new ADR, a changed
//     ladder, a fixed look-ahead — never for a refactor
//     (internal/strategy.RulesVersion).
//   - build is the running build's identifier (internal/buildinfo.Version).
//     It alone may contain "+".
//
// Two builds sharing the same rulesVersion must replay each other's journals
// byte-identically — that is what replay equivalence compares on — and the
// build suffix exists for traceability only.
func ComposeStrategyVersion(strategyID, rulesVersion, build string) string {
	return strategyID + "/" + rulesVersion + "+" + build
}

// DecomposeStrategyVersion is ComposeStrategyVersion's inverse: it splits a
// StrategyVersion back into the strategy id, rules version, and build it was
// composed from. The FIRST "/" separates the strategy id from the rest, and
// the FIRST "+" in what remains separates the rules version from the build.
//
// A string this project never composed is refused rather than partially
// decomposed, because a caller comparing rules versions must not act on a
// part that was never there.
//
// # Clean parts are what make the decomposition unique, and they are enforced elsewhere
//
// "desk/turtle" composed with rules "1.0.0" yields "desk/turtle/1.0.0+abc",
// which cutting at the first "/" reads as the id "desk" and the rules version
// "turtle/1.0.0". The check below catches that one, but it CANNOT catch the
// whole class: "desk/turtle+eu" composes to "desk/turtle+eu/1.0.0+abc", which
// decomposes cleanly to the id "desk", the rules version "turtle", and a
// build of "eu/1.0.0+abc" — a perfectly legitimate triple, since only the
// build may carry delimiters. Two different triples produce that one string
// and nothing in it distinguishes them.
//
// So uniqueness is not a property this function can establish on its own. It
// holds because ConfigurationPayload.Validate rejects a StrategyID carrying
// either delimiter, which makes every composed string this project writes
// have exactly one decomposition into legal parts. The check below is the
// second line, not the guarantee.
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
	if strings.Contains(rules, "/") {
		return "", "", "", fmt.Errorf("event: strategy version %q does not decompose unambiguously: the rules version reads as %q, so the strategy id carried a %q", strategyVersion, rules, "/")
	}
	return id, rules, buildPart, nil
}
