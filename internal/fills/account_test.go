package fills_test

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
	"github.com/richard-whittemore/TrendInvesting/internal/replay"
)

// The simulated account: in a backtest internal/fills is the broker (ADR
// 0020), so it keeps the account's cash and holdings and states them, as of
// each Session's close, in an account.snapshot. The snapshot is the next
// Session's previous-close basis (ADR 0010), and it reaches the reducer in
// the order a LEAN run delivers it: after the next Session's open-instant
// fills, before its bars (ADR 0021, as amended).

// accountCash is the cash these fixtures open the account with. It funds the
// whole four-Unit ladder of campaignLifeBars (about 4 x 3,333 x 157), since
// the subject here is the ledger, not the affordability check.
const accountCash = 3_000_000.0

// runAccount drives bars through RunBar with the simulator keeping the
// account, then states the last Session's close.
func runAccount(t *testing.T, cash float64, bars []event.CompletedBarPayload) composed {
	t.Helper()
	cfg := baselineConfig()
	simulator, reducer := newComposed(t, cfg)
	ctx := context.Background()
	recorder := journal.NewRecorder(reducer)
	if _, err := fills.Deliver(ctx, simulator, recorder, configurationEnvelope(t, cfg)); err != nil {
		t.Fatalf("Deliver(configuration) error = %v", err)
	}
	if err := simulator.OpenAccount(cash); err != nil {
		t.Fatalf("OpenAccount() error = %v", err)
	}
	for i, b := range bars {
		if _, err := fills.RunBar(ctx, simulator, recorder, barEnvelope(t, b)); err != nil {
			t.Fatalf("RunBar(bar %d) error = %v", i+1, err)
		}
	}
	if _, err := fills.StateLastClose(ctx, simulator, recorder); err != nil {
		t.Fatalf("StateLastClose() error = %v", err)
	}
	return recorded(t, recorder)
}

func decodeSnapshot(t *testing.T, e event.Envelope) event.AccountSnapshotPayload {
	t.Helper()
	var payload event.AccountSnapshotPayload
	decodeInto(t, e, &payload)
	return payload
}

// ledgerAt is the account's cash as of asOf, recomputed from the recorded
// fills alone: the opening cash, less every buy's cost and commission, plus
// every sell's proceeds less commission (ADR 0013's costs; ADR 0020's
// ledger).
func ledgerAt(t *testing.T, opening float64, inputs []event.Envelope, asOf time.Time) float64 {
	t.Helper()
	cash := opening
	for _, fill := range fillPayloads(t, inputs) {
		if fill.FilledAt.After(asOf) {
			continue
		}
		value := float64(fill.Quantity) * fill.Price
		switch fill.Kind {
		case event.FillKindEntry, event.FillKindAdd:
			cash -= value + fill.Commission
		default:
			cash += value - fill.Commission
		}
	}
	return cash
}

// heldAt is the shares the recorded fills leave held as of asOf.
func heldAt(t *testing.T, inputs []event.Envelope, asOf time.Time) int64 {
	t.Helper()
	var held int64
	for _, fill := range fillPayloads(t, inputs) {
		if fill.FilledAt.After(asOf) {
			continue
		}
		switch fill.Kind {
		case event.FillKindEntry, event.FillKindAdd:
			held += fill.Quantity
		default:
			held -= fill.Quantity
		}
	}
	return held
}

