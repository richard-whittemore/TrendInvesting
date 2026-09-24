package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// These are transaction fixtures, not new trading-rule goldens. The existing
// package-private invariant seam supplies state; the rule corpus remains fixed
// under ADR 0016. Every fixture is constructed twice so rollback comparisons do
// not depend on the copy implementation being tested.
func transitionFixture(t *testing.T, kind string) (*Reducer, event.Envelope) {
	t.Helper()
	r := newConfiguredReducerForInvariantTest(t)
	buildCorruptedCampaignState(t, r, 98)
	r.maxUnits = 4
	r.hasAvailableCash = true
	r.availableCash = 1_000_000
	r.availableCashAsOf = day(1)
	s := r.instruments["AAPL"]
	s.previousClose, s.hasPreviousClose = 100, true
	s.lastBarHigh, s.lastBarPeriodEnd, s.lastBarEarliestFillAt = 110, day(2), day(1)
	c := s.campaign
	c.maxUnits = 4
	c.unitsOpened = 1
	c.units = append(make([]unitState, 0, 4), c.units...)
	for range 20 {
		s.exitChannel.Add(99)
	}
	s.entryChannel.Add(101)
	s.n.Add(2)
	r.acceptedFills["old-stop"] = acceptedFillState{unitIDs: []string{"old-unit"}}
	r.delisted["OLD"] = day(1)
	s.pendingAddProposal = &pendingAddProposalState{proposalID: "add", periodEnd: day(2), earliestFillAt: day(1), unitIndex: 2, quantity: 1, level: 100.5, previousUnitFill: 100}
	fill := event.FillPayload{InstrumentID: "AAPL", Kind: event.FillKindAdd, CampaignID: c.campaignID, ProposalID: "add", FillID: "new-fill", Direction: event.DirectionLong, Quantity: 1, Price: 100.5, Level: 100, FilledAt: day(2)}
	var payload any = fill
	typ, schema := event.FillEventType, event.FillSchemaVersion
	switch kind {
	case "entry":
		s.campaign, s.pendingAddProposal = nil, nil
		s.pendingProposal = &pendingProposalState{proposalID: "entry", signalID: "signal", periodEnd: day(2), earliestFillAt: day(1), direction: event.DirectionLong, quantity: 1, n: 1, stopMultiple: 2, entryLevel: 100}
		fill.Kind, fill.CampaignID, fill.ProposalID, fill.Price = event.FillKindEntry, "", "entry", 100
		payload = fill
	case "add":
	case "partial-stop", "full-stop":
		fill.Kind, fill.ProposalID, fill.Price = event.FillKindStop, "", 98
		fill.UnitIDs = []string{c.units[0].openingFillID}
		if kind == "partial-stop" {
			u := c.units[0]
			u.index, u.openingFillID = 2, "second-unit"
			c.units = append(c.units, u)
			c.unitsOpened = 2
			s.pendingAddProposal.unitIndex = 3
		}
		payload = fill
	case "exit":
		s.pendingAddProposal = nil
		s.pendingExitProposal = &pendingExitProposalState{proposalID: "exit", campaignID: c.campaignID, periodEnd: day(2), earliestFillAt: day(1), quantity: 1, level: 99}
		fill.Kind, fill.ProposalID, fill.Price = event.FillKindExit, "exit", 99
		payload = fill
	case "bar":
		s.pendingAddProposal = nil
		e := invariantTestBarEnvelope(t, 1, "AAPL", day(2))
		var bar event.CompletedBarPayload
		if err := json.Unmarshal(e.Payload, &bar); err != nil {
			t.Fatal(err)
		}
		bar.Raw.Low, bar.SplitAdjusted.Low = 98, 98
		payload, typ, schema = bar, event.CompletedBarEventType, event.CompletedBarSchemaVersion
	case "snapshot":
		payload = event.AccountSnapshotPayload{AsOf: day(2), Equity: 800_000, AvailableCash: 700_000, Currency: "USD"}
		typ, schema = event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion
	case "cash":
		payload = event.CashMovementPayload{AsOf: day(2), EquityBefore: 1_000_000, Amount: -100_000, Currency: "USD"}
		typ, schema = event.CashMovementEventType, event.CashMovementSchemaVersion
	case "delisting":
		payload = event.CorporateActionPayload{InstrumentID: "AAPL", Kind: event.CorporateActionKindDelisting, EffectiveAt: day(2)}
		typ, schema = event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion
	case "end":
		// An earlier instrument must also be rolled back when the final one fails.
		s.pendingAddProposal = nil
		s.pendingExitProposal = &pendingExitProposalState{proposalID: "exit", campaignID: c.campaignID, periodEnd: day(2), earliestFillAt: day(1), quantity: 1, level: 99}
		c.units[0].exitOrderLevel, c.units[0].exitOrderSource = 99, event.ExitOrderSourceExitChannel
		r.instruments["AAA"] = &instrumentState{pendingProposal: &pendingProposalState{proposalID: "earlier", signalID: "signal", periodEnd: day(1), quantity: 1, entryLevel: 100}}
		payload, typ, schema = event.RunCompletedPayload{}, event.RunCompletedEventType, event.RunCompletedSchemaVersion
	default:
		t.Fatalf("unknown fixture %s", kind)
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return r, event.Envelope{ID: "transition-input", Type: typ, SchemaVersion: schema, EventTime: day(3), RecordedAt: day(3), Payload: bytes}
}

// rejectFinalPayload uses the private transaction builder as a test-only fault
// seam. It changes the final built payload's required Rule and calls that exact
// payload type's validator. This reaches otherwise construction-unreachable
// validation failures without production hooks, globals, or altered validators.
func rejectFinalPayload(t *testing.T, out []event.Envelope, want string) error {
	t.Helper()
	if len(out) == 0 || out[len(out)-1].Type != want {
		t.Fatalf("final output = %v, want %s", out, want)
	}
	var p interface{ Validate() error }
	switch want {
	case event.AddProposalEventType:
		p = &event.AddProposalPayload{}
	case event.ProposalExpiredEventType:
		p = &event.ProposalExpiredPayload{}
	case event.CampaignExitedEventType:
		p = &event.CampaignExitedPayload{}
	case event.ExitOrderSetEventType:
		p = &event.ExitOrderSetPayload{}
	case event.DrawdownStepAppliedEventType:
		p = &event.DrawdownStepAppliedPayload{}
	case event.NotionalAccountCashAdjustedEventType:
		p = &event.NotionalAccountCashAdjustedPayload{}
	default:
		t.Fatalf("no validator for %s", want)
	}
	if err := json.Unmarshal(out[len(out)-1].Payload, p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("unmodified final payload invalid: %v", err)
	}
	reflect.ValueOf(p).Elem().FieldByName("Rule").SetString("")
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "rule is required") {
		t.Fatalf("invalid final payload: %v", err)
	}
	return err
}

