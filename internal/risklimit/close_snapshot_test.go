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

// TestALateReconciliationDoesNotStandForAFillDatedAfterItsReading: a
// reconciliation delivered after a fill but read before it applies only the
// changes dated before its reading, so it does not stand for a later
// snapshot that the fill should have changed (ADR 0019, amended 2026-10-21,
// point 6: a change dated at or after the reconciliation's reading ends its
// standing).
func TestALateReconciliationDoesNotStandForAFillDatedAfterItsReading(t *testing.T) {
	g := booked(t)
	released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5, FilledAt: at.Add(90 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10)); r.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("read before the fill, the reconciliation matches: %+v", r.Result)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(10, 180))
	if !journal || r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("a journalled fill IBKR does not show is unexplained: %v %+v", journal, r.Result)
	}
	if r.State == nil || r.State.State != event.EngineStateDegraded || !slices.Equal(r.State.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("want Degraded on SPY, got %+v", r.State)
	}
}

// TestACorporateActionEffectiveAfterAReconciliationHaltsTheSnapshot: a
// delisting journalled in advance, effective after the morning
// reconciliation's reading, is outside that reconciliation and inside the
// close snapshot's window, so the snapshot halts (ADR 0019, amended
// 2026-10-21: a Halt is never suppressed).
func TestACorporateActionEffectiveAfterAReconciliationHaltsTheSnapshot(t *testing.T) {
	g := booked(t)
	delisting := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(90 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:tlt-delisting", delisting); err != nil {
		t.Fatal(err)
	}
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 10)); r.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("read before the delisting, the reconciliation matches: %+v", r.Result)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(10, 180))
	if !journal || r.Result.Action != event.ReconciliationActionHalt || g.State() != risklimit.StateHalted {
		t.Fatalf("a delisting halts: %v %+v state %s", journal, r.Result, g.State())
	}
}

// TestADifferenceResolvedAndRecurringIsReportedAgain: a later
// reconciliation against the same book that finds an instrument matched
// ends its reported difference, so a new difference of the same size at the
// close is a new report (ADR 0019, amended 2026-10-21, point 6).
func TestADifferenceResolvedAndRecurringIsReportedAgain(t *testing.T) {
	g := booked(t)
	if r := reconcileWith(t, g, "reconciliation:1", morningOf(1000, 12)); r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("the reconciliation degrades: %+v", r.Result)
	}
	second := morningOf(1000, 10)
	second.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:2", second); r.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("the second reconciliation matches: %+v", r.Result)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(12, 180))
	if !journal || r.Result.Classification != event.ReconciliationUnexplained || !slices.Equal(r.Result.ScopeInstrumentIDs, []string{"SPY"}) {
		t.Fatalf("a recurring SPY difference is reported: %v %+v", journal, r.Result)
	}
}

// TestAReconciliationReadAfterTheSnapshotDoesNotStandForIt: a snapshot read
// before a reconciliation does not apply a fill dated between the two
// readings that the reconciliation applied, so the reconciliation's verdict
// does not stand for it (ADR 0019, amended 2026-10-21, point 6).
func TestAReconciliationReadAfterTheSnapshotDoesNotStandForIt(t *testing.T) {
	g := booked(t)
	released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
		Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5, FilledAt: at.Add(90 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	late := morningOf(997.5, 12)
	late.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", late); r.Result.Classification != event.ReconciliationExplained {
		t.Fatalf("read after the fill, the reconciliation is explained: %+v", r.Result)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(12, 60))
	if !journal || r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("a snapshot read before the fill does not show it: %v %+v", journal, r.Result)
	}
}

