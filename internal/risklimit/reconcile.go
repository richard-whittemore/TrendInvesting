package risklimit

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
	"github.com/richard-whittemore/TrendInvesting/internal/reconcile"
)

// Reconciliation is the gate's answer to one account.reconciliation input:
// the classification to journal under ResultID, and State when it moved the
// account's engine state, at StateSchemaVersion, to be journalled under
// StateID.
type Reconciliation struct {
	Result             event.ReconciliationResultPayload
	ResultID           string
	State              *event.EngineStatePayload
	StateSchemaVersion uint32
	StateID            string
	// Settled is each pending cash difference this reconciliation decided,
	// each with its own result and state, journalled before Result (ADR
	// 0019, amended 2026-10-21, "a cash difference the reader has not yet
	// read").
	Settled []Reconciliation
}

// SetBook records the journal's book that later reconciliations start from:
// the latest account snapshot, which the planner sizes from (ADR 0026, point
// 2). The changes journalled since stay recorded; the classifier applies
// those after the book's time. Quantity units must match the account;
// holdings are checked separately through CheckBook.
func (g *Gate) SetBook(b reconcile.Book) error {
	if err := g.checkQuantityScale(b.QuantityScale); err != nil {
		return err
	}
	g.book, g.classified, g.latestReadAsOf, g.reported, g.reportedAsOf = &b, nil, time.Time{}, nil, nil
	return nil
}

// CheckBook checks b, an account snapshot about to become the book, against
// the book in force and every change journalled since it
// (reconcile.Holdings), moves the engine state as Reconcile does, and then
// sets b as the book (ADR 0019, amended 2026-10-21): a holding no journalled
// fill explains degrades the account on its instrument, and b still becomes
// the book the planner sizes from, the Degraded scope freezing what differs.
// It reports whether the check is to be journalled:
//
//   - never for the run's first book, which has none to be checked against
//     (the IBKR adapter checks that reading against the previous close, or
//     the starting book the Run approval states; ADR 0019, amended
//     2026-10-18);
//   - never when the check continues: a snapshot the fills explain is the
//     ordinary close, and recording nothing keeps every such journal's bytes;
//   - never when b's holdings are exactly the positions a reconciliation
//     already classified against the same book and the same changes: the
//     IBKR adapter sends each reading's reconciliation ahead of its snapshot,
//     and that reconciliation's result, matched or not, already stands for
//     those holdings, so one difference is never reported twice. A new book,
//     or a fill or corporate action recorded after that reconciliation, or
//     dated between its reading and b's (whichever came first), ends its
//     standing, so the snapshot is then checked (classifiedStandsFor);
//   - never for a difference already reported against the same book: an
//     unexplained result whose every instrument was already reported with
//     the same residual, and whose cash is not in doubt, repeats a journalled
//     result, so it is not journalled or paged again however many fills of
//     other instruments came between. A new instrument or a changed residual
//     is a new difference and is reported (ADR 0019, amended 2026-10-21,
//     point 6: one difference, one report).
//
// A book with incompatible quantity units is refused with an error and
// halts the account; the previous book remains in force.
// An unverifiable snapshot halts and is journalled, unless the account is
// already Halted on every one of its reasons (alreadyHaltedOn) and every
// unexplained holding it carries was already reported (alreadyReported): a
// Halt is suppressed only when both hold, so a Halt for a new change or a new
// unexplained holding is always journalled. Neither the classified-holdings
// case nor the reported-difference case alone suppresses a Halt.
func (g *Gate) CheckBook(b reconcile.Book) (out Reconciliation, journal bool, err error) {
	if err := g.checkQuantityScale(b.QuantityScale); err != nil {
		reason := err.Error()
		r := event.ReconciliationResultPayload{
			QuantityScale: g.quantityScale, AccountID: g.accountID,
			ReconciliationID: reconcile.CloseSnapshotPrefix + b.AnchorID, InputID: b.AnchorID,
			Classification: event.ReconciliationUnverifiable, Action: event.ReconciliationActionHalt,
			Components: []event.ReconciliationComponent{}, Unverifiable: []string{reason}, Detail: "unverifiable: " + reason,
		}
		return g.consequence(b.AnchorID, r), true, err
	}
	defer func() {
		if bookErr := g.SetBook(b); bookErr != nil {
			err = bookErr
		}
	}()
	if g.book == nil {
		return Reconciliation{}, false, nil
	}
	r := reconcile.Holdings(g.accountID, g.book, g.changes, b)
	switch {
	case r.Action == event.ReconciliationActionContinue:
		return Reconciliation{}, false, nil
	case r.Action == event.ReconciliationActionHalt:
		// A Halt is suppressed only when the account is already Halted on
		// every one of its reasons AND every unexplained holding it carries
		// was already reported: nothing new. Otherwise it is journalled.
		if g.alreadyHaltedOn(r) && g.alreadyReported(r) {
			return Reconciliation{}, false, nil
		}
	case g.classifiedStandsFor(b) || g.alreadyReported(r):
		return Reconciliation{}, false, nil
	}
	// The book is reset as CheckBook returns, so what this result reports is
	// not recorded: the next check starts from b.
	return g.consequence(b.AnchorID, r), true, nil
}

