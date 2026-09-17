package main

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

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
	var report *replay.Report
	if reference != "" {
		want, err := decisionsFromJournal(reference)
		if err != nil {
			return err
		}
		report, err = replay.Diff(want, decisions)
		if err != nil {
			return fmt.Errorf("backtest: compare decisions: %w", err)
		}
	}
	var text bytes.Buffer
	for _, e := range decisions {
		line, id, err := decisionLine(e)
		if err != nil {
			return fmt.Errorf("backtest: decision %d: %w", e.Sequence, err)
		}
		if date != "" && e.EventTime.UTC().Format(time.DateOnly) != date {
			continue
		}
		if instrument != "" && id != instrument {
			continue
		}
		text.WriteString(line)
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
