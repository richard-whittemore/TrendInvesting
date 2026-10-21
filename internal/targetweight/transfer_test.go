package targetweight_test

import (
	"math"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
	"github.com/richard-whittemore/TrendInvesting/internal/report"
	"github.com/richard-whittemore/TrendInvesting/internal/targetweight"
)

// The transfer seam at the handler (ADR 0019, amended 2026-10-21): the Flex
// reader's journalled deposits and withdrawals, account.cash-movement
// version 2, and its status, account.flex-status, are inputs; a deposit
// they explain degrades nothing, and one they have not yet read is a
// pending cash read until they do.

// bookedIRA is a momentum handler whose book is the close snapshot of 2
// January 2025: 10 SPY and $1,000.00.
func bookedIRA(t *testing.T) (*targetweight.Handler, *stream, time.Time) {
	t.Helper()
	h, s := momentumHandler(t)
	at := closeOf("2025-01-02")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:0", at, event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: iraAccount, AsOf: at, NetLiquidation: 30000, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 10}}},
		SettledCash: 1000, TotalCash: 1000,
	}))
	return h, s, at
}

// newYorkDay is the start of a New York day, the time IBKR gives a transfer
// it dates by day alone.
func newYorkDay(date string) time.Time {
	d, err := time.ParseInLocation(time.DateOnly, date, nysecalendar.NewYork)
	if err != nil {
		panic(err)
	}
	return d
}

func movementInput(t *testing.T, s *stream, id string, amount float64, at time.Time) event.Envelope {
	return s.envelope(t, event.CashMovementEventType, event.CashMovementTransferVersion, event.FlexLedgerIDPrefix+id, at,
		event.CashMovementPayload{AsOf: at, Amount: amount, EquityBefore: 30000, Currency: "USD", TransactionID: id})
}

func flexStatusInput(t *testing.T, s *stream, state, coveredTo string, checked time.Time) event.Envelope {
	return s.envelope(t, event.FlexStatusEventType, event.FlexStatusSchemaVersion, "ibkr-flex-status:"+checked.Format(time.RFC3339), checked,
		event.FlexStatusPayload{AccountID: iraAccount, State: state, CheckedAt: checked, CoveredTo: coveredTo})
}

func TestAJournalledDepositReconcilesWithNoIncident(t *testing.T) {
	h, s, at := bookedIRA(t)
	if out := mustApply(t, h, movementInput(t, s, "T1", 500, newYorkDay("2025-01-03"))); out != nil {
		t.Fatalf("a cash movement decides nothing: %+v", out)
	}
	obs := observation(iraAccount, 10, 1500)
	obs.AsOf = at.Add(12 * time.Hour)
	out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", obs))
	if len(out) != 1 {
		t.Fatalf("want the result alone, got %+v", out)
	}
	var r event.ReconciliationResultPayload
	decode(t, out[0], &r)
	if r.Classification != event.ReconciliationExplained || r.Components[0].Explanations[0] != "ibkr-flex:T1" {
		t.Fatalf("got %s: %+v", r.Classification, r.Components)
	}
}

func TestADepositTheReaderHasNotReadIsPendingThenExplained(t *testing.T) {
	h, s, at := bookedIRA(t)
	mustApply(t, h, flexStatusInput(t, s, event.FlexStateRead, "20250102", at.Add(30*time.Minute)))
	// The deposit lands on 3 January; the reconciliation that evening finds
	// $500 more than the book.
	obs := observation(iraAccount, 10, 1500)
	obs.AsOf = newYorkDay("2025-01-03").Add(17 * time.Hour)
	out := mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", obs))
	if len(out) != 1 {
		t.Fatalf("a pending cash read moves no state: %+v", out)
	}
	var pending event.ReconciliationResultPayload
	decode(t, out[0], &pending)
	if pending.Classification != event.ReconciliationPendingCashRead || pending.Action != event.ReconciliationActionAlert {
		t.Fatalf("got %s/%s", pending.Classification, pending.Action)
	}
	// That evening's close snapshot holds it, and the reader records it.
	next := closeOf("2025-01-03")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:1", next, event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: iraAccount, AsOf: next, NetLiquidation: 30500, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 10}}},
		SettledCash: 1500, TotalCash: 1500,
	}))
	mustApply(t, h, movementInput(t, s, "T1", 500, newYorkDay("2025-01-03")))
	mustApply(t, h, flexStatusInput(t, s, event.FlexStateRead, "20250102", next.Add(5*time.Hour)))
	obs.AsOf, obs.ReconciliationID = newYorkDay("2025-01-06").Add(17*time.Hour), "morning:2"
	out = mustApply(t, h, reconciliationInput(t, s, "reconciliation:2", obs))
	if len(out) != 2 {
		t.Fatalf("want the settled difference and the result, got %+v", out)
	}
	var settled, result event.ReconciliationResultPayload
	decode(t, out[0], &settled)
	decode(t, out[1], &result)
	if settled.Classification != event.ReconciliationExplained || settled.ReconciliationID != "morning:1" || result.Classification != event.ReconciliationMatched {
		t.Fatalf("settled %s %s, result %s", settled.Classification, settled.ReconciliationID, result.Classification)
	}
	if out[0].ID != "reconciliation-result:"+iraAccount+":reconciliation:2:pending-cash:reconciliation:1" {
		t.Errorf("the settled result's ID: %s", out[0].ID)
	}
}

