package reconcile_test

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// close is the previous session's close, when the book was taken; morning is
// the next morning's reconciliation, before the open.
var (
	closeAt = time.Date(2025, time.March, 4, 21, 0, 0, 0, time.UTC)
	morning = time.Date(2025, time.March, 5, 13, 0, 0, 0, time.UTC)
)

// iraBook is an IRA's account snapshot at the close: 147 SPY and 20 TLT,
// $512.30 of cash.
func iraBook() *reconcile.Book {
	b := reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{
			AccountID: "ira", AsOf: closeAt, NetLiquidation: 16000, Currency: "USD",
			Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 147}, {InstrumentID: "TLT", Quantity: 20}},
		},
		SettledCash: 512.3, TotalCash: 512.3,
	})
	return &b
}

// observed is IBKR's morning view: positions as given and cash in dollars.
func observed(cash float64, positions ...event.AccountPosition) event.ReconciliationPayload {
	if positions == nil {
		positions = []event.AccountPosition{}
	}
	return event.ReconciliationPayload{
		AccountID: "ira", ReconciliationID: "morning:2025-03-05", Trigger: event.ReconciliationTriggerMorning,
		AsOf: morning, Unreadable: []string{}, Positions: positions, TotalCash: cash, Currency: "USD",
	}
}

func held(id string, q int64) event.AccountPosition {
	return event.AccountPosition{InstrumentID: id, Quantity: q}
}

func classify(t *testing.T, book *reconcile.Book, changes []reconcile.Change, obs event.ReconciliationPayload) event.ReconciliationResultPayload {
	t.Helper()
	r := reconcile.Classify(book, changes, "reconciliation:1", obs)
	if err := r.Validate(); err != nil {
		t.Fatalf("the classification is not a valid result: %v\n%+v", err, r)
	}
	return r
}

func component(t *testing.T, r event.ReconciliationResultPayload, instrument string) event.ReconciliationComponent {
	t.Helper()
	for _, c := range r.Components {
		if c.InstrumentID == instrument && (instrument != "" || c.Component == event.ReconciliationComponentCash) {
			return c
		}
	}
	t.Fatalf("no component for %q in %+v", instrument, r.Components)
	return event.ReconciliationComponent{}
}

