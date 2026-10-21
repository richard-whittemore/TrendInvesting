package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// The Flex reader's ledger record: its source and the prefix of its ID
// (adapter/ibkr/flex.py, SOURCE and ID_PREFIX).
const (
	flexSource   = "ibkr-flex"
	flexIDPrefix = "ibkr-flex:"
)

// CashMovement is one deposit (a positive Amount) or withdrawal (a negative
// one) the Flex reader recorded, as of its broker time.
type CashMovement struct {
	AsOf          time.Time
	Amount        float64
	TransactionID string
}

// CashLedger is what the equity comparison knows of the owner's deposits and
// withdrawals: the Flex reader's ledger of them (docs/manual/reconciliation.md,
// "Deposits and withdrawals, read from IBKR"). Read is set when the ledger
// was read whole; Unreadable says why it was not when it exists but cannot
// be read. Neither is set when no ledger is named or the file does not exist
// yet. A ledger that is not read moves no figure, and the comparison then
// says it assumes no transfer: it is never taken for an empty one silently.
//
// CoveredTo is the last day, YYYY-MM-DD, the reader's last accepted reading
// covered (its status file's covered_to), "" when that is not known: a
// transfer dated after it may not be in the ledger yet. NotAccepted says
// why a ledger was not taken for the account it was paired with
// (LoadCashLedgerFor); nothing of it is read.
type CashLedger struct {
	Read        bool
	Unreadable  string
	NotAccepted string
	CoveredTo   string
	Movements   []CashMovement
}

// flexStatusSuffix names the reader's status file beside its ledger
// (adapter/ibkr/flex.py, STATUS_SUFFIX), whose covered_to, yyyyMMdd, is the
// last day its last accepted reading covered (docs/manual/reconciliation.md,
// "The status file").
const flexStatusSuffix = ".status.json"

// LoadCashLedger reads the ledger at path with readFile, and how far it
// reaches from the status file beside it (CashLedger.CoveredTo). An empty
// path is no ledger. A ledger file that does not exist is not yet recorded,
// unless the status file records a reading, which writes no ledger until it
// has a transfer to write: that is a ledger read and empty. Any other
// failure to read it is Unreadable, without the path, which a reader of the
// page is not told.
func LoadCashLedger(path string, readFile func(string) ([]byte, error)) CashLedger {
	if path == "" {
		return CashLedger{}
	}
	coveredTo := coveredToOf(path+flexStatusSuffix, readFile)
	raw, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if coveredTo == "" {
			return CashLedger{}
		}
		return CashLedger{Read: true, CoveredTo: coveredTo}
	}
	if err != nil {
		return CashLedger{Unreadable: fmt.Sprintf("the ledger cannot be read: %v", withoutPath(err))}
	}
	ledger := ParseCashLedger(raw)
	if ledger.Read {
		ledger.CoveredTo = coveredTo
	}
	return ledger
}

// LoadCashLedgerFor is LoadCashLedger for the account whose instance name
// is instance: the server keeps each account's engine logs in
// engines/<instance> and its Flex ledger in flex/<instance>
// (deploy/systemd, ti-flex@.service and ti-engine@.service), so a ledger
// whose folder names another instance is not accepted, and none of it is
// read: a transfer mirrored on the wrong account would misstate both.
func LoadCashLedgerFor(path, instance string, readFile func(string) ([]byte, error)) CashLedger {
	if path != "" && instance != "" {
		if owner := filepath.Base(filepath.Dir(path)); owner != instance {
			return CashLedger{NotAccepted: fmt.Sprintf("it belongs to %s, not %s", owner, instance)}
		}
	}
	return LoadCashLedger(path, readFile)
}

// coveredToOf is the status file's covered_to as YYYY-MM-DD, or "" when the
// file is absent, unreadable, or records no covered day (a reading running
// or set aside).
func coveredToOf(path string, readFile func(string) ([]byte, error)) string {
	raw, err := readFile(path)
	if err != nil {
		return ""
	}
	var status struct {
		CoveredTo string `json:"covered_to"`
	}
	if json.Unmarshal(raw, &status) != nil {
		return ""
	}
	day, err := time.Parse("20060102", status.CoveredTo)
	if err != nil {
		return ""
	}
	return day.Format(time.DateOnly)
}

// ledgerRecord is one line of the ledger, as the Flex reader writes it.
type ledgerRecord struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	EventTime     time.Time       `json:"event_time"`
	Source        string          `json:"source"`
	Payload       json.RawMessage `json:"payload"`
	Note          string          `json:"note,omitempty"`
}

// ParseCashLedger reads a ledger's bytes. Every line must be a cash movement
// at version 2 from the Flex reader that validates at its version
// (event.CashMovementPayload.ValidateSchema), whose ID is its transaction ID
// and is not repeated, and the file must end with a whole line: one line
// that is not refuses the whole ledger, as the reader itself refuses to
// append to it, so a part of the record is never taken for all of it.
func ParseCashLedger(raw []byte) CashLedger {
	if len(raw) == 0 {
		return CashLedger{Read: true}
	}
	if raw[len(raw)-1] != '\n' {
		return CashLedger{Unreadable: "the ledger does not end with a whole line"}
	}
	seen := map[string]bool{}
	var movements []CashMovement
	for n, line := range bytes.Split(raw[:len(raw)-1], []byte("\n")) {
		m, ok := readLedgerLine(line)
		if !ok || seen[m.TransactionID] {
			return CashLedger{Unreadable: fmt.Sprintf("ledger line %d is not a cash movement as the Flex reader writes it", n+1)}
		}
		seen[m.TransactionID] = true
		movements = append(movements, m)
	}
	return CashLedger{Read: true, Movements: movements}
}

// readLedgerLine reads one line by the reader's own field set, strictly:
// the envelope's RECORD_FIELDS and OPTIONAL_RECORD_FIELDS (the note) and the
// payload's PAYLOAD_FIELDS and OPTIONAL_PAYLOAD_FIELDS (adapter/ibkr/flex.py,
// read_ledger), the same set the reader refuses to append past.
func readLedgerLine(line []byte) (CashMovement, bool) {
	var r ledgerRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || r.Type != event.CashMovementEventType || r.SchemaVersion != event.CashMovementTransferVersion || r.Source != flexSource {
		return CashMovement{}, false
	}
	var p event.CashMovementPayload
	pd := json.NewDecoder(bytes.NewReader(r.Payload))
	pd.DisallowUnknownFields()
	if pd.Decode(&p) != nil || p.ValidateSchema(r.SchemaVersion) != nil || r.ID != flexIDPrefix+p.TransactionID || !r.EventTime.Equal(p.AsOf) {
		return CashMovement{}, false
	}
	return CashMovement{AsOf: p.AsOf, Amount: p.Amount, TransactionID: p.TransactionID}, true
}
