package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/nysecalendar"
)

// FlexSource is the Source of every input the engine reads from the IBKR
// Flex reader's files (adapter/ibkr/flex.py SOURCE): the ledger's cash
// movements and the reader's status.
const FlexSource = "ibkr-flex"

// FlexLedgerIDPrefix begins a ledger record's ID; the rest is IBKR's
// transaction ID (adapter/ibkr/flex.py ID_PREFIX).
const FlexLedgerIDPrefix = "ibkr-flex:"

// FlexLedgerRecord is one line of the Flex reader's ledger: a deposit or
// withdrawal, account.cash-movement version 2 (CashMovementTransferVersion),
// under the ID the reader gave it.
type FlexLedgerRecord struct {
	ID      string
	Payload CashMovementPayload
}

// flexLedgerLine is a ledger line as the reader writes it (_record).
type flexLedgerLine struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	SchemaVersion uint32          `json:"schema_version"`
	EventTime     time.Time       `json:"event_time"`
	Source        string          `json:"source"`
	Payload       json.RawMessage `json:"payload"`
	Note          string          `json:"note,omitempty"`
}

// The notes the reader writes on a record that reached IBKR's report out of
// order (adapter/ibkr/flex.py NOTES).
var flexLedgerNotes = map[string]bool{
	"": true,
	"a later transfer in the same interval was written first; its equity_before omits this one and is not revised":    true,
	"an earlier transfer in the same interval was written first; its equity_before omits this one and is not revised": true,
}

