package replay_test

// These tests exercise replay equivalence's three underlying properties
// against a real internal/strategy.Reducer, the concrete Handler it is built
// to protect: replay-twice identity, arrival-time independence, and that a
// Signal never survives its own bar. They live here rather than in
// internal/strategy because they are about what replay.Engine plus a
// Handler guarantee, not about any one strategy rule, and internal/strategy
// has no reason to import internal/replay's test helpers back. Feeding
// events straight to a fresh Reducer, with no fill SIMULATOR in the loop, is
// deliberate: a journal's inputs include fills the simulator produced, so
// this is the reducer's own determinism, which is what replay equivalence
// actually tests (the simulator's determinism is a separate, stronger
// property, exercised end to end in cmd/backtest against the committed
// golden journal). The fills below are hand-authored fixture inputs standing
// in for what that simulator would have delivered — the same discipline
// internal/strategy's own event-seam fixtures (campaign_test.go) already
// follow.
//
// propertyFixture (below) is deliberately richer than a fixture that only
// ever raises and declines Setups: it opens a Campaign, takes an Add, and
// exits it, because a reducer that only ever evaluates Setups never
// exercises the Stop Ladder, the Notional Account or the Add Ladder — the
// code where a wall-clock leak or an ordering dependency does the most
// damage, since that is where state accumulates across bars.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/sizing"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

const propertyFixtureStrategyVersion = "replay-fixture/1.1.0+test"

// propertyCampaignN is N as the fixture's 20 warm-up bars leave it: every
// one of them has a True Range of exactly 2 (flatBar's Open/Close sit at the
// midpoint of High and Low, and each bar's High steps up by 1 from the one
// before, so every gap term but High-Low itself resolves to 0 or 2 — see
// flatBar's own doc comment). Twenty identical True Range values seed
// Wilder's average at exactly that value and hold it there, so N is 2 the
// moment bar 21 is decided. Asserted, not merely assumed: see
// TestThePropertyFixtureOpensACampaignTakesAnAddAndExits.
const propertyCampaignN = 2.0

// propertyEntryFillPrice is what bar 21's proposal actually fills at: above
// the warmed-up Entry Channel high of 120 (bar 20's own High) — the
// direction ADR 0013's slippage always pushes a long entry — and BELOW bar
// 21's own High of 121.5, an ordinary fill inside the bar's range (ADR
// 0005). The Add Ladder below is measured from this price, never from the
// level the proposal named (CONTEXT.md: "Add Ladder"), and kept close to
// bar 21's own High deliberately: were the breakout bar's High to itself
// clear the first Add rung, opening the Campaign would immediately chain
// into proposing an Add from THAT SAME bar (openCampaign's own same-bar
// chain, campaign.go), which is a real and separately-tested behaviour but
// not the one this fixture is for — it would fold the Add into the entry
// bar rather than exercising the reducer's handling of a Campaign already
// open across a later, independent bar.
const propertyEntryFillPrice = 121.0

// propertyFixtureConfiguration is a small, valid Baseline-shaped
// configuration: a 20-bar Entry Channel, matching indicator.DefaultPeriod so
// N and the channel warm up together in the fewest bars.
func propertyFixtureConfiguration() event.ConfigurationPayload {
	return event.ConfigurationPayload{
		StrategyID:             "replay-fixture",
		SizingMode:             event.SizingModeVolatilityNormalised,
		UnitVolatilityFraction: 0.005,
		StopMultiple:           2,
		EntryChannelLength:     20,
		ExitChannelLength:      10,
		MaxUnits:               4,
		SlippageN:              0.05,
		TierBDistanceInN:       1,
		DollarsPerPoint:        1,
		NotionalAccount: event.NotionalAccountConfig{
			StartingEquity: 1_000_000,
			RebasingMonth:  1,
			RebasingDay:    1,
		},
		Commission: event.CommissionConfig{
			PerShare:                    0.005,
			MinimumPerOrder:             1,
			MaximumFractionOfTradeValue: 0.01,
		},
	}
}

func propertyDay(n int) time.Time {
	return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

func mustMarshalT(t *testing.T, v any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%v) error = %v", v, err)
	}
	return encoded
}

