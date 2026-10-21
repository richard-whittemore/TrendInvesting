package reconcile_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// transfer is a journalled deposit (amount > 0) or withdrawal of the Flex
// reader's ledger, dated at.
func transfer(id string, amount float64, at time.Time) reconcile.Change {
	return reconcile.CashMovementChange(event.FlexLedgerIDPrefix+id, event.CashMovementPayload{
		AsOf: at, Amount: amount, EquityBefore: 16000, Currency: "USD", TransactionID: id,
	})
}

// midnight is the start of 5 March in New York, the day after the book's
// close: the time IBKR gives a transfer it dates by day alone.
var midnight = time.Date(2025, time.March, 5, 5, 0, 0, 0, time.UTC)

func TestAJournalledDepositExplainsTheCash(t *testing.T) {
	r := classify(t, iraBook(), []reconcile.Change{transfer("T1", 1000, midnight)}, observed(1512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained || r.Action != event.ReconciliationActionContinue {
		t.Fatalf("got %s/%s: %s", r.Classification, r.Action, r.Detail)
	}
	if c := component(t, r, ""); c.Expected != 151230 || !slices.Equal(c.Explanations, []string{"ibkr-flex:T1"}) {
		t.Errorf("the deposit is applied to the cash: %+v", c)
	}
	// A transfer moves no holding: no position is listed for it.
	if len(r.Components) != 3 {
		t.Errorf("want cash, SPY and TLT, got %+v", r.Components)
	}
	for _, c := range r.Components[1:] {
		if len(c.Explanations) != 0 {
			t.Errorf("a transfer explains no holding: %+v", c)
		}
	}
}

func TestAJournalledWithdrawalIsListedBesideTheFillsWhenCashDiffers(t *testing.T) {
	// A $200 withdrawal journalled, and IBKR shows $10 less again: the
	// difference names the transfers beside the fills.
	r := classify(t, iraBook(), []reconcile.Change{transfer("T2", -200, midnight)}, observed(302.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationUnexplained {
		t.Fatalf("got %s", r.Classification)
	}
	if !strings.Contains(r.Detail, "deposits and withdrawals -200.00") {
		t.Errorf("the detail lists the transfers: %q", r.Detail)
	}
	if c := component(t, r, ""); c.Residual != -1000 || !slices.Equal(c.Explanations, []string{"ibkr-flex:T2"}) {
		t.Errorf("cash: %+v", c)
	}
	// With no transfer applied, the detail keeps its terms.
	plain := classify(t, iraBook(), nil, observed(302.3, held("SPY", 147), held("TLT", 20)))
	if strings.Contains(plain.Detail, "deposits and withdrawals") {
		t.Errorf("no transfer, no term: %q", plain.Detail)
	}
}

func TestATransferTheEveningOfAClosingBookIsInThatBook(t *testing.T) {
	// The book is the close of 4 March (4 p.m. New York), read after the
	// close, so it holds a transfer IBKR times that evening (ADR 0019, "the
	// reading after the close counts as the close"); one before the book is
	// in it too. Neither is applied again.
	evening := closeAt.Add(90 * time.Minute)
	before := closeAt.Add(-time.Hour)
	r := classify(t, iraBook(), []reconcile.Change{transfer("T3", 1000, evening), transfer("T4", 50, before)}, observed(512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationMatched {
		t.Fatalf("got %s: %s", r.Classification, r.Detail)
	}
	// A book taken at a read time, not a close, holds only what came before it.
	read := iraBook()
	read.AsOf = closeAt.Add(time.Hour)
	r = classify(t, read, []reconcile.Change{transfer("T3", 1000, read.AsOf.Add(time.Minute))}, observed(1512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained {
		t.Fatalf("a read-time book: got %s: %s", r.Classification, r.Detail)
	}
}

func TestAWithdrawalLargerThanTheCashOnHandIsNamedAndNeverPending(t *testing.T) {
	// IBKR would refuse a withdrawal of more than the account's cash, so one
	// journalled is a difference to look at, named, whatever the reader's
	// coverage (ADR 0019, amended 2026-10-21).
	o := reconcile.Reconcile(iraBook(), []reconcile.Change{transfer("T5", -1000, midnight)}, "reconciliation:1", observed(512.3, held("SPY", 147), held("TLT", 20)))
	if err := o.Result.Validate(); err != nil {
		t.Fatal(err)
	}
	if o.Result.Classification != event.ReconciliationUnexplained || !slices.Equal(o.Result.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
		t.Fatalf("got %s %v", o.Result.Classification, o.Result.ScopeCashComponents)
	}
	if !o.Overdrawn || !strings.Contains(o.Result.Detail, "ibkr-flex:T5") || !strings.Contains(o.Result.Detail, "more than the cash") {
		t.Errorf("the withdrawal is named: overdrawn %v, %q", o.Overdrawn, o.Result.Detail)
	}
	ok := reconcile.Reconcile(iraBook(), []reconcile.Change{transfer("T6", -500, midnight)}, "reconciliation:1", observed(12.3, held("SPY", 147), held("TLT", 20)))
	if ok.Overdrawn || ok.Result.Classification != event.ReconciliationExplained {
		t.Errorf("a withdrawal within the cash: %v %s", ok.Overdrawn, ok.Result.Classification)
	}
}

func TestTheCloseSnapshotCheckIgnoresTransfers(t *testing.T) {
	next := *iraBook()
	next.AnchorID, next.AsOf = "snapshot:2", closeAt.Add(24*time.Hour)
	r := reconcile.Holdings("ira", iraBook(), []reconcile.Change{transfer("T1", 1000, midnight)}, next)
	if r.Action != event.ReconciliationActionContinue || len(r.Components) != 2 {
		t.Fatalf("got %s with %+v", r.Action, r.Components)
	}
}

func TestTransfersInSumsWhatAWindowHolds(t *testing.T) {
	changes := []reconcile.Change{transfer("T1", 1000, midnight), transfer("T2", -200, midnight.Add(time.Hour)), transfer("T3", 5, morning)}
	sum, ids, err := reconcile.TransfersIn(changes, midnight, morning, nil, "USD")
	if err != nil || sum != 80000 || !slices.Equal(ids, []string{"ibkr-flex:T1", "ibkr-flex:T2"}) {
		t.Fatalf("got %d %v %v", sum, ids, err)
	}
	sum, ids, _ = reconcile.TransfersIn(changes, midnight, morning, map[string]bool{"ibkr-flex:T1": true}, "USD")
	if sum != -20000 || !slices.Equal(ids, []string{"ibkr-flex:T2"}) {
		t.Errorf("a transfer already counted is left out: %d %v", sum, ids)
	}
	from, err := reconcile.HeldUntil(*iraBook())
	if err != nil || !from.Equal(midnight) {
		t.Errorf("a close book holds its evening: %v %v", from, err)
	}
}

func TestATransferTheBookCannotCountIsUnverifiable(t *testing.T) {
	obs := observed(512.3, held("SPY", 147), held("TLT", 20))
	// More cents than a book can count, alone and summed.
	huge := transfer("T7", 1e30, midnight)
	if r := classify(t, iraBook(), []reconcile.Change{huge}, obs); r.Classification != event.ReconciliationUnverifiable || !strings.Contains(r.Detail, "cash movement ibkr-flex:T7") {
		t.Errorf("an amount with no cent count: %s %q", r.Classification, r.Detail)
	}
	if _, _, err := reconcile.TransfersIn([]reconcile.Change{huge}, midnight, morning, nil, "USD"); err == nil {
		t.Errorf("TransfersIn sums no amount without a cent count")
	}
	big := []reconcile.Change{transfer("T8", 9e16, midnight), transfer("T9", 9e16, midnight)}
	if r := classify(t, iraBook(), big, obs); r.Classification != event.ReconciliationUnverifiable || !strings.Contains(r.Detail, "ibkr-flex:T9: the expected cash") {
		t.Errorf("a sum past an int64: %s %q", r.Classification, r.Detail)
	}
	if _, _, err := reconcile.TransfersIn(big, midnight, morning, nil, "USD"); err == nil {
		t.Errorf("TransfersIn refuses a sum past an int64")
	}
}

func TestWhetherABookHoldsItsEveningComesFromTheCalendar(t *testing.T) {
	// A book on a Saturday is no close: it holds what came before it.
	saturday := *iraBook()
	saturday.AsOf = time.Date(2025, time.March, 8, 17, 0, 0, 0, time.UTC)
	if from, err := reconcile.HeldUntil(saturday); err != nil || !from.Equal(saturday.AsOf) {
		t.Errorf("Saturday: %v %v", from, err)
	}
	// A book past the calendar's last year cannot be placed: a transfer
	// against it is unverifiable, never guessed at.
	late := iraBook()
	late.AsOf = time.Date(2031, time.March, 4, 21, 0, 0, 0, time.UTC)
	if _, err := reconcile.HeldUntil(*late); err == nil {
		t.Fatalf("2031 is not in the calendar")
	}
	obs := observed(1512.3, held("SPY", 147), held("TLT", 20))
	obs.AsOf = late.AsOf.Add(time.Hour)
	if r := classify(t, late, []reconcile.Change{transfer("T1", 1000, late.AsOf.Add(time.Minute))}, obs); r.Classification != event.ReconciliationUnverifiable || !strings.Contains(r.Detail, "is a close is not known") {
		t.Errorf("got %s %q", r.Classification, r.Detail)
	}
}

func TestTheOverdrawnTestRunsInTimeOrderNotJournalOrder(t *testing.T) {
	// The reader writes same-day transfers in (as of, transaction ID) order
	// and may write an earlier one after a later one, so a withdrawal dated
	// by day alone can be journalled before the deposit that funded it. The
	// test is run in time order, IBKR's transaction ID breaking a tie: the
	// $2,000 deposit (T10) comes before the $1,000 withdrawal (T20), so
	// nothing was overdrawn, and the $12.30 still missing is an ordinary
	// difference (ADR 0019, amended 2026-10-21).
	changes := []reconcile.Change{transfer("T20", -1000, midnight), transfer("T10", 2000, midnight)}
	o := reconcile.Reconcile(iraBook(), changes, "reconciliation:1", observed(1500, held("SPY", 147), held("TLT", 20)))
	if o.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("got %s: %s", o.Result.Classification, o.Result.Detail)
	}
	if o.Overdrawn || strings.Contains(o.Result.Detail, "more than the cash") {
		t.Errorf("the deposit came first, so no withdrawal was overdrawn: %v %q", o.Overdrawn, o.Result.Detail)
	}
	// A withdrawal dated before the deposit is overdrawn, whatever the
	// journal order.
	late := []reconcile.Change{transfer("T10", 2000, midnight.Add(time.Hour)), transfer("T20", -1000, midnight)}
	if o := reconcile.Reconcile(iraBook(), late, "reconciliation:1", observed(1500, held("SPY", 147), held("TLT", 20))); !o.Overdrawn {
		t.Errorf("the withdrawal an hour before the deposit took more than the cash: %q", o.Result.Detail)
	}
}

func TestTransfersInRefusesATransferInAnotherCurrency(t *testing.T) {
	// A transfer in another currency cannot settle a difference in the
	// book's, as Classify finds it unverifiable (ADR 0019, "Explained
	// means").
	euro := reconcile.CashMovementChange(event.FlexLedgerIDPrefix+"T1", event.CashMovementPayload{
		AsOf: midnight, Amount: 1000, EquityBefore: 16000, Currency: "EUR", TransactionID: "T1",
	})
	if _, _, err := reconcile.TransfersIn([]reconcile.Change{euro}, midnight, morning, nil, "USD"); err == nil || !strings.Contains(err.Error(), "EUR") {
		t.Errorf("want a refusal naming the currency, got %v", err)
	}
	if sum, _, err := reconcile.TransfersIn([]reconcile.Change{transfer("T2", 5, midnight)}, midnight, morning, nil, "USD"); err != nil || sum != 500 {
		t.Errorf("a transfer in the book's currency is summed: %d %v", sum, err)
	}
}

func TestAnOverdrawnTestThatCannotRunInTimeOrderFindsNoOverdraft(t *testing.T) {
	// Each partial sum fits in journal order, but two deposits of 9e16
	// dollars side by side in time order do not: the time-order run says
	// nothing about a withdrawal then, rather than guess.
	changes := []reconcile.Change{
		transfer("T1", 9e16, midnight.Add(time.Hour)),
		transfer("T3", -9e16, midnight.Add(3*time.Hour)),
		transfer("T2", 9e16, midnight.Add(2*time.Hour)),
	}
	if o := reconcile.Reconcile(iraBook(), changes, "reconciliation:1", observed(512.3, held("SPY", 147), held("TLT", 20))); o.Overdrawn {
		t.Errorf("no withdrawal is judged overdrawn: %q", o.Result.Detail)
	}
}
