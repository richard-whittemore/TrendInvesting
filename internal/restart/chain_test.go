package restart_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

func hashOf(name string) string { return fmt.Sprintf("sha256:%064x", len(name)*7919+int(name[0])) }

// configOf is the run's configuration input, naming its account and
// mode, the two a chain is matched on.
func configOf(t *testing.T, name, account, mode string) journal.Entry {
	t.Helper()
	return recorded(t, journal.KindInput, "cfg:"+name, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion,
		event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: account, Mode: mode})
}

// unapproved is a synthetic log of account joint in mode holding its
// configuration and no approval.
func unapproved(t *testing.T, name, mode string) journal.Recovery {
	t.Helper()
	return journal.Recovery{Entries: []journal.Entry{configOf(t, name, "joint", mode)}, FinalRecordHash: hashOf(name)}
}

// run is a synthetic paper log of account joint whose final hash is
// hashOf(name), holding its configuration and then one approval, which
// names previous, or states the starting book when previous is "".
func run(t *testing.T, name, previous string, extra ...journal.Entry) journal.Recovery {
	t.Helper()
	return runOf(t, name, previous, "joint", event.TradingModePaper, extra...)
}

func runOf(t *testing.T, name, previous, account, mode string, extra ...journal.Entry) journal.Recovery {
	t.Helper()
	latched := false
	p := event.OperatorRunApprovedPayload{
		AccountID: "joint", ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC),
		Boost: event.BoostStateInForce{Latched: &latched},
	}
	if previous == "" {
		p.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 1000, Currency: "USD"}
	} else {
		p.PreviousLog, p.PreviousLogFinalHash = previous, hashOf(previous)
	}
	entries := []journal.Entry{configOf(t, name, account, mode), recorded(t, journal.KindInput, "approved:"+name, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, p)}
	entries = append(entries, extra...)
	return journal.Recovery{Entries: entries, FinalRecordHash: hashOf(name)}
}

func reader(logs map[string]journal.Recovery) restart.LogReader {
	return func(name string) (journal.Recovery, error) {
		r, ok := logs[name]
		if !ok {
			return journal.Recovery{}, fmt.Errorf("open %s: %w", name, os.ErrNotExist)
		}
		return r, nil
	}
}

func names(chain []restart.Link) []string {
	var out []string
	for _, l := range chain {
		out = append(out, l.Name)
	}
	return out
}

// TestFollowChainReturnsEveryRunOldestFirst: a three-run chain, the first
// run stating the starting book and naming no previous log (ADR 0027).
func TestFollowChainReturnsEveryRunOldestFirst(t *testing.T) {
	logs := map[string]journal.Recovery{
		"a.log": run(t, "a.log", ""), "b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log"),
	}
	chain, err := restart.FollowChain("c.log", reader(logs))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(chain); !slices.Equal(got, []string{"a.log", "b.log", "c.log"}) {
		t.Fatalf("chain = %v, want a, b, c oldest first", got)
	}
}

// TestFollowChainEndsAtADryRunsFirstRunByItsMode: a dry run's first run
// is started with no approval, so a dry-run log holding no approval, known
// by the mode its configuration records, is the end of the chain.
func TestFollowChainEndsAtADryRunsFirstRunByItsMode(t *testing.T) {
	first := unapproved(t, "a.log", event.TradingModeDryRun)
	first.Entries = append(first.Entries, recorded(t, journal.KindInput, "other", event.KillSwitchEventType, event.KillSwitchSchemaVersion, struct{}{}))
	logs := map[string]journal.Recovery{"a.log": first, "b.log": runOf(t, "b.log", "a.log", "joint", event.TradingModeDryRun)}
	chain, err := restart.FollowChain("b.log", reader(logs))
	if err != nil || !slices.Equal(names(chain), []string{"a.log", "b.log"}) {
		t.Fatalf("chain = %v, err = %v; want a.log then b.log", names(chain), err)
	}
}

