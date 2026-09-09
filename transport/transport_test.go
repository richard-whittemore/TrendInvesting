package transport_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/transport"
)

const testStrategyVersion = "spike-0"

// socketPath returns a short socket path. The default TMPDIR on macOS is a
// /var/folders/... path long enough to approach the 103-byte sun_path limit on
// its own, which would make these tests fail for a reason unrelated to what
// they assert.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tt-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func newBar(t *testing.T, id string, sequence uint64) event.Envelope {
	t.Helper()
	payload := json.RawMessage(fmt.Sprintf(`{"symbol":"AAPL","close":%d}`, sequence))
	return event.Envelope{
		ID:                id,
		Type:              "market.bar.completed",
		SchemaVersion:     1,
		EventTime:         time.Date(2026, 9, 9, 20, 0, 0, 0, time.UTC),
		RecordedAt:        time.Date(2026, 9, 9, 20, 0, 1, 0, time.UTC),
		Sequence:          sequence,
		CorrelationID:     "run-1",
		Source:            "lean-adapter",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: "cfg-abc",
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}
}

// echoDecider answers every bar with a decision that cites it.
func echoDecider(_ context.Context, bar event.Envelope) (event.Envelope, error) {
	payload := json.RawMessage(`{"action":"hold"}`)
	return event.Envelope{
		ID:                "decision-" + bar.ID,
		Type:              "decision.proposed",
		SchemaVersion:     1,
		EventTime:         bar.EventTime,
		RecordedAt:        bar.RecordedAt,
		Sequence:          bar.Sequence,
		CorrelationID:     bar.CorrelationID,
		CausationID:       bar.ID,
		Source:            "decision-engine",
		StrategyVersion:   testStrategyVersion,
		ConfigurationHash: bar.ConfigurationHash,
		PayloadHash:       event.HashPayload(payload),
		Payload:           payload,
	}, nil
}

// startServer runs a server and returns its socket path.
func startServer(t *testing.T, decide transport.Decider, cfg transport.ServerConfig) (server *transport.Server, path string) {
	t.Helper()
	path = socketPath(t)
	server, err := transport.Listen(path, decide, cfg)
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
	return server, path
}

func dial(t *testing.T, path string, cfg transport.ClientConfig) *transport.Client {
	t.Helper()
	client, err := transport.DialConfig(path, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// --- contract -------------------------------------------------------------

func TestBarProducesDecisionCitingIt(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	bar := newBar(t, "bar-1", 1)
	decision, err := client.Decide(t.Context(), bar)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.CausationID != bar.ID {
		t.Errorf("causation id = %q, want %q", decision.CausationID, bar.ID)
	}
	if decision.CorrelationID != bar.CorrelationID {
		t.Errorf("correlation id = %q, want %q", decision.CorrelationID, bar.CorrelationID)
	}
	if err := decision.Validate(); err != nil {
		t.Errorf("decision is not a valid envelope: %v", err)
	}
}

func TestConnectionCarriesManyBarsInOrder(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	for sequence := uint64(1); sequence <= 50; sequence++ {
		bar := newBar(t, fmt.Sprintf("bar-%d", sequence), sequence)
		decision, err := client.Decide(t.Context(), bar)
		if err != nil {
			t.Fatalf("decide %d: %v", sequence, err)
		}
		if decision.CausationID != bar.ID {
			t.Fatalf("bar %d answered by %q", sequence, decision.CausationID)
		}
	}
}

func TestReplyForAnotherBarIsRejected(t *testing.T) {
	t.Parallel()
	misciting := func(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
		decision, err := echoDecider(ctx, bar)
		decision.CausationID = "some-other-bar"
		return decision, err
	}
	_, path := startServer(t, misciting, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	if _, err := client.Decide(t.Context(), newBar(t, "bar-1", 1)); !errors.Is(err, transport.ErrOutOfOrder) {
		t.Fatalf("err = %v, want ErrOutOfOrder", err)
	}
	// A desynchronised connection must not be reused.
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("reuse after desync: err = %v, want ErrUnavailable", err)
	}
}

// --- fault: the Go side is absent, or dies -------------------------------

func TestDialFailsWhenEngineIsAbsent(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	_, err := transport.Dial(path)
	if !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestDecideFailsWhenEngineDiesMidExchange(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	blocking := func(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return echoDecider(ctx, bar)
	}
	server, path := startServer(t, blocking, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	type outcome struct{ err error }
	results := make(chan outcome, 1)
	go func() {
		_, err := client.Decide(context.Background(), newBar(t, "bar-1", 1))
		results <- outcome{err: err}
	}()

	<-entered
	if err := server.Close(); err != nil {
		t.Fatalf("close server: %v", err)
	}
	close(release)

	got := <-results
	if !errors.Is(got.err, transport.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", got.err)
	}
}

func TestShutdownIsNotBlockedByAStuckDecider(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	forever := make(chan struct{})
	t.Cleanup(func() { close(forever) })
	stuck := func(_ context.Context, _ event.Envelope) (event.Envelope, error) {
		close(entered)
		<-forever // deliberately ignores cancellation
		return event.Envelope{}, errors.New("never")
	}
	server, path := startServer(t, stuck, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})
	go func() { _, _ = client.Decide(context.Background(), newBar(t, "bar-1", 1)) }()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind a decider that ignores cancellation")
	}
}

func TestDecideFailsFastOnAnAbandonedConnection(t *testing.T) {
	t.Parallel()
	server, path := startServer(t, echoDecider, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	if _, err := client.Decide(t.Context(), newBar(t, "bar-1", 1)); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close server: %v", err)
	}
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("after engine exit: err = %v, want ErrUnavailable", err)
	}
	if _, err := client.Decide(t.Context(), newBar(t, "bar-3", 3)); !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("third decide: err = %v, want ErrUnavailable", err)
	}
}

