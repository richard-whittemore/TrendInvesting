package event_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// ADR 0015 preserves each schema's meaning; ADR 0020 first permits a
// negative available-cash remainder in decline schema 4.
func TestProposalDeclinedValidationBySchema(t *testing.T) {
	for _, version := range []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8} {
		for _, cash := range []float64{-1, 0, 1, math.NaN(), math.Inf(-1)} {
			t.Run(fmt.Sprintf("schema-%d/cash-%v", version, cash), func(t *testing.T) {
				p := validProposalDeclinedInsufficientCash()
				p.RequiredCash = 2
				p.AvailableCash = cash
				err := p.ValidateSchema(version)
				wantValid := version >= 2 && version <= event.ProposalDeclinedSchemaVersion && !math.IsNaN(cash) && !math.IsInf(cash, 0) && (cash >= 0 || version >= 4)
				if (err == nil) != wantValid {
					t.Fatalf("ValidateSchema(%d), cash %v = %v; want valid %v", version, cash, err, wantValid)
				}
				if version >= 2 && version <= 3 && cash == -1 && !strings.Contains(err.Error(), "available cash must not be negative") {
					t.Fatalf("wrong historical validation error: %v", err)
				}
			})
		}
	}
	for _, version := range []uint32{2, 3, 4, event.ProposalDeclinedSchemaVersion} {
		p := validProposalDeclinedInsufficientCash()
		p.Detail = ""
		if err := p.ValidateSchema(version); err == nil || !strings.Contains(err.Error(), "detail is required") {
			t.Fatalf("schema %d bypassed payload validation: %v", version, err)
		}
	}
}

// ADR 0015/ADR 0008: DeclineReasonUnitCapExceeded and its Cap/CapLimit/
// PostTradeExposure fields are recognised only from schema 5 onward. An
// older schema version cannot have asserted the reason at all — the
// identical discipline TestProposalDeclinedValidationBySchema exercises for
// AvailableCash's own meaning change.
func TestProposalDeclinedUnitCapExceededValidationBySchema(t *testing.T) {
	for _, version := range []uint32{2, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			p := validProposalDeclinedUnitCapExceeded()
			err := p.ValidateSchema(version)
			wantValid := version >= 5
			if (err == nil) != wantValid {
				t.Fatalf("ValidateSchema(%d) = %v; want valid %v", version, err, wantValid)
			}
			if version < 5 && (err == nil || !strings.Contains(err.Error(), "not recognised before proposal declined schema 5")) {
				t.Fatalf("schema %d: want the schema-gating error, got %v", version, err)
			}
		})
	}
}

// TestProposalDeclinedInsufficientHistoryValidationBySchema is
// TestProposalDeclinedUnitCapExceededValidationBySchema's counterpart for
// DeclineReasonInsufficientHistory (ADR 0010, as amended 2026-09-25):
// recognised only from schema 7 onward, the version that introduced it.
func TestProposalDeclinedInsufficientHistoryValidationBySchema(t *testing.T) {
	for _, version := range []uint32{2, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			p := validProposalDeclinedInsufficientHistory()
			err := p.ValidateSchema(version)
			wantValid := version >= 7
			if (err == nil) != wantValid {
				t.Fatalf("ValidateSchema(%d) = %v; want valid %v", version, err, wantValid)
			}
			if version < 7 && (err == nil || !strings.Contains(err.Error(), "not recognised before proposal declined schema 7")) {
				t.Fatalf("schema %d: want the schema-gating error, got %v", version, err)
			}
		})
	}
}

// TestProposalDeclinedIneligibleValidationBySchema is
// TestProposalDeclinedInsufficientHistoryValidationBySchema's counterpart for
// DeclineReasonIneligible (ADR 0009): recognised only from schema 8 onward,
// the version that introduced it.
func TestProposalDeclinedIneligibleValidationBySchema(t *testing.T) {
	for _, version := range []uint32{2, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("schema-%d", version), func(t *testing.T) {
			p := validProposalDeclinedIneligible()
			err := p.ValidateSchema(version)
			wantValid := version >= 8
			if (err == nil) != wantValid {
				t.Fatalf("ValidateSchema(%d) = %v; want valid %v", version, err, wantValid)
			}
			if version < 8 && (err == nil || !strings.Contains(err.Error(), "not recognised before proposal declined schema 8")) {
				t.Fatalf("schema %d: want the schema-gating error, got %v", version, err)
			}
		})
	}
}
