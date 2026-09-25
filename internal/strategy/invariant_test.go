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
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/indicator"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
)

// invariantTestStrategyVersion/ConfigurationPayload mirror
// campaign_test.go's testStrategyVersion/validConfigurationPayload,
// duplicated here rather than imported: this file is `package strategy`, a
// different package from `strategy_test` where those live.
const invariantTestStrategyVersion = "invariant-test-1.0.0"

// invariantTestConfigurationPayload is a valid configuration payload used
// only to construct a Reducer for this file's tests — #50/ADR 0016 makes
// NewReducer derive its configuration hash from the payload (and validate
// it), so this file needs one even though the invariant under test never
// reads it.
func invariantTestConfigurationPayload() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "invariant-test",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		// ADR 0008's other three Unit caps, generous here since this fixture
		// is not testing them: only the per-instrument cap above is exercised
		// by name in this file.
		MaxUnitsPerIndustry: 1_000_000,
		MaxUnitsPerSector:   1_000_000,
		MaxUnitsTotalLong:   1_000_000,
		SlippageN:           0.05,
		TierBDistanceInN:    1.0,
		DollarsPerPoint:     1,
		RiskAtStopFraction:  0,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		// #18: ADR 0013's commission model. Nothing in this file reads it,
		// but ConfigurationPayload.Validate requires the cap since schema
		// version 4, so this fixture must state a whole configuration.
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1.00,
			MaximumFractionOfTradeValue: 0.01,
		},
		BuyOrderType: event.OrderTypeStopLimit,
		GapBufferN:   1,
	}
}

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
	r, err := NewReducer(invariantTestStrategyVersion, invariantTestConfigurationPayload())
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	r.configured = true
	r.entryChannelLength = 55
	r.exitChannelLength = 20
	r.tierBDistanceInN = 1.0
	// #15: evaluateCampaign's per-bar event now reports AggregateOpenRisk,
	// which requires a usable dollarsPerPoint to derive (sizing.AggregateOpenRisk
	// fails closed on a non-positive one) — this fixture's bar reaches that
	// path (TestCampaignWithAValidProtectiveStopDoesNotHalt), so it must be
	// set here even though this file's own point is the Protective Stop
	// invariant, not sizing.
	r.dollarsPerPoint = 1
	// ADR 0008's group and total-long caps, generous here since this file's
	// fixtures build Campaign state by hand and are not testing the caps
	// themselves (unit_caps_test.go is): a zero default would make every
	// fixture's single Campaign read as exceeding an unconfigured
	// zero-Unit cap.
	r.maxUnits = 4
	r.maxUnitsPerIndustry = 1_000_000
	r.maxUnitsPerSector = 1_000_000
	r.maxUnitsTotalLong = 1_000_000
	// ADR 0005 and ADR 0020, as amended 2026-09-24: the Baseline's
	// stop-limit order and the costs its hold reserves.
	r.buyOrderType = event.OrderTypeStopLimit
	r.gapBufferN = 1
	r.slippageN = 0.05
	r.commission = sizing.CommissionSchedule{PerShare: 0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: 0.01}
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
// checked). High 101 is safe for the halt-path tests, none of which reach
// evaluateCampaign at all: see invariantTestBarEnvelopeAt for the two
// "does not halt" tests, which must not let their high clear an Add Ladder
// rung.
func invariantTestBarEnvelope(t *testing.T, sequence uint64, instrumentID string, periodEnd time.Time) event.Envelope {
	t.Helper()
	return invariantTestBarEnvelopeAt(t, sequence, instrumentID, periodEnd, 101)
}

// invariantTestBarEnvelopeAt is invariantTestBarEnvelope with an explicit
// high. buildCorruptedCampaignState freezes fillPrice 100 and campaignN 1,
// so the Campaign's next Add Ladder rung (sizing.NextAddLevel) is 100.5:
// TestCampaignWithAValidProtectiveStopDoesNotHalt and
// TestCampaignWithAStopAtOrAboveEntryDoesNotHalt proceed far enough to
// evaluate that open Campaign (unlike the halt-path tests, which stop
// before doing so), so their own bar must stay AT OR BELOW the rung — high
// 100, not 101 — so their exact single Campaign-evaluated assertion holds
// because no Add opportunity was reached, not merely because the reducer
// happens not to evaluate Adds at bar time today (ADR 0021: Adds are
// decided at Session close, and this fixture sends no session close at
// all). Keeping the fixture itself unreachable-by-construction is what
// makes the assertion robust to that detail changing.
func invariantTestBarEnvelopeAt(t *testing.T, sequence uint64, instrumentID string, periodEnd time.Time, high float64) event.Envelope {
	t.Helper()
	bar := event.CompletedBarPayload{
		InstrumentID: instrumentID,
		PeriodEnd:    periodEnd,
		SplitAdjusted: event.PriceView{
			View: event.ViewSplitAdjusted, Open: 100, High: high, Low: 99, Close: 100, Volume: 1_000_000,
		},
		Raw: event.PriceView{
			View: event.ViewRaw, Open: 100, High: high, Low: 99, Close: 100, Volume: 1_000_000,
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
		ConfigurationHash: event.ConfigurationHash(invariantTestConfigurationPayload()),
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
					// Recorded as every Unit's Exit Order is in the Apply
					// that sets its stop (exit_order.go), so a bar that
					// moves no level emits none here either.
					exitOrderLevel:  protectiveStop,
					exitOrderSource: event.ExitOrderSourceProtectiveStop,
				}},
			},
		},
	}
	return instrumentID
}

