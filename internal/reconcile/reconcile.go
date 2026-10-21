// Package reconcile classifies one reconciliation of an account's book
// against the broker (ADR 0019, "Explain changes by causing events; never
// adopt a balance"): the journal's book, every journalled change since it,
// and IBKR's observation give each component a residual, and a component is
// explained only when its residual is zero. It is pure: everything it
// compares arrives as an argument, and it never adopts the broker's figure.
package reconcile

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// Book is the journal's book a reconciliation starts from: the latest
// account snapshot the engine read, the book its decisions rest on (ADR 0026,
// point 2). Cash is in whole cents and set only when the snapshot states it.
type Book struct {
	AnchorID  string
	AsOf      time.Time
	Positions map[string]int64
	CashSet   bool
	Cash      int64
	Currency  string
	// Fault, when set, is why the book's cash is no cent count: Classify
	// cannot compare against it and reports the book unverifiable.
	Fault string
}

// PositionsBook is the book of account.snapshot version 3, envelope id: its
// holdings, and no cash, which that version does not carry.
func PositionsBook(id string, p event.AccountPositionsSnapshotPayload) Book {
	positions := make(map[string]int64, len(p.Positions))
	for _, position := range p.Positions {
		positions[position.InstrumentID] = position.Quantity
	}
	return Book{AnchorID: id, AsOf: p.AsOf, Positions: positions, Currency: p.Currency}
}

// CashBook is the book of account.snapshot version 4, envelope id: its
// holdings and its total cash, which includes unsettled proceeds and so is
// the balance IBKR's TotalCashValue reports.
func CashBook(id string, p event.AccountCashSnapshotPayload) Book {
	b := PositionsBook(id, p.AccountPositionsSnapshotPayload)
	b.CashSet = true
	cash, err := Cents(p.TotalCash)
	if err != nil {
		b.Fault = fmt.Sprintf("the book's total cash: %v", err)
	}
	b.Cash = cash
	return b
}

// The kinds of change a book can be bridged by.
const (
	ChangeFill     = "fill"
	ChangeDividend = event.CorporateActionKindDividend
	ChangeSplit    = event.CorporateActionKindSplit
	// ChangeCashMovement is a deposit or withdrawal the Flex reader
	// journalled (account.cash-movement version 2): it moves cash alone.
	ChangeCashMovement = "cash-movement"
)

// Change is one journalled event that changes the book: a fill of a released
// order, a dividend's posted cash, or a split (ADR 0019, "Explained means":
// "a typed, validated event recorded before this reconciliation input"). ID
// is its stable identity, a fill's fill ID or a corporate action's envelope
// ID. Shares is a fill's signed change in raw shares; Cash its signed effect
// in whole cents, and Commission the part of Cash that is a fill's
// commission, in whole cents, a cost already inside Cash (so Cash+Commission
// is the fill's signed value: negative for a buy's cost, positive for a
// sale's proceeds). Any other corporate action is recorded with its own kind, which no
// book can apply.
type Change struct {
	ID           string
	Kind         string
	InstrumentID string
	At           time.Time
	Shares       int64
	Cash         int64
	Commission   int64
	// Currency is the currency of a corporate action's cash, empty for a
	// fill, which is in the account's own.
	Currency             string
	NewShares, OldShares int64
	// CashFault, when set, is why a cash figure of the change (a fill's value
	// or commission, a dividend's amount, cash in lieu) is no cent count:
	// Classify reports the reconciliation unverifiable if it applies the
	// change while comparing cash, and ignores it when it does not. Share
	// counts are int64 already; their arithmetic is checked as it is applied.
	CashFault string
}