// TestAFillInAReadingsOwnSecondAgreesWithTheClassifier: the standing window
// is half-open on whole seconds exactly as the classifier applies a change
// (apply when !at.Before(book.AsOf) && at.Before(obs.AsOf)), so a fill
// stamped at the reconciliation's second is skipped by it and lies in the
// window, and one stamped at the snapshot's second is skipped by both
// and lies outside it (ADR 0019, amended 2026-10-21, point 6).
func TestAFillInAReadingsOwnSecondAgreesWithTheClassifier(t *testing.T) {
	cases := []struct {
		name                string
		reconcileMin        int
		snapshotMin         int
		fillMin             int
		reconciled          int64 // shares the reconciliation shows
		snapshot            int64 // shares the snapshot shows
		wantStandsAndSilent bool
	}{
		{"fill in the reconciliation's second, reconciliation first", 60, 180, 60, 10, 10, false},
		{"fill in the snapshot's second, reconciliation first", 60, 180, 180, 10, 10, true},
		{"fill in the reconciliation's second, snapshot first", 180, 60, 180, 10, 10, true},
		{"fill in the snapshot's second, snapshot first", 180, 60, 60, 12, 12, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := booked(t)
			released(t, check(t, g, order(event.OrderSideBuy, 2, 10))[0])
			if err := g.ObserveFill(event.TargetWeightFillPayload{
				AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "SPY",
				Side: event.OrderSideBuy, Quantity: 2, Price: 1, Commission: 0.5,
				FilledAt: at.Add(time.Duration(c.fillMin) * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			p := morningOf(1000, c.reconciled)
			p.AsOf = at.Add(time.Duration(c.reconcileMin) * time.Minute)
			reconcileWith(t, g, "reconciliation:1", p)
			snapshot := closeSnapshot(c.snapshot, c.snapshotMin)
			if got := g.ClassifiedStandsFor(snapshot); got != c.wantStandsAndSilent {
				t.Fatalf("the reconciliation stands for the snapshot: %v, want %v", got, c.wantStandsAndSilent)
			}
			r, journal := checkBookWith(t, g, snapshot)
			if journal == c.wantStandsAndSilent {
				t.Fatalf("journalled %v, want %v: %+v", journal, !c.wantStandsAndSilent, r.Result)
			}
			if journal && r.Result.Classification != event.ReconciliationUnexplained {
				t.Errorf("the snapshot is checked and unexplained: %+v", r.Result)
			}
		})
	}
}

// TestASnapshotIsNotHaltedAgainForTheChangeAReconciliationHaltedOn: the
// reconciliation read after a delisting halted the account; a snapshot with
// the same holdings and the same unapplicable change is the same Halt, so it
// is not journalled or paged a second time (ADR 0019, amended 2026-10-21,
// point 6: one difference, one report).
func TestASnapshotIsNotHaltedAgainForTheChangeAReconciliationHaltedOn(t *testing.T) {
	g := booked(t)
	delisting := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(70 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:tlt-delisting", delisting); err != nil {
		t.Fatal(err)
	}
	p := morningOf(1000, 10)
	p.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", p); r.Result.Action != event.ReconciliationActionHalt || g.State() != risklimit.StateHalted {
		t.Fatalf("the reconciliation halts: %+v state %s", r.Result, g.State())
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(10, 180)); journal {
		t.Fatalf("the same Halt is not journalled twice: %+v", r.Result)
	}
	// A change the first Halt did not name is a new Halt.
	other := event.CorporateActionPayload{InstrumentID: "IEF", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(210 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:ief-delisting", other); err != nil {
		t.Fatal(err)
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(10, 270)); !journal || r.Result.Action != event.ReconciliationActionHalt {
		t.Fatalf("a new unapplicable change is a new Halt: %v %+v", journal, r.Result)
	}
}

// TestAnEarlierReadReconciliationDoesNotReopenAReportedDifference: a
// reconciliation read before the one that reported a difference, delivered
// late, says nothing about whether it was resolved (ADR 0019, amended
// 2026-10-21, point 6).
func TestAnEarlierReadReconciliationDoesNotReopenAReportedDifference(t *testing.T) {
	g := booked(t)
	late := morningOf(1000, 12)
	late.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:2", late); r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("the later reading reports the difference: %+v", r.Result)
	}
	early := morningOf(1000, 10)
	if r := reconcileWith(t, g, "reconciliation:1", early); r.Result.Classification != event.ReconciliationMatched {
		t.Fatalf("the earlier reading matches: %+v", r.Result)
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 180)); journal {
		t.Fatalf("the earlier reading did not resolve the difference: %+v", r.Result)
	}
}

