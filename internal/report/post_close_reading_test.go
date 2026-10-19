package report_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// broker records the adapter's broker snapshot for session n: version 2 as
// of its close when reason is empty, else stamped at its read time with
// reason; version 1 at the close when v1 is set.
func (s *stream) broker(n int, reason string, v1 bool) {
	p := event.BrokerSnapshotPayload{AccountID: "joint", AsOf: day(n), NetLiquidation: 10000, ExcessLiquidity: 10000,
		Positions: []event.BrokerPosition{}, OpenOrders: []event.BrokerOrder{}}
	version := event.BrokerSnapshotReadAtVersion
	switch {
	case v1:
		version = event.BrokerSnapshotSchemaVersion
	case reason == "":
		p.ReadAt, p.AsOfClose = day(n).Add(70*time.Minute), true
	default:
		p.AsOf = day(n).Add(70 * time.Minute)
		p.ReadAt, p.NotAsOfClose = p.AsOf, reason
	}
	s.add(event.BrokerSnapshotEventType, version, fmt.Sprintf("broker:%d", n), p.AsOf, p)
}

// TestAStaleWithholdSaysWhyThePostCloseReadingWasNotTheClose: when the
// adapter's reading after the close could not stand for it (the owner's
// decision of 2026-10-18; ADR 0019, amended), the engine withholds the
// session as stale-account-snapshot, and that one page names the reason the
// broker snapshot states. A later withheld session of the same run is the
// same incident; a withhold with no such snapshot before it says nothing of
// one.
func TestAStaleWithholdSaysWhyThePostCloseReadingWasNotTheClose(t *testing.T) {
	const why = "IBKR shows 1 open order for the account (working, pending or cancel-pending) after the close"
	s := newStream(t)
	s.broker(0, why, false)
	s.withheld(0, event.WithheldStaleAccountSnapshot)
	s.broker(1, "", false)
	s.withheld(1, event.WithheldStaleAccountSnapshot)
	s.decided(2, 0.98)
	s.broker(3, "", true)
	s.withheld(3, event.WithheldStaleAccountSnapshot)
	s.broker(4, "", false)
	s.withheld(4, event.WithheldMissingBars)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != report.KindMissingData || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
	}
	for _, want := range []string{why, "broker:0", "could not stand for the close", "docs/runbooks/first-paper-order.md"} {
		if !contains(got[0].Body, want) {
			t.Errorf("the page lacks %q: %q", want, got[0].Body)
		}
	}
	for _, a := range got[1:] {
		if contains(a.Body, "could not stand for the close") {
			t.Errorf("a withhold with no such reading names one: %q", a.Body)
		}
	}
}

// TestABrokerSnapshotThatDoesNotReadRaisesNoAlert: a broker snapshot is an
// input the log keeps even when the engine refused it, so one that does not
// read is passed over, as a venue's report is.
func TestABrokerSnapshotThatDoesNotReadRaisesNoAlert(t *testing.T) {
	s := newStream(t)
	s.add(event.BrokerSnapshotEventType, event.BrokerSnapshotReadAtVersion, "broker:bad", day(0), event.BrokerSnapshotPayload{})
	s.withheld(0, event.WithheldStaleAccountSnapshot)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || contains(got[0].Body, "could not stand for the close") {
		t.Fatalf("alerts = %+v", got)
	}
}
