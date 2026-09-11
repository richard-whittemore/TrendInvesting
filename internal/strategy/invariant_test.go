package strategy

// This file is deliberately `package strategy`, not `strategy_test`: it is
// the "test-only path" #12's ticket brief asks for to exercise
// checkCampaignHasAProtectiveStop, and it reaches that path by constructing
// an impossible campaignState directly through this package's own unexported
// fields, never through any exported production API. No production code is
// weakened or given a new exported hook to make this reachable — the
// invariant it tests is unreachable from any valid input stream, which is
// exactly the point (see checkCampaignHasAProtectiveStop's doc comment):
// openCampaign is the only place state.campaign is ever assigned in the
// whole package, and it never runs without a protectiveStop that has already
// passed sizing.ProtectiveStopLevel's and event.CampaignOpenedPayload's own
// checks. A white-box test corrupting memory by hand is therefore the only
// way to reach the halt at all.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// invariantTestStrategyVersion/ConfigurationHash mirror
// campaign_test.go's testStrategyVersion/testConfigurationHash, duplicated
// here rather than imported: this file is `package strategy`, a different
// package from `strategy_test` where those constants live.
const (
	invariantTestStrategyVersion   = "invariant-test-1.0.0"
	invariantTestConfigurationHash = "invariant-test-cfg"
)

// day mirrors reducer_test.go's day helper (package strategy_test), which
// this file cannot import since it lives in package strategy itself.
func day(i int) time.Time {
	return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
}

// newConfiguredReducerForInvariantTest returns a Reducer already past
// applyConfiguration, with entryChannelLength and tierBDistanceInN set to
// harmless values: the invariant check under test runs before any of that
// state is read, but stateFor still needs entryChannelLength to build a
// fresh indicator.EntryChannel if this test ever grows a case that reaches
// it.
func newConfiguredReducerForInvariantTest(t *testing.T) *Reducer {
	t.Helper()
	r, err := NewReducer(invariantTestStrategyVersion, invariantTestConfigurationHash)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	r.configured = true
	r.entryChannelLength = 55
	r.exitChannelLength = 20
	r.tierBDistanceInN = 1.0
	notionalAccount, err := NewNotionalAccount(1_000_000, 1, 1)
	if err != nil {
		t.Fatalf("NewNotionalAccount() error = %v", err)
	}
	r.notionalAccount = notionalAccount
	return r
}

