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
	for _, version := range []uint32{0, 1, 2, 3, 4, 5} {
		for _, cash := range []float64{-1, 0, 1, math.NaN(), math.Inf(-1)} {
			t.Run(fmt.Sprintf("schema-%d/cash-%v", version, cash), func(t *testing.T) {
				p := validProposalDeclinedInsufficientCash()
				p.RequiredCash = 2
				p.AvailableCash = cash
				err := p.ValidateSchema(version)
				wantValid := version >= 2 && version <= 4 && !math.IsNaN(cash) && !math.IsInf(cash, 0) && (cash >= 0 || version == 4)
				if (err == nil) != wantValid {
					t.Fatalf("ValidateSchema(%d), cash %v = %v; want valid %v", version, cash, err, wantValid)
				}
				if version >= 2 && version <= 3 && cash == -1 && !strings.Contains(err.Error(), "available cash must not be negative") {
					t.Fatalf("wrong historical validation error: %v", err)
				}
			})
		}
	}
	for _, version := range []uint32{2, 3, event.ProposalDeclinedSchemaVersion} {
		p := validProposalDeclinedInsufficientCash()
		p.Detail = ""
		if err := p.ValidateSchema(version); err == nil || !strings.Contains(err.Error(), "detail is required") {
			t.Fatalf("schema %d bypassed payload validation: %v", version, err)
		}
	}
}
