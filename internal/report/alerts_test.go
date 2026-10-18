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

// TestAnUnflaggedRefusalAlertsUnlessTheJournalProvesTheGateFlags: a journal
// recorded before the gate flagged kill-switch or stale refusals carries no
// flag on any of them, and its refusals each alert, as they did before; an
// unflagged refusal is skipped only once an earlier one of the same reason
// in the journal carried the flag (ADR 0019, "Alerting": an alert is a
// safety requirement, so it is never silenced on a missing flag alone).
func TestAnUnflaggedRefusalAlertsUnlessTheJournalProvesTheGateFlags(t *testing.T) {
	s := newStream(t)
	s.flaggedRefusal(0, "tw-a", event.RefusedKillSwitch, false)
	s.flaggedRefusal(1, "tw-b", event.RefusedStaleBrokerSnapshot, false)
	s.flaggedRefusal(2, "tw-c", event.RefusedKillSwitch, true)
	s.flaggedRefusal(3, "tw-d", event.RefusedKillSwitch, false)
	s.flaggedRefusal(4, "tw-e", event.RefusedStaleBrokerSnapshot, false)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, a := range got {
		keys = append(keys, a.Key)
	}
	want := []string{"refusal:refuse:tw-a", "refusal:refuse:tw-b", "refusal:refuse:tw-c", "refusal:refuse:tw-e"}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Fatalf("alert keys = %v, want %v", keys, want)
	}
}

// TestAStaleEpisodeOpenedByAWithheldSaleAlertsOnce: the gate's stale episode
// may open on a withheld sale back to 98% rather than on a refusal; that
// withhold carries the episode's one urgent alert (ADR 0019, amendment of
// 2026-10-17, reading 10, "Stale episode"), and the unflagged refusals of the
// same episode send nothing more.
func TestAStaleEpisodeOpenedByAWithheldSaleAlertsOnce(t *testing.T) {
	s := newStream(t)
	s.flaggedRefusal(0, "tw-a", event.RefusedStaleBrokerSnapshot, true)
	s.decided(1, 0.98)
	s.staleSaleWithheld(2, "sell-a", true)
	s.flaggedRefusal(3, "tw-b", event.RefusedStaleBrokerSnapshot, false)
	s.staleSaleWithheld(4, "sell-b", false)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "refusal:refuse:tw-a" {
		t.Fatalf("alerts = %+v, want the first episode's refusal and the second episode's withheld sale", got)
	}
	a := got[1]
	if a.Key != "missing-data:sale-withheld:sale-withheld:sell-a" || a.Kind != report.KindMissingData || a.Severity != report.Urgent || a.AccountID != "joint" {
		t.Fatalf("withheld-sale alert = %+v", a)
	}
	if !contains(a.Body, "sell-a") || !contains(a.Body, event.SaleWithheldStaleBrokerSnapshot) || !contains(a.Body, "snapshot too old") {
		t.Fatalf("body = %q", a.Body)
	}
}

