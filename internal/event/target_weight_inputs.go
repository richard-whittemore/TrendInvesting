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
// broker reports one account holds, and its net liquidation, at AsOf, a
// whole second.
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
	errs = append(errs, wholeSecondTime("as of", p.AsOf)...)
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

// MarkRecordedSchemaVersion is version 1 of MarkRecordedPayload.
const MarkRecordedSchemaVersion uint32 = 1

// MarkRecordedEvidenceVersion is version 2 of MarkRecordedPayload, the
// version the IBKR adapter writes: version 1 with BrokerLast and
// BrokerLastSet. Under the owner's decision of 2026-10-18 (ADR 0026 point 1,
// amended) the mark is the delivered bar's official close, the price the
// backtest marks at, and IBKR's last price is recorded beside it. Nothing
// is sized from IBKR's price, but the gate's price band compares a checked
// order's reference price with it when it is set, and with the official
// close otherwise (ADR 0026 point 3, amended 2026-10-18), so the band has
// teeth only when IBKR gives a price; no decision needs IBKR market-data
// permissions.
const MarkRecordedEvidenceVersion uint32 = 2

// MarkRecordedContractVersion is version 3 of MarkRecordedPayload, the
// version the IBKR adapter writes for an IRA: version 2 with Contract, the
// IBKR contract the mark is for, as the adapter resolved it by name with
// IBKR's contract details (ADR 0026 point 3). The compact IRA stream carries
// no bar for a stock newly picked, so the adapter marks every ranked member
// and states the contract it resolved for each; a name it cannot resolve to
// one US-dollar stock on SMART gets no mark, and the gate refuses any order
// for it (missing-mark). The price band reads it as version 2.
const MarkRecordedContractVersion uint32 = 3

// MarkRecordedVersions returns the versions a mark is read at.
func MarkRecordedVersions() []uint32 {
	return []uint32{MarkRecordedSchemaVersion, MarkRecordedEvidenceVersion, MarkRecordedContractVersion}
}

// MarkContract is the IBKR contract a version-3 mark is for: IBKR's contract
// ID, the SMART routing it was resolved on, its primary exchange and its
// currency (US dollars: the one kind of contract the engine trades).
type MarkContract struct {
	ConID           int64  `json:"con_id"`
	Exchange        string `json:"exchange"`
	PrimaryExchange string `json:"primary_exchange"`
	Currency        string `json:"currency"`
}

// Validate checks the contract is a positive IBKR contract ID for a
// US-dollar contract routed by SMART with its primary exchange named.
func (c MarkContract) Validate() error {
	var errs []error
	if c.ConID <= 0 {
		errs = append(errs, errors.New("contract con_id must be IBKR's positive contract ID"))
	}
	if c.Exchange != "SMART" {
		errs = append(errs, fmt.Errorf("contract exchange %q is not SMART", c.Exchange))
	}
	if c.PrimaryExchange == "" {
		errs = append(errs, errors.New("contract primary exchange is required"))
	}
	if c.Currency != "USD" {
		errs = append(errs, fmt.Errorf("contract currency %q is not USD", c.Currency))
	}
	return errors.Join(errs...)
}

// MarkRecordedPayload is one instrument's mark at AsOf, as Source reported it.
type MarkRecordedPayload struct {
	InstrumentID string    `json:"instrument_id"`
	AsOf         time.Time `json:"as_of"`
	Price        float64   `json:"price"`
	Source       string    `json:"source"`
	// BrokerLast, from version 2, is IBKR's last price for the instrument
	// read beside the mark, the price band's comparison when set (see
	// MarkRecordedEvidenceVersion); BrokerLastSet is false, and
	// BrokerLast zero, when IBKR gave none. Version 1 carries neither.
	BrokerLast    float64 `json:"broker_last,omitempty"`
	BrokerLastSet bool    `json:"broker_last_set,omitempty"`
	// Contract, at version 3 only, is the IBKR contract the mark is for
	// (MarkRecordedContractVersion); nil at versions 1 and 2.
	Contract *MarkContract `json:"contract,omitempty"`
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
	switch {
	case !isFinite(p.BrokerLast):
		errs = append(errs, errors.New("broker last must be finite"))
	case p.BrokerLastSet && p.BrokerLast <= 0:
		errs = append(errs, errors.New("a broker last that is set must be positive"))
	case !p.BrokerLastSet && p.BrokerLast != 0:
		errs = append(errs, errors.New("broker last carries a value but none is set"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid mark recorded payload: %w", err)
	}
	return nil
}

// ValidateSchema is Validate at the version the mark arrived at: version 1
// carries no broker evidence (MarkRecordedEvidenceVersion), and only version
// 3 carries, and must carry, a contract (MarkRecordedContractVersion).
func (p MarkRecordedPayload) ValidateSchema(version uint32) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if version != MarkRecordedContractVersion && p.Contract != nil {
		return fmt.Errorf("invalid mark recorded payload: version %d carries no contract; that is version 3's", version)
	}
	switch version {
	case MarkRecordedSchemaVersion:
		if p.BrokerLastSet || p.BrokerLast != 0 {
			return errors.New("invalid mark recorded payload: version 1 carries no broker last; that is version 2's")
		}
	case MarkRecordedEvidenceVersion:
	case MarkRecordedContractVersion:
		if p.Contract == nil {
			return errors.New("invalid mark recorded payload: version 3 states the contract the mark is for")
		}
		if err := p.Contract.Validate(); err != nil {
			return fmt.Errorf("invalid mark recorded payload: %w", err)
		}
	default:
		return fmt.Errorf("invalid mark recorded payload: schema version %d is not one of %v", version, MarkRecordedVersions())
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

// wholeSecondTime is requiredTime for a time the risk gate orders by the
// whole second IBKR stamps it with: a snapshot's as-of time or a fill's
// execution time. Finer precision would claim an order within the second
// that the broker never showed, so it is refused at the input (ADR 0015:
// validated on the way in; fail closed).
func wholeSecondTime(name string, t time.Time) []error {
	if errs := requiredTime(name, t); errs != nil {
		return errs
	}
	if t.Nanosecond() != 0 {
		return []error{fmt.Errorf("%s must be a whole second; IBKR stamps no finer", name)}
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
