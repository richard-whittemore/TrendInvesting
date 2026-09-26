// Package fills is the intraday fill model of ADR 0005 and the cost model of
// ADR 0013: the component that holds the resting orders in force for an
// instrument and decides, from one completed bar, which of them executed, at
// what price, and in what order.
//
// # Why "fills" and not "simulator"
//
// The package is named for the domain concept it owns — the fill, and the
// model that produces one — rather than for the fact that in slice 1 it is
// simulating. That follows the rest of internal/: "indicator" owns a
// measurement, "sizing" owns an arithmetic, "strategy" owns the decisions.
// The rules in here are not simulation-specific: the same three rules are
// what a reviewer will check a LEAN or broker fill against, and a package
// called "simulator" would misdescribe that second use the moment it
// arrives. The Source stamped on the envelopes this package produces IS
// "simulator", because that field answers a different question — who
// produced this particular event — and in a backtest the answer is the
// simulation.
//
// # What it is not
//
// It makes no strategy decision. It never chooses a level, a size, a stop, or
// whether an order should exist: every resting order it holds was put there
// by an event internal/strategy emitted, and every level it fills against was
// computed by that reducer from completed prior bars. Nor does it hold
// position state: the Campaign records in here exist only to know which
// orders are still in force, and the authority on the position is always the
// reducer (docs/architecture.md: "Positions, protective stops, and pyramid
// state change only from recorded brokerage events").
//
// It is pure and deterministic in the sense the rest of internal/ is: no
// clock, no randomness, no map iteration affecting output, no I/O. Time
// arrives in the events.
//
// # Prices, and which view they are read from
//
// Every comparison and every fill price is read from the bar's SPLIT-ADJUSTED
// view (ADR 0004), because that is the view every level the simulator fills
// against was computed on: an Entry Channel high, an Exit Channel low, an Add
// rung and a Protective Stop are all derived from split-adjusted prices by
// the reducer, and comparing them against raw prices would silently fill
// orders at levels that never existed. The fill is reported in the same view
// it was decided in, which is what keeps a Campaign's whole life in one
// view: ADR 0004, as amended, requires that a Campaign's money never
// subtracts prices read from two different views.
//
// # The commission schedule the Baseline declares
//
// ADR 0013 names Interactive Brokers' fee model. The Baseline's figures —
// $0.005 per share, a $1.00 minimum per order, capped at 1 % of trade value —
// are IB's published US-stock Fixed pricing as it stood on 2026-09-11
// (https://www.interactivebrokers.com/en/pricing/commissions-stocks.php).
// They are a baseline-declared adaptation under ADR 0012's provenance
// taxonomy, not a methodology source, and they live in
// event.ConfigurationPayload.Commission rather than in this code, so a
// Variant that runs a different schedule is a declared experiment. Note that
// the page above refuses automated retrieval, so the figures were read from a
// search of it on that date rather than from the page itself; every test that
// depends on them parameterises them, and whoever owns the Baseline
// configuration must confirm them against the live schedule before any
// paper-trading or limited-live gate.
package fills

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// Source is stamped on every envelope this package produces. It names the
// component that emitted the event (docs/architecture.md), and is the ONLY
// thing that distinguishes a simulated fill from an adapter-produced one: the
// payload, the schema version and every field on it are identical in shape
// between the two producers.
const Source = "simulator"

// idTimeLayout is the fixed, nanosecond-precision layout every deterministic
// id in this system is built from, matching internal/strategy's own: two bars
// can never collapse to one id through formatting, and no wall clock or
// randomness is involved (.golangci.yml forbids both in internal/).
const idTimeLayout = "2006-01-02T15:04:05.000000000Z"

// maxFillsPerBar bounds the per-bar fixpoint in RunSession.
//
// Termination does not depend on it: every pass either fills an order —
// removing it, or removing the Units it closed — or stops, and new orders
// arrive only from the reducer, which is itself bounded by the Campaign's
// frozen maximum Units (ADR 0008). The Baseline's four Units make at most ten
// fills in one bar. This is the backstop for a reducer that ever stopped
// being bounded, and it fails the run rather than looping.
const maxFillsPerBar = 64

// Simulator holds the resting orders in force, per instrument, and the
// minimum it needs to know about each open Campaign to keep them right.
//
// It is not safe for concurrent use, matching replay.Engine's sequential Run
// and internal/strategy.Reducer's own per-run statefulness.
type Simulator struct {
	slippageN       float64
	commission      CommissionModel
	dollarsPerPoint float64

	strategyVersion   string
	configurationHash string

	// sequence numbers the COMPOSED input stream — bars and the fills
	// interleaved between them. It lives here, rather than being taken from
	// the bar producer, because only the loop knows the running order once
	// fills are interleaved; see RunSession.
	sequence uint64

	books map[string]*book

	// recordedAt is the RecordedAt of the bar RunSession is currently working,
	// which is what every fill decided from that bar inherits: the simulator
	// observes the same recording moment as the data that produced it, and
	// has no clock of its own (.golangci.yml forbids time.Now in internal/).
	//
	// RunSession sets it from each bar's envelope BEFORE that bar's
	// open-instant pass, and again before its intrabar fixpoint, which runs
	// after the Session's close (ADR 0021) — deliberately, and not as a side
	// effect of
	// delivering the bar, because that pass delivers its fills before the bar
	// itself. A stamp that followed delivery order would date those fills to
	// the previous bar.
	recordedAt time.Time

	// startingEquity is the configured starting equity (ADR 0007), which the
	// simulated account's equity opens at (see OpenAccount).
	startingEquity float64
	// account is the simulated brokerage account, nil until OpenAccount.
	account *account
	// sessionsRun counts the Sessions RunSession has run.
	sessionsRun int
}