func propertyConfigEnvelope(t *testing.T, cfg event.ConfigurationPayload, at, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, cfg)
	return event.Envelope{
		ID:                "cfg-1",
		Type:              event.ConfigurationEventType,
		SchemaVersion:     event.ConfigurationSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        recordedAt,
		Sequence:          1,
		Source:            "fixture",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// propertyAvailableCash is the available-cash figure this file's fixture
// supplies: comfortably clear of any Unit its prices and starting equity can
// size, since these are replay-equivalence properties rather than
// affordability tests (the cash-skip rule's own tests live in
// internal/strategy).
const propertyAvailableCash = 1_000_000_000.0

// propertySnapshotEnvelope is ADR 0010's cash basis, delivered once before
// any bar: the reducer refuses to size a Unit until an account.snapshot has
// supplied an available-cash figure, and its AsOf is at or before every
// decision bar's previous close.
func propertySnapshotEnvelope(t *testing.T, sequence uint64, cfg event.ConfigurationPayload, at, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, event.AccountSnapshotPayload{
		AsOf:          at,
		Equity:        cfg.NotionalAccount.StartingEquity,
		AvailableCash: propertyAvailableCash,
		Currency:      "USD",
	})
	return event.Envelope{
		ID:                "account-snapshot-1",
		Type:              event.AccountSnapshotEventType,
		SchemaVersion:     event.AccountSnapshotSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// flatBar builds a bar whose split-adjusted and raw views are identical,
// with Open and Close both at the midpoint of High and Low so the
// cross-field OHLC checks are satisfied trivially.
func flatBar(instrumentID string, periodEnd time.Time, high, low float64) event.CompletedBarPayload {
	mid := (high + low) / 2
	view := event.PriceView{Open: mid, High: high, Low: low, Close: mid, Volume: 1_000_000}
	split, raw := view, view
	split.View, raw.View = event.ViewSplitAdjusted, event.ViewRaw
	return event.CompletedBarPayload{InstrumentID: instrumentID, PeriodEnd: periodEnd, SplitAdjusted: split, Raw: raw}
}

func propertyBarEnvelope(t *testing.T, sequence uint64, bar event.CompletedBarPayload, cfg event.ConfigurationPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, bar)
	return event.Envelope{
		ID:                fmt.Sprintf("bar-%d", sequence),
		Type:              event.CompletedBarEventType,
		SchemaVersion:     event.CompletedBarSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         bar.PeriodEnd,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// propertyDecisionID mirrors internal/strategy's own (unexported) decisionID
// so this fixture can name a proposal, or the Campaign it opens, before the
// run that produces it — exactly what internal/strategy's own fixtures do
// (testDecisionID, campaign_test.go). Not taken on trust: the assertions
// below decode the actual emitted envelopes and check their IDs against it.
func propertyDecisionID(kind, instrumentID string, periodEnd time.Time) string {
	return fmt.Sprintf("%s:%s:%s", kind, instrumentID, periodEnd.UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

// propertyFillEnvelope wraps a hand-authored fill (this file's stand-in for
// the intraday fill simulator, ADR 0005) in an envelope.
func propertyFillEnvelope(t *testing.T, sequence uint64, fill event.FillPayload, cfg event.ConfigurationPayload, recordedAt time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshalT(t, fill)
	return event.Envelope{
		ID:                fmt.Sprintf("fill-%d", sequence),
		Type:              event.FillEventType,
		SchemaVersion:     event.FillSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         fill.FilledAt,
		RecordedAt:        recordedAt,
		Sequence:          sequence,
		Source:            "fill-simulator",
		StrategyVersion:   propertyFixtureStrategyVersion,
		ConfigurationHash: event.ConfigurationHash(cfg),
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// propertyFixture builds the small property fixture's input stream: 20
// warm-up bars, a breakout that Signals and is filled (opening a Campaign),
// a further bar that reaches the Add Ladder's first rung and is filled
// (adding a Unit), a stop fill that closes the whole Campaign, and — once
// the instrument is a Setup again (CONTEXT.md: "Campaign") — a fresh
// breakout whose proposal is NOT confirmed by the bar after it, so it is
// superseded rather than filled (ADR 0011).
//
// This is the shortest sequence found that reaches Campaign-open, an Add
// and an exit while keeping "a Signal never survives its bar" meaningful:
// the previous version of this fixture only ever raised a Signal and let it
// expire, so it never delivered a fill and never opened a Campaign — leaving
// the Add Ladder, the Stop Ladder and the Notional Account completely
// unexercised by replay equivalence, exactly where state accumulates across
// bars and a wall-clock leak or an ordering dependency does the most damage.
//
// recordedAt computes each envelope's RecordedAt from its EventTime, so a
// caller can shift it independently of EventTime to test arrival-time
// independence.
func propertyFixture(t *testing.T, recordedAt arrivalSchedule) (cfg event.ConfigurationPayload, envelopes []event.Envelope) {
	t.Helper()
	cfg = propertyFixtureConfiguration()
	envelopes = []event.Envelope{
		propertyConfigEnvelope(t, cfg, propertyDay(0), recordedAt(0, propertyDay(0))),
		propertySnapshotEnvelope(t, 2, cfg, propertyDay(0), recordedAt(0, propertyDay(0))),
	}

	seq := uint64(3)
	arrival := 1
	// Each bar is a Session of its own, ended by market.session.closed
	// before any later input (ADR 0021).
	closeSession := func(periodEnd time.Time) {
		payload := mustMarshalT(t, event.SessionClosedPayload{PeriodEnd: periodEnd, InstrumentIDs: []string{"AAPL"}})
		envelopes = append(envelopes, event.Envelope{
			ID:                fmt.Sprintf("session-%d", seq),
			Type:              event.SessionClosedEventType,
			SchemaVersion:     event.SessionClosedSchemaVersion,
			EnvelopeVersion:   event.CurrentEnvelopeVersion,
			EventTime:         periodEnd,
			RecordedAt:        recordedAt(arrival, periodEnd),
			Sequence:          seq,
			Source:            "fixture",
			StrategyVersion:   propertyFixtureStrategyVersion,
			ConfigurationHash: event.ConfigurationHash(cfg),
			PayloadHash:       event.HashPayload(payload),
			Payload:           payload,
		})
		seq++
		arrival++
	}

	for i := 0; i < 20; i++ {
		high := 100 + float64(i+1) // 101..120: channel high after warm-up is 120
		bar := flatBar("AAPL", propertyDay(i+1), high, high-2)
		envelopes = append(envelopes, propertyBarEnvelope(t, seq, bar, cfg, recordedAt(arrival, bar.PeriodEnd)))
		seq++
		arrival++
		closeSession(bar.PeriodEnd)
	}

	// Bar 21: high 121.5 exceeds the warmed-up channel high of 120 — a
	// breakout, a Signal, and (this fixture's prices and starting equity
	// size at least one whole Unit) a trade proposal. Kept just above the
	// channel high, rather than far above it, for propertyEntryFillPrice's
	// own reason: see its doc comment.
	breakout := flatBar("AAPL", propertyDay(21), 121.5, 119.5)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, breakout, cfg, recordedAt(arrival, breakout.PeriodEnd)))
	seq++
	arrival++
	closeSession(breakout.PeriodEnd)

	// Unlike the fixture this replaced, bar 21's proposal IS executed: a
	// one-share partial fill (event.FillPayload's own partial-fill rule)
	// opens the Campaign without this fixture having to reproduce the
	// reducer's own sizing arithmetic to name a "correct" quantity.
	campaignID := propertyDecisionID("campaign", "AAPL", propertyDay(21))
	entryFill := event.FillPayload{
		InstrumentID: "AAPL",
		Kind:         event.FillKindEntry,
		ProposalID:   propertyDecisionID("proposal", "AAPL", propertyDay(21)),
		FillID:       "sim-fill-entry",
		Direction:    event.DirectionLong,
		Quantity:     1,
		Price:        propertyEntryFillPrice,
		Level:        propertyEntryFillPrice,
		FilledAt:     propertyDay(21),
	}
	envelopes = append(envelopes, propertyFillEnvelope(t, seq, entryFill, cfg, recordedAt(arrival, entryFill.FilledAt)))
	seq++
	arrival++

	// Bar 22: the Add Ladder's first rung (The Turtle Rules p.19: half a
	// campaign N above the previous Unit's ACTUAL fill), reached without
	// also breaching the Protective Stop (117: entry - 2N) or the
	// already-warm Exit Channel low (110 over the preceding 10 bars, ADR
	// 0005) — Low stays comfortably above both.
	rung, err := sizing.NextAddLevel(propertyEntryFillPrice, propertyCampaignN, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("sizing.NextAddLevel() error = %v", err)
	}
	// High clears the rung by exactly 1 and no more: enough above rung2 to
	// propose the Add, but (mirroring propertyEntryFillPrice's own reason)
	// still short of rung3 — measured from addFillPrice below — so adding
	// Unit 2 does not itself chain into proposing Unit 3 from this same bar.
	addBar := flatBar("AAPL", propertyDay(22), rung+1, 119)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, addBar, cfg, recordedAt(arrival, addBar.PeriodEnd)))
	seq++
	arrival++
	closeSession(addBar.PeriodEnd)

	addFillPrice := rung + 0.5

	// The Protective Stop in force when the stop fill arrives: the entry's
	// own 2N stop, raised by half an N because a second Unit was added
	// (The Turtle Rules p.23; ADR 0002). Derived through sizing rather
	// than written down, so the fixture cannot drift from the rule.
	entryStop, err := sizing.ProtectiveStopLevel(propertyEntryFillPrice, propertyCampaignN, cfg.StopMultiple, sizing.DirectionLong)
	if err != nil {
		t.Fatalf("sizing.ProtectiveStopLevel() error = %v", err)
	}
	propertyStopLevel, err := sizing.RaisedStop(entryStop, propertyCampaignN)
	if err != nil {
		t.Fatalf("sizing.RaisedStop() error = %v", err)
	}
	// ADR 0013: slippage is 0.05N against the trader, so a long stop fills
	// BELOW its level.
	propertyStopSlippage := cfg.SlippageN * propertyCampaignN
	addFill := event.FillPayload{
		InstrumentID:    "AAPL",
		Kind:            event.FillKindAdd,
		CampaignID:      campaignID,
		ProposalID:      propertyDecisionID("add-proposal-unit-2", "AAPL", propertyDay(22)),
		FillID:          "sim-fill-add",
		Direction:       event.DirectionLong,
		Quantity:        1,
		Price:           addFillPrice,
		Level:           rung,
		SlippageApplied: addFillPrice - rung,
		FilledAt:        propertyDay(22),
	}
	envelopes = append(envelopes, propertyFillEnvelope(t, seq, addFill, cfg, recordedAt(arrival, addFill.FilledAt)))
	seq++
	arrival++

	// The exit: one stop fill closing BOTH Units at once (The Turtle Rules
	// p.19's "all four could be added in one day" allowance, mirrored here
	// for closing) — the shortest way to reach an exit without a further
	// bar to breach the Exit Channel. A stop fill's price is never checked
	// against any level (event.FillPayload's own doc comment), so this
	// fixture's exit needs no further arithmetic.
	stopFill := event.FillPayload{
		InstrumentID:    "AAPL",
		Kind:            event.FillKindStop,
		CampaignID:      campaignID,
		FillID:          "sim-fill-stop",
		UnitIDs:         []string{"sim-fill-entry", "sim-fill-add"},
		Direction:       event.DirectionLong,
		Quantity:        2,
		Price:           propertyStopLevel - propertyStopSlippage,
		Level:           propertyStopLevel,
		SlippageApplied: propertyStopSlippage,
		FilledAt:        propertyDay(22),
	}
	envelopes = append(envelopes, propertyFillEnvelope(t, seq, stopFill, cfg, recordedAt(arrival, stopFill.FilledAt)))
	seq++
	arrival++

	// The Campaign has exited, so AAPL is a Setup again (CONTEXT.md:
	// "Campaign"). Bar 23 is a fresh breakout — its high of 300 clears
	// every high folded into the Entry Channel so far (bar 22's rung+1 is
	// the largest) — raising a second Signal and proposal.
	freshBreakout := flatBar("AAPL", propertyDay(23), 300, 200)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, freshBreakout, cfg, recordedAt(arrival, freshBreakout.PeriodEnd)))
	seq++
	arrival++
	closeSession(freshBreakout.PeriodEnd)

	// Bar 24: high 250 does not exceed the channel high, now 300 (bar 23
	// entered the window) — not a breakout, so the proposal bar 23 raised
	// is superseded rather than confirmed. This is the fixture's "a Signal
	// never survives its bar" case (ADR 0011): no fill is ever delivered
	// for it.
	quiet := flatBar("AAPL", propertyDay(24), 250, 248)
	envelopes = append(envelopes, propertyBarEnvelope(t, seq, quiet, cfg, recordedAt(arrival, quiet.PeriodEnd)))
	seq++
	arrival++
	closeSession(quiet.PeriodEnd)

	return cfg, envelopes
}

// arrivalSchedule decides when the input at index i, carrying eventTime, was
// recorded as arriving.
type arrivalSchedule func(index int, eventTime time.Time) time.Time

// arrivedWhenItHappened is the baseline schedule: RecordedAt tracks EventTime.
func arrivedWhenItHappened(_ int, eventTime time.Time) time.Time { return eventTime }

// arrivedOnADifferentSchedule is the schedule that makes arrival-time
// independence a real test rather than a restatement.
//
// A single constant offset applied to every input preserves every interval
// between consecutive arrivals, so it cannot detect a reducer that reads
// ELAPSED time between events — which is at least as likely a wall-clock
// leak as one reading an absolute instant, and is the reading a uniform
// shift is structurally blind to. The per-index steps below vary, so every
// interval changes and no two change alike.
//
// The offsets are cumulative and strictly increasing, so a monotonic arrival
// stream stays monotonic: the shifted stream is one a real clock could have
// produced, and a failure therefore means the reducer is wrong rather than
// that the input was impossible. Every step is far under the fixture's
// one-day bar spacing, so ordering is never inverted.
func arrivedOnADifferentSchedule(index int, eventTime time.Time) time.Time {
	offset := 37 * time.Minute
	for k := 0; k <= index; k++ {
		offset += time.Duration(k%7+1) * time.Minute
	}
	return eventTime.Add(offset)
}

func runFixture(t *testing.T, cfg event.ConfigurationPayload, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	reducer, err := strategy.NewReducer(propertyFixtureStrategyVersion, cfg)
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

// TestReplayTwiceIsByteIdentical: the headline property. Two freshly
// constructed reducers, given the same input stream, must emit decisions
// that are byte-identical in order — a journal's evidentiary value depends
// on there being no other possible outcome from the same inputs.
func TestReplayTwiceIsByteIdentical(t *testing.T) {
	t.Parallel()

	cfg, inputs := propertyFixture(t, arrivedWhenItHappened)

	first := runFixture(t, cfg, inputs)
	second := runFixture(t, cfg, inputs)

	if d := replay.Equivalent(first, second); d != nil {
		t.Fatalf("two replays of the same inputs diverged at %d:\n want %+v\n got  %+v", d.Index, d.Want, d.Got)
	}
	if len(first) == 0 {
		t.Fatal("the fixture produced no decisions at all; this test would pass vacuously")
	}
}

// leakyCounterHandler is a deliberately wrong Handler: every emission is
// stamped with *counter, incremented on each call. Two instances built with
// counter POINTING AT THE SAME int model state that leaks across what
// should be two independent, freshly constructed reducers (a package-level
// cache or singleton is the realistic shape of this bug); two instances each
// given their OWN int model the correct shape — genuinely fresh construction.
type leakyCounterHandler struct{ counter *int }

func (h *leakyCounterHandler) Apply(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
	*h.counter++
	payload := json.RawMessage(fmt.Sprintf(`{"call_count":%d}`, *h.counter))
	return []event.Envelope{{
		ID:                "count-" + in.ID,
		Type:              "test.counted",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         in.EventTime,
		RecordedAt:        in.RecordedAt,
		Source:            "counting-fixture",
		StrategyVersion:   in.StrategyVersion,
		ConfigurationHash: in.ConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}}, nil
}

func runCountingHandler(t *testing.T, handler *leakyCounterHandler, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	engine, err := replay.New(handler)
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}

// TestTheReplayTwiceComparisonCatchesACounterSharedAcrossFreshInstances is
// the test for the test, for replay-twice identity — mirroring
// TestTheArrivalTimeComparisonCatchesAnIntervalLeak's role for arrival-time
// independence: it proves TestReplayTwiceIsByteIdentical's own comparison
// (replay.Equivalent over two independent runs) actually catches a
// non-deterministic handler, not only that a correct one passes.
func TestTheReplayTwiceComparisonCatchesACounterSharedAcrossFreshInstances(t *testing.T) {
	t.Parallel()

	_, inputs := propertyFixture(t, arrivedWhenItHappened)

	// The defect: two handler instances sharing ONE counter, so the second
	// "fresh" instance's first emission carries a count left over from the
	// first instance's run.
	shared := new(int)
	first := runCountingHandler(t, &leakyCounterHandler{counter: shared}, inputs)
	second := runCountingHandler(t, &leakyCounterHandler{counter: shared}, inputs)
	if d := replay.Equivalent(first, second); d == nil {
		t.Fatal("two handler instances sharing one counter were NOT caught as diverging; the replay-twice comparison is not testing what it claims")
	}

	// The control: two genuinely fresh instances, each with its own counter
	// starting at zero, must be byte-identical — the case a correct reducer
	// (and TestReplayTwiceIsByteIdentical, against the real one) is in.
	third := runCountingHandler(t, &leakyCounterHandler{counter: new(int)}, inputs)
	fourth := runCountingHandler(t, &leakyCounterHandler{counter: new(int)}, inputs)
	if d := replay.Equivalent(third, fourth); d != nil {
		t.Fatalf("two independently-counted runs diverged unexpectedly:\n want %+v\n got  %+v", d.Want, d.Got)
	}
}

// TestArrivalTimeIndependence is the test that catches a wall-clock leak: an
// input stream fed with a different, but still valid, RecordedAt on every
// envelope must produce decisions identical to the original run in every
// field EXCEPT RecordedAt itself — which the reducer deliberately copies
// through from the causing input (internal/strategy.Reducer.stamp) — so a
// leak would show up as a difference somewhere else: a Tier, a price, a
// quantity computed from wall-clock arrival rather than from the ordered
// event stream.
//
// The arrivals are RESCHEDULED, not translated. A single constant offset
// leaves every interval between consecutive arrivals intact and so cannot
// catch a reducer reading elapsed time between events; see
// arrivedOnADifferentSchedule and TestTheArrivalTimeComparisonCatchesAnIntervalLeak,
// which holds that sensitivity in place.
func TestArrivalTimeIndependence(t *testing.T) {
	t.Parallel()

	cfg, original := propertyFixture(t, arrivedWhenItHappened)
	_, shifted := propertyFixture(t, arrivedOnADifferentSchedule)

	assertArrivalsAreRescheduledNotTranslated(t, original, shifted)

	fromOriginal := runFixture(t, cfg, original)
	fromShifted := runFixture(t, cfg, shifted)

	if len(fromOriginal) != len(fromShifted) {
		t.Fatalf("got %d decision(s) from the original arrival times and %d from the shifted ones", len(fromOriginal), len(fromShifted))
	}
	if len(fromOriginal) == 0 {
		t.Fatal("the fixture produced no decisions at all; this test would pass vacuously")
	}

	shiftedByID := make(map[string]time.Time, len(shifted))
	for _, in := range shifted {
		shiftedByID[in.ID] = in.RecordedAt
	}

	for i := range fromOriginal {
		want, got := fromOriginal[i], fromShifted[i]

		// RecordedAt is expected to differ: it tracks the (now shifted)
		// causing input, never a wall clock of the reducer's own. Confirm it
		// really did track the shift, so clearing it below is not
		// vacuously comparing two zero values.
		wantRecordedAt, ok := shiftedByID[got.CausationID]
		if !ok {
			t.Fatalf("decision %d (%s) names causation %q, which is not one of the shifted inputs", i, got.ID, got.CausationID)
		}
		if !got.RecordedAt.Equal(wantRecordedAt) {
			t.Fatalf("decision %d (%s) RecordedAt = %s, want the shifted input's own %s", i, got.ID, got.RecordedAt, wantRecordedAt)
		}
		if got.RecordedAt.Equal(want.RecordedAt) {
			t.Fatalf("decision %d (%s) RecordedAt did not change with the shifted input; this test would not catch a leak that ignored RecordedAt entirely", i, got.ID)
		}

		// Every other field must be untouched by the shift.
		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if d := replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}); d != nil {
			t.Fatalf("decision %d diverged on something other than RecordedAt:\n want %+v\n got  %+v", i, d.Want, d.Got)
		}
	}
}

