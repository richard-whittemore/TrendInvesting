package strategy_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// This file holds the tests for a venue's order lifecycle report
// (event.OrderLifecycleEventType), and for the order in which a live
// producer such as the LEAN adapter delivers a fill relative to the bars
// around it.

// orderLifecycle is one lifecycle report for the breakout bar's proposal
// order, at status, on at.
func orderLifecycle(status string, at time.Time) event.OrderLifecyclePayload {
	return event.OrderLifecyclePayload{
		InstrumentID: "AAPL",
		OrderID:      "1",
		Tag:          testDecisionID("proposal", "AAPL", day(56)),
		Status:       status,
		Quantity:     133,
		StopPrice:    200,
		OccurredAt:   at,
	}
}

func orderLifecycleEnvelope(t *testing.T, sequence uint64, payload event.OrderLifecyclePayload) event.Envelope {
	t.Helper()
	encoded := mustMarshal(t, payload)
	return event.Envelope{
		ID:                fmt.Sprintf("order-lifecycle-%d", sequence),
		Type:              event.OrderLifecycleEventType,
		SchemaVersion:     event.OrderLifecycleSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         payload.OccurredAt,
		RecordedAt:        payload.OccurredAt,
		Sequence:          sequence,
		Source:            "lean-adapter",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
}

func (s *stream) lifecycle(payload event.OrderLifecyclePayload) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, orderLifecycleEnvelope(s.t, s.seq, payload))
	return s
}

// nextSessionOpeningFill is openingFill as a live producer reports it: the
// proposal made at day(56)'s close rests into the next session and fills
// there, so the venue stamps it at day(57)'s close and the producer delivers
// it before day(57)'s bar (adapter/lean/README.md, "Fills").
func nextSessionOpeningFill() event.FillPayload {
	fill := openingFill("AAPL")
	fill.FilledAt = day(57)
	return fill
}

// TestOrderLifecycleIsRecordedWithNoDecision is the central claim: a
// lifecycle report changes no position and decides nothing, so a run
// carrying reports around its fill emits exactly the decisions of the same
// run without them, once stream position is stripped, and no decision is
// caused by a report. Falsified by emitting an engine-state decision from
// applyOrderLifecycle.
func TestOrderLifecycleIsRecordedWithNoDecision(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	without := newStream(t, cfg).bars(breakoutBars("AAPL")).
		fill(nextSessionOpeningFill()).
		bar(quietBar("AAPL")).
		endOfStream(day(57))
	with := newStream(t, cfg).bars(breakoutBars("AAPL")).
		lifecycle(orderLifecycle(event.OrderLifecycleStatusSubmitted, day(56))).
		fill(nextSessionOpeningFill()).
		lifecycle(orderLifecycle(event.OrderLifecycleStatusUpdated, day(57))).
		bar(quietBar("AAPL")).
		lifecycle(orderLifecycle(event.OrderLifecycleStatusCanceled, day(57))).
		endOfStream(day(57))

	withoutEmitted := without.mustRun()
	withEmitted := with.mustRun()
	if len(withoutEmitted) != len(withEmitted) {
		t.Fatalf("lifecycle reports changed the number of decisions: %d without, %d with", len(withoutEmitted), len(withEmitted))
	}
	for i := range withoutEmitted {
		if !bytes.Equal(withoutStreamPosition(t, withoutEmitted[i]), withoutStreamPosition(t, withEmitted[i])) {
			t.Fatalf("decision %d differs with lifecycle reports:\nwithout %s\nwith    %s", i,
				withoutStreamPosition(t, withoutEmitted[i]), withoutStreamPosition(t, withEmitted[i]))
		}
	}
	for _, input := range with.envelopes {
		if input.Type != event.OrderLifecycleEventType {
			continue
		}
		for _, decision := range withEmitted {
			if decision.CausationID == input.ID {
				t.Errorf("lifecycle report %s caused decision %s (%s); a report decides nothing", input.ID, decision.ID, decision.Type)
			}
		}
	}
}

