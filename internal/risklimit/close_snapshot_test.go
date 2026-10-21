package risklimit_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
	"github.com/richard-whittemore/TrendInvesting/internal/risklimit"
)

// The close-snapshot seam: an account snapshot about to become the book is
// checked against the book in force and the fills journalled since it, and
// a holding they do not explain degrades the account (ADR 0019, amended
// 2026-10-21).

// closeSnapshot is the account snapshot "snapshot:2", taken minutes after
// the fixture time, holding spy SPY and $1,000.00.
func closeSnapshot(spy int64, minutes int) reconcile.Book {
	return reconcile.CashBook("snapshot:2", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: account(spy, minutes), SettledCash: 1000, TotalCash: 1000,
	})
}

func checkBookWith(t *testing.T, g *risklimit.Gate, b reconcile.Book) (risklimit.Reconciliation, bool) {
	t.Helper()
	r, journal := g.CheckBook(b)
	if !journal {
		return r, false
	}
	if err := r.Result.Validate(); err != nil {
		t.Fatalf("invalid result: %v", err)
	}
	if r.State != nil {
		if err := r.State.ValidateSchema(r.StateSchemaVersion); err != nil {
			t.Fatalf("invalid engine state: %v", err)
		}
	}
	return r, true
}

func TestACloseSnapshotHoldingNoFillExplainsDegradesItsInstrument(t *testing.T) {
	g := booked(t)
	r, journal := checkBookWith(t, g, closeSnapshot(12, 30))
	if !journal {
		t.Fatal("a difference is journalled, never adopted silently")
	}
	if r.Result.Classification != event.ReconciliationUnexplained || r.ResultID != "reconciliation-result:joint:snapshot:2" || r.Result.InputID != "snapshot:2" {
		t.Fatalf("result %s %s: %+v", r.ResultID, r.Result.Classification, r.Result)
	}
	s := r.State
	if s == nil || s.State != event.EngineStateDegraded || s.Reason != event.EngineStateReasonReconciliationUnexplained || !slices.Equal(s.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("want Degraded on SPY, got %+v", s)
	}
	if r.StateID != "engine-state:joint:snapshot:2" || g.Incident() != r.StateID || g.State() != risklimit.StateDegraded {
		t.Errorf("the incident is the snapshot's: %s, gate %s %q", r.StateID, g.State(), g.Incident())
	}
	if !strings.Contains(s.Detail, reconcile.CloseSnapshotPrefix+"snapshot:2") {
		t.Errorf("the state names the check: %q", s.Detail)
	}
	refusedFor(t, check(t, g, order(event.OrderSideBuy, 1, 0))[0], event.RefusedEngineDegraded)
}

func TestACloseSnapshotTheFillsExplainChangesNothing(t *testing.T) {
	g := booked(t)
	released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5, FilledAt: at.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 30)); journal {
		t.Fatalf("an explained snapshot records nothing: %+v", r)
	}
	if g.State() != risklimit.StateNormal {
		t.Errorf("state %s", g.State())
	}
}

func TestTheFirstBookHasNothingToBeCheckedAgainst(t *testing.T) {
	g := gate(t, limits(), event.TradingModeDryRun)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 30)); journal {
		t.Fatalf("the first book of a run is the adapter's to check against the previous close: %+v", r)
	}
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12)); r.Result.AnchorID != "snapshot:2" || r.Result.Classification != event.ReconciliationMatched {
		t.Errorf("the snapshot is the book: %+v", r.Result)
	}
}

// TestHoldingsAReconciliationAlreadyClassifiedAreNotClassifiedAgain: the
// IBKR adapter sends each reading's reconciliation ahead of its snapshot, so
// a difference is reported once, by the reconciliation, and the snapshot
// holding the same shares adds nothing.
func TestHoldingsAReconciliationAlreadyClassifiedAreNotClassifiedAgain(t *testing.T) {
	g := booked(t)
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12)); r.State == nil {
		t.Fatal("the reconciliation degrades")
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 60)); journal {
		t.Fatalf("the same holdings are not reported twice: %+v", r)
	}
	// A snapshot that differs from what the reconciliation saw is checked.
	g = booked(t)
	reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10))
	if r, journal := checkBookWith(t, g, closeSnapshot(13, 60)); !journal || r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("holdings other than the reconciliation's are checked: %v %+v", journal, r.Result)
	}
}

