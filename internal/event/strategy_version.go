package event

import (
	"fmt"
	"strings"
)

// StrategyVersionDelimiters are the two characters ADR 0016 makes structural
// in a composed StrategyVersion.
const StrategyVersionDelimiters = "/+"

// ComposeStrategyVersion returns the Envelope.StrategyVersion this project
// stamps on every emission: "<strategy-id>/<rules-version>+<build>" (ADR
// 0016).
//
// strategyID and rulesVersion must carry neither delimiter or the result
// cannot be decomposed back; only build may. ConfigurationPayload.Validate
// enforces that for StrategyID.
func ComposeStrategyVersion(strategyID, rulesVersion, build string) string {
	return strategyID + "/" + rulesVersion + "+" + build
}

// DecomposeStrategyVersion is ComposeStrategyVersion's inverse: the FIRST "/"
// separates the strategy id from the rest, and the FIRST "+" in what remains
// separates the rules version from the build. A string this project never
// composed is refused rather than partially decomposed.
//
// It does NOT establish that a decomposition is unique — the "/" check below
// catches only part of that class, and cannot catch a strategy id like
// "desk/turtle+eu". Uniqueness comes from ConfigurationPayload.Validate
// refusing a delimiter-bearing StrategyID; do not drop that check on the
// strength of this one (TestAnAmbiguousStrategyVersionIsUnreachableRatherThanDetectable).
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
