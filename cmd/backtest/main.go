// Command backtest runs a declared configuration over a bar fixture and
// writes the run's journal, verifies a journal it wrote earlier, checks one
// for replay equivalence, or diffs two journals' decisions against each
// other, or reads a journal as human-readable decision sentences.
//
//	backtest -config <configuration.json> -bars <bars.json> -out <journal.jsonl>
//	     [-registry <runs/> -run-id <id> [-variant <id>]]
//	backtest -verify <journal.jsonl>
//	backtest -replay <journal.jsonl>
//	backtest -decisions <journal.jsonl> [-date YYYY-MM-DD] [-instrument ID] [-reference <journal.jsonl>]
//	backtest -registry <runs/> -runs <configuration-hash>
//	backtest -diff-want <journal.jsonl> -diff-got <journal.jsonl>
//
// It is composition only: it wires the reducer, the fill simulator, the bar
// source and the journal writer together and contains no rules
// (AGENTS.md rule 7). See docs/running-a-backtest.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
)

// main wires the run to an interrupt that cancels it rather than ending it.
//
// An interrupt reaches the run as a cancelled context and nothing else: it
// never ends the process, because a run killed between its last bar and its
// registry entry is the one outcome the registry cannot record, and a
// deliberately abandoned run is retained beside the completed ones (ADR
// 0012). The handler stays installed for the life of the command, so a second
// interrupt is absorbed too — the window it would otherwise open is exactly
// the finalisation this exists to protect. SIGKILL is still SIGKILL.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	// Released here rather than deferred: the exit below would skip a defer,
	// and the handler has to outlive the whole run, finalisation included.
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses the invocation and performs it, writing what it has to say to
// out. It exists separately from main so the command is testable as a
// function rather than as a process.
//
// ctx carries the operator's interrupt to the run and is not consulted by the
// operations that read something already written.
func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("backtest", flag.ContinueOnError)
	flags.SetOutput(out)
	configPath := flags.String("config", "", "path to the JSON strategy configuration to run")
	barsPath := flags.String("bars", "", "path to the JSON array of completed bars to run over")
	outPath := flags.String("out", "", "path to write the run's journal to")
	verifyPath := flags.String("verify", "", "path of a journal to verify instead of running a backtest")
	replayPath := flags.String("replay", "", "path of a journal to check for replay equivalence instead of running a backtest")
	registryPath := flags.String("registry", "", "path of the run registry to record this run in, or to read recorded runs from")
	runID := flags.String("run-id", "", "the identifier this run is recorded under in the registry")
	// Defaulted to empty rather than to registry.Baseline so that "the
	// operator declared a Variant" is distinguishable from "the operator
	// declared nothing". A flag that always carries a value cannot be checked
	// against the operations that would ignore it.
	variant := flags.String("variant", "", "the Variant this run declares; defaults to the Baseline")
	runsHash := flags.String("runs", "", "configuration hash whose recorded runs to list instead of running a backtest")
	diffWantPath := flags.String("diff-want", "", "path of the reference journal to diff against another (used together with -diff-got)")
	diffGotPath := flags.String("diff-got", "", "path of the candidate journal to diff against the reference (used together with -diff-want)")
	// A string rather than an int for the same reason -variant is: a flag
	// that always carries a value cannot be checked against the operations
	// that would ignore it.
	maxRecords := flags.String("max-records", "", "bounds the records this run holds in memory before its journal is written, checked between inputs; defaults to 2000000")
	decisionsPath := flags.String("decisions", "", "path of a journal to read as a human-readable decision log")
	decisionDate := flags.String("date", "", "UTC event date to display with -decisions (YYYY-MM-DD)")
	instrument := flags.String("instrument", "", "exact instrument ID to display with -decisions")
	reference := flags.String("reference", "", "reference journal to compare in full with -decisions")
	if err := flags.Parse(args); err != nil {
		return err
	}

	for _, option := range []named{{"-date", *decisionDate}, {"-instrument", *instrument}, {"-reference", *reference}} {
		if option.value != "" && *decisionsPath == "" {
			return fmt.Errorf("backtest: %s requires -decisions", option.flag)
		}
	}

	// -diff-want and -diff-got name one operation between them, so either
	// both are given or neither is. An audit command must not exit zero
	// having done something other than what was asked (docs/development.md
	// principle 4, fail closed), and half an operation is that: a lone
	// -diff-want would fall through to a backtest run. Checked before
	// checkOneOperation, which can say "these flags name more than one
	// operation" but not "half of one operation was given".
	if (*diffWantPath == "") != (*diffGotPath == "") {
		return errors.New("backtest: -diff-want and -diff-got must be given together")
	}

	// One invocation performs exactly one operation. Letting a mode win by
	// branch order would let an audit command exit zero having done something
	// other than what was asked — reporting a verified chain, say, while
	// silently discarding the replay the operator also requested.
	//
	// -registry is the one flag two operations share: it names where a run is
	// recorded, and where recorded runs are read from. It is therefore checked
	// as a run flag only when the invocation is not asking to read the
	// registry.
	runFlags := []named{{"-config", *configPath}, {"-bars", *barsPath}, {"-out", *outPath}, {"-run-id", *runID}, {"-variant", *variant}, {"-max-records", *maxRecords}}
	if *runsHash == "" {
		runFlags = append(runFlags, named{"-registry", *registryPath})
	}
	if err := checkOneOperation([]named{{"-decisions", *decisionsPath}, {"-verify", *verifyPath}, {"-replay", *replayPath}, {"-runs", *runsHash}, {"-diff-want and -diff-got", *diffWantPath}}, runFlags); err != nil {
		return err
	}

	if *decisionsPath != "" {
		return doDecisions(*decisionsPath, *decisionDate, *instrument, *reference, out)
	}
	if *verifyPath != "" {
		return verify(*verifyPath, out)
	}
	if *replayPath != "" {
		return doReplay(*replayPath, out)
	}
	if *runsHash != "" {
		if *registryPath == "" {
			return errors.New("backtest: -runs reads a registry; -registry names which one")
		}
		return listRuns(*registryPath, *runsHash, out)
	}
	if *diffWantPath != "" {
		return doDiff(*diffWantPath, *diffGotPath, out)
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
	// -run-id and -variant describe how a run is RECORDED, so neither means
	// anything without a registry to record it in. Accepting one without
	// -registry would leave an operator believing they had declared a Variant
	// when nothing kept the declaration — the failure that matters most for a
	// registry whose point is that the graveyard of failed Variants survives.
	if *registryPath == "" {
		for _, flag := range []named{{"-run-id", *runID}, {"-variant", *variant}} {
			if flag.value != "" {
				missing = append(missing, fmt.Errorf("%s describes how a run is recorded, and -registry names no registry to record it in", flag.flag))
			}
		}
	} else if *runID == "" {
		// A run is recorded under an identifier the operator chooses.
		// Deriving one here would have to come from the clock or a counter,
		// and a registry whose identifiers are not reproducible cannot say
		// that two records describe the same run.
		missing = append(missing, errors.New("-run-id is required with -registry: a run is recorded under an identifier the operator chooses"))
	}
	if err := errors.Join(missing...); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	recordBound, err := parseRecordBound(*maxRecords)
	if err != nil {
		return err
	}

	// The Baseline is what a run declares when it declares nothing, and it is
	// applied here rather than as the flag's default so that the check above
	// can tell the two apart.
	declaredVariant := *variant
	if *registryPath != "" && declaredVariant == "" {
		declaredVariant = registry.Baseline
	}

	return backtest(ctx, options{
		configPath:   *configPath,
		barsPath:     *barsPath,
		outPath:      *outPath,
		registryPath: *registryPath,
		runID:        *runID,
		variant:      declaredVariant,
		build:        buildinfo.Version,
		maxRecords:   recordBound,
	}, out)
}