// TestASnapshotIsHaltedForANewChangeBesideARememberedOne: the snapshot's
// window holds the delisting a journalled Halt already gave and a new one, so
// not every reason was given and the Halt is journalled (ADR 0019, amended
// 2026-10-21, point 6: a Halt for a new change always is).
func TestASnapshotIsHaltedForANewChangeBesideARememberedOne(t *testing.T) {
	g := booked(t)
	remembered := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(70 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:tlt-delisting", remembered); err != nil {
		t.Fatal(err)
	}
	p := morningOf(1000, 10)
	p.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", p); r.Result.Action != event.ReconciliationActionHalt {
		t.Fatalf("the reconciliation halts: %+v", r.Result)
	}
	other := event.CorporateActionPayload{InstrumentID: "IEF", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(210 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:ief-delisting", other); err != nil {
		t.Fatal(err)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(10, 270))
	if !journal || r.Result.Action != event.ReconciliationActionHalt || len(r.Result.Unverifiable) < 2 {
		t.Fatalf("a new change beside a remembered one is a new Halt: %v %+v", journal, r.Result)
	}
}

// TestAHaltedSnapshotWithANewUnexplainedHoldingIsJournalled: a snapshot that
// repeats an earlier Halt's reason but also holds shares nothing explains is
// not the same Halt, so it is journalled and never adopted silently (ADR
// 0019, amended 2026-10-21, point 6).
func TestAHaltedSnapshotWithANewUnexplainedHoldingIsJournalled(t *testing.T) {
	g := booked(t)
	delisting := event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: at.Add(70 * time.Minute)}
	if err := g.ObserveCorporateAction("ca:tlt-delisting", delisting); err != nil {
		t.Fatal(err)
	}
	p := morningOf(1000, 10)
	p.AsOf = at.Add(2 * time.Hour)
	if r := reconcileWith(t, g, "reconciliation:1", p); r.Result.Action != event.ReconciliationActionHalt {
		t.Fatalf("the reconciliation halts: %+v", r.Result)
	}
	r, journal := checkBookWith(t, g, closeSnapshot(12, 180))
	if !journal || r.Result.Action != event.ReconciliationActionHalt {
		t.Fatalf("a new unexplained holding is journalled: %v %+v", journal, r.Result)
	}
}

// TestALateEarlierReadingDoesNotOverwriteAReportedResidual: a reconciliation
// read before the one that reported an instrument, delivered late, keeps the
// newer residual, so the close snapshot showing it is not paged again (ADR
// 0019, amended 2026-10-21, point 6).
func TestALateEarlierReadingDoesNotOverwriteAReportedResidual(t *testing.T) {
	g := booked(t)
	later := morningOf(1000, 12)
	later.AsOf = at.Add(2 * time.Hour)
	reconcileWith(t, g, "reconciliation:2", later)
	earlier := morningOf(1000, 11)
	if r := reconcileWith(t, g, "reconciliation:1", earlier); r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("the earlier reading differs: %+v", r.Result)
	}
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 180)); journal {
		t.Fatalf("the later reading's residual stands: %+v", r.Result)
	}
}

// resolvedThenDelivered: a reading at +1h reports SPY +2, a reading at +2h
// finds it explained, then a reading read at readAt, delivered after both,
// shows +2 again.
func resolvedThenDelivered(t *testing.T, readAt time.Duration) *risklimit.Gate {
	t.Helper()
	g := booked(t)
	reported := morningOf(1000, 12)
	reconcileWith(t, g, "reconciliation:1", reported)
	resolved := morningOf(1000, 10)
	resolved.AsOf = at.Add(2 * time.Hour)
	reconcileWith(t, g, "reconciliation:2", resolved)
	again := morningOf(1000, 12)
	again.AsOf = at.Add(readAt)
	reconcileWith(t, g, "reconciliation:3", again)
	// A last reading older than the third is older knowledge: it does not
	// replace the classified verdict, and the close snapshot is judged by what
	// was reported.
	old := morningOf(1000, 10)
	old.AsOf = at.Add(10 * time.Minute)
	reconcileWith(t, g, "reconciliation:4", old)
	return g
}