// FillChange is fill p's change: a buy adds its shares and costs its value
// and commission, a sell removes its shares and credits its value less
// commission. The booking rule: the value, the exact product of the
// quantity and the decimal price, and the commission are each rounded to the
// cent, half away from zero, as IBKR books them.
func FillChange(p event.TargetWeightFillPayload) Change {
	value, valueErr := roundCents(new(big.Rat).Mul(decimal(p.Price), new(big.Rat).SetInt64(p.Quantity)))
	commission, commissionErr := Cents(p.Commission)
	c := Change{ID: p.FillID, Kind: ChangeFill, InstrumentID: p.InstrumentID, At: p.FilledAt, Shares: p.Quantity, Commission: commission}
	var sumErr error
	if p.Side == event.OrderSideSell {
		c.Shares = -p.Quantity
		c.Cash, sumErr = subCents(value, commission)
	} else {
		var cost int64
		if cost, sumErr = addCents(value, commission); sumErr == nil {
			c.Cash, sumErr = subCents(0, cost)
		}
	}
	if err := errors.Join(valueErr, commissionErr, sumErr); err != nil {
		c.CashFault = fmt.Sprintf("fill %s: %v", p.FillID, err)
	}
	return c
}

// CorporateActionChange is the change of corporate action p, envelope id: a
// dividend credits its posted cash, a split converts the holding at its
// ratio and credits its cash in lieu (ADRs 0023 and 0024). Any other kind
// keeps its kind, which Classify cannot apply.
func CorporateActionChange(id string, p event.CorporateActionPayload) Change {
	c := Change{ID: id, Kind: p.Kind, InstrumentID: p.InstrumentID, At: p.EffectiveAt, Currency: p.Currency}
	var err error
	switch p.Kind {
	case event.CorporateActionKindDividend:
		c.Cash, err = Cents(p.CashAmount)
	case event.CorporateActionKindSplit:
		c.NewShares, c.OldShares = p.NewShares, p.OldShares
		c.Cash, err = Cents(p.CashInLieu)
	}
	if err != nil {
		c.CashFault = fmt.Sprintf("corporate action %s: %v", id, err)
	}
	return c
}

// CashMovementChange is the change of a journalled deposit (a positive
// Amount) or withdrawal, envelope id: its amount in whole cents, dated at its
// as of (ADR 0007; ADR 0019, amended 2026-10-21). It moves no holding.
func CashMovementChange(id string, p event.CashMovementPayload) Change {
	c := Change{ID: id, Kind: ChangeCashMovement, At: p.AsOf, Currency: p.Currency}
	var err error
	if c.Cash, err = Cents(p.Amount); err != nil {
		c.CashFault = fmt.Sprintf("cash movement %s: %v", id, err)
	}
	return c
}

// HeldUntil is the moment up to which book's cash holds every transfer: a
// book stamped at its New York session's official close was read after the
// close, so it holds the rest of that New York day, as the Flex reader
// takes it to (adapter/ibkr/flex.py _held_until; ADR 0019, "the reading
// after the close counts as the close"); any other book holds what came
// before its own time. A book in a year the NYSE calendar does not hold is
// an error: whether it is a close is not known.
func HeldUntil(book Book) (time.Time, error) {
	local := book.AsOf.In(nysecalendar.NewYork)
	trading, err := nysecalendar.IsTradingDay(local)
	if err != nil {
		return time.Time{}, fmt.Errorf("whether the book as of %s is a close is not known: %w", stamp(book.AsOf), err)
	}
	if !trading {
		return book.AsOf, nil
	}
	// SessionClose fails only for a day that is no session or is out of
	// range, and IsTradingDay has just ruled out both.
	if closed, _ := nysecalendar.SessionClose(local); !closed.Equal(book.AsOf) {
		return book.AsOf, nil
	}
	y, m, d := local.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, nysecalendar.NewYork), nil
}

// TransfersIn sums, in whole cents, the journalled transfers among changes
// dated from from up to, not including, to, leaving out those counted
// already, and names them in journal order. A transfer whose amount is no
// cent count, or a sum that does not fit an int64, is an error.
func TransfersIn(changes []Change, from, to time.Time, counted map[string]bool) (sum int64, ids []string, err error) {
	ids = []string{}
	for _, c := range changes {
		at := c.At.Truncate(time.Second)
		if c.Kind != ChangeCashMovement || counted[c.ID] || at.Before(from) || !at.Before(to) {
			continue
		}
		if c.CashFault != "" {
			return 0, nil, errors.New(c.CashFault)
		}
		if sum, err = addCents(sum, c.Cash); err != nil {
			return 0, nil, err
		}
		ids = append(ids, c.ID)
	}
	return sum, ids, nil
}

