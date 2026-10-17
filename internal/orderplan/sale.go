package orderplan

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// salePhase separates a sale back to 98% from an ordinary sell in client
// order ID derivation, so the two can never share an ID under one tag.
const salePhase = "sale-to-98"

// SaleInput is what a sale back to 98% of net liquidation is planned from:
// its tag (the incident, kill-switch engagement or urgent alarm it serves),
// kind and trigger, the unlevered weight it sells back to, the configured
// price band, and the latest broker snapshot (nil when there is none) with
// the client order IDs the journal has released and the shares already being
// sold that the snapshot cannot show. Only the incident kind refuses to sell
// beside an unknown working sell.
type SaleInput struct {
	AccountID    string
	Tag          string
	InstrumentID string
	Kind         string
	Trigger      string
	Target       float64
	PriceBand    float64
	SnapshotID   string
	Snapshot     *event.BrokerSnapshotPayload
	Recognised   map[string]bool
	// WorkingSells is every working and pending sell of the instrument that
	// the snapshot cannot show, ours and unknown, cancel-pending included,
	// by the risk gate's one pending rule (risklimit.Gate.PendingSells, here
	// against the broker snapshot): a released sell the snapshot does not
	// list yet is still being sold.
	WorkingSells int64
	// At is when the sale's trigger fired, and MaxSnapshotAge the oldest the
	// snapshot may be then.
	At             time.Time
	MaxSnapshotAge time.Duration
}

// Sale is a planned sale back to 98%, or the reason none was planned:
// exactly one of Order and Withheld.
type Sale struct {
	Order    *event.OrderPlannedPayload
	Withheld *event.SaleWithheldPayload
}

// SaleClientOrderID derives a sale's client order ID from its tag and
// instrument (ADR 0019, amended 2026-10-15: "a client identifier derived
// from the incident, like a restored stop").
func SaleClientOrderID(tag, instrumentID string) string {
	return ClientOrderID(tag, instrumentID, salePhase)
}

// PlanSale plans one sell-only order bringing the instrument toward Target
// of net liquidation and never below it, sized and checked exactly as ADR
// 0019's 2026-10-15 amendment states, which ADR 0019's 2026-10-16 amendment
// and ADR 0026 point 4 adopt:
//
//   - one price basis: net liquidation and the instrument's mark come from
//     one broker snapshot, never the previous close;
//   - keep ceil(Target x V / mark) shares, so the sale never targets less
//     than Target;
//   - the quantity is the smallest of what reaches Target (holding - keep)
//     and the holding minus every working and pending sell, ours and
//     unknown, cancel-pending included (WorkingSells, the gate's count, not
//     only the sells the snapshot lists); and, so that the sale together
//     with those working sells never takes exposure below Target ("never
//     below it"), the holding minus every working sell minus keep;
//   - no order when the snapshot is older at the trigger than MaxSnapshotAge,
//     when the holding, net liquidation, open orders or mark cannot be read
//     or net liquidation or the mark is not strictly positive, or, for the
//     incident sale, when any working sell on the instrument is not one the
//     journal released (CONTAINMENT REQUIRED).
//
// The sale is a limit at the band's lower edge around the snapshot's mark
// (ADR 0026, point 2, "Order type and price"). An error is a defect in the
// caller, never a market condition.
func PlanSale(in SaleInput) (Sale, error) {
	if err := in.validate(); err != nil {
		return Sale{}, err
	}
	clientID := SaleClientOrderID(in.Tag, in.InstrumentID)
	withheld := func(reason, detail string) (Sale, error) {
		return Sale{Withheld: &event.SaleWithheldPayload{
			AccountID: in.AccountID, Tag: in.Tag, ClientOrderID: clientID, InstrumentID: in.InstrumentID,
			Kind: in.Kind, Trigger: in.Trigger, Reason: reason, Detail: detail, SnapshotID: in.SnapshotID,
		}}, nil
	}
	s := in.Snapshot
	if s == nil {
		return withheld(event.SaleWithheldUnreadable, "no broker snapshot has been recorded, so the holding, net liquidation and open orders cannot be read; act by hand")
	}
	for _, c := range []string{event.BrokerComponentNetLiquidation, event.BrokerComponentOpenOrders, event.BrokerComponentPositions} {
		if !s.Readable(c) {
			return withheld(event.SaleWithheldUnreadable, fmt.Sprintf("broker snapshot %s could not read %s; act by hand", in.SnapshotID, c))
		}
	}
	// A readable snapshot's freshness is checked before its values.
	if detail := StaleDetail(in.SnapshotID, s.AsOf, in.At, in.MaxSnapshotAge); detail != "" {
		return withheld(event.SaleWithheldStaleBrokerSnapshot, detail+"; the sale waits for a fresh snapshot")
	}
	position, held := s.Position(in.InstrumentID)
	switch {
	case !(s.NetLiquidation > 0):
		// keep = ceil(Target x V / mark) is zero or negative here, and the
		// smallest of the three bounds would sell the whole holding or past
		// it into a short: never short (ADR 0019), so never sized from it.
		return withheld(event.SaleWithheldNotPositive, fmt.Sprintf("broker snapshot %s reports net liquidation %v, which no sale can be sized from; act by hand", in.SnapshotID, s.NetLiquidation))
	case held && position.MarkSet && !(position.Mark > 0):
		return withheld(event.SaleWithheldNotPositive, fmt.Sprintf("broker snapshot %s reports a mark of %v for %s, which no sale can be sized from; act by hand", in.SnapshotID, position.Mark, in.InstrumentID))
	case !held:
		return withheld(event.SaleWithheldNothingToSell, fmt.Sprintf("broker snapshot %s holds no %s", in.SnapshotID, in.InstrumentID))
	case position.Quantity < 0:
		return withheld(event.SaleWithheldNothingToSell, fmt.Sprintf("broker snapshot %s holds %d %s, a short, which is never sold further; act by hand", in.SnapshotID, position.Quantity, in.InstrumentID))
	case !position.MarkSet:
		return withheld(event.SaleWithheldUnreadable, fmt.Sprintf("broker snapshot %s has no mark for %s; act by hand", in.SnapshotID, in.InstrumentID))
	}
	if in.Kind == event.SaleKindIncident {
		for _, o := range s.OpenOrders {
			if o.InstrumentID == in.InstrumentID && o.Side == event.OrderSideSell && !in.Recognised[o.ClientOrderID] {
				return withheld(event.SaleWithheldUnknownWorkingSell, fmt.Sprintf(
					"working sell %s (client ID %q, %d shares, %s) on %s is not one the journal released: CONTAINMENT REQUIRED; act by hand",
					o.BrokerOrderID, o.ClientOrderID, o.RemainingQuantity, o.Status, in.InstrumentID))
			}
		}
	}

	keep := event.KeepShares(in.Target, s.NetLiquidation, position.Mark)
	if keep >= math.MaxInt64 {
		return Sale{}, fmt.Errorf("orderplan: %v shares to keep cannot be represented", keep)
	}
	working := in.WorkingSells
	figures := event.SaleFigures{
		Kind: in.Kind, Trigger: in.Trigger, SnapshotID: in.SnapshotID, WorkingSells: working,
		KeepShares:    int64(keep),
		ToTarget:      position.Quantity - int64(keep),
		WithinHolding: position.Quantity - working,
		NeverBelow:    position.Quantity - working - int64(keep),
	}
	quantity := min(figures.ToTarget, figures.WithinHolding, figures.NeverBelow)
	if quantity <= 0 {
		return withheld(event.SaleWithheldNothingToSell, fmt.Sprintf(
			"%s holds %d with %d working to sell; keeping %d at mark %v of net liquidation %v leaves nothing to sell",
			in.InstrumentID, position.Quantity, working, figures.KeepShares, position.Mark, s.NetLiquidation))
	}
	return Sale{Order: &event.OrderPlannedPayload{
		AccountID: in.AccountID, Tag: in.Tag, ClientOrderID: clientID, InstrumentID: in.InstrumentID,
		Side: event.OrderSideSell, Phase: event.OrderSideSell, OrderType: event.OrderTypeLimit,
		Quantity: quantity, LimitPrice: LimitPrice(event.OrderSideSell, position.Mark, in.PriceBand),
		TargetWeight: in.Target, NetLiquidation: s.NetLiquidation, HeldQuantity: position.Quantity,
		ReferencePrice: position.Mark, PriceBand: in.PriceBand, Sale: figures,
	}}, nil
}

