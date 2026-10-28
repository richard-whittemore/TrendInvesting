package restart

import (
	"encoding/json"
	"fmt"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// CheckApprovedLatch refuses an approval whose latch is not the one the
// approved log ends with: its last strategy.targets.decided's latched_after,
// or, when that run decided nothing, the latch its own approval stated (ADR
// 0026, point 5: the latch is restored, never recomputed). A log with
// neither leaves the approval's latch as the fresh run's start. The approved
// log's approval is read at the schema version it was recorded at (ADR 0015).
// logName names the approved log in a refusal.
func CheckApprovedLatch(entries []journal.Entry, approval event.OperatorRunApprovedPayload, logName string) error {
	if approval.Boost.Latched == nil {
		return fmt.Errorf("engine: %s is a Boost run's, but the approval states an IRA's momentum state and no latch; the latch in force is restored, never assumed (ADR 0026, point 5)", logName)
	}
	stated := *approval.Boost.Latched
	latched, from, found, err := latchAtEnd(entries, logName)
	switch {
	case err != nil || !found || latched == stated:
		return err
	case from.Type == event.TargetsDecidedEventType:
		return fmt.Errorf("engine: %s's last decision, %s, has latched_after %t, but the approval states latched %t; the latch in force is restored, never changed by a restart (ADR 0026, point 5)",
			logName, from.ID, latched, stated)
	}
	return fmt.Errorf("engine: %s decided nothing, and the approval it started from, %s, states latched %t; this approval states latched %t, but the latch in force is restored, never changed by a restart (ADR 0026, point 5)",
		logName, from.ID, latched, stated)
}

// latchAtEnd returns the latch the approved log ends with, as
// CheckApprovedLatch describes, and the record that states it; found is
// false for a log with neither a decision nor an approval.
func latchAtEnd(entries []journal.Entry, logName string) (latched bool, from event.Envelope, found bool, err error) {
	for i := len(entries) - 1; i >= 0; i-- {
		env := entries[i].Envelope
		switch env.Type {
		case event.TargetsDecidedEventType:
			var decided event.TargetsDecidedPayload
			if err := json.Unmarshal(env.Payload, &decided); err != nil {
				return false, env, false, fmt.Errorf("engine: decode %s in %s: %w", env.ID, logName, err)
			}
			return decided.Boost.LatchedAfter, env, true, nil
		case event.OperatorRunApprovedEventType:
			previous, err := event.DecodeRecordedOperatorRunApproved(env.Payload, env.SchemaVersion)
			if err != nil {
				return false, env, false, fmt.Errorf("engine: %s in %s: %w", env.ID, logName, err)
			}
			if previous.Boost.Latched == nil {
				// An IRA approval states no latch; it starts none.
				return false, env, false, nil
			}
			return *previous.Boost.Latched, env, true, nil
		}
	}
	return false, event.Envelope{}, false, nil
}
