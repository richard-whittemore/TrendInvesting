package strategy

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// holdIDs lists the proposal ids of r's standing holds, in placement order.
func holdIDs(r *Reducer) []string {
	ids := make([]string, 0, len(r.holds))
	for _, h := range r.holds {
		ids = append(ids, h.proposalID)
	}
	return ids
}

// TestEveryWayAProposalEndsReleasesItsHold drives each way an entry or Add
// proposal can stop being outstanding through the reducer's own handlers,
// and requires its hold released (ADR 0020, as amended 2026-09-24: "What
// releases a hold"): its fill, its expiry at the next bar or the end of the
// stream, and its cancellation by a partial stop-out or a delisting. A fill
// is also debited exactly once, at its actual cost.
func TestEveryWayAProposalEndsReleasesItsHold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture  string
		released string
		debited  bool
		prepare  func(*Reducer)
	}{
		{fixture: "entry", released: "entry", debited: true},
		{fixture: "add", released: "add", debited: true},
		{fixture: "partial-stop", released: "add"},
		{fixture: "delisting", released: "add"},
		{fixture: "end", released: "earlier"},
		{fixture: "bar", released: "expiring", prepare: func(r *Reducer) {
			s := r.instruments["AAPL"]
			s.pendingAddProposal = &pendingAddProposalState{proposalID: "expiring", periodEnd: day(1), earliestFillAt: day(0), unitIndex: 2, quantity: 1, level: 100.5, previousUnitFill: 100}
			placeFixtureHolds(r)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+" releases "+tt.released, func(t *testing.T) {
			t.Parallel()
			r, input := transitionFixture(t, tt.fixture)
			if tt.prepare != nil {
				tt.prepare(r)
			}
			if !slices.Contains(holdIDs(r), tt.released) {
				t.Fatalf("fixture is wrong: no hold for %q stands before the input (holds %q)", tt.released, holdIDs(r))
			}
			debitsBefore := len(r.fillDebits)
			if _, err := r.transact(func(tx *transition) ([]event.Envelope, error) { return tx.apply(input) }); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if slices.Contains(holdIDs(r), tt.released) {
				t.Errorf("hold %q still stands after its proposal ended (holds %q)", tt.released, holdIDs(r))
			}
			if got := len(r.fillDebits) - debitsBefore; tt.debited && got != 1 {
				t.Errorf("the fill added %d debit(s), want exactly one: its hold is replaced by its actual cost once", got)
			}
		})
	}
}

