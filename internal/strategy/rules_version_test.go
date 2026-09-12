package strategy_test

import (
	"regexp"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

var semanticVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// TestRulesVersionIsADeclaredSemanticVersion is #50/ADR 0016's requirement
// that the rules version is "a hand-bumped semantic version declared in
// code" — not empty, and not derived from anything else (buildinfo.Version,
// a git hash, a timestamp).
func TestRulesVersionIsADeclaredSemanticVersion(t *testing.T) {
	t.Parallel()

	if !semanticVersionPattern.MatchString(strategy.RulesVersion) {
		t.Fatalf("strategy.RulesVersion = %q, want a semantic version matching %s", strategy.RulesVersion, semanticVersionPattern)
	}
}

// TestComposedStrategyVersionUsesTheDeclaredRulesVersion is the seam test:
// event.ComposeStrategyVersion, fed strategy.RulesVersion, produces exactly
// the StrategyVersion ADR 0016 specifies.
func TestComposedStrategyVersionUsesTheDeclaredRulesVersion(t *testing.T) {
	t.Parallel()

	got := event.ComposeStrategyVersion("turtle-baseline", strategy.RulesVersion, "test-build")
	want := "turtle-baseline/" + strategy.RulesVersion + "+test-build"
	if got != want {
		t.Fatalf("ComposeStrategyVersion(..., strategy.RulesVersion, ...) = %q, want %q", got, want)
	}
}