// TestTheAccountStatesEverySessionsCloseAsTheNextSessionsBasis: one
// snapshot per Session, as of that Session's period end, reaching the
// reducer after every fill of the Session it states and before the next
// Session's first bar. The last Session's is stated by StateLastClose.
// Falsified by stating each close at the end of its own Session (before the
// next Session's open-instant fills) or at the next Session's period end.
func TestTheAccountStatesEverySessionsCloseAsTheNextSessionsBasis(t *testing.T) {
	t.Parallel()

	// The Campaign of campaignLifeBars holds four Units after day 59; day 60
	// opens below every Unit's Protective Stop, so each stop fills at the
	// open (ADR 0005), before day 60's bar reaches the reducer.
	bars := campaignLifeBars()[:59]
	bars = append(bars, bar(day(60), 150.0, 150.5, 149.0, 150.0), bar(day(61), 150.0, 150.5, 149.0, 150.0))
	run := runAccount(t, accountCash, bars)
	snapshots := envelopesOfType(run.Inputs, event.AccountSnapshotEventType)
	if len(snapshots) != len(bars) {
		t.Fatalf("%d snapshots for %d Sessions, want one per Session", len(snapshots), len(bars))
	}
	index := make(map[string]int, len(run.Inputs))
	barAt := make(map[time.Time]int, len(bars))
	for i, e := range run.Inputs {
		index[e.ID] = i
		if e.Type == event.CompletedBarEventType {
			barAt[e.EventTime] = i
		}
	}
	for i, snapshot := range snapshots {
		payload := decodeSnapshot(t, snapshot)
		if !payload.AsOf.Equal(bars[i].PeriodEnd) || !snapshot.EventTime.Equal(payload.AsOf) || snapshot.Source != fills.Source || payload.Currency != "USD" {
			t.Fatalf("snapshot %d = %+v (event time %s, source %q), want as of the Session's close %s, stated by the simulator in USD",
				i+1, payload, snapshot.EventTime, snapshot.Source, bars[i].PeriodEnd)
		}
		at := index[snapshot.ID]
		for j, e := range run.Inputs {
			switch e.Type {
			case event.FillEventType:
				if filled := decodeFill(t, e).FilledAt; !filled.After(payload.AsOf) && j > at {
					t.Fatalf("a fill of %s reached the reducer after the snapshot stating that close", filled)
				}
			case event.CompletedBarEventType, event.SessionClosedEventType:
				if e.EventTime.After(payload.AsOf) && j < at {
					t.Fatalf("the %s of %s reached the reducer before the snapshot of the previous close %s", e.Type, e.EventTime, payload.AsOf)
				}
			}
		}
	}
	// An open-instant fill is one delivered before its own bar. Each must
	// reach the reducer before the snapshot of the previous close, which
	// must in turn precede that bar.
	var gapped int
	for j, e := range run.Inputs {
		if e.Type != event.FillEventType {
			continue
		}
		filled := decodeFill(t, e).FilledAt
		if j > barAt[filled] {
			continue
		}
		gapped++
		previous := snapshots[barIndex(t, bars, filled)-1]
		if at := index[previous.ID]; j >= at || at >= barAt[filled] {
			t.Fatalf("the open-instant fill of %s is at %d, the previous close's snapshot at %d and the bar at %d; want fill, snapshot, bar", filled, j, at, barAt[filled])
		}
	}
	if gapped == 0 {
		t.Fatalf("no fill was decided at a bar's open; the fixture no longer exercises the order%s", describe(run.Inputs))
	}
}

// barIndex is the index of the bar ending at periodEnd.
func barIndex(t *testing.T, bars []event.CompletedBarPayload, periodEnd time.Time) int {
	t.Helper()
	for i, b := range bars {
		if b.PeriodEnd.Equal(periodEnd) {
			return i
		}
	}
	t.Fatalf("no bar ends at %s", periodEnd)
	return -1
}

// TestTheSnapshotCashIsTheSimulatorsLedgerToTheCent: across the entry, three
// Adds and the exit, every snapshot's cash equals the opening cash less
// every buy's cost and commission, plus every sell's proceeds less
// commission, up to its own close. Falsified by leaving commission out of
// either side, or by crediting a sell at its level rather than its price.
func TestTheSnapshotCashIsTheSimulatorsLedgerToTheCent(t *testing.T) {
	t.Parallel()

	run := runAccount(t, accountCash, campaignLifeBars())
	var sold bool
	for _, snapshot := range envelopesOfType(run.Inputs, event.AccountSnapshotEventType) {
		payload := decodeSnapshot(t, snapshot)
		want := ledgerAt(t, accountCash, run.Inputs, payload.AsOf)
		if math.Abs(payload.AvailableCash-want) >= 0.005 {
			t.Fatalf("snapshot as of %s states cash %.6f, want the ledger's %.6f", payload.AsOf, payload.AvailableCash, want)
		}
		if heldAt(t, run.Inputs, payload.AsOf) == 0 && payload.AvailableCash > accountCash {
			sold = true
		}
	}
	if !sold {
		t.Fatal("the fixture never closed its Campaign at a profit, so the credit side was not exercised")
	}
}

