package fills_test

import (
	"math"
	"strings"
	"testing"

	"github.com/richard-whittemore/TrendInvesting/internal/fills"
)

// Slippage and commission are the two figures this package multiplies a fill
// price by, so a non-finite one does not stop at the fill: it propagates into
// the Campaign's entry price, its realised result, and every Add rung
// measured from that fill. NaN compares false against every threshold, so
// each guard is an explicit finiteness test rather than a range test, and
// these are the cases that hold them to the field they name.

func TestNewRefusesANonFiniteSlippage(t *testing.T) {
	t.Parallel()

	cfg := baselineConfig()
	cfg.SlippageN = math.NaN()

	_, err := fills.New(cfg, testStrategyVersion, testConfigurationHash)
	if err == nil {
		t.Fatal("fills.New() error = nil, want a non-finite slippage to be refused")
	}
	if !strings.Contains(err.Error(), "slippage in n must be finite") {
		t.Errorf("error = %v, want it to name the slippage", err)
	}
}

func TestExecuteRefusesANonFiniteSlippage(t *testing.T) {
	t.Parallel()

	_, err := fills.Execute(fills.SideBuy, 156, fills.Range{Reference: 150, High: 160, Low: 149}, math.NaN())
	if err == nil {
		t.Fatal("Execute() error = nil, want a non-finite slippage to be refused")
	}
	if !strings.Contains(err.Error(), "slippage must be finite") {
		t.Errorf("error = %v, want it to name the slippage", err)
	}
}

func TestChargeRefusesANonFiniteCommissionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   fills.CommissionModel
		wantErr string
	}{
		{
			name:    "per share",
			model:   fills.CommissionModel{PerShare: math.NaN(), MinimumPerOrder: 1, MaximumFractionOfTradeValue: 0.01},
			wantErr: "commission per share must be finite",
		},
		{
			name:    "minimum per order",
			model:   fills.CommissionModel{PerShare: 0.005, MinimumPerOrder: math.Inf(1), MaximumFractionOfTradeValue: 0.01},
			wantErr: "commission minimum per order must be finite",
		},
		{
			name:    "maximum fraction of trade value",
			model:   fills.CommissionModel{PerShare: 0.005, MinimumPerOrder: 1, MaximumFractionOfTradeValue: math.NaN()},
			wantErr: "commission maximum fraction of trade value must be finite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.model.Charge(100, 156, 1)
			if err == nil {
				t.Fatalf("Charge() error = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}
