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

// validFill returns a fill that satisfies every validation rule: the full
// 133-share Unit of validTradeProposal, executed at campaignEntryPrice.
func validFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID: "AAPL",
		ProposalID:   "proposal:AAPL:2026-02-27T00:00:00.000000000Z",
		FillID:       "sim-fill-0001",
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        campaignEntryPrice,
		FilledAt:     proposalPeriodEnd,
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

func TestFillPayloadValidateAggregatesEveryField(t *testing.T) {
	t.Parallel()

	var payload event.FillPayload

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want aggregated error")
	}
	for _, want := range []string{
		"instrument id",
		"proposal id",
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
	if event.FillSchemaVersion != 1 {
		t.Errorf("FillSchemaVersion = %d, want 1 (a new payload starts at 1)", event.FillSchemaVersion)
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
		"proposal_id",
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
