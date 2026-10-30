package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// OrderSkippedEventType identifies a trade the order planner sized but could
// not order: whole shares rounded it to nothing (ADR 0026, point 2, D7-L: "the
// planner then rounds toward zero to whole shares"), fitting it to the budget
// scaled it below one share, or, for a sell, its whole holding is already
// committed to working sells. It is recorded so no planned trade disappears
// silently: the journal says which instrument, how many shares the rule asked
// for, and why none is ordered.
const OrderSkippedEventType = "strategy.order.skipped"

// OrderSkippedSchemaVersion is the current schema version of OrderSkippedPayload.
const OrderSkippedSchemaVersion uint32 = 1

// The closed set of reasons a trade is skipped.
const (
	// OrderSkippedBelowOneShare: the trade the planner sized, past the
	// rebalance band, is less than one share, so rounding toward zero leaves
	// none.
	OrderSkippedBelowOneShare = "below-one-share"
	// OrderSkippedScaledBelowOneShare: a buy fitted to the budget (ADR 0026,
	// point 2, "Scaling") is left with less than one share.
	OrderSkippedScaledBelowOneShare = "scaled-below-one-share"
	// OrderSkippedHeldByWorkingSells: a sell finds its whole holding already
	// committed to working or pending sells.
	OrderSkippedHeldByWorkingSells = "held-by-working-sells"
)

// OrderSkippedPayload is one skipped trade: the order it would have been,
// its size in Shares as the planner computed it, a float64 recorded without
// rounding (the budget's Scale applied for a scaled buy, Scale 0 otherwise)
// and the figures it was sized from.
type OrderSkippedPayload struct {
	QuantityScale int64 `json:"quantity_scale,omitempty"`
	// RequestedValue is D7-F's exact decimal monetary request before budget
	// scaling; Scale records the applied dimensionless ratio for a scaled
	// skip (ADR 0026, D7-F and "Scaling"). No share quantity is a float.
	RequestedValue      string  `json:"requested_value,omitempty"`
	FractionalIncrement string  `json:"fractional_increment,omitempty"`
	AccountID           string  `json:"account_id"`
	Tag                 string  `json:"tag"`
	ClientOrderID       string  `json:"client_order_id"`
	InstrumentID        string  `json:"instrument_id"`
	Side                string  `json:"side"`
	Phase               string  `json:"phase"`
	Reason              string  `json:"reason"`
	Shares              float64 `json:"shares"`
	Scale               float64 `json:"scale,omitzero"`
	TargetWeight        float64 `json:"target_weight"`
	NetLiquidation      float64 `json:"net_liquidation"`
	HeldQuantity        int64   `json:"held_quantity"`
	ReferencePrice      float64 `json:"reference_price"`
}

// Validate checks identity, the reason and the figures.
func (p OrderSkippedPayload) Validate() error {
	if err := quantityScaleError(p.QuantityScale); err != nil {
		return err
	}
	if p.QuantityScale != 0 {
		if _, err := FractionalIncrementTicks(p.FractionalIncrement); err != nil {
			return err
		}
		if !plainDecimal(p.RequestedValue) {
			return errors.New("requested value must be a nonnegative decimal")
		}
		value, ok := new(big.Rat).SetString(p.RequestedValue)
		if !ok || value.Sign() < 0 {
			return errors.New("requested value must be a nonnegative decimal")
		}
		if p.Shares != 0 {
			return errors.New("fractional skip records requested value, never floating-point shares")
		}
	} else if p.RequestedValue != "" || p.FractionalIncrement != "" || p.Reason == OrderSkippedBelowIncrement || p.Reason == OrderSkippedScaledBelowIncrement {
		return errors.New("fractional skip fields require fractional quantity scale")
	}

	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, 1, 1)...)
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: a skipped trade served a journalled decision"))
	}
	if p.Phase != p.Side {
		errs = append(errs, fmt.Errorf("phase %q is not the side %q", p.Phase, p.Side))
	}
	switch p.Reason {
	case OrderSkippedBelowOneShare, OrderSkippedBelowIncrement, OrderSkippedHeldByWorkingSells:
		if p.Scale != 0 {
			errs = append(errs, errors.New("only a scaled buy carries a scale"))
		}
	case OrderSkippedScaledBelowOneShare, OrderSkippedScaledBelowIncrement:
		if p.Side != OrderSideBuy || !(p.Scale >= 0 && p.Scale < 1) {
			errs = append(errs, errors.New("a scaled skip is a buy with a scale in [0, 1)"))
		}
	default:
		errs = append(errs, fmt.Errorf("skip reason %q is not recognised", p.Reason))
	}
	if !isFinite(p.Shares) || p.Shares < 0 || (p.Reason != OrderSkippedHeldByWorkingSells && p.Shares >= 1) {
		errs = append(errs, errors.New("shares must be finite, not negative, and below one share unless held by working sells"))
	}
	if !isFinite(p.TargetWeight) || p.TargetWeight < 0 {
		errs = append(errs, errors.New("target weight must be finite and not negative"))
	}
	if !isFinite(p.NetLiquidation) || p.NetLiquidation <= 0 || !isFinite(p.ReferencePrice) || p.ReferencePrice <= 0 {
		errs = append(errs, errors.New("net liquidation and reference price must be finite and positive"))
	}
	if p.HeldQuantity < 0 {
		errs = append(errs, errors.New("held quantity must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order skipped payload: %w", err)
	}
	return nil
}