func TestTransferInputsAreChecked(t *testing.T) {
	h, s, at := bookedIRA(t)
	v1 := s.envelope(t, event.CashMovementEventType, event.CashMovementSchemaVersion, "m", at, event.CashMovementPayload{AsOf: at, Amount: 1, EquityBefore: 1, Currency: "USD"})
	mustRefuse(t, h, v1, "schema version")
	other := flexStatusInput(t, s, event.FlexStateRunning, "", at)
	other = s.envelope(t, event.FlexStatusEventType, event.FlexStatusSchemaVersion, other.ID, at, event.FlexStatusPayload{AccountID: "joint", State: event.FlexStateRunning, CheckedAt: at})
	mustRefuse(t, h, other, "account")
	mustRefuse(t, h, s.envelope(t, event.FlexStatusEventType, event.FlexStatusSchemaVersion, "ibkr-flex-status:x", at,
		event.FlexStatusPayload{AccountID: iraAccount, State: "asleep", CheckedAt: at}), "state")
	mustApply(t, h, movementInput(t, s, "T1", 500, at))
	mustRefuse(t, h, movementInput(t, s, "T1", 600, at), "reused")
	// Boost reads both too: its book states no cash, so neither changes a
	// result.
	b, bs := bookedBoost(t)
	if out := mustApply(t, b, bs.envelope(t, event.CashMovementEventType, event.CashMovementTransferVersion, "ibkr-flex:T1", sessionEnd(0),
		event.CashMovementPayload{AsOf: sessionEnd(0), Amount: 5, EquityBefore: 8530, Currency: "USD", TransactionID: "T1"})); out != nil {
		t.Errorf("Boost: %+v", out)
	}
	if out := mustApply(t, b, bs.envelope(t, event.FlexStatusEventType, event.FlexStatusSchemaVersion, "ibkr-flex-status:1", sessionEnd(0),
		event.FlexStatusPayload{AccountID: "joint", State: event.FlexStateRunning, CheckedAt: sessionEnd(0)})); out != nil {
		t.Errorf("Boost: %+v", out)
	}
}

