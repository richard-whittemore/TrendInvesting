package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The reader's internal buffer is 64 KiB, so an over-long line must exceed
// that to exercise the multi-read resynchronisation path at all.
const beyondReadBuffer = 70 * 1024

func TestNextResynchronisesAfterAnOversizedFrame(t *testing.T) {
	t.Parallel()
	stream := strings.Repeat("A", beyondReadBuffer) + "\n" + `{"ok":true}` + "\n"
	reader := newFrameReader(strings.NewReader(stream), 1024)

	if _, err := reader.next(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("first frame: err = %v, want ErrFrameTooLarge", err)
	}
	frame, err := reader.next()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if string(frame) != `{"ok":true}` {
		t.Errorf("second frame = %q, want the frame after the oversized one", frame)
	}
}

func TestNextGivesUpWhenAnOversizedFrameExceedsTheDrainBudget(t *testing.T) {
	t.Parallel()
	// maxDrainRatio * 1024 is far below the length of this line, so the reader
	// must abandon the stream rather than discard unbounded input.
	stream := strings.Repeat("A", 200*1024) + "\n"
	reader := newFrameReader(strings.NewReader(stream), 1024)

	if _, err := reader.next(); !errors.Is(err, errUnsynchronised) {
		t.Fatalf("err = %v, want errUnsynchronised", err)
	}
}

func TestNextReportsAFrameTruncatedByAClosedPeer(t *testing.T) {
	t.Parallel()
	reader := newFrameReader(strings.NewReader(`{"partial":`), DefaultMaxFrameBytes)
	if _, err := reader.next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestNextReportsACleanEndOfStream(t *testing.T) {
	t.Parallel()
	reader := newFrameReader(strings.NewReader(""), DefaultMaxFrameBytes)
	if _, err := reader.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestNextReportsAPeerLostDuringAnOversizedFrame(t *testing.T) {
	t.Parallel()
	// No terminating newline: the oversized frame is also truncated.
	reader := newFrameReader(strings.NewReader(strings.Repeat("A", beyondReadBuffer)), 1024)
	if _, err := reader.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestWriteFrameRefusesAFrameOverTheLimit(t *testing.T) {
	t.Parallel()
	var sink strings.Builder
	err := writeFrame(&sink, strings.Repeat("A", 2048), 1024)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if sink.Len() != 0 {
		t.Errorf("wrote %d bytes; an over-limit frame must not reach the wire", sink.Len())
	}
}

func TestProtocolErrorNamesItsCode(t *testing.T) {
	t.Parallel()
	err := &ProtocolError{Code: CodeTimeout, Message: "took too long"}
	if got, want := err.Error(), "transport: timeout: took too long"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// internalTestTimeout bounds the waits in this file. It is separate from the
// external test package's constant only because the two cannot see each other.
const internalTestTimeout = 10 * time.Second

// TestServeContextReportsAnAcceptFailure pins the case where the listener
// fails while the context is still live.
//
// The test reaches for the unexported listener rather than adding a hook to
// production code: closing it is the smallest way to make Accept fail without
// also marking the server closed, which is what Server.Close would do.
func TestServeContextReportsAnAcceptFailure(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "ti-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	decide := func(_ context.Context, bar event.Envelope) (event.Envelope, error) {
		return bar, nil
	}
	server, err := Listen(filepath.Join(dir, "s.sock"), decide, ServerConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Deliberately never cancelled: the accept failure alone must end the call.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- server.ServeContext(ctx) }()

	if err := server.listener.Close(); err != nil {
		t.Fatalf("close the listener out from under the server: %v", err)
	}

	select {
	case err := <-served:
		if err == nil {
			t.Fatal("ServeContext returned nil; the accept failure must be reported")
		}
		if !strings.Contains(err.Error(), "accept") {
			t.Errorf("err = %v, want it to name the accept failure", err)
		}
	case <-time.After(internalTestTimeout):
		t.Fatal("ServeContext never returned: its watcher is still waiting on a context that was never cancelled")
	}
}
