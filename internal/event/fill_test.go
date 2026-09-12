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
		// #18: the level the order rested at, the slippage applied against
		// the trader, and the commission charged. Level here is the trade
		// proposal's own entry level (200); the executed price sits above it
		// by the slippage ADR 0013 requires on every fill.
		Level:           200,
		SlippageApplied: 0.05,
		Commission:      1.00,
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
		UnitIDs:      []string{"sim-fill-0001"},
		Direction:    event.DirectionLong,
		Quantity:     133,
		Price:        126.09441570423544,
		FilledAt:     proposalPeriodEnd.AddDate(0, 0, 1),
		// #18: a sell executes BELOW its level once slippage is applied
		// against the trader, the mirror of validFill's buy.
		Level:           126.14441570423544,
		SlippageApplied: 0.05,
		Commission:      1.00,
	}
}

// validExitFill returns #13's third Kind: an exit fill closing the same
// Campaign as validStopFill, but referencing the exit proposal it executes
// (Kind requires BOTH CampaignID and ProposalID for an exit — the fill names
// the Campaign it closes AND the resting order it executed, unlike a stop
// fill, which closes a Campaign directly with no proposal of its own).
func validExitFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID:    "AAPL",
		Kind:            event.FillKindExit,
		CampaignID:      "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		ProposalID:      "exit-proposal:AAPL:2026-03-20T00:00:00.000000000Z",
		FillID:          "sim-fill-0003",
		Direction:       event.DirectionLong,
		Quantity:        133,
		Price:           179.5,
		FilledAt:        proposalPeriodEnd.AddDate(0, 0, 21),
		Level:           179.55,
		SlippageApplied: 0.05,
		Commission:      1.00,
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
			mutate:  func(f *event.FillPayload) { f.Kind = "bogus" },
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
		{
			// #15: UnitIDs is a stop-only field; an entry fill naming one is
			// a producer defect, the same closed-shape rule every other
			// kind-specific field in this payload already follows.
			name:    "entry fill names unit ids",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = []string{"some-unit"} },
			wantErr: "unit ids must be empty for an entry fill",
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
		{
			// #15: a stop fill must name which units its own protective
			// stop closed, because the gap case can leave units at
			// different levels — "closes everything" is no longer a safe
			// default.
			name:    "missing unit ids",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = nil },
			wantErr: "unit ids is required",
		},
		{
			name:    "empty unit id",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = []string{""} },
			wantErr: "unit ids[0] is empty",
		},
		{
			name:    "duplicate unit id",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = []string{"sim-fill-0001", "sim-fill-0001"} },
			wantErr: "more than once",
		},
		{
			// Several units, closed by one gapped fill, is legitimate.
			name:   "several unit ids",
			mutate: func(f *event.FillPayload) { f.UnitIDs = []string{"sim-fill-0001", "sim-fill-add-2", "sim-fill-add-3"} },
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
		{
			name:    "exit fill names unit ids",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = []string{"some-unit"} },
			wantErr: "unit ids must be empty for an exit fill",
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
		"level",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %v, want substring %q", err, want)
		}
	}
}

// TestFillPayloadValidateCostFields covers #18's three additions across
// every Kind: the Level the order rested at, the slippage applied against the
// trader, and the commission charged.
//
// The rule the table encodes is deliberately asymmetric. Level is required
// and positive — every fill in this system executes a resting order at a
// stated level (ADR 0005), and a zero Level is exactly what a schema-3 record
// decodes to, so it must fail closed. SlippageApplied and Commission are
// required to be finite and non-negative but MAY be zero: ADR 0013's "never
// zero" rule is a rule about the simulator's CONFIGURATION, enforced where
// the run is configured, not a claim this payload can make about a real
// venue's execution (#30) — a venue that filled exactly at the level applied
// no slippage, and a commission-free venue charged nothing.
func TestFillPayloadValidateCostFields(t *testing.T) {
	t.Parallel()

	kinds := []struct {
		name string
		base func() event.FillPayload
	}{
		{"entry", validFill},
		{"stop", validStopFill},
		{"exit", validExitFill},
		{"add", validAddFill},
	}
	cases := []struct {
		name    string
		mutate  func(*event.FillPayload)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "zero level",
			mutate:  func(f *event.FillPayload) { f.Level = 0 },
			wantErr: "level must be positive",
		},
		{
			name:    "negative level",
			mutate:  func(f *event.FillPayload) { f.Level = -1 },
			wantErr: "level must be positive",
		},
		{
			name:    "non-finite level",
			mutate:  func(f *event.FillPayload) { f.Level = math.NaN() },
			wantErr: "level must be finite",
		},
		{
			name:    "zero slippage applied is accepted",
			mutate:  func(f *event.FillPayload) { f.SlippageApplied = 0 },
			wantErr: "",
		},
		{
			name:    "negative slippage applied",
			mutate:  func(f *event.FillPayload) { f.SlippageApplied = -0.05 },
			wantErr: "slippage applied must not be negative",
		},
		{
			name:    "non-finite slippage applied",
			mutate:  func(f *event.FillPayload) { f.SlippageApplied = math.Inf(1) },
			wantErr: "slippage applied must be finite",
		},
		{
			name:    "zero commission is accepted",
			mutate:  func(f *event.FillPayload) { f.Commission = 0 },
			wantErr: "",
		},
		{
			name:    "negative commission",
			mutate:  func(f *event.FillPayload) { f.Commission = -1 },
			wantErr: "commission must not be negative",
		},
		{
			name:    "non-finite commission",
			mutate:  func(f *event.FillPayload) { f.Commission = math.Inf(-1) },
			wantErr: "commission must be finite",
		},
	}

	for _, kind := range kinds {
		for _, tc := range cases {
			t.Run(kind.name+" "+tc.name, func(t *testing.T) {
				t.Parallel()

				payload := kind.base()
				if tc.mutate != nil {
					tc.mutate(&payload)
				}
				err := payload.Validate()
				switch {
				case tc.wantErr == "" && err != nil:
					t.Fatalf("Validate() error = %v, want nil", err)
				case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
					t.Fatalf("Validate() error = %v, want substring %q", err, tc.wantErr)
				}
			})
		}
	}
}

