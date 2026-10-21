package report

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// History is the runs before the current one, which only the equity
// comparison reads. Broken is set when the chain of runs cannot be followed
// back to the first run: Earlier then holds the intact runs newer than the
// break alone. A broken history never refuses the journal: the current
// log's alerts, state and day's figures do not depend on it (ADR 0019,
// "Alerting").
//
// Since is set when the chain holds runs of another mode before the current
// mode's: the engine accepts a paper run approved against a dry-run log and
// a live run against a paper log, so such a chain is whole, but Earlier then
// holds only the current mode's runs, from the earliest contiguous one.
type History struct {
	Earlier []event.Envelope
	Broken  *restart.ChainError
	Since   *ModeStart
}

// ModeStart is where a history of one mode starts: the log of the first run
// in the current mode, that mode, and the date (YYYY-MM-DD) the run's
// configuration was recorded.
type ModeStart struct {
	Log, Mode, Date string
}

// HistorySince is the line the equity comparison gives for a history that
// starts at the first run of the current mode, "" when it holds every run.
func HistorySince(h History) string {
	if h.Since == nil {
		return ""
	}
	return fmt.Sprintf("history since %s, the first %s run", h.Since.Date, h.Since.Mode)
}

// EarlierRuns returns the history of the run whose durable log is at path:
// the envelopes of every run before it, oldest first, found by following
// each run's approval back to the first run (restart.FollowChain, the rule
// the engine checks an approval by and the IBKR adapter's run_chain walks
// by). Each earlier log is read with readFile from beside path. A file that
// is not a durable log, such as an end-of-run journal, follows no chain and
// has none before it.
func EarlierRuns(path string, readFile func(string) ([]byte, error)) History {
	raw, err := readFile(path)
	if err != nil {
		_, current := filepath.Split(path)
		return History{Broken: &restart.ChainError{Run: current, Cause: restart.CauseUnreadable, Reason: fmt.Sprintf("that log cannot be read (%v)", withoutPath(err))}}
	}
	return EarlierRunsOf(path, raw, func(earlier string) (journal.Recovery, error) {
		raw, err := readFile(earlier)
		if err != nil {
			return journal.Recovery{}, withoutPath(err)
		}
		return journal.RecoverLog(bytes.NewReader(raw))
	})
}

// EarlierRunsOf is EarlierRuns for a caller that holds the current log's
// bytes, raw, and recovers each earlier log itself: recoverLog is given the
// earlier log's path beside path. A chain that cannot be followed is
// returned as History.Broken naming the link, never cut short silently.
func EarlierRunsOf(path string, raw []byte, recoverLog func(path string) (journal.Recovery, error)) History {
	if !isDurableLog(raw) {
		return History{} // an end-of-run journal: it follows no chain
	}
	currentRecovery, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		_, current := filepath.Split(path)
		return History{Broken: &restart.ChainError{Run: current, Cause: restart.CauseUnreadable, Reason: fmt.Sprintf("that log does not recover (%v)", err)}}
	}
	return EarlierRunsOfRecovery(path, currentRecovery, recoverLog)
}

// isDurableLog reports whether raw is a durable log, whose header line
// carries its log version, not an end-of-run journal.
func isDurableLog(raw []byte) bool {
	first, _, _ := bytes.Cut(raw, []byte("\n"))
	return bytes.Contains(first, []byte(`"log_version"`))
}

// EarlierRunsOfRecovery is EarlierRunsOf for a caller that has recovered the
// current durable log itself, so it is neither read nor recovered again.
func EarlierRunsOfRecovery(path string, currentRecovery journal.Recovery, recoverLog func(path string) (journal.Recovery, error)) History {
	dir, current := filepath.Split(path)
	chain, err := restart.FollowChain(current, func(name string) (journal.Recovery, error) {
		if name == current {
			return currentRecovery, nil
		}
		return recoverLog(dir + name)
	})
	// Every error FollowChain returns is a *restart.ChainError.
	var broken *restart.ChainError
	if errors.As(err, &broken) {
		chain = broken.Intact
	}
	chain, since := currentModeOf(chain)
	var earlier []event.Envelope
	for _, link := range chain {
		if link.Name == current {
			continue
		}
		for _, e := range link.Recovery.Entries {
			earlier = append(earlier, e.Envelope)
		}
	}
	return History{Earlier: earlier, Broken: broken, Since: since}
}

// currentModeOf returns the runs of chain, oldest first, from the earliest
// of the unbroken run of the current (last) run's mode, and where that
// history starts when runs of another mode come before it. The equity
// comparison is one mode's: a dry run's or paper run's figures never join
// a live run's.
func currentModeOf(chain []restart.Link) ([]restart.Link, *ModeStart) {
	if len(chain) == 0 {
		return chain, nil
	}
	mode := chain[len(chain)-1].Mode
	first := len(chain) - 1
	for first > 0 && chain[first-1].Mode == mode {
		first--
	}
	if first == 0 {
		return chain, nil
	}
	start := ModeStart{Log: chain[first].Name, Mode: mode}
	if entries := chain[first].Recovery.Entries; len(entries) > 0 {
		start.Date = entries[0].Envelope.EventTime.UTC().Format(time.DateOnly)
	}
	return chain[first:], &start
}

// withoutPath strips the path from a file error: the reason is shown to a
// reader of the page, who is not told the server's paths.
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// HistoryUnavailable is the line the equity comparison gives for a broken
// chain of runs, naming the log whose approval names the broken link and
// that link.
func HistoryUnavailable(broken *restart.ChainError) string {
	if broken.Previous == "" {
		return "history unavailable: the chain of runs is broken at " + broken.Run
	}
	return fmt.Sprintf("history unavailable: the chain of runs is broken between %s and %s", broken.Run, broken.Previous)
}

// historyStartsAt is the log of the first intact run, the run the equity
// comparison starts from when the chain is broken.
func historyStartsAt(broken *restart.ChainError) string {
	if len(broken.Intact) > 0 {
		return broken.Intact[0].Name
	}
	return broken.Run
}
