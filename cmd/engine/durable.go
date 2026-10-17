package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// createLogFile creates the file this process streams its durable log to,
// exclusively, so an existing log is never appended to or overwritten (ADR
// 0018), and flushes its directory so the name survives a power loss along
// with the header CreateLog flushes into it (ADR 0027). The directory is
// taken as spelled, not cleaned: a ".." after a symbolic link climbs from the
// link's target, so cleaning would name, and flush, another directory.
func createLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("engine: %s already exists: a durable log is recorded evidence and is never appended to or overwritten (ADR 0018); to continue its run, pass it to -resume and name a new -log", path)
		}
		return nil, fmt.Errorf("engine: create the durable log: %w", err)
	}
	dir, _ := splitAsSpelled(path)
	if err := syncDir(dir); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("engine: flush the durable log's directory %s: %w", dir, err)
	}
	return file, nil
}

// recoverLogFile reads a previous process's durable log. It never opens the
// file for writing: the log stays the evidence it is, torn tail included.
// A log whose content cannot be recovered (ADR 0027 §5) is reported as an
// *unrecoverableLogError; a failure to open or read the file is not, since
// the file may be intact.
func recoverLogFile(path string) (journal.Recovery, error) {
	file, err := openDurableLog(path)
	if err != nil {
		return journal.Recovery{}, fmt.Errorf("engine: open the durable log to resume from: %w", err)
	}
	defer func() { _ = file.Close() }()
	reader := &faultRecordingReader{r: file}
	recovery, err := journal.RecoverLog(reader)
	switch {
	case err == nil:
		return recovery, nil
	case reader.fault != nil:
		return journal.Recovery{}, fmt.Errorf("engine: resume from %s: %w", path, err)
	}
	return journal.Recovery{}, &unrecoverableLogError{path: path, err: err}
}

// openDurableLog opens a durable log for reading. It is the one seam a
// test faults a read through, since a file's mode cannot fail a read after
// the open, and does not stop root.
var openDurableLog = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// unrecoverableLogError reports a durable log read in full whose content is
// not a recoverable log (ADR 0027 §5): damaged, or cut short before its
// header was complete.
type unrecoverableLogError struct {
	path string
	err  error
}

func (e *unrecoverableLogError) Error() string {
	return fmt.Sprintf("engine: resume from %s: %v", e.path, e.err)
}

func (e *unrecoverableLogError) Unwrap() error { return e.err }

// faultRecordingReader records the first error, other than the end of the
// file, its reader returns, so a fault in reading a log is told from damage
// to what it holds.
type faultRecordingReader struct {
	r     io.Reader
	fault error
}

func (f *faultRecordingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && f.fault == nil {
		f.fault = err
	}
	return n, err
}

var errDifferentConfigurationInput = errors.New("engine: the durable log opens with a different configuration input from the one this invocation composed; resume with the -config and -as-of the logged run started with")

// checkResumable refuses a durable log this invocation cannot continue: one
// recorded by another configuration or build, or one opening with a
// different configuration input from the one this invocation composed (a
// different -config or -as-of). It runs before this process creates its own
// log, so a refused restart leaves nothing new behind.
func checkResumable(recovery journal.Recovery, logHeader journal.LogHeader, configEnvelope event.Envelope) error {
	if recovery.Header != logHeader {
		// A changed start is the operator's to correct whatever else differs,
		// so it outranks the build-upgrade procedure.
		if len(recovery.Entries) > 0 {
			first := recovery.Entries[0].Envelope
			if !first.EventTime.Equal(configEnvelope.EventTime) || !first.RecordedAt.Equal(configEnvelope.RecordedAt) {
				return errDifferentConfigurationInput
			}
		}
		if err := buildUpgradeRefusal(recovery.Header, logHeader); err != nil {
			return err
		}
		return fmt.Errorf("engine: the durable log was recorded by run %s / %s, not this run %s / %s; a log resumes only under the configuration and build that recorded it",
			recovery.Header.ConfigurationHash, recovery.Header.StrategyVersion, logHeader.ConfigurationHash, logHeader.StrategyVersion)
	}
	if len(recovery.Entries) == 0 {
		return nil
	}
	if !bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), event.CanonicalEnvelopeBytes(configEnvelope)) {
		return errDifferentConfigurationInput
	}
	return nil
}

// buildUpgradeRefusal reports the refusal for a log that differs from this
// run only in its build, naming the procedure ADR 0027 sets: a run
// does not resume across builds. It returns nil for
// any other difference, which keeps the generic refusal.
func buildUpgradeRefusal(logged, current journal.LogHeader) error {
	if logged.ConfigurationHash != current.ConfigurationHash {
		return nil
	}
	loggedID, loggedRules, loggedBuild, loggedErr := event.DecomposeStrategyVersion(logged.StrategyVersion)
	currentID, currentRules, currentBuild, currentErr := event.DecomposeStrategyVersion(current.StrategyVersion)
	if loggedErr != nil || currentErr != nil || loggedID != currentID || loggedRules != currentRules || loggedBuild == currentBuild {
		return nil //nolint:nilerr // an unparseable or non-build difference keeps the generic refusal
	}
	return fmt.Errorf("engine: the durable log was recorded by build %q, not this build %q, and a run does not resume across builds (ADR 0027, owner decision 2026-10-16); "+
		"stop the run at the end of a session, keep the old log as evidence, and start a fresh run on the new build from a reconciled baseline (ADR 0019 recovery): see docs/runbooks/durable-log.md",
		loggedBuild, currentBuild)
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