// Cents is x dollars in whole cents, rounded half away from zero on x's
// decimal figure, the shortest decimal that reads back as x (the figure its
// producer wrote), never on the binary product x*100, which puts a half cent
// such as 0.145 just below the half: binary floating-point epsilon is not a
// monetary tolerance (ADR 0019, "Quantities and unexplained cash residuals
// both have zero tolerance"). x is finite: the payloads it comes from refuse
// anything else. A finite figure whose cents do not fit an int64 is an error,
// never a wrapped count.
func Cents(x float64) (int64, error) { return roundCents(decimal(x)) }

// addCents is a+b in whole cents; it is an error when the sum does not fit an
// int64, never a wrapped count.
func addCents(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("%d cents plus %d cents is more cents than a book can count", a, b)
	}
	return a + b, nil
}

// sharesAfter is held shares after c: a fill adds its shares and a split
// scales them, truncating toward zero. It is an error when the count does not
// fit an int64, or when a split has no old or no new shares.
func sharesAfter(held int64, c Change) (int64, error) {
	if c.Kind != ChangeSplit {
		if (c.Shares > 0 && held > math.MaxInt64-c.Shares) || (c.Shares < 0 && held < math.MinInt64-c.Shares) {
			return 0, fmt.Errorf("%d shares plus %d shares is more shares than a book can count", held, c.Shares)
		}
		return held + c.Shares, nil
	}
	if c.OldShares <= 0 || c.NewShares <= 0 {
		return 0, fmt.Errorf("a split of %d new for %d old shares is no split", c.NewShares, c.OldShares)
	}
	scaled := new(big.Int).Mul(big.NewInt(held), big.NewInt(c.NewShares))
	scaled.Quo(scaled, big.NewInt(c.OldShares))
	if !scaled.IsInt64() {
		return 0, fmt.Errorf("%d shares split %d for %d is more shares than a book can count", held, c.NewShares, c.OldShares)
	}
	return scaled.Int64(), nil
}

// subCents is a-b in whole cents, with addCents' guarantee.
func subCents(a, b int64) (int64, error) {
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return 0, fmt.Errorf("%d cents less %d cents is more cents than a book can count", a, b)
	}
	return a - b, nil
}

// decimal is x's shortest decimal figure, exactly.
func decimal(x float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(x, 'f', -1, 64))
	if !ok {
		panic(fmt.Sprintf("reconcile: %v has no decimal figure", x))
	}
	return r
}

// roundCents is dollars d in whole cents, rounded half away from zero; it is
// an error when the cents do not fit an int64.
func roundCents(d *big.Rat) (int64, error) {
	scaled := new(big.Rat).Mul(d, big.NewRat(100, 1))
	quo, rem := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2)).Cmp(scaled.Denom()) >= 0 {
		quo.Add(quo, big.NewInt(int64(scaled.Num().Sign())))
	}
	if !quo.IsInt64() {
		return 0, fmt.Errorf("%s dollars is more cents than a book can count", d.FloatString(2))
	}
	return quo.Int64(), nil
}

// Classify reconciles obs, the account.reconciliation envelope inputID,
// against book and changes, the journal's events since the book in journal
// order (ADR 0019, "Explain changes by causing events").
//
//   - A change is applied when it is stamped at or after the book's second
//     and before the observation's: a fact stamped in a snapshot's second
//     post-dates that snapshot, the convention the pending rule uses
//     (internal/risklimit), so it is never counted on both sides.
//   - The positions compared are the union of the book's, the broker's and
//     those a change touched; an instrument not listed is not held. Every
//     share and every cent counts: no residual is small enough to ignore.
//   - Cash is compared only when the book states it, and as one net total:
//     IBKR's TotalCashValue is one figure, so the expected cash's journalled
//     parts (fills' values, their commissions, dividends, cash in lieu) are
//     stated beside a residual but cannot each be matched, and unjournalled
//     activities that offset one another are not detected. The result's
//     detail says so (ADR 0019, "Explained means": offsetting activities do
//     not explain one another).
//   - No book, an observation that cannot be read or precedes the book, a
//     currency that differs, or a change no book can apply is unverifiable:
//     the action halts. Otherwise a residual is unexplained, and the scope is
//     exactly the unexplained components: the action degrades. Otherwise the
//     result continues, matched when no change was applied.
func Classify(book *Book, changes []Change, inputID string, obs event.ReconciliationPayload) event.ReconciliationResultPayload {
	return Reconcile(book, changes, inputID, obs).Result
}

