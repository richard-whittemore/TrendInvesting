package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// costsFileName is the Flex reader's costs ledger, in the same folder as its
// cash-movements ledger (adapter/ibkr/flex.py, COSTS_NAME).
const costsFileName = "costs.jsonl"

// costCurrency is the currency the account's money is totalled in; a posting
// in another is counted aside, never added (CostLedger, Costs.Note).
const costCurrency = "USD"

// CostPosting is one commission, fee or broker-interest posting the Flex
// reader recorded. Amount is as IBKR states it: negative is money out of the
// account.
type CostPosting struct {
	Kind          string
	Amount        float64
	Currency      string
	AsOf          time.Time
	TransactionID string
}

// CostLedger is what the costs block knows of the account's commissions,
// fees and broker interest: the Flex reader's costs ledger
// (docs/manual/alerts-and-dashboard.md, "Costs"). Its fields mean what
// CashLedger's do: Read when the ledger was read whole, Unreadable when it
// exists and was not, NotAccepted when it belongs to another account, and
// CoveredTo the last day the reader's last accepted reading covered. State
// is the costs status file's state ("read", "running", "set-aside",
// "coverage-gap"; "" when there is none) and CheckedAt the New York day of
// that reading: the costs are read in their own step, so they can be set
// aside while the cash ledger is read (adapter/ibkr/flex.py, read_once).
// Postings holds each cost once, at its latest value; Restated counts every
// restatement line the ledger holds (a cost restated twice counts twice),
// RestatedTo is the New York day of the latest one's posting, Reversed counts
// the restatements to zero, and RestatedLast is the costs status's own count
// of the restatements the last reading recorded (ParseCostLedger,
// costsStatusOf).
type CostLedger struct {
	Read         bool
	Unreadable   string
	NotAccepted  string
	CoveredTo    string
	State        string
	CheckedAt    string
	Postings     []CostPosting
	Restated     int
	RestatedTo   string
	Reversed     int
	RestatedLast int
}

// LoadCostLedgerFor reads the costs ledger beside the cash ledger at
// cashLedgerPath, for the account whose instance name is instance. It is
// accepted by the same folder rule as the cash ledger (LoadCashLedgerFor): a
// ledger whose folder names another instance is not read, since a cost
// counted on the wrong account would misstate both. An empty path is no
// ledger; a missing file is not yet recorded unless the status file beside
// it records a reading, which writes no ledger until it has a cost to write.
func LoadCostLedgerFor(cashLedgerPath, instance string, readFile func(string) ([]byte, error)) CostLedger {
	if cashLedgerPath == "" {
		return CostLedger{}
	}
	dir := filepath.Dir(cashLedgerPath)
	if instance != "" {
		if owner := filepath.Base(dir); owner != instance {
			return CostLedger{NotAccepted: fmt.Sprintf("it belongs to %s, not %s", owner, instance)}
		}
	}
	path := filepath.Join(dir, costsFileName)
	coveredTo := coveredToOf(path+flexStatusSuffix, readFile)
	state, checkedAt, restatedLast := costsStatusOf(path+flexStatusSuffix, readFile)
	raw, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if coveredTo == "" && state == "" {
			return CostLedger{}
		}
		return CostLedger{Read: true, CoveredTo: coveredTo, State: state, CheckedAt: checkedAt, RestatedLast: restatedLast}
	}
	if err != nil {
		return CostLedger{Unreadable: fmt.Sprintf("the costs ledger cannot be read: %v", withoutPath(err))}
	}
	ledger := ParseCostLedger(raw)
	if ledger.Read {
		ledger.CoveredTo, ledger.State, ledger.CheckedAt, ledger.RestatedLast = coveredTo, state, checkedAt, restatedLast
	}
	return ledger
}

// costsStatusOf is the state of the costs status file at path and the New
// York day of its checked_at, "" for either it does not hold, and the
// number of restatements the reading recorded (its "restated"), 0 when it
// says none.
func costsStatusOf(path string, readFile func(string) ([]byte, error)) (state, checkedAt string, restated int) {
	raw, err := readFile(path)
	if err != nil {
		return "", "", 0
	}
	var status struct {
		State     string    `json:"state"`
		CheckedAt time.Time `json:"checked_at"`
		Restated  int       `json:"restated"`
	}
	if json.Unmarshal(raw, &status) != nil {
		return "", "", 0
	}
	if !status.CheckedAt.IsZero() {
		checkedAt = newYorkDateOf(status.CheckedAt)
	}
	return status.State, checkedAt, max(status.Restated, 0)
}

