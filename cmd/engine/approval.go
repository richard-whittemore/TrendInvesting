package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// durableLogSuffix names a durable log in an engine's log directory. A run
// is recorded there as <name>.log, so any such file is an earlier run.
const durableLogSuffix = ".log"

// The restart model (owner decision 2026-10-16): a crash resumes from the
// durable log with -resume (ADR 0027); every other restart, leaving Halted,
// a build upgrade or a cleared rebalance, is a fresh run, and a fresh run of
// an engine whose log directory already holds an earlier run decides nothing
// until the owner's approval is recorded as its input. -approval names a
// file holding that operator.run.approved payload; it must name the latest
// earlier log in the directory and that log's final record hash, so what is
// approved is the evidence the owner reviewed.
//
// A crash is resumed, and the crashed run is always the latest one, so
// -resume must name the latest log in the directory of -log; resuming an
// older log would continue a history a newer run has already moved past.
//
// The approved latch must be the one the approved log ends with: its last
// strategy.targets.decided's latched_after, or, when that run decided
// nothing, the latch its own approval stated (ADR 0026, point 5: the latch
// is restored, never recomputed). A log with neither leaves the approval's
// latch as the fresh run's start.
//
// The approved incident state must not loosen the one the approved log
// ends in, read from the incident its own approval carried in force and its
// strategy.engine.state records after it: leaving Degraded or
// Halted is never automatic (ADR 0019, amended 2026-09-24, "The state
// survives a restart"), so the approval either clears that incident by its
// ID or carries it in force, under that ID, at least as tight. The
// incident's ID is the one the inherited incident in force carries, or else
// the ID of the first strategy.engine.state record out of Normal
// (event.IncidentInForce). An approval of a log that ended Normal clears no
// incident and carries none in force.
//
// A log holding nothing past its configuration decided nothing and records
// no state, so the state an approval of it is checked against is the latest
// earlier log holding more (stateBefore); the approval still names the
// latest log and its final record hash.
//
// A resume creates its new log, then copies the old log's committed groups
// into it one by one, the configuration first (ADR 0027 §6), so a resume
// refused for divergence, or a crash before or during the copy, leaves a
// newer log whose committed records are a prefix of the old one's under the
// same header: empty, the configuration only, or more. Such a log is an
// abandoned attempt, not a run: it ends in a cut-down copy of the old run's
// state, so it is never the latest log and never the run before a start
// (runsByAge); it stays in the directory as evidence (ADR 0018). A fresh
// run never opens with the configuration input an earlier log opens with,
// so a fresh run cut short is never mistaken for one (freshRunApproval).
//
// A fresh run records its configuration, then its approval, one input at a
// time, so a crash between the two leaves a log holding nothing past its
// configuration. Resuming that log alone would start from no latch and no
// incident, losing the approved state in force, so it resumes only with
// -approval, checked against the run before it as a fresh run's is, and
// recorded as its second input. A crash of that resume in the same window
// leaves a copy of that log, an abandoned attempt (runsByAge), so the same
// log is resumed again with the same approval; the run before it is the
// latest run that is not an attempt at the same start (runBeforeStart), and
// a fresh run never repeats the start of an earlier log (ADR 0027, the
// restart model).
//
// Every *.log in the root of the directory of -log is read; a log the
// engine cannot recover (ADR 0027 §5) refuses every start until the
// operator moves it, unchanged, into the directory's quarantine folder,
// which the engine never reads (ADR 0018: evidence is never edited or
// deleted).
//
// freshRunApproval applies these rules to one invocation and returns the
// approval to record, or nil when none is needed: a backtest, a resume of a
// log whose start is complete, or the first run in the directory. It runs
// before this process creates its log, so a refused start leaves nothing
// new behind.
func freshRunApproval(opts options, comp composition) (*event.OperatorRunApprovedPayload, error) {
	if opts.mode == modeBacktest {
		if opts.approvalPath != "" {
			return nil, errors.New("engine: -approval applies to a run that can reach a broker; a backtest is never approved")
		}
		return nil, nil
	}
	if opts.resumePath != "" {
		return resumeApproval(opts, comp)
	}
	dir, _ := splitAsSpelled(opts.logPath)
	latest, _, err := latestLog(opts)
	if err != nil {
		return nil, err
	}
	if latest == "" {
		if opts.approvalPath != "" {
			return nil, fmt.Errorf("engine: -approval names an earlier run, but %s holds no earlier run's durable log (*%s); a first run needs no approval", dir, durableLogSuffix)
		}
		return nil, nil
	}
	if opts.approvalPath == "" {
		return nil, fmt.Errorf("engine: %s already holds an earlier run's durable log, %s, so this is a fresh run, and a fresh run decides nothing until the owner's approval is recorded: pass -approval with an operator.run.approved file naming %s and its final record hash (docs/runbooks/durable-log.md, \"Restarting an engine\"); after a crash, -resume that log instead",
			dir, latest, latest)
	}
	approval, err := checkApproval(opts, comp, dir, latest)
	if err != nil {
		return nil, err
	}
	prefix, _ := filepath.Split(opts.logPath)
	previous, err := recoverLogFile(prefix + latest)
	if err != nil {
		return nil, err
	}
	if attemptAtStart(previous, comp) {
		return nil, fmt.Errorf("engine: %s holds nothing past its configuration, and this run's configuration input is the one it opens with, so a crash of this run before its approval was recorded would leave a log no resume could tell from another attempt at that same start: resume %s with -resume, or declare a new start with -as-of (ADR 0027, the restart model)",
			latest, latest)
	}
	if earlier, err := logOpeningWith(opts, comp); err != nil || earlier != "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine: %s opens with this run's configuration input, so a crash of this run before its approval was recorded would leave a log no resume could tell from a resume of %s abandoned after its configuration (ADR 0027 §6): declare a new start with -as-of (ADR 0027, the restart model)",
			earlier, earlier)
	}
	return approval, nil
}

