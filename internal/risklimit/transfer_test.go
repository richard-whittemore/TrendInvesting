package risklimit_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// The transfer seam (ADR 0019, amended 2026-10-21): a deposit or withdrawal
// the Flex reader journalled explains the cash it moved, and a cash
// difference on a day the reader has not yet read is pending, not Degraded,
// until the reader has covered the day or two more sessions have closed.

// deposited records a journalled transfer of amount dollars, minutes after
// the fixture time, under the ledger ID of IBKR's transaction id.
func deposited(t *testing.T, g *risklimit.Gate, id string, amount float64, minutes int) {
	t.Helper()
	if err := g.ObserveCashMovement(event.FlexLedgerIDPrefix+id, event.CashMovementPayload{
		AsOf: at.Add(time.Duration(minutes) * time.Minute), Amount: amount, EquityBefore: 10000, Currency: "USD", TransactionID: id,
	}); err != nil {
		t.Fatal(err)
	}
}

// flexRead records the Flex reader's status: read, covering New York days
// through coveredTo; flexAside a reading set aside.
func flexRead(t *testing.T, g *risklimit.Gate, coveredTo string, checked time.Time) {
	t.Helper()
	if err := g.ObserveFlexStatus(event.FlexStatusPayload{AccountID: "joint", State: event.FlexStateRead, CheckedAt: checked, CoveredTo: coveredTo}); err != nil {
		t.Fatal(err)
	}
}

func flexAside(t *testing.T, g *risklimit.Gate, checked time.Time) {
	t.Helper()
	if err := g.ObserveFlexStatus(event.FlexStatusPayload{AccountID: "joint", State: event.FlexStateSetAside, CheckedAt: checked}); err != nil {
		t.Fatal(err)
	}
}

// day is a reconciliation n calendar days after the fixture's, as of the
// same time of day, IBKR showing cash and 10 SPY.
func day(n int, cash float64) event.ReconciliationPayload {
	p := morningOf(cash, 10)
	p.AsOf = p.AsOf.AddDate(0, 0, n)
	p.ReconciliationID = "morning:" + p.AsOf.Format(time.DateOnly)
	return p
}

// rebook sets the book the close snapshot of n days after the fixture gives: 10
// SPY and cash.
func rebook(g *risklimit.Gate, n int, cash float64) {
	p := account(10, 0)
	p.AsOf = p.AsOf.AddDate(0, 0, n).Add(6 * time.Hour) // 4 p.m. New York: the close
	g.SetBook(reconcile.CashBook("snapshot:"+p.AsOf.Format(time.DateOnly), event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: p, SettledCash: cash, TotalCash: cash,
	}))
}

func TestAJournalledDepositExplainsTheCashAndNothingIsDegraded(t *testing.T) {
	g := booked(t)
	deposited(t, g, "T1", 500, 30)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	if r.Result.Classification != event.ReconciliationExplained || r.State != nil || len(r.Settled) != 0 {
		t.Fatalf("got %s, state %+v: %s", r.Result.Classification, r.State, r.Result.Detail)
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("a journalled deposit leaves the account %s", g.State())
	}
	released(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0])
}

