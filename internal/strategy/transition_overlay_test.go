package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// These tests pin the copy-on-write mechanics of a transition
// (docs/development.md: reducer transactions): an instrument is copied on
// first access, a newly accepted fill is buffered, and both reach published
// state only when the whole input commits.

var errInjected = errors.New("injected rejection")

func runCompletedEnvelope(t *testing.T, at time.Time) event.Envelope {
	t.Helper()
	payload, err := json.Marshal(event.RunCompletedPayload{})
	if err != nil {
		t.Fatal(err)
	}
	return event.Envelope{ID: "run-completed", Type: event.RunCompletedEventType, SchemaVersion: event.RunCompletedSchemaVersion, EventTime: at, RecordedAt: at, Payload: payload}
}

// sessionClosedEnvelope ends the Session at periodEnd, naming ids (ADR 0021).
func sessionClosedEnvelope(t *testing.T, periodEnd time.Time, ids ...string) event.Envelope {
	t.Helper()
	payload, err := json.Marshal(event.SessionClosedPayload{PeriodEnd: periodEnd, InstrumentIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return event.Envelope{ID: "session-closed", Type: event.SessionClosedEventType, SchemaVersion: event.SessionClosedSchemaVersion, EventTime: periodEnd, RecordedAt: periodEnd, Payload: payload}
}

// twoInstrumentFixture is transitionFixture's bar case plus an untouched,
// warmed second instrument BBB.
func twoInstrumentFixture(t *testing.T) (*Reducer, event.Envelope) {
	t.Helper()
	r, input := transitionFixture(t, "bar")
	other, _ := transitionFixture(t, "bar")
	b := other.instruments["AAPL"]
	b.campaign, b.pendingProposal, b.pendingExitProposal, b.pendingAddProposal = nil, nil, nil, nil
	r.instruments["BBB"] = b
	return r, input
}

func TestRejectedTransitionLeavesTouchedAndUntouchedInstrumentsUnchanged(t *testing.T) {
	t.Parallel()
	r, input := twoInstrumentFixture(t)
	before, _ := twoInstrumentFixture(t)
	publishedA, publishedB := r.instruments["AAPL"], r.instruments["BBB"]

	_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		out, err := tx.apply(input)
		if err != nil || len(out) == 0 {
			t.Fatalf("bar for AAPL: %v, %v", out, err)
		}
		if a, _ := tx.instrument("AAPL"); a == publishedA {
			t.Error("the transaction mutated the published AAPL state instead of its own copy")
		}
		return nil, errInjected
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(r, before) {
		t.Fatal("a rejected transition changed published state")
	}
	if r.instruments["AAPL"] != publishedA || r.instruments["BBB"] != publishedB {
		t.Fatal("a rejected transition replaced published instrument state")
	}

	// Committing the same bar replaces only the instrument it touched; the
	// untouched one is neither copied nor republished.
	if _, err := r.Apply(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if r.instruments["AAPL"] == publishedA {
		t.Error("the committed bar did not publish AAPL's new state")
	}
	if r.instruments["BBB"] != publishedB {
		t.Error("a bar for AAPL copied the untouched instrument BBB")
	}
	if !reflect.DeepEqual(r.instruments["BBB"], before.instruments["BBB"]) {
		t.Error("a bar for AAPL changed BBB")
	}
}

func TestRejectedFillIsNotRememberedAndItsRetryFailsAgain(t *testing.T) {
	t.Parallel()
	r, input := transitionFixture(t, "entry")
	for attempt := range 2 {
		_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
			out, err := tx.apply(input)
			if err != nil {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
			// A retry absorbed as a duplicate delivery would return nothing.
			if len(out) == 0 {
				t.Fatalf("attempt %d: the fill was absorbed as a duplicate of a rejected acceptance", attempt)
			}
			if _, seen := tx.acceptedFill("new-fill"); !seen {
				t.Fatalf("attempt %d: the transaction does not see its own accepted fill", attempt)
			}
			return nil, errInjected
		})
		if !errors.Is(err, errInjected) {
			t.Fatalf("attempt %d: err = %v", attempt, err)
		}
		if _, leaked := r.acceptedFills["new-fill"]; leaked {
			t.Fatalf("attempt %d: a rejected fill reached the accepted-fill history", attempt)
		}
	}
}

func TestCommittedTransitionPublishesNewInstrumentAndFill(t *testing.T) {
	t.Parallel()
	r := newConfiguredReducerForInvariantTest(t)
	if _, err := r.Apply(context.Background(), invariantTestBarEnvelope(t, 1, "NEWCO", day(1))); err != nil {
		t.Fatal(err)
	}
	state, ok := r.instruments["NEWCO"]
	if !ok || !state.lastPeriodEnd.Equal(day(1)) {
		t.Fatalf("new instrument not published: %+v", state)
	}

	r, input := transitionFixture(t, "entry")
	if _, err := r.Apply(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	fill, ok := r.acceptedFills["new-fill"]
	if !ok || fill.instrumentID != "AAPL" || fill.kind != event.FillKindEntry {
		t.Fatalf("accepted fill not published: %+v", fill)
	}
	if r.instruments["AAPL"].campaign == nil {
		t.Fatal("the Campaign the fill opened was not published with it")
	}
}

func TestDuplicateDetectionSeesEveryFillAcceptedEarlierInTheRun(t *testing.T) {
	t.Parallel()

	t.Run("earlier input", func(t *testing.T) {
		t.Parallel()
		r, input := transitionFixture(t, "entry")
		if _, err := r.Apply(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		out, err := r.Apply(context.Background(), input)
		if err != nil || len(out) != 0 {
			t.Fatalf("re-delivery = %v, %v; want an idempotent no-op", out, err)
		}
		var fill event.FillPayload
		if err := json.Unmarshal(input.Payload, &fill); err != nil {
			t.Fatal(err)
		}
		fill.Quantity++
		changed := input
		changed.Payload = mustMarshalTransitionPayload(t, fill)
		if _, err := r.Apply(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "reused fill identifier") {
			t.Fatalf("reused id with different contents: %v", err)
		}
	})

	t.Run("same input", func(t *testing.T) {
		t.Parallel()
		r, input := transitionFixture(t, "entry")
		out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
			first, err := tx.apply(input)
			if err != nil {
				return nil, err
			}
			again, err := tx.apply(input)
			if err != nil {
				return nil, err
			}
			if len(again) != 0 {
				t.Errorf("second delivery in one transaction emitted %v", again)
			}
			return first, nil
		})
		if err != nil || len(out) == 0 {
			t.Fatalf("transaction = %v, %v", out, err)
		}
		if _, ok := r.acceptedFills["new-fill"]; !ok {
			t.Fatal("fill not published")
		}
	})
}

// A whole-universe pass must see instruments created earlier in the same
// transaction, in the transaction's own state.
func TestWholeUniversePassSeesInstrumentsCreatedInTheSameTransaction(t *testing.T) {
	t.Parallel()

	t.Run("chronology check", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
			if _, err := tx.apply(invariantTestBarEnvelope(t, 1, "NEWCO", day(5))); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.apply(sessionClosedEnvelope(t, day(5), "NEWCO")); err != nil {
				t.Fatal(err)
			}
			return tx.apply(runCompletedEnvelope(t, day(4)))
		})
		if err == nil || !strings.Contains(err.Error(), "NEWCO") {
			t.Fatalf("err = %v, want the stream end to precede NEWCO's bar", err)
		}
		if len(r.instruments) != 0 {
			t.Fatal("a rejected transaction published a created instrument")
		}
	})

	t.Run("expiry", func(t *testing.T) {
		t.Parallel()
		r := newConfiguredReducerForInvariantTest(t)
		out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
			if _, err := tx.apply(invariantTestBarEnvelope(t, 1, "NEWCO", day(5))); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.apply(sessionClosedEnvelope(t, day(5), "NEWCO")); err != nil {
				t.Fatal(err)
			}
			state, _ := tx.instrument("NEWCO")
			state.pendingProposal = &pendingProposalState{proposalID: "created", signalID: "signal", periodEnd: day(5), earliestFillAt: day(5), direction: event.DirectionLong, quantity: 1, n: 1, stopMultiple: 2, entryLevel: 100}
			return tx.apply(runCompletedEnvelope(t, day(6)))
		})
		if err != nil || len(out) != 1 || out[0].Type != event.ProposalExpiredEventType {
			t.Fatalf("end of stream = %v, %v; want the created instrument's proposal expired", out, err)
		}
		if state := r.instruments["NEWCO"]; state == nil || state.pendingProposal != nil || !r.streamEnded {
			t.Fatalf("committed state = %+v", state)
		}
	})
}

