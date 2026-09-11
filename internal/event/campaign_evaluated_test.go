package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

var campaignEvaluatedPeriodEnd = time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC)

// campaignEvaluatedUnitQuantity/campaignEvaluatedDollarsPerPoint/
// campaignEvaluatedNotionalAccount are the fixture's own sizing figures,
// matching the single-Unit Campaign campaignEntryPrice/proposalN (fill.go's
// own fixtures) describe.
const (
	campaignEvaluatedUnitQuantity    = int64(133)
	campaignEvaluatedDollarsPerPoint = 1.0
	campaignEvaluatedNotionalAccount = 1_000_000.0
)

// validCampaignEvaluated returns a ready-channel, no-breach reading for the
// Campaign validCampaignOpened describes: the Exit Channel low (180) sits
// below the Campaign's Protective Stop, and both sit below the bar that
// produced this reading, so nothing here is a breach. Units holds the
// single Unit this fixture's Campaign has ever held, so ProtectiveStop (the
// minimum across Units) coincides with that Unit's own — #11/#12's own
// single-Unit fixtures are unaffected by #15's Units/aggregate-risk
// addition.
func validCampaignEvaluated() event.CampaignEvaluatedPayload {
	stop := 126.09441570423544
	aggregateOpenRisk := (campaignEntryPrice - stop) * float64(campaignEvaluatedUnitQuantity) * campaignEvaluatedDollarsPerPoint
	return event.CampaignEvaluatedPayload{
		CampaignID:     "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:   "AAPL",
		PeriodEnd:      campaignEvaluatedPeriodEnd,
		ProtectiveStop: stop,
		Units: []event.CampaignEvaluatedUnit{
			{UnitIndex: 1, EntryPrice: campaignEntryPrice, Quantity: campaignEvaluatedUnitQuantity, ProtectiveStop: stop},
		},
		ExitChannelLow:            180,
		ExitChannelReady:          true,
		ExitConditionMet:          false,
		DollarsPerPoint:           campaignEvaluatedDollarsPerPoint,
		AggregateOpenRisk:         aggregateOpenRisk,
		NotionalAccount:           campaignEvaluatedNotionalAccount,
		AggregateOpenRiskFraction: aggregateOpenRisk / campaignEvaluatedNotionalAccount,
	}
}

func TestCampaignEvaluatedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignEvaluatedPayload)
		wantErr string
	}{
		{name: "valid campaign evaluated"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing period end",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.PeriodEnd = time.Time{} },
			wantErr: "period end",
		},
		{
			name:    "zero protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = 0 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "negative protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = -1 },
			wantErr: "protective stop must be positive",
		},
		{
			name:    "non-finite protective stop",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ProtectiveStop = math.NaN() },
			wantErr: "protective stop must be finite",
		},
		{
			name:    "negative exit channel low",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = -1 },
			wantErr: "exit channel low must not be negative",
		},
		{
			name:    "non-finite exit channel low",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = math.NaN() },
			wantErr: "exit channel low must be finite",
		},
		{
			// Matches SetupEvaluatedPayload.EntryChannelHigh's convention: a
			// ready channel must report a real (positive) level.
			name:    "exit channel low zero while ready",
			mutate:  func(p *event.CampaignEvaluatedPayload) { p.ExitChannelLow = 0 },
			wantErr: "exit channel low must be positive when ready",
		},
		{
			// The mirror case: not ready must report exactly zero, the same
			// convention N and EntryChannelHigh already use, so a consumer
			// reading a level without checking readiness first sees an
			// unambiguous "not evaluable" value rather than a stale one.
			name: "exit channel low nonzero while not ready",
			mutate: func(p *event.CampaignEvaluatedPayload) {
				p.ExitChannelReady = false
				p.ExitConditionMet = false
			},
			wantErr: "exit channel low must be zero while not ready",
		},
		{
			// The Exit Channel must be ready for a breach to be possible at
			// all: an unready channel has no real level to have fallen below.
			name: "exit condition met while not ready",
			mutate: func(p *event.CampaignEvaluatedPayload) {
				p.ExitChannelReady = false
				p.ExitChannelLow = 0
				p.ExitConditionMet = true
			},
			wantErr: "exit condition met must be false while the exit channel is not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignEvaluated()
			if tt.mutate != nil {
				tt.mutate(&payload)
			}

			err := payload.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// TestCampaignEvaluatedPayloadValidateAcceptsNotReadyWithNoBreach covers the
// legitimate "not ready" reading directly: possible only when a Campaign
// opened very early (fewer than 20 preceding completed bars exist yet), it
// must validate cleanly with ExitChannelLow at its zero convention and
// ExitConditionMet false.
func TestCampaignEvaluatedPayloadValidateAcceptsNotReadyWithNoBreach(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluated()
	payload.ExitChannelReady = false
	payload.ExitChannelLow = 0
	payload.ExitConditionMet = false

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a legitimate not-ready reading", err)
	}
}

