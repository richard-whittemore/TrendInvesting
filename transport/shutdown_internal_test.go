package transport

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// shutdownRace is a lifetime context whose cancellation is already visible
// through Err when the decision's result wakes decideWithTimeout, while its
// Done channel has not yet closed. That is the ordering a shutdown can produce: Close had
// begun, the decision was in the result channel, and the select took the
// result. With a real context the select picks at random when both are
// ready, which is why the integration test only flaked.
type shutdownRace struct {
	context.Context
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (c *shutdownRace) Done() <-chan struct{} { return c.done }

func (c *shutdownRace) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *shutdownRace) cancel() {
	c.mu.Lock()
	c.err = context.Canceled
	c.mu.Unlock()
}

// A decision that completes while the server is shutting down must not be
// delivered: the reply is CodeUnavailable, so the adapter refuses to trade
// instead of acting on a decision from an engine that was going away (fail
// closed, ADR 0014).
func TestADecisionFinishedDuringShutdownIsNotDelivered(t *testing.T) {
	t.Parallel()
	ctx := &shutdownRace{Context: context.Background(), done: make(chan struct{})}
	defer close(ctx.done)
	s := &Server{lifetime: ctx, stop: ctx.cancel}
	s.decide = func(_ context.Context, bar event.Envelope) (event.Envelope, error) {
		s.stop() // Close begins as the decision finishes
		return shutdownTestDecision(bar), nil
	}
	resp := s.respond(shutdownTestFrame(t))
	if resp.Error == nil || resp.Error.Code != CodeUnavailable {
		t.Fatalf("response %+v, want error code %s", resp, CodeUnavailable)
	}
	if resp.Envelope != nil {
		t.Fatal("a decision was delivered during shutdown")
	}
}

// Before shutdown, a decision is delivered as before.
func TestADecisionBeforeShutdownIsDelivered(t *testing.T) {
	t.Parallel()
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	s := &Server{lifetime: lifetime, stop: stop}
	s.decide = func(_ context.Context, bar event.Envelope) (event.Envelope, error) {
		return shutdownTestDecision(bar), nil
	}
	resp := s.respond(shutdownTestFrame(t))
	if resp.Error != nil || resp.Envelope == nil {
		t.Fatalf("response %+v, want a decision", resp)
	}
}

func shutdownTestFrame(t *testing.T) []byte {
	t.Helper()
	payload := json.RawMessage(`{"symbol":"AAPL","close":1}`)
	bar := event.Envelope{
		ID:                "bar-1",
		Type:              "market.bar.completed",
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		SchemaVersion:     1,
		EventTime:         time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 9, 9, 20, 0, 1, 0, time.UTC),
		Sequence:          1,
		CorrelationID:     "run-1",
		Source:            "lean-adapter",
		StrategyVersion:   "test-strategy-v1",
		ConfigurationHash: "cfg-abc",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
	frame, err := json.Marshal(bar)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func shutdownTestDecision(bar event.Envelope) event.Envelope {
	d := bar
	d.ID = "decision-" + bar.ID
	d.Type = "decision.proposed"
	d.CausationID = bar.ID
	d.Source = "decision-engine"
	d.Payload = json.RawMessage(`{"action":"hold"}`)
	d.PayloadHash = event.HashPayload(d.Payload)
	return d
}
