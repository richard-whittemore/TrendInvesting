package report

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func TestRebalancePageFallsBackToPeriodEndWithoutCloseSession(t *testing.T) {
	end := time.Date(2026, 1, 30, 21, 0, 0, 0, time.UTC)
	page := rebalanceWithheldAlert("trad-ira", event.Envelope{EventTime: end}, event.TargetsDecidedPayload{SessionPeriodEnd: end, WithheldReason: event.WithheldMissingBars})
	if !strings.Contains(page.Key, "2026-01-30") || !strings.Contains(page.Body, "2026-01-30") {
		t.Fatalf("fallback must name the decision session: %+v", page)
	}
}
