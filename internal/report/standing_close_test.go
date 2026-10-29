package report_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// flatClosesWithWithhold records two flat closes around a withheld session.
// Its read-time snapshot is not a close (ADR 0026 point 1, snapshot time;
// ADR 0019, amendment of 2026-10-18). The transfer exposes the false interval
// a date-only pairing introduces, even when its two endpoints are flat.
func flatClosesWithWithhold(t *testing.T) (*stream, report.CashLedger) {
	s := newStream(t)
	s.bar(0, "SPY", 100)
	s.snapshot(0, 4000, 0)
	s.decided(0, 0.98)
	s.bar(1, "SPY", 101)
	s.snapshotAs("read-time", "joint", day(1).Add(time.Hour), day(1).Add(time.Hour), 4500)
	s.withheld(1, event.WithheldStaleAccountSnapshot)
	s.bar(2, "SPY", 102)
	s.snapshot(2, 4000, 0)
	s.decided(2, 0.98)
	ledger := report.CashLedger{Read: true, Movements: []report.CashMovement{
		{AsOf: day(1), Amount: 100, TransactionID: "deposit"},
		{AsOf: day(2), Amount: -100, TransactionID: "withdrawal"},
	}}
	return s, ledger
}

func TestAccountReturnsPairOnlyStandingClosesAcrossAWithheldDay(t *testing.T) {
	s, ledger := flatClosesWithWithhold(t)
	got, err := report.SummarizeWithCash(report.History{}, ledger, s.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	text := report.RenderText(got)
	if !strings.Contains(text, "time-weighted: account +0.00%") || !strings.Contains(text, "Day, 2024-01-02 to 2024-01-04: account +0.00%") {
		t.Errorf("flat standing closes must print 0.00%% account returns:\n%s", text)
	}
	if len(got.Equity.Points) != 2 {
		t.Errorf("account points = %d, want 2 (withheld session omitted)", len(got.Equity.Points))
	}
	withheld, err := report.SummarizeWithCash(report.History{}, ledger, s.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if withheld.Equity.Available || withheld.Equity.DayAvailable {
		t.Errorf("withheld reading supplied a close: %+v", withheld.Equity)
	}
}

func TestEarlierRunsPairOnlyStandingCloses(t *testing.T) {
	s, ledger := flatClosesWithWithhold(t)
	current := newStream(t)
	current.out = append(current.out, s.out[len(s.out)-3:]...)
	got, err := report.SummarizeWithCash(report.History{Earlier: s.out[:len(s.out)-3]}, ledger, current.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Equity.Points) != 2 || !near(got.Equity.AccountReturn, 0) || !near(got.Equity.DayAccountReturn, 0) {
		t.Errorf("earlier read-time snapshot entered account returns: %+v", got.Equity)
	}
	// A cached history retains the decision that withheld an exact-time
	// snapshot too (ForHistory; Boost lessons L11).
	for i := range s.out {
		if s.out[i].ID != "read-time" {
			continue
		}
		var p event.AccountPositionsSnapshotPayload
		if err := json.Unmarshal(s.out[i].Payload, &p); err != nil {
			t.Fatal(err)
		}
		p.AsOf = day(1)
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		s.out[i].Payload, s.out[i].PayloadHash = raw, event.HashPayload(raw)
	}
	var recovery journal.Recovery
	for _, e := range s.out[:len(s.out)-3] {
		kind := journal.KindInput
		if e.Type == event.TargetsDecidedEventType {
			kind = journal.KindDecision
		}
		recovery.Entries = append(recovery.Entries, journal.Entry{Kind: kind, Envelope: e})
	}
	var earlier []event.Envelope
	for _, e := range report.ForHistory(recovery, "SPY").Entries {
		earlier = append(earlier, e.Envelope)
	}
	got, err = report.SummarizeWithCash(report.History{Earlier: earlier}, ledger, current.out, "2024-01-04", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Equity.Points) != 2 || !near(got.Equity.AccountReturn, 0) || !near(got.Equity.DayAccountReturn, 0) {
		t.Fatalf("cached history lost the withheld decision: %+v", got.Equity)
	}
}

func TestReadTimeAndWithheldSnapshotsNeverSupplyAnAccountPoint(t *testing.T) {
	for _, tc := range []struct {
		name               string
		readTime, withheld bool
	}{
		{"read time without a decision", true, false},
		{"exact timestamp on a withheld session", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStream(t)
			s.bar(0, "SPY", 100)
			s.snapshot(0, 4000, 0)
			s.bar(1, "SPY", 101)
			at := day(1)
			if tc.readTime {
				at = at.Add(time.Hour)
			}
			s.snapshotAs("not-close", "joint", at, at, 4500)
			if tc.withheld {
				s.withheld(1, event.WithheldStaleAccountSnapshot)
			}
			s.bar(2, "SPY", 102)
			s.snapshot(2, 4000, 0)
			got, err := report.Summarize(s.out, "2024-01-04", "SPY")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Equity.Points) != 2 || !near(got.Equity.DayAccountReturn, 0) {
				t.Fatalf("not a standing close: %+v", got.Equity)
			}
		})
	}
}