// DecodeFlexLedger reads the Flex reader's ledger, raw, one record a line,
// each checked as the reader writes it (adapter/ibkr/flex.py read_ledger):
// exactly its fields, account.cash-movement at version 2 from ibkr-flex, a
// payload valid at that version with no field it does not write, its ID the
// prefix and the payload's transaction ID, its event time the payload's as
// of, and no ID twice. The ledger is refused whole, naming the line, when one
// line is not so, and when it does not end with a whole line: a cash movement
// the engine cannot read is never skipped (ADR 0019, "Explained means").
func DecodeFlexLedger(raw []byte) ([]FlexLedgerRecord, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != '\n' {
		return nil, errors.New("the Flex ledger does not end with a whole line")
	}
	lines := bytes.Split(raw[:len(raw)-1], []byte("\n"))
	records := make([]FlexLedgerRecord, 0, len(lines))
	seen := map[string]bool{}
	for n, line := range lines {
		r, err := decodeFlexLedgerLine(line)
		if err == nil && seen[r.ID] {
			err = fmt.Errorf("%s is written twice", r.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("the Flex ledger line %d is not a cash movement as the reader writes it: %w", n+1, err)
		}
		seen[r.ID] = true
		records = append(records, r)
	}
	return records, nil
}

func decodeFlexLedgerLine(line []byte) (FlexLedgerRecord, error) {
	var l flexLedgerLine
	if err := strictJSON(line, &l); err != nil {
		return FlexLedgerRecord{}, err
	}
	if l.Type != CashMovementEventType || l.SchemaVersion != CashMovementTransferVersion || l.Source != FlexSource {
		return FlexLedgerRecord{}, fmt.Errorf("it is %s version %d from %q, not %s version %d from %s", l.Type, l.SchemaVersion, l.Source, CashMovementEventType, CashMovementTransferVersion, FlexSource)
	}
	if !flexLedgerNotes[l.Note] {
		return FlexLedgerRecord{}, fmt.Errorf("note %q is not one the reader writes", l.Note)
	}
	var p CashMovementPayload
	if err := strictJSON(l.Payload, &p); err != nil {
		return FlexLedgerRecord{}, fmt.Errorf("its payload: %w", err)
	}
	if err := p.ValidateSchema(CashMovementTransferVersion); err != nil {
		return FlexLedgerRecord{}, err
	}
	if l.ID != FlexLedgerIDPrefix+p.TransactionID {
		return FlexLedgerRecord{}, fmt.Errorf("its ID %q is not %s and its transaction ID %q", l.ID, FlexLedgerIDPrefix, p.TransactionID)
	}
	if !l.EventTime.Equal(p.AsOf) {
		return FlexLedgerRecord{}, fmt.Errorf("its event time %s is not its as of %s", l.EventTime.UTC().Format(time.RFC3339), p.AsOf.UTC().Format(time.RFC3339))
	}
	return FlexLedgerRecord{ID: l.ID, Payload: p}, nil
}

// strictJSON decodes one JSON object into v, refusing a field v does not
// name and anything after the object.
func strictJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// FlexStatusEventType identifies the Flex reader's latest status, as the
// engine read it from the status file beside the ledger
// (adapter/ibkr/flex.py STATUS_SUFFIX): whether the reader's last reading
// read IBKR's statement, and the last day every reading so far covered. The
// reconciliation reads it to tell a cash difference the reader has not yet
// had the chance to explain from one it has (ADR 0019, amended 2026-10-21,
// "a cash difference the reader has not yet read").
const FlexStatusEventType = "account.flex-status"

// FlexStatusSchemaVersion is the current schema version of FlexStatusPayload.
const FlexStatusSchemaVersion uint32 = 1

// The states the reader's status file names (docs/manual/reconciliation.md,
// "The status file").
const (
	FlexStateRunning           = "running"
	FlexStateRead              = "read"
	FlexStateSetAside          = "set-aside"
	FlexStateCoverageGap       = "coverage-gap"
	FlexStateAmbiguousTransfer = "ambiguous-transfer"
)

// FlexStatusPayload is the Flex reader's status for AccountID's ledger:
// State, the time its reading started (CheckedAt, a whole second) and, for
// a reading that read, CoveredTo, the last complete New York day every
// reading so far has covered, as yyyymmdd.
type FlexStatusPayload struct {
	AccountID string    `json:"account_id"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checked_at"`
	CoveredTo string    `json:"covered_to,omitempty"`
}

// Validate checks the account, a state the reader writes, the time, and
// that CoveredTo is a yyyymmdd date exactly when the reading read.
func (p FlexStatusPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	switch p.State {
	case FlexStateRead:
		if _, err := time.Parse("20060102", p.CoveredTo); err != nil {
			errs = append(errs, fmt.Errorf("a reading that read states the day it covered to as yyyymmdd, not %q", p.CoveredTo))
		}
	case FlexStateRunning, FlexStateSetAside, FlexStateCoverageGap, FlexStateAmbiguousTransfer:
		if p.CoveredTo != "" {
			errs = append(errs, fmt.Errorf("a %s status states no day covered", p.State))
		}
	default:
		errs = append(errs, fmt.Errorf("state %q is not one the Flex reader writes", p.State))
	}
	errs = append(errs, wholeSecondTime("checked at", p.CheckedAt)...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid Flex status payload: %w", err)
	}
	return nil
}

// Covers reports whether the reader has read IBKR's statement through the
// New York day of at: its last reading read, and covered that day.
func (p FlexStatusPayload) Covers(at time.Time) bool {
	return p.State == FlexStateRead && p.CoveredTo >= at.In(nysecalendar.NewYork).Format("20060102")
}

// DecodeFlexStatus reads the reader's status file, raw, as accountID's
// status: its state, its checked_at and, when it read, its covered_to. The
// other fields the reader writes (counts, the period, a reason) are for the
// owner and are not read.
func DecodeFlexStatus(raw []byte, accountID string) (FlexStatusPayload, error) {
	var f struct {
		State     string    `json:"state"`
		CheckedAt time.Time `json:"checked_at"`
		CoveredTo string    `json:"covered_to"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &f); err != nil {
		return FlexStatusPayload{}, fmt.Errorf("the Flex status is not as the reader writes it: %w", err)
	}
	p := FlexStatusPayload{AccountID: accountID, State: f.State, CheckedAt: f.CheckedAt, CoveredTo: strings.TrimSpace(f.CoveredTo)}
	if err := p.Validate(); err != nil {
		return FlexStatusPayload{}, err
	}
	return p, nil
}
