package restart

import (
	"bytes"
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

// Link is one run of a chain: its log's name, what it holds, the mode its
// configuration records, and the approval it was started with, nil for a
// run that holds none.
type Link struct {
	Name     string
	Recovery journal.Recovery
	Mode     string
	Approval *event.OperatorRunApprovedPayload
}

// The causes of a broken chain of runs, one per kind of break, so a reader
// can tell one break from another at the same link.
const (
	// CauseUnreadable is a log that cannot be read or recovered.
	CauseUnreadable = "unreadable"
	// CauseHashMismatch is an earlier log that is not the one the approval
	// names: its final record hash differs.
	CauseHashMismatch = "hash-mismatch"
	// CauseIdentity is a log of another account, or one whose
	// configuration does not name its account and mode.
	CauseIdentity = "identity"
	// CauseUnapproved is a log holding no approval that is not a dry run's
	// first run.
	CauseUnapproved = "unapproved"
	// CauseApproval is a log holding more than one approval, or one that
	// does not read.
	CauseApproval = "approval"
	// CauseLoop is a chain of approvals that names a log twice.
	CauseLoop = "loop"
)

// ChainError is a chain of runs that cannot be followed back to its first
// run. Run is the log whose approval names the broken link, and Previous the
// log it names: the link a person looks at. Cause is the kind of break.
type ChainError struct {
	Run, Previous string
	Cause         string
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
// broken link, as is an earlier dry-run log holding nothing past its
// configuration, which cannot be told from a fresh run's start cut short
// before its approval was recorded. The current log is the head of the
// chain, named by no other log, so the current log holding only its
// configuration, in any mode, is a start in progress, not a break: it is
// returned as the chain's one link (StartInProgress). Each earlier log must be the one the approval names,
// by PreviousLogApproved, and of the current log's account, so no other
// account's records join the history. Its mode may differ: the engine
// accepts a fresh run's approval naming the latest log whatever its mode,
// so a paper run approved against a dry-run log, or a live run against a
// paper log, is a link like any other, its hash checked; each Link carries
// its mode, for a reader that keeps one mode's runs. A chain that cannot be
// followed is refused with a *ChainError naming the link, never cut short: a
// history that silently starts late would read as the whole one. The error
// holds the intact runs newer than the break, for a reader that shows those
// alone and says where they start. The current log's own final hash is
// named by no approval, so it is not checked.
func FollowChain(current string, read LogReader) ([]Link, error) {
	recovery, err := read(current)
	if err != nil {
		return nil, &ChainError{Run: current, Cause: CauseUnreadable, Reason: fmt.Sprintf("that log cannot be read (%v)", err)}
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
	name, mode := current, want.Mode
	for {
		approval, refused := approvalOf(name, recovery)
		if refused != nil {
			return refuse(refused)
		}
		starting := name == current && approval == nil && CutShort(recovery)
		if approval == nil && !starting && (mode != event.TradingModeDryRun || CutShort(recovery)) {
			return refuse(unapprovedLink(current, name, newestFirst, mode))
		}
		newestFirst = append(newestFirst, Link{Name: name, Recovery: recovery, Mode: mode, Approval: approval})
		if approval == nil || approval.FirstRun() {
			break
		}
		// A previous log that is a path, not a file name beside this one,
		// does not decode (Validate), so it is refused in approvalOf.
		previous := approval.PreviousLog
		if seen[previous] {
			return refuse(&ChainError{Run: name, Previous: previous, Cause: CauseLoop, Reason: fmt.Sprintf("the approvals from %s name %s twice; the chain of runs must end at the first run", current, previous)})
		}
		seen[previous] = true
		earlier, err := read(previous)
		if err != nil {
			return refuse(&ChainError{Run: name, Previous: previous, Cause: CauseUnreadable, Reason: fmt.Sprintf("that log cannot be read (%v): restore it from a backup of the log as the engine wrote it, keeping its modification time (cp -p or rsync -a) so it stays older than the current log, owned by the engine's user; if it cannot be recovered, the history starts at the first intact run, and a log moved into quarantine is never put back", err)})
		}
		if !PreviousLogApproved(earlier, *approval) {
			return refuse(&ChainError{Run: name, Previous: previous, Cause: CauseHashMismatch, Reason: fmt.Sprintf("the approval names final record hash %s, but that log's final record hash is %s: it is not the log the owner approved", approval.PreviousLogFinalHash, earlier.FinalRecordHash)})
		}
		got, refused := identityOf(previous, earlier)
		if refused != nil {
			refused.Run, refused.Previous = name, previous
			return refuse(refused)
		}
		if got.AccountID != want.AccountID {
			return refuse(&ChainError{Run: name, Previous: previous, Cause: CauseIdentity, Reason: fmt.Sprintf("that log is of account %q, but %s is of account %q: a chain of runs is one account's", got.AccountID, current, want.AccountID)})
		}
		name, recovery, mode = previous, earlier, got.Mode
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
		return &ChainError{Run: name, Cause: CauseUnapproved, Reason: reason}
	}
	return &ChainError{Run: newestFirst[len(newestFirst)-1].Name, Previous: name, Cause: CauseUnapproved, Reason: reason}
}

// oldestFirst returns the links of newestFirst in the opposite order.
func oldestFirst(newestFirst []Link) []Link {
	chain := make([]Link, len(newestFirst))
	for i, l := range newestFirst {
		chain[len(newestFirst)-1-i] = l
	}
	return chain
}

// identity is the account and mode a run's configuration records: a chain
// of runs is one account's, and each link carries its mode.
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
			return identity{}, &ChainError{Run: name, Cause: CauseIdentity, Reason: "its configuration does not name its account and a mode of dry-run, paper or live"}
		}
		return identity{id.AccountID, id.Mode}, nil
	}
	return identity{}, &ChainError{Run: name, Cause: CauseIdentity, Reason: "it records no target-weight configuration, so its account and mode are unknown"}
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
			return nil, &ChainError{Run: name, Cause: CauseApproval, Reason: "it holds more than one run approval; a run holds at most one"}
		}
		approval, err := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
		if err != nil {
			return nil, &ChainError{Run: name, Cause: CauseApproval, Reason: fmt.Sprintf("its run approval does not read: %v", err)}
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

// ChainHead returns the current run among the logs of one directory: the log
// no other log's approval names as its previous log, the head of the chain
// of runs. Each log's approval is its second record (a run records its
// configuration, then its approval, ADR 0027), so start, which answers with
// at least a log's first two records, is all most logs are read by. Logs
// that tie, the log a resume copied and the resumed run's log, an abandoned
// resume attempt or start, are decided by the engine's own rule (LatestRun):
// only the logs that share the newest tied log's start are read whole, by
// read, since only such a log can be an attempt of another. A directory in
// which every log is named, a loop, falls back to that rule over every log.
//
// newer is the newest log that some approval names as a previous log but
// that is later in OlderLog order than the head: a log restored from a
// backup without its modification time, which the engine, finding the
// latest run by modification time, would take for the latest run at its
// next start. It is "" when there is none. A log whose start cannot be read
// refuses, as LatestRun does.
func ChainHead(files []LoggedFile, start, read LogReader) (head, newer string, err error) {
	sorted := slices.Clone(files)
	sort.Slice(sorted, func(i, j int) bool { return OlderLog(sorted[i], sorted[j]) })
	starts := make([]journal.Recovery, len(sorted))
	named := map[string]bool{}
	for i, f := range sorted {
		r, err := start(f.Name)
		if err != nil {
			return "", "", fmt.Errorf("restart: read %s to find the current run: %w", f.Name, err)
		}
		starts[i] = r
		// A log whose approval does not read names no log here; following
		// the chain refuses it, naming it.
		if approval, refused := approvalOf(f.Name, r); refused == nil && approval != nil && !approval.FirstRun() {
			named[approval.PreviousLog] = true
		}
	}
	var tied []int
	for i, f := range sorted {
		if !named[f.Name] && len(starts[i].Entries) > 0 {
			tied = append(tied, i)
		}
	}
	switch {
	case len(tied) == 0:
		head, err = LatestRun(sorted, read)
	default:
		head, err = latestOfTied(sorted, starts, tied, read)
	}
	if err != nil || head == "" {
		return head, "", err
	}
	after := false
	for _, f := range sorted {
		if f.Name == head {
			after = true
			continue
		}
		if after && named[f.Name] {
			newer = f.Name
		}
	}
	return head, newer, nil
}

// latestOfTied is the engine's latest run among the tied logs, sorted[i] for
// each i in tied, oldest first. The newest is the latest run unless it is an
// abandoned attempt, a prefix of an older log, which can only be a log
// sharing its start; those are read whole and classified together, and the
// next newest is tried when every such log is set aside.
func latestOfTied(sorted []LoggedFile, starts []journal.Recovery, tied []int, read LogReader) (string, error) {
	// The oldest tied log has no older one to be an attempt of.
	for k := len(tied) - 1; k > 0; k-- {
		newest := tied[k]
		var group []LoggedFile
		for _, i := range tied[:k] {
			if sameStart(starts[i], starts[newest]) {
				group = append(group, sorted[i])
			}
		}
		if len(group) == 0 {
			return sorted[newest].Name, nil
		}
		latest, err := LatestRun(append(group, sorted[newest]), read)
		if err != nil {
			return "", err
		}
		if latest == sorted[newest].Name {
			return latest, nil
		}
	}
	return sorted[tied[0]].Name, nil
}

// sameStart reports whether two logs' first records agree, as a resume's log
// and the log it copied, or an abandoned attempt and its source, do: the
// same header and, over the records both starts hold, the same records.
func sameStart(a, b journal.Recovery) bool {
	if a.Header != b.Header {
		return false
	}
	for k := range min(len(a.Entries), len(b.Entries)) {
		if a.Entries[k].Kind != b.Entries[k].Kind || !bytes.Equal(event.CanonicalEnvelopeBytes(a.Entries[k].Envelope), event.CanonicalEnvelopeBytes(b.Entries[k].Envelope)) {
			return false
		}
	}
	return true
}

// StartInProgress reports whether chain, as FollowChain returns it, is a
// start in progress: the current log alone, holding no approval and nothing
// past its configuration. A run records its configuration, then its
// approval or, a dry run's first run, its first input (ADR 0027, the restart
// model), so such a run has no earlier run yet and no history to show.
func StartInProgress(chain []Link) bool {
	return len(chain) == 1 && chain[0].Approval == nil && CutShort(chain[0].Recovery)
}
