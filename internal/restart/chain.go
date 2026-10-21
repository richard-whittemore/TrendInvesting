package restart

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"
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
	// Intact is the runs newer than the break, oldest first, the current
	// run last: the part of the chain that was followed.
	Intact []Link
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
// does, and stops at a first run: an approval that names no previous log and
// states the starting book, or a dry run's log holding no approval, since a
// dry run's first run is started with none (ADR 0027, the restart model). A
// paper or live log with no approval states no starting book, so it is a
// broken link, as is a dry-run log holding nothing past its configuration,
// which cannot be told from a fresh run's start cut short before its
// approval was recorded. Each earlier log must be the one the approval names,
// by PreviousLogApproved, and of the current log's account and mode, so no
// other account's or mode's records join the history. A chain that cannot be
// followed is refused with a *ChainError naming the link, never cut short: a
// history that silently starts late would read as the whole one. The error
// holds the intact runs newer than the break, for a reader that shows those
// alone and says where they start. The current log's own final hash is
// named by no approval, so it is not checked.
func FollowChain(current string, read LogReader) ([]Link, error) {
	recovery, err := read(current)
	if err != nil {
		return nil, &ChainError{Run: current, Reason: fmt.Sprintf("that log cannot be read (%v)", err)}
	}
	want, refused := identityOf(current, recovery)
	if refused != nil {
		return nil, refused
	}
	seen := map[string]bool{current: true}
	var newestFirst []Link
	refuse := func(e *ChainError) ([]Link, error) {
		e.Intact = oldestFirst(newestFirst)
		return nil, e
	}
	name := current
	for {
		approval, refused := approvalOf(name, recovery)
		if refused != nil {
			return refuse(refused)
		}
		if approval == nil && (want.Mode != event.TradingModeDryRun || CutShort(recovery)) {
			return refuse(unapprovedLink(current, name, newestFirst, want.Mode))
		}
		newestFirst = append(newestFirst, Link{Name: name, Recovery: recovery, Approval: approval})
		if approval == nil || approval.FirstRun() {
			break
		}
		// A previous log that is a path, not a file name beside this one,
		// does not decode (Validate), so it is refused in approvalOf.
		previous := approval.PreviousLog
		if seen[previous] {
			return refuse(&ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("the approvals from %s name %s twice; the chain of runs must end at the first run", current, previous)})
		}
		seen[previous] = true
		earlier, err := read(previous)
		if err != nil {
			return refuse(&ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("that log cannot be read (%v): restore it from a backup of the log as the engine wrote it; if it cannot be recovered, the history starts at the first intact run, and a log moved into quarantine is never put back", err)})
		}
		if !PreviousLogApproved(earlier, *approval) {
			return refuse(&ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("the approval names final record hash %s, but that log's final record hash is %s: it is not the log the owner approved", approval.PreviousLogFinalHash, earlier.FinalRecordHash)})
		}
		got, refused := identityOf(previous, earlier)
		if refused != nil {
			refused.Run, refused.Previous = name, previous
			return refuse(refused)
		}
		if got != want {
			return refuse(&ChainError{Run: name, Previous: previous, Reason: fmt.Sprintf("that log is of account %q in mode %q, but %s is of account %q in mode %q: a chain of runs is one account's in one mode", got.AccountID, got.Mode, current, want.AccountID, want.Mode)})
		}
		name, recovery = previous, earlier
	}
	return oldestFirst(newestFirst), nil
}

// unapprovedLink is the refusal of name, a log holding no approval that is
// not a dry run's first run, named with the run whose approval names it.
func unapprovedLink(current, name string, newestFirst []Link, mode string) *ChainError {
	reason := fmt.Sprintf("it holds no run approval, so it states no starting book and is not a first run: a %s run is started with an approval, and a log without one is a start cut short before it was recorded", mode)
	if mode == event.TradingModeDryRun {
		reason = "it holds no run approval and nothing past its configuration, so it cannot be told from a fresh run's start cut short before its approval was recorded"
	}
	if name == current {
		return &ChainError{Run: name, Reason: reason}
	}
	return &ChainError{Run: newestFirst[len(newestFirst)-1].Name, Previous: name, Reason: reason}
}

// oldestFirst returns the links of newestFirst in the opposite order.
func oldestFirst(newestFirst []Link) []Link {
	chain := make([]Link, len(newestFirst))
	for i, l := range newestFirst {
		chain[len(newestFirst)-1-i] = l
	}
	return chain
}

// identity is what a chain of runs is matched on: one account in one mode.
type identity struct{ AccountID, Mode string }

// identityOf returns the account and mode the run's configuration input
// records. Every version of the target-weight configuration records both.
func identityOf(name string, recovery journal.Recovery) (identity, *ChainError) {
	for _, entry := range recovery.Entries {
		env := entry.Envelope
		if entry.Kind != journal.KindInput || env.Type != event.TargetWeightConfigurationEventType {
			continue
		}
		var id struct {
			AccountID string `json:"account_id"`
			Mode      string `json:"mode"`
		}
		if err := json.Unmarshal(env.Payload, &id); err != nil || id.AccountID == "" || !event.ValidTradingMode(id.Mode) {
			return identity{}, &ChainError{Run: name, Reason: "its configuration does not name its account and a mode of dry-run, paper or live"}
		}
		return identity{id.AccountID, id.Mode}, nil
	}
	return identity{}, &ChainError{Run: name, Reason: "it records no target-weight configuration, so its account and mode are unknown"}
}

// approvalOf returns the run's one committed approval, nil when it holds
// none; two, or one that does not decode, refuse.
func approvalOf(name string, recovery journal.Recovery) (*event.OperatorRunApprovedPayload, *ChainError) {
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

// ListLogs returns the durable logs in dir, the regular files named with
// suffix, in OlderLog order. A log that vanishes while it is listed is
// skipped; one that cannot be examined for any other reason refuses, so the
// latest run is never passed over silently.
func ListLogs(dir, suffix string) ([]LoggedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list the durable log's directory %s: %w", dir, err)
	}
	return listed(dir, suffix, entries)
}

// listed is ListLogs of the entries read from dir.
func listed(dir, suffix string, entries []fs.DirEntry) ([]LoggedFile, error) {
	var logs []LoggedFile
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("examine %s in %s: %w", entry.Name(), dir, err)
		}
		logs = append(logs, LoggedFile{Name: entry.Name(), Modified: info.ModTime()})
	}
	sort.Slice(logs, func(i, j int) bool { return OlderLog(logs[i], logs[j]) })
	return logs, nil
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
