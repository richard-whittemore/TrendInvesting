package event_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// endOfStreamExpiry is the expiry of a proposal that was still outstanding
// when the input stream ended: no later bar ever arrived to supersede it, so
// it ended with the run itself.
func endOfStreamExpiry(kind string) event.ProposalExpiredPayload {
	payload := validProposalExpired()
	payload.Kind = kind
	payload.Reason = event.ExpiryReasonInputStreamEnded
	// The run ended at the proposal's own bar: there is no later bar, which
	// is exactly why this reason exists.
	payload.ExpiredAt = payload.PeriodEnd
	switch kind {
	case event.ProposalKindExit:
		payload.SignalID = ""
		payload.Rule = event.RuleExitProposalExpiresWithItsBar
	case event.ProposalKindAdd:
		payload.SignalID = ""
		payload.Rule = event.RuleAddProposalExpiresWithItsBar
	}
	return payload
}

// TestEndOfStreamExpiryIsValidForEveryProposalKind: entry, Add and exit
// proposals can all be outstanding when the stream ends, so the reason is
// valid for all three — unlike ExpiryReasonSupersededByStop, which only an
// Add proposal can carry.
func TestEndOfStreamExpiryIsValidForEveryProposalKind(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{event.ProposalKindEntry, event.ProposalKindAdd, event.ProposalKindExit} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			if err := endOfStreamExpiry(kind).Validate(); err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

// TestEndOfStreamExpiryNeedNotFollowALaterBar: the next-bar rule
// (ExpiredAt strictly after PeriodEnd) is specific to
// ExpiryReasonSupersededByNextBar. A stream that ends leaves the proposal
// expiring AT its own bar, since no later bar exists.
func TestEndOfStreamExpiryNeedNotFollowALaterBar(t *testing.T) {
	t.Parallel()

	payload := endOfStreamExpiry(event.ProposalKindEntry)
	if !payload.ExpiredAt.Equal(payload.PeriodEnd) {
		t.Fatalf("fixture no longer expires at its own period end: %s vs %s", payload.ExpiredAt, payload.PeriodEnd)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

// TestEndOfStreamExpiryStillCannotPredateTheEarliestPossibleFill: the
// EarliestFillAt rule holds for every reason — nothing can end before the
// earliest instant an order for it could have existed.
func TestEndOfStreamExpiryStillCannotPredateTheEarliestPossibleFill(t *testing.T) {
	t.Parallel()

	payload := endOfStreamExpiry(event.ProposalKindEntry)
	payload.EarliestFillAt = payload.ExpiredAt

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want one naming the earliest possible execution")
	}
	if !strings.Contains(err.Error(), "earliest instant") {
		t.Fatalf("Validate() error = %v, want one naming the earliest possible execution", err)
	}
}