// classifiedStandsFor reports whether the latest reconciliation's verdict
// stands for b: it was classified against the book in force, b's holdings
// are exactly its positions, and both checks applied the same changes, so
// no recorded fill or corporate action after the book is dated, to the
// second, between the reconciliation's reading and b's (ADR 0019, amended
// 2026-10-21, point 6: a change dated between the two readings, whichever
// came first, ends its standing).
func (g *Gate) classifiedStandsFor(b reconcile.Book) bool {
	if g.classified == nil || !maps.Equal(g.classified, b.Positions) {
		return false
	}
	lo, hi := g.classifiedAsOf, b.AsOf
	if hi.Before(lo) {
		lo, hi = hi, lo
	}
	for _, c := range g.changes {
		t := c.At.Truncate(time.Second)
		// A transfer moves cash alone, and this check compares holdings.
		if c.Kind != reconcile.ChangeCashMovement && !t.Before(g.book.AsOf) && !t.Before(lo) && t.Before(hi) {
			return false
		}
	}
	return true
}

// ObserveCorporateAction records a journalled market.corporate-action,
// envelope id, as a change a reconciliation's book is bridged by: a
// dividend's posted cash or a split's new holding (ADRs 0023 and 0024). Any
// other kind is recorded too, so a reconciliation it falls inside is
// unverifiable rather than blind to it. A corporate action is one fact under
// its envelope ID: a redelivery of the same fact changes nothing, and the ID
// stating a different one is refused and leaves what was recorded, since a
// dividend or a split counted twice would explain a difference the broker
// never made (ADR 0019, "Explained means").
func (g *Gate) ObserveCorporateAction(id string, p event.CorporateActionPayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a corporate action needs its envelope ID")
	}
	if seen, ok := g.corporateActions[id]; ok {
		if sameCorporateAction(seen, p) {
			return nil
		}
		return fmt.Errorf("risklimit: corporate action %s is reused for a different action", id)
	}
	g.corporateActions[id] = p
	g.changes = append(g.changes, reconcile.CorporateActionChange(id, p))
	// As for a fill: a reconciliation classified before this action did not
	// see it (ADR 0019, amended 2026-10-21, point 6; CheckBook).
	g.classified = nil
	return nil
}

// ObserveCashMovement records a deposit or withdrawal the Flex reader
// journalled (account.cash-movement version 2), envelope id, as a change a
// reconciliation's cash is bridged by (ADR 0007; ADR 0019, amended
// 2026-10-21). As a corporate action, it is one fact under its ID: a
// redelivery changes nothing, and the ID stating a different movement is
// refused, since a transfer counted twice would explain cash IBKR never
// moved. It moves no holding, so it ends no reconciliation's standing for a
// close snapshot's holdings (CheckBook).
func (g *Gate) ObserveCashMovement(id string, p event.CashMovementPayload) error {
	if err := p.ValidateSchema(event.CashMovementTransferVersion); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return errors.New("risklimit: a cash movement needs its envelope ID")
	}
	if seen, ok := g.cashMovements[id]; ok {
		if seen.AsOf.Equal(p.AsOf) && seen.Amount == p.Amount && seen.Currency == p.Currency && seen.TransactionID == p.TransactionID {
			return nil
		}
		return fmt.Errorf("risklimit: cash movement %s is reused for a different movement", id)
	}
	g.cashMovements[id] = p
	g.changes = append(g.changes, reconcile.CashMovementChange(id, p))
	return nil
}

