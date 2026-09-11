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

// --- CampaignExitedPayload (#12) -----------------------------------------

// campaignExitedStopPrice is where validCampaignExited's stop fill actually
// executes: campaignFillPrice - 2*proposalN's protective stop MINUS a small
// gap, so ExitPrice sits strictly below ProtectiveStopLevel — proving the
// payload records what was filled, never the level (ADR 0005's gap rule).
var campaignExitedStopPrice = (campaignEntryPrice - 2*proposalN) - 0.50

// validCampaignExited returns the Campaign-exited that would close
// validCampaignOpened by a stop fill that gapped through the level.
func validCampaignExited() event.CampaignExitedPayload {
	entry := campaignEntryPrice
	exit := campaignExitedStopPrice
	n := proposalN
	dpp := 1.0
	var unitQuantity int64 = 133
	realisedResult := float64(133) * (exit - entry) * dpp
	return event.CampaignExitedPayload{
		CampaignID:            "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:          "AAPL",
		FillID:                "sim-fill-0002",
		ExitedAt:              proposalPeriodEnd.AddDate(0, 0, 1),
		Reason:                event.ExitReasonStop,
		EntryPrice:            entry,
		ExitPrice:             exit,
		Quantity:              133,
		CampaignN:             n,
		DollarsPerPoint:       dpp,
		UnitQuantity:          unitQuantity,
		ProtectiveStopLevel:   entry - 2*n,
		RealisedResult:        realisedResult,
		AverageMoveInN:        (exit - entry) / n,
		RealisedResultInUnitN: realisedResult / (float64(unitQuantity) * n * dpp),
		Units:                 1,
		Rule:                  event.RuleCampaignExitedByStop,
		ADR:                   event.ADRCampaignExitRecordsTheFill,
	}
}

func TestCampaignExitedPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.CampaignExitedPayload)
		wantErr string
	}{
		{name: "valid campaign exited"},
		{
			name:    "missing campaign id",
			mutate:  func(p *event.CampaignExitedPayload) { p.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			name:    "missing instrument id",
			mutate:  func(p *event.CampaignExitedPayload) { p.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			name:    "missing fill id",
			mutate:  func(p *event.CampaignExitedPayload) { p.FillID = "" },
			wantErr: "fill id",
		},
		{
			name:    "missing exited at",
			mutate:  func(p *event.CampaignExitedPayload) { p.ExitedAt = time.Time{} },
			wantErr: "exited at",
		},
		{
			name:    "unrecognised reason",
			mutate:  func(p *event.CampaignExitedPayload) { p.Reason = "margin-call" },
			wantErr: "reason",
		},
		{
			name:    "missing rule",
			mutate:  func(p *event.CampaignExitedPayload) { p.Rule = "" },
			wantErr: "rule",
		},
		{
			name:    "missing adr",
			mutate:  func(p *event.CampaignExitedPayload) { p.ADR = "" },
			wantErr: "adr",
		},
		{
			name:    "zero entry price",
			mutate:  func(p *event.CampaignExitedPayload) { p.EntryPrice = 0 },
			wantErr: "entry price must be positive",
		},
		{
			name:    "zero exit price",
			mutate:  func(p *event.CampaignExitedPayload) { p.ExitPrice = 0 },
			wantErr: "exit price must be positive",
		},
		{
			name:    "zero quantity",
			mutate:  func(p *event.CampaignExitedPayload) { p.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "negative quantity",
			mutate:  func(p *event.CampaignExitedPayload) { p.Quantity = -1 },
			wantErr: "quantity",
		},
		{
			name:    "zero campaign n",
			mutate:  func(p *event.CampaignExitedPayload) { p.CampaignN = 0 },
			wantErr: "campaign n must be positive",
		},
		{
			name:    "zero dollars per point",
			mutate:  func(p *event.CampaignExitedPayload) { p.DollarsPerPoint = 0 },
			wantErr: "dollars per point must be positive",
		},
		{
			name:    "protective stop level at zero",
			mutate:  func(p *event.CampaignExitedPayload) { p.ProtectiveStopLevel = 0 },
			wantErr: "protective stop level must be positive",
		},
		{
			name:    "protective stop level at or above entry price",
			mutate:  func(p *event.CampaignExitedPayload) { p.ProtectiveStopLevel = p.EntryPrice },
			wantErr: "must be below the entry price",
		},
		{
			// Exact float64 equality, same discipline as every other derived
			// field in this package.
			name: "realised result does not match its derivation",
			mutate: func(p *event.CampaignExitedPayload) {
				p.RealisedResult += 0.01
			},
			wantErr: "realised result",
		},
		{
			name: "average move in n does not match its derivation",
			mutate: func(p *event.CampaignExitedPayload) {
				p.AverageMoveInN += 0.01
			},
			wantErr: "average move in n",
		},
		{
			// #74 review ("N Result Ignores Units"): the aggregate Unit-N
			// reading, re-derived via sizing.RealisedResultInUnitN.
			name: "realised result in unit n does not match its derivation",
			mutate: func(p *event.CampaignExitedPayload) {
				p.RealisedResultInUnitN += 0.01
			},
			wantErr: "realised result in unit n",
		},
		{
			name:    "zero unit quantity",
			mutate:  func(p *event.CampaignExitedPayload) { p.UnitQuantity = 0 },
			wantErr: "unit quantity",
		},
		{
			name:    "negative unit quantity",
			mutate:  func(p *event.CampaignExitedPayload) { p.UnitQuantity = -1 },
			wantErr: "unit quantity",
		},
		{
			// A gap fill below the level is legitimate under ADR 0005: the
			// payload must accept ExitPrice strictly below
			// ProtectiveStopLevel, proving it never rejects "the fill missed
			// the level".
			name: "exit price below the protective stop level on a gap is accepted",
			mutate: func(p *event.CampaignExitedPayload) {
				// Already true of validCampaignExited() itself; this case
				// documents the intent explicitly with no further mutation.
			},
		},
		{
			// #14: a Campaign that added Units closes more than one at once
			// (see the type's own "Multi-Unit aggregation" doc comment).
			name:   "units above one is accepted",
			mutate: func(p *event.CampaignExitedPayload) { p.Units = 4 },
		},
		{
			name:    "zero units",
			mutate:  func(p *event.CampaignExitedPayload) { p.Units = 0 },
			wantErr: "units",
		},
		{
			name:    "negative units",
			mutate:  func(p *event.CampaignExitedPayload) { p.Units = -1 },
			wantErr: "units",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validCampaignExited()
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

func TestCampaignExitedPayloadValidateRejectsNonFiniteFields(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name    string
		apply   func(p *event.CampaignExitedPayload, f float64)
		wantErr string
	}
	fields := []fieldCase{
		{"entry price", func(p *event.CampaignExitedPayload, f float64) { p.EntryPrice = f }, "entry price must be finite"},
		{"exit price", func(p *event.CampaignExitedPayload, f float64) { p.ExitPrice = f }, "exit price must be finite"},
		{"campaign n", func(p *event.CampaignExitedPayload, f float64) { p.CampaignN = f }, "campaign n must be finite"},
		{"dollars per point", func(p *event.CampaignExitedPayload, f float64) { p.DollarsPerPoint = f }, "dollars per point must be finite"},
		{"protective stop level", func(p *event.CampaignExitedPayload, f float64) { p.ProtectiveStopLevel = f }, "protective stop level must be finite"},
		{"realised result", func(p *event.CampaignExitedPayload, f float64) { p.RealisedResult = f }, "realised result must be finite"},
		{"average move in n", func(p *event.CampaignExitedPayload, f float64) { p.AverageMoveInN = f }, "average move in n must be finite"},
		{"realised result in unit n", func(p *event.CampaignExitedPayload, f float64) { p.RealisedResultInUnitN = f }, "realised result in unit n must be finite"},
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
				payload := validCampaignExited()
				field.apply(&payload, nf.value)

				err := payload.Validate()
				if err == nil || !strings.Contains(err.Error(), field.wantErr) {
					t.Fatalf("Validate() error = %v, want substring %q", err, field.wantErr)
				}
			})
		}
	}
}

func TestCampaignExitedPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.CampaignExitedPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"campaign id",
		"instrument id",
		"fill id",
		"exited at",
		"reason",
		"rule",
		"adr",
		"entry price",
		"exit price",
		"quantity",
		"unit quantity",
		"campaign n",
		"dollars per point",
		"protective stop level",
		"units",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestCampaignExitedEventConstants(t *testing.T) {
	t.Parallel()

	if event.CampaignExitedEventType != "strategy.campaign.exited" {
		t.Errorf("CampaignExitedEventType = %q, want %q", event.CampaignExitedEventType, "strategy.campaign.exited")
	}
	// Bumped 1 -> 2 for #14: Units was added.
	if event.CampaignExitedSchemaVersion != 2 {
		t.Errorf("CampaignExitedSchemaVersion = %d, want 2", event.CampaignExitedSchemaVersion)
	}
	if event.RuleCampaignExitedByStop != "campaign.exited.by-stop" {
		t.Errorf("RuleCampaignExitedByStop = %q", event.RuleCampaignExitedByStop)
	}
	// ADR 0005 is the fill model: gaps fill at the open, so a stop's actual
	// exit price may sit below the level — this event cites the ADR whose
	// rule that is.
	if event.ADRCampaignExitRecordsTheFill != "0005" {
		t.Errorf("ADRCampaignExitRecordsTheFill = %q, want %q", event.ADRCampaignExitRecordsTheFill, "0005")
	}
	if event.ExitReasonStop != "stop" {
		t.Errorf("ExitReasonStop = %q, want %q", event.ExitReasonStop, "stop")
	}
	// #13: the Exit-Channel exit reuses this same event type and payload
	// (ADR 0002, The Turtle Rules p.26), adding a Reason value rather than
	// minting a second event type — the pattern this payload's own doc
	// comment already anticipated.
	if event.ExitReasonExitChannel != "exit-channel" {
		t.Errorf("ExitReasonExitChannel = %q, want %q", event.ExitReasonExitChannel, "exit-channel")
	}
	if event.RuleCampaignExitedByExitChannel != "campaign.exited.by-exit-channel" {
		t.Errorf("RuleCampaignExitedByExitChannel = %q", event.RuleCampaignExitedByExitChannel)
	}
}

// validCampaignExitedByExitChannel returns the Campaign-exited that would
// close validCampaignOpened by an exit-channel fill gapping below the
// channel level (ADR 0005's gap rule applies to every fill kind alike).
func validCampaignExitedByExitChannel() event.CampaignExitedPayload {
	entry := campaignEntryPrice
	exit := 179.5
	n := proposalN
	dpp := 1.0
	// ProtectiveStopLevel restates the Campaign's stop as it stood at close,
	// independent of the exit-channel level that actually triggered this
	// exit (the two are unrelated numbers — see CampaignExitedPayload's doc
	// comment).
	stopLevel := entry - 2*n
	var unitQuantity int64 = 133
	realisedResult := float64(133) * (exit - entry) * dpp
	return event.CampaignExitedPayload{
		CampaignID:            "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		InstrumentID:          "AAPL",
		FillID:                "sim-fill-0003",
		ExitedAt:              proposalPeriodEnd.AddDate(0, 0, 21),
		Reason:                event.ExitReasonExitChannel,
		EntryPrice:            entry,
		ExitPrice:             exit,
		Quantity:              133,
		CampaignN:             n,
		DollarsPerPoint:       dpp,
		UnitQuantity:          unitQuantity,
		ProtectiveStopLevel:   stopLevel,
		RealisedResult:        realisedResult,
		AverageMoveInN:        (exit - entry) / n,
		RealisedResultInUnitN: realisedResult / (float64(unitQuantity) * n * dpp),
		Units:                 1,
		Rule:                  event.RuleCampaignExitedByExitChannel,
		ADR:                   event.ADRCampaignExitRecordsTheFill,
	}
}

// TestCampaignExitedPayloadValidateAcceptsExitChannelReason covers the
// ticket's own reason value directly, since TestCampaignExitedPayloadValidate
// above only ever mutates AWAY from validCampaignExited's stop-kind fixture.
func TestCampaignExitedPayloadValidateAcceptsExitChannelReason(t *testing.T) {
	t.Parallel()

	if err := validCampaignExitedByExitChannel().Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a valid exit-channel exit", err)
	}
}

