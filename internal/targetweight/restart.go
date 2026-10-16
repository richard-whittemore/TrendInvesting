package targetweight

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A restart reads an earlier run's recorded records back, never recomputing
// them from a fresh window of bars (ADR 0026, point 5). The restarted run is
// fed, straight after its configuration:
//
//  1. strategy.run.options: the first session to decide, unchanged from the
//     earlier run, and how many recorded records follow (the prelude);
//  2. the prelude: the earlier run's strategy.targets.decided,
//     strategy.order.planned, risk.order.released, risk.order.refused,
//     strategy.engine.state and strategy.restart.recovered records, and its
//     execution.order.lifecycle, execution.fill and operator.rebalance.cleared
//     inputs, in journal order. From them the run recovers Boost's latch and
//     target in force, the book in force with b and each sleeve's share,
//     every planned and released order with what the venue reported of it,
//     the rebalance in progress and its phase, the engine state and its
//     scope. When the prelude ends it emits strategy.restart.recovered;
//  3. the earlier run's market inputs again (calendar, bars, universe,
//     account snapshots, marks and session closes), from which the
//     indicators and the closes are rebuilt from the journal's own bars.
//     Sessions up to the last recovered decision's are never decided again.
//
// A rebalance in progress at the restart is held: nothing more is sent for its
// decision time until the owner clears it with operator.rebalance.cleared
// (plan.md section 5, "Fail closed"). RestartInputs builds items 1 to 3 from
// the earlier journal; the inputs that follow are the live ones.

// RestartInputs returns the inputs a restarted run is fed straight after its
// configuration, from earlier, the envelopes of the earlier run's journal in
// record order (inputs and decisions alike; the earlier run may itself have
// been a restart). opts are the earlier run's options, and at, the restart's
// time, stamps the run options input. Sequence, causation and correlation
// are cleared: the caller numbers the inputs as it feeds them.
func RestartInputs(earlier []event.Envelope, opts Options, at time.Time) ([]event.Envelope, error) {
	if opts.DecideFrom.IsZero() {
		return nil, errors.New("targetweight: the first session to decide is required")
	}
	if at.IsZero() {
		return nil, errors.New("targetweight: a restart needs its time")
	}
	if len(earlier) == 0 || earlier[0].Type != event.TargetWeightConfigurationEventType {
		return nil, errors.New("targetweight: an earlier journal opens with its configuration")
	}
	configuration := earlier[0]
	var prelude, history []event.Envelope
	for _, e := range earlier[1:] {
		if e.ConfigurationHash != configuration.ConfigurationHash {
			return nil, fmt.Errorf("targetweight: %s %s belongs to another configuration than the earlier run's", e.Type, e.ID)
		}
		e.Sequence, e.CausationID, e.CorrelationID = 0, "", ""
		switch e.Type {
		case event.TargetWeightConfigurationEventType:
			return nil, fmt.Errorf("targetweight: the earlier journal is configured twice (%s)", e.ID)
		case event.RunOptionsEventType, event.RunCompletedEventType:
		case event.TargetsDecidedEventType, event.OrderPlannedEventType, event.OrderReleasedEventType,
			event.OrderRefusedEventType, event.EngineStateEventType, event.RestartRecoveredEventType,
			event.OrderLifecycleEventType, event.FillEventType, event.RebalanceClearedEventType:
			prelude = append(prelude, e)
		default:
			history = append(history, e)
		}
	}
	payload := event.RunOptionsPayload{DecideFrom: opts.DecideFrom, RecoveredRecords: len(prelude)}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("targetweight: %w", err)
	}
	options := event.Envelope{
		ID: "run-options:" + at.UTC().Format(idTime), Type: event.RunOptionsEventType,
		EnvelopeVersion: event.CurrentEnvelopeVersion, SchemaVersion: event.RunOptionsSchemaVersion,
		EventTime: at, RecordedAt: at, Source: source,
		StrategyVersion: configuration.StrategyVersion, ConfigurationHash: configuration.ConfigurationHash,
		PayloadHash: event.HashPayload(raw), Payload: raw,
	}
	out := make([]event.Envelope, 0, 1+len(prelude)+len(history))
	out = append(out, options)
	out = append(out, prelude...)
	return append(out, history...), nil
}

