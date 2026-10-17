package event

import (
	"errors"
	"fmt"
	"time"
)

// TargetsDecidedEventType identifies a target-weight strategy's decision for
// one account at one decision time (ADR 0026, point 1). It decides targets
// only; it never names an order.
const TargetsDecidedEventType = "strategy.targets.decided"

// TargetsDecidedSchemaVersion is the current schema version of TargetsDecidedPayload.
//
//   - Version 1 recorded Boost decisions only.
//   - Version 2 adds momentum decisions: StrategyID momentum, the Momentum
//     figures, and the withheld reason rebalance-in-progress. Boost is
//     omitted from a momentum decision and Momentum from a Boost one, so a
//     Boost decision's payload bytes are version 1's.
//   - Version 3 adds the glide path to momentum decisions: the Glide figures
//     (the rebalance session, the owner's age in months and the bond share
//     b), and each target's sleeve Shares, which record what the momentum
//     and bond sleeves each contribute to a fund both hold (ADR 0026,
//     point 1). Both are omitted from a Boost decision, whose payload bytes
//     are still version 1's.
//   - Version 4 records D7-L's substitution in momentum decisions (ADR 0026,
//     point 2, "D7-L"): the MinimumLot figures the picks were valued against,
//     the name status unaffordable, and each unaffordable name's one share
//     and target value. All are omitted when unset, so a Boost decision's
//     bytes are still version 1's.
const TargetsDecidedSchemaVersion uint32 = 4

// The two outcomes of a target decision (ADR 0026, point 1).
const (
	TargetsOutcomeDecided  = "decided"
	TargetsOutcomeWithheld = "withheld"
)

// The closed set of reasons a decision is withheld (ADR 0026, points 1 and 5).
const (
	// WithheldMissingBars: a session closed without the instrument's bar.
	// Boost never computes RSI across a gap (plan.md section 2, "Boost").
	WithheldMissingBars = "missing-bars"
	// WithheldStaleAccountSnapshot: no account snapshot as of the session's
	// close, so nothing can be valued at the previous close (ADR 0020).
	WithheldStaleAccountSnapshot = "stale-account-snapshot"
	// WithheldMissingUniverse: no market.universe.snapshot for the decision
	// time, so momentum cannot rank (ADR 0026, point 1, "Universe").
	WithheldMissingUniverse = "missing-universe"
	// WithheldInsufficientTrendHistory: fewer than 200 SPY closes, which the
	// backtest treats as risk-on and live withholds (ADR 0026, point 5).
	WithheldInsufficientTrendHistory = "insufficient-trend-history"
	// WithheldNoEligibleNames: the ranking left no name to hold, none of
	// them skipped as unaffordable (a book D7-L emptied is decided).
	WithheldNoEligibleNames = "no-eligible-names"
	// WithheldRebalanceInProgress: an order of an earlier decision is not
	// yet terminal, so no new rebalance is planned until it is (ADR 0026,
	// point 5: nothing is sent mid-rebalance until the owner clears it).
	WithheldRebalanceInProgress = "rebalance-in-progress"
)

// TargetWeight is one instrument's target as a fraction of net liquidation.
// A momentum target records the share each sleeve contributes, momentum then
// bond, whose sum in that order is Weight (plan.md section 2, "Bond sleeve
// and glide path": a fund both sleeves hold is one target, the sum, with each
// sleeve's share in the journal). A Boost target has none.
type TargetWeight struct {
	InstrumentID string        `json:"instrument_id"`
	Weight       float64       `json:"weight"`
	Shares       []SleeveShare `json:"shares,omitempty"`
}

// The closed set of sleeves a momentum target's shares name.
const (
	SleeveMomentum = "momentum"
	SleeveBond     = "bond"
)

// SleeveShare is the part of a target one sleeve contributes.
type SleeveShare struct {
	Sleeve string  `json:"sleeve"`
	Weight float64 `json:"weight"`
}

// GlideFigures are the glide path's figures behind a momentum decision (ADR
// 0026, point 1): the rebalance session the age is counted on, YYYY-MM-DD,
// the owner's age on it in whole months, and the bond share b. The momentum
// book is scaled to 1 - b and the bond sleeve holds 98% of b; the momentum
// Names' weights are the book's before that scaling.
type GlideFigures struct {
	RebalanceSession string  `json:"rebalance_session"`
	AgeMonths        int     `json:"age_months"`
	BondShare        float64 `json:"bond_share"`
}