// ObserveFlexStatus records the Flex reader's status for the account: the
// latest, by the time its reading started, is the one a cash difference is
// judged against (ADR 0019, amended 2026-10-21). A status for another
// account is refused.
func (g *Gate) ObserveFlexStatus(p event.FlexStatusPayload) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("risklimit: %w", err)
	}
	if p.AccountID != g.accountID {
		return fmt.Errorf("risklimit: Flex status for account %q; this gate is %q", p.AccountID, g.accountID)
	}
	if g.flex == nil || !p.CheckedAt.Before(g.flex.CheckedAt) {
		g.flex = &p
	}
	return nil
}

// sameCorporateAction reports whether a and b state the same action: every
// term equal, and the same effective instant whatever its time zone.
func sameCorporateAction(a, b event.CorporateActionPayload) bool {
	if !a.EffectiveAt.Equal(b.EffectiveAt) {
		return false
	}
	a.EffectiveAt, b.EffectiveAt = time.Time{}, time.Time{}
	return a == b
}

// Reconcile classifies a recorded account.reconciliation, envelope id,
// against the journal's book and every change since it (reconcile.Classify),
// and moves the engine state as ADR 0019 (amended 2026-09-24) sets:
//
//   - matched or explained changes nothing: a later match never returns a
//     Degraded or Halted account to Normal;
//   - unexplained degrades the account, its scope widened by exactly the
//     unexplained instruments and cash components; a cash-only difference
//     freezes no instrument and blocks buys;
//   - unverifiable halts the whole account.
//
// A state record is returned only when the state or its scope changes, and
// it states the whole scope; Halted is never loosened. The first record out
// of Normal is the incident.
func (g *Gate) Reconcile(id string, p event.ReconciliationPayload) (Reconciliation, error) {
	if err := g.checkQuantityScale(p.QuantityScale); err != nil {
		return Reconciliation{}, err
	}
	if err := p.Validate(); err != nil {
		return Reconciliation{}, fmt.Errorf("risklimit: %w", err)
	}
	if id == "" {
		return Reconciliation{}, errors.New("risklimit: a reconciliation needs its envelope ID")
	}
	if p.AccountID != g.accountID {
		return Reconciliation{}, fmt.Errorf("risklimit: reconciliation for account %q; this gate is %q", p.AccountID, g.accountID)
	}
	settled := g.settlePending(id, p)
	o := reconcile.Reconcile(g.book, g.changes, id, p)
	r := g.pend(id, o, p)
	// A reading taken before the latest classified one is older knowledge: it
	// does not replace that verdict, even after a recorded change cleared it
	// (ADR 0019, amended 2026-10-21, point 6).
	if g.book != nil && p.Readable(event.ReconciliationComponentPositions) &&
		!p.AsOf.Before(g.latestReadAsOf) {
		g.latestReadAsOf = p.AsOf
		g.classified = map[string]int64{}
		for _, position := range p.Positions {
			g.classified[position.InstrumentID] = position.Quantity
		}
		g.classifiedAsOf = p.AsOf
	}
	g.report(r, p.AsOf, g.book != nil && p.Readable(event.ReconciliationComponentPositions) && !p.AsOf.Before(g.book.AsOf))
	out := g.consequence(id, r)
	out.Settled = settled
	return out, nil
}

// pendingSessions is how many sessions may close after a pending cash read
// before it is decided whatever the Flex reader says (ADR 0019, amended
// 2026-10-21).
const pendingSessions = event.PendingCashReadSessions

