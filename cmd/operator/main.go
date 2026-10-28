// Command operator records the owner's instructions to a target-weight
// engine as inputs it reads, never as live reads of a file or command:
//
//	operator kill-switch -inbox <dir> -account <id> -state engaged|released -by <name> -detail <text> [-at <RFC3339>]
//	operator rebalance-cleared -inbox <dir> -account <id> -decision <decision id> -orders <id,id,...> -by <name> -detail <text> [-at <RFC3339>]
//	operator acknowledge -inbox <dir> -account <id> -alert <alert key> -by <name> [-detail <text>] [-at <RFC3339>]
//	operator approve -log <previous durable log> -account <id> -by <name> -out <approval.json> [-clear-incident <incident id>] [-positions <SYM=N,...> -cash <amount> -currency <code>] [-at <RFC3339>]
//	operator approve -first-run [-strategy boost|momentum] -account <id> -by <name> -positions <SYM=N,...> -cash <amount> -currency <code> -out <approval.json> [-at <RFC3339>]
//
// kill-switch, rebalance-cleared and acknowledge write one record into the
// engine's operator inbox (the engine's -operator), which the engine
// delivers as an operator.kill-switch, operator.rebalance.cleared or
// operator.alert.acknowledged input before the adapter's next input (ADR
// 0026, points 3 and 5; ADR 0019, amended 2026-09-24, "Alerting";
// internal/operator).
// approve writes the operator.run.approved file a fresh run is started with
// (the engine's -approval; ADR 0027, amended 2026-10-17), stating what the
// previous log ends in (internal/restart); with -first-run, the approval of
// an account's first run, which names no previous log and states the
// starting book the owner checked in IBKR (-positions, -cash, -currency),
// which a fresh run's approval may state too (ADR 0019 and ADR 0027,
// amended 2026-10-18). A momentum log's approval states the rebalance in
// progress the log ends with, or none, in place of Boost's latch (ADR 0026,
// point 5); a first run's names its strategy with -strategy. Every input is validated before
// anything is written, and nothing is ever overwritten (ADR 0018). It is
// composition only (AGENTS.md rule 7).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/operator"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

const usage = "usage: operator <kill-switch|rebalance-cleared|acknowledge|approve> [flags]; see docs/manual/operator.md"

// accountCurrency is the one currency a starting book is stated in: the IBKR
// adapter reads IBKR's balances in it (adapter/ibkr snapshot.BASE_CURRENCY)
// and compares a reading's cash with the book's, so a book in another would
// subtract cash in two currencies (ADR 0019 and ADR 0027, amended
// 2026-10-18). Multi-currency accounts are out of scope.
const accountCurrency = "USD"

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
	case "acknowledge":
		return acknowledge(args[1:], out)
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
		for _, id := range strings.Split(*orders, ",") {
			ids = append(ids, strings.TrimSpace(id))
		}
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

// acknowledge records the owner's acknowledgement of the alert whose key the
// alert states; the reporter stops repeating that alert once the engine has
// journalled it (ADR 0019, amended 2026-09-24, "Alerting").
func acknowledge(args []string, out io.Writer) error {
	flags, c := inboxFlags("acknowledge", out)
	key := flags.String("alert", "", "the alert key the alert ends with")
	if err := parse(flags, args); err != nil {
		return err
	}
	// The key is copied from an alert; a trailing newline or a stray space
	// would name no alert, and the acknowledgement would silently do nothing.
	trimmed := strings.TrimSpace(*key)
	if strings.ContainsAny(trimmed, "\r\n") {
		return fmt.Errorf("operator: -alert must hold no newline: it is one line, the alert key exactly as the alert ends with it, not %q", *key)
	}
	key = &trimmed
	at, err := when(*c.at)
	if err != nil {
		return err
	}
	r, err := operator.AlertAcknowledged(event.AlertAcknowledgedPayload{
		AccountID: *c.account, AlertKey: *key, AcknowledgedBy: *c.by, AcknowledgedAt: at, Detail: *c.detail,
	})
	if err != nil {
		return err
	}
	return record(*c.inbox, r, "alert "+*key, out)
}