// BoostFigures are the figures behind a Boost decision, so the journal alone
// can check it (ADR 0026, point 1): the adjusted close, RSI(2), both means,
// the latched boost state before and after (template.py:89-94), and the
// target in force before this decision (template.py's last_want, unset before
// the first decision, template.py:97-100). An indicator not ready carries 0.
type BoostFigures struct {
	InstrumentID     string  `json:"instrument_id"`
	Close            float64 `json:"close"`
	RSI              float64 `json:"rsi"`
	RSIReady         bool    `json:"rsi_ready"`
	TrendMean        float64 `json:"trend_mean"`
	TrendMeanReady   bool    `json:"trend_mean_ready"`
	ExitMean         float64 `json:"exit_mean"`
	ExitMeanReady    bool    `json:"exit_mean_ready"`
	LatchedBefore    bool    `json:"latched_before"`
	LatchedAfter     bool    `json:"latched_after"`
	TargetInForce    float64 `json:"target_in_force"`
	TargetInForceSet bool    `json:"target_in_force_set"`
}

// TargetsDecidedPayload is one account's target decision.
//
// A withheld decision carries no targets, but it still carries the Boost
// figures: the latched state advances with every bar (template.py:89-94), and
// a restart recovers it from the last decision of either outcome (ADR 0026,
// point 5).
type TargetsDecidedPayload struct {
	AccountID  string `json:"account_id"`
	StrategyID string `json:"strategy_id"`
	// DecisionTime is when the decision was taken; SessionPeriodEnd is the
	// session whose closes it used. A decision never precedes its session.
	DecisionTime     time.Time `json:"decision_time"`
	SessionPeriodEnd time.Time `json:"session_period_end"`
	Outcome          string    `json:"outcome"`
	WithheldReason   string    `json:"withheld_reason,omitempty"`
	Detail           string    `json:"detail,omitempty"`
	// Targets is sorted strictly ascending by instrument.
	Targets []TargetWeight `json:"targets"`
	// Boost is set for StrategyID boost and Momentum for momentum; the other
	// is zero and omitted.
	Boost    BoostFigures    `json:"boost,omitzero"`
	Momentum MomentumFigures `json:"momentum,omitzero"`
	// Glide is set for StrategyID momentum, on either outcome.
	Glide GlideFigures `json:"glide,omitzero"`
}

// MomentumFigures are the figures behind a momentum decision, so the journal
// alone can check it (ADR 0026, point 1): the session whose closes it used,
// the risk-off inputs, the universe filter counts and every ranked name.
// A decision withheld before the ranking records no names.
type MomentumFigures struct {
	// CloseSession is the last session before the decision, YYYY-MM-DD.
	CloseSession string `json:"close_session"`
	// The trend test: TrendCloses counts the trend instrument's closes,
	// TrendClose and TrendMean are 0 when it was not computed.
	TrendInstrument string  `json:"trend_instrument"`
	TrendCloses     int     `json:"trend_closes"`
	TrendClose      float64 `json:"trend_close"`
	TrendMean       float64 `json:"trend_mean"`
	RiskOff         bool    `json:"risk_off"`
	// How many universe rows each filter kept: all rows, the listing type,
	// the price floor, and the members ranked.
	UniverseRows           int `json:"universe_rows"`
	UniverseListingType    int `json:"universe_listing_type"`
	UniverseAtOrAboveFloor int `json:"universe_at_or_above_floor"`
	UniverseMembers        int `json:"universe_members"`
	// Names holds every universe member in universe rank order.
	Names []MomentumName `json:"names"`
	// MinimumLot is set exactly when the momentum rule decided a book under
	// D7-L: net liquidation V at the previous close and the momentum
	// sleeve's scale, 1 - b. A pick's target value is its weight x
	// MomentumScale x V. The figures stay when the run then withholds that
	// book (missing bars, a rebalance in progress); a decision the rule
	// withheld, no-eligible-names included, compared no pick and has none.
	MinimumLot MinimumLotFigures `json:"minimum_lot,omitzero"`
}

// MinimumLotFigures are what D7-L valued each pick against.
type MinimumLotFigures struct {
	NetLiquidation float64 `json:"net_liquidation"`
	MomentumScale  float64 `json:"momentum_scale"`
}