func TestAMatchingObservationIsMatchedAndContinues(t *testing.T) {
	r := classify(t, iraBook(), nil, observed(512.3, held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationMatched || r.Action != event.ReconciliationActionContinue {
		t.Fatalf("got %s/%s", r.Classification, r.Action)
	}
	if r.AnchorID != "snapshot:1" || r.InputID != "reconciliation:1" || !r.CashCompared || r.Currency != "USD" {
		t.Errorf("identity: %+v", r)
	}
	// Every component is listed, unchanged ones too.
	if len(r.Components) != 3 {
		t.Fatalf("want cash, SPY and TLT, got %+v", r.Components)
	}
	if c := component(t, r, ""); c.Book != 51230 || c.Observed != 51230 || !c.Explained {
		t.Errorf("cash in whole cents: %+v", c)
	}
}

func TestEveryShareCounts(t *testing.T) {
	for _, q := range []int64{146, 148} {
		r := classify(t, iraBook(), nil, observed(512.3, held("SPY", q), held("TLT", 20)))
		if r.Classification != event.ReconciliationUnexplained || r.Action != event.ReconciliationActionDegrade {
			t.Fatalf("SPY %d: got %s/%s", q, r.Classification, r.Action)
		}
		if !slices.Equal(r.ScopeInstrumentIDs, []string{"SPY"}) || len(r.ScopeCashComponents) != 0 {
			t.Errorf("SPY %d: scope %v %v; only SPY is in doubt", q, r.ScopeInstrumentIDs, r.ScopeCashComponents)
		}
		if c := component(t, r, "SPY"); c.Residual != q-147 || c.Explained {
			t.Errorf("SPY %d: %+v", q, c)
		}
		if !component(t, r, "TLT").Explained {
			t.Errorf("TLT matched and is not in the scope")
		}
		if !strings.Contains(r.Detail, "SPY") {
			t.Errorf("the detail names the difference: %q", r.Detail)
		}
	}
}

func TestEveryCentCounts(t *testing.T) {
	for _, cash := range []float64{512.29, 512.31} {
		r := classify(t, iraBook(), nil, observed(cash, held("SPY", 147), held("TLT", 20)))
		if r.Classification != event.ReconciliationUnexplained {
			t.Fatalf("cash %v: got %s", cash, r.Classification)
		}
		// A cash-only difference names no instrument (ADR 0019, amended
		// 2026-09-24): it blocks buys and leaves every position managed.
		if len(r.ScopeInstrumentIDs) != 0 || !slices.Equal(r.ScopeCashComponents, []string{event.ReconciliationComponentCash}) {
			t.Errorf("cash %v: scope %v %v", cash, r.ScopeInstrumentIDs, r.ScopeCashComponents)
		}
	}
}

func TestABrokerOnlyHoldingIsNamed(t *testing.T) {
	r := classify(t, iraBook(), nil, observed(512.3, held("IEF", 5), held("SPY", 147), held("TLT", 20)))
	if r.Classification != event.ReconciliationUnexplained || !slices.Equal(r.ScopeInstrumentIDs, []string{"IEF"}) {
		t.Fatalf("got %s %v", r.Classification, r.ScopeInstrumentIDs)
	}
	if c := component(t, r, "IEF"); c.Book != 0 || c.Observed != 5 {
		t.Errorf("IEF: %+v", c)
	}
	gone := classify(t, iraBook(), nil, observed(512.3, held("SPY", 147)))
	if !slices.Equal(gone.ScopeInstrumentIDs, []string{"TLT"}) {
		t.Errorf("a holding the broker no longer reports is named too: %v", gone.ScopeInstrumentIDs)
	}
}

func TestAJournalledFillExplainsItsSharesAndCash(t *testing.T) {
	fill := reconcile.FillChange(event.TargetWeightFillPayload{
		AccountID: "ira", ClientOrderID: "tw-1", FillID: "exec-1", InstrumentID: "TLT", Side: event.OrderSideBuy,
		Quantity: 3, Price: 91.17, Commission: 1, FilledAt: closeAt.Add(time.Hour),
	})
	// 3 x 91.17 = 273.51, plus $1 commission: cash 512.30 - 274.51 = 237.79.
	r := classify(t, iraBook(), []reconcile.Change{fill}, observed(237.79, held("SPY", 147), held("TLT", 23)))
	if r.Classification != event.ReconciliationExplained || r.Action != event.ReconciliationActionContinue {
		t.Fatalf("got %s/%s: %s", r.Classification, r.Action, r.Detail)
	}
	if c := component(t, r, "TLT"); c.Book != 20 || c.Expected != 23 || !slices.Equal(c.Explanations, []string{"exec-1"}) {
		t.Errorf("TLT: %+v", c)
	}
	if c := component(t, r, ""); c.Expected != 23779 || !slices.Equal(c.Explanations, []string{"exec-1"}) {
		t.Errorf("cash: %+v", c)
	}
	sell := reconcile.FillChange(event.TargetWeightFillPayload{
		AccountID: "ira", ClientOrderID: "tw-2", FillID: "exec-2", InstrumentID: "SPY", Side: event.OrderSideSell,
		Quantity: 7, Price: 500, Commission: 1.25, FilledAt: closeAt.Add(time.Hour),
	})
	r = classify(t, iraBook(), []reconcile.Change{sell}, observed(4011.05, held("SPY", 140), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained {
		t.Fatalf("a sell: got %s: %s", r.Classification, r.Detail)
	}
}

// TestAFillOutsideTheWindowExplainsNothing: a fill stamped before the
// book's second is already in the book; one stamped in the observation's
// second post-dates it (the pending rule's same-second convention), so
// neither is applied, and a broker figure that reflects one is a difference.
func TestAFillOutsideTheWindowExplainsNothing(t *testing.T) {
	for name, at := range map[string]time.Time{"before the book": closeAt.Add(-time.Second), "in the observation's second": morning} {
		fill := reconcile.FillChange(event.TargetWeightFillPayload{
			AccountID: "ira", ClientOrderID: "tw-1", FillID: "exec-1", InstrumentID: "TLT", Side: event.OrderSideBuy,
			Quantity: 3, Price: 100, FilledAt: at,
		})
		r := classify(t, iraBook(), []reconcile.Change{fill}, observed(212.3, held("SPY", 147), held("TLT", 23)))
		if r.Classification != event.ReconciliationUnexplained {
			t.Errorf("%s: got %s", name, r.Classification)
		}
	}
	inBookSecond := reconcile.FillChange(event.TargetWeightFillPayload{
		AccountID: "ira", ClientOrderID: "tw-1", FillID: "exec-1", InstrumentID: "TLT", Side: event.OrderSideBuy,
		Quantity: 3, Price: 100, FilledAt: closeAt,
	})
	if r := classify(t, iraBook(), []reconcile.Change{inBookSecond}, observed(212.3, held("SPY", 147), held("TLT", 23))); r.Classification != event.ReconciliationExplained {
		t.Errorf("a fill in the book's own second post-dates the book: got %s", r.Classification)
	}
}

func TestADividendAndASplitExplainTheirChanges(t *testing.T) {
	dividend := reconcile.CorporateActionChange("ca:div", event.CorporateActionPayload{
		InstrumentID: "TLT", Kind: event.CorporateActionKindDividend, EffectiveAt: closeAt.Add(2 * time.Hour), CashAmount: 6.4, Currency: "USD",
	})
	split := reconcile.CorporateActionChange("ca:split", event.CorporateActionPayload{
		InstrumentID: "SPY", Kind: event.CorporateActionKindSplit, EffectiveAt: closeAt.Add(3 * time.Hour),
		NewShares: 2, OldShares: 1, EngineSharesPerRawShare: 1,
	})
	r := classify(t, iraBook(), []reconcile.Change{dividend, split}, observed(518.7, held("SPY", 294), held("TLT", 20)))
	if r.Classification != event.ReconciliationExplained {
		t.Fatalf("got %s: %s", r.Classification, r.Detail)
	}
	if c := component(t, r, "SPY"); c.Expected != 294 || !slices.Equal(c.Explanations, []string{"ca:split"}) {
		t.Errorf("SPY: %+v", c)
	}
	if c := component(t, r, ""); !slices.Equal(c.Explanations, []string{"ca:div"}) {
		t.Errorf("cash: %+v", c)
	}
	// A reverse split's fraction is paid in cash: 147 / 10 is 14 shares and
	// cash in lieu for the rest.
	reverse := reconcile.CorporateActionChange("ca:reverse", event.CorporateActionPayload{
		InstrumentID: "SPY", Kind: event.CorporateActionKindSplit, EffectiveAt: closeAt.Add(3 * time.Hour),
		NewShares: 1, OldShares: 10, EngineSharesPerRawShare: 1, RawSharesLost: 1, CashInLieu: 350, Currency: "USD",
	})
	if r := classify(t, iraBook(), []reconcile.Change{reverse}, observed(862.3, held("SPY", 14), held("TLT", 20))); r.Classification != event.ReconciliationExplained {
		t.Errorf("reverse split: got %s: %s", r.Classification, r.Detail)
	}
}

func TestUnverifiableEvidenceHalts(t *testing.T) {
	unreadable := observed(0)
	unreadable.Unreadable, unreadable.Currency = []string{event.ReconciliationComponentPositions, event.ReconciliationComponentCash}, ""
	noCash := observed(0, held("SPY", 147), held("TLT", 20))
	noCash.Unreadable, noCash.Currency = []string{event.ReconciliationComponentCash}, ""
	euros := observed(512.3, held("SPY", 147), held("TLT", 20))
	euros.Currency = "EUR"
	early := observed(512.3, held("SPY", 147), held("TLT", 20))
	early.AsOf = closeAt.Add(-time.Minute)
	delisting := reconcile.CorporateActionChange("ca:delist", event.CorporateActionPayload{
		InstrumentID: "TLT", Kind: event.CorporateActionKindDelisting, EffectiveAt: closeAt.Add(time.Hour),
	})
	foreign := reconcile.CorporateActionChange("ca:div", event.CorporateActionPayload{
		InstrumentID: "TLT", Kind: event.CorporateActionKindDividend, EffectiveAt: closeAt.Add(time.Hour), CashAmount: 1, Currency: "CAD",
	})
	for name, c := range map[string]struct {
		book    *reconcile.Book
		changes []reconcile.Change
		obs     event.ReconciliationPayload
		want    string
	}{
		"no book":             {nil, nil, observed(512.3, held("SPY", 147)), "no account snapshot"},
		"positions unread":    {iraBook(), nil, unreadable, "positions"},
		"cash unread":         {iraBook(), nil, noCash, "cash"},
		"another currency":    {iraBook(), nil, euros, "EUR"},
		"before the book":     {iraBook(), nil, early, "precedes"},
		"unsupported action":  {iraBook(), []reconcile.Change{delisting}, observed(512.3, held("SPY", 147)), "delisting"},
		"cash in another one": {iraBook(), []reconcile.Change{foreign}, observed(513.3, held("SPY", 147), held("TLT", 20)), "CAD"},
	} {
		r := classify(t, c.book, c.changes, c.obs)
		if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt {
			t.Errorf("%s: got %s/%s", name, r.Classification, r.Action)
			continue
		}
		if !strings.Contains(strings.Join(r.Unverifiable, "; "), c.want) {
			t.Errorf("%s: reasons %v do not mention %q", name, r.Unverifiable, c.want)
		}
	}
}

// TestABookWithoutCashComparesPositionsOnly: Boost's account snapshot
// (version 3) states no cash, so the margin account's cash is not compared
// and the result says so.
func TestABookWithoutCashComparesPositionsOnly(t *testing.T) {
	b := reconcile.PositionsBook("snapshot:joint", event.AccountPositionsSnapshotPayload{
		AccountID: "joint", AsOf: closeAt, NetLiquidation: 8530, Currency: "USD",
		Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 147}},
	})
	obs := observed(-4700, held("SPY", 147))
	obs.AccountID = "joint"
	r := classify(t, &b, nil, obs)
	if r.Classification != event.ReconciliationMatched || r.CashCompared || len(r.Components) != 1 {
		t.Fatalf("got %s, cash compared %v, components %+v", r.Classification, r.CashCompared, r.Components)
	}
	if !strings.Contains(r.Detail, "no cash") {
		t.Errorf("the detail says cash was not compared: %q", r.Detail)
	}
}

func TestCentsRoundsHalfAwayFromZero(t *testing.T) {
	for x, want := range map[float64]int64{512.3: 51230, -4700: -470000, 0.005: 1, -0.005: -1, 273.51: 27351} {
		if got, err := reconcile.Cents(x); err != nil || got != want {
			t.Errorf("Cents(%v) = %d, %v, want %d", x, got, err, want)
		}
	}
}

// TestCentsRoundsTheDecimalNotItsBinaryNeighbour: a half cent written in
// decimal is often just below the half in binary, so rounding x*100 as a
// float books 0.145 as 14 cents. ADR 0019 ("Quantities and unexplained cash
// residuals both have zero tolerance": binary floating-point epsilon is not
// a monetary tolerance) asks for the declared rule, half away from zero, on
// the decimal figure, and a fill's value on the exact product of its
// quantity and decimal price.
func TestCentsRoundsTheDecimalNotItsBinaryNeighbour(t *testing.T) {
	for x, want := range map[float64]int64{0.145: 15, 1.005: 101, 2.195: 220, 1.025: 103, -0.145: -15, -1.005: -101, 0.144: 14, 1.0049: 100} {
		if got, err := reconcile.Cents(x); err != nil || got != want {
			t.Errorf("Cents(%v) = %d, %v, want %d", x, got, err, want)
		}
	}
	// 3 at 1.005 is 3.015 exactly, 3.0149999999999997 as a float product;
	// a commission of 0.145 is 15 cents.
	buy := reconcile.FillChange(event.TargetWeightFillPayload{FillID: "f", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 3, Price: 1.005, Commission: 0.145})
	if buy.Cash != -302-15 {
		t.Errorf("a buy of 3 at 1.005 with 0.145 commission costs %d cents, want %d", buy.Cash, -302-15)
	}
	sell := reconcile.FillChange(event.TargetWeightFillPayload{FillID: "f", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 7, Price: 2.195, Commission: 0.145})
	if sell.Cash != 1537-15 {
		t.Errorf("a sale of 7 at 2.195 less 0.145 commission credits %d cents, want %d", sell.Cash, 1537-15)
	}
}

// TestCentsRefusesAFigureThatIsNotANumber: JSON cannot carry NaN or an
// infinity, and the payloads Cents reads refuse them, so one reaching it is
// a defect in the caller, never a figure to book.
func TestCentsRefusesAFigureThatIsNotANumber(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Cents(NaN) returned")
		}
	}()
	_, _ = reconcile.Cents(math.NaN())
}

