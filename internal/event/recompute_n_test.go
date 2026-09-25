package event_test

import (
	"math"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// TestAddNContracts pins ADR 0006's explicit Variant operand on all three
// decisions. Their opening CampaignN remains independent and immutable.
func TestAddNContracts(t *testing.T) {
	for _, n := range []float64{0, 2.2, -1, math.NaN(), math.Inf(1)} {
		add := validAddProposal()
		add.AddN = n
		added := validCampaignUnitAdded()
		added.AddN = n
		initial := validProtectiveStopSet()
		initial.AddN = n
		raised := validProtectiveStopSetRaised()
		raised.AddN = n
		add.Level = add.PreviousUnitFill + float64(0.5*add.EffectiveN())
		add.PriceCap = add.Level + float64(add.GapBufferN*add.EffectiveN())
		added.ProtectiveStop = added.FillPrice - float64(added.StopMultiple*added.EffectiveN())
		initial.Level = initial.EntryPrice - float64(initial.StopMultiple*initial.EffectiveN())
		raised.Level = raised.PreviousLevel + float64(0.5*raised.EffectiveN())
		for _, p := range []interface{ Validate() error }{add, added, initial, raised} {
			err := p.Validate()
			valid := n == 0 || n == 2.2
			if (err == nil) != valid {
				t.Errorf("%T add_n=%v: %v", p, n, err)
			}
		}
	}
	cfg := validConfiguration()
	baseline := event.ConfigurationHash(cfg)
	cfg.RecomputeNAtAdd = true
	if baseline == event.ConfigurationHash(cfg) {
		t.Fatal("Variant missing from configuration identity")
	}
}