// newYorkDateOf is the New York day of at: IBKR's day, by which the Flex
// reader cuts the first run's approval day (adapter/ibkr/flex.py,
// new_costs), and so the day the costs are counted on.
func newYorkDateOf(at time.Time) string {
	return at.In(nysecalendar.NewYork).Format(time.DateOnly)
}

// costRestated and costRestatedNote mark a cost IBKR restated, as the Flex
// reader records it (adapter/ibkr/flex.py, RESTATED): the cost's ID with
// costRestated and the restatement's number appended, and a note naming the
// value it replaces.
const (
	costRestated     = ":restated:"
	costRestatedNote = "restated: was "
)

// ParseCostLedger reads a costs ledger's bytes, strictly as ParseCashLedger
// reads the cash ledger's: every line must be an account.cost at version 1
// from the Flex reader that validates (event.CostPayload.ValidateSchema),
// whose ID is its transaction ID and is not repeated, and the file must end
// with a whole line. A cost IBKR restated is recorded again under its next
// restatement's ID (costRestated) with a note, and the latest value counts.
// One line that is not refuses the whole ledger.
func ParseCostLedger(raw []byte) CostLedger {
	if len(raw) == 0 {
		return CostLedger{Read: true}
	}
	if raw[len(raw)-1] != '\n' {
		return CostLedger{Unreadable: "the costs ledger does not end with a whole line"}
	}
	at := map[string]int{}
	restatements := map[string]int{}
	var restatedLines, reversed int
	var restatedTo string
	var postings []CostPosting
	for n, line := range bytes.Split(raw[:len(raw)-1], []byte("\n")) {
		c, id, note, ok := readCostLine(line)
		i, seen := at[c.TransactionID]
		// A restatement to zero reverses the earlier amount; a new cost of nothing is not a cost.
		if ok && c.Amount == 0 && !seen {
			ok = false
		}
		want := flexIDPrefix + c.TransactionID
		if seen {
			want += costRestated + strconv.Itoa(restatements[c.TransactionID]+1)
		}
		if !ok || id != want || seen != strings.HasPrefix(note, costRestatedNote) || (!seen && note != "") {
			return CostLedger{Unreadable: fmt.Sprintf("costs ledger line %d is not a cost as the Flex reader writes it", n+1)}
		}
		if seen {
			restatements[c.TransactionID]++
			restatedLines++
			restatedTo = newYorkDateOf(c.AsOf)
			if c.Amount == 0 {
				reversed++
			}
			postings[i] = c
			continue
		}
		at[c.TransactionID] = len(postings)
		postings = append(postings, c)
	}
	return CostLedger{Read: true, Postings: postings, Restated: restatedLines, RestatedTo: restatedTo, Reversed: reversed}
}

// readCostLine is a costs line's posting, ID and note, and whether it is an
// account.cost from the Flex reader that validates.
func readCostLine(line []byte) (c CostPosting, id, note string, ok bool) {
	var r ledgerRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || r.Type != event.CostEventType || r.SchemaVersion != event.CostSchemaVersion || r.Source != flexSource {
		return CostPosting{}, "", "", false
	}
	var p event.CostPayload
	pd := json.NewDecoder(bytes.NewReader(r.Payload))
	pd.DisallowUnknownFields()
	if pd.Decode(&p) != nil {
		return CostPosting{}, "", "", false
	}
	// Nothing is a valid amount only on a restatement line (ParseCostLedger decides which this is).
	checked := p
	if checked.Amount == 0 {
		checked.Amount = 1
	}
	if checked.ValidateSchema(r.SchemaVersion) != nil || !r.EventTime.Equal(p.AsOf) {
		return CostPosting{}, "", "", false
	}
	return CostPosting{Kind: p.Kind, Amount: p.Amount, Currency: p.Currency, AsOf: p.AsOf, TransactionID: p.TransactionID}, r.ID, r.Note, true
}

// CostTotals are the money paid out, by kind, over a period: commissions,
// fees and interest paid as positive amounts, and interest received, which
// is money in, separately as a positive amount. A refund (a posting of the
// opposite sign) reduces its kind.
type CostTotals struct {
	Commissions      float64
	Fees             float64
	InterestPaid     float64
	InterestReceived float64
}

// Interest is the net interest cost: interest paid less interest received.
func (t CostTotals) Interest() float64 { return t.InterestPaid - t.InterestReceived }

// Net is what the account paid in all, interest received taken off.
func (t CostTotals) Net() float64 { return t.Commissions + t.Fees + t.Interest() }