// RecordedOptions returns the run options a journal's inputs record, so a
// replay builds its handler from the journal alone (ADR 0016: run-level
// facts are recorded beside the configuration hash).
func RecordedOptions(inputs []event.Envelope) (Options, error) {
	if len(inputs) < 2 || inputs[1].Type != event.RunOptionsEventType {
		return Options{}, errors.New("targetweight: the journal records no run options straight after its configuration; replay must be told the first session to decide")
	}
	p, err := decodeInput[event.RunOptionsPayload](inputs[1], event.RunOptionsSchemaVersion)
	if err != nil {
		return Options{}, err
	}
	return Options{DecideFrom: p.DecideFrom}, nil
}

// applyRunOptions reads the run options, which come once, straight after the
// configuration, and must be the options the run was built with.
func (h *Handler) applyRunOptions(in event.Envelope) error {
	if h.inputs != 1 {
		return fmt.Errorf("targetweight: run options %s come once, straight after the configuration", in.ID)
	}
	p, err := decodeInput[event.RunOptionsPayload](in, event.RunOptionsSchemaVersion)
	if err != nil {
		return err
	}
	if in.ConfigurationHash != h.hash {
		return fmt.Errorf("targetweight: run options %s belong to another configuration", in.ID)
	}
	if !p.DecideFrom.Equal(h.decideFrom) {
		return fmt.Errorf("targetweight: run options decide from %s, but the run was built to decide from %s", p.DecideFrom.Format(idTime), h.decideFrom.Format(idTime))
	}
	h.optioned = true
	h.prelude, h.preludeTotal = p.RecoveredRecords, p.RecoveredRecords
	h.restartID, h.restartAt = in.ID, in.EventTime
	return nil
}

// applyPrelude reads one recorded record of the restart prelude, and when it
// is the last, emits what the restart recovered.
func (h *Handler) applyPrelude(in event.Envelope) ([]event.Envelope, error) {
	if in.ConfigurationHash != h.hash {
		return nil, fmt.Errorf("targetweight: recorded %s %s belongs to another configuration", in.Type, in.ID)
	}
	var err error
	if h.momentum != nil {
		err = h.momentum.applyRecorded(in)
	} else {
		err = h.applyBoostRecorded(in)
	}
	if err != nil {
		return nil, err
	}
	h.prelude--
	if h.prelude > 0 {
		return nil, nil
	}
	p := event.RestartRecoveredPayload{
		AccountID: h.cfg.AccountID, StrategyID: h.cfg.StrategyID, RecoveredRecords: h.preludeTotal,
		EngineState: h.gate.State(), Scope: h.gate.Scope(),
	}
	if h.momentum != nil {
		h.momentum.recovered(&p)
	} else {
		p.LastDecisionID, p.QuietUntil, p.BoostLatched = h.lastRecovered, h.quietUntil, h.latched
		if h.inForceSet {
			p.BookDecisionID = h.lastRecovered
			p.Book = []event.TargetWeight{{InstrumentID: h.cfg.Boost.InstrumentID, Weight: h.inForce}}
		}
	}
	b := &emissions{h: h, in: in, at: h.restartAt}
	b.add(fmt.Sprintf("restart-recovered:%s:%s", h.cfg.AccountID, h.restartID), event.RestartRecoveredEventType, event.RestartRecoveredSchemaVersion, p)
	return b.result()
}