// TestAPendingCashIncreaseIsNotInvestedUntilTheReaderExplainsIt: while a
// cash increase is pending a reading of IBKR's statement, the buys are
// sized from the book's cash, the broker's less the unexplained increase,
// so a credit that is reversed or posted in error is never invested; once
// the reader's record explains it, the next decision invests it (ADR 0019,
// amended 2026-10-21).
func TestAPendingCashIncreaseIsNotInvestedUntilTheReaderExplainsIt(t *testing.T) {
	const deposited = 2500
	base := released(t, simulateMomentum(t, momentumOptions{}).decisions)[4:]
	pending := simulateMomentum(t, momentumOptions{deposit: map[string]float64{"2025-02-28": deposited}})
	results := ofType(pending.decisions, event.ReconciliationResultEventType)
	if len(results) != 1 {
		t.Fatalf("want the pending result alone, got %d results", len(results))
	}
	var r event.ReconciliationResultPayload
	decode(t, results[0], &r)
	if r.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("the deposit the reader has not read: %s: %s", r.Classification, r.Detail)
	}
	buys := released(t, pending.decisions)[4:]
	if len(base) == 0 || len(buys) == 0 {
		t.Fatalf("%d and %d buys", len(base), len(buys))
	}
	if got, want := buys[0].Figures.CashAvailable, base[0].Figures.CashAvailable; math.Abs(got-want) > 1e-6 {
		t.Errorf("pending: the first buy is checked against %v, want %v, the budget without the unexplained %v", got, want, float64(deposited))
	}
	explained := simulateMomentum(t, momentumOptions{deposit: map[string]float64{"2025-02-28": deposited}, depositRecorded: true})
	var settled bool
	for _, e := range ofType(explained.decisions, event.ReconciliationResultEventType) {
		var p event.ReconciliationResultPayload
		decode(t, e, &p)
		settled = settled || p.InputID == "reconciliation:2025-02-28" && p.Classification == event.ReconciliationExplained
	}
	if !settled {
		t.Fatal("the reader's record explains the pending difference")
	}
	buys = released(t, explained.decisions)[4:]
	if got, want := buys[0].Figures.CashAvailable, base[0].Figures.CashAvailable+deposited; math.Abs(got-want) > 1e-6 {
		t.Errorf("explained: the first buy is checked against %v, want %v, the budget with the deposit", got, want)
	}
}

// TestAPendingDifferenceReadTwicePagesOnce: a reconnect's reading of the
// same pending difference is journalled, but the reporter pages once for
// the difference, and once for its decision (ADR 0019, amended 2026-10-21:
// one difference, one reconciliation alert).
func TestAPendingDifferenceReadTwicePagesOnce(t *testing.T) {
	h, s, at := bookedIRA(t)
	mustApply(t, h, flexStatusInput(t, s, event.FlexStateRead, "20250102", at.Add(30*time.Minute)))
	// The reporter reads the account from the run's configuration.
	journal := []event.Envelope{s.envelope(t, event.TargetWeightConfigurationEventType, event.TargetWeightConfigurationSchemaVersion, "configuration", at, momentumConfiguration())}
	obs := observation(iraAccount, 10, 1500)
	obs.AsOf = newYorkDay("2025-01-03").Add(15 * time.Hour)
	journal = append(journal, mustApply(t, h, reconciliationInput(t, s, "reconciliation:1", obs))...)
	obs.AsOf, obs.ReconciliationID = obs.AsOf.Add(time.Hour), "reconnect:1"
	journal = append(journal, mustApply(t, h, reconciliationInput(t, s, "reconciliation:2", obs))...)
	if got := len(ofType(journal, event.ReconciliationResultEventType)); got != 2 {
		t.Fatalf("each reading is journalled: %d results", got)
	}
	pages := func() int {
		alerts, err := report.Alerts(journal)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range alerts {
			if a.Kind == report.KindReconciliation {
				n++
			}
		}
		return n
	}
	if got := pages(); got != 1 {
		t.Fatalf("two readings of one difference page %d times, want once", got)
	}
	// That evening's close snapshot holds the cash, and the reader covers
	// the day without recording a transfer: both readings are decided
	// unexplained, and the decision pages once.
	next := closeOf("2025-01-03")
	mustApply(t, h, s.envelope(t, event.AccountSnapshotEventType, event.AccountSnapshotCashVersion, "snapshot:1", next, event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: iraAccount, AsOf: next, NetLiquidation: 30500, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 10}}},
		SettledCash: 1500, TotalCash: 1500,
	}))
	mustApply(t, h, flexStatusInput(t, s, event.FlexStateRead, "20250103", newYorkDay("2025-01-03").Add(21*time.Hour+30*time.Minute)))
	obs.AsOf, obs.ReconciliationID = newYorkDay("2025-01-06").Add(9*time.Hour), "morning:2"
	decided := mustApply(t, h, reconciliationInput(t, s, "reconciliation:3", obs))
	if got := len(ofType(decided, event.ReconciliationResultEventType)); got != 3 {
		t.Fatalf("want both readings decided and the new reading's result, got %d results", got)
	}
	journal = append(journal, decided...)
	if got := pages(); got != 2 {
		t.Errorf("the difference and its decision page %d times, want twice", got)
	}
}
