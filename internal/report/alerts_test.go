package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func kinds(alerts []report.Alert) []report.Kind {
	out := make([]report.Kind, len(alerts))
	for i, a := range alerts {
		out[i] = a.Kind
	}
	return out
}

func TestEveryRefusalAlertsOnceUrgently(t *testing.T) {
	s := newStream(t)
	s.refused(0, "tw-a", event.RefusedMarginExposure)
	s.refused(1, "tw-b", event.RefusedPriceBand)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != report.KindRefusal || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
	}
	if got[0].Key == got[1].Key {
		t.Fatal("two refusals share a key")
	}
	if !contains(got[0].Body, event.RefusedMarginExposure) || got[0].AccountID != "joint" {
		t.Fatalf("body = %q", got[0].Body)
	}
}

func TestADegradedOrHaltedStateAlertsOncePerIncident(t *testing.T) {
	s := newStream(t)
	s.engineState(0, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY")
	s.engineState(1, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY")
	s.engineState(2, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	s.engineState(3, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if want := []report.Kind{report.KindDegraded, report.KindHalted}; len(got) != 2 || got[0].Kind != want[0] || got[1].Kind != want[1] {
		t.Fatalf("kinds = %v", kinds(got))
	}
	for _, a := range got {
		if a.Severity != report.Urgent || !contains(a.Body, "new") {
			t.Fatalf("alert = %+v", a)
		}
	}
	if !contains(got[0].Body, "SPY") {
		t.Fatalf("degraded body lacks its scope: %q", got[0].Body)
	}
}

func TestAWithheldRunIsOneIncidentAndADecisionEndsIt(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldMissingBars)
	s.withheld(1, event.WithheldMissingBars)
	s.decided(2, 0.98)
	s.withheld(3, event.WithheldMissingBars)
	s.withheld(4, event.WithheldNoEligibleNames)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("kinds = %v", kinds(got))
	}
	if got[0].Kind != report.KindMissingData || got[1].Kind != report.KindMissingData || got[2].Kind != report.KindWithheld {
		t.Fatalf("kinds = %v", kinds(got))
	}
	if got[0].Key == got[1].Key {
		t.Fatal("a run restarted after a decision shares the earlier incident's key")
	}
	if got[2].Severity != report.Routine || got[0].Severity != report.Urgent {
		t.Fatalf("severities = %v %v", got[0].Severity, got[2].Severity)
	}
}

func TestAStaleSnapshotAndAMissingMarkAreMissingData(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldStaleAccountSnapshot)
	s.refused(1, "tw-a", event.RefusedMissingMark)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Kind{report.KindMissingData, report.KindRefusal, report.KindMissingData}
	if len(got) != 3 {
		t.Fatalf("kinds = %v", kinds(got))
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Fatalf("kinds = %v, want %v", kinds(got), want)
		}
	}
}

func TestAlertsAreStableAcrossRepeatedReads(t *testing.T) {
	s := newStream(t)
	s.refused(0, "tw-a", event.RefusedOrderSize)
	s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	first, _ := report.Alerts(s.out)
	second, _ := report.Alerts(s.out)
	if len(first) != len(second) {
		t.Fatal("alerts differ between reads")
	}
	for i := range first {
		if first[i].Key != second[i].Key {
			t.Fatal("keys differ between reads")
		}
	}
}

func TestFreshDropsSeenKeysAndDuplicates(t *testing.T) {
	alerts := []report.Alert{{Key: "a"}, {Key: "b"}, {Key: "a"}, {Key: "c"}}
	got := report.Fresh(alerts, map[string]bool{"b": true})
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "c" {
		t.Fatalf("fresh = %+v", got)
	}
}

func TestHeartbeatLossAlertsOncePerSilence(t *testing.T) {
	last := day(0)
	if _, lost := report.HeartbeatAlert("joint", last, last.Add(time.Hour), 2*time.Hour); lost {
		t.Fatal("within the timeout is not a loss")
	}
	if _, lost := report.HeartbeatAlert("joint", last, last.Add(2*time.Hour), 2*time.Hour); lost {
		t.Fatal("exactly at the timeout is not yet a loss")
	}
	a, lost := report.HeartbeatAlert("joint", last, last.Add(3*time.Hour), 2*time.Hour)
	if !lost || a.Kind != report.KindHeartbeat || a.Severity != report.Urgent {
		t.Fatalf("alert = %+v, lost %v", a, lost)
	}
	b, _ := report.HeartbeatAlert("joint", last, last.Add(9*time.Hour), 2*time.Hour)
	if a.Key != b.Key {
		t.Fatal("one silence must be one incident however long it lasts")
	}
	c, _ := report.HeartbeatAlert("joint", last.Add(time.Hour), last.Add(9*time.Hour), 2*time.Hour)
	if a.Key == c.Key {
		t.Fatal("a later silence is a new incident")
	}
}

func TestAlertsFailOnAMalformedPayload(t *testing.T) {
	s := newStream(t)
	s.refused(0, "tw-a", event.RefusedOrderSize)
	s.out[1].Payload = []byte(`{`)
	if _, err := report.Alerts(s.out); err == nil {
		t.Fatal("expected a decode error")
	}
}
