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

// TestRulesVersionDeclaresTheHeldNameScoredAcrossItsGap pins ADR 0016's
// same-rules-version replay guarantee for a held name whose window has a
// gap: a momentum decision scores it across the gap as the backtest did,
// keeps BacktestHistorySessions sessions of closes, and records each gap's
// sessions (ADR 0026 point 5, amended 2026-10-18, the owner's decision),
// where rules 1.13.0 and earlier left a held name out as short-history and
// sold it, so the build must not claim 1.13.0 or earlier.
func TestRulesVersionDeclaresTheHeldNameScoredAcrossItsGap(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.14.0 once a momentum decision scores a held name across its gap", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheReplanAfterARefusal pins ADR 0016's
// same-rules-version replay guarantee for Boost's re-plan: after a refusal
// that leaves the engine Normal, the next decision plans the difference to
// the target in force though the target is unchanged (ADR 0026, point 2,
// "Boost", amended 2026-10-18), so a journal holding such a refusal decides
// differently under an earlier rules version. 1.15.0 is the held-name gap
// rule, so this one is 1.16.0.
func TestRulesVersionDeclaresTheReplanAfterARefusal(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.15.0 once Boost re-plans a refused order at the next decision", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheHeldNameKeptOneLotShort pins ADR 0016's
// same-rules-version replay guarantee for D7-L's held-name exception: a
// held pick whose one share exceeds its target keeps its holding, place and
// weight, with no order, where earlier versions skipped it and sold it in
// full (ADR 0026, point 2, "D7-L", amended 2026-10-18), so a journal holding
// such a name decides differently and the build must not claim 1.16.0 or
// earlier.
func TestRulesVersionDeclaresTheHeldNameKeptOneLotShort(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.16.0 once D7-L keeps a held name one lot short", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheReplanAfterAPartFill pins ADR 0016's
// same-rules-version replay guarantee for a Boost order that ends with shares
// unfilled, and for a session with no SPY bar: the next decided session plans
// the remaining difference to the target in force, as after a refusal, and a
// gap is skipped rather than withholding every later session (ADR 0026,
// points 2 "Boost" and 5, amended 2026-10-18), so a journal holding either
// decides differently and the build must not claim 1.17.0 or earlier.
func TestRulesVersionDeclaresTheReplanAfterAPartFill(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.17.0 once Boost re-plans an unfilled remainder and skips a missing bar", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresThePostCloseReadingInputs pins ADR 0016's
// same-rules-version replay guarantee for the inputs the IBKR adapter writes
// under the owner's decision of 2026-10-18 (ADR 0019, ADR 0026 point 1,
// amended): account.broker.snapshot version 2 and market.mark.recorded
// version 2, which a 1.18.0 build refuses, so a journal holding either does
// not replay under 1.18.0 and the build must not claim it or earlier.
func TestRulesVersionDeclaresThePostCloseReadingInputs(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0", "1.18.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.18.0 once the handler reads broker snapshot and mark version 2", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheCloseSnapshotCheck pins ADR 0016's
// same-rules-version replay guarantee for the close-snapshot check (ADR
// 0019, amended 2026-10-21): an account snapshot whose holdings the book and
// the fills journalled since it do not explain degrades the account, where
// 1.19.0 took it as the book, so a journal holding one decides differently
// and the build must not claim 1.19.0 or earlier.
func TestRulesVersionDeclaresTheCloseSnapshotCheck(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0", "1.18.0", "1.19.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.19.0 once a close snapshot no fill explains degrades the account", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheDryRunBoostSyntheticReports pins ADR 0016's
// same-rules-version replay guarantee for a dry-run Boost run's synthetic
// terminal reports (ADR 0026, point 3, "Mode", amended 2026-10-22): each
// order it releases is ended at its release, so a later exit is sized with
// no sell of its own working, where 1.21.0 counted every unsent sell as
// still being sold. A dry-run Boost journal decides differently, so the
// build must not claim 1.21.0 or earlier.
func TestRulesVersionDeclaresTheDryRunBoostSyntheticReports(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0", "1.18.0", "1.19.0", "1.20.0", "1.21.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.21.0 once a dry-run Boost run ends each unsent release with a synthetic report", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheRetiredOrder pins ADR 0016's
// same-rules-version replay guarantee for a retired order (the owner's
// decision of 2026-10-22; ADR 0026, point 2, "Boost", amended 2026-10-22):
// an execution.order.retired ends a released order no venue received with
// nothing filled, so Boost re-plans it and a momentum rebalance completes,
// where 1.22.0 refused the input and kept the order working. A journal
// holding one decides differently, so the build must not claim 1.22.0 or
// earlier.
func TestRulesVersionDeclaresTheRetiredOrder(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0", "1.18.0", "1.19.0", "1.20.0", "1.21.0", "1.22.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.22.0 once a retired order is read", targetweight.RulesVersion)
		}
	}
}

// TestRulesVersionDeclaresTheNamedLiveSale pins ADR 0016's
// same-rules-version replay guarantee for the run's own live sale back to
// 98% (ADR 0026, point 2, "Boost", and point 5, amended 2026-10-22): each
// Boost decision records it, at decision schema version 8, whether or not a
// venue has named it and whether or not part of it has filled, and a run
// restarted from the decision reads the venue's later reports on it, where
// 1.23.0 recorded no such sale and refused those reports. A journal holding
// one decides differently, so the build must not claim 1.23.0 or earlier.
func TestRulesVersionDeclaresTheNamedLiveSale(t *testing.T) {
	t.Parallel()

	for _, earlier := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "1.8.0", "1.9.0", "1.10.0", "1.11.0", "1.12.0", "1.13.0", "1.14.0", "1.15.0", "1.16.0", "1.17.0", "1.18.0", "1.19.0", "1.20.0", "1.21.0", "1.22.0", "1.23.0"} {
		if targetweight.RulesVersion == earlier {
			t.Fatalf("targetweight.RulesVersion = %q, want a version above 1.23.0 once a decision records a live sale a venue named", targetweight.RulesVersion)
		}
	}
}