// record installs r in inbox under its file name, whole and never over
// another record: written to a temporary name the engine does not read,
// flushed, linked into place, and the inbox flushed, before it is reported.
// The inbox belongs to the user that writes the owner's instructions, which
// the engine does not run as (ti-operator on the server; ADR 0026,
// kill-switch amendment), and the engine reads it through its group, so a
// record is refused unless this tool runs as the inbox's owner: run as
// another user it would put an instruction in the wrong hands, or leave one
// the engine cannot read.
func record(inbox string, r operator.Record, what string, out io.Writer) error {
	if inbox == "" {
		return errors.New("operator: -inbox is required: the engine's operator inbox, its -operator folder")
	}
	info, err := os.Stat(inbox)
	if err != nil {
		return fmt.Errorf("operator: the inbox: %w", err)
	}
	if owner, known := inboxOwner(info); known && owner != effectiveUser() {
		return fmt.Errorf("operator: the inbox %s belongs to user %d, the user that writes the owner's instructions, but this tool runs as user %d: run it as the inbox's owner, `sudo -u ti-operator operator ...` on the server (docs/manual/operator.md)",
			inbox, owner, effectiveUser())
	}
	path := filepath.Join(inbox, r.FileName())
	if err := install(path, r.Encode()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "recorded %s (%s) in %s; the engine delivers it before the adapter's next input and journals it in its durable log\n", r.ID, what, path)
	return err
}

// syncDirectory flushes dir, so a name just linked or created in it
// survives a power loss along with the file it names (ADR 0027 §3). It is
// the seam a test observes the flush through.
var syncDirectory = func(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

// inboxOwner returns the user that owns the inbox, the user that writes the
// owner's instructions, when the file system says; effectiveUser is the user this tool runs as. They
// are the seams a test sets a different owner through.
var (
	inboxOwner = func(info os.FileInfo) (uid int, known bool) {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return int(st.Uid), true
	}
	effectiveUser = os.Geteuid
)

// install writes raw at path, refusing to replace anything there. The
// record is mode 0640 in the inbox's group, the engine's: the engine reads
// it through that group, and only its writer can change it.
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
	if err := readableByTheInboxsGroup(file, dir); err != nil {
		return fmt.Errorf("operator: make %s readable by the engine's group: %w", path, err)
	}
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
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("operator: flush the inbox %s, so the record's name survives a power loss: %w", dir, err)
	}
	return nil
}

// readableByTheInboxsGroup gives file the group of the folder dir and mode
// 0640. A new file takes its writer's group, not its folder's, so the
// group is set explicitly.
func readableByTheInboxsGroup(file *os.File, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := file.Chown(-1, int(st.Gid)); err != nil {
			return err
		}
	}
	return file.Chmod(0o640)
}

