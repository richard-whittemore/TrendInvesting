package restart

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// LogReader recovers the durable log named, a bare file name beside the log a
// chain starts from.
type LogReader func(name string) (journal.Recovery, error)

// Link is one run of a chain: its log's name, what it holds, and the
// approval it was started with, nil for a run that holds none.
type Link struct {
	Name     string
	Recovery journal.Recovery
	Approval *event.OperatorRunApprovedPayload
}

// ChainError is a chain of runs that cannot be followed back to its first
// run. Run is the log whose approval names the broken link, and Previous the
// log it names: the link a person looks at.
type ChainError struct {
	Run, Previous string
	Reason        string
}

func (e *ChainError) Error() string {
	if e.Previous == "" {
		return fmt.Sprintf("restart: the chain of runs is broken at %s: %s", e.Run, e.Reason)
	}
	return fmt.Sprintf("restart: the chain of runs is broken between %s and %s, the run its approval names before it: %s", e.Run, e.Previous, e.Reason)
}

// PreviousLogApproved reports whether recovery is the log as the approval
// names it: its final committed record hash is the one the owner approved
// (ADR 0027, the restart model). It is the one rule the engine checks a fresh
// run's approval by and a chain is followed by.
func PreviousLogApproved(recovery journal.Recovery, approval event.OperatorRunApprovedPayload) bool {
	return recovery.FinalRecordHash == approval.PreviousLogFinalHash
}

// FollowChain returns every run of the chain that ends at the log named
// current, oldest first, the current run last. It follows each run's
// operator.run.approved to the log it names, as the IBKR adapter's run_chain
// does, and stops at a first run (an approval that names no previous log and
// states the starting book) or at a run holding no approval. Each earlier
// log must be the one the approval names, by PreviousLogApproved. A chain
// that cannot be followed is refused whole with a *ChainError naming the
// link, never cut short: a history that silently starts late would read as
// the whole one. The current log's own final hash is named by no approval,
// so it is not checked.
func FollowChain(current string, read LogReader) ([]Link, error) {
	recovery, err := read(current)
	if err != nil {
		return nil, &ChainError{Run: current, Reason: fmt.Sprintf("that log cannot be read (%v)", err)}
	}
	seen := map[string]bool{current: true}
	var newestFirst []Link
	name := current
	for {
		approval, err := approvalOf(name, recovery)
		if err != nil {
			return nil, err
		}
		newestFirst = append(newestFirst, Link{Name: name, Recovery: recovery, Approval: approval})
		if approval == nil || approval.FirstRun() {
			break
		}
		// A previous log that is a path, not a file name beside this one,
		// does not decode (Validate), so it is refused in approvalOf.
		previous := approval.PreviousLog
		if seen[previous] {
			return nil, &ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("the approvals from %s name %s twice; the chain of runs must end at the first run", current, previous)}
		}
		seen[previous] = true
		earlier, err := read(previous)
		if err != nil {
			return nil, &ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("that log cannot be read (%v): every earlier run's log stays beside the engine's, so put it back as the engine wrote it", err)}
		}
		if !PreviousLogApproved(earlier, *approval) {
			return nil, &ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("the approval names final record hash %s, but that log's final record hash is %s: it is not the log the owner approved", approval.PreviousLogFinalHash, earlier.FinalRecordHash)}
		}
		name, recovery = previous, earlier
	}
	chain := make([]Link, len(newestFirst))
	for i, l := range newestFirst {
		chain[len(newestFirst)-1-i] = l
	}
	return chain, nil
}

// approvalOf returns the run's one committed approval, nil when it holds
// none; two, or one that does not decode, refuse.
func approvalOf(name string, recovery journal.Recovery) (*event.OperatorRunApprovedPayload, error) {
	var found *event.OperatorRunApprovedPayload
	for _, entry := range recovery.Entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.OperatorRunApprovedEventType {
			continue
		}
		if found != nil {
			return nil, &ChainError{Run: name, Reason: "it holds more than one run approval; a run holds at most one"}
		}
		approval, err := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
		if err != nil {
			return nil, &ChainError{Run: name, Reason: fmt.Sprintf("its run approval does not read: %v", err)}
		}
		found = &approval
	}
	return found, nil
}

// LoggedFile is a durable log in a log directory and when it was last
// modified.
type LoggedFile struct {
	Name     string
	Modified time.Time
}

// OlderLog is the order the engine keeps its logs in: least recently
// modified first, and two modified at the same instant by name, the later
// name being the later log.
func OlderLog(a, b LoggedFile) bool {
	if !a.Modified.Equal(b.Modified) {
		return a.Modified.Before(b.Modified)
	}
	return a.Name < b.Name
}

// LatestRun returns the name of the latest run among the logs of one
// directory, "" when there is none, by the engine's own rule: every log is
// read, abandoned attempts and starts are set aside (Classify), and the last
// run in OlderLog order is the latest. A log that cannot be read refuses
// rather than being skipped, as it does at the engine's start.
func LatestRun(files []LoggedFile, read LogReader) (string, error) {
	sorted := slices.Clone(files)
	sort.Slice(sorted, func(i, j int) bool { return OlderLog(sorted[i], sorted[j]) })
	names := make([]string, len(sorted))
	recovered := make([]journal.Recovery, len(sorted))
	for i, f := range sorted {
		names[i] = f.Name
		r, err := read(f.Name)
		if err != nil {
			return "", fmt.Errorf("restart: read %s to find the latest run: %w", f.Name, err)
		}
		recovered[i] = r
	}
	latest, _ := Classify(names, recovered).Latest()
	return latest, nil
}
