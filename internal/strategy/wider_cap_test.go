package strategy_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
	"github.com/richard-whittemore/TrendInvesting/internal/strategy"
)

// TestTotalLongCapsProperty fills seeded competing Sessions through ADR 0008's
// configured limits. Other caps and cash are generous to isolate total long.
func TestTotalLongCapsProperty(t *testing.T) {
	for _, limit := range []int{12, 24, 36} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) { testTotalLongCap(t, limit) })
	}
}

func testTotalLongCap(t *testing.T, limit int) {
	t.Parallel()

	const (
		maxUnits          = 1
		maxUnitsPerSector = 1000000
	)
	cfg := compactChannelConfig(maxUnits, 1_000_000, maxUnitsPerSector, limit)
	cfg.UnitVolatilityFraction = 0.0001
	p := probeWiderCap(t, cfg)

	reducer, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatalf("NewReducer() error = %v", err)
	}
	var seq uint64
	nextSeq := func() uint64 { seq++; return seq }
	apply := func(envelope event.Envelope) []event.Envelope {
		t.Helper()
		out, err := reducer.Apply(context.Background(), envelope)
		if err != nil {
			t.Fatalf("Apply(%s) error = %v", envelope.Type, err)
		}
		return out
	}
	apply(configEnvelopeFor(t, cfg, nextSeq()))
	snapshot := defaultAccountSnapshot(cfg)
	apply(accountSnapshotEnvelope(t, nextSeq(), snapshot, snapshot.AsOf))

	rng := rand.New(rand.NewSource(20260924))
	filled, competingSessions, next, dayOffset := 0, 0, 0, 0
	for round := 0; round < 18; round++ {
		// Between two and four fresh instruments warm up together and break
		// out in one Session.
		var ids []string
		var series [][]event.CompletedBarPayload
		for range 2 + rng.Intn(3) {
			id := fmt.Sprintf("S%02d", next)
			next++
			ids = append(ids, id)
			series = append(series, compactEntryBars(id, dayOffset))
		}
		var closeEmitted []event.Envelope
		for i := range series[0] {
			for _, bars := range series {
				apply(barEnvelope(t, nextSeq(), bars[i], bars[i].PeriodEnd))
			}
			closeEmitted = apply(sessionClosedEnvelope(t, nextSeq(), series[0][i].PeriodEnd, ids))
		}
		periodEnd := series[0][len(series[0])-1].PeriodEnd
		dayOffset += len(series[0]) + 1

		capDeclines := 0
		for _, e := range envelopesOfType(closeEmitted, event.ProposalDeclinedEventType) {
			if decodeProposalDeclined(t, e).Reason == event.DeclineReasonUnitCapExceeded {
				capDeclines++
			}
		}
		proposed := envelopesOfType(closeEmitted, event.TradeProposalEventType)
		if len(proposed) > 0 && capDeclines > 0 {
			// A Session that both proposed and declined for a cap: the
			// competition this test exists for.
			competingSessions++
		}
		for _, e := range proposed {
			id := decodeTradeProposal(t, e).InstrumentID
			for _, out := range apply(fillEnvelope(t, nextSeq(), compactEntryFill(id, periodEnd, "fill-"+id, p.Quantity, p.EntryLevel))) {
				if out.Type == event.CampaignOpenedEventType {
					filled++
					if filled > limit {
						t.Fatalf("post-fill exposure %d exceeds %d", filled, limit)
					}
				}
			}
		}
		// Every instrument holds at most one Unit (maxUnits 1, no Adds
		// here), so the Unclassified Group and total long both hold one per
		// opened Campaign.
		if filled > maxUnitsPerSector || filled > limit {
			t.Fatalf("after round %d: %d Units filled, exceeding the Unclassified Group cap %d or the total-long cap %d", round, filled, maxUnitsPerSector, limit)
		}
	}
	if filled != limit {
		t.Fatalf("filled %d Units, want the total-long cap %d reached exactly: the fixture must fill up to the cap", filled, limit)
	}
	if competingSessions == 0 {
		t.Fatal("fixture error: no Session both proposed and declined for a cap, so same-Session competition was never exercised")
	}
}

// probeWiderCap observes the synthetic entry through Apply without registering
// new scenarios in an already released decision corpus (ADR 0016).
func probeWiderCap(t *testing.T, cfg event.ConfigurationPayload) event.TradeProposalPayload {
	t.Helper()
	r, err := strategy.NewReducer(testStrategyVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	apply := func(e event.Envelope) []event.Envelope {
		out, err := r.Apply(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	next := func() uint64 { seq++; return seq }
	apply(configEnvelopeFor(t, cfg, next()))
	snap := defaultAccountSnapshot(cfg)
	apply(accountSnapshotEnvelope(t, next(), snap, snap.AsOf))
	var out []event.Envelope
	for _, b := range compactEntryBars("PROBE", 0) {
		apply(barEnvelope(t, next(), b, b.PeriodEnd))
		out = apply(sessionClosedEnvelope(t, next(), b.PeriodEnd, []string{"PROBE"}))
	}
	proposals := envelopesOfType(out, event.TradeProposalEventType)
	if len(proposals) != 1 {
		t.Fatalf("probe proposals %d", len(proposals))
	}
	return decodeTradeProposal(t, proposals[0])
}