// Outcome is one reconciliation's result, and whether a journalled
// withdrawal was larger than the cash on hand before it.
type Outcome struct {
	Result event.ReconciliationResultPayload
	// Overdrawn is set when a journalled withdrawal applied takes the
	// expected cash below zero: IBKR refuses a withdrawal of more than the
	// account's cash, so the difference it leaves is named in the detail and
	// is never pending a reading (ADR 0019, amended 2026-10-21).
	Overdrawn bool
}

// Reconcile is Classify, with the outcome's facts beside the result.
//
//   - A journalled deposit or withdrawal is applied to the cash alone when it
//     is dated from the moment the book holds transfers until (HeldUntil) and
//     before the observation; a book that states no cash ignores it.
func Reconcile(book *Book, changes []Change, inputID string, obs event.ReconciliationPayload) Outcome {
	r, overdrawn := classify(book, changes, inputID, obs, "; the book states no cash, so cash is not compared")
	return Outcome{Result: r, Overdrawn: overdrawn}
}

// classify is Reconcile, with uncompared stating why cash is not compared
// when the book states none.
func classify(book *Book, changes []Change, inputID string, obs event.ReconciliationPayload, uncompared string) (event.ReconciliationResultPayload, bool) {
	r := event.ReconciliationResultPayload{
		AccountID: obs.AccountID, ReconciliationID: obs.ReconciliationID, InputID: inputID, Components: []event.ReconciliationComponent{},
	}
	var reasons []string
	positionsRead := obs.Readable(event.ReconciliationComponentPositions)
	if !positionsRead {
		reasons = append(reasons, "the broker's positions could not be read")
	}
	if book == nil {
		reasons = append(reasons, "no account snapshot has been recorded, so the journal has no book to compare")
		return finish(r, reasons, "", nil, uncompared), false
	}
	r.AnchorID = book.AnchorID
	if obs.AsOf.Before(book.AsOf) {
		reasons = append(reasons, fmt.Sprintf("the observation as of %s precedes the book as of %s", stamp(obs.AsOf), stamp(book.AsOf)))
	}
	if book.Fault != "" {
		reasons = append(reasons, book.Fault)
	}
	cashRead := false
	if book.CashSet {
		r.CashCompared, r.Currency = true, book.Currency
		switch {
		case !obs.Readable(event.ReconciliationComponentCash):
			reasons = append(reasons, "the broker's total cash could not be read")
		case obs.Currency != book.Currency:
			reasons = append(reasons, fmt.Sprintf("the broker's cash is in %s; the book's is in %s", obs.Currency, book.Currency))
		default:
			cashRead = true
		}
	}
	observedCash := int64(0)
	if cashRead {
		var err error
		if observedCash, err = Cents(obs.TotalCash); err != nil {
			reasons = append(reasons, fmt.Sprintf("the broker's total cash: %v", err))
			cashRead = false
		}
	}

	expected := map[string]int64{}
	maps.Copy(expected, book.Positions)
	explained := map[string][]string{}
	cash, cashExplained := book.Cash, []string{}
	var parts cashParts
	var overdrawn []string
	heldUntil, heldErr := time.Time{}, error(nil)
	if cashRead && slices.ContainsFunc(changes, func(c Change) bool { return c.Kind == ChangeCashMovement }) {
		if heldUntil, heldErr = HeldUntil(*book); heldErr != nil {
			reasons = append(reasons, heldErr.Error())
		}
	}
	for _, c := range changes {
		at := c.At.Truncate(time.Second)
		if c.Kind == ChangeCashMovement {
			// A transfer moves cash alone: it is applied only when cash is
			// compared, from the moment the book holds transfers until.
			if !cashRead || heldErr != nil || at.Before(heldUntil) || !at.Before(obs.AsOf) {
				continue
			}
		} else if at.Before(book.AsOf) || !at.Before(obs.AsOf) {
			continue
		}
		switch {
		case c.Kind != ChangeFill && c.Kind != ChangeDividend && c.Kind != ChangeSplit && c.Kind != ChangeCashMovement:
			reasons = append(reasons, fmt.Sprintf("corporate action %s (%s of %s) is not one a target-weight book can apply", c.ID, c.Kind, c.InstrumentID))
			continue
		case r.CashCompared && c.Cash != 0 && c.Currency != "" && c.Currency != book.Currency:
			reasons = append(reasons, fmt.Sprintf("%s pays cash in %s; the book's is in %s", c.ID, c.Currency, book.Currency))
			continue
		}
		if cashRead && c.CashFault != "" {
			reasons = append(reasons, c.CashFault)
			continue
		}
		if c.Kind == ChangeCashMovement {
			next, err := addCents(cash, c.Cash)
			if err == nil {
				err = parts.add(c)
			}
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("%s: the expected cash: %v", c.ID, err))
				continue
			}
			if c.Cash < 0 && next < 0 {
				overdrawn = append(overdrawn, fmt.Sprintf("withdrawal %s of %s is more than the cash on hand before it, %s, which IBKR refuses", c.ID, dollars(-c.Cash), dollars(cash)))
			}
			cash = next
			cashExplained = append(cashExplained, c.ID)
			continue
		}
		nextShares, err := sharesAfter(expected[c.InstrumentID], c)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: the expected shares of %s: %v", c.ID, c.InstrumentID, err))
			continue
		}
		nextCash := cash
		if cashRead {
			if nextCash, err = addCents(cash, c.Cash); err == nil {
				err = parts.add(c)
			}
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("%s: the expected cash: %v", c.ID, err))
				continue
			}
		}
		expected[c.InstrumentID] = nextShares
		if c.Kind != ChangeDividend {
			explained[c.InstrumentID] = append(explained[c.InstrumentID], c.ID)
		}
		if cashRead && (c.Cash != 0 || c.Kind == ChangeFill) {
			cash = nextCash
			cashExplained = append(cashExplained, c.ID)
		}
	}

	if cashRead {
		appendEntry(&r, &reasons, event.ReconciliationComponent{
			Component: event.ReconciliationComponentCash, Book: book.Cash, Expected: cash, Observed: observedCash, Explanations: cashExplained,
		})
	}
	if positionsRead {
		observedQ := map[string]int64{}
		for _, p := range obs.Positions {
			observedQ[p.InstrumentID] = p.Quantity
		}
		ids := slices.Concat(slices.Collect(maps.Keys(expected)), slices.Collect(maps.Keys(observedQ)))
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			appendEntry(&r, &reasons, event.ReconciliationComponent{
				Component: event.ReconciliationComponentPosition, InstrumentID: id,
				Book: book.Positions[id], Expected: expected[id], Observed: observedQ[id], Explanations: append([]string{}, explained[id]...),
			})
		}
	}
	r = finish(r, reasons, parts.String(book.Cash), overdrawn, uncompared)
	return r, len(overdrawn) > 0 && r.Classification == event.ReconciliationUnexplained
}