// Every access to one instrument within a transaction shares one copy, so a
// later payload sees what an earlier one did to it (ADR 0006/0007).
func TestLaterAccessInOneTransactionSeesEarlierMutation(t *testing.T) {
	t.Parallel()
	r := newConfiguredReducerForInvariantTest(t)
	if _, err := r.Apply(context.Background(), invariantTestBarEnvelope(t, 1, "AAPL", day(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(context.Background(), sessionClosedEnvelope(t, day(1), "AAPL")); err != nil {
		t.Fatal(err)
	}
	next := invariantTestBarEnvelope(t, 2, "AAPL", day(2))
	_, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		if _, err := tx.apply(next); err != nil {
			t.Fatal(err)
		}
		return tx.apply(next)
	})
	if err == nil {
		t.Fatal("a second bar for the same period in one transaction was accepted")
	}
	if got := r.instruments["AAPL"].lastPeriodEnd; !got.Equal(day(1)) {
		t.Fatalf("published lastPeriodEnd = %v, want the rejected transaction discarded", got)
	}
}

// A session close reads every instrument of its Session but copies only the
// ones it proposes for; the rest stay published state, untouched
// (docs/development.md: reducer transactions). Falsified by reaching the
// ranking or Add pass through instrument rather than peekInstrument.
func TestASessionCloseCopiesOnlyTheInstrumentsItProposesFor(t *testing.T) {
	t.Parallel()

	r := benchUniverse(t, 5)
	periodEnd := day(benchWarmUpBars + 1)
	ids := []string{"I0000", "I0001", "I0002", "I0003", "I0004"}
	for _, id := range ids {
		high := 101.0
		if id == "I0003" {
			high = 200
		}
		if _, err := r.Apply(context.Background(), benchBarEnvelopeAt(t, id, periodEnd, high)); err != nil {
			t.Fatal(err)
		}
	}
	var touched []string
	out, err := r.transact(func(tx *transition) ([]event.Envelope, error) {
		out, err := tx.apply(benchSessionClosedEnvelope(t, periodEnd, ids))
		for id := range tx.touched {
			touched = append(touched, id)
		}
		return out, err
	})
	if err != nil || len(out) != 1 || out[0].Type != event.TradeProposalEventType {
		t.Fatalf("session close = %v, %v; want I0003's one trade proposal", out, err)
	}
	if !reflect.DeepEqual(touched, []string{"I0003"}) {
		t.Fatalf("the session close copied %v, want only I0003", touched)
	}
}

func mustMarshalTransitionPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
