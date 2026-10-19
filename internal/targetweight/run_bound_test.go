package targetweight

import (
	"slices"
	"strconv"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestADryRunsUndecidedOrdersAreBoundedAndTheCapIsRecorded: a dry-run sends
// nothing, so no verdict ever reaches the run of rejections and every order
// released would stay in it for the run's life. The run keeps the newest
// BoostRunLimit and counts the rest as dropped (ADR 0026, point 5).
func TestADryRunsUndecidedOrdersAreBoundedAndTheCapIsRecorded(t *testing.T) {
	h := &Handler{}
	const released = 50
	for i := range released {
		h.run = append(h.run, runOrder{id: "tw-" + strconv.Itoa(i)})
		h.trimRun()
	}
	if len(h.run) != event.BoostRunLimit || h.run[len(h.run)-1].id != "tw-49" {
		t.Fatalf("run of %d ending %v, want the newest %d", len(h.run), h.run[len(h.run)-1], event.BoostRunLimit)
	}
	if h.runDropped != released-event.BoostRunLimit {
		t.Errorf("dropped %d, want %d", h.runDropped, released-event.BoostRunLimit)
	}
}

// TestTheRunKeepsOnlyWhatTheCountAndTheStopNeed: before the stop the run
// starts after the latest accepted order; at the stop it keeps the newest
// rejections that made it and the undecided orders, nothing the venue has
// ruled on otherwise (ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestTheRunKeepsOnlyWhatTheCountAndTheStopNeed(t *testing.T) {
	ids := func(run []runOrder) (out []string) {
		for _, r := range run {
			out = append(out, r.id)
		}
		return out
	}
	rej, acc, und := func(id string) runOrder { return runOrder{id: id, rejected: true} },
		func(id string) runOrder { return runOrder{id: id, accepted: true} }, func(id string) runOrder { return runOrder{id: id} }
	for name, c := range map[string]struct {
		rejections int
		run        []runOrder
		want       []string
	}{
		"an acceptance ends the run":        {0, []runOrder{rej("a"), acc("b"), und("c"), rej("d")}, []string{"c", "d"}},
		"before the stop nothing else goes": {1, []runOrder{rej("a"), und("b"), rej("c")}, []string{"a", "b", "c"}},
		"the stop keeps the newest rejections and the undecided": {2, []runOrder{rej("a"), rej("b"), und("c"), acc("d"), rej("e"), rej("f"), und("g")},
			[]string{"c", "e", "f", "g"}},
		"the stop caps the undecided, the oldest dropped": {2, []runOrder{rej("a"), und("c1"), und("c2"), und("c3"), und("c4"), und("c5"), und("c6"), und("c7"), rej("b")},
			[]string{"a", "c2", "c3", "c4", "c5", "c6", "c7", "b"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{rejections: c.rejections, run: c.run}
			h.trimRun()
			got := ids(h.run)
			if !slices.Equal(got, c.want) {
				t.Fatalf("run %v, want %v", got, c.want)
			}
		})
	}
}

// TestAVerdictReadTwiceIsRecordedOnce: the venue's report of a rejection
// delivered again neither counts nor records the order a second time
// (ADR 0026, point 2, "Boost", amended 2026-10-18).
func TestAVerdictReadTwiceIsRecordedOnce(t *testing.T) {
	h := &Handler{rejections: 1, run: []runOrder{{id: "a", rejected: true}}}
	h.observeOutcome("a", true)
	if h.rejections != 1 || len(h.stoppedRejections) != 0 || h.withheld != (event.BoostReplan{}) {
		t.Errorf("rejections %d, stopped %v, withheld %v; want the repeat ignored", h.rejections, h.stoppedRejections, h.withheld)
	}
}