// TestFillPayloadDoesNotPoliceThePriceAgainstTheLevel pins the restraint
// FillPayload's own doc comment already states for the entry level, now that
// the level is carried on the payload: this contract records what happened,
// it does not re-derive whether the producer's fill model was honoured. ADR
// 0005 makes #18's simulator the sole authority on fill legitimacy, and a
// live venue may legitimately improve on a level.
func TestFillPayloadDoesNotPoliceThePriceAgainstTheLevel(t *testing.T) {
	t.Parallel()

	payload := validFill()
	// A buy executed BELOW the level it rested at: impossible under ADR
	// 0005's own model, and still not this payload's business to reject.
	payload.Level = payload.Price + 10

	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil: the payload must not re-derive the producer's fill model", err)
	}
}

// TestFillRecordedBeforeTheCostFieldsIsRejected is the #18 half of the schema
// bump, asserted through Validate rather than through the version constant: a
// schema-3 record decodes Level as zero, which is not a legitimate level, so
// it fails closed rather than being silently read as a fill that rested at
// nothing (ADR 0015).
func TestFillRecordedBeforeTheCostFieldsIsRejected(t *testing.T) {
	t.Parallel()

	payload := validFill()
	payload.Level = 0
	payload.SlippageApplied = 0
	payload.Commission = 0

	err := payload.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want an error for a fill carrying no level")
	}
	if !strings.Contains(err.Error(), "level") {
		t.Errorf("Validate() error = %v, want it to name the level", err)
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
	if event.FillSchemaVersion != 4 {
		t.Errorf("FillSchemaVersion = %d, want 4 (#12 added Kind and CampaignID; #13 and #14 each added a further Kind value without a further bump; #15 added UnitIDs, required for a stop fill; #18 added Level, SlippageApplied and Commission)", event.FillSchemaVersion)
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
	if event.FillKindAdd != "add" {
		t.Errorf("FillKindAdd = %q, want %q", event.FillKindAdd, "add")
	}
}

// validAddFill returns #14's fourth Kind: an add fill adding a further Unit
// to the same Campaign as validStopFill/validExitFill, referencing the add
// proposal it executes (Kind requires BOTH CampaignID and ProposalID for an
// add — like an exit, unlike a stop, it always has a specific outstanding
// proposal to join back to).
func validAddFill() event.FillPayload {
	return event.FillPayload{
		InstrumentID:    "AAPL",
		Kind:            event.FillKindAdd,
		CampaignID:      "campaign:AAPL:2026-02-27T00:00:00.000000000Z",
		ProposalID:      "add-proposal-unit-2:AAPL:2026-03-05T00:00:00.000000000Z",
		FillID:          "sim-fill-0004",
		Direction:       event.DirectionLong,
		Quantity:        133,
		Price:           220.04,
		FilledAt:        proposalPeriodEnd.AddDate(0, 0, 6),
		Level:           219.99,
		SlippageApplied: 0.05,
		Commission:      1.10,
	}
}

// TestAddFillPayloadValidate covers the add-kind pairing rule from
// validAddFill: like an exit fill, and unlike a stop fill, an add fill
// requires BOTH CampaignID and ProposalID.
func TestAddFillPayloadValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*event.FillPayload)
		wantErr string
	}{
		{name: "valid add fill"},
		{
			name:    "missing campaign id",
			mutate:  func(f *event.FillPayload) { f.CampaignID = "" },
			wantErr: "campaign id",
		},
		{
			// Unlike a stop fill, an add fill's ProposalID is required: it is
			// the join back to the add proposal (strategy.add.proposed) this
			// fill executes.
			name:    "missing proposal id",
			mutate:  func(f *event.FillPayload) { f.ProposalID = "" },
			wantErr: "proposal id",
		},
		{
			name:    "add fill names unit ids",
			mutate:  func(f *event.FillPayload) { f.UnitIDs = []string{"some-unit"} },
			wantErr: "unit ids must be empty for an add fill",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := validAddFill()
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

func TestAddFillPayloadJSONTags(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validAddFill())
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got, ok := asMap["kind"]; !ok || got != "add" {
		t.Errorf("encoded payload kind = %v, want %q: %s", got, "add", encoded)
	}
	if got, ok := asMap["campaign_id"]; !ok || got == "" {
		t.Errorf("encoded payload campaign_id = %v, want it populated: %s", got, encoded)
	}
	if got, ok := asMap["proposal_id"]; !ok || got == "" {
		t.Errorf("encoded payload proposal_id = %v, want it populated: %s", got, encoded)
	}
}

func TestAddFillPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := validAddFill()

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
		"level",
		"slippage_applied",
		"commission",
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
	if got, ok := asMap["unit_ids"]; !ok {
		t.Errorf("encoded payload missing expected key %q: %s", "unit_ids", encoded)
	} else if arr, ok := got.([]any); !ok || len(arr) == 0 {
		t.Errorf("encoded payload unit_ids = %v, want it populated: %s", got, encoded)
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