// applyBoostRecorded reads one of a Boost run's recorded records: its
// decisions give the latch and the target in force, the last one's session
// the end of the quiet sessions, and its engine states the gate's state.
// Boost tracks no order after its release, so planned, released and refused
// orders and an earlier restart's record carry nothing more to recover.
func (h *Handler) applyBoostRecorded(in event.Envelope) error {
	switch in.Type {
	case event.TargetsDecidedEventType:
		p, err := h.decodeRecovered(in)
		if err != nil {
			return err
		}
		h.recoverDecision(in.ID, p)
		return nil
	case event.EngineStateEventType:
		return h.restoreEngineState(in)
	case event.OrderPlannedEventType:
		_, err := decodeAccount(h, in, event.OrderPlannedSchemaVersion, func(p event.OrderPlannedPayload) string { return p.AccountID })
		return err
	case event.OrderReleasedEventType:
		_, err := decodeAccount(h, in, event.OrderReleasedSchemaVersion, func(p event.OrderReleasedPayload) string { return p.AccountID })
		return err
	case event.OrderRefusedEventType:
		_, err := decodeAccount(h, in, event.OrderRefusedSchemaVersion, func(p event.OrderRefusedPayload) string { return p.AccountID })
		return err
	case event.RestartRecoveredEventType:
		_, err := decodeAccount(h, in, event.RestartRecoveredSchemaVersion, func(p event.RestartRecoveredPayload) string { return p.AccountID })
		return err
	}
	return fmt.Errorf("targetweight: %s %s is not a record a Boost restart reads", in.Type, in.ID)
}

// decodeRecovered decodes a recorded Boost decision and checks it is this
// configuration's and account's.
func (h *Handler) decodeRecovered(in event.Envelope) (event.TargetsDecidedPayload, error) {
	p, err := decodeInput[event.TargetsDecidedPayload](in, event.TargetsDecidedSchemaVersion)
	if err != nil {
		return p, err
	}
	if in.ConfigurationHash != h.hash || p.AccountID != h.cfg.AccountID || p.Boost.InstrumentID != h.cfg.Boost.InstrumentID {
		return p, fmt.Errorf("targetweight: recovered decision %s belongs to another configuration or account", in.ID)
	}
	return p, nil
}

// recoverDecision takes the latch and the target in force from a recorded
// Boost decision, never recomputing them (template.py:89-100).
func (h *Handler) recoverDecision(id string, p event.TargetsDecidedPayload) {
	h.latched = p.Boost.LatchedAfter
	h.inForce, h.inForceSet = p.Boost.TargetInForce, p.Boost.TargetInForceSet
	if p.Outcome == event.TargetsOutcomeDecided {
		h.inForce, h.inForceSet = p.Targets[0].Weight, true
	}
	h.quietUntil = p.SessionPeriodEnd
	h.lastRecovered = id
}

// restoreEngineState reads back a recorded strategy.engine.state at either
// schema version, so the gate resumes in the state and scope it left.
func (h *Handler) restoreEngineState(in event.Envelope) error {
	if in.SchemaVersion != event.EngineStateSchemaVersion && in.SchemaVersion != event.EngineStateScopedVersion {
		return fmt.Errorf("targetweight: %s schema version %d is not one this build reads (ADR 0015)", in.Type, in.SchemaVersion)
	}
	var p event.EngineStatePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return fmt.Errorf("targetweight: decode %s %s: %w", in.Type, in.ID, err)
	}
	if err := h.gate.RestoreState(in.ID, p, in.SchemaVersion); err != nil {
		return fmt.Errorf("targetweight: %s %s: %w", in.Type, in.ID, err)
	}
	return nil
}

// decodeAccount decodes a recorded record and checks it is this account's.
func decodeAccount[T interface{ Validate() error }](h *Handler, in event.Envelope, version uint32, account func(T) string) (T, error) {
	p, err := decodeInput[T](in, version)
	if err != nil {
		return p, err
	}
	if account(p) != h.cfg.AccountID {
		return p, fmt.Errorf("targetweight: recorded %s %s is account %q's; this run decides %q", in.Type, in.ID, account(p), h.cfg.AccountID)
	}
	return p, nil
}
