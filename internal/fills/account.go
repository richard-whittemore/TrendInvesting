package fills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// accountCurrency is the one currency the simulated account is kept in.
// Multi-currency accounts are out of scope, and the reducer pins the first
// account event's currency and refuses any other.
const accountCurrency = "USD"

// account is the simulated brokerage account: in a backtest this package is
// the broker (ADR 0020), so it holds the cash and shares its own fills moved
// and states them, as of each Session's close, in an account.snapshot.
//
// Every figure is in the one view a Campaign's money is computed in, the
// split-adjusted view its fills are priced in (ADR 0004, as amended).
// quantity x price is invariant under a split adjustment, so cash is the
// same number in either view; holdings are valued at the split-adjusted
// close for the same reason.
type account struct {
	// cash is the opening cash, less every buy's cost and commission, plus
	// every sell's proceeds less commission, plus every accepted cash
	// movement, in the order they were applied. It is not floored: a fill
	// the account could not pay for leaves it negative, and the statement
	// that would report it stops the run instead (see closeOf).
	cash float64
	// otherEquity is the part of the configured starting equity the opening
	// cash does not account for: max(0, starting equity - opening cash). It
	// is not cash and is never traded, so it enters equity and nothing else.
	// A cash account has no liabilities, so an account opened with more cash
	// than the starting equity is worth its cash, and this is zero.
	otherEquity float64
	// holdings is the shares held per instrument, from the fills alone.
	holdings map[string]int64
	// closes is each instrument's latest split-adjusted close.
	closes map[string]float64
	// dollarsPerPoint is the contract multiplier every price is scaled by.
	dollarsPerPoint float64
	// pending is the statement of the last Session run, not yet delivered.
	pending *event.Envelope
}

// Account is a read-only view of the simulated account, for inspection by a
// test or a driver. It is a copy, never the simulator's own state.
type Account struct {
	Cash     float64
	Holdings map[string]int64
}

// OpenAccount makes the simulator keep the account a backtest trades,
// opening with cash, and state it after every Session (see RunSession).
// Without it the simulator keeps no account and states none, and a caller
// supplies its own account.snapshot through Deliver.
//
// The account opens with no holdings, before any Session has run: the
// statement of each close is derived from the opening cash and everything
// that happened since, so an opening after the first Session would describe
// an account nobody kept. cash is finite and not negative; zero is a
// legitimate opening. Equity opens at the configured starting equity, or at
// cash if that is larger (see account.otherEquity).
func (s *Simulator) OpenAccount(cash float64) error {
	switch {
	case s.account != nil:
		return errors.New("fills: the simulated account is already open")
	case s.sessionsRun > 0:
		return errors.New("fills: the simulated account must open before the first Session; its statements are derived from the opening cash and every fill since")
	case math.IsNaN(cash) || math.IsInf(cash, 0) || cash < 0:
		return fmt.Errorf("fills: the simulated account must open with finite, nonnegative cash, got %v", cash)
	}
	s.account = &account{
		cash:            cash,
		otherEquity:     max(0, s.startingEquity-cash),
		holdings:        make(map[string]int64),
		closes:          make(map[string]float64),
		dollarsPerPoint: s.dollarsPerPoint,
	}
	return nil
}

// Account reports the simulated account; the zero Account when the
// simulator keeps none.
func (s *Simulator) Account() Account {
	if s.account == nil {
		return Account{}
	}
	holdings := make(map[string]int64, len(s.account.holdings))
	for id, quantity := range s.account.holdings {
		holdings[id] = quantity
	}
	return Account{Cash: s.account.cash, Holdings: holdings}
}

// StateLastClose delivers the statement of the last Session run, if one is
// still pending. RunSession delivers each Session's statement inside the
// next Session; the last one has no next Session, so the driver states it
// here, after its final Session and before anything stamped later.
func StateLastClose(ctx context.Context, sim *Simulator, handler replay.Handler) (Result, error) {
	var result Result
	if sim == nil || handler == nil {
		return result, errors.New("fills: a simulator and a handler are both required")
	}
	err := sim.statePendingClose(ctx, handler, &result)
	return result, err
}

// statePendingClose delivers the pending statement, if there is one.
func (s *Simulator) statePendingClose(ctx context.Context, handler replay.Handler, result *Result) error {
	if s.account == nil || s.account.pending == nil {
		return nil
	}
	pending := *s.account.pending
	s.account.pending = nil
	return s.deliver(ctx, handler, pending, reference{}, result)
}

