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
