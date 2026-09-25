package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/journal"
)

// corporateActionsSplitFixture holds two splits of AAPL during
// bars_four_units.json's four-Unit Campaign, listed latest first: a
// synthetic 7-for-1 on 2026-01-25 whose rounded factor left one share short,
// and a 2-for-1 on 2026-01-28 that left two short (CONTEXT.md: "Cash in
// lieu"; ADR 0023). The fixture's two views agree, so one engine share is one
// raw share after each.
const corporateActionsSplitFixture = "testdata/corporate_actions_split_cash_in_lieu.json"

// TestASplitsCashInLieuCarriesACampaignAcrossTheJournal is the command-seam
// test: both splits apply, each to the most recent Units, in effective
// order whatever the file's order; the simulated account states the cash at
// the next close; the Exit-Channel exit sells exactly what is left; and the
// journal verifies, replays and re-runs.
func TestASplitsCashInLieuCarriesACampaignAcrossTheJournal(t *testing.T) {
	out := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := options{
		configPath:           configurationFixture,
		barsPath:             fourUnitBarsFixture,
		corporateActionsPath: corporateActionsSplitFixture,
		outPath:              out,
		build:                testBuild,
	}
	var log bytes.Buffer
	if err := backtest(context.Background(), opts, &log); err != nil {
		t.Fatalf("backtest(%+v) error = %v\n%s", opts, err, log.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	decisions := decisionsOfType(t, written, event.CampaignCashInLieuEventType)
	if len(decisions) != 2 {
		t.Fatalf("got %d cash in lieu decision(s), want 2: every split applies", len(decisions))
	}
	var first, second event.CampaignCashInLieuPayload
	if err := json.Unmarshal(decisions[0].Payload, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(decisions[1].Payload, &second); err != nil {
		t.Fatal(err)
	}
	wantFirst := []event.UnitReduction{{UnitIndex: 4, QuantityBefore: 5000, QuantityAfter: 4999}}
	wantSecond := []event.UnitReduction{{UnitIndex: 4, QuantityBefore: 4999, QuantityAfter: 4998}, {UnitIndex: 3, QuantityBefore: 5000, QuantityAfter: 4999}}
	if !slices.Equal(first.Reductions, wantFirst) || first.CashInLieu != 38.9 || first.NewShares != 7 {
		t.Fatalf("first split = %+v, want the 7-for-1 reducing %+v", first, wantFirst)
	}
	if !slices.Equal(second.Reductions, wantSecond) || second.QuantityAfter != 19997 {
		t.Fatalf("second split = %+v, want %+v leaving 19997", second, wantSecond)
	}

	_, records, err := journal.Read(bytes.NewReader(written))
	if err != nil {
		t.Fatal(err)
	}
	snapshots := map[time.Time]float64{}
	var exited event.CampaignExitedPayload
	var sold int64
	for _, r := range records {
		switch r.Envelope.Type {
		case event.AccountSnapshotEventType:
			var s event.AccountSnapshotPayload
			if err := json.Unmarshal(r.Envelope.Payload, &s); err != nil {
				t.Fatal(err)
			}
			snapshots[s.AsOf.UTC()] = s.AvailableCash
		case event.FillEventType:
			var f event.FillPayload
			if err := json.Unmarshal(r.Envelope.Payload, &f); err != nil {
				t.Fatal(err)
			}
			if f.Kind == event.FillKindExit || f.Kind == event.FillKindStop {
				sold += f.Quantity
			}
		case event.CampaignExitedEventType:
			if err := json.Unmarshal(r.Envelope.Payload, &exited); err != nil {
				t.Fatal(err)
			}
		}
	}
	if sold != 19997 {
		t.Fatalf("the Campaign's closing fills sold %d, want the 19997 held after both splits", sold)
	}
	if exited.Quantity != 20000 {
		t.Fatalf("campaign exited quantity = %d, want its whole life's 20000, the three lost shares included", exited.Quantity)
	}
	// No fill moves the account between these closes, so each statement
	// differs from the one before by its split's cash alone (ADR 0020: the
	// credit is stated at the next close, never spent before it).
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	if got, want := snapshots[day(26)], snapshots[day(25)]+38.9; got != want {
		t.Errorf("cash stated at the 26th = %v, want the 25th's plus the first split's cash, %v", got, want)
	}
	if got, want := snapshots[day(29)], snapshots[day(28)]+78.8; got != want {
		t.Errorf("cash stated at the 29th = %v, want the 28th's plus the second split's cash, %v", got, want)
	}

	for _, mode := range []string{"-verify", "-replay", "-rerun"} {
		var checked bytes.Buffer
		if err := run(context.Background(), []string{mode, out}, &checked); err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, checked.String())
		}
	}
}

// TestDecisionTextStatesACashInLieu: the decision log reads the recorded
// reductions and cash back, recomputing nothing (docs/development.md
// principle 3).
func TestDecisionTextStatesACashInLieu(t *testing.T) {
	at := time.Date(2026, 1, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		payload event.CampaignCashInLieuPayload
		want    string
	}{
		{
			event.CampaignCashInLieuPayload{
				CampaignID: "campaign:AAPL", InstrumentID: "AAPL", CorporateActionID: "corporate-action:AAPL", EffectiveAt: at,
				NewShares: 7, OldShares: 1, EngineSharesPerRawShare: 1, RawSharesLost: 2, EngineSharesLost: 2,
				CashInLieu: 78.8, Currency: "USD",
				Reductions:     []event.UnitReduction{{UnitIndex: 4, QuantityBefore: 4999, QuantityAfter: 4998}, {UnitIndex: 3, QuantityBefore: 5000, QuantityAfter: 4999}},
				QuantityBefore: 19999, QuantityAfter: 19997,
				Rule: event.RuleCashInLieuMostRecentUnitsFirst, ADR: event.ADRSplitCashInLieu,
			},
			`took one raw share (1 engine share) off Units [4 3] of Campaign "campaign:AAPL", most recent first, because the 7-for-1 split of corporate action "corporate-action:AAPL" lost 2 raw share(s) and paid 78.8 USD cash in lieu; the Campaign now holds 19997 shares, 19999 before`,
		},
		{
			event.CampaignCashInLieuPayload{
				CampaignID: "campaign:AAPL", InstrumentID: "AAPL", CorporateActionID: "corporate-action:AAPL", EffectiveAt: at,
				NewShares: 2, OldShares: 1, EngineSharesPerRawShare: 28, CashInLieu: 2.75, Currency: "USD",
				Reductions: []event.UnitReduction{}, QuantityBefore: 619136, QuantityAfter: 619136,
				Rule: event.RuleCashInLieuMostRecentUnitsFirst, ADR: event.ADRSplitCashInLieu,
			},
			`recorded 2.75 USD cash in lieu for Campaign "campaign:AAPL" from the 2-for-1 split of corporate action "corporate-action:AAPL", which lost no whole share; the Campaign still holds 619136 shares`,
		},
	} {
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		e := event.Envelope{Type: event.CampaignCashInLieuEventType, SchemaVersion: event.CampaignCashInLieuSchemaVersion, Payload: raw}
		got, err := decisionSentence(e)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("got  %s\nwant %s", got, tc.want)
		}
	}
}