// TestTheDetailCountsEachExplainingEventOnce: a fill moves a position and
// the cash, and is listed under both, but it is one journalled event, so
// the result's detail, the durable evidence the owner reads, counts it once.
func TestTheDetailCountsEachExplainingEventOnce(t *testing.T) {
	fill := reconcile.FillChange(event.TargetWeightFillPayload{
		AccountID: "ira", ClientOrderID: "tw-1", FillID: "exec-1", InstrumentID: "TLT", Side: event.OrderSideBuy,
		Quantity: 3, Price: 91.17, Commission: 1, FilledAt: closeAt.Add(time.Hour),
	})
	r := classify(t, iraBook(), []reconcile.Change{fill}, observed(237.79, held("SPY", 147), held("TLT", 23)))
	if !strings.Contains(r.Detail, "explained by 1 journalled event(s)") {
		t.Errorf("one fill is one event: %q", r.Detail)
	}
}

// TestTheCashComparisonStatesItsParts: the journal knows each part of the
// expected cash (fills' values, their commissions, dividends and cash in
// lieu), but IBKR's TotalCashValue is one net figure, so a residual can only
// be stated against the whole, with its parts beside it, and a journalled
// credit and an equal unjournalled debit cancel undetected. ADR 0019
// ("Explained means") says offsetting activities do not explain one another;
// until the observation carries IBKR's cash activity rows the result says
// plainly that cash is compared as one net total.
func TestTheCashComparisonStatesItsParts(t *testing.T) {
	fill := reconcile.FillChange(event.TargetWeightFillPayload{
		AccountID: "ira", ClientOrderID: "tw-1", FillID: "exec-1", InstrumentID: "TLT", Side: event.OrderSideBuy,
		Quantity: 3, Price: 91.17, Commission: 1, FilledAt: closeAt.Add(time.Hour),
	})
	dividend := reconcile.CorporateActionChange("ca:div", event.CorporateActionPayload{
		InstrumentID: "TLT", Kind: event.CorporateActionKindDividend, EffectiveAt: closeAt.Add(2 * time.Hour), CashAmount: 6.4, Currency: "USD",
	})
	// 512.30 - 273.51 - 1.00 + 6.40 = 244.19 expected; the broker shows 244.00.
	r := classify(t, iraBook(), []reconcile.Change{fill, dividend}, observed(244, held("SPY", 147), held("TLT", 23)))
	if r.Classification != event.ReconciliationUnexplained {
		t.Fatalf("got %s: %s", r.Classification, r.Detail)
	}
	for _, want := range []string{"book 512.30", "fills -273.51", "commissions -1.00", "dividends 6.40", "cash in lieu 0.00", "expected 244.19", "broker 244.00", "residual -0.19", "one net total"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the detail states %q: %q", want, r.Detail)
		}
	}
	for name, cash := range map[string]float64{"matched": 512.3} {
		m := classify(t, iraBook(), nil, observed(cash, held("SPY", 147), held("TLT", 20)))
		if !strings.Contains(m.Detail, "one net total") {
			t.Errorf("%s: the limit is stated: %q", name, m.Detail)
		}
	}
	explained := classify(t, iraBook(), []reconcile.Change{dividend}, observed(518.7, held("SPY", 147), held("TLT", 20)))
	if !strings.Contains(explained.Detail, "one net total") {
		t.Errorf("explained: the limit is stated: %q", explained.Detail)
	}
}