// assertArrivalsAreRescheduledNotTranslated is the sensitivity guard on the
// arrival-time property: it confirms the two streams differ in the GAPS
// between consecutive arrivals and not merely in their absolute instants. If
// this ever passes vacuously the property test above silently weakens to the
// uniform-shift version, which no interval-reading leak can fail.
func assertArrivalsAreRescheduledNotTranslated(t *testing.T, original, shifted []event.Envelope) {
	t.Helper()

	if len(original) != len(shifted) || len(original) < 2 {
		t.Fatalf("got %d original and %d shifted inputs; need the same count and at least two to have an interval at all", len(original), len(shifted))
	}

	changed := 0
	for i := 1; i < len(original); i++ {
		was := original[i].RecordedAt.Sub(original[i-1].RecordedAt)
		now := shifted[i].RecordedAt.Sub(shifted[i-1].RecordedAt)
		if now != was {
			changed++
		}
		if now < 0 {
			t.Fatalf("arrival %d goes backwards in the shifted stream (%s after %s); a rescheduled stream must still be one a real clock could produce",
				i, shifted[i].RecordedAt, shifted[i-1].RecordedAt)
		}
	}
	if changed == 0 {
		t.Fatal("every interval between consecutive arrivals survived the shift unchanged; this is a translation, not a reschedule, and a reducer reading elapsed time between events would pass")
	}
}

