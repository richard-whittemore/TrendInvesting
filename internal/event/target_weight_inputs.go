package event

import (
	"errors"
	"fmt"
	"time"
)

// AccountSnapshotPositionsVersion is version 3 of account.snapshot
// (AccountSnapshotEventType): the broker's positions and net liquidation for
// one account, which the target-weight order planner values orders against
// (ADR 0026, point 2 and Consequences).
//
//   - Version 3 replaces version 2's Equity and AvailableCash with AccountID,
//     NetLiquidation and Positions. Version 2 stays what the Turtle reducer
//     reads (AccountSnapshotSchemaVersion); each consumer refuses the other's
//     version rather than reading a figure it does not carry (ADR 0015).
const AccountSnapshotPositionsVersion uint32 = 3

// AccountPosition is one holding as the broker reports it, in raw shares.
type AccountPosition struct {
	InstrumentID string `json:"instrument_id"`
	Quantity     int64  `json:"quantity"`
}

// AccountPositionsSnapshotPayload is account.snapshot version 3: what the
// broker reports one account holds, and its net liquidation, at AsOf.
type AccountPositionsSnapshotPayload struct {
	AccountID string    `json:"account_id"`
	AsOf      time.Time `json:"as_of"`
	// NetLiquidation is V in ADR 0026's planner, as of the previous close.
	NetLiquidation float64 `json:"net_liquidation"`
	Currency       string  `json:"currency"`
	// Positions is sorted strictly ascending by instrument, one encoding per
	// holding set; an instrument not listed is not held. A zero quantity is
	// refused: an empty holding is stated by absence, never by a zero row.
	Positions []AccountPosition `json:"positions"`
}

// Quantity returns the quantity held in instrumentID, or 0 when not held.
func (p AccountPositionsSnapshotPayload) Quantity(instrumentID string) int64 {
	for _, position := range p.Positions {
		if position.InstrumentID == instrumentID {
			return position.Quantity
		}
	}
	return 0
}

// Validate checks the account, time, net liquidation, currency and positions.
func (p AccountPositionsSnapshotPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	if !isFinite(p.NetLiquidation) || p.NetLiquidation <= 0 {
		errs = append(errs, errors.New("net liquidation must be finite and positive"))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	for i, position := range p.Positions {
		if position.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("position %d: instrument id is required", i+1))
			continue
		}
		if position.Quantity == 0 {
			errs = append(errs, fmt.Errorf("position %s: quantity must not be zero; an empty holding is stated by absence", position.InstrumentID))
		}
		if i > 0 && position.InstrumentID <= p.Positions[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("positions must be strictly ascending by instrument; %q follows %q", position.InstrumentID, p.Positions[i-1].InstrumentID))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid account positions snapshot payload: %w", err)
	}
	return nil
}

// MarkRecordedEventType identifies a recorded broker mark: the price the
// risk gate's price band is checked against (ADR 0026, point 3). It is an
// input, never a live read.
const MarkRecordedEventType = "market.mark.recorded"

// MarkRecordedSchemaVersion is the current schema version of MarkRecordedPayload.
const MarkRecordedSchemaVersion uint32 = 1

// MarkRecordedPayload is one instrument's mark at AsOf, as Source reported it.
type MarkRecordedPayload struct {
	InstrumentID string    `json:"instrument_id"`
	AsOf         time.Time `json:"as_of"`
	Price        float64   `json:"price"`
	Source       string    `json:"source"`
}

// Validate checks the instrument, time, price and source.
func (p MarkRecordedPayload) Validate() error {
	var errs []error
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	if !isFinite(p.Price) || p.Price <= 0 {
		errs = append(errs, errors.New("price must be finite and positive"))
	}
	if p.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid mark recorded payload: %w", err)
	}
	return nil
}

// requiredTime reports a missing or unwritable required timestamp.
func requiredTime(name string, t time.Time) []error {
	switch {
	case t.IsZero():
		return []error{fmt.Errorf("%s is required", name)}
	case !writableTime(t):
		return []error{fmt.Errorf("%s cannot be written as RFC 3339", name)}
	}
	return nil
}

// ascendingIDs reports an empty or out-of-order ID in a list that must be
// strictly ascending.
func ascendingIDs(name string, ids []string) []error {
	var errs []error
	for i, id := range ids {
		if id == "" {
			errs = append(errs, fmt.Errorf("%s %d is empty", name, i+1))
			continue
		}
		if i > 0 && id <= ids[i-1] {
			errs = append(errs, fmt.Errorf("%s must be strictly ascending; %q follows %q", name, id, ids[i-1]))
		}
	}
	return errs
}
