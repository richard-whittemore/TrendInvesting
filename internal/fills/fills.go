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
// what a reviewer will check a LEAN or broker fill against (#30), and a
// package called "simulator" would misdescribe that second use the moment it
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
// it was decided in. Raw-view accounting is #38's, and in slice 1 the two
// views are identical in every fixture.
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
// configuration (#50) must confirm them against the live schedule before any
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
// payload, the schema version and every field on it are identical in shape,
// which is this ticket's own criterion.
const Source = "simulator"

// idTimeLayout is the fixed, nanosecond-precision layout every deterministic
// id in this system is built from, matching internal/strategy's own: two bars
// can never collapse to one id through formatting, and no wall clock or
// randomness is involved (.golangci.yml forbids both in internal/).
const idTimeLayout = "2006-01-02T15:04:05.000000000Z"

// maxFillsPerBar bounds the per-bar fixpoint in RunBar.
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
	// fills are interleaved; see RunBar.
	sequence uint64

	books map[string]*book

	// recordedAt is the RecordedAt of the most recent bar delivered, which is
	// what every fill decided from that bar inherits: the simulator observes
	// the same recording moment as the data that produced it, and has no
	// clock of its own (.golangci.yml forbids time.Now in internal/).
	recordedAt time.Time
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
type book struct {
	campaign *campaign
	entry    *order
	add      *order
	exit     *order
}

// order is one resting order other than a Protective Stop. A stop is held on
// its own Unit instead (see unit), because a stop belongs to a Unit and is
// raised and closed with it.
type order struct {
	kind       string // event.FillKind*
	side       string
	proposalID string
	campaignID string
	level      float64
	quantity   int64
	// n is the N this order's slippage is measured in: the decision N the
	// Signal was sized under for an entry (ADR 0003), the Campaign's frozen N
	// for an Add or an exit (ADR 0006).
	n   float64
	ref reference
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
// which Units it still holds, each with its own current Protective Stop.
//
// It is deliberately not position state. internal/strategy owns that, and
// every field here is a copy of something the reducer journalled; nothing in
// this package ever decides that a Unit exists, only that an order belonging
// to one is still resting.
type campaign struct {
	id           string
	instrumentID string
	direction    string
	n            float64
	// units is every Unit still open, in ascending index order. Held as a
	// slice rather than a map so iteration order is the index order, never
	// Go's map order (.greptile/rules.md's determinism rule).
	units []*unit
}

// unit is one held Unit and the Protective Stop resting for it.
type unit struct {
	index         int
	openingFillID string
	quantity      int64
	// stop is this Unit's own current level. Zero until the
	// strategy.protective-stop.set event for it arrives, which the reducer
	// always emits in the same Apply return as the Unit itself.
	stop float64
	ref  reference
}

func (c *campaign) unitAt(index int) *unit {
	for _, u := range c.units {
		if u.index == index {
			return u
		}
	}
	return nil
}

// quantity is every held Unit's quantity: what an exit order closes, since
// every Unit exits together (CONTEXT.md: "Campaign").
func (c *campaign) quantity() int64 {
	var total int64
	for _, u := range c.units {
		total += u.quantity
	}
	return total
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
		strategyVersion:   strategyVersion,
		configurationHash: configurationHash,
		books:             make(map[string]*book),
	}, nil
}