// logOpeningWith returns the name of a log in the directory of -log,
// abandoned attempts included, recorded under this composition's header
// and opening with its configuration input, or "" when there is none.
func logOpeningWith(opts options, comp composition) (string, error) {
	dir, _ := splitAsSpelled(opts.logPath)
	names, err := logsByAge(dir)
	if err != nil {
		return "", err
	}
	prefix, _ := filepath.Split(opts.logPath)
	header := journal.NewLogHeader(comp.configurationHash, comp.strategyVersion)
	opening := event.CanonicalEnvelopeBytes(comp.configEnvelope)
	for _, name := range names {
		recovery, err := recoverRun(prefix, name)
		if err != nil {
			return "", err
		}
		if recovery.Header == header && len(recovery.Entries) > 0 && bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), opening) {
			return name, nil
		}
	}
	return "", nil
}

// runBeforeStart returns the run a resume of resumed, a log cut short in
// its start, must treat as the one before that start: the latest other log
// in dir that is not itself an attempt at the same start (attemptAtStart),
// or "" when there is none. A resume of a cut-short start records its
// configuration before its approval, so a crash in that window again leaves
// another such attempt, and the approval that start was given still names
// the run before the first attempt.
func runBeforeStart(opts options, comp composition, resumed string) (string, error) {
	logs, _, err := runsByAge(opts)
	if err != nil {
		return "", err
	}
	prefix, _ := filepath.Split(opts.logPath)
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i] == resumed {
			continue
		}
		other, err := recoverLogFile(prefix + logs[i])
		if err != nil {
			return "", err
		}
		if !attemptAtStart(other, comp) {
			return logs[i], nil
		}
	}
	return "", nil
}

// attemptAtStart reports whether recovery is a log of this composition's
// start that holds nothing past it: recorded under the same configuration
// and build, opening with this run's configuration input, if anything. Such
// a log decided nothing, and nothing in it tells one attempt at the start
// from another, so a fresh run never repeats that start (freshRunApproval).
func attemptAtStart(recovery journal.Recovery, comp composition) bool {
	if recovery.Header != journal.NewLogHeader(comp.configurationHash, comp.strategyVersion) || len(recovery.Entries) > 1 {
		return false
	}
	return len(recovery.Entries) == 0 ||
		bytes.Equal(event.CanonicalEnvelopeBytes(recovery.Entries[0].Envelope), event.CanonicalEnvelopeBytes(comp.configEnvelope))
}

