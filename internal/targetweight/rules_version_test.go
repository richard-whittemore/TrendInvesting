package targetweight_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// TestRulesVersionDeclaresTheVersion2ConfigurationBreak pins ADR 0016's
// same-rules-version replay guarantee: the handler refuses a version-1
// configuration envelope (ADR 0015), so a journal written under rules 1.0.0
// cannot replay under this build and the build must not claim 1.0.0.
func TestRulesVersionDeclaresTheVersion2ConfigurationBreak(t *testing.T) {
	t.Parallel()

	if targetweight.RulesVersion == "1.0.0" {
		t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.0.0 once the configuration schema moved to version 2", targetweight.RulesVersion)
	}
}

// TestRulesVersionDeclaresTheVersion2BarBreak pins the same guarantee for the
// bar input: the handler reads market.bar.completed version 2 and its split-
// and dividend-adjusted close, refusing version 1, so a journal written under
// rules 1.1.0 cannot replay under this build and the build must not claim it.
func TestRulesVersionDeclaresTheVersion2BarBreak(t *testing.T) {
	t.Parallel()

	for _, old := range []string{"1.0.0", "1.1.0"} {
		if targetweight.RulesVersion == old {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.1.0 once the handler reads bar version 2 and the adjusted close", targetweight.RulesVersion)
		}
	}
}
