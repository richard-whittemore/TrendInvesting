package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// KillSwitchEventType identifies the operator's kill switch for one account
// (ADR 0026, point 3; docs/live-trading/plan.md section 5, "Kill switch"). It
// is a recorded input, never a live read of a file or command.
const KillSwitchEventType = "operator.kill-switch"

// KillSwitchSchemaVersion is the current schema version of KillSwitchPayload.
const KillSwitchSchemaVersion uint32 = 1

// KillSwitchPayload engages or releases the kill switch. While engaged the
// gate refuses every order except ADR 0019's one sale back to 98% in a
// margin account; it changes no engine state (owner decision 2026-10-16).
type KillSwitchPayload struct {
	AccountID string    `json:"account_id"`
	AsOf      time.Time `json:"as_of"`
	Engaged   bool      `json:"engaged"`
	// Operator names who set the switch; Detail says how and why.
	Operator string `json:"operator"`
	Detail   string `json:"detail"`
}

// UnmarshalJSON decodes the payload and requires engaged to be stated as a
// boolean: an input that omits it, or sets it null, would otherwise read as
// false and release a switch that refuses every order but the sale (ADR
// 0026, point 3). Only an explicit false releases it.
func (p *KillSwitchPayload) UnmarshalJSON(raw []byte) error {
	type plain KillSwitchPayload
	var stated struct {
		plain
		Engaged *bool `json:"engaged"`
	}
	if err := json.Unmarshal(raw, &stated); err != nil {
		return err
	}
	if stated.Engaged == nil {
		return errors.New("kill switch payload: engaged must be stated as true or false")
	}
	*p = KillSwitchPayload(stated.plain)
	p.Engaged = *stated.Engaged
	return nil
}

