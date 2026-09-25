package strategy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// This file holds the tests for a symbol change (ADR 0024): the
// symbol-change kind of market.corporate-action, and the
// strategy.instrument.symbol-changed decision the reducer records for it.
//
// The headline concern is instrument identity: this reducer's instrument id
// is already opaque (docs/adr/0024, symbol_change.go's own doc comment), so
// a symbol change must carry a Campaign's whole state — including its
// denormalised campaignState.instrumentID, which every later Add proposal
// and Exit Order reads directly — to the new id, rather than looking like a
// delisting followed by a fresh instrument.

func symbolChangeAction(oldID, newID string, when time.Time) event.CorporateActionPayload {
	return event.CorporateActionPayload{
		InstrumentID:    oldID,
		Kind:            event.CorporateActionKindSymbolChange,
		EffectiveAt:     when,
		NewInstrumentID: newID,
	}
}

func decodeInstrumentSymbolChanged(t *testing.T, envelope event.Envelope) event.InstrumentSymbolChangedPayload {
	t.Helper()
	if envelope.Type != event.InstrumentSymbolChangedEventType || envelope.SchemaVersion != event.InstrumentSymbolChangedSchemaVersion {
		t.Fatalf("envelope = %s/%d, want %s/%d", envelope.Type, envelope.SchemaVersion, event.InstrumentSymbolChangedEventType, event.InstrumentSymbolChangedSchemaVersion)
	}
	var payload event.InstrumentSymbolChangedPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(payload) error = %v", err)
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("emitted instrument symbol changed payload fails its own Validate(): %v", err)
	}
	return payload
}

// symbolChangeAt is the moment between day(56)'s Session and day(57)'s at
// which the fixtures' symbol changes take effect — mirroring splitAt's own
// placement immediately after the fixture's opening fill.
var symbolChangeAt = day(56).Add(12 * time.Hour)

// TestASymbolChangeCarriesTheCampaignAcrossMidCampaign is the headline
// reducer-seam case (issue #38's "a symbol change mid-Campaign"). AAPL opens
// a Campaign, is renamed to AAPL2 before the next Session, and an Add and
// the eventual exit both happen under the new id. The Campaign's identity
// (CampaignID), its frozen N and Unit size, and its whole life are
// unaffected — this is not a delisting followed by a new instrument.
func TestASymbolChangeCarriesTheCampaignAcrossMidCampaign(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	campaignN := breakoutFixtureN(t, cfg)
	rung2, err := sizing.NextAddLevel(campaignFillPrice, campaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("NextAddLevel() error = %v", err)
	}

	s := newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", symbolChangeAt))
	changeID := s.envelopes[len(s.envelopes)-1].ID

	before := s.mustRun()
	fromChange := causedBy(before, changeID)
	if len(fromChange) != 1 {
		t.Fatalf("the symbol change caused %d decision(s), want 1 (the symbol-changed decision): %v", len(fromChange), fromChange)
	}
	got := decodeInstrumentSymbolChanged(t, fromChange[0])
	want := event.InstrumentSymbolChangedPayload{
		InstrumentID:      "AAPL",
		NewInstrumentID:   "AAPL2",
		CorporateActionID: changeID,
		EffectiveAt:       symbolChangeAt,
		CampaignID:        campaignID,
		Rule:              event.RuleSymbolChangeCarriesInstrumentState,
		ADR:               event.ADRSymbolChangeCarriesInstrumentState,
	}
	if got != want {
		t.Fatalf("symbol changed = %+v, want %+v", got, want)
	}

	// The Add opportunity and its fill both happen under the NEW id. If the
	// rename had left campaignState.instrumentID at the old id, the Add
	// proposal below would wrongly cite "AAPL" instead of "AAPL2".
	emitted := s.bar(addOpportunityBar("AAPL2", day(57), rung2+5)).mustRun()
	proposal := decodeAddProposal(t, onlyEnvelopeOfType(t, emitted, event.AddProposalEventType))
	if proposal.InstrumentID != "AAPL2" {
		t.Fatalf("add proposal instrument id = %q, want %q: the Campaign's denormalised instrument id must move with the rename (ADR 0024)", proposal.InstrumentID, "AAPL2")
	}
	if proposal.CampaignID != campaignID {
		t.Fatalf("add proposal campaign id = %q, want the SAME campaign %q the rename carried across", proposal.CampaignID, campaignID)
	}

	s = s.fill(addFill("AAPL2", campaignID, 2, day(57), "sim-fill-add-2", rung2, 133, day(57)))

	// The exit, under the new id, closes the SAME Campaign with both Units'
	// whole life: no strategy.campaign.exited/opened pair ever fired at the
	// rename boundary, so this is the Campaign's only exit.
	breachAt := day(58)
	exitFill := closingExitFill("AAPL2", campaignID, 100, breachAt, day(59))
	exitFill.Quantity = 266
	emitted = s.bar(postEntryBar("AAPL2", breachAt, 99)).fill(exitFill).mustRun()
	exited := decodeCampaignExited(t, onlyEnvelopeOfType(t, emitted, event.CampaignExitedEventType))
	if exited.CampaignID != campaignID {
		t.Fatalf("exited campaign id = %q, want the same campaign %q throughout its life", exited.CampaignID, campaignID)
	}
	if exited.InstrumentID != "AAPL2" {
		t.Fatalf("exited instrument id = %q, want %q", exited.InstrumentID, "AAPL2")
	}
	if exited.Quantity != 266 || exited.Units != 2 {
		t.Fatalf("exited = {quantity %d, units %d}, want {266, 2}: both Units, opened before and after the rename, belong to the one Campaign it carried across", exited.Quantity, exited.Units)
	}
}