func approve(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("operator approve", flag.ContinueOnError)
	flags.SetOutput(out)
	logPath := flags.String("log", "", "the previous run's durable log, the latest run in the engine's -log folder")
	account := flags.String("account", "", "the account the engine decides")
	by := flags.String("by", "", "who approves")
	outPath := flags.String("out", "", "where to write the approval, the engine's -approval file")
	clearIncident := flags.String("clear-incident", "", "the incident the previous run ended in, to clear it (ADR 0019's recovery procedure)")
	saleAttempted := flags.Bool("sale-attempted", false, "state the incident's one sale back to 98% as attempted, for the sale you made by hand for that incident and the log does not record, with the book after it: -positions, -cash and -currency (ADR 0019)")
	firstRun := flags.Bool("first-run", false, "approve the account's first run: no -log, and the starting book stated")
	strategy := flags.String("strategy", event.TargetWeightStrategyBoost, "with -first-run, the configuration's strategy_id: boost (the joint account) or momentum (an IRA); a fresh run's is read from -log")
	positions := flags.String("positions", "", "the starting book's holdings checked in IBKR, SYM=shares,...; empty when none")
	cash := flags.String("cash", "", "the starting book's total cash (IBKR's TotalCashValue)")
	currency := flags.String("currency", "", "the starting book's cash currency, such as USD")
	neverSent := flags.String("never-sent", "", "the adapter's settlements of the orders the previous run released and it never sent, its PENDING_FILE.never-sent file, written by `relay.py settle-ended-run` once that run ended: a sale back to 98% settled there is stated not attempted, so the fresh run tries it again (ADR 0019)")
	atFlag := flags.String("at", "", "when, as an RFC 3339 time; now when omitted")
	if err := parse(flags, args); err != nil {
		return err
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case *firstRun && *logPath != "":
		return errors.New("operator: -first-run approves an account's first run, which has no previous log: leave out -log")
	case *firstRun && set["clear-incident"]:
		return errors.New("operator: -first-run approves an account's first run, which has no incident to clear: leave out -clear-incident")
	case *firstRun && *neverSent != "":
		return errors.New("operator: -first-run approves an account's first run, which no earlier run left an order for: leave out -never-sent")
	case *firstRun && *saleAttempted:
		return errors.New("operator: -first-run approves an account's first run, which has no incident whose sale to state: leave out -sale-attempted")
	case *saleAttempted && (!set["positions"] || !set["cash"] || !set["currency"]):
		return errors.New("operator: -sale-attempted states a sale made by hand, so state the book after it: -positions, -cash and -currency, read in IBKR once the sale has filled")
	case *firstRun && *strategy != event.TargetWeightStrategyBoost && *strategy != event.TargetWeightStrategyMomentum:
		return fmt.Errorf("operator: -strategy %q: the configuration's strategy_id is %s or %s", *strategy, event.TargetWeightStrategyBoost, event.TargetWeightStrategyMomentum)
	case !*firstRun && set["strategy"]:
		return errors.New("operator: -strategy names a first run's strategy; a fresh run's is read from -log")
	case !*firstRun && *logPath == "":
		return errors.New("operator: -log is required: the previous run's durable log (or -first-run for an account's first run)")
	case *outPath == "":
		return errors.New("operator: -out is required: where to write the approval")
	}
	at, err := when(*atFlag)
	if err != nil {
		return err
	}
	book, err := startingBook(set, *positions, *cash, *currency, *firstRun)
	if err != nil {
		return err
	}
	// A starting book's approval time is its cutoff: the adapter counts
	// only the fills at or after it (ADR 0019, amended 2026-10-18: a book
	// and the movements that explain it share one cutoff). A time later
	// than now would leave out fills already in the account, so it is
	// refused; the owner approves right after reading the book.
	if book != nil && at.After(time.Now()) {
		return fmt.Errorf("operator: -at %s is later than now: a starting book's approval time is its cutoff, so approve right after reading the book in IBKR, leaving out -at (docs/runbooks/first-paper-order.md)", at.Format(time.RFC3339))
	}
	if *firstRun {
		approval := event.OperatorRunApprovedPayload{
			AccountID: *account, ApprovedBy: *by, ApprovedAt: at,
			Boost: event.BoostStateInForce{Latched: new(false)}, StartingBook: book,
		}
		if *strategy == event.TargetWeightStrategyMomentum {
			// An IRA's first run: no latch, and no rebalance in progress,
			// since no earlier run released an order.
			approval.Boost, approval.Momentum = event.BoostStateInForce{}, &event.MomentumStateInForce{}
		}
		if err := approval.Validate(); err != nil {
			return fmt.Errorf("operator: %w", err)
		}
		// A validated approval marshals (validated-payload-json).
		raw, _ := json.MarshalIndent(approval, "", "  ")
		if err := create(*outPath, append(raw, '\n')); err != nil {
			return err
		}
		return describeFirstRun(out, approval, *outPath)
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
	var settlements []event.NeverSentSettlement
	if *neverSent != "" {
		raw, err := os.ReadFile(*neverSent)
		if err != nil {
			return fmt.Errorf("operator: read the adapter's settlements: %w", err)
		}
		if settlements, err = restart.ReadSettlements(raw, *neverSent); err != nil {
			return err
		}
	}
	end, err := restart.ReadEndState(recovery.Entries, *account, name, settlements...)
	if err != nil {
		return err
	}
	approval, err := restart.ProposeApproval(end, restart.ApprovalRequest{
		AccountID: *account, PreviousLog: name, PreviousLogFinalHash: recovery.FinalRecordHash,
		ApprovedBy: *by, ApprovedAt: at, ClearIncident: *clearIncident, SaleAttempted: *saleAttempted, StartingBook: book != nil,
	})
	if err != nil {
		return err
	}
	if book != nil {
		approval.StartingBook = book
		if err := approval.Validate(); err != nil {
			return fmt.Errorf("operator: %w", err)
		}
	}
	// A validated approval marshals (validated-payload-json).
	raw, _ := json.MarshalIndent(approval, "", "  ")
	if err := create(*outPath, append(raw, '\n')); err != nil {
		return err
	}
	return describe(out, name, end, approval, *outPath)
}

// startingBook reads the starting book from -positions, -cash and
// -currency: nil when none of them is set and none is needed (a fresh run
// that takes the previous run's book), else all three, read strictly, and
// required for a first run. set names the flags given.
func startingBook(set map[string]bool, positions, cash, currency string, required bool) (*event.StartingBook, error) {
	if !required && !set["positions"] && !set["cash"] && !set["currency"] {
		return nil, nil
	}
	for _, name := range []string{"positions", "cash", "currency"} {
		if !set[name] {
			return nil, fmt.Errorf("operator: -%s is required: a starting book states the holdings (-positions, empty when none), the total cash (-cash) and its currency (-currency) checked in IBKR", name)
		}
	}
	if currency != accountCurrency {
		return nil, fmt.Errorf("operator: -currency %q: the adapter reads IBKR's cash in %s, so state IBKR's %s TotalCashValue with -currency %s", currency, accountCurrency, accountCurrency, accountCurrency)
	}
	book := &event.StartingBook{Positions: []event.AccountPosition{}, Currency: currency}
	seen := map[string]bool{}
	if positions != "" {
		for _, item := range strings.Split(positions, ",") {
			symbol, shares, ok := strings.Cut(item, "=")
			quantity, err := strconv.ParseInt(shares, 10, 64)
			if !ok || symbol == "" || err != nil {
				return nil, fmt.Errorf("operator: -positions %q: %q is not SYM=shares, a whole number of shares", positions, item)
			}
			if seen[symbol] {
				return nil, fmt.Errorf("operator: -positions %q names %s twice", positions, symbol)
			}
			seen[symbol] = true
			book.Positions = append(book.Positions, event.AccountPosition{InstrumentID: symbol, Quantity: quantity})
		}
	}
	sort.Slice(book.Positions, func(i, j int) bool { return book.Positions[i].InstrumentID < book.Positions[j].InstrumentID })
	amount, err := strconv.ParseFloat(cash, 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil, fmt.Errorf("operator: -cash %q is not an amount of cash", cash)
	}
	book.TotalCash = amount
	if err := book.Validate(); err != nil {
		return nil, fmt.Errorf("operator: %w", err)
	}
	return book, nil
}

// describeFirstRun says what a first run's approval states and what the
// operator does next.
func describeFirstRun(out io.Writer, approval event.OperatorRunApprovedPayload, path string) error {
	var b strings.Builder
	b.WriteString("the account's first run starts from the book you checked in IBKR: ")
	writeBook(&b, *approval.StartingBook)
	fmt.Fprintf(&b, "wrote %s: start the first run with -approval %s (docs/runbooks/first-paper-order.md)\n", path, path)
	_, err := io.WriteString(out, b.String())
	return err
}

// writeBook writes the holdings and cash of book, as the owner typed them,
// and what the adapter does with it, so a typo is seen before the run starts.
func writeBook(b *strings.Builder, book event.StartingBook) {
	b.WriteString("holdings")
	if len(book.Positions) == 0 {
		b.WriteString(" none")
	}
	for _, p := range book.Positions {
		fmt.Fprintf(b, " %s %d", p.InstrumentID, p.Quantity)
	}
	fmt.Fprintf(b, ", total cash %s %s; the adapter compares its first reading after a close with it\n", strconv.FormatFloat(book.TotalCash, 'f', -1, 64), book.Currency)
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
	if err := file.Sync(); err != nil {
		return fmt.Errorf("operator: flush the approval: %w", err)
	}
	dir := filepath.Dir(path)
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("operator: flush %s, so the approval's name survives a power loss: %w", dir, err)
	}
	return nil
}