func TestADepositBeforeTheReaderHasRunIsPendingThenClean(t *testing.T) {
	g := booked(t)
	// The reader's last reading covered the day before: today's deposit is
	// in no statement yet.
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	if r.Result.Classification != event.ReconciliationPendingCashRead || r.Result.Action != event.ReconciliationActionAlert || r.State != nil {
		t.Fatalf("got %s/%s, state %+v", r.Result.Classification, r.Result.Action, r.State)
	}
	if !strings.Contains(r.Result.Detail, "20250304") || !strings.Contains(r.Result.Detail, "a cash increase") ||
		!strings.Contains(r.Result.Detail, "withholds no buy") || !strings.Contains(r.Result.Detail, "the account stays Normal") {
		t.Errorf("the detail says what the reader covered and what it means: %q", r.Result.Detail)
	}
	if g.State() != risklimit.StateNormal {
		t.Fatalf("a pending cash read leaves the account %s", g.State())
	}
	released(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0])

	// That evening the close snapshot holds the deposit, and the reader
	// records it; the next reconciliation finds the book matched and the
	// pending difference explained by the record.
	rebook(g, 0, 1500)
	deposited(t, g, "T1", 500, 30)
	flexRead(t, g, "20250304", at.Add(12*time.Hour))
	next := reconcileWith(t, g, "reconciliation:2", day(1, 1500))
	if next.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("the next reconciliation: %s: %s", next.Result.Classification, next.Result.Detail)
	}
	if len(next.Settled) != 1 {
		t.Fatalf("the pending difference is settled once, got %+v", next.Settled)
	}
	s := next.Settled[0]
	if err := s.Result.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Result.Classification != event.ReconciliationExplained || s.Result.ReconciliationID != "morning" || s.State != nil {
		t.Fatalf("settled: %s %s, state %+v", s.Result.Classification, s.Result.ReconciliationID, s.State)
	}
	if cash := s.Result.Components[0]; !slices.Contains(cash.Explanations, "ibkr-flex:T1") || cash.Residual != 0 {
		t.Errorf("the deposit explains it: %+v", cash)
	}
	if s.ResultID == next.ResultID || !strings.HasPrefix(s.ResultID, "reconciliation-result:joint:") {
		t.Errorf("the settled result has its own ID: %q", s.ResultID)
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("clean: %s", g.State())
	}
	// Settled once: a later reconciliation settles nothing more.
	if again := reconcileWith(t, g, "reconciliation:3", day(2, 1500)); len(again.Settled) != 0 {
		t.Errorf("settled again: %+v", again.Settled)
	}
}

func TestAPendingDifferenceTheReaderHasCoveredIsDegraded(t *testing.T) {
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	rebook(g, 0, 1500)
	// The reader has read 5 March and recorded no transfer: the difference
	// remains, and it is a difference.
	flexRead(t, g, "20250305", at.Add(36*time.Hour))
	next := reconcileWith(t, g, "reconciliation:2", day(2, 1500))
	if len(next.Settled) != 1 {
		t.Fatalf("want the pending difference decided, got %+v", next.Settled)
	}
	s := next.Settled[0]
	if err := s.Result.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Result.Classification != event.ReconciliationUnexplained || s.State == nil || s.State.State != event.EngineStateDegraded {
		t.Fatalf("got %s, state %+v", s.Result.Classification, s.State)
	}
	if !slices.Equal(s.State.ScopeCashComponents, []string{event.ReconciliationComponentCash}) || !strings.Contains(s.Result.Detail, "covered") {
		t.Errorf("Degraded on cash, saying the reader covered the day: %+v %q", s.State, s.Result.Detail)
	}
	if g.State() != risklimit.StateDegraded {
		t.Errorf("the account is %s", g.State())
	}
}

func TestAPendingCashReadNeverLastsMoreThanTwoSessions(t *testing.T) {
	// 5 March 2025 is a Wednesday. The reader is set aside every evening.
	g := booked(t)
	flexAside(t, g, at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	rebook(g, 0, 1500)
	for n, cal := range []int{1, 2} { // Thursday, Friday: one and two closes since
		if r := reconcileWith(t, g, "reconciliation:"+string(rune('2'+n)), day(cal, 1500)); len(r.Settled) != 0 {
			t.Fatalf("after %d session(s) the difference still pends, got %+v", n+1, r.Settled)
		}
	}
	if g.State() != risklimit.StateNormal {
		t.Fatalf("pending: %s", g.State())
	}
	// Monday: a third close since. It is decided, whatever the reader says.
	r := reconcileWith(t, g, "reconciliation:9", day(5, 1500))
	if len(r.Settled) != 1 || r.Settled[0].State == nil || r.Settled[0].State.State != event.EngineStateDegraded {
		t.Fatalf("want Degraded after more than two sessions, got %+v", r.Settled)
	}
	// The cap is formatted from the shared constant, never written out.
	if want := fmt.Sprintf("more than the %d sessions a difference may wait", event.PendingCashReadSessions); !strings.Contains(r.Settled[0].Result.Detail, want) {
		t.Errorf("the detail says why, with the cap from the constant: want %q in %q", want, r.Settled[0].Result.Detail)
	}
}

func TestAWithdrawalLargerThanTheCashIsDegradedAsTodayAndNamed(t *testing.T) {
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	// $2,000 journalled out of $1,000: IBKR would refuse it, and shows the
	// $1,000 still there.
	deposited(t, g, "W1", -2000, 30)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10))
	if r.Result.Classification != event.ReconciliationUnexplained || r.State == nil || r.State.State != event.EngineStateDegraded {
		t.Fatalf("got %s, state %+v", r.Result.Classification, r.State)
	}
	if !strings.Contains(r.Result.Detail, "ibkr-flex:W1") || !strings.Contains(r.State.Detail, "ibkr-flex:W1") {
		t.Errorf("the withdrawal is named: %q", r.Result.Detail)
	}
}