// closeOf records the statement of the Session that closed at periodEnd,
// to be delivered after the next Session's open-instant fills and before
// its bars (see RunSession), or by StateLastClose.
//
// It is taken here, once the Session's own fills are done, rather than when
// it is delivered: by then the next Session's open-instant fills have moved
// the account, and those happened after this close. The reducer keeps
// debiting them after the statement replaces its basis, because they are
// stamped after its as-of (ADR 0020).
//
// A statement that cannot be true stops the run. Negative cash means a fill
// spent money the account did not hold, which ADR 0020 says halts rather
// than trades on (ADR 0019: an unexplained difference halts); equity at or
// below zero is not an account the Notional Account can be measured from
// (ADR 0007).
func (s *Simulator) closeOf(periodEnd, recordedAt time.Time) error {
	if s.account == nil {
		return nil
	}
	equity, err := s.account.equity()
	if err != nil {
		return err
	}
	payload := event.AccountSnapshotPayload{
		AsOf:          periodEnd,
		Equity:        equity,
		AvailableCash: s.account.cash,
		Currency:      accountCurrency,
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("fills: the simulated account cannot be stated as of %s (cash %v, equity %v): a fill spent cash the account did not hold, which halts the run rather than trading on (ADR 0020, ADR 0019): %w",
			periodEnd.Format(time.RFC3339), s.account.cash, equity, err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// validated-payload-json (docs/development.md).
		return fmt.Errorf("fills: marshal account snapshot payload: %w", err)
	}
	envelope := event.Envelope{
		ID:                "account-snapshot:" + periodEnd.UTC().Format(idTimeLayout),
		Type:              event.AccountSnapshotEventType,
		SchemaVersion:     event.AccountSnapshotSchemaVersion,
		EnvelopeVersion:   event.CurrentEnvelopeVersion,
		EventTime:         periodEnd,
		RecordedAt:        recordedAt,
		Source:            Source,
		StrategyVersion:   s.strategyVersion,
		ConfigurationHash: s.configurationHash,
		PayloadHash:       event.HashPayload(encoded),
		Payload:           encoded,
	}
	s.account.pending = &envelope
	return nil
}

// equity is mark-to-market equity at the latest closes: cash, plus every
// holding at its instrument's split-adjusted close, plus the opening equity
// that is not cash. Holdings are summed in instrument order, never Go's map
// order, so the figure is the same on every run.
func (a *account) equity() (float64, error) {
	ids := make([]string, 0, len(a.holdings))
	for id, quantity := range a.holdings {
		if quantity != 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	equity := a.cash + a.otherEquity
	for _, id := range ids {
		closeAt, ok := a.closes[id]
		if !ok {
			return 0, fmt.Errorf("fills: the simulated account holds %d shares of %q, which has no close to value them at", a.holdings[id], id)
		}
		equity += a.value(a.holdings[id], closeAt)
	}
	return equity, nil
}

// value is quantity x price x dollars per point as one rounded product, so
// it is never fused into the addition it feeds (docs/development.md). It is
// the same product the reducer debits a buy fill by (ADR 0020).
func (a *account) value(quantity int64, price float64) float64 {
	return float64(float64(quantity) * price * a.dollarsPerPoint)
}

// record moves the account for one input the handler accepted: a fill, or
// a cash movement.
func (s *Simulator) record(envelope event.Envelope) error {
	if s.account == nil {
		return nil
	}
	switch envelope.Type {
	case event.FillEventType:
		var fill event.FillPayload
		if err := decodePayload(envelope, &fill); err != nil {
			return err
		}
		return s.account.recordFill(fill)
	case event.CashMovementEventType:
		var movement event.CashMovementPayload
		if err := decodePayload(envelope, &movement); err != nil {
			return err
		}
		s.account.cash += movement.Amount
	}
	return nil
}

// recordFill moves cash and holdings by one fill: a buy costs quantity x
// price x dollars per point plus commission, the figure the reducer debits
// (ADR 0020), and a sell returns the same product less commission. Price
// already carries the slippage (ADR 0013). A sell of more than is held fails
// closed: the account would hold a short position the long-only Baseline
// never takes (ADR 0002).
func (a *account) recordFill(fill event.FillPayload) error {
	value := a.value(fill.Quantity, fill.Price)
	switch fill.Kind {
	case event.FillKindEntry, event.FillKindAdd:
		a.cash -= value + fill.Commission
		a.holdings[fill.InstrumentID] += fill.Quantity
	case event.FillKindStop, event.FillKindExit:
		if fill.Quantity > a.holdings[fill.InstrumentID] {
			return fmt.Errorf("fills: fill %q sells %d shares of %q, but the simulated account holds %d", fill.FillID, fill.Quantity, fill.InstrumentID, a.holdings[fill.InstrumentID])
		}
		a.cash += value - fill.Commission
		a.holdings[fill.InstrumentID] -= fill.Quantity
	default:
		return fmt.Errorf("fills: fill %q has kind %q, which the simulated account cannot record", fill.FillID, fill.Kind)
	}
	return nil
}

// cashInLieu settles a split's cash in lieu (ADR 0023): the account holds
// the shares the broker could not deliver fewer, and is credited the cash it
// paid for them. The credit is the account's; the reducer's spendable cash
// sees it only in the next Session's statement (ADR 0020). An account that
// does not hold the Campaign's quantity before fails closed before anything
// moves, so it always ends at the decision's quantity after.
func (a *account) cashInLieu(instrumentID string, holdingBefore, sharesLost int64, cash float64) error {
	if a.holdings[instrumentID] != holdingBefore {
		return fmt.Errorf("fills: a split's cash in lieu expects the simulated account to hold %d shares of %q, but it holds %d; settling would leave it off the decision's quantity after (ADR 0023)", holdingBefore, instrumentID, a.holdings[instrumentID])
	}
	a.holdings[instrumentID] -= sharesLost
	a.cash += cash
	return nil
}

// delist settles a Delisting Exit: the Campaign is closed at the last
// available price, the split-adjusted close of its last completed bar, with
// no fill and no order (ADR 0009; ADR 0004, as amended), so the account
// receives its whole holding at that price and holds nothing more.
func (a *account) delist(instrumentID string) error {
	held := a.holdings[instrumentID]
	if held == 0 {
		return nil
	}
	closeAt, ok := a.closes[instrumentID]
	if !ok {
		return fmt.Errorf("fills: %q is delisted while the simulated account holds %d shares and has no close to settle them at", instrumentID, held)
	}
	a.cash += a.value(held, closeAt)
	delete(a.holdings, instrumentID)
	return nil
}