// TestFollowChainRefusesALinkWithNeitherApprovalNorStartingBook: a paper or
// live run with no approval states no starting book, so it is not a first
// run: a fresh run's start cut short before its approval was recorded is a
// broken link, never a silent first run that drops every run before it. A
// dry-run log holding nothing past its configuration cannot be told from
// such a start either.
func TestFollowChainRefusesALinkWithNeitherApprovalNorStartingBook(t *testing.T) {
	cases := map[string]struct {
		logs          map[string]journal.Recovery
		current, want string
	}{
		"earlier paper link": {map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": unapproved(t, "b.log", event.TradingModePaper), "c.log": run(t, "c.log", "b.log")}, "c.log", "b.log"},
		"current live log":   {map[string]journal.Recovery{"a.log": pastItsConfiguration(t, unapproved(t, "a.log", event.TradingModeLive))}, "a.log", "a.log"},
		"cut-short dry run":  {map[string]journal.Recovery{"a.log": runOf(t, "a.log", "", "joint", event.TradingModeDryRun), "b.log": unapproved(t, "b.log", event.TradingModeDryRun), "c.log": runOf(t, "c.log", "b.log", "joint", event.TradingModeDryRun)}, "c.log", "b.log"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			chain, err := restart.FollowChain(c.current, reader(c.logs))
			var broken *restart.ChainError
			if !errors.As(err, &broken) || chain != nil {
				t.Fatalf("chain = %v, err = %v; want a *restart.ChainError and no chain", names(chain), err)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "no run approval") {
				t.Fatalf("err = %v, want it to name %s and its missing approval", err, c.want)
			}
		})
	}
}

// pastItsConfiguration is recovery with one more input after its
// configuration, so it is not a start in progress.
func pastItsConfiguration(t *testing.T, recovery journal.Recovery) journal.Recovery {
	t.Helper()
	recovery.Entries = append(recovery.Entries, recorded(t, journal.KindInput, "other", event.KillSwitchEventType, event.KillSwitchSchemaVersion, struct{}{}))
	return recovery
}

// TestFollowChainTakesACurrentLogHoldingOnlyItsConfigurationForAStart: the
// current log is the head of the chain, named by no other log, so one that
// holds only its configuration is a start in progress, in any mode, not a
// break: its run is the chain's one link and has no earlier run
// (StartInProgress). An earlier link holding only its configuration is
// still a broken link (TestFollowChainRefusesALinkWithNeitherApprovalNorStartingBook).
func TestFollowChainTakesACurrentLogHoldingOnlyItsConfigurationForAStart(t *testing.T) {
	for _, mode := range []string{event.TradingModeDryRun, event.TradingModePaper, event.TradingModeLive} {
		t.Run(mode, func(t *testing.T) {
			logs := map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": unapproved(t, "b.log", mode)}
			chain, err := restart.FollowChain("b.log", reader(logs))
			if err != nil || !slices.Equal(names(chain), []string{"b.log"}) || !restart.StartInProgress(chain) {
				t.Fatalf("chain = %v, err = %v; want b.log alone, a start in progress", names(chain), err)
			}
		})
	}
	whole, err := restart.FollowChain("b.log", reader(map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": run(t, "b.log", "a.log")}))
	if err != nil || restart.StartInProgress(whole) {
		t.Fatalf("a run past its approval is a start in progress: %v, %v", names(whole), err)
	}
	dry, err := restart.FollowChain("a.log", reader(map[string]journal.Recovery{"a.log": pastItsConfiguration(t, unapproved(t, "a.log", event.TradingModeDryRun))}))
	if err != nil || restart.StartInProgress(dry) {
		t.Fatalf("a dry run's first run past its configuration is a start in progress: %v, %v", names(dry), err)
	}
}

// TestFollowChainRefusesAnEarlierLogOfAnotherAccount: every earlier log is
// of the current log's account before its records join the history;
// another account's log is a broken link naming the log and both accounts.
// A change of mode is no break (TestFollowChainFollowsAModeChangeTheEngineAccepts).
func TestFollowChainRefusesAnEarlierLogOfAnotherAccount(t *testing.T) {
	cases := map[string]struct {
		a    journal.Recovery
		want []string
	}{
		"another account":              {runOf(t, "a.log", "", "ira", event.TradingModePaper), []string{"a.log", `"ira"`, `"joint"`}},
		"another account in a dry run": {runOf(t, "a.log", "", "ira", event.TradingModeDryRun), []string{"a.log", `"ira"`, `"joint"`}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			logs := map[string]journal.Recovery{"a.log": c.a, "b.log": run(t, "b.log", "a.log")}
			chain, err := restart.FollowChain("b.log", reader(logs))
			var broken *restart.ChainError
			if !errors.As(err, &broken) || chain != nil || broken.Previous != "a.log" {
				t.Fatalf("chain = %v, err = %v; want a *restart.ChainError at a.log", names(chain), err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %s", err, w)
				}
			}
		})
	}
}

// TestChainErrorHoldsTheIntactRunsNewerThanTheBreak: a refused chain still
// names the runs from the break to the current one, so a reader can show
// those alone, saying where the history starts.
func TestChainErrorHoldsTheIntactRunsNewerThanTheBreak(t *testing.T) {
	logs := map[string]journal.Recovery{"b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")}
	_, err := restart.FollowChain("c.log", reader(logs))
	var broken *restart.ChainError
	if !errors.As(err, &broken) || !slices.Equal(names(broken.Intact), []string{"b.log", "c.log"}) {
		t.Fatalf("err = %v, intact = %v; want b.log then c.log", err, names(broken.Intact))
	}
}