// book is one instrument's resting orders.
//
// At most one proposal of each kind can be outstanding for an instrument at a
// time — internal/strategy guarantees it (a pending entry proposal is always
// cleared before a Campaign, and so an exit or Add proposal, can exist; exit
// and Add proposals are mutually exclusive per bar under ADR 0010's exit
// precedence) — so each is a single slot rather than a list, and a second one
// arriving would be a reducer defect this package would rather notice than
// absorb.
//
// The two buys, entry and add, are orders in their own right. The sells are
// not held here: each held Unit rests exactly one, its Exit Order, on the
// Unit itself (see unit). exit is the outstanding Exit-Channel exit proposal,
// which is not an order of its own — it is one of the two levels a Unit's
// Exit Order is derived from — and is kept so that a Unit whose Exit Order
// rests at the exit level is reported as filling that proposal.
type book struct {
	campaign *campaign
	entry    *order
	add      *order
	exit     *exitProposal
}

// order is one resting buy: an entry or an Add.
type order struct {
	kind       string // event.FillKind*
	side       string
	proposalID string
	campaignID string
	level      float64
	quantity   int64
	// n is the N this order's slippage is measured in: the decision N the
	// Signal was sized under for an entry (ADR 0003), the Campaign's frozen N
	// for an Add (ADR 0006).
	n float64
	// orderType is the order it rests as (ADR 0005, as amended 2026-09-24),
	// and decides how it fills; priceCap is a stop-limit's limit, its
	// proposal's own PriceCap, and is never read for a stop-market order.
	orderType event.OrderType
	priceCap  float64
	ref       reference
}

// exitProposal is the outstanding Exit-Channel exit proposal for an open
// Campaign: the proposal an exit fill reports, and the level a Unit's Exit
// Order must rest at when it names the Exit Channel as its source.
type exitProposal struct {
	proposalID string
	campaignID string
	level      float64
}

// reference is the price an order was resting from, and the bar that price
// belongs to. See Range.Reference for why an order created by a fill inside a
// bar must not be referenced to that bar's open.
//
// The zero value means "no reference of its own": the order was already
// resting when the bar opened, so the bar's open is its reference. A
// reference whose bar is not the bar being evaluated has expired the same
// way — by the next bar the order has been resting since before the open.
type reference struct {
	bar   time.Time
	price float64
}

// rangeFor is the bar's Range as this order sees it.
func (r reference) rangeFor(periodEnd time.Time, view event.PriceView) Range {
	price := view.Open
	if !r.bar.IsZero() && r.bar.Equal(periodEnd) {
		price = r.price
	}
	return Range{Reference: price, High: view.High, Low: view.Low}
}

// campaign is the minimum this package needs to know about an open Campaign:
// which Units it still holds, each with its own current Exit Order.
//
// It is deliberately not position state. internal/strategy owns that, and
// every field here is a copy of something the reducer journalled; nothing in
// this package ever decides that a Unit exists, only that an order belonging
// to one is still resting.
type campaign struct {
	id           string
	instrumentID string
	direction    string
	// n is the Campaign's frozen N (ADR 0006), which the slippage on every
	// Exit Order is measured in.
	n float64
	// units is every Unit still open, in ascending index order. Held as a
	// slice rather than a map so iteration order is the index order, never
	// Go's map order (.greptile/rules.md's determinism rule).
	units []*unit
}

// unit is one held Unit and the one sell order resting for it: its Exit
// Order (CONTEXT.md: "Exit Order"), at the higher of its own Protective Stop
// and, while an Exit-Channel exit is proposed, that exit's level. The level
// is the reducer's, read from strategy.exit-order.set; this package never
// combines the two levels itself.
type unit struct {
	index         int
	openingFillID string
	quantity      int64
	// exitLevel is where this Unit's Exit Order rests. Zero until the first
	// strategy.exit-order.set for it arrives — which the reducer emits in the
	// same Apply return as the Unit itself — and until then nothing rests
	// for it.
	exitLevel float64
	// exitSource is the event.ExitOrderSource* governing exitLevel, which
	// decides whether a fill of the order is reported as a stop or an exit.
	exitSource string
	ref        reference
}

func (c *campaign) unitAt(index int) *unit {
	for _, u := range c.units {
		if u.index == index {
			return u
		}
	}
	return nil
}

func (c *campaign) removeUnits(indexes []int) {
	remaining := c.units[:0]
	for _, u := range c.units {
		keep := true
		for _, index := range indexes {
			if u.index == index {
				keep = false
				break
			}
		}
		if keep {
			remaining = append(remaining, u)
		}
	}
	c.units = remaining
}