// Observe folds one envelope into the resting-order book.
//
// The whole book is learned from the reducer's own emissions: a trade, Add or
// exit proposal creates a resting order; a Campaign-opened, Unit-added or
// Protective-Stop-set event creates or moves a Unit's own stop; a
// proposal-expired, units-stopped or Campaign-exited event removes what is no
// longer in force. Nothing here is inferred from bars or invented.
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
// creates. RunBar passes the price of the fill that caused the emission; a
// caller with no such context (Observe itself) passes the zero reference,
// which means "the bar's open".
func (s *Simulator) observe(envelope event.Envelope, ref reference) error {
	switch envelope.Type {
	case event.TradeProposalEventType:
		return s.observeTradeProposal(envelope, ref)
	case event.AddProposalEventType:
		return s.observeAddProposal(envelope, ref)
	case event.ExitProposalEventType:
		return s.observeExitProposal(envelope, ref)
	case event.CampaignOpenedEventType:
		return s.observeCampaignOpened(envelope)
	case event.CampaignUnitAddedEventType:
		return s.observeUnitAdded(envelope)
	case event.ProtectiveStopSetEventType:
		return s.observeProtectiveStopSet(envelope, ref)
	case event.CampaignUnitsStoppedEventType:
		return s.observeUnitsStopped(envelope)
	case event.CampaignExitedEventType:
		return s.observeCampaignExited(envelope)
	case event.ProposalExpiredEventType:
		return s.observeProposalExpired(envelope)

	// Recognised and deliberately without effect on the resting-order book.
	// Listed one by one rather than caught by a default branch, so that a
	// NEW event type reaches the error below and is considered rather than
	// absorbed.
	case event.SetupEvaluatedEventType, // a Setup was evaluated; no order
		event.SignalEventType,                      // a Signal fired; the proposal that follows is the order
		event.ProposalDeclinedEventType,            // a Signal produced no position, so no order
		event.CampaignEvaluatedEventType,           // the levels in force, already known from the stop-set events
		event.EngineStateEventType,                 // a halt; the run stops, so the book is moot
		event.DrawdownStepAppliedEventType,         // ADR 0007's ladder; affects sizing, not resting orders
		event.NotionalAccountRebasedEventType,      // likewise
		event.NotionalAccountRecoveredEventType,    // likewise
		event.NotionalAccountCashAdjustedEventType, // likewise
		event.ConfigurationEventType,               // an input, carried at construction
		event.CompletedBarEventType,                // an input, handled by RunBar itself
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
	b := s.bookFor(payload.InstrumentID)
	b.entry = &order{
		kind:       event.FillKindEntry,
		side:       SideBuy,
		proposalID: envelope.ID,
		level:      payload.EntryLevel,
		quantity:   payload.Quantity,
		n:          payload.N,
		ref:        ref,
	}
	return nil
}

func (s *Simulator) observeAddProposal(envelope event.Envelope, ref reference) error {
	var payload event.AddProposalPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.add = &order{
		kind:       event.FillKindAdd,
		side:       SideBuy,
		proposalID: envelope.ID,
		campaignID: payload.CampaignID,
		level:      payload.Level,
		quantity:   payload.Quantity,
		n:          payload.CampaignN,
		ref:        ref,
	}
	return nil
}

// observeExitProposal takes the N its slippage is measured in from the
// Campaign rather than from the payload: an exit proposal carries no sizing
// of its own (event.ExitProposalPayload's doc comment), and the N in force is
// the Campaign's frozen one (ADR 0006). A proposal for a Campaign this
// package has never seen open fails closed rather than being priced from a
// guess.
func (s *Simulator) observeExitProposal(envelope event.Envelope, ref reference) error {
	var payload event.ExitProposalPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: exit proposal %q names campaign %q, which this simulator has no open campaign for; the slippage on an exit is measured in that campaign's frozen n (ADR 0006) and cannot be derived without it",
			payload.InstrumentID, envelope.ID, payload.CampaignID)
	}
	b.exit = &order{
		kind:       event.FillKindExit,
		side:       SideSell,
		proposalID: envelope.ID,
		campaignID: payload.CampaignID,
		level:      payload.Level,
		quantity:   payload.Quantity,
		n:          b.campaign.n,
		ref:        ref,
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

// observeProtectiveStopSet is where a Protective Stop becomes a resting
// order, and where a Stop Ladder raise moves one (#15). One event type serves
// both — the reducer discriminates with Reason — and so does this, because
// the effect on the book is identical: the Unit's own stop is now at Level.
func (s *Simulator) observeProtectiveStopSet(envelope event.Envelope, ref reference) error {
	var payload event.ProtectiveStopSetPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	if b.campaign == nil || b.campaign.id != payload.CampaignID {
		return fmt.Errorf("fills: instrument %q: protective-stop-set names campaign %q, which this simulator has no open campaign for", payload.InstrumentID, payload.CampaignID)
	}
	u := b.campaign.unitAt(payload.UnitIndex)
	if u == nil {
		return fmt.Errorf("fills: instrument %q: protective-stop-set names unit %d of campaign %q, which this simulator does not hold", payload.InstrumentID, payload.UnitIndex, payload.CampaignID)
	}
	u.stop = payload.Level
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
// The Add order is dropped here as well as on the reducer's own expiry event,
// and that is not redundant: a stop fill that closes a Campaign outright
// leaves any outstanding Add proposal to expire with the NEXT bar (only a
// PARTIAL stop cancels it immediately), so between the exit and that expiry
// the reducer still holds the proposal while the Campaign it belongs to is
// gone. Filling it would be a fill for a Campaign that no longer exists.
func (s *Simulator) observeCampaignExited(envelope event.Envelope) error {
	var payload event.CampaignExitedPayload
	if err := decodePayload(envelope, &payload); err != nil {
		return err
	}
	b := s.bookFor(payload.InstrumentID)
	b.campaign = nil
	b.add = nil
	b.exit = nil
	return nil
}

// observeProposalExpired drops whichever order the expiry names. It covers
// both of the reducer's expiry paths: ADR 0011's ordinary next-bar expiry,
// and #15's cancellation of a pending Add the instant a stop fill partially
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
	for _, slot := range []**order{&b.entry, &b.add, &b.exit} {
		if *slot != nil && (*slot).proposalID == payload.ProposalID {
			*slot = nil
		}
	}
	return nil
}

// Resting returns the orders in force for instrumentID, in a deterministic
// order: the entry, Add and exit proposals first, then one entry per held
// Unit's Protective Stop in ascending Unit order. It exists for inspection —
// by a test, or by #19's driver reporting what was left outstanding when a
// run ended — and never to be mutated.
func (s *Simulator) Resting(instrumentID string) []Order {
	b, ok := s.books[instrumentID]
	if !ok {
		return nil
	}
	var out []Order
	for _, o := range []*order{b.entry, b.add, b.exit} {
		if o == nil {
			continue
		}
		out = append(out, Order{
			Kind: o.kind, Side: o.side, InstrumentID: instrumentID,
			CampaignID: o.campaignID, ProposalID: o.proposalID,
			Level: o.level, Quantity: o.quantity, N: o.n,
		})
	}
	if b.campaign != nil {
		for _, u := range b.campaign.units {
			if u.stop <= 0 {
				continue
			}
			out = append(out, Order{
				Kind: event.FillKindStop, Side: SideSell, InstrumentID: instrumentID,
				CampaignID: b.campaign.id,
				Level:      u.stop, Quantity: u.quantity, N: b.campaign.n,
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
	Quantity     int64
	N            float64
	UnitIndexes  []int
	UnitIDs      []string
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
}

// covered prices every resting order for instrumentID against this bar and
// returns the ones it reached, in the order ADR 0005 rule 3 requires them to
// be filled: buys first, then sells worst-price-first. See RunBar's doc
// comment for the enumeration behind that ordering.
func (s *Simulator) covered(instrumentID string, periodEnd time.Time, view event.PriceView) ([]candidate, error) {
	b, ok := s.books[instrumentID]
	if !ok {
		return nil, nil
	}
	var buys, sells []candidate

	for _, o := range []*order{b.entry, b.add} {
		if o == nil {
			continue
		}
		c, filled, err := s.price(o.kind, o.side, o.level, o.n, o.quantity, o.ref.rangeFor(periodEnd, view))
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

	if b.campaign != nil {
		// One resting order per Unit, and so one fill per Unit — never one
		// fill covering several of them.
		//
		// That is what the source describes: a stop belongs to a Unit, the
		// Stop Ladder moves each one individually, and a Unit that filled
		// further away keeps its own level (The Turtle Rules p.22-23's Crude
		// example, where the fourth Unit's stop sits at 28.40 while Units 1-3
		// stay at 27.70). It is also the only shape that can be right here,
		// because under this system's rules two Units NEVER share a level:
		// Unit k+1's stop is its own fill less 2 N, Unit k's has risen by
		// half N, and the two coincide only if the later Unit filled exactly
		// half an N above the earlier one — which slippage, strictly positive
		// by ADR 0013, always prevents. Grouping Units into one fill would
		// therefore be code that never ran, and it would have had to compare
		// two derived prices for equality to decide.
		//
		// event.FillPayload.UnitIDs stays plural: the contract permits a
		// producer that genuinely closes several Units at one level (a
		// Variant that set every stop from the newest fill would), and this
		// producer simply always names exactly one.
		for _, u := range b.campaign.units {
			if u.stop <= 0 {
				// A Unit whose stop-set event has not arrived yet cannot be
				// filled against. The reducer emits the two in the same Apply
				// return, so this is unreachable in practice; skipping rather
				// than erroring keeps the ordering of two emissions from
				// becoming load-bearing here.
				continue
			}
			c, filled, err := s.price(event.FillKindStop, SideSell, u.stop, b.campaign.n, u.quantity, u.ref.rangeFor(periodEnd, view))
			if err != nil {
				return nil, err
			}
			if !filled {
				continue
			}
			c.campaignID = b.campaign.id
			c.unitIndexes = []int{u.index}
			c.unitIDs = []string{u.openingFillID}
			sells = append(sells, c)
		}

		if b.exit != nil {
			// The quantity is the Campaign's CURRENT holding, not the
			// proposal's own: an earlier partial stop in this same bar may
			// already have closed some Units, and the reducer checks an exit
			// fill against what the Campaign still holds.
			c, filled, err := s.price(b.exit.kind, b.exit.side, b.exit.level, b.exit.n, b.campaign.quantity(), b.exit.ref.rangeFor(periodEnd, view))
			if err != nil {
				return nil, err
			}
			if filled && c.quantity > 0 {
				c.proposalID = b.exit.proposalID
				c.campaignID = b.exit.campaignID
				sells = append(sells, c)
			}
		}
	}

	// Buys in ascending level: a lower buy-stop is reached on the way up
	// before a higher one, and the Add chain depends on that order anyway
	// (rung n+1 is measured from rung n's fill, so it cannot be evaluated
	// first). At most one buy is ever outstanding for an instrument in
	// practice; the sort makes the order stated rather than incidental.
	sortStable(buys, func(a, b candidate) bool { return a.level < b.level })
	// Sells worst-price-first: the lowest price a long seller could have got
	// is the pessimistic assumption, and it also decides which of two
	// competing closes happened when both cannot (a stop that empties the
	// Campaign cancels the exit, and vice versa). The sort is stable and the
	// comparison is price alone, so two sells at the same price keep the
	// order they were collected in — ascending Unit index, with the exit
	// last — rather than needing a tie-break that would never run: no two
	// orders here can share a price, since each Unit's stop stands at its own
	// level (see the loop above).
	sortStable(sells, func(a, b candidate) bool { return a.price < b.price })
	return append(buys, sells...), nil
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

// price applies ADR 0005 rules 1 and 2 to one order.
func (s *Simulator) price(kind, side string, level, n float64, quantity int64, r Range) (candidate, bool, error) {
	if !isFinite(n) || n <= 0 {
		return candidate{}, false, fmt.Errorf("fills: a %s order at level %v carries n %v; slippage is measured in n (ADR 0013) and cannot be derived from a non-positive one", kind, level, n)
	}
	execution, err := Execute(side, level, r, s.slippageN*n)
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
