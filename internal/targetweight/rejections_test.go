package targetweight

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestAVerdictReadTwiceIsRecordedOnce: the venue's report of a rejection
// delivered again neither counts nor records the order a second time, as
// only the venue's first word on an order is read (ADR 0026, point 2,
// "Boost", amended 2026-10-18).
func TestAVerdictReadTwiceIsRecordedOnce(t *testing.T) {
	h := &Handler{verdicts: BoostVerdicts{rejections: 1, inARow: []event.BoostRejectedOrder{{ClientOrderID: "a"}}}}
	h.observeOutcome("a", true)
	if h.verdicts.rejections != 1 || len(h.verdicts.inARow) != 1 || len(h.stoppedRejections) != 0 || h.withheld != (event.BoostReplan{}) {
		t.Errorf("rejections %d named %v, stopped %v, withheld %v; want the repeat ignored",
			h.verdicts.rejections, h.verdicts.inARow, h.stoppedRejections, h.withheld)
	}
}
