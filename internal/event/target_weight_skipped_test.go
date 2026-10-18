package event_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func validOrderSkipped() event.OrderSkippedPayload {
	return event.OrderSkippedPayload{
		AccountID: "ira-traditional", Tag: "targets-decided:ira:t", ClientOrderID: "tw-1", InstrumentID: "AAA",
		Side: event.OrderSideBuy, Phase: event.OrderSideBuy, Reason: event.OrderSkippedBelowOneShare, Shares: 0.6,
		TargetWeight: 0.05, NetLiquidation: 10000, HeldQuantity: 4, ReferencePrice: 120,
	}
}

func TestOrderSkippedValidate(t *testing.T) {
	v := validOrderSkipped()
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	scaled := v
	scaled.Reason, scaled.Scale = event.OrderSkippedScaledBelowOneShare, 0.4
	held := v
	held.Side, held.Phase, held.Reason, held.Shares = event.OrderSideSell, event.OrderSideSell, event.OrderSkippedHeldByWorkingSells, 4
	for _, p := range []event.OrderSkippedPayload{scaled, held} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.Reason, err)
		}
	}
	type P = event.OrderSkippedPayload
	expectInvalid(t, "account", v, func(p *P) { p.AccountID = "" }, "account id")
	expectInvalid(t, "tag", v, func(p *P) { p.Tag = "" }, "tag")
	expectInvalid(t, "phase", v, func(p *P) { p.Phase = event.OrderSideSell }, "phase")
	expectInvalid(t, "reason", v, func(p *P) { p.Reason = "cheap" }, "reason")
	expectInvalid(t, "scale without scaling", v, func(p *P) { p.Scale = 0.5 }, "scale")
	expectInvalid(t, "scaled sell", scaled, func(p *P) { p.Side, p.Phase = event.OrderSideSell, event.OrderSideSell }, "scaled skip")
	expectInvalid(t, "scale of 1", scaled, func(p *P) { p.Scale = 1 }, "scaled skip")
	expectInvalid(t, "a whole share", v, func(p *P) { p.Shares = 1 }, "shares")
	expectInvalid(t, "negative shares", v, func(p *P) { p.Shares = -0.5 }, "shares")
	expectInvalid(t, "target", v, func(p *P) { p.TargetWeight = math.NaN() }, "target weight")
	expectInvalid(t, "net liquidation", v, func(p *P) { p.NetLiquidation = 0 }, "net liquidation")
	expectInvalid(t, "held", v, func(p *P) { p.HeldQuantity = -1 }, "held quantity")
}

// nameAt mutates a copy of name i, so no case leaks into the next through the
// Names slice the copies of a payload share.
func nameAt(i int, f func(n *event.MomentumName)) func(*event.TargetsDecidedPayload) {
	return func(p *event.TargetsDecidedPayload) {
		p.Momentum.Names = append([]event.MomentumName(nil), p.Momentum.Names...)
		f(&p.Momentum.Names[i])
	}
}

func TestUnaffordableNamesValidate(t *testing.T) {
	v := validMomentumDecided()
	v.Momentum.MinimumLot = event.MinimumLotFigures{NetLiquidation: 30000, MomentumScale: 0.9}
	v.Momentum.Names[1] = event.MomentumName{InstrumentID: "BBB", UniverseRank: 2, DollarVolume: 4e8, Sector: "Health", SectorSource: "ishares-iwv-gics",
		Status: event.MomentumStatusUnaffordable, Closes: 300, Score: 0.2, Volatility: 0.03, ScoreRank: 2, OneShare: 900, TargetValue: 800}
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	type P = event.TargetsDecidedPayload
	expectInvalid(t, "no lot figures", v, func(p *P) { p.Momentum.MinimumLot = event.MinimumLotFigures{} }, "minimum lot figures")
	expectInvalid(t, "scale", v, func(p *P) { p.Momentum.MinimumLot.MomentumScale = 1.5 }, "scale in (0, 1]")
	expectInvalid(t, "net liquidation", v, func(p *P) { p.Momentum.MinimumLot.NetLiquidation = 0 }, "positive net liquidation")
	expectInvalid(t, "affordable", v, nameAt(1, func(n *event.MomentumName) { n.OneShare = 700 }), "exceeds its target value")
	expectInvalid(t, "figures on a pick", v, nameAt(0, func(n *event.MomentumName) { n.OneShare = 10 }), "only an unaffordable name")
	if err := v.Validate(); err != nil {
		t.Errorf("a case changed the valid record: %v", err)
	}

	// A book D7-L found wholly unaffordable is withheld with its figures.
	w := validMomentumDecided()
	w.Momentum.MinimumLot = event.MinimumLotFigures{NetLiquidation: 30000, MomentumScale: 0.9}
	w.Momentum.Names[0] = event.MomentumName{InstrumentID: "AAA", UniverseRank: 1, DollarVolume: 5e8, Sector: "Tech", SectorSource: "ishares-iwv-gics",
		Status: event.MomentumStatusUnaffordable, Closes: 300, Score: 0.3, Volatility: 0.02, ScoreRank: 1, OneShare: 900, TargetValue: 800}
	w.Outcome, w.WithheldReason, w.Detail, w.Targets = event.TargetsOutcomeWithheld, event.WithheldAllUnaffordable, "every candidate's one share exceeds its target value", nil
	if err := w.Validate(); err != nil {
		t.Errorf("an all-unaffordable withhold: %v", err)
	}
}