func (t *CostTotals) add(kind string, paid float64) {
	switch kind {
	case event.CostCommission:
		t.Commissions += paid
	case event.CostFee:
		t.Fees += paid
	case event.CostInterestPaid:
		t.InterestPaid += paid
	case event.CostInterestReceived:
		t.InterestReceived -= paid
	}
}

// Costs is the running total of what the account has paid in commissions,
// fees and broker interest, for the summary's month to date, year to date and
// since the account's start, from the commissions the journal holds on its
// fills and the Flex reader's costs ledger. A commission the ledger states
// for an execution the journal also holds as a fill is counted once, as the
// fill's (costMatchesFill).
//
// Share is the since-start net cost as a share of AverageNetLiquidation, the
// mean of the account's recorded net liquidations through the summary's
// date; ShareAvailable is false when none is recorded. Incomplete is a short
// reason the figures lack the Flex reader's costs (the summary line says it);
// Note says in words what is counted and what may be missing.
type Costs struct {
	MonthStart, YearStart, StartDate string
	Month, Year, Since               CostTotals
	AverageNetLiquidation            float64
	Share                            float64
	ShareAvailable                   bool
	// MatchedCommissions counts the ledger's commission postings left out
	// because a journalled fill already counts them; OtherCurrency the
	// postings in a currency other than USD, which are not totalled;
	// FillsNotRead the fills that did not read, whose commissions are not
	// counted.
	MatchedCommissions int
	OtherCurrency      int
	FillsNotRead       int
	Incomplete         string
	Note               string
}

// fillCost is a fill's commission as the journal holds it.
type fillCost struct {
	id         string
	commission float64
	at         time.Time
}

// earlierFillType is the type of what ForHistory keeps of an earlier run's
// fill: its ID, commission and time (earlierFill) at the fill's version,
// so a cache of earlier logs holds no more of a fill than the costs read.
// It is never journalled.
const earlierFillType = "report.earlier-fill-cost"

// earlierFill is the payload of an earlierFillType envelope.
type earlierFill struct {
	FillID     string    `json:"fill_id"`
	Commission float64   `json:"commission"`
	FilledAt   time.Time `json:"filled_at"`
}

// earlierFillOf is what ForHistory keeps of the fill e: an earlierFillType
// envelope at e's version and event time, whose payload is empty when e
// does not read (readFillCost then refuses it, so it is counted as a fill
// not read).
func earlierFillOf(e event.Envelope) event.Envelope {
	kept := event.Envelope{Type: earlierFillType, SchemaVersion: e.SchemaVersion, EventTime: e.EventTime}
	if f, err := readFillCost(e); err == nil {
		kept.Payload, _ = json.Marshal(earlierFill{f.id, f.commission, f.at})
	}
	return kept
}

// readFillCost decodes a fill at either version this build reads
// (readFill), or what ForHistory kept of one, for its ID, commission and
// time.
func readFillCost(e event.Envelope) (fillCost, error) {
	if e.Type == earlierFillType {
		var p earlierFill
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return fillCost{}, err
		}
		if p.FillID == "" || p.FilledAt.IsZero() {
			return fillCost{}, errors.New("an earlier fill without its ID or time")
		}
		return fillCost{p.FillID, p.Commission, p.FilledAt}, nil
	}
	if e.SchemaVersion == event.FillTargetWeightVersion {
		var p event.TargetWeightFillPayload
		if err := read(e, targetWeightFillVersions, &p); err != nil {
			return fillCost{}, err
		}
		return fillCost{p.FillID, p.Commission, p.FilledAt}, nil
	}
	var p event.FillPayload
	if err := read(e, fillVersions, &p); err != nil {
		return fillCost{}, err
	}
	return fillCost{p.FillID, p.Commission, p.FilledAt}, nil
}

// executionBase is an IBKR execution ID without its last segment, which a
// correction of the execution changes and keeps the rest of
// (adapter/ibkr/reports.py, _exec_base); "" when the ID has none.
func executionBase(id string) string {
	i := strings.LastIndex(id, ".")
	if i <= 0 || i == len(id)-1 {
		return ""
	}
	return id[:i]
}