// SkippedSharesDecimals is the most decimals a skipped trade's shares are
// shown with.
const SkippedSharesDecimals = 6

// FormatSkippedShares is the one text of a skipped trade's Shares, used by
// the summary, the dashboard and the journal query. The record keeps the
// planner's float64 unrounded (ADR 0026, point 8), so a sum such as 0.1+0.2
// would print as 0.30000000000000004; the text is bounded to
// SkippedSharesDecimals decimals with trailing zeros dropped. A non-zero size
// that the six decimals would show as zero is printed in scientific notation
// with three significant digits instead, so a skipped trade never reads as
// "0 shares wanted"; negative zero is shown as "0". Only the text is bounded;
// the record is untouched.
func FormatSkippedShares(v float64) string {
	if v == 0 {
		return "0"
	}
	s := strconv.FormatFloat(v, 'f', SkippedSharesDecimals, 64)
	if strings.Trim(s, "-0.") == "" {
		return strconv.FormatFloat(v, 'g', 3, 64)
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// SharesText is FormatSkippedShares of the record's own Shares.
func (p OrderSkippedPayload) SharesText() string { return FormatSkippedShares(p.Shares) }

// Fractional skips retain the unrounded monetary request and the increment,
// never a binary floating-point quantity (ADR 0015; ADR 0026, D7-F).
const OrderSkippedFractionalSchemaVersion uint32 = 2
const OrderSkippedBelowIncrement = "below-increment"
const OrderSkippedScaledBelowIncrement = "scaled-below-increment"

// SchemaVersion states this skip's quantity representation (ADR 0015).
func (p OrderSkippedPayload) SchemaVersion() uint32 {
	if p.QuantityScale != 0 {
		return OrderSkippedFractionalSchemaVersion
	}
	return OrderSkippedSchemaVersion
}

// OrderSkippedVersions includes legacy skipped whole-share trades (ADR 0015).
func OrderSkippedVersions() []uint32 { return []uint32{1, 2} }

// ValidateSchema refuses a quantity-unit mismatch (ADR 0015).
func (p OrderSkippedPayload) ValidateSchema(v uint32) error {
	if v < 1 || v > 2 {
		return fmt.Errorf("unsupported skip version %d", v)
	}
	if err := quantitySchema(p.QuantityScale, v, 2); err != nil {
		return err
	}
	return p.Validate()
}

// MarshalJSON preserves version 1's bytes and omits its floating-point Shares
// field from the fractional shape, which records RequestedValue (ADR 0015).
func (p OrderSkippedPayload) MarshalJSON() ([]byte, error) {
	type plain OrderSkippedPayload
	if p.QuantityScale == 0 {
		return json.Marshal(plain(p))
	}
	return json.Marshal(struct {
		plain
		Shares *float64 `json:"shares,omitempty"`
	}{plain: plain(p)})
}

// plainDecimal accepts only digits, one optional point and an optional leading
// minus (ADR 0026, D7-F's exact decimal monetary evidence). SetString alone
// also accepts base prefixes and underscores, which are not wire decimals.
func plainDecimal(value string) bool {
	value = strings.TrimPrefix(value, "-")
	digits, points := 0, 0
	for _, ch := range value {
		switch {
		case ch >= '0' && ch <= '9':
			digits++
		case ch == '.':
			points++
		default:
			return false
		}
	}
	return digits > 0 && points <= 1
}
