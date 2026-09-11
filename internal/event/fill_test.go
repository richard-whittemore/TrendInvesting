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

// campaignEntryPrice is the price the fixtures below fill at.
//
// It is deliberately NOT the trade proposal's entry level (200, see
// validTradeProposal): the whole point of #11 is that a Campaign records the
// price that actually filled, including the producer's slippage (ADR 0013),
// rather than the level the Signal fired at. A fixture that used 200 for both
// could not tell the two apart.
//
// A float64 variable rather than an untyped constant, for the reason recorded
// on proposalN: Validate re-derives the Protective Stop exactly, and Go
// evaluates untyped constant arithmetic in arbitrary precision, so a fixture
// folded from constants would test constant folding rather than the
// producer's float64 arithmetic.
var campaignEntryPrice = 201.25

// validFill returns an entry fill that satisfies every validation rule: the
// full 133-share Unit of validTradeProposal, executed at campaignEntryPrice.
func validFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindEntry,
		ProposalID:   "proposal:AAPL:2026-02-27T00:00:00.000000000Z",
		FillID:       "sim-fill-0001",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        campaignEntryPrice,
		FilledAt:     proposalPeriodEnd,
	}
}

// validStopFill returns a stop fill that satisfies every validation rule:
// the full 133-share Unit closing campaign:AAPL:2026-02-27T00:00:00.000000000Z,
// the same Campaign validCampaignOpened describes.
func validStopFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindStop,
		CampaignID:   "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		FillID:       "sim-fill-0002",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        126.09441570423544,
		FilledAt:     proposalPeriodEnd.AddDate(0, 0, 1),
	}
}

// validExitFill returns #13's third Kind: an exit fill closing the same
// Campaign as validStopFill, but referencing the exit proposal it executes
// (Kind requires BOTH CampaignID and ProposalID for an exit — the fill names
// the Campaign it closes AND the resting order it executed, unlike a stop
// fill, which closes a Campaign directly with no proposal of its own).
func validExitFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindExit,
		CampaignID:   "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		ProposalID:   "exit-proposal:AAPL:2026-03-20T00:00:00.000000000Z",
		FillID:       "sim-fill-0003",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        179.5,
		FilledAt:     proposalPeriodEnd.AddDate(0, 0, 21),
	}
}

func TestFillPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.FillPayload)
		wantErr string
	}{
		{name: "valid fill"},
		{
			name:    "missing instrument id",
			mutate:  func(f *event.FillPayload) { f.InstrumentID = "" },
			wantErr: "instrument id",
		},
		{
			// A fill that does not name the proposal it executes cannot be
			// reconciled against anything the strategy asked for, which
			// docs/architecture.md treats as a safe-mode condition rather than
			// something to absorb.
			name:    "missing proposal id",
			mutate:  func(f *event.FillPayload) { f.ProposalID = "" },
			wantErr: "proposal id",
		},
		{
			name:    "missing kind",
			mutate:  func(f *event.FillPayload) { f.Kind = "" },
			wantErr: "kind",
		},
		{
			name:    "unrecognised kind",
			mutate:  func(f *event.FillPayload) { f.Kind = "add" },
			wantErr: "kind",
		},
		{
			// An entry fill has no Campaign yet: naming one would claim a
			// position exists before the fill that is supposed to create it.
			name:    "entry fill names a campaign",
			mutate:  func(f *event.FillPayload) { f.CampaignID = "some-campaign" },
			wantErr: "campaign id",
		},
		{
			// A fill relabelled as a stop while still carrying the entry's
			// proposal id: the stop-specific table below (TestStopFillPayloadValidate)
			// covers the rest of the stop-kind pairing rules from
			// validStopFill; this pins the same rule from the opposite base
			// fixture, so both directions are covered.
			name: "fill relabelled as a stop still names a proposal",
			mutate: func(f *event.FillPayload) {
				f.Kind = event.FillKindStop
				f.CampaignID = "campaign:AAPL:2026-02-27T00:00:00.000000000Z"
				// f.ProposalID is left set from validFill().
			},
			wantErr: "proposal id",
		},
		{
			// The fill id is the idempotency key: duplicate delivery is
			// expected from any transport, and without an id there is no way
			// to tell a re-delivery from a second, genuinely different fill.
			name:    "missing fill id",
			mutate:  func(f *event.FillPayload) { f.FillID = "" },
			wantErr: "fill id",
		},
		{
			name:    "unrecognised direction",
			mutate:  func(f *event.FillPayload) { f.Direction = "short" },
			wantErr: "direction",
		},
		{
			name:    "missing direction",
			mutate:  func(f *event.FillPayload) { f.Direction = "" },
			wantErr: "direction",
		},
		{
			// A zero-quantity fill is not a fill. Nothing executed, so nothing
			// about position state may move.
			name:    "zero quantity",
			mutate:  func(f *event.FillPayload) { f.Quantity = 0 },
			wantErr: "quantity",
		},
		{
			name:    "negative quantity",
			mutate:  func(f *event.FillPayload) { f.Quantity = -1 },
			wantErr: "quantity",
		},
		{
			name:    "zero price",
			mutate:  func(f *event.FillPayload) { f.Price = 0 },
			wantErr: "price",
		},
		{
			name:    "negative price",
			mutate:  func(f *event.FillPayload) { f.Price = -1 },
			wantErr: "price",
		},
		{
			// Time arrives in the event, never from a wall clock
			// (.greptile/rules.md, determinism). A fill without one cannot be
			// stamped onto a decision at all.
			name:    "missing filled at",
			mutate:  func(f *event.FillPayload) { f.FilledAt = time.Time{} },
			wantErr: "filled at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validFill()
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

// TestStopFillPayloadValidate covers the stop-kind pairing rules from
// validStopFill, the mirror image of the entry-kind cases pinned above.
func TestStopFillPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.FillPayload)
		wantErr string
	}{
		{name: "valid stop fill"},
		{
			// A stop fill for an unknown campaign cannot be reconciled
			// against anything the strategy holds.
			name:    "missing campaign id",
			mutate:  func(f *event.FillPayload) { f.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			// A stop closes a Campaign, not a proposal: naming one is a
			// producer defect the payload rejects rather than absorbs.
			name:    "stop fill names a proposal id",
			mutate:  func(f *event.FillPayload) { f.ProposalID = "some-proposal" },
			wantErr: "proposal id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validStopFill()
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

// TestExitFillPayloadValidate covers the exit-kind pairing rule from
// validExitFill: unlike entry (requires ProposalID, forbids CampaignID) and
// stop (requires CampaignID, forbids ProposalID), an exit fill requires
// BOTH — it names the Campaign it closes and the exit proposal it executed.
func TestExitFillPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.FillPayload)
		wantErr string
	}{
		{name: "valid exit fill"},
		{
			name:    "missing campaign id",
			mutate:  func(f *event.FillPayload) { f.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			// Unlike a stop fill, an exit fill's ProposalID is not merely
			// tolerated — it is required: it is the join back to the exit
			// proposal (strategy.exit.proposed) this fill executes.
			name:    "missing proposal id",
			mutate:  func(f *event.FillPayload) { f.ProposalID = "" },
			wantErr: "proposal id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validExitFill()
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

func TestFillPayloadValidateRejectsNonFinitePrice(t *testing.T) {
	t.Parallel()

	for _, nf := range []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(nf.name, func(t *testing.T) {
			t.Parallel()

			payload := validFill()
			payload.Price = nf.value

			err := payload.Validate()
			if err == nil || !strings.Contains(err.Error(), "price must be finite") {
				t.Fatalf("Validate() error = %v, want substring %q", err, "price must be finite")
			}
		})
	}
}

// TestFillPayloadValidateAggregatesEveryField uses a totally zero payload,
// whose Kind is therefore "" — an unrecognised kind, not either pairing
// branch — so "proposal id"/"campaign id" are deliberately NOT asserted
// here; TestStopFillPayloadValidate and the entry-kind table above cover
// those once Kind is one of the two recognised values.
func TestFillPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.FillPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"instrument id",
		"kind",
		"fill id",
		"direction",
		"quantity",
		"price",
		"filled at",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

func TestFillEventConstants(t *testing.T) {
	t.Parallel()

	// "execution.fill", not "strategy.*": a fill is an external fact produced
	// by the execution venue (or, in a backtest, by #18's simulator), never a
	// decision this system made. docs/architecture.md: "Go never assumes an
	// intended order was filled."
	if event.FillEventType != "execution.fill" {
		t.Errorf("FillEventType = %q, want %q", event.FillEventType, "execution.fill")
	}
	if event.FillSchemaVersion != 2 {
		t.Errorf("FillSchemaVersion = %d, want 2 (#12 added Kind and CampaignID)", event.FillSchemaVersion)
	}
	if event.FillKindEntry != "entry" {
		t.Errorf("FillKindEntry = %q, want %q", event.FillKindEntry, "entry")
	}
	if event.FillKindStop != "stop" {
		t.Errorf("FillKindStop = %q, want %q", event.FillKindStop, "stop")
	}
	if event.FillKindExit != "exit" {
		t.Errorf("FillKindExit = %q, want %q", event.FillKindExit, "exit")
	}
}

func TestFillPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validFill()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.FillPayload
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

func TestFillPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validFill())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"instrument_id",
		"kind",
		"proposal_id",
		"campaign_id",
		"fill_id",
		"direction",
		"quantity",
		"price",
		"filled_at",
	} {
		if _, ok := asMap[key]; !ok {
			t.Errorf("encoded payload missing expected key %q: %s", key, encoded)
		}
	}
}

// TestStopFillPayloadJSONTags mirrors TestFillPayloadJSONTags from the
// stop-kind fixture, so campaign_id's presence is asserted from a payload
// that actually populates it (validFill leaves it at its zero value).
func TestStopFillPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validStopFill())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got, ok := asMap["kind"]; !ok || got != "stop" {
		t.Errorf("encoded payload kind = %v, want %q: %s", got, "stop", encoded)
	}
	if got, ok := asMap["campaign_id"]; !ok || got == "" {
		t.Errorf("encoded payload campaign_id = %v, want it populated: %s", got, encoded)
	}
}

// TestStopFillPayloadRoundTrip mirrors TestFillPayloadRoundTrip for the
// stop-kind fixture.
func TestStopFillPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validStopFill()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.FillPayload
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

// TestExitFillPayloadJSONTags mirrors TestStopFillPayloadJSONTags for the
// exit-kind fixture, so both campaign_id and proposal_id are asserted
// populated together (an exit fill, unlike a stop fill, requires both).
func TestExitFillPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validExitFill())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got, ok := asMap["kind"]; !ok || got != "exit" {
		t.Errorf("encoded payload kind = %v, want %q: %s", got, "exit", encoded)
	}
	if got, ok := asMap["campaign_id"]; !ok || got == "" {
		t.Errorf("encoded payload campaign_id = %v, want it populated: %s", got, encoded)
	}
	if got, ok := asMap["proposal_id"]; !ok || got == "" {
		t.Errorf("encoded payload proposal_id = %v, want it populated: %s", got, encoded)
	}
}

// TestExitFillPayloadRoundTrip mirrors TestFillPayloadRoundTrip for the
// exit-kind fixture.
func TestExitFillPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validExitFill()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded event.FillPayload
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
