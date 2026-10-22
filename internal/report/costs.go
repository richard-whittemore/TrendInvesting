package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
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
// CoveredTo the last day the reader's last accepted reading covered.
type CostLedger struct {
	Read        bool
	Unreadable  string
	NotAccepted string
	CoveredTo   string
	Postings    []CostPosting
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
	raw, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if coveredTo == "" {
			return CostLedger{}
		}
		return CostLedger{Read: true, CoveredTo: coveredTo}
	}
	if err != nil {
		return CostLedger{Unreadable: fmt.Sprintf("the costs ledger cannot be read: %v", withoutPath(err))}
	}
	ledger := ParseCostLedger(raw)
	if ledger.Read {
		ledger.CoveredTo = coveredTo
	}
	return ledger
}

// ParseCostLedger reads a costs ledger's bytes, strictly as ParseCashLedger
// reads the cash ledger's: every line must be an account.cost at version 1
// from the Flex reader that validates (event.CostPayload.ValidateSchema),
// whose ID is its transaction ID and is not repeated, and the file must end
// with a whole line. One line that is not refuses the whole ledger.
func ParseCostLedger(raw []byte) CostLedger {
	if len(raw) == 0 {
		return CostLedger{Read: true}
	}
	if raw[len(raw)-1] != '\n' {
		return CostLedger{Unreadable: "the costs ledger does not end with a whole line"}
	}
	seen := map[string]bool{}
	var postings []CostPosting
	for n, line := range bytes.Split(raw[:len(raw)-1], []byte("\n")) {
		c, ok := readCostLine(line)
		if !ok || seen[c.TransactionID] {
			return CostLedger{Unreadable: fmt.Sprintf("costs ledger line %d is not a cost as the Flex reader writes it", n+1)}
		}
		seen[c.TransactionID] = true
		postings = append(postings, c)
	}
	return CostLedger{Read: true, Postings: postings}
}

func readCostLine(line []byte) (CostPosting, bool) {
	var r ledgerRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || r.Note != "" || r.Type != event.CostEventType || r.SchemaVersion != event.CostSchemaVersion || r.Source != flexSource {
		return CostPosting{}, false
	}
	var p event.CostPayload
	pd := json.NewDecoder(bytes.NewReader(r.Payload))
	pd.DisallowUnknownFields()
	if pd.Decode(&p) != nil || p.ValidateSchema(r.SchemaVersion) != nil || r.ID != flexIDPrefix+p.TransactionID || !r.EventTime.Equal(p.AsOf) {
		return CostPosting{}, false
	}
	return CostPosting{Kind: p.Kind, Amount: p.Amount, Currency: p.Currency, AsOf: p.AsOf, TransactionID: p.TransactionID}, true
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

// readFillCost decodes a fill at either version this build reads
// (readFill) for its ID, commission and time.
func readFillCost(e event.Envelope) (fillCost, error) {
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

// costMatchesFill reports whether a commission posting with transaction ID
// id is the commission of a fill whose execution ID is among fills: the same
// ID, or a correction of the same execution.
func costMatchesFill(id string, fills, bases map[string]bool) bool {
	return fills[id] || (executionBase(id) != "" && bases[executionBase(id)])
}

// costsOf totals the costs through date from the fills in earlier and
// envelopes (those before end) and the ledger. netBy is the account's net
// liquidation by date.
func costsOf(earlier, envelopes []event.Envelope, end time.Time, date string, ledger CostLedger, netBy map[string]float64) Costs {
	var c Costs
	month, year := date[:7], date[:4]
	c.MonthStart, c.YearStart = month+"-01", year+"-01-01"
	count := func(kind string, at time.Time, paid float64) {
		d := dateOf(at)
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
	fills, bases := map[string]bool{}, map[string]bool{}
	for _, list := range [][]event.Envelope{earlier, envelopes} {
		for _, e := range list {
			if e.Type != event.FillEventType || !e.EventTime.Before(end) {
				continue
			}
			f, err := readFillCost(e)
			if err != nil {
				c.FillsNotRead++
				continue
			}
			if fills[f.id] {
				continue // a redelivery of one execution
			}
			fills[f.id] = true
			if b := executionBase(f.id); b != "" {
				bases[b] = true
			}
			count(event.CostCommission, f.at, f.commission)
		}
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
// costs are missing, the short reason (incomplete).
func costsNote(c Costs, ledger CostLedger, date string) (incomplete, note string) {
	parts := []string{"commissions on fills as the journal holds them"}
	switch {
	case ledger.NotAccepted != "":
		incomplete = "costs ledger not accepted"
		parts = append(parts, fmt.Sprintf("costs ledger not accepted: %s; fees, interest and other commissions are not counted", ledger.NotAccepted))
	case ledger.Read:
		parts = append(parts, "fees, interest and other commissions as recorded by the Flex reader")
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

// lastTwoDaysCosts is the reader's lag, as for transfers (lastTwoDays): a
// posting dated D is first in the statement read on the evening of the next
// session.
const lastTwoDaysCosts = "costs from the last two trading sessions may not be recorded yet"
