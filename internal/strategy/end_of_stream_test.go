package strategy_test

import (
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// This file holds #68's tests: a proposal still outstanding when the input
// stream ends reaches a terminal event like any other.
//
// The mechanism is an end-of-stream INPUT event (event.RunCompletedEventType),
// not a hook on replay.Handler, so the expiries it causes have a causing
// input like every other emission and a replay of the journal's input stream
// reproduces them unchanged.

// runCompletedEnvelope is the end-of-stream fact, as an input envelope.
func runCompletedEnvelope(t *testing.T, sequence uint64, at time.Time) event.Envelope {
	t.Helper()
	payload := mustMarshal(t, event.RunCompletedPayload{CompletedAt: at})
	return event.Envelope{
		ID:                "run-completed",
		Type:              event.RunCompletedEventType,
		SchemaVersion:     event.RunCompletedSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         at,
		RecordedAt:        at,
		Sequence:          sequence,
		Source:            "fixture",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: testConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

func (s *stream) endOfStream(at time.Time) *stream {
	s.seq++
	s.envelopes = append(s.envelopes, runCompletedEnvelope(s.t, s.seq, at))
	return s
}

// lastEmission is the final decision of a run, which is where an
// end-of-stream expiry must appear.
func lastEmission(t *testing.T, emitted []event.Envelope) event.Envelope {
	t.Helper()
	if len(emitted) == 0 {
		t.Fatal("the run emitted nothing at all")
	}
	return emitted[len(emitted)-1]
}

// TestAProposalOutstandingWhenTheStreamEndsReachesATerminalEvent covers all
// three kinds a run can end holding: an entry proposal from a breakout bar,
// an Add proposal from a rung reached on the last bar, and an exit proposal
// from an Exit Channel breach on the last bar.
func TestAProposalOutstandingWhenTheStreamEndsReachesATerminalEvent(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	tests := []struct {
		name        string
		build       func() *stream
		completedAt time.Time
		wantKind    string
		wantRule    string
	}{
		{
			name:        "an entry proposal raised by the last bar",
			build:       func() *stream { return newStream(t, cfg).bars(breakoutBars("AAPL")) },
			completedAt: day(56),
			wantKind:    event.ProposalKindEntry,
			wantRule:    event.RuleSignalExpiresWithItsBar,
		},
		{
			name: "an add proposal raised by the last bar",
			build: func() *stream {
				return newStream(t, cfg).
					bars(breakoutBars("AAPL")).
					fill(openingFill("AAPL")).
					bar(addOpportunityBar("AAPL", day(57), 500))
			},
			completedAt: day(57),
			wantKind:    event.ProposalKindAdd,
			wantRule:    event.RuleAddProposalExpiresWithItsBar,
		},
		{
			name: "an exit proposal raised by the last bar",
			build: func() *stream {
				return newStream(t, cfg).
					bars(breakoutBars("AAPL")).
					fill(openingFill("AAPL")).
					bar(postEntryBar("AAPL", day(57), 99))
			},
			completedAt: day(57),
			wantKind:    event.ProposalKindExit,
			wantRule:    event.RuleExitProposalExpiresWithItsBar,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			emitted := tt.build().endOfStream(tt.completedAt).mustRun()

			final := lastEmission(t, emitted)
			if final.Type != event.ProposalExpiredEventType {
				t.Fatalf("the run's final emission is %s, want %s", final.Type, event.ProposalExpiredEventType)
			}
			expiry := decodeProposalExpired(t, final)
			if expiry.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", expiry.Kind, tt.wantKind)
			}
			if expiry.Reason != event.ExpiryReasonInputStreamEnded {
				t.Errorf("Reason = %q, want %q", expiry.Reason, event.ExpiryReasonInputStreamEnded)
			}
			if expiry.Rule != tt.wantRule {
				t.Errorf("Rule = %q, want %q", expiry.Rule, tt.wantRule)
			}
			if !expiry.ExpiredAt.Equal(tt.completedAt) {
				t.Errorf("ExpiredAt = %s, want the instant the stream ended (%s)", expiry.ExpiredAt, tt.completedAt)
			}
			if final.CausationID == "" {
				t.Error("the expiry names no causing input; an end-of-stream emission must be caused by the end-of-stream event")
			}
		})
	}
}

// TestAStreamThatEndsWithNothingOutstandingEmitsNoExpiry: the end-of-stream
// event is not an excuse to emit something. A run whose last bar resolved
// everything ends silently.
func TestAStreamThatEndsWithNothingOutstandingEmitsNoExpiry(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	before := newStream(t, cfg).bars(breakoutBars("AAPL")).bar(quietBar("AAPL")).mustRun()
	after := newStream(t, cfg).bars(breakoutBars("AAPL")).bar(quietBar("AAPL")).endOfStream(day(57)).mustRun()

	if len(after) != len(before) {
		t.Fatalf("the end-of-stream event added %d emission(s) to a run with nothing outstanding", len(after)-len(before))
	}
}

// TestEveryProposalInACompletedRunReachesExactlyOneTerminalEvent is #68's
// own invariant, asserted directly over a run that ends holding a proposal.
func TestEveryProposalInACompletedRunReachesExactlyOneTerminalEvent(t *testing.T) {
	t.Parallel()

	emitted := newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		endOfStream(day(56)).
		mustRun()

	proposed := countFor(t, emitted, event.TradeProposalEventType, "AAPL")
	opened := countFor(t, emitted, event.CampaignOpenedEventType, "AAPL")
	expired := countFor(t, emitted, event.ProposalExpiredEventType, "AAPL")

	if proposed == 0 {
		t.Fatal("the fixture proposed nothing; it cannot show that every proposal is resolved")
	}
	if proposed != opened+expired {
		t.Fatalf("%d proposal(s) raised, but %d opened a Campaign and %d expired", proposed, opened, expired)
	}
}

// TestEndOfStreamExpiriesAreOrderedByInstrument: the reducer holds its
// instruments in a map, and a journal's decision order must not depend on Go's
// map iteration order (.greptile/rules.md: determinism).
func TestEndOfStreamExpiriesAreOrderedByInstrument(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()
	emitted := newStream(t, cfg).
		bars(breakoutBars("ZZZZ")).
		bars(breakoutBars("AAAA")).
		endOfStream(day(56)).
		mustRun()

	var order []string
	for _, envelope := range emitted {
		if envelope.Type == event.ProposalExpiredEventType {
			order = append(order, instrumentOf(t, envelope))
		}
	}
	want := []string{"AAAA", "ZZZZ"}
	if len(order) != len(want) {
		t.Fatalf("got %d end-of-stream expiries %v, want %v", len(order), order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expiries arrived in order %v, want %v", order, want)
		}
	}
}

// TestAnInputAfterTheStreamEndedFailsClosed: the end-of-stream event states
// that there is no more input. A later one contradicts it, and a contradiction
// is a defect in the producer, not something to absorb.
func TestAnInputAfterTheStreamEndedFailsClosed(t *testing.T) {
	t.Parallel()

	cfg := validConfigurationPayload()

	t.Run("a bar", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			endOfStream(day(56)).
			bar(quietBar("AAPL")).
			wantRunError("input stream has already ended")
	})

	t.Run("a second end-of-stream event", func(t *testing.T) {
		t.Parallel()
		newStream(t, cfg).
			bars(breakoutBars("AAPL")).
			endOfStream(day(56)).
			endOfStream(day(57)).
			wantRunError("input stream has already ended")
	})
}

// TestTheStreamCannotEndBeforeTheLastBarItDelivered: the end-of-stream
// instant is what every expiry is stamped with, so a producer that states
// one earlier than the data it already sent fails closed rather than
// recording an expiry that predates its own bar.
func TestTheStreamCannotEndBeforeTheLastBarItDelivered(t *testing.T) {
	t.Parallel()

	newStream(t, validConfigurationPayload()).
		bars(breakoutBars("AAPL")).
		endOfStream(day(55)).
		wantRunError("precedes")
}