// pendingCash is one cash difference reported as a pending cash read: the
// reconciliation that found it, its cash component, and its window, the
// transfers dated from from up to to, those it counted already excluded. to
// is the as-of of the reading that found it, which a repeat leaves as it is:
// a snapshot whose cash is held until to or later holds the difference
// (PendingCashIncrease).
type pendingCash struct {
	inputID string
	result  event.ReconciliationResultPayload
	cash    event.ReconciliationComponent
	from    time.Time
	to      time.Time
	counted map[string]bool
	// transfers is the deposits and withdrawals among counted, the fills
	// left out: with the book, the residual and the currency, they are the
	// difference's identity (sameDifference).
	transfers map[string]bool
	// alerted is set once a buy refused on this difference has raised the
	// urgent alert: one pending difference pages once (withheldOnCash).
	alerted bool
	// repeats is each later reconciliation that found the same difference
	// against the same book, as a reconnect's does: it is decided with this
	// one, each under its own result (pend).
	repeats []repeatedRead
}

// repeatedRead is a reconciliation that found a pending difference again:
// its input and its reconciliation ID.
type repeatedRead struct {
	inputID          string
	reconciliationID string
}

// sameDifference returns the undecided pending difference r, found against
// the book in force from from, repeats, or nil: the same book, the same
// residual in the same currency, after the same transfers. A fill is no
// transfer: a reading after a fill against the same book that finds the
// same residual finds the same difference (ADR 0019, amended 2026-10-21).
func (g *Gate) sameDifference(r event.ReconciliationResultPayload, cash event.ReconciliationComponent, from time.Time, transfers map[string]bool) *pendingCash {
	for i := range g.pendingCash {
		pc := &g.pendingCash[i]
		if pc.result.AnchorID == r.AnchorID && pc.from.Equal(from) && pc.result.Currency == r.Currency &&
			pc.cash.Residual == cash.Residual && maps.Equal(pc.transfers, transfers) {
			return pc
		}
	}
	return nil
}

// pend returns o's result as a pending cash read when the difference may be
// a transfer the Flex reader has not yet recorded, and records it to be
// decided later (settlePending): the difference is in the cash alone, no
// journalled withdrawal is larger than the cash before it (IBKR refuses
// one), the journal holds the reader's status, and that status does not
// cover the observation's New York day. Otherwise it is o's result,
// unchanged: a cash difference with no reader, or one the reader has
// covered, degrades as before (ADR 0019, amended 2026-10-21).
func (g *Gate) pend(id string, o reconcile.Outcome, p event.ReconciliationPayload) event.ReconciliationResultPayload {
	r := o.Result
	if r.Classification != event.ReconciliationUnexplained || len(r.ScopeInstrumentIDs) > 0 || o.Overdrawn ||
		g.flex == nil || g.flex.Covers(p.AsOf) || g.book == nil {
		return r
	}
	from, err := reconcile.HeldUntil(*g.book)
	if err != nil {
		return r
	}
	var cash event.ReconciliationComponent
	for _, c := range r.Components {
		if c.Component == event.ReconciliationComponentCash {
			cash = c
		}
	}
	movements := map[string]bool{}
	for _, c := range g.changes {
		if c.Kind == reconcile.ChangeCashMovement {
			movements[c.ID] = true
		}
	}
	counted, transfers := map[string]bool{}, map[string]bool{}
	for _, e := range cash.Explanations {
		counted[e] = true
		if movements[e] {
			transfers[e] = true
		}
	}
	read := "has not read IBKR's statement since its reading of " + stamp(g.flex.CheckedAt) + " (" + g.flex.State + ")"
	if g.flex.State == event.FlexStateRead {
		read = "has read IBKR's cash transactions only through " + g.flex.CoveredTo
	}
	// A cash increase may be reversed, so buys are sized without it while it
	// pends (PendingCashIncrease); a decrease may be a withdrawal, so every
	// buy is withheld while it pends (withheldOnCash).
	direction := fmt.Sprintf("it is a cash increase of %s %s, like a deposit: this result withholds no buy, and buys are sized from the cash less it until the reader's records explain it, so a credit that is reversed is never invested", cents(cash.Residual), r.Currency)
	if cash.Residual < 0 {
		direction = fmt.Sprintf("it is a cash decrease of %s %s, like a withdrawal: buys are withheld on the whole account while it pends, so no plan is sized on cash that may not be there, and sells and exits continue", cents(-cash.Residual), r.Currency)
	}
	// A repeat of a difference already pending is that difference: it is
	// judged against the first reading's day and decided with it, so its
	// increase is left out of the budget once (ADR 0019, amended
	// 2026-10-21).
	same := g.sameDifference(r, cash, from, transfers)
	found, repeat := p.AsOf, ""
	if same != nil {
		found = same.to
		repeat = fmt.Sprintf("the same difference reconciliation %s (%s) found, still pending, decided with it; ", same.result.ReconciliationID, same.inputID)
	}
	r.Classification, r.Action, r.ScopeCashComponents = event.ReconciliationPendingCashRead, event.ReconciliationActionAlert, nil
	r.Detail = fmt.Sprintf("pending-cash-read: %s%s; the Flex reader %s, so a deposit or withdrawal it has not yet recorded may explain it; %s; %s; the difference is checked again at each reconciliation until the reader's records explain it, or it is Degraded on cash once the reader has covered %s or more than %d sessions have closed since",
		repeat, strings.TrimPrefix(r.Detail, "unexplained: "), read, direction, g.stateClause(), found.In(nysecalendar.NewYork).Format("20060102"), pendingSessions)
	if same != nil {
		// The difference paged with its first reading: the repeat names
		// it, so the reporter does not page again (one difference, one
		// reconciliation alert).
		r.RepeatOf = same.result.ReconciliationID
		same.repeats = append(same.repeats, repeatedRead{inputID: id, reconciliationID: r.ReconciliationID})
		return r
	}
	g.pendingCash = append(g.pendingCash, pendingCash{inputID: id, result: r, cash: cash, from: from, to: p.AsOf, counted: counted, transfers: transfers})
	return r
}