// TestTheArrivalTimeComparisonCatchesAnIntervalLeak is the test for the test.
//
// It runs the arrival-time comparison against a handler that deliberately
// leaks the elapsed time between consecutive arrivals into what it emits, and
// requires the comparison to report a divergence. It then runs the SAME
// handler under a uniform shift and requires that to be missed — which is the
// evidence that a constant offset is structurally blind to this leak, and the
// reason the schedule above varies its steps.
//
// Without this, the property test could only show that a correct reducer
// passes, which is the weaker half of what is worth knowing.
func TestTheArrivalTimeComparisonCatchesAnIntervalLeak(t *testing.T) {
	t.Parallel()

	baseline := buildArrivals(t, arrivedWhenItHappened)
	rescheduled := buildArrivals(t, arrivedOnADifferentSchedule)
	translated := buildArrivals(t, func(_ int, eventTime time.Time) time.Time {
		return eventTime.Add(37 * time.Minute)
	})

	if diverged := intervalLeakDiverges(t, baseline, rescheduled); !diverged {
		t.Fatal("a reducer leaking the interval between arrivals was NOT caught by the rescheduled stream; the arrival-time property is not testing what it claims")
	}
	if diverged := intervalLeakDiverges(t, baseline, translated); diverged {
		t.Fatal("a uniform shift caught the interval leak, so it is not blind after all; the reasoning behind the varying schedule needs re-reading")
	}
}