// resumeApproval checks a -resume and returns the approval it records: nil,
// unless the resumed log's start was cut short before its approval was
// recorded.
func resumeApproval(opts options, comp composition) (*event.OperatorRunApprovedPayload, error) {
	if err := checkResumesLatest(opts); err != nil {
		return nil, err
	}
	recovery, err := recoverLogFile(opts.resumePath)
	if err != nil {
		return nil, err
	}
	dir, _ := splitAsSpelled(opts.logPath)
	before := ""
	if len(recovery.Entries) <= 1 {
		_, resumed := filepath.Split(opts.resumePath)
		if before, err = runBeforeStart(opts, comp, resumed); err != nil {
			return nil, err
		}
	}
	pending := before != ""
	switch {
	case pending && opts.approvalPath == "":
		return nil, fmt.Errorf("engine: %s holds nothing past its configuration, and %s holds an earlier run, %s, so its start was a fresh run whose approval was never recorded: resume it with -approval naming %s and its final record hash, the approval that run was started with (ADR 0027, the restart model)",
			opts.resumePath, dir, before, before)
	case !pending && opts.approvalPath != "":
		return nil, errors.New("engine: -approval and -resume cannot both be given: a crash resumes from its durable log as it stood, and any other restart is a fresh run started with -approval and no -resume; only a log cut short before its approval was recorded resumes with one")
	case !pending:
		return nil, nil
	}
	return checkApproval(opts, comp, dir, before)
}

// checkApproval reads the approval file and checks it against latest, the
// earlier run it must name, in dir.
func checkApproval(opts options, comp composition, dir, latest string) (*event.OperatorRunApprovedPayload, error) {
	approval, err := readApproval(opts.approvalPath)
	if err != nil {
		return nil, err
	}
	if approval.AccountID != comp.accountID {
		return nil, fmt.Errorf("engine: the approval in %s is for account %q; this configuration decides %q", opts.approvalPath, approval.AccountID, comp.accountID)
	}
	if approval.PreviousLog != latest {
		return nil, fmt.Errorf("engine: the approval in %s names %s, but the latest earlier run in %s is %s; approve the latest run", opts.approvalPath, approval.PreviousLog, dir, latest)
	}
	// The directory as spelled, separator included (filepath.Split), so the
	// log read is the one listed, never one a cleaned path would name.
	prefix, _ := filepath.Split(opts.logPath)
	recovery, err := recoverLogFile(prefix + latest)
	if err != nil {
		return nil, err
	}
	if recovery.FinalRecordHash != approval.PreviousLogFinalHash {
		return nil, fmt.Errorf("engine: the approval in %s names %s with final record hash %s, but that log's final record hash is %s; the approval must be of the log as it stands",
			opts.approvalPath, latest, approval.PreviousLogFinalHash, recovery.FinalRecordHash)
	}
	entries, stateLog := recovery.Entries, latest
	if len(entries) <= 1 {
		if entries, stateLog, err = stateBefore(opts, latest); err != nil {
			return nil, err
		}
	}
	if err := checkApprovedLatch(entries, approval, stateLog); err != nil {
		return nil, err
	}
	if err := checkIncidentInForce(entries, approval, stateLog); err != nil {
		return nil, err
	}
	return &approval, nil
}

// stateBefore returns the entries and name of the latest run older than
// named, a log holding nothing past its configuration, that holds more: the
// run whose state is still in force, since a log holding only its
// configuration decided nothing and records no state. It returns no entries
// when there is none, as for a first run.
func stateBefore(opts options, named string) ([]journal.Entry, string, error) {
	logs, _, err := runsByAge(opts)
	if err != nil {
		return nil, "", err
	}
	prefix, _ := filepath.Split(opts.logPath)
	older := false
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i] == named {
			older = true
			continue
		}
		if !older {
			continue
		}
		recovery, err := recoverLogFile(prefix + logs[i])
		if err != nil {
			return nil, "", err
		}
		if len(recovery.Entries) > 1 {
			return recovery.Entries, logs[i], nil
		}
	}
	return nil, named, nil
}