// stateClause states the account's engine state as a pending cash read
// leaves it: a pending cash read is no state, so it is the state already in
// force, a Degraded one with its scope.
func (g *Gate) stateClause() string {
	switch g.state {
	case StateHalted:
		return "the account stays Halted"
	case StateDegraded:
		scope := slices.Clone(g.scope)
		if len(g.cashScope) > 0 {
			scope = append(scope, "cash")
		}
		return "the account stays Degraded on " + strings.Join(scope, ", ")
	}
	return "the account stays Normal"
}

// withheldOnCash returns the earliest pending cash decrease still
// undecided, or nil when none: while one pends, every buy is withheld, since
// it may be a withdrawal and the cash a buy is sized on may not be there;
// sells and exits go (ADR 0019, amended 2026-10-21). Its alerted flag makes
// the first refusal on it the one that pages.
func (g *Gate) withheldOnCash() *pendingCash {
	for i := range g.pendingCash {
		if g.pendingCash[i].cash.Residual < 0 {
			return &g.pendingCash[i]
		}
	}
	return nil
}

// PendingCashIncrease is the sum, in dollars, of every pending cash
// increase still undecided that snapshot's cash could hold: cash IBKR shows
// that no journalled transfer or fill explains yet, found by a reading at
// or before the moment the snapshot's cash is held until
// (reconcile.HeldUntil: a snapshot stamped at the close was read after it,
// with the evening's reconciliation, and holds the rest of that New York
// day; ADR 0019, point 2). When that moment is not known, every pending increase
// counts: the budget fails closed. Buys are sized from the snapshot's cash
// less this, so a credit that is reversed or posted in error is never
// invested; an increase found by a reading after the snapshot's cash was
// read is not in that cash, so it is not taken from it again. Once the Flex
// reader's records explain an increase, it leaves the sum and the next
// decision invests it (ADR 0019, point 3, amended 2026-10-21).
func (g *Gate) PendingCashIncrease(snapshot reconcile.Book) float64 {
	until, err := reconcile.HeldUntil(snapshot)
	var sum int64
	for _, pc := range g.pendingCash {
		if pc.cash.Residual > 0 && (err != nil || !pc.to.After(until)) {
			sum += pc.cash.Residual
		}
	}
	return float64(sum) / 100
}

