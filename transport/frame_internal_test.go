package transport

import (
	"errors"
	"io"
	"strings"
	"testing"
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
