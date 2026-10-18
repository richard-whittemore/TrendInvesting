package event_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestGapSessionsValidate: a momentum name's gap sessions are dates,
// ascending, on or before the close session; a gap name names at least one
// and a short history none (ADR 0026 point 5, amended 2026-10-18).
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
		"gap with none":   {with(event.MomentumStatusGap), "names its missing sessions"},
		"short with some": {with(event.MomentumStatusShortHistory, "2025-02-20"), "short history"},
	} {
		expectInvalid(t, name, c.v, func(*P) {}, c.want)
	}
}