// TestCampaignWithoutAProtectiveStopHaltsTheEngine enforces CONTEXT.md's
// Protective Stop invariant: a Campaign found at the start of a bar without
// a Protective Stop that is positive, halts the engine — emitting
// event.EngineStateEventType and failing the run — rather than being
// silently tolerated or continuing to trade the instrument.
//
// Covers every way the invariant can fail except a positive but infinite
// stop, which TestCampaignWithAPositiveInfiniteProtectiveStopHaltsTheEngine
// covers on its own (checking only `protectiveStop > 0` would fail to
// reject +Inf). Not reachable from any valid input stream
// (see this file's package doc comment); constructed directly through
// buildCorruptedCampaignState.
//
// Requiring a held stop to remain below entry would reject CONTEXT.md's
// risk-free Unit: a stop RAISED by the Stop Ladder can legitimately reach
// or exceed its Unit's entry under a narrow enough Stop Multiple. Those
// break-even or profit-protecting levels must not halt the engine — see
// TestCampaignWithAStopAtOrAboveEntryDoesNotHalt below.
func TestCampaignWithoutAProtectiveStopHaltsTheEngine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		protectiveStop float64
	}{
		{name: "stop at zero", protectiveStop: 0},
		{name: "stop negative", protectiveStop: -5},
		{name: "stop is NaN", protectiveStop: math.NaN()},
		{name: "stop is negative infinity", protectiveStop: math.Inf(-1)},
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

// TestCampaignWithAPositiveInfiniteProtectiveStopHaltsTheEngine rejects
// checking only `protectiveStop > 0`, which accepts positive infinity.
// The invariant uses sizing.ValidStopLevel to require a finite, positive
// stop, matching event.CampaignEvaluatedPayload.Validate,
// event.CampaignOpenedPayload.Validate and event.ProtectiveStopSetPayload.Validate.
// A +Inf stop is not a usable protective level and must halt the engine
// rather than pass the capital-safety check.
func TestCampaignWithAPositiveInfiniteProtectiveStopHaltsTheEngine(t *testing.T) {
	t.Parallel()

	r := newConfiguredReducerForInvariantTest(t)
	instrumentID := buildCorruptedCampaignState(t, r, math.Inf(1))

	bar := invariantTestBarEnvelope(t, 1, instrumentID, day(2))
	emissions, err := r.Apply(context.Background(), bar)

	if err == nil {
		t.Fatal("Apply() error = nil, want the capital-safety invariant to halt the run for a +Inf protective stop")
	}
	if !strings.Contains(err.Error(), "capital-safety invariant violated") {
		t.Errorf("Apply() error = %v, want it to name the capital-safety invariant", err)
	}
	if len(emissions) != 1 || emissions[0].Type != event.EngineStateEventType {
		t.Fatalf("emissions = %+v, want exactly 1 engine-state halt", emissions)
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

	bar := invariantTestBarEnvelopeAt(t, 1, instrumentID, day(2), 100)
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

// TestCampaignWithAStopAtOrAboveEntryDoesNotHalt rejects treating a risk-free
// Unit (CONTEXT.md) as corrupt: a stop AT
// or ABOVE its own Unit's entry — reachable in practice only via the Stop
// Ladder's repeated raises under a narrow enough Stop Multiple (Variant
// territory; the Baseline's own 2N/four-Unit configuration never reaches
// it) — is a legitimate break-even or profit-protecting level, not the
// corrupted state checkCampaignHasAProtectiveStop exists to catch, and must
// not halt the engine. Also confirms the per-bar Campaign-evaluated event
// still validates cleanly and reports this Unit's own contribution to
// AggregateOpenRisk as exactly zero (sizing.AggregateOpenRisk's own
// max(0, entry-stop) rule).
func TestCampaignWithAStopAtOrAboveEntryDoesNotHalt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		protectiveStop float64
	}{
		{name: "stop equal to entry price", protectiveStop: 100},
		{name: "stop above entry price", protectiveStop: 150},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newConfiguredReducerForInvariantTest(t)
			instrumentID := buildCorruptedCampaignState(t, r, tt.protectiveStop)

			bar := invariantTestBarEnvelopeAt(t, 1, instrumentID, day(2), 100)
			emissions, err := r.Apply(context.Background(), bar)
			if err != nil {
				t.Fatalf("Apply() error = %v, want nil: a stop at or above entry is a legitimate risk-free position, not an invariant violation", err)
			}
			if len(emissions) != 1 {
				t.Fatalf("got %d emission(s), want exactly 1 (the bar's own Campaign-evaluated event)", len(emissions))
			}
			var payload event.CampaignEvaluatedPayload
			if err := json.Unmarshal(emissions[0].Payload, &payload); err != nil {
				t.Fatalf("decode campaign evaluated payload: %v", err)
			}
			if err := payload.Validate(); err != nil {
				t.Errorf("emitted campaign evaluated payload fails its own Validate(): %v", err)
			}
			if payload.ProtectiveStop != tt.protectiveStop {
				t.Errorf("ProtectiveStop = %v, want the fixture's %v", payload.ProtectiveStop, tt.protectiveStop)
			}
			if payload.AggregateOpenRisk != 0 {
				t.Errorf("AggregateOpenRisk = %v, want exactly 0: a unit whose stop is at or above its own entry has zero downside risk", payload.AggregateOpenRisk)
			}
		})
	}
}

// TestCampaignWithoutAProtectiveStopHaltsTheEngineThroughReplayEngineRun
// rejects discarding the halt emission when Apply also returns an error:
// replay.Engine.Run must return the engine-state halt alongside that error,
// not merely fail closed without journal evidence. This complements the
// direct Handler check in TestCampaignWithoutAProtectiveStopHaltsTheEngine
// and the engine contract pinned by internal/replay's
// TestEngineRunJournalsPriorAndFinalEmissionsAlongsideHandlerError: final
// emissions on failure are stamped, validated, and returned to the caller.
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
