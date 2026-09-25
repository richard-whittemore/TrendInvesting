package strategy_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestRecomputedNAddLadder pins the synthetic arithmetic of ADR 0006's
// declared Variant, using the half-N ladder and 2N stops of T p.19-23.
// Sixty-four bars of H=101,L=99,C=100 give N=2. Entry fills at 105 for
// floor(1,000,000*0.005/2)=2500 shares, stop=105-2*2=101.
// The entry bar has TR=6, so next N=(19*2+6)/20=2.2. Add 2's rung is
// 105+0.5*2.2=106.1, cap=108.3, size=floor(5000/2.2)=2272.
// Its actual fill 106.5 sets stop=106.5-2*2.2=102.1 and raises Unit 1's
// stop to 101+0.5*2.2=102.1. That bar's TR=4 makes next N=2.29.
// Add 3's rung is 106.5+0.5*2.29=107.645, size=floor(5000/2.29)=2183;
// fill 108 sets stop=108-2*2.29=103.42 and raises both earlier stops to
// 102.1+0.5*2.29=103.245. These are invented mechanics inputs, not a
// source-transcribed performance scenario. Each decision excludes its bar.
func TestRecomputedNAddLadder(t *testing.T) {
	for _, recompute := range []bool{false, true} {
		name := "baseline"
		if recompute {
			name = "recompute-n-at-add"
		}
		t.Run(name, func(t *testing.T) {
			cfg := validConfigurationPayload()
			cfg.RecomputeNAtAdd = recompute
			cfg.StrategyID = name
			s := recomputeEntry(t, cfg, 105)
			campaignID := testDecisionID("campaign", "AAPL", day(65))
			s.bar(completedBar("AAPL", day(66), 107, 103, 106)).
				fill(addFill("AAPL", campaignID, 2, day(66), "add2", 106.5, 2000, day(66))).
				bar(completedBar("AAPL", day(67), 108, 104, 107)).
				fill(addFill("AAPL", campaignID, 3, day(67), "add3", 108, 2000, day(67)))
			out := s.mustRun()
			proposals := envelopesOfType(out, event.AddProposalEventType)
			if len(proposals) != 2 {
				t.Fatalf("got %d Adds, want 2", len(proposals))
			}
			ns, levels, quantities := []float64{2, 2}, []float64{106, 107.5}, []int64{2500, 2500}
			stops := []float64{102.5, 104}
			if recompute {
				ns, levels, quantities, stops = []float64{2.2, 2.29}, []float64{106.1, 107.645}, []int64{2272, 2183}, []float64{102.1, 103.42}
			}
			for i, e := range proposals {
				p := decodeAddProposal(t, e)
				nearN(t, "rung", p.Level, levels[i])
				nearN(t, "cap", p.PriceCap, levels[i]+ns[i])
				nearN(t, "decision N", p.EffectiveN(), ns[i])
				nearN(t, "opening N", p.CampaignN, 2)
				if p.Quantity != quantities[i] {
					t.Errorf("quantity=%d, want %d", p.Quantity, quantities[i])
				}
			}
			for i, e := range envelopesOfType(out, event.CampaignUnitAddedEventType) {
				p := decodeCampaignUnitAdded(t, e)
				nearN(t, "new stop", p.ProtectiveStop, stops[i])
			}
			raised := 0
			for _, e := range envelopesOfType(out, event.ProtectiveStopSetEventType) {
				p := decodeProtectiveStopSet(t, e)
				if p.Reason != event.ProtectiveStopReasonInitial {
					n := 2.0
					if recompute {
						n = 2.29
						if raised == 0 {
							n = 2.2
						}
					}
					nearN(t, "raise", p.Level-p.PreviousLevel, 0.5*n)
					raised++
				}
			}
			if raised != 3 {
				t.Fatalf("got %d raises", raised)
			}
		})
	}
}

func nearN(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.15g, want %.15g", label, got, want)
	}
}

