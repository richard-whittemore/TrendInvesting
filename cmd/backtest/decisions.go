package main

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// renderedDecision is one decision's explained line and the instrument it
// names, kept together so the display filters never re-render anything.
type renderedDecision struct {
	line       string
	instrument string
}

// decisionLines explains a whole stream, which is also what validates it:
// decisionLine checks the envelope, the declared schema version and the
// typed payload while it builds each sentence (ADR 0015). Candidate and
// reference decisions are both evidence, so both pass through here before
// any comparison rests on either; a stream that skipped it would have an
// unsupported schema or an invalid payload reported as a disagreement
// between the two runs rather than refused as evidence.
func decisionLines(decisions []event.Envelope, role string) ([]renderedDecision, error) {
	rendered := make([]renderedDecision, len(decisions))
	for i, e := range decisions {
		line, instrument, err := decisionLine(e)
		if err != nil {
			return nil, fmt.Errorf("backtest: %s %d: %w", role, e.Sequence, err)
		}
		rendered[i] = renderedDecision{line: line, instrument: instrument}
	}
	return rendered, nil
}

// doDecisions renders recorded evidence only, after the same unconditional
// chain and identity checks as the journal diff (ADR 0017).
// Filters select displayed decisions; the reference comparison remains complete.
func doDecisions(path, date, instrument, reference string, out io.Writer) error {
	if date != "" {
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return fmt.Errorf("backtest: -date must be YYYY-MM-DD: %w", err)
		}
	}
	decisions, err := decisionsFromJournal(path)
	if err != nil {
		return err
	}
	rendered, err := decisionLines(decisions, "decision")
	if err != nil {
		return err
	}
	var report *replay.Report
	if reference != "" {
		want, err := decisionsFromJournal(reference)
		if err != nil {
			return err
		}
		if _, err := decisionLines(want, "reference decision"); err != nil {
			return err
		}
		report, err = replay.Diff(want, decisions)
		if err != nil {
			return fmt.Errorf("backtest: compare decisions: %w", err)
		}
	}
	var text bytes.Buffer
	for i, e := range decisions {
		if date != "" && e.EventTime.UTC().Format(time.DateOnly) != date {
			continue
		}
		if instrument != "" && rendered[i].instrument != instrument {
			continue
		}
		text.WriteString(rendered[i].line)
		text.WriteByte('\n')
	}
	if reference != "" {
		if report == nil {
			text.WriteString("no divergence\n")
		} else {
			text.WriteString(report.String() + "\n")
		}
	}
	if _, err := out.Write(text.Bytes()); err != nil {
		return fmt.Errorf("backtest: report decisions: %w", err)
	}
	if report != nil {
		return fmt.Errorf("backtest: %s", report.String())
	}
	return nil
}