func TestWithNoFlexStatusACashDifferenceDegradesAsToday(t *testing.T) {
	g := booked(t)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	if r.Result.Classification != event.ReconciliationUnexplained || r.State == nil {
		t.Fatalf("got %s, state %+v", r.Result.Classification, r.State)
	}
}

func TestAHoldingThatDiffersIsNeverPending(t *testing.T) {
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 9))
	if r.Result.Classification != event.ReconciliationUnexplained || r.State == nil {
		t.Fatalf("got %s, state %+v", r.Result.Classification, r.State)
	}
}

func TestACashMovementIsOneFactUnderItsID(t *testing.T) {
	g := booked(t)
	deposited(t, g, "T1", 500, 30)
	deposited(t, g, "T1", 500, 30)
	if err := g.ObserveCashMovement("ibkr-flex:T1", event.CashMovementPayload{AsOf: at, Amount: 600, EquityBefore: 10000, Currency: "USD", TransactionID: "T1"}); err == nil {
		t.Errorf("a different movement under a recorded ID is refused")
	}
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10)); r.Result.Classification != event.ReconciliationExplained {
		t.Errorf("counted once: %s %s", r.Result.Classification, r.Result.Detail)
	}
	if err := g.ObserveFlexStatus(event.FlexStatusPayload{AccountID: "other", State: event.FlexStateRunning, CheckedAt: at}); err == nil {
		t.Errorf("another account's status is refused")
	}
}

func TestTransferInputsTheGateCannotUseAreRefused(t *testing.T) {
	g := booked(t)
	if err := g.ObserveCashMovement("ibkr-flex:T1", event.CashMovementPayload{AsOf: at, Amount: 1, EquityBefore: 1, Currency: "USD"}); err == nil {
		t.Errorf("a movement with no transaction ID is not version 2's")
	}
	if err := g.ObserveCashMovement("", event.CashMovementPayload{AsOf: at, Amount: 1, EquityBefore: 1, Currency: "USD", TransactionID: "T1"}); err == nil {
		t.Errorf("a movement needs its envelope ID")
	}
	if err := g.ObserveFlexStatus(event.FlexStatusPayload{AccountID: "joint", State: "asleep", CheckedAt: at}); err == nil {
		t.Errorf("a status the reader does not write is refused")
	}
	// An older status does not replace a newer one.
	flexRead(t, g, "20250305", at)
	flexAside(t, g, at.Add(-time.Hour))
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10)); r.Result.Classification != event.ReconciliationUnexplained {
		t.Errorf("the newer status covers the day: %s", r.Result.Classification)
	}
}

func TestAPendingDifferenceKeepsWhatItCountedAndWaitsForLaterReadings(t *testing.T) {
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	// $300 out is journalled; IBKR shows $500 out: $200 more is pending.
	deposited(t, g, "W1", -300, 20)
	r := reconcileWith(t, g, "reconciliation:1", morningOf(500, 10))
	if r.Result.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("got %s", r.Result.Classification)
	}
	// A reading taken before the pending one decides nothing about it.
	earlier := morningOf(700, 10)
	earlier.AsOf = at.Add(30 * time.Minute)
	if e := reconcileWith(t, g, "reconciliation:0", earlier); len(e.Settled) != 0 {
		t.Fatalf("an earlier reading settled %+v", e.Settled)
	}
	rebook(g, 0, 500)
	deposited(t, g, "W2", -200, 25)
	next := reconcileWith(t, g, "reconciliation:2", day(1, 500))
	if len(next.Settled) != 1 || next.Settled[0].Result.Classification != event.ReconciliationExplained {
		t.Fatalf("settled %+v", next.Settled)
	}
	s := next.Settled[0].Result
	if !slices.Equal(s.Components[0].Explanations, []string{"ibkr-flex:W1", "ibkr-flex:W2"}) || !strings.Contains(s.Detail, "-200.00") {
		t.Errorf("the withdrawal counted then and the one recorded since: %+v %q", s.Components[0], s.Detail)
	}
}

