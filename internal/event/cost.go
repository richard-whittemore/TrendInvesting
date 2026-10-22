package event

import (
	"errors"
	"fmt"
	"time"
)

// CostEventType identifies a cost the account paid or received: a
// commission, a fee, or broker interest. The IBKR Flex reader
// (adapter/ibkr/flex.py) writes one per posting to the costs ledger beside
// its cash-movements ledger; the reporter and the dashboard total them
// (internal/report, Costs). No engine reads them and none can place an
// order.
const CostEventType = "account.cost"

// CostSchemaVersion is version 1 of CostPayload.
const CostSchemaVersion uint32 = 1

// The kinds of cost (adapter/ibkr/flex.py, COST_KINDS).
const (
	CostCommission       = "commission"
	CostFee              = "fee"
	CostInterestPaid     = "interest-paid"
	CostInterestReceived = "interest-received"
)

// CostKinds returns the kinds ValidateSchema accepts, in the order a total
// states them.
func CostKinds() []string {
	return []string{CostCommission, CostFee, CostInterestPaid, CostInterestReceived}
}

// CostPayload is one posting the broker made to the account as a cost.
type CostPayload struct {
	// Kind is one of CostKinds.
	Kind string `json:"kind"`
	// Amount is as IBKR states the posting: negative is money out of the
	// account (a commission, a fee, interest paid), positive money in
	// (interest received, or a commission refunded). Finite and non-zero; the
	// costs ledger also holds a zero on a restatement line, which reverses the
	// earlier amount (internal/report, ParseCostLedger).
	Amount float64 `json:"amount"`
	// Currency is the currency Amount is stated in.
	Currency string `json:"currency"`
	// AsOf is when the broker dated the posting.
	AsOf time.Time `json:"as_of"`
	// TransactionID is IBKR's own ID for the posting, unique per account:
	// the reader's key, and the key a commission is matched to a journalled
	// fill by when the broker's ID is that fill's execution ID.
	TransactionID string `json:"transaction_id"`
}

// ValidateSchema checks the cost at the version it arrived at.
func (p CostPayload) ValidateSchema(version uint32) error {
	var errs []error
	if version != CostSchemaVersion {
		errs = append(errs, fmt.Errorf("schema version %d is not %d", version, CostSchemaVersion))
	}
	known := false
	for _, k := range CostKinds() {
		known = known || p.Kind == k
	}
	if !known {
		errs = append(errs, fmt.Errorf("kind %q is not one of %v", p.Kind, CostKinds()))
	}
	switch {
	case !isFinite(p.Amount):
		errs = append(errs, errors.New("amount must be finite"))
	case p.Amount == 0:
		errs = append(errs, errors.New("amount must be non-zero"))
	}
	if p.Currency == "" {
		errs = append(errs, errors.New("currency is required"))
	}
	switch {
	case p.AsOf.IsZero():
		errs = append(errs, errors.New("as of is required"))
	case !writableTime(p.AsOf):
		errs = append(errs, errors.New("as of cannot be written as RFC 3339"))
	}
	if p.TransactionID == "" {
		errs = append(errs, errors.New("transaction id is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid cost payload: %w", err)
	}
	return nil
}

// Validate is ValidateSchema at CostSchemaVersion.
func (p CostPayload) Validate() error { return p.ValidateSchema(CostSchemaVersion) }
