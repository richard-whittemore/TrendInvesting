package journalquery_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/journalquery"
)

func operatorJournal(t *testing.T) []byte {
	t.Helper()
	d := at(time.March, 2)
	approval := event.OperatorRunApprovedPayload{
		AccountID: "A", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("0", 64),
		ApprovedBy: "owner", ApprovedAt: d, Boost: event.BoostStateInForce{Latched: new(true)},
		IncidentInForce: &event.IncidentInForce{IncidentID: "engine-state:A:1", State: event.EngineStateDegraded, ScopeInstrumentIDs: []string{"SPY"}, SaleAttempted: new(false)},
	}
	cleared := approval
	cleared.IncidentID, cleared.IncidentInForce = "engine-state:A:1", nil
	normal := approval
	normal.IncidentInForce = nil
	return build(t,
		rec{journal.KindInput, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, d, approval},
		rec{journal.KindInput, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, d, cleared},
		rec{journal.KindInput, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, d, normal},
		rec{journal.KindInput, event.KillSwitchEventType, event.KillSwitchSchemaVersion, d.Add(time.Hour),
			event.KillSwitchPayload{AccountID: "A", AsOf: d.Add(time.Hour), Engaged: true, Operator: "owner", Detail: "news"}},
		dec(event.EngineStateEventType, event.EngineStateSchemaVersion, d.Add(time.Hour),
			event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonRiskLimitBreached, Detail: "not an operator input"}),
		rec{journal.KindInput, event.KillSwitchEventType, event.KillSwitchSchemaVersion, at(time.March, 3),
			event.KillSwitchPayload{AccountID: "A", AsOf: at(time.March, 3), Engaged: false, Operator: "owner", Detail: "clear"}},
		rec{journal.KindInput, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, at(time.March, 4),
			event.RebalanceClearedPayload{AccountID: "B", AsOf: at(time.March, 4), DecisionID: "targets-decided:B:1", ClientOrderIDs: []string{"o1", "o2"}, Operator: "owner", Detail: "cancelled"}},
	)
}

// TestTheOperatorReportListsEveryOperatorInput: each recorded operator
// input, the kill switch, a fresh run's approval and a rebalance clearance,
// is visible in the log as what it did, who recorded it and why (ADR 0026,
// points 3 and 5; ADR 0027, amended 2026-10-17).
func TestTheOperatorReportListsEveryOperatorInput(t *testing.T) {
	js := []journalquery.Journal{load(t, "run.journal", operatorJournal(t))}
	table, err := journalquery.Operator(js, journalquery.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) != 6 {
		t.Fatalf("rows %v, want 6", table.Rows)
	}
	for i, want := range []struct{ input, action, by, detail, account string }{
		{"run-approved", "after run-1.log, latched true; incident engine-state:A:1 in force, degraded on SPY, sale attempted false", "owner", "-", "A"},
		{"run-approved", "after run-1.log, latched true; clears incident engine-state:A:1", "owner", "-", "A"},
		{"run-approved", "after run-1.log, latched true; no incident", "owner", "-", "A"},
		{"kill-switch", "engaged", "owner", "news", "A"},
		{"kill-switch", "released", "owner", "clear", "A"},
		{"rebalance-cleared", "cleared targets-decided:B:1: o1;o2", "owner", "cancelled", "B"},
	} {
		got := struct{ input, action, by, detail, account string }{cell(table, i, "input"), cell(table, i, "action"), cell(table, i, "by"), cell(table, i, "detail"), cell(table, i, "account")}
		if got != want {
			t.Errorf("row %d: %+v, want %+v", i, got, want)
		}
	}
	if cell(table, 3, "time") != "2026-03-02T22:00:00Z" {
		t.Errorf("the kill switch's time is %s, to the second", cell(table, 3, "time"))
	}

	f, err := journalquery.NewFilter("B", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if table, err = journalquery.Operator(js, f); err != nil || len(table.Rows) != 1 {
		t.Errorf("account B: %v %v", table.Rows, err)
	}
	if f, err = journalquery.NewFilter("", "", "2026-03-03", "2026-03-03"); err != nil {
		t.Fatal(err)
	}
	if table, err = journalquery.Operator(js, f); err != nil || len(table.Rows) != 1 || cell(table, 0, "action") != "released" {
		t.Errorf("the third of March: %v %v", table.Rows, err)
	}
}

// TestAnUnreadableOperatorInputIsRefusedByEveryReport: an operator input
// that cannot be read refuses the report, as every other record does.
func TestAnUnreadableOperatorInputIsRefusedByEveryReport(t *testing.T) {
	d := at(time.March, 2)
	for _, r := range []rec{
		{journal.KindInput, event.KillSwitchEventType, event.KillSwitchSchemaVersion, d, map[string]any{"account_id": "A"}},
		{journal.KindInput, event.RebalanceClearedEventType, event.RebalanceClearedSchemaVersion, d, "x"},
		{journal.KindInput, event.OperatorRunApprovedEventType, event.OperatorRunApprovedSchemaVersion, d, map[string]any{}},
		{journal.KindInput, event.KillSwitchEventType, 9, d, map[string]any{}},
	} {
		js := []journalquery.Journal{load(t, "j", build(t, r))}
		for name, fn := range map[string]func([]journalquery.Journal, journalquery.Filter) (journalquery.Table, error){
			"operator": journalquery.Operator, "orders": journalquery.Orders, "refusals": journalquery.Refusals,
		} {
			if _, err := fn(js, journalquery.Filter{}); err == nil {
				t.Errorf("%s at version %d: %s accepted", r.typ, r.schema, name)
			}
		}
	}
	if _, err := journalquery.Operator([]journalquery.Journal{load(t, "a", operatorJournal(t)), load(t, "a", operatorJournal(t))}, journalquery.Filter{}); err == nil {
		t.Error("two copies of one journal are reported")
	}
}