func TestAPendingDifferenceTheTransfersCannotBeSummedForIsDegraded(t *testing.T) {
	g := booked(t)
	flexAside(t, g, at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	rebook(g, 0, 1500)
	if err := g.ObserveCashMovement("ibkr-flex:T9", event.CashMovementPayload{AsOf: at.Add(10 * time.Minute), Amount: 1e30, EquityBefore: 1, Currency: "USD", TransactionID: "T9"}); err != nil {
		t.Fatal(err)
	}
	next := reconcileWith(t, g, "reconciliation:2", day(1, 1500))
	if len(next.Settled) != 1 || next.Settled[0].State == nil || !strings.Contains(next.Settled[0].Result.Detail, "cannot be summed") {
		t.Fatalf("settled %+v", next.Settled)
	}
}

func TestAPendingDifferenceTheCalendarCannotCountFromIsDegraded(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	book := account(10, 0)
	book.AsOf = time.Date(2030, time.December, 30, 15, 0, 0, 0, time.UTC)
	g.SetBook(reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: book, SettledCash: 1000, TotalCash: 1000}))
	flexAside(t, g, book.AsOf.Add(-time.Hour))
	obs := morningOf(1500, 10)
	obs.AsOf = book.AsOf.Add(time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", obs); r.Result.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("got %s", r.Result.Classification)
	}
	obs.AsOf, obs.ReconciliationID = time.Date(2031, time.January, 2, 15, 0, 0, 0, time.UTC), "morning:2"
	g.SetBook(reconcile.CashBook("snapshot:2", event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: func() event.AccountPositionsSnapshotPayload {
		b := book
		b.AsOf = obs.AsOf.Add(-time.Hour)
		return b
	}(), SettledCash: 1500, TotalCash: 1500}))
	next := reconcileWith(t, g, "reconciliation:2", obs)
	if len(next.Settled) != 1 || !strings.Contains(next.Settled[0].Result.Detail, "cannot be counted") {
		t.Fatalf("settled %+v", next.Settled)
	}
}

func TestABookTheCalendarCannotPlaceIsNeverPending(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	book := account(10, 0)
	book.AsOf = time.Date(2031, time.March, 4, 15, 0, 0, 0, time.UTC)
	g.SetBook(reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: book, SettledCash: 1000, TotalCash: 1000}))
	flexAside(t, g, book.AsOf.Add(-time.Hour))
	obs := morningOf(1500, 10)
	obs.AsOf = book.AsOf.Add(time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", obs); r.Result.Classification != event.ReconciliationUnexplained || r.State == nil {
		t.Fatalf("got %s", r.Result.Classification)
	}
}

func TestAPendingCashDecreaseWithholdsEveryBuyUntilTheReaderExplainsIt(t *testing.T) {
	// $500 less than the book, on a day the reader has not read: it may be
	// a withdrawal, so no plan is sized on cash that may not be there. Buys
	// are withheld on the whole account; sells go; the state is unchanged
	// (ADR 0019, amended 2026-10-21).
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	r := reconcileWith(t, g, "reconciliation:1", morningOf(500, 10))
	if r.Result.Classification != event.ReconciliationPendingCashRead || r.State != nil {
		t.Fatalf("got %s, state %+v", r.Result.Classification, r.State)
	}
	for _, want := range []string{"a cash decrease", "buys are withheld on the whole account", "sells and exits continue", "the account stays Normal"} {
		if !strings.Contains(r.Result.Detail, want) {
			t.Errorf("the detail says %q: %q", want, r.Result.Detail)
		}
	}
	buy := check(t, g, order(event.OrderSideBuy, 1, 0))[0]
	refusedFor(t, buy, event.RefusedPendingCashDecrease)
	if err := buy.Refused.ValidateSchema(buy.Refused.SchemaVersion()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buy.Refused.Detail, "reconciliation:1") {
		t.Errorf("the refusal names the pending difference: %+v", buy.Refused)
	}
	released(t, check(t, g, order(event.OrderSideSell, 1, 10))[0])
	if g.State() != risklimit.StateNormal {
		t.Fatalf("withheld buys are not a state: %s", g.State())
	}

	// The reader records the withdrawal: the difference is explained, and
	// buys go again.
	rebook(g, 0, 500)
	deposited(t, g, "W1", -500, 30)
	flexRead(t, g, "20250305", at.Add(12*time.Hour))
	next := reconcileWith(t, g, "reconciliation:2", day(1, 500))
	if len(next.Settled) != 1 || next.Settled[0].Result.Classification != event.ReconciliationExplained {
		t.Fatalf("settled %+v", next.Settled)
	}
	released(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0])
}