// invariantTestBarEnvelope builds a minimal, valid completed-bar envelope
// for instrumentID at periodEnd, with a bar shape harmless enough that if
// the invariant check under test did NOT stop the run, the rest of
// applyCompletedBar would proceed without erroring for an unrelated reason
// (which would make a passing test ambiguous about what it actually
// checked).
func invariantTestBarEnvelope(t *testing.T, sequence uint64, instrumentID string, periodEnd time.Time) event.Envelope {
	t.Helper()
	bar := event.CompletedBarPayload{
		InstrumentID: instrumentID,
		PeriodEnd:    periodEnd,
		SplitAdjusted: event.PriceView{
			View: event.ViewSplitAdjusted, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000,
		},
		Raw: event.PriceView{
			View: event.ViewRaw, Open: 100, High: 101, Low: 99, Close: 100, Volume: 1_000_000,
		},
	}
	payload, err := json.Marshal(bar)
	if err != nil {
		t.Fatalf("json.Marshal(bar) error = %v", err)
	}
	return event.Envelope{
		ID:                "invariant-bar",
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        periodEnd,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   invariantTestStrategyVersion,
		ConfigurationHash: invariantTestConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// buildCorruptedCampaignState installs, directly into r's unexported
// instruments map, an instrument state whose Campaign carries the given
// (invalid) protectiveStop against a fixed entryPrice of 100 — the
// impossible state checkCampaignHasAProtectiveStop exists to catch. It
// returns the instrument id used, so a caller does not have to hard-code it
// twice.
func buildCorruptedCampaignState(t *testing.T, r *Reducer, protectiveStop float64) string {
	t.Helper()
	const instrumentID = "AAPL"

	n, err := indicator.NewWilderAverage(indicator.DefaultPeriod)
	if err != nil {
		t.Fatalf("indicator.NewWilderAverage() error = %v", err)
	}
	entryChannel, err := indicator.NewEntryChannel(r.entryChannelLength)
	if err != nil {
		t.Fatalf("indicator.NewEntryChannel() error = %v", err)
	}
	// #13: every instrumentState now carries an Exit Channel too, fed
	// alongside the Entry Channel regardless of Campaign state. A fresh,
	// empty one here (never Added to) is deliberately NOT ready — the point
	// of this file's fixture is the Protective Stop invariant, not the Exit
	// Channel, so TestCampaignWithAValidProtectiveStopDoesNotHalt's bar must
	// report ExitChannelReady false rather than a fabricated level.
	exitChannel, err := indicator.NewExitChannel(r.exitChannelLength)
	if err != nil {
		t.Fatalf("indicator.NewExitChannel() error = %v", err)
	}

	r.instruments = map[string]*instrumentState{
		instrumentID: {
			n:             n,
			entryChannel:  entryChannel,
			exitChannel:   exitChannel,
			lastPeriodEnd: day(1),
			campaign: &campaignState{
				campaignID:   "campaign:AAPL:corrupted",
				instrumentID: instrumentID,
				proposalID:   "proposal:AAPL:corrupted",
				signalID:     "signal:AAPL:corrupted",
				direction:    event.DirectionLong,
				campaignN:    1,
				unitQuantity: 1,
				stopMultiple: 2,
				// #14: maxUnits 1, not 4 — this fixture is already at its
				// (deliberately tiny) maximum, so evaluateAdd proposes
				// nothing regardless of the fixture bar's high. This file's
				// point is the Protective Stop invariant, not the Add
				// Ladder; TestCampaignWithAValidProtectiveStopDoesNotHalt's
				// single expected emission would otherwise pick up a
				// legitimate (but unrelated) Add proposal.
				maxUnits: 1,
				openedAt: day(1),
				units: []unitState{{
					index:          1,
					openingFillID:  "sim-fill-corrupted",
					fillPrice:      100,
					quantity:       1,
					protectiveStop: protectiveStop,
					filledAt:       day(1),
				}},
			},
		},
	}
	return instrumentID
}

// TestCampaignWithoutAProtectiveStopHaltsTheEngine is #12's required
// invariant test: a Campaign found, at the start of a completed bar, without
// a Protective Stop that is positive and below its entry price, halts the
// engine — emitting event.EngineStateEventType and failing the run — rather
// than being silently tolerated or continuing to trade the instrument.
//
// Both ways the invariant can fail are covered: a stop at or below zero, and
// a stop at or above the entry price. Neither is reachable from any valid
// input stream (see this file's package doc comment); both are constructed
// directly through buildCorruptedCampaignState.
func TestCampaignWithoutAProtectiveStopHaltsTheEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		protectiveStop float64
	}{
		{name: "stop at zero", protectiveStop: 0},
		{name: "stop negative", protectiveStop: -5},
		{name: "stop equal to entry price", protectiveStop: 100},
		{name: "stop above entry price", protectiveStop: 150},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newConfiguredReducerForInvariantTest(t)
			instrumentID := buildCorruptedCampaignState(t, r, tt.protectiveStop)

			bar := invariantTestBarEnvelope(t, 1, instrumentID, day(2))
			emissions, err := r.Apply(context.Background(), bar)

			if err == nil {
				t.Fatal("Apply() error = nil, want the capital-safety invariant to halt the run")
			}
			if !strings.Contains(err.Error(), "capital-safety invariant violated") {
				t.Errorf("Apply() error = %v, want it to name the capital-safety invariant", err)
			}
			if !strings.Contains(err.Error(), instrumentID) {
				t.Errorf("Apply() error = %v, want it to name the instrument %q", err, instrumentID)
			}

			if len(emissions) != 1 {
				t.Fatalf("got %d emission(s), want exactly 1 (the engine-state halt), even though Apply also returned an error", len(emissions))
			}
			halt := emissions[0]
			if halt.Type != event.EngineStateEventType {
				t.Fatalf("emission Type = %q, want %q", halt.Type, event.EngineStateEventType)
			}
			if halt.SchemaVersion != event.EngineStateSchemaVersion {
				t.Errorf("emission SchemaVersion = %d, want %d", halt.SchemaVersion, event.EngineStateSchemaVersion)
			}
			if halt.Source != sourceReducer {
				t.Errorf("emission Source = %q, want %q", halt.Source, sourceReducer)
			}
			if halt.PayloadHash != event.HashPayload(halt.Payload) {
				t.Error("emission PayloadHash does not attest its own payload")
			}

			var payload event.EngineStatePayload
			if err := json.Unmarshal(halt.Payload, &payload); err != nil {
				t.Fatalf("decode engine state payload: %v", err)
			}
			if payload.State != event.EngineStateHalted {
				t.Errorf("State = %q, want %q", payload.State, event.EngineStateHalted)
			}
			if payload.Reason != event.EngineStateReasonCampaignWithoutProtectiveStop {
				t.Errorf("Reason = %q, want %q", payload.Reason, event.EngineStateReasonCampaignWithoutProtectiveStop)
			}
			if payload.Detail == "" {
				t.Error("Detail is empty, want the figures behind the halt")
			}
			if err := payload.Validate(); err != nil {
				t.Errorf("emitted engine state payload fails its own Validate(): %v", err)
			}
		})
	}
}

