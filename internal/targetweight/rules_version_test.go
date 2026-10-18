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

// TestRulesVersionDeclaresTheDryRunSyntheticReports pins ADR 0016's
// same-rules-version replay guarantee for dry-run: a dry-run momentum run
// records a synthetic terminal report (execution.order.lifecycle version 3)
// for each order it releases and plans its buys at the same close, where an
// earlier build waited and withheld later months as rebalance-in-progress, so a
// dry-run journal written under rules 1.10.0 or earlier cannot replay under
// this build and the build must not claim 1.10.0 or earlier (ADR 0026, point
// 3, "Mode", amended 2026-10-17).
func TestRulesVersionDeclaresTheDryRunSyntheticReports(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.10.0 once a dry-run records synthetic terminal reports", targetweight.RulesVersion)
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

// TestRulesVersionDeclaresTheHalfCentLimitMatch pins ADR 0016's
// same-rules-version replay guarantee for the gate's lifecycle match: a
// report on a released order whose limit is within half a cent of the order's
// (event.SameLimitPrice) is read where rules 1.6.0 refused it unless the two
// were bit-equal, so a journal holding such a report decides differently and
// the build must not claim 1.6.0 or earlier.
func TestRulesVersionDeclaresTheHalfCentLimitMatch(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.6.0 once the gate matches a lifecycle report's limit within half a cent", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresReconciliation: the handler reads
// account.reconciliation, market.corporate-action and
// operator.alert.acknowledged, and a reconciliation degrades or halts the
// account, so a journal written under this build cannot replay under an
// earlier rules version, which refuses those inputs. 1.8.0 is the lifecycle
// match's, so this build must be above 1.8.0; 1.9.0 is this change's own
// version (ADR 0016: each rules change takes its own version).
func TestRulesVersionDeclaresReconciliation(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want 1.9.0 or later once a reconciliation moves the engine state", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheOwnersClearance pins ADR 0016's
// same-rules-version replay guarantee for the owner's clearance and the
// kill switch's time order: a momentum run reads operator.rebalance.cleared
// and ends a rebalance in progress (ADR 0026, point 5), and the gate refuses
// a kill switch stating a time earlier than the last one it applied (ADR
// 0019, amendment of 2026-10-17), so the build must not claim a version a
// journal written without those rules carries. 1.10.0 was reserved for
// these rules but 1.11.0 (dry-run synthetic reports) landed first, so this
// one is 1.12.0.
func TestRulesVersionDeclaresTheOwnersClearance(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.11.0 once a momentum run reads the owner's clearance of a rebalance in progress and the gate orders kill switches by their stated time", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheUnlabelledNamePicked pins ADR 0016's
// same-rules-version replay guarantee for names with no sector label: a
// momentum decision ranks and picks such a name uncapped, as the backtest
// did, where rules 1.12.0 and earlier left it out as no-sector (ADR 0026
// point 8, the owner's decision of 2026-10-18), so a journal holding an
// unlabelled name in the book's reach decides differently and the build
// must not claim 1.12.0 or earlier.
func TestRulesVersionDeclaresTheUnlabelledNamePicked(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.12.0 once a momentum decision picks a name with no sector label", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheReplanAfterARefusal pins ADR 0016's
// same-rules-version replay guarantee for Boost's re-plan: after a refusal
// that leaves the engine Normal, the next decision plans the difference to
// the target in force though the target is unchanged (ADR 0026, point 2,
// "Boost", amended 2026-10-18), so a journal holding such a refusal decides
// differently under an earlier rules version. 1.13.0 is reserved for a rules
// change landing in parallel, so this one is 1.14.0.
func TestRulesVersionDeclaresTheReplanAfterARefusal(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.13.0 once Boost re-plans a refused order at the next decision", targetweight.RulesVersion)
		}
	}
}
