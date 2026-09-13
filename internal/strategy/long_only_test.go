package strategy_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// TestTheBaselineIsLongOnly is the tripwire under four branches that cannot
// be tested because they cannot be reached: the direction-mismatch checks in
// applyFill, applyStopFill, applyExitFill and applyAddFill, each listed in
// internal/coverageaudit/exclusions.json as unreachable while the Baseline is
// long-only.
//
// They are unreachable for one reason and one only: "long" is the single
// direction the wire contract accepts, so a fill and the Campaign or proposal
// it is reconciled against can never differ. This test asserts that reason
// rather than leaving it as a claim in a comment. The day a second direction
// is accepted anywhere, this fails — and those four branches become live
// code needing tests of their own, not entries on a list.
func TestTheBaselineIsLongOnly(t *testing.T) {
	t.Parallel()

	const short = "short"

	t.Run("a fill payload", func(t *testing.T) {
		t.Parallel()
		fill := withCostFields(openingFill("AAPL"))
		fill.Direction = short
		assertDirectionRefused(t, fill.Validate())
	})

	t.Run("a campaign opened payload", func(t *testing.T) {
		t.Parallel()
		opened := event.CampaignOpenedPayload{Direction: short}
		if err := opened.Validate(); err == nil || !strings.Contains(err.Error(), "direction") {
			t.Errorf("CampaignOpenedPayload.Validate() error = %v, want it to name the direction", err)
		}
	})

	t.Run("the sizing arithmetic", func(t *testing.T) {
		t.Parallel()
		if _, err := sizing.ProtectiveStopLevel(100, 2, 2, short); err == nil {
			t.Error("ProtectiveStopLevel() error = nil for a short campaign; its stop sits above entry, a different formula")
		}
		if _, err := sizing.NextAddLevel(100, 2, short); err == nil {
			t.Error("NextAddLevel() error = nil for a short campaign; its add ladder descends, a different formula")
		}
	})

	if event.DirectionLong != "long" {
		t.Errorf("event.DirectionLong = %q, want %q", event.DirectionLong, "long")
	}
}

func assertDirectionRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Validate() error = nil, want a direction other than long to be refused")
	}
	if !strings.Contains(err.Error(), "direction") {
		t.Errorf("Validate() error = %v, want it to name the direction", err)
	}
}