// --- fault: slow consumer -------------------------------------------------

func TestSlowDeciderIsReportedAsTimeout(t *testing.T) {
	t.Parallel()
	slow := func(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
		<-ctx.Done()
		return echoDecider(ctx, bar)
	}
	_, path := startServer(t, slow, transport.ServerConfig{DecisionTimeout: 20 * time.Millisecond})
	client := dial(t, path, transport.ClientConfig{})

	_, err := client.Decide(t.Context(), newBar(t, "bar-1", 1))
	var protocolErr *transport.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
	if protocolErr.Code != transport.CodeTimeout {
		t.Errorf("code = %q, want %q", protocolErr.Code, transport.CodeTimeout)
	}
	// The engine is alive, so the session must survive its own timeout.
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); err == nil {
		t.Error("expected the second bar to time out too, got success")
	}
}

func TestClientDeadlineAbandonsTheConnection(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := func(ctx context.Context, bar event.Envelope) (event.Envelope, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return echoDecider(ctx, bar)
	}
	_, path := startServer(t, slow, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Decide(ctx, newBar(t, "bar-1", 1)); !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); !errors.Is(err, transport.ErrUnavailable) {
		t.Fatalf("reuse after deadline: err = %v, want ErrUnavailable", err)
	}
}

// --- fault: bad and oversized frames -------------------------------------

func TestOversizedRequestIsRejectedAndTheSessionSurvives(t *testing.T) {
	t.Parallel()
	const serverLimit = 4096
	_, path := startServer(t, echoDecider, transport.ServerConfig{MaxFrameBytes: serverLimit})
	// The client is given a larger ceiling so the oversized frame is actually
	// put on the wire and the server's handling is what is under test.
	client := dial(t, path, transport.ClientConfig{MaxFrameBytes: 1 << 20})

	huge := newBar(t, "bar-1", 1)
	huge.Payload = json.RawMessage(`{"symbols":"` + strings.Repeat("A", 20000) + `"}`)
	huge.PayloadHash = event.HashPayload(huge.Payload)

	_, err := client.Decide(t.Context(), huge)
	var protocolErr *transport.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
	if protocolErr.Code != transport.CodeOversized {
		t.Errorf("code = %q, want %q", protocolErr.Code, transport.CodeOversized)
	}
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); err != nil {
		t.Fatalf("session did not survive an oversized frame: %v", err)
	}
}

func TestClientRefusesToSendAnOversizedRequest(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{MaxFrameBytes: 2048})

	huge := newBar(t, "bar-1", 1)
	huge.Payload = json.RawMessage(`{"symbols":"` + strings.Repeat("A", 8192) + `"}`)
	huge.PayloadHash = event.HashPayload(huge.Payload)

	if _, err := client.Decide(t.Context(), huge); !errors.Is(err, transport.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	// Nothing was written, so the connection is still usable.
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); err != nil {
		t.Fatalf("connection unusable after a refused send: %v", err)
	}
}