// New returns a Simulator configured from cfg and stamping every envelope it
// produces with Source, strategyVersion and configurationHash — the same
// provenance contract internal/strategy.NewReducer holds itself to, for the
// same reason: an event that cannot be traced to the code and configuration
// that produced it is not auditable evidence (docs/architecture.md).
//
// A configuration with a non-positive SlippageN is refused here, before
// cfg.Validate is consulted, so the error names the actual rule: ADR 0013
// says a backtest run with zero slippage is invalid by construction, and the
// simulator is the component that would otherwise produce the unslipped fills
// such a run consists of. The whole configuration is then validated too, so a
// simulator can never be built from one the reducer would reject.
func New(cfg event.ConfigurationPayload, strategyVersion, configurationHash string) (*Simulator, error) {
	switch {
	case !isFinite(cfg.SlippageN):
		return nil, errors.New("fills: slippage in n must be finite")
	case cfg.SlippageN <= 0:
		return nil, fmt.Errorf("fills: slippage in n must be positive, got %v: a run with zero slippage is invalid by construction (ADR 0013), and this is the component that would produce its unslipped fills", cfg.SlippageN)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("fills: %w", err)
	}
	if strategyVersion == "" {
		return nil, errors.New("fills: strategy version is required")
	}
	if configurationHash == "" {
		return nil, errors.New("fills: configuration hash is required")
	}
	return &Simulator{
		slippageN: cfg.SlippageN,
		commission: CommissionModel{
			PerShare:                    cfg.Commission.PerShare,
			MinimumPerOrder:             cfg.Commission.MinimumPerOrder,
			MaximumFractionOfTradeValue: cfg.Commission.MaximumFractionOfTradeValue,
		},
		dollarsPerPoint:   cfg.DollarsPerPoint,
		startingEquity:    cfg.NotionalAccount.StartingEquity,
		strategyVersion:   strategyVersion,
		configurationHash: configurationHash,
		books:             make(map[string]*book),
	}, nil
}

// Observe folds one envelope into the resting-order book.
//
// The whole book is learned from the reducer's own emissions: a trade or Add
// proposal creates a resting buy; a Campaign-opened or Unit-added event
// introduces a Unit, and an Exit-Order-set event places or moves that Unit's
// one sell order; an exit proposal records the proposal an Exit Order at the
// Exit Channel fills; a proposal-expired, units-stopped or Campaign-exited
// event removes what is no longer in force. Nothing here is inferred from
// bars or invented.
//
// An event type it does not recognise fails closed rather than being ignored.
// That is deliberate and is the opposite of what a "just ignore what you do
// not need" reading would do: this package's correctness depends on its book
// matching what the reducer believes, and a new decision event that creates
// or cancels an order would, if silently dropped, leave the book quietly
// wrong — filling orders the strategy has withdrawn, or missing ones it has
// made. docs/development.md principle 4, applied to emissions rather than
// inputs.
func (s *Simulator) Observe(envelope event.Envelope) error {
	return s.observe(envelope, reference{})
}

// observe is Observe with the reference to attach to any order the envelope
// creates. RunSession passes the price of the fill that caused the emission; a
// caller with no such context (Observe itself) passes the zero reference,
// which means "the bar's open".
func (s *Simulator) observe(envelope event.Envelope, ref reference) error {
	switch envelope.Type {
	case event.TradeProposalEventType:
		return s.observeTradeProposal(envelope, ref)
	case event.AddProposalEventType:
		return s.observeAddProposal(envelope, ref)
	case event.ExitProposalEventType:
		return s.observeExitProposal(envelope)
	case event.CampaignOpenedEventType:
		return s.observeCampaignOpened(envelope)
	case event.CampaignUnitAddedEventType:
		return s.observeUnitAdded(envelope)
	case event.ProtectiveStopSetEventType:
		return s.observeProtectiveStopSet(envelope)
	case event.ExitOrderSetEventType:
		return s.observeExitOrderSet(envelope, ref)
	case event.CampaignUnitsStoppedEventType:
		return s.observeUnitsStopped(envelope)
	case event.CampaignExitedEventType:
		return s.observeCampaignExited(envelope)
	case event.ProposalExpiredEventType:
		return s.observeProposalExpired(envelope)
	case event.CampaignCashInLieuEventType:
		return s.observeCashInLieu(envelope)
	case event.CampaignDividendEventType:
		return s.observeDividend(envelope)
	case event.InstrumentSymbolChangedEventType:
		return s.observeSymbolChanged(envelope)

	// Recognised and deliberately without effect on the resting-order book.
	// Listed one by one rather than caught by a default branch, so that a
	// NEW event type reaches the error below and is considered rather than
	// absorbed.
	case event.WatchlistPublishedEventType, // ADR 0011: observability only, never a resting-order effect
		event.SetupEvaluatedEventType,              // a Setup was evaluated; no order
		event.SignalEventType,                      // a Signal fired; the proposal that follows is the order
		event.ProposalDeclinedEventType,            // a Signal produced no position, so no order
		event.CampaignEvaluatedEventType,           // the levels in force; the orders come from the exit-order-set events
		event.EngineStateEventType,                 // a halt; the run stops, so the book is moot
		event.DrawdownStepAppliedEventType,         // ADR 0007's ladder; affects sizing, not resting orders
		event.NotionalAccountRebasedEventType,      // likewise
		event.NotionalAccountRecoveredEventType,    // likewise
		event.NotionalAccountCashAdjustedEventType, // likewise
		event.ConfigurationEventType,               // an input, carried at construction
		event.CompletedBarEventType,                // an input, handled by RunSession itself
		event.SessionClosedEventType,               // likewise
		event.MarketCorporateActionEventType,       // any kind: the reducer's emissions resolve it (ADR 0009's cancellations, ADR 0023's cash in lieu, ADR 0024's dividend and symbol change)
		event.RunCompletedEventType,                // ADR 0011: proposal-expired and exit-order-set emissions resolve the book
		event.FillEventType,                        // this package's own output
		event.AccountSnapshotEventType,             // an input; no resting-order consequence
		event.CashMovementEventType:                // likewise
		return nil
	default:
		return fmt.Errorf("fills: event type %q is not recognised; the resting-order book is learned entirely from the reducer's emissions, so an unrecognised one may create or cancel an order and is refused rather than ignored", envelope.Type)
	}
}

