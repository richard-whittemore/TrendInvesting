package restart_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// sessionInput is the input whose committed reply released the decisions
// after it.
func sessionInput(t *testing.T, id string) journal.Entry {
	return recorded(t, journal.KindInput, id, event.SessionClosedEventType, event.SessionClosedSchemaVersion, event.SessionClosedPayload{
		PeriodEnd: time.Date(2026, time.October, 17, 20, 0, 0, 0, time.UTC), InstrumentIDs: []string{"SPY"},
	})
}

// neverSent is the adapter's settlement, for the ended run of run-1.log, of
// order client, a sale of 5 SPY at 90 that input session:1 released and the
// adapter never sent.
func neverSent(client string) event.NeverSentSettlement {
	return event.NeverSentSettlement{
		Log: "run-1.log", Run: "operator-run-approved:run-0.log", InputID: "session:1", Evidence: event.NeverSentUnmarked,
		Retirement: event.OrderRetiredPayload{
			AccountID: "joint", ClientOrderID: client, InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 5, LimitPrice: 90,
			Reason: event.OrderRetiredByOwnerReason, Detail: "the run ended before the adapter sent it", RetiredBy: "owner",
			RetiredAt: time.Date(2026, time.October, 17, 21, 0, 0, 0, time.UTC),
		},
	}
}

// endedWithAnUnsentSale is a log that ended Halted after input session:1
// planned and released the incident's sale, joint-sale-1, which the adapter
// never sent.
func endedWithAnUnsentSale(t *testing.T) []journal.Entry {
	halted := engineStateEntry(t, event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "x"})
	return []journal.Entry{sessionInput(t, "session:1"), halted, incidentSale(t, incidentID), saleReleased(t, "joint-sale-1")}
}

