// Package reconcile classifies one reconciliation of an account's book
// against the broker (ADR 0019, "Explain changes by causing events; never
// adopt a balance"): the journal's book, every journalled change since it,
// and IBKR's observation give each component a residual, and a component is
// explained only when its residual is zero. It is pure: everything it
// compares arrives as an argument, and it never adopts the broker's figure.
package reconcile

import (
	"fmt"
	"maps"
	"math"
	"slices"
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
	b.CashSet, b.Cash = true, Cents(p.TotalCash)
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
// in whole cents. Any other corporate action is recorded with its own kind,
// which no book can apply.
type Change struct {
	ID           string
	Kind         string
	InstrumentID string
	At           time.Time
	Shares       int64
	Cash         int64
	// Currency is the currency of a corporate action's cash, empty for a
	// fill, which is in the account's own.
	Currency             string
	NewShares, OldShares int64
}

// FillChange is fill p's change: a buy adds its shares and costs its value
// and commission, a sell removes its shares and credits its value less
// commission. The booking rule: the value and the commission are each
// rounded to the cent, half away from zero, as IBKR books them.
func FillChange(p event.TargetWeightFillPayload) Change {
	value := Cents(float64(float64(p.Quantity) * p.Price))
	c := Change{ID: p.FillID, Kind: ChangeFill, InstrumentID: p.InstrumentID, At: p.FilledAt, Shares: p.Quantity, Cash: -value - Cents(p.Commission)}
	if p.Side == event.OrderSideSell {
		c.Shares, c.Cash = -p.Quantity, value-Cents(p.Commission)
	}
	return c
}

// CorporateActionChange is the change of corporate action p, envelope id: a
// dividend credits its posted cash, a split converts the holding at its
// ratio and credits its cash in lieu (ADRs 0023 and 0024). Any other kind
// keeps its kind, which Classify cannot apply.
func CorporateActionChange(id string, p event.CorporateActionPayload) Change {
	c := Change{ID: id, Kind: p.Kind, InstrumentID: p.InstrumentID, At: p.EffectiveAt, Currency: p.Currency}
	switch p.Kind {
	case event.CorporateActionKindDividend:
		c.Cash = Cents(p.CashAmount)
	case event.CorporateActionKindSplit:
		c.NewShares, c.OldShares, c.Cash = p.NewShares, p.OldShares, Cents(p.CashInLieu)
	}
	return c
}

// Cents is x dollars in whole cents, rounded half away from zero. A product
// with no addition needs no rounding barrier (docs/development.md).
func Cents(x float64) int64 { return int64(math.Round(x * 100)) }

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
//   - Cash is compared only when the book states it.
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
		return finish(r, reasons)
	}
	r.AnchorID = book.AnchorID
	if obs.AsOf.Before(book.AsOf) {
		reasons = append(reasons, fmt.Sprintf("the observation as of %s precedes the book as of %s", stamp(obs.AsOf), stamp(book.AsOf)))
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

	expected := map[string]int64{}
	maps.Copy(expected, book.Positions)
	explained := map[string][]string{}
	cash, cashExplained := book.Cash, []string{}
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
		if c.Kind == ChangeSplit {
			expected[c.InstrumentID] = expected[c.InstrumentID] * c.NewShares / c.OldShares
		} else {
			expected[c.InstrumentID] += c.Shares
		}
		if c.Kind != ChangeDividend {
			explained[c.InstrumentID] = append(explained[c.InstrumentID], c.ID)
		}
		if c.Cash != 0 || c.Kind == ChangeFill {
			cash += c.Cash
			cashExplained = append(cashExplained, c.ID)
		}
	}

	if cashRead {
		r.Components = append(r.Components, entry(event.ReconciliationComponent{
			Component: event.ReconciliationComponentCash, Book: book.Cash, Expected: cash, Observed: Cents(obs.TotalCash), Explanations: cashExplained,
		}))
	}
	if positionsRead {
		observedQ := map[string]int64{}
		for _, p := range obs.Positions {
			observedQ[p.InstrumentID] = p.Quantity
		}
		ids := slices.Concat(slices.Collect(maps.Keys(expected)), slices.Collect(maps.Keys(observedQ)))
		slices.Sort(ids)
		for _, id := range slices.Compact(ids) {
			r.Components = append(r.Components, entry(event.ReconciliationComponent{
				Component: event.ReconciliationComponentPosition, InstrumentID: id,
				Book: book.Positions[id], Expected: expected[id], Observed: observedQ[id], Explanations: append([]string{}, explained[id]...),
			}))
		}
	}
	return finish(r, reasons)
}

// entry completes a component's residual and whether it is explained.
func entry(c event.ReconciliationComponent) event.ReconciliationComponent {
	c.Residual = c.Observed - c.Expected
	c.Explained = c.Residual == 0
	return c
}

// finish classifies r from its components and the unverifiable reasons, and
// states its detail.
func finish(r event.ReconciliationResultPayload, reasons []string) event.ReconciliationResultPayload {
	var differences []string
	applied := 0
	for _, c := range r.Components {
		applied += len(c.Explanations)
		switch {
		case c.Explained:
		case c.Component == event.ReconciliationComponentCash:
			r.ScopeCashComponents = append(r.ScopeCashComponents, c.Component)
			differences = append(differences, fmt.Sprintf("total cash: expected %s %s, broker %s (residual %s)", dollars(c.Expected), r.Currency, dollars(c.Observed), dollars(c.Residual)))
		default:
			r.ScopeInstrumentIDs = append(r.ScopeInstrumentIDs, c.InstrumentID)
			differences = append(differences, fmt.Sprintf("%s: book %d, expected %d, broker %d (residual %+d)", c.InstrumentID, c.Book, c.Expected, c.Observed, c.Residual))
		}
	}
	cash := ""
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
	case applied > 0:
		r.Classification, r.Action = event.ReconciliationExplained, event.ReconciliationActionContinue
		r.Detail = fmt.Sprintf("explained by %d journalled event(s) since the book %s%s", applied, r.AnchorID, cash)
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
