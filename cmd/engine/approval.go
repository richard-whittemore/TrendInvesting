package main

import (
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
// ends in, read from its strategy.engine.state records: leaving Degraded or
// Halted is never automatic (ADR 0019, amended 2026-09-24, "The state
// survives a restart"), so the approval either clears the incident by its
// ID or carries it in force at least as tight.
//
// A fresh run records its configuration, then its approval, one input at a
// time, so a crash between the two leaves a log holding nothing past its
// configuration. Resuming that log alone would start from no latch and no
// incident, losing the approved state in force, so it resumes only with
// -approval, checked against the run before it as a fresh run's is, and
// recorded as its second input.
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
	latest, err := latestLog(dir, "")
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
	return checkApproval(opts, comp, dir, latest)
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
	_, resumed := filepath.Split(opts.resumePath)
	before, err := latestLog(dir, resumed)
	if err != nil {
		return nil, err
	}
	pending := before != "" && len(recovery.Entries) <= 1
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
	if err := checkApprovedLatch(recovery.Entries, approval, latest); err != nil {
		return nil, err
	}
	if err := checkIncidentInForce(recovery.Entries, approval, latest); err != nil {
		return nil, err
	}
	return &approval, nil
}

// checkIncidentInForce refuses an approval that would loosen the engine
// state the approved log ends in: its strategy.engine.state decisions folded
// as the gate folds them, Halted overriding and Degraded scopes widening to
// their union (internal/risklimit). Clearing the incident by its ID is the
// owner's recovery step (ADR 0019); otherwise the incident in force must be
// Halted, or Degraded over at least the same instruments.
func checkIncidentInForce(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	state, scope := "", []string(nil)
	for _, entry := range entries {
		env := entry.Envelope
		if entry.Kind != journal.KindDecision || env.Type != event.EngineStateEventType {
			continue
		}
		var p event.EngineStatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
		}
		if p.State == event.EngineStateHalted || state == event.EngineStateHalted {
			state, scope = event.EngineStateHalted, nil
			continue
		}
		state = event.EngineStateDegraded
		for _, id := range p.ScopeInstrumentIDs {
			if !slices.Contains(scope, id) {
				scope = append(scope, id)
			}
		}
	}
	if state == "" || approval.IncidentID != "" {
		return nil
	}
	in := approval.IncidentInForce
	switch {
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
	latest, err := latestLog(dir, "")
	if err != nil {
		return err
	}
	prefix, _ := filepath.Split(opts.logPath)
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

// latestLog returns the name of the most recently modified durable log in
// dir other than except, or "" when there is none. Two logs modified at the
// same instant are ordered by name, the later name being the latest.
func latestLog(dir, except string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("engine: list the durable log's directory %s: %w", dir, err)
	}
	type found struct {
		name     string
		modified time.Time
	}
	var logs []found
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), durableLogSuffix) || entry.Name() == except {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("engine: examine %s in %s: %w", entry.Name(), dir, err)
		}
		logs = append(logs, found{entry.Name(), info.ModTime()})
	}
	if len(logs) == 0 {
		return "", nil
	}
	sort.Slice(logs, func(i, j int) bool {
		if !logs[i].modified.Equal(logs[j].modified) {
			return logs[i].modified.Before(logs[j].modified)
		}
		return logs[i].name < logs[j].name
	})
	return logs[len(logs)-1].name, nil
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