// TestCorporateActionsAreDeliveredInATotalOrder: actions sharing an
// effective time, an instrument and a kind still have one order, taken from
// the whole payload, so the file's order never decides which is applied
// first.
func TestCorporateActionsAreDeliveredInATotalOrder(t *testing.T) {
	at := time.Date(2026, 1, 25, 12, 0, 0, 0, time.UTC)
	split := func(lost int64, cash float64) event.CorporateActionPayload {
		return event.CorporateActionPayload{
			InstrumentID: "AAPL", Kind: event.CorporateActionKindSplit, EffectiveAt: at,
			NewShares: 7, OldShares: 1, EngineSharesPerRawShare: 1, RawSharesLost: lost, CashInLieu: cash, Currency: "USD",
		}
	}
	delisting := event.CorporateActionPayload{InstrumentID: "AAPL", Kind: event.CorporateActionKindDelisting, EffectiveAt: at}
	actions := []event.CorporateActionPayload{split(1, 10), split(0, 2.75), delisting, split(1, 9)}
	want := []event.CorporateActionPayload{delisting, split(0, 2.75), split(1, 10), split(1, 9)}
	for _, due := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}} {
		ordered, err := effectiveOrder(actions, slices.Clone(due))
		if err != nil {
			t.Fatalf("effectiveOrder(%v) error = %v", due, err)
		}
		var got []event.CorporateActionPayload
		for _, i := range ordered {
			got = append(got, actions[i])
		}
		if !slices.Equal(got, want) {
			t.Fatalf("order from %v = %+v, want %+v", due, got, want)
		}
	}
}