func (s *Simulator) bookFor(instrumentID string) *book {
	if b, ok := s.books[instrumentID]; ok {
		return b
	}
	b := &book{}
	s.books[instrumentID] = b
	return b
}

func decodePayload[T any](envelope event.Envelope, into *T) error {
	if err := json.Unmarshal(envelope.Payload, into); err != nil {
		return fmt.Errorf("fills: decode %s payload: %w", envelope.Type, err)
	}
	return nil
}

func (s *Simulator) observeTradeProposal(envelope event.Envelope, ref reference) error {
	var payload event.TradeProposalPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	priceCap, err := buyPriceCap(envelope, payload.OrderType, payload.PriceCap, payload.EntryLevel)
	if err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.entry = &order{
		orderType:  payload.OrderType,
		kind:       event.FillKindEntry,
		side:       SideBuy,
		proposalID: envelope.ID,
		level:      payload.EntryLevel,
		quantity:   payload.Quantity,
		n:          payload.N,
		priceCap:   priceCap,
		ref:        ref,
	}
	return nil
}

// buyPriceCap is the limit an entry or Add proposal rests with: its
// PriceCap for a stop-limit, none for a stop-market (ADR 0005, as amended
// 2026-09-24). A stop-limit's cap must be a finite price at or above its
// level: zero is not a way of saying "uncapped", which only the order type
// says, and a cap the simulator could not honour would let a fill cost more
// than its hold. A stop-market proposal stating a cap contradicts itself and
// is refused too. Any other order type fails closed: this simulator would
// otherwise rest an order the reducer did not describe.
func buyPriceCap(envelope event.Envelope, orderType event.OrderType, priceCap, level float64) (float64, error) {
	switch orderType {
	case event.OrderTypeStopLimit:
		if !isFinite(priceCap) || priceCap <= 0 || priceCap < level {
			return 0, fmt.Errorf("fills: stop-limit proposal %s at level %v states price cap %v, which is not a finite price at or above its level; refusing an order whose limit could not bound its fill (ADR 0005)", envelope.ID, level, priceCap)
		}
		return priceCap, nil
	case event.OrderTypeStopMarket:
		if priceCap != 0 {
			return 0, fmt.Errorf("fills: stop-market proposal %s states price cap %v; a stop-market order has none, so resting it would ignore the cap it states (ADR 0005)", envelope.ID, priceCap)
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("fills: proposal %s rests as order type %q, which this simulator cannot fill; refusing rather than guessing (ADR 0005)", envelope.ID, orderType)
	}
}

func (s *Simulator) observeAddProposal(envelope event.Envelope, ref reference) error {
	var payload event.AddProposalPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	priceCap, err := buyPriceCap(envelope, payload.OrderType, payload.PriceCap, payload.Level)
	if err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.add = &order{
		orderType:  payload.OrderType,
		kind:       event.FillKindAdd,
		side:       SideBuy,
		proposalID: envelope.ID,
		campaignID: payload.CampaignID,
		level:      payload.Level,
		quantity:   payload.Quantity,
		n:          payload.EffectiveN(),
		priceCap:   priceCap,
		ref:        ref,
	}
	return nil
}

// observeExitProposal records the outstanding exit proposal. It places no
// order: each Unit's Exit Order moves to the exit level only when the
// reducer's strategy.exit-order.set says so, because the exit level governs a
// Unit only where it is above that Unit's own Protective Stop. A proposal for
// a Campaign this package has never seen open fails closed: an exit fill is
// priced with that Campaign's frozen N (ADR 0006), and there is none to use.
func (s *Simulator) observeExitProposal(envelope event.Envelope) error {
	var payload event.ExitProposalPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: exit proposal %q names campaign %q, which this simulator has no open campaign for; the slippage on an exit is measured in that campaign's frozen n (ADR 0006) and cannot be derived without it",
			payload.InstrumentID, envelope.ID, payload.CampaignID)
	}
	b.exit = &exitProposal{
		proposalID: envelope.ID,
		campaignID: payload.CampaignID,
		level:      payload.Level,
	}
	return nil
}

func (s *Simulator) observeCampaignOpened(envelope event.Envelope) error {
	var payload event.CampaignOpenedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.campaign = &campaign{
		id:           payload.CampaignID,
		instrumentID: payload.InstrumentID,
		direction:    payload.Direction,
		n:            payload.CampaignN,
		units: []*unit{{
			index:         1,
			openingFillID: payload.FillID,
			quantity:      payload.FilledQuantity,
		}},
	}
	// The proposal this fill executed is resolved, so it is no longer
	// resting. The reducer clears its own pending proposal at the same
	// moment, for the same reason.
	b.entry = nil
	return nil
}

func (s *Simulator) observeUnitAdded(envelope event.Envelope) error {
	var payload event.CampaignUnitAddedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: unit-added names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	b.campaign.units = append(b.campaign.units, &unit{
		index:         payload.UnitIndex,
		openingFillID: payload.FillID,
		quantity:      payload.Quantity,
	})
	b.add = nil
	return nil
}