// TestAFigureBeyondTheCentRangeIsUnverifiable: a finite dollar figure whose
// cents do not fit an int64 is no cent count, so a reconciliation that meets
// one in the broker's cash, the book's cash, or a fill's value or commission
// halts instead of comparing a wrapped number (ADR 0019, "Explained means":
// an unreadable figure is never matched).
func TestAFigureBeyondTheCentRangeIsUnverifiable(t *testing.T) {
	const huge = 1e17
	bookWith := func(cash float64) *reconcile.Book {
		b := reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{
			AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: closeAt, Currency: "USD", Positions: []event.AccountPosition{}},
			SettledCash:                     cash, TotalCash: cash,
		})
		return &b
	}
	fill := func(price, commission float64) reconcile.Change {
		c := reconcile.FillChange(event.TargetWeightFillPayload{FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 1, Price: price, Commission: commission, FilledAt: closeAt.Add(time.Hour)})
		return c
	}
	cases := map[string]struct {
		book    *reconcile.Book
		changes []reconcile.Change
		obs     event.ReconciliationPayload
	}{
		"broker cash":     {bookWith(512.3), nil, observed(huge)},
		"book cash":       {bookWith(huge), nil, observed(512.3)},
		"fill value":      {bookWith(512.3), []reconcile.Change{fill(huge, 0)}, observed(512.3, held("SPY", 1))},
		"fill commission": {bookWith(512.3), []reconcile.Change{fill(1, huge)}, observed(512.3, held("SPY", 1))},
	}
	for name, c := range cases {
		r := classify(t, c.book, c.changes, c.obs)
		if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt || len(r.Unverifiable) == 0 {
			t.Errorf("%s: got %s/%s %v, want unverifiable and halt", name, r.Classification, r.Action, r.Unverifiable)
		}
	}
}