// parseRecordBound reads -max-records: the bound on the records a run holds
// in memory before its journal is written (ADR 0017). An invocation that named
// no bound reads as zero, which options.recordBound resolves to the default.
func parseRecordBound(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	bound, err := strconv.Atoi(value)
	if err != nil || bound < 1 {
		return 0, fmt.Errorf("backtest: -max-records bounds the records a run holds in memory before its journal is written, and must be a whole number of at least 1, not %q", value)
	}
	return bound, nil
}

// named is a flag and the value the invocation gave it, empty when unset.
type named struct {
	flag, value string
}

// checkOneOperation refuses an invocation that names more than one operation.
// The modes that read something that already exists are mutually exclusive
// with each other and with the flags that describe a run to perform, so an
// operator who asks for two things is told rather than silently given one of
// them.
func checkOneOperation(modes, run []named) error {
	var named, runFlags []string
	for _, mode := range modes {
		if mode.value != "" {
			named = append(named, mode.flag)
		}
	}
	for _, flag := range run {
		if flag.value != "" {
			runFlags = append(runFlags, flag.flag)
		}
	}

	switch {
	case len(named) > 1:
		return fmt.Errorf("backtest: %s name different operations; give exactly one", strings.Join(named, " and "))
	case len(named) == 1 && len(runFlags) > 0:
		return fmt.Errorf("backtest: %s reads what already exists and would ignore %s; give one or the other", named[0], strings.Join(runFlags, ", "))
	}
	return nil
}