// observeProtectiveStopSet checks that a Protective Stop set or raised by
// the reducer belongs to a Unit this book holds, and places no order: the
// stop is one of the two levels a Unit's Exit Order is derived from, and the
// reducer records the order itself, at the level the stop and any proposed
// exit together decide, with strategy.exit-order.set (observeExitOrderSet).
// A stop for a Campaign or Unit the book does not hold still fails closed,
// because the Exit Order that follows it would have nothing to rest on.
func (s *Simulator) observeProtectiveStopSet(envelope event.Envelope) error {
	var payload event.ProtectiveStopSetPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: protective-stop-set names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	if b.campaign.unitAt(payload.UnitIndex) == nil {
		return fmt.Errorf("fills: instrument %q: protective-stop-set names unit %d of campaign %q, which this simulator does not hold", payload.InstrumentID, payload.UnitIndex, payload.CampaignID)
	}
	return nil
}

// observeExitOrderSet places or moves one Unit's Exit Order (CONTEXT.md:
// "Exit Order"): the one sell order that Unit rests, for its own shares, at
// the level the reducer recorded — the higher of its Protective Stop and,
// while an Exit-Channel exit is proposed, that exit's level (ADR 0005, as
// amended). It is the only event that puts a sell order in this book.
//
// Every disagreement with the rest of what the producer has said fails
// closed: an order for a Unit the book does not hold, for other than that
// Unit's own shares, or naming the Exit Channel when no exit is proposed at
// that level. Placing it anyway would sell shares the Unit does not hold, or
// report an exit fill for a proposal that is not the one outstanding.
func (s *Simulator) observeExitOrderSet(envelope event.Envelope, ref reference) error {
	var payload event.ExitOrderSetPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	// The payload's own contract (ADR 0015): a zero, negative or non-finite
	// level would otherwise be stored and then skipped as "no order",
	// silently leaving the Unit unprotected.
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("fills: instrument %q: invalid exit-order-set payload: %w", payload.InstrumentID, err)
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: exit-order-set names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	u := b.campaign.unitAt(payload.UnitIndex)
	if u == nil {
		return fmt.Errorf("fills: instrument %q: exit-order-set names unit %d of campaign %q, which this simulator does not hold", payload.InstrumentID, payload.UnitIndex, payload.CampaignID)
	}
	if payload.Quantity != u.quantity {
		return fmt.Errorf("fills: instrument %q: exit-order-set rests unit %d of campaign %q for quantity %d, but the unit holds %d; a Unit's Exit Order covers exactly its own shares",
			payload.InstrumentID, payload.UnitIndex, payload.CampaignID, payload.Quantity, u.quantity)
	}
	switch payload.Source {
	case event.ExitOrderSourceProtectiveStop:
		// The Unit's own stop governs; nothing else to reconcile.
	case event.ExitOrderSourceExitChannel:
		if b.exit == nil || b.exit.campaignID != payload.CampaignID {
			return fmt.Errorf("fills: instrument %q: exit-order-set rests unit %d of campaign %q at the exit channel, but there is no exit proposal outstanding for that campaign",
				payload.InstrumentID, payload.UnitIndex, payload.CampaignID)
		}
		// Exact comparison: the reducer copies the proposed level into the
		// Exit Order unchanged (event.ExitOrderSetPayload's doc comment).
		if payload.Level != b.exit.level {
			return fmt.Errorf("fills: instrument %q: exit-order-set rests unit %d of campaign %q at the exit channel level %v, but the outstanding exit proposal %q is at %v",
				payload.InstrumentID, payload.UnitIndex, payload.CampaignID, payload.Level, b.exit.proposalID, b.exit.level)
		}
	default:
		return fmt.Errorf("fills: instrument %q: exit-order-set for unit %d of campaign %q names source %q, which is not a recognised exit order source",
			payload.InstrumentID, payload.UnitIndex, payload.CampaignID, payload.Source)
	}
	u.exitLevel = payload.Level
	u.exitSource = payload.Source
	u.ref = ref
	return nil
}

func (s *Simulator) observeUnitsStopped(envelope event.Envelope) error {
	var payload event.CampaignUnitsStoppedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: units-stopped names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	b.campaign.removeUnits(payload.UnitIndexes)
	return nil
}

// observeCampaignExited drops the Campaign and every order belonging to it.
//
// The Add order is dropped here as well as on the reducer's own expiry event.
// A stop fill that closes a Campaign, in part or in full, already expires
// any outstanding Add proposal in the same reply, before campaign-exited
// (ADR 0011, as amended 2026-09-24), so this is belt and braces: filling an
// Add order that outlived its Campaign would be a fill for a Campaign that
// no longer exists.
func (s *Simulator) observeCampaignExited(envelope event.Envelope) error {
	var payload event.CampaignExitedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.campaign = nil
	b.add = nil
	b.exit = nil
	if s.account != nil && payload.Reason == event.ExitReasonDelisting {
		return s.account.delist(payload.InstrumentID)
	}
	return nil
}