// buildArrivals returns the fixture's input stream under one arrival schedule.
func buildArrivals(t *testing.T, schedule arrivalSchedule) []event.Envelope {
	t.Helper()
	_, envelopes := propertyFixture(t, schedule)
	return envelopes
}

// intervalLeakDiverges runs an interval-leaking handler over both streams and
// reports whether comparing the emissions, modulo RecordedAt, finds a
// divergence — exactly the comparison TestArrivalTimeIndependence performs.
func intervalLeakDiverges(t *testing.T, original, shifted []event.Envelope) bool {
	t.Helper()

	fromOriginal := runLeakingHandler(t, original)
	fromShifted := runLeakingHandler(t, shifted)
	if len(fromOriginal) != len(fromShifted) || len(fromOriginal) == 0 {
		t.Fatalf("the leaking handler emitted %d and %d decisions; it must emit the same non-zero number for the comparison to mean anything", len(fromOriginal), len(fromShifted))
	}

	for i := range fromOriginal {
		want, got := fromOriginal[i], fromShifted[i]
		want.RecordedAt, got.RecordedAt = time.Time{}, time.Time{}
		if replay.Equivalent([]event.Envelope{want}, []event.Envelope{got}) != nil {
			return true
		}
	}
	return false
}

func runLeakingHandler(t *testing.T, inputs []event.Envelope) []event.Envelope {
	t.Helper()
	engine, err := replay.New(&intervalLeakingHandler{})
	if err != nil {
		t.Fatalf("replay.New() error = %v", err)
	}
	emitted, err := engine.Run(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Engine.Run() error = %v", err)
	}
	return emitted
}

