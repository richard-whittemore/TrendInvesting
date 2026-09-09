package event_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// validCampaignOpened returns the Campaign that validFill would open from
// validTradeProposal: the full 133-share Unit, frozen at the proposal's N,
// entered at the actual fill price rather than the proposed entry level.
//
// Hand-worked: the Protective Stop is the actual fill price less two campaign
// N (The Turtle Rules p.22; ADR 0006 freezes the N), 201.25 - 2 x 37.5777...
// = 126.09... — which is NOT the proposal's stop intent of 124.84..., because
// the proposal's intent was measured from the intended level and the Campaign's
// stop is measured from what actually filled.
func validCampaignOpened() event.CampaignOpenedPayload {
	return event.CampaignOpenedPayload{
		CampaignID:     "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:   "AAPL",
		ProposalID:     "proposal:AAPL:2026-02-27T00:00:00.000000000Z",
		SignalID:       "signal:AAPL:2026-02-27T00:00:00.000000000Z",
		FillID:         "sim-fill-0001",
		Rule:           event.RuleCampaignOpenedFromFill,
		ADR:            event.ADRCampaignFrozenAtEntry,
		Direction:      event.DirectionLong,
		CampaignN:      proposalN,
		UnitQuantity:   133,
		FilledQuantity: 133,
		EntryPrice:     campaignEntryPrice,
		StopMultiple:   2,
		ProtectiveStop: campaignEntryPrice - 2*proposalN,
		Units:          1,
		OpenedAt:       proposalPeriodEnd,
	}
}

// validPartiallyFilledCampaign is the same Campaign opened by a partial fill:
// the Unit size stays frozen at the full 133 shares (ADR 0006 — the Add Ladder
// and Stop Ladder are computed from the frozen Unit), while FilledQuantity
// records how much of it actually executed.
func validPartiallyFilledCampaign() event.CampaignOpenedPayload {
	p := validCampaignOpened()
	p.FilledQuantity = 100
	return p
}

func TestCampaignOpenedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		base    func() event.CampaignOpenedPayload
		mutate  func(*event.CampaignOpenedPayload)
		wantErr string
	}{
		{name: "valid campaign opened"},
		{name: "valid partially filled campaign", base: validPartiallyFilledCampaign},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignOpenedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignOpenedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing proposal id",
			mutate:  func(p *event.CampaignOpenedPayload) { p.ProposalID = "" },
			wantErr: "proposal id",
		},
		{
			name:    "missing signal id",
			mutate:  func(p *event.CampaignOpenedPayload) { p.SignalID = "" },
			wantErr: "signal id",
		},
		{
			// The opening fill's id is what makes a re-delivered fill a no-op
			// rather than a second Campaign; a Campaign that does not record
			// it cannot be reconciled against the execution record.
			name:    "missing fill id",
			mutate:  func(p *event.CampaignOpenedPayload) { p.FillID = "" },
			wantErr: "fill id",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.CampaignOpenedPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.CampaignOpenedPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "unrecognised direction",
			mutate:  func(p *event.CampaignOpenedPayload) { p.Direction = "short" },
			wantErr: "direction",
		},
		{
			// ADR 0006: the whole Add Ladder and Stop Ladder are computed from
			// the frozen campaign N, so a zero or negative one would make every
			// later level meaningless (or divide by nothing).
			name:    "zero campaign n",
			mutate:  func(p *event.CampaignOpenedPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "negative campaign n",
			mutate:  func(p *event.CampaignOpenedPayload) { p.CampaignN = -1 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero unit quantity",
			mutate:  func(p *event.CampaignOpenedPayload) { p.UnitQuantity = 0 },
			wantErr: "unit quantity",
		},
		{
			name:    "negative unit quantity",
			mutate:  func(p *event.CampaignOpenedPayload) { p.UnitQuantity = -1 },
			wantErr: "unit quantity",
		},
		{
			name:    "zero filled quantity",
			mutate:  func(p *event.CampaignOpenedPayload) { p.FilledQuantity = 0 },
			wantErr: "filled quantity",
		},
		{
			name:    "negative filled quantity",
			mutate:  func(p *event.CampaignOpenedPayload) { p.FilledQuantity = -1 },
			wantErr: "filled quantity",
		},
		{
			// A fill larger than the Unit that was proposed is an
			// over-execution: the position risks more than the sizing
			// arithmetic budgeted for, which is the one direction the
			// truncation rules never allow (see TradeProposalPayload's
			// invariant 4).
			name:    "filled quantity above unit quantity",
			mutate:  func(p *event.CampaignOpenedPayload) { p.FilledQuantity = 134 },
			wantErr: "filled quantity",
		},
		{
			name:    "zero entry price",
			mutate:  func(p *event.CampaignOpenedPayload) { p.EntryPrice = 0 },
			wantErr: "entry price",
		},
		{
			name:    "negative entry price",
			mutate:  func(p *event.CampaignOpenedPayload) { p.EntryPrice = -1 },
			wantErr: "entry price",
		},
		{
			name:    "zero stop multiple",
			mutate:  func(p *event.CampaignOpenedPayload) { p.StopMultiple = 0 },
			wantErr: "stop multiple",
		},
		{
			name:    "negative stop multiple",
			mutate:  func(p *event.CampaignOpenedPayload) { p.StopMultiple = -1 },
			wantErr: "stop multiple",
		},
		{
			// The stated stop must be the one the payload's own fields
			// produce. Exact float64 equality, for the reason recorded on
			// TradeProposalPayload.Validate: a tolerance would let a
			// differently-derived stop through, which is the defect the check
			// exists to catch.
			name: "protective stop does not match its derivation",
			mutate: func(p *event.CampaignOpenedPayload) {
				p.ProtectiveStop = campaignEntryPrice - 2*proposalN + 0.01
			},
			wantErr: "does not match the derivation",
		},
		{
			// A long position cannot be stopped out at or below zero, so such
			// a Campaign is unprotected in fact while looking protected in the
			// journal.
			name: "protective stop at zero",
			mutate: func(p *event.CampaignOpenedPayload) {
				p.CampaignN = campaignEntryPrice / 2
				p.ProtectiveStop = p.EntryPrice - p.StopMultiple*p.CampaignN
			},
			wantErr: "protective stop must be positive",
		},
		{
			name: "protective stop above the entry price",
			mutate: func(p *event.CampaignOpenedPayload) {
				p.StopMultiple = 2
				p.CampaignN = -1 // negative N would put the stop above entry
				p.ProtectiveStop = p.EntryPrice - p.StopMultiple*p.CampaignN
			},
			wantErr: "must be below the entry price",
		},
		{
			// This is the Campaign-*opened* event: it always describes the
			// first Unit. Adds emit their own event (#13), so a value other
			// than one here means a producer conflated the two.
			name:    "units other than one",
			mutate:  func(p *event.CampaignOpenedPayload) { p.Units = 2 },
			wantErr: "units",
		},
		{
			name:    "zero units",
			mutate:  func(p *event.CampaignOpenedPayload) { p.Units = 0 },
			wantErr: "units",
		},
		{
			name:    "missing opened at",
			mutate:  func(p *event.CampaignOpenedPayload) { p.OpenedAt = time.Time{} },
			wantErr: "opened at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base := tt.base
			if base == nil {
				base = validCampaignOpened
			}
			payload := base()
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

func TestCampaignOpenedPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.CampaignOpenedPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"campaign n", func(p *event.CampaignOpenedPayload, f float64) { p.CampaignN = f }, "campaign n must be finite"},
		{"entry price", func(p *event.CampaignOpenedPayload, f float64) { p.EntryPrice = f }, "entry price must be finite"},
		{"stop multiple", func(p *event.CampaignOpenedPayload, f float64) { p.StopMultiple = f }, "stop multiple must be finite"},
		{"protective stop", func(p *event.CampaignOpenedPayload, f float64) { p.ProtectiveStop = f }, "protective stop must be finite"},
	}

	nonFinite := []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}

	for _, field := range fields {
		for _, nf := range nonFinite {
			t.Run(field.name+" "+nf.name, func(t *testing.T) {
				t.Parallel()
				payload := validCampaignOpened()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestCampaignOpenedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignOpenedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"proposal id",
		"signal id",
		"fill id",
		"rule",
		"adr",
		"direction",
		"campaign n",
		"unit quantity",
		"filled quantity",
		"entry price",
		"stop multiple",
		"protective stop",
		"units",
		"opened at",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignOpenedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignOpenedEventType != "strategy.campaign.opened" {
		t.Errorf("CampaignOpenedEventType = %q, want %q", event.CampaignOpenedEventType, "strategy.campaign.opened")
	}
	if event.CampaignOpenedSchemaVersion != 1 {
		t.Errorf("CampaignOpenedSchemaVersion = %d, want 1 (a new payload starts at 1)", event.CampaignOpenedSchemaVersion)
	}
	// The rule names the mechanism the ticket exists to encode: a Campaign
	// comes into being from a recorded fill, never from a submitted order.
	if event.RuleCampaignOpenedFromFill != "campaign.opened.from-fill" {
		t.Errorf("RuleCampaignOpenedFromFill = %q", event.RuleCampaignOpenedFromFill)
	}
	// ADR 0006 is the decision that freezes campaign N and the Unit share
	// count at first entry, and that requires them to be journaled because
	// replay depends on them rather than on recomputation.
	if event.ADRCampaignFrozenAtEntry != "0006" {
		t.Errorf("ADRCampaignFrozenAtEntry = %q, want %q", event.ADRCampaignFrozenAtEntry, "0006")
	}
}

func TestCampaignOpenedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignOpened()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignOpenedPayload
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

// TestCampaignOpenedPayloadCarriesEverythingTheLaddersNeed pins ADR 0006's
// consequence as a test rather than only as a doc comment: the Add Ladder
// (#13) and the Stop Ladder (#12) must be computable from the journalled
// Campaign alone, with no recomputation of N and no reference to the
// configuration in force later in the run.
func TestCampaignOpenedPayloadCarriesEverythingTheLaddersNeed(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignOpened())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignOpenedPayload
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	// The Baseline's Add Ladder adds a Unit every half a campaign N above the
	// entry (The Turtle Rules p.19-20), and its Stop Ladder places each stop
	// StopMultiple campaign N below its Unit's entry (p.22). Both are pure
	// arithmetic over the three frozen numbers, so the expectations below are
	// hand-computed from 201.25 and 37.57779214788228 rather than read back
	// from the payload:
	//
	//	first Add   = 201.25 + 0.5 x 37.5777... = 220.03889607394115
	//	second Add  = 201.25 + 1.0 x 37.5777... = 238.8277921478823
	//	first stop  = 201.25 - 2.0 x 37.5777... = 126.09441570423544
	//	Add size    = the frozen 133 shares, at every rung
	const tolerance = 1e-9
	for _, rung := range []struct {
		inN  float64
		want float64
	}{
		{0.5, 220.03889607394115},
		{1.0, 238.8277921478823},
	} {
		got := decoded.EntryPrice + rung.inN*decoded.CampaignN
		if diff := math.Abs(got - rung.want); diff > tolerance {
			t.Errorf("add level at %vN = %v, want %v (diff %v)", rung.inN, got, rung.want, diff)
		}
	}
	if diff := math.Abs(decoded.ProtectiveStop - 126.09441570423544); diff > tolerance {
		t.Errorf("protective stop = %v, want 126.09441570423544", decoded.ProtectiveStop)
	}
	if decoded.UnitQuantity != 133 {
		t.Errorf("unit quantity = %d, want the frozen 133 (every rung of the ladder is one Unit of this size)", decoded.UnitQuantity)
	}
}

func TestCampaignOpenedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignOpened())
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
		"proposal_id",
		"signal_id",
		"fill_id",
		"rule",
		"adr",
		"direction",
		"campaign_n",
		"unit_quantity",
		"filled_quantity",
		"entry_price",
		"stop_multiple",
		"protective_stop",
		"units",
		"opened_at",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
