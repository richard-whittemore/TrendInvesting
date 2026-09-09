package spike_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/transport"
	"github.com/richard-whittemore/TrendInvesting/transport/spike"
)

var errNotReady = errors.New("risk controller is not ready")

// testTimeout bounds every wait in this package, so a client call that never
// completes fails the test instead of stalling the package.
const testTimeout = 10 * time.Second

func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// fixedClock advances by a known step on every reading, so a measured duration
// in a test is an arithmetic fact rather than a race with the machine.
func fixedClock(step time.Duration) spike.Clock {
	current := time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := current
		current = current.Add(step)
		return now
	}
}

func stoppedClock() spike.Clock {
	at := time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

func TestBarEnvelopeIsValidAndCoversTheUniverse(t *testing.T) {
	t.Parallel()
	bar := spike.NewBarEnvelope(1, 1000, stoppedClock())
	if err := bar.Validate(); err != nil {
		t.Fatalf("bar envelope is invalid: %v", err)
	}
	var slice spike.BarSlice
	if err := json.Unmarshal(bar.Payload, &slice); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(slice.Bars) != 1000 {
		t.Errorf("universe = %d, want 1000", len(slice.Bars))
	}
}

func TestThePayloadHashCoversOnlyThePayload(t *testing.T) {
	t.Parallel()
	bar := spike.NewBarEnvelope(1, 10, stoppedClock())
	if bar.EnvelopeVersion != event.CurrentEnvelopeVersion {
		t.Fatalf("envelope version = %d, want %d", bar.EnvelopeVersion, event.CurrentEnvelopeVersion)
	}
	if err := bar.Validate(); err != nil {
		t.Fatalf("bar envelope is invalid: %v", err)
	}

	// Adding envelope_version changed the envelope, not the payload. The claim
	// that hashing is unaffected is worth checking rather than assuming: if the
	// hash ever covered envelope-level fields, every producer and verifier
	// would have to agree on the whole struct's encoding.
	downgraded := bar
	downgraded.EnvelopeVersion = 0
	if got := event.HashPayload(downgraded.Payload); got != bar.PayloadHash {
		t.Errorf("payload hash changed with an envelope-level field: %s != %s", got, bar.PayloadHash)
	}
	err := downgraded.Validate()
	if err == nil {
		t.Fatal("expected an envelope with no version to fail validation")
	}
	if strings.Contains(err.Error(), "payload hash does not match") {
		t.Errorf("a version rejection must not surface as a payload-hash mismatch: %v", err)
	}
}

func TestBarEnvelopeIsReproducible(t *testing.T) {
	t.Parallel()
	first := spike.NewBarEnvelope(7, 50, stoppedClock())
	second := spike.NewBarEnvelope(7, 50, stoppedClock())
	if string(first.Payload) != string(second.Payload) {
		t.Error("the same sequence produced different bytes; latency runs would not be comparable")
	}
	other := spike.NewBarEnvelope(8, 50, stoppedClock())
	if string(first.Payload) == string(other.Payload) {
		t.Error("different sequences produced identical bytes")
	}
}

func TestDeciderAnswersWithAValidDecisionCitingTheBar(t *testing.T) {
	t.Parallel()
	bar := spike.NewBarEnvelope(1, 200, stoppedClock())
	decision, err := spike.Decider(stoppedClock())(bounded(t), bar)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if err := decision.Validate(); err != nil {
		t.Fatalf("decision envelope is invalid: %v", err)
	}
	if decision.CausationID != bar.ID {
		t.Errorf("causation id = %q, want %q", decision.CausationID, bar.ID)
	}
	var payload spike.Decision
	if err := json.Unmarshal(decision.Payload, &payload); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if payload.Considered != 200 {
		t.Errorf("considered = %d, want 200", payload.Considered)
	}
	if len(payload.Proposals) == 0 {
		t.Error("expected the reference decider to propose something")
	}
}

func TestDeciderRejectsAPayloadItCannotRead(t *testing.T) {
	t.Parallel()
	bar := spike.NewBarEnvelope(1, 1, stoppedClock())
	bar.Payload = json.RawMessage(`{"bars":"not a list"}`)
	if _, err := spike.Decider(stoppedClock())(bounded(t), bar); err == nil {
		t.Fatal("expected an error for an unreadable payload")
	}
}

func TestSummarisePercentiles(t *testing.T) {
	t.Parallel()
	samples := make([]time.Duration, 0, 100)
	// 100 samples of 1..100ms, shuffled by construction rather than sorted, so
	// Summarise is shown to sort rather than to assume.
	for i := 100; i >= 1; i-- {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	stats := spike.Summarise(samples)

	for _, want := range []struct {
		label string
		got   time.Duration
		want  time.Duration
	}{
		{"count", time.Duration(stats.Count), 100},
		{"min", stats.Min, 1 * time.Millisecond},
		{"median", stats.Median, 50 * time.Millisecond},
		{"p95", stats.P95, 95 * time.Millisecond},
		{"p99", stats.P99, 99 * time.Millisecond},
		{"max", stats.Max, 100 * time.Millisecond},
		{"mean", stats.Mean, 50500 * time.Microsecond},
	} {
		if want.got != want.want {
			t.Errorf("%s = %v, want %v", want.label, want.got, want.want)
		}
	}
}

func TestSummariseOfNothingIsEmpty(t *testing.T) {
	t.Parallel()
	if got := spike.Summarise(nil); got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
}

func TestSummariseOfOneSample(t *testing.T) {
	t.Parallel()
	stats := spike.Summarise([]time.Duration{7 * time.Millisecond})
	if stats.Median != 7*time.Millisecond || stats.P99 != 7*time.Millisecond {
		t.Errorf("stats = %+v, want every percentile to be the single sample", stats)
	}
}

func TestStatsRenderATableRow(t *testing.T) {
	t.Parallel()
	stats := spike.Summarise([]time.Duration{2 * time.Millisecond})
	stats.RequestBytes = 4096
	row := stats.String()
	for _, want := range []string{"n=1", "req=4096B", "median=2ms", "p99=2ms"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q does not contain %q", row, want)
		}
	}
}

func TestRunMeasuresEveryRoundTrip(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "spk-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")

	server, err := transport.Listen(path, spike.Decider(stoppedClock()), transport.ServerConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		wg.Wait()
	})

	client, err := transport.Dial(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// The clock advances 1ms per reading and Run takes two readings per round
	// trip, so every sample is exactly 1ms.
	stats, err := spike.Run(bounded(t), client, 5, 20, 2, fixedClock(time.Millisecond))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Count != 5 {
		t.Errorf("count = %d, want 5", stats.Count)
	}
	if stats.Median != time.Millisecond {
		t.Errorf("median = %v, want 1ms", stats.Median)
	}
	if stats.RequestBytes == 0 {
		t.Error("request size was not measured")
	}
}

func TestRunReportsAnUnavailableEngine(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "spk-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")

	rejecting := func(_ context.Context, _ event.Envelope) (event.Envelope, error) {
		return event.Envelope{}, errNotReady
	}
	server, err := transport.Listen(path, rejecting, transport.ServerConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.Serve(); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		_ = server.Close()
		wg.Wait()
	})

	client, err := transport.Dial(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := spike.Run(bounded(t), client, 3, 5, 0, stoppedClock()); err == nil {
		t.Fatal("expected Run to report the engine's refusal")
	}
}
