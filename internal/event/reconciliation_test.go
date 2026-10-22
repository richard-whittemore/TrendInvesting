package event_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validReconciliation() event.ReconciliationPayload {
	return event.ReconciliationPayload{
		AccountID: "joint", ReconciliationID: "morning:2025-03-05", Trigger: event.ReconciliationTriggerMorning,
		AsOf:       twSession.Add(12 * time.Hour),
		Unreadable: []string{},
		Positions:  []event.AccountPosition{{InstrumentID: "SPY", Quantity: 147}},
		TotalCash:  -4700, Currency: "USD",
	}
}

func validReconciliationResult() event.ReconciliationResultPayload {
	return event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning:2025-03-05", InputID: "reconciliation:1", AnchorID: "snapshot:1",
		Classification: event.ReconciliationUnexplained, Action: event.ReconciliationActionDegrade,
		CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{
			{Component: event.ReconciliationComponentCash, Book: -470000, Expected: -470000, Observed: -470000, Explained: true},
			{Component: event.ReconciliationComponentPosition, InstrumentID: "SPY", Book: 147, Expected: 147, Observed: 140, Residual: -7, Explanations: []string{}},
		},
		ScopeInstrumentIDs: []string{"SPY"},
		Detail:             "SPY: book 147, expected 147, broker 140",
	}
}

func validAlertAcknowledged() event.AlertAcknowledgedPayload {
	return event.AlertAcknowledgedPayload{
		AccountID: "joint", AlertKey: "reconciliation:joint:reconciliation-result:joint:reconciliation:1",
		AcknowledgedBy: "owner", AcknowledgedAt: twSession.Add(13 * time.Hour), Detail: "looking at the broker's activity",
	}
}

func TestReconciliationFixturesAreValid(t *testing.T) {
	for _, p := range []validator{validReconciliation(), validReconciliationResult(), validAlertAcknowledged()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%T: %v", p, err)
		}
	}
}

func TestReconciliationValidate(t *testing.T) {
	v := validReconciliation()
	type P = event.ReconciliationPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "id", v, func(p *P) { p.ReconciliationID = "" }, "reconciliation id")
	expectInvalid(t, "trigger", v, func(p *P) { p.Trigger = "hourly" }, "trigger")
	expectInvalid(t, "sub-second as of", v, func(p *P) { p.AsOf = p.AsOf.Add(time.Millisecond) }, "whole second")
	expectInvalid(t, "unknown component", v, func(p *P) { p.Unreadable = []string{"open-orders"} }, "not recognised")
	expectInvalid(t, "descending components", v, func(p *P) {
		p.Unreadable = []string{event.ReconciliationComponentCash, event.ReconciliationComponentPositions}
		p.Positions, p.TotalCash, p.Currency = nil, 0, ""
	}, "strictly ascending")
	expectInvalid(t, "positions unstated", v, func(p *P) { p.Positions = nil }, "[] when none are held")
	expectInvalid(t, "unreadable positions with rows", v, func(p *P) { p.Unreadable = []string{event.ReconciliationComponentPositions} }, "carry no rows")
	expectInvalid(t, "position id", v, func(p *P) { p.Positions = []event.AccountPosition{{Quantity: 1}} }, "instrument id")
	expectInvalid(t, "zero row", v, func(p *P) { p.Positions = []event.AccountPosition{{InstrumentID: "SPY"}} }, "zero")
	expectInvalid(t, "unsorted", v, func(p *P) {
		p.Positions = []event.AccountPosition{{InstrumentID: "TLT", Quantity: 1}, {InstrumentID: "SPY", Quantity: 1}}
	}, "strictly ascending")
	expectInvalid(t, "cash", v, func(p *P) { p.TotalCash = math.NaN() }, "finite")
	expectInvalid(t, "currency", v, func(p *P) { p.Currency = "" }, "currency")
	expectInvalid(t, "unreadable cash with a value", v, func(p *P) {
		p.Unreadable = []string{event.ReconciliationComponentCash}
	}, "carries no value")

	short := v
	short.Positions = []event.AccountPosition{{InstrumentID: "SPY", Quantity: -3}}
	if err := short.Validate(); err != nil {
		t.Errorf("a short is recorded as observed, for the classifier to name: %v", err)
	}
	unreadable := v
	unreadable.Unreadable = []string{event.ReconciliationComponentPositions, event.ReconciliationComponentCash}
	unreadable.Positions, unreadable.TotalCash, unreadable.Currency = []event.AccountPosition{}, 0, ""
	if err := unreadable.Validate(); err != nil {
		t.Errorf("an observation that read nothing is still a valid input: %v", err)
	}
}