// TestAnEarlierReadingDeliveredAfterAResolutionRecordsNothing: the later
// reading found the instrument explained, so a reading read before it, though
// delivered afterwards, neither re-records the old residual nor hides the
// difference from the close snapshot (ADR 0019, amended 2026-10-21, point 6).
func TestAnEarlierReadingDeliveredAfterAResolutionRecordsNothing(t *testing.T) {
	g := resolvedThenDelivered(t, 90*time.Minute)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 180)); !journal {
		t.Fatalf("nothing was recorded, so the difference is reported: %+v", r.Result)
	}
}

// TestAReadingLaterThanAResolutionRecordsItsResidual: a difference that
// recurs after the resolution is a new difference, recorded once.
func TestAReadingLaterThanAResolutionRecordsItsResidual(t *testing.T) {
	g := resolvedThenDelivered(t, 3*time.Hour)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 240)); journal {
		t.Fatalf("the later reading's residual stands: %+v", r.Result)
	}
}

// TestADelayedEarlierReadingDoesNotReplaceTheLatestClassifiedVerdict: a book
// of 10 read as 12, then 10, then a delayed reading of 12 taken between them
// is older knowledge than the 10: it must not become the verdict a later
// snapshot of 12 is de-duplicated against (ADR 0019, amended 2026-10-21,
// point 6).
func TestADelayedEarlierReadingDoesNotReplaceTheLatestClassifiedVerdict(t *testing.T) {
	g := booked(t)
	first := morningOf(1000, 12)
	first.AsOf = at.Add(time.Hour)
	reconcileWith(t, g, "reconciliation:1", first)
	second := morningOf(1000, 10)
	second.AsOf = at.Add(2 * time.Hour)
	reconcileWith(t, g, "reconciliation:2", second)
	delayed := morningOf(1000, 12)
	delayed.AsOf = at.Add(90 * time.Minute)
	reconcileWith(t, g, "reconciliation:3", delayed)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 180)); !journal {
		t.Fatalf("the latest verdict was 10, so a snapshot of 12 is a new difference: %+v", r.Result)
	}
}

