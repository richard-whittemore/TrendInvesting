package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// decisionsFromJournal reads the journal at path and returns the decisions
// it recorded — the stream replay.Diff compares.
//
// Unlike -replay, -diff has no reducer of its own to re-derive a comparison
// from: it takes both sides' decisions straight from the file, so a forged
// or edited record would otherwise be treated as evidence rather than
// refused. journal.Verify (was this file edited after it was written) and
// journal.CheckIdentity (does every record actually describe the run the
// header names) both run before journal.Split hands back a stream to
// compare — the same two checks -replay's own journal reading applies,
// reused rather than re-implemented, and left unconditional: an audit tool
// with an opt-out to skip validating its evidence would eventually be run
// with it.
func decisionsFromJournal(path string) ([]event.Envelope, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backtest: open %s: %w", path, err)
	}

	if _, err := journal.Verify(bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("backtest: %s: %w", path, err)
	}

	header, records, err := journal.Read(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("backtest: %s: %w", path, err)
	}
	if err := journal.CheckIdentity(header, records); err != nil {
		return nil, fmt.Errorf("backtest: %s: %w", path, err)
	}
	_, decisions, err := journal.Split(records)
	if err != nil {
		return nil, fmt.Errorf("backtest: %s: %w", path, err)
	}
	return decisions, nil
}

// doDiff compares two journals' decisions and reports where they first
// disagree: "these two runs disagree" pinpointed to a sequence, an event,
// and a field, rather than merely discovered.
//
// The comparison is decisions only, the same stream replay equivalence
// itself compares (ADR 0017) rather than the whole interleaved record
// stream: two journals can legitimately record different inputs — a
// different bar fixture, a different arrival schedule — and the question
// this asks is whether they decided the same things, not whether they were
// fed the same events.
//
// A divergence is reported in two forms at once: a JSON-encoded
// replay.Report on out for a machine consumer such as CI, and a
// human-readable line naming the same divergence in the returned error.
func doDiff(wantPath, gotPath string, out io.Writer) error {
	want, err := decisionsFromJournal(wantPath)
	if err != nil {
		return err
	}
	got, err := decisionsFromJournal(gotPath)
	if err != nil {
		return err
	}

	report, err := replay.Diff(want, got)
	if err != nil {
		return fmt.Errorf("backtest: diff %s and %s: %w", wantPath, gotPath, err)
	}
	if report == nil {
		if _, err := fmt.Fprintf(out, "%s and %s report no divergence\n", wantPath, gotPath); err != nil {
			return fmt.Errorf("backtest: report the diff: %w", err)
		}
		return nil
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("backtest: encode the divergence report: %w", err)
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("backtest: report the diff: %w", err)
	}
	return fmt.Errorf("backtest: %s vs %s: %s", wantPath, gotPath, report.String())
}
