package restart_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// TestFollowChainFollowsAModeChangeTheEngineAccepts: the engine accepts a
// paper run approved against a dry-run log and a live run against a paper
// log, so such a link is followed, its hash checked, and never called
// broken; each link carries its own mode for a reader that keeps one mode.
func TestFollowChainFollowsAModeChangeTheEngineAccepts(t *testing.T) {
	dry := unapproved(t, "a.log", event.TradingModeDryRun)
	dry.Entries = append(dry.Entries, recorded(t, journal.KindInput, "other", event.KillSwitchEventType, event.KillSwitchSchemaVersion, struct{}{}))
	cases := map[string]struct {
		logs  map[string]journal.Recovery
		modes []string
	}{
		"paper after dry run": {map[string]journal.Recovery{"a.log": dry, "b.log": runOf(t, "b.log", "a.log", "joint", event.TradingModePaper)},
			[]string{event.TradingModeDryRun, event.TradingModePaper}},
		"live after paper": {map[string]journal.Recovery{"a.log": runOf(t, "a.log", "", "joint", event.TradingModePaper), "b.log": runOf(t, "b.log", "a.log", "joint", event.TradingModeLive)},
			[]string{event.TradingModePaper, event.TradingModeLive}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			chain, err := restart.FollowChain("b.log", reader(c.logs))
			if err != nil || !slices.Equal(names(chain), []string{"a.log", "b.log"}) {
				t.Fatalf("chain = %v, err = %v; want a.log then b.log", names(chain), err)
			}
			for i, l := range chain {
				if l.Mode != c.modes[i] {
					t.Errorf("%s mode = %q, want %q", l.Name, l.Mode, c.modes[i])
				}
			}
			changed := c.logs["a.log"]
			changed.FinalRecordHash = hashOf("zz")
			c.logs["a.log"] = changed
			var broken *restart.ChainError
			if _, err := restart.FollowChain("b.log", reader(c.logs)); !errors.As(err, &broken) || broken.Cause != restart.CauseHashMismatch {
				t.Fatalf("a changed log across the mode change: err = %v, want a hash mismatch", err)
			}
		})
	}
}

// TestChainErrorNamesItsCause: each kind of break carries its own cause, so
// a reader can tell a log restored wrongly from one still missing.
func TestChainErrorNamesItsCause(t *testing.T) {
	changed := run(t, "a.log", "")
	changed.FinalRecordHash = hashOf("zz")
	cases := map[string]struct {
		logs map[string]journal.Recovery
		want string
	}{
		"missing":         {map[string]journal.Recovery{"b.log": run(t, "b.log", "a.log")}, restart.CauseUnreadable},
		"hash mismatch":   {map[string]journal.Recovery{"a.log": changed, "b.log": run(t, "b.log", "a.log")}, restart.CauseHashMismatch},
		"another account": {map[string]journal.Recovery{"a.log": runOf(t, "a.log", "", "ira", event.TradingModePaper), "b.log": run(t, "b.log", "a.log")}, restart.CauseIdentity},
		"no approval":     {map[string]journal.Recovery{"a.log": unapproved(t, "a.log", event.TradingModePaper), "b.log": run(t, "b.log", "a.log")}, restart.CauseUnapproved},
		"loop":            {map[string]journal.Recovery{"a.log": run(t, "a.log", "b.log"), "b.log": run(t, "b.log", "a.log")}, restart.CauseLoop},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := restart.FollowChain("b.log", reader(c.logs))
			var broken *restart.ChainError
			if !errors.As(err, &broken) || broken.Cause != c.want {
				t.Fatalf("err = %v (cause %q), want cause %q", err, causeOf(broken), c.want)
			}
		})
	}
}

func causeOf(e *restart.ChainError) string {
	if e == nil {
		return ""
	}
	return e.Cause
}

// starts is a LogReader that answers only with a log's first two records
// and counts the logs it read; full fails the test when called.
func starts(t *testing.T, logs map[string]journal.Recovery, read map[string]int) restart.LogReader {
	return func(name string) (journal.Recovery, error) {
		read[name]++
		r, ok := logs[name]
		if !ok {
			return journal.Recovery{}, fmt.Errorf("no %s", name)
		}
		r.Entries = r.Entries[:min(2, len(r.Entries))]
		return r, nil
	}
}

func noFullRead(t *testing.T) restart.LogReader {
	return func(name string) (journal.Recovery, error) {
		t.Fatalf("%s was read whole; the chain head needs only each log's first records", name)
		return journal.Recovery{}, nil
	}
}

// TestChainHeadOfAThousandLogsReadsOnlyTheirStarts: the current run is the
// log no other log's approval names as its previous log, found from each
// log's first records alone.
func TestChainHeadOfAThousandLogsReadsOnlyTheirStarts(t *testing.T) {
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	logs := map[string]journal.Recovery{}
	var files []restart.LoggedFile
	previous := ""
	for i := range 1000 {
		name := fmt.Sprintf("run%04d.log", i)
		logs[name] = run(t, name, previous)
		files = append(files, restart.LoggedFile{Name: name, Modified: at.Add(time.Duration(i) * time.Minute)})
		previous = name
	}
	read := map[string]int{}
	head, newer, err := restart.ChainHead(files, starts(t, logs, read), noFullRead(t))
	if err != nil || head != "run0999.log" || newer != "" {
		t.Fatalf("head = %q, newer = %q, err = %v", head, newer, err)
	}
	if len(read) != 1000 {
		t.Fatalf("read the starts of %d logs, want all 1000 once", len(read))
	}
}