func TestAPendingCashDecreasePagesOncePerPendingDifference(t *testing.T) {
	// Every buy withheld is journalled, refused pending-cash-decrease, but
	// only the first refusal of the pending difference raises the urgent
	// alert, as the kill switch's first refusal of its engagement does
	// (ADR 0019, amended 2026-10-21): two decisions of two buys each page
	// once.
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(500, 10))
	pages := 0
	for decision := range 2 {
		for _, out := range check(t, g, order(event.OrderSideBuy, 1, 0), order(event.OrderSideBuy, 2, 0)) {
			refusedFor(t, out, event.RefusedPendingCashDecrease)
			if err := out.Refused.ValidateSchema(out.Refused.SchemaVersion()); err != nil {
				t.Fatalf("decision %d: %v", decision, err)
			}
			if out.Refused.Alert {
				pages++
			}
		}
	}
	if pages != 1 {
		t.Fatalf("one pending difference pages once, got %d pages", pages)
	}
	// Once it is decided, a new pending decrease is a new difference, and
	// pages again.
	rebook(g, 0, 500)
	deposited(t, g, "W1", -500, 30)
	flexRead(t, g, "20250305", at.Add(12*time.Hour))
	reconcileWith(t, g, "reconciliation:2", day(1, 500))
	reconcileWith(t, g, "reconciliation:3", day(1, 200))
	if out := check(t, g, order(event.OrderSideBuy, 1, 0))[0]; out.Refused == nil || !out.Refused.Alert {
		t.Errorf("a new pending decrease pages: %+v", out)
	}
}

func TestARepeatedReadingOfAPendingDifferenceIsTheSameDifference(t *testing.T) {
	// A reconnect reconciles again against the same book: the same cash
	// difference is one pending difference, not two, so its increase is
	// left out of the buy budget once, and the reading that repeats it is
	// decided with it (ADR 0019, amended 2026-10-21: one difference).
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	again := morningOf(1500, 10)
	again.AsOf, again.ReconciliationID = again.AsOf.Add(time.Hour), "reconnect"
	r := reconcileWith(t, g, "reconciliation:2", again)
	if r.Result.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("got %s", r.Result.Classification)
	}
	if !strings.Contains(r.Result.Detail, "the same difference reconciliation morning (reconciliation:1) found") {
		t.Errorf("the repeat names the reading that found it: %q", r.Result.Detail)
	}
	if got := g.PendingCashIncrease(); got != 500 {
		t.Fatalf("one difference of $500 pends, got %v", got)
	}
	if r.Result.RepeatOf != "morning" {
		t.Errorf("the repeat names the reading that found it: %q", r.Result.RepeatOf)
	}
	rebook(g, 0, 1500)
	deposited(t, g, "T1", 500, 30)
	flexRead(t, g, "20250305", at.Add(12*time.Hour))
	next := reconcileWith(t, g, "reconciliation:3", day(1, 1500))
	if len(next.Settled) != 2 {
		t.Fatalf("both readings are decided, got %+v", next.Settled)
	}
	for i, input := range []string{"reconciliation:1", "reconciliation:2"} {
		s := next.Settled[i]
		if err := s.Result.Validate(); err != nil {
			t.Fatal(err)
		}
		if s.Result.InputID != input || s.Result.Classification != event.ReconciliationExplained {
			t.Errorf("settled %d: %s %s", i, s.Result.InputID, s.Result.Classification)
		}
		if want := []string{"", "morning"}[i]; s.Result.RepeatOf != want {
			t.Errorf("settled %d repeats %q, want %q", i, s.Result.RepeatOf, want)
		}
	}
	if next.Settled[0].ResultID == next.Settled[1].ResultID {
		t.Errorf("each has its own ID: %s", next.Settled[0].ResultID)
	}
	if got := g.PendingCashIncrease(); got != 0 {
		t.Errorf("decided: %v pends", got)
	}
}