// TestACorporateActionBeyondTheCentRangeIsUnverifiable: a dividend's or a
// split's cash that no cent count can hold halts the reconciliation that
// applies it, like any other figure beyond the range.
func TestACorporateActionBeyondTheCentRangeIsUnverifiable(t *testing.T) {
	at := closeAt.Add(time.Hour)
	for name, c := range map[string]reconcile.Change{
		"dividend": reconcile.CorporateActionChange("ca:1", event.CorporateActionPayload{Kind: event.CorporateActionKindDividend, InstrumentID: "SPY", EffectiveAt: at, CashAmount: 1e17, Currency: "USD"}),
		"split":    reconcile.CorporateActionChange("ca:2", event.CorporateActionPayload{Kind: event.CorporateActionKindSplit, InstrumentID: "SPY", EffectiveAt: at, NewShares: 2, OldShares: 1, CashInLieu: 1e17, Currency: "USD"}),
	} {
		r := classify(t, iraBook(), []reconcile.Change{c}, observed(512.3, held("SPY", 147), held("TLT", 20)))
		if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt {
			t.Errorf("%s: got %s/%s, want unverifiable and halt", name, r.Classification, r.Action)
		}
	}
}

// TestSumsOfInRangeFiguresThatWrapAreUnverifiable: each figure fits an int64
// of cents but a sum of two does not, so the fill's cash, the running expected
// cash, the cash parts and the residual are checked sums, and an overflow
// makes the reconciliation unverifiable instead of comparing a wrapped count.
func TestSumsOfInRangeFiguresThatWrapAreUnverifiable(t *testing.T) {
	const big = 5e16 // 5e18 cents, in range; two of them wrap
	bookWith := func(cash float64) *reconcile.Book {
		b := reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{
			AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: closeAt, Currency: "USD", Positions: []event.AccountPosition{}},
			SettledCash:                     cash, TotalCash: cash,
		})
		return &b
	}
	at := closeAt.Add(time.Hour)
	dividend := reconcile.CorporateActionChange("ca:1", event.CorporateActionPayload{Kind: event.CorporateActionKindDividend, InstrumentID: "SPY", EffectiveAt: at, CashAmount: big, Currency: "USD"})
	cases := map[string]struct {
		book    *reconcile.Book
		changes []reconcile.Change
		obs     event.ReconciliationPayload
	}{
		"buy value and commission":  {bookWith(0), []reconcile.Change{reconcile.FillChange(event.TargetWeightFillPayload{FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 1, Price: big, Commission: big, FilledAt: at})}, observed(0, held("SPY", 1))},
		"sell value and commission": {bookWith(0), []reconcile.Change{reconcile.FillChange(event.TargetWeightFillPayload{FillID: "f1", InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: 1, Price: big, Commission: -big, FilledAt: at})}, observed(0, held("SPY", -1))},
		"running expected cash":     {bookWith(big), []reconcile.Change{dividend}, observed(0)},
		"residual":                  {bookWith(-big), nil, observed(big)},
	}
	for name, c := range cases {
		r := classify(t, c.book, c.changes, c.obs)
		if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt || len(r.Unverifiable) == 0 {
			t.Errorf("%s: got %s/%s %v, want unverifiable and halt", name, r.Classification, r.Action, r.Unverifiable)
		}
	}
}