// TestHeldOneLotShortNamesValidate: a held pick D7-L keeps one lot short
// (ADR 0026, point 2, "D7-L", amended 2026-10-18) keeps its weight and
// records its one-share price above its target value, needs the minimum-lot
// figures, and records the holding it keeps: whole shares, their value and
// that value's share of net liquidation, all positive. No other status
// records a holding.
func TestHeldOneLotShortNamesValidate(t *testing.T) {
	v := validMomentumDecided()
	v.Momentum.MinimumLot = event.MinimumLotFigures{NetLiquidation: 30000, MomentumScale: 1}
	v.Momentum.Names[0].Status = event.MomentumStatusHeldOneLotShort
	v.Momentum.Names[0].OneShare, v.Momentum.Names[0].TargetValue = 20000, 14700
	v.Momentum.Names[0].HeldQuantity, v.Momentum.Names[0].HeldValue, v.Momentum.Names[0].HeldShare = 2, 40000, 40000.0/30000
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	type P = event.TargetsDecidedPayload
	first := func(f func(n *event.MomentumName)) func(*P) { return nameAt(0, f) }
	expectInvalid(t, "no lot figures", v, func(p *P) { p.Momentum.MinimumLot = event.MinimumLotFigures{} }, "minimum lot figures")
	expectInvalid(t, "affordable", v, first(func(n *event.MomentumName) { n.OneShare = 14000 }), "exceeds its target value")
	expectInvalid(t, "no weight", v, first(func(n *event.MomentumName) { n.Weight = 0 }), "keeps its weight")
	expectInvalid(t, "no holding", v, first(func(n *event.MomentumName) { n.HeldQuantity = 0 }), "holding it keeps")
	expectInvalid(t, "no held value", v, first(func(n *event.MomentumName) { n.HeldValue = 0 }), "holding it keeps")
	expectInvalid(t, "no held share", v, first(func(n *event.MomentumName) { n.HeldShare = 0 }), "holding it keeps")
	// The figures agree with the record: the value is the shares at the one-share
	// price, and the share is that value over the net liquidation, each as the
	// writer computes it, so a record cannot understate the concentration.
	expectInvalid(t, "value not shares x one share", v, first(func(n *event.MomentumName) { n.HeldValue = 1 }), "shares x one share")
	expectInvalid(t, "value off by one share", v, first(func(n *event.MomentumName) {
		n.HeldValue, n.HeldShare = 20000, 20000.0/30000
	}), "shares x one share")
	expectInvalid(t, "share not value over net liquidation", v, first(func(n *event.MomentumName) { n.HeldShare = 0.00001 }), "value over net liquidation")
	expectInvalid(t, "share against another net liquidation", v, func(p *P) { p.Momentum.MinimumLot.NetLiquidation = 40000 }, "value over net liquidation")
	if err := v.Validate(); err != nil {
		t.Errorf("a case changed the valid record: %v", err)
	}
	expectInvalid(t, "holding on a pick", validMomentumDecided(), func(p *P) { p.Momentum.Names[0].HeldValue = 10 }, "only a held name kept one lot short")
}