// TestASymbolChangeOfAnUnknownInstrumentIsANoOp mirrors applyDelisting's
// identical no-op: an instrument this reducer has never seen a bar for
// cannot have any state to carry across, so nothing is journalled — but the
// old id is still retired (see the next test).
func TestASymbolChangeOfAnUnknownInstrumentIsANoOp(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).corporateAction(symbolChangeAction("MSFT", "MSFT2", day(1))).mustRun()
	if len(emitted) != 0 {
		t.Fatalf("emitted = %v, want none: an unknown instrument has no state to carry across", emitted)
	}
}

// TestASymbolChangeIntoAnAlreadyTrackedInstrumentFailsClosed pins ADR 0019's
// own rule, quoted in symbol_change.go: "a symbol rename must not merge
// unrelated instruments."
func TestASymbolChangeIntoAnAlreadyTrackedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		bar(addOpportunityBar("MSFT", day(57), 200)).
		corporateAction(symbolChangeAction("AAPL", "MSFT", day(57).Add(12*time.Hour))).
		wantRunError("already tracks", "merge two instruments")
}

// TestASymbolChangeOfADelistedInstrumentFailsClosed and
// TestASymbolChangeIntoADelistedInstrumentFailsClosed pin ADR 0009: neither
// side of a symbol change may already be delisted.
func TestASymbolChangeOfADelistedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		corporateAction(delistingAction("AAPL", day(1))).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", day(2))).
		wantRunError("already delisted")
}

func TestASymbolChangeIntoADelistedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		corporateAction(delistingAction("MSFT", day(1))).
		corporateAction(symbolChangeAction("AAPL", "MSFT", day(2))).
		wantRunError("already delisted")
}

// TestASymbolChangeBeforeItsLastBarFailsClosed pins the chronology every
// corporate action shares (ADR 0023/0024): it cannot predate the last
// completed bar this reducer holds for the old instrument.
func TestASymbolChangeBeforeItsLastBarFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", day(55))).
		wantRunError("predates the last completed bar")
}

// TestABarForARenamedInstrumentFailsClosed: unlike a delisted instrument's
// bar, which is absorbed, a bar for an instrument this reducer has already
// renamed away is a reconciliation failure and fails closed (ADR 0024),
// because the old id DID have a stake here before the rename.
func TestABarForARenamedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", symbolChangeAt)).
		bar(addOpportunityBar("AAPL", day(57), 250)).
		wantRunError("a symbol change already moved its Campaign", "AAPL2")
}

// TestASymbolChangeIntoAnInstrumentWithABarInTheOpenSessionFailsClosed pins
// the ADR 0010/0021 ordering symbol_change.go's own ADDITIONAL check exists
// for: the dispatcher (applyCorporateAction) only ever checks the OLD id
// against the open Session, so a producer delivering the change after the
// NEW id's own bar of that same, still-open Session would slip through
// without this check.
func TestASymbolChangeIntoAnInstrumentWithABarInTheOpenSessionFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		barOnly(addOpportunityBar("AAPL2", day(57), 200)).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", day(57).Add(12*time.Hour))).
		wantRunError("already holds", "state it between Sessions")
}

// TestAFillForARenamedInstrumentFailsClosed: an execution naming the OLD
// instrument id after a symbol change is a reconciliation failure, the same
// shape as a fill for a delisted instrument (campaign.go's applyFill).
func TestAFillForARenamedInstrumentFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	campaignID := testDecisionID("campaign", "AAPL", day(56))
	newStream(t, cfg).
		bars(breakoutBars("AAPL")).
		fill(openingFill("AAPL")).
		corporateAction(symbolChangeAction("AAPL", "AAPL2", symbolChangeAt)).
		fill(closingExitFill("AAPL", campaignID, 100, day(57), day(57))).
		wantRunError("a symbol change moved its Campaign and proposals to", "AAPL2")
}
