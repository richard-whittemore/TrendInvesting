package transport

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxFrameBytes bounds a single request or reply. One completed daily
// bar for a 1,000-symbol universe is a few hundred kilobytes of JSON, so a
// mebibyte leaves headroom without letting a runaway producer exhaust memory.
const DefaultMaxFrameBytes = 1 << 20

// maxDrainRatio bounds how much of an oversized line the reader will discard
// while resynchronising. Beyond this the peer is assumed to be broken rather
// than merely wrong, and the connection is abandoned.
const maxDrainRatio = 16

// ErrFrameTooLarge reports a frame longer than the configured maximum. The
// reader has already resynchronised to the next newline when it is returned,
// so the caller may keep using the connection.
var ErrFrameTooLarge = errors.New("transport: frame exceeds maximum size")

// errUnsynchronised reports that an oversized frame could not be skipped
// within the drain budget, so the stream position is no longer known.
var errUnsynchronised = errors.New("transport: cannot resynchronise after oversized frame")

// frameReader reads newline-delimited frames with a hard size ceiling.
//
// bufio.Scanner is not used: its buffer ceiling is a fixed property of the
// scanner and an over-long token is indistinguishable from a broken stream,
// which loses exactly the distinction this protocol needs.
type frameReader struct {
	buf *bufio.Reader
	max int
}

func newFrameReader(r io.Reader, maxFrameBytes int) *frameReader {
	return &frameReader{buf: bufio.NewReaderSize(r, 64*1024), max: maxFrameBytes}
}

// next returns the next frame without its terminating newline.
func (fr *frameReader) next() ([]byte, error) {
	frame := make([]byte, 0, 512)
	for {
		chunk, err := fr.buf.ReadSlice('\n')
		if len(frame)+len(chunk) > fr.max {
			return nil, fr.discardOversized(err)
		}
		frame = append(frame, chunk...)
		switch {
		case err == nil:
			return frame[:len(frame)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(frame) > 0:
			// A peer that died mid-frame is not a clean end of stream, and
			// conflating the two would hide a crash as an orderly shutdown.
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// discardOversized resynchronises after an over-long frame and reports why the
// frame was rejected.
//
// readErr is the error from the read that crossed the limit. If it is nil the
// terminating newline has already been consumed and the stream is back in
// step; draining again would block for ever waiting for a newline that has
// already gone past.
func (fr *frameReader) discardOversized(readErr error) error {
	switch {
	case readErr == nil:
		return ErrFrameTooLarge
	case errors.Is(readErr, bufio.ErrBufferFull):
		if err := fr.drain(); err != nil {
			return err
		}
		return ErrFrameTooLarge
	default:
		// The peer died or failed part-way through an oversized frame; that
		// is a broken connection, not a rejected message.
		return readErr
	}
}

// drain discards input up to and including the next newline, so that one
// oversized message costs one message rather than the whole session.
func (fr *frameReader) drain() error {
	budget := fr.max * maxDrainRatio
	for {
		chunk, err := fr.buf.ReadSlice('\n')
		budget -= len(chunk)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			if budget <= 0 {
				return errUnsynchronised
			}
			continue
		default:
			return err
		}
	}
}

// writeFrame encodes v as one newline-terminated JSON frame. The frame is
// written with a single Write so that a reader never observes a partial frame
// interleaved with another goroutine's.
func writeFrame(w io.Writer, v any, maxFrameBytes int) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("transport: encode frame: %w", err)
	}
	if len(encoded)+1 > maxFrameBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrFrameTooLarge, len(encoded)+1, maxFrameBytes)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("transport: write frame: %w", err)
	}
	return nil
}