// The closed set of MomentumName.Status values: what the rule did with a
// universe member (internal/targetweight/momentum's Status).
const (
	MomentumStatusScored         = "scored"
	MomentumStatusPicked         = "picked"
	MomentumStatusSectorCapped   = "sector-capped"
	MomentumStatusBeyondTopN     = "beyond-top-n"
	MomentumStatusNoSector       = "no-sector"
	MomentumStatusZeroVolatility = "zero-volatility"
	MomentumStatusShortHistory   = "short-history"
	MomentumStatusGap            = "gap"
	// MomentumStatusUnaffordable: D7-L skipped the pick, since one share at
	// its previous raw close exceeds its target value.
	MomentumStatusUnaffordable = "unaffordable"
)

// MomentumName is one ranked universe member: its universe rank and dollar
// volume, sector label and source, what the rule did with it, its closes,
// score, volatility and score rank (0 when unscored), its weight (0 unless
// picked; a pick has 0 when the whole book is risk-off) and whether the backtest would have picked it (ADR 0026, point 8).
type MomentumName struct {
	InstrumentID      string  `json:"instrument_id"`
	UniverseRank      int     `json:"universe_rank"`
	DollarVolume      float64 `json:"dollar_volume"`
	Sector            string  `json:"sector"`
	SectorSource      string  `json:"sector_source"`
	Status            string  `json:"status"`
	Closes            int     `json:"closes"`
	Score             float64 `json:"score"`
	Volatility        float64 `json:"volatility"`
	ScoreRank         int     `json:"score_rank"`
	Weight            float64 `json:"weight"`
	BacktestWouldPick bool    `json:"backtest_would_pick"`
	// OneShare and TargetValue are set for an unaffordable name: its
	// previous raw close and the target value it exceeded.
	OneShare    float64 `json:"one_share,omitzero"`
	TargetValue float64 `json:"target_value,omitzero"`
}