// laterRevision reports whether fill f is a later record of its execution
// than kept: a higher last segment of the execution ID, which a correction
// raises (executionBase). IBKR's execution IDs are hex-segmented, so the
// segments are compared as hexadecimal numbers where both parse as hex (0a
// follows 09, 10 follows 9); otherwise, or when equal, the later fill time
// decides, and on a full tie the later journalled fill is the execution's
// (the order the envelopes are read in), so a last segment that is not a
// number is never ordered by its bytes.
func laterRevision(f, kept fillCost) bool {
	a, errA := strconv.ParseUint(f.id[strings.LastIndex(f.id, ".")+1:], 16, 64)
	b, errB := strconv.ParseUint(kept.id[strings.LastIndex(kept.id, ".")+1:], 16, 64)
	if errA == nil && errB == nil && a != b {
		return a > b
	}
	return !f.at.Before(kept.at)
}

// costMatchesFill reports whether a commission posting with transaction ID
// id is the commission of a fill whose execution ID is among fills: the same
// ID, or a correction of the same execution. Cash Transactions is expected
// to hold only commission adjustments under IBKR's numeric transaction IDs,
// the per-trade commissions being on the fills under dotted execution IDs;
// that a posting under an execution ID is the fill's is an assumption until
// the first live statement is checked (docs/manual/reconciliation.md, "The
// reader writes every commission").
func costMatchesFill(id string, fills, bases map[string]bool) bool {
	return fills[id] || (executionBase(id) != "" && bases[executionBase(id)])
}

// costsOf totals the costs through date, a New York day, from the fills in
// earlier and envelopes filled before that day ends in New York (utcEnd is
// the midnight that ends it in UTC), and the
// ledger: the same cut for both, so a fill late in the UTC evening counts on
// its New York day as a posting does (newYorkDateOf). netBy is the account's
// net liquidation by date.
func costsOf(earlier, envelopes []event.Envelope, utcEnd time.Time, date string, ledger CostLedger, netBy map[string]float64) Costs {
	var c Costs
	end := time.Date(utcEnd.Year(), utcEnd.Month(), utcEnd.Day(), 0, 0, 0, 0, nysecalendar.NewYork)
	month, year := date[:7], date[:4]
	c.MonthStart, c.YearStart = month+"-01", year+"-01-01"
	count := func(kind string, at time.Time, paid float64) {
		d := newYorkDateOf(at)
		if d > date {
			return
		}
		if c.StartDate == "" || d < c.StartDate {
			c.StartDate = d
		}
		c.Since.add(kind, paid)
		if d[:4] == year {
			c.Year.add(kind, paid)
		}
		if d[:7] == month {
			c.Month.add(kind, paid)
		}
	}
	// One commission per execution: a redelivery has its ID, a correction
	// its base with another last segment (adapter/ibkr/reports.py,
	// _exec_base), and the latest revision is the execution's
	// (laterRevision).
	fills, bases := map[string]bool{}, map[string]bool{}
	var executions []string
	latest := map[string]fillCost{}
	for _, list := range [][]event.Envelope{earlier, envelopes} {
		for _, e := range list {
			if e.Type != event.FillEventType && e.Type != earlierFillType {
				continue
			}
			f, err := readFillCost(e)
			if err != nil {
				// A fill that does not read has no fill time, so only its envelope's time can place it.
				if e.EventTime.Before(end) {
					c.FillsNotRead++
				}
				continue
			}
			// The cut is the fill's own time, the one count dates its commission by.
			if !f.at.Before(end) {
				continue
			}
			fills[f.id] = true
			key := f.id
			if b := executionBase(f.id); b != "" {
				bases[b], key = true, b
			}
			kept, seen := latest[key]
			if !seen {
				executions = append(executions, key)
			}
			if !seen || laterRevision(f, kept) {
				latest[key] = f
			}
		}
	}
	for _, key := range executions {
		f := latest[key]
		count(event.CostCommission, f.at, f.commission)
	}
	for _, p := range ledger.Postings {
		switch {
		case p.Currency != costCurrency:
			c.OtherCurrency++
		case p.Kind == event.CostCommission && costMatchesFill(p.TransactionID, fills, bases):
			c.MatchedCommissions++
		default:
			count(p.Kind, p.AsOf, -p.Amount)
		}
	}
	var sum float64
	for d, net := range netBy {
		sum += net
		if c.StartDate == "" || d < c.StartDate {
			c.StartDate = d
		}
	}
	if len(netBy) > 0 && sum > 0 {
		c.AverageNetLiquidation = sum / float64(len(netBy))
		c.Share, c.ShareAvailable = c.Since.Net()/c.AverageNetLiquidation, true
	}
	c.Incomplete, c.Note = costsNote(c, ledger, date)
	return c
}