// settlePending decides each pending cash difference reported before p, the
// reconciliation input id, against the transfers journalled since and the
// Flex reader's status (ADR 0019, amended 2026-10-21):
//
//   - the transfers now journalled in its window explain it to the cent:
//     explained, and it is settled;
//   - otherwise, once the reader's status covers its day, or more than
//     pendingSessions sessions have closed since it (or the calendar cannot
//     count them), it is unexplained, and the account is Degraded on cash;
//   - otherwise it stays pending, and nothing is journalled: one difference,
//     one alert.
//
// Each result decided is journalled under its own ID, the pending
// reconciliation's beside id.
func (g *Gate) settlePending(id string, p event.ReconciliationPayload) []Reconciliation {
	var out []Reconciliation
	kept := g.pendingCash[:0]
	for _, pc := range g.pendingCash {
		if !p.AsOf.After(pc.to) {
			kept = append(kept, pc)
			continue
		}
		sum, ids, sumErr := reconcile.TransfersIn(g.changes, pc.from, pc.to, pc.counted, pc.result.Currency)
		sessions, _, calErr := nysecalendar.Closes(pc.to, p.AsOf)
		r := pc.result
		c := pc.cash
		c.Explanations = append(slices.Clone(c.Explanations), ids...)
		var why string
		switch {
		case sumErr == nil && sum == c.Residual:
			c.Expected, c.Residual, c.Explained = c.Expected+sum, 0, true
			r.Classification, r.Action = event.ReconciliationExplained, event.ReconciliationActionContinue
			r.Detail = fmt.Sprintf("explained: the cash difference of %s %s that reconciliation %s found pending a reading is explained by the transfers the Flex reader has since recorded: %s",
				cents(pc.cash.Residual), r.Currency, r.ReconciliationID, strings.Join(ids, ", "))
		case sumErr != nil:
			why = "the transfers recorded since cannot be summed: " + sumErr.Error()
		case g.flex != nil && g.flex.Covers(pc.to):
			why = "the Flex reader has covered " + pc.to.In(nysecalendar.NewYork).Format("20060102") + " and its records do not explain it"
		case calErr != nil:
			why = "the sessions since it cannot be counted: " + calErr.Error()
		case sessions > pendingSessions:
			why = fmt.Sprintf("%d sessions have closed since it, more than the %d sessions a difference may wait for the Flex reader", sessions, pendingSessions)
		default:
			kept = append(kept, pc)
			continue
		}
		if why != "" {
			if sumErr == nil {
				c.Expected += sum
				c.Residual -= sum
			}
			r.Classification, r.Action = event.ReconciliationUnexplained, event.ReconciliationActionDegrade
			r.ScopeCashComponents = []string{event.ReconciliationComponentCash}
			r.Detail = fmt.Sprintf("unexplained: total cash: the difference that reconciliation %s found pending a reading, now %s %s after the transfers recorded since (%s), is not explained: %s",
				r.ReconciliationID, cents(c.Residual), r.Currency, strings.Join(ids, ", "), why)
		}
		r.InputID = pc.inputID
		r.Components = []event.ReconciliationComponent{c}
		out = append(out, g.consequence(id+":pending-cash:"+pc.inputID, r))
		// Each reading that repeated the difference is decided with it, so
		// none is left undecided in the log (restart.CheckPendingCash).
		for _, rep := range pc.repeats {
			rr := r
			rr.InputID, rr.ReconciliationID, rr.RepeatOf = rep.inputID, rep.reconciliationID, r.ReconciliationID
			out = append(out, g.consequence(id+":pending-cash:"+rep.inputID, rr))
		}
	}
	g.pendingCash = kept
	return out
}