// intervalLeakingHandler is a deliberately wrong handler: it computes the
// elapsed time since the previous input ARRIVED and puts it in what it emits.
// A decision derived from how long the gap was, rather than from the ordered
// event stream, is exactly the defect arrival-time independence exists to
// catch.
type intervalLeakingHandler struct{ previousArrival time.Time }

func (h *intervalLeakingHandler) Apply(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
	var elapsed time.Duration
	if !h.previousArrival.IsZero() {
		elapsed = in.RecordedAt.Sub(h.previousArrival)
	}
	h.previousArrival = in.RecordedAt

	payload := json.RawMessage(fmt.Sprintf(`{"elapsed_since_previous_arrival_ns":%d}`, elapsed))
	return []event.Envelope{{
		ID:                "leak-" + in.ID,
		Type:              "test.leaked",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         in.EventTime,
		RecordedAt:        in.RecordedAt,
		Source:            "leaking-fixture",
		StrategyVersion:   in.StrategyVersion,
		ConfigurationHash: in.ConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}}, nil
}

// TestASignalNeverSurvivesItsBar: each Signal's own bar is the only bar it
// belongs to. Two Signals fire in this fixture — bar 21's, which is FILLED
// (opening the Campaign the rest of the fixture exercises), and bar 23's,
// which is not. Only the second is this test's subject: a Signal that gets
// filled has been acted on, not left to survive past its bar. Bar 24 does
// not renew, repeat, or extend bar 23's Signal — it only records that the
// proposal it raised is now dead, dated to the bar it was raised on
// (PeriodEnd) even though the supersession is only observable once bar 24
// arrives (ExpiredAt) — CONTEXT.md: "Signal", ADR 0011.
func TestASignalNeverSurvivesItsBar(t *testing.T) {
	t.Parallel()

	cfg, inputs := propertyFixture(t, arrivedWhenItHappened)
	emitted := runFixture(t, cfg, inputs)

	var signals []event.Envelope
	var expiries []event.ProposalExpiredPayload
	for _, e := range emitted {
		switch e.Type {
		case event.SignalEventType:
			signals = append(signals, e)
		case event.ProposalExpiredEventType:
			var payload event.ProposalExpiredPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("decode %s: %v", e.ID, err)
			}
			expiries = append(expiries, payload)
		}
	}

	if len(signals) != 2 {
		t.Fatalf("got %d Signal(s), want exactly 2 (bar 21's, filled, and bar 23's, left to expire)", len(signals))
	}
	if !signals[0].EventTime.Equal(propertyDay(21)) {
		t.Fatalf("the first Signal's EventTime is %s, want bar 21 (%s)", signals[0].EventTime, propertyDay(21))
	}
	if !signals[1].EventTime.Equal(propertyDay(23)) {
		t.Fatalf("the second Signal's EventTime is %s, want bar 23 (%s)", signals[1].EventTime, propertyDay(23))
	}

	if len(expiries) != 1 {
		t.Fatalf("got %d expiry(ies), want exactly 1 (bar 23's unfilled proposal, superseded by bar 24; bar 21's proposal was filled, not expired)", len(expiries))
	}
	expiry := expiries[0]
	if expiry.Reason != event.ExpiryReasonSupersededByNextBar {
		t.Fatalf("expiry reason = %q, want %q", expiry.Reason, event.ExpiryReasonSupersededByNextBar)
	}
	// PeriodEnd names the bar the expired proposal BELONGED to — bar 23,
	// where the second Signal fired — never bar 24, which only supersedes
	// it. A Signal "surviving its bar" would look exactly like this field
	// naming the wrong bar.
	if !expiry.PeriodEnd.Equal(propertyDay(23)) {
		t.Fatalf("expiry PeriodEnd = %s, want bar 23 (%s): the proposal belongs to the bar that raised it, not the bar that superseded it", expiry.PeriodEnd, propertyDay(23))
	}
	if !expiry.ExpiredAt.Equal(propertyDay(24)) {
		t.Fatalf("expiry ExpiredAt = %s, want bar 24 (%s): supersession is only observable once the next bar arrives", expiry.ExpiredAt, propertyDay(24))
	}
}