// TestTheSnapshotEquityMarksHoldingsAtTheSplitAdjustedClose: with a
// Campaign open, equity is cash plus the shares held at the Session's
// split-adjusted close, the one view a Campaign's money is computed in (ADR
// 0004, as amended). The raw view is doubled here so that reading it would
// show. Falsified by marking holdings at the raw close, at the last fill
// price, or not at all.
func TestTheSnapshotEquityMarksHoldingsAtTheSplitAdjustedClose(t *testing.T) {
	t.Parallel()

	bars := campaignLifeBars()
	for i := range bars {
		raw := &bars[i].Raw
		raw.Open, raw.High, raw.Low, raw.Close = 2*raw.Open, 2*raw.High, 2*raw.Low, 2*raw.Close
	}
	run := runAccount(t, accountCash, bars)
	closes := make(map[time.Time]float64, len(bars))
	for _, b := range bars {
		closes[b.PeriodEnd] = b.SplitAdjusted.Close
	}
	var open int
	for _, snapshot := range envelopesOfType(run.Inputs, event.AccountSnapshotEventType) {
		payload := decodeSnapshot(t, snapshot)
		held := heldAt(t, run.Inputs, payload.AsOf)
		want := payload.AvailableCash + float64(float64(held)*closes[payload.AsOf])
		if math.Abs(payload.Equity-want) >= 0.005 {
			t.Fatalf("snapshot as of %s: equity %.6f with %d shares held, want cash plus holdings at the split-adjusted close, %.6f", payload.AsOf, payload.Equity, held, want)
		}
		if held > 0 {
			open++
		}
	}
	if open == 0 {
		t.Fatal("no snapshot was stated with a Campaign open")
	}
}

// TestASnapshotFollowingItsFillsIsNotDebitedTwice: the entry fill is
// debited on the breakout Session, and the next Session's snapshot, which
// states that close, already reflects it, so the reducer drops the debit
// (ADR 0020: "A fill debits the ledger exactly once"). The Add proposed at
// the next Session's close is therefore checked against the snapshot's cash
// alone: here, cash for exactly one more Unit and a dollar, which the Add
// must pass. Falsified by stating the snapshot as of an earlier instant, so
// the debit stands on top of the balance that already paid it.
func TestASnapshotFollowingItsFillsIsNotDebitedTwice(t *testing.T) {
	t.Parallel()

	bars := append(warmUpBars(), breakoutBar(), bar(day(57), 155.7, 156.7, 155.5, 156.5))
	entry := float64(fixtureUnitQuantity) * (155.5 + fixtureSlippage)
	commission := math.Max(1, 0.005*float64(fixtureUnitQuantity))
	// Unit 2 is checked at its rung and fills a slippage above it, so the
	// account holds the fill's cost: enough to pass the check once, and
	// never enough to pass it with the entry debited twice.
	rung := 155.5 + fixtureSlippage + 0.5*fixtureN
	cash := entry + commission + float64(float64(fixtureUnitQuantity)*(rung+fixtureSlippage)) + commission + 1
	run := runAccount(t, cash, bars)
	if n := len(envelopesOfType(run.Decisions, event.CampaignUnitAddedEventType)); n != 1 {
		t.Fatalf("%d Units added, want Unit 2 funded by the snapshot's cash%s", n, describe(run.Decisions))
	}
	for _, e := range envelopesOfType(run.Decisions, event.ProposalDeclinedEventType) {
		var decline event.ProposalDeclinedPayload
		decodeInto(t, e, &decline)
		t.Fatalf("declined %+v: the entry fill was debited on top of a snapshot that already reflects it", decline)
	}
}