func cents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// report records the residual of each instrument r finds unexplained, the
// differences journalled against the book in force, and forgets each
// instrument r finds explained, so a difference that recurs after it was
// resolved is a new difference (ADR 0019, amended 2026-10-21, point 6). r
// was read at asOf: a reading before the latest one about an instrument,
// whether that one reported or resolved it, says nothing new about it, so it
// neither forgets nor replaces what the later reading found. A resolved
// instrument keeps its reading time, without a residual, for that guard.
// compared is whether r compared the broker's positions with the book: such a
// reading names every instrument either holds, so a recorded residual whose
// instrument it omits (the holding is gone, and was never in the book) is
// forgotten the same way, unless the reading is earlier than the one that
// recorded it.
func (g *Gate) report(r event.ReconciliationResultPayload, asOf time.Time, compared bool) {
	if compared {
		named := map[string]bool{}
		for _, c := range r.Components {
			named[c.InstrumentID] = true
		}
		for id := range g.reported {
			if !named[id] && !asOf.Before(g.reportedAsOf[id]) {
				delete(g.reported, id)
				g.reportedAsOf[id] = asOf
			}
		}
	}
	for _, c := range r.Components {
		switch {
		case c.InstrumentID == "":
		case c.Explained:
			if at, ok := g.reportedAsOf[c.InstrumentID]; ok && asOf.Before(at) {
				continue
			}
			delete(g.reported, c.InstrumentID)
			if g.reportedAsOf == nil {
				g.reported, g.reportedAsOf = map[string]int64{}, map[string]time.Time{}
			}
			g.reportedAsOf[c.InstrumentID] = asOf
		default:
			if at, ok := g.reportedAsOf[c.InstrumentID]; ok && asOf.Before(at) {
				continue
			}
			if g.reported == nil {
				g.reported, g.reportedAsOf = map[string]int64{}, map[string]time.Time{}
			}
			g.reported[c.InstrumentID] = c.Residual
			g.reportedAsOf[c.InstrumentID] = asOf
		}
	}
}

// alreadyHaltedOn reports whether the account is Halted and every reason r,
// an unverifiable result, gives was already given by a journalled Halt: the
// same unapplicable change, not a new one.
func (g *Gate) alreadyHaltedOn(r event.ReconciliationResultPayload) bool {
	if g.state != StateHalted || len(r.Unverifiable) == 0 {
		return false
	}
	for _, reason := range r.Unverifiable {
		if !g.haltedOn[reason] {
			return false
		}
	}
	return true
}

// alreadyReported reports whether every difference of r, an unexplained
// result (CheckBook journals a Halt before asking), was already reported
// with the same residual against the book in force; a close snapshot's check
// compares no cash.
func (g *Gate) alreadyReported(r event.ReconciliationResultPayload) bool {
	for _, c := range r.Components {
		if c.InstrumentID == "" || c.Explained {
			continue
		}
		if residual, ok := g.reported[c.InstrumentID]; !ok || residual != c.Residual {
			return false
		}
	}
	return true
}

// consequence journals r, the classification of input id, and moves the
// engine state as Reconcile states.
func (g *Gate) consequence(id string, r event.ReconciliationResultPayload) Reconciliation {
	out := Reconciliation{Result: r, ResultID: "reconciliation-result:" + g.accountID + ":" + id}
	var state *event.EngineStatePayload
	if r.Action == event.ReconciliationActionHalt {
		if g.haltedOn == nil {
			g.haltedOn = map[string]bool{}
		}
		for _, reason := range r.Unverifiable {
			g.haltedOn[reason] = true
		}
	}
	switch {
	case r.Action == event.ReconciliationActionHalt && g.state != StateHalted:
		state = &event.EngineStatePayload{State: event.EngineStateHalted, Reason: event.EngineStateReasonReconciliationUnverifiable, Detail: "reconciliation " + r.ReconciliationID + " " + r.Detail}
		g.scope, g.cashScope = nil, nil
		out.StateID = g.move(StateHalted, id)
	case r.Action == event.ReconciliationActionDegrade && g.state != StateHalted:
		scope, cash := union(g.scope, r.ScopeInstrumentIDs), union(g.cashScope, r.ScopeCashComponents)
		if g.state == StateDegraded && slices.Equal(scope, g.scope) && slices.Equal(cash, g.cashScope) {
			break
		}
		g.scope, g.cashScope = scope, cash
		state = &event.EngineStatePayload{
			State: event.EngineStateDegraded, Reason: event.EngineStateReasonReconciliationUnexplained,
			Detail:             "reconciliation " + r.ReconciliationID + " " + r.Detail,
			ScopeInstrumentIDs: slices.Clone(scope), ScopeCashComponents: slices.Clone(cash),
		}
		out.StateID = g.move(StateDegraded, id)
	}
	if state != nil {
		out.State, out.StateSchemaVersion = state, event.EngineStateScopedVersion
	}
	return out
}