func TestUnreadableFrameIsRejected(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("this is not an envelope\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readResponse(t, conn)
	if reply.Error == nil || reply.Error.Code != transport.CodeInvalidFrame {
		t.Fatalf("reply = %+v, want %s", reply, transport.CodeInvalidFrame)
	}
}

func TestInvalidEnvelopeIsRejectedWithItsIdentifier(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	bar := newBar(t, "bar-1", 1)
	bar.PayloadHash = "0000"
	encoded, err := json.Marshal(bar)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readResponse(t, conn)
	if reply.Error == nil || reply.Error.Code != transport.CodeInvalidEnvelope {
		t.Fatalf("reply = %+v, want %s", reply, transport.CodeInvalidEnvelope)
	}
	if reply.Error.CausationID != bar.ID {
		t.Errorf("causation id = %q, want %q", reply.Error.CausationID, bar.ID)
	}
}

func TestDeciderFailureIsReportedWithoutDroppingTheSession(t *testing.T) {
	t.Parallel()
	failing := func(_ context.Context, _ event.Envelope) (event.Envelope, error) {
		return event.Envelope{}, errors.New("risk controller is not ready")
	}
	_, path := startServer(t, failing, transport.ServerConfig{})
	client := dial(t, path, transport.ClientConfig{})

	_, err := client.Decide(t.Context(), newBar(t, "bar-1", 1))
	var protocolErr *transport.ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("err = %v, want a ProtocolError", err)
	}
	if protocolErr.Code != transport.CodeDeciderFailed {
		t.Errorf("code = %q, want %q", protocolErr.Code, transport.CodeDeciderFailed)
	}
	if protocolErr.CausationID != "bar-1" {
		t.Errorf("causation id = %q, want %q", protocolErr.CausationID, "bar-1")
	}
	if _, err := client.Decide(t.Context(), newBar(t, "bar-2", 2)); !errors.As(err, &protocolErr) {
		t.Fatalf("session lost after a decider failure: %v", err)
	}
}

func readResponse(t *testing.T, conn net.Conn) transport.Response {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	for !strings.Contains(string(buf), "\n") {
		n, err := conn.Read(chunk)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		buf = append(buf, chunk[:n]...)
	}
	var reply transport.Response
	line, _, _ := strings.Cut(string(buf), "\n")
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("decode reply %q: %v", line, err)
	}
	return reply
}

// --- binding and startup ordering ----------------------------------------

func TestListenRejectsAnOverlongSocketPath(t *testing.T) {
	t.Parallel()
	path := "/tmp/" + strings.Repeat("x", transport.MaxSocketPathBytes)
	_, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
	if err == nil {
		t.Fatal("expected an error for an overlong socket path")
	}
	if !strings.Contains(err.Error(), "limit is") {
		t.Errorf("err = %v, want it to name the limit", err)
	}
}

func TestListenRequiresADecider(t *testing.T) {
	t.Parallel()
	if _, err := transport.Listen(socketPath(t), nil, transport.ServerConfig{}); err == nil {
		t.Fatal("expected an error when no decider is supplied")
	}
}

func TestListenReclaimsASocketLeftByAKilledEngine(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
	if err != nil {
		t.Fatalf("listen over a stale socket file: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestListenRefusesToDisplaceALiveEngine(t *testing.T) {
	t.Parallel()
	_, path := startServer(t, echoDecider, transport.ServerConfig{})
	_, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
	if err == nil {
		t.Fatal("expected an error when another server is already listening")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("err = %v, want it to say another server is listening", err)
	}
}

func TestServeContextStopsOnCancellation(t *testing.T) {
	t.Parallel()
	path := socketPath(t)
	server, err := transport.Listen(path, echoDecider, transport.ServerConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.ServeContext(ctx) }()

	client := dial(t, path, transport.ClientConfig{})
	if _, err := client.Decide(t.Context(), newBar(t, "bar-1", 1)); err != nil {
		t.Fatalf("decide: %v", err)
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("ServeContext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeContext did not return after cancellation")
	}
}

func TestPathReportsTheBoundSocket(t *testing.T) {
	t.Parallel()
	server, path := startServer(t, echoDecider, transport.ServerConfig{})
	if server.Path() != path {
		t.Errorf("Path() = %q, want %q", server.Path(), path)
	}
}