func TestRebalanceOnlyWithheldKeepsStandingAccountPoint(t *testing.T) {
	for _, reason := range []string{event.WithheldAllUnaffordable, event.WithheldRebalanceInProgress, event.WithheldNoEligibleNames, event.WithheldMissingBars} {
		t.Run(reason, func(t *testing.T) {
			s, _ := iraDay(t)
			s.bar(0, "SPY", 100)
			s.bar(1, "SPY", 100)
			s.add(event.AccountSnapshotEventType, 4, "next", day(1), event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "trad-ira", AsOf: day(1), Currency: "USD", NetLiquidation: 100000}})
			s.add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "withheld", day(1), event.TargetsDecidedPayload{AccountID: "trad-ira", StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: day(1), SessionPeriodEnd: day(1), Outcome: event.TargetsOutcomeWithheld, WithheldReason: reason, Detail: "synthetic withheld rebalance", Momentum: event.MomentumFigures{CloseSession: "2024-01-03", TrendInstrument: "SPY"}, Glide: event.GlideFigures{AgeMonths: 600, RebalanceSession: "2024-01-04", BondShare: 0.48}})
			for _, historical := range []bool{false, true} {
				var h report.History
				current := s.out
				if historical {
					h.Earlier, current = s.out[1:], s.out[:1]
				}
				got, err := report.SummarizeWithHistory(h, current, "2024-01-03", "SPY")
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Equity.Points) != 2 {
					t.Errorf("historical=%v: standing point lost: %+v", historical, got.Equity)
				}
			}
		})
	}
}

func TestUnreadablePastRecordMakesAccountPointUnknown(t *testing.T) {
	for _, typ := range []string{event.TargetsDecidedEventType, event.AccountSnapshotEventType} {
		for _, malformed := range []bool{false, true} {
			for _, historical := range []bool{false, true} {
				s := newStream(t)
				for n := range 3 {
					s.bar(n, "SPY", 100)
					s.snapshot(n, 4000, 0)
					s.decided(n, 0.98)
				}
				for i := range s.out {
					e := &s.out[i]
					if e.Type == typ && e.EventTime.Equal(day(1)) {
						if malformed {
							e.Payload = []byte(`{"broken":`)
							e.PayloadHash = event.HashPayload(e.Payload)
						} else {
							e.SchemaVersion = 999
						}
					}
				}
				var h report.History
				current := s.out
				if historical {
					h.Earlier, current = s.out[1:7], append(append([]event.Envelope{}, s.out[:1]...), s.out[7:]...)
				}
				got, err := report.SummarizeWithHistory(h, current, "2024-01-04", "SPY")
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Equity.Points) != 2 || !strings.Contains(report.RenderText(got), "account point unknown for 2024-01-03") {
					t.Errorf("type=%s malformed=%v historical=%v: %s", typ, malformed, historical, report.RenderText(got))
				}
			}
		}
	}
}

func TestGapTransferAppliedAtEndOnAccountAndShadow(t *testing.T) {
	for _, amount := range []float64{100, -100} {
		s, _ := flatClosesWithWithhold(t)
		s.snapshot(2, 4000+amount, 0)
		// Replace the last close rather than append an equal-time snapshot.
		s.out[len(s.out)-3] = s.out[len(s.out)-1]
		s.out = s.out[:len(s.out)-1]
		ledger := report.CashLedger{Read: true, Movements: []report.CashMovement{{AsOf: day(1), Amount: amount, TransactionID: "gap"}}}
		got, err := report.SummarizeWithCash(report.History{}, ledger, s.out, "2024-01-04", "SPY")
		if err != nil {
			t.Fatal(err)
		}
		if !near(got.Equity.AccountReturn, 0) || !near(got.Equity.BenchmarkReturn, 0.02) || !near(got.Equity.ShadowValue, 4080+amount) {
			t.Fatalf("gap transfer earned a shadow return: %+v", got.Equity)
		}
	}
}