// appendEntry completes c's residual and whether it is explained, and adds it
// to r; a residual that does not fit an int64 is an unverifiable reason
// instead.
func appendEntry(r *event.ReconciliationResultPayload, reasons *[]string, c event.ReconciliationComponent) {
	residual, err := subCents(c.Observed, c.Expected)
	if err != nil {
		*reasons = append(*reasons, fmt.Sprintf("the %s residual: %v", c.Component, err))
		return
	}
	c.Residual, c.Explained = residual, residual == 0
	r.Components = append(r.Components, c)
}

// cashParts are the journalled parts of the expected cash, in whole cents.
type cashParts struct {
	fills, commissions, dividends, inLieu, transfers int64
	// transferred is set once a transfer is counted, so a result with none
	// states the terms it always has.
	transferred bool
}

// add counts c in its part; it is an error, leaving p unchanged, when a part
// no longer fits an int64.
func (p *cashParts) add(c Change) error {
	next := *p
	var err error
	switch c.Kind {
	case ChangeFill:
		var value int64
		if value, err = addCents(c.Cash, c.Commission); err == nil {
			if next.fills, err = addCents(next.fills, value); err == nil {
				next.commissions, err = subCents(next.commissions, c.Commission)
			}
		}
	case ChangeDividend:
		next.dividends, err = addCents(next.dividends, c.Cash)
	case ChangeSplit:
		next.inLieu, err = addCents(next.inLieu, c.Cash)
	case ChangeCashMovement:
		next.transfers, err = addCents(next.transfers, c.Cash)
		next.transferred = true
	}
	if err == nil {
		*p = next
	}
	return err
}

