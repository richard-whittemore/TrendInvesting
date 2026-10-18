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
	// Fault, when set, is why a figure of the change is no cent count:
	// Classify reports the reconciliation unverifiable if it applies the
	// change.
	Fault string
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
		c.Fault = fmt.Sprintf("fill %s: %v", p.FillID, err)
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
		c.Fault = fmt.Sprintf("corporate action %s: %v", id, err)
	}
	return c
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
		return finish(r, reasons, "")
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
	for _, c := range changes {
		at := c.At.Truncate(time.Second)
		if at.Before(book.AsOf) || !at.Before(obs.AsOf) {
			continue
		}
		switch {
		case c.Kind != ChangeFill && c.Kind != ChangeDividend && c.Kind != ChangeSplit:
			reasons = append(reasons, fmt.Sprintf("corporate action %s (%s of %s) is not one a target-weight book can apply", c.ID, c.Kind, c.InstrumentID))
			continue
		case r.CashCompared && c.Cash != 0 && c.Currency != "" && c.Currency != book.Currency:
			reasons = append(reasons, fmt.Sprintf("%s pays cash in %s; the book's is in %s", c.ID, c.Currency, book.Currency))
			continue
		}
		if c.Fault != "" {
			reasons = append(reasons, c.Fault)
			continue
		}
		nextCash, err := addCents(cash, c.Cash)
		if err == nil {
			err = parts.add(c)
		}
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: the expected cash: %v", c.ID, err))
			continue
		}
		if c.Kind == ChangeSplit {
			expected[c.InstrumentID] = expected[c.InstrumentID] * c.NewShares / c.OldShares
		} else {
			expected[c.InstrumentID] += c.Shares
		}
		if c.Kind != ChangeDividend {
			explained[c.InstrumentID] = append(explained[c.InstrumentID], c.ID)
		}
		if c.Cash != 0 || c.Kind == ChangeFill {
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
	return finish(r, reasons, parts.String(book.Cash))
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
type cashParts struct{ fills, commissions, dividends, inLieu int64 }

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
	}
	if err == nil {
		*p = next
	}
	return err
}

// String states the book's cash and each part, the terms the expected cash
// sums.
func (p cashParts) String(book int64) string {
	return fmt.Sprintf("book %s, fills %s, commissions %s, dividends %s, cash in lieu %s",
		dollars(book), dollars(p.fills), dollars(p.commissions), dollars(p.dividends), dollars(p.inLieu))
}

// netCashLimit is the limit every result that compares cash states.
const netCashLimit = "; cash is compared as one net total, IBKR's TotalCashValue, so unjournalled cash activities that offset one another are not detected"

// finish classifies r from its components and the unverifiable reasons, and
// states its detail; parts states the expected cash's journalled parts.
func finish(r event.ReconciliationResultPayload, reasons []string, parts string) event.ReconciliationResultPayload {
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
			differences = append(differences, fmt.Sprintf("total cash: %s, expected %s %s, broker %s (residual %s)", parts, dollars(c.Expected), r.Currency, dollars(c.Observed), dollars(c.Residual)))
		default:
			r.ScopeInstrumentIDs = append(r.ScopeInstrumentIDs, c.InstrumentID)
			differences = append(differences, fmt.Sprintf("%s: book %d, expected %d, broker %d (residual %+d)", c.InstrumentID, c.Book, c.Expected, c.Observed, c.Residual))
		}
	}
	cash := netCashLimit
	if !r.CashCompared {
		cash = "; the book states no cash, so cash is not compared"
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