// TestADelayedEarlierReadingAfterAChangeDoesNotReplaceTheLatestVerdict: a
// fill that clears the classified verdict does not make an older reading,
// delivered afterwards, the latest one again (ADR 0019, amended 2026-10-21,
// point 6).
func TestADelayedEarlierReadingAfterAChangeDoesNotReplaceTheLatestVerdict(t *testing.T) {
	g := booked(t)
	o := order(event.OrderSideBuy, 1, 0)
	o.Planned.InstrumentID = "TLT"
	released(t, check(t, g, o)[0])
	first := morningOf(1000, 12)
	first.AsOf = at.Add(time.Hour)
	reconcileWith(t, g, "reconciliation:1", first)
	second := morningOf(1000, 10)
	second.AsOf = at.Add(2 * time.Hour)
	reconcileWith(t, g, "reconciliation:2", second)
	if err := g.ObserveFill(event.TargetWeightFillPayload{
		AccountID: "joint", ClientOrderID: "tw-buy", FillID: "exec-1", InstrumentID: "TLT",
		Side: event.OrderSideBuy, Quantity: 1, Price: 1, Commission: 0, FilledAt: at.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	delayed := morningOf(1000, 12)
	delayed.AsOf = at.Add(90 * time.Minute)
	reconcileWith(t, g, "reconciliation:3", delayed)
	if r, journal := checkBookWith(t, g, closeSnapshot(12, 240)); !journal {
		t.Fatalf("the latest verdict was 10, so a snapshot of 12 is a new difference: %+v", r.Result)
	}
}

// withIEF is the morning reading of 10 SPY, and 2 IEF when held is set.
func withIEF(minutes int, held bool) event.ReconciliationPayload {
	p := morningOf(1000, 10)
	p.AsOf = at.Add(time.Duration(minutes) * time.Minute)
	if held {
		p.Positions = append([]event.AccountPosition{{InstrumentID: "IEF", Quantity: 2}}, p.Positions...)
	}
	return p
}

func snapshotWithIEF(minutes int) reconcile.Book {
	s := account(10, minutes)
	s.Positions = append([]event.AccountPosition{{InstrumentID: "IEF", Quantity: 2}}, s.Positions...)
	return reconcile.CashBook("snapshot:2", event.AccountCashSnapshotPayload{AccountPositionsSnapshotPayload: s, SettledCash: 1000, TotalCash: 1000})
}

// TestAReadingOmittingAReportedHoldingForgetsItsResidual: IEF was never in
// the book; a reading reports 2, a later reading no longer shows it, so a
// snapshot holding 2 again is a new difference and is journalled (ADR 0019,
// amended 2026-10-21, point 6).
func TestAReadingOmittingAReportedHoldingForgetsItsResidual(t *testing.T) {
	g := booked(t)
	if r := reconcileWith(t, g, "reconciliation:1", withIEF(60, true)); r.Result.Classification != event.ReconciliationUnexplained {
		t.Fatalf("the first reading differs: %+v", r.Result)
	}
	reconcileWith(t, g, "reconciliation:2", withIEF(120, false))
	if r, journal := checkBookWith(t, g, snapshotWithIEF(180)); !journal {
		t.Fatalf("the holding recurred after the reading that omitted it: %+v", r.Result)
	}
}

// TestADelayedReadingDoesNotRestoreAForgottenResidual: a reading taken before
// the one that omitted the holding, delivered after it, records nothing.
func TestADelayedReadingDoesNotRestoreAForgottenResidual(t *testing.T) {
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", withIEF(60, true))
	reconcileWith(t, g, "reconciliation:2", withIEF(120, false))
	reconcileWith(t, g, "reconciliation:3", withIEF(90, true))
	if r, journal := checkBookWith(t, g, snapshotWithIEF(180)); !journal {
		t.Fatalf("the delayed reading recorded nothing, so the holding is reported: %+v", r.Result)
	}
}

// TestAnUnreadableReadingLeavesEveryResidualIntact: a reading whose positions
// could not be read names no instrument, which is not a statement that none
// is held, so the residual an earlier reading recorded stays and a snapshot
// holding the same difference is not reported twice (ADR 0019, amended
// 2026-10-21, point 6).
func TestAnUnreadableReadingLeavesEveryResidualIntact(t *testing.T) {
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", withIEF(60, true))
	unrelatedSplit(t, g) // ends the first reading's standing, so only the recorded residual can suppress the report
	blind := withIEF(120, false)
	blind.Positions, blind.Unreadable = []event.AccountPosition{}, []string{event.ReconciliationComponentPositions}
	reconcileWith(t, g, "reconciliation:2", blind)
	if r, journal := checkBookWith(t, g, snapshotWithIEF(180)); journal {
		t.Fatalf("the unreadable reading forgot nothing, so the difference is already reported: %+v", r.Result)
	}
}

// TestAReadingBeforeTheBookLeavesEveryResidualIntact: a reading dated before
// the book was taken is not a comparison with it, so its omission of an
// instrument forgets nothing (ADR 0019, amended 2026-10-21, point 6).
func TestAReadingBeforeTheBookLeavesEveryResidualIntact(t *testing.T) {
	g := booked(t)
	reconcileWith(t, g, "reconciliation:1", withIEF(-60, true))
	reconcileWith(t, g, "reconciliation:2", withIEF(-30, false))
	if r, journal := checkBookWith(t, g, snapshotWithIEF(180)); journal {
		t.Fatalf("the earlier reading forgot nothing, so the difference is already reported: %+v", r.Result)
	}
}