// registryStore reads a run registry rooted at a directory: the
// registry.Store the domain defines, implemented over the filesystem here
// because internal/ performs no I/O (docs/development.md).
//
// Directory listing goes through Readdirnames rather than os.ReadDir. Names
// are all registry.Store asks for, and the file metadata os.ReadDir would
// also return is what carries this toolchain's known os defect (GO-2026-4602,
// fixed in go1.25.8), which govulncheck reports for any caller of it. Reading
// names alone asks for less and is therefore unaffected.
type registryStore struct{ root string }

func (s registryStore) ReadDir(dir string) ([]string, error) {
	file, err := os.Open(s.path(dir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return file.Readdirnames(-1)
}

func (s registryStore) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(s.path(name))
}

func (s registryStore) path(name string) string {
	return filepath.Join(s.root, filepath.FromSlash(name))
}

// listRuns reports every run recorded under configurationHash, so that an
// operator holding a hash reaches the journals it produced — which `-replay`
// then reproduces — without reading the registry's layout by hand.
//
// A registry root that does not exist is reported rather than read as an
// empty registry: "this configuration has never been run" and "this registry
// is not there" are different findings, and only one is evidence.
func listRuns(root, configurationHash string, out io.Writer) error {
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("backtest: read the run registry: %w", err)
	}
	entries, err := registry.Runs(registryStore{root: root}, configurationHash)
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	if len(entries) == 0 {
		if _, err := fmt.Fprintf(out, "no run is recorded under %s\n", configurationHash); err != nil {
			return fmt.Errorf("backtest: report the recorded runs: %w", err)
		}
		return nil
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s to %s\t%s\n",
			entry.RunID, entry.Status, entry.Variant,
			entry.SpanStart.UTC().Format(time.RFC3339), entry.SpanEnd.UTC().Format(time.RFC3339),
			journalOf(root, entry)); err != nil {
			return fmt.Errorf("backtest: report the recorded runs: %w", err)
		}
	}
	return nil
}

// journalOf resolves a recorded journal path against the registry root it is
// stored relative to, so that what is printed is what `-replay` can be given.
func journalOf(root string, entry registry.Entry) string {
	if entry.Artefacts.JournalPath == "" {
		return "(no journal)"
	}
	return filepath.Join(root, filepath.FromSlash(entry.Artefacts.JournalPath))
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