// costsNote says what the figures count (note) and, when the Flex reader's
// costs are missing, the short reason (incomplete), which the summary line
// carries: the costs are incomplete whenever their reading is not "read" or
// has fallen behind the summary's day by more than the reader's lag
// (costsBehind).
func costsNote(c Costs, ledger CostLedger, date string) (incomplete, note string) {
	parts := []string{"commissions on fills as the journal holds them"}
	switch {
	case ledger.NotAccepted != "":
		incomplete = "costs ledger not accepted"
		parts = append(parts, fmt.Sprintf("costs ledger not accepted: %s; fees, interest and other commissions are not counted", ledger.NotAccepted))
	case ledger.Read:
		parts = append(parts, "fees, interest and other commissions as recorded by the Flex reader")
		switch {
		case ledger.State == "running":
			incomplete = fmt.Sprintf("costs incomplete: the Flex reader's reading of %s did not finish", ledger.CheckedAt)
		case ledger.State != "" && ledger.State != "read":
			incomplete = "costs incomplete: the Flex reader is set aside as of " + ledger.CheckedAt
		case ledger.State == "" || ledger.CoveredTo == "":
			incomplete = costsReachUnknown
		case costsBehind(ledger.CoveredTo, date):
			incomplete = "costs incomplete: the Flex reader is behind as of " + ledger.CoveredTo
		}
		if incomplete != "" && incomplete != costsReachUnknown {
			parts = append(parts, incomplete)
		}
		switch {
		case ledger.CoveredTo == "":
			parts = append(parts, "how far the Flex reader has recorded is not known; "+lastTwoDaysCosts)
		case date > ledger.CoveredTo:
			parts = append(parts, "costs recorded through "+ledger.CoveredTo, lastTwoDaysCosts)
		default:
			parts = append(parts, "costs recorded through "+ledger.CoveredTo)
		}
	case ledger.Unreadable != "":
		incomplete = "costs ledger not readable"
		parts = append(parts, fmt.Sprintf("costs ledger not readable (%s): fees, interest and other commissions are not counted", ledger.Unreadable))
	default:
		incomplete = "fees and interest not yet recorded by the Flex reader"
		parts = append(parts, "fees and interest not yet recorded by the Flex reader")
	}
	if ledger.Restated > 0 {
		text := fmt.Sprintf("%d restatement(s) by IBKR (latest %s), each cost counted at its latest value", ledger.Restated, ledger.RestatedTo)
		if ledger.Reversed > 0 {
			text += fmt.Sprintf(", %d reversing a cost to zero", ledger.Reversed)
		}
		parts = append(parts, text)
	}
	if ledger.RestatedLast > 0 {
		parts = append(parts, fmt.Sprintf("%d restated in the last reading", ledger.RestatedLast))
	}
	if c.MatchedCommissions > 0 {
		parts = append(parts, fmt.Sprintf("%d commission posting(s) already counted on a fill are not counted again", c.MatchedCommissions))
	}
	if c.OtherCurrency > 0 {
		parts = append(parts, fmt.Sprintf("%d posting(s) in another currency than %s are not totalled", c.OtherCurrency, costCurrency))
	}
	if c.FillsNotRead > 0 {
		parts = append(parts, fmt.Sprintf("%d fill(s) that did not read are not counted", c.FillsNotRead))
	}
	return incomplete, "Counted: " + strings.Join(parts, "; ") + ". Interest received is taken off the net cost."
}

// costsReachUnknown is the costs' incomplete reason when how far the reader
// has recorded is not known; the note says it in its own words.
const costsReachUnknown = "costs incomplete: how far the Flex reader has recorded costs is not known"

// costsBehind reports whether costs recorded through coveredTo have fallen
// behind the summary's date by more than the reader's lag
// (lastTwoDaysCosts), measured after the reader's evening run: the reading
// of the last session before date covers to the day before its statement's
// last day, so more than two sessions after coveredTo and before date is
// behind. A day the calendar does not hold is behind, since how far behind
// is not known.
func costsBehind(coveredTo, date string) bool {
	from, err1 := time.ParseInLocation(time.DateOnly, coveredTo, nysecalendar.NewYork)
	through, err2 := time.ParseInLocation(time.DateOnly, date, nysecalendar.NewYork)
	if err1 != nil || err2 != nil {
		return true
	}
	n, _, err := nysecalendar.Closes(from.AddDate(0, 0, 1), through)
	return err != nil || n > 2
}

// lastTwoDaysCosts is the reader's lag, as for transfers (lastTwoDays): a
// posting dated D is first in the statement read on the evening of the next
// session.
const lastTwoDaysCosts = "costs from the last two trading sessions may not be recorded yet"
