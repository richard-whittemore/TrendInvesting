package report

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

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
type History struct {
	Earlier []event.Envelope
	Broken  *restart.ChainError
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
		return History{Broken: &restart.ChainError{Run: current, Reason: fmt.Sprintf("that log cannot be read (%v)", withoutPath(err))}}
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
	if first, _, _ := bytes.Cut(raw, []byte("\n")); !bytes.Contains(first, []byte(`"log_version"`)) {
		return History{} // an end-of-run journal: it follows no chain
	}
	dir, current := filepath.Split(path)
	currentRecovery, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		return History{Broken: &restart.ChainError{Run: current, Reason: fmt.Sprintf("that log does not recover (%v)", err)}}
	}
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
	var earlier []event.Envelope
	for _, link := range chain {
		if link.Name == current {
			continue
		}
		for _, e := range link.Recovery.Entries {
			earlier = append(earlier, e.Envelope)
		}
	}
	return History{Earlier: earlier, Broken: broken}
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