// observeCashInLieu applies a split's cash in lieu as the reducer decided it
// (ADR 0023): each named Unit shrinks from its quantity before to its
// quantity after, and the simulated account, if kept, holds the shares lost
// fewer and is credited the cash. The Units' Exit Orders are re-rested by
// the strategy.exit-order.set decisions that follow it, so its level is left
// alone here.
//
// Every disagreement with the book fails closed before anything moves: a
// Campaign the book does not hold, a Unit it does not hold, or a Unit whose
// quantity is not the decision's quantity before, or an account that does
// not hold the Campaign's quantity before. Applying it anyway would shrink
// the wrong shares.
func (s *Simulator) observeCashInLieu(envelope event.Envelope) error {
	var payload event.CampaignCashInLieuPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("fills: instrument %q: %w", payload.InstrumentID, err)
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: cash in lieu names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	units := make([]*unit, len(payload.Reductions))
	for i, r := range payload.Reductions {
		u := b.campaign.unitAt(r.UnitIndex)
		if u == nil {
			return fmt.Errorf("fills: instrument %q: cash in lieu reduces unit %d of campaign %q, which this simulator does not hold", payload.InstrumentID, r.UnitIndex, payload.CampaignID)
		}
		if u.quantity != r.QuantityBefore {
			return fmt.Errorf("fills: instrument %q: cash in lieu reduces unit %d of campaign %q from %d shares, but the unit holds %d", payload.InstrumentID, r.UnitIndex, payload.CampaignID, r.QuantityBefore, u.quantity)
		}
		units[i] = u
	}
	if s.account != nil {
		if err := s.account.cashInLieu(payload.InstrumentID, payload.QuantityBefore, payload.EngineSharesLost, payload.CashInLieu); err != nil {
			return err
		}
	}
	for i, u := range units {
		u.quantity = payload.Reductions[i].QuantityAfter
	}
	return nil
}

// observeDividend credits the simulated account the dividend's cash (ADR
// 0024), with no effect on the resting-order book: a dividend changes no
// Unit's quantity and re-rests no Exit Order, unlike a split's cash in lieu.
func (s *Simulator) observeDividend(envelope event.Envelope) error {
	var payload event.CampaignDividendPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("fills: instrument %q: %w", payload.InstrumentID, err)
	}
	if s.account != nil {
		s.account.dividend(payload.CashAmount)
	}
	return nil
}

// observeSymbolChanged moves everything this package learned about the old
// instrument id to the new one (ADR 0024): the resting-order book — so a
// standing entry, Add or exit order is still found under the id the next
// bar's fills are priced against — the campaign's own denormalised
// instrumentID (the fills package's own campaign struct, read only in this
// package's error messages), and, when a simulated account is kept, its
// holding and latest close.
//
// This mirrors the reducer's own applySymbolChange (internal/strategy):
// nothing here decides whether the change is valid — the reducer's
// InstrumentSymbolChangedPayload is only ever produced once every guard has
// already passed — so this package just carries state across the same way
// it learns everything else, entirely from the reducer's emissions.
func (s *Simulator) observeSymbolChanged(envelope event.Envelope) error {
	var payload event.InstrumentSymbolChangedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("fills: instrument %q: %w", payload.InstrumentID, err)
	}
	if b, ok := s.books[payload.InstrumentID]; ok {
		delete(s.books, payload.InstrumentID)
		s.books[payload.NewInstrumentID] = b
		if b.campaign != nil {
			b.campaign.instrumentID = payload.NewInstrumentID
		}
	}
	if s.account != nil {
		if err := s.account.renameInstrument(payload.InstrumentID, payload.NewInstrumentID); err != nil {
			return err
		}
	}
	return nil
}

// observeProposalExpired drops whichever order the expiry names. It covers
// both of the reducer's expiry paths: ADR 0011's ordinary next-bar expiry,
// and the cancellation of a pending Add the instant a stop fill partially
// closes the Campaign.
//
// It matches on the proposal id rather than on Kind alone, so an expiry for a
// proposal this package never held — or one it has already resolved with a
// fill — is a no-op rather than clearing an unrelated order.
func (s *Simulator) observeProposalExpired(envelope event.Envelope) error {
	var payload event.ProposalExpiredPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	for _, slot := range []**order{&b.entry, &b.add} {
		if *slot != nil && (*slot).proposalID == payload.ProposalID {
			*slot = nil
		}
	}
	if b.exit != nil && b.exit.proposalID == payload.ProposalID {
		b.exit = nil
	}
	return nil
}

// Resting returns the orders in force for instrumentID, in a deterministic
// order: the entry and Add orders first, then one Exit Order per held Unit
// that has one, in ascending Unit order — Kind event.FillKindExit, naming
// the exit proposal, where the Unit's Exit Order rests at the Exit Channel,
// and event.FillKindStop where it rests at its own Protective Stop. It
// exists for inspection — by a test, or by a driver reporting what was left
// outstanding when a run ended — and never to be mutated.
func (s *Simulator) Resting(instrumentID string) []Order {
	b, ok := s.books[instrumentID]
	if !ok {
		return nil
	}
	var out []Order
	for _, o := range []*order{b.entry, b.add} {
		if o == nil {
			continue
		}
		out = append(out, Order{
			Kind: o.kind, Side: o.side, InstrumentID: instrumentID,
			CampaignID: o.campaignID, ProposalID: o.proposalID,
			Level: o.level, PriceCap: o.priceCap, Quantity: o.quantity, N: o.n,
		})
	}
	if b.campaign != nil {
		for _, u := range b.campaign.units {
			if u.exitLevel <= 0 {
				continue
			}
			kind, proposalID := event.FillKindStop, ""
			if u.exitSource == event.ExitOrderSourceExitChannel && b.exit != nil {
				kind, proposalID = event.FillKindExit, b.exit.proposalID
			}
			out = append(out, Order{
				Kind: kind, Side: SideSell, InstrumentID: instrumentID,
				CampaignID: b.campaign.id, ProposalID: proposalID,
				Level: u.exitLevel, Quantity: u.quantity, N: b.campaign.n,
				UnitIndexes: []int{u.index}, UnitIDs: []string{u.openingFillID},
			})
		}
	}
	return out
}