// onlyEmissionOfType asserts exactly one envelope of typ exists in emitted
// and returns it.
func onlyEmissionOfType(t *testing.T, emitted []event.Envelope, typ string) event.Envelope {
	t.Helper()
	var found []event.Envelope
	for _, e := range emitted {
		if e.Type == typ {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d emission(s) of type %q, want exactly 1", len(found), typ)
	}
	return found[0]
}

// TestThePropertyFixtureOpensACampaignTakesAnAddAndExits is this fixture's
// own real deliverable: an explicit assertion that it reaches Campaign-open,
// an Add and an exit, so it cannot silently regress to raising and declining
// Setups alone — this file's own header names the defect that would
// reproduce.
func TestThePropertyFixtureOpensACampaignTakesAnAddAndExits(t *testing.T) {
	t.Parallel()

	cfg, inputs := propertyFixture(t, arrivedWhenItHappened)
	emitted := runFixture(t, cfg, inputs)

	opened := onlyEmissionOfType(t, emitted, event.CampaignOpenedEventType)
	var openedPayload event.CampaignOpenedPayload
	if err := json.Unmarshal(opened.Payload, &openedPayload); err != nil {
		t.Fatalf("decode %s: %v", opened.ID, err)
	}
	if openedPayload.CampaignN != propertyCampaignN {
		t.Fatalf("Campaign-opened CampaignN = %v, want %v (this fixture's own hand-derived N; a mismatch means the warm-up bars no longer produce the N the Add rung above was computed from)", openedPayload.CampaignN, propertyCampaignN)
	}
	if openedPayload.FilledQuantity != 1 {
		t.Errorf("Campaign-opened FilledQuantity = %d, want 1", openedPayload.FilledQuantity)
	}
	if openedPayload.EntryPrice != propertyEntryFillPrice {
		t.Errorf("Campaign-opened EntryPrice = %v, want the entry fill's own %v", openedPayload.EntryPrice, propertyEntryFillPrice)
	}

	added := onlyEmissionOfType(t, emitted, event.CampaignUnitAddedEventType)
	var addedPayload event.CampaignUnitAddedPayload
	if err := json.Unmarshal(added.Payload, &addedPayload); err != nil {
		t.Fatalf("decode %s: %v", added.ID, err)
	}
	if addedPayload.CampaignID != openedPayload.CampaignID {
		t.Errorf("Campaign-unit-added CampaignID = %q, want the opened Campaign's own %q", addedPayload.CampaignID, openedPayload.CampaignID)
	}
	if addedPayload.Units != 2 {
		t.Errorf("Campaign-unit-added Units = %d, want 2 (one Add on top of the opening Unit)", addedPayload.Units)
	}
	if addedPayload.Quantity != 1 {
		t.Errorf("Campaign-unit-added Quantity = %d, want 1", addedPayload.Quantity)
	}

	exited := onlyEmissionOfType(t, emitted, event.CampaignExitedEventType)
	var exitedPayload event.CampaignExitedPayload
	if err := json.Unmarshal(exited.Payload, &exitedPayload); err != nil {
		t.Fatalf("decode %s: %v", exited.ID, err)
	}
	if exitedPayload.CampaignID != openedPayload.CampaignID {
		t.Errorf("Campaign-exited CampaignID = %q, want the opened Campaign's own %q", exitedPayload.CampaignID, openedPayload.CampaignID)
	}
	if exitedPayload.Reason != event.ExitReasonStop {
		t.Errorf("Campaign-exited Reason = %q, want %q", exitedPayload.Reason, event.ExitReasonStop)
	}
	if exitedPayload.Quantity != 2 {
		t.Errorf("Campaign-exited Quantity = %d, want 2 (both the opening Unit and the Add, stopped out together)", exitedPayload.Quantity)
	}
}