// Validate checks the account, time, operator and detail.
func (p KillSwitchPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	if p.Operator == "" {
		errs = append(errs, errors.New("operator is required"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid kill switch payload: %w", err)
	}
	return nil
}

// BrokerSnapshotEventType identifies one IBKR account snapshot: holdings with
// their marks, net liquidation, Excess Liquidity and every open order, taken
// together at one time. It is the broker observation ADR 0019's one sale back
// to 98% is sized from (amended 2026-10-15, "One price basis"), the reading
// the margin-cushion alarm watches (plan.md section 5), and the broker
// quantity a Degraded account's sells are sized within (ADR 0019, amended
// 2026-09-24). It is an input: replay never asks the broker again.
const BrokerSnapshotEventType = "account.broker.snapshot"

// BrokerSnapshotSchemaVersion is the current schema version of BrokerSnapshotPayload.
const BrokerSnapshotSchemaVersion uint32 = 1

// The components of a broker snapshot that can be unreadable. An unreadable
// component is not zero (ADR 0019, "An unavailable balance is not zero"): it
// carries no value, and a sale that needs it is not sent.
const (
	BrokerComponentExcessLiquidity = "excess-liquidity"
	BrokerComponentNetLiquidation  = "net-liquidation"
	BrokerComponentOpenOrders      = "open-orders"
	BrokerComponentPositions       = "positions"
)

// The statuses of an open broker order that can still execute. A
// cancel-pending order may still fill, so it counts as working (ADR 0026,
// point 2.5).
const (
	BrokerOrderWorking       = "working"
	BrokerOrderPending       = "pending"
	BrokerOrderCancelPending = "cancel-pending"
)

// BrokerPosition is one holding as IBKR reports it, in raw shares, with
// IBKR's current mark for it from the same snapshot.
type BrokerPosition struct {
	InstrumentID string  `json:"instrument_id"`
	Quantity     int64   `json:"quantity"`
	Mark         float64 `json:"mark"`
	MarkSet      bool    `json:"mark_set"`
}

// BrokerOrder is one open order IBKR reports, ours or unknown. ClientOrderID
// is empty when IBKR reports none; an order is recognised only when the
// journal released its client ID, which the gate decides, never the producer.
type BrokerOrder struct {
	BrokerOrderID     string `json:"broker_order_id"`
	ClientOrderID     string `json:"client_order_id"`
	InstrumentID      string `json:"instrument_id"`
	Side              string `json:"side"`
	Status            string `json:"status"`
	RemainingQuantity int64  `json:"remaining_quantity"`
}

// BrokerSnapshotPayload is one IBKR account snapshot at AsOf.
type BrokerSnapshotPayload struct {
	AccountID string    `json:"account_id"`
	AsOf      time.Time `json:"as_of"`
	// MarketOpen is the producer's record that the market was open at AsOf;
	// the alarm counts a missing reading as urgent only while it is. It must
	// be stated (UnmarshalJSON).
	MarketOpen bool `json:"market_open"`
	// Unreadable lists, strictly ascending, the components IBKR could not
	// supply; each carries no value.
	Unreadable      []string `json:"unreadable"`
	NetLiquidation  float64  `json:"net_liquidation"`
	ExcessLiquidity float64  `json:"excess_liquidity"`
	// Positions is strictly ascending by instrument, with no zero rows; an
	// instrument not listed is not held. A readable list is stated, empty as
	// []: a missing or null list is no reading.
	Positions []BrokerPosition `json:"positions"`
	// OpenOrders is strictly ascending by broker order ID. A readable list is
	// stated, empty as []: a missing or null list is no reading, and a list
	// IBKR could not supply is marked unreadable, never sent as none.
	OpenOrders []BrokerOrder `json:"open_orders"`
}

// UnmarshalJSON decodes the snapshot and requires market_open to be stated as
// a boolean: omitted or null it would read as false, a closed market, and
// silence the alarm's missing-reading rule (plan.md section 5). A missing or
// null open_orders or positions decodes as unstated, which Validate refuses
// unless that component is marked unreadable (ADR 0019, "An unavailable balance is not
// zero").
func (p *BrokerSnapshotPayload) UnmarshalJSON(raw []byte) error {
	type plain BrokerSnapshotPayload
	var stated struct {
		plain
		MarketOpen *bool `json:"market_open"`
	}
	if err := json.Unmarshal(raw, &stated); err != nil {
		return err
	}
	if stated.MarketOpen == nil {
		return errors.New("broker snapshot payload: market_open must be stated as true or false")
	}
	*p = BrokerSnapshotPayload(stated.plain)
	p.MarketOpen = *stated.MarketOpen
	return nil
}

// Readable reports whether component was read.
func (p BrokerSnapshotPayload) Readable(component string) bool {
	return !slices.Contains(p.Unreadable, component)
}

// Position returns the holding of instrumentID and whether one is listed.
func (p BrokerSnapshotPayload) Position(instrumentID string) (BrokerPosition, bool) {
	for _, position := range p.Positions {
		if position.InstrumentID == instrumentID {
			return position, true
		}
	}
	return BrokerPosition{}, false
}

// Validate checks identity, each component's value against its readability,
// the positions and the open orders.
func (p BrokerSnapshotPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	for i, c := range p.Unreadable {
		switch c {
		case BrokerComponentExcessLiquidity, BrokerComponentNetLiquidation, BrokerComponentOpenOrders, BrokerComponentPositions:
		default:
			errs = append(errs, fmt.Errorf("unreadable component %q is not recognised", c))
		}
		if i > 0 && c <= p.Unreadable[i-1] {
			errs = append(errs, fmt.Errorf("unreadable components must be strictly ascending; %q follows %q", c, p.Unreadable[i-1]))
		}
	}
	if p.Readable(BrokerComponentNetLiquidation) {
		// Recorded as IBKR reports it, even when not positive: the gate
		// refuses to size anything from such a value (ADR 0019: never short).
		if !isFinite(p.NetLiquidation) {
			errs = append(errs, errors.New("net liquidation must be finite"))
		}
	} else if p.NetLiquidation != 0 {
		errs = append(errs, errors.New("an unreadable net liquidation carries no value"))
	}
	if p.Readable(BrokerComponentExcessLiquidity) {
		if !isFinite(p.ExcessLiquidity) {
			errs = append(errs, errors.New("excess liquidity must be finite"))
		}
	} else if p.ExcessLiquidity != 0 {
		errs = append(errs, errors.New("an unreadable excess liquidity carries no value"))
	}
	switch {
	case !p.Readable(BrokerComponentPositions) && len(p.Positions) != 0:
		errs = append(errs, errors.New("unreadable positions carry no rows"))
	case p.Readable(BrokerComponentPositions) && p.Positions == nil:
		errs = append(errs, errors.New("readable positions must be stated, [] when none are held; an unavailable list is marked unreadable"))
	}
	switch {
	case !p.Readable(BrokerComponentOpenOrders) && len(p.OpenOrders) != 0:
		errs = append(errs, errors.New("unreadable open orders carry no rows"))
	case p.Readable(BrokerComponentOpenOrders) && p.OpenOrders == nil:
		errs = append(errs, errors.New("readable open orders must be stated, [] when none work; an unavailable list is marked unreadable"))
	}
	for i, position := range p.Positions {
		if position.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("position %d: instrument id is required", i+1))
			continue
		}
		if position.Quantity == 0 {
			errs = append(errs, fmt.Errorf("position %s: quantity must not be zero; an empty holding is stated by absence", position.InstrumentID))
		}
		switch {
		case !isFinite(position.Mark):
			errs = append(errs, fmt.Errorf("position %s: mark must be finite", position.InstrumentID))
		case !position.MarkSet && position.Mark != 0:
			errs = append(errs, fmt.Errorf("position %s: mark carries a value but none is set", position.InstrumentID))
		}
		if i > 0 && position.InstrumentID <= p.Positions[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("positions must be strictly ascending by instrument; %q follows %q", position.InstrumentID, p.Positions[i-1].InstrumentID))
		}
	}
	for i, o := range p.OpenOrders {
		if o.BrokerOrderID == "" {
			errs = append(errs, fmt.Errorf("open order %d: broker order id is required", i+1))
		} else if i > 0 && o.BrokerOrderID <= p.OpenOrders[i-1].BrokerOrderID {
			errs = append(errs, fmt.Errorf("open orders must be strictly ascending by broker order id; %q follows %q", o.BrokerOrderID, p.OpenOrders[i-1].BrokerOrderID))
		}
		if o.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("open order %d: instrument id is required", i+1))
		}
		if o.Side != OrderSideBuy && o.Side != OrderSideSell {
			errs = append(errs, fmt.Errorf("open order %d: side %q is not buy or sell", i+1, o.Side))
		}
		switch o.Status {
		case BrokerOrderWorking, BrokerOrderPending, BrokerOrderCancelPending:
		default:
			errs = append(errs, fmt.Errorf("open order %d: status %q is not working, pending or cancel-pending", i+1, o.Status))
		}
		if o.RemainingQuantity <= 0 {
			errs = append(errs, fmt.Errorf("open order %d: remaining quantity must be positive", i+1))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid broker snapshot payload: %w", err)
	}
	return nil
}

