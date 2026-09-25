package fills_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/fills"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// A split's cash in lieu (CONTEXT.md: "Cash in lieu"; ADR 0023) reaches the
// book and the simulated account only through the reducer's own emissions:
// strategy.campaign.cash-in-lieu shrinks the named Units and moves the
// account, and the strategy.exit-order.set decisions that follow re-rest
// their Exit Orders. The corporate action itself changes nothing here.

// splitIn is a synthetic 7-for-1 of testInstrument at when, one engine share
// a raw share after it (the fixture's two views agree), that lost lost raw
// shares and paid cash for them.
func splitIn(t *testing.T, id string, when time.Time, lost int64, cash float64) event.Envelope {
	t.Helper()
	return envelope(t, id, event.MarketCorporateActionEventType, event.MarketCorporateActionSchemaVersion, when,
		event.CorporateActionPayload{
			InstrumentID: testInstrument, Kind: event.CorporateActionKindSplit, EffectiveAt: when,
			NewShares: 7, OldShares: 1, EngineSharesPerRawShare: 1,
			RawSharesLost: lost, CashInLieu: cash, Currency: "USD",
		})
}

// fourUnits runs campaignLifeBars to its fourth Unit (day 59) with the
// account kept, and returns what is needed to carry on.
func fourUnits(t *testing.T) (*fills.Simulator, *journal.Recorder, []event.CompletedBarPayload) {
	t.Helper()
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
	bars := campaignLifeBars()
	for _, b := range bars[:59] {
		if _, err := fills.RunBar(ctx, simulator, recorder, barEnvelope(t, b)); err != nil {
			t.Fatal(err)
		}
	}
	if stops := stopsResting(simulator); len(stops) != 4 {
		t.Fatalf("the fixture must hold four Units by day 59, holds %d: %v", len(stops), stops)
	}
	return simulator, recorder, bars[59:]
}

// stopsResting is the book's Exit Orders by Unit index.
func stopsResting(sim *fills.Simulator) map[int]int64 {
	out := make(map[int]int64)
	for _, o := range sim.Resting(testInstrument) {
		if o.Side == fills.SideSell {
			for _, index := range o.UnitIndexes {
				out[index] = o.Quantity
			}
		}
	}
	return out
}

// TestASplitsCashInLieuReachesTheBookAndTheAccountFromTheEmissions is the
// headline fill-simulator case: a rounded 7-for-1 left two shares short of
// four Units. Units 4 and 3 lose one share each from the emissions alone;
// the account holds two fewer shares and the cash; and the Campaign's later
// Exit-Channel exit sells exactly what is left.
func TestASplitsCashInLieuReachesTheBookAndTheAccountFromTheEmissions(t *testing.T) {
	t.Parallel()

	simulator, recorder, rest := fourUnits(t)
	ctx := context.Background()
	before := stopsResting(simulator)
	account := simulator.Account()
	const cash = 2 * 0.99 * 158.1
	result, err := fills.Deliver(ctx, simulator, recorder, splitIn(t, "split-1", day(59).Add(12*time.Hour), 2, cash))
	if err != nil {
		t.Fatalf("Deliver(split) error = %v", err)
	}
	if got := len(envelopesOfType(result.Decisions, event.CampaignCashInLieuEventType)); got != 1 {
		t.Fatalf("got %d cash in lieu decision(s), want 1", got)
	}
	after := stopsResting(simulator)
	want := map[int]int64{1: before[1], 2: before[2], 3: before[3] - 1, 4: before[4] - 1}
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("Exit Orders after the split = %v, want %v", after, want)
	}
	gotAccount := simulator.Account()
	if gotAccount.Cash != account.Cash+cash || gotAccount.Holdings[testInstrument] != account.Holdings[testInstrument]-2 {
		t.Fatalf("account after the split = %+v, want cash %v and %d held", gotAccount, account.Cash+cash, account.Holdings[testInstrument]-2)
	}

	var exits []event.FillPayload
	for _, b := range rest {
		out, err := fills.RunBar(ctx, simulator, recorder, barEnvelope(t, b))
		if err != nil {
			t.Fatalf("RunBar(%s) error = %v", b.PeriodEnd.Format(time.RFC3339), err)
		}
		for _, f := range fillPayloads(t, out.Inputs) {
			if f.Kind == event.FillKindExit || f.Kind == event.FillKindStop {
				exits = append(exits, f)
			}
		}
	}
	var sold int64
	for _, f := range exits {
		sold += f.Quantity
	}
	if held := account.Holdings[testInstrument] - 2; sold != held {
		t.Fatalf("the Campaign's exits sold %d, want the %d held after the split", sold, held)
	}
	if got := simulator.Account().Holdings[testInstrument]; got != 0 {
		t.Fatalf("the account still holds %d after the Campaign closed", got)
	}
}

// TestASplitInputChangesNoRestingOrder: the corporate action itself, of
// either kind, is never interpreted by the book (ADR 0023; fills.observe).
func TestASplitInputChangesNoRestingOrder(t *testing.T) {
	t.Parallel()

	simulator, _, _ := fourUnits(t)
	before := simulator.Resting(testInstrument)
	if err := simulator.Observe(splitIn(t, "split-1", day(59).Add(12*time.Hour), 2, 300)); err != nil {
		t.Fatalf("Observe(split) error = %v", err)
	}
	if !reflect.DeepEqual(before, simulator.Resting(testInstrument)) {
		t.Fatal("a split input changed the resting-order book before the reducer's emissions")
	}
}