// String states the book's cash and each part, the terms the expected cash
// sums.
func (p cashParts) String(book int64) string {
	s := fmt.Sprintf("book %s, fills %s, commissions %s, dividends %s, cash in lieu %s",
		dollars(book), dollars(p.fills), dollars(p.commissions), dollars(p.dividends), dollars(p.inLieu))
	if p.transferred {
		s += ", deposits and withdrawals " + dollars(p.transfers)
	}
	return s
}

// netCashLimit is the limit every result that compares cash states.
const netCashLimit = "; cash is compared as one net total, IBKR's TotalCashValue, so unjournalled cash activities that offset one another are not detected"

// finish classifies r from its components and the unverifiable reasons, and
// states its detail; parts states the expected cash's journalled parts,
// overdrawn each withdrawal larger than the cash before it, and uncompared why
// cash is not compared when it is not.
func finish(r event.ReconciliationResultPayload, reasons []string, parts string, overdrawn []string, uncompared string) event.ReconciliationResultPayload {
	var differences []string
	events := map[string]bool{}
	for _, c := range r.Components {
		for _, id := range c.Explanations {
			events[id] = true
		}
		switch {
		case c.Explained:
		case c.Component == event.ReconciliationComponentCash:
			r.ScopeCashComponents = append(r.ScopeCashComponents, c.Component)
			difference := fmt.Sprintf("total cash: %s, expected %s %s, broker %s (residual %s)", parts, dollars(c.Expected), r.Currency, dollars(c.Observed), dollars(c.Residual))
			for _, o := range overdrawn {
				difference += "; " + o
			}
			differences = append(differences, difference)
		default:
			r.ScopeInstrumentIDs = append(r.ScopeInstrumentIDs, c.InstrumentID)
			differences = append(differences, fmt.Sprintf("%s: book %d, expected %d, broker %d (residual %+d)", c.InstrumentID, c.Book, c.Expected, c.Observed, c.Residual))
		}
	}
	cash := netCashLimit
	if !r.CashCompared {
		cash = uncompared
	}
	switch {
	case len(reasons) > 0:
		r.Classification, r.Action, r.Unverifiable = event.ReconciliationUnverifiable, event.ReconciliationActionHalt, reasons
		r.ScopeInstrumentIDs, r.ScopeCashComponents = nil, nil
		r.Detail = "unverifiable: " + strings.Join(reasons, "; ")
	case len(differences) > 0:
		r.Classification, r.Action = event.ReconciliationUnexplained, event.ReconciliationActionDegrade
		r.Detail = "unexplained: " + strings.Join(differences, "; ") + cash
	case len(events) > 0:
		r.Classification, r.Action = event.ReconciliationExplained, event.ReconciliationActionContinue
		r.Detail = fmt.Sprintf("explained by %d journalled event(s) since the book %s%s", len(events), r.AnchorID, cash)
	default:
		r.Classification, r.Action = event.ReconciliationMatched, event.ReconciliationActionContinue
		r.Detail = "matched the book " + r.AnchorID + cash
	}
	return r
}

func dollars(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
