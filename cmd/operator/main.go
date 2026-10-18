// Command operator records the owner's instructions to a target-weight
// engine as inputs it reads, never as live reads of a file or command:
//
//	operator kill-switch -inbox <dir> -account <id> -state engaged|released -by <name> -detail <text> [-at <RFC3339>]
//	operator rebalance-cleared -inbox <dir> -account <id> -decision <decision id> -orders <id,id,...> -by <name> -detail <text> [-at <RFC3339>]
//	operator approve -log <previous durable log> -account <id> -by <name> -out <approval.json> [-clear-incident <incident id>] [-at <RFC3339>]
//
// kill-switch and rebalance-cleared write one record into the engine's
// operator inbox (the engine's -operator), which the engine delivers as an
// operator.kill-switch or operator.rebalance.cleared input before the
// adapter's next input (ADR 0026, points 3 and 5; internal/operator).
// approve writes the operator.run.approved file a fresh run is started with
// (the engine's -approval; ADR 0027, amended 2026-10-17), stating what the
// previous log ends in (internal/restart). Every input is validated before
// anything is written, and nothing is ever overwritten (ADR 0018). It is
// composition only (AGENTS.md rule 7).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

const usage = "usage: operator <kill-switch|rebalance-cleared|approve> [flags]; see docs/manual/operator.md"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "kill-switch":
		return killSwitch(args[1:], out)
	case "rebalance-cleared":
		return rebalanceCleared(args[1:], out)
	case "approve":
		return approve(args[1:], out)
	}
	return fmt.Errorf("operator: unknown command %q; %s", args[0], usage)
}

// common are the flags every inbox record takes.
type common struct {
	inbox, account, by, detail, at *string
}

func inboxFlags(name string, out io.Writer) (*flag.FlagSet, common) {
	flags := flag.NewFlagSet("operator "+name, flag.ContinueOnError)
	flags.SetOutput(out)
	return flags, common{
		inbox:   flags.String("inbox", "", "the engine's operator inbox, its -operator folder"),
		account: flags.String("account", "", "the account the engine decides"),
		by:      flags.String("by", "", "who records this"),
		detail:  flags.String("detail", "", "what was done and why"),
		at:      flags.String("at", "", "when, as an RFC 3339 time; now when omitted"),
	}
}

// parse parses args and checks what every command needs.
func parse(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%s: unexpected arguments %v", flags.Name(), flags.Args())
	}
	return nil
}

// when is -at, or now to the second.
func when(at string) (time.Time, error) {
	if at == "" {
		return time.Now().UTC().Truncate(time.Second), nil
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, fmt.Errorf("operator: -at must be an RFC 3339 time: %w", err)
	}
	return t.UTC(), nil
}

func killSwitch(args []string, out io.Writer) error {
	flags, c := inboxFlags("kill-switch", out)
	state := flags.String("state", "", "engaged or released; required")
	if err := parse(flags, args); err != nil {
		return err
	}
	var engaged bool
	switch *state {
	case "engaged":
		engaged = true
	case "released":
	default:
		return fmt.Errorf("operator: -state must be engaged or released, not %q: the switch is stated, never assumed", *state)
	}
	at, err := when(*c.at)
	if err != nil {
		return err
	}
	r, err := operator.KillSwitch(event.KillSwitchPayload{AccountID: *c.account, AsOf: at, Engaged: engaged, Operator: *c.by, Detail: *c.detail})
	if err != nil {
		return err
	}
	return record(*c.inbox, r, *state, out)
}

func rebalanceCleared(args []string, out io.Writer) error {
	flags, c := inboxFlags("rebalance-cleared", out)
	decision := flags.String("decision", "", "the strategy.targets.decided ID of the rebalance in progress")
	orders := flags.String("orders", "", "every order of it not terminal, client order IDs separated by commas")
	if err := parse(flags, args); err != nil {
		return err
	}
	at, err := when(*c.at)
	if err != nil {
		return err
	}
	var ids []string
	if *orders != "" {
		ids = strings.Split(*orders, ",")
		sort.Strings(ids)
	}
	r, err := operator.RebalanceCleared(event.RebalanceClearedPayload{
		AccountID: *c.account, AsOf: at, DecisionID: *decision, ClientOrderIDs: ids, Operator: *c.by, Detail: *c.detail,
	})
	if err != nil {
		return err
	}
	return record(*c.inbox, r, "decision "+*decision, out)
}

