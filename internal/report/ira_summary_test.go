package report_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
)

// iraDay is synthetic record evidence for the summary's holdings seam
// (Boost lessons L9). Every payload is checked by its event validator.
func iraDay(t *testing.T) (*stream, []event.AccountPosition) {
	t.Helper()
	s := &stream{t: t}
	raw, err := os.ReadFile("../../cmd/engine/testdata/momentum-dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg event.TargetWeightConfigurationPayload
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Momentum.TopN, cfg.Momentum.UniverseSize = 25, 500
	add := func(typ string, version uint32, id string, at time.Time, p interface{ Validate() error }) {
		t.Helper()
		if v, ok := p.(interface{ ValidateSchema(uint32) error }); ok {
			if err := v.ValidateSchema(version); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		} else if err := p.Validate(); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		s.add(typ, version, id, at, p)
	}
	add(event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", day(0).Add(-time.Hour), cfg)
	add(event.OperatorRunApprovedEventType, 6, "approval", day(0).Add(-time.Minute), event.OperatorRunApprovedPayload{
		AccountID: "trad-ira", ApprovedBy: "fixture", ApprovedAt: day(0).Add(-time.Minute), Momentum: &event.MomentumStateInForce{},
		StartingBook: &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 100000, Currency: "USD"},
	})
	var holdings []event.AccountPosition
	var targets []event.TargetWeight
	for i := range 25 {
		id := strings.Repeat(string(rune('A'+i)), 3)
		holdings = append(holdings, event.AccountPosition{InstrumentID: id, Quantity: int64(i + 1)})
		targets = append(targets, event.TargetWeight{InstrumentID: id, Weight: 0.02, Shares: []event.SleeveShare{{Sleeve: event.SleeveMomentum, Weight: 0.02}}})
	}
	for _, id := range []string{"IEF", "SHY", "SPY"} {
		holdings = append(holdings, event.AccountPosition{InstrumentID: id, Quantity: 10})
		targets = append(targets, event.TargetWeight{InstrumentID: id, Weight: 0.16, Shares: []event.SleeveShare{{Sleeve: event.SleeveBond, Weight: 0.16}}})
	}
	sort.Slice(holdings, func(i, j int) bool { return holdings[i].InstrumentID < holdings[j].InstrumentID })
	sort.Slice(targets, func(i, j int) bool { return targets[i].InstrumentID < targets[j].InstrumentID })
	add(event.AccountSnapshotEventType, 4, "snapshot", day(0), event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "trad-ira", AsOf: day(0), Currency: "USD", NetLiquidation: 100000, Positions: holdings},
		SettledCash:                     1234, TotalCash: 2345,
	})
	for _, h := range holdings {
		add(event.MarkRecordedEventType, 3, "mark:"+h.InstrumentID, day(0), event.MarkRecordedPayload{
			InstrumentID: h.InstrumentID, AsOf: day(0), Price: 10, Source: "fixture",
			Contract: &event.MarkContract{ConID: 1, Exchange: "SMART", PrimaryExchange: "SYNTHETIC", Currency: "USD"},
		})
	}
	add(event.TargetsDecidedEventType, event.TargetsDecidedSchemaVersion, "targets", day(0), event.TargetsDecidedPayload{
		AccountID: "trad-ira", StrategyID: event.TargetWeightStrategyMomentum, DecisionTime: day(0), SessionPeriodEnd: day(0), Outcome: event.TargetsOutcomeDecided, Targets: targets,
		Glide:    event.GlideFigures{RebalanceSession: "2024-01-03", AgeMonths: 600, BondShare: 0.48},
		Momentum: event.MomentumFigures{CloseSession: "2024-01-02", TrendInstrument: "SPY"},
	})
	add(event.OrderPlannedEventType, event.OrderPlannedSchemaVersion, "plan", day(0), event.OrderPlannedPayload{
		AccountID: "trad-ira", Tag: "targets", ClientOrderID: "buy-AAA", InstrumentID: "AAA", Side: event.OrderSideBuy, Phase: event.OrderSideBuy, OrderType: event.OrderTypeLimit,
		Quantity: 2, LimitPrice: 10, TargetWeight: 0.02, NetLiquidation: 100000, ReferencePrice: 10, PriceBand: 0.05,
	})
	add(event.FillEventType, 5, "fill", day(0).Add(time.Minute), event.TargetWeightFillPayload{
		AccountID: "trad-ira", ClientOrderID: "buy-AAA", FillID: "execution", InstrumentID: "AAA", Side: event.OrderSideBuy, Quantity: 2, Price: 10, FilledAt: day(0).Add(time.Minute),
	})
	return s, holdings
}

func TestIRASummaryReadsItsRecordVersionsAndShowsItsBook(t *testing.T) {
	s, holdings := iraDay(t)
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 0 {
		t.Errorf("IRA inputs not read = %d, want 0", got.RejectedInputs)
	}
	if _, err := report.Alerts(s.out); err != nil {
		t.Fatalf("IRA record versions must remain readable by the reporter: %v", err)
	}
	if !reflect.DeepEqual(got.Holdings, holdings) {
		t.Errorf("IRA holdings = %v, want all 25 synthetic names and IEF/SHY/SPY", got.Holdings)
	}
	text := report.RenderText(got)
	for _, want := range []string{"Net liquidation 100000.00", "Settled cash 1234.00; total cash 2345.00", "buy-AAA buy 2 AAA limit 10.00: planned", "Fills: buy AAA 2 @ 10.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("IRA summary lacks %q", want)
		}
	}
	for _, h := range holdings {
		if !strings.Contains(text, fmt.Sprintf(" %s %d", h.InstrumentID, h.Quantity)) || !strings.Contains(text, "  "+h.InstrumentID+" ") {
			t.Errorf("holding or target missing for %s", h.InstrumentID)
		}
	}
}