// Validate checks the identity, the outcome and its reason, the targets and
// the Boost figures.
func (p TargetsDecidedPayload) Validate() error {
	var errs []error
	if p.AccountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	boost := p.StrategyID == TargetWeightStrategyBoost
	switch p.StrategyID {
	case TargetWeightStrategyBoost:
		errs = append(errs, p.Boost.validate()...)
		if !p.Momentum.isZero() {
			errs = append(errs, errors.New("a boost decision carries no momentum figures"))
		}
		if p.Glide != (GlideFigures{}) {
			errs = append(errs, errors.New("a boost decision carries no glide figures"))
		}
	case TargetWeightStrategyMomentum:
		errs = append(errs, p.Momentum.validate()...)
		errs = append(errs, p.Glide.validate(p.Momentum.CloseSession)...)
		if p.Boost != (BoostFigures{}) {
			errs = append(errs, errors.New("a momentum decision carries no boost figures"))
		}
	default:
		errs = append(errs, fmt.Errorf("strategy id %q is not a target-weight strategy this schema records", p.StrategyID))
	}
	decisionErrs := requiredTime("decision time", p.DecisionTime)
	sessionErrs := requiredTime("session period end", p.SessionPeriodEnd)
	errs = append(errs, decisionErrs...)
	errs = append(errs, sessionErrs...)
	if len(decisionErrs) == 0 && len(sessionErrs) == 0 && p.DecisionTime.Before(p.SessionPeriodEnd) {
		errs = append(errs, errors.New("decision time is before the session whose closes it used"))
	}
	switch p.Outcome {
	case TargetsOutcomeDecided:
		if p.WithheldReason != "" {
			errs = append(errs, errors.New("a decided outcome carries no withheld reason"))
		}
		if len(p.Targets) == 0 {
			errs = append(errs, errors.New("a decided outcome carries at least one target"))
		}
		if boost && !(p.Boost.Close > 0) {
			errs = append(errs, errors.New("a decided Boost outcome carries a positive close"))
		}
	case TargetsOutcomeWithheld:
		switch p.WithheldReason {
		case WithheldMissingBars, WithheldStaleAccountSnapshot, WithheldMissingUniverse,
			WithheldInsufficientTrendHistory, WithheldNoEligibleNames, WithheldRebalanceInProgress:
		default:
			errs = append(errs, fmt.Errorf("withheld reason %q is not recognised", p.WithheldReason))
		}
		if p.Detail == "" {
			errs = append(errs, errors.New("a withheld outcome needs its detail"))
		}
		if len(p.Targets) != 0 {
			errs = append(errs, errors.New("a withheld outcome carries no targets"))
		}
	default:
		errs = append(errs, fmt.Errorf("outcome %q is not decided or withheld", p.Outcome))
	}
	for i, target := range p.Targets {
		if target.InstrumentID == "" {
			errs = append(errs, fmt.Errorf("target %d: instrument id is required", i+1))
		} else if i > 0 && target.InstrumentID <= p.Targets[i-1].InstrumentID {
			errs = append(errs, fmt.Errorf("targets must be strictly ascending by instrument; %q follows %q", target.InstrumentID, p.Targets[i-1].InstrumentID))
		}
		if !isFinite(target.Weight) || target.Weight < 0 {
			errs = append(errs, fmt.Errorf("target %d: weight must be finite and not negative", i+1))
		}
		if boost && len(target.Shares) > 0 {
			errs = append(errs, fmt.Errorf("target %d: a boost target carries no sleeve shares", i+1))
		}
		if p.StrategyID == TargetWeightStrategyMomentum {
			errs = append(errs, target.validateShares(i+1)...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid targets decided payload: %w", err)
	}
	return nil
}

// validateShares checks a momentum target's sleeve shares: at least one,
// momentum before bond, each sleeve once, each finite and not negative, and
// their sum in that order exactly the target's weight.
func (t TargetWeight) validateShares(n int) []error {
	if len(t.Shares) == 0 {
		return []error{fmt.Errorf("target %d: a momentum target needs its sleeve shares", n)}
	}
	var errs []error
	sum := 0.0
	for i, s := range t.Shares {
		switch {
		case s.Sleeve != SleeveMomentum && s.Sleeve != SleeveBond:
			errs = append(errs, fmt.Errorf("target %d: sleeve %q is not momentum or bond", n, s.Sleeve))
		case i > 0 && (s.Sleeve != SleeveBond || t.Shares[i-1].Sleeve != SleeveMomentum):
			errs = append(errs, fmt.Errorf("target %d: sleeve shares come momentum then bond, each once", n))
		}
		if !isFinite(s.Weight) || s.Weight < 0 {
			errs = append(errs, fmt.Errorf("target %d: sleeve share must be finite and not negative", n))
		}
		sum += s.Weight
	}
	if sum != t.Weight {
		errs = append(errs, fmt.Errorf("target %d: sleeve shares sum to %v, not the weight %v", n, sum, t.Weight))
	}
	return errs
}

func (g GlideFigures) validate(closeSession string) []error {
	var errs []error
	if g.AgeMonths < 1 {
		errs = append(errs, errors.New("glide age in months must be positive"))
	}
	if !isFinite(g.BondShare) || g.BondShare < 0 || g.BondShare > 1 {
		errs = append(errs, errors.New("glide bond share must lie in [0, 1]"))
	}
	if _, err := time.Parse(time.DateOnly, g.RebalanceSession); err != nil {
		errs = append(errs, fmt.Errorf("glide rebalance session %q is not a YYYY-MM-DD date", g.RebalanceSession))
	} else if g.RebalanceSession <= closeSession {
		errs = append(errs, errors.New("glide rebalance session must come after the close session"))
	}
	return errs
}

func (m MomentumFigures) isZero() bool {
	return m.CloseSession == "" && m.TrendInstrument == "" && m.TrendCloses == 0 && m.TrendClose == 0 &&
		m.TrendMean == 0 && !m.RiskOff && m.UniverseRows == 0 && m.UniverseListingType == 0 &&
		m.UniverseAtOrAboveFloor == 0 && m.UniverseMembers == 0 && len(m.Names) == 0 && m.MinimumLot == (MinimumLotFigures{})
}

func (m MomentumFigures) validate() []error {
	var errs []error
	if _, err := time.Parse(time.DateOnly, m.CloseSession); err != nil {
		errs = append(errs, fmt.Errorf("momentum close session %q is not a YYYY-MM-DD date", m.CloseSession))
	}
	if m.TrendInstrument == "" {
		errs = append(errs, errors.New("momentum trend instrument is required"))
	}
	for _, v := range []float64{m.TrendClose, m.TrendMean} {
		if !isFinite(v) || v < 0 {
			errs = append(errs, errors.New("momentum trend close and mean must be finite and not negative"))
		}
	}
	for _, n := range []int{m.TrendCloses, m.UniverseRows, m.UniverseListingType, m.UniverseAtOrAboveFloor, m.UniverseMembers} {
		if n < 0 {
			errs = append(errs, errors.New("momentum counts must not be negative"))
		}
	}
	if len(m.Names) != m.UniverseMembers && len(m.Names) != 0 {
		errs = append(errs, fmt.Errorf("momentum records %d names for %d universe members", len(m.Names), m.UniverseMembers))
	}
	lot := m.MinimumLot != (MinimumLotFigures{})
	if lot && (!isFinite(m.MinimumLot.NetLiquidation) || m.MinimumLot.NetLiquidation <= 0 ||
		!isFinite(m.MinimumLot.MomentumScale) || m.MinimumLot.MomentumScale <= 0 || m.MinimumLot.MomentumScale > 1) {
		errs = append(errs, errors.New("momentum minimum lot needs a positive net liquidation and a scale in (0, 1]"))
	}
	for i, n := range m.Names {
		errs = append(errs, n.validate(i+1)...)
		if n.Status == MomentumStatusUnaffordable && !lot {
			errs = append(errs, fmt.Errorf("momentum name %s: an unaffordable name needs the minimum lot figures", n.InstrumentID))
		}
	}
	return errs
}

func (n MomentumName) validate(rank int) []error {
	var errs []error
	if n.InstrumentID == "" {
		errs = append(errs, fmt.Errorf("momentum name %d: instrument id is required", rank))
	}
	if n.UniverseRank != rank {
		errs = append(errs, fmt.Errorf("momentum name %s: universe rank %d is not its place %d", n.InstrumentID, n.UniverseRank, rank))
	}
	switch n.Status {
	case MomentumStatusScored, MomentumStatusPicked, MomentumStatusSectorCapped, MomentumStatusBeyondTopN,
		MomentumStatusNoSector, MomentumStatusZeroVolatility, MomentumStatusShortHistory, MomentumStatusGap:
		if n.OneShare != 0 || n.TargetValue != 0 {
			errs = append(errs, fmt.Errorf("momentum name %s: only an unaffordable name records a one-share price and target value", n.InstrumentID))
		}
	case MomentumStatusUnaffordable:
		if !isFinite(n.OneShare) || !isFinite(n.TargetValue) || n.TargetValue < 0 || !(n.OneShare > n.TargetValue) {
			errs = append(errs, fmt.Errorf("momentum name %s: an unaffordable name's one share exceeds its target value", n.InstrumentID))
		}
	default:
		errs = append(errs, fmt.Errorf("momentum name %s: status %q is not recognised", n.InstrumentID, n.Status))
	}
	if (n.Sector == "") != (n.SectorSource == "") {
		errs = append(errs, fmt.Errorf("momentum name %s: a sector label and its source come together", n.InstrumentID))
	}
	if n.Closes < 0 || n.ScoreRank < 0 {
		errs = append(errs, fmt.Errorf("momentum name %s: closes and score rank must not be negative", n.InstrumentID))
	}
	for _, v := range []float64{n.DollarVolume, n.Volatility, n.Weight} {
		if !isFinite(v) || v < 0 {
			errs = append(errs, fmt.Errorf("momentum name %s: dollar volume, volatility and weight must be finite and not negative", n.InstrumentID))
		}
	}
	if !isFinite(n.Score) {
		errs = append(errs, fmt.Errorf("momentum name %s: score must be finite", n.InstrumentID))
	}
	if n.Weight > 0 && n.Status != MomentumStatusPicked {
		errs = append(errs, fmt.Errorf("momentum name %s: only a picked name has a weight", n.InstrumentID))
	}
	return errs
}

func (b BoostFigures) validate() []error {
	var errs []error
	if b.InstrumentID == "" {
		errs = append(errs, errors.New("boost instrument id is required"))
	}
	if !isFinite(b.Close) || b.Close < 0 {
		errs = append(errs, errors.New("boost close must be finite and not negative"))
	}
	for _, f := range []struct {
		name  string
		value float64
		ready bool
	}{{"rsi", b.RSI, b.RSIReady}, {"trend mean", b.TrendMean, b.TrendMeanReady}, {"exit mean", b.ExitMean, b.ExitMeanReady}} {
		switch {
		case !isFinite(f.value) || f.value < 0:
			errs = append(errs, fmt.Errorf("boost %s must be finite and not negative", f.name))
		case !f.ready && f.value != 0:
			errs = append(errs, fmt.Errorf("boost %s carries a value but is not ready", f.name))
		}
	}
	switch {
	case !isFinite(b.TargetInForce) || b.TargetInForce < 0:
		errs = append(errs, errors.New("boost target in force must be finite and not negative"))
	case !b.TargetInForceSet && b.TargetInForce != 0:
		errs = append(errs, errors.New("boost target in force carries a value but none is set"))
	}
	return errs
}

// OrderPlannedEventType identifies one order the strategy-independent planner
// derived from a target decision (ADR 0026, point 2).
const OrderPlannedEventType = "strategy.order.planned"

// OrderPlannedSchemaVersion is the current schema version of OrderPlannedPayload.
const OrderPlannedSchemaVersion uint32 = 1

// Order sides, which are also the planner's phases (ADR 0026, point 2,
// "Order identity").
const (
	OrderSideBuy  = "buy"
	OrderSideSell = "sell"
)

// OrderTypeLimit is the only order type planned: a limit at the price band's
// edge, marketable but never filled worse than the band (ADR 0026, point 2).
const OrderTypeLimit = "limit"

// OrderPlannedPayload is one planned order and the figures it was sized from:
// net liquidation V, the holding q and the raw reference close p, all as of
// the previous close (ADR 0026, point 2, "Valuation basis").
type OrderPlannedPayload struct {
	AccountID string `json:"account_id"`
	// Tag is the ID of the strategy.targets.decided envelope this order
	// serves; ClientOrderID derives from it, the instrument and the phase.
	Tag           string `json:"tag"`
	ClientOrderID string `json:"client_order_id"`
	InstrumentID  string `json:"instrument_id"`
	Side          string `json:"side"`
	Phase         string `json:"phase"`
	OrderType     string `json:"order_type"`
	// Quantity is in whole raw shares, always positive; Side gives the sign.
	Quantity       int64   `json:"quantity"`
	LimitPrice     float64 `json:"limit_price"`
	TargetWeight   float64 `json:"target_weight"`
	NetLiquidation float64 `json:"net_liquidation"`
	HeldQuantity   int64   `json:"held_quantity"`
	ReferencePrice float64 `json:"reference_price"`
	PriceBand      float64 `json:"price_band"`
}

// Validate checks identity, side, type, quantity and prices, and that the
// limit lies on the side of the reference price its order type requires: a
// buy's limit at or above it, a sell's at or below.
func (p OrderPlannedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: an order serves a journalled decision"))
	}
	if p.Phase != OrderSideBuy && p.Phase != OrderSideSell {
		errs = append(errs, fmt.Errorf("phase %q is not buy or sell", p.Phase))
	}
	if p.OrderType != OrderTypeLimit {
		errs = append(errs, fmt.Errorf("order type %q is not %q", p.OrderType, OrderTypeLimit))
	}
	if !isFinite(p.TargetWeight) || p.TargetWeight < 0 {
		errs = append(errs, errors.New("target weight must be finite and not negative"))
	}
	if !isFinite(p.NetLiquidation) || p.NetLiquidation <= 0 {
		errs = append(errs, errors.New("net liquidation must be finite and positive"))
	}
	if p.HeldQuantity < 0 {
		errs = append(errs, errors.New("held quantity must not be negative: the planner never plans against a short"))
	}
	referenceOK := isFinite(p.ReferencePrice) && p.ReferencePrice > 0
	if !referenceOK {
		errs = append(errs, errors.New("reference price must be finite and positive"))
	}
	if !isFinite(p.PriceBand) || p.PriceBand <= 0 || p.PriceBand >= 1 {
		errs = append(errs, errors.New("price band must lie strictly between 0 and 1"))
	}
	if referenceOK && isFinite(p.LimitPrice) {
		if p.Side == OrderSideBuy && p.LimitPrice < p.ReferencePrice {
			errs = append(errs, errors.New("a buy's limit must not be below its reference price"))
		}
		if p.Side == OrderSideSell && p.LimitPrice > p.ReferencePrice {
			errs = append(errs, errors.New("a sell's limit must not be above its reference price"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order planned payload: %w", err)
	}
	return nil
}

func orderIdentity(accountID, clientOrderID, instrumentID, side string, quantity int64, limit float64) []error {
	var errs []error
	if accountID == "" {
		errs = append(errs, errors.New("account id is required"))
	}
	if clientOrderID == "" {
		errs = append(errs, errors.New("client order id is required"))
	}
	if instrumentID == "" {
		errs = append(errs, errors.New("instrument id is required"))
	}
	if side != OrderSideBuy && side != OrderSideSell {
		errs = append(errs, fmt.Errorf("side %q is not buy or sell", side))
	}
	if quantity <= 0 {
		errs = append(errs, errors.New("quantity must be positive"))
	}
	if !isFinite(limit) || limit <= 0 {
		errs = append(errs, errors.New("limit price must be finite and positive"))
	}
	return errs
}

// The risk gate's two outcomes for a planned order (ADR 0026, point 3). Only
// a released order, once durably journalled, may be sent.
const (
	OrderReleasedEventType = "risk.order.released"
	OrderRefusedEventType  = "risk.order.refused"
)

// OrderReleasedSchemaVersion and OrderRefusedSchemaVersion are the current
// schema versions of OrderReleasedPayload and OrderRefusedPayload.
//
//   - Version 1 checked the order's own instrument's exposure.
//   - Version 2 checks account-wide exposure, and adds the IRA's cash check
//     to RiskFigures (CashChecked, BuyCost, CashAvailable) and its refusal
//     reasons to the closed set.
const (
	OrderReleasedSchemaVersion uint32 = 2
	OrderRefusedSchemaVersion  uint32 = 2
)

// The closed set of reasons the risk gate refuses an order (ADR 0026, point 3).
const (
	// RefusedUndecidedOrder: the tag names no journalled decision. Halts.
	RefusedUndecidedOrder = "undecided-order"
	// RefusedMarginExposure: an exposure-increasing order would leave the
	// account above its pre-trade limit.
	RefusedMarginExposure = "margin-exposure"
	// RefusedOrderCount: the plan has more orders than the limit allows.
	RefusedOrderCount = "order-count"
	// RefusedOrderSize: the order is a larger share of net liquidation than allowed.
	RefusedOrderSize = "order-size"
	// RefusedPriceBand: the reference price lies outside the band around the mark.
	RefusedPriceBand = "price-band"
	// RefusedMissingMark: no mark recorded for the instrument at or after the
	// session's close, so the band cannot be checked.
	RefusedMissingMark = "missing-mark"
	// RefusedEngineDegraded: the account is Degraded, which allows no buy and
	// freezes the instruments its scope names (ADR 0026, point 4).
	RefusedEngineDegraded = "engine-degraded"
	// RefusedEngineHalted: the account is Halted, which allows no order.
	RefusedEngineHalted = "engine-halted"
	// RefusedIRAExposure: a buy would leave an IRA's exposure above its
	// limit, 100% (ADR 0026, point 3).
	RefusedIRAExposure = "ira-exposure"
	// RefusedInsufficientCash: a buy's worst-case cost exceeds the IRA's
	// cash available under D5, net of committed buys; an IRA never borrows.
	RefusedInsufficientCash = "insufficient-cash"
	// RefusedSellExceedsHolding: a sell exceeds the holding net of every
	// working or pending sell (ADR 0019, amended 2026-09-24).
	RefusedSellExceedsHolding = "sell-exceeds-holding"
	// RefusedUnvaluedHolding: an exposure-increasing order while a holding
	// has no reference price, so account-wide exposure cannot be valued.
	RefusedUnvaluedHolding = "unvalued-holding"
)

// RiskFigures are the values the gate checked an order against, so the
// journal alone can check the outcome. Mark is 0 when MarkSet is false.
// Exposure is account-wide: every holding at its reference price over net
// liquidation. CashChecked is true for an IRA's buy, whose worst-case BuyCost
// was checked against CashAvailable; otherwise both are 0.
type RiskFigures struct {
	NetLiquidation   float64 `json:"net_liquidation"`
	ReferencePrice   float64 `json:"reference_price"`
	ExposureBefore   float64 `json:"exposure_before"`
	ExposureAfter    float64 `json:"exposure_after"`
	OrderFraction    float64 `json:"order_fraction"`
	Mark             float64 `json:"mark"`
	MarkSet          bool    `json:"mark_set"`
	MarkDeviation    float64 `json:"mark_deviation"`
	OrdersInDecision int     `json:"orders_in_decision"`
	CashChecked      bool    `json:"cash_checked,omitzero"`
	BuyCost          float64 `json:"buy_cost,omitzero"`
	CashAvailable    float64 `json:"cash_available,omitzero"`
}

func (f RiskFigures) validate() []error {
	var errs []error
	if !isFinite(f.NetLiquidation) || f.NetLiquidation <= 0 {
		errs = append(errs, errors.New("figures net liquidation must be finite and positive"))
	}
	if !isFinite(f.ReferencePrice) || f.ReferencePrice <= 0 {
		errs = append(errs, errors.New("figures reference price must be finite and positive"))
	}
	for _, v := range []struct {
		name  string
		value float64
	}{{"exposure before", f.ExposureBefore}, {"exposure after", f.ExposureAfter}, {"order fraction", f.OrderFraction}, {"mark deviation", f.MarkDeviation}} {
		if !isFinite(v.value) || v.value < 0 {
			errs = append(errs, fmt.Errorf("figures %s must be finite and not negative", v.name))
		}
	}
	switch {
	case !isFinite(f.Mark) || f.Mark < 0:
		errs = append(errs, errors.New("figures mark must be finite and not negative"))
	case f.MarkSet && f.Mark == 0:
		errs = append(errs, errors.New("figures mark must be positive when set"))
	case !f.MarkSet && (f.Mark != 0 || f.MarkDeviation != 0):
		errs = append(errs, errors.New("figures mark carries a value but none is set"))
	}
	if f.OrdersInDecision < 0 {
		errs = append(errs, errors.New("figures orders in decision must not be negative"))
	}
	switch {
	case !isFinite(f.BuyCost) || f.BuyCost < 0 || !isFinite(f.CashAvailable):
		errs = append(errs, errors.New("figures buy cost must be finite and not negative, and cash available finite"))
	case !f.CashChecked && (f.BuyCost != 0 || f.CashAvailable != 0):
		errs = append(errs, errors.New("figures cash carries a value but was not checked"))
	}
	return errs
}

// OrderReleasedPayload is a planned order the gate passed. Send is false in
// dry-run: the order is journalled and never sent (ADR 0026, point 3, "Mode").
type OrderReleasedPayload struct {
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Mode          string      `json:"mode"`
	Send          bool        `json:"send"`
	Figures       RiskFigures `json:"figures"`
}

// Validate checks identity, mode, the dry-run rule and the figures.
func (p OrderReleasedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	if p.Tag == "" {
		errs = append(errs, errors.New("tag is required: no order is released without a journalled decision"))
	}
	if !validTradingMode(p.Mode) {
		errs = append(errs, fmt.Errorf("mode %q is not dry-run, paper or live", p.Mode))
	}
	if p.Mode == TradingModeDryRun && p.Send {
		errs = append(errs, errors.New("a dry-run release is never sent"))
	}
	errs = append(errs, p.Figures.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order released payload: %w", err)
	}
	return nil
}

// OrderRefusedPayload is a planned order the gate refused, with its reason
// from the closed set and the figures behind it. Tag may be empty: an
// untagged order is exactly what RefusedUndecidedOrder refuses.
type OrderRefusedPayload struct {
	AccountID     string      `json:"account_id"`
	PlanID        string      `json:"plan_id"`
	Tag           string      `json:"tag"`
	ClientOrderID string      `json:"client_order_id"`
	InstrumentID  string      `json:"instrument_id"`
	Side          string      `json:"side"`
	Quantity      int64       `json:"quantity"`
	LimitPrice    float64     `json:"limit_price"`
	Reason        string      `json:"reason"`
	Detail        string      `json:"detail"`
	Figures       RiskFigures `json:"figures"`
}

// Validate checks identity, the reason, the detail and the figures.
func (p OrderRefusedPayload) Validate() error {
	var errs []error
	errs = append(errs, orderIdentity(p.AccountID, p.ClientOrderID, p.InstrumentID, p.Side, p.Quantity, p.LimitPrice)...)
	if p.PlanID == "" {
		errs = append(errs, errors.New("plan id is required"))
	}
	switch p.Reason {
	case RefusedUndecidedOrder, RefusedMarginExposure, RefusedOrderCount, RefusedOrderSize,
		RefusedPriceBand, RefusedMissingMark, RefusedEngineDegraded, RefusedEngineHalted,
		RefusedIRAExposure, RefusedInsufficientCash, RefusedSellExceedsHolding, RefusedUnvaluedHolding:
	default:
		errs = append(errs, fmt.Errorf("refusal reason %q is not recognised", p.Reason))
	}
	if p.Detail == "" {
		errs = append(errs, errors.New("detail is required: a refusal without its figures cannot be checked"))
	}
	errs = append(errs, p.Figures.validate()...)
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid order refused payload: %w", err)
	}
	return nil
}