// TestCampaignExitedPayloadMultiUnitAggregationMatchesPerUnitSum is #14's
// own headline case for the "Multi-Unit aggregation" design decision
// recorded on the type's doc comment: EntryPrice as the quantity-weighted
// average fill price, together with RealisedResult's UNCHANGED
// Quantity x (ExitPrice - EntryPrice) x DollarsPerPoint formula, must equal
// the sum of what each Unit realised on its own — proving the aggregate
// formula was not a simplification that silently changed the number.
//
// It also covers the PR #74 review finding ("N Result Ignores Units") this
// fixture was extended to catch: RealisedResultInUnitN, not AverageMoveInN,
// is what sums to each Unit's own QUANTITY-WEIGHTED N contribution — a Unit
// filled for only a fraction of the frozen Unit size contributes that same
// fraction of its own N move, exactly mirroring how a partial Unit already
// contributes only its own fraction to the dollar RealisedResult above. The
// distinction the finding named — several Units each moving a real amount
// of N must not be reported as if only one Unit had — is what separates
// this from AverageMoveInN, which is a plain per-share average with no
// quantity weighting of its own beyond the entry price average.
func TestCampaignExitedPayloadMultiUnitAggregationMatchesPerUnitSum(t *testing.T) {
	t.Parallel()

	// Four Units, quantities and fills chosen to be genuinely unequal so the
	// weighted average is not the same as a plain average, and so a
	// partially-filled Unit's own N contribution is visibly scaled down.
	quantities := []int64{133, 66, 54, 47}
	fills := []float64{201.25, 220.04, 238.83, 257.62}
	exit := 300.0
	dpp := 1.0
	n := proposalN
	var unitQuantity int64 = 133 // the campaign's frozen full Unit size

	var perUnitSum float64
	var perUnitNWeightedSum float64
	var totalQuantity int64
	var weightedNumerator float64
	for i := range quantities {
		perUnitSum += float64(quantities[i]) * (exit - fills[i]) * dpp
		perUnitNWeightedSum += (float64(quantities[i]) * (exit - fills[i])) / (float64(unitQuantity) * n)
		totalQuantity += quantities[i]
		weightedNumerator += float64(quantities[i]) * fills[i]
	}
	weightedEntry := weightedNumerator / float64(totalQuantity)

	aggregateResult := float64(totalQuantity) * (exit - weightedEntry) * dpp
	if diff := aggregateResult - perUnitSum; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("aggregate result %v does not match the sum of per-unit results %v (diff %v)", aggregateResult, perUnitSum, diff)
	}
	averageMoveInN := (exit - weightedEntry) / n
	realisedResultInUnitN := aggregateResult / (float64(unitQuantity) * n * dpp)
	// The headline assertion: RealisedResultInUnitN, not AverageMoveInN,
	// is what sums to each Unit's own quantity-weighted N contribution. If
	// the payload only carried the (renamed) average, a reader would see
	// ~averageMoveInN and understate the Campaign's real N result.
	if diff := realisedResultInUnitN - perUnitNWeightedSum; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("realised result in unit n %v does not match the sum of per-unit quantity-weighted n contributions %v (diff %v)", realisedResultInUnitN, perUnitNWeightedSum, diff)
	}
	if averageMoveInN == realisedResultInUnitN {
		t.Fatalf("average move in n (%v) coincidentally equals realised result in unit n (%v); the fixture must keep them genuinely different so the two fields are provably distinct", averageMoveInN, realisedResultInUnitN)
	}

	payload := validCampaignExited()
	payload.Quantity = totalQuantity
	payload.EntryPrice = weightedEntry
	payload.ExitPrice = exit
	payload.DollarsPerPoint = dpp
	payload.RealisedResult = aggregateResult
	payload.CampaignN = n
	payload.UnitQuantity = unitQuantity
	payload.AverageMoveInN = averageMoveInN
	payload.RealisedResultInUnitN = realisedResultInUnitN
	payload.ProtectiveStopLevel = weightedEntry - 2*n
	payload.Units = len(quantities)

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil for a genuinely multi-unit exit", err)
	}
}

// TestCampaignExitedPayloadByExitChannelRoundTrip mirrors
// TestCampaignExitedPayloadRoundTrip for the exit-channel fixture.
func TestCampaignExitedPayloadByExitChannelRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignExitedByExitChannel()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignExitedPayload
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

func TestCampaignExitedPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validCampaignExited()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.CampaignExitedPayload
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

func TestCampaignExitedPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validCampaignExited())
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
		"fill_id",
		"exited_at",
		"reason",
		"entry_price",
		"exit_price",
		"quantity",
		"campaign_n",
		"dollars_per_point",
		"unit_quantity",
		"protective_stop_level",
		"realised_result",
		"average_move_in_n",
		"realised_result_in_unit_n",
		"units",
		"rule",
		"adr",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}