func TestReconciliationRequiresUnreadableStated(t *testing.T) {
	raw, err := json.Marshal(validReconciliation())
	if err != nil {
		t.Fatal(err)
	}
	var stated map[string]any
	if err := json.Unmarshal(raw, &stated); err != nil {
		t.Fatal(err)
	}
	if _, ok := stated["unreadable"]; !ok {
		t.Fatal("the payload states unreadable, [] when every component was read")
	}
	delete(stated, "unreadable")
	raw, _ = json.Marshal(stated)
	var p event.ReconciliationPayload
	if err := json.Unmarshal(raw, &p); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("an observation that omits unreadable decoded: %v", err)
	}
}

func TestReconciliationResultValidate(t *testing.T) {
	v := validReconciliationResult()
	type P = event.ReconciliationResultPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "id", v, func(p *P) { p.ReconciliationID = "" }, "reconciliation id")
	expectInvalid(t, "input", v, func(p *P) { p.InputID = "" }, "input id")
	expectInvalid(t, "classification", v, func(p *P) { p.Classification = "probably-fine" }, "classification")
	expectInvalid(t, "action", v, func(p *P) { p.Action = event.ReconciliationActionContinue }, "action")
	expectInvalid(t, "detail", v, func(p *P) { p.Detail = "" }, "detail")
	expectInvalid(t, "no scope", v, func(p *P) { p.ScopeInstrumentIDs = nil }, "scope")
	expectInvalid(t, "scope order", v, func(p *P) { p.ScopeInstrumentIDs = []string{"TLT", "SPY"} }, "strictly ascending")
	expectInvalid(t, "residual", v, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), p.Components...)
		p.Components[1].Residual = -6
	}, "residual")
	expectInvalid(t, "explained residual", v, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), p.Components...)
		p.Components[1].Explained = true
	}, "explained")
	expectInvalid(t, "component kind", v, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), p.Components...)
		p.Components[0].Component = "orders"
	}, "component")
	expectInvalid(t, "position names its instrument", v, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), p.Components...)
		p.Components[1].InstrumentID = ""
	}, "instrument")
	expectInvalid(t, "explanation id", v, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), p.Components...)
		p.Components[1].Explanations = []string{""}
	}, "explanation")
	expectInvalid(t, "cash currency", v, func(p *P) { p.Currency = "" }, "currency")

	matched := v
	matched.Classification, matched.Action, matched.ScopeInstrumentIDs = event.ReconciliationMatched, event.ReconciliationActionContinue, nil
	matched.Components = matched.Components[:1]
	if err := matched.Validate(); err != nil {
		t.Errorf("matched: %v", err)
	}
	expectInvalid(t, "a match names no scope", matched, func(p *P) { p.ScopeCashComponents = []string{event.ReconciliationComponentCash} }, "scope")
	expectInvalid(t, "a match has no unexplained component", matched, func(p *P) {
		p.Components = append([]event.ReconciliationComponent(nil), v.Components...)
	}, "explained")
	halted := v
	halted.Classification, halted.Action, halted.ScopeInstrumentIDs = event.ReconciliationUnverifiable, event.ReconciliationActionHalt, nil
	halted.Unverifiable = []string{"the broker's positions could not be read"}
	if err := halted.Validate(); err != nil {
		t.Errorf("unverifiable: %v", err)
	}
	expectInvalid(t, "unverifiable names its reasons", halted, func(p *P) { p.Unverifiable = nil }, "reason")
	expectInvalid(t, "a halt names no scope", halted, func(p *P) { p.ScopeInstrumentIDs = []string{"SPY"} }, "scope")
	expectInvalid(t, "reasons only when unverifiable", v, func(p *P) { p.Unverifiable = []string{"x"} }, "reason")
	noCash := matched
	noCash.CashCompared, noCash.Currency, noCash.Components = false, "", nil
	if err := noCash.Validate(); err != nil {
		t.Errorf("a book with no cash compares none: %v", err)
	}
	expectInvalid(t, "cash component only when compared", noCash, func(p *P) { p.Components = matched.Components }, "cash")
}