func TestTheCheckedSnapshotBecomesTheBook(t *testing.T) {
	g := booked(t)
	checkBookWith(t, g, closeSnapshot(12, 30))
	r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12))
	if r.Result.AnchorID != "snapshot:2" || r.Result.Classification != event.ReconciliationMatched || r.State != nil {
		t.Fatalf("the later reconciliation starts from the snapshot: %+v", r.Result)
	}
	// The earlier reconciliation's view is the old book's: a snapshot after
	// the new book is checked afresh.
	next := reconcile.CashBook("snapshot:3", event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: account(14, 120), SettledCash: 1000, TotalCash: 1000})
	if r, journal := checkBookWith(t, g, next); !journal || !slices.Equal(r.Result.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("snapshot:3: %v %+v", journal, r.Result)
	}
}

// TestAChangeAfterAReconciliationIsCheckedOnTheNextSnapshot: a fill or a
// corporate action observed after a reconciliation means its positions were
// not classified against the book and those changes, so a snapshot holding
// the same shares is checked afresh (ADR 0019, amended 2026-10-21, point 6).
func TestAChangeAfterAReconciliationIsCheckedOnTheNextSnapshot(t *testing.T) {
	g := booked(t)
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10)); r.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("the reconciliation matches: %+v", r.Result)
	}
	released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5, FilledAt: at.Add(70 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(10, 90))
	if !journal || r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("a journalled fill IBKR does not show is unexplained: %v %+v", journal, r.Result)
	}
	if r.State == nil || r.State.State != event.EngineStateDegraded || !slices.Equal(r.State.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("want Degraded on SPY, got %+v", r.State)
	}

	// A split observed after the reconciliation is checked the same way.
	s := booked(t)
	reconcileWith(t, s, "reconciliation:1", morningOf(1000, 10))
	split := event.CorporateActionPayload{InstrumentID: "SPY", Kind: event.CorporateActionKindSplit, EffectiveAt: at.Add(70 * time.Minute), NewShares: 2, OldShares: 1, EngineSharesPerRawShare: 1, Currency: "USD"}
	if err := s.ObserveCorporateAction("ca:split", split); err != nil {
		t.Fatal(err)
	}
	if r, journal := checkBookWith(t, s, closeSnapshot(10, 90)); !journal || r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("a split IBKR does not show is unexplained: %v %+v", journal, r.Result)
	}
}

// unrelatedSplit is a split of an instrument the account does not hold,
// observed an hour and ten minutes after the fixture time.
func unrelatedSplit(t *testing.T, g *risklimit.Gate) {
	t.Helper()
	split := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindSplit, EffectiveAt: at.Add(70 * time.Minute), NewShares: 2, OldShares: 1, EngineSharesPerRawShare: 1, Currency: "USD"}
	if err := g.ObserveCorporateAction("ca:tlt-split", split); err != nil {
		t.Fatal(err)
	}
}

// TestADifferenceAlreadyReportedIsNotReportedAgainAfterAnUnrelatedChange: a
// change to another instrument ends the reconciliation's standing for the
// next snapshot, but the snapshot's SPY difference is the one the
// reconciliation reported, so it is neither journalled nor paged twice
// (ADR 0019, amended 2026-10-21, point 6: one difference, one report).
func TestADifferenceAlreadyReportedIsNotReportedAgainAfterAnUnrelatedChange(t *testing.T) {
	g := booked(t)
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12)); r.State == nil {
		t.Fatal("the reconciliation degrades")
	}
	unrelatedSplit(t, g)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 90)); journal {
		t.Fatalf("the same SPY difference is not reported twice: %+v", r.Result)
	}
	if g.State() != risklimit.StateDegraded {
		t.Errorf("the account stays Degraded, got %s", g.State())
	}
}

// TestAChangedDifferenceIsReportedAfterAnUnrelatedChange: the same
// instrument with a different unexplained delta is a new difference.
func TestAChangedDifferenceIsReportedAfterAnUnrelatedChange(t *testing.T) {
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12))
	unrelatedSplit(t, g)
	r, journal := checkBookWith(t, g, closeSnapshot(13, 90))
	if !journal || r.Result.Classification != event.ReconciliationUnexplained || !slices.Equal(r.Result.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("a changed SPY difference is reported: %v %+v", journal, r.Result)
	}
}

// TestAnUnverifiableSnapshotIsReportedWhateverWasReportedBefore: a change no
// book can apply halts the account, a verdict a standing difference never
// hides (ADR 0019, amended 2026-10-21).
func TestAnUnverifiableSnapshotIsReportedWhateverWasReportedBefore(t *testing.T) {
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12))
	delisting := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(70 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:tlt-delisting", delisting); err != nil {
		t.Fatal(err)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(12, 90))
	if !journal || r.Result.Action != event.ReconciliationActionHalt || g.State() != risklimit.StateHalted {
		t.Fatalf("a delisting halts: %v %+v state %s", journal, r.Result, g.State())
	}
}