func TestAPendingDifferenceReadAgainAfterAFillIsTheSameDifference(t *testing.T) {
	// A difference's identity is its book, residual, currency and the
	// transfers it counted, not the fills: a reconnect after the day's fill
	// finds the same unexplained deposit, one pending difference whose
	// increase is left out of the buy budget once, and the repeat names the
	// reading that found it, so it does not page again (ADR 0019, amended
	// 2026-10-21: the same residual after the same transfers; one
	// difference, one reconciliation alert).
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
	first := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	if first.Result.Classification != event.ReconciliationPendingCashRead || first.Result.RepeatOf != "" {
		t.Fatalf("the first reading pends and repeats nothing: %s %q", first.Result.Classification, first.Result.RepeatOf)
	}
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5, FilledAt: at.Add(90 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	again := morningOf(1497.5, 12)
	again.AsOf, again.ReconciliationID = again.AsOf.Add(time.Hour), "reconnect"
	r := reconcileWith(t, g, "reconciliation:2", again)
	if r.Result.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("got %s: %s", r.Result.Classification, r.Result.Detail)
	}
	if r.Result.RepeatOf != "morning" {
		t.Errorf("the repeat names the reading that found the difference: %q", r.Result.RepeatOf)
	}
	if got := g.PendingCashIncrease(); got != 500 {
		t.Fatalf("one difference of $500 pends, got %v", got)
	}
}

func TestAPendingCashDecreaseIsDegradedOnCashAtTheTwoSessionCap(t *testing.T) {
	g := booked(t)
	flexAside(t, g, at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(500, 10))
	rebook(g, 0, 500)
	reconcileWith(t, g, "reconciliation:2", day(1, 500))
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedPendingCashDecrease)
	r := reconcileWith(t, g, "reconciliation:3", day(5, 500))
	if len(r.Settled) != 1 || r.Settled[0].State == nil || r.Settled[0].State.State != event.EngineStateDegraded {
		t.Fatalf("want Degraded on cash at the cap, got %+v", r.Settled)
	}
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
}

func TestThePendingDetailStatesTheAccountsActualState(t *testing.T) {
	// An account already Degraded on cash: a later pending cash read says
	// so, not that buys go.
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10)) // no reader: Degraded on cash
	rebook(g, 0, 1500)
	flexRead(t, g, "20250305", at.Add(12*time.Hour))
	r := reconcileWith(t, g, "reconciliation:2", day(1, 1800))
	if r.Result.Classification != event.ReconciliationPendingCashRead {
		t.Fatalf("got %s", r.Result.Classification)
	}
	if !strings.Contains(r.Result.Detail, "the account stays Degraded on cash") || strings.Contains(r.Result.Detail, "stays Normal") {
		t.Errorf("the detail states the account's state: %q", r.Result.Detail)
	}
}

func TestAPendingDifferenceIsNotExplainedByATransferInAnotherCurrency(t *testing.T) {
	g := booked(t)
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	rebook(g, 0, 1500)
	if err := g.ObserveCashMovement("ibkr-flex:E1", event.CashMovementPayload{
		AsOf: at.Add(30 * time.Minute), Amount: 500, EquityBefore: 10000, Currency: "EUR", TransactionID: "E1",
	}); err != nil {
		t.Fatal(err)
	}
	next := reconcileWith(t, g, "reconciliation:2", day(1, 1500))
	if len(next.Settled) != 1 || next.Settled[0].Result.Classification != event.ReconciliationUnexplained || next.Settled[0].State == nil {
		t.Fatalf("want Degraded on cash, got %+v", next.Settled)
	}
	if !strings.Contains(next.Settled[0].Result.Detail, "EUR") {
		t.Errorf("the detail names the currency: %q", next.Settled[0].Result.Detail)
	}
}

func TestThePendingDetailOfAHaltedAccountSaysItStaysHalted(t *testing.T) {
	g := booked(t)
	blind := morningOf(0, 0)
	blind.Positions, blind.TotalCash = nil, 0
	blind.Unreadable, blind.Currency = []string{event.ReconciliationComponentPositions, event.ReconciliationComponentCash}, ""
	if r := reconcileWith(t, g, "reconciliation:0", blind); r.State == nil || g.State() != risklimit.StateHalted {
		t.Fatalf("setup: want Halted, got %s", g.State())
	}
	flexRead(t, g, "20250304", at.Add(-12*time.Hour))
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1500, 10))
	if r.Result.Classification != event.ReconciliationPendingCashRead || !strings.Contains(r.Result.Detail, "the account stays Halted") {
		t.Errorf("got %s: %q", r.Result.Classification, r.Result.Detail)
	}
}
