package event_test

import (
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestGapSessionsValidate: a momentum name's gap sessions are dates,
// ascending, on or before the close session; a short history names none,
// and a gap name saved before they were recorded names none (ADR 0026 point 5, amended 2026-10-18).
func TestGapSessionsValidate(t *testing.T) {
	type P = event.TargetsDecidedPayload
	with := func(status string, gaps ...string) P {
		v := validMomentumDecided()
		v.Momentum.Names = append([]event.MomentumName(nil), v.Momentum.Names...)
		v.Momentum.Names[1].Status, v.Momentum.Names[1].GapSessions = status, gaps
		return v
	}
	for name, v := range map[string]P{
		"gap":                    with(event.MomentumStatusGap, "2025-02-20"),
		"held, scored across it": with(event.MomentumStatusScored, "2025-02-20", "2025-03-04"),
		// A gap name the writer saved before it recorded gap sessions: the
		// log it is in must still be read (reports and replay).
		"gap saved before gap sessions": with(event.MomentumStatusGap),
	} {
		if err := v.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		v    P
		want string
	}{
		"not a date":      {with(event.MomentumStatusGap, "20 Feb"), "gap session"},
		"out of order":    {with(event.MomentumStatusGap, "2025-02-21", "2025-02-20"), "ascending"},
		"repeated":        {with(event.MomentumStatusGap, "2025-02-20", "2025-02-20"), "ascending"},
		"after the close": {with(event.MomentumStatusGap, "2025-03-05"), "after the close session"},
		"short with some": {with(event.MomentumStatusShortHistory, "2025-02-20"), "short history"},
	} {
		expectInvalid(t, name, c.v, func(*P) {}, c.want)
	}
}

// TestAGapNameWrittenNowNamesItsMissingSessions: a gap name written under
// the current rules names the sessions it is missing (ADR 0026 point 5,
// amended 2026-10-18), though a record written before gap sessions existed
// is still read. ValidateWritten is the writer's check; Validate stays the
// reader's.
func TestAGapNameWrittenNowNamesItsMissingSessions(t *testing.T) {
	v := validMomentumDecided()
	v.Momentum.Names = append([]event.MomentumName(nil), v.Momentum.Names...)
	v.Momentum.Names[1].Status, v.Momentum.Names[1].GapSessions = event.MomentumStatusGap, nil
	if err := v.Validate(); err != nil {
		t.Fatalf("a reader refuses an old gap name: %v", err)
	}
	if err := v.ValidateWritten(); err == nil || !strings.Contains(err.Error(), "names its missing sessions") {
		t.Errorf("the writer stamps a gap name with no sessions: %v", err)
	}
	v.Momentum.Names[1].GapSessions = []string{"2025-02-20"}
	if err := v.ValidateWritten(); err != nil {
		t.Errorf("a gap name with its sessions is refused: %v", err)
	}
}
