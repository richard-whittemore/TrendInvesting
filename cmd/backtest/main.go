// Command backtest runs a declared configuration over a bar fixture and
// writes the run's journal, or verifies a journal it wrote earlier.
//
//	backtest -config <configuration.json> -bars <bars.json> -out <journal.jsonl>
//	backtest -verify <journal.jsonl>
//
// It is composition only: it wires the reducer, the fill simulator, the bar
// source and the journal writer together and contains no rules
// (AGENTS.md rule 7). See docs/running-a-backtest.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses the invocation and performs it, writing what it has to say to
// out. It exists separately from main so the command is testable as a
// function rather than as a process.
func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("backtest", flag.ContinueOnError)
	flags.SetOutput(out)
	configPath := flags.String("config", "", "path to the JSON strategy configuration to run")
	barsPath := flags.String("bars", "", "path to the JSON array of completed bars to run over")
	outPath := flags.String("out", "", "path to write the run's journal to")
	verifyPath := flags.String("verify", "", "path of a journal to verify instead of running a backtest")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *verifyPath != "" {
		return verify(*verifyPath, out)
	}

	var missing []error
	if *configPath == "" {
		missing = append(missing, errors.New("-config is required: the configuration the run is performed under"))
	}
	if *barsPath == "" {
		missing = append(missing, errors.New("-bars is required: the bar fixture the run is performed over"))
	}
	if *outPath == "" {
		missing = append(missing, errors.New("-out is required: where to write the run's journal"))
	}
	if err := errors.Join(missing...); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	return backtest(*configPath, *barsPath, *outPath, out)
}

// verify recomputes a journal's chain and reports what it found: whether the
// file was edited after it was written, and the final record hash a run
// registry anchors (ADR 0017).
func verify(path string, out io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backtest: open the journal to verify: %w", err)
	}
	defer func() { _ = file.Close() }()

	verification, err := journal.Verify(file)
	if err != nil {
		return fmt.Errorf("backtest: %s: %w", path, err)
	}

	report := fmt.Sprintf(`journal            %s
configuration hash %s
strategy version   %s
span               %s to %s
records            %d
final record hash  %s
chain              verified
`,
		path,
		verification.Header.ConfigurationHash,
		verification.Header.StrategyVersion,
		verification.Header.SpanStart.UTC().Format(time.RFC3339),
		verification.Header.SpanEnd.UTC().Format(time.RFC3339),
		verification.RecordCount,
		verification.FinalRecordHash)
	if _, err := io.WriteString(out, report); err != nil {
		return fmt.Errorf("backtest: report the verification: %w", err)
	}
	return nil
}
