// Command journal-query answers questions about what a run did, from its
// journals, and never writes to them.
//
//	journal-query <report> [-account A] [-instrument I] [-from YYYY-MM-DD] [-to YYYY-MM-DD]
//	              [-format text|csv|json] [-final-hash H] <journal>...
//
// Reports: orders, fills, refusals, totals, positions, reconcile. It is
// composition only (AGENTS.md rule 7): it reads the named files, hands their
// bytes to internal/journalquery and prints the table that comes back.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// reports maps each report name to the query that builds it.
var reports = map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
	"operator":  journalquery.Operator,
	"orders":    journalquery.Orders,
	"fills":     journalquery.Fills,
	"refusals":  journalquery.Refusals,
	"positions": journalquery.Positions,
	"reconcile": journalquery.Reconcile,
	"totals":    journalquery.Totals,
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: journal-query <orders|fills|refusals|totals|positions|reconcile|operator> [flags] journal [journal ...]")
	}
	report := args[0]
	flags := flag.NewFlagSet("journal-query "+report, flag.ContinueOnError)
	flags.SetOutput(out)
	account := flags.String("account", "", "exact account ID to report")
	instrument := flags.String("instrument", "", "exact instrument ID to report")
	from := flags.String("from", "", "first UTC date to report, YYYY-MM-DD, inclusive")
	format := flags.String("format", journalquery.FormatText, "output form: text, csv or json")
	allowTrailing := flags.Bool("allow-trailing-fills", false, "reconcile: accept fills recorded after an account's last snapshot")
	finalHash := flags.String("final-hash", "", "reconcile: the run's final record hash from the run registry; the journal must end on it")
	to := flags.String("to", "", "last UTC date to report, YYYY-MM-DD, inclusive")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("name at least one journal to query")
	}
	if !journalquery.ValidFormat(*format) {
		return fmt.Errorf("format %q is not text, csv or json", *format)
	}
	filter, err := journalquery.NewFilter(*account, *instrument, *from, *to)
	if err != nil {
		return err
	}
	anchored := false
	flags.Visit(func(f *flag.Flag) { anchored = anchored || f.Name == "final-hash" })
	if anchored && report != "reconcile" {
		return errors.New("-final-hash belongs to reconcile")
	}
	if anchored && flags.NArg() != 1 {
		return errors.New("-final-hash names one run's final record hash, so name exactly one journal with it")
	}
	filter.AllowTrailingFills = *allowTrailing
	build, ok := reports[report]
	if !ok {
		return fmt.Errorf("unknown report %q", report)
	}
	var journals []journalquery.Journal
	for _, path := range flags.Args() {
		data, err := os.ReadFile(path) // #nosec G703 -- a command reads the journal its operator named, and only reads it.
		if err != nil {
			return err
		}
		j, err := journalquery.Load(path, data)
		if err != nil {
			return err
		}
		if anchored {
			if err := j.CheckFinalHash(*finalHash); err != nil {
				return err
			}
		}
		journals = append(journals, j)
	}
	table, buildErr := build(journals, filter)
	// A reconciliation that finds disagreements or fills no snapshot has covered returns its full table with
	// the error: print the rows, then fail, so nothing is hidden.
	var disagreement *journalquery.DisagreementError
	var trailing *journalquery.TrailingFillsError
	if buildErr != nil && !errors.As(buildErr, &disagreement) && !errors.As(buildErr, &trailing) {
		return buildErr
	}
	if err := table.Write(out, *format); err != nil {
		return err
	}
	return buildErr
}
