package event_test

import (
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

// A recorded fraction is read back; an absent one (an earlier run's record)
// reads as zero, never as the new-load default (ADR 0026, 2026-10-31).
func TestCashBufferReadsTheRecordedFractionAndZeroWhenAbsent(t *testing.T) {
	if got := (event.ExecutionConfiguration{}).CashBuffer(); got != 0 {
		t.Errorf("absent fraction = %v, want 0", got)
	}
	f := 0.02
	if got := (event.ExecutionConfiguration{CashBufferFraction: &f}).CashBuffer(); got != 0.02 {
		t.Errorf("recorded fraction = %v, want 0.02", got)
	}
}

// The default applies to a momentum configuration with no fraction, keeps
// one already given, and never touches Boost.
func TestDefaultCashBufferAppliesOnlyToAMomentumConfigurationWithoutOne(t *testing.T) {
	p := &event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyMomentum}
	p.DefaultCashBuffer()
	if got := p.Execution.CashBuffer(); got != 0.01 {
		t.Errorf("momentum default = %v, want 0.01", got)
	}
	given := 0.03
	p = &event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyMomentum}
	p.Execution.CashBufferFraction = &given
	p.DefaultCashBuffer()
	if got := p.Execution.CashBuffer(); got != 0.03 {
		t.Errorf("given fraction overwritten: %v, want 0.03", got)
	}
	other := &event.TargetWeightConfigurationPayload{StrategyID: event.TargetWeightStrategyBoost}
	other.DefaultCashBuffer()
	if other.Execution.CashBufferFraction != nil {
		t.Errorf("Boost gained a cash buffer")
	}
}
