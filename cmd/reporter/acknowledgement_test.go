package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// paperEntries is a synthetic paper Boost run that releases one order to be
// sent, followed by acknowledged's venue report when it is set.
func paperEntries(t *testing.T, acknowledged bool) []journal.Entry {
	t.Helper()
	cfg := event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost, AccountID: "DU1234567", Mode: event.TradingModePaper, Adapter: event.AdapterIBKR, Boost: event.BoostConfiguration{InstrumentID: "SPY", InvestedFraction: 0.98, BoostSize: 1.5, RSIPeriod: 2, RSIEntry: 10, TrendDays: 200, ExitSMA: 5, WarmUpStart: "2024-01-02"},
		Limits:    event.MarginLimits{MaxExposure: 1.5, MaxOrdersPerDecision: 1, MaxOrderFraction: 1, PriceBand: 0.05, ExcessLiquidityWarning: 0.4, ExcessLiquidityUrgent: 0.2, ExcessLiquidityStaleMinutes: 30},
		Execution: event.ExecutionConfiguration{MaxSnapshotAge: 30}}
	released := event.OrderReleasedPayload{AccountID: "DU1234567", PlanID: "p", Tag: "targets:0", ClientOrderID: "tw-a", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 9, LimitPrice: 101, Mode: event.TradingModePaper, Send: true,
		Figures: event.RiskFigures{NetLiquidation: 4000, ReferencePrice: 100, ExposureBefore: 0, ExposureAfter: 0.23, OrderFraction: 0.23, OrdersInDecision: 1}}
	closed := event.SessionClosedPayload{PeriodEnd: t0, InstrumentIDs: []string{"SPY"}}
	entries := []journal.Entry{
		{Kind: journal.KindInput, Envelope: envelope(t, 1, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "cfg", t0.Add(-time.Hour), cfg)},
		{Kind: journal.KindInput, Envelope: envelope(t, 2, event.SessionClosedEventType, event.SessionClosedSchemaVersion, "session", t0, closed)},
		{Kind: journal.KindDecision, Envelope: envelope(t, 3, event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, "release", t0, released)},
	}
	if acknowledged {
		at := t0.Add(time.Minute)
		ack := event.LimitOrderLifecyclePayload{AccountID: "DU1234567", ClientOrderID: "tw-a", OrderID: "7", InstrumentID: "SPY", Side: event.OrderSideBuy,
			Status: event.OrderLifecycleStatusSubmitted, Quantity: 9, LimitPrice: 101, OccurredAt: at}
		entries = append(entries, journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack", at, ack)})
	}
	return entries
}

func writeEntriesLog(t *testing.T, path string, entries []journal.Entry) {
	t.Helper()
	var buf syncBuffer
	log, err := journal.CreateLog(&buf, journal.NewLogHeader("h", "v"))
	if err != nil {
		t.Fatal(err)
	}
	var group []journal.Entry
	for _, e := range entries {
		if e.Kind == journal.KindInput && len(group) > 0 {
			if err := log.Append(group); err != nil {
				t.Fatal(err)
			}
			group = nil
		}
		group = append(group, e)
	}
	if err := log.Append(group); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheReporterAlertsOnceOnAnUnacknowledgedOrderUntilItIsAcknowledged: an
// order to be sent with no acknowledgement within -ack-within reaches the
// owner once; its acknowledgement then sends one routine notice that it
// cleared (ADR 0019, "Alerting").
func TestTheReporterAlertsOnceOnAnUnacknowledgedOrderUntilItIsAcknowledged(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "paper.log")
	writeEntriesLog(t, logPath, paperEntries(t, false))
	args := []string{"-journal", logPath, "-state", filepath.Join(dir, "state.json"), "-channel", "stdout,ntfy", "-ack-within", "10m"}
	now := func() time.Time { return t0.Add(time.Hour) }

	var first, second, third bytes.Buffer
	if err := run(args, fakeEnv, now, &first, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "order tw-a not acknowledged") || !strings.Contains(first.String(), "[urgent]") {
		t.Fatalf("first run:\n%s", first.String())
	}
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.String(), "not acknowledged") {
		t.Fatalf("second run repeated the alert:\n%s", second.String())
	}
	writeEntriesLog(t, logPath, paperEntries(t, true))
	if err := run(args, fakeEnv, now, &third, fakeClient); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third.String(), "order tw-a acknowledged") || strings.Contains(third.String(), "not acknowledged") {
		t.Fatalf("after the acknowledgement:\n%s", third.String())
	}
}

// TestAnUnreadVenueReportDoesNotSilenceTheOtherAlerts: a venue report the
// engine refused for not validating is still committed, and the log is
// append-only, so it is in every later reading. It raises its own urgent
// alert once, acknowledges nothing, and the reporter's other alerts keep
// reaching the owner (ADR 0019, "Alerting").
func TestAnUnreadVenueReportDoesNotSilenceTheOtherAlerts(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "paper.log")
	at := t0.Add(time.Minute)
	entries := append(paperEntries(t, false), journal.Entry{Kind: journal.KindInput, Envelope: envelope(t, 4, event.OrderLifecycleEventType, event.OrderLifecycleLimitVersion, "ack-bad", at,
		event.LimitOrderLifecyclePayload{ClientOrderID: "tw-a", OccurredAt: at})})
	writeEntriesLog(t, logPath, entries)
	args := []string{"-journal", logPath, "-state", filepath.Join(dir, "state.json"), "-channel", "stdout,ntfy", "-ack-within", "10m"}
	now := func() time.Time { return t0.Add(time.Hour) }

	var first, second bytes.Buffer
	if err := run(args, fakeEnv, now, &first, fakeClient); err != nil {
		t.Fatalf("first run: %v\n%s", err, first.String())
	}
	for _, want := range []string{"order tw-a not acknowledged", "venue report ack-bad not read"} {
		if !strings.Contains(first.String(), want) {
			t.Errorf("first run lacks %q:\n%s", want, first.String())
		}
	}
	if strings.Contains(first.String(), "journal") && strings.Contains(first.String(), "refused") {
		t.Errorf("the unread report refused the whole journal:\n%s", first.String())
	}
	if err := run(args, fakeEnv, now, &second, fakeClient); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.String(), "not read") || strings.Contains(second.String(), "not acknowledged") {
		t.Errorf("second run repeated an alert:\n%s", second.String())
	}
}

func TestTheAcknowledgementBoundMustBePositive(t *testing.T) {
	fakeEnv, fakeClient := acceptingNtfy(t)
	dir := t.TempDir()
	args := []string{"-journal", writeJournal(t, dir), "-state", filepath.Join(dir, "s.json"), "-channel", "ntfy", "-ack-within", "0s"}
	if err := run(args, fakeEnv, time.Now, &bytes.Buffer{}, fakeClient); err == nil || !strings.Contains(err.Error(), "-ack-within must be positive") {
		t.Fatalf("-ack-within 0s: err = %v", err)
	}
}
