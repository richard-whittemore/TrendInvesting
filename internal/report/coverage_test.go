package report_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

func (s *stream) fill(n int) {
	s.add(event.FillEventType, event.FillSchemaVersion, "fill:1", day(n),
		event.FillPayload{InstrumentID: "SPY", Kind: event.FillKindEntry, ProposalID: "p", FillID: "f", Direction: "long", Quantity: 3, Price: 100, FilledAt: day(n), Level: 100})
}

// rich records one of every event the summary reads, on day 0.
func rich(t *testing.T) *stream {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 3)
	s.decided(0, 0.98)
	s.planned(0, "tw-a", 3)
	s.released(0, "tw-a", 3)
	s.refused(0, "tw-b", event.RefusedOrderSize)
	s.engineState(0, event.EngineStateHalted, event.EngineStateReasonRiskLimitBreached)
	s.fill(0)
	return s
}

func TestEveryPayloadThatCannotBeDecodedFailsTheReport(t *testing.T) {
	base := rich(t)
	for i, e := range base.out {
		if i == 0 {
			continue
		}
		s := rich(t)
		s.out[i].Payload = []byte(`{`)
		if _, err := report.Summarize(s.out, "2024-01-02", "SPY"); err == nil {
			t.Errorf("Summarize accepted a corrupt %s", e.Type)
		}
	}
	for _, i := range []int{3, 6, 7} { // decided, refused, engine state
		s := rich(t)
		s.out[i].Payload = []byte(`{`)
		if _, err := report.Alerts(s.out); err == nil {
			t.Errorf("Alerts accepted a corrupt %s", s.out[i].Type)
		}
	}
}

func TestAlertsNeedAConfiguration(t *testing.T) {
	if _, err := report.Alerts(nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSummaryOfALaterDayShowsNoEarlierDaysOrdersDecisionsOrFills(t *testing.T) {
	s := rich(t)
	s.bar(1, "SPY", 101)
	s.snapshot(1, 4100, 3)
	got, err := report.Summarize(s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders)+len(got.Decisions)+len(got.Fills) != 0 || got.Refusals != 0 || got.NetLiquidation != 4100 {
		t.Fatalf("got %+v", got)
	}
	if got.EngineState != report.StateHalted {
		t.Fatalf("a halt persists until recovery: %q", got.EngineState)
	}
}

func TestSummaryListsFillsAndIgnoresTheOtherSnapshotVersion(t *testing.T) {
	s := rich(t)
	s.add(event.AccountSnapshotEventType, event.AccountSnapshotSchemaVersion, "snapshot:v2", day(0), map[string]any{"unused": true})
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Fills) != 1 || got.Fills[0].Quantity != 3 || got.NetLiquidation != 4000 {
		t.Fatalf("got %+v", got)
	}
}

func TestAReleaseOrRefusalWithoutAPlanStillShowsAsAnOrder(t *testing.T) {
	s := newStream(t)
	s.refused(0, "tw-x", event.RefusedUndecidedOrder)
	s.released(0, "tw-y", 5)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 2 || got.Orders[0].Status != report.OrderRefused || got.Orders[1].Status != report.OrderReleased {
		t.Fatalf("orders = %+v", got.Orders)
	}
}

func TestRenderTextCoversEveryEmptyAndWithheldCase(t *testing.T) {
	empty := report.RenderText(report.Summary{AccountID: "joint", Date: "2024-01-02", Benchmark: "SPY", EngineState: report.StateNormal})
	for _, want := range []string{"No account snapshot", "Holdings: none", "no decision today", "Orders:\n  none", "Fills: none", "not enough paired"} {
		if !strings.Contains(empty, want) {
			t.Errorf("empty summary lacks %q:\n%s", want, empty)
		}
	}
	s := newStream(t)
	s.withheld(0, event.WithheldMissingBars)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if text := report.RenderText(got); !strings.Contains(text, "withheld (missing-bars)") {
		t.Errorf("text:\n%s", text)
	}
	full := rich(t)
	got, err = report.Summarize(full.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if text := report.RenderText(got); !strings.Contains(text, "SPY 3 @ 100.00") || !strings.Contains(text, "SPY 98%") {
		t.Errorf("text:\n%s", text)
	}
}

func TestLoadRefusesAJournalWhoseHeaderIdentityDiffersFromItsRecords(t *testing.T) {
	envelopes := buildDay(t)
	var entries []journal.Entry
	for _, e := range envelopes {
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: e})
	}
	header := journal.NewHeader("another-configuration", "v", envelopes[0].EventTime, day(1))
	var buf bytes.Buffer
	if err := journal.Write(&buf, header, entries); err != nil {
		t.Fatal(err)
	}
	if _, _, err := report.Load(buf.Bytes()); err == nil {
		t.Fatal("expected an identity error")
	}
}

func TestDecodeFailureInTheConfigurationFailsAccountOf(t *testing.T) {
	s := newStream(t)
	s.out[0].Payload = []byte(`{`)
	if _, _, err := report.AccountOf(s.out); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAccountOfSkipsEventsBeforeTheConfiguration(t *testing.T) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	reordered := append([]event.Envelope{s.out[1]}, s.out[0])
	account, strategy, err := report.AccountOf(reordered)
	if err != nil || account != "joint" || strategy != event.TargetWeightStrategyBoost {
		t.Fatalf("got %q %q %v", account, strategy, err)
	}
}