// MarginAlarmEventType identifies a change in the joint account's
// margin-cushion alarm (plan.md section 5, "A margin-cushion alarm"): Excess
// Liquidity as a share of net liquidation, warning below 40%, urgent below
// 20%, and no reading for 30 minutes while the market is open counting as
// urgent. It is a decision, emitted only when the level changes.
const MarginAlarmEventType = "risk.margin.alarm"

// MarginAlarmSchemaVersion is the current schema version of MarginAlarmPayload.
const MarginAlarmSchemaVersion uint32 = 1

// The alarm's levels.
const (
	MarginAlarmClear   = "clear"
	MarginAlarmWarning = "warning"
	MarginAlarmUrgent  = "urgent"
)

// The alarm's causes: a reading's ratio; a reading whose net liquidation is
// zero or less, which gives no ratio but lies below every threshold, so it is
// urgent at once; or no reading for the stale period while the market is
// open.
const (
	MarginAlarmCauseRatio       = "ratio"
	MarginAlarmCauseNonPositive = "non-positive-net-liquidation"
	MarginAlarmCauseNoReading   = "no-reading"
)

// MarginAlarmPayload is the alarm's new level, the reading behind it and the
// configured thresholds, so the journal alone can check it.
type MarginAlarmPayload struct {
	AccountID string `json:"account_id"`
	// SnapshotID is the account.broker.snapshot envelope evaluated.
	SnapshotID string    `json:"snapshot_id"`
	AsOf       time.Time `json:"as_of"`
	Level      string    `json:"level"`
	Cause      string    `json:"cause"`
	// The reading, when the snapshot had one; Ratio is ExcessLiquidity over
	// NetLiquidation, and zero for a net liquidation that is not positive.
	ExcessLiquidity float64 `json:"excess_liquidity"`
	NetLiquidation  float64 `json:"net_liquidation"`
	Ratio           float64 `json:"ratio"`
	ReadingSet      bool    `json:"reading_set"`
	// LastReadingAt is the last snapshot with a reading, unset when the run
	// has had none.
	LastReadingAt  time.Time `json:"last_reading_at"`
	LastReadingSet bool      `json:"last_reading_set"`
	Warning        float64   `json:"warning"`
	Urgent         float64   `json:"urgent"`
	StaleMinutes   int       `json:"stale_minutes"`
	Detail         string    `json:"detail"`
}

