package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// durableLogSuffix names a durable log in an engine's log directory. A run
// is recorded there as <name>.log, so any such file is an earlier run.
const durableLogSuffix = ".log"

// The restart model (owner decision 2026-10-16): this file finds, orders
// and reads the logs, and decides which refusal each case gets. The rules an
// approval is checked against, the latch and the incident in force, live in
// internal/restart (CheckApprovedLatch, CheckIncidentInForce) and are not
// restated here. A crash resumes from the durable log with -resume (ADR
// 0027), and -resume must name the latest log in the directory of -log;
// every other restart is a fresh run, which decides nothing until the
// owner's -approval, a file holding an operator.run.approved payload naming
// the latest earlier log and its final record hash, is recorded as its input.
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
// same header: the configuration only, or more. Such a log is an abandoned
// attempt, not a run: it ends in a cut-down copy of the old run's state, so
// it is never the latest log and never the run before a start (runsByAge);
// it stays in the directory as evidence (ADR 0018). It is attributed to the
// newest log it is a prefix of, the one it copied. A fresh run never opens
// with the configuration input an earlier log opens with, so a fresh run cut
// short after its configuration is never mistaken for one
// (freshRunApproval).
//
// A run creates its log, header first, before it commits its first record,
// so a fresh run that crashes in that window, like a resume that crashes
// before its first copy, leaves a log holding no committed record. Nothing
// in it tells the two apart, and it holds nothing to resume, so it is an
// abandoned start: never a run, never the source of an abandoned attempt,
// and a resume of it is refused (checkResumesLatest); the fresh run is
// started again, or, if the latest run crashed, that run is resumed.
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
// Every *.log in the root of the directory of -log is read; a log whose
// content the engine cannot recover (ADR 0027 §5) refuses every start until
// the operator moves it, unchanged, into the directory's quarantine folder,
// which the engine never reads (ADR 0018: evidence is never edited or
// deleted). A log the engine cannot open or read, for its permissions or a
// disk fault, refuses every start too. Three cases are told apart
// (readFault): a permission fault, where the fix is the file's owner or mode;
// a disk fault on a log that is or may be the latest run, where the fix is
// the disk and the log is never set aside while the disk may yet be fixed,
// for it may be the intact crashed run, and a disk written off is given up by
// the runbook's give-up steps; and a disk fault on a superseded log, which is set aside only after
// the runbook's journal check shows a later run superseded it.
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
		return firstRunApproval(opts, comp, dir)
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
	if restart.AttemptAtStart(previous, logHeader(comp), comp.configEnvelope) {
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

// reachesAccount reports whether a run of mode can reach a broker account
// whose holdings it must not adopt unchecked: paper and live. Its first run
// is approved with the starting book (firstRunApproval).
func reachesAccount(mode string) bool { return mode == modePaper || mode == modeLive }

// firstRunApproval returns the approval a first run in dir records: a first
// run's approval stating the starting book, the owner-confirmed holdings and
// cash the IBKR adapter compares the run's first reading after a close with,
// so IBKR's book is never adopted unchecked (ADR 0019 and ADR 0027, amended
// 2026-10-18). A paper or live first run needs one; any other first run may
// state one; a fresh run's approval, which names an earlier run, is refused.
func firstRunApproval(opts options, comp composition, dir string) (*event.OperatorRunApprovedPayload, error) {
	if opts.approvalPath == "" {
		if reachesAccount(opts.mode) {
			return nil, fmt.Errorf("engine: %s holds no earlier run, so this is the account's first run, and a first %s run decides nothing until the owner's approval states the starting book, the holdings and cash checked in IBKR that the adapter compares its first reading with: pass -approval with the file `operator approve -first-run` writes (docs/runbooks/first-paper-order.md)", dir, opts.mode)
		}
		return nil, nil
	}
	approval, err := readApproval(opts.approvalPath)
	if err != nil {
		return nil, err
	}
	if !approval.FirstRun() {
		return nil, fmt.Errorf("engine: the approval in %s names an earlier run, %q, but %s holds no earlier run's durable log (*%s): this is a first run, approved with the starting book and no previous log (`operator approve -first-run`)", opts.approvalPath, approval.PreviousLog, dir, durableLogSuffix)
	}
	if approval.AccountID != comp.accountID {
		return nil, fmt.Errorf("engine: the approval in %s is for account %q; this configuration decides %q", opts.approvalPath, approval.AccountID, comp.accountID)
	}
	return &approval, nil
}

// logOpeningWith returns the name of a log in the directory of -log,
// abandoned attempts included, recorded under this composition's header
// and opening with its configuration input, or "" when there is none.
func logOpeningWith(opts options, comp composition) (string, error) {
	names, recovered, err := readLogs(opts)
	if err != nil {
		return "", err
	}
	for i, name := range names {
		if restart.OpensWith(recovered[i], logHeader(comp), comp.configEnvelope) {
			return name, nil
		}
	}
	return "", nil
}

// runBeforeStart returns the run a resume of resumed, a log cut short in
// its start, must treat as the one before that start: the latest other log
// in dir that is not itself an attempt at the same start
// (restart.AttemptAtStart),
// or "" when there is none. A resume of a cut-short start records its
// configuration before its approval, so a crash in that window again leaves
// another such attempt, and the approval that start was given still names
// the run before the first attempt.
func runBeforeStart(opts options, comp composition, resumed string) (string, error) {
	found, err := runsByAge(opts)
	if err != nil {
		return "", err
	}
	prefix, _ := filepath.Split(opts.logPath)
	logs := found.Runs
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i] == resumed {
			continue
		}
		other, err := recoverLogFile(prefix + logs[i])
		if err != nil {
			return "", err
		}
		if !restart.AttemptAtStart(other, logHeader(comp), comp.configEnvelope) {
			return logs[i], nil
		}
	}
	return "", nil
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
	if restart.CutShort(recovery) {
		_, resumed := filepath.Split(opts.resumePath)
		if before, err = runBeforeStart(opts, comp, resumed); err != nil {
			return nil, err
		}
	}
	pending := before != ""
	if restart.CutShort(recovery) && !pending && (opts.approvalPath != "" || reachesAccount(opts.mode)) {
		// A first run cut short before its approval was recorded resumes
		// with the approval it was started with, as a fresh run's does.
		return firstRunApproval(opts, comp, dir)
	}
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
	if approval.FirstRun() {
		return nil, fmt.Errorf("engine: the approval in %s is a first run's, but %s holds an earlier run, %s: approve the latest run, naming it and its final record hash (`operator approve -log`)", opts.approvalPath, dir, latest)
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
	if !restart.PreviousLogApproved(recovery, approval) {
		return nil, fmt.Errorf("engine: the approval in %s names %s with final record hash %s, but that log's final record hash is %s; the approval must be of the log as it stands",
			opts.approvalPath, latest, approval.PreviousLogFinalHash, recovery.FinalRecordHash)
	}
	entries, stateLog := recovery.Entries, latest
	if restart.CutShort(recovery) {
		if entries, stateLog, err = stateBefore(opts, latest); err != nil {
			return nil, err
		}
	}
	if err := restart.CheckApprovedLatch(entries, approval, stateLog); err != nil {
		return nil, err
	}
	if err := restart.CheckIncidentInForce(entries, approval, stateLog); err != nil {
		return nil, err
	}
	if err := restart.CheckSaleAttempt(entries, approval, stateLog); err != nil {
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
	found, err := runsByAge(opts)
	if err != nil {
		return nil, "", err
	}
	prefix, _ := filepath.Split(opts.logPath)
	logs, older := found.Runs, false
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
		if !restart.CutShort(recovery) {
			return recovery.Entries, logs[i], nil
		}
	}
	return nil, named, nil
}

