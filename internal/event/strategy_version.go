package event

import (
	"fmt"
	"regexp"
	"strings"
)

// StrategyVersionDelimiters are the two characters ADR 0016 makes structural
// in a composed StrategyVersion.
const StrategyVersionDelimiters = "/+"

// validIdentifierPattern is the permitted character set and length for both
// a StrategyID and a RulesVersion.
var validIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// isPathAlias reports whether s is one of the two special directory names
// that do not name a distinct location. They fail differently and both are
// wrong here: filepath.Join(root, "..") resolves OUTSIDE root, while
// filepath.Join(root, ".") aliases root ITSELF rather than a child of it.
// Both match validIdentifierPattern, so this check must be stated separately
// rather than encoded in the regex: excluding exactly two literals from every
// other dot-containing string is clearer as an equality test than as a
// negative lookahead.
func isPathAlias(s string) bool {
	return s == "." || s == ".."
}

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
	if !validIdentifierPattern.MatchString(id) {
		return "", "", "", fmt.Errorf("event: strategy version %q decomposes to a strategy id %q that is not a valid identifier (must match %s)", strategyVersion, id, validIdentifierPattern)
	}
	if isPathAlias(id) {
		return "", "", "", fmt.Errorf("event: strategy version %q decomposes to a strategy id %q that is a path-traversal alias and cannot be used safely in a directory path", strategyVersion, id)
	}
	if !validIdentifierPattern.MatchString(rules) {
		return "", "", "", fmt.Errorf("event: strategy version %q decomposes to a rules version %q that is not a valid identifier (must match %s)", strategyVersion, rules, validIdentifierPattern)
	}
	if isPathAlias(rules) {
		return "", "", "", fmt.Errorf("event: strategy version %q decomposes to a rules version %q that is a path-traversal alias and cannot be used safely in a directory path", strategyVersion, rules)
	}
	return id, rules, buildPart, nil
}