// describe says what the approval states and what the operator does next.
func describe(out io.Writer, name string, end restart.EndState, approval event.OperatorRunApprovedPayload, path string) error {
	var b strings.Builder
	switch r := end.Rebalance; {
	case !end.Momentum:
		fmt.Fprintf(&b, "%s ends with Boost latched %t", name, end.Latched)
	case r == nil:
		fmt.Fprintf(&b, "%s ends with no rebalance in progress", name)
	default:
		fmt.Fprintf(&b, "%s ends mid-rebalance: %s, decided at %s, %s, orders %s not terminal; the fresh run sends nothing for it, and decides no later month, until you deal with each order at IBKR and clear it with `operator rebalance-cleared -decision %s -orders %s` (docs/runbooks/missed-rebalance.md)",
			name, r.DecisionID, r.DecisionTime.UTC().Format(time.RFC3339), r.Phase, strings.Join(r.ClientOrderIDs, ", "), r.DecisionID, strings.Join(r.ClientOrderIDs, ","))
	}
	switch {
	case approval.IncidentID != "":
		fmt.Fprintf(&b, "; the approval clears incident %s (%s)", approval.IncidentID, end.State)
	case end.IncidentID != "":
		fmt.Fprintf(&b, "; the approval carries incident %s in force, %s", end.IncidentID, end.State)
		if scope := slices.Concat(end.Scope, end.ScopeCash); len(scope) > 0 {
			fmt.Fprintf(&b, " on %s", strings.Join(scope, ", "))
		}
		fmt.Fprintf(&b, ", its one sale back to 98%% attempted %t", *approval.IncidentInForce.SaleAttempted)
		if !end.SaleAttempted && *approval.IncidentInForce.SaleAttempted {
			b.WriteString(" (stated by -sale-attempted: the log records no attempt)")
		}
		// A sale stated attempted (-sale-attempted: the owner sold by hand)
		// stays attempted whatever is settled, so the fresh run makes no sale
		// (ADR 0019, amended 2026-10-17, reading 4).
		for _, s := range approval.IncidentInForce.SaleSettlements {
			fmt.Fprintf(&b, "; sale %s settled by the adapter as never sent (%s), so it is retired", s.Retirement.ClientOrderID, s.Evidence)
			if *approval.IncidentInForce.SaleAttempted {
				b.WriteString(", and the sale stays attempted as -sale-attempted states: the fresh run makes no sale")
			} else {
				b.WriteString(" and the fresh run tries the sale again under a new client order ID")
			}
		}
	default:
		b.WriteString("; it ended normal")
	}
	b.WriteString("\n")
	if approval.StartingBook != nil {
		b.WriteString("the fresh run starts from the book you checked in IBKR, not the previous run's last close: ")
		writeBook(&b, *approval.StartingBook)
	}
	fmt.Fprintf(&b, "wrote %s: start the fresh run with -approval %s (docs/runbooks/durable-log.md, \"Restarting an engine\")\n", path, path)
	if end.KillSwitchEngaged {
		b.WriteString("the kill switch was engaged when it ended; a fresh run starts with it released, so record it again with `operator kill-switch -state engaged` before starting the engine\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}
