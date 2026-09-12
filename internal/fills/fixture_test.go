package fills_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds the fixtures the event-seam tests share: one instrument,
// one Baseline configuration, and a bar series built so that every number the
// tests assert can be worked out on paper.
//
// # The arithmetic the whole fixture rests on
//
// Every warm-up bar has True Range exactly 1.5 (see warmUpBars), so N is
// exactly 1.5 from the end of warm-up onward — seeded as the 20-bar simple
// average of 1.5 and held there by Wilder's own update, (19 x 1.5 + 1.5)/20,
// which is 1.5 exactly in float64 as well as on paper. That makes:
//
//	slippage            0.05 N       = 0.075
//	Add Ladder rung     +0.5 N       = +0.75 above the previous Unit's FILL
//	Protective Stop     -2 N         = -3 below its own Unit's fill
//	Stop Ladder raise   +0.5 N       = +0.75 per later Unit added
//	Unit quantity       0.005 x 1,000,000 / (1.5 x 1) = 3333.33 -> 3333 shares
//
// The Entry Channel over the 55 warm-up bars tops out at 155.5 and the Exit
// Channel (20 bars) at 135, both stated in warmUpBars' own comment.

const (
	testStrategyVersion   = "test-strategy-1.0.0"
	testConfigurationHash = "cfg-test"
	testInstrument        = "AAPL"

	// fixtureN is what N is worth throughout, by construction.
	fixtureN = 1.5
	// fixtureSlippage is 0.05 N, ADR 0013's Baseline.
	fixtureSlippage = 0.05 * fixtureN
	// fixtureUnitQuantity is the whole Unit every fill in these fixtures
	// executes, frozen at the Campaign's first entry (ADR 0006).
	fixtureUnitQuantity int64 = 3333
)

// baselineConfig is the Baseline (ADRs 0002/0003/0005/0007/0008/0013) with
// the Interactive Brokers commission schedule this ticket declares.
func baselineConfig() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "turtle-baseline",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2.0,
		EntryChannelLength:     55,
		ExitChannelLength:      20,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       1.0,
		DollarsPerPoint:        1,
		RiskAtStopFraction:     0,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1.00,
			MaximumFractionOfTradeValue: 0.01,
		},
	}
}

func day(i int) time.Time {
	return time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
}

// bar builds a completed bar whose split-adjusted and raw views are
// identical. ADR 0004 keeps the two views separate and #38 owns raw-view
// accounting; in slice 1 the fixtures make them the same, so nothing here
// depends on which view is read while the simulator deliberately reads the
// split-adjusted one (the view every level it fills against was computed on).
func bar(periodEnd time.Time, open, high, low, closeAt float64) event.CompletedBarPayload {
	view := func(label string) event.PriceView {
		return event.PriceView{View: label, Open: open, High: high, Low: low, Close: closeAt, Volume: 1_000_000}
	}
	return event.CompletedBarPayload{
		InstrumentID:  testInstrument,
		PeriodEnd:     periodEnd,
		SplitAdjusted: view(event.ViewSplitAdjusted),
		Raw:           view(event.ViewRaw),
	}
}

// warmUpBars is bars 1..55: a quiet, steadily rising series.
//
// Bar i opens at 100+i, highs at 100+i+0.5, lows at 100+i-1 and closes at
// 100+i. True Range is therefore max(1.5, 1.5, 0) = 1.5 on every bar
// including the first, so N is exactly 1.5 from warm-up onward.
//
// At the end of the series the Entry Channel (55 bars) stands at 155.5 —
// bar 55's high — and the Exit Channel (20 bars) at 135, bar 36's low.
func warmUpBars() []event.CompletedBarPayload {
	bars := make([]event.CompletedBarPayload, 0, 55)
	for i := 1; i <= 55; i++ {
		base := 100 + float64(i)
		bars = append(bars, bar(day(i), base, base+0.5, base-1, base))
	}
	return bars
}

// breakoutBar is bar 56 in every fixture except the entered-then-stopped one:
// a clean breakout above the 155.5 Entry Channel whose own low (155) stays
// well clear of the Protective Stop the entry fill will set at 154.075.
func breakoutBar() event.CompletedBarPayload {
	return bar(day(56), 155.5, 157, 155, 156.5)
}