func assertRejectedTransition(t *testing.T, kind, finalType string) {
	t.Helper()
	r, input := transitionFixture(t, kind)
	before, _ := transitionFixture(t, kind)
	// A committed prefix makes an accidental append observable as well as a
	// nonempty return. No failed decision may enter the emission stream.
	stream := []event.Envelope{{ID: "committed-prefix", Payload: json.RawMessage(`{}`)}}
	wantStream := append([]event.Envelope(nil), stream...)
	var first string
	for attempt := 0; attempt < 2; attempt++ {
		out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
			emissions, err := tx.apply(input)
			if err != nil {
				t.Fatalf("handler rejected before the final payload: %v", err)
			}
			return emissions, rejectFinalPayload(t, emissions, finalType)
		})
		stream = append(stream, out...)
		if err == nil {
			t.Fatalf("attempt %d accepted the rejected transition", attempt)
		}
		if attempt == 0 {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("retry error %q, want %q", err, first)
		}
		if !reflect.DeepEqual(r.acceptedFills, before.acceptedFills) {
			t.Errorf("attempt %d changed accepted fills", attempt)
		}
		if !reflect.DeepEqual(r, before) {
			t.Errorf("attempt %d changed reducer state", attempt)
		}
		if !reflect.DeepEqual(stream, wantStream) {
			t.Errorf("attempt %d leaked emissions", attempt)
		}
	}
	// Removing the injected fault accepts the identical event and commits it.
	out, err := r.Apply(context.Background(), input)
	if err != nil || len(out) == 0 || out[len(out)-1].Type != finalType {
		t.Fatalf("valid retry: %v, %v", out, err)
	}
	if reflect.DeepEqual(r, before) {
		t.Error("valid transition did not commit")
	}
}

func TestCashMovementRejectedLeavesAccountUnscaled(t *testing.T) {
	assertRejectedTransition(t, "cash", event.NotionalAccountCashAdjustedEventType)
}
func TestPartialStopRejectedExpiryLeavesStateAndAcceptedFillsUnchanged(t *testing.T) {
	assertRejectedTransition(t, "partial-stop", event.ProposalExpiredEventType)
}
func TestTransitionRejectsFinalPayload(t *testing.T) {
	for _, tt := range []struct{ kind, final string }{
		{"entry", event.AddProposalEventType},
		{"add", event.AddProposalEventType},
		{"full-stop", event.CampaignExitedEventType},
		{"exit", event.CampaignExitedEventType},
		{"bar", event.ExitOrderSetEventType},
		{"snapshot", event.DrawdownStepAppliedEventType},
		{"delisting", event.CampaignExitedEventType},
		{"end", event.ExitOrderSetEventType},
	} {
		t.Run(tt.kind, func(t *testing.T) { assertRejectedTransition(t, tt.kind, tt.final) })
	}
}

func TestTransitionDeepCopyHasNoAliases(t *testing.T) {
	r, _ := transitionFixture(t, "entry")
	before, _ := transitionFixture(t, "entry")
	campaign, _ := transitionFixture(t, "end")
	expectedCampaign, _ := transitionFixture(t, "end")
	r.instruments["OTHER"] = campaign.instruments["AAPL"]
	before.instruments["OTHER"] = expectedCampaign.instruments["AAPL"]
	add, _ := transitionFixture(t, "add")
	expectedAdd, _ := transitionFixture(t, "add")
	r.instruments["ADD"] = add.instruments["AAPL"]
	before.instruments["ADD"] = expectedAdd.instruments["AAPL"]
	_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		s := tx.instruments["AAPL"]
		s.pendingProposal.quantity++
		s.n.Add(77)
		s.entryChannel.Add(999)
		s.exitChannel.Add(1)
		// Advance the seed far enough to overwrite existing backing storage.
		for range 20 {
			s.n.Add(88)
		}
		tx.instruments["OTHER"].campaign.units[0].quantity++
		tx.instruments["OTHER"].pendingExitProposal.quantity++
		tx.instruments["ADD"].pendingAddProposal.quantity++
		tx.notionalAccount.current++
		tx.acceptedFills["old-stop"].unitIDs[0] = "changed"
		delete(tx.acceptedFills, "old-stop")
		delete(tx.delisted, "OLD")
		delete(tx.instruments, "OTHER")
		tx.availableCash++
		return nil, errors.New("discard modified copy")
	})
	if err == nil {
		t.Fatal("missing rejection")
	}
	if !reflect.DeepEqual(r, before) {
		t.Fatal("mutating the copy changed the original")
	}
}
