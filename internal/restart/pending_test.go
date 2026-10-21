package restart_test

import (
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/restart"
)

// A fresh run starts with no pending cash difference, so one the approved
// log leaves undecided would be forgotten: the approval is refused unless it
// states the starting book the owner checked in IBKR after the transfer,
// which supersedes it (ADR 0019, amended 2026-10-21).

// resultEntry is a journalled reconciliation result of input, classified c.
func resultEntry(t *testing.T, id, input, c string) journal.Entry {
	action := map[string]string{
		event.ReconciliationPendingCashRead: event.ReconciliationActionAlert,
		event.ReconciliationExplained:       event.ReconciliationActionContinue,
		event.ReconciliationUnexplained:     event.ReconciliationActionDegrade,
	}[c]
	return recorded(t, journal.KindDecision, id, event.ReconciliationResultEventType, event.ReconciliationResultSchemaVersion, event.ReconciliationResultPayload{
		AccountID: "joint", ReconciliationID: "morning:2026-10-20", InputID: input, Classification: c, Action: action, CashCompared: true, Currency: "USD",
		Components: []event.ReconciliationComponent{{Component: event.ReconciliationComponentCash, Book: 100000, Expected: 100000, Observed: 50000, Residual: -50000}},
		Detail:     c + ": x",
	})
}

func freshRequest() restart.ApprovalRequest {
	return restart.ApprovalRequest{
		AccountID: "joint", PreviousLog: "run-1.log", PreviousLogFinalHash: "sha256:" + strings.Repeat("a", 64),
		ApprovedBy: "owner", ApprovedAt: time.Date(2026, time.October, 21, 15, 0, 0, 0, time.UTC),
	}
}

func TestAFreshRunIsRefusedWhileACashDifferenceIsPendingUnlessItStatesTheBook(t *testing.T) {
	pending := []journal.Entry{resultEntry(t, "reconciliation-result:joint:reconciliation:7", "reconciliation:7", event.ReconciliationPendingCashRead)}
	end, err := restart.ReadEndState(pending, "joint", "run-1.log")
	if err != nil {
		t.Fatal(err)
	}
	if len(end.PendingCash) != 1 || end.PendingCash[0].InputID != "reconciliation:7" {
		t.Fatalf("the pending difference is read: %+v", end.PendingCash)
	}
	_, err = restart.ProposeApproval(end, freshRequest())
	if err == nil || !strings.Contains(err.Error(), "reconciliation:7") || !strings.Contains(err.Error(), "-500.00 USD") || !strings.Contains(err.Error(), "-positions") {
		t.Fatalf("want a refusal naming the pending difference and the way out, got %v", err)
	}
	withBook := freshRequest()
	withBook.StartingBook = true
	if _, err := restart.ProposeApproval(end, withBook); err != nil {
		t.Errorf("a starting book supersedes the pending difference: %v", err)
	}

	// The engine refuses such an approval too, whoever wrote it.
	approval := event.OperatorRunApprovedPayload{AccountID: "joint", PreviousLog: "run-1.log", Boost: event.BoostStateInForce{Latched: new(false)}}
	if err := restart.CheckPendingCash(pending, approval, "run-1.log"); err == nil || !strings.Contains(err.Error(), "reconciliation:7") {
		t.Errorf("the engine refuses an approval without a book: %v", err)
	}
	approval.StartingBook = &event.StartingBook{Positions: []event.AccountPosition{}, TotalCash: 500, Currency: "USD"}
	if err := restart.CheckPendingCash(pending, approval, "run-1.log"); err != nil {
		t.Errorf("an approval with a starting book: %v", err)
	}
}

func TestADecidedPendingCashDifferenceIsNoBarToAFreshRun(t *testing.T) {
	for _, settled := range []string{event.ReconciliationExplained, event.ReconciliationUnexplained} {
		entries := []journal.Entry{
			resultEntry(t, "reconciliation-result:joint:reconciliation:7", "reconciliation:7", event.ReconciliationPendingCashRead),
			resultEntry(t, "reconciliation-result:joint:reconciliation:8:pending-cash:reconciliation:7", "reconciliation:7", settled),
		}
		end, err := restart.ReadEndState(entries, "joint", "run-1.log")
		if err != nil || len(end.PendingCash) != 0 {
			t.Fatalf("%s: %+v %v", settled, end.PendingCash, err)
		}
		approval := event.OperatorRunApprovedPayload{AccountID: "joint"}
		if err := restart.CheckPendingCash(entries, approval, "run-1.log"); err != nil {
			t.Errorf("%s: %v", settled, err)
		}
	}
	bad := recorded(t, journal.KindDecision, "reconciliation-result:joint:x", event.ReconciliationResultEventType, 1, "not a result")
	if _, err := restart.ReadEndState([]journal.Entry{bad}, "joint", "run-1.log"); err == nil || !strings.Contains(err.Error(), "reconciliation-result:joint:x") {
		t.Errorf("an undecodable result is refused, named: %v", err)
	}
}
