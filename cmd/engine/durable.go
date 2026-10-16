package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// createLogFile creates the file this process streams its durable log to,
// exclusively, so an existing log is never appended to or overwritten (ADR
// 0018), and flushes its directory so the name survives a power loss along
// with the header CreateLog flushes into it (ADR 0027).
func createLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("engine: %s already exists: a durable log is recorded evidence and is never appended to or overwritten (ADR 0018); to continue its run, pass it to -resume and name a new -log", path)
		}
		return nil, fmt.Errorf("engine: create the durable log: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("engine: flush the durable log's directory %s: %w", filepath.Dir(path), err)
	}
	return file, nil
}

// recoverLogFile reads a previous process's durable log. It never opens the
// file for writing: the log stays the evidence it is, torn tail included.
func recoverLogFile(path string) (journal.Recovery, error) {
	file, err := os.Open(path)
	if err != nil {
		return journal.Recovery{}, fmt.Errorf("engine: open the durable log to resume from: %w", err)
	}
	defer func() { _ = file.Close() }()
	recovery, err := journal.RecoverLog(file)
	if err != nil {
		return journal.Recovery{}, fmt.Errorf("engine: resume from %s: %w", path, err)
	}
	return recovery, nil
}

// checkResumable refuses a durable log this invocation cannot continue: one
// recorded by another configuration or build, or one opening with a
// different configuration input from the one this invocation composed (a
// different -config or -as-of). It runs before this process creates its own
// log, so a refused restart leaves nothing new behind.
func checkResumable(recovery journal.Recovery, logHeader journal.LogHeader, configEnvelope event.Envelope) error {
	if recovery.Header != logHeader {
		return fmt.Errorf("engine: the durable log was recorded by run %s / %s, not this run %s / %s; a log resumes only under the configuration and build that recorded it",
			recovery.Header.ConfigurationHash, recovery.Header.StrategyVersion, logHeader.ConfigurationHash, logHeader.StrategyVersion)
	}
	if len(recovery.Entries) == 0 {
		return nil
	}
	if !bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), event.CanonicalEnvelopeBytes(configEnvelope)) {
		return errors.New("engine: the durable log opens with a different configuration input from the one this invocation composed; resume with the -config and -as-of the logged run started with")
	}
	return nil
}

// resumeRun rebuilds this process's state from a previous process's durable
// log before the socket opens (ADR 0026 §5; ADR 0027): every recovered input
// goes through wireEngine, so the contiguity cursor and the reducer both end
// where the previous process's committed record ends, and the adapter's next
// input continues that sequence.
//
// Nothing rebuilt here is sent anywhere: what the log holds may already have
// been acted on, and matching it against the broker is reconciliation's job
// (ADR 0019).
func resumeRun(ctx context.Context, wireEngine *replay.Engine, recorder *journal.Recorder, recovery journal.Recovery) error {
	if err := journal.Resume(ctx, replay.HandlerFunc(wireEngine.Apply), recorder, recovery); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}

// reportResume states what a restart recovered, and any torn tail it left
// out, so an operator knows which input the adapter must send again.
func reportResume(out io.Writer, path string, recovery journal.Recovery) error {
	if _, err := fmt.Fprintf(out, "resumed %d record(s) from %s\n", len(recovery.Entries), path); err != nil {
		return fmt.Errorf("engine: report the resume: %w", err)
	}
	if recovery.Torn == nil {
		return nil
	}
	if _, err := fmt.Fprintf(out, "left out a torn tail of %d byte(s) at byte %d of %s: an input whose write a crash interrupted; none of its decisions was released, so the adapter sends it again\n",
		recovery.Torn.Length, recovery.Torn.Offset, path); err != nil {
		return fmt.Errorf("engine: report the resume: %w", err)
	}
	return nil
}
