package targetweight_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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

// TestRulesVersionDeclaresTheDeclaredUniverseInput pins ADR 0016's
// same-rules-version replay guarantee for the declared universe input:
// configuration schema version 6 is required and version 5 refused (ADR
// 0015), and a decision session whose ranked members sent neither a bar nor
// a history is withheld where it was decided, so a journal written under
// rules 1.5.0 cannot replay under this build and the build must not claim
// 1.5.0 or earlier.
func TestRulesVersionDeclaresTheDeclaredUniverseInput(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.5.0 once the configuration declares the universe input", targetweight.RulesVersion)
		}
	}
}

// TestHandlerRefusesAVersion5Configuration: version 5 has no universe input,
// so a momentum run cannot know which input shape it reads from a version-5
// configuration; the handler refuses it rather than inferring the shape (ADR
// 0015).
func TestHandlerRefusesAVersion5Configuration(t *testing.T) {
	t.Parallel()

	cfg := momentumConfiguration()
	h, err := targetweight.New(strategyVersion, cfg, targetweight.Options{DecideFrom: closeOf("2025-01-02")})
	if err != nil {
		t.Fatal(err)
	}
	s := &stream{hash: h.ConfigurationHash()}
	mustRefuse(t, h, s.envelope(t, event.TargetWeightConfigurationEventType, 5, "configuration", closeOf("2025-01-02"), cfg), "schema version 5")
}

// TestRulesVersionDeclaresReconciliation: the handler reads
// account.reconciliation, market.corporate-action and
// operator.alert.acknowledged, and a reconciliation degrades or halts the
// account, so a journal written under this build cannot replay under rules
// 1.6.0, which refuses those inputs.
func TestRulesVersionDeclaresReconciliation(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.6.0 once a reconciliation moves the engine state", targetweight.RulesVersion)
		}
	}
}
