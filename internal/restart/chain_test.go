package restart_test

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

func hashOf(name string) string { return fmt.Sprintf("sha256:%064x", len(name)*7919+int(name[0])) }

// run is a synthetic log recovery whose final hash is hashOf(name) and whose
// one approval, when given, names previous.
func run(t *testing.T, name, previous string, extra ...journal.Entry) journal.Recovery {
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
	entries := []journal.Entry{recorded(t, journal.KindInput, "approved:"+name, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, p)}
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

// TestFollowChainOfALogWithNoApprovalIsThatLogAlone: a run with no approval
// (a dry run's first) is the end of the chain.
func TestFollowChainOfALogWithNoApprovalIsThatLogAlone(t *testing.T) {
	logs := map[string]journal.Recovery{"a.log": {FinalRecordHash: hashOf("a.log")}}
	chain, err := restart.FollowChain("a.log", reader(logs))
	if err != nil || !slices.Equal(names(chain), []string{"a.log"}) {
		t.Fatalf("chain = %v, err = %v; want a.log alone", names(chain), err)
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
	two.Entries = append(two.Entries, two.Entries[0])
	if _, err := restart.FollowChain("b.log", reader(map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": two})); err == nil || !strings.Contains(err.Error(), "b.log") {
		t.Fatalf("two approvals: err = %v, want a refusal naming b.log", err)
	}
	path := run(t, "b.log", "a.log")
	path.Entries[0].Envelope.Payload = []byte(strings.Replace(string(path.Entries[0].Envelope.Payload), `"a.log"`, `"../a.log"`, 1))
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