// Order is one resting order, as reported by Resting. It is a read-only
// snapshot, not the simulator's own state.
type Order struct {
	Kind         string
	Side         string
	InstrumentID string
	CampaignID   string
	ProposalID   string
	Level        float64
	// PriceCap is a stop-limit buy's limit, zero for an order with none.
	PriceCap    float64
	Quantity    int64
	N           float64
	UnitIndexes []int
	UnitIDs     []string
}

// candidate is one order the current bar covers, priced.
type candidate struct {
	kind        string
	side        string
	proposalID  string
	campaignID  string
	level       float64
	quantity    int64
	n           float64
	unitIndexes []int
	unitIDs     []string
	price       float64
	atReference bool
	// orderQuantities lists, when this fill journals several broker orders
	// together, each order's own quantity. ADR 0013's commission minimum and
	// ceiling apply per order, so the fill's commission is the sum of each
	// order's charge, never one charge on the combined quantity. Empty means
	// the fill is one order of quantity.
	orderQuantities []int64
}

// covered prices every resting order for instrumentID against this bar and
// returns the ones it reached, in the order they are to be filled: buys
// first (ADR 0005 rule 3), then every covered Exit Order resting at a Unit's
// own Protective Stop, worst price first, and only once none of those is
// left, the Exit Orders resting at the Exit Channel, as one exit fill. See
// RunBar's doc comment for the enumeration behind that ordering.
func (s *Simulator) covered(instrumentID string, periodEnd time.Time, view event.PriceView) ([]candidate, error) {
	b, ok := s.books[instrumentID]
	if !ok {
		return nil, nil
	}
	var buys []candidate

	for _, o := range []*order{b.entry, b.add} {
		if o == nil {
			continue
		}
		c, filled, err := s.priceBuy(o, o.ref.rangeFor(periodEnd, view))
		if err != nil {
			return nil, err
		}
		if !filled {
			continue
		}
		c.proposalID = o.proposalID
		c.campaignID = o.campaignID
		buys = append(buys, c)
	}

	// Buys in ascending level: a lower buy-stop is reached on the way up
	// before a higher one, and the Add chain depends on that order anyway
	// (rung n+1 is measured from rung n's fill, so it cannot be evaluated
	// first). At most one buy is ever outstanding for an instrument in
	// practice; the sort makes the order stated rather than incidental.
	sortStable(buys, func(a, b candidate) bool { return a.level < b.level })

	if b.campaign == nil {
		return buys, nil
	}
	sells, err := s.coveredExitOrders(b.campaign, b.exit, periodEnd, view)
	if err != nil {
		return nil, err
	}
	return append(buys, sells...), nil
}

// coveredExitOrders prices every held Unit's Exit Order against this bar and
// returns the ones it reached, in fill order.
//
// # One order per Unit, and so no competing sells
//
// Each Unit rests exactly one sell order, for its own shares, at the level
// the reducer's strategy.exit-order.set last recorded for it (CONTEXT.md:
// "Exit Order"; ADR 0005, as amended). No two orders ever sell the same
// shares, so every covered order fills at its own level on ADR 0005's own
// terms — min(level, reference) less slippage — whatever order they are
// delivered in, and the total sold can never exceed what the Campaign holds.
// A Unit with no Exit Order recorded yet has nothing resting and is skipped:
// the reducer emits a Unit's first Exit Order in the same Apply return as
// the Unit itself, so this is unreachable in the composed loop, and skipping
// rather than erroring keeps the order of those emissions from becoming
// load-bearing here.
//
// # How each fill is reported
//
// A Unit whose Exit Order rests at its own Protective Stop fills as an
// event.FillKindStop naming that one Unit, which the reducer applies as a
// stop — and so as a partial close when other Units remain. Units never
// share a stop level under this system's rules: Unit k+1's stop is its own
// fill less 2 N, Unit k's has risen by half N, and the two coincide only if
// the later Unit filled exactly half an N above the earlier one, which
// slippage, strictly positive by ADR 0013, always prevents. So there is one
// stop fill per Unit (The Turtle Rules p.22-23's Crude example keeps each
// Unit's stop at its own level).
//
// Units whose Exit Orders rest at the Exit Channel all rest at the one
// proposed exit level, and fill as a single event.FillKindExit for the exit
// proposal. The reducer accepts an exit fill only for everything the
// Campaign still holds (every Unit exits together; CONTEXT.md: "Campaign"),
// so it is offered only once no stop fill is left to deliver: a Unit resting
// at its own stop does so because that stop is at or above the exit level,
// so any bar that reaches the exit level has reached it too, and by the time
// the exit is offered those Units are closed. Should a held Unit nonetheless
// remain outside the exit — an order the bar did not reach, or none at all —
// the exit fill cannot describe what happened and this fails closed.
//
// Stop fills keep ADR 0005 rule 3's worst-price-first order among
// themselves. Since no two sells compete for the same shares, that order
// changes no price; it only fixes the order in which they are journalled.
func (s *Simulator) coveredExitOrders(c *campaign, proposal *exitProposal, periodEnd time.Time, view event.PriceView) ([]candidate, error) {
	var stops []candidate
	var atExit []*unit
	for _, u := range c.units {
		if u.exitLevel <= 0 {
			continue
		}
		if u.exitSource == event.ExitOrderSourceExitChannel {
			atExit = append(atExit, u)
			continue
		}
		cand, filled, err := s.price(event.FillKindStop, SideSell, u.exitLevel, c.n, u.quantity, u.ref.rangeFor(periodEnd, view))
		if err != nil {
			return nil, err
		}
		if !filled {
			continue
		}
		cand.campaignID = c.id
		cand.unitIndexes = []int{u.index}
		cand.unitIDs = []string{u.openingFillID}
		stops = append(stops, cand)
	}
	// Worst price first. The sort is stable and compares price alone, so two
	// stops at one price would keep ascending Unit order — a tie-break that
	// never runs, since no two Units share a stop (see above).
	sortStable(stops, func(a, b candidate) bool { return a.price < b.price })
	if len(stops) > 0 || len(atExit) == 0 {
		return stops, nil
	}

	exit, filled, err := s.exitAtChannel(c, proposal, atExit, periodEnd, view)
	if err != nil || !filled {
		return nil, err
	}
	return []candidate{exit}, nil
}