// checkResumesLatest refuses a -resume that names a log other than the
// latest in the directory of -log.
func checkResumesLatest(opts options) error {
	dir, _ := splitAsSpelled(opts.logPath)
	latest, found, err := latestLog(opts)
	if err != nil {
		return err
	}
	prefix, _ := filepath.Split(opts.logPath)
	for _, start := range found.EmptyStarts {
		if !samePath(opts.resumePath, prefix+start) {
			continue
		}
		step := fmt.Sprintf("start the fresh run again, with its -as-of and -approval naming the latest run, %q; only if %q itself crashed, resume that log instead", latest, latest)
		if latest == "" {
			step = fmt.Sprintf("start the fresh run again; %s holds no earlier run, so it needs no approval", dir)
		}
		return fmt.Errorf("engine: -resume names %s, which holds no committed record: a fresh run that crashed before its first record, or a resume that crashed before its first copy, leaves such a log, an abandoned start with nothing to resume (ADR 0027, the restart model); keep %s as evidence and %s",
			opts.resumePath, start, step)
	}
	for attempt, source := range found.Abandoned {
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

// latestLog returns the name of the latest run in the directory of -log, or
// "" when there is none, and the logs runsByAge found there.
func latestLog(opts options) (latest string, found restart.Directory, err error) {
	if found, err = runsByAge(opts); err != nil {
		return "", found, err
	}
	latest, _ = found.Latest()
	return latest, found, nil
}

// runsByAge reads every log in the directory of -log, least recently
// modified first as logsByAge orders them, and sorts them into runs,
// abandoned resume attempts and abandoned starts (restart.Classify).
func runsByAge(opts options) (restart.Directory, error) {
	names, recovered, err := readLogs(opts)
	if err != nil {
		return restart.Directory{}, err
	}
	return restart.Classify(names, recovered), nil
}

// quarantineFolder names the folder in a log directory into which the
// operator moves a log the engine cannot recover, unchanged (ADR 0018). The
// engine reads only the directory's root, so it never reads that folder.
const quarantineFolder = "quarantine"

// readLogs returns the names of the logs in the directory of -log, least
// recently modified first as logsByAge orders them, and what each holds,
// naming the operator's step when one cannot be recovered. Every log is read
// to find the latest run, and none is skipped: one whose content cannot be
// recovered (ADR 0027 §5) stops every start until it is set aside, and one
// that cannot be opened or read stops every start until the fault is fixed,
// until it is set aside when it is not the latest run, or until the disk is
// written off and the runbook's give-up steps are followed (readFault).
func readLogs(opts options) ([]string, []journal.Recovery, error) {
	dir, _ := splitAsSpelled(opts.logPath)
	logs, err := logsByAge(dir)
	if err != nil {
		return nil, nil, err
	}
	prefix, _ := filepath.Split(opts.logPath)
	names := make([]string, len(logs))
	recovered := make([]journal.Recovery, len(logs))
	failed := make([]error, len(logs))
	for i, log := range logs {
		names[i] = log.Name
		recovered[i], failed[i] = recoverLogFile(prefix + log.Name)
	}
	for i, err := range failed {
		var unrecoverable *unrecoverableLogError
		switch {
		case err == nil:
		case errors.As(err, &unrecoverable):
			return nil, nil, fmt.Errorf("%w; every log in the directory is read to find the latest run, so move %s, unchanged, into %s and start again, never editing or deleting it (ADR 0018; docs/runbooks/durable-log.md, \"An unrecoverable log\")",
				err, names[i], prefix+quarantineFolder)
		default:
			return nil, nil, readFault(err, prefix, logs, recovered, failed, i)
		}
	}
	return names, recovered, nil
}

// readFault is the refusal for logs[i], which could not be opened or read,
// naming the operator's step for the case restart.ClassifyReadFault finds:
// a permission fault, fixed at the file; a log that is or may be the latest
// run, never set aside while the disk may yet be fixed, and given up only by
// the runbook's give-up steps; or a superseded log, set aside only after the
// check the runbook gives.
func readFault(err error, prefix string, logs []restart.LoggedFile, recovered []journal.Recovery, failed []error, i int) error {
	read := make([]restart.ReadLog, len(logs))
	for j, log := range logs {
		read[j] = restart.ReadLog{Name: log.Name, Modified: log.Modified, Recovery: recovered[j], Err: failed[j]}
	}
	name := logs[i].Name
	switch kind, newer := restart.ClassifyReadFault(read, i); kind {
	case restart.PermissionFault:
		return fmt.Errorf("%w; %s could not be read, as the engine is not permitted to read it, not for damage to what it holds: fix its owner or mode and start again, leaving the log where it is (docs/runbooks/durable-log.md, \"A log that cannot be read\")",
			err, name)
	case restart.MayBeLatestFault:
		return fmt.Errorf("%w; %s could not be read, a fault in reading it, such as the disk, not damage to what it holds, and no log holding a record was modified after it, so it is or may be the latest run, an intact crashed run: fix the fault and start again, leaving the log where it is: never set aside while the disk may yet be fixed; if the disk is written off, follow the runbook's give-up steps (docs/runbooks/durable-log.md, \"A log that cannot be read\")",
			err, name)
	default:
		return fmt.Errorf("%w; %s could not be read, a fault in reading it, such as the disk, not damage to what it holds: fix the fault and start again; if it persists, %s, a log holding a record, was modified after it, so it is not the latest run unless %s is a resume of it abandoned partway: once the service journal shows a start after it was last written that finished its resume or recorded an approval, move %s, unchanged, into %s and start again, never editing or deleting it (ADR 0018; docs/runbooks/durable-log.md, \"A log that cannot be read\")",
			err, name, newer, newer, name, prefix+quarantineFolder)
	}
}

// logHeader is the durable log header this composition records under.
func logHeader(comp composition) journal.LogHeader {
	return journal.NewLogHeader(comp.configurationHash, comp.strategyVersion)
}

// logsByAge returns the durable logs in dir, least recently modified first
// (restart.ListLogs, the listing the dashboard finds the latest run by).
func logsByAge(dir string) ([]restart.LoggedFile, error) {
	logs, err := restart.ListLogs(dir, durableLogSuffix)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return logs, nil
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
	// A fresh run's approval is named by the run it follows; a first run's,
	// which follows none, by its declared start and its own payload, so two
	// first runs of other starts are two runs (the run's identity, ADR 0027).
	id := "operator-run-approved:" + approval.PreviousLogFinalHash
	if approval.FirstRun() {
		id = "operator-run-approved:first-run:" + asOf.UTC().Format(time.RFC3339) + ":" + event.HashPayload(encoded)
	}
	return event.Envelope{
		ID:                id,
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