// TestShareArithmeticThatWrapsOrDividesByZeroIsUnverifiable: a split's new
// share count and every running share total are checked, and a split of no
// old or no new shares is no split, so each halts instead of comparing a
// wrapped or undefined count.
func TestShareArithmeticThatWrapsOrDividesByZeroIsUnverifiable(t *testing.T) {
	at := closeAt.Add(time.Hour)
	split := func(newShares, oldShares int64) reconcile.Change {
		return reconcile.CorporateActionChange("ca:1", event.CorporateActionPayload{Kind: event.CorporateActionKindSplit, InstrumentID: "SPY", EffectiveAt: at, NewShares: newShares, OldShares: oldShares, Currency: "USD"})
	}
	fill := func(id string, q int64) reconcile.Change {
		return reconcile.FillChange(event.TargetWeightFillPayload{FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: q, FilledAt: at})
	}
	sell := func(id string, q int64) reconcile.Change {
		return reconcile.FillChange(event.TargetWeightFillPayload{FillID: id, InstrumentID: "SPY", Side: event.OrderSideSell, Quantity: q, FilledAt: at})
	}
	cases := map[string][]reconcile.Change{
		"split product wraps": {split(math.MaxInt64, 1)},
		"split of no old":     {split(2, 0)},
		"split of no new":     {split(0, 1)},
		"split of negative":   {split(-2, 1)},
		"running shares wrap": {fill("f1", math.MaxInt64)},
		"running shares dive": {sell("f1", math.MaxInt64), sell("f2", math.MaxInt64)},
	}
	for name, changes := range cases {
		r := classify(t, iraBook(), changes, observed(512.3, held("SPY", 147), held("TLT", 20)))
		if r.Classification != event.ReconciliationUnverifiable || r.Action != event.ReconciliationActionHalt || len(r.Unverifiable) == 0 {
			t.Errorf("%s: got %s/%s %v, want unverifiable and halt", name, r.Classification, r.Action, r.Unverifiable)
		}
	}
}