// campaignLifeBars is the full-life fixture: warm-up, a breakout, three
// rising bars that add Units 2, 3 and 4 through the Add chain, twenty quiet
// bars that lift the Exit Channel above every Protective Stop, and finally a
// bar that breaks the Exit Channel without reaching any stop.
//
// The twenty quiet bars are what make the Exit-Channel exit reachable at all.
// A Protective Stop sits 2 N below its Unit's fill while the Exit Channel
// tracks the lowest low of the last twenty bars, so early in a Campaign the
// stop is far ABOVE the channel and any bar deep enough to break the channel
// has already taken out every stop on the way down. Only once price has held
// up for a full channel length does the channel climb past the stops and
// become the binding exit. Bar 80 then dips to 157.0: below the 159.3
// channel, above the highest stop at 156.55.
func campaignLifeBars() []event.CompletedBarPayload {
	bars := warmUpBars()
	bars = append(bars,
		breakoutBar(),
		// Unit 2: rung 157.825 (Unit 1 filled at 157.075).
		bar(day(57), 157.2, 158.2, 157, 158),
		// Unit 3: rung 158.65 (Unit 2 filled at 157.9).
		bar(day(58), 158.3, 159.0, 158.2, 158.8),
		// Unit 4: rung 159.475 (Unit 3 filled at 158.725). No fifth Unit is
		// ever proposed (ADR 0008, MaxUnits 4).
		bar(day(59), 159.0, 159.8, 158.9, 159.6),
	)
	for k := 60; k <= 79; k++ {
		open := 159.5 + float64(k-60)*0.2
		bars = append(bars, bar(day(k), open, open+0.3, open-0.2, open+0.15))
	}
	// The Exit Channel now stands at 159.3 (bar 60's low). This bar's low
	// breaks it without reaching the highest Protective Stop (156.55).
	bars = append(bars, bar(day(80), 163.0, 163.1, 157.0, 158.0))
	return bars
}

// --- driving the composed simulator and reducer --------------------------

// composed is one run of the backtest loop: a fresh simulator and a fresh
// reducer, driven bar by bar through fills.RunBar, accumulating the input
// stream the loop produced and every decision the reducer emitted.
type composed struct {
	Inputs    []event.Envelope
	Decisions []event.Envelope
}

func runComposed(t *testing.T, cfg event.ConfigurationPayload, bars []event.CompletedBarPayload) composed {
	t.Helper()
	simulator, reducer := newComposed(t, cfg)
	return driveComposed(t, simulator, reducer, cfg, bars)
}

// newComposed builds the pair the loop drives: a simulator and a reducer,
// configured identically and with the same provenance, exactly as #19's
// driver will compose them.
func newComposed(t *testing.T, cfg event.ConfigurationPayload) (*fills.Simulator, *strategy.Reducer) {
	t.Helper()
	simulator, err := fills.New(cfg, testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("fills.New() error = %v", err)
	}
	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	return simulator, reducer
}

func driveComposed(t *testing.T, simulator *fills.Simulator, reducer *strategy.Reducer, cfg event.ConfigurationPayload, bars []event.CompletedBarPayload) composed {
	t.Helper()

	ctx := context.Background()
	var out composed

	result, err := fills.Deliver(ctx, simulator, reducer, configurationEnvelope(t, cfg))
	if err != nil {
		t.Fatalf("Deliver(configuration) error = %v", err)
	}
	out.Inputs = append(out.Inputs, result.Inputs...)
	out.Decisions = append(out.Decisions, result.Decisions...)

	for i, b := range bars {
		result, err := fills.RunBar(ctx, simulator, reducer, barEnvelope(t, b))
		if err != nil {
			t.Fatalf("RunBar(bar %d, period end %s) error = %v", i+1, b.PeriodEnd.Format(time.RFC3339), err)
		}
		out.Inputs = append(out.Inputs, result.Inputs...)
		out.Decisions = append(out.Decisions, result.Decisions...)
	}
	return out
}

func configurationEnvelope(t *testing.T, cfg event.ConfigurationPayload) event.Envelope {
	t.Helper()
	return envelope(t, "cfg-1", event.ConfigurationEventType, event.ConfigurationSchemaVersion, day(0), cfg)
}

func barEnvelope(t *testing.T, b event.CompletedBarPayload) event.Envelope {
	t.Helper()
	return envelope(t, "bar:"+b.PeriodEnd.Format(time.RFC3339), event.CompletedBarEventType, event.CompletedBarSchemaVersion, b.PeriodEnd, b)
}

