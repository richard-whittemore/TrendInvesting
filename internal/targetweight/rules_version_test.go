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

// TestRulesVersionDeclaresTheGlidePath pins ADR 0016's same-rules-version
// replay guarantee for the glide path: configuration schema version 3 and
// targets.decided version 3 change every momentum decision, so a journal
// written under rules 1.1.0 cannot replay under this build and the build
// must not claim 1.1.0 or earlier.
func TestRulesVersionDeclaresTheGlidePath(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.1.0 once momentum decisions carry the glide path", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheVersion2BarBreak pins the same guarantee for the
// bar input: the handler reads market.bar.completed version 2 and its split-
// and dividend-adjusted close, refusing version 1, so a journal written under
// rules 1.2.0 (the glide path on version-1 bars) cannot replay under this
// build and the build must not claim 1.2.0 or earlier.
func TestRulesVersionDeclaresTheVersion2BarBreak(t *testing.T) {
	t.Parallel()

	for _, old := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		if targetweight.RulesVersion == old {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.2.0 once the handler reads bar version 2 and the adjusted close", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresMinimumLot pins ADR 0016's same-rules-version
// replay guarantee for D7-L: configuration schema version 4 and
// targets.decided version 4 change which names a momentum decision holds, so
// a journal written under rules 1.3.0 (bar version 2, before the minimum
// lot) cannot replay under this build and the build must not claim 1.3.0 or
// earlier.
func TestRulesVersionDeclaresMinimumLot(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.3.0 once momentum decisions apply D7-L", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheVersion5ConfigurationBreak: the configuration
// schema moved to version 5 (the margin-cushion thresholds and
// MaxSnapshotAge) and the gate gained the kill switch, the broker-sized sells
// and the sale back to 98%, so a journal written under rules 1.4.0 cannot
// replay under this build.
func TestRulesVersionDeclaresTheVersion5ConfigurationBreak(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.4.0 once the configuration schema moved to version 5", targetweight.RulesVersion)
		}
	}
}

func TestRulesVersionDeclaresTheOwnersClearance(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.5.0 once a momentum run reads the owner's clearance of a rebalance in progress", targetweight.RulesVersion)
		}
	}
}