// record installs r in inbox under its file name, whole and never over
// another record: written to a temporary name the engine does not read,
// flushed, then linked into place.
func record(inbox string, r operator.Record, what string, out io.Writer) error {
	if inbox == "" {
		return errors.New("operator: -inbox is required: the engine's operator inbox, its -operator folder")
	}
	path := filepath.Join(inbox, r.FileName())
	if err := install(path, r.Encode()); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "recorded %s (%s) in %s; the engine delivers it before the adapter's next input and journals it in its durable log\n", r.ID, what, path)
	return err
}

// install writes raw at path, refusing to replace anything there.
func install(path string, raw []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".operator-*.partial")
	if err != nil {
		return fmt.Errorf("operator: write into %s: %w", dir, err)
	}
	temporary := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()
	if _, err := file.Write(raw); err != nil {
		return fmt.Errorf("operator: write %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("operator: flush %s: %w", path, err)
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("operator: %s already holds a record, which is never overwritten (ADR 0018); record it again a second later", path)
		}
		return fmt.Errorf("operator: install %s: %w", path, err)
	}
	return nil
}

func approve(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("operator approve", flag.ContinueOnError)
	flags.SetOutput(out)
	logPath := flags.String("log", "", "the previous run's durable log, the latest run in the engine's -log folder")
	account := flags.String("account", "", "the account the engine decides")
	by := flags.String("by", "", "who approves")
	outPath := flags.String("out", "", "where to write the approval, the engine's -approval file")
	clearIncident := flags.String("clear-incident", "", "the incident the previous run ended in, to clear it (ADR 0019's recovery procedure)")
	atFlag := flags.String("at", "", "when, as an RFC 3339 time; now when omitted")
	if err := parse(flags, args); err != nil {
		return err
	}
	switch {
	case *logPath == "":
		return errors.New("operator: -log is required: the previous run's durable log")
	case *outPath == "":
		return errors.New("operator: -out is required: where to write the approval")
	}
	at, err := when(*atFlag)
	if err != nil {
		return err
	}
	file, err := os.Open(*logPath)
	if err != nil {
		return fmt.Errorf("operator: open the previous log: %w", err)
	}
	recovery, err := journal.RecoverLog(file)
	_ = file.Close()
	if err != nil {
		return fmt.Errorf("operator: %s cannot be read as a durable log (docs/runbooks/durable-log.md): %w", *logPath, err)
	}
	name := filepath.Base(*logPath)
	if restart.CutShort(recovery) {
		return fmt.Errorf("operator: %s holds nothing past its configuration: its start was cut short, so resume it with the engine's -resume, and the approval it was started with if it had one (docs/runbooks/durable-log.md)", name)
	}
	end, err := restart.ReadEndState(recovery.Entries, name)
	if err != nil {
		return err
	}
	approval, err := restart.ProposeApproval(end, restart.ApprovalRequest{
		AccountID: *account, PreviousLog: name, PreviousLogFinalHash: recovery.FinalRecordHash,
		ApprovedBy: *by, ApprovedAt: at, ClearIncident: *clearIncident,
	})
	if err != nil {
		return err
	}
	// A validated approval marshals (validated-payload-json).
	raw, _ := json.MarshalIndent(approval, "", "  ")
	if err := create(*outPath, append(raw, '\n')); err != nil {
		return err
	}
	return describe(out, name, end, approval, *outPath)
}

// create writes raw at path, refusing to replace anything there.
func create(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("operator: %s already exists; an approval is evidence and is never overwritten (ADR 0018)", path)
		}
		return fmt.Errorf("operator: create the approval: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(raw); err != nil {
		return fmt.Errorf("operator: write the approval: %w", err)
	}
	return file.Sync()
}

// describe says what the approval states and what the operator does next.
func describe(out io.Writer, name string, end restart.EndState, approval event.OperatorRunApprovedPayload, path string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s ends with Boost latched %t", name, end.Latched)
	switch {
	case approval.IncidentID != "":
		fmt.Fprintf(&b, "; the approval clears incident %s (%s)", approval.IncidentID, end.State)
	case end.IncidentID != "":
		fmt.Fprintf(&b, "; the approval carries incident %s in force, %s", end.IncidentID, end.State)
		if len(end.Scope) > 0 {
			fmt.Fprintf(&b, " on %s", strings.Join(end.Scope, ", "))
		}
		fmt.Fprintf(&b, ", its one sale back to 98%% attempted %t", end.SaleAttempted)
	default:
		b.WriteString("; it ended normal")
	}
	fmt.Fprintf(&b, "\nwrote %s: start the fresh run with -approval %s (docs/runbooks/durable-log.md, \"Restarting an engine\")\n", path, path)
	if end.KillSwitchEngaged {
		b.WriteString("the kill switch was engaged when it ended; a fresh run starts with it released, so record it again with `operator kill-switch -state engaged` before starting the engine\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}