// exitAtChannel prices the Exit Orders resting at the Exit Channel as the
// one exit fill of proposal. See coveredExitOrders.
func (s *Simulator) exitAtChannel(c *campaign, proposal *exitProposal, atExit []*unit, periodEnd time.Time, view event.PriceView) (candidate, bool, error) {
	if proposal == nil || proposal.campaignID != c.id {
		return candidate{}, false, fmt.Errorf("fills: instrument %q: unit %d of campaign %q rests at the exit channel, but no exit proposal is outstanding for that campaign", c.instrumentID, atExit[0].index, c.id)
	}
	r := atExit[0].ref.rangeFor(periodEnd, view)
	var quantity int64
	var orderQuantities []int64
	for _, u := range atExit {
		// Exact comparison: each level is a copy of the one proposed level,
		// never a derived price.
		if u.exitLevel != proposal.level || u.ref.rangeFor(periodEnd, view) != r {
			return candidate{}, false, fmt.Errorf("fills: instrument %q: unit %d of campaign %q rests at the exit channel at %v, which is not the one order at the level of exit proposal %q (%v) the other units rest at",
				c.instrumentID, u.index, c.id, u.exitLevel, proposal.proposalID, proposal.level)
		}
		quantity += u.quantity
		orderQuantities = append(orderQuantities, u.quantity)
	}
	cand, filled, err := s.price(event.FillKindExit, SideSell, proposal.level, c.n, quantity, r)
	if err != nil || !filled {
		return candidate{}, false, err
	}
	if len(atExit) != len(c.units) {
		return candidate{}, false, fmt.Errorf("fills: instrument %q: the exit at %v for campaign %q was reached, but the campaign holds %d unit(s) and only %d rest at the exit; an exit fill closes everything the campaign still holds, so it cannot describe this bar",
			c.instrumentID, proposal.level, c.id, len(c.units), len(atExit))
	}
	cand.proposalID = proposal.proposalID
	cand.campaignID = c.id
	cand.orderQuantities = orderQuantities
	return cand, true, nil
}

// sortStable is an insertion sort, used rather than sort.SliceStable because
// these slices hold at most a handful of entries and an explicit, stable,
// comparison-only sort makes the determinism obvious.
func sortStable(items []candidate, less func(a, b candidate) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// priceBuy applies ADR 0005 rules 1 and 2 to one entry or Add: as a
// stop-limit capped at its price cap when it has one, and as a stop-market
// order otherwise (ADR 0005, as amended 2026-09-24).
func (s *Simulator) priceBuy(o *order, r Range) (candidate, bool, error) {
	if o.orderType == event.OrderTypeStopMarket {
		return s.price(o.kind, o.side, o.level, o.n, o.quantity, r)
	}
	if !isFinite(o.n) || o.n <= 0 {
		return candidate{}, false, fmt.Errorf("fills: a %s order at level %v carries n %v; slippage is measured in n (ADR 0013) and cannot be derived from a non-positive one", o.kind, o.level, o.n)
	}
	execution, err := ExecuteStopLimit(o.level, o.priceCap, r, float64(s.slippageN*o.n))
	if err != nil || !execution.Filled {
		return candidate{}, false, err
	}
	return candidate{
		kind: o.kind, side: o.side, level: o.level, quantity: o.quantity, n: o.n,
		price: execution.Price, atReference: execution.AtReference,
	}, true, nil
}

// price applies ADR 0005 rules 1 and 2 to one order.
func (s *Simulator) price(kind, side string, level, n float64, quantity int64, r Range) (candidate, bool, error) {
	if !isFinite(n) || n <= 0 {
		return candidate{}, false, fmt.Errorf("fills: a %s order at level %v carries n %v; slippage is measured in n (ADR 0013) and cannot be derived from a non-positive one", kind, level, n)
	}
	execution, err := Execute(side, level, r, float64(s.slippageN*n))
	if err != nil {
		return candidate{}, false, err
	}
	if !execution.Filled {
		return candidate{}, false, nil
	}
	return candidate{
		kind: kind, side: side, level: level, quantity: quantity, n: n,
		price: execution.Price, atReference: execution.AtReference,
	}, true, nil
}
