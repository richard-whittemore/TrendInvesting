package fills_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// as renames a bar series to another instrument.
func as(instrumentID string, bars []event.CompletedBarPayload) []event.CompletedBarPayload {
	out := make([]event.CompletedBarPayload, len(bars))
	for i, b := range bars {
		b.InstrumentID = instrumentID
		out[i] = b
	}
	return out
}

// runSessions drives two instruments' bar series through RunSession, one
// Session per period end, with each Session's bars in the order first then
// second.
func runSessions(t *testing.T, first, second []event.CompletedBarPayload) composed {
	t.Helper()
	simulator, reducer := newComposed(t, baselineConfig())
	ctx := context.Background()
	recorder := journal.NewRecorder(reducer)
	for _, e := range []event.Envelope{configurationEnvelope(t, baselineConfig()), accountSnapshotEnvelope(t, baselineConfig())} {
		if _, err := fills.Deliver(ctx, simulator, recorder, e); err != nil {
			t.Fatalf("Deliver() error = %v", err)
		}
	}
	for i := range first {
		session := []event.Envelope{barEnvelope(t, first[i]), barEnvelope(t, second[i])}
		session[0].ID += ":" + first[i].InstrumentID
		session[1].ID += ":" + second[i].InstrumentID
		if _, err := fills.RunSession(ctx, simulator, recorder, session); err != nil {
			t.Fatalf("RunSession(%s) error = %v", first[i].PeriodEnd, err)
		}
	}
	return recorded(t, recorder)
}

// TestASessionFillsEveryInstrumentsEntryInsideItsOwnBar: the Session's bars
// are delivered, then its close, and only then does each instrument's
// intrabar fixpoint run — so an entry proposed at the close still fills in
// the bar that raised it (ADR 0005), and the fixpoints run in ascending
// instrument order whatever order the bars arrived in (ADR 0021).
// Falsified by running each fixpoint straight after its own bar: no entry
// would exist to fill yet.
func TestASessionFillsEveryInstrumentsEntryInsideItsOwnBar(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar())
	for _, reversed := range []bool{false, true} {
		first, second := as("AAPL", bars), as("MSFT", bars)
		if reversed {
			first, second = second, first
		}
		run := runSessions(t, first, second)

		var types []string
		for _, e := range run.Inputs[len(run.Inputs)-5:] {
			id := e.Type
			if e.Type == event.FillEventType {
				id += ":" + decodeFill(t, e).InstrumentID
			}
			types = append(types, id)
		}
		want := []string{
			event.CompletedBarEventType, event.CompletedBarEventType, event.SessionClosedEventType,
			event.FillEventType + ":AAPL", event.FillEventType + ":MSFT",
		}
		if strings.Join(types, " ") != strings.Join(want, " ") {
			t.Fatalf("reversed=%v: the breakout Session's inputs are %v, want %v", reversed, types, want)
		}
		if n := len(envelopesOfType(run.Decisions, event.CampaignOpenedEventType)); n != 2 {
			t.Fatalf("reversed=%v: %d Campaigns opened, want 2", reversed, n)
		}
	}
}

// TestRunSessionStatesTheSessionItDelivered: the close names every bar of
// the Session, sorted, at the Session's period end, with the bars' own
// provenance.
func TestRunSessionStatesTheSessionItDelivered(t *testing.T) {
	t.Parallel()

	bars := warmUpBars()
	run := runSessions(t, as("MSFT", bars[:1]), as("AAPL", bars[:1]))
	closed := onlyOfType(t, run.Inputs, event.SessionClosedEventType)
	var payload event.SessionClosedPayload
	if err := json.Unmarshal(closed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.PeriodEnd.Equal(day(1)) || strings.Join(payload.InstrumentIDs, ",") != "AAPL,MSFT" {
		t.Fatalf("session close payload = %+v, want day(1) naming AAPL,MSFT", payload)
	}
	if closed.ID != "session-closed:"+day(1).Format("2006-01-02T15:04:05.000000000Z") ||
		!closed.EventTime.Equal(day(1)) || closed.Source != "fixture" ||
		closed.ConfigurationHash != testConfigurationHash || closed.StrategyVersion != testStrategyVersion {
		t.Fatalf("session close envelope = %+v, want the Session's own identity and the bars' provenance", closed)
	}
}

// TestRunSessionStopsWhenTheCloseIsRejected: a handler refusing the close
// stops the Session there, with the close recorded as the input it refused
// and no fixpoint run after it.
func TestRunSessionStopsWhenTheCloseIsRejected(t *testing.T) {
	t.Parallel()

	simulator, _ := newComposed(t, baselineConfig())
	handler := replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type == event.SessionClosedEventType {
			return nil, errors.New("close refused")
		}
		return nil, nil
	})
	result, err := fills.RunSession(context.Background(), simulator, handler, []event.Envelope{barEnvelope(t, warmUpBars()[0])})
	if err == nil || !strings.Contains(err.Error(), "close refused") {
		t.Fatalf("RunSession() error = %v, want the handler's refusal", err)
	}
	if n := len(result.Inputs); n != 2 || result.Inputs[1].Type != event.SessionClosedEventType {
		t.Fatalf("inputs = %d, want the bar then the refused close", n)
	}
}