// TestChainHeadIsNotARestoredLogWithAFreshTime: a log restored from a backup
// without its modification time is the newest by time, but the current run
// names it, so the head is still the current run; the restored log is
// returned as newer, for a warning.
func TestChainHeadIsNotARestoredLogWithAFreshTime(t *testing.T) {
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	logs := map[string]journal.Recovery{"a.log": run(t, "a.log", ""), "b.log": run(t, "b.log", "a.log"), "c.log": run(t, "c.log", "b.log")}
	files := []restart.LoggedFile{{Name: "a.log", Modified: at.Add(time.Hour)}, {Name: "b.log", Modified: at}, {Name: "c.log", Modified: at.Add(time.Minute)}}
	head, newer, err := restart.ChainHead(files, starts(t, logs, map[string]int{}), noFullRead(t))
	if err != nil || head != "c.log" || newer != "a.log" {
		t.Fatalf("head = %q, newer = %q, err = %v; want c.log, with a.log newer", head, newer, err)
	}
}

// TestChainHeadBreaksATieByTheEnginesRule: a resumed run's log and the log
// it resumed both name the same previous log, and an abandoned attempt or an
// empty start names none; the engine's latest-run rule picks among them,
// reading whole only the logs that share a start.
func TestChainHeadBreaksATieByTheEnginesRule(t *testing.T) {
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	other := func(id string) journal.Entry {
		return recorded(t, journal.KindInput, id, event.KillSwitchEventType, event.KillSwitchSchemaVersion, struct{}{})
	}
	a := run(t, "a.log", "")
	b := run(t, "b.log", "a.log", other("1"))
	resumed := run(t, "b.log", "a.log", other("1"), other("2"))
	resumed.FinalRecordHash = hashOf("b2.log")
	attempt := run(t, "b.log", "a.log", other("1"))
	attempt.FinalRecordHash = hashOf("b3.log")
	logs := map[string]journal.Recovery{"a.log": a, "b.log": b, "b2.log": resumed, "b3.log": attempt, "e.log": {}}
	files := []restart.LoggedFile{{Name: "a.log", Modified: at}, {Name: "b.log", Modified: at.Add(time.Minute)}, {Name: "b2.log", Modified: at.Add(2 * time.Minute)},
		{Name: "b3.log", Modified: at.Add(3 * time.Minute)}, {Name: "e.log", Modified: at.Add(4 * time.Minute)}}
	whole := map[string]int{}
	full := func(name string) (journal.Recovery, error) {
		whole[name]++
		return logs[name], nil
	}
	head, newer, err := restart.ChainHead(files, starts(t, logs, map[string]int{}), full)
	if err != nil || head != "b2.log" || newer != "" {
		t.Fatalf("head = %q, newer = %q, err = %v; want b2.log, the resumed run", head, newer, err)
	}
	if whole["a.log"] != 0 || whole["e.log"] != 0 {
		t.Fatalf("read whole %v; only the logs sharing a start need it", whole)
	}
	if _, _, err := restart.ChainHead(files, starts(t, map[string]journal.Recovery{}, map[string]int{}), full); err == nil {
		t.Fatal("a log whose start cannot be read must refuse, not be skipped")
	}
	if head, _, err := restart.ChainHead(nil, starts(t, logs, map[string]int{}), full); err != nil || head != "" {
		t.Fatalf("no logs: head = %q, err = %v", head, err)
	}
}

// TestChainHeadOfUnrelatedStartsReadsNoLogWhole: logs that tie but share no
// start, another header or other first records, cannot be attempts of one
// another, so the newest is the head and none is read whole; a log that a
// tie needs read whole and cannot be read refuses.
func TestChainHeadOfUnrelatedStartsReadsNoLogWhole(t *testing.T) {
	at := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	x, y, z := run(t, "x.log", ""), run(t, "y.log", ""), run(t, "z.log", "")
	y.Header = journal.NewLogHeader("other", "v")
	logs := map[string]journal.Recovery{"x.log": x, "y.log": y, "z.log": z}
	files := []restart.LoggedFile{{Name: "x.log", Modified: at}, {Name: "y.log", Modified: at.Add(time.Minute)}, {Name: "z.log", Modified: at.Add(2 * time.Minute)}}
	if head, _, err := restart.ChainHead(files, starts(t, logs, map[string]int{}), noFullRead(t)); err != nil || head != "z.log" {
		t.Fatalf("head = %q, err = %v; want z.log", head, err)
	}
	copied := run(t, "x.log", "")
	copied.FinalRecordHash = hashOf("xx.log")
	logs["xx.log"] = copied
	files = append(files, restart.LoggedFile{Name: "xx.log", Modified: at.Add(3 * time.Minute)})
	failing := func(name string) (journal.Recovery, error) {
		return journal.Recovery{}, fmt.Errorf("disk fault reading %s", name)
	}
	if _, _, err := restart.ChainHead(files, starts(t, logs, map[string]int{}), failing); err == nil {
		t.Fatal("a tied log that cannot be read whole must refuse")
	}
}
