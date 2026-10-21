package report

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// EarlierRuns returns the envelopes of every run before the one whose durable
// log is at path, oldest first, found by following each run's approval back
// to the first run (restart.FollowChain, the rule the engine checks an
// approval by and the IBKR adapter's run_chain walks by). Each earlier log is
// read with readFile from beside path. A file that is not a durable log, such
// as an end-of-run journal, follows no chain and has none before it. A chain
// that cannot be followed is refused with a *restart.ChainError naming the
// link, never cut short.
func EarlierRuns(path string, readFile func(string) ([]byte, error)) ([]event.Envelope, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if first, _, _ := bytes.Cut(raw, []byte("\n")); !bytes.Contains(first, []byte(`"log_version"`)) {
		return nil, nil // an end-of-run journal: it follows no chain
	}
	currentRecovery, err := journal.RecoverLog(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dir, current := filepath.Split(path)
	chain, err := restart.FollowChain(current, func(name string) (journal.Recovery, error) {
		if name == current {
			return currentRecovery, nil
		}
		raw, err := readFile(dir + name)
		if err != nil {
			// The reason is shown to a reader of the page, who is not told
			// the server's paths.
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return journal.Recovery{}, err
		}
		return journal.RecoverLog(bytes.NewReader(raw))
	})
	if err != nil {
		return nil, err
	}
	var earlier []event.Envelope
	for _, link := range chain[:len(chain)-1] {
		for _, e := range link.Recovery.Entries {
			earlier = append(earlier, e.Envelope)
		}
	}
	return earlier, nil
}