// checkIncidentInForce refuses an approval that would loosen the engine
// state the approved log ends in: the incident its own approval carried in
// force, then its strategy.engine.state decisions, folded as the gate folds
// them, Halted overriding and Degraded scopes widening to
// their union (internal/risklimit). Clearing the incident by its ID is the
// owner's recovery step (ADR 0019); otherwise the incident in force must be
// that incident, under its ID, Halted, or Degraded over at least the same
// instruments. The incident's ID is the inherited incident's, or else the
// ID of the first strategy.engine.state record out of Normal
// (event.IncidentInForce); a log that ended Normal has none to clear and
// none to carry in force.
func checkIncidentInForce(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	state, scope, incident := "", []string(nil), ""
	fold := func(id, next string, ids []string) {
		if incident == "" {
			incident = id
		}
		if next == event.EngineStateHalted || state == event.EngineStateHalted {
			state, scope = event.EngineStateHalted, nil
			return
		}
		state = event.EngineStateDegraded
		for _, id := range ids {
			if !slices.Contains(scope, id) {
				scope = append(scope, id)
			}
		}
	}
	for _, entry := range entries {
		env := entry.Envelope
		switch {
		case entry.Kind == journal.KindInput && env.Type == event.OperatorRunApprovedEventType:
			// The approved log's own approval: the incident it carried in
			// force was restored without a strategy.engine.state decision,
			// so it is the state the log starts in.
			inherited, err := event.DecodeOperatorRunApproved(env.Payload)
			if err != nil {
				return fmt.Errorf("engine: %s in %s: %w", env.ID, logName, err)
			}
			if in := inherited.IncidentInForce; in != nil {
				fold(in.IncidentID, in.State, in.ScopeInstrumentIDs)
			}
		case entry.Kind == journal.KindDecision && env.Type == event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			fold(env.ID, p.State, p.ScopeInstrumentIDs)
		}
	}
	in := approval.IncidentInForce
	switch {
	case state == "" && approval.IncidentID != "":
		return fmt.Errorf("engine: the approval clears incident %q, but %s records no incident: it ended normal, and an approval clears only the incident the log ended in (ADR 0019)", approval.IncidentID, logName)
	case state == "" && in != nil:
		return fmt.Errorf("engine: the approval carries incident %q in force, but %s records no incident: it ended normal, and an approval carries in force only the incident the log ended in (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", in.IncidentID, logName)
	case state == "":
		return nil
	case approval.IncidentID != "" && approval.IncidentID != incident:
		return fmt.Errorf("engine: the approval clears incident %q, but %s ended %s in incident %s: an approval clears only that incident, by its ID (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", approval.IncidentID, logName, state, incident)
	case in != nil && in.IncidentID != incident:
		return fmt.Errorf("engine: the approval carries incident %q in force, but %s ended %s in incident %s: the incident in force is that one, under its ID (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", in.IncidentID, logName, state, incident)
	case approval.IncidentID != "":
		return nil
	case in == nil:
		return fmt.Errorf("engine: %s ended %s, but the approval neither clears that incident (incident_id) nor carries it in force (incident_in_force): leaving Degraded or Halted is never automatic (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", logName, state)
	case in.State == event.EngineStateHalted:
		return nil
	case state == event.EngineStateHalted:
		return fmt.Errorf("engine: %s ended halted, but the approval carries its incident in force as %s: a restart never loosens Halted (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", logName, in.State)
	}
	for _, id := range scope {
		if !slices.Contains(in.ScopeInstrumentIDs, id) {
			return fmt.Errorf("engine: %s ended degraded on %s, but the approval's incident in force does not name %s: a restart never narrows a Degraded scope (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", logName, strings.Join(scope, ", "), id)
		}
	}
	return nil
}