// TestAlertsRefuseAWithheldSaleTheyCannotRead: a withheld sale that does not
// validate, or is at a version this build does not read, refuses the
// journal rather than drop an alert it might carry.
func TestAlertsRefuseAWithheldSaleTheyCannotRead(t *testing.T) {
	for name, mutate := range map[string]func(s *stream){
		"invalid":         func(s *stream) { s.rewrite(1, event.SaleWithheldPayload{AccountID: "joint"}) },
		"unknown version": func(s *stream) { s.out[1].SchemaVersion = 9 },
	} {
		s := newStream(t)
		s.staleSaleWithheld(0, "sell-a", true)
		mutate(s)
		if _, err := report.Alerts(s.out); err == nil {
			t.Errorf("%s: Alerts accepted it", name)
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

// TestABoostGapPagesOnceAndSaysDecisionsContinue: a Boost session with no
// SPY bar is skipped and the next bar decides again (ADR 0026, point 5,
// amended 2026-10-18), so one urgent page covers the whole gap, names its
// first missing session and says decisions continue over the closes
// received; a later gap is another page.
func TestABoostGapPagesOnceAndSaysDecisionsContinue(t *testing.T) {
	s := newStream(t)
	s.withheld(0, event.WithheldMissingBars)
	s.withheld(1, event.WithheldMissingBars)
	s.decided(2, 0.98)
	s.withheld(3, event.WithheldMissingBars)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kinds = %v, want one page per gap", kinds(got))
	}
	a := got[0]
	if a.Kind != report.KindMissingData || a.Severity != report.Urgent {
		t.Errorf("gap page: %s %v", a.Kind, a.Severity)
	}
	for _, want := range []string{day(0).Format(time.DateOnly), "skipped", "decides again at the next session with a bar", "not filled in"} {
		if !contains(a.Body, want) {
			t.Errorf("gap page lacks %q: %q", want, a.Body)
		}
	}
}

// TestAWithheldRejectionReplanPagesOnce: a Boost decision that records a
// re-plan withheld after the venue rejected the re-planned order again
// (ADR 0026, point 2, "Boost", amended 2026-10-18) is one urgent page naming
// the order and its shares and saying the program sends no more buys until
// the owner approves a fresh run, while its sells still go; the decisions
// after it page nothing more.
func TestAWithheldRejectionReplanPagesOnce(t *testing.T) {
	s := newStream(t)
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0),
		event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(0), SessionPeriodEnd: day(0),
			Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
			Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100,
				ReplanWithheld: event.BoostReplan{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-7", Released: 37}}})
	s.decided(1, 0.98)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("kinds = %v, want one page", kinds(got))
	}
	a := got[0]
	if a.Kind != report.KindRefusal || a.Severity != report.Urgent {
		t.Errorf("page: %s %v", a.Kind, a.Severity)
	}
	// Two orders rejected in a row stop Boost's buys until the owner
	// approves a fresh run, whatever the target does, while its sells, the
	// exit and the margin alarm's sale back to 98%, still go (ADR 0026,
	// points 2, "Boost", amended 2026-10-18, and 4), and the page says so,
	// so the owner never sells by hand into a program sale.
	for _, want := range append([]string{"tw-7", "37", "rejected", "two orders in a row", "sends no more buys", "exit", "sale back to 98%", "still go",
		"operator approve", "-clear-incident", "fresh run"}, handSale...) {
		if !contains(a.Body, want) {
			t.Errorf("page lacks %q: %q", want, a.Body)
		}
	}
}

// handSale is the hand-sale procedure a page sends the owner to while the
// program's sells may still be working (docs/manual/states-and-kill-switch.md):
// stop new orders first, the kill switch or the engine, cancel the working
// sale in IBKR's app, confirm none remains, and only then sell.
var handSale = []string{"kill switch", "stop the engine", "cancel", "IBKR's app", "confirm", "only then"}

// TestARejectionWhileBuysAreStoppedPagesAsSuch: a sell the venue rejects
// after two rejections in a row stopped Boost's buys is recorded once, with
// the count of rejections since the stop (ADR 0026, points 2, "Boost",
// amended 2026-10-18, and 4), and paged once per order as another rejection
// while buys are stopped, saying which one it is, not as the stop.
func TestARejectionWhileBuysAreStoppedPagesAsSuch(t *testing.T) {
	s := newStream(t)
	s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0),
		event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(0), SessionPeriodEnd: day(0),
			Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
			Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100, VenueRejections: 2, RejectionsWhileStopped: 3,
				RejectionRun: []event.BoostRunOrder{{ClientOrderID: "tw-1", Verdict: event.RunVerdictRejected}, {ClientOrderID: "tw-2", Verdict: event.RunVerdictRejected}},
				RejectedWhileStopped: []event.BoostReplan{
					{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-8", Released: 6},
					{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-9", Released: 5},
				}}})
	s.decided(1, 0.98)
	got, err := report.Alerts(s.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("kinds = %v, want one page per rejected order", kinds(got))
	}
	for i, c := range []struct{ id, count string }{{"tw-8", "2nd"}, {"tw-9", "3rd"}} {
		a := got[i]
		if a.Kind != report.KindRefusal || a.Severity != report.Urgent || contains(a.Title, "two orders in a row") {
			t.Errorf("page %d: %s %v %q", i, a.Kind, a.Severity, a.Title)
		}
		for _, want := range append([]string{c.id, "another rejection while Boost's buys are stopped", c.count + " since the stop", "planned again", "fresh run"}, handSale...) {
			if !contains(a.Title+" "+a.Body, want) {
				t.Errorf("page %d lacks %q: %q %q", i, want, a.Title, a.Body)
			}
		}
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

// ack adds an acknowledgement record to the stream as the journal holds it.
func (s *stream) ack(id string, version uint32, p event.AlertAcknowledgedPayload) {
	s.add(event.AlertAcknowledgedEventType, version, id, day(1), p)
}

// TestOnlyAnAcknowledgementTheEngineDeliveredStopsTheRepeats: the journal
// keeps an acknowledgement for another account next to the delivered ones; it
// is not counted and the alert keeps repeating. Acknowledgements the engine
// refuses on time, validity or readability are never journalled (ADR 0019,
// "Alerting"; owner decision 2026-10-18).
func TestOnlyAnAcknowledgementTheEngineDeliveredStopsTheRepeats(t *testing.T) {
	good := func(key string) event.AlertAcknowledgedPayload {
		return event.AlertAcknowledgedPayload{AccountID: "joint", AlertKey: key, AcknowledgedBy: "owner", AcknowledgedAt: day(1)}
	}
	t.Run("another account is not counted", func(t *testing.T) {
		s := newStream(t)
		s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
		alerts, err := report.Alerts(s.out)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("alerts = %v, %v", alerts, err)
		}
		p := good(alerts[0].Key)
		p.AccountID = "ira"
		s.ack("ack", event.AlertAcknowledgedSchemaVersion, p)
		got, err := report.Alerts(s.out)
		if err != nil {
			t.Fatalf("a readable acknowledgement for another account is no error: %v", err)
		}
		if len(got) != 1 || got[0].Acknowledged {
			t.Fatalf("it must not stop the repeats: %+v", got)
		}
	})
	unreadable := map[string]func(key string, s *stream){
		"a payload that fails validation": func(key string, s *stream) {
			p := good(key)
			p.AcknowledgedBy = ""
			s.ack("ack", event.AlertAcknowledgedSchemaVersion, p)
		},
		"a payload that does not parse": func(key string, s *stream) {
			s.ack("ack", event.AlertAcknowledgedSchemaVersion, good(key))
			s.out[len(s.out)-1].Payload = []byte(`{`)
		},
		"a schema version this build does not read": func(key string, s *stream) {
			s.ack("ack", 9, good(key))
		},
	}
	for name, record := range unreadable {
		t.Run("unreadable acknowledgement errors the read: "+name, func(t *testing.T) {
			s := newStream(t)
			s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
			alerts, err := report.Alerts(s.out)
			if err != nil || len(alerts) != 1 {
				t.Fatalf("alerts = %v, %v", alerts, err)
			}
			record(alerts[0].Key, s)
			if _, err := report.Alerts(s.out); err == nil {
				t.Fatal("an acknowledgement the engine could not have journalled means a build mismatch: want a read error")
			}
		})
	}
	t.Run("delivered", func(t *testing.T) {
		s := newStream(t)
		s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
		alerts, _ := report.Alerts(s.out)
		s.ack("ack", event.AlertAcknowledgedSchemaVersion, good(alerts[0].Key))
		got, err := report.Alerts(s.out)
		if err != nil || len(got) != 1 || !got[0].Acknowledged {
			t.Fatalf("a delivered acknowledgement stops the repeats: %+v, %v", got, err)
		}
	})
	t.Run("a refused one beside a delivered one", func(t *testing.T) {
		s := newStream(t)
		s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
		alerts, _ := report.Alerts(s.out)
		p := good(alerts[0].Key)
		p.AccountID = "ira"
		s.ack("ack:refused", event.AlertAcknowledgedSchemaVersion, p)
		s.ack("ack:delivered", event.AlertAcknowledgedSchemaVersion, good(alerts[0].Key))
		got, _ := report.Alerts(s.out)
		if !got[0].Acknowledged {
			t.Fatal("the delivered acknowledgement still counts")
		}
	})
}

