package reconcile_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// The close-snapshot check: the holdings of the account snapshot about to
// become the book, against the book in force and every change journalled
// since it (ADR 0019, "Explain changes by causing events; never adopt a
// balance"; amended 2026-10-21).

// nextClose is the session's close after closeAt, when the next snapshot is
// taken.
var nextClose = closeAt.Add(24 * time.Hour)

// nextBook is the IRA's next account snapshot, at nextClose, with cash.
func nextBook(cash float64, positions ...event.AccountPosition) reconcile.Book {
	if positions == nil {
		positions = []event.AccountPosition{}
	}
	return reconcile.CashBook("snapshot:2", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{
			AccountID: "ira", AsOf: nextClose, NetLiquidation: 16000, Currency: "USD", Positions: positions,
		},
		SettledCash: cash, TotalCash: cash,
	})
}

func holdings(t *testing.T, book *reconcile.Book, changes []reconcile.Change, next reconcile.Book) event.ReconciliationResultPayload {
	t.Helper()
	r := reconcile.Holdings("ira", book, changes, next)
	if err := r.Validate(); err != nil {
		t.Fatalf("the check is not a valid result: %v\n%+v", err, r)
	}
	return r
}

func buy(id string, q int64, at time.Time) reconcile.Change {
	return reconcile.FillChange(event.TargetWeightFillPayload{
		FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: q, Price: 100, Commission: 1, FilledAt: at,
	})
}

func TestACloseSnapshotHoldingTheBookIsMatched(t *testing.T) {
	r := holdings(t, iraBook(), nil, nextBook(512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationMatched || r.Action != event.ReconciliationActionContinue {
		t.Fatalf("got %s/%s: %s", r.Classification, r.Action, r.Detail)
	}
	if r.ReconciliationID != reconcile.CloseSnapshotPrefix+"snapshot:2" || r.InputID != "snapshot:2" || r.AnchorID != "snapshot:1" {
		t.Errorf("identity: the check is named for the snapshot and starts from the book: %+v", r)
	}
	// The snapshot's cash is not compared: it moves with postings no event
	// records yet (interest, fees), which the adapter's own drift bound
	// watches (ADR 0019, amended 2026-10-18).
	if r.CashCompared || r.Currency != "" {
		t.Errorf("cash is not compared: %+v", r)
	}
}

func TestACloseSnapshotThatAFillExplainsIsExplained(t *testing.T) {
	r := holdings(t, iraBook(), []reconcile.Change{buy("fill:1", 10, closeAt.Add(18*time.Hour))}, nextBook(-500, held("SPY", 157), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained || r.Action != event.ReconciliationActionContinue {
		t.Fatalf("got %s/%s: %s", r.Classification, r.Action, r.Detail)
	}
	if c := component(t, r, "SPY"); c.Book != 147 || c.Expected != 157 || c.Observed != 157 || !slices.Equal(c.Explanations, []string{"fill:1"}) {
		t.Errorf("SPY: %+v", c)
	}
}

// TestAnIntraSessionTradeIsUnexplained: shares bought during the session
// that no journalled fill explains (a trade by hand, or a fill the engine
// was never given) are a difference in the close snapshot, never adopted
// silently: the account is degraded on the instrument.
func TestAnIntraSessionTradeIsUnexplained(t *testing.T) {
	for name, tc := range map[string]struct {
		changes []reconcile.Change
		spy     int64
	}{
		"a trade by hand":          {nil, 150},
		"a fill never journalled":  {[]reconcile.Change{buy("fill:1", 10, closeAt.Add(18*time.Hour))}, 167},
		"a journalled fill absent": {[]reconcile.Change{buy("fill:1", 10, closeAt.Add(18*time.Hour))}, 147},
	} {
		r := holdings(t, iraBook(), tc.changes, nextBook(512.3, held("SPY", tc.spy), held("TLT", 20)))
		if r.Classification != event.ReconciliationUnexplained || r.Action != event.ReconciliationActionDegrade {
			t.Fatalf("%s: got %s/%s", name, r.Classification, r.Action)
		}
		if !slices.Equal(r.ScopeInstrumentIDs, []string{"SPY"}) || len(r.ScopeCashComponents) != 0 {
			t.Errorf("%s: only SPY is in doubt: %v %v", name, r.ScopeInstrumentIDs, r.ScopeCashComponents)
		}
		if !strings.Contains(r.Detail, "close snapshot snapshot:2") || !strings.Contains(r.Detail, "SPY") {
			t.Errorf("%s: the detail names the snapshot and the instrument: %q", name, r.Detail)
		}
		// The owner reads every cause the check reports, a fill IBKR does
		// not show among them.
		for _, cause := range []string{"a trade the engine did not place", "a fill it was not given", "a journalled fill IBKR does not show"} {
			if !strings.Contains(r.Detail, cause) {
				t.Errorf("%s: the detail names %q: %q", name, cause, r.Detail)
			}
		}
	}
}

func TestAHoldingOnlyTheCloseSnapshotShowsIsNamed(t *testing.T) {
	r := holdings(t, iraBook(), nil, nextBook(512.3, held("IEF", 5), held("SPY", 147), held("TLT", 20)))
	if !slices.Equal(r.ScopeInstrumentIDs, []string{"IEF"}) {
		t.Fatalf("scope %v", r.ScopeInstrumentIDs)
	}
}

// TestAFillInTheCloseSnapshotsSecondIsNotInIt: a fill stamped in the new
// snapshot's second post-dates it, the convention every book follows, so it
// is the new book's change, not this one's.
func TestAFillInTheCloseSnapshotsSecondIsNotInIt(t *testing.T) {
	r := holdings(t, iraBook(), []reconcile.Change{buy("fill:1", 10, nextClose.Add(300*time.Millisecond))}, nextBook(512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationMatched {
		t.Fatalf("got %s: %s", r.Classification, r.Detail)
	}
}

func TestACorporateActionNoBookCanApplyMakesTheCheckUnverifiable(t *testing.T) {
	delisting := reconcile.CorporateActionChange("ca:1", event.CorporateActionPayload{InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: closeAt.Add(time.Hour)})
	r := holdings(t, iraBook(), []reconcile.Change{delisting}, nextBook(512.3, held("SPY", 147)))
	if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt {
		t.Fatalf("got %s/%s", r.Classification, r.Action)
	}
}

// TestACashFaultCannotHaltTheHoldingsCheck: the check compares no cash, so a
// book or a change whose cash is no cent count is not its concern.
func TestACashFaultCannotHaltTheHoldingsCheck(t *testing.T) {
	book := iraBook()
	book.Fault = "the book's total cash: too many cents"
	bad := buy("fill:1", 10, closeAt.Add(time.Hour))
	bad.CashFault = "fill fill:1: too many cents"
	r := holdings(t, book, []reconcile.Change{bad}, nextBook(512.3, held("SPY", 157), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained {
		t.Fatalf("got %s: %s", r.Classification, r.Detail)
	}
}