// TestACashFigureNothingComparesCannotHalt: cash arithmetic runs only when
// cash is compared, so a positions-only book with a fill value beyond any
// cent count still matches on its positions.
func TestACashFigureNothingComparesCannotHalt(t *testing.T) {
	b := reconcile.PositionsBook("snapshot:joint", event.AccountPositionsSnapshotPayload{
		AccountID: "joint", AsOf: closeAt, Currency: "USD",
		Positions: []event.AccountPosition{{InstrumentID: "SPY", Quantity: 1}},
	})
	at := closeAt.Add(time.Hour)
	fill := func(id string) reconcile.Change {
		return reconcile.FillChange(event.TargetWeightFillPayload{FillID: id, InstrumentID: "SPY", Side: event.OrderSideBuy, Quantity: 1, Price: 5e16, FilledAt: at})
	}
	obs := observed(0, held("SPY", 3))
	obs.AccountID = "joint"
	r := classify(t, &b, []reconcile.Change{fill("f1"), fill("f2")}, obs)
	if r.Action != event.ReconciliationActionContinue || r.CashCompared {
		t.Fatalf("got %s/%s %v, want positions to explain the fills", r.Classification, r.Action, r.Unverifiable)
	}
}

// TestOnlyTheCashPartsWrapping: the book's cash offsets two large dividends,
// so the running cash never leaves range but the dividends part does; the
// reason names the expected cash.
func TestOnlyTheCashPartsWrapping(t *testing.T) {
	const big = 5e16
	b := reconcile.CashBook("snapshot:1", event.AccountCashSnapshotPayload{
		AccountPositionsSnapshotPayload: event.AccountPositionsSnapshotPayload{AccountID: "ira", AsOf: closeAt, Currency: "USD", Positions: []event.AccountPosition{}},
		SettledCash:                     -big, TotalCash: -big,
	})
	at := closeAt.Add(time.Hour)
	div := func(id string) reconcile.Change {
		return reconcile.CorporateActionChange(id, event.CorporateActionPayload{Kind: event.CorporateActionKindDividend, InstrumentID: "SPY", EffectiveAt: at, CashAmount: big, Currency: "USD"})
	}
	r := classify(t, &b, []reconcile.Change{div("ca:1"), div("ca:2")}, observed(big))
	if r.Action != event.ReconciliationActionHalt || !strings.Contains(strings.Join(r.Unverifiable, ";"), "the expected cash") {
		t.Fatalf("got %s/%s %v, want a halt naming the expected cash", r.Classification, r.Action, r.Unverifiable)
	}
}