// TestTheAccountCreditsADelistingAtTheLastAvailablePrice: a Delisting Exit
// closes the Campaign at the split-adjusted close of its last bar with no
// fill (ADR 0009; ADR 0004, as amended), so the account receives the
// holding at that price, and holds nothing more. Falsified by leaving the
// shares held after the Campaign is gone.
func TestTheAccountCreditsADelistingAtTheLastAvailablePrice(t *testing.T) {
	t.Parallel()

	cfg := baselineConfig()
	simulator, reducer := newComposed(t, cfg)
	ctx := context.Background()
	recorder := journal.NewRecorder(reducer)
	if _, err := fills.Deliver(ctx, simulator, recorder, configurationEnvelope(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := simulator.OpenAccount(accountCash); err != nil {
		t.Fatal(err)
	}
	bars := append(warmUpBars(), breakoutBar())
	for _, b := range bars {
		if _, err := fills.RunBar(ctx, simulator, recorder, barEnvelope(t, b)); err != nil {
			t.Fatal(err)
		}
	}
	before := simulator.Account()
	delisting := envelope(t, "delisting-1", event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, day(56), event.CorporateActionPayload{
		InstrumentID: testInstrument, Kind: event.CorporateActionKindDelisting, EffectiveAt: day(56),
	})
	if _, err := fills.Deliver(ctx, simulator, recorder, delisting); err != nil {
		t.Fatal(err)
	}
	after := simulator.Account()
	held := before.Holdings[testInstrument]
	if held == 0 {
		t.Fatal("the fixture held nothing to delist")
	}
	want := before.Cash + float64(float64(held)*breakoutBar().SplitAdjusted.Close)
	if after.Cash != want || after.Holdings[testInstrument] != 0 {
		t.Fatalf("after the delisting: %+v, want cash %v and nothing held", after, want)
	}
}

// TestACashMovementMovesTheLedgerAfterTheCloseItFollows: a withdrawal the
// reducer accepts leaves the account, and the Session close pending when it
// arrives is stated first, since account events share one timeline (ADR
// 0007). Falsified by leaving the movement out of the ledger, or by stating
// the pending close after it.
func TestACashMovementMovesTheLedgerAfterTheCloseItFollows(t *testing.T) {
	t.Parallel()

	cfg := baselineConfig()
	simulator, reducer := newComposed(t, cfg)
	ctx := context.Background()
	recorder := journal.NewRecorder(reducer)
	if _, err := fills.Deliver(ctx, simulator, recorder, configurationEnvelope(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := simulator.OpenAccount(accountCash); err != nil {
		t.Fatal(err)
	}
	if _, err := fills.RunBar(ctx, simulator, recorder, barEnvelope(t, warmUpBars()[0])); err != nil {
		t.Fatal(err)
	}
	movedAt := day(1).Add(time.Hour)
	movement := envelope(t, "withdrawal-1", event.CashMovementEventType, event.CashMovementSchemaVersion, movedAt, event.CashMovementPayload{
		AsOf: movedAt, Amount: -250_000, EquityBefore: accountCash, Currency: "USD",
	})
	result, err := fills.Deliver(ctx, simulator, recorder, movement)
	if err != nil {
		t.Fatalf("Deliver(cash movement) error = %v", err)
	}
	if len(result.Inputs) != 2 || result.Inputs[0].Type != event.AccountSnapshotEventType || result.Inputs[1].Type != event.CashMovementEventType {
		t.Fatalf("inputs %s, want the pending close's snapshot, then the movement", describe(result.Inputs))
	}
	if got := simulator.Account().Cash; got != accountCash-250_000 {
		t.Fatalf("cash after the withdrawal = %v, want %v", got, accountCash-250_000)
	}
	// Nothing is pending once it is stated, so the last close has nothing
	// left to state.
	if result, err := fills.StateLastClose(ctx, simulator, recorder); err != nil || len(result.Inputs) != 0 {
		t.Fatalf("StateLastClose() = %s, %v; want nothing left to state", describe(result.Inputs), err)
	}
}

// TestTheAccountRefusesWhatWouldMakeItsStatementUntrue fails closed on every
// way the statement could stop describing the account: an opening balance
// that is not a figure, a second opening, an opening after the run began,
// another producer's snapshot, and a fill the account could not pay for
// (ADR 0020: a fill the ledger cannot fund halts the run; ADR 0019).
func TestTheAccountRefusesWhatWouldMakeItsStatementUntrue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := baselineConfig()
	for _, cash := range []float64{-1, math.NaN(), math.Inf(1)} {
		simulator, _ := newComposed(t, cfg)
		if err := simulator.OpenAccount(cash); err == nil {
			t.Errorf("OpenAccount(%v) succeeded", cash)
		}
	}

	simulator, reducer := newComposed(t, cfg)
	if err := simulator.OpenAccount(0); err != nil {
		t.Fatalf("OpenAccount(0) error = %v; no cash is a legitimate opening", err)
	}
	if err := simulator.OpenAccount(1); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second OpenAccount() error = %v, want it refused", err)
	}
	if _, err := fills.Deliver(ctx, simulator, reducer, accountSnapshotEnvelope(t, cfg)); err == nil || !strings.Contains(err.Error(), "states the account") {
		t.Errorf("Deliver(another producer's snapshot) error = %v, want it refused", err)
	}

	late, lateReducer := newComposed(t, cfg)
	if _, err := fills.Deliver(ctx, late, lateReducer, configurationEnvelope(t, cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := fills.RunBar(ctx, late, lateReducer, barEnvelope(t, warmUpBars()[0])); err != nil {
		t.Fatal(err)
	}
	if err := late.OpenAccount(accountCash); err == nil {
		t.Error("OpenAccount() after a Session succeeded")
	}

	// A handler that proposes an entry the account cannot pay for: the fill
	// is recorded, and the close that would state a negative balance stops
	// the run instead.
	broke, _ := newComposed(t, cfg)
	if err := broke.OpenAccount(1_000); err != nil {
		t.Fatal(err)
	}
	proposal := restingEntryProposal(t, 155.5)
	opener := openCampaignOnEntryFill(t, proposal.ID)
	handler := replay.HandlerFunc(func(ctx context.Context, in event.Envelope) ([]event.Envelope, error) {
		if in.Type == event.SessionClosedEventType {
			return []event.Envelope{proposal}, nil
		}
		return opener.Apply(ctx, in)
	})
	_, err := fills.RunBar(ctx, broke, handler, barEnvelope(t, breakoutBar()))
	if err == nil || !strings.Contains(err.Error(), "ADR 0020") {
		t.Fatalf("RunBar() error = %v, want the unfunded fill to stop the run", err)
	}
}

// TestTheAccountStatementIsDeterministic: the ledger's holdings are valued
// in instrument order, never Go's map order, so two runs of one fixture
// state the same bytes.
func TestTheAccountStatementIsDeterministic(t *testing.T) {
	t.Parallel()

	first := runAccount(t, accountCash, campaignLifeBars())
	second := runAccount(t, accountCash, campaignLifeBars())
	a, b := envelopesOfType(first.Inputs, event.AccountSnapshotEventType), envelopesOfType(second.Inputs, event.AccountSnapshotEventType)
	encode := func(envelopes []event.Envelope) string {
		raw, err := json.Marshal(envelopes)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if encode(a) != encode(b) {
		t.Fatal("two runs of one fixture stated different accounts")
	}
	ids := make([]string, 0, len(a))
	for _, e := range a {
		ids = append(ids, e.ID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("snapshot ids %v are not in Session order", ids)
	}
}
