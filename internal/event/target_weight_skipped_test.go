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
	expectInvalid(t, "affordable", v, func(p *P) { p.Momentum.Names[1].OneShare = 700 }, "exceeds its target value")
	expectInvalid(t, "figures on a pick", v, func(p *P) { p.Momentum.Names[0].OneShare = 10 }, "only an unaffordable name")
}