func TestFollowChainRefusesABrokenLinkNamingIt(t *testing.T) {
	changed := run(t, "a.log", "")
	changed.FinalRecordHash = "sha256:" + strings.Repeat("0", 64)
	cases := map[string]struct {
		logs map[string]journal.Recovery
		want []string
	}{
		"hash mismatch": {map[string]journal.Recovery{"a.log": changed, "b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")},
			[]string{"b.log", "a.log", "final record hash"}},
		"missing file": {map[string]journal.Recovery{"b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")},
			[]string{"b.log", "a.log", "cannot be read"}},
		"cycle": {map[string]journal.Recovery{"a.log": run(t, "a.log", "c.log"), "b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")},
			[]string{"a.log", "c.log", "twice"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			chain, err := restart.FollowChain("c.log", reader(c.logs))
			if err == nil || chain != nil {
				t.Fatalf("chain = %v, err = %v; want a refusal and no partial chain", names(chain), err)
			}
			var broken *restart.ChainError
			if !errors.As(err, &broken) {
				t.Fatalf("err = %T, want *restart.ChainError", err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestFollowChainRefusesTwoApprovalsAndANameThatIsAPath(t *testing.T) {
	two := run(t, "b.log", "a.log")
	two.Entries = append(two.Entries, two.Entries[1])
	if _, err := restart.FollowChain("b.log", reader(map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": two})); err == nil || !strings.Contains(err.Error(), "b.log") {
		t.Fatalf("two approvals: err = %v, want a refusal naming b.log", err)
	}
	path := run(t, "b.log", "a.log")
	path.Entries[1].Envelope.Payload = []byte(strings.Replace(string(path.Entries[1].Envelope.Payload), `"a.log"`, `"../a.log"`, 1))
	if _, err := restart.FollowChain("b.log", reader(map[string]journal.Recovery{"b.log": path})); err == nil || !strings.Contains(err.Error(), "b.log") {
		t.Fatalf("path name: err = %v, want a refusal naming b.log", err)
	}
}

func TestPreviousLogApprovedIsTheRuleTheEngineChecksAnApprovalBy(t *testing.T) {
	r := run(t, "a.log", "")
	approval := event.OperatorRunApprovedPayload{PreviousLog: "a.log", PreviousLogFinalHash: r.FinalRecordHash}
	if !restart.PreviousLogApproved(r, approval) {
		t.Fatal("the log as approved was refused")
	}
	approval.PreviousLogFinalHash = hashOf("zz")
	if restart.PreviousLogApproved(r, approval) {
		t.Fatal("a log other than the one approved was accepted")
	}
}

// TestLatestRunIsTheMostRecentlyModifiedRunNotAnAbandonedAttempt: the run a
// reader follows the chain from is the one the engine would name as the
// latest (Classify), ordered by modification time and then by name.
func TestLatestRunIsTheMostRecentlyModifiedRunNotAnAbandonedAttempt(t *testing.T) {
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	files := []restart.LoggedFile{{Name: "c.log", Modified: at}, {Name: "a.log", Modified: at.Add(-time.Hour)}, {Name: "b.log", Modified: at}}
	logs := map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")}
	got, err := restart.LatestRun(files, reader(logs))
	if err != nil || got != "c.log" {
		t.Fatalf("latest = %q, err = %v; want c.log (modified with b.log, later name)", got, err)
	}
	if got, err := restart.LatestRun(nil, reader(nil)); err != nil || got != "" {
		t.Fatalf("latest of no logs = %q, err = %v", got, err)
	}
	if _, err := restart.LatestRun(files, reader(map[string]journal.Recovery{})); err == nil {
		t.Fatal("a log that cannot be read must refuse, not be skipped")
	}
}

func TestFollowChainSkipsRecordsThatAreNotTheApprovalAndRefusesAnUnreadableCurrentLog(t *testing.T) {
	other := recorded(t, journal.KindInput, "other", event.KillSwitchEventType, event.KillSwitchSchemaVersion, struct{}{})
	logs := map[string]journal.Recovery{"a.log": run(t, "a.log", "", other), "b.log": run(t, "b.log", "a.log", other)}
	if chain, err := restart.FollowChain("b.log", reader(logs)); err != nil || len(chain) != 2 {
		t.Fatalf("chain = %v, err = %v", names(chain), err)
	}
	var broken *restart.ChainError
	if _, err := restart.FollowChain("zz.log", reader(logs)); !errors.As(err, &broken) || broken.Run != "zz.log" {
		t.Fatalf("err = %v, want a chain error naming zz.log", err)
	}
}

// TestFollowChainRefusesALogWhoseAccountAndModeAreUnknown: a log with no
// configuration, or one naming no mode, cannot be matched to the chain.
func TestFollowChainRefusesALogWhoseAccountAndModeAreUnknown(t *testing.T) {
	bare := run(t, "a.log", "")
	bare.Entries = bare.Entries[1:]
	noMode := run(t, "a.log", "")
	noMode.Entries[0] = configOf(t, "a.log", "joint", "")
	cases := map[string]struct {
		logs          map[string]journal.Recovery
		current, want string
	}{
		"current with no configuration": {map[string]journal.Recovery{"a.log": bare}, "a.log", "no target-weight configuration"},
		"earlier with no configuration": {map[string]journal.Recovery{"a.log": bare, "b.log": run(t, "b.log", "a.log")}, "b.log", "no target-weight configuration"},
		"earlier naming no mode":        {map[string]journal.Recovery{"a.log": noMode, "b.log": run(t, "b.log", "a.log")}, "b.log", "does not name its account and a mode"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := restart.FollowChain(c.current, reader(c.logs))
			var broken *restart.ChainError
			if !errors.As(err, &broken) || !strings.Contains(err.Error(), "a.log") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a.log refused: %s", err, c.want)
			}
		})
	}
}

// entry is a directory entry whose Info answers err.
type entry struct {
	name string
	mode fs.FileMode
	err  error
}

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return e.mode.IsDir() }
func (e entry) Type() fs.FileMode          { return e.mode.Type() }
func (e entry) Info() (fs.FileInfo, error) { return nil, e.err }

// TestListLogsSkipsOnlyAVanishedLog: the engine's listing, shared with the
// dashboard, lists the regular files with the suffix oldest first, skips a
// log that vanished while listed and refuses one that cannot be examined
// for any other reason, so the latest run is never passed over.
func TestListLogsSkipsOnlyAVanishedLog(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"b.log", "a.log", "notes.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at.Add(time.Duration(-i)*time.Hour), at.Add(time.Duration(-i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "quarantine.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	logs, err := restart.ListLogs(dir, ".log")
	if err != nil || len(logs) != 2 || logs[0].Name != "a.log" || logs[1].Name != "b.log" {
		t.Fatalf("logs = %v, err = %v; want a.log then b.log", logs, err)
	}
	if _, err := restart.ListLogs(filepath.Join(dir, "none"), ".log"); err == nil {
		t.Fatal("a directory that cannot be listed must refuse")
	}
	if logs, err := restart.Listed(dir, ".log", []fs.DirEntry{entry{name: "gone.log", err: fs.ErrNotExist}}); err != nil || len(logs) != 0 {
		t.Fatalf("a vanished log: logs = %v, err = %v; want it skipped", logs, err)
	}
	if _, err := restart.Listed(dir, ".log", []fs.DirEntry{entry{name: "x.log", err: fs.ErrPermission}}); err == nil || !strings.Contains(err.Error(), "x.log") {
		t.Fatalf("a log that cannot be examined: err = %v, want a refusal naming it", err)
	}
}