// TestASaleTheAdapterNeverSentIsNotAttemptedOnceSettled: the run ended with
// the incident's sale released and never sent, and the adapter's settlement
// says so; the engine's own retirement rule (targetweight.RetirementVerdict)
// applies it, so the approval states the sale not attempted and one of its
// sales retired, carrying the settlement as its evidence, and the fresh run
// tries it again under the next attempt's client order ID (ADR 0019,
// amended 2026-10-17, reading 4, as the owner's decision of 2026-10-22
// reads it; ADR 0026, point 2, "Order identity"). Without the settlement
// the attempt stays spent (fail closed, AGENTS.md).
func TestASaleTheAdapterNeverSentIsNotAttemptedOnceSettled(t *testing.T) {
	entries := endedWithAnUnsentSale(t)
	unsettled, err := restart.ReadEndState(entries, "joint", "run-1.log")
	if err != nil {
		t.Fatal(err)
	}
	if !unsettled.SaleAttempted || unsettled.SaleRetiredAttempts != 0 {
		t.Fatalf("unsettled: %+v, want the sale attempted", unsettled)
	}

	end, err := restart.ReadEndState(entries, "joint", "run-1.log", neverSent("joint-sale-1"))
	if err != nil {
		t.Fatal(err)
	}
	if end.SaleAttempted || end.SaleClientOrderID != "" || end.SaleRetiredAttempts != 1 || len(end.SaleSettlements) != 1 {
		t.Fatalf("settled: %+v, want the sale not attempted and one retired", end)
	}
	p, err := restart.ProposeApproval(end, restart.ApprovalRequest{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("a", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 17, 22, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	in := p.IncidentInForce
	if in == nil || *in.SaleAttempted || in.SaleRetiredAttempts != 1 || len(in.SaleSettlements) != 1 || in.SaleSettlements[0].Retirement.ClientOrderID != "joint-sale-1" {
		t.Fatalf("approval: %+v, want the sale not attempted, one retired, with its settlement", in)
	}
	if err := restart.CheckSaleAttempt(entries, p, "run-1.log"); err != nil {
		t.Errorf("CheckSaleAttempt = %v, want the approval accepted against the log", err)
	}
	// The settlement survives the approval file's own round trip.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := event.DecodeOperatorRunApproved(raw); err != nil {
		t.Errorf("DecodeOperatorRunApproved = %v, want the proposed approval read back", err)
	}
}

// TestASaleTheAdapterSentStaysAttempted: a sale a venue named, reported
// executed or IBKR listed was sent, so it stays attempted; a settlement
// claiming it never sent is refused, naming the settlement, never read as a
// retirement (ADR 0019, amended 2026-10-15; fail closed, AGENTS.md).
func TestASaleTheAdapterSentStaysAttempted(t *testing.T) {
	for name, named := range map[string]journal.Entry{
		"reported": saleReported(t, "joint-sale-1"),
		"executed": fillOf(t, "joint-sale-1", "joint"),
		"listed":   saleListed(t, "joint-sale-1"),
	} {
		entries := append(endedWithAnUnsentSale(t), named)
		end, err := restart.ReadEndState(entries, "joint", "run-1.log")
		if err != nil || !end.SaleAttempted {
			t.Errorf("%s, no settlement: %+v %v, want the sale attempted", name, end, err)
		}
		if _, err := restart.ReadEndState(entries, "joint", "run-1.log", neverSent("joint-sale-1")); err == nil || !strings.Contains(err.Error(), "joint-sale-1") || !strings.Contains(err.Error(), "venue") {
			t.Errorf("%s, settled as never sent: %v, want the settlement refused", name, err)
		}
	}
}

// TestASettlementThatDoesNotMatchTheLogIsRefused: each settlement is checked
// against the approved log by the engine's retirement rule and the input
// that released the order; one that does not match refuses the approval
// with a named error, never ignored (fail closed, AGENTS.md). A settlement
// of another log is not this approval's, and is left out.
func TestASettlementThatDoesNotMatchTheLogIsRefused(t *testing.T) {
	entries := endedWithAnUnsentSale(t)
	type change func(*event.NeverSentSettlement)
	for _, c := range []struct {
		name   string
		change change
		want   string
	}{
		{"another input", func(s *event.NeverSentSettlement) { s.InputID = "session:9" }, "session:9"},
		{"another quantity", func(s *event.NeverSentSettlement) { s.Retirement.Quantity = 6 }, "released as"},
		{"another account", func(s *event.NeverSentSettlement) { s.Retirement.AccountID = "ira" }, "account"},
		{"an order never released", func(s *event.NeverSentSettlement) { s.Retirement.ClientOrderID = "joint-sale-9" }, "did not release"},
		{"unknown evidence", func(s *event.NeverSentSettlement) { s.Evidence = "probably" }, "evidence"},
		{"the adapter's own reason", func(s *event.NeverSentSettlement) {
			s.Retirement.Reason, s.Retirement.RetiredBy = event.OrderRetiredSessionOver, event.OrderRetiredByAdapter
		}, "owner"},
		{"no run", func(s *event.NeverSentSettlement) { s.Run = "" }, "no run"},
	} {
		s := neverSent("joint-sale-1")
		c.change(&s)
		if _, err := restart.ReadEndState(entries, "joint", "run-1.log", s); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "run-1.log") {
			t.Errorf("%s: ReadEndState = %v, want a refusal saying %q in run-1.log", c.name, err, c.want)
		}
	}
	twice := []event.NeverSentSettlement{neverSent("joint-sale-1"), neverSent("joint-sale-1")}
	if _, err := restart.ReadEndState(entries, "joint", "run-1.log", twice...); err == nil || !strings.Contains(err.Error(), "retired already") {
		t.Errorf("settled twice: %v, want the second refused", err)
	}
	retired := append(endedWithAnUnsentSale(t), retiredEntry(t, "joint-sale-1"))
	if _, err := restart.ReadEndState(retired, "joint", "run-1.log", neverSent("joint-sale-1")); err == nil || !strings.Contains(err.Error(), "retired already") {
		t.Errorf("retired in the log, then settled: %v, want the settlement refused", err)
	}
	other := neverSent("joint-sale-1")
	other.Log = "run-0.log"
	if end, err := restart.ReadEndState(entries, "joint", "run-1.log", other); err != nil || !end.SaleAttempted {
		t.Errorf("another log's settlement: %+v %v, want it left out and the sale attempted", end, err)
	}
}

// TestTheEngineChecksTheApprovalsSettlements: the engine reads the approval
// against the approved log with the same rule, so an approval stating the
// sale not attempted holds only with a settlement the log bears out; one
// whose settlement the log refutes, or that settles an order that is no
// sale of the incident, is refused (ADR 0019, amended 2026-10-15; fail
// closed, AGENTS.md).
func TestTheEngineChecksTheApprovalsSettlements(t *testing.T) {
	entries := endedWithAnUnsentSale(t)
	carried := func(settlements ...event.NeverSentSettlement) event.OperatorRunApprovedPayload {
		return event.OperatorRunApprovedPayload{AccountID: "joint", PreviousLog: "run-1.log", IncidentInForce: &event.IncidentInForce{
			IncidentID: incidentID, State: event.EngineStateHalted, SaleAttempted: new(false), SaleRetiredAttempts: len(settlements), SaleSettlements: settlements}}
	}
	if err := restart.CheckSaleAttempt(entries, carried(neverSent("joint-sale-1")), "run-1.log"); err != nil {
		t.Errorf("settled: %v, want accepted", err)
	}
	handSold := carried(neverSent("joint-sale-1"))
	handSold.IncidentInForce.SaleAttempted = new(true)
	if err := restart.CheckSaleAttempt(entries, handSold, "run-1.log"); err != nil {
		t.Errorf("settled, and a hand sale stated attempted: %v, want accepted", err)
	}
	if err := restart.CheckSaleAttempt(append(entries, saleReported(t, "joint-sale-1")), handSold, "run-1.log"); err == nil {
		t.Error("a hand sale stated attempted beside a settlement the log refutes: accepted, want the settlement still checked")
	}
	if err := restart.CheckSaleAttempt(entries, carried(), "run-1.log"); err == nil || !strings.Contains(err.Error(), "joint-sale-1") {
		t.Errorf("unsettled, stated not attempted: %v, want refused", err)
	}
	refuted := carried(neverSent("joint-sale-1"))
	if err := restart.CheckSaleAttempt(append(entries, saleReported(t, "joint-sale-1")), refuted, "run-1.log"); err == nil || !strings.Contains(err.Error(), "venue") {
		t.Errorf("a settlement the log refutes: %v, want refused", err)
	}
	buy := neverSent("tw-buy-1")
	buy.Retirement.Side = event.OrderSideBuy
	withBuy := append(endedWithAnUnsentSale(t), recorded(t, journal.KindDecision, "order-released:tw-buy-1", event.OrderReleasedEventType, event.OrderReleasedSchemaVersion, event.OrderReleasedPayload{
		AccountID: "joint", ClientOrderID: "tw-buy-1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 5, LimitPrice: 90, Mode: event.TradingModePaper, Send: true,
	}))
	if err := restart.CheckSaleAttempt(withBuy, carried(neverSent("joint-sale-1"), buy), "run-1.log"); err == nil || !strings.Contains(err.Error(), "tw-buy-1") {
		t.Errorf("a settlement of an order that is no sale of the incident: %v, want refused", err)
	}
	// The operator tool carries only the incident's sales: a settled buy
	// beside the sale is checked and left out of the approval.
	end, err := restart.ReadEndState(withBuy, "joint", "run-1.log", neverSent("joint-sale-1"), buy)
	if err != nil || len(end.SaleSettlements) != 1 || end.SaleRetiredAttempts != 1 {
		t.Errorf("a settled buy beside the sale: %+v %v, want only the sale carried", end, err)
	}
}

// TestReadSettlementsReadsTheAdaptersLinesStrictly: the adapter's
// settlement file is read strictly, one settlement a line, every key
// stated and none unknown; anything else refuses, naming the file and the
// line, rather than being read as no settlement (fail closed, AGENTS.md).
func TestReadSettlementsReadsTheAdaptersLinesStrictly(t *testing.T) {
	line := func(s event.NeverSentSettlement) string {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	good := line(neverSent("joint-sale-1"))
	got, err := restart.ReadSettlements([]byte(good+"\n\n"+line(neverSent("joint-sale-2"))+"\n"), "p.never-sent")
	if err != nil || len(got) != 2 || got[1].Retirement.ClientOrderID != "joint-sale-2" {
		t.Fatalf("ReadSettlements = %+v %v, want both lines", got, err)
	}
	if got, err := restart.ReadSettlements(nil, "p.never-sent"); err != nil || len(got) != 0 {
		t.Errorf("an empty file: %+v %v, want none", got, err)
	}
	for name, c := range map[string]struct{ raw, want string }{
		"not JSON":                  {good + "\nnot json\n", "line 2"},
		"an unknown key":            {strings.Replace(good, `"log"`, `"extra":1,"log"`, 1) + "\n", "line 1"},
		"a missing key":             {strings.Replace(good, `"evidence":"unmarked",`, "", 1) + "\n", "evidence"},
		"retirement null":           {`{"evidence":"unmarked","input_id":"session:1","log":"run-1.log","retirement":null,"run":"r"}` + "\n", "not an object"},
		"two values":                {good + good + "\n", "line 1"},
		"retirement not an object":  {strings.Replace(good, `"retirement":{`, `"retirement":5,"x":{`, 1) + "\n", "line 1"},
		"a retirement key missing":  {strings.Replace(good, `"side":"sell",`, "", 1) + "\n", "retirement.side"},
		"a value of the wrong type": {strings.Replace(good, `"quantity":5`, `"quantity":"5"`, 1) + "\n", "line 1"},
	} {
		if _, err := restart.ReadSettlements([]byte(c.raw), "p.never-sent"); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "p.never-sent") {
			t.Errorf("%s: ReadSettlements = %v, want a refusal naming %q in p.never-sent", name, err, c.want)
		}
	}
}
