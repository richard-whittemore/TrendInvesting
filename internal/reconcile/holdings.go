package reconcile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// CloseSnapshotPrefix begins the reconciliation ID of a close-snapshot
// check (Holdings); the rest is the account snapshot's envelope ID.
const CloseSnapshotPrefix = "close-snapshot:"

// Holdings checks next, an account snapshot about to become the book, against
// book, the book in force, and changes, the journal's events in journal
// order: each holding next states must be book's adjusted by every change
// stamped from book's second up to, not including, next's (ADR 0019,
// "Explain changes by causing events; never adopt a balance", amended
// 2026-10-21). It is Classify with next's holdings as the observation, so
// the window, the union of positions and the zero tolerance are the same.
//
//   - A holding no change explains, such as a trade made during the session
//     that the engine did not place, a fill it was never given, or a
//     journalled fill IBKR's snapshot does not show, is unexplained, and
//     the scope is exactly those instruments: the action degrades. A snapshot is never adopted as the book silently.
//   - Cash is not compared, whatever the books state: a snapshot's cash moves
//     with postings no event records yet (interest, fees), which the IBKR
//     adapter bounds before it stamps a reading as the close (ADR 0019,
//     amended 2026-10-18). A cash fault of book or of a change is therefore
//     not this check's concern.
//   - A change no book can apply makes it unverifiable, as in Classify: the
//     action halts.
//
// The result is a risk.reconciliation.result whose InputID is next's
// envelope ID and whose ReconciliationID is CloseSnapshotPrefix and that ID.
func Holdings(accountID string, book *Book, changes []Change, next Book) event.ReconciliationResultPayload {
	ids := make([]string, 0, len(next.Positions))
	for id, q := range next.Positions {
		if q != 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	positions := make([]event.AccountPosition, 0, len(ids))
	for _, id := range ids {
		positions = append(positions, event.AccountPosition{InstrumentID: id, Quantity: next.Positions[id]})
	}
	obs := event.ReconciliationPayload{
		AccountID: accountID, ReconciliationID: CloseSnapshotPrefix + next.AnchorID, Trigger: event.ReconciliationTriggerMorning,
		AsOf: next.AsOf, Unreadable: []string{}, Positions: positions,
	}
	var positionsOnly *Book
	if book != nil {
		b := *book
		b.CashSet, b.Cash, b.Fault = false, 0, ""
		positionsOnly = &b
	}
	uncompared := fmt.Sprintf("; the holdings of the close snapshot %s are compared with the book and the changes journalled since it, and its cash is not compared", next.AnchorID)
	r := classify(positionsOnly, changes, next.AnchorID, obs, uncompared)
	if r.Classification == event.ReconciliationUnexplained {
		r.Detail = strings.Replace(r.Detail, "unexplained: ", "unexplained: holdings no journalled fill explains (a trade the engine did not place, a fill it was not given, or a journalled fill IBKR does not show): ", 1)
	}
	return r
}
