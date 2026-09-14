// Command backtest runs a declared configuration over a bar fixture and
// writes the run's journal, verifies a journal it wrote earlier, or checks
// one for replay equivalence.
//
//	backtest -config <configuration.json> -bars <bars.json> -out <journal.jsonl>
//	     [-registry <runs/> -run-id <id> [-variant <id>]]
//	backtest -verify <journal.jsonl>
//	backtest -replay <journal.jsonl>
//	backtest -registry <runs/> -runs <configuration-hash>
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
	"path/filepath"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/buildinfo"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/registry"
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
	replayPath := flags.String("replay", "", "path of a journal to check for replay equivalence instead of running a backtest")
	registryPath := flags.String("registry", "", "path of the run registry to record this run in, or to read recorded runs from")
	runID := flags.String("run-id", "", "the identifier this run is recorded under in the registry")
	variant := flags.String("variant", registry.Baseline, "the Variant this run declares, or the Baseline")
	runsHash := flags.String("runs", "", "configuration hash whose recorded runs to list instead of running a backtest")
	if err := flags.Parse(args); err != nil {
		return err
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
	runFlags := []named{{"-config", *configPath}, {"-bars", *barsPath}, {"-out", *outPath}, {"-run-id", *runID}}
	if *runsHash == "" {
		runFlags = append(runFlags, named{"-registry", *registryPath})
	}
	if err := checkOneOperation([]named{{"-verify", *verifyPath}, {"-replay", *replayPath}, {"-runs", *runsHash}}, runFlags); err != nil {
		return err
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
	// A run is recorded under an identifier the operator chooses. Deriving
	// one here would have to come from the clock or a counter, and a run
	// registry whose identifiers are not reproducible cannot say that two
	// records describe the same run.
	switch {
	case *registryPath != "" && *runID == "":
		missing = append(missing, errors.New("-run-id is required with -registry: a run is recorded under an identifier the operator chooses"))
	case *registryPath == "" && *runID != "":
		missing = append(missing, errors.New("-run-id names a run in a registry, and -registry names no registry to record it in"))
	}
	if err := errors.Join(missing...); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	return backtest(options{
		configPath:   *configPath,
		barsPath:     *barsPath,
		outPath:      *outPath,
		registryPath: *registryPath,
		runID:        *runID,
		variant:      *variant,
		build:        buildinfo.Version,
	}, out)
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
			entry.Artefacts.JournalPath); err != nil {
			return fmt.Errorf("backtest: report the recorded runs: %w", err)
		}
	}
	return nil
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