func (in SaleInput) validate() error {
	var errs []error
	if in.AccountID == "" || in.Tag == "" || in.InstrumentID == "" {
		errs = append(errs, errors.New("a sale needs its account, tag and instrument"))
	}
	if in.Kind != event.SaleKindIncident && in.Kind != event.SaleKindUrgent {
		errs = append(errs, fmt.Errorf("sale kind %q is not incident or urgent", in.Kind))
	}
	if in.Trigger != event.SaleTriggerExit && in.Trigger != event.SaleTriggerMarginUrgent {
		errs = append(errs, fmt.Errorf("sale trigger %q is not exit or margin-urgent", in.Trigger))
	}
	if !(in.Target > 0 && in.Target <= 1) {
		errs = append(errs, errors.New("a sale's target must lie in (0, 1]"))
	}
	if !(in.PriceBand > 0 && in.PriceBand < 1) {
		errs = append(errs, errors.New("the price band must lie strictly between 0 and 1"))
	}
	if in.WorkingSells < 0 {
		errs = append(errs, errors.New("a sale's working sells must not be negative"))
	}
	if in.MaxSnapshotAge <= 0 {
		errs = append(errs, errors.New("a sale needs a positive maximum snapshot age"))
	}
	if in.Snapshot == nil && in.SnapshotID != "" {
		errs = append(errs, errors.New("a snapshot ID names no broker snapshot"))
	}
	if in.Snapshot != nil {
		if in.SnapshotID == "" {
			errs = append(errs, errors.New("a broker snapshot needs its envelope ID"))
		}
		if err := in.Snapshot.Validate(); err != nil {
			errs = append(errs, err)
		} else if in.Snapshot.AccountID != in.AccountID {
			errs = append(errs, fmt.Errorf("broker snapshot of account %q cannot size a sale for %q", in.Snapshot.AccountID, in.AccountID))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("orderplan: %w", err)
	}
	return nil
}

// StaleDetail returns why broker snapshot id, taken at asOf, is stale at at,
// or "" when it is fresh: older than maxAge, or taken after at, which is
// stale too, never fresh, and records the skew (ADR 0019: age is the
// triggering event's journalled time minus the snapshot's broker timestamp;
// the wall clock is never read).
func StaleDetail(id string, asOf, at time.Time, maxAge time.Duration) string {
	switch age := at.Sub(asOf); {
	case age < 0:
		return fmt.Sprintf("broker snapshot %s was taken %v after the trigger, a clock skew, so it is stale", id, -age)
	case age > maxAge:
		return fmt.Sprintf("broker snapshot %s is %v old at the trigger; the most allowed is %v", id, age, maxAge)
	}
	return ""
}