// TestARejectionWhileBuysAreStoppedNamesItsPlace: the page names the
// rejection's place since the stop in plain English.
func TestARejectionWhileBuysAreStoppedNamesItsPlace(t *testing.T) {
	for count, want := range map[int]string{1: "1st", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd", 23: "23rd"} {
		s := newStream(t)
		s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets:0", day(0),
			event.TargetsDecidedPayload{AccountID: "joint", StrategyID: event.TargetWeightStrategyBoost, DecisionTime: day(0), SessionPeriodEnd: day(0),
				Outcome: event.TargetsOutcomeDecided, Targets: []event.TargetWeight{{InstrumentID: "SPY", Weight: 0.98}},
				Boost: event.BoostFigures{InstrumentID: "SPY", Close: 100, VenueRejections: 2, RejectionsWhileStopped: count,
					RejectionRun:         []event.BoostRunOrder{{ClientOrderID: "tw-1", Verdict: event.RunVerdictRejected}, {ClientOrderID: "tw-2", Verdict: event.RunVerdictRejected}},
					RejectedWhileStopped: []event.BoostReplan{{Cause: event.ReplanCauseRejected, ClientOrderID: "tw-8", Released: 6}}}})
		got, err := report.Alerts(s.out)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !contains(got[0].Body, "the "+want+" since the stop") {
			t.Errorf("count %d: pages %+v, want the %s", count, got, want)
		}
	}
}