// checkResumesLatest refuses a -resume that names a log other than the
// latest in the directory of -log.
func checkResumesLatest(opts options) error {
	dir, _ := splitAsSpelled(opts.logPath)
	latest, abandoned, err := latestLog(opts)
	if err != nil {
		return err
	}
	prefix, _ := filepath.Split(opts.logPath)
	for attempt, source := range abandoned {
		if samePath(opts.resumePath, prefix+attempt) {
			return fmt.Errorf("engine: -resume names %s, a resume of %s abandoned partway: its committed records are a prefix of that log's, so it is not a run (ADR 0027 §6, the restart model); resume the latest run, %q, and keep %s as evidence",
				opts.resumePath, source, latest, attempt)
		}
	}
	if latest == "" || !samePath(opts.resumePath, prefix+latest) {
		return fmt.Errorf("engine: -resume names %s, but the latest run in %s, the directory of -log, is %q: only a crash is resumed, and the crashed run is the latest one (ADR 0027, the restart model); resume that log, or start a fresh run with -approval",
			opts.resumePath, dir, latest)
	}
	return nil
}

// checkApprovedLatch refuses an approval whose latch is not the one the
// approved log ends with.
func checkApprovedLatch(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	stated := *approval.Boost.Latched
	for i := len(entries) - 1; i >= 0; i-- {
		env := entries[i].Envelope
		switch env.Type {
		case event.TargetsDecidedEventType:
			var decided event.TargetsDecidedPayload
			if err := json.Unmarshal(env.Payload, &decided); err != nil {
				return fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			if decided.Boost.LatchedAfter != stated {
				return fmt.Errorf("engine: %s's last decision, %s, has latched_after %t, but the approval states latched %t; the latch in force is restored, never changed by a restart (ADR 0026, point 5)",
					logName, env.ID, decided.Boost.LatchedAfter, stated)
			}
			return nil
		case event.OperatorRunApprovedEventType:
			previous, err := event.DecodeOperatorRunApproved(env.Payload)
			if err != nil {
				return fmt.Errorf("engine: %s in %s: %w", env.ID, logName, err)
			}
			if *previous.Boost.Latched != stated {
				return fmt.Errorf("engine: %s decided nothing, and the approval it started from, %s, states latched %t; this approval states latched %t, but the latch in force is restored, never changed by a restart (ADR 0026, point 5)",
					logName, env.ID, *previous.Boost.Latched, stated)
			}
			return nil
		}
	}
	return nil
}

// latestLog returns the name of the latest run in the directory of -log, or
// "" when there is none, and the abandoned resume attempts runsByAge left
// out.
func latestLog(opts options) (latest string, abandoned map[string]string, err error) {
	runs, abandoned, err := runsByAge(opts)
	if err != nil || len(runs) == 0 {
		return "", abandoned, err
	}
	return runs[len(runs)-1], abandoned, nil
}

// runsByAge returns the runs in the directory of -log, least recently
// modified first, as logsByAge orders them, leaving out each log whose
// committed records are a prefix of an older log's under the same header,
// however few, none included: a resume of that log abandoned partway (ADR
// 0027 §6: a resume's new log begins with the old log's committed part,
// byte for byte, the configuration first). It also returns the abandoned
// attempts, each mapped to the log it copied. A log holding only its
// configuration that is no such prefix is a start cut short
// (attemptAtStart, stateBefore), and stays a run.
func runsByAge(opts options) (runs []string, abandoned map[string]string, err error) {
	dir, _ := splitAsSpelled(opts.logPath)
	names, err := logsByAge(dir)
	if err != nil {
		return nil, nil, err
	}
	prefix, _ := filepath.Split(opts.logPath)
	recovered := make([]journal.Recovery, len(names))
	for i, name := range names {
		if recovered[i], err = recoverRun(prefix, name); err != nil {
			return nil, nil, err
		}
	}
	abandoned = map[string]string{}
	for i, name := range names {
		source := ""
		for j := range i {
			if committedPrefixOf(recovered[i], recovered[j]) {
				source = names[j]
				break
			}
		}
		if source != "" {
			abandoned[name] = source
			continue
		}
		runs = append(runs, name)
	}
	return runs, abandoned, nil
}

// committedPrefixOf reports whether attempt's committed entries, however
// few, are a prefix of source's under the same header, compared by kind and
// canonical envelope bytes as a resume compares them (ADR 0027 §6).
func committedPrefixOf(attempt, source journal.Recovery) bool {
	if attempt.Header != source.Header || len(attempt.Entries) > len(source.Entries) {
		return false
	}
	for k, entry := range attempt.Entries {
		other := source.Entries[k]
		if entry.Kind != other.Kind || !bytes.Equal(event.CanonicalEnvelopeBytes(entry.Envelope), event.CanonicalEnvelopeBytes(other.Envelope)) {
			return false
		}
	}
	return true
}

// quarantineFolder names the folder in a log directory into which the
// operator moves a log the engine cannot recover, unchanged (ADR 0018). The
// engine reads only the directory's root, so it never reads that folder.
const quarantineFolder = "quarantine"

// recoverRun recovers the log name in the directory spelled prefix,
// naming the operator's step when it cannot: every log in the directory is
// read to find the latest run, so one that cannot be recovered (ADR 0027
// §5) stops every start until it is set aside.
func recoverRun(prefix, name string) (journal.Recovery, error) {
	recovery, err := recoverLogFile(prefix + name)
	if err != nil {
		return journal.Recovery{}, fmt.Errorf("%w; every log in the directory is read to find the latest run, so move %s, unchanged, into %s and start again, never editing or deleting it (ADR 0018; docs/runbooks/durable-log.md, \"An unrecoverable log\")",
			err, name, prefix+quarantineFolder)
	}
	return recovery, nil
}

// logsByAge returns the names of the durable logs in dir, least recently
// modified first. Two logs modified at the same instant are ordered by name,
// the later name being the later log.
func logsByAge(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: list the durable log's directory %s: %w", dir, err)
	}
	type found struct {
		name     string
		modified time.Time
	}
	var logs []found
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), durableLogSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("engine: examine %s in %s: %w", entry.Name(), dir, err)
		}
		logs = append(logs, found{entry.Name(), info.ModTime()})
	}
	sort.Slice(logs, func(i, j int) bool {
		if !logs[i].modified.Equal(logs[j].modified) {
			return logs[i].modified.Before(logs[j].modified)
		}
		return logs[i].name < logs[j].name
	})
	names := make([]string, len(logs))
	for i, log := range logs {
		names[i] = log.name
	}
	return names, nil
}