func recomputeEntry(t *testing.T, cfg event.ConfigurationPayload, price float64) *stream {
	t.Helper()
	s := newStream(t, cfg)
	for i := 1; i <= 64; i++ {
		s.bar(completedBar("AAPL", day(i), 101, 99, 100))
	}
	s.bar(completedBar("AAPL", day(65), 105, 99, 104))
	f := openingFill("AAPL")
	f.ProposalID = testDecisionID("proposal", "AAPL", day(65))
	f.Price = price
	f.Quantity = 2500
	f.FilledAt = day(65)
	return s.fill(f)
}

// TestRecomputedNProposalRetainsItsOperandAcrossSessions pins ADR 0011's
// surviving fill-chain proposal: its fill uses the N it authorised, while
// the next rung uses the newer prior-bar N (ADR 0006), never the current bar.
func TestRecomputedNProposalRetainsItsOperandAcrossSessions(t *testing.T) {
	cfg := validConfigurationPayload()
	cfg.RecomputeNAtAdd = true
	id := testDecisionID("campaign", "AAPL", day(65))
	out := recomputeEntry(t, cfg, 101.1).
		bar(completedBar("AAPL", day(66), 108, 103, 106)).
		fill(addFill("AAPL", id, 2, day(65), "retained", 106.5, 2500, day(66))).
		fill(addFill("AAPL", id, 3, day(66), "newer", 108, 2200, day(66))).mustRun()
	ps := envelopesOfType(out, event.AddProposalEventType)
	if len(ps) != 2 {
		t.Fatalf("got %d proposals", len(ps))
	}
	nearN(t, "surviving proposal N", decodeAddProposal(t, ps[0]).AddN, 2)
	nearN(t, "new chain N", decodeAddProposal(t, ps[1]).AddN, 2.2)
	adds := envelopesOfType(out, event.CampaignUnitAddedEventType)
	nearN(t, "retained stop", decodeCampaignUnitAdded(t, adds[0]).ProtectiveStop, 102.5)
	nearN(t, "newer stop", decodeCampaignUnitAdded(t, adds[1]).ProtectiveStop, 103.6)
}

// TestRecomputedNChangesOnlyTheSizingDenominator keeps account drift out of
// ADR 0006's single-dimension ablation (ADR 0012), even after a drawdown.
func TestRecomputedNChangesOnlyTheSizingDenominator(t *testing.T) {
	cfg := validConfigurationPayload()
	cfg.RecomputeNAtAdd = true
	out := recomputeEntry(t, cfg, 105).
		snapshot(event.AccountSnapshotPayload{AsOf: day(65), Equity: 800000, AvailableCash: 800000, Currency: "USD"}).
		bar(completedBar("AAPL", day(66), 107, 103, 106)).mustRun()
	p := decodeAddProposal(t, envelopesOfType(out, event.AddProposalEventType)[0])
	if p.Quantity != 2272 {
		t.Fatalf("quantity %d, want floor(5000/2.2)=2272", p.Quantity)
	}
}

// TestRecomputedNCanIncreaseUnitSize pins falling N, including fractional
// truncation from the original sizing budget, under ADR 0006's Variant.
func TestRecomputedNCanIncreaseUnitSize(t *testing.T) {
	cfg := validConfigurationPayload()
	cfg.RecomputeNAtAdd = true
	s := recomputeEntry(t, cfg, 105)
	for i := 66; i <= 69; i++ {
		s.bar(completedBar("AAPL", day(i), 105, 105, 105))
	}
	out := s.bar(completedBar("AAPL", day(70), 107, 105, 106)).mustRun()
	p := decodeAddProposal(t, envelopesOfType(out, event.AddProposalEventType)[0])
	// N: 2.2 -> 2.14 -> 2.033 -> 1.93135 -> 1.8347825.
	nearN(t, "contracted N", p.AddN, 1.8347825)
	if p.Quantity != 2725 {
		t.Fatalf("quantity %d, want floor(5000/1.8347825)=2725", p.Quantity)
	}
}