// envelope builds an input envelope with a deliberately non-simulator Source:
// bars and configuration come from the data producer (#27), never from the
// fill simulator. Sequence is left at zero — RunBar and Deliver stamp the
// composed input stream's own contiguous sequence.
func envelope(t *testing.T, id, eventType string, schemaVersion uint32, at time.Time, payload any) event.Envelope {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return event.Envelope{
		ID:                id,
		Type:              eventType,
		SchemaVersion:     schemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

// --- a resting entry order, without the reducer ---------------------------

// restingEntryProposal is a trade proposal envelope a test can Observe
// directly, for the cases this reducer cannot produce — an entry order left
// resting into a later bar. Every figure is the fixture's own, so the
// proposal is exactly what the reducer would have raised had its entry level
// been level rather than the breakout bar's own high.
func restingEntryProposal(t *testing.T, level float64) event.Envelope {
	t.Helper()
	proposal := event.TradeProposalPayload{
		InstrumentID:           testInstrument,
		PeriodEnd:              day(56),
		SignalID:               "signal:AAPL:day-56",
		Rule:                   event.RuleUnitSizingVolatilityNormalised,
		ADR:                    event.ADRUnitSizing,
		Direction:              event.DirectionLong,
		EntryLevel:             level,
		Quantity:               fixtureUnitQuantity,
		N:                      fixtureN,
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		RiskAtStop:             0.005 * 2,
		RealisedRiskAtStop:     float64(fixtureUnitQuantity) * 2 * fixtureN * 1 / 1_000_000,
		DollarsPerPoint:        1,
		NotionalAccount:        1_000_000,
		ProtectiveStopIntent:   level - 2*fixtureN,
	}
	if err := proposal.Validate(); err != nil {
		t.Fatalf("the fixture proposal is invalid: %v", err)
	}
	return envelope(t, "proposal:AAPL:day-56", event.TradeProposalEventType, event.TradeProposalSchemaVersion, day(56), proposal)
}

// openCampaignOnEntryFill is a stand-in for the reducer that answers an entry
// fill the way the reducer does: with a Campaign-opened decision. The
// simulator needs that answer, because an entry order leaves its book only
// when a Campaign it opened is journalled — nothing in this package decides
// that a proposal has been executed.
//
// It is deliberately the minimum that behaves correctly at that seam, and its
// payload is validated like any other, so it cannot drift into being a double
// that accepts what the real reducer would refuse.
func openCampaignOnEntryFill(t *testing.T, proposalID string) replay.Handler {
	t.Helper()
	return replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type != event.FillEventType {
			return nil, nil
		}
		var fill event.FillPayload
		decodeInto(t, in, &fill)
		if fill.Kind != event.FillKindEntry {
			return nil, nil
		}
		campaignID := "campaign:" + testInstrument + ":" + fill.FilledAt.UTC().Format(time.RFC3339Nano)
		opened := event.CampaignOpenedPayload{
			CampaignID:     campaignID,
			InstrumentID:   fill.InstrumentID,
			ProposalID:     proposalID,
			SignalID:       "signal:AAPL:day-56",
			FillID:         fill.FillID,
			Rule:           event.RuleCampaignOpenedFromFill,
			ADR:            event.ADRCampaignFrozenAtEntry,
			Direction:      fill.Direction,
			CampaignN:      fixtureN,
			UnitQuantity:   fixtureUnitQuantity,
			FilledQuantity: fill.Quantity,
			EntryPrice:     fill.Price,
			StopMultiple:   2,
			ProtectiveStop: fill.Price - 2*fixtureN,
			Units:          1,
			OpenedAt:       fill.FilledAt,
		}
		if err := opened.Validate(); err != nil {
			t.Fatalf("the stand-in reducer built an invalid campaign-opened payload: %v", err)
		}
		return []event.Envelope{envelope(t, campaignID, event.CampaignOpenedEventType, event.CampaignOpenedSchemaVersion, fill.FilledAt, opened)}, nil
	})
}

// --- reading what came out ------------------------------------------------

func envelopesOfType(envelopes []event.Envelope, eventType string) []event.Envelope {
	var matched []event.Envelope
	for _, e := range envelopes {
		if e.Type == eventType {
			matched = append(matched, e)
		}
	}
	return matched
}

// fillPayloads decodes every fill the simulator produced, in the order it
// produced them. The ORDER is load-bearing in several tests: ADR 0005 rule 3
// is a statement about which fill happened first within a bar.
func fillPayloads(t *testing.T, inputs []event.Envelope) []event.FillPayload {
	t.Helper()
	var out []event.FillPayload
	for _, e := range envelopesOfType(inputs, event.FillEventType) {
		out = append(out, decodeFill(t, e))
	}
	return out
}

func decodeFill(t *testing.T, e event.Envelope) event.FillPayload {
	t.Helper()
	var payload event.FillPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(fill) error = %v", err)
	}
	return payload
}

func decodeInto(t *testing.T, e event.Envelope, into any) {
	t.Helper()
	if err := json.Unmarshal(e.Payload, into); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", e.Type, err)
	}
}

// onlyOfType returns the single envelope of eventType, failing if there is
// not exactly one.
func onlyOfType(t *testing.T, envelopes []event.Envelope, eventType string) event.Envelope {
	t.Helper()
	matched := envelopesOfType(envelopes, eventType)
	if len(matched) != 1 {
		t.Fatalf("got %d %s event(s), want exactly 1", len(matched), eventType)
	}
	return matched[0]
}

// closeTo compares two prices with a tolerance, never with ==
// (.greptile/rules.md: "never compare prices for equality with ==").
func closeTo(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff <= 1e-9
}

func assertPrice(t *testing.T, what string, got, want float64) {
	t.Helper()
	if !closeTo(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// replayThrough runs inputs through a fresh reducer and replay.Engine, which
// is how a journal is replayed (#20). It is deliberately NOT the composed
// loop: the point of every test that uses it is that the input stream the
// loop produced stands on its own.
func replayThrough(t *testing.T, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	reducer, err := strategy.NewReducer(testStrategyVersion, testConfigurationHash)
	if err != nil {
		t.Fatalf("strategy.NewReducer() error = %v", err)
	}
	engine, err := replay.New(reducer)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}

func describe(envelopes []event.Envelope) string {
	out := ""
	for i, e := range envelopes {
		out += fmt.Sprintf("\n  %2d %-36s %s", i, e.Type, e.ID)
	}
	return out
}