// readApproval reads and validates an approval file, refusing a key it
// leaves out or one this build does not know rather than guessing.
func readApproval(path string) (event.OperatorRunApprovedPayload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("engine: read the approval: %w", err)
	}
	approval, err := event.DecodeOperatorRunApproved(raw)
	if err != nil {
		return event.OperatorRunApprovedPayload{}, fmt.Errorf("engine: the approval in %s is refused: %w", path, err)
	}
	return approval, nil
}

// approvalSequence is the approval input's sequence: straight after the
// configuration, so the adapter's first input carries approvalSequence+1.
const approvalSequence = configurationSequence + 1

// approvalEnvelope wraps the approval as the input this process delivers
// before it opens its socket. Its times are the run's declared start, as the
// configuration's are, so a journal's chain is repeatable (ADR 0017); when
// the owner approved is in the payload.
func approvalEnvelope(approval event.OperatorRunApprovedPayload, comp composition, asOf time.Time) (event.Envelope, error) {
	encoded, err := json.Marshal(approval)
	if err != nil {
		return event.Envelope{}, fmt.Errorf("engine: encode the approval payload: %w", err)
	}
	return event.Envelope{
		ID:                "operator-run-approved:" + approval.PreviousLogFinalHash,
		Type:              event.OperatorRunApprovedEventType,
		SchemaVersion:     event.OperatorRunApprovedSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         asOf,
		RecordedAt:        asOf,
		Sequence:          approvalSequence,
		Source:            sourceEngine,
		StrategyVersion:   comp.strategyVersion,
		ConfigurationHash: comp.configurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}, nil
}