// TestCampaignWithAValidProtectiveStopDoesNotHalt is the sanity check
// alongside the table above: the SAME corrupted-state machinery, given a
// legitimate stop (positive, below entry), must not halt at all. Without
// this, a defect that halted on every open Campaign regardless of its stop
// would still pass the table above.
func TestCampaignWithAValidProtectiveStopDoesNotHalt(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	instrumentID := buildCorruptedCampaignState(t, r, 75) // below the fixture's entryPrice of 100

	bar := invariantTestBarEnvelope(t, 1, instrumentID, day(2))
	emissions, err := r.Apply(context.Background(), bar)
	if err != nil {
		t.Fatalf("Apply() error = %v, want nil for a campaign with a legitimate protective stop", err)
	}
	// The instrument is in a Campaign, so CONTEXT.md's Setup gate still
	// suppresses the Setup/Signal/proposal path entirely — but #13 gives
	// every Campaign bar its own Campaign-evaluated event, reporting the
	// level(s) in force. The fixture's Exit Channel was never warmed up
	// (buildCorruptedCampaignState's doc comment), so it must report NOT
	// ready rather than fabricate a level, and propose no exit.
	if len(emissions) != 1 {
		t.Fatalf("got %d emission(s), want exactly 1 (the bar's own Campaign-evaluated event, #13)", len(emissions))
	}
	evaluated := emissions[0]
	if evaluated.Type != event.CampaignEvaluatedEventType {
		t.Fatalf("emission Type = %q, want %q", evaluated.Type, event.CampaignEvaluatedEventType)
	}
	var payload event.CampaignEvaluatedPayload
	if err := json.Unmarshal(evaluated.Payload, &payload); err != nil {
		t.Fatalf("decode campaign evaluated payload: %v", err)
	}
	if payload.ExitChannelReady {
		t.Error("ExitChannelReady = true, want false: this fixture's exit channel was never fed a single bar")
	}
	if payload.ExitChannelLow != 0 {
		t.Errorf("ExitChannelLow = %v, want 0 while not ready", payload.ExitChannelLow)
	}
	if payload.ExitConditionMet {
		t.Error("ExitConditionMet = true, want false")
	}
	if payload.ProtectiveStop != 75 {
		t.Errorf("ProtectiveStop = %v, want the fixture's 75", payload.ProtectiveStop)
	}
}

// TestCampaignWithoutAProtectiveStopHaltsTheEngineThroughReplayEngineRun
// covers the review-round requirement that the halt is observable at the
// seam that actually matters: the journal replay.Engine.Run produces, not
// merely the Handler seam TestCampaignWithoutAProtectiveStopHaltsTheEngine
// exercises directly.
//
// Before this ticket's review round, internal/replay.Engine.Run discarded
// every emission a handler returned whenever Apply also returned an error,
// so the engine-state halt this package emits would never reach a caller of
// Run at all — the run would fail closed SILENTLY. The engine's contract
// was changed (internal/replay/engine_test.go's
// TestEngineRunJournalsPriorAndFinalEmissionsAlongsideHandlerError) so that
// a handler's final emission on failure is stamped, validated, and
// returned alongside the error; this test is the corresponding assertion
// from #12's own side of that seam.
func TestCampaignWithoutAProtectiveStopHaltsTheEngineThroughReplayEngineRun(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	instrumentID := buildCorruptedCampaignState(t, r, -5)

	engine, err := replay.New(r)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}

	bar := invariantTestBarEnvelope(t, 1, instrumentID, day(2))
	emitted, err := engine.Run(context.Background(), []event.Envelope{bar})

	if err == nil {
		t.Fatal("Run() error = nil, want the capital-safety invariant to halt the run")
	}
	if !strings.Contains(err.Error(), "capital-safety invariant violated") {
		t.Errorf("Run() error = %v, want it to name the capital-safety invariant", err)
	}

	if len(emitted) == 0 {
		t.Fatal("got 0 emissions from Run(), want the engine-state halt to be journalled despite the error")
	}
	last := emitted[len(emitted)-1]
	if last.Type != event.EngineStateEventType {
		t.Fatalf("last emission Type = %q, want %q", last.Type, event.EngineStateEventType)
	}
	// Stamped by the engine, exactly like any successful emission: a
	// contiguous output sequence starting at 1, and causation from the bar
	// that triggered it.
	if last.Sequence != 1 {
		t.Errorf("last emission Sequence = %d, want 1 (the engine's own stamped sequence)", last.Sequence)
	}
	if last.CausationID != bar.ID {
		t.Errorf("last emission CausationID = %q, want the bar's id %q", last.CausationID, bar.ID)
	}
	if last.PayloadHash != event.HashPayload(last.Payload) {
		t.Error("last emission PayloadHash does not attest its own payload")
	}

	var payload event.EngineStatePayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("decode engine state payload: %v", err)
	}
	if payload.State != event.EngineStateHalted {
		t.Errorf("State = %q, want %q", payload.State, event.EngineStateHalted)
	}
	if payload.Reason != event.EngineStateReasonCampaignWithoutProtectiveStop {
		t.Errorf("Reason = %q, want %q", payload.Reason, event.EngineStateReasonCampaignWithoutProtectiveStop)
	}
}