// TestACashInLieuTheBookCannotApplyFailsClosed: a decision naming a
// Campaign or Unit the book does not hold, or a quantity it does not hold,
// is refused rather than applied to the wrong shares.
func TestACashInLieuTheBookCannotApplyFailsClosed(t *testing.T) {
	t.Parallel()

	simulator, recorder, _ := fourUnits(t)
	var campaignID string
	for _, e := range recorder.Entries() {
		if e.Envelope.Type == event.CampaignOpenedEventType {
			campaignID = e.Envelope.ID
		}
	}
	held := stopsResting(simulator)
	decision := func(mutate func(*event.CampaignCashInLieuPayload)) event.Envelope {
		payload := event.CampaignCashInLieuPayload{
			CampaignID: campaignID, InstrumentID: testInstrument, CorporateActionID: "split-1",
			EffectiveAt: day(59).Add(12 * time.Hour), NewShares: 7, OldShares: 1, EngineSharesPerRawShare: 1,
			RawSharesLost: 1, EngineSharesLost: 1, CashInLieu: 150, Currency: "USD",
			Reductions:     []event.UnitReduction{{UnitIndex: 4, QuantityBefore: held[4], QuantityAfter: held[4] - 1}},
			QuantityBefore: held[1] + held[2] + held[3] + held[4], QuantityAfter: held[1] + held[2] + held[3] + held[4] - 1,
			Rule: event.RuleCashInLieuMostRecentUnitsFirst, ADR: event.ADRSplitCashInLieu,
		}
		mutate(&payload)
		return envelope(t, "cash-in-lieu", event.CampaignCashInLieuEventType, event.CampaignCashInLieuSchemaVersion, payload.EffectiveAt, payload)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*event.CampaignCashInLieuPayload)
		want   string
	}{
		{"another campaign", func(p *event.CampaignCashInLieuPayload) { p.CampaignID = "campaign:other" }, "no open campaign"},
		{"a unit not held", func(p *event.CampaignCashInLieuPayload) { p.Reductions[0].UnitIndex = 5 }, "does not hold"},
		{"a quantity not held", func(p *event.CampaignCashInLieuPayload) {
			p.Reductions[0].QuantityBefore++
			p.Reductions[0].QuantityAfter++
			p.QuantityBefore++
			p.QuantityAfter++
		}, "holds"},
		{"an invalid decision", func(p *event.CampaignCashInLieuPayload) { p.Rule = "" }, "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := simulator.Observe(decision(tt.mutate))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Observe() error = %v, want substring %q", err, tt.want)
			}
		})
	}
	undecodable := decision(func(*event.CampaignCashInLieuPayload) {})
	undecodable.Payload = []byte(`42`)
	if err := simulator.Observe(undecodable); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("Observe(undecodable) error = %v, want a decode failure", err)
	}
	if !reflect.DeepEqual(held, stopsResting(simulator)) {
		t.Fatal("a refused cash in lieu changed the book")
	}

	t.Run("more shares than the account holds", func(t *testing.T) {
		// A book learned from the decisions alone, with no fill ever
		// recorded, holds the Units while the account holds nothing: the
		// account refuses to lose shares it never had, and the book is left
		// as it was.
		mirror, _ := newComposed(t, baselineConfig())
		if err := mirror.OpenAccount(accountCash); err != nil {
			t.Fatal(err)
		}
		for _, e := range recorder.Entries() {
			if e.Kind == journal.KindDecision {
				if err := mirror.Observe(e.Envelope); err != nil {
					t.Fatalf("Observe(%s) error = %v", e.Envelope.Type, err)
				}
			}
		}
		err := mirror.Observe(decision(func(*event.CampaignCashInLieuPayload) {}))
		if err == nil || !strings.Contains(err.Error(), "simulated account holds 0") {
			t.Fatalf("Observe() error = %v, want the account's refusal", err)
		}
		if !reflect.DeepEqual(held, stopsResting(mirror)) {
			t.Fatal("a refused cash in lieu changed the mirrored book")
		}
	})
}

// TestACashInLieuWithoutAnAccountStillShrinksTheBook: a simulator that keeps
// no account (its caller states the account itself) still keeps its book in
// step with the Units.
func TestACashInLieuWithoutAnAccountStillShrinksTheBook(t *testing.T) {
	t.Parallel()

	cfg := baselineConfig()
	simulator, reducer := newComposed(t, cfg)
	ctx := context.Background()
	for _, input := range []event.Envelope{configurationEnvelope(t, cfg), accountSnapshotEnvelope(t, cfg)} {
		if _, err := fills.Deliver(ctx, simulator, reducer, input); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range campaignLifeBars()[:59] {
		if _, err := fills.RunBar(ctx, simulator, reducer, barEnvelope(t, b)); err != nil {
			t.Fatal(err)
		}
	}
	before := stopsResting(simulator)
	if _, err := fills.Deliver(ctx, simulator, reducer, splitIn(t, "split-1", day(59).Add(12*time.Hour), 1, 150)); err != nil {
		t.Fatal(err)
	}
	if after := stopsResting(simulator); after[4] != before[4]-1 || after[3] != before[3] {
		t.Fatalf("Exit Orders %v -> %v, want only unit 4 one share smaller", before, after)
	}
	if got := simulator.Account(); got.Cash != 0 || len(got.Holdings) != 0 {
		t.Fatalf("a simulator keeping no account reports %+v", got)
	}
}
