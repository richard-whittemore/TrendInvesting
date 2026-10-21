package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
type CashLedger struct {
	Read       bool
	Unreadable string
	Movements  []CashMovement
}

// LoadCashLedger reads the ledger at path with readFile. An empty path, or a
// file that does not exist, is a ledger not yet recorded; any other failure
// to read it is Unreadable, without the path, which a reader of the page is
// not told.
func LoadCashLedger(path string, readFile func(string) ([]byte, error)) CashLedger {
	if path == "" {
		return CashLedger{}
	}
	raw, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return CashLedger{}
	}
	if err != nil {
		return CashLedger{Unreadable: fmt.Sprintf("the ledger cannot be read: %v", withoutPath(err))}
	}
	return ParseCashLedger(raw)
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