// TestCampaignEvaluatedPayloadValidateAcceptsABreach covers the other
// legitimate combination: ready and breached.
func TestCampaignEvaluatedPayloadValidateAcceptsABreach(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluated()
	payload.ExitConditionMet = true

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a legitimate breach reading", err)
	}
}

func TestCampaignEvaluatedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignEvaluatedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"period end",
		"protective stop",
		"units is required",
		"dollars per point",
		"notional account",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignEvaluatedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignEvaluatedEventType != "strategy.campaign.evaluated" {
		t.Errorf("CampaignEvaluatedEventType = %q, want %q", event.CampaignEvaluatedEventType, "strategy.campaign.evaluated")
	}
	if event.CampaignEvaluatedSchemaVersion != 2 {
		t.Errorf("CampaignEvaluatedSchemaVersion = %d, want 2", event.CampaignEvaluatedSchemaVersion)
	}
}

// --- #15: Units, and the aggregate open risk re-derivation -----------------

// validCampaignEvaluatedTwoUnits returns a legitimate two-Unit reading, with
// Unit 1's stop already raised to match Unit 2's own (the non-gap case) —
// so ProtectiveStop (the minimum) equals BOTH units' own level.
func validCampaignEvaluatedTwoUnits(t *testing.T) event.CampaignEvaluatedPayload {
	t.Helper()
	const (
		entry1 = 28.30
		entry2 = 28.90
		n      = 1.20
	)
	stop, err := sizing.ProtectiveStopLevel(entry2, n, 2.0, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("ProtectiveStopLevel() error = %v", err)
	}
	units := []event.CampaignEvaluatedUnit{
		{UnitIndex: 1, EntryPrice: entry1, Quantity: 100, ProtectiveStop: stop},
		{UnitIndex: 2, EntryPrice: entry2, Quantity: 100, ProtectiveStop: stop},
	}
	sizingUnits := make([]sizing.UnitOpenRisk, len(units))
	for i, u := range units {
		sizingUnits[i] = sizing.UnitOpenRisk{EntryPrice: u.EntryPrice, ProtectiveStop: u.ProtectiveStop, Quantity: u.Quantity}
	}
	aggregate, err := sizing.AggregateOpenRisk(sizingUnits, 1.0)
	if err != nil {
		t.Fatalf("AggregateOpenRisk() error = %v", err)
	}
	const notionalAccount = 1_000_000.0
	return event.CampaignEvaluatedPayload{
		CampaignID:                "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:              "AAPL",
		PeriodEnd:                 campaignEvaluatedPeriodEnd,
		ProtectiveStop:            stop,
		Units:                     units,
		ExitChannelLow:            0,
		ExitChannelReady:          false,
		ExitConditionMet:          false,
		DollarsPerPoint:           1.0,
		AggregateOpenRisk:         aggregate,
		NotionalAccount:           notionalAccount,
		AggregateOpenRiskFraction: aggregate / notionalAccount,
	}
}

func TestCampaignEvaluatedPayloadValidateAcceptsMultipleUnits(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluatedTwoUnits(t)
	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a legitimate two-unit reading", err)
	}
}

// TestCampaignEvaluatedPayloadValidateProtectiveStopMustBeTheMinimum is the
// ticket's required check: ProtectiveStop must equal the minimum across
// Units' own ProtectiveStop, not some other figure.
func TestCampaignEvaluatedPayloadValidateProtectiveStopMustBeTheMinimum(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluatedTwoUnits(t)
	payload.Units[0].ProtectiveStop -= 1 // now the true minimum, but ProtectiveStop is not updated to match
	payload.Units[0].EntryPrice += 2     // keep entry > stop after lowering it

	err := payload.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not equal the minimum") {
		t.Fatalf("Validate() error = %v, want substring %q", err, "does not equal the minimum")
	}
}