func TestAlertAcknowledgedValidate(t *testing.T) {
	v := validAlertAcknowledged()
	type P = event.AlertAcknowledgedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account")
	expectInvalid(t, "key", v, func(p *P) { p.AlertKey = "" }, "alert key")
	expectInvalid(t, "by", v, func(p *P) { p.AcknowledgedBy = "" }, "acknowledged by")
	expectInvalid(t, "at", v, func(p *P) { p.AcknowledgedAt = time.Time{} }, "acknowledged at")
}

func TestEngineStateReconciliationReasons(t *testing.T) {
	for _, reason := range []string{event.EngineStateReasonReconciliationUnexplained, event.EngineStateReasonReconciliationUnverifiable} {
		cash := event.EngineStatePayload{State: event.EngineStateDegraded, Reason: reason, Detail: "d", ScopeCashComponents: []string{event.ReconciliationComponentCash}}
		if err := cash.ValidateSchema(event.EngineStateScopedVersion); err != nil {
			t.Errorf("%s at version 2: %v", reason, err)
		}
		if err := cash.ValidateSchema(event.EngineStateSchemaVersion); err == nil {
			t.Errorf("%s is not a version-1 reason", reason)
		}
	}
}

func TestMatchedResultUsesNoExplanation(t *testing.T) {
	p := validReconciliationResult()
	p.Classification, p.Action, p.ScopeInstrumentIDs = event.ReconciliationMatched, event.ReconciliationActionContinue, nil
	p.Components = []event.ReconciliationComponent{{Component: event.ReconciliationComponentCash, Book: 1, Expected: 2, Observed: 2, Explained: true, Explanations: []string{"fill-1"}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "matched") {
		t.Fatalf("a match that needed an explanation is explained, not matched: %v", err)
	}
	p.Classification = event.ReconciliationExplained
	if err := p.Validate(); err != nil {
		t.Fatalf("explained: %v", err)
	}
}

func TestReconciliationDecodes(t *testing.T) {
	raw, err := json.Marshal(validReconciliation())
	if err != nil {
		t.Fatal(err)
	}
	var p event.ReconciliationPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Validate() != nil || p.Unreadable == nil {
		t.Fatalf("decode: %v, %+v", err, p)
	}
	if err := json.Unmarshal([]byte(`{"unreadable":"positions"}`), &p); err == nil {
		t.Error("an unreadable list that is not a list decoded")
	}
}

func TestReconciliationActionFollowsClassification(t *testing.T) {
	v := validReconciliationResult()
	type P = event.ReconciliationResultPayload
	matched := v
	matched.Classification, matched.ScopeInstrumentIDs, matched.Components = event.ReconciliationMatched, nil, v.Components[:1]
	expectInvalid(t, "a match continues", matched, func(p *P) { p.Action = event.ReconciliationActionHalt }, "continues")
	halted := v
	halted.Classification, halted.ScopeInstrumentIDs, halted.Unverifiable = event.ReconciliationUnverifiable, nil, []string{"x"}
	expectInvalid(t, "unverifiable halts", halted, func(p *P) { p.Action = event.ReconciliationActionDegrade }, "halts")
}

// TestTheIBKRAdapterPayloadsDecodeAndValidate is the contract with the IBKR
// adapter (adapter/ibkr/snapshot.py): its test produces exactly these files,
// and the engine decodes and validates them, so a field one side renames or
// leaves unstated fails here.
func TestTheIBKRAdapterPayloadsDecodeAndValidate(t *testing.T) {
	read := func(name string, version uint32, into interface{ Validate() error }) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", "adapter", "ibkr", "tests", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		validate := into.Validate
		if v, ok := into.(interface{ ValidateSchema(uint32) error }); ok {
			validate = func() error { return v.ValidateSchema(version) }
		}
		if err := validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The adapter writes the broker snapshot and the mark at version 2 and
	// the account snapshot Boost reads at version 3 (the owner's decision of
	// 2026-10-18; ADR 0026 point 1, amended).
	var snapshot event.BrokerSnapshotPayload
	read("broker_snapshot.json", event.BrokerSnapshotReadAtVersion, &snapshot)
	if !snapshot.MarketOpen || len(snapshot.OpenOrders) != 2 || snapshot.Positions[0].Quantity != 147 || snapshot.AsOfClose || snapshot.NotAsOfClose == "" {
		t.Errorf("broker snapshot %+v", snapshot)
	}
	var atClose event.BrokerSnapshotPayload
	read("broker_snapshot_close.json", event.BrokerSnapshotReadAtVersion, &atClose)
	if !atClose.AsOfClose || !atClose.ReadAt.After(atClose.AsOf) || atClose.MarketOpen {
		t.Errorf("broker snapshot as of the close %+v", atClose)
	}
	var account event.AccountPositionsSnapshotPayload
	read("account_snapshot.json", event.AccountSnapshotPositionsVersion, &account)
	if account.NetLiquidation != 8530 || account.Quantity("SPY") != 147 || !account.AsOf.Equal(atClose.AsOf) {
		t.Errorf("account snapshot %+v", account)
	}
	var mark event.MarkRecordedPayload
	read("mark.json", event.MarkRecordedEvidenceVersion, &mark)
	if mark.Price != 401.5 || !mark.BrokerLastSet || mark.BrokerLast != 401.17 {
		t.Errorf("mark %+v", mark)
	}
	var reconciliation event.ReconciliationPayload
	read("reconciliation.json", event.ReconciliationSchemaVersion, &reconciliation)
	if reconciliation.TotalCash != -4700 || reconciliation.Currency != "USD" {
		t.Errorf("reconciliation %+v", reconciliation)
	}
	// The Flex reader (adapter/ibkr/flex.py) writes each deposit or
	// withdrawal at version 2, with IBKR's transaction ID.
	var movement event.CashMovementPayload
	read("cash_movement.json", event.CashMovementTransferVersion, &movement)
	if movement.Amount != 2500 || movement.EquityBefore != 8530 || movement.TransactionID != "900000101" || movement.Currency != "USD" {
		t.Errorf("cash movement %+v", movement)
	}
	// A transfer in the first run's window, before any snapshot, takes the
	// next snapshot less itself, and says so.
	var approximated event.CashMovementPayload
	read("cash_movement_approximated.json", event.CashMovementTransferVersion, &approximated)
	if approximated.EquityBefore != 6530 || approximated.EquityBeforeBasis != event.EquityBeforeNextSnapshotLessAmount {
		t.Errorf("approximated cash movement %+v", approximated)
	}
	// The same reader writes each commission, fee and interest posting to
	// its costs ledger at version 1.
	var cost event.CostPayload
	read("cost.json", event.CostSchemaVersion, &cost)
	if cost.Kind != event.CostCommission || cost.Amount != -1 || cost.TransactionID != "900000106" || cost.Currency != "USD" {
		t.Errorf("cost %+v", cost)
	}
}