func TestRunSessionRefusesWhatIsNotOneSession(t *testing.T) {
	t.Parallel()

	b := warmUpBars()
	other := b[0]
	other.InstrumentID = "MSFT"
	later := b[1]
	later.InstrumentID = "MSFT"
	snapshot := accountSnapshotEnvelope(t, baselineConfig())
	tests := []struct {
		name string
		bars []event.Envelope
		want string
	}{
		{"no bars", nil, "at least one bar"},
		{"not a bar", []event.Envelope{snapshot}, "requires"},
		{"two period ends", []event.Envelope{barEnvelope(t, b[0]), barEnvelope(t, later)}, "one period end"},
		{"one instrument twice", []event.Envelope{barEnvelope(t, b[0]), barEnvelope(t, b[0])}, "twice"},
		{"undecodable", []event.Envelope{{Type: event.CompletedBarEventType, Payload: json.RawMessage(`{`)}}, "decode"},
		{"invalid", []event.Envelope{{Type: event.CompletedBarEventType, Payload: json.RawMessage(`{}`)}}, "invalid completed bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			simulator, reducer := newComposed(t, baselineConfig())
			_, err := fills.RunSession(context.Background(), simulator, reducer, tt.bars)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RunSession() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := fills.RunSession(context.Background(), nil, nil, []event.Envelope{barEnvelope(t, other)}); err == nil {
		t.Fatal("RunSession() without a simulator and handler succeeded")
	}
}

// TestTheSessionCloseDoesNotDependOnBarArrivalOrder: the close is known only
// once every bar of the Session has been recorded, so it takes the latest
// bar's RecordedAt, and its other provenance from the lowest instrument ID,
// never from whichever bar happened to arrive first. The reducer stamps the
// close's provenance onto every proposal it makes, so an arrival-dependent
// close would make the same bars yield different proposal bytes (ADR 0021:
// order-independence).
func TestTheSessionCloseDoesNotDependOnBarArrivalOrder(t *testing.T) {
	t.Parallel()

	aapl, msft := barEnvelope(t, as("AAPL", warmUpBars()[:1])[0]), barEnvelope(t, as("MSFT", warmUpBars()[:1])[0])
	aapl.ID += ":AAPL"
	msft.ID += ":MSFT"
	later := msft.RecordedAt.Add(90 * time.Second)
	msft.RecordedAt = later

	closeOf := func(session []event.Envelope) []byte {
		t.Helper()
		simulator, _ := newComposed(t, baselineConfig())
		var closed event.Envelope
		handler := replay.HandlerFunc(func(_ context.Context, in event.Envelope) ([]event.Envelope, error) {
			if in.Type == event.SessionClosedEventType {
				closed = in
			}
			return nil, nil
		})
		if _, err := fills.RunSession(context.Background(), simulator, handler, session); err != nil {
			t.Fatalf("RunSession() error = %v", err)
		}
		if !closed.RecordedAt.Equal(later) {
			t.Errorf("close RecordedAt = %s, want the latest bar's %s", closed.RecordedAt, later)
		}
		encoded, err := json.Marshal(closed)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	if a, b := closeOf([]event.Envelope{aapl, msft}), closeOf([]event.Envelope{msft, aapl}); !bytes.Equal(a, b) {
		t.Fatalf("the close depends on bar arrival order:\n%s\n%s", a, b)
	}
}