// TestCampaignEvaluatedPayloadValidateRejectsUnitsOutOfOrder covers a
// non-ascending UnitIndex list.
func TestCampaignEvaluatedPayloadValidateRejectsUnitsOutOfOrder(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluatedTwoUnits(t)
	payload.Units[0], payload.Units[1] = payload.Units[1], payload.Units[0]

	err := payload.Validate()
	if err == nil || !strings.Contains(err.Error(), "strictly ascending") {
		t.Fatalf("Validate() error = %v, want substring %q", err, "strictly ascending")
	}
}

// TestCampaignEvaluatedPayloadValidateRejectsAggregateOpenRiskMismatch is
// the ticket's required negative: a payload claiming the CORRECTLY-laddered
// aggregate while its own Units list carries the prototype's bug (each Unit
// given a fresh, un-raised stop) must be rejected — the validator, not just
// the reducer, must reject it (issue #15's "assert the validator rejects a
// payload claiming the ladder's aggregate while carrying those stops").
func TestCampaignEvaluatedPayloadValidateRejectsAggregateOpenRiskMismatch(t *testing.T) {
	t.Parallel()

	payload := validCampaignEvaluatedTwoUnits(t)
	// Claim a smaller aggregate than what these (correctly-laddered) Units
	// actually derive to.
	payload.AggregateOpenRisk = 1
	payload.AggregateOpenRiskFraction = payload.AggregateOpenRisk / payload.NotionalAccount

	err := payload.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not match the derivation") {
		t.Fatalf("Validate() error = %v, want substring %q", err, "does not match the derivation")
	}
}

// TestCampaignEvaluatedPayloadValidateRejectsFreshRiskPerUnit is the
// ticket's headline negative, at the payload seam: four Units, each given a
// FRESH 2N stop (the prototype's risk-multiplication bug) rather than the
// correctly-raised Stop Ladder, with AggregateOpenRisk claiming the
// (smaller, correct) laddered figure. Validate must reject it.
func TestCampaignEvaluatedPayloadValidateRejectsFreshRiskPerUnit(t *testing.T) {
	t.Parallel()

	const n = 1.20
	entries := []float64{28.30, 28.90, 29.50, 30.10}
	units := make([]event.CampaignEvaluatedUnit, len(entries))
	for i, entry := range entries {
		stop, err := sizing.ProtectiveStopLevel(entry, n, 2.0, sizing.DirectionLong)
		if err != nil {
			t.Fatalf("ProtectiveStopLevel(%v) error = %v", entry, err)
		}
		units[i] = event.CampaignEvaluatedUnit{UnitIndex: i + 1, EntryPrice: entry, Quantity: 100, ProtectiveStop: stop}
	}
	minStop := units[0].ProtectiveStop
	for _, u := range units[1:] {
		if u.ProtectiveStop < minStop {
			minStop = u.ProtectiveStop
		}
	}
	// The correctly-laddered aggregate this fixture WRONGLY claims: 5N x
	// quantity, computed independently of the (bugged) Units list above.
	const notionalAccount = 1_000_000.0
	wrongAggregate := 100.0 * 5 * n
	payload := event.CampaignEvaluatedPayload{
		CampaignID:                "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:              "AAPL",
		PeriodEnd:                 campaignEvaluatedPeriodEnd,
		ProtectiveStop:            minStop,
		Units:                     units,
		ExitChannelLow:            0,
		ExitChannelReady:          false,
		ExitConditionMet:          false,
		DollarsPerPoint:           1.0,
		AggregateOpenRisk:         wrongAggregate,
		NotionalAccount:           notionalAccount,
		AggregateOpenRiskFraction: wrongAggregate / notionalAccount,
	}

	err := payload.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not match the derivation") {
		t.Fatalf("Validate() error = %v, want substring %q (a fresh 2N stop per unit is NOT the ladder's aggregate)", err, "does not match the derivation")
	}
}

func TestCampaignEvaluatedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignEvaluated()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignEvaluatedPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}

	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-Marshal() error = %v", err)
	}
	if !bytes.Equal(encoded, reEncoded) {
		t.Fatalf("round trip not stable:\n  first:  %s\n  second: %s", encoded, reEncoded)
	}
}

func TestCampaignEvaluatedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignEvaluated())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"campaign_id",
		"instrument_id",
		"period_end",
		"protective_stop",
		"exit_channel_low",
		"exit_channel_ready",
		"exit_condition_met",
		"units",
		"dollars_per_point",
		"aggregate_open_risk",
		"notional_account",
		"aggregate_open_risk_fraction",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
