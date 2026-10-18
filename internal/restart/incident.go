package restart

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// CheckIncidentInForce refuses an approval that would loosen the engine
// state the approved log ends in: the incident its own approval carried in
// force, then its strategy.engine.state decisions, folded as the gate folds
// them, Halted overriding and Degraded scopes widening to
// their union (internal/risklimit). Clearing the incident by its ID is the
// owner's recovery step (ADR 0019); otherwise the incident in force must be
// that incident, under its ID, Halted, or Degraded over at least the same
// instruments and cash components. The incident's ID is the inherited incident's, or else the
// ID of the first strategy.engine.state record out of Normal
// (event.IncidentInForce); a log that ended Normal has none to clear and
// none to carry in force. logName names the approved log in a refusal.
func CheckIncidentInForce(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	incident, state, scope, cash, err := incidentAtEnd(entries, logName)
	if err != nil {
		return err
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
			return fmt.Errorf("engine: %s ended degraded on %s, but the approval's incident in force does not name %s: a restart never narrows a Degraded scope (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", logName, strings.Join(slices.Concat(scope, cash), ", "), id)
		}
	}
	for _, c := range cash {
		if !slices.Contains(in.ScopeCashComponents, c) {
			return fmt.Errorf("engine: %s ended degraded on %s, but the approval's incident in force does not name the cash component %s: a restart never narrows a Degraded scope (ADR 0019, amended 2026-09-24, \"The state survives a restart\")", logName, strings.Join(slices.Concat(scope, cash), ", "), c)
		}
	}
	return nil
}

// incidentAtEnd folds the incident the approved log ends in, as
// CheckIncidentInForce describes: its ID, its state and its scope of
// instruments and cash components, or nothing when the log ended Normal.
func incidentAtEnd(entries []journal.Entry, logName string) (incident, state string, scope, cash []string, err error) {
	widen := func(to, ids []string) []string {
		for _, id := range ids {
			if !slices.Contains(to, id) {
				to = append(to, id)
			}
		}
		return to
	}
	fold := func(id, next string, ids, cashIDs []string) {
		if incident == "" {
			incident = id
		}
		if next == event.EngineStateHalted || state == event.EngineStateHalted {
			state, scope, cash = event.EngineStateHalted, nil, nil
			return
		}
		state, scope, cash = event.EngineStateDegraded, widen(scope, ids), widen(cash, cashIDs)
	}
	for _, entry := range entries {
		env := entry.Envelope
		switch {
		case entry.Kind == journal.KindInput && env.Type == event.OperatorRunApprovedEventType:
			// The approved log's own approval: the incident it carried in
			// force was restored without a strategy.engine.state decision,
			// so it is the state the log starts in. It is read at the
			// schema version it was recorded at (ADR 0015).
			inherited, err := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
			if err != nil {
				return "", "", nil, nil, fmt.Errorf("engine: %s in %s: %w", env.ID, logName, err)
			}
			if in := inherited.IncidentInForce; in != nil {
				fold(in.IncidentID, in.State, in.ScopeInstrumentIDs, in.ScopeCashComponents)
			}
		case entry.Kind == journal.KindDecision && env.Type == event.EngineStateEventType:
			var p event.EngineStatePayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				return "", "", nil, nil, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			fold(env.ID, p.State, p.ScopeInstrumentIDs, p.ScopeCashComponents)
		}
	}
	return incident, state, scope, cash, nil
}