func TestIRASummaryKeepsItsBookWhenANewerCashSnapshotIsInvalid(t *testing.T) {
	s, holdings := iraDay(t)
	for i, raw := range []string{
		`{"account_id":"trad-ira","as_of":"2024-01-02T22:00:00Z","currency":"USD","net_liquidation":200000,"positions":[],"settled_cash":"invalid","total_cash":0}`,
		`{"account_id":"other","as_of":"2024-01-02T22:00:00Z","currency":"USD","net_liquidation":200000,"positions":[],"settled_cash":0,"total_cash":0}`,
		`{"account_id":"trad-ira","as_of":"2024-01-02T20:00:00Z","currency":"USD","net_liquidation":200000,"positions":[],"settled_cash":0,"total_cash":0}`,
	} {
		e := s.out[2]
		e.ID, e.Payload, e.PayloadHash = fmt.Sprintf("invalid:%d", i), []byte(raw), event.HashPayload([]byte(raw))
		s.out = append(s.out, e)
	}
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 3 || got.NetLiquidation != 100000 || !reflect.DeepEqual(got.Holdings, holdings) || !strings.Contains(report.RenderText(got), "Settled cash 1234.00; total cash 2345.00") {
		t.Fatalf("refused snapshot replaced the IRA book: %+v", got)
	}
}

func TestIRASummaryRefusesOtherSnapshotShapes(t *testing.T) {
	for _, v := range []uint32{3, 4, 5} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			s, holdings := iraDay(t)
			e := s.out[2]
			e.ID, e.SchemaVersion = "wrong-shape", v
			var p event.AccountCashSnapshotPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			p.AsOf, p.Positions, p.TotalCash = day(0).Add(time.Hour), nil, 9999
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			e.Payload, e.PayloadHash = raw, event.HashPayload(raw)
			s.out = append(s.out, e)
			got, err := report.Summarize(s.out, "2024-01-02", "SPY")
			if err != nil {
				t.Fatal(err)
			}
			if v == 4 {
				if got.RejectedInputs != 0 || len(got.Holdings) != 0 || got.TotalCash != 9999 || !got.HoldingsAsOf.Equal(p.AsOf) {
					t.Fatalf("supported later snapshot not accepted: %+v", got)
				}
			} else if got.RejectedInputs != 1 || !reflect.DeepEqual(got.Holdings, holdings) {
				t.Fatalf("wrong snapshot shape moved the book: %+v", got)
			}
		})
	}
}

func TestBoostSummaryRefusesCashSnapshot(t *testing.T) {
	s := newStream(t)
	s.snapshot(0, 4000, 10)
	s.add(event.AccountSnapshotEventType, 4, "cash-shape", day(0).Add(time.Hour), event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "joint", AsOf: day(0).Add(time.Hour), Currency: "USD", NetLiquidation: 9000}, TotalCash: 9000})
	got, err := report.Summarize(s.out, "2024-01-02", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 1 || got.NetLiquidation != 4000 || len(got.Holdings) != 1 || got.HaveCash {
		t.Fatalf("Boost accepted cash shape: %+v", got)
	}
}

func TestIRASummaryPairsCashSnapshotsAcrossRuns(t *testing.T) {
	first, _ := iraDay(t)
	first.bar(0, "SPY", 100)
	current := &stream{t: t, out: append([]event.Envelope{}, first.out[:1]...)}
	current.bar(1, "SPY", 110)
	p := event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{
		AccountID: "trad-ira", AsOf: day(1), Currency: "USD", NetLiquidation: 105000}, SettledCash: -10, TotalCash: 20}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	current.add(event.AccountSnapshotEventType, 4, "next-snapshot", day(1), p)
	got, err := report.SummarizeWithHistory(report.History{Earlier: first.out}, current.out, "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	if got.RejectedInputs != 0 || len(got.Equity.Points) != 2 || !near(got.Equity.AccountReturn, 0.05) || !strings.Contains(report.RenderText(got), "Settled cash -10.00; total cash 20.00") {
		t.Fatalf("IRA cash snapshots did not pair across runs: %+v", got)
	}
}

func TestReporterReadsApprovalsAtVersionsOneThroughSix(t *testing.T) {
	for v := uint32(1); v <= 6; v++ {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			s := newStream(t)
			p := event.OperatorRunApprovedPayload{AccountID: "joint", PreviousLog: "previous.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
				ApprovedBy: "fixture", ApprovedAt: day(0), Boost: event.BoostStateInForce{Latched: new(false)}}
			if err := p.ValidateSchema(v); err != nil {
				t.Fatal(err)
			}
			s.add(event.OperatorRunApprovedEventType, v, "approval", day(0), p)
			if _, err := report.Alerts(s.out); err != nil {
				t.Fatal(err)
			}
			if _, _, err := report.StateAtStartAlert(s.out, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBoostFixtureSummaryStaysByteIdentical(t *testing.T) {
	got, err := report.Summarize(buildDay(t), "2024-01-03", "SPY")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/boost-day.txt")
	if err != nil {
		t.Fatal(err)
	}
	if text := report.RenderText(got); text != string(want) {
		t.Fatalf("Boost summary changed:\n%s", text)
	}
}
