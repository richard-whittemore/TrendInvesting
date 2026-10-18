package report_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
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

// TestAKillSwitchOrStaleRefusalAlertsOnlyWhenFlagged: a kill-switch
// engagement alerts once (ADR 0026, point 3, kill switch: "alert once") and a
// stale episode once (ADR 0019, amendment of 2026-10-17, reading 10): the
// gate flags the first refusal of each, and the later refusals it records
// without the flag send nothing, so a switch left on is not paged at every
// close.
func TestAKillSwitchOrStaleRefusalAlertsOnlyWhenFlagged(t *testing.T) {
	s := newStream(t)
	s.flaggedRefusal(0, "tw-a", event.RefusedKillSwitch, true)
	s.flaggedRefusal(1, "tw-b", event.RefusedKillSwitch, false)
	s.flaggedRefusal(2, "tw-c", event.RefusedStaleBrokerSnapshot, true)
	s.flaggedRefusal(3, "tw-d", event.RefusedStaleBrokerSnapshot, false)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "refusal:refuse:tw-a" || got[1].Key != "refusal:refuse:tw-c" {
		t.Fatalf("alerts = %+v, want the flagged refusals tw-a and tw-c only", got)
	}
	for _, a := range got {
		if a.Kind != report.KindRefusal || a.Severity != report.Urgent {
			t.Errorf("alert = %+v", a)
		}
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
	// ADR 0019, "Engine states": the named instruments are frozen, and only
	// instruments outside the scope keep their risk-reducing management.
	for _, want := range []string{"frozen", "outside the scope", "Protective Stop", "protection of the named positions"} {
		if !contains(got[0].Body, want) {
			t.Errorf("degraded body lacks %q: %q", want, got[0].Body)
		}
	}
	if contains(got[0].Body, "orders that reduce exposure continue.") {
		t.Errorf("degraded body says risk-reducing orders continue for the whole account: %q", got[0].Body)
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

// TestAnAllUnaffordableWithholdIsUrgent: a book D7-L found wholly
// unaffordable may rest on a bad net liquidation or bad raw closes, so the
// owner is paged now, with the detail naming the valuation, not left to the
// daily read.
func TestAnAllUnaffordableWithholdIsUrgent(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldAllUnaffordable)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != report.KindWithheld || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
	}
	if !contains(got[0].Body, "detail for all-unaffordable") {
		t.Errorf("body lacks the detail: %q", got[0].Body)
	}
}

// TestALabelsMissingWithholdIsUrgent: a month withheld because too many
// ranked members have no sector label points at a broken label source or
// mapping, which a person fixes before the next decision, so the owner is
// paged now, as for all-unaffordable.
func TestALabelsMissingWithholdIsUrgent(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldLabelsMissing)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != report.KindWithheld || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
	}
	if !contains(got[0].Body, "detail for labels-missing") {
		t.Errorf("body lacks the detail: %q", got[0].Body)
	}
}

// TestMissingHistoriesAreMissingData: a decision withheld because the
// compact stream sent no close history for the ranked members names absent
// inputs, as missing bars do.
func TestMissingHistoriesAreMissingData(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldHistoriesMissing)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != report.KindMissingData || got[0].Severity != report.Urgent {
		t.Fatalf("alerts = %+v", got)
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

func ny(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, nysecalendar.NewYork)
	if err != nil {
		panic(err)
	}
	return t
}

// Silence is measured in NYSE sessions, not hours: a weekend or a holiday
// with nothing written is not a loss (ADR 0019 leaves the timeout to the
// owner; the default is two sessions).
func TestHeartbeatLossIsCountedInSessionsAndAlertsOncePerSilence(t *testing.T) {
	friday := ny("2026-10-16 17:30")
	if _, lost, err := report.HeartbeatAlert("joint", friday, ny("2026-10-19 09:00"), 2); lost || err != nil {
		t.Fatalf("a weekend with nothing written is not a loss (lost %v, err %v)", lost, err)
	}
	thursday := ny("2026-04-02 18:00")
	if _, lost, err := report.HeartbeatAlert("joint", thursday, ny("2026-04-06 15:59"), 2); lost || err != nil {
		t.Fatalf("Good Friday and a weekend are not a loss (lost %v, err %v)", lost, err)
	}
	monday := ny("2026-10-12 18:00")
	if _, lost, _ := report.HeartbeatAlert("joint", monday, ny("2026-10-14 15:59"), 2); lost {
		t.Fatal("one session closed with nothing written is within two")
	}
	a, lost, err := report.HeartbeatAlert("joint", monday, ny("2026-10-14 16:30"), 2)
	if err != nil || !lost || a.Kind != report.KindHeartbeat || a.Severity != report.Urgent || !contains(a.Body, "2 NYSE sessions") {
		t.Fatalf("alert = %+v, lost %v, err %v", a, lost, err)
	}
	b, _, _ := report.HeartbeatAlert("joint", monday, ny("2026-10-20 16:30"), 2)
	if a.Key != b.Key {
		t.Fatal("one silence must be one incident however long it lasts")
	}
	c, _, _ := report.HeartbeatAlert("joint", monday.Add(time.Hour), ny("2026-10-20 16:30"), 2)
	if a.Key == c.Key {
		t.Fatal("a later silence is a new incident")
	}
	if _, _, err := report.HeartbeatAlert("joint", ny("2030-12-30 18:00"), ny("2031-01-06 18:00"), 2); err == nil {
		t.Fatal("a silence the calendar does not cover must be an error, never a guess")
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

func TestTheSameEngineStateAfterAnotherIsANewIncident(t *testing.T) {
	s := newStream(t)
	s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	s.engineState(1, event.EngineStateDegraded, event.EngineStateReasonRiskLimitBreached, "SPY")
	s.engineState(2, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Key == got[2].Key {
		t.Fatalf("a second halt is a second incident: %v", got)
	}
	fresh := report.Fresh(got, map[string]bool{got[0].Key: true})
	if len(fresh) != 2 || fresh[1].Kind != report.KindHalted {
		t.Fatalf("the second halt was dropped as already delivered: %v", fresh)
	}
}

func TestAHeartbeatTheReporterCannotCheckIsItselfAnUrgentAlertKeyedByTheProblem(t *testing.T) {
	at := ny("2026-10-17 12:00")
	a := report.HeartbeatUncheckedAlert("joint", errors.New("no calendar for 2020"), at)
	b := report.HeartbeatUncheckedAlert("joint", errors.New("no calendar for 2035"), at)
	if a.Kind != report.KindHeartbeatUnchecked || a.Severity != report.Urgent || a.AccountID != "joint" || !contains(a.Body, "cannot check the heartbeat: no calendar for 2020") {
		t.Fatalf("alert = %+v", a)
	}
	if a.Key == b.Key || a.Key != report.HeartbeatUncheckedAlert("joint", errors.New("no calendar for 2020"), at).Key {
		t.Fatal("one failure is one incident and a different failure is another")
	}
}