// TestAHoldIsPlacedOnceAndReleasedOnlyIfItStands pins the ledger's two
// consistency checks: a proposal reserves its Unit once, and releasing a
// hold that does not stand means the ledger and the outstanding proposals
// disagree, which stops the run rather than trading on.
func TestAHoldIsPlacedOnceAndReleasedOnlyIfItStands(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	tx := r.begin()
	if err := tx.placeHold("p", "AAPL", unclassifiedClassification, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.placeHold("p", "AAPL", unclassifiedClassification, 1); err == nil || !strings.Contains(err.Error(), "already has a hold") {
		t.Fatalf("second hold for one proposal = %v, want refused", err)
	}
	if err := tx.releaseHold("p"); err != nil {
		t.Fatal(err)
	}
	if err := tx.releaseHold("p"); err == nil || !strings.Contains(err.Error(), "no hold standing") {
		t.Fatalf("release of a hold that does not stand = %v, want refused", err)
	}
}

// TestAPriceCapBeyondTheRepresentableRangeIsNotAHold: a cap that leaves the
// float64 range reports no hold, so its Unit is skipped as one no cash could
// fund rather than reserved at an infinite cost.
func TestAPriceCapBeyondTheRepresentableRangeIsNotAHold(t *testing.T) {
	t.Parallel()

	tx := newConfiguredReducerForInvariantTest(t).begin()
	tx.gapBufferN = 2
	if _, _, ok := tx.buyHold(1, math.MaxFloat64, math.MaxFloat64); ok {
		t.Fatal("buyHold reported an overflowing price cap as a hold")
	}
}

// TestAProposalWithoutItsHoldStopsTheRun: every outstanding entry and Add
// proposal has exactly one hold, so a proposal ending with none to release
// means the cash and cap reservations no longer match the proposals, and
// each way a proposal ends refuses to go on trading against that ledger
// (ADR 0020, as amended 2026-09-24).
func TestAProposalWithoutItsHoldStopsTheRun(t *testing.T) {
	t.Parallel()

	pendingEntry := func(r *Reducer) {
		s := r.instruments["AAPL"]
		s.campaign = nil
		s.pendingProposal = &pendingProposalState{proposalID: "expiring-entry", signalID: "signal", periodEnd: day(1), earliestFillAt: day(0), direction: event.DirectionLong, quantity: 1, n: 1, stopMultiple: 2, entryLevel: 100}
	}
	pendingAdd := func(r *Reducer) {
		r.instruments["AAPL"].pendingAddProposal = &pendingAddProposalState{proposalID: "expiring-add", periodEnd: day(1), earliestFillAt: day(0), unitIndex: 2, quantity: 1, level: 100.5, previousUnitFill: 100}
	}
	tests := []struct {
		name    string
		fixture string
		prepare func(*Reducer)
	}{
		{name: "an entry fill", fixture: "entry"},
		{name: "an Add fill", fixture: "add"},
		{name: "a partial stop cancelling an Add", fixture: "partial-stop"},
		{name: "a delisting cancelling an Add", fixture: "delisting"},
		{name: "the end of the stream expiring an entry", fixture: "end"},
		{name: "the end of the stream expiring an Add", fixture: "end", prepare: func(r *Reducer) {
			delete(r.instruments, "AAA")
			pendingAdd(r)
		}},
		{name: "a bar expiring an entry", fixture: "bar", prepare: pendingEntry},
		{name: "a bar expiring an Add", fixture: "bar", prepare: pendingAdd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, input := transitionFixture(t, tt.fixture)
			if tt.prepare != nil {
				tt.prepare(r)
			}
			r.holds = nil
			_, err := r.transact(func(tx *transition) ([]event.Envelope, error) { return tx.apply(input) })
			if err == nil || !strings.Contains(err.Error(), "no hold standing") {
				t.Fatalf("apply = %v, want the missing hold refused", err)
			}
		})
	}
}

// TestAProposalThatWouldReserveTwiceStopsTheRun: a hold already standing for
// the proposal an input would make means its Unit is reserved already, and
// reserving it again would count it twice.
func TestAProposalThatWouldReserveTwiceStopsTheRun(t *testing.T) {
	t.Parallel()

	t.Run("a chained Add", func(t *testing.T) {
		t.Parallel()
		r, input := transitionFixture(t, "add")
		r.holds = append(r.holds, hold{proposalID: decisionID("add-proposal-unit-3", "AAPL", day(2)), instrumentID: "AAPL", classification: unclassifiedClassification, cost: 1})
		_, err := r.transact(func(tx *transition) ([]event.Envelope, error) { return tx.apply(input) })
		if err == nil || !strings.Contains(err.Error(), "already has a hold") {
			t.Fatalf("apply = %v, want the second hold refused", err)
		}
	})
	t.Run("an entry", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		r.hasAvailableCash, r.availableCash, r.availableCashAsOf = true, 1_000_000, day(0)
		r.configuredSizingMode, r.sizingMode = event.SizingModeVolatilityNormalised, sizing.ModeVolatilityNormalised
		r.unitVolatilityFrac, r.stopMultiple = 0.005, 2
		tx := r.begin()
		if err := tx.placeHold(decisionID("proposal", "AAPL", day(2)), "AAPL", unclassifiedClassification, 1); err != nil {
			t.Fatal(err)
		}
		_, err := tx.sizeUnit("AAPL", day(2), event.Envelope{}, "signal", 100, 2, true, day(1))
		if err == nil || !strings.Contains(err.Error(), "already has a hold") {
			t.Fatalf("sizeUnit = %v, want the second hold refused", err)
		}
	})
}