// Validate checks identity, level and cause, the reading against the cause,
// and the thresholds.
func (p MarginAlarmPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.SnapshotID == "" {
		errs = append(errs, errors.New("snapshot id is required"))
	}
	errs = append(errs, requiredTime("as of", p.AsOf)...)
	switch p.Level {
	case MarginAlarmClear, MarginAlarmWarning, MarginAlarmUrgent:
	default:
		errs = append(errs, fmt.Errorf("level %q is not clear, warning or urgent", p.Level))
	}
	switch p.Cause {
	case MarginAlarmCauseRatio:
		if !p.ReadingSet {
			errs = append(errs, errors.New("a ratio alarm carries its reading"))
		}
	case MarginAlarmCauseNonPositive:
		if !p.ReadingSet {
			errs = append(errs, errors.New("a non-positive net liquidation alarm carries its reading"))
		}
		if p.Level != MarginAlarmUrgent {
			errs = append(errs, errors.New("a net liquidation that is not positive is urgent"))
		}
	case MarginAlarmCauseNoReading:
		if p.ReadingSet {
			errs = append(errs, errors.New("a no-reading alarm carries no reading"))
		}
		if p.Level != MarginAlarmUrgent {
			errs = append(errs, errors.New("no reading while the market is open is urgent"))
		}
	default:
		errs = append(errs, fmt.Errorf("cause %q is not ratio, non-positive-net-liquidation or no-reading", p.Cause))
	}
	for _, v := range []struct {
		name  string
		value float64
	}{{"excess liquidity", p.ExcessLiquidity}, {"net liquidation", p.NetLiquidation}, {"ratio", p.Ratio}} {
		if !isFinite(v.value) {
			errs = append(errs, fmt.Errorf("%s must be finite", v.name))
		}
	}
	switch {
	case !p.ReadingSet && (p.ExcessLiquidity != 0 || p.NetLiquidation != 0 || p.Ratio != 0):
		errs = append(errs, errors.New("a reading carries values only when set"))
	case p.Cause == MarginAlarmCauseNonPositive && p.NetLiquidation > 0:
		errs = append(errs, errors.New("a non-positive net liquidation alarm's net liquidation must be zero or less"))
	case p.Cause == MarginAlarmCauseNonPositive && p.Ratio != 0:
		errs = append(errs, errors.New("a net liquidation that is not positive gives no ratio"))
	case p.Cause == MarginAlarmCauseNonPositive:
	case p.ReadingSet && !(p.NetLiquidation > 0):
		errs = append(errs, errors.New("a ratio reading's net liquidation must be positive"))
	case p.ReadingSet && p.Ratio != p.ExcessLiquidity/p.NetLiquidation:
		// Excess Liquidity can be negative in a margin deficit, so the
		// ratio is checked by derivation, not by sign.
		errs = append(errs, errors.New("ratio must be excess liquidity over net liquidation"))
	}
	if p.LastReadingSet {
		if !writableTime(p.LastReadingAt) || p.LastReadingAt.IsZero() {
			errs = append(errs, errors.New("last reading at must be a writable time when set"))
		}
	} else if !p.LastReadingAt.IsZero() {
		errs = append(errs, errors.New("last reading at carries a value but none is set"))
	}
	if !isFinite(p.Warning) || !isFinite(p.Urgent) || !(0 < p.Urgent && p.Urgent < p.Warning && p.Warning < 1) {
		errs = append(errs, errors.New("thresholds must satisfy 0 < urgent < warning < 1"))
	}
	if p.StaleMinutes < 1 {
		errs = append(errs, errors.New("stale minutes must be at least 1"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid margin alarm payload: %w", err)
	}
	return nil
}

// The two kinds of sale back to 98% of net liquidation.
const (
	// SaleKindIncident is ADR 0019's one sale (amended 2026-10-15 and
	// 2026-10-16): attempted once per incident, tagged with the incident.
	SaleKindIncident = "incident"
	// SaleKindUrgent is ADR 0026's urgent Excess Liquidity sale in Normal and
	// Degraded (point 4), tagged with the urgent alarm decision.
	SaleKindUrgent = "urgent"
)

// What fired a sale back to 98%: the strategy's own exit, or the margin
// alarm turning urgent (ADR 0019, amended 2026-10-15, "When").
const (
	SaleTriggerExit         = "exit"
	SaleTriggerMarginUrgent = "margin-urgent"
)

// SaleFigures are the sizing figures of a sale back to 98%, all from one
// broker snapshot (ADR 0019, amended 2026-10-15, "Sized from the broker" and
// "One price basis"). On an OrderPlannedPayload, HeldQuantity is the
// broker's holding, ReferencePrice the snapshot's mark, NetLiquidation the
// snapshot's and TargetWeight the unlevered weight.
//
//   - KeepShares = ceil(TargetWeight x NetLiquidation / mark), so the sale
//     never targets less than 98%.
//   - ToTarget = holding - KeepShares: what reaches 98%.
//   - WithinHolding = holding - WorkingSells: so no combination of fills can
//     open a short.
//   - NeverBelow = holding - WorkingSells - KeepShares: so the sale and every
//     working sell together never take exposure below 98% ("never below
//     it"). It is never larger than the other two.
//
// The quantity is the smallest of the three.
type SaleFigures struct {
	Kind          string `json:"kind"`
	Trigger       string `json:"trigger"`
	SnapshotID    string `json:"snapshot_id"`
	WorkingSells  int64  `json:"working_sells"`
	KeepShares    int64  `json:"keep_shares"`
	ToTarget      int64  `json:"to_target"`
	WithinHolding int64  `json:"within_holding"`
	NeverBelow    int64  `json:"never_below"`
}

// KeepShares returns ceil(target x netLiquidation / mark), the shares a sale
// back to target keeps (ADR 0019, amended 2026-10-15, "One price basis").
// The product feeds no addition, so it needs no rounding barrier.
func KeepShares(target, netLiquidation, mark float64) float64 {
	return math.Ceil(target * netLiquidation / mark)
}

func (s SaleFigures) set() bool { return s != SaleFigures{} }

// validate checks a sale's figures against the order carrying them.
func (s SaleFigures) validate(p OrderPlannedPayload) []error {
	var errs []error
	if p.Side != OrderSideSell {
		errs = append(errs, errors.New("a sale back to 98% is a sell"))
	}
	switch s.Kind {
	case SaleKindIncident, SaleKindUrgent:
	default:
		errs = append(errs, fmt.Errorf("sale kind %q is not incident or urgent", s.Kind))
	}
	switch s.Trigger {
	case SaleTriggerExit, SaleTriggerMarginUrgent:
	default:
		errs = append(errs, fmt.Errorf("sale trigger %q is not exit or margin-urgent", s.Trigger))
	}
	if s.SnapshotID == "" {
		errs = append(errs, errors.New("sale snapshot id is required: the sale is sized from one broker snapshot"))
	}
	if s.WorkingSells < 0 {
		errs = append(errs, errors.New("sale working sells must not be negative"))
	}
	if isFinite(p.TargetWeight) && isFinite(p.NetLiquidation) && p.NetLiquidation > 0 && isFinite(p.ReferencePrice) && p.ReferencePrice > 0 {
		if keep := KeepShares(p.TargetWeight, p.NetLiquidation, p.ReferencePrice); float64(s.KeepShares) != keep {
			errs = append(errs, fmt.Errorf("sale keep shares %d is not ceil(%v x %v / %v) = %v", s.KeepShares, p.TargetWeight, p.NetLiquidation, p.ReferencePrice, keep))
		}
	}
	if s.ToTarget != p.HeldQuantity-s.KeepShares {
		errs = append(errs, errors.New("sale to target must be the holding less the shares kept"))
	}
	if s.WithinHolding != p.HeldQuantity-s.WorkingSells {
		errs = append(errs, errors.New("sale within holding must be the holding less every working sell"))
	}
	if s.NeverBelow != p.HeldQuantity-s.WorkingSells-s.KeepShares {
		errs = append(errs, errors.New("sale never below must be the holding less every working sell and the shares kept"))
	}
	if p.Quantity != min(s.ToTarget, s.WithinHolding, s.NeverBelow) {
		errs = append(errs, errors.New("a sale's quantity is the smallest of to target, within holding and never below"))
	}
	return errs
}

// SaleWithheldEventType identifies a sale back to 98% that fired but was not
// planned, so the alert can ask the owner to act by hand (ADR 0019, amended
// 2026-10-15, "No order when it cannot be checked").
const SaleWithheldEventType = "risk.sale.withheld"

// SaleWithheldSchemaVersion is the current schema version of SaleWithheldPayload.
const SaleWithheldSchemaVersion uint32 = 1

// The closed set of reasons a sale back to 98% is withheld.
const (
	// SaleWithheldUnreadable: no broker snapshot, or its holding, net
	// liquidation, open orders or mark could not be read.
	SaleWithheldUnreadable = "broker-values-unreadable"
	// SaleWithheldUnknownWorkingSell: a working sell on the instrument is
	// not one the journal released (CONTAINMENT REQUIRED).
	SaleWithheldUnknownWorkingSell = "unknown-working-sell"
	// SaleWithheldNothingToSell: the holding is already at or below 98%,
	// net of every working sell.
	SaleWithheldNothingToSell = "nothing-to-sell"
	// SaleWithheldStaleBrokerSnapshot: the latest broker snapshot is older at the
	// trigger than the configured maximum age, so it cannot size the sale
	// (ADR 0019: "a stale matching snapshot is not success").
	SaleWithheldStaleBrokerSnapshot = "stale-broker-snapshot"
	// SaleWithheldNotPositive: the latest broker snapshot is readable, but
	// its net liquidation or the instrument's mark is not strictly positive,
	// so ceil(target x V / mark) shares to keep would be zero or negative and
	// the smallest of the three bounds would sell into a short (ADR 0019:
	// never short).
	SaleWithheldNotPositive = "non-positive-broker-values"
	// SaleWithheldLapsed: a sale withheld from a stale or non-positive
	// snapshot was still withheld when what fired it stopped being in force
	// (the exit no longer refused or no longer planned, or the alarm no
	// longer urgent), so it is not decided again; the attempt stays unspent
	// (ADR 0019, amendment of 2026-10-17, reading 1, "Lapse").
	SaleWithheldLapsed = "lapsed"
)

// SaleWithheldPayload is a sale back to 98% that was not planned, with the
// client order ID it would have carried, so a later attempt in the same
// incident is recognised.
type SaleWithheldPayload struct {
	AccountID     string `json:"account_id"`
	Tag           string `json:"tag"`
	ClientOrderID string `json:"client_order_id"`
	InstrumentID  string `json:"instrument_id"`
	Kind          string `json:"kind"`
	Trigger       string `json:"trigger"`
	Reason        string `json:"reason"`
	Detail        string `json:"detail"`
	// SnapshotID is the broker snapshot consulted; empty when there was none.
	SnapshotID string `json:"snapshot_id"`
	// Alert marks the stale-broker-snapshot withhold that opens the
	// account's stale episode, which raises its one urgent alert (ADR 0019,
	// amendment of 2026-10-17, reading 10, "Stale episode").
	Alert bool `json:"alert,omitempty"`
}

// Attempted reports whether the withheld sale counts as the one attempt: an
// unreadable latest snapshot or an unknown working sell, for which the alert
// asks the owner to act by hand and a later automatic sale could duplicate
// the owner's. A sale withheld with nothing to sell, from a stale snapshot, or
// from a net liquidation or mark that is not strictly positive is not: it
// changes no state, and the next trigger with a readable, fresh latest
// snapshot decides again under the same tag.
func (p SaleWithheldPayload) Attempted() bool {
	return p.Reason == SaleWithheldUnreadable || p.Reason == SaleWithheldUnknownWorkingSell
}

// Validate checks identity, kind, trigger, reason and detail.
func (p SaleWithheldPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required"))
	}
	if p.ClientOrderID == "" {
		errs = append(errs, errors.New("client order id is required"))
	}
	if p.InstrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	switch p.Kind {
	case SaleKindIncident, SaleKindUrgent:
	default:
		errs = append(errs, fmt.Errorf("kind %q is not incident or urgent", p.Kind))
	}
	switch p.Trigger {
	case SaleTriggerExit, SaleTriggerMarginUrgent:
	default:
		errs = append(errs, fmt.Errorf("trigger %q is not exit or margin-urgent", p.Trigger))
	}
	switch p.Reason {
	case SaleWithheldUnreadable, SaleWithheldUnknownWorkingSell, SaleWithheldNothingToSell, SaleWithheldStaleBrokerSnapshot, SaleWithheldNotPositive, SaleWithheldLapsed:
	default:
		errs = append(errs, fmt.Errorf("reason %q is not recognised", p.Reason))
	}
	if p.Alert && p.Reason != SaleWithheldStaleBrokerSnapshot {
		errs = append(errs, errors.New("only a stale-broker-snapshot withhold raises the alert"))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid sale withheld payload: %w", err)
	}
	return nil
}