// TestOrderLifecycleJournalReplaysByteIdentically: the reports are inputs
// like any other, so a journal holding them verifies and replays to the
// same decisions (ADR 0017).
func TestOrderLifecycleJournalReplaysByteIdentically(t *testing.T) {
	t.Parallel()

	s := newStream(t, validConfigurationPayload()).bars(breakoutBars("AAPL")).
		lifecycle(orderLifecycle(event.OrderLifecycleStatusSubmitted, day(56))).
		fill(nextSessionOpeningFill()).
		bar(quietBar("AAPL")).
		endOfStream(day(57))
	verifyMovementJournal(t, s, s.mustRun())
}

// TestAFillFromTheNextSessionPrecedesThatSessionsBar pins the order a live
// producer must deliver a fill in. The proposal made at a Session's close
// is outstanding until the instrument's next bar expires it (ADR 0011), so
// a fill executed during the next session must reach the reducer BEFORE
// that session's bar: it then opens the Campaign, stamped at the fill's own
// time. Delivered after the bar, the same fill names a proposal that bar
// already expired, and the run fails closed rather than open a Campaign
// from an order the strategy is no longer offering.
func TestAFillFromTheNextSessionPrecedesThatSessionsBar(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).bars(breakoutBars("AAPL")).
		fill(nextSessionOpeningFill()).
		bar(quietBar("AAPL")).
		mustRun()
	var opened *event.CampaignOpenedPayload
	for _, e := range emitted {
		if e.Type == event.CampaignOpenedEventType {
			p := decodeCampaignOpened(t, e)
			opened = &p
		}
	}
	if opened == nil {
		t.Fatal("no campaign opened from a fill delivered before the next session's bar")
	}
	if !opened.OpenedAt.Equal(day(57)) {
		t.Fatalf("campaign opened at %s, want the fill's own time %s", opened.OpenedAt, day(57))
	}

	newStream(t, cfg).bars(breakoutBars("AAPL")).
		bar(quietBar("AAPL")).
		fill(nextSessionOpeningFill()).
		wantRunError("there is no pending trade proposal")
}

// TestAnOrderLifecycleReportThatCannotBeReadFailsClosed: like every other
// input, a report that is out of place, at an unknown schema or invalid is
// refused rather than recorded as though it were understood.
func TestAnOrderLifecycleReportThatCannotBeReadFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	t.Run("before the configuration", func(t *testing.T) {
		t.Parallel()
		reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := replay.New(reducer)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine.Run(context.Background(), []event.Envelope{
			orderLifecycleEnvelope(t, 1, orderLifecycle(event.OrderLifecycleStatusSubmitted, day(56))),
		})
		if err == nil || !strings.Contains(err.Error(), "before a configuration event") {
			t.Fatalf("Run() error = %v, want the refusal every input gives before its configuration", err)
		}
	})

	t.Run("an unknown schema version", func(t *testing.T) {
		t.Parallel()
		s := newStream(t, cfg).bars(breakoutBars("AAPL"))
		s.seq++
		envelope := orderLifecycleEnvelope(t, s.seq, orderLifecycle(event.OrderLifecycleStatusSubmitted, day(56)))
		envelope.SchemaVersion = event.OrderLifecycleSchemaVersion + 1
		s.envelopes = append(s.envelopes, envelope)
		s.wantRunError("order lifecycle payload schema version", "ADR 0015")
	})

	t.Run("an invalid payload", func(t *testing.T) {
		t.Parallel()
		report := orderLifecycle("filled", day(56))
		newStream(t, cfg).bars(breakoutBars("AAPL")).
			lifecycle(report).
			wantRunError("not a recognised order lifecycle status")
	})

	t.Run("an undecodable payload", func(t *testing.T) {
		t.Parallel()
		s := newStream(t, cfg).bars(breakoutBars("AAPL"))
		s.seq++
		envelope := orderLifecycleEnvelope(t, s.seq, orderLifecycle(event.OrderLifecycleStatusSubmitted, day(56)))
		envelope.Payload = []byte(`{"quantity":"many"}`)
		envelope.PayloadHash = event.HashPayload(envelope.Payload)
		s.envelopes = append(s.envelopes, envelope)
		s.wantRunError("decode order lifecycle payload")
	})

	t.Run("after a deliberate stop", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).bars(breakoutBars("AAPL")).
			stop(validRunStoppedPayload(), day(56)).
			lifecycle(orderLifecycle(event.OrderLifecycleStatusCanceled, day(56))).
			wantRunError("the run was deliberately stopped")
	})
}
